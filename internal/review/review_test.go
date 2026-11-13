package review_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/demo"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func setup(t *testing.T) (*pgxpool.Pool, review.Reviewer) {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	seeded, err := demo.Seed(ctx, pool, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Reviewer') RETURNING id`,
		seeded.WorkspaceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return pool, review.Reviewer{ID: id, WorkspaceID: seeded.WorkspaceID}
}

func attention(t *testing.T, pool *pgxpool.Pool, r review.Reviewer) []review.AttentionItem {
	t.Helper()
	items, err := review.Attention(context.Background(), pool, r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func find(items []review.AttentionItem, contact string) *review.AttentionItem {
	for i := range items {
		if items[i].Contact == contact {
			return &items[i]
		}
	}
	return nil
}

func TestAttentionListsEveryKindByDeadline(t *testing.T) {
	pool, r := setup(t)
	items := attention(t, pool, r)
	want := []struct{ contact, kind string }{
		{"Dana Whitfield", review.Unanswered},
		{"Marcus Lee", review.HeldDraft},
		{"Priya Nair", review.FailedRun},
		{"Kelly Brooks", review.HeldDraft},
		{"Jorge Ramos", review.HeldDraft},
		{"Tom Alvarez", review.UnknownSend},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items: %+v", len(items), items)
	}
	for i, w := range want {
		if items[i].Contact != w.contact || items[i].Kind != w.kind {
			t.Errorf("item %d = %s %s, want %s %s", i, items[i].Contact, items[i].Kind, w.contact, w.kind)
		}
	}
	if find(items, "Linda Park") != nil {
		t.Error("a resolved, answered thread is listed")
	}
	if got := find(items, "Dana Whitfield").Reason; got != "No reply yet: possible gas leak" {
		t.Errorf("Dana's reason = %q", got)
	}
}

func TestApproveSendsThroughTheSandboxAndClearsTheItem(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	marcus := find(attention(t, pool, r), "Marcus Lee")
	edit := &review.EmailPayload{Subject: "Re: water heater", Body: "Edited by a person"}
	if err := review.Approve(ctx, pool, r, marcus.ID, edit); err != nil {
		t.Fatal(err)
	}
	if find(attention(t, pool, r), "Marcus Lee") != nil {
		t.Fatal("approved draft still needs attention")
	}
	v, err := review.Thread(ctx, pool, r.WorkspaceID, marcus.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	last := v.Timeline[len(v.Timeline)-1]
	var sent struct {
		Text    string `json:"text"`
		Sandbox bool   `json:"sandbox"`
	}
	_ = json.Unmarshal(last.Payload, &sent)
	if last.Type != "email.sent" || sent.Text != "Edited by a person" || !sent.Sandbox || v.State != "waiting" {
		t.Fatalf("last event %s %+v, thread state %s", last.Type, sent, v.State)
	}
	if err := review.Approve(ctx, pool, r, marcus.ID, nil); !errors.Is(err, review.ErrConflict) {
		t.Fatalf("second approval: %v", err)
	}
}

func TestOptOutBlocksApproval(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	jorge := find(attention(t, pool, r), "Jorge Ramos")
	if _, err := pool.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source)
		SELECT contact_id, 'email', 'test' FROM threads WHERE id = $1`, jorge.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := review.Approve(ctx, pool, r, jorge.ID, nil); !errors.Is(err, review.ErrOptedOut) {
		t.Fatalf("err = %v", err)
	}
}

func TestApprovingNewInfoVoidsDraftsThatQuotedTheOld(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	versions, err := review.ApprovedInfoVersions(ctx, pool, r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if versions[0].Version != 13 || versions[0].PendingQuoting != 3 {
		t.Fatalf("newest = v%d quoting %d", versions[0].Version, versions[0].PendingQuoting)
	}
	voided, err := review.ApproveInfo(ctx, pool, r, versions[0].ID)
	if err != nil || voided != 3 {
		t.Fatalf("voided %d, err %v", voided, err)
	}
	for _, it := range attention(t, pool, r) {
		if it.Kind == review.HeldDraft {
			t.Errorf("held draft for %s survived the price change", it.Contact)
		}
	}
}

func TestTakeOverIsExclusive(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	kelly := find(attention(t, pool, r), "Kelly Brooks")
	if err := review.TakeOver(ctx, pool, r, kelly.ThreadID); err != nil {
		t.Fatal(err)
	}
	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Other') RETURNING id`,
		r.WorkspaceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	o := review.Reviewer{ID: other, WorkspaceID: r.WorkspaceID}
	if err := review.TakeOver(ctx, pool, o, kelly.ThreadID); !errors.Is(err, review.ErrConflict) {
		t.Fatalf("second take-over: %v", err)
	}
	if err := review.Approve(ctx, pool, o, kelly.ID, nil); !errors.Is(err, review.ErrConflict) {
		t.Fatalf("approval by someone else during take-over: %v", err)
	}
	if err := review.HandBack(ctx, pool, r, kelly.ThreadID); err != nil {
		t.Fatal(err)
	}
}

func TestMarkDoneTakesAThreadOffTheListUntilReopened(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	if err := review.Resolve(ctx, pool, r, find(attention(t, pool, r), "Kelly Brooks").ThreadID); !errors.Is(err, review.ErrHeldDraft) {
		t.Fatalf("marking done with a held draft: %v", err)
	}
	dana := find(attention(t, pool, r), "Dana Whitfield")
	onList := func() bool {
		for _, it := range attention(t, pool, r) {
			if it.ThreadID == dana.ThreadID {
				return true
			}
		}
		return false
	}
	if err := review.Resolve(ctx, pool, r, dana.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := review.Resolve(ctx, pool, r, dana.ThreadID); err != nil {
		t.Fatalf("marking done twice: %v", err)
	}
	if onList() {
		t.Error("a thread marked done is still on the needs-attention list")
	}
	if err := review.Reopen(ctx, pool, r, dana.ThreadID); err != nil {
		t.Fatal(err)
	}
	if !onList() {
		t.Error("a reopened thread is not back on the list")
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = $1 AND type IN ('thread.resolved', 'thread.reopened')`,
		dana.ThreadID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("%d state events, err %v; want one per change", events, err)
	}
	stranger := review.Reviewer{ID: r.ID, WorkspaceID: "00000000-0000-0000-0000-000000000000"}
	if err := review.Resolve(ctx, pool, stranger, dana.ThreadID); !errors.Is(err, review.ErrNotFound) {
		t.Fatalf("marking done from another workspace: %v", err)
	}
}

func TestOtherWorkspacesAreInvisible(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	marcus := find(attention(t, pool, r), "Marcus Lee")
	stranger := review.Reviewer{ID: r.ID, WorkspaceID: "00000000-0000-0000-0000-000000000000"}
	if _, err := review.Thread(ctx, pool, stranger.WorkspaceID, marcus.ThreadID); !errors.Is(err, review.ErrNotFound) {
		t.Fatalf("thread from another workspace: %v", err)
	}
	if err := review.Approve(ctx, pool, stranger, marcus.ID, nil); !errors.Is(err, review.ErrConflict) {
		t.Fatalf("approval from another workspace: %v", err)
	}
}

func TestSetupReadsWhatIsConfigured(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Acme Repairs & Co.', 'UTC') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	state := func() map[string]string {
		st, err := review.Setup(ctx, pool, ws)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, s := range st.Steps {
			out[s.Key] = s.State
		}
		if st.Complete {
			out["complete"] = "yes"
		}
		return out
	}
	if got := state(); got["inbox"] != "todo" || got["sending"] != "todo" || got["approved_info"] != "todo" || got["agent"] != "todo" || got["complete"] != "" {
		t.Fatalf("new workspace: %v", got)
	}

	addr, err := review.CreateInbox(ctx, pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, "in+acme-repairs-co-") || !strings.HasSuffix(addr, "@"+review.InboundDomain) {
		t.Fatalf("address %q", addr)
	}
	again, err := review.CreateInbox(ctx, pool, ws)
	if err != nil || again != addr {
		t.Fatalf("second CreateInbox = %q, %v; want the same address", again, err)
	}
	for range 2 {
		if err := review.UseSandbox(ctx, pool, ws); err != nil {
			t.Fatal(err)
		}
	}
	got := state()
	if got["inbox"] != "done" || got["sending"] != "partial" {
		t.Fatalf("after inbox and sandbox: %v", got)
	}
	var sandboxes int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM connectors WHERE workspace_id = $1`, ws).Scan(&sandboxes)
	if sandboxes != 1 {
		t.Fatalf("%d sandbox connectors", sandboxes)
	}
}

func TestInboxAddressesAreUnguessable(t *testing.T) {
	a, b := review.NewInboxAddress("Acme Plumbing"), review.NewInboxAddress("Acme Plumbing")
	if a == b {
		t.Fatalf("two workspaces named Acme Plumbing got the same address %q", a)
	}
	if got := review.NewInboxAddress("!!!"); !strings.HasPrefix(got, "in+inbox-") {
		t.Fatalf("name with no letters: %q", got)
	}
}

func TestDemoSetupIsCompleteExceptRealSending(t *testing.T) {
	pool, r := setup(t)
	st, err := review.Setup(context.Background(), pool, r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st.Steps {
		want := "done"
		if s.Key == "sending" {
			want = "partial"
		}
		if s.State != want {
			t.Errorf("%s = %s, want %s", s.Key, s.State, want)
		}
	}
	if st.Complete {
		t.Error("a sandbox-only workspace counts as set up")
	}
}

func TestReplySendsThroughTheSandboxAndVoidsHeldDrafts(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	marcus := find(attention(t, pool, r), "Marcus Lee")
	res, err := review.Reply(ctx, pool, r, marcus.ThreadID, review.EmailPayload{Body: "Hi Marcus, I'll call you this afternoon."})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "delivered" {
		t.Fatalf("state = %s, want delivered through the sandbox", res.State)
	}
	if find(attention(t, pool, r), "Marcus Lee") != nil {
		t.Fatal("thread still needs attention after a person replied")
	}
	v, err := review.Thread(ctx, pool, r.WorkspaceID, marcus.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Proposals) != 0 {
		t.Fatal("the agent's held draft survived the person's reply")
	}
	last := v.Timeline[len(v.Timeline)-1]
	var sent struct {
		Subject string `json:"subject"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(last.Payload, &sent)
	if last.Type != "email.sent" || last.Actor != "Reviewer" || sent.Subject != "Re: Rough-in for a bathroom remodel next Tuesday?" {
		t.Fatalf("last event %s by %q: %+v", last.Type, last.Actor, sent)
	}
}

func TestReplyRefusals(t *testing.T) {
	pool, r := setup(t)
	ctx := context.Background()
	dana := find(attention(t, pool, r), "Dana Whitfield")
	if _, err := review.Reply(ctx, pool, r, dana.ThreadID, review.EmailPayload{Body: "  "}); !errors.Is(err, review.ErrEmptyReply) {
		t.Errorf("empty reply: %v", err)
	}
	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Other') RETURNING id`, r.WorkspaceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := review.TakeOver(ctx, pool, review.Reviewer{ID: other, WorkspaceID: r.WorkspaceID}, dana.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := review.Reply(ctx, pool, r, dana.ThreadID, review.EmailPayload{Body: "hi"}); !errors.Is(err, review.ErrConflict) {
		t.Errorf("reply while someone else holds the thread: %v", err)
	}

	// A workspace with no way to send says so instead of pretending.
	var ws, person, contact, thread string
	_ = pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Bare', 'UTC') RETURNING id`).Scan(&ws)
	_ = pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'P') RETURNING id`, ws).Scan(&person)
	_ = pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id) VALUES ($1) RETURNING id`, ws).Scan(&contact)
	_, _ = pool.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'email', 'a@example.com')`, ws, contact)
	_ = pool.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel) VALUES ($1, $2, 'email') RETURNING id`, ws, contact).Scan(&thread)
	if _, err := review.Reply(ctx, pool, review.Reviewer{ID: person, WorkspaceID: ws}, thread, review.EmailPayload{Body: "hi"}); !errors.Is(err, review.ErrNoSender) {
		t.Errorf("no sender: %v", err)
	}
}

func TestSelfHostedInboxUsesConfiguredDomain(t *testing.T) {
	pool := testutil.DB(t)
	var ws string
	if err := pool.QueryRow(context.Background(), `INSERT INTO workspaces (name,timezone) VALUES ('Self hosted','UTC') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	address, err := review.CreateInbox(context.Background(), pool, ws, "mail.example.org")
	if err != nil || !strings.HasSuffix(address, "@mail.example.org") {
		t.Fatalf("inbox=%q,%v", address, err)
	}
	if _, err := review.CreateInbox(context.Background(), pool, ws, "https://wrong.example"); err == nil {
		t.Fatal("URL accepted as an inbound domain")
	}
}
