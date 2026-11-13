package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/review"
)

var (
	ErrEmptyDraft = errors.New("write the reply first")
	ErrSMSLength  = errors.New("an SMS reply can be at most 1600 characters")
	ErrChatLength = errors.New("a chat reply can be at most 4000 characters")
	ErrNoteNeeded = errors.New("say why a person is needed")
)

// Draft is a reply an agent wants sent on a thread.
type Draft struct {
	// Subject is for email; empty means "Re:" the thread's subject.
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// Reason is shown to the person deciding: why this reply.
	Reason string `json:"reason"`
	// QuickReplies are buttons under a chat reply (chat only, at most 4).
	QuickReplies []string `json:"quick_replies,omitempty"`
}

// Decision is what policy made of a draft. Held drafts wait for a person;
// denied ones are not stored as drafts at all.
type Decision struct {
	ProposalID string `json:"proposal_id,omitempty"`
	Decision   string `json:"decision"`
	Rule       string `json:"rule"`
	Reason     string `json:"reason"`
}

type check struct {
	Label string `json:"label"`
	OK    bool   `json:"ok"`
}

// Propose drafts a reply on a thread. Nothing an agent drafts is sent until a
// person approves it: this is the approve mode every agent starts in, and the
// only one built so far. The approval binds exactly this text and recipient.
func Propose(ctx context.Context, pool *pgxpool.Pool, a Agent, threadID string, d Draft) (Decision, error) {
	if !validID(threadID) {
		return Decision{}, ErrNotFound
	}
	var out Decision
	err := db.Tx(ctx, pool, func(tx pgx.Tx) (err error) {
		out, err = propose(ctx, tx, a, threadID, d, "", nil)
		return err
	})
	return out, err
}

