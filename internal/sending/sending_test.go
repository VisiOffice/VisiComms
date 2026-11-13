package sending

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/demo"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

type fakeProvider struct {
	status string
	sent   [][]byte
	err    error
	onSend func()
}

func (f *fakeProvider) RegisterDomain(context.Context, string) (Identity, error) {
	return Identity{Status: f.status, DKIMTokens: []string{"tok1", "tok2", "tok3"}}, nil
}

func (f *fakeProvider) DomainStatus(context.Context, string) (Identity, error) {
	return Identity{Status: f.status, DKIMTokens: []string{"tok1", "tok2", "tok3"}}, nil
}

func (f *fakeProvider) Send(_ context.Context, raw []byte, _ map[string]string) (string, error) {
	if f.onSend != nil {
		f.onSend()
	}
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, raw)
	return "0100018f-fake-ses-id", nil
}

func TestSendResultSurvivesCallerCancellation(t *testing.T) {
	w := newWorld(t)
	actionID, _ := w.approve(t, "Marcus Lee")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.provider.onSend = cancel
	if out, err := Dispatch(ctx, w.pool, w.provider, actionID); err != nil || out != Sent {
		t.Fatalf("Dispatch = %s, %v", out, err)
	}
	if state := w.state(t, actionID); state != "accepted" {
		t.Fatalf("state = %s, want accepted despite cancelled caller", state)
	}
	var mid string
	if err := w.pool.QueryRow(context.Background(), `SELECT provider_message_id FROM delivery_attempts WHERE action_id = $1`, actionID).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if mid != "0100018f-fake-ses-id" {
		t.Fatalf("provider message ID = %q", mid)
	}
}

// world is a demo workspace sending through SES as quotes@acmeplumbing.example.
type world struct {
	pool     *pgxpool.Pool
	r        review.Reviewer
	provider *fakeProvider
}

func newWorld(t *testing.T) world {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	seeded, err := demo.Seed(ctx, pool, "Acme Plumbing")
	if err != nil {
		t.Fatal(err)
	}
	var person string
	if err := pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Reviewer') RETURNING id`, seeded.WorkspaceID).Scan(&person); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{status: "SUCCESS"}
	d, err := AddDomain(ctx, pool, p, seeded.WorkspaceID, "AcmePlumbing.example")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(d.Records[0].Value, "visicomms-verify=")
	orig := LookupTXT
	t.Cleanup(func() { LookupTXT = orig })
	LookupTXT = func(_ context.Context, name string) ([]string, error) {
		if name == "_visicomms.acmeplumbing.example" {
			return []string{"visicomms-verify=" + token}, nil
		}
		return nil, errors.New("no such record")
	}
	if d, err = CheckDomain(ctx, pool, p, seeded.WorkspaceID, d.ID); err != nil || !d.Verified {
		t.Fatalf("domain not verified: %+v %v", d, err)
	}
	if err := SetFrom(ctx, pool, seeded.WorkspaceID, From{Name: "Acme Plumbing", Address: "Quotes@AcmePlumbing.example"}); err != nil {
		t.Fatal(err)
	}
	return world{pool: pool, r: review.Reviewer{ID: person, WorkspaceID: seeded.WorkspaceID}, provider: p}
}

func (w world) item(t *testing.T, contact string) review.AttentionItem {
	t.Helper()
	items, err := review.Attention(context.Background(), w.pool, w.r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Contact == contact {
			return it
		}
	}
	t.Fatalf("%s not in attention", contact)
	return review.AttentionItem{}
}

func (w world) approve(t *testing.T, contact string) (actionID, threadID string) {
	t.Helper()
	ctx := context.Background()
	it := w.item(t, contact)
	if err := review.Approve(ctx, w.pool, w.r, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT id FROM actions WHERE proposal_id = $1`, it.ID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	return actionID, it.ThreadID
}

func (w world) state(t *testing.T, actionID string) string {
	t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `SELECT state FROM actions WHERE id = $1`, actionID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOwnershipIsRequiredBeforeSendingAsADomain(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws string
	_ = pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Impostor', 'UTC') RETURNING id`).Scan(&ws)
	// SES already trusts the domain (another workspace verified it), but this
	// workspace has not published its own record.
	p := &fakeProvider{status: "SUCCESS"}
	orig := LookupTXT
	t.Cleanup(func() { LookupTXT = orig })
	LookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"visicomms-verify=someone-elses-token"}, nil
	}
	d, err := AddDomain(ctx, pool, p, ws, "acmeplumbing.example")
	if err != nil {
		t.Fatal(err)
	}
	if d, err = CheckDomain(ctx, pool, p, ws, d.ID); err != nil || d.Verified || d.OwnershipChecked {
		t.Fatalf("verified without its own ownership record: %+v %v", d, err)
	}
	if err := SetFrom(ctx, pool, ws, From{Address: "quotes@acmeplumbing.example"}); !errors.Is(err, ErrDomainNotReady) {
		t.Fatalf("SetFrom on an unproven domain: %v", err)
	}
	if _, err := AddDomain(ctx, pool, p, ws, "not a domain"); !errors.Is(err, ErrBadDomain) {
		t.Fatalf("bad domain: %v", err)
	}
}

