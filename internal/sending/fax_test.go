package sending

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/riverqueue/river"
)

const faxPDF = "%PDF-1.4\nfixture for transport tests\n%%EOF"

func queuedFax(t *testing.T, w world, sw *SignalWire) (review.FaxResult, files.Document) {
	t.Helper()
	ctx := context.Background()
	if err := ConnectPhoneNumber(ctx, w.pool, sw, w.r.WorkspaceID, smsLocal, "fax"); err != nil {
		t.Fatal(err)
	}
	doc, err := files.Store(ctx, w.pool, w.r.WorkspaceID, "quote.pdf", []byte(faxPDF))
	if err != nil {
		t.Fatal(err)
	}
	w.r.SMSProject = smsProject
	result, err := review.SendFax(ctx, w.pool, w.r, review.FaxRequest{From: smsLocal, To: smsRemote, DocumentID: doc.ID})
	if err != nil {
		t.Fatal(err)
	}
	return result, doc
}

func TestFaxSendUsesImmutablePDFAndReconcilesEarlyDelivery(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	calls := 0
	var token string
	sw := smsProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/Faxes.json") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("From") != smsLocal || r.PostForm.Get("To") != smsRemote || r.PostForm.Get("StatusCallback") != "https://comms.example.com/webhooks/signalwire/fax/status" {
			t.Fatalf("incorrect fax route: %v", r.PostForm)
		}
		media, err := url.Parse(r.PostForm.Get("MediaUrl"))
		if err != nil {
			t.Fatal(err)
		}
		if media.Host != "comms.example.com" || !strings.HasPrefix(media.Path, "/fax-media/") {
			t.Fatal("fax uses arbitrary public media")
		}
		token = strings.TrimPrefix(media.Path, "/fax-media/")
		doc, err := files.LoadGrant(ctx, w.pool, token)
		if err != nil || string(doc.Content) != faxPDF {
			t.Fatalf("provider document=%q,%v", doc.Content, err)
		}
		if err := ApplyFaxStatus(ctx, w.pool, FaxStatus{SID: "FX-out", Status: "delivered", Project: smsProject, Pages: 3}); err != nil {
			t.Fatal(err)
		}
		return resendResponse(201, `{"sid":"FX-out","status":"queued"}`), nil
	})
	result, _ := queuedFax(t, w, sw)
	if out, err := DispatchFax(ctx, w.pool, sw, result.ActionID); err != nil || out != Sent || w.state(t, result.ActionID) != "delivered" {
		t.Fatalf("DispatchFax=%s,%v state=%s", out, err, w.state(t, result.ActionID))
	}
	if out, err := DispatchFax(ctx, w.pool, sw, result.ActionID); err != nil || out != Skipped || calls != 1 {
		t.Fatalf("duplicate fax=%s,%v calls=%d", out, err, calls)
	}
	if _, err := files.LoadGrant(ctx, w.pool, token); !db.NotFound(err) {
		t.Fatalf("completed fax grant still accessible: %v", err)
	}
	view, err := review.Thread(ctx, w.pool, w.r.WorkspaceID, result.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Channel != "fax" || view.PhoneNumber != smsLocal || view.RemoteAddress != smsRemote {
		t.Fatalf("wrong fax thread: %+v", view)
	}
	var pages int
	if err := w.pool.QueryRow(ctx, `SELECT (payload->>'pages')::int FROM events WHERE thread_id=$1 AND type='fax.delivered'`, result.ThreadID).Scan(&pages); err != nil || pages != 3 {
		t.Fatalf("delivery pages=%d,%v", pages, err)
	}
}

