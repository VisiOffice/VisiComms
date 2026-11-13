package review

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// SendAllowed turns a message policy allowed without a person into an action,
// inside the caller's transaction. r is the integration's own principal. The
// action goes through the executor like any other, which checks opt-out,
// take-over and the workspace's pause again just before sending.
func SendAllowed(ctx context.Context, tx pgx.Tx, r Reviewer, threadID, channel string, payload, recipients json.RawMessage, key string) (ReplyResult, error) {
	var res ReplyResult
	var connectorID string
	var err error
	switch channel {
	case "email":
		err = tx.QueryRow(ctx, senderSQL, r.WorkspaceID, r.Provider).Scan(&connectorID)
	case "sms":
		if r.SMSProject == "" {
			return res, ErrNoSender
		}
		err = tx.QueryRow(ctx, `SELECT n.connector_id FROM phone_numbers n JOIN threads t ON t.phone_number = n.number
			JOIN connectors c ON c.id = n.connector_id
			WHERE t.id = $1 AND n.workspace_id = $2 AND n.provider_project_id = $3 AND c.health = 'ok'`,
			threadID, r.WorkspaceID, r.SMSProject).Scan(&connectorID)
	default:
		return res, ErrNoAddress
	}
	if db.NotFound(err) {
		return res, ErrNoSender
	}
	if err != nil {
		return res, err
	}
	bound := Bound{Recipients: recipients, Payload: payload, LinkedStates: json.RawMessage("[]")}
	if err := tx.QueryRow(ctx, `INSERT INTO actions (workspace_id, thread_id, requested_by, connector_id, action,
			payload, recipients, payload_sha256, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		r.WorkspaceID, threadID, r.ID, connectorID, channel+".send", payload, recipients, bound.Hash(), key).Scan(&res.ActionID); err != nil {
		return res, err
	}
	if err := event(ctx, tx, r, threadID, "action.allowed", map[string]any{"action_id": res.ActionID, "rule": "send_permission"}); err != nil {
		return res, err
	}
	if err := dispatch(ctx, tx, r, res.ActionID); err != nil {
		return res, err
	}
	if err := tx.QueryRow(ctx, `SELECT state FROM actions WHERE id = $1`, res.ActionID).Scan(&res.State); err != nil {
		return res, err
	}
	if res.State != "queued" {
		return res, nil
	}
	var content EmailPayload
	_ = json.Unmarshal(payload, &content)
	return res, event(ctx, tx, r, threadID, channel+".queued", map[string]any{
		"action_id": res.ActionID, "subject": content.Subject, "text": content.Body, "to": recipients,
	})
}
