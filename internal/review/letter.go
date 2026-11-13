package review

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/postal"
)

var (
	ErrNoMail        = errors.New("connect a Lob account under Connections → Mail before sending letters")
	ErrMailAddress   = errors.New("choose one of the contact's mailing addresses that letters can reach")
	ErrLetterPages   = errors.New("Lob takes at most 60 pages, or 120 printed on both sides")
	ErrOverMailLimit = errors.New("this letter would pass the workspace's monthly mail limit; raise it under Connections → Mail")
)

// LetterRequest is a person sending a PDF as a letter.
type LetterRequest struct {
	ContactID  string `json:"contact_id"`
	AddressID  string `json:"address_id"`
	DocumentID string `json:"document_id"`
	postal.Options
	RequestID string `json:"request_id,omitempty"`
}

type LetterResult struct {
	ActionID string `json:"action_id"`
	ThreadID string `json:"thread_id"`
	State    string `json:"state"`
	postal.Estimate
}

// EstimateLetter prices a stored PDF as a letter with the given options.
func EstimateLetter(ctx context.Context, q db.Querier, workspaceID, documentID string, o postal.Options) (postal.Estimate, error) {
	if err := o.Check(); err != nil {
		return postal.Estimate{}, err
	}
	doc, err := files.Load(ctx, q, workspaceID, documentID)
	if db.NotFound(err) {
		return postal.Estimate{}, ErrFaxDocument
	}
	if err != nil {
		return postal.Estimate{}, err
	}
	return o.Estimate(postal.Pages(doc.Content)), nil
}