func TestFaxFailureUnknownAndCancellation(t *testing.T) {
	for _, kind := range []string{"refused", "unknown", "paused", "tampered", "wrong-project"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			calls := 0
			sw := smsProvider(t, func(*http.Request) (*http.Response, error) {
				calls++
				status := 422
				if kind == "unknown" {
					status = 500
				}
				return resendResponse(status, `{}`), nil
			})
			result, _ := queuedFax(t, w, sw)
			want := Failed
			var err error
			switch kind {
			case "unknown":
				want = Unknown
			case "paused":
				want = Cancelled
				_, err = w.pool.Exec(ctx, `UPDATE workspaces SET sending_paused_at=now() WHERE id=$1`, w.r.WorkspaceID)
			case "tampered":
				want = Cancelled
				_, err = w.pool.Exec(ctx, `UPDATE actions SET payload=jsonb_set(payload,'{sha256}','"changed"') WHERE id=$1`, result.ActionID)
			case "wrong-project":
				want = Cancelled
				sw.Project = "another-project"
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, err := DispatchFax(ctx, w.pool, sw, result.ActionID); err != nil || out != want {
				t.Fatalf("DispatchFax=%s,%v", out, err)
			}
			if out, err := DispatchFax(ctx, w.pool, sw, result.ActionID); err != nil || out != Skipped {
				t.Fatalf("retry=%s,%v", out, err)
			}
			if want == Cancelled && calls != 0 || want != Cancelled && calls != 1 {
				t.Fatalf("provider calls=%d", calls)
			}
		})
	}
}

func TestFaxDocumentsAreImmutableAndWorkspaceScoped(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"FX-out"}`), nil })
	_, doc := queuedFax(t, w, sw)
	if _, err := w.pool.Exec(ctx, `UPDATE documents SET content=$2 WHERE id=$1`, doc.ID, []byte("%PDF-changed")); err == nil {
		t.Fatal("approved PDF changed")
	}
	var other string
	if err := w.pool.QueryRow(ctx, `INSERT INTO workspaces(name,timezone) VALUES('Other','UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	otherDoc, err := files.Store(ctx, w.pool, other, "other.pdf", []byte(faxPDF))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.Load(ctx, w.pool, w.r.WorkspaceID, otherDoc.ID); !db.NotFound(err) {
		t.Fatalf("cross-workspace document read: %v", err)
	}
	w.r.SMSProject = smsProject
	if _, err := review.SendFax(ctx, w.pool, w.r, review.FaxRequest{From: smsLocal, To: smsRemote, DocumentID: otherDoc.ID}); !errors.Is(err, review.ErrFaxDocument) {
		t.Fatalf("cross-workspace fax: %v", err)
	}
	if _, err := files.Store(ctx, w.pool, w.r.WorkspaceID, "bad.pdf", []byte("not a PDF")); !errors.Is(err, files.ErrPDF) {
		t.Fatalf("non-PDF accepted: %v", err)
	}
	if _, err := files.Store(ctx, w.pool, w.r.WorkspaceID, "huge.pdf", []byte("%PDF-"+strings.Repeat("a", files.MaxPDF))); !errors.Is(err, files.ErrPDF) {
		t.Fatalf("oversized PDF accepted: %v", err)
	}
}

