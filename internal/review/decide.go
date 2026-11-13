package review

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/db"
)

var (
	// ErrConflict means the state changed under the reviewer: the proposal was
	// already decided, the thread was taken over by someone else, or the
	// recipient opted out. The reviewer reloads and looks again.
	ErrConflict = errors.New("this changed since you opened it")
	// ErrOptedOut blocks a send to a contact who asked not to be contacted.
	ErrOptedOut = errors.New("the contact opted out of this channel")
	// ErrHeldDraft stops a thread being marked done while a draft on it
	// still waits for a decision.
	ErrHeldDraft = errors.New("approve or reject the held draft first")
)

// Reviewer is the person deciding.
type Reviewer struct {
	ID          string
	WorkspaceID string
	// Provider limits new actions to the configured installation transport.
	Provider   string
	SMSProject string
	// Enqueue hands an action for a real provider to the executor, inside the
	// deciding transaction. Nil leaves such actions queued (tests, or no
	// provider configured).
	Enqueue func(ctx context.Context, tx pgx.Tx, actionID string) error
	// ViaAgent is the agent a person told to decide for them (MCP
	// proposals_decide). It is recorded on the decision, and an agent can
	// never decide its own proposal.
	ViaAgent string
	// Said is the person's words telling the agent to decide, recorded
	// with the decision.
	Said string
}

// ErrOwnProposal is an agent deciding a proposal it made itself.
var ErrOwnProposal = errors.New("an agent cannot decide its own proposal; a person must")

// decided is the event payload for a decision, with the agent it came
// through, if any.
func (r Reviewer) decided(payload map[string]any) map[string]any {
	if r.ViaAgent != "" {
		payload["via_agent"], payload["person_said"] = r.ViaAgent, r.Said
	}
	return payload
}

// senderSQL picks the connector a send goes through: a real, healthy
// provider before the sandbox.
const senderSQL = `SELECT id FROM connectors WHERE workspace_id = $1 AND health = 'ok'
	AND kind IN ('sandbox', 'ses', 'resend', 'postmark')
 AND ($2 = '' OR kind = $2 OR kind = 'sandbox') ORDER BY kind = 'sandbox', created_at LIMIT 1`

// EmailPayload is the content of an email proposal or action.
type EmailPayload struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// QuickReplies are buttons under a chat reply; tapping one sends its
	// words. Chat only, and not changed by an edit at approval.
	QuickReplies []string `json:"quick_replies,omitempty"`
	// Via is where a person typed a chat reply, such as "slack", so it is
	// not copied back there. Set by the server, never by a request.
	Via string `json:"-"`
}

// Bound is what an approval covers. Any change to any part is a different
// hash, so an approval cannot be stretched to cover something else.
type Bound struct {
	Recipients          json.RawMessage `json:"recipients"`
	Payload             json.RawMessage `json:"payload"`
	LinkedStates        json.RawMessage `json:"linked_states"`
	ApprovedInfoVersion *string         `json:"approved_info_version_id"`
}

// Hash is over a canonical form (keys sorted, no whitespace) so it is the
// same whether the JSON came from Go or back out of Postgres, which reorders
// jsonb keys. The executor recomputes it just before sending.
func (b Bound) Hash() []byte {
	data, _ := json.Marshal(Bound{
		Recipients: canonical(b.Recipients), Payload: canonical(b.Payload),
		LinkedStates: canonical(b.LinkedStates), ApprovedInfoVersion: b.ApprovedInfoVersion,
	})
	sum := sha256.Sum256(data)
	return sum[:]
}

func canonical(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, _ := json.Marshal(v)
	return out
}