func TestApprovedDraftIsSentOnceThroughSES(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, threadID := w.approve(t, "Marcus Lee")
	if s := w.state(t, actionID); s != "queued" {
		t.Fatalf("after approval the SES action is %s, want queued for the executor", s)
	}
	out, err := Dispatch(ctx, w.pool, w.provider, actionID)
	if err != nil || out != Sent {
		t.Fatalf("Dispatch = %s, %v", out, err)
	}
	if len(w.provider.sent) != 1 {
		t.Fatalf("%d messages sent", len(w.provider.sent))
	}
	raw := string(w.provider.sent[0])
	for _, want := range []string{"From: \"Acme Plumbing\" <quotes@acmeplumbing.example>", "To: <marcus@roofco.example>", "Subject: Re: Rough-in for a bathroom remodel next Tuesday?", "between 9 and 11"} {
		if !strings.Contains(raw, want) {
			t.Errorf("message missing %q:\n%s", want, raw)
		}
	}
	if s := w.state(t, actionID); s != "accepted" {
		t.Fatalf("state = %s", s)
	}
	var mid, threadState string
	_ = w.pool.QueryRow(ctx, `SELECT provider_message_id FROM delivery_attempts WHERE action_id = $1`, actionID).Scan(&mid)
	_ = w.pool.QueryRow(ctx, `SELECT state FROM threads WHERE id = $1`, threadID).Scan(&threadState)
	if mid != "0100018f-fake-ses-id" || threadState != "waiting" {
		t.Fatalf("message id %q, thread %s", mid, threadState)
	}

	// Running the job again must not send again.
	if out, err := Dispatch(ctx, w.pool, w.provider, actionID); err != nil || out != Skipped || len(w.provider.sent) != 1 {
		t.Fatalf("second Dispatch = %s, %v, %d sent", out, err, len(w.provider.sent))
	}
}

func TestReplyIsSentAsTheCustomersThread(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	dana := w.item(t, "Dana Whitfield")
	res, err := review.Reply(ctx, w.pool, w.r, dana.ThreadID, review.EmailPayload{Body: "Hi Dana, paint can't go in, but Mesa takes it free."})
	if err != nil || res.State != "queued" {
		t.Fatalf("Reply = %+v, %v", res, err)
	}
	if out, err := Dispatch(ctx, w.pool, w.provider, res.ActionID); err != nil || out != Sent {
		t.Fatalf("Dispatch = %s, %v (the reply's approval hash must survive jsonb)", out, err)
	}
	if raw := string(w.provider.sent[0]); !strings.Contains(raw, "In-Reply-To: <demo-") {
		t.Fatalf("reply not threaded:\n%s", raw)
	}
}

func TestProviderRefusalAndSilence(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	refused, _ := w.approve(t, "Marcus Lee")
	w.provider.err = &Rejected{Reason: "MessageRejected: Email address is not verified."}
	if out, _ := Dispatch(ctx, w.pool, w.provider, refused); out != Failed || w.state(t, refused) != "failed" {
		t.Fatalf("refusal: %s / %s", out, w.state(t, refused))
	}

	silent, _ := w.approve(t, "Kelly Brooks")
	w.provider.err = errors.New("context deadline exceeded")
	if out, _ := Dispatch(ctx, w.pool, w.provider, silent); out != Unknown || w.state(t, silent) != "unknown" {
		t.Fatalf("timeout: %s / %s", out, w.state(t, silent))
	}
	// An unknown send is surfaced for a person and never retried.
	items, _ := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	found := false
	for _, it := range items {
		if it.Kind == review.UnknownSend && it.ID == silent {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown send not in Needs attention")
	}
}

func TestChecksJustBeforeSending(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	optOut, thread := w.approve(t, "Marcus Lee")
	_, _ = w.pool.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source) SELECT contact_id, 'email', 'test' FROM threads WHERE id = $1`, thread)
	if out, _ := Dispatch(ctx, w.pool, w.provider, optOut); out != Cancelled {
		t.Errorf("opt-out after approval: %s", out)
	}

	takeOver, thread := w.approve(t, "Kelly Brooks")
	var other string
	_ = w.pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Other') RETURNING id`, w.r.WorkspaceID).Scan(&other)
	_, _ = w.pool.Exec(ctx, `UPDATE threads SET taken_over_by = $2 WHERE id = $1`, thread, other)
	if out, _ := Dispatch(ctx, w.pool, w.provider, takeOver); out != Cancelled {
		t.Errorf("take-over after approval: %s", out)
	}

	infoChanged, _ := w.approve(t, "Jorge Ramos")
	var v13 string
	_ = w.pool.QueryRow(ctx, `SELECT id FROM approved_info_versions WHERE workspace_id = $1 AND version = 13`, w.r.WorkspaceID).Scan(&v13)
	if _, err := review.ApproveInfo(ctx, w.pool, w.r, v13); err != nil {
		t.Fatal(err)
	}
	if out, _ := Dispatch(ctx, w.pool, w.provider, infoChanged); out != Cancelled {
		t.Errorf("approved info changed after approval: %s", out)
	}
	if len(w.provider.sent) != 0 {
		t.Fatalf("%d messages sent despite the checks", len(w.provider.sent))
	}
}

func TestSweepReportsAnInterruptedSendAsUnknown(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, _ := w.approve(t, "Marcus Lee")
	_, _ = w.pool.Exec(ctx, `UPDATE actions SET state = 'dispatching', updated_at = now() - interval '11 minutes' WHERE id = $1`, actionID)
	if err := (&sweepWorker{pool: w.pool}).Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s := w.state(t, actionID); s != "unknown" {
		t.Fatalf("stuck send is %s, want unknown", s)
	}
}
