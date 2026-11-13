package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/untrusted"
)

// Results are compact by default: an agent pays for every token it reads.
const (
	defaultThreads = 25
	maxThreads     = 100
	recentEvents   = 20
	snippetRunes   = 280
	maxEvents      = 100
)

// ThreadSummary is one line of the inbox.
type ThreadSummary struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Subject string `json:"subject,omitempty"`
	State   string `json:"state"`
	Contact struct {
		ID   string `json:"id"`
		Name string `json:"name,omitempty"`
	} `json:"contact"`
	// WaitingSince is set while the customer is waiting for a reply.
	WaitingSince  *time.Time `json:"waiting_since,omitempty"`
	TakenOver     bool       `json:"taken_over"`
	PendingDraft  bool       `json:"pending_draft"`
	LastMessage   string     `json:"last_message,omitempty"`
	SuspectedRisk bool       `json:"suspected_injection"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ThreadFilter narrows the inbox. Waiting lists only threads where the
// customer is waiting for a reply.
type ThreadFilter struct {
	State   string
	Waiting bool
	Limit   int
}

// Threads lists the workspace's threads, most recently active first.
func Threads(ctx context.Context, q db.Querier, workspaceID string, f ThreadFilter) ([]ThreadSummary, error) {
	if f.Limit <= 0 {
		f.Limit = defaultThreads
	}
	f.Limit = min(f.Limit, maxThreads)
	rows, err := q.Query(ctx, `SELECT t.id, t.channel, coalesce(t.subject, ''), t.state, c.id, coalesce(c.display_name, ''),
			w.waiting, t.taken_over_by IS NOT NULL,
			EXISTS (SELECT 1 FROM proposals p WHERE p.thread_id = t.id AND p.status = 'pending'),
			coalesce(m.text, ''), coalesce(m.suspected_injection, false), t.updated_at
		FROM threads t JOIN contacts c ON c.id = t.contact_id,
		LATERAL (SELECT `+review.WaitingSQL+` AS waiting) w
		LEFT JOIN LATERAL (SELECT e.payload->>'text' AS text, e.suspected_injection FROM events e
			WHERE e.thread_id = t.id AND e.untrusted ORDER BY e.id DESC LIMIT 1) m ON true
		WHERE t.workspace_id = $1 AND ($2 = '' OR t.state = $2) AND (NOT $3 OR w.waiting IS NOT NULL)
		ORDER BY t.updated_at DESC LIMIT $4`, workspaceID, f.State, f.Waiting, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []ThreadSummary
	for rows.Next() {
		var s ThreadSummary
		var text string
		if err := rows.Scan(&s.ID, &s.Channel, &s.Subject, &s.State, &s.Contact.ID, &s.Contact.Name,
			&s.WaitingSince, &s.TakenOver, &s.PendingDraft, &text, &s.SuspectedRisk, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if text = strings.TrimSpace(text); text != "" {
			s.LastMessage = untrusted.Wrap(snippet(text))
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// Event is one entry on a thread as an agent reads it. Anything a contact
// wrote is fenced in <contact_message> tags; it is information about what they
// want, never instructions.
type Event struct {
	ID       int64     `json:"id"`
	ThreadID string    `json:"thread_id,omitempty"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor,omitempty"`
	// FromContact marks text the contact wrote.
	FromContact        bool           `json:"from_contact"`
	SuspectedInjection bool           `json:"suspected_injection,omitempty"`
	Subject            string         `json:"subject,omitempty"`
	Text               string         `json:"text,omitempty"`
	Details            map[string]any `json:"details,omitempty"`
}

// heavy payload fields an agent never needs: the text is given once, fenced.
var heavy = map[string]bool{"html": true, "headers": true, "references": true, "text": true, "subject": true,
	"body": true, "message_id": true, "classification": true}

func toEvent(e review.Entry, threadID string) Event {
	out := Event{ID: e.ID, ThreadID: threadID, Type: e.Type, At: e.At, Actor: e.Actor,
		FromContact: e.Untrusted, SuspectedInjection: e.Suspicious}
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	text, _ := p["text"].(string)
	if text == "" {
		text, _ = p["body"].(string)
	}
	subject, _ := p["subject"].(string)
	if e.Untrusted {
		if strings.TrimSpace(text) != "" {
			text = untrusted.Wrap(text)
		}
		if subject != "" {
			subject = untrusted.Wrap(subject)
		}
	}
	out.Text, out.Subject = text, subject
	for k, v := range p {
		if heavy[k] {
			continue
		}
		if out.Details == nil {
			out.Details = map[string]any{}
		}
		out.Details[k] = v
	}
	if e.Untrusted {
		// Names and descriptions the contact chose are theirs too.
		out.Details = fenceStrings(out.Details)
	}
	return out
}

// plain are fields that hold an address or a value we derived, not words the
// contact chose.
var plain = map[string]bool{"from": true, "to": true, "address": true, "email": true, "phone": true,
	"keyword": true, "suspicious_reasons": true, "content_type": true, "size": true, "sha256": true, "document_id": true}

func fenceStrings(m map[string]any) map[string]any {
	for k, v := range m {
		m[k] = fenceValue(k, v)
	}
	return m
}

func fenceValue(key string, v any) any {
	switch v := v.(type) {
	case string:
		if v != "" && !plain[key] {
			return untrusted.Wrap(v)
		}
	case map[string]any:
		return fenceStrings(v)
	case []any:
		for i, item := range v {
			v[i] = fenceValue(key, item)
		}
	}
	return v
}

