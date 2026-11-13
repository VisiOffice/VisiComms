package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/db"
)

var (
	// ErrNoSender means the workspace has no way to send yet.
	ErrNoSender = errors.New("this workspace can't send yet: connect sending, or use the sandbox, from the setup checklist")
	// ErrNoAddress means the contact has no email address to reply to.
	ErrNoAddress = errors.New("this contact has no address for this channel")
	// ErrEmptyReply means there is nothing to send.
	ErrEmptyReply = errors.New("write a message first")
	ErrSMSLength  = errors.New("SMS replies must contain at most 1600 characters")
	ErrChatLength = errors.New("chat replies must contain at most 4000 characters")
)

// ReplyResult says what became of a person's reply.
type ReplyResult struct {
	ActionID string `json:"action_id"`
	// State is the action's state: "delivered" through the sandbox, or
	// "queued" for a real provider until the executor sends it.
	State string `json:"state"`
}

// Reply sends a person's own reply on a thread. It passes the same checks as
// an approved draft (opt-out; nobody else holding the thread) and goes out
// through the executor. Any held agent draft on the thread is voided, so it
// can't be sent after the person has answered.
func Reply(ctx context.Context, pool *pgxpool.Pool, r Reviewer, threadID string, msg EmailPayload) (ReplyResult, error) {
	msg.Subject = strings.TrimSpace(msg.Subject)
	if strings.TrimSpace(msg.Body) == "" {
		return ReplyResult{}, ErrEmptyReply
	}
	var res ReplyResult
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var contactID, subject, channel, local, remote string
		var takenOverBy *string
		err := tx.QueryRow(ctx, `SELECT contact_id, coalesce(subject, ''), taken_over_by, channel, coalesce(phone_number,''), coalesce(remote_address,'') FROM threads
			WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, threadID, r.WorkspaceID).Scan(&contactID, &subject, &takenOverBy, &channel, &local, &remote)
		if db.NotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if takenOverBy != nil && *takenOverBy != r.ID {
			return ErrConflict
		}
		if optedOut, err := recipientsOptedOut(ctx, tx, contactID, channel); err != nil {
			return err
		} else if optedOut {
			return ErrOptedOut
		}

		var to, connectorID string
		var payload, recipients []byte
		if channel == "sms" {
			if r.SMSProject == "" {
				return ErrNoSender
			}
			if utf8.RuneCountInString(msg.Body) > 1600 {
				return ErrSMSLength
			}
			to = remote
			err = tx.QueryRow(ctx, `SELECT n.connector_id FROM phone_numbers n JOIN connectors c ON c.id=n.connector_id
   WHERE n.number=$1 AND n.workspace_id=$2 AND n.provider_project_id=$3 AND c.health='ok'`, local, r.WorkspaceID, r.SMSProject).Scan(&connectorID)
			if db.NotFound(err) {
				return ErrNoSender
			}
			if err != nil {
				return err
			}
			payload, err = json.Marshal(map[string]any{"from": local, "body": msg.Body})
			recipients, _ = json.Marshal([]map[string]string{{"phone": to}})
		} else if channel == "email" {
			err = tx.QueryRow(ctx, `SELECT address FROM identities WHERE contact_id = $1 AND channel = 'email'
			ORDER BY verified DESC, created_at LIMIT 1`, contactID).Scan(&to)
			if db.NotFound(err) {
				return ErrNoAddress
			}
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, senderSQL, r.WorkspaceID, r.Provider).Scan(&connectorID)
			if db.NotFound(err) {
				return ErrNoSender
			}
			if err != nil {
				return err
			}

			if msg.Subject == "" {
				msg.Subject = replySubject(subject)
			}
			// The executor sets In-Reply-To and References from the last message
			// the customer sent, so the reply lands in their thread.
			var inReplyTo *string
			_ = tx.QueryRow(ctx, `SELECT provider_ref FROM events WHERE thread_id = $1 AND type = 'email.received'
			AND provider_ref IS NOT NULL ORDER BY id DESC LIMIT 1`, threadID).Scan(&inReplyTo)
			payload, err = json.Marshal(map[string]any{"subject": msg.Subject, "body": msg.Body, "in_reply_to": inReplyTo})
			if err != nil {
				return err
			}
			recipients, _ = json.Marshal([]map[string]string{{"email": to}})

		} else if channel == "chat" {
			// Delivered by VisiComms itself to the visitor's widget: there is no
			// provider, so no connector.
			if utf8.RuneCountInString(msg.Body) > chat.MaxMessage {
				return ErrChatLength
			}
			var visitorID string
			err = tx.QueryRow(ctx, `SELECT id FROM chat_visitors WHERE thread_id = $1`, threadID).Scan(&visitorID)
			if db.NotFound(err) {
				return ErrNoAddress
			}
			if err != nil {
				return err
			}
			msg.Subject = ""
			replies, qerr := chat.CleanQuickReplies(msg.QuickReplies)
			if qerr != nil {
				return qerr
			}
			content := map[string]any{"body": msg.Body}
			if len(replies) > 0 {
				content["quick_replies"] = replies
			}
			if msg.Via != "" {
				content["via"] = msg.Via
			}
			payload, err = json.Marshal(content)
			recipients, _ = json.Marshal([]map[string]string{{"chat_visitor": visitorID}})
		} else {
			return ErrNoAddress
		}
		if err != nil {
			return err
		}
		bound := Bound{Recipients: recipients, Payload: payload, LinkedStates: json.RawMessage("[]")}

		rows, err := tx.Query(ctx, `UPDATE proposals SET status = 'voided', decision_note = 'A person replied'
			WHERE thread_id = $1 AND status = 'pending' RETURNING id`, threadID)
		if err != nil {
			return err
		}
		voided, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range voided {
			if err := event(ctx, tx, r, threadID, "proposal.voided", map[string]any{"proposal_id": id, "reason": "A person replied"}); err != nil {
				return err
			}
		}

		err = tx.QueryRow(ctx, `INSERT INTO actions (workspace_id, thread_id, requested_by, connector_id, action,
				payload, recipients, payload_sha256, idempotency_key)
			VALUES ($1, $2, $3, nullif($4, '')::uuid, $9, $5, $6, $7, $8) RETURNING id`,
			r.WorkspaceID, threadID, r.ID, connectorID, payload, recipients, bound.Hash(), "reply:"+randomID(), channel+".send").Scan(&res.ActionID)
		if err != nil {
			return err
		}
		if err := dispatch(ctx, tx, r, res.ActionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM actions WHERE id = $1`, res.ActionID).Scan(&res.State); err != nil {
			return err
		}
		if res.State == "queued" {
			return event(ctx, tx, r, threadID, channel+".queued", map[string]any{
				"action_id": res.ActionID, "subject": msg.Subject, "text": msg.Body, "to": json.RawMessage(recipients),
			})
		}
		return nil
	})
	return res, err
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

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
