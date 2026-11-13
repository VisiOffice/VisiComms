package web_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// Calls the AI answers are recorded; the recording lands on the thread
// once, and only a signed-in person in the workspace can play it, through
// VisiComms rather than a provider link.
func TestAICallsAreRecordedAndPlayable(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	s.Auth = &auth.Service{DB: s.DB}
	voice.EnableAI(&voice.OpenAI{APIKey: "k", ProjectID: "proj", WebhookSecret: "whsec_c2VjcmV0"})
	t.Cleanup(func() { voice.EnableAI(nil) })
	h := s.Routes()
	flow := voice.Flow{Answer: voice.Action{Mode: voice.AI}, Agent: &voice.Agent{Opening: "Hello."}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}
	body := signedSMS(h, voice.PathIncoming, callForm("CA-rec"), "signing-key", smsPublicURL).Body.String()
	if !strings.Contains(body, `record="record-from-answer-dual"`) || !strings.Contains(body, smsPublicURL+voice.PathAIRecording) {
		t.Fatalf("AI dial does not record: %s", body)
	}
	var callID string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM calls WHERE call_sid = 'CA-rec'`).Scan(&callID); err != nil {
		t.Fatal(err)
	}

	recording := callForm("CA-rec", "RecordingUrl", "https://test.signalwire.com/api/laml/2010-04-01/Accounts/x/Recordings/RE1", "RecordingSid", "RE1", "RecordingDuration", "42", "RecordingStatus", "completed")
	for range 2 {
		if r := signedSMS(h, voice.PathAIRecording, recording, "signing-key", smsPublicURL); r.Code != 200 {
			t.Fatalf("recording callback = %d %s", r.Code, r.Body)
		}
	}
	var n int
	var outcome *string
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM events WHERE type = 'call.recorded' AND payload->>'call_id' = $1::text), (SELECT outcome FROM calls WHERE id = $1::uuid)`, callID).Scan(&n, &outcome); err != nil || n != 1 || outcome != nil {
		t.Fatalf("%d recordings, outcome %v, %v", n, outcome, err)
	}

	var fetched []*http.Request
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r)
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/mpeg"}, "Content-Range": {"bytes 0-3/10"}}, Body: io.NopCloser(strings.NewReader("ID3x"))}, nil
	})}
	session := func(name string, workspace string) string {
		var person string
		if err := s.DB.QueryRow(ctx, `INSERT INTO principals(workspace_id,kind,name) VALUES($1,'human',$2) RETURNING id`, workspace, name).Scan(&person); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(name))
		if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_sha256,principal_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, digest[:], person); err != nil {
			t.Fatal(err)
		}
		return name
	}
	play := func(token string, ids ...string) *httptest.ResponseRecorder {
		id := callID
		if len(ids) > 0 {
			id = ids[0]
		}
		req := httptest.NewRequest("GET", "/api/calls/"+id+"/recording", nil)
		req.Header.Set("Range", "bytes=0-3")
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "vc_session", Value: token})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	r := play(session("listener-session", ws))
	if r.Code != 206 || r.Body.String() != "ID3x" || r.Header().Get("Content-Range") != "bytes 0-3/10" || r.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("play = %d %q %v", r.Code, r.Body, r.Header())
	}
	if len(fetched) != 1 || !strings.HasSuffix(fetched[0].URL.Path, "/RE1.mp3") || fetched[0].Header.Get("Range") != "bytes=0-3" {
		t.Fatalf("fetched %v", fetched)
	}
	if user, _, ok := fetched[0].BasicAuth(); !ok || user != smsProjectID {
		t.Fatalf("recording fetched without the project's credentials")
	}

	if r := play(""); r.Code != 401 {
		t.Fatalf("anonymous play = %d", r.Code)
	}
	var other string
	if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces(name,timezone) VALUES('Other','UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if r := play(session("outsider-session", other)); r.Code != 404 || len(fetched) != 1 {
		t.Fatalf("other workspace play = %d, %d fetches", r.Code, len(fetched))
	}

	// A recording URL off SignalWire's hosts is never fetched.
	signedSMS(h, voice.PathIncoming, callForm("CA-evil"), "signing-key", smsPublicURL)
	signedSMS(h, voice.PathAIRecording, callForm("CA-evil", "RecordingUrl", "https://attacker.example/rec", "RecordingDuration", "3"), "signing-key", smsPublicURL)
	var evil string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM calls WHERE call_sid = 'CA-evil'`).Scan(&evil); err != nil {
		t.Fatal(err)
	}
	if r := play("listener-session", evil); r.Code != 502 || len(fetched) != 1 {
		t.Fatalf("untrusted host play = %d, %d fetches", r.Code, len(fetched))
	}

	// No MP3 for a recording: the WAV is played instead.
	var paths []string
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, ".mp3") {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-wav"}}, Body: io.NopCloser(strings.NewReader("RIFF"))}, nil
	})}
	if r := play("listener-session"); r.Code != 200 || r.Body.String() != "RIFF" || r.Header().Get("Content-Type") != "audio/x-wav" || len(paths) != 2 || !strings.HasSuffix(paths[1], "/RE1") {
		t.Fatalf("wav fallback = %d %q %v", r.Code, r.Body, paths)
	}

	// SignalWire redirects to its media host: followed there, without the
	// project's credentials.
	type hit struct {
		host string
		auth bool
	}
	var hits []hit
	redirectTo := "https://files.signalwire.com/signed/RE1.mp3?sig=x"
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(r *http.Request) (*http.Response, error) {
		_, _, ok := r.BasicAuth()
		hits = append(hits, hit{r.URL.Host, ok})
		if r.URL.Host != "files.signalwire.com" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {redirectTo}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("ID3y"))}, nil
	})}
	if r := play("listener-session"); r.Code != 200 || r.Body.String() != "ID3y" || len(hits) != 2 || !hits[0].auth || hits[1].host != "files.signalwire.com" || hits[1].auth {
		t.Fatalf("redirect = %d %q %v", r.Code, r.Body, hits)
	}

	// A redirect anywhere else is not followed.
	hits = nil
	redirectTo = "https://attacker.example/RE1.mp3"
	if r := play("listener-session"); r.Code != 502 || len(hits) != 1 {
		t.Fatalf("off-host redirect = %d, %v", r.Code, hits)
	}
}
