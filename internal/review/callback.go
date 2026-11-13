package review

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// TextBackResult says where a text sent to a caller went.
type TextBackResult struct {
	ReplyResult
	// SMSThreadID is the text conversation with the caller on the number
	// they called. Their answer arrives there.
	SMSThreadID string `json:"sms_thread_id"`
}

// TextBack answers a call by texting the caller from the number they
// called. The text goes out on the SMS conversation with that caller, which
// is where their answer lands, through Reply and all of its checks (opt-out,
// a connected number, nobody else holding the thread). The call's thread
// records that it was answered by text.
func TextBack(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID, body string) (TextBackResult, error) {
	var res TextBackResult
	if strings.TrimSpace(body) == "" {
		return res, ErrEmptyReply
	}
	if utf8.RuneCountInString(body) > 1600 {
		return res, ErrSMSLength
	}
	local, remote, err := callerOf(ctx, pool, r, threadID)
	if err != nil {
		return res, err
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var callContact string
		if err := tx.QueryRow(ctx, `SELECT contact_id FROM threads WHERE id = $1`, threadID).Scan(&callContact); err != nil {
			return err
		}
		// The caller's number becomes a texting address of the same contact,
		// unless someone already texts from it.
		if _, err := tx.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'sms', $3)
			ON CONFLICT (workspace_id, channel, address) DO NOTHING`, r.WorkspaceID, callContact, remote); err != nil {
			return err
		}
		var contact string
		if err := tx.QueryRow(ctx, `SELECT contact_id FROM identities WHERE workspace_id = $1 AND channel = 'sms' AND address = $2`, r.WorkspaceID, remote).Scan(&contact); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject, phone_number, remote_address)
			VALUES ($1, $2, 'sms', $3, $4, $3)
			ON CONFLICT (workspace_id, remote_address, phone_number) WHERE channel = 'sms'
			DO UPDATE SET updated_at = now() RETURNING id`, r.WorkspaceID, contact, remote, local).Scan(&res.SMSThreadID)
	})
	if err != nil {
		return res, err
	}
	if res.ReplyResult, err = Reply(ctx, pool, r, res.SMSThreadID, EmailPayload{Body: body}); err != nil {
		return res, err
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		return event(ctx, tx, r, threadID, "call.texted_back", map[string]any{
			"from": local, "to": remote, "text": body, "sms_thread_id": res.SMSThreadID, "action_id": res.ActionID,
		})
	})
	return res, err
}

// CallBack is the number a person calls a caller back on, and from: the
// caller's number, from the one they called. It records the attempt on the
// call's thread; the console then places the call from its browser phone.
func CallBack(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) (from, to string, err error) {
	from, to, err = callerOf(ctx, pool, r, threadID)
	if err != nil {
		return "", "", err
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		return event(ctx, tx, r, threadID, "call.called_back", map[string]any{"from": from, "to": to})
	})
	return from, to, err
}

// callerOf loads a call thread's two numbers: ours, and the caller's.
func callerOf(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string) (local, remote string, err error) {
	var channel string
	var takenOverBy *string
	// An entrance's caller ID is its intercom bridge, which cannot read a text.
	err = pool.QueryRow(ctx, `SELECT channel, coalesce(phone_number, ''),
			CASE WHEN EXISTS (SELECT 1 FROM entrances e WHERE e.workspace_id = t.workspace_id AND e.caller_id = t.remote_address) THEN '' ELSE coalesce(remote_address, '') END,
			taken_over_by FROM threads t
		WHERE id = $1 AND workspace_id = $2`, threadID, r.WorkspaceID).Scan(&channel, &local, &remote, &takenOverBy)
	if db.NotFound(err) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if takenOverBy != nil && *takenOverBy != r.ID {
		return "", "", ErrConflict
	}
	if channel != "voice" || local == "" || !strings.HasPrefix(remote, "+") {
		return "", "", ErrNoAddress
	}
	return local, remote, nil
}