// Approve releases a held proposal, with the reviewer's edit when given. The
// checks that could have changed since the draft are made again here, and the
// approved action is handed to the executor in the same transaction.
func Approve(ctx context.Context, pool *pgxpool.Pool, r Reviewer, proposalID string, edit *EmailPayload) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		p, err := lockPending(ctx, tx, r, proposalID)
		if err != nil {
			return err
		}
		if p.takenOverBy != nil && *p.takenOverBy != r.ID {
			return ErrConflict
		}
		if optedOut, err := recipientsOptedOut(ctx, tx, p.contactID, p.channel); err != nil {
			return err
		} else if optedOut {
			return ErrOptedOut
		}

		var connectorID string
		if p.channel == "sms" {
			if r.SMSProject == "" {
				return ErrNoSender
			}
			err = tx.QueryRow(ctx, `SELECT n.connector_id FROM phone_numbers n JOIN threads t ON t.phone_number=n.number
   JOIN connectors c ON c.id=n.connector_id WHERE t.id=$1 AND n.workspace_id=$2 AND n.provider_project_id=$3 AND c.health='ok'`, p.threadID, r.WorkspaceID, r.SMSProject).Scan(&connectorID)
		} else if p.channel == "email" {
			err = tx.QueryRow(ctx, senderSQL, r.WorkspaceID, r.Provider).Scan(&connectorID)
		} else if p.channel == "chat" {
			// Delivered by VisiComms itself; no provider, so no connector.
		} else {
			return ErrConflict
		}
		if db.NotFound(err) {
			return ErrNoSender
		}
		if err != nil {
			return err
		}
		payload := p.payload
		edited := false
		if edit != nil {
			if p.channel == "sms" || p.channel == "chat" {
				if strings.TrimSpace(edit.Body) == "" {
					return ErrEmptyReply
				}
				if p.channel == "sms" && utf8.RuneCountInString(edit.Body) > 1600 {
					return ErrSMSLength
				}
				if p.channel == "chat" && utf8.RuneCountInString(edit.Body) > chat.MaxMessage {
					return ErrChatLength
				}
			}
			var original EmailPayload
			_ = json.Unmarshal(p.payload, &original)
			if edit.Subject != original.Subject || edit.Body != original.Body {
				// Only the words change: anything else in the draft (the SMS
				// sender, the email's In-Reply-To) stays as it was.
				var draft map[string]any
				if err := json.Unmarshal(p.payload, &draft); err != nil {
					return err
				}
				draft["body"] = edit.Body
				if p.channel == "email" {
					draft["subject"] = edit.Subject
				}
				payload, err = json.Marshal(draft)
				if err != nil {
					return err
				}
				edited = true
			}
		}
		bound := Bound{Recipients: p.recipients, Payload: payload, LinkedStates: p.linkedStates, ApprovedInfoVersion: p.approvedInfoVersionID}

		if _, err := tx.Exec(ctx, `UPDATE proposals SET status = 'approved', decided_by = $2, decided_at = now() WHERE id = $1`,
			proposalID, r.ID); err != nil {
			return err
		}
		var actionID string
		err = tx.QueryRow(ctx, `INSERT INTO actions (workspace_id, thread_id, proposal_id, requested_by, connector_id,
				action, payload, recipients, payload_sha256, idempotency_key)
			VALUES ($1, $2, $3, $4, nullif($10, '')::uuid,
				$5, $6, $7, $8, $9) RETURNING id`,
			r.WorkspaceID, p.threadID, proposalID, p.requestedBy, p.action, payload, p.recipients, bound.Hash(),
			"proposal:"+proposalID, connectorID).Scan(&actionID)
		if err != nil {
			return err
		}
		if err := event(ctx, tx, r, p.threadID, "proposal.approved", r.decided(map[string]any{
			"proposal_id": proposalID, "action_id": actionID, "edited": edited,
		})); err != nil {
			return err
		}
		return dispatch(ctx, tx, r, actionID)
	})
}

