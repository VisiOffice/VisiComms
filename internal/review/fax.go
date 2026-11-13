package review

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrFaxDocument = errors.New("choose a PDF document from this workspace")

type FaxRequest struct {
	From       string `json:"from"`
	To         string `json:"to"`
	DocumentID string `json:"document_id"`
	RequestID  string `json:"request_id,omitempty"`
}
type FaxResult struct {
	ActionID string `json:"action_id"`
	ThreadID string `json:"thread_id"`
	State    string `json:"state"`
}

// SendFax queues a human's explicit send of an immutable PDF. The same route
// can start a new conversation or reply using its original phone-number pair.
func SendFax(ctx context.Context, pool *pgxpool.Pool, r Reviewer, req FaxRequest) (FaxResult, error) {
	var out FaxResult
	var err error
	req.From, err = phone.Normalize(req.From)
	if err != nil {
		return out, err
	}
	req.To, err = phone.Normalize(req.To)
	if err != nil {
		return out, err
	}
	if r.SMSProject == "" {
		return out, ErrNoSender
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		key := "fax:" + randomID()
		if req.RequestID != "" {
			key = "fax:" + r.WorkspaceID + ":" + req.RequestID
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
				return err
			}
			var payload, recipients []byte
			err := tx.QueryRow(ctx, `SELECT id,thread_id,state,payload,recipients FROM actions WHERE workspace_id=$1 AND idempotency_key=$2`, r.WorkspaceID, key).Scan(&out.ActionID, &out.ThreadID, &out.State, &payload, &recipients)
			if err == nil {
				var previous map[string]string
				var to []map[string]string
				if json.Unmarshal(payload, &previous) != nil || json.Unmarshal(recipients, &to) != nil || len(to) != 1 || previous["from"] != req.From || previous["document_id"] != req.DocumentID || to[0]["phone"] != req.To {
					return ErrConflict
				}
				return nil
			}
			if !db.NotFound(err) {
				return err
			}
		}
		var connector string
		err := tx.QueryRow(ctx, `SELECT n.connector_id FROM phone_numbers n JOIN connectors c ON c.id=n.connector_id
   WHERE n.number=$1 AND n.workspace_id=$2 AND n.provider_project_id=$3 AND n.capabilities->>'fax'='true' AND c.health='ok'`, req.From, r.WorkspaceID, r.SMSProject).Scan(&connector)
		if db.NotFound(err) {
			return ErrNoSender
		}
		if err != nil {
			return err
		}
		doc, err := files.Load(ctx, tx, r.WorkspaceID, req.DocumentID)
		if db.NotFound(err) {
			return ErrFaxDocument
		}
		if err != nil {
			return err
		}
		conversation, err := ingest.FaxConversation(ctx, tx, r.WorkspaceID, req.To, req.From)
		if err != nil {
			return err
		}
		out.ThreadID = conversation.ThreadID
		var takenOver *string
		if err := tx.QueryRow(ctx, `SELECT taken_over_by FROM threads WHERE id=$1 FOR UPDATE`, out.ThreadID).Scan(&takenOver); err != nil {
			return err
		}
		if takenOver != nil && *takenOver != r.ID {
			return ErrConflict
		}
		if blocked, err := recipientsOptedOut(ctx, tx, conversation.ContactID, "fax"); err != nil {
			return err
		} else if blocked {
			return ErrOptedOut
		}
		payload, _ := json.Marshal(map[string]string{"from": req.From, "document_id": doc.ID, "sha256": doc.SHA256})
		recipients, _ := json.Marshal([]map[string]string{{"phone": req.To}})
		bound := Bound{Payload: payload, Recipients: recipients, LinkedStates: json.RawMessage(`[]`)}
		err = tx.QueryRow(ctx, `INSERT INTO actions(workspace_id,thread_id,requested_by,connector_id,action,payload,recipients,payload_sha256,idempotency_key)
   VALUES($1,$2,$3,$4,'fax.send',$5,$6,$7,$8) RETURNING id`, r.WorkspaceID, out.ThreadID, r.ID, connector, payload, recipients, bound.Hash(), key).Scan(&out.ActionID)
		if err != nil {
			return err
		}
		if err := event(ctx, tx, r, out.ThreadID, "fax.queued", map[string]any{"action_id": out.ActionID, "document_id": doc.ID, "filename": doc.Filename, "sha256": doc.SHA256, "from": req.From, "to": req.To}); err != nil {
			return err
		}
		if r.Enqueue != nil {
			if err := r.Enqueue(ctx, tx, out.ActionID); err != nil {
				return err
			}
		}
		out.State = "queued"
		return nil
	})
	return out, err
}
