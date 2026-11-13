// Package ingest turns provider webhooks into events and current state, in
// one transaction each.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/email"
	"github.com/VisiOffice/VisiComms/internal/untrusted"
)

const defaultProvider = "cloudflare_email"

// ErrUnknownRecipient means no workspace receives mail at the To address.
var ErrUnknownRecipient = errors.New("no workspace receives mail at this address")

// ErrNoSender means the email had no usable sender address.
var ErrNoSender = errors.New("email has no sender address")

// Result is what storing one inbound email did.
type Result struct {
	EventID   int64
	ThreadID  string
	ContactID string
	// Duplicate is set when this Message-ID was already stored; nothing changed.
	Duplicate      bool
	Classification email.Classification
	Suspicious     []string
}

// Workspace resolves the workspace that receives mail at to.
func Workspace(ctx context.Context, q db.Querier, to string) (string, error) {
	addr := email.ParseAddress(to).Email
	var id string
	err := q.QueryRow(ctx, `SELECT workspace_id FROM inbound_addresses WHERE address = $1`, addr).Scan(&id)
	if db.NotFound(err) {
		return "", ErrUnknownRecipient
	}
	return id, err
}

// Email stores one inbound email: the contact and identity, the thread, the
// event and any opt-out, all in one transaction. A redelivery of the same
// Message-ID changes nothing.
func Email(ctx context.Context, pool *pgxpool.Pool, workspaceID string, in email.Inbound) (Result, error) {
	provider := in.Provider
	if provider == "" {
		provider = defaultProvider
	}
	if in.Contact.Email == "" {
		return Result{}, ErrNoSender
	}
	res := Result{
		Classification: email.Classify(in),
		Suspicious:     untrusted.SuspiciousReasons(in.Text, in.HTML),
	}
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		// Serialise everything from one sender, so concurrent deliveries can't
		// create two contacts or two threads, or store one message twice.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			workspaceID+":"+in.Contact.Email); err != nil {
			return err
		}
		if in.MessageID != "" {
			err := tx.QueryRow(ctx, `SELECT id, thread_id, contact_id FROM events
				WHERE workspace_id = $1 AND provider = $2 AND provider_ref = $3`,
				workspaceID, provider, in.MessageID).Scan(&res.EventID, &res.ThreadID, &res.ContactID)
			if err == nil {
				res.Duplicate = true
				return nil
			}
			if !db.NotFound(err) {
				return err
			}
		}

		contactID, err := findOrCreateContact(ctx, tx, workspaceID, in.Contact)
		if err != nil {
			return err
		}
		res.ContactID = contactID

		threadID, err := findOrCreateThread(ctx, tx, workspaceID, contactID, in)
		if err != nil {
			return err
		}
		res.ThreadID = threadID

		if res.Classification.OptOut {
			if _, err := tx.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source)
				VALUES ($1, 'email', 'inbound email') ON CONFLICT DO NOTHING`, contactID); err != nil {
				return err
			}
		}

		var messageID *string
		if in.MessageID != "" {
			messageID = &in.MessageID
		}
		return tx.QueryRow(ctx, `INSERT INTO events
			(workspace_id, thread_id, contact_id, type, channel, payload, provider, provider_ref,
			 untrusted, suspected_injection, occurred_at)
			VALUES ($1, $2, $3, 'email.received', 'email', $4, $5, $6, true, $7, $8)
			RETURNING id`,
			workspaceID, threadID, contactID, payload(in, res), provider, messageID,
			len(res.Suspicious) > 0, in.Date).Scan(&res.EventID)
	})
	return res, err
}

func payload(in email.Inbound, res Result) map[string]any {
	p := map[string]any{
		"message_id":     in.MessageID,
		"to":             in.To,
		"from":           in.From,
		"subject":        in.Subject,
		"text":           in.Text,
		"html":           in.HTML,
		"references":     in.References,
		"headers":        in.Headers,
		"attachments":    in.Attachments,
		"classification": res.Classification,
	}
	if in.ReplyTo != nil {
		p["reply_to"] = in.ReplyTo
	}
	if len(res.Suspicious) > 0 {
		p["suspicious_reasons"] = res.Suspicious
	}
	return p
}

func findOrCreateContact(ctx context.Context, tx pgx.Tx, workspaceID string, a email.Address) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT contact_id FROM identities
		WHERE workspace_id = $1 AND channel = 'email' AND address = $2`, workspaceID, a.Email).Scan(&id)
	if err == nil || !db.NotFound(err) {
		return id, err
	}
	var name *string
	if a.Name != "" {
		name = &a.Name
	}
	if err := tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`,
		workspaceID, name).Scan(&id); err != nil {
		return "", err
	}
	// Receiving mail from an address does not verify it: From is easy to forge.
	_, err = tx.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address)
		VALUES ($1, $2, 'email', $3)`, workspaceID, id, a.Email)
	return id, err
}

// findOrCreateThread matches, in order: any Message-ID this email replies to
// that we received or sent; an unresolved email thread with the same contact
// active in the last 24 hours; else a new thread. A reply reopens a resolved
// thread, and every new message bumps the thread version.
func findOrCreateThread(ctx context.Context, tx pgx.Tx, workspaceID, contactID string, in email.Inbound) (string, error) {
	var id string
	if refs := referenceIDs(in.References); len(refs) > 0 {
		err := tx.QueryRow(ctx, `SELECT thread_id FROM (
				SELECT thread_id, recorded_at AS at FROM events
				WHERE workspace_id = $1 AND (provider_ref = ANY($2) OR payload->>'message_id' = ANY($2)) AND thread_id IS NOT NULL
				UNION ALL
				SELECT a.thread_id, d.sent_at FROM delivery_attempts d JOIN actions a ON a.id = d.action_id
				WHERE a.workspace_id = $1 AND d.provider_message_id = ANY($2) AND a.thread_id IS NOT NULL
			) m ORDER BY at DESC LIMIT 1`, workspaceID, refs).Scan(&id)
		if err == nil {
			return id, touchThread(ctx, tx, id)
		}
		if !db.NotFound(err) {
			return "", err
		}
	}

	err := tx.QueryRow(ctx, `SELECT id FROM threads
		WHERE workspace_id = $1 AND contact_id = $2 AND channel = 'email' AND state <> 'resolved'
		  AND updated_at > now() - interval '24 hours'
		ORDER BY updated_at DESC LIMIT 1`, workspaceID, contactID).Scan(&id)
	if err == nil {
		return id, touchThread(ctx, tx, id)
	}
	if !db.NotFound(err) {
		return "", err
	}

	var subject *string
	if in.Subject != "" {
		subject = &in.Subject
	}
	err = tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject)
		VALUES ($1, $2, 'email', $3) RETURNING id`, workspaceID, contactID, subject).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("creating thread: %w", err)
	}
	return id, nil
}

func touchThread(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE threads SET version = version + 1, updated_at = now(),
		state = CASE WHEN state = 'resolved' THEN 'open' ELSE state END WHERE id = $1`, id)
	return err
}

// referenceIDs is each reference as given, plus, for a reply to one of our
// SES sends, the bare SES message id: the customer replies to
// <id@email.amazonses.com>, and SES reported <id> when it accepted the send.
func referenceIDs(refs []string) []string {
	out := append([]string{}, refs...)
	for _, r := range refs {
		local, host, ok := strings.Cut(strings.Trim(r, "<>"), "@")
		if ok && strings.HasSuffix(host, "amazonses.com") {
			out = append(out, local)
		}
	}
	return out
}