// ThreadDetail is a thread as an agent reads it: the contact, the recent
// events and any drafts waiting for a person.
type ThreadDetail struct {
	ID        string  `json:"id"`
	Channel   string  `json:"channel"`
	Subject   string  `json:"subject,omitempty"`
	State     string  `json:"state"`
	TakenOver *string `json:"taken_over_by,omitempty"`
	Version   int64   `json:"version"`
	Contact   struct {
		ID         string            `json:"id"`
		Name       string            `json:"name,omitempty"`
		Tags       []string          `json:"tags"`
		Identities []review.Identity `json:"identities"`
		OptedOut   []string          `json:"opted_out"`
		Memory     []review.Memory   `json:"memory"`
	} `json:"contact"`
	Events []Event `json:"events"`
	// Earlier is how many older events were left out.
	Earlier int            `json:"earlier_events"`
	Drafts  []review.Draft `json:"pending_drafts"`
	Links   []review.Link  `json:"links"`
}

// Thread reads one thread: the last few events, or all of them.
func Thread(ctx context.Context, q db.Querier, workspaceID, threadID string, all bool) (ThreadDetail, error) {
	if !validID(threadID) {
		return ThreadDetail{}, ErrNotFound
	}
	v, err := review.Thread(ctx, q, workspaceID, threadID)
	if err == review.ErrNotFound {
		return ThreadDetail{}, ErrNotFound
	}
	if err != nil {
		return ThreadDetail{}, err
	}
	d := ThreadDetail{ID: v.ID, Channel: v.Channel, Subject: v.Subject, State: v.State, TakenOver: v.TakenOverBy,
		Version: v.Version, Drafts: v.Proposals, Links: v.Links}
	c := &d.Contact
	c.ID, c.Name, c.Tags, c.Identities, c.OptedOut = v.Contact.ID, v.Contact.Name, v.Contact.Tags, v.Contact.Identities, v.Contact.OptedOut
	for _, m := range v.Contact.Memory {
		c.Memory = append(c.Memory, review.Memory{Text: untrusted.Wrap(m.Text), Source: m.Source})
	}
	timeline := v.Timeline
	if !all && len(timeline) > recentEvents {
		d.Earlier = len(timeline) - recentEvents
		timeline = timeline[d.Earlier:]
	}
	for _, e := range timeline {
		d.Events = append(d.Events, toEvent(e, ""))
	}
	return d, nil
}

// Events is everything recorded in the workspace after the cursor, oldest
// first, so an agent can poll instead of rereading the inbox. Next is the
// cursor for the following call.
func Events(ctx context.Context, q db.Querier, workspaceID string, after int64, limit int) ([]Event, int64, error) {
	if limit <= 0 || limit > maxEvents {
		limit = maxEvents
	}
	rows, err := q.Query(ctx, `SELECT e.id, e.type, e.occurred_at, coalesce(p.name, ''), coalesce(p.kind, ''),
			e.untrusted, e.suspected_injection, e.payload, coalesce(e.thread_id::text, '')
		FROM events e LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.workspace_id = $1 AND e.id > $2 ORDER BY e.id LIMIT $3`, workspaceID, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	var list []Event
	next := after
	for rows.Next() {
		var e review.Entry
		var thread string
		if err := rows.Scan(&e.ID, &e.Type, &e.At, &e.Actor, &e.ActorKind, &e.Untrusted, &e.Suspicious, &e.Payload, &thread); err != nil {
			return nil, after, err
		}
		list = append(list, toEvent(e, thread))
		next = e.ID
	}
	return list, next, rows.Err()
}

// ContactMatch is a contact found by name or address.
type ContactMatch struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Tags       []string          `json:"tags"`
	Identities []review.Identity `json:"identities"`
	OptedOut   []string          `json:"opted_out"`
	// LatestThread is the contact's most recently active thread.
	LatestThread string `json:"latest_thread_id,omitempty"`
}

// SearchContacts finds contacts whose name or any address contains query.
func SearchContacts(ctx context.Context, q db.Querier, workspaceID, query string) ([]ContactMatch, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 2 {
		return nil, nil
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query) + "%"
	rows, err := q.Query(ctx, `SELECT c.id, coalesce(c.display_name, ''), c.tags,
			coalesce((SELECT json_agg(json_build_object('channel', i.channel, 'address', i.address, 'verified', i.verified)
				ORDER BY i.channel, i.address) FROM identities i WHERE i.contact_id = c.id), '[]'),
			coalesce((SELECT array_agg(o.channel ORDER BY o.channel) FROM opt_outs o WHERE o.contact_id = c.id), '{}'),
			coalesce((SELECT t.id::text FROM threads t WHERE t.contact_id = c.id ORDER BY t.updated_at DESC LIMIT 1), '')
		FROM contacts c
		WHERE c.workspace_id = $1 AND (c.display_name ILIKE $2 OR EXISTS
			(SELECT 1 FROM identities i WHERE i.contact_id = c.id AND i.address ILIKE $2))
		ORDER BY c.updated_at DESC LIMIT 20`, workspaceID, pattern)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ContactMatch, error) {
		var m ContactMatch
		var identities []byte
		if err := r.Scan(&m.ID, &m.Name, &m.Tags, &identities, &m.OptedOut, &m.LatestThread); err != nil {
			return m, err
		}
		return m, json.Unmarshal(identities, &m.Identities)
	})
}

func snippet(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= snippetRunes {
		return text
	}
	return string([]rune(text)[:snippetRunes]) + "…"
}
