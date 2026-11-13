package sending

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/riverqueue/river"
)

const smsLocal = "+14155550123"
const smsRemote = "+14155550999"
const smsProject = "project-test"

func smsProvider(t *testing.T, send resendTransport) *SignalWire {
	t.Helper()
	sw, err := NewSignalWire("test.signalwire.com", smsProject, "token", "signing-key", "https://comms.example.com")
	if err != nil {
		t.Fatal(err)
	}
	sw.HTTP = &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		project, token, _ := r.BasicAuth()
		if project != smsProject || token != "token" || r.URL.Host != "test.signalwire.com" {
			t.Fatal("incorrect provider authentication")
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/IncomingPhoneNumbers.json") {
			return resendResponse(200, `{"incoming_phone_numbers":[{"sid":"PN1","account_sid":"project-test","phone_number":"+14155550123","capabilities":{"sms":true,"voice":true,"fax":true}}]}`), nil
		}
		return send(r)
	})}
	return sw
}
func smsThread(t *testing.T, w world, sw *SignalWire) string {
	t.Helper()
	ctx := context.Background()
	if err := ConnectSMSNumber(ctx, w.pool, sw, w.r.WorkspaceID, smsLocal); err != nil {
		t.Fatal(err)
	}
	in, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "SM-in", From: smsRemote, To: smsLocal, Body: "Can you help?", Project: smsProject})
	if err != nil {
		t.Fatal(err)
	}
	return in.ThreadID
}
func smsReply(t *testing.T, w world, thread string) string {
	t.Helper()
	w.r.SMSProject = smsProject
	res, err := review.Reply(context.Background(), w.pool, w.r, thread, review.EmailPayload{Body: "Hello from SMS"})
	if err != nil {
		t.Fatal(err)
	}
	return res.ActionID
}

func TestSMSReplyUsesOriginalNumberAndSharedExecutor(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	calls := 0
	sw := smsProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/Messages.json") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("From") != smsLocal || r.PostForm.Get("To") != smsRemote || r.PostForm.Get("Body") != "Hello from SMS" || r.PostForm.Get("StatusCallback") != "https://comms.example.com/webhooks/signalwire/sms/status" {
			t.Fatalf("wrong route: %v", r.PostForm)
		}
		// The callback can arrive before the API response is recorded.
		if err := ApplySMSStatus(ctx, w.pool, SMSStatus{SID: "SM-out", Status: "delivered", Project: smsProject}); err != nil {
			t.Fatal(err)
		}
		return resendResponse(201, `{"sid":"SM-out","status":"queued"}`), nil
	})
	thread := smsThread(t, w, sw)
	actionID := smsReply(t, w, thread)
	if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Sent || w.state(t, actionID) != "delivered" {
		t.Fatalf("DispatchSMS=%s,%v state=%s", out, err, w.state(t, actionID))
	}
	if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Skipped || calls != 1 {
		t.Fatalf("duplicate: %s,%v calls=%d", out, err, calls)
	}
	view, err := review.Thread(ctx, w.pool, w.r.WorkspaceID, thread)
	if err != nil || view.Channel != "sms" || view.PhoneNumber != smsLocal || view.RemoteAddress != smsRemote {
		t.Fatalf("thread=%+v,%v", view, err)
	}
	for _, e := range view.Timeline {
		if strings.HasPrefix(e.Type, "email.") {
			t.Fatal("SMS recorded as email")
		}
	}
	items, err := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ThreadID == thread && item.Kind == review.Unanswered {
			t.Fatal("accepted reply still unanswered")
		}
	}
}

