// Package notify tells people what happened in their workspace: each kind of
// event can be sent at once (email or text), gathered into a daily summary
// email, or left in the console. A person sets their own; an agent can set
// them over MCP.
package notify

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
)

const (
	Instant = "instant"
	Daily   = "daily"
	Off     = "off"
)

// Category is a kind of event a person can be told about.
type Category struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Default     string   `json:"default"`
	events      []string // event types in this category
}

// Categories, in the order the console shows them.
var Categories = []Category{
	{Key: "voicemail", Label: "Voicemails", Description: "A caller left a message. Includes the transcript.", Default: Instant, events: []string{"voice.voicemail"}},
	{Key: "ai_call", Label: "Calls the AI answered", Description: "The AI picked up. Includes what the caller wanted and any message.", Default: Instant, events: []string{"call.ai_handled"}},
	{Key: "missed_call", Label: "Missed calls", Description: "A caller hung up before anyone answered or they left a message.", Default: Instant, events: []string{"call.missed"}},
	{Key: "answered_call", Label: "Answered calls", Description: "A call was answered.", Default: Daily, events: []string{"call.answered"}},
	{Key: "sms", Label: "Text messages", Description: "A customer texted one of your numbers.", Default: Instant, events: []string{"sms.received"}},
	{Key: "chat", Label: "Website chats", Description: "A visitor wrote in your website chat.", Default: Instant, events: []string{"chat.received"}},
	{Key: "email", Label: "Emails", Description: "A customer emailed you.", Default: Daily, events: []string{"email.received"}},
	{Key: "fax", Label: "Faxes", Description: "A fax arrived.", Default: Instant, events: []string{"fax.received"}},
	{Key: "approval", Label: "Waiting for approval", Description: "An agent drafted something that waits for a person.", Default: Instant, events: []string{"proposal.held"}},
	{Key: "escalation", Label: "Escalations", Description: "An agent handed a thread to a person.", Default: Instant, events: []string{"thread.escalated"}},
	{Key: "failure", Label: "Failed sends", Description: "An email, text or fax could not be delivered.", Default: Instant, events: []string{"email.failed", "email.bounced", "email.complained", "sms.failed", "fax.failed", "fax.receive_failed"}},
}

// categoryOf maps an event type to its category key, "" for none.
func categoryOf(eventType string) string {
	for _, c := range Categories {
		if slices.Contains(c.events, eventType) {
			return c.Key
		}
	}
	return ""
}

func eventTypes() []string {
	var all []string
	for _, c := range Categories {
		all = append(all, c.events...)
	}
	return all
}

// Setting is one person's choice for one category.
type Setting struct {
	Category string   `json:"category"`
	Delivery string   `json:"delivery"`
	Channels []string `json:"channels"`
}

// Person is someone who can be notified, with their settings (defaults
// filled in).
type Person struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	Phone    string    `json:"phone,omitempty"`
	Settings []Setting `json:"settings"`
}

var (
	ErrInvalid       = errors.New("invalid notification setting")
	ErrUnknownPerson = errors.New("no such person in this workspace")
	ErrObserver      = errors.New("an agent in observe mode cannot change notifications")
)

// People returns the workspace's people and their settings.
func People(ctx context.Context, q db.Querier, workspaceID string) ([]Person, error) {
	rows, err := q.Query(ctx, `SELECT id, name, coalesce(email, ''), coalesce(notify_phone, '') FROM principals
		WHERE workspace_id = $1 AND kind = 'human' AND revoked_at IS NULL ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	people, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Person, error) {
		var p Person
		return p, r.Scan(&p.ID, &p.Name, &p.Email, &p.Phone)
	})
	if err != nil {
		return nil, err
	}
	for i := range people {
		if people[i].Settings, err = settings(ctx, q, people[i].ID); err != nil {
			return nil, err
		}
	}
	return people, nil
}

func settings(ctx context.Context, q db.Querier, principalID string) ([]Setting, error) {
	rows, err := q.Query(ctx, `SELECT category, delivery, channels FROM notification_settings WHERE principal_id = $1`, principalID)
	if err != nil {
		return nil, err
	}
	stored, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Setting, error) {
		var s Setting
		return s, r.Scan(&s.Category, &s.Delivery, &s.Channels)
	})
	if err != nil {
		return nil, err
	}
	out := make([]Setting, 0, len(Categories))
	for _, c := range Categories {
		s := Setting{Category: c.Key, Delivery: c.Default, Channels: []string{"email"}}
		for _, st := range stored {
			if st.Category == c.Key {
				s = st
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Change is an update to one person's notifications. Nil fields are left as
// they are.
type Change struct {
	Settings []Setting `json:"settings"`
	// Phone is the mobile number instant texts go to; "" clears it.
	Phone *string `json:"phone"`
}

// Update applies a change to a person in the workspace, made by actorID (the
// person themself, another person, or an agent).
func Update(ctx context.Context, pool *pgxpool.Pool, workspaceID, personID, actorID string, ch Change) (Person, error) {
	for i := range ch.Settings {
		s := &ch.Settings[i]
		if !slices.ContainsFunc(Categories, func(c Category) bool { return c.Key == s.Category }) {
			return Person{}, errors.Join(ErrInvalid, errors.New("unknown category "+s.Category))
		}
		if s.Delivery != Instant && s.Delivery != Daily && s.Delivery != Off {
			return Person{}, errors.Join(ErrInvalid, errors.New("delivery must be instant, daily or off"))
		}
		if len(s.Channels) == 0 {
			s.Channels = []string{"email"}
		}
		slices.Sort(s.Channels)
		s.Channels = slices.Compact(s.Channels)
		for _, c := range s.Channels {
			if c != "email" && c != "sms" {
				return Person{}, errors.Join(ErrInvalid, errors.New("channels are email and sms"))
			}
		}
	}
	var phoneNumber *string
	if ch.Phone != nil && *ch.Phone != "" {
		n, err := phone.Normalize(*ch.Phone)
		if err != nil {
			return Person{}, errors.Join(ErrInvalid, err)
		}
		phoneNumber = &n
	}
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE id::text = $1 AND workspace_id = $2 AND kind = 'human' AND revoked_at IS NULL)`, personID, workspaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrUnknownPerson
		}
		if ch.Phone != nil {
			if _, err := tx.Exec(ctx, `UPDATE principals SET notify_phone = $2 WHERE id = $1`, personID, phoneNumber); err != nil {
				return err
			}
		}
		for _, s := range ch.Settings {
			if _, err := tx.Exec(ctx, `INSERT INTO notification_settings (principal_id, category, delivery, channels, updated_by)
				VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)
				ON CONFLICT (principal_id, category) DO UPDATE SET delivery = $3, channels = $4, updated_by = NULLIF($5, '')::uuid, updated_at = now()`,
				personID, s.Category, s.Delivery, s.Channels, actorID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Person{}, err
	}
	people, err := People(ctx, pool, workspaceID)
	for _, p := range people {
		if p.ID == personID {
			return p, err
		}
	}
	return Person{}, err
}