func TestFaxGrantExpiresAndWrongTokenFails(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var token string
	sw := smsProvider(t, func(r *http.Request) (*http.Response, error) {
		r.ParseForm()
		media, _ := url.Parse(r.PostForm.Get("MediaUrl"))
		token = strings.TrimPrefix(media.Path, "/fax-media/")
		return resendResponse(201, `{"sid":"FX-out"}`), nil
	})
	result, _ := queuedFax(t, w, sw)
	if _, err := DispatchFax(ctx, w.pool, sw, result.ActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := files.LoadGrant(ctx, w.pool, token); err != nil {
		t.Fatal(err)
	}
	if _, err := files.LoadGrant(ctx, w.pool, strings.Repeat("0", 64)); !db.NotFound(err) {
		t.Fatal("wrong token accepted")
	}
	if _, err := w.pool.Exec(ctx, `UPDATE document_grants SET expires_at=now()-interval '1 second' WHERE action_id=$1`, result.ActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := files.LoadGrant(ctx, w.pool, token); !db.NotFound(err) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestFaxReceiveDedupesDocumentAndKeepsChannelSeparate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return nil, nil })
	if err := ConnectPhoneNumber(ctx, w.pool, sw, w.r.WorkspaceID, smsLocal, "fax"); err != nil {
		t.Fatal(err)
	}
	fax := ingest.Fax{SID: "FX-in", From: smsRemote, To: smsLocal, Project: smsProject, Status: "received", Pages: 2, PDF: []byte(faxPDF)}
	first, err := ingest.ReceiveFax(ctx, w.pool, fax)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ingest.ReceiveFax(ctx, w.pool, fax)
	if err != nil || !again.Duplicate || again.EventID != first.EventID {
		t.Fatalf("fax redelivery=%+v,%v", again, err)
	}
	var n int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE workspace_id=$1`, w.r.WorkspaceID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("documents=%d,%v", n, err)
	}
	sms, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "SM-in", From: smsRemote, To: smsLocal, Project: smsProject, Body: "hello"})
	if err != nil || sms.ThreadID == first.ThreadID || sms.ContactID == first.ContactID {
		t.Fatalf("fax silently merged with SMS: %+v,%v", sms, err)
	}
	items, err := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.ThreadID == first.ThreadID && it.Kind == review.Unanswered {
			found = true
		}
	}
	if !found {
		t.Fatal("incoming fax not in Needs attention")
	}
}

func TestFaxBusyCallbackAndWorkerRouting(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"FX-out"}`), nil })
	result, _ := queuedFax(t, w, sw)
	worker := &sendWorker{pool: w.pool, signalwire: &SignalWireProjects{DB: w.pool, Installation: sw}}
	if err := worker.Work(ctx, &river.Job[SendArgs]{Args: SendArgs{ActionID: result.ActionID}}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"busy", "sending", "busy"} {
		if err := ApplyFaxStatus(ctx, w.pool, FaxStatus{SID: "FX-out", Status: state, Project: smsProject}); err != nil {
			t.Fatal(err)
		}
	}
	if w.state(t, result.ActionID) != "failed" {
		t.Fatal("busy fax not failed")
	}
	items, err := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ThreadID == result.ThreadID && it.Kind == review.SendFailed {
			return
		}
	}
	t.Fatal("failed fax not in Needs attention")
}

func TestFaxRequestRetryDoesNotQueueAnotherSend(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sw := smsProvider(t, func(*http.Request) (*http.Response, error) { return nil, nil })
	_, doc := queuedFax(t, w, sw)
	w.r.SMSProject = smsProject
	req := review.FaxRequest{From: smsLocal, To: smsRemote, DocumentID: doc.ID, RequestID: "retry-one"}
	first, err := review.SendFax(ctx, w.pool, w.r, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := review.SendFax(ctx, w.pool, w.r, req)
	if err != nil || again.ActionID != first.ActionID {
		t.Fatalf("retry=%+v %v", again, err)
	}
	req.To = "+14155550888"
	if _, err := review.SendFax(ctx, w.pool, w.r, req); !errors.Is(err, review.ErrConflict) {
		t.Fatalf("changed retry accepted: %v", err)
	}
	if err := ApplySMSStatus(ctx, w.pool, SMSStatus{SID: "FX-same", Status: "delivered", Project: smsProject}); err != nil {
		t.Fatal(err)
	}
	sw.HTTP = &http.Client{Transport: resendTransport(func(*http.Request) (*http.Response, error) { return resendResponse(201, `{"sid":"FX-same"}`), nil })}
	if _, err := DispatchFax(ctx, w.pool, sw, first.ActionID); err != nil {
		t.Fatal(err)
	}
	if state := w.state(t, first.ActionID); state != "accepted" {
		t.Fatalf("SMS callback changed fax to %s", state)
	}
}
