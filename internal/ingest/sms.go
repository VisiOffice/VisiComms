package ingest

import (
	"context"
	"errors"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/VisiOffice/VisiComms/internal/untrusted"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSMSMessage = errors.New("SMS requires a message SID and valid international From/To numbers")

type SMS struct {
	SID, From, To, Body, Project string
	MediaCount                   int
}

func SMSKeyword(body string) string {
	switch strings.ToUpper(strings.TrimSpace(body)) {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT", "REVOKE", "OPT OUT":
		return "stop"
	case "START", "UNSTOP":
		return "start"
	case "HELP", "INFO":
		return "help"
	}
	return ""
}

// ReceiveSMS records one provider message, its conversation and any STOP in
// one transaction. Retry dedupe is independent of the message text or date.
// PhoneContact finds the contact who texts from a number, or else who called
// from it, since the same number calling and texting is the same person. A
// number nobody has used before becomes a new contact named name. Either
// way the number ends up as that contact's texting address.
func PhoneContact(ctx context.Context, tx pgx.Tx, ws, number string, name *string) (string, error) {
	var contact string
	err := tx.QueryRow(ctx, `SELECT contact_id FROM identities WHERE workspace_id=$1 AND channel IN ('sms','voice') AND address=$2
		ORDER BY channel='sms' DESC LIMIT 1`, ws, number).Scan(&contact)
	if db.NotFound(err) {
		err = tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id,display_name) VALUES ($1,$2) RETURNING id`, ws, name).Scan(&contact)
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identities (workspace_id,contact_id,channel,address) VALUES ($1,$2,'sms',$3)
		ON CONFLICT (workspace_id,channel,address) DO NOTHING`, ws, contact, number)
	return contact, err
}

func ReceiveSMS(ctx context.Context, pool *pgxpool.Pool, in SMS) (Result, error) {
	var result Result
	var err error
	in.From, err = phone.Normalize(in.From)
	if err != nil {
		return result, ErrSMSMessage
	}
	in.To, err = phone.Normalize(in.To)
	if err != nil || in.SID == "" {
		return result, ErrSMSMessage
	}
	keyword := SMSKeyword(in.Body)
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var ws string
		err := tx.QueryRow(ctx, `SELECT workspace_id FROM phone_numbers WHERE number=$1 AND provider_project_id=$2 AND capabilities->>'sms'='true'`, in.To, in.Project).Scan(&ws)
		if db.NotFound(err) {
			return ErrUnknownRecipient
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+":sms:"+in.From); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT id,thread_id,contact_id FROM events WHERE workspace_id=$1 AND provider='signalwire_sms' AND provider_ref=$2`, ws, in.SID).Scan(&result.EventID, &result.ThreadID, &result.ContactID)
		if err == nil {
			result.Duplicate = true
			return nil
		}
		if !db.NotFound(err) {
			return err
		}
		if result.ContactID, err = PhoneContact(ctx, tx, ws, in.From, &in.From); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO threads (workspace_id,contact_id,channel,subject,phone_number,remote_address)
   VALUES ($1,$2,'sms',$3,$4,$3)
   ON CONFLICT (workspace_id,remote_address,phone_number) WHERE channel='sms'
   DO UPDATE SET state='open',version=threads.version+1,updated_at=now() RETURNING id`, ws, result.ContactID, in.From, in.To).Scan(&result.ThreadID)
		if err != nil {
			return err
		}
		if keyword == "stop" {
			if _, err := tx.Exec(ctx, `INSERT INTO opt_outs (contact_id,channel,source) VALUES ($1,'sms','inbound SMS STOP') ON CONFLICT DO NOTHING`, result.ContactID); err != nil {
				return err
			}
			// Cancel every queued SMS for this contact, including other local numbers.
			if _, err := tx.Exec(ctx, `WITH cancelled AS (
    UPDATE actions a SET state='cancelled',updated_at=now() FROM threads t
    WHERE a.thread_id=t.id AND t.contact_id=$1 AND a.workspace_id=$2 AND a.action='sms.send' AND a.state='queued'
    RETURNING a.id,a.thread_id,a.workspace_id,a.requested_by)
    INSERT INTO events (workspace_id,thread_id,contact_id,type,actor_id,channel,payload)
    SELECT workspace_id,thread_id,$1,'sms.cancelled',requested_by,'sms',jsonb_build_object('action_id',id,'reason','The contact opted out of SMS.') FROM cancelled`, result.ContactID, ws); err != nil {
				return err
			}
		} else if keyword == "start" {
			// A customer can undo a keyword STOP, never a block imposed by a human.
			if _, err := tx.Exec(ctx, `DELETE FROM opt_outs WHERE contact_id=$1 AND channel='sms' AND source='inbound SMS STOP'`, result.ContactID); err != nil {
				return err
			}
		}
		suspicious := untrusted.SuspiciousReasons(in.Body, "")
		kind := "message"
		if keyword != "" {
			kind = "control"
		}
		payload := map[string]any{"text": in.Body, "from": in.From, "to": in.To, "keyword": keyword, "media_count": in.MediaCount, "classification": map[string]any{"kind": kind, "opt_out": keyword == "stop"}}
		return tx.QueryRow(ctx, `INSERT INTO events (workspace_id,thread_id,contact_id,type,channel,payload,provider,provider_ref,untrusted,suspected_injection)
   VALUES ($1,$2,$3,'sms.received','sms',$4,'signalwire_sms',$5,true,$6) RETURNING id`, ws, result.ThreadID, result.ContactID, payload, in.SID, len(suspicious) > 0).Scan(&result.EventID)
	})
	return result, err
}
