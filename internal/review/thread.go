package review

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

var ErrNotFound = errors.New("not found")

type ThreadView struct {
	ID            string  `json:"id"`
	Subject       string  `json:"subject"`
	Channel       string  `json:"channel"`
	PhoneNumber   string  `json:"phone_number,omitempty"`
	RemoteAddress string  `json:"remote_address,omitempty"`
	State         string  `json:"state"`
	TakenOverBy   *string `json:"taken_over_by,omitempty"`
	// TakenOverByID tells the console whether the holder is the viewer.
	TakenOverByID *string   `json:"taken_over_by_id,omitempty"`
	Version       int64     `json:"version"`
	Contact       Contact   `json:"contact"`
	Timeline      []Entry   `json:"timeline"`
	Proposals     []Draft   `json:"proposals"`
	Links         []Link    `json:"links"`
	UpdatedAt     time.Time `json:"updated_at"`
	// VisitorTyping is set on a chat thread while the visitor is typing.
	VisitorTyping bool `json:"visitor_typing,omitempty"`
}

type Contact struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Tags       []string   `json:"tags"`
	Trusted    bool       `json:"trusted"`
	Memory     []Memory   `json:"memory"`
	Identities []Identity `json:"identities"`
	OptedOut   []string   `json:"opted_out"`
}

type Identity struct {
	Channel  string `json:"channel"`
	Address  string `json:"address"`
	Verified bool   `json:"verified"`
}

// Memory is what the agent noted about a contact: untrusted, since it comes
// from what the customer wrote.
type Memory struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

// Entry is one event on the thread's timeline.
type Entry struct {
	ID         int64           `json:"id"`
	Type       string          `json:"type"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor,omitempty"`
	ActorKind  string          `json:"actor_kind,omitempty"`
	Untrusted  bool            `json:"untrusted"`
	Suspicious bool            `json:"suspicious"`
	Payload    json.RawMessage `json:"payload"`
}

// Draft is a held proposal as the reviewer sees it.
type Draft struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Action              string          `json:"action"`
	Agent               string          `json:"agent"`
	Payload             json.RawMessage `json:"payload"`
	Recipients          json.RawMessage `json:"recipients"`
	Rule                string          `json:"rule"`
	Reason              string          `json:"reason"`
	Checks              json.RawMessage `json:"checks"`
	ApprovedInfoVersion *int            `json:"approved_info_version,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
}

type Link struct {
	System     string     `json:"system"`
	RecordType string     `json:"record_type"`
	ExternalID string     `json:"external_id"`
	State      string     `json:"state"`
	PushedAt   *time.Time `json:"pushed_at,omitempty"`
}

// Thread loads one thread with everything the thread view shows.
func Thread(ctx context.Context, q db.Querier, workspaceID, threadID string) (ThreadView, error) {
	var v ThreadView
	var memory []byte
	err := q.QueryRow(ctx, `SELECT t.id, coalesce(t.subject, ''), t.channel, t.state, tp.name, t.taken_over_by, t.version, t.updated_at,
			c.id, coalesce(c.display_name, ''), c.tags, c.trusted, c.memory, coalesce(t.phone_number,''), coalesce(t.remote_address,'')
		FROM threads t JOIN contacts c ON c.id = t.contact_id LEFT JOIN principals tp ON tp.id = t.taken_over_by
		WHERE t.id = $1 AND t.workspace_id = $2`, threadID, workspaceID).
		Scan(&v.ID, &v.Subject, &v.Channel, &v.State, &v.TakenOverBy, &v.TakenOverByID, &v.Version, &v.UpdatedAt,
			&v.Contact.ID, &v.Contact.Name, &v.Contact.Tags, &v.Contact.Trusted, &memory, &v.PhoneNumber, &v.RemoteAddress)
	if db.NotFound(err) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(memory, &v.Contact.Memory); err != nil {
		return v, err
	}

	rows, err := q.Query(ctx, `SELECT channel, address, verified FROM identities WHERE contact_id = $1 ORDER BY channel, address`, v.Contact.ID)
	if err != nil {
		return v, err
	}
	if v.Contact.Identities, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Identity]); err != nil {
		return v, err
	}
	rows, err = q.Query(ctx, `SELECT channel FROM opt_outs WHERE contact_id = $1 ORDER BY channel`, v.Contact.ID)
	if err != nil {
		return v, err
	}
	if v.Contact.OptedOut, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return v, err
	}

	rows, err = q.Query(ctx, `SELECT e.id, e.type, e.occurred_at, coalesce(p.name, ''), coalesce(p.kind, ''),
			e.untrusted, e.suspected_injection, e.payload
		FROM events e LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.thread_id = $1 ORDER BY e.occurred_at, e.id`, v.ID)
	if err != nil {
		return v, err
	}
	if v.Timeline, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Entry]); err != nil {
		return v, err
	}

	rows, err = q.Query(ctx, `SELECT p.id, p.status, p.action, a.name, p.payload, p.recipients, p.rule,
			coalesce(p.reason, ''), p.checks, ai.version, p.created_at
		FROM proposals p JOIN principals a ON a.id = p.requested_by
		LEFT JOIN approved_info_versions ai ON ai.id = p.approved_info_version_id
		WHERE p.thread_id = $1 AND p.status = 'pending' ORDER BY p.created_at`, v.ID)
	if err != nil {
		return v, err
	}
	if v.Proposals, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Draft]); err != nil {
		return v, err
	}

	rows, err = q.Query(ctx, `SELECT system, record_type, external_id, coalesce(state, ''), pushed_at
		FROM links WHERE thread_id = $1 OR (thread_id IS NULL AND contact_id = $2) ORDER BY system, record_type`, v.ID, v.Contact.ID)
	if err != nil {
		return v, err
	}
	v.Links, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Link])
	return v, err
}