func TestSMSStopCancelsQueuedRepliesAndStartIsScoped(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { t.Fatal("STOP allowed a send"); return nil, nil })
	thread := smsThread(t, w, sw)
	actionID := smsReply(t, w, thread)
	w.r.SMSProject = smsProject
	stop := ingest.SMS{SID: "SM-stop", From: smsRemote, To: smsLocal, Body: " stop ", Project: smsProject, MediaCount: 1}
	first, err := ingest.ReceiveSMS(ctx, w.pool, stop)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ingest.ReceiveSMS(ctx, w.pool, stop)
	if err != nil || !again.Duplicate || again.EventID != first.EventID {
		t.Fatalf("retry=%+v,%v", again, err)
	}
	if w.state(t, actionID) != "cancelled" {
		t.Fatal("queued send survived STOP")
	}
	if _, err := review.Reply(ctx, w.pool, w.r, thread, review.EmailPayload{Body: "blocked"}); !errors.Is(err, review.ErrOptedOut) {
		t.Fatalf("reply after STOP: %v", err)
	}
	if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Skipped {
		t.Fatalf("cancelled dispatch=%s,%v", out, err)
	}
	// SMS STOP does not opt this contact out of email.
	var emailOut bool
	if err := w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM opt_outs WHERE contact_id=$1 AND channel='email')`, first.ContactID).Scan(&emailOut); err != nil || emailOut {
		t.Fatalf("email incorrectly blocked: %v,%v", emailOut, err)
	}
	stop.SID, stop.Body = "SM-start", "START"
	if _, err := ingest.ReceiveSMS(ctx, w.pool, stop); err != nil {
		t.Fatal(err)
	}
	smsReply(t, w, thread)
	// A manual block must survive START.
	if _, err := w.pool.Exec(ctx, `INSERT INTO opt_outs(contact_id,channel,source) VALUES ($1,'sms','human block')`, first.ContactID); err != nil {
		t.Fatal(err)
	}
	stop.SID = "SM-start-2"
	if _, err := ingest.ReceiveSMS(ctx, w.pool, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := review.Reply(ctx, w.pool, w.r, thread, review.EmailPayload{Body: "blocked"}); !errors.Is(err, review.ErrOptedOut) {
		t.Fatalf("START cleared human block: %v", err)
	}
}

func TestSMSChecksPauseTakeoverAndBoundPayload(t *testing.T) {
	for _, kind := range []string{"pause", "takeover", "payload", "recipient", "project"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			sw := smsProvider(t, func(*http.Request) (*http.Response, error) { t.Fatal("blocked send reached provider"); return nil, nil })
			thread := smsThread(t, w, sw)
			actionID := smsReply(t, w, thread)
			var err error
			switch kind {
			case "pause":
				_, err = w.pool.Exec(ctx, `UPDATE workspaces SET sending_paused_at=now() WHERE id=$1`, w.r.WorkspaceID)
			case "takeover":
				_, err = w.pool.Exec(ctx, `UPDATE threads SET taken_over_by=(SELECT requested_by FROM proposals LIMIT 1) WHERE id=$1`, thread)
			case "payload":
				_, err = w.pool.Exec(ctx, `UPDATE actions SET payload=jsonb_set(payload,'{body}','"changed"') WHERE id=$1`, actionID)
			case "recipient":
				_, err = w.pool.Exec(ctx, `UPDATE threads SET remote_address='+14155550888' WHERE id=$1`, thread)
			case "project":
				sw.Project = "another-project"
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Cancelled {
				t.Fatalf("DispatchSMS=%s,%v", out, err)
			}
		})
	}
}

func TestSMSFailureAndUnknownAreNotRetried(t *testing.T) {
	for _, code := range []int{422, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			w := newWorld(t)
			calls := 0
			sw := smsProvider(t, func(*http.Request) (*http.Response, error) { calls++; return resendResponse(code, `{}`), nil })
			thread := smsThread(t, w, sw)
			actionID := smsReply(t, w, thread)
			want := Failed
			if code == 500 {
				want = Unknown
			}
			if out, err := DispatchSMS(context.Background(), w.pool, sw, actionID); err != nil || out != want {
				t.Fatalf("DispatchSMS=%s,%v", out, err)
			}
			if out, err := DispatchSMS(context.Background(), w.pool, sw, actionID); err != nil || out != Skipped || calls != 1 {
				t.Fatalf("retry=%s,%v calls=%d", out, err, calls)
			}
		})
	}
}

func TestSignalWireNumberCannotBeClaimedByAnotherWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected send"); return nil, nil })
	smsThread(t, w, sw)
	if err := ConnectSMSNumber(ctx, w.pool, sw, w.r.WorkspaceID, smsLocal); err != nil {
		t.Fatalf("reconnecting own number: %v", err)
	}

	var another string
	if err := w.pool.QueryRow(ctx, `INSERT INTO workspaces(name,timezone) VALUES('Other','UTC') RETURNING id`).Scan(&another); err != nil {
		t.Fatal(err)
	}
	if err := ConnectSMSNumber(ctx, w.pool, sw, another, smsLocal); !errors.Is(err, ErrNumberAssigned) {
		t.Fatalf("reassignment=%v", err)
	}
	if err := ConnectSMSNumber(ctx, w.pool, sw, w.r.WorkspaceID, "+14155550000"); !errors.Is(err, ErrSMSNumber) {
		t.Fatalf("unowned number=%v", err)
	}
}

func TestSignalWireCompatibilitySignature(t *testing.T) {
	sw, err := NewSignalWire("test.signalwire.com", smsProject, "token", "key", "https://comms.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Independent construction in the sorted-form format.
	mac := hmac.New(sha1.New, []byte("key"))
	mac.Write([]byte("https://comms.example.com/webhooks/signalwire/smsBodyHelloFrom+14155550999To+14155550123"))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	form := url.Values{"To": {smsLocal}, "Body": {"Hello"}, "From": {smsRemote}}
	if !sw.Verify("/webhooks/signalwire/sms", signature, form) {
		t.Fatal("valid form rejected")
	}
	if sw.Verify("/webhooks/signalwire/sms/status", signature, form) {
		t.Fatal("signature reused on another route")
	}
	form.Set("Body", "changed")
	if sw.Verify("/webhooks/signalwire/sms", signature, form) {
		t.Fatal("tampered form accepted")
	}
}

func TestSMSDeliveryEventsDoNotRegressTerminalState(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"SM-out"}`), nil })
	thread := smsThread(t, w, sw)
	actionID := smsReply(t, w, thread)
	if _, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"delivered", "sent", "queued", "failed", "delivered"} {
		if err := ApplySMSStatus(ctx, w.pool, SMSStatus{SID: "SM-out", Status: status, Project: smsProject}); err != nil {
			t.Fatal(err)
		}
	}
	if w.state(t, actionID) != "delivered" {
		t.Fatal("terminal state regressed")
	}
}