// Reject declines a held proposal. The note goes back to the agent.
func Reject(ctx context.Context, pool *pgxpool.Pool, r Reviewer, proposalID, note string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		p, err := lockPending(ctx, tx, r, proposalID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE proposals SET status = 'rejected', decided_by = $2, decided_at = now(), decision_note = $3
			WHERE id = $1`, proposalID, r.ID, note); err != nil {
			return err
		}
		return event(ctx, tx, r, p.threadID, "proposal.rejected", r.decided(map[string]any{"proposal_id": proposalID, "note": note}))
	})
}

// TakeOver pauses agents on a thread: they can still read and draft, but
// nothing they propose is sent until the thread is handed back. Queued
// actions on the thread are cancelled.
func TakeOver(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE threads SET taken_over_by = $3, taken_over_at = now(), version = version + 1, updated_at = now()
			WHERE id = $1 AND workspace_id = $2 AND (taken_over_by IS NULL OR taken_over_by = $3)`, threadID, r.WorkspaceID, r.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE actions SET state = 'cancelled', updated_at = now() WHERE thread_id = $1 AND state = 'queued'`, threadID); err != nil {
			return err
		}
		return event(ctx, tx, r, threadID, "thread.taken_over", map[string]any{})
	})
}

// HandBack returns a taken-over thread to the agents.
func HandBack(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE threads SET taken_over_by = NULL, taken_over_at = NULL, version = version + 1, updated_at = now()
			WHERE id = $1 AND workspace_id = $2 AND taken_over_by = $3`, threadID, r.WorkspaceID, r.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		return event(ctx, tx, r, threadID, "thread.handed_back", map[string]any{})
	})
}

// Resolve marks a thread done: it leaves the needs-attention list. A new
// message or call from the contact reopens it.
func Resolve(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) error {
	return setState(ctx, pool, r, threadID, "resolved", "thread.resolved")
}

// Reopen puts a resolved thread back on the needs-attention list.
func Reopen(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) error {
	return setState(ctx, pool, r, threadID, "open", "thread.reopened")
}

func setState(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID, state, typ string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var current string
		err := tx.QueryRow(ctx, `SELECT state FROM threads WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
			threadID, r.WorkspaceID).Scan(&current)
		if db.NotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if (state == "resolved") == (current == "resolved") {
			return nil
		}
		if state == "resolved" {
			var held bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM proposals WHERE thread_id = $1 AND status = 'pending')`,
				threadID).Scan(&held); err != nil {
				return err
			}
			if held {
				return ErrHeldDraft
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE threads SET state = $2, version = version + 1, updated_at = now() WHERE id = $1`,
			threadID, state); err != nil {
			return err
		}
		return event(ctx, tx, r, threadID, typ, map[string]any{})
	})
}

type pending struct {
	threadID, contactID, requestedBy, action, channel string
	takenOverBy, approvedInfoVersionID                *string
	payload, recipients, linkedStates                 json.RawMessage
}

func lockPending(ctx context.Context, tx pgx.Tx, r Reviewer, proposalID string) (p pending, err error) {
	defer func() {
		if err == nil && r.ViaAgent != "" && p.requestedBy == r.ViaAgent {
			err = ErrOwnProposal
		}
	}()
	err = tx.QueryRow(ctx, `SELECT p.thread_id, t.contact_id, p.requested_by, p.action, t.taken_over_by,
			p.approved_info_version_id, p.payload, p.recipients, p.linked_states, t.channel
		FROM proposals p JOIN threads t ON t.id = p.thread_id
		WHERE p.id = $1 AND p.workspace_id = $2 AND p.status = 'pending'
		FOR UPDATE OF p, t`, proposalID, r.WorkspaceID).
		Scan(&p.threadID, &p.contactID, &p.requestedBy, &p.action, &p.takenOverBy,
			&p.approvedInfoVersionID, &p.payload, &p.recipients, &p.linkedStates, &p.channel)
	if db.NotFound(err) {
		return p, ErrConflict
	}
	return p, err
}

func recipientsOptedOut(ctx context.Context, tx pgx.Tx, contactID string, channels ...string) (bool, error) {
	channel := "email"
	if len(channels) > 0 {
		channel = channels[0]
	}
	var out bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM opt_outs WHERE contact_id = $1 AND channel = $2)`, contactID, channel).Scan(&out)
	return out, err
}

func event(ctx context.Context, tx pgx.Tx, r Reviewer, threadID, typ string, payload map[string]any) error {
	_, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		SELECT workspace_id, id, contact_id, $3, $4, channel, $5 FROM threads WHERE id = $1 AND workspace_id = $2`,
		threadID, r.WorkspaceID, typ, r.ID, payload)
	if err != nil {
		return fmt.Errorf("recording %s: %w", typ, err)
	}
	return nil
}
