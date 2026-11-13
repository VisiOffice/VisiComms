package sending

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/review"
)

// fakeSNS signs messages with a throwaway key and serves its certificate to
// the verifier as if from sns.us-west-2.amazonaws.com.
type fakeSNS struct {
	key       *rsa.PrivateKey
	certPEM   []byte
	confirmed []string
}

func newFakeSNS(t *testing.T) *fakeSNS {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sns.amazonaws.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeSNS{key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (f *fakeSNS) RoundTrip(r *http.Request) (*http.Response, error) {
	body := []byte("ok")
	if r.URL.Path == "/cert.pem" {
		body = f.certPEM
	} else {
		f.confirmed = append(f.confirmed, r.URL.String())
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
}

func (f *fakeSNS) verifier() *SNSVerifier { return &SNSVerifier{HTTP: &http.Client{Transport: f}} }

func (f *fakeSNS) sign(m SNSMessage) SNSMessage {
	m.SignatureVersion = "2"
	m.SigningCertURL = "https://sns.us-west-2.amazonaws.com/cert.pem"
	sum := sha256.Sum256([]byte(m.signedString()))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	return m
}

func TestSNSSignatures(t *testing.T) {
	f := newFakeSNS(t)
	v := f.verifier()
	ctx := context.Background()
	m := f.sign(SNSMessage{Type: "Notification", MessageID: "m1", TopicArn: "arn:aws:sns:us-west-2:1:visicomms-ses-events",
		Message: `{"eventType":"Delivery"}`, Timestamp: "2026-10-14T12:00:00.000Z"})
	if err := v.Verify(ctx, m); err != nil {
		t.Fatalf("genuine message refused: %v", err)
	}
	tampered := m
	tampered.Message = `{"eventType":"Complaint"}`
	if err := v.Verify(ctx, tampered); err == nil {
		t.Fatal("tampered message verified")
	}
	elsewhere := m
	elsewhere.SigningCertURL = "https://evil.example.com/cert.pem"
	if err := v.Verify(ctx, elsewhere); err == nil {
		t.Fatal("certificate from a non-SNS host accepted")
	}
	confirm := f.sign(SNSMessage{Type: "SubscriptionConfirmation", MessageID: "c1", TopicArn: m.TopicArn, Message: "confirm",
		Timestamp: m.Timestamp, Token: "tok", SubscribeURL: "https://sns.us-west-2.amazonaws.com/?Action=ConfirmSubscription&Token=tok"})
	if err := v.Verify(ctx, confirm); err != nil {
		t.Fatalf("confirmation refused: %v", err)
	}
	if err := v.Confirm(ctx, SNSMessage{SubscribeURL: "https://evil.example.com/confirm"}); err == nil {
		t.Fatal("confirmed through a non-SNS link")
	}
}

func sesEventJSON(t *testing.T, eventType, messageID string, extra map[string]any) string {
	t.Helper()
	e := map[string]any{"eventType": eventType, "mail": map[string]any{"messageId": messageID}}
	for k, v := range extra {
		e[k] = v
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func (w world) sent(t *testing.T, contact string) (actionID, threadID string) {
	t.Helper()
	actionID, threadID = w.approve(t, contact)
	if out, err := Dispatch(context.Background(), w.pool, w.provider, actionID); err != nil || out != Sent {
		t.Fatalf("Dispatch = %s, %v", out, err)
	}
	return actionID, threadID
}

func (w world) optedOut(t *testing.T, threadID string) string {
	t.Helper()
	var source string
	_ = w.pool.QueryRow(context.Background(), `SELECT coalesce((SELECT o.source FROM opt_outs o JOIN threads t ON t.contact_id = o.contact_id WHERE t.id = $1), '')`, threadID).Scan(&source)
	return source
}

func TestDeliveryEventsAreAppliedOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, threadID := w.sent(t, "Marcus Lee")
	body := sesEventJSON(t, "Delivery", "0100018f-fake-ses-id", nil)
	for range 2 { // SNS can deliver twice
		if err := ApplyEvent(ctx, w.pool, "sns-1", body); err != nil {
			t.Fatal(err)
		}
	}
	if s := w.state(t, actionID); s != "delivered" {
		t.Fatalf("state = %s", s)
	}
	var n int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = $1 AND type = 'email.delivered'`, threadID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d delivered events", n)
	}
	// Events for mail we cannot match are retained without affecting a thread.
	if err := ApplyEvent(ctx, w.pool, "sns-2", sesEventJSON(t, "Delivery", "someone-elses-id", nil)); err != nil {
		t.Fatal(err)
	}
}

func TestEventBeforeSendResultIsReconciled(t *testing.T) {
	for _, eventType := range []string{"Delivery", "Bounce", "Complaint"} {
		t.Run(eventType, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			actionID, threadID := w.approve(t, "Marcus Lee")
			body := sesEventJSON(t, eventType, "0100018f-fake-ses-id", map[string]any{
				"bounce": map[string]any{"bounceType": "Permanent"},
			})
			w.provider.onSend = func() {
				if err := ApplyEvent(ctx, w.pool, "early", body); err != nil {
					t.Fatal(err)
				}
				var pending bool
				if err := w.pool.QueryRow(ctx, `SELECT processed_at IS NULL FROM provider_events WHERE event_id = 'early'`).Scan(&pending); err != nil || !pending {
					t.Fatalf("event must be durable and pending: %v, %v", pending, err)
				}
			}
			if out, err := Dispatch(ctx, w.pool, w.provider, actionID); err != nil || out != Sent {
				t.Fatalf("Dispatch = %s, %v", out, err)
			}
			want := "accepted"
			if eventType == "Delivery" {
				want = "delivered"
			} else if eventType == "Bounce" {
				want = "failed"
			}
			if got := w.state(t, actionID); got != want {
				t.Fatalf("state = %s, want %s", got, want)
			}
			if eventType != "Delivery" && w.optedOut(t, threadID) == "" {
				t.Fatal("early bounce or complaint did not suppress further sends")
			}
			if err := ApplyEvent(ctx, w.pool, "early", body); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE provider = 'ses' AND provider_ref = 'early'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("applied %d times: %v", n, err)
			}
		})
	}
}

func TestSweepReconcilesStoredEvents(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, _ := w.sent(t, "Marcus Lee")
	// Simulate a process stopping after receipt, before applying the event.
	body := sesEventJSON(t, "Delivery", "0100018f-fake-ses-id", nil)
	if _, err := w.pool.Exec(ctx, `INSERT INTO provider_events (provider, event_id, message_id, payload)
		VALUES ('ses', 'stored', '0100018f-fake-ses-id', $1)`, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	// Older unmatched receipts must not prevent matching events being replayed.
	if err := ApplyEvent(ctx, w.pool, "unrelated", sesEventJSON(t, "Delivery", "other-message", nil)); err != nil {
		t.Fatal(err)
	}
	if err := (&sweepWorker{pool: w.pool}).Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.state(t, actionID); got != "delivered" {
		t.Fatalf("state = %s, want delivered after recovery", got)
	}
	var processed bool
	if err := w.pool.QueryRow(ctx, `SELECT processed_at IS NOT NULL FROM provider_events WHERE event_id = 'stored'`).Scan(&processed); err != nil || !processed {
		t.Fatalf("receipt not marked processed: %v", err)
	}
}

func TestHardBounceFailsTheSendAndStopsMail(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, threadID := w.sent(t, "Marcus Lee")
	err := ApplyEvent(ctx, w.pool, "sns-b", sesEventJSON(t, "Bounce", "0100018f-fake-ses-id", map[string]any{
		"bounce": map[string]any{"bounceType": "Permanent", "bouncedRecipients": []any{map[string]any{
			"emailAddress": "marcus@roofco.example", "diagnosticCode": "smtp; 550 5.1.1 user unknown"}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if s := w.state(t, actionID); s != "failed" {
		t.Fatalf("state = %s", s)
	}
	if src := w.optedOut(t, threadID); src != "hard bounce" {
		t.Fatalf("opt-out source = %q", src)
	}
	items, _ := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	for _, it := range items {
		if it.Kind == review.SendFailed && it.ThreadID == threadID {
			if it.Reason != "Not delivered: smtp; 550 5.1.1 user unknown" {
				t.Fatalf("reason %q", it.Reason)
			}
			return
		}
	}
	t.Fatal("bounced send not in Needs attention")
}

func TestComplaintOptsTheContactOut(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, threadID := w.sent(t, "Kelly Brooks")
	if err := ApplyEvent(ctx, w.pool, "sns-c", sesEventJSON(t, "Complaint", "0100018f-fake-ses-id", map[string]any{
		"complaint": map[string]any{"complaintFeedbackType": "abuse"}})); err != nil {
		t.Fatal(err)
	}
	if src := w.optedOut(t, threadID); src != "spam complaint" {
		t.Fatalf("opt-out source = %q", src)
	}
	items, _ := review.Attention(ctx, w.pool, w.r.WorkspaceID)
	found := false
	for _, it := range items {
		found = found || (it.Kind == review.Complaint && it.ThreadID == threadID)
	}
	if !found {
		t.Fatal("complaint not in Needs attention")
	}
}

func TestDeliverySettlesAnUnknownSend(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actionID, _ := w.sent(t, "Marcus Lee")
	_, _ = w.pool.Exec(ctx, `UPDATE actions SET state = 'unknown' WHERE id = $1`, actionID)
	if err := ApplyEvent(ctx, w.pool, "sns-d", sesEventJSON(t, "Delivery", "0100018f-fake-ses-id", nil)); err != nil {
		t.Fatal(err)
	}
	if s := w.state(t, actionID); s != "delivered" {
		t.Fatalf("state = %s, want delivered", s)
	}
}