// propose is Propose inside a transaction. to, when set, is the email
// address to write to, which must be one of the contact's; otherwise the
// contact's best address is used. send, when set, sends the message instead
// of holding it, provided nothing on the thread requires a person.
func propose(ctx context.Context, tx pgx.Tx, a Agent, threadID string, d Draft, to string, send *integrationSend) (Decision, error) {
	d.Subject, d.Reason = strings.TrimSpace(d.Subject), strings.TrimSpace(d.Reason)
	if strings.TrimSpace(d.Body) == "" {
		return Decision{}, ErrEmptyDraft
	}
	var out Decision
	err := func() error {
		var contactID, channel, subject, local, remote, holder string
		var takenOver bool
		err := tx.QueryRow(ctx, `SELECT t.contact_id, t.channel, coalesce(t.subject, ''), coalesce(t.phone_number, ''),
				coalesce(t.remote_address, ''), t.taken_over_by IS NOT NULL,
				CASE WHEN t.assignee_id <> $3 AND t.claimed_until > now() THEN coalesce(p.name, 'another agent') ELSE '' END
			FROM threads t LEFT JOIN principals p ON p.id = t.assignee_id
			WHERE t.id = $1 AND t.workspace_id = $2 FOR UPDATE OF t`, threadID, a.WorkspaceID, a.ID).
			Scan(&contactID, &channel, &subject, &local, &remote, &takenOver, &holder)
		if db.NotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		deny := func(rule, reason string) error {
			out = Decision{Decision: "denied", Rule: rule, Reason: reason}
			return event(ctx, tx, a, threadID, "proposal.denied", map[string]any{"rule": rule, "reason": reason})
		}
		if holder != "" {
			return deny("claimed", holder+" is working on this thread (threads_claim). Leave it to them.")
		}

		var optedOut bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM opt_outs WHERE contact_id = $1 AND channel = $2)`,
			contactID, channel).Scan(&optedOut); err != nil {
			return err
		}
		if optedOut {
			return deny("opted_out", "The contact opted out of "+channel+".")
		}

		var payload, recipients []byte
		switch channel {
		case "email":
			err := tx.QueryRow(ctx, `SELECT address FROM identities WHERE contact_id = $1 AND channel = 'email'
				AND ($2 = '' OR address = $2) ORDER BY verified DESC, created_at LIMIT 1`, contactID, to).Scan(&to)
			if db.NotFound(err) {
				return deny("no_address", "The contact has no email address.")
			}
			if err != nil {
				return err
			}
			if d.Subject == "" {
				d.Subject = replySubject(subject)
			}
			// As for a person's reply: the executor threads it under the
			// customer's last message.
			var inReplyTo *string
			_ = tx.QueryRow(ctx, `SELECT provider_ref FROM events WHERE thread_id = $1 AND type = 'email.received'
				AND provider_ref IS NOT NULL ORDER BY id DESC LIMIT 1`, threadID).Scan(&inReplyTo)
			payload, _ = json.Marshal(map[string]any{"subject": d.Subject, "body": d.Body, "in_reply_to": inReplyTo})
			recipients, _ = json.Marshal([]map[string]string{{"email": to}})
		case "sms":
			if utf8.RuneCountInString(d.Body) > 1600 {
				return ErrSMSLength
			}
			if local == "" || remote == "" {
				return deny("no_address", "This thread has no phone numbers to reply between.")
			}
			payload, _ = json.Marshal(map[string]any{"from": local, "body": d.Body})
			recipients, _ = json.Marshal([]map[string]string{{"phone": remote}})
		case "chat":
			if utf8.RuneCountInString(d.Body) > chat.MaxMessage {
				return ErrChatLength
			}
			var visitorID string
			err := tx.QueryRow(ctx, `SELECT id FROM chat_visitors WHERE thread_id = $1`, threadID).Scan(&visitorID)
			if db.NotFound(err) {
				return deny("no_address", "This chat has no visitor to reply to.")
			}
			if err != nil {
				return err
			}
			replies, err := chat.CleanQuickReplies(d.QuickReplies)
			if err != nil {
				return err
			}
			content := map[string]any{"body": d.Body}
			if len(replies) > 0 {
				content["quick_replies"] = replies
			}
			payload, _ = json.Marshal(content)
			recipients, _ = json.Marshal([]map[string]string{{"chat_visitor": visitorID}})
		default:
			return deny("channel_not_supported", "Agents can draft email, SMS and chat replies only.")
		}

		var injection bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE thread_id = $1 AND suspected_injection)`,
			threadID).Scan(&injection); err != nil {
			return err
		}
		out = Decision{Decision: "held"}
		switch {
		case takenOver:
			out.Rule, out.Reason = "taken_over", "A person has taken over this thread."
		case injection:
			out.Rule, out.Reason = "suspected_injection", "A message on this thread looks like an attempt to instruct an agent."
		case a.Autonomy == "observe":
			out.Rule, out.Reason = "observe", "This agent only drafts; a person sends."
		default:
			out.Rule, out.Reason = "agent_approve_mode", "Every draft from this agent waits for a person."
		}
		checks, _ := json.Marshal([]check{
			{"Not opted out", true},
			{"No suspected injection on the thread", !injection},
			{"Thread not taken over", !takenOver},
		})

		// A newer draft from the same agent replaces its older one, so a
		// person never approves a stale answer.
		rows, err := tx.Query(ctx, `UPDATE proposals SET status = 'voided', decision_note = 'Replaced by a newer draft'
			WHERE thread_id = $1 AND requested_by = $2 AND status = 'pending' RETURNING id`, threadID, a.ID)
		if err != nil {
			return err
		}
		replaced, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range replaced {
			if err := event(ctx, tx, a, threadID, "proposal.voided", map[string]any{"proposal_id": id, "reason": "Replaced by a newer draft"}); err != nil {
				return err
			}
		}

		if send != nil && !takenOver && !injection {
			sent, err := sendsLastHour(ctx, tx, a.ID)
			if err != nil {
				return err
			}
			if sent >= MaxSendsPerHour {
				return deny("send_rate", fmt.Sprintf("This integration has reached its limit of %d messages an hour.", MaxSendsPerHour))
			}
			if send.result, err = review.SendAllowed(ctx, tx, send.reviewer, threadID, channel, payload, recipients, send.key); err != nil {
				return err
			}
			out = send.decision()
			return nil
		}

		bound := review.Bound{Recipients: recipients, Payload: payload, LinkedStates: json.RawMessage("[]")}
		var reason *string
		if d.Reason != "" {
			reason = &d.Reason
		}
		if err := tx.QueryRow(ctx, `INSERT INTO proposals (workspace_id, thread_id, requested_by, action, payload, recipients,
				payload_sha256, rule, reason, checks)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			a.WorkspaceID, threadID, a.ID, channel+".send", payload, recipients, bound.Hash(), out.Rule, reason, checks).
			Scan(&out.ProposalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE threads SET version = version + 1, updated_at = now() WHERE id = $1`, threadID); err != nil {
			return err
		}
		return event(ctx, tx, a, threadID, "proposal.held", map[string]any{"proposal_id": out.ProposalID, "rule": out.Rule, "reason": d.Reason})
	}()
	return out, err
}

