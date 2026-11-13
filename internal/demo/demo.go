// Package demo seeds a workspace with realistic data so the console can be
// seen working: a made-up plumbing company's inbox, a sandbox email connector
// that sends nothing, and two versions of approved info.
package demo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/review"
)

// Seeded is what was created.
type Seeded struct {
	WorkspaceID string
	Inbox       string
}

type seeder struct {
	ctx    context.Context
	tx     pgx.Tx
	now    time.Time
	ws     string
	inbox  string
	agent  string
	ops    string
	v12    string
	err    error
	msgSeq int
}

// Seed creates a new demo workspace.
func Seed(ctx context.Context, pool *pgxpool.Pool, name string) (Seeded, error) {
	var out Seeded
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		s := &seeder{ctx: ctx, tx: tx, now: time.Now()}
		s.build(name)
		out = Seeded{WorkspaceID: s.ws, Inbox: s.inbox}
		return s.err
	})
	return out, err
}

func (s *seeder) one(dest any, sql string, args ...any) {
	if s.err != nil {
		return
	}
	s.err = s.tx.QueryRow(s.ctx, sql, args...).Scan(dest)
	if s.err != nil {
		s.err = fmt.Errorf("%w in: %.80s", s.err, sql)
	}
}

func (s *seeder) exec(sql string, args ...any) {
	if s.err != nil {
		return
	}
	if _, err := s.tx.Exec(s.ctx, sql, args...); err != nil {
		s.err = fmt.Errorf("%w in: %.80s", err, sql)
	}
}

func (s *seeder) ago(d time.Duration) time.Time { return s.now.Add(-d) }

func (s *seeder) build(name string) {
	s.one(&s.ws, `INSERT INTO workspaces (name, timezone, business_hours) VALUES ($1, 'America/Los_Angeles',
		'{"mon":[["07:00","17:00"]],"tue":[["07:00","17:00"]],"wed":[["07:00","17:00"]],"thu":[["07:00","17:00"]],"fri":[["07:00","17:00"]],"sat":[["08:00","12:00"]]}')
		RETURNING id`, name)
	s.inbox = review.NewInboxAddress(name)
	s.exec(`INSERT INTO inbound_addresses (address, workspace_id) VALUES ($1, $2)`, s.inbox, s.ws)
	s.exec(`INSERT INTO connectors (workspace_id, kind, name, health, checked_at)
		VALUES ($1, 'sandbox', 'Sandbox email (nothing is sent)', 'ok', now())`, s.ws)
	s.one(&s.agent, `INSERT INTO principals (workspace_id, kind, name, autonomy, config_version)
		VALUES ($1, 'agent', 'Inbox agent v3', 'approve', 'runner-v3 · prompt 2026-10-01') RETURNING id`, s.ws)
	s.one(&s.ops, `INSERT INTO principals (workspace_id, kind, name, email)
		VALUES ($1, 'human', 'Sam Ortiz', 'sam@acmeplumbing.example') RETURNING id`, s.ws)

	s.one(&s.v12, `INSERT INTO approved_info_versions (workspace_id, version, content, created_by)
		VALUES ($1, 12, $2, $3) RETURNING id`, s.ws, approvedInfo(false), s.ops)
	s.exec(`UPDATE approved_info_versions SET approved_by = $2, approved_at = $3 WHERE id = $1`, s.v12, s.ops, s.ago(50*time.Hour))
	s.exec(`INSERT INTO approved_info_versions (workspace_id, version, content, created_by) VALUES ($1, 13, $2, $3)`,
		s.ws, approvedInfo(true), s.ops)

	s.dana()
	s.marcus()
	s.priya()
	s.tom()
	s.kelly()
	s.jorge()
	s.linda()
}

