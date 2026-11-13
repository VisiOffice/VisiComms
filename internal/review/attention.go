// Package review is what a person sees and decides in the console: the
// needs-attention list, a thread, and the decisions on held drafts. Every
// query is scoped to the person's workspace.
package review

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Kinds of item waiting on a person.
const (
	HeldDraft      = "held_draft"
	Unanswered     = "unanswered"
	FailedRun      = "failed_run"
	UnknownSend    = "unknown_send"
	SendFailed     = "send_failed"
	Complaint      = "complaint"
	ConnectorError = "connector_error"
	Escalated      = "escalated"
)

// defaultResponseWindow stands in for business-hours deadlines until those
// are computed per workspace.
const defaultResponseWindow = 24 * time.Hour

type AttentionItem struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	ThreadID string `json:"thread_id,omitempty"`
	Contact  string `json:"contact,omitempty"`
	Subject  string `json:"subject,omitempty"`
	// Reason says in plain words why a person is needed.
	Reason string `json:"reason"`
	// WaitingSince is when the customer started waiting (or the problem began).
	WaitingSince time.Time  `json:"waiting_since"`
	DueAt        *time.Time `json:"due_at,omitempty"`
}

// Attention lists everything waiting on a person, most urgent first: by
// response deadline, then by how long the customer has waited, so older
// inquiries are never buried by new ones.
func Attention(ctx context.Context, q db.Querier, workspaceID string) ([]AttentionItem, error) {
	var items []AttentionItem
	for _, load := range []func(context.Context, db.Querier, string) ([]AttentionItem, error){
		heldDrafts, unanswered, escalated, failedRuns, unknownSends, failedSends, complaints, connectorErrors, failedFaxReceives,
	} {
		got, err := load(ctx, q, workspaceID)
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.DueAt != nil && b.DueAt != nil && !a.DueAt.Equal(*b.DueAt):
			return a.DueAt.Before(*b.DueAt)
		case a.DueAt != nil && b.DueAt == nil:
			return true
		case a.DueAt == nil && b.DueAt != nil:
			return false
		}
		return a.WaitingSince.Before(b.WaitingSince)
	})
	return items, nil
}

// WaitingSQL is when the customer started waiting on thread t: the first
// inbound message (a missed call, voicemail or message the AI took counts)
// after our last outbound one (an answered call, or a person texting or
// calling the caller back, counts), or NULL.
const WaitingSQL = `(SELECT min(e.occurred_at) FROM events e WHERE e.thread_id = t.id AND e.type IN ('email.received','sms.received','chat.received','fax.received','call.missed','voice.voicemail','call.ai_handled')
	AND e.payload->'classification'->>'kind' = 'message'
 AND NOT EXISTS (SELECT 1 FROM opt_outs oo WHERE oo.contact_id=t.contact_id AND oo.channel=t.channel)
	AND e.id > coalesce((SELECT max(o.id) FROM events o WHERE o.thread_id = t.id AND o.type IN ('email.sent','sms.sent','chat.sent','fax.sent','call.answered','call.texted_back','call.called_back')), 0))`

func withDue(items []AttentionItem, due []*time.Time) []AttentionItem {
	for i := range items {
		if due[i] == nil && !items[i].WaitingSince.IsZero() {
			d := items[i].WaitingSince.Add(defaultResponseWindow)
			due[i] = &d
		}
		items[i].DueAt = due[i]
	}
	return items
}

func scanThreadItems(rows pgx.Rows, kind string) ([]AttentionItem, error) {
	var items []AttentionItem
	var due []*time.Time
	for rows.Next() {
		it := AttentionItem{Kind: kind}
		var d *time.Time
		var waiting *time.Time
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.Contact, &it.Subject, &it.Reason, &waiting, &d); err != nil {
			return nil, err
		}
		if waiting != nil {
			it.WaitingSince = *waiting
		}
		items = append(items, it)
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return withDue(items, due), nil
}

func heldDrafts(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT p.id, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			'Held: ' || coalesce(p.reason, p.rule),
			coalesce(`+WaitingSQL+`, p.created_at), t.response_due_at
		FROM proposals p JOIN threads t ON t.id = p.thread_id JOIN contacts c ON c.id = t.contact_id
		WHERE p.workspace_id = $1 AND p.status = 'pending'`, ws)
	if err != nil {
		return nil, err
	}
	return scanThreadItems(rows, HeldDraft)
}

func unanswered(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT t.id, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			coalesce((SELECT 'No reply yet: ' || (e.payload->>'note') FROM events e
				WHERE e.thread_id = t.id AND e.type IN ('triage.needs_person', 'thread.escalated') ORDER BY e.id DESC LIMIT 1),
				'No reply yet'),
			w.waiting, t.response_due_at
		FROM threads t JOIN contacts c ON c.id = t.contact_id,
		LATERAL (SELECT `+WaitingSQL+` AS waiting) w
		WHERE t.workspace_id = $1 AND t.state <> 'resolved' AND w.waiting IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.thread_id = t.id AND p.status = 'pending')
		  AND NOT EXISTS (SELECT 1 FROM events f WHERE f.thread_id = t.id AND f.type = 'run.failed' AND f.occurred_at >= w.waiting)`, ws)
	if err != nil {
		return nil, err
	}
	return scanThreadItems(rows, Unanswered)
}

