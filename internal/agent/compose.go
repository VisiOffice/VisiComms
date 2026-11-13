package agent

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/phone"
)

var (
	ErrComposeChannel = errors.New("channel must be email or sms")
	ErrComposeTo      = errors.New("to must be an email address (for email) or an international phone number beginning with + (for sms)")
	ErrComposeSubject = errors.New("an email needs a subject")
	// ErrComposeFrom is an SMS whose sending number is missing or not the
	// workspace's.
	ErrComposeFrom = errors.New("from must be one of this workspace's texting numbers")
)

// Message is a new email or text to someone who has not written in.
type Message struct {
	Channel string `json:"channel"`
	To      string `json:"to"`
	// Name is the contact's name, used when they are new.
	Name string `json:"name"`
	// From is the workspace number a text goes from; it may be left out
	// when the workspace has one.
	From string `json:"from"`
	Draft
}

// Composed is what became of a new message.
type Composed struct {
	Decision
	ThreadID  string `json:"thread_id,omitempty"`
	ContactID string `json:"contact_id,omitempty"`
	// ActionID and State are set when an integration's message was sent.
	ActionID string `json:"message_id,omitempty"`
	State    string `json:"state,omitempty"`
	// Replayed is a retry of a send already made; nothing new went out.
	Replayed bool `json:"replayed,omitempty"`
}

// errDenied rolls back a compose policy refused, so a refusal leaves no
// empty thread or contact behind.
var errDenied = errors.New("denied")

// Compose starts a thread with a new message, under the same policy as a
// reply: it is held for a person (or refused, e.g. the person opted out),
// never sent by the agent. A new address becomes a new, unverified contact.
// A text goes on the conversation with that number if there is one already.
func Compose(ctx context.Context, pool *pgxpool.Pool, a Agent, m Message) (Composed, error) {
	return compose(ctx, pool, a, m, nil)
}

func compose(ctx context.Context, pool *pgxpool.Pool, a Agent, m Message, send *integrationSend) (Composed, error) {
	var out Composed
	to, err := composeAddress(m.Channel, m.To)
	if err != nil {
		return out, err
	}
	m.Subject, m.Name = strings.TrimSpace(m.Subject), strings.TrimSpace(m.Name)
	if m.Channel == "email" && m.Subject == "" {
		return out, ErrComposeSubject
	}
	if strings.TrimSpace(m.Body) == "" {
		return out, ErrEmptyDraft
	}
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if send != nil {
			if err := lockSends(ctx, tx, a.ID); err != nil {
				return err
			}
			if done, err := send.replay(ctx, tx, m, to, &out); done || err != nil {
				return err
			}
		}
		var from string
		if m.Channel == "sms" {
			if from, err = textingNumber(ctx, tx, a.WorkspaceID, m.From); err != nil {
				return err
			}
		}
		var name *string
		if m.Name != "" {
			name = &m.Name
		}
		if m.Channel == "sms" {
			out.ContactID, err = ingest.PhoneContact(ctx, tx, a.WorkspaceID, to, name)
		} else {
			err = tx.QueryRow(ctx, `SELECT contact_id FROM identities WHERE workspace_id = $1 AND channel = $2 AND address = $3`,
				a.WorkspaceID, m.Channel, to).Scan(&out.ContactID)
			if db.NotFound(err) {
				if err = tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`,
					a.WorkspaceID, name).Scan(&out.ContactID); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, $3, $4)`,
					a.WorkspaceID, out.ContactID, m.Channel, to)
			}
		}
		if err != nil {
			return err
		}
		if m.Channel == "email" {
			err = tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject) VALUES ($1, $2, 'email', $3) RETURNING id`,
				a.WorkspaceID, out.ContactID, m.Subject).Scan(&out.ThreadID)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject, phone_number, remote_address)
				VALUES ($1, $2, 'sms', $3, $4, $3)
				ON CONFLICT (workspace_id, remote_address, phone_number) WHERE channel = 'sms'
				DO UPDATE SET updated_at = now() RETURNING id`, a.WorkspaceID, out.ContactID, to, from).Scan(&out.ThreadID)
		}
		if err != nil {
			return err
		}
		d := m.Draft
		if m.Channel == "sms" {
			d.Subject = ""
		}
		if out.Decision, err = propose(ctx, tx, a, out.ThreadID, d, emailOnly(m.Channel, to), send); err != nil {
			return err
		}
		if out.Decision.Decision == "denied" {
			return errDenied
		}
		if send != nil {
			out.ActionID, out.State = send.result.ActionID, send.result.State
		}
		return nil
	})
	if errors.Is(err, errDenied) {
		return Composed{Decision: out.Decision}, nil
	}
	return out, err
}

func emailOnly(channel, to string) string {
	if channel == "email" {
		return to
	}
	return ""
}

func composeAddress(channel, to string) (string, error) {
	switch channel {
	case "email":
		addr, err := mail.ParseAddress(strings.TrimSpace(to))
		if err != nil || !strings.Contains(addr.Address, "@") {
			return "", ErrComposeTo
		}
		return strings.ToLower(addr.Address), nil
	case "sms":
		n, err := phone.Normalize(to)
		if err != nil {
			return "", ErrComposeTo
		}
		return n, nil
	}
	return "", ErrComposeChannel
}

// textingNumber is the number a new text goes from: the one named, or the
// workspace's only texting number.
func textingNumber(ctx context.Context, tx pgx.Tx, workspaceID, named string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT number FROM phone_numbers WHERE workspace_id = $1 AND capabilities->>'sms' = 'true' ORDER BY number`, workspaceID)
	if err != nil {
		return "", err
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	if len(numbers) == 0 {
		return "", fmt.Errorf("%w: it has none", ErrComposeFrom)
	}
	if named == "" {
		if len(numbers) == 1 {
			return numbers[0], nil
		}
		return "", fmt.Errorf("%w; it has several, so name one: %s", ErrComposeFrom, strings.Join(numbers, ", "))
	}
	n, _ := phone.Normalize(named)
	for _, number := range numbers {
		if number == n {
			return n, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrComposeFrom, strings.Join(numbers, ", "))
}