func approvedInfo(sundays bool) map[string]any {
	emergency := "Emergency calls (no water, a burst pipe, sewage backing up) are answered 6 am to 10 pm, every day but Sunday. Take the address and a callback number; never promise an arrival time."
	if sundays {
		emergency = "Emergency calls (no water, a burst pipe, sewage backing up) are answered 6 am to 10 pm, every day. Take the address and a callback number; never promise an arrival time."
	}
	return map[string]any{
		"service_area": []string{"Riverside", "Fairview", "Oak Hill", "Lakeside"},
		"articles": []map[string]any{
			{"title": "Hours", "content": "Office hours are 7 am to 5 pm on weekdays and 8 am to noon on Saturday."},
			{"title": "Emergencies", "content": emergency},
			{"title": "What we do", "content": "Leaks, clogs, water heaters (tank and tankless), fixtures, repiping and sewer line camera inspections. We do not do gas lines or septic systems."},
			{"title": "Booking a visit", "content": "A visit needs the address, the problem, and a two-hour window that suits the customer. The office confirms every booking; agents may offer windows but not confirm them."},
			{"title": "Quotes", "content": "Every job is quoted on site after a look. Never quote a figure by message; offer a visit instead."},
		},
	}
}

type contact struct{ id, thread string }

func (s *seeder) person(name, email string, tags []string, memory any) contact {
	var c contact
	mem, _ := json.Marshal(memory)
	if memory == nil {
		mem = []byte("[]")
	}
	if tags == nil {
		tags = []string{}
	}
	s.one(&c.id, `INSERT INTO contacts (workspace_id, display_name, tags, memory) VALUES ($1, $2, $3, $4) RETURNING id`,
		s.ws, name, tags, mem)
	s.exec(`INSERT INTO identities (workspace_id, contact_id, channel, address, verified) VALUES ($1, $2, 'email', $3, $4)`,
		s.ws, c.id, email, false)
	return c
}

func (s *seeder) thread(c *contact, subject string, due *time.Time) {
	s.one(&c.thread, `INSERT INTO threads (workspace_id, contact_id, channel, subject, response_due_at)
		VALUES ($1, $2, 'email', $3, $4) RETURNING id`, s.ws, c.id, subject, due)
}

func (s *seeder) received(c contact, at time.Time, from, subject, text string) {
	if s.err != nil {
		return
	}
	s.msgSeq++
	s.exec(`INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, provider, provider_ref, untrusted, occurred_at)
		VALUES ($1, $2, $3, 'email.received', 'email', $4, 'cloudflare_email', $5, true, $6)`,
		s.ws, c.thread, c.id, map[string]any{
			"from": map[string]string{"email": from}, "subject": subject, "text": text,
			"classification": map[string]any{"kind": "message", "reason": "default"},
		}, fmt.Sprintf("<demo-%d-%s@mail.example>", s.msgSeq, c.thread[:8]), at)
}

func (s *seeder) agentEvent(c contact, at time.Time, typ string, payload map[string]any) {
	s.exec(`INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, 'email', $6, $7)`, s.ws, c.thread, c.id, typ, s.agent, payload, at)
}

type check struct {
	Label string `json:"label"`
	OK    bool   `json:"ok"`
}

