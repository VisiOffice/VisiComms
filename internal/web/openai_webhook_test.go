package web_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// fakeOpenAI accepts calls and plays one conversation over the websocket:
// it greets, hears the caller, and takes a message.
type fakeOpenAI struct {
	mu       sync.Mutex
	accepted map[string]map[string]any
	rejected []string
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/realtime" {
		f.converse(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/realtime/calls/"), "/")
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch parts[1] {
	case "accept":
		f.accepted[parts[0]] = body
	case "reject":
		f.rejected = append(f.rejected, parts[0])
	}
}

func (f *fakeOpenAI) converse(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	send := func(v map[string]any) {
		raw, _ := json.Marshal(v)
		conn.Write(ctx, websocket.MessageText, raw)
	}
	read := func() map[string]any {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		var v map[string]any
		json.Unmarshal(raw, &v)
		return v
	}
	if ev := read(); ev["type"] != "response.create" {
		return
	}
	send(map[string]any{"type": "response.created"})
	send(map[string]any{"type": "response.output_audio_transcript.done", "transcript": "Thanks for calling Birch Dental."})
	send(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "transcript": "Can I book a cleaning in March?"})
	send(map[string]any{"type": "response.function_call_arguments.done", "name": "take_message", "call_id": "fc_1",
		"arguments": `{"caller_name":"Dana","callback_number":"+13105550188","reason":"Cleaning in March","summary":"Wants a March appointment."}`})
	if ev := read(); ev["type"] != "conversation.item.create" {
		return
	}
	read() // response.create
	conn.Close(websocket.StatusNormalClosure, "")
}

