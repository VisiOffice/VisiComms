package web_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

func callForm(sid string, extra ...string) url.Values {
	f := url.Values{"AccountSid": {smsProjectID}, "CallSid": {sid}, "From": {smsCustomer}, "To": {smsOurNumber}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

func TestCallsFollowTheNumbersFlowAndEndInVoicemail(t *testing.T) {
	s, ws := smsServer(t)
	h := s.Routes()
	ctx := context.Background()
	call := func(path string, form url.Values) string {
		t.Helper()
		rec := signedSMS(h, path, form, "signing-key", smsPublicURL)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/xml" {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	events := func(callSID string) []string {
		rows, err := s.DB.Query(ctx, `SELECT e.type FROM events e JOIN calls c ON e.payload->>'call_id' = c.id::text WHERE c.call_sid = $1 ORDER BY e.id`, callSID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ string
			rows.Scan(&typ)
			out = append(out, typ)
		}
		return out
	}

	if rec := signedSMS(h, voice.PathIncoming, callForm("CA-0"), "wrong", smsPublicURL); rec.Code != 403 {
		t.Fatalf("unsigned call = %d", rec.Code)
	}
	if body := call(voice.PathIncoming, callForm("CA-x", "To", "+14155550000")); !strings.Contains(body, "<Hangup/>") {
		t.Fatalf("call to an unknown number: %s", body)
	}

	// No flow yet: take a message.
	if body := call(voice.PathIncoming, callForm("CA-1")); !strings.Contains(body, `<Record`) || !strings.Contains(body, voice.PathRecorded) {
		t.Fatalf("default flow: %s", body)
	}
	call(voice.PathRecorded, callForm("CA-1", "RecordingUrl", "https://test.signalwire.com/rec/1", "RecordingDuration", "12"))
	call(voice.PathTranscript, callForm("CA-1", "TranscriptionStatus", "completed", "TranscriptionText", "Hi, is Tuesday at 4 still free?"))
	if got := strings.Join(events("CA-1"), ","); got != "call.received,voice.voicemail,voice.transcript" {
		t.Fatalf("events = %s", got)
	}
	var untrusted bool
	if err := s.DB.QueryRow(ctx, `SELECT untrusted FROM events WHERE type = 'voice.transcript'`).Scan(&untrusted); err != nil || !untrusted {
		t.Fatalf("transcript untrusted=%v %v", untrusted, err)
	}

	// Forward, unanswered: voicemail.
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", voice.Flow{Answer: voice.Action{Mode: voice.Forward, ForwardTo: "+13105550100"}}, true); err != nil {
		t.Fatal(err)
	}
	if body := call(voice.PathIncoming, callForm("CA-2")); !strings.Contains(body, "<Number>+13105550100</Number>") || !strings.Contains(body, `timeout="20"`) {
		t.Fatalf("forward: %s", body)
	}
	if body := call(voice.PathDial, callForm("CA-2", "DialCallStatus", "no-answer")); !strings.Contains(body, "<Record") {
		t.Fatalf("unanswered forward: %s", body)
	}
	// The caller hangs up during the greeting.
	call(voice.PathStatus, callForm("CA-2", "CallStatus", "completed"))
	if got := strings.Join(events("CA-2"), ","); got != "call.received,call.missed" {
		t.Fatalf("events = %s", got)
	}

	// Forward, answered.
	call(voice.PathIncoming, callForm("CA-3"))
	if body := call(voice.PathDial, callForm("CA-3", "DialCallStatus", "completed")); !strings.Contains(body, "<Hangup/>") {
		t.Fatalf("answered: %s", body)
	}
	call(voice.PathStatus, callForm("CA-3", "CallStatus", "completed"))
	if got := strings.Join(events("CA-3"), ","); got != "call.received,call.answered" {
		t.Fatalf("events = %s", got)
	}

	// A recording that lands after the hang-up still counts as a voicemail.
	call(voice.PathIncoming, callForm("CA-4"))
	call(voice.PathStatus, callForm("CA-4", "CallStatus", "completed"))
	call(voice.PathRecorded, callForm("CA-4", "RecordingUrl", "https://test.signalwire.com/rec/4", "RecordingDuration", "5"))
	var outcome string
	if err := s.DB.QueryRow(ctx, `SELECT outcome FROM calls WHERE call_sid = 'CA-4'`).Scan(&outcome); err != nil || outcome != "voicemail" {
		t.Fatalf("outcome=%q %v", outcome, err)
	}

	// IVR: a key, or nothing.
	menu := voice.Flow{Answer: voice.Action{Mode: voice.IVR}, Menu: &voice.Menu{
		Prompt:   "For leasing press 1.",
		Options:  []voice.Option{{Digit: "1", Label: "Leasing", Action: voice.Action{Mode: voice.Forward, ForwardTo: "+13105550101"}}},
		NoChoice: voice.Action{Mode: voice.Voicemail},
	}}
	if _, err := voice.Save(ctx, s.DB, ws, smsOurNumber, "", menu, true); err != nil {
		t.Fatal(err)
	}
	if body := call(voice.PathIncoming, callForm("CA-5")); !strings.Contains(body, "<Gather") || !strings.Contains(body, "For leasing press 1.") {
		t.Fatalf("menu: %s", body)
	}
	if body := call(voice.PathMenu, callForm("CA-5", "Digits", "1")); !strings.Contains(body, "+13105550101") {
		t.Fatalf("pressed 1: %s", body)
	}
	if body := call(voice.PathMenu, callForm("CA-5")); !strings.Contains(body, "<Record") {
		t.Fatalf("pressed nothing: %s", body)
	}

	// Calls from the same number share one voice thread.
	var threads int
	if err := s.DB.QueryRow(ctx, `SELECT count(DISTINCT thread_id) FROM calls`).Scan(&threads); err != nil || threads != 1 {
		t.Fatalf("threads=%d %v", threads, err)
	}
	// The voicemail after the answered call leaves the caller waiting.
	var waiting bool
	if err := s.DB.QueryRow(ctx, `SELECT `+review.WaitingSQL+` IS NOT NULL FROM threads t WHERE t.channel = 'voice'`).Scan(&waiting); err != nil || !waiting {
		t.Fatalf("waiting=%v %v", waiting, err)
	}
}

func TestAgentsChangeCallFlowsByAutonomy(t *testing.T) {
	s, ws := smsServer(t)
	h := s.Routes()
	ctx := context.Background()
	a, token, err := agent.Create(ctx, s.DB, ws, "Claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/numbers/" + url.PathEscape(smsOurNumber) + "/call-flow"
	forward := `{"answer":{"mode":"forward","forward_to":"+13105550100"}}`

	if rec := agentRequest(h, "PUT", path, token, `{"answer":{"mode":"ai"}}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), "not available yet") {
		t.Fatalf("ai before it exists = %d %s", rec.Code, rec.Body)
	}
	if rec := agentRequest(h, "PUT", "/api/v1/numbers/+14155550000/call-flow", token, forward); rec.Code != 404 {
		t.Fatalf("unknown number = %d", rec.Code)
	}
	rec := agentRequest(h, "PUT", path, token, forward)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("approve-mode change = %d %s", rec.Code, rec.Body)
	}
	flows, err := voice.Get(ctx, s.DB, ws, smsOurNumber)
	if err != nil || flows.Active.Flow.Answer.Mode != voice.Voicemail || flows.Pending == nil {
		t.Fatalf("a pending change ran: %+v %v", flows, err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE principals SET autonomy = 'autonomous' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec := agentRequest(h, "PUT", path, token, forward); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"active"`) {
		t.Fatalf("autonomous change = %d %s", rec.Code, rec.Body)
	}
	if flows, _ := voice.Get(ctx, s.DB, ws, smsOurNumber); flows.Active.Flow.Answer.ForwardTo != "+13105550100" || flows.Pending != nil {
		t.Fatalf("after activating: %+v", flows)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE principals SET autonomy = 'observe' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec := agentRequest(h, "PUT", path, token, forward); rec.Code != 403 {
		t.Fatalf("observer change = %d", rec.Code)
	}
	if rec := agentRequest(h, "GET", "/api/v1/numbers", token, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "+13105550100") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
}
