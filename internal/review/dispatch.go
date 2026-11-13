package review

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/chat"
)

// dispatch hands an approved or allowed action to the executor. The sandbox
// connector completes here: it records the send as delivered and contacts
// nobody, which is what demo workspaces use. A real provider's action is
// queued for internal/sending, which re-checks it and sends it exactly once.
func dispatch(ctx context.Context, tx pgx.Tx, r Reviewer, actionID string) error {
	var kind *string
	var threadID, agentID, action string
	var payload, recipients json.RawMessage
	err := tx.QueryRow(ctx, `SELECT c.kind, a.thread_id, a.requested_by, a.payload, a.recipients, a.action
		FROM actions a LEFT JOIN connectors c ON c.id = a.connector_id WHERE a.id = $1`, actionID).
		Scan(&kind, &threadID, &agentID, &payload, &recipients, &action)
	if err != nil {
		return err
	}
	if action == "chat.send" {
		return deliverChat(ctx, tx, r, actionID, threadID, agentID, payload)
	}
	if kind == nil {
		return nil
	}
	if *kind != "sandbox" {
		// A real provider: the executor sends it after this transaction
		// commits, re-checking everything first.
		if r.Enqueue != nil {
			return r.Enqueue(ctx, tx, actionID)
		}
		return nil
	}

	if _, err := tx.Exec(ctx, `INSERT INTO delivery_attempts (action_id, connector_id, provider_message_id, outcome, provider_response)
		SELECT id, connector_id, 'sandbox-' || id, 'accepted', '{"sandbox": true}' FROM actions WHERE id = $1`, actionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE actions SET state = 'delivered', updated_at = now() WHERE id = $1`, actionID); err != nil {
		return err
	}
	var email EmailPayload
	_ = json.Unmarshal(payload, &email)
	if _, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		SELECT workspace_id, id, contact_id, 'email.sent', $2, channel, $3 FROM threads WHERE id = $1`,
		threadID, agentID, map[string]any{
			"action_id": actionID, "subject": email.Subject, "text": email.Body,
			"to": recipients, "sandbox": true, "approved_by": r.ID,
		}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE threads SET state = 'waiting', version = version + 1, updated_at = now() WHERE id = $1`, threadID)
	return err
}

// deliverChat completes a chat reply in the deciding transaction: there is no
// provider, the message is the event the visitor's widget reads. The widget's
// stream is woken when this transaction commits. The visitor sees a person's
// name, or the widget's name for an agent's reply.
func deliverChat(ctx context.Context, tx pgx.Tx, r Reviewer, actionID, threadID, requestedBy string, payload json.RawMessage) error {
	var body struct {
		Body         string   `json:"body"`
		QuickReplies []string `json:"quick_replies"`
		Via          string   `json:"via"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return err
	}
	var sender string
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN p.kind = 'human' THEN split_part(p.name, ' ', 1) ELSE w.name END
		FROM principals p, chat_visitors v JOIN chat_widgets w ON w.id = v.widget_id
		WHERE p.id = $1 AND v.thread_id = $2`, requestedBy, threadID).Scan(&sender); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delivery_attempts (action_id, provider_message_id, outcome, provider_response)
		VALUES ($1::uuid, 'chat-' || $1::text, 'accepted', '{"chat": true}')`, actionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE actions SET state = 'delivered', updated_at = now() WHERE id = $1`, actionID); err != nil {
		return err
	}
	ev := map[string]any{"action_id": actionID, "text": body.Body, "sender": sender}
	if len(body.QuickReplies) > 0 {
		ev["quick_replies"] = body.QuickReplies
	}
	if body.Via != "" {
		ev["via"] = body.Via
	}
	if r.ID != requestedBy {
		ev["approved_by"] = r.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		SELECT workspace_id, id, contact_id, 'chat.sent', $2, channel, $3 FROM threads WHERE id = $1`,
		threadID, requestedBy, ev); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE threads SET state = 'waiting', version = version + 1, updated_at = now() WHERE id = $1`, threadID); err != nil {
		return err
	}
	return chat.Notify(ctx, tx, threadID)
}