// SendLetter queues a person's explicit send of a PDF to one of a contact's
// mailing addresses. Their click is the approval; the executor re-checks
// everything before Lob is called.
func SendLetter(ctx context.Context, pool *pgxpool.Pool, r Reviewer, req LetterRequest) (LetterResult, error) {
	var out LetterResult
	if err := req.Options.Check(); err != nil {
		return out, err
	}
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		key := "letter:" + randomID()
		if req.RequestID != "" {
			key = "letter:" + r.WorkspaceID + ":" + req.RequestID
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
				return err
			}
			var payload []byte
			err := tx.QueryRow(ctx, `SELECT id, thread_id, state, payload FROM actions WHERE workspace_id = $1 AND idempotency_key = $2`, r.WorkspaceID, key).
				Scan(&out.ActionID, &out.ThreadID, &out.State, &payload)
			if err == nil {
				var previous postal.LetterPayload
				if json.Unmarshal(payload, &previous) != nil || previous.DocumentID != req.DocumentID || previous.Options != req.Options {
					return ErrConflict
				}
				out.Estimate = postal.Estimate{Pages: previous.Pages, CostCents: previous.CostCents}
				return nil
			}
			if !db.NotFound(err) {
				return err
			}
		}
		var connector string
		var settings map[string]string
		err := tx.QueryRow(ctx, `SELECT id, settings FROM connectors WHERE workspace_id = $1 AND kind = 'lob' AND health = 'ok'`, r.WorkspaceID).Scan(&connector, &settings)
		if db.NotFound(err) {
			return ErrNoMail
		}
		if err != nil {
			return err
		}
		address, err := postal.Get(ctx, tx, r.WorkspaceID, req.ContactID, req.AddressID)
		if db.NotFound(err) || (err == nil && !address.Deliverable()) {
			return ErrMailAddress
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
		pages := postal.Pages(doc.Content)
		if pages > postal.MaxPages(req.DoubleSided) {
			return ErrLetterPages
		}
		out.Estimate = req.Options.Estimate(pages)
		limit, _ := strconv.Atoi(settings["cap_cents"])
		spent, err := postal.MonthSpend(ctx, tx, r.WorkspaceID, "")
		if err != nil {
			return err
		}
		if spent+out.CostCents > limit {
			return ErrOverMailLimit
		}
		out.ThreadID, err = letterThread(ctx, tx, r.WorkspaceID, req.ContactID, address.OneLine())
		if err != nil {
			return err
		}
		var contact string
		var takenOver *string
		if err := tx.QueryRow(ctx, `SELECT contact_id, taken_over_by FROM threads WHERE id = $1 FOR UPDATE`, out.ThreadID).Scan(&contact, &takenOver); err != nil {
			return err
		}
		if takenOver != nil && *takenOver != r.ID {
			return ErrConflict
		}
		if blocked, err := recipientsOptedOut(ctx, tx, contact, "postal"); err != nil {
			return err
		} else if blocked {
			return ErrOptedOut
		}
		payload, _ := json.Marshal(postal.LetterPayload{DocumentID: doc.ID, SHA256: doc.SHA256, From: settings["from_address"],
			Options: req.Options, Pages: out.Pages, CostCents: out.CostCents})
		recipients, _ := json.Marshal([]postal.LetterRecipient{{IdentityID: address.IdentityID, Name: address.Name, Address: address.OneLine()}})
		bound := Bound{Payload: payload, Recipients: recipients, LinkedStates: json.RawMessage(`[]`)}
		err = tx.QueryRow(ctx, `INSERT INTO actions (workspace_id, thread_id, requested_by, connector_id, action, payload, recipients, payload_sha256, idempotency_key)
			VALUES ($1, $2, $3, $4, 'postal.send', $5, $6, $7, $8) RETURNING id`,
			r.WorkspaceID, out.ThreadID, r.ID, connector, payload, recipients, bound.Hash(), key).Scan(&out.ActionID)
		if err != nil {
			return err
		}
		if err := event(ctx, tx, r, out.ThreadID, "postal.queued", map[string]any{
			"action_id": out.ActionID, "document_id": doc.ID, "filename": doc.Filename, "sha256": doc.SHA256,
			"name": address.Name, "address": address.OneLine(), "pages": out.Pages, "cost_cents": out.CostCents,
			"extra_service": req.ExtraService, "color": req.Color, "double_sided": req.DoubleSided, "test": settings["mode"] == "test",
		}); err != nil {
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

// letterThread is the contact's one postal thread, pointed at the address of
// its latest letter.
func letterThread(ctx context.Context, tx pgx.Tx, workspaceID, contactID, address string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject, remote_address)
		VALUES ($1, $2, 'postal', 'Letters', $3)
		ON CONFLICT (workspace_id, contact_id) WHERE channel = 'postal'
		DO UPDATE SET remote_address = EXCLUDED.remote_address, state = 'open', version = threads.version + 1, updated_at = now()
		RETURNING id`, workspaceID, contactID, address).Scan(&id)
	return id, err
}

// Letter is a sent letter that can still be cancelled at Lob.
type Letter struct {
	ActionID, ThreadID, ConnectorID, LobID string
}

// ErrNotCancellable means the letter was never accepted by Lob, or is
// already cancelled or finished.
var ErrNotCancellable = errors.New("this letter can no longer be cancelled")

// CancellableLetter finds a workspace's letter that Lob accepted.
func CancellableLetter(ctx context.Context, q db.Querier, workspaceID, actionID string) (Letter, error) {
	l := Letter{ActionID: actionID}
	err := q.QueryRow(ctx, `SELECT a.thread_id, a.connector_id, d.provider_message_id FROM actions a
		JOIN delivery_attempts d ON d.action_id = a.id AND d.outcome = 'accepted'
		WHERE a.workspace_id = $1 AND a.id::text = $2 AND a.action = 'postal.send' AND a.state IN ('accepted', 'unknown')
		ORDER BY d.sent_at DESC LIMIT 1`, workspaceID, actionID).Scan(&l.ThreadID, &l.ConnectorID, &l.LobID)
	if db.NotFound(err) {
		return l, ErrNotCancellable
	}
	return l, err
}

// LetterCancelled records that Lob cancelled the letter at a person's
// request. Lob does not charge for it.
func LetterCancelled(ctx context.Context, pool *pgxpool.Pool, r Reviewer, l Letter) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE actions SET state = 'cancelled', updated_at = now() WHERE id::text = $1 AND state IN ('accepted', 'unknown')`, l.ActionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return event(ctx, tx, r, l.ThreadID, "postal.cancelled", map[string]any{"action_id": l.ActionID, "provider_message_id": l.LobID,
			"reason": "Cancelled before printing."})
	})
}

// SaveAddress adds or updates a contact's mailing address, as checked by
// Lob, for a person.
func SaveAddress(ctx context.Context, pool *pgxpool.Pool, r Reviewer, contactID string, a postal.Address, deliverability string) (postal.Saved, error) {
	var saved postal.Saved
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contacts WHERE workspace_id = $1 AND id::text = $2)`, r.WorkspaceID, contactID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		var err error
		saved, err = postal.Save(ctx, tx, r.WorkspaceID, contactID, a, deliverability)
		return err
	})
	return saved, err
}
