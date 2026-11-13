package web_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// relayFake answers SignalWire's browser token endpoint.
type relayFake struct{ resource string }

func (f *relayFake) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/api/relay/rest/jwt" {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	}
	f.resource = string(body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jwt_token":"jwt-1","refresh_token":"r"}`)),
		Header: http.Header{"Content-Type": {"application/json"}}}, nil
}

func TestRingingTheAppFallsThroughToTheNextStep(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	h := s.Routes()
	call := func(path string, form url.Values) string {
		t.Helper()
		rec := signedSMS(h, path, form, "signing-key", smsPublicURL)
		if rec.Code != 200 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	// The dial callback a step's markup names, as SignalWire would call it.
	dialPath := func(body string) string {
		t.Helper()
		i := strings.Index(body, voice.PathDial+"?")
		if i < 0 {
			t.Fatalf("no dial callback in %s", body)
		}
		return strings.ReplaceAll(body[i:i+strings.Index(body[i:], `"`)], "&amp;", "&")
	}
	flow := voice.Flow{Answer: voice.Action{Mode: voice.RingApp, Then: &voice.Action{Mode: voice.Forward, ForwardTo: "+13105550100"}}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}

	// Nobody online: straight to the forward.
	if body := call(voice.PathIncoming, callForm("CA-r1")); !strings.Contains(body, "<Number>+13105550100</Number>") || strings.Contains(body, "<Verto>") {
		t.Fatalf("nobody online: %s", body)
	}

	var person string
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name, email) VALUES ($1, 'human', 'Morgan', 'morgan@birchdental.example') RETURNING id`, ws).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := voice.SetPresence(ctx, s.DB, ws, person, true); err != nil {
		t.Fatal(err)
	}
	body := call(voice.PathIncoming, callForm("CA-r2"))
	if !strings.Contains(body, "<Verto>visicomms-"+person+"@test.verto.signalwire.com</Verto>") {
		t.Fatalf("online: %s", body)
	}
	// Unanswered in the browser: the forward.
	next := call(dialPath(body), callForm("CA-r2", "DialCallStatus", "no-answer"))
	if !strings.Contains(next, "<Number>+13105550100</Number>") {
		t.Fatalf("after the browser: %s", next)
	}
	// Unanswered forward, the last step: a message.
	if last := call(dialPath(next), callForm("CA-r2", "DialCallStatus", "no-answer")); !strings.Contains(last, "<Record") {
		t.Fatalf("after the forward: %s", last)
	}
	// Answered in the browser.
	call(voice.PathIncoming, callForm("CA-r3"))
	if done := call(dialPath(body), callForm("CA-r3", "DialCallStatus", "completed")); !strings.Contains(done, "<Hangup/>") {
		t.Fatalf("answered: %s", done)
	}

	// Away: not rung.
	if err := voice.SetPresence(ctx, s.DB, ws, person, false); err != nil {
		t.Fatal(err)
	}
	if body := call(voice.PathIncoming, callForm("CA-r4")); strings.Contains(body, "<Verto>") {
		t.Fatalf("away: %s", body)
	}
}

func TestBrowserPhone(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	s.Auth = &auth.Service{DB: s.DB}
	fake := &relayFake{}
	s.SignalWire.Installation.HTTP = &http.Client{Transport: fake}
	var person string
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals(workspace_id,kind,name) VALUES($1,'human','Morgan') RETURNING id`, ws).Scan(&person); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("phone-session"))
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_sha256,principal_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, digest[:], person); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "vc_session", Value: "phone-session"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := request("POST", "/api/phone/token", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token":"jwt-1"`) || !strings.Contains(rec.Body.String(), `"resource":"visicomms-`+person+`"`) {
		t.Fatalf("token = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(fake.resource, `"resource":"visicomms-`+person+`"`) {
		t.Fatalf("asked SignalWire for %s", fake.resource)
	}
	if rec := request("PUT", "/api/phone/presence", `{"online":true}`); rec.Code != 200 {
		t.Fatalf("presence = %d %s", rec.Code, rec.Body)
	}
	if online, err := voice.Online(ctx, s.DB, ws); err != nil || len(online) != 1 {
		t.Fatalf("online = %v %v", online, err)
	}
	// A call ringing in the app shows who is calling.
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", voice.Flow{Answer: voice.Action{Mode: voice.RingApp}}, true); err != nil {
		t.Fatal(err)
	}
	signedSMS(h, voice.PathIncoming, callForm("CA-p1"), "signing-key", smsPublicURL)
	if rec := request("GET", "/api/phone/calls", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"from":"`+smsCustomer+`"`) {
		t.Fatalf("calls = %d %s", rec.Code, rec.Body)
	}
}
