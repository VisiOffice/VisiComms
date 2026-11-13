package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrFax = errors.New("fax requires a SID, valid international numbers and a received or failed status")

type Fax struct {
	SID, From, To, Project, Status, ErrorCode string
	Pages                                     int
	PDF                                       []byte
}

// FaxConversation is shared by inbound faxes and human-initiated sends. Phone
// identities are channel-specific and never silently merged with SMS or email.
func FaxConversation(ctx context.Context, tx pgx.Tx, ws, remote, local string) (Result, error) {
	var result Result
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+":fax:"+remote); err != nil {
		return result, err
	}
	err := tx.QueryRow(ctx, `SELECT contact_id FROM identities WHERE workspace_id=$1 AND channel='fax' AND address=$2`, ws, remote).Scan(&result.ContactID)
	if db.NotFound(err) {
		if err := tx.QueryRow(ctx, `INSERT INTO contacts(workspace_id,display_name) VALUES($1,$2) RETURNING id`, ws, remote).Scan(&result.ContactID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identities(workspace_id,contact_id,channel,address) VALUES($1,$2,'fax',$3)`, ws, result.ContactID, remote); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO threads(workspace_id,contact_id,channel,subject,phone_number,remote_address)
  VALUES($1,$2,'fax',$3,$4,$3)
  ON CONFLICT(workspace_id,remote_address,phone_number) WHERE channel='fax'
  DO UPDATE SET state='open',version=threads.version+1,updated_at=now() RETURNING id`, ws, result.ContactID, remote, local).Scan(&result.ThreadID)
	return result, err
}

// ReceiveFax stores the document and receipt atomically. A failed reception is
// visible in the same conversation but never creates an empty PDF document.
func ReceiveFax(ctx context.Context, pool *pgxpool.Pool, in Fax) (Result, error) {
	var result Result
	var err error
	in.From, err = phone.Normalize(in.From)
	if err != nil {
		return result, ErrFax
	}
	in.To, err = phone.Normalize(in.To)
	if err != nil || in.SID == "" || (in.Status != "received" && in.Status != "failed") {
		return result, ErrFax
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var ws string
		err := tx.QueryRow(ctx, `SELECT workspace_id FROM phone_numbers WHERE number=$1 AND provider_project_id=$2 AND capabilities->>'fax'='true'`, in.To, in.Project).Scan(&ws)
		if db.NotFound(err) {
			return ErrUnknownRecipient
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+":fax-receipt:"+in.SID); err != nil {
			return err
		}
		ref := in.SID + ":" + in.Status
		err = tx.QueryRow(ctx, `SELECT id,thread_id,contact_id FROM events WHERE workspace_id=$1 AND provider='signalwire_fax_in' AND provider_ref=$2`, ws, ref).Scan(&result.EventID, &result.ThreadID, &result.ContactID)
		if err == nil {
			result.Duplicate = true
			return nil
		}
		if !db.NotFound(err) {
			return err
		}
		result, err = FaxConversation(ctx, tx, ws, in.From, in.To)
		if err != nil {
			return err
		}
		typ := "fax.receive_failed"
		payload := map[string]any{"from": in.From, "to": in.To, "pages": in.Pages, "reason": in.ErrorCode}
		if in.Status == "received" {
			doc, err := files.Store(ctx, tx, ws, "received-fax.pdf", in.PDF)
			if err != nil {
				return err
			}
			typ = "fax.received"
			payload["document_id"], payload["sha256"], payload["filename"] = doc.ID, doc.SHA256, doc.Filename
			payload["text"] = fmt.Sprintf("Received fax (%d pages)", in.Pages)
			payload["classification"] = map[string]string{"kind": "message"}
		}
		return tx.QueryRow(ctx, `INSERT INTO events(workspace_id,thread_id,contact_id,type,channel,payload,provider,provider_ref,untrusted)
   VALUES($1,$2,$3,$4,'fax',$5,'signalwire_fax_in',$6,true) RETURNING id`, ws, result.ThreadID, result.ContactID, typ, payload, ref).Scan(&result.EventID)
	})
	return result, err
}
