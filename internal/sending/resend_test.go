package sending

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/email"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/review"
)

type resendTransport func(*http.Request) (*http.Response, error)

func (f resendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func resendResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func mockResend(t *testing.T, handle resendTransport) *Resend {
	t.Helper()
	r, err := NewResend("re_test")
	if err != nil {
		t.Fatal(err)
	}
	r.HTTP = &http.Client{Transport: resendTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.resend.com" || req.Header.Get("Authorization") != "Bearer re_test" {
			t.Fatalf("invalid request destination or authentication")
		}
		return handle(req)
	})}
	return r
}

func TestResendSendPreservesReplyAndUsesIdempotency(t *testing.T) {
	r := mockResend(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != "POST" || req.URL.Path != "/emails" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Idempotency-Key") != "visicomms/action-123" {
			t.Fatal("missing stable idempotency key")
		}
		var in struct {
			From, Subject, Text string
			To                  []string
			Headers             map[string]string
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.Subject != "Re: Café inquiry" || in.Text != "Hello café!\r\nSecond line." || len(in.To) != 1 || in.To[0] != "customer@example.com" {
			t.Fatalf("corrupted message: %+v", in)
		}
		if in.Headers["In-Reply-To"] != "<customer-1@example.com>" || in.Headers["References"] != "<customer-1@example.com>" {
			t.Fatal("missing threading headers")
		}
		return resendResponse(200, `{"id":"resend-123"}`), nil
	})
	msg := Message{FromName: "Business", FromAddress: "hello@example.com", To: []string{"customer@example.com"}, Subject: "Re: Café inquiry", Body: "Hello café!\nSecond line.", InReplyTo: "<customer-1@example.com>", Date: time.Now()}
	id, err := r.Send(context.Background(), msg.Build(), map[string]string{"action_id": "action-123"})
	if err != nil || id != "resend-123" {
		t.Fatalf("Send = %s, %v", id, err)
	}
}

func TestResendSendCarriesHTML(t *testing.T) {
	r := mockResend(t, func(req *http.Request) (*http.Response, error) {
		var in struct{ Text, HTML string }
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.Text != "Plain café" || in.HTML != `<p style="color:#007aff">HTML café</p>` {
			t.Fatalf("corrupted bodies: %+v", in)
		}
		return resendResponse(200, `{"id":"resend-html"}`), nil
	})
	msg := Message{FromAddress: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Body: "Plain café", HTML: `<p style="color:#007aff">HTML café</p>`, Date: time.Now()}
	if id, err := r.Send(context.Background(), msg.Build(), nil); err != nil || id != "resend-html" {
		t.Fatalf("Send = %s, %v", id, err)
	}
}

func TestResendSendFailuresAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		transportErr bool
		rejected     bool
	}{
		{"refusal", 422, `{"message":"invalid"}`, false, true},
		{"rate limit", 429, `{}`, false, true},
		{"server failure", 500, `{}`, false, false},
		{"timeout response", 408, `{}`, false, false},
		{"idempotency conflict", 409, `{}`, false, false},
		{"empty ID", 200, `{}`, false, false},
		{"malformed response", 200, `invalid`, false, false},
		{"transport loss", 0, ``, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := mockResend(t, func(*http.Request) (*http.Response, error) {
				calls++
				if tc.transportErr {
					return nil, errors.New("connection lost")
				}
				return resendResponse(tc.status, tc.body), nil
			})
			_, err := r.Send(context.Background(), Message{FromAddress: "a@example.com", To: []string{"b@example.com"}, Body: "hello"}.Build(), nil)
			var rejected *Rejected
			if err == nil || errors.As(err, &rejected) != tc.rejected || calls != 1 {
				t.Fatalf("error=%v, calls=%d", err, calls)
			}
		})
	}
}