func (s *seeder) hold(c contact, at time.Time, to, subject, body, rule, reason string, checks []check, quotesInfo bool) {
	payload := map[string]string{"subject": subject, "body": body}
	recipients := []map[string]string{{"email": to}}
	var aiv *string
	if quotesInfo {
		aiv = &s.v12
	}
	raw, _ := json.Marshal(map[string]any{"recipients": recipients, "payload": payload, "linked_states": []any{}, "approved_info_version_id": aiv})
	sum := sha256.Sum256(raw)
	var id string
	s.one(&id, `INSERT INTO proposals (workspace_id, thread_id, requested_by, action, payload, recipients,
			approved_info_version_id, payload_sha256, rule, reason, checks, created_at)
		VALUES ($1, $2, $3, 'email.send', $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		s.ws, c.thread, s.agent, payload, recipients, aiv, sum[:], rule, reason, checks, at)
	s.agentEvent(c, at, "proposal.held", map[string]any{"proposal_id": id, "rule": rule, "reason": reason})
}

func (s *seeder) link(c contact, system, typ, id, state string, pushed time.Time) {
	s.exec(`INSERT INTO links (workspace_id, thread_id, contact_id, system, record_type, external_id, state, pushed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, s.ws, c.thread, c.id, system, typ, id, state, pushed)
}

func due(t time.Time) *time.Time { d := t.Add(4 * time.Hour); return &d }

func (s *seeder) dana() {
	at := s.ago(3*time.Hour + 12*time.Minute)
	c := s.person("Dana Whitfield", "dana.whitfield@example.com", []string{"homeowner"}, nil)
	s.thread(&c, "Gas smell near the water heater", due(at))
	s.received(c, at, "dana.whitfield@example.com", "Gas smell near the water heater",
		"Hi, there's a faint gas smell near our water heater in the garage since this morning. Can someone come look at it today? Thanks, Dana")
	s.agentEvent(c, at.Add(20*time.Second), "triage.needs_person", map[string]any{
		"note": "possible gas leak", "detail": "Gas lines are not a service in approved info, and a gas smell needs a person now.",
	})
}

func (s *seeder) marcus() {
	at := s.ago(2*time.Hour + 5*time.Minute)
	c := s.person("Marcus Lee", "marcus@roofco.example", []string{"contractor"},
		[]map[string]string{{"text": "Renovation contractor, works in Riverside.", "source": "From the customer's email · untrusted"}})
	s.thread(&c, "Rough-in for a bathroom remodel next Tuesday?", due(at))
	s.received(c, at, "marcus@roofco.example", "Rough-in for a bathroom remodel next Tuesday?",
		"Hi, we're remodeling a bathroom at 4418 Elm St, Riverside. Could you do the plumbing rough-in next Tuesday, and what would it cost?")
	s.link(c, "dispatch", "job_request", "JR-20417", "pending", at.Add(30*time.Second))
	s.hold(c, at.Add(70*time.Second), "marcus@roofco.example", "Re: Rough-in for a bathroom remodel next Tuesday?",
		"Hi Marcus,\n\nYes, Riverside is in our area, and we do remodel rough-ins. We quote every job on site, so the first step is a visit: would Monday between 9 and 11 or between 1 and 3 suit you?\n\nTuesday for the work itself is not confirmed yet; the office will confirm once we have seen the job.\n\nAcme Plumbing",
		"offers a visit window", "Offers a visit window",
		[]check{{"Matches approved info v12", true}, {"Address inside service area", true}, {"No date confirmed", true}, {"Recipient not opted out", true}, {"Recipient identity verified", false}}, true)
}

func (s *seeder) priya() {
	at := s.ago(1*time.Hour + 40*time.Minute)
	c := s.person("Priya Nair", "priya.nair@example.com", nil, nil)
	s.thread(&c, "Tankless water heater", due(at))
	s.received(c, at, "priya.nair@example.com", "Tankless water heater",
		"Do you install tankless water heaters in Fairview? Our old tank is about 15 years old.")
	s.agentEvent(c, at.Add(2*time.Minute), "run.failed", map[string]any{"reason": "Run timed out twice before drafting. No reply sent."})
}

func (s *seeder) tom() {
	at := s.ago(26 * time.Hour)
	c := s.person("Tom Alvarez", "tom@alvarezbuild.example", []string{"contractor"}, nil)
	s.thread(&c, "Quote follow-up", nil)
	s.received(c, at, "tom@alvarezbuild.example", "Quote for repiping",
		"We need a four-unit building in Oak Hill repiped. Can you send a quote after the walk-through?")
	s.link(c, "dispatch", "quote", "Q-20261007-0031", "sent", at.Add(3*time.Hour))
	s.exec(`INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload, occurred_at)
		VALUES ($1, $2, $3, 'email.sent', $4, 'email', $5, $6)`, s.ws, c.thread, c.id, s.ops,
		map[string]any{"subject": "Re: Quote for repiping", "text": "Hi Tom, your quote Q-20261007-0031 is attached. It's good for 30 days.", "to": []map[string]string{{"email": "tom@alvarezbuild.example"}}},
		at.Add(3*time.Hour))
	if s.err != nil {
		return
	}
	var action string
	sum := sha256.Sum256([]byte("tom-follow-up"))
	s.one(&action, `INSERT INTO actions (workspace_id, thread_id, requested_by, action, payload, recipients, payload_sha256, state, idempotency_key, created_at, updated_at)
		VALUES ($1, $2, $3, 'email.send', $4, $5, $6, 'unknown', $7, $8, $8) RETURNING id`,
		s.ws, c.thread, s.agent,
		map[string]string{"subject": "Re: Quote for repiping", "body": "Hi Tom, just checking you received the quote. Happy to answer any questions."},
		[]map[string]string{{"email": "tom@alvarezbuild.example"}}, sum[:], "demo-tom-"+c.thread[:8], s.ago(58*time.Minute))
	s.exec(`INSERT INTO delivery_attempts (action_id, sent_at, outcome, provider_response) VALUES ($1, $2, 'unknown', '{"error": "timeout after 30s; provider did not answer"}')`,
		action, s.ago(58*time.Minute))
	s.agentEvent(c, s.ago(58*time.Minute), "email.send_unknown", map[string]any{"action_id": action, "note": "Provider did not confirm. Not retried."})
}

func (s *seeder) kelly() {
	at := s.ago(31 * time.Minute)
	c := s.person("Kelly Brooks", "kbrooks@example.com", []string{"customer"}, nil)
	s.thread(&c, "Can we move Thursday's visit?", due(at))
	s.link(c, "dispatch", "job", "J-4471", "scheduled", s.ago(4*24*time.Hour))
	s.received(c, at, "kbrooks@example.com", "Can we move Thursday's visit?",
		"Hi, something came up on Thursday. Could the plumber come Friday morning instead?")
	s.hold(c, at.Add(50*time.Second), "kbrooks@example.com", "Re: Can we move Thursday's visit?",
		"Hi Kelly,\n\nOf course. Would Friday between 8 and 10 or between 10 and noon work? Once you pick one, the office will confirm and move job J-4471.\n\nAcme Plumbing",
		"changes a booking", "Changes a booking",
		[]check{{"Matches approved info v12", true}, {"Linked job J-4471 is scheduled", true}, {"Recipient not opted out", true}}, true)
}

func (s *seeder) jorge() {
	at := s.ago(12 * time.Minute)
	c := s.person("Jorge Ramos", "jorge.ramos@example.com", nil, nil)
	s.thread(&c, "Kitchen sink keeps clogging", due(at))
	s.received(c, at, "jorge.ramos@example.com", "Kitchen sink keeps clogging",
		"Our kitchen sink in Lakeside has clogged three times this month. Can someone come out this week?")
	s.hold(c, at.Add(40*time.Second), "jorge.ramos@example.com", "Re: Kitchen sink keeps clogging",
		"Hi Jorge,\n\nSorry about the sink. Lakeside is in our area, and a repeat clog is worth a proper look. Would Wednesday between 1 and 3 or Thursday between 9 and 11 suit you? The office will confirm the visit.\n\nAcme Plumbing",
		"first message to a new contact", "First message to a new contact",
		[]check{{"Matches approved info v12", true}, {"Lakeside is inside the service area", true}, {"Recipient not opted out", true}}, true)
}

func (s *seeder) linda() {
	at := s.ago(28 * time.Hour)
	c := s.person("Linda Park", "linda.park@example.com", nil, nil)
	s.thread(&c, "Do you work on septic systems?", nil)
	s.received(c, at, "linda.park@example.com", "Do you work on septic systems?", "Our septic tank needs looking at. Is that something you do?")
	s.exec(`INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload, occurred_at)
		VALUES ($1, $2, $3, 'email.sent', $4, 'email', $5, $6)`, s.ws, c.thread, c.id, s.agent,
		map[string]any{"subject": "Re: Do you work on septic systems?", "text": "Hi Linda, sorry, we don't work on septic systems. We do handle sewer line camera inspections if that would help.", "to": []map[string]string{{"email": "linda.park@example.com"}}},
		at.Add(25*time.Minute))
	s.exec(`UPDATE threads SET state = 'resolved' WHERE id = $1`, c.thread)
}
