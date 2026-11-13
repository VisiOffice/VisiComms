package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/demo"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

type fixture struct {
	pool   *pgxpool.Pool
	agent  agent.Agent
	person review.Reviewer
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	seeded, err := demo.Seed(ctx, pool, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := agent.Create(ctx, pool, seeded.WorkspaceID, "Inbox helper", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatal(err)
	}
	var person string
	if err := pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Reviewer') RETURNING id`,
		seeded.WorkspaceID).Scan(&person); err != nil {
		t.Fatal(err)
	}
	return fixture{pool, a, review.Reviewer{ID: person, WorkspaceID: seeded.WorkspaceID}}
}

func (f fixture) thread(t *testing.T, contact string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `SELECT t.id FROM threads t JOIN contacts c ON c.id = t.contact_id
		WHERE t.workspace_id = $1 AND c.display_name = $2`, f.agent.WorkspaceID, contact).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) attention(t *testing.T, contact string) *review.AttentionItem {
	t.Helper()
	items, err := review.Attention(context.Background(), f.pool, f.agent.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if items[i].Contact == contact {
			return &items[i]
		}
	}
	return nil
}

func TestTokensAreHashedAndRevocable(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('W', 'UTC') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	a, token, err := agent.Create(ctx, pool, ws, "  Claude  ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, agent.TokenPrefix) || a.Name != "Claude" || a.Autonomy != "approve" {
		t.Fatalf("agent %+v, token %q", a, token[:4])
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE token_sha256 = convert_to($1, 'UTF8')`, token).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("token stored in the clear: %d %v", stored, err)
	}
	if got, err := agent.Authenticate(ctx, pool, token); err != nil || got.ID != a.ID || got.LastUsedAt == nil {
		t.Fatalf("authenticate = %+v, %v", got, err)
	}
	for _, bad := range []string{"", token + "x", strings.TrimPrefix(token, agent.TokenPrefix)} {
		if _, err := agent.Authenticate(ctx, pool, bad); !errors.Is(err, agent.ErrDenied) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
	if _, _, err := agent.Create(ctx, pool, ws, " ", nil); !errors.Is(err, agent.ErrBadName) {
		t.Errorf("blank name: %v", err)
	}
	if err := agent.Revoke(ctx, pool, ws, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Authenticate(ctx, pool, token); !errors.Is(err, agent.ErrDenied) {
		t.Fatalf("revoked token still works: %v", err)
	}
	list, err := agent.List(ctx, pool, ws)
	if err != nil || len(list) != 1 || list[0].RevokedAt == nil {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := agent.Revoke(ctx, pool, ws, "not-an-id"); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("bad id: %v", err)
	}
}

func TestReadingFencesWhatTheContactWrote(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	waiting, err := agent.Threads(ctx, f.pool, f.agent.WorkspaceID, agent.ThreadFilter{Waiting: true})
	if err != nil {
		t.Fatal(err)
	}
	var dana *agent.ThreadSummary
	for i := range waiting {
		if waiting[i].WaitingSince == nil {
			t.Errorf("%s listed as waiting without a waiting time", waiting[i].Contact.Name)
		}
		if waiting[i].Contact.Name == "Dana Whitfield" {
			dana = &waiting[i]
		}
	}
	if dana == nil || !strings.HasPrefix(dana.LastMessage, "<contact_message>") || !strings.Contains(dana.LastMessage, "garage") {
		t.Fatalf("Dana's summary = %+v", dana)
	}

	thread, err := agent.Thread(ctx, f.pool, f.agent.WorkspaceID, dana.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var fromContact bool
	for _, e := range thread.Events {
		if e.FromContact {
			fromContact = true
			if !strings.HasPrefix(e.Text, "<contact_message>") || !strings.HasPrefix(e.Subject, "<contact_message>") {
				t.Errorf("contact text not fenced: %q / %q", e.Subject, e.Text)
			}
			if _, ok := e.Details["html"]; ok {
				t.Error("raw HTML handed to the agent")
			}
		}
	}
	if !fromContact {
		t.Fatal("no contact message on Dana's thread")
	}

	// Another workspace's agent sees nothing of it.
	var other string
	if err := f.pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Thread(ctx, f.pool, other, dana.ID, false); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("cross-workspace read: %v", err)
	}
	stranger := agent.Agent{ID: f.agent.ID, WorkspaceID: other}
	if _, err := agent.Propose(ctx, f.pool, stranger, dana.ID, agent.Draft{Body: "hi"}); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("cross-workspace draft: %v", err)
	}
}

func TestADraftWaitsForAPersonWhoSendsExactlyIt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dana := f.thread(t, "Dana Whitfield")

	first, err := agent.Propose(ctx, f.pool, f.agent, dana, agent.Draft{Body: "A water heater fits.", Reason: "sizing question"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != "held" || first.Rule != "agent_approve_mode" {
		t.Fatalf("decision = %+v", first)
	}
	second, err := agent.Propose(ctx, f.pool, f.agent, dana, agent.Draft{Body: "A water heater fits. Paint can't go in.", Reason: "sizing and paint"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := agent.GetProposal(ctx, f.pool, f.agent, first.ProposalID); err != nil || p.Status != "voided" {
		t.Fatalf("replaced draft = %+v, %v", p, err)
	}

	item := f.attention(t, "Dana Whitfield")
	if item == nil || item.Kind != review.HeldDraft || item.ID != second.ProposalID {
		t.Fatalf("attention = %+v", item)
	}
	if err := review.Approve(ctx, f.pool, f.person, second.ProposalID, &review.EmailPayload{Subject: "Re: your repair", Body: "Edited by a person"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := f.pool.QueryRow(ctx, `SELECT payload FROM actions WHERE proposal_id = $1`, second.ProposalID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["body"] != "Edited by a person" || payload["subject"] != "Re: your repair" || payload["in_reply_to"] == nil {
		t.Fatalf("approved payload = %v", payload)
	}
	p, err := agent.GetProposal(ctx, f.pool, f.agent, second.ProposalID)
	if err != nil || p.Status != "approved" || p.SendState != "delivered" {
		t.Fatalf("proposal after approval = %+v, %v", p, err)
	}
	mine, err := agent.Proposals(ctx, f.pool, f.agent, "", false)
	if err != nil || len(mine) != 2 {
		t.Fatalf("own proposals = %d, %v", len(mine), err)
	}
}

func TestPolicyDeniesAndHoldsBeforeAnyoneDecides(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dana := f.thread(t, "Dana Whitfield")

	if _, err := agent.Propose(ctx, f.pool, f.agent, dana, agent.Draft{Body: "  "}); !errors.Is(err, agent.ErrEmptyDraft) {
		t.Fatalf("empty draft: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE threads SET taken_over_by = $2 WHERE id = $1`, dana, f.person.ID); err != nil {
		t.Fatal(err)
	}
	held, err := agent.Propose(ctx, f.pool, f.agent, dana, agent.Draft{Body: "hello"})
	if err != nil || held.Rule != "taken_over" {
		t.Fatalf("taken over = %+v, %v", held, err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source) SELECT contact_id, 'email', 'test' FROM threads WHERE id = $1`, dana); err != nil {
		t.Fatal(err)
	}
	denied, err := agent.Propose(ctx, f.pool, f.agent, dana, agent.Draft{Body: "hello again"})
	if err != nil || denied.Decision != "denied" || denied.Rule != "opted_out" || denied.ProposalID != "" {
		t.Fatalf("opted out = %+v, %v", denied, err)
	}
	var drafts int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE thread_id = $1 AND requested_by = $2`, dana, f.agent.ID).Scan(&drafts); err != nil || drafts != 1 {
		t.Fatalf("drafts stored = %d, %v", drafts, err)
	}
}

