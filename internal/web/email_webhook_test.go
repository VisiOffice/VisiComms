package web_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/testutil"
	"github.com/VisiOffice/VisiComms/internal/web"
)

const secret = "test-secret"

var clock = time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)

func server(t *testing.T) (*web.Server, http.Handler) {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Acme Plumbing', 'America/Los_Angeles') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO inbound_addresses (address, workspace_id) VALUES ('in+acmeplumbing@in.visicomms.app', $1)`, ws); err != nil {
		t.Fatal(err)
	}
	s := &web.Server{
		DB:                 pool,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		InboundEmailSecret: secret,
		Now:                func() time.Time { return clock },
	}
	return s, s.Routes()
}

func post(h http.Handler, body string, ts time.Time, key string) *httptest.ResponseRecorder {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(stamp + "." + body))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/email", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-VisiComms-Timestamp", stamp)
	req.Header.Set("X-VisiComms-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const marcus = `{"event_type":"email.received","payload":{"messageId":"<m1@roofco.example>","to":"in+acmeplumbing@in.visicomms.app",
	"from":"Marcus Lee <marcus@roofco.example>","subject":"water heater?","text":"What does a week cost?"}}`

func TestEmailWebhookStoresASignedEmail(t *testing.T) {
	_, h := server(t)
	rec := post(h, marcus, clock, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		EventID   int64 `json:"event_id"`
		Duplicate bool  `json:"duplicate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.EventID == 0 || body.Duplicate {
		t.Fatalf("body %s (%v)", rec.Body, err)
	}
	again := post(h, marcus, clock, secret)
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"duplicate":true`) {
		t.Fatalf("redelivery: %d %s", again.Code, again.Body)
	}
}

func TestEmailWebhookRejectsBadSignatures(t *testing.T) {
	s, h := server(t)
	cases := map[string]*httptest.ResponseRecorder{
		"wrong key":       post(h, marcus, clock, "other"),
		"replayed later":  post(h, marcus, clock.Add(-10*time.Minute), secret),
		"from the future": post(h, marcus, clock.Add(10*time.Minute), secret),
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/email", strings.NewReader(marcus))
	unsigned := httptest.NewRecorder()
	h.ServeHTTP(unsigned, req)
	cases["unsigned"] = unsigned
	for name, rec := range cases {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}

	s.InboundEmailSecret = ""
	if rec := post(s.Routes(), marcus, clock, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no secret configured: status %d, want every email rejected", rec.Code)
	}
}

func TestEmailWebhookUnknownRecipient(t *testing.T) {
	_, h := server(t)
	body := strings.Replace(marcus, "in+acmeplumbing@", "in+nobody@", 1)
	if rec := post(h, body, clock, secret); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestEmailWebhookAcceptsMillisecondTimestamps(t *testing.T) {
	_, h := server(t)
	stamp := strconv.FormatInt(clock.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "." + marcus))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/email", strings.NewReader(marcus))
	req.Header.Set("X-VisiComms-Timestamp", stamp)
	req.Header.Set("X-VisiComms-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestSESWebhookRefusesOtherTopics(t *testing.T) {
	s, _ := server(t)
	s.SNS = &sending.SNSVerifier{}
	s.SESEventsTopic = "arn:aws:sns:us-west-2:1:visicomms-ses-events"
	body := `{"Type":"Notification","TopicArn":"arn:aws:sns:us-west-2:1:someone-else","Message":"{}"}`
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/ses", strings.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other topic: %d", rec.Code)
	}
	unsigned := strings.Replace(body, "someone-else", "visicomms-ses-events", 1)
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/ses", strings.NewReader(unsigned)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned message on our topic: %d", rec.Code)
	}
}
