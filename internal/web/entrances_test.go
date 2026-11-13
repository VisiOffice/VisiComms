package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const gateNumber = "+14155550777"

// A gate's flow: forward, so the markup shows it ran instead of the number's
// voicemail.
const gateFlow = `{"answer":{"mode":"forward","forward_to":"+13105550111"},"ring_seconds":20,"greeting":"Nobody is at the gate."}`

func TestGateCallsRunTheEntrancesFlow(t *testing.T) {
	s, ws := smsServer(t)
	s.Auth = &auth.Service{DB: s.DB}
	h := s.Routes()
	ctx := context.Background()
	session, _ := consoleSession(t, s, ws, "Morgan", "morgan@example.com")

	// The gate has called before, as an ordinary caller.
	if rec := signedSMS(h, voice.PathIncoming, callForm("CA-before", "From", gateNumber), "signing-key", smsPublicURL); rec.Code != 200 || strings.Contains(rec.Body.String(), "+13105550111") {
		t.Fatalf("call before the entrance = %d %s", rec.Code, rec.Body)
	}
	var before string
	if err := s.DB.QueryRow(ctx, `SELECT thread_id FROM calls WHERE call_sid = 'CA-before'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	rec := consoleCall(h, "POST", "/api/entrances", session, `{"name":"Front gate","caller_id":"+1 (415) 555-0777","flow":`+gateFlow+`}`)
	if rec.Code != 201 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var gate voice.Entrance
	json.Unmarshal(rec.Body.Bytes(), &gate)
	if gate.CallerID != gateNumber {
		t.Fatalf("caller ID = %q", gate.CallerID)
	}
	if rec := consoleCall(h, "POST", "/api/entrances", session, `{"name":"Back gate","caller_id":"`+gateNumber+`","flow":`+gateFlow+`}`); rec.Code != 409 {
		t.Fatalf("a second entrance with the same caller ID = %d", rec.Code)
	}

	// From the gate's caller ID, the entrance's flow runs, on the thread the
	// gate already had, now named after it.
	rec = signedSMS(h, voice.PathIncoming, callForm("CA-gate", "From", gateNumber), "signing-key", smsPublicURL)
	if !strings.Contains(rec.Body.String(), "<Number>+13105550111</Number>") {
		t.Fatalf("gate call = %s", rec.Body)
	}
	var thread, name, entrance string
	if err := s.DB.QueryRow(ctx, `SELECT c.thread_id, ct.display_name, coalesce(c.entrance_id::text, '') FROM calls c JOIN threads t ON t.id = c.thread_id
		JOIN contacts ct ON ct.id = t.contact_id WHERE c.call_sid = 'CA-gate'`).Scan(&thread, &name, &entrance); err != nil {
		t.Fatal(err)
	}
	if thread != before || name != "Front gate" || entrance != gate.ID {
		t.Fatalf("gate call on thread %s (before %s), contact %q, entrance %q", thread, before, name, entrance)
	}
	// Anyone else still gets the number's flow.
	if rec := signedSMS(h, voice.PathIncoming, callForm("CA-other"), "signing-key", smsPublicURL); strings.Contains(rec.Body.String(), "+13105550111") {
		t.Fatalf("another caller ran the gate's flow: %s", rec.Body)
	}

	// A SIP account: SignalWire is asked to hand its calls to VisiComms.
	var asked map[string]any
	s.SignalWire.Installation.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://test.signalwire.com/api/relay/rest/endpoints/sip" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL)
		}
		json.NewDecoder(r.Body).Decode(&asked)
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"id":"ep-1"}`)), Header: http.Header{}}, nil
	})}
	rec = consoleCall(h, "POST", "/api/entrances/"+gate.ID+"/sip", session, `{}`)
	if rec.Code != 200 {
		t.Fatalf("SIP account = %d %s", rec.Code, rec.Body)
	}
	var account voice.SIPAccount
	json.Unmarshal(rec.Body.Bytes(), &account)
	if account.Server != "test.sip.signalwire.com" || !strings.HasPrefix(account.Username, "gate-") || len(account.Password) < 20 || account.Port != 5060 {
		t.Fatalf("account = %+v", account)
	}
	if asked["username"] != account.Username || asked["password"] != account.Password || asked["call_handler"] != "laml_webhooks" ||
		asked["call_request_url"] != smsPublicURL+voice.PathIncoming || asked["call_status_callback_url"] != smsPublicURL+voice.PathStatus {
		t.Fatalf("SignalWire was asked for %v", asked)
	}
	// The password is shown once and never again.
	rec = consoleCall(h, "GET", "/api/entrances", session, "")
	if strings.Contains(rec.Body.String(), account.Password) || !strings.Contains(rec.Body.String(), account.Username) || !strings.Contains(rec.Body.String(), `"sip_available":true`) {
		t.Fatalf("list = %s", rec.Body)
	}

	// The intercom calling from its SIP account runs the entrance's flow,
	// with no number dialled.
	sipCall := func(sid, user string) string {
		form := url.Values{"AccountSid": {smsProjectID}, "CallSid": {sid}, "From": {"sip:" + user + "@test.sip.signalwire.com"}, "To": {"sip:1000@test.sip.signalwire.com"}}
		rec := signedSMS(h, voice.PathIncoming, form, "signing-key", smsPublicURL)
		if rec.Code != 200 {
			t.Fatalf("SIP call = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if got := sipCall("CA-sip", account.Username); !strings.Contains(got, "<Number>+13105550111</Number>") {
		t.Fatalf("SIP call = %s", got)
	}
	sipCall("CA-sip", account.Username) // repeated
	sipCall("CA-sip-2", account.Username)
	var calls, threads int
	var number *string
	if err := s.DB.QueryRow(ctx, `SELECT count(*), count(DISTINCT thread_id), max(number) FROM calls WHERE call_sid LIKE 'CA-sip%' AND entrance_id::text = $1`, gate.ID).Scan(&calls, &threads, &number); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || threads != 1 || number != nil {
		t.Fatalf("SIP calls: %d calls on %d threads, number %v", calls, threads, number)
	}
	// Its status callback finds the call.
	if rec := signedSMS(h, voice.PathStatus, url.Values{"AccountSid": {smsProjectID}, "CallSid": {"CA-sip-2"}, "CallStatus": {"no-answer"}}, "signing-key", smsPublicURL); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var outcome string
	if err := s.DB.QueryRow(ctx, `SELECT coalesce(outcome, '') FROM calls WHERE call_sid = 'CA-sip-2'`).Scan(&outcome); err != nil || outcome != "missed" {
		t.Fatalf("outcome = %q, %v", outcome, err)
	}
	// Any other SIP account is refused.
	if got := sipCall("CA-stranger", "gate-unknown"); !strings.Contains(got, "<Hangup/>") {
		t.Fatalf("unknown SIP account = %s", got)
	}

	// Removing the entrance removes its SIP account at SignalWire.
	var deleted string
	s.SignalWire.Installation.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		deleted = r.Method + " " + r.URL.Path
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}
	if rec := consoleCall(h, "DELETE", "/api/entrances/"+gate.ID, session, ""); rec.Code != 200 || deleted != "DELETE /api/relay/rest/endpoints/sip/ep-1" {
		t.Fatalf("delete = %d, SignalWire saw %q", rec.Code, deleted)
	}
	if got := sipCall("CA-after", account.Username); !strings.Contains(got, "<Hangup/>") {
		t.Fatalf("a removed entrance's SIP account = %s", got)
	}
}