func TestSMSLengthAndWorkspaceIsolation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"SM-out"}`), nil })
	thread := smsThread(t, w, sw)
	w.r.SMSProject = smsProject
	if _, err := review.Reply(ctx, w.pool, w.r, thread, review.EmailPayload{Body: strings.Repeat("a", 1601)}); !errors.Is(err, review.ErrSMSLength) {
		t.Fatalf("long SMS=%v", err)
	}
	other := w.r
	other.WorkspaceID = "00000000-0000-0000-0000-000000000000"
	if _, err := review.Reply(ctx, w.pool, other, thread, review.EmailPayload{Body: "hello"}); !errors.Is(err, review.ErrNotFound) {
		t.Fatalf("cross-workspace reply=%v", err)
	}
	if _, err := review.Thread(ctx, w.pool, other.WorkspaceID, thread); !errors.Is(err, review.ErrNotFound) {
		t.Fatalf("cross-workspace read=%v", err)
	}
	// No automatic email/phone contact merging occurs.
	var identities int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE contact_id=(SELECT contact_id FROM threads WHERE id=$1) AND channel='email'`, thread).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("email identity merged: %d,%v", identities, err)
	}
}

func TestSMSIngestionIsConcurrentAndIdempotent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return nil, nil })
	smsThread(t, w, sw)
	results := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "same-message", From: smsRemote, To: smsLocal, Body: "hello", Project: smsProject})
			results <- err
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE provider='signalwire_sms' AND provider_ref='same-message'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events=%d,%v", n, err)
	}
}

func TestSMSCallerCancellationStillRecordsSend(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) {
		cancel()
		return resendResponse(201, `{"sid":"SM-out"}`), nil
	})
	thread := smsThread(t, w, sw)
	actionID := smsReply(t, w, thread)
	if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Sent || w.state(t, actionID) != "accepted" {
		t.Fatalf("cancelled caller: %s,%v", out, err)
	}
}