func signedWebhook(h http.Handler, at time.Time, secret []byte, id, body string) *httptest.ResponseRecorder {
	ts := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s.%s.%s", id, ts, body)
	req := httptest.NewRequest("POST", "/webhooks/openai/realtime", strings.NewReader(body))
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func incoming(openAICallID, callID string) string {
	return fmt.Sprintf(`{"type":"realtime.call.incoming","data":{"call_id":%q,"sip_headers":[{"name":"X-VisiComms-Call","value":%q}]}}`, openAICallID, callID)
}

func TestTheAIAnswersAndTakesAMessage(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	fake := &fakeOpenAI{accepted: map[string]map[string]any{}}
	api := httptest.NewServer(fake)
	defer api.Close()
	secret := []byte("0123456789abcdef")
	o := &voice.OpenAI{APIKey: "sk-test", ProjectID: "proj_test", WebhookSecret: "whsec_" + base64.StdEncoding.EncodeToString(secret),
		BaseURL: api.URL, SocketURL: "ws" + strings.TrimPrefix(api.URL, "http")}
	voice.EnableAI(o)
	t.Cleanup(func() { voice.EnableAI(nil) })
	s.OpenAI = o
	h := s.Routes()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}

	if _, err := review.DraftApprovedInfo(ctx, s.DB, ws, []byte(`{"articles":[{"title":"Main office","content":"Cleanings on weekdays.","channels":["phone"]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `WITH p AS (INSERT INTO principals (workspace_id, kind, name, email) VALUES ($1, 'human', 'Morgan', 'morgan@birchdental.example') RETURNING id)
		UPDATE approved_info_versions SET approved_at = now(), approved_by = (SELECT id FROM p) WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", voice.Flow{Answer: voice.Action{Mode: voice.AI}}, true); err == nil {
		t.Fatal("an AI flow without agent settings saved")
	}
	flow := voice.Flow{Answer: voice.Action{Mode: voice.AI}, Agent: &voice.Agent{Opening: "Thanks for calling Birch Dental.", Instructions: "Never promise an appointment time."}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}

	rec := signedSMS(h, voice.PathIncoming, callForm("CA-ai"), "signing-key", smsPublicURL)
	var callID string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM calls WHERE call_sid = 'CA-ai'`).Scan(&callID); err != nil {
		t.Fatal(err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<Sip") || !strings.Contains(body, "sip:proj_test@sip.api.openai.com") || !strings.Contains(body, "X-VisiComms-Call="+callID) {
		t.Fatalf("AI answer: %s", body)
	}

	if rec := signedWebhook(h, now, []byte("wrong"), "wh_0", incoming("rtc_1", callID)); rec.Code != 401 {
		t.Fatalf("unsigned webhook = %d", rec.Code)
	}
	if rec := signedWebhook(h, now, secret, "wh_1", incoming("rtc_1", callID)); rec.Code != 200 {
		t.Fatalf("webhook = %d %s", rec.Code, rec.Body)
	}
	// OpenAI retries; the call is answered once.
	signedWebhook(h, now, secret, "wh_1", incoming("rtc_1", callID))
	// A call VisiComms did not dial is turned away.
	signedWebhook(h, now, secret, "wh_2", incoming("rtc_stranger", "00000000-0000-0000-0000-000000000000"))

	fake.mu.Lock()
	session, accepted := fake.accepted["rtc_1"]
	instructions, _ := session["instructions"].(string)
	accepts, rejected := len(fake.accepted), fake.rejected
	fake.mu.Unlock()
	if !accepted || accepts != 1 || len(rejected) != 1 || rejected[0] != "rtc_stranger" {
		t.Fatalf("accepted %v (%d), rejected %v", accepted, accepts, rejected)
	}
	for _, want := range []string{"Cleanings on weekdays.", "Never promise an appointment time."} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions lack %q", want)
		}
	}

	// SignalWire reports the end of the call before the conversation is
	// recorded; once the AI has spoken, it is not a missed call.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var greeted bool
		if s.DB.QueryRow(ctx, `SELECT ai_greeted_at IS NOT NULL FROM calls WHERE id = $1`, callID).Scan(&greeted); greeted {
			break
		}
	}
	signedSMS(h, voice.PathStatus, callForm("CA-ai", "CallStatus", "completed"), "signing-key", smsPublicURL)
	var summary, outcome string
	var untrusted bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := s.DB.QueryRow(ctx, `SELECT payload->>'summary', untrusted FROM events WHERE type = 'call.ai_handled' AND payload->>'call_id' = $1`, callID).Scan(&summary, &untrusted)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(summary, "Cleaning in March") || !strings.Contains(summary, "+13105550188") || !untrusted {
		t.Fatalf("summary %q untrusted=%v", summary, untrusted)
	}
	var missed int
	if err := s.DB.QueryRow(ctx, `SELECT outcome, (SELECT count(*) FROM events WHERE type = 'call.missed') FROM calls WHERE id = $1`, callID).Scan(&outcome, &missed); err != nil || outcome != "answered" || missed != 0 {
		t.Fatalf("outcome %q, %d missed, %v", outcome, missed, err)
	}
	var waiting bool
	if err := s.DB.QueryRow(ctx, `SELECT `+review.WaitingSQL+` IS NOT NULL FROM threads t JOIN calls c ON c.thread_id = t.id WHERE c.id = $1`, callID).Scan(&waiting); err != nil || !waiting {
		t.Fatalf("message waiting=%v %v", waiting, err)
	}
}

// An AI leg that ends before the AI said anything leaves the caller a
// message instead of hanging up on them.
func TestASilentAILegTakesAMessage(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	o := &voice.OpenAI{APIKey: "k", ProjectID: "proj", WebhookSecret: "whsec_c2VjcmV0"}
	voice.EnableAI(o)
	t.Cleanup(func() { voice.EnableAI(nil) })
	h := s.Routes()
	flow := voice.Flow{Answer: voice.Action{Mode: voice.AI}, Agent: &voice.Agent{Opening: "Hello."}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}
	signedSMS(h, voice.PathIncoming, callForm("CA-silent"), "signing-key", smsPublicURL)
	if _, err := s.DB.Exec(ctx, `UPDATE calls SET ai_call_id = 'rtc_x' WHERE call_sid = 'CA-silent'`); err != nil {
		t.Fatal(err)
	}
	if body := signedSMS(h, voice.PathAIDial, callForm("CA-silent", "DialCallStatus", "completed"), "signing-key", smsPublicURL).Body.String(); !strings.Contains(body, "<Record") {
		t.Fatalf("silent AI leg: %s", body)
	}
}

// fakeLive is GPT-Live: it accepts sessions and plays one call over the
// sideband, greeting, hearing the caller in fragments, and having its backend
// take a message.
type fakeLive struct {
	mu       sync.Mutex
	accepted map[string]map[string]any
	rejected []string
	greeting string
	result   map[string]any
}

func (f *fakeLive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/live/sessions/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, verb, _ := strings.Cut(rest, "/")
	if verb == "attach" {
		f.converse(w, r)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch verb {
	case "accept":
		f.accepted[id], _ = body["session"].(map[string]any)
	case "reject":
		f.rejected = append(f.rejected, id)
	}
}

func (f *fakeLive) converse(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	send := func(v map[string]any) {
		raw, _ := json.Marshal(v)
		conn.Write(ctx, websocket.MessageText, raw)
	}
	read := func() map[string]any {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		var v map[string]any
		json.Unmarshal(raw, &v)
		return v
	}
	ev := read()
	f.mu.Lock()
	f.greeting, _ = ev["content"].(string)
	f.mu.Unlock()
	for _, d := range []string{"Hi, thanks for calling ", "Cedar Court."} {
		send(map[string]any{"type": "session.output_transcript.delta", "delta": d})
	}
	for _, d := range []string{"Can I book a cleaning ", "in March?"} {
		send(map[string]any{"type": "session.input_transcript.delta", "delta": d})
	}
	send(map[string]any{"type": "response.event", "delegation_id": "item_1", "event": map[string]any{"type": "response.function_call_arguments.done", "arguments": "{}"}})
	send(map[string]any{"type": "response.event", "delegation_id": "item_1", "event": map[string]any{"type": "response.output_item.done",
		"item": map[string]any{"type": "function_call", "name": "take_message", "call_id": "call_1",
			"arguments": `{"caller_name":"Dana","callback_number":"+13105550188","reason":"Room in March","summary":"Wants a March appointment."}`}}})
	result := read()
	if next := read(); next["type"] != "response.create" {
		return
	}
	f.mu.Lock()
	f.result = result
	f.mu.Unlock()
	send(map[string]any{"type": "session.closed", "reason": "remote_hangup"})
}

func liveIncoming(sessionID, callID string) string {
	return fmt.Sprintf(`{"type":"live.transport.incoming","data":{"type":"sip","session_id":%q,"sip_headers":[{"name":"X-VisiComms-Call","value":%q}]}}`, sessionID, callID)
}

func TestGPTLiveAnswersAndTakesAMessage(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	fake := &fakeLive{accepted: map[string]map[string]any{}}
	api := httptest.NewServer(fake)
	defer api.Close()
	secret := []byte("0123456789abcdef")
	o := &voice.OpenAI{APIKey: "sk-test", ProjectID: "proj_test", WebhookSecret: "whsec_" + base64.StdEncoding.EncodeToString(secret), API: voice.APILive,
		BaseURL: api.URL, SocketURL: "ws" + strings.TrimPrefix(api.URL, "http")}
	voice.EnableAI(o)
	t.Cleanup(func() { voice.EnableAI(nil) })
	s.OpenAI = o
	h := s.Routes()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	flow := voice.Flow{Answer: voice.Action{Mode: voice.AI}, Agent: &voice.Agent{Opening: "Hi, thanks for calling Cedar Court.", Voice: "gleam"}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", flow, true); err != nil {
		t.Fatal(err)
	}
	signedSMS(h, voice.PathIncoming, callForm("CA-live"), "signing-key", smsPublicURL)
	var callID string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM calls WHERE call_sid = 'CA-live'`).Scan(&callID); err != nil {
		t.Fatal(err)
	}

	// The realtime API announces the same call; GPT-Live answers it.
	signedWebhook(h, now, secret, "wh_rt", incoming("rtc_1", callID))
	// A Live webhook for another transport is not a phone call.
	signedWebhook(h, now, secret, "wh_web", `{"type":"live.transport.incoming","data":{"type":"webrtc","session_id":"live_web"}}`)
	if rec := signedWebhook(h, now, secret, "wh_live", liveIncoming("live_1", callID)); rec.Code != 200 {
		t.Fatalf("webhook = %d %s", rec.Code, rec.Body)
	}

	fake.mu.Lock()
	session, accepted := fake.accepted["live_1"]
	accepts, rejected := len(fake.accepted), len(fake.rejected)
	fake.mu.Unlock()
	if !accepted || accepts != 1 || rejected != 0 {
		t.Fatalf("accepted %v (%d), rejected %d", accepted, accepts, rejected)
	}
	if session["type"] != "live" || session["model"] != voice.LiveModel {
		t.Fatalf("session = %v %v", session["type"], session["model"])
	}
	audio, _ := json.Marshal(session["audio"])
	if string(audio) != `{"output":{"voice":"gleam"}}` {
		t.Fatalf("audio = %s; SIP negotiates the format", audio)
	}
	delegation, _ := json.Marshal(session["delegation"])
	if !strings.Contains(string(delegation), `"take_message"`) || strings.Contains(string(delegation), "transfer_to_person") {
		t.Fatalf("delegation without anyone to transfer to = %s", delegation)
	}

	var transcript, summary string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s.DB.QueryRow(ctx, `SELECT payload->>'transcript', payload->>'summary' FROM events WHERE type = 'call.ai_handled' AND payload->>'call_id' = $1`, callID).Scan(&transcript, &summary) == nil {
			break
		}
	}
	fake.mu.Lock()
	greeting, result := fake.greeting, fake.result
	fake.mu.Unlock()
	if !strings.Contains(greeting, `"This call may be recorded. Hi, thanks for calling Cedar Court."`) {
		t.Fatalf("greeting = %q", greeting)
	}
	if item, _ := result["item"].(map[string]any); result["type"] != "response.item.create" || item["call_id"] != "call_1" || !strings.Contains(fmt.Sprint(item["output"]), "recorded") {
		t.Fatalf("tool result = %v", result)
	}
	for _, want := range []string{`"text": "Hi, thanks for calling Cedar Court."`, `"text": "Can I book a cleaning in March?"`} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript %s lacks %s", transcript, want)
		}
	}
	if !strings.Contains(summary, "Room in March") {
		t.Fatalf("summary = %q", summary)
	}
}