func TestEscalateReachesAPersonAndResolveCloses(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	linda := f.thread(t, "Linda Park")
	if f.attention(t, "Linda Park") != nil {
		t.Fatal("Linda's resolved thread already needs attention")
	}
	if err := agent.Escalate(ctx, f.pool, f.agent, linda, " "); !errors.Is(err, agent.ErrNoteNeeded) {
		t.Fatalf("escalate without a note: %v", err)
	}
	if err := agent.Escalate(ctx, f.pool, f.agent, linda, "Asked for a refund"); err != nil {
		t.Fatal(err)
	}
	item := f.attention(t, "Linda Park")
	if item == nil || item.Kind != review.Escalated || item.Reason != "Escalated: Asked for a refund" {
		t.Fatalf("attention = %+v", item)
	}
	// A person acting on the thread clears it.
	if err := review.TakeOver(ctx, f.pool, f.person, linda); err != nil {
		t.Fatal(err)
	}
	if f.attention(t, "Linda Park") != nil {
		t.Fatal("still listed after a person took over")
	}

	if err := agent.Resolve(ctx, f.pool, f.agent, linda, "done"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM threads WHERE id = $1`, linda).Scan(&state); err != nil || state != "resolved" {
		t.Fatalf("state = %s, %v", state, err)
	}
}

func TestEventsResumeFromACursor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	all, next, err := agent.Events(ctx, f.pool, f.agent.WorkspaceID, 0, 0)
	if err != nil || len(all) == 0 {
		t.Fatalf("events = %d, %v", len(all), err)
	}
	if err := agent.Escalate(ctx, f.pool, f.agent, f.thread(t, "Linda Park"), "check this"); err != nil {
		t.Fatal(err)
	}
	newer, _, err := agent.Events(ctx, f.pool, f.agent.WorkspaceID, next, 0)
	if err != nil || len(newer) != 1 || newer[0].Type != "thread.escalated" || newer[0].Actor != "Inbox helper" {
		raw, _ := json.Marshal(newer)
		t.Fatalf("after cursor = %s, %v", raw, err)
	}
	if found, err := agent.SearchContacts(ctx, f.pool, f.agent.WorkspaceID, "whitfield"); err != nil || len(found) != 1 || found[0].LatestThread == "" {
		t.Fatalf("search = %+v, %v", found, err)
	}
	if found, _ := agent.SearchContacts(ctx, f.pool, f.agent.WorkspaceID, "%"); len(found) != 0 {
		t.Fatalf("a lone wildcard matched %d contacts", len(found))
	}
}

func TestATextReachesWhoeverCalledFromTheNumber(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ws := f.agent.WorkspaceID
	const local, caller = "+13105550100", "+13105550177"
	var connector, contact string
	if err := f.pool.QueryRow(ctx, `INSERT INTO connectors (workspace_id, kind, name, settings, health) VALUES ($1, 'signalwire', 'n', '{}', 'ok') RETURNING id`, ws).Scan(&connector); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO phone_numbers (number, workspace_id, connector_id, provider_project_id, capabilities) VALUES ($1, $2, $3, 'p', '{"sms":true,"voice":true}')`, local, ws, connector); err != nil {
		t.Fatal(err)
	}
	// Someone has called, but never texted.
	if err := f.pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`, ws, caller).Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'voice', $3)`, ws, contact, caller); err != nil {
		t.Fatal(err)
	}

	out, err := agent.Compose(ctx, f.pool, f.agent, agent.Message{Channel: "sms", To: caller, Name: "Rosa", Draft: agent.Draft{Body: "Following up on your call", Reason: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.ContactID != contact {
		t.Fatalf("text went to a new contact %s, not the caller %s", out.ContactID, contact)
	}
	in, err := ingest.ReceiveSMS(ctx, f.pool, ingest.SMS{SID: "SM-1", From: caller, To: local, Body: "Thanks", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if in.ContactID != contact || in.ThreadID != out.ThreadID {
		t.Fatalf("their reply landed on contact %s thread %s", in.ContactID, in.ThreadID)
	}
	var contacts int
	if err := f.pool.QueryRow(ctx, `SELECT count(DISTINCT contact_id) FROM identities WHERE workspace_id = $1 AND address = $2`, ws, caller).Scan(&contacts); err != nil {
		t.Fatal(err)
	}
	if contacts != 1 {
		t.Fatalf("the number belongs to %d contacts", contacts)
	}
}