// Keep the SMS payload's sender bound when a reviewer edits the message.
func TestSMSApprovalEditPreservesSender(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"SM-out"}`), nil })
	thread := smsThread(t, w, sw)
	w.r.SMSProject = smsProject
	payload := json.RawMessage(`{"from":"+14155550123","body":"draft"}`)
	recipients := json.RawMessage(`[{"phone":"+14155550999"}]`)
	bound := review.Bound{Payload: payload, Recipients: recipients, LinkedStates: json.RawMessage(`[]`)}
	var proposal string
	if err := w.pool.QueryRow(ctx, `INSERT INTO proposals(workspace_id,thread_id,requested_by,action,payload,recipients,payload_sha256,rule) VALUES($1,$2,$3,'sms.send',$4,$5,$6,'human review') RETURNING id`, w.r.WorkspaceID, thread, w.r.ID, payload, recipients, bound.Hash()).Scan(&proposal); err != nil {
		t.Fatal(err)
	}
	if err := review.Approve(ctx, w.pool, w.r, proposal, &review.EmailPayload{Body: "edited"}); err != nil {
		t.Fatal(err)
	}
	var actionID string
	if err := w.pool.QueryRow(ctx, `SELECT id FROM actions WHERE proposal_id=$1`, proposal).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if out, err := DispatchSMS(ctx, w.pool, sw, actionID); err != nil || out != Sent {
		t.Fatalf("edited SMS=%s,%v", out, err)
	}
}

func TestSMSWorkerRunsWithoutEmailProvider(t *testing.T) {
	w := newWorld(t)
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"SM-worker"}`), nil })
	thread := smsThread(t, w, sw)
	actionID := smsReply(t, w, thread)
	worker := &sendWorker{pool: w.pool, signalwire: &SignalWireProjects{DB: w.pool, Installation: sw}}
	if err := worker.Work(context.Background(), &river.Job[SendArgs]{Args: SendArgs{ActionID: actionID}}); err != nil {
		t.Fatal(err)
	}
	if state := w.state(t, actionID); state != "accepted" {
		t.Fatalf("state=%s", state)
	}
}

// A missed call is answered by text from the number the caller dialled. The
// text goes out on the caller's SMS conversation, where their answer lands,
// and the call stops waiting.
func TestTextBackAnswersACallFromTheNumberCalled(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var sent url.Values
	sw := smsProvider(t, func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		sent = r.PostForm
		return resendResponse(201, `{"sid":"SM-back","status":"queued"}`), nil
	})
	if err := ConnectSMSNumber(ctx, w.pool, sw, w.r.WorkspaceID, smsLocal); err != nil {
		t.Fatal(err)
	}
	var contact, call string
	if err := w.pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`, w.r.WorkspaceID, smsRemote).Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `WITH i AS (INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'voice', $3))
		INSERT INTO threads (workspace_id, contact_id, channel, subject, phone_number, remote_address) VALUES ($1, $2, 'voice', $3, $4, $3) RETURNING id`,
		w.r.WorkspaceID, contact, smsRemote, smsLocal).Scan(&call); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload) VALUES ($1, $2, $3, 'call.missed', 'voice', '{"classification":{"kind":"message"}}')`, w.r.WorkspaceID, call, contact); err != nil {
		t.Fatal(err)
	}
	waiting := func(thread string) bool {
		items, err := review.Attention(ctx, w.pool, w.r.WorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ThreadID == thread && item.Kind == review.Unanswered {
				return true
			}
		}
		return false
	}
	if !waiting(call) {
		t.Fatal("the missed call is not waiting")
	}

	w.r.SMSProject = smsProject
	res, err := review.TextBack(ctx, w.pool, w.r, call, "Hi, sorry we missed you. We have openings in January.")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := DispatchSMS(ctx, w.pool, sw, res.ActionID); err != nil || out != Sent {
		t.Fatalf("DispatchSMS=%s,%v", out, err)
	}
	if sent.Get("From") != smsLocal || sent.Get("To") != smsRemote || !strings.Contains(sent.Get("Body"), "January") {
		t.Fatalf("sent %v", sent)
	}
	if waiting(call) {
		t.Fatal("the call still waits after a text back")
	}
	view, err := review.Thread(ctx, w.pool, w.r.WorkspaceID, call)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range view.Timeline {
		found = found || (e.Type == "call.texted_back" && strings.Contains(string(e.Payload), res.SMSThreadID))
	}
	if !found {
		t.Fatal("the call thread does not show the text")
	}
	in, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "SM-answer", From: smsRemote, To: smsLocal, Body: "Great, thanks", Project: smsProject})
	if err != nil || in.ThreadID != res.SMSThreadID || in.ContactID != contact {
		t.Fatalf("the caller's answer landed on %s (contact %s), want %s (contact %s): %v", in.ThreadID, in.ContactID, res.SMSThreadID, contact, err)
	}

	from, to, err := review.CallBack(ctx, w.pool, w.r, call)
	if err != nil || from != smsLocal || to != smsRemote {
		t.Fatalf("call back %s -> %s, %v", from, to, err)
	}
	if _, _, err := review.CallBack(ctx, w.pool, w.r, res.SMSThreadID); !errors.Is(err, review.ErrNoAddress) {
		t.Fatalf("call back on a text thread: %v", err)
	}
	if _, err := review.TextBack(ctx, w.pool, w.r, res.SMSThreadID, "x"); !errors.Is(err, review.ErrNoAddress) {
		t.Fatalf("text back on a text thread: %v", err)
	}
}
