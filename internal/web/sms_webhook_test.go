package web_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/secrets"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/web"
	"github.com/google/uuid"
)

const smsPublicURL = "https://comms.example.com"
const smsProjectID = "test-project"
const smsOurNumber = "+14155550123"
const smsCustomer = "+14155550999"

func smsServer(t *testing.T) (*web.Server, string) {
	t.Helper()
	s, _ := server(t)
	ctx := context.Background()
	sw, err := sending.NewSignalWire("test.signalwire.com", smsProjectID, "token", "signing-key", smsPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	s.SignalWire = &sending.SignalWireProjects{DB: s.DB, Installation: sw, PublicURL: smsPublicURL}
	var ws, connector string
	if err := s.DB.QueryRow(ctx, `SELECT workspace_id FROM inbound_addresses LIMIT 1`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO connectors(workspace_id,kind,name,settings,health) VALUES($1,'signalwire','SMS','{"from_address":"+14155550123","project_id":"test-project"}','ok') RETURNING id`, ws).Scan(&connector); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO phone_numbers(number,workspace_id,connector_id,provider_project_id,capabilities) VALUES($1,$2,$3,$4,'{"sms":true}')`, smsOurNumber, ws, connector, smsProjectID); err != nil {
		t.Fatal(err)
	}
	return s, ws
}
func signedSMS(h http.Handler, path string, form url.Values, key, origin string) *httptest.ResponseRecorder {
	keys := []string{}
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	payload := origin + path
	for _, k := range keys {
		payload += k + form.Get(k)
	}
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(payload))
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-SignalWire-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	// The internal proxy's Host is not the canonical signed URL.
	req.Host = "internal-proxy:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
func incomingSMS() url.Values {
	return url.Values{"AccountSid": {smsProjectID}, "From": {smsCustomer}, "To": {smsOurNumber}, "Body": {"Can you help?"}, "MessageSid": {"SM-in"}}
}

func TestSMSHTTPRejectsUnauthenticatedAndWrongProject(t *testing.T) {
	s, _ := smsServer(t)
	form := incomingSMS()
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "wrong", smsPublicURL); rec.Code != 403 {
		t.Fatalf("bad signature=%d", rec.Code)
	}
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "signing-key", "https://attacker.example"); rec.Code != 403 {
		t.Fatalf("wrong canonical URL=%d", rec.Code)
	}
	form.Set("AccountSid", "other-project")
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "signing-key", smsPublicURL); rec.Code != 403 {
		t.Fatalf("wrong project=%d", rec.Code)
	}
	var n int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type='sms.received'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("forged event stored=%d,%v", n, err)
	}
}

// A workspace's own project is verified with its own signing key; the
// installation's key does not open another project's callbacks.
func TestSMSHTTPWorkspaceProject(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	keys, _ := secrets.New(bytes.Repeat([]byte{3}, 32))
	s.SignalWire.Keys = keys
	connector := uuid.NewString()
	sealed, err := keys.Seal(ctx, s.DB, ws, connector, []byte(`{"token":"own-token","signing_key":"own-key"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO connectors(id,workspace_id,kind,name,settings,credentials_ciphertext,health)
   VALUES($1,$2,'signalwire_project','SignalWire project','{"space":"own.signalwire.com","project_id":"own-project"}',$3,'ok')`, connector, ws, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE phone_numbers SET provider_project_id='own-project' WHERE number=$1`, smsOurNumber); err != nil {
		t.Fatal(err)
	}
	form := incomingSMS()
	form.Set("AccountSid", "own-project")
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "signing-key", smsPublicURL); rec.Code != 403 {
		t.Fatalf("installation key on a workspace project=%d", rec.Code)
	}
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", incomingSMS(), "signing-key", smsPublicURL); rec.Code != 404 {
		t.Fatalf("number moved off the installation project still received there=%d", rec.Code)
	}
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "own-key", smsPublicURL); rec.Code != 200 {
		t.Fatalf("own project=%d %s", rec.Code, rec.Body)
	}
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='sms.received' AND workspace_id=$1`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("received=%d,%v", n, err)
	}
}

func TestSMSHTTPLoopAndDeliveryCallbacks(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	for range 2 {
		rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", incomingSMS(), "signing-key", smsPublicURL)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<Response>") {
			t.Fatalf("inbound=%d %s", rec.Code, rec.Body)
		}
	}
	var thread, person string
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='sms.received'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate inbound=%d,%v", n, err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT thread_id FROM events WHERE type='sms.received'`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals(workspace_id,kind,name) VALUES($1,'human','Operator') RETURNING id`, ws).Scan(&person); err != nil {
		t.Fatal(err)
	}
	reviewer := review.Reviewer{WorkspaceID: ws, ID: person, SMSProject: smsProjectID}
	reply, err := review.Reply(ctx, s.DB, reviewer, thread, review.EmailPayload{Body: "We can help"})
	if err != nil {
		t.Fatal(err)
	}
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"sid":"SM-out"}`))}, nil
	})}
	if out, err := sending.DispatchSMS(ctx, s.DB, s.SignalWire.Installation, reply.ActionID); err != nil || out != sending.Sent {
		t.Fatalf("send=%s,%v", out, err)
	}
	status := url.Values{"AccountSid": {smsProjectID}, "MessageSid": {"SM-out"}, "MessageStatus": {"delivered"}, "From": {smsOurNumber}, "To": {smsCustomer}}
	for range 2 {
		if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms/status", status, "signing-key", smsPublicURL); rec.Code != 200 {
			t.Fatalf("status=%d %s", rec.Code, rec.Body)
		}
	}
	var state string
	if err := s.DB.QueryRow(ctx, `SELECT state FROM actions WHERE id=$1`, reply.ActionID).Scan(&state); err != nil || state != "delivered" {
		t.Fatalf("state=%s,%v", state, err)
	}
	stop := incomingSMS()
	stop.Set("MessageSid", "SM-stop")
	stop.Set("Body", "STOP")
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", stop, "signing-key", smsPublicURL); rec.Code != 200 {
		t.Fatalf("STOP=%d", rec.Code)
	}
	var blocked bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM opt_outs WHERE channel='sms')`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("STOP not applied=%v,%v", blocked, err)
	}
}

func TestSMSUnknownNumberAndJSONAreRejected(t *testing.T) {
	s, _ := smsServer(t)
	form := incomingSMS()
	form.Set("To", "+14155550000")
	if rec := signedSMS(s.Routes(), "/webhooks/signalwire/sms", form, "signing-key", smsPublicURL); rec.Code != 404 {
		t.Fatalf("unknown number=%d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/webhooks/signalwire/sms", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("unexpected JSON accepted=%d", rec.Code)
	}
}
