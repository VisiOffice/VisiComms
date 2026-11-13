package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/recordings"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

type memStore struct {
	mu      sync.Mutex
	files   map[string][]byte
	deleted []string
}

func (m *memStore) Put(_ context.Context, key, _ string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[key] = append([]byte(nil), data...)
	return "v1", nil
}

func (m *memStore) Get(_ context.Context, key, _ string) (recordings.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[key]
	if !ok {
		return recordings.Object{}, errors.New("no such key")
	}
	return recordings.Object{Body: io.NopCloser(bytes.NewReader(data)), ContentType: "audio/mpeg", ContentLength: int64(len(data))}, nil
}

func (m *memStore) Delete(_ context.Context, key, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, key)
	m.deleted = append(m.deleted, key+"@"+version)
	return nil
}

// A recording is copied into VisiComms' bucket and deleted from SignalWire;
// it then plays from the bucket, and a year after the call both copies are
// gone. A copy that fails keeps SignalWire's and is retried later.
func TestRecordingsMoveOffSignalWireAndExpireAfterAYear(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	s.Auth = &auth.Service{DB: s.DB}
	voice.EnableAI(&voice.OpenAI{APIKey: "k", ProjectID: "proj", WebhookSecret: "whsec_c2VjcmV0"})
	t.Cleanup(func() { voice.EnableAI(nil) })
	store := &memStore{files: map[string][]byte{}}
	s.Recordings = store
	h := s.Routes()
	flow := voice.Flow{Answer: voice.Action{Mode: voice.AI}, Agent: &voice.Agent{Opening: "Hello."}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}
	record := func(sid, re string) string {
		signedSMS(h, voice.PathIncoming, callForm(sid), "signing-key", smsPublicURL)
		signedSMS(h, voice.PathAIRecording, callForm(sid, "RecordingUrl", "https://test.signalwire.com/api/laml/2010-04-01/Accounts/"+smsProjectID+"/Calls/"+sid+"/Recordings/"+re, "RecordingSid", re, "RecordingDuration", "18", "RecordingStatus", "completed"), "signing-key", smsPublicURL)
		var id string
		if err := s.DB.QueryRow(ctx, `SELECT id FROM calls WHERE call_sid = $1`, sid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	good, bad := record("CA-keep", "REgood"), record("CA-fail", "REbad")

	var mu sync.Mutex
	var deletes, gets []string
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.Path)
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		gets = append(gets, r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "REbad"):
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}, nil
		case r.URL.Host != "files.signalwire.com":
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://files.signalwire.com/x" + r.URL.Path}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("ID3-the-call"))}, nil
	})}
	archiver := &recordings.Archiver{DB: s.DB, SignalWire: s.SignalWire, Store: store, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	now := time.Now()
	if err := archiver.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}

	key := "recordings/" + ws + "/" + good + ".mp3"
	if string(store.files[key]) != "ID3-the-call" || len(store.files) != 1 {
		t.Fatalf("stored %v", store.files)
	}
	if len(deletes) != 1 || deletes[0] != "/api/laml/2010-04-01/Accounts/"+smsProjectID+"/Recordings/REgood.json" {
		t.Fatalf("SignalWire deletes %v", deletes)
	}
	var sum []byte
	var attempts int
	var next time.Time
	if err := s.DB.QueryRow(ctx, `SELECT sha256 FROM recordings WHERE call_id = $1::uuid`, good).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256([]byte("ID3-the-call")); !bytes.Equal(sum, want[:]) {
		t.Fatal("stored checksum does not match")
	}
	if err := s.DB.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM recordings WHERE call_id = $1::uuid AND archived_at IS NULL AND provider_deleted_at IS NULL`, bad).Scan(&attempts, &next); err != nil || attempts != 1 || !next.After(now) {
		t.Fatalf("failed copy: attempts %d, next %v, %v", attempts, next, err)
	}

	// A second pass right away leaves the failed one until its retry time.
	gets = nil
	if err := archiver.Sweep(ctx, now.Add(time.Second)); err != nil || len(gets) != 0 || len(deletes) != 1 {
		t.Fatalf("second pass: gets %v deletes %v %v", gets, deletes, err)
	}

	digest := sha256.Sum256([]byte("archive-session"))
	var person string
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals(workspace_id,kind,name) VALUES($1,'human','Listener') RETURNING id`, ws).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_sha256,principal_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, digest[:], person); err != nil {
		t.Fatal(err)
	}
	play := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/calls/"+id+"/recording", nil)
		req.AddCookie(&http.Cookie{Name: "vc_session", Value: "archive-session"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := play(good); r.Code != 200 || r.Body.String() != "ID3-the-call" || len(gets) != 0 {
		t.Fatalf("play from the bucket = %d %q, SignalWire fetches %v", r.Code, r.Body, gets)
	}

	// A year on, both copies are deleted and the recording is gone.
	if _, err := s.DB.Exec(ctx, `UPDATE recordings SET recorded_at = recorded_at - interval '366 days'`); err != nil {
		t.Fatal(err)
	}
	deletes = nil
	if err := archiver.Sweep(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 0 || len(store.deleted) != 1 || store.deleted[0] != key+"@v1" {
		t.Fatalf("bucket after a year: %v, deleted %v", store.files, store.deleted)
	}
	if len(deletes) != 1 || !strings.HasSuffix(deletes[0], "/Recordings/REbad.json") {
		t.Fatalf("SignalWire deletes at expiry %v", deletes)
	}
	for _, id := range []string{good, bad} {
		if r := play(id); r.Code != http.StatusGone {
			t.Fatalf("expired play = %d", r.Code)
		}
	}
	var expired int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM recordings WHERE expired_at IS NOT NULL AND provider_deleted_at IS NOT NULL`).Scan(&expired); err != nil || expired != 2 {
		t.Fatalf("%d expired, %v", expired, err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