// Resolve closes a thread. A new message from the contact reopens it.
func Resolve(ctx context.Context, pool *pgxpool.Pool, a Agent, threadID, note string) error {
	if !validID(threadID) {
		return ErrNotFound
	}
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM threads WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, threadID, a.WorkspaceID).Scan(&state)
		if db.NotFound(err) {
			return ErrNotFound
		}
		if err != nil || state == "resolved" {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE threads SET state = 'resolved', version = version + 1, updated_at = now() WHERE id = $1`, threadID); err != nil {
			return err
		}
		return event(ctx, tx, a, threadID, "thread.resolved", map[string]any{"note": strings.TrimSpace(note)})
	})
}

// Escalate hands a thread to a person: it goes on the needs-attention list
// with the agent's note until a person acts on it.
func Escalate(ctx context.Context, pool *pgxpool.Pool, a Agent, threadID, note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return ErrNoteNeeded
	}
	if !validID(threadID) {
		return ErrNotFound
	}
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE threads SET state = CASE WHEN state = 'resolved' THEN 'open' ELSE state END,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND workspace_id = $2`, threadID, a.WorkspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return event(ctx, tx, a, threadID, "thread.escalated", map[string]any{"note": note})
	})
}

// Proposal is one of the agent's drafts and what became of it.
type Proposal struct {
	ID         string          `json:"id"`
	ThreadID   string          `json:"thread_id"`
	Action     string          `json:"action"`
	Status     string          `json:"status"`
	Rule       string          `json:"rule"`
	Reason     string          `json:"reason,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	Recipients json.RawMessage `json:"recipients"`
	// DecisionNote is the person's note on a rejection: feedback for the agent.
	DecisionNote string     `json:"decision_note,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	// SendState is the approved send's state: queued, accepted, delivered,
	// failed or unknown.
	SendState string    `json:"send_state,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// RequestedBy is who asked for it; Yours says whether that was you.
	RequestedBy string `json:"requested_by"`
	Yours       bool   `json:"yours"`
}

// proposalSQL takes the workspace ($1), the agent ($2), and whether to
// include other principals' proposals ($3).
const proposalSQL = `SELECT p.id, p.thread_id, p.action, p.status, p.rule, coalesce(p.reason, ''), p.payload, p.recipients,
		coalesce(p.decision_note, ''), p.decided_at, coalesce(x.state, ''), p.created_at, r.name, p.requested_by = $2
	FROM proposals p LEFT JOIN actions x ON x.proposal_id = p.id JOIN principals r ON r.id = p.requested_by
	WHERE p.workspace_id = $1 AND ($3 OR p.requested_by = $2)`

// Proposals lists the agent's own drafts, newest first, optionally by
// status. everyone lists every principal's instead, for a person who asks
// the agent what is waiting for a decision.
func Proposals(ctx context.Context, q db.Querier, a Agent, status string, everyone bool) ([]Proposal, error) {
	rows, err := q.Query(ctx, proposalSQL+` AND ($4 = '' OR p.status = $4) ORDER BY p.created_at DESC LIMIT 50`,
		a.WorkspaceID, a.ID, everyone, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Proposal])
}

// GetProposal is any proposal in the agent's workspace.
func GetProposal(ctx context.Context, q db.Querier, a Agent, id string) (Proposal, error) {
	if !validID(id) {
		return Proposal{}, ErrNotFound
	}
	rows, err := q.Query(ctx, proposalSQL+` AND p.id = $4`, a.WorkspaceID, a.ID, true, id)
	if err != nil {
		return Proposal{}, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Proposal])
	if db.NotFound(err) {
		return p, ErrNotFound
	}
	return p, err
}

func event(ctx context.Context, tx pgx.Tx, a Agent, threadID, typ string, payload map[string]any) error {
	_, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		SELECT workspace_id, id, contact_id, $3, $4, channel, $5 FROM threads WHERE id = $1 AND workspace_id = $2`,
		threadID, a.WorkspaceID, typ, a.ID, payload)
	if err != nil {
		return fmt.Errorf("recording %s: %w", typ, err)
	}
	return nil
}

func replySubject(subject string) string {
	if subject == "" {
		return "Re: your message"
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}