const verifiedResendDomain = `{"id":"domain-1","name":"acmeplumbing.example","status":"verified","capabilities":{"sending":"enabled"},"records":[{"record":"DKIM","type":"TXT","name":"resend._domainkey","value":"p=test","status":"verified"},{"record":"SPF","type":"MX","name":"send","value":"feedback.example.com","priority":10,"status":"verified"}]}`

func setupResend(t *testing.T, w world, send resendTransport) *Resend {
	t.Helper()
	r := mockResend(t, func(req *http.Request) (*http.Response, error) {
		switch req.Method + " " + req.URL.Path {
		case "GET /domains":
			return resendResponse(200, `{"data":[{"id":"domain-1","name":"acmeplumbing.example"}],"has_more":false}`), nil
		case "GET /domains/domain-1":
			return resendResponse(200, verifiedResendDomain), nil
		case "POST /domains/domain-1/verify":
			return resendResponse(200, `{"id":"domain-1"}`), nil
		default:
			return send(req)
		}
	})
	ctx := context.Background()
	d, err := AddDomain(ctx, w.pool, r, w.r.WorkspaceID, "acmeplumbing.example")
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "resend" || len(d.Records) != 3 || d.Records[1].Type != "TXT" || d.Records[1].Name != "resend._domainkey.acmeplumbing.example" || d.Records[2].Priority == nil || *d.Records[2].Priority != 10 {
		t.Fatalf("incorrect records: %+v", d)
	}
	token := d.Records[0].Value
	LookupTXT = func(context.Context, string) ([]string, error) { return []string{token}, nil }
	d, err = CheckDomain(ctx, w.pool, r, w.r.WorkspaceID, d.ID)
	if err != nil || !d.Verified {
		t.Fatalf("CheckDomain=%+v, %v", d, err)
	}
	if err := SetFrom(ctx, w.pool, w.r.WorkspaceID, From{Name: "Acme Plumbing", Address: "quotes@acmeplumbing.example"}, "resend"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResendReplyDeliveryAndThreading(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sent := 0
	r := setupResend(t, w, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/emails" {
			t.Fatalf("unexpected request: %s", req.URL)
		}
		sent++
		// Delivery can precede the send result's database record.
		if err := ApplyResendEvent(ctx, w.pool, "resend-event", []byte(`{"type":"email.delivered","data":{"email_id":"resend-123","message_id":"<outgoing@resend.example>"}}`)); err != nil {
			t.Fatal(err)
		}
		return resendResponse(200, `{"id":"resend-123"}`), nil
	})
	w.r.Provider = "resend"
	dana := w.item(t, "Dana Whitfield")
	res, err := review.Reply(ctx, w.pool, w.r, dana.ThreadID, review.EmailPayload{Body: "Hello from Resend"})
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := w.pool.QueryRow(ctx, `SELECT c.kind FROM actions a JOIN connectors c ON c.id=a.connector_id WHERE a.id=$1`, res.ActionID).Scan(&kind); err != nil || kind != "resend" {
		t.Fatalf("wrong connector: %s, %v", kind, err)
	}
	if outcome, err := Dispatch(ctx, w.pool, r, res.ActionID); err != nil || outcome != Sent || w.state(t, res.ActionID) != "delivered" {
		t.Fatalf("Dispatch=%s,%v state=%s", outcome, err, w.state(t, res.ActionID))
	}
	if out, err := Dispatch(ctx, w.pool, r, res.ActionID); err != nil || out != Skipped || sent != 1 {
		t.Fatalf("duplicate dispatch: %s,%v calls=%d", out, err, sent)
	}
	// Ensure the reference matches the RFC Message-ID, not Resend's API ID.
	if _, err := w.pool.Exec(ctx, `UPDATE threads SET state='resolved', updated_at=now()-interval '2 days' WHERE id=$1`, dana.ThreadID); err != nil {
		t.Fatal(err)
	}
	in := email.Parse(map[string]any{"from": "dana@example.com", "to": "in@example.com", "message_id": "<reply@example.com>", "references": "<outgoing@resend.example>", "text": "Thanks"}, time.Now())
	in.Provider = "resend"
	received, err := ingest.Email(ctx, w.pool, w.r.WorkspaceID, in)
	if err != nil || received.ThreadID != dana.ThreadID {
		t.Fatalf("reply lost its thread: %+v, %v", received, err)
	}
}