// escalated are threads an agent handed to a person when no customer is
// waiting (those are already listed as unanswered, with the agent's note),
// until a person acts on the thread.
func escalated(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			'Escalated: ' || coalesce(x.payload->>'note', 'an agent asked for a person'), x.occurred_at, NULL::timestamptz
		FROM threads t JOIN contacts c ON c.id = t.contact_id,
		LATERAL (SELECT * FROM events e WHERE e.thread_id = t.id AND e.type = 'thread.escalated' ORDER BY e.id DESC LIMIT 1) x
		WHERE t.workspace_id = $1 AND t.state <> 'resolved' AND `+WaitingSQL+` IS NULL
		  AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.thread_id = t.id AND p.status = 'pending')
		  AND NOT EXISTS (SELECT 1 FROM events h JOIN principals hp ON hp.id = h.actor_id
			WHERE h.thread_id = t.id AND h.id > x.id AND hp.kind = 'human')`, ws)
	if err != nil {
		return nil, err
	}
	items, err := scanThreadItems(rows, Escalated)
	for i := range items {
		items[i].DueAt = nil
	}
	return items, err
}

func failedRuns(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT f.id::text, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			coalesce(f.payload->>'reason', 'The agent run failed'), coalesce(w.waiting, f.occurred_at), t.response_due_at
		FROM threads t JOIN contacts c ON c.id = t.contact_id,
		LATERAL (SELECT `+WaitingSQL+` AS waiting) w,
		LATERAL (SELECT * FROM events e WHERE e.thread_id = t.id AND e.type = 'run.failed' ORDER BY e.id DESC LIMIT 1) f
		WHERE t.workspace_id = $1 AND t.state <> 'resolved' AND w.waiting IS NOT NULL AND f.occurred_at >= w.waiting
		  AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.thread_id = t.id AND p.status = 'pending')`, ws)
	if err != nil {
		return nil, err
	}
	return scanThreadItems(rows, FailedRun)
}

func unknownSends(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT a.id, coalesce(t.id::text, ''), coalesce(c.display_name, ''), coalesce(t.subject, ''),
			'The provider did not confirm this send. Check delivery before sending again.', a.updated_at, NULL::timestamptz
		FROM actions a LEFT JOIN threads t ON t.id = a.thread_id LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE a.workspace_id = $1 AND a.state = 'unknown'`, ws)
	if err != nil {
		return nil, err
	}
	items, err := scanThreadItems(rows, UnknownSend)
	for i := range items {
		items[i].DueAt = nil
	}
	return items, err
}

// failedSends are replies that were refused or bounced, until the thread has
// had a successful send since.
func failedSends(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT a.id, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			'Not delivered: ' || coalesce((SELECT e.payload->>'reason' FROM events e WHERE e.thread_id = t.id
				AND e.type IN ('email.failed', 'email.bounced', 'sms.failed', 'fax.failed', 'postal.failed', 'postal.returned') AND e.payload->>'action_id' = a.id::text ORDER BY e.id DESC LIMIT 1), 'the provider refused it'),
			a.updated_at, NULL::timestamptz
		FROM actions a JOIN threads t ON t.id = a.thread_id JOIN contacts c ON c.id = t.contact_id
		WHERE a.workspace_id = $1 AND a.state = 'failed' AND t.state <> 'resolved'
		  AND NOT EXISTS (SELECT 1 FROM actions b WHERE b.thread_id = a.thread_id AND b.state IN ('accepted', 'delivered') AND b.updated_at > a.updated_at)`, ws)
	if err != nil {
		return nil, err
	}
	items, err := scanThreadItems(rows, SendFailed)
	for i := range items {
		items[i].DueAt = nil
	}
	return items, err
}

// complaints are spam complaints from the last two weeks. The contact is
// already opted out; a person should know, and look at what was sent.
func complaints(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT e.id::text, t.id, coalesce(c.display_name, ''), coalesce(t.subject, ''),
			'Marked a reply as spam. They are opted out of email.', e.recorded_at, NULL::timestamptz
		FROM events e JOIN threads t ON t.id = e.thread_id JOIN contacts c ON c.id = t.contact_id
		WHERE e.workspace_id = $1 AND e.type = 'email.complained' AND e.recorded_at > now() - interval '14 days'
		  AND t.state <> 'resolved'`, ws)
	if err != nil {
		return nil, err
	}
	items, err := scanThreadItems(rows, Complaint)
	for i := range items {
		items[i].DueAt = nil
	}
	return items, err
}

func connectorErrors(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT id, name, coalesce(last_error, 'Failing health check'), coalesce(checked_at, updated_at)
		FROM connectors WHERE workspace_id = $1 AND health = 'failing'`, ws)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AttentionItem, error) {
		it := AttentionItem{Kind: ConnectorError}
		err := r.Scan(&it.ID, &it.Subject, &it.Reason, &it.WaitingSince)
		return it, err
	})
}

func failedFaxReceives(ctx context.Context, q db.Querier, ws string) ([]AttentionItem, error) {
	rows, err := q.Query(ctx, `SELECT e.id::text,t.id,coalesce(c.display_name,''),coalesce(t.subject,''),
 'Fax reception failed: '||coalesce(e.payload->>'reason','unknown error'),e.occurred_at,NULL::timestamptz
 FROM events e JOIN threads t ON t.id=e.thread_id JOIN contacts c ON c.id=t.contact_id
 WHERE e.workspace_id=$1 AND e.type='fax.receive_failed' AND t.state<>'resolved'
 AND NOT EXISTS(SELECT 1 FROM events newer WHERE newer.thread_id=t.id AND newer.type='fax.received' AND newer.id>e.id)`, ws)
	if err != nil {
		return nil, err
	}
	return scanThreadItems(rows, SendFailed)
}
