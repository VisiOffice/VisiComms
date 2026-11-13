package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/review"
)

// MaxSendsPerHour caps what one integration can send, so a leaked token or a
// runaway job cannot do much before someone notices.
const MaxSendsPerHour = 1000

var (
	// ErrSendNotAllowed is a token no person has allowed to send.
	ErrSendNotAllowed = errors.New("this token is not allowed to send: a person must turn on sending for it under Connections, or leave out send to hold the message for approval")
	// ErrSendKey is a send with no usable idempotency key.
	ErrSendKey = errors.New("send needs an idempotency_key of at most 200 characters, the same on every retry of this message")
	// ErrKeyReused is one idempotency key used for two different messages.
	ErrKeyReused = errors.New("this idempotency_key was already used for a different message")
)

// integrationSend asks propose to send rather than hold, when nothing
// requires a person. result is filled when it did.
type integrationSend struct {
	reviewer review.Reviewer
	key      string
	result   review.ReplyResult
}

func (s *integrationSend) decision() Decision {
	return Decision{Decision: "allowed", Rule: "send_permission", Reason: "A person allowed this integration to send without approval."}
}

// ComposeAndSend is Compose for a trusted integration: the message goes out
// without a person, unless something requires one. Opt-outs are still refused
// and a thread a person took over or that looks like an injection attempt is
// still held. A retry with the same key returns the first result and sends
// nothing.
func ComposeAndSend(ctx context.Context, pool *pgxpool.Pool, a Agent, r review.Reviewer, m Message, key string) (Composed, error) {
	if !a.MaySend || a.Autonomy == "observe" {
		return Composed{}, ErrSendNotAllowed
	}
	if key == "" || len(key) > 200 {
		return Composed{}, ErrSendKey
	}
	return compose(ctx, pool, a, m, &integrationSend{reviewer: r, key: "integration:" + a.ID + ":" + key})
}

// replay finds the send this key already made. The agent's lock is held, so
// two copies of one request cannot both miss it.
func (s *integrationSend) replay(ctx context.Context, tx pgx.Tx, m Message, to string, out *Composed) (bool, error) {
	var payload, recipients json.RawMessage
	err := tx.QueryRow(ctx, `SELECT a.id, a.state, a.thread_id, t.contact_id, a.payload, a.recipients
		FROM actions a JOIN threads t ON t.id = a.thread_id WHERE a.idempotency_key = $1`, s.key).
		Scan(&out.ActionID, &out.State, &out.ThreadID, &out.ContactID, &payload, &recipients)
	if db.NotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var first struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	var sentTo []struct {
		Email string `json:"email"`
		Phone string `json:"phone"`
	}
	if err := json.Unmarshal(payload, &first); err != nil {
		return false, err
	}
	if err := json.Unmarshal(recipients, &sentTo); err != nil {
		return false, err
	}
	subject := m.Subject
	if m.Channel == "sms" {
		subject = ""
	}
	if len(sentTo) != 1 || sentTo[0].Email+sentTo[0].Phone != to || first.Body != m.Body || first.Subject != subject {
		return false, ErrKeyReused
	}
	out.Decision, out.Replayed = s.decision(), true
	return true, nil
}

// lockSends serialises one integration's sends for the transaction, so the
// idempotency lookup and the hourly count see each other's work.
func lockSends(ctx context.Context, tx pgx.Tx, agentID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "integration_send:"+agentID)
	return err
}

func sendsLastHour(ctx context.Context, tx pgx.Tx, agentID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM actions WHERE requested_by = $1 AND proposal_id IS NULL
		AND created_at > now() - interval '1 hour'`, agentID).Scan(&n)
	return n, err
}

// SetMaySend grants or withdraws an integration's permission to send. It is
// for token agents only: a connection a person made through claude.ai always
// waits for approval.
func SetMaySend(ctx context.Context, q db.Querier, workspaceID, agentID, personID string, allowed bool) error {
	if !validID(agentID) {
		return ErrNotFound
	}
	tag, err := q.Exec(ctx, `UPDATE principals SET may_send = $4, may_send_by = $3, may_send_at = now()
		WHERE id = $1 AND workspace_id = $2 AND kind = 'agent' AND token_sha256 IS NOT NULL
			AND oauth_grant_id IS NULL AND revoked_at IS NULL`, agentID, workspaceID, personID, allowed)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SentMessage is where a message an integration sent has got to.
type SentMessage struct {
	ID                string    `json:"id"`
	ThreadID          string    `json:"thread_id"`
	Channel           string    `json:"channel"`
	State             string    `json:"state"`
	Reason            string    `json:"reason,omitempty"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// GetSentMessage reads one of the agent's own sends.
func GetSentMessage(ctx context.Context, q db.Querier, a Agent, id string) (SentMessage, error) {
	if !validID(id) {
		return SentMessage{}, ErrNotFound
	}
	var m SentMessage
	err := q.QueryRow(ctx, `SELECT a.id, a.thread_id, t.channel, a.state,
			coalesce((SELECT e.payload->>'reason' FROM events e WHERE e.thread_id = a.thread_id AND e.payload->>'action_id' = a.id::text
				AND e.payload ? 'reason' ORDER BY e.id DESC LIMIT 1), ''),
			coalesce((SELECT d.provider_message_id FROM delivery_attempts d WHERE d.action_id = a.id ORDER BY d.sent_at DESC LIMIT 1), ''),
			a.created_at, a.updated_at
		FROM actions a JOIN threads t ON t.id = a.thread_id
		WHERE a.id = $1 AND a.workspace_id = $2 AND a.requested_by = $3 AND a.proposal_id IS NULL`, id, a.WorkspaceID, a.ID).
		Scan(&m.ID, &m.ThreadID, &m.Channel, &m.State, &m.Reason, &m.ProviderMessageID, &m.CreatedAt, &m.UpdatedAt)
	if db.NotFound(err) {
		return m, ErrNotFound
	}
	return m, err
}