func TestResendDomainCreationAndPagination(t *testing.T) {
	calls := []string{}
	r := mockResend(t, func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.RequestURI())
		switch req.Method + " " + req.URL.RequestURI() {
		case "GET /domains?limit=100":
			return resendResponse(200, `{"data":[{"id":"other","name":"other.example"}],"has_more":true}`), nil
		case "GET /domains?limit=100&after=other":
			return resendResponse(200, `{"data":[],"has_more":false}`), nil
		case "POST /domains":
			return resendResponse(200, verifiedResendDomain), nil
		}
		t.Fatalf("unexpected request: %s", req.URL)
		return nil, nil
	})
	id, err := r.RegisterDomain(context.Background(), "acmeplumbing.example")
	if err != nil || id.Status != "SUCCESS" || len(calls) != 3 {
		t.Fatalf("RegisterDomain=%+v,%v calls=%v", id, err, calls)
	}
}

func TestResendEventsCannotSettleSESSends(t *testing.T) {
	w := newWorld(t)
	actionID, _ := w.sent(t, "Marcus Lee")
	if err := ApplyResendEvent(context.Background(), w.pool, "other-provider", []byte(`{"type":"email.delivered","data":{"email_id":"0100018f-fake-ses-id"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := w.state(t, actionID); got != "accepted" {
		t.Fatalf("Resend event changed SES action to %s", got)
	}
}

func TestResendBouncesAndComplaintsStopFurtherReplies(t *testing.T) {
	for _, eventType := range []string{"email.bounced", "email.complained", "email.failed", "email.delivery_delayed"} {
		t.Run(eventType, func(t *testing.T) {
			w := newWorld(t)
			r := setupResend(t, w, func(*http.Request) (*http.Response, error) { return resendResponse(200, `{"id":"resend-123"}`), nil })
			w.r.Provider = "resend"
			actionID, threadID := w.approve(t, "Marcus Lee")
			if out, err := Dispatch(context.Background(), w.pool, r, actionID); err != nil || out != Sent {
				t.Fatalf("Dispatch=%s,%v", out, err)
			}
			event := map[string]any{"type": eventType, "data": map[string]any{"email_id": "resend-123", "bounce": map[string]string{"message": "recipient does not exist"}, "failed": map[string]string{"reason": "invalid_recipient"}}}
			body, _ := json.Marshal(event)
			for range 2 {
				if err := ApplyResendEvent(context.Background(), w.pool, "outcome", body); err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE provider='resend' AND provider_ref='outcome'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("events=%d, %v", n, err)
			}
			if eventType == "email.bounced" || eventType == "email.failed" {
				if got := w.state(t, actionID); got != "failed" {
					t.Fatalf("state=%s", got)
				}
				// A later Delivery must not resurrect a failed send.
				if err := ApplyResendEvent(context.Background(), w.pool, "late-delivery", []byte(`{"type":"email.delivered","data":{"email_id":"resend-123"}}`)); err != nil {
					t.Fatal(err)
				}
				if got := w.state(t, actionID); got != "failed" {
					t.Fatalf("late delivery changed failed send to %s", got)
				}
			}
			if eventType == "email.bounced" || eventType == "email.complained" {
				if w.optedOut(t, threadID) == "" {
					t.Fatal("contact was not suppressed")
				}
				if _, err := review.Reply(context.Background(), w.pool, w.r, threadID, review.EmailPayload{Body: "must not send"}); !errors.Is(err, review.ErrOptedOut) {
					t.Fatalf("reply after suppression: %v", err)
				}
			}
		})
	}
}
