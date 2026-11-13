package web_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/secrets"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/web"
)

// fakeLob answers like Lob's API and records the letters it is sent.
type fakeLob struct {
	t       *testing.T
	letters []*http.Request
	forms   []map[string]string
	files   [][]byte
	deleted []string
}

func (f *fakeLob) RoundTrip(r *http.Request) (*http.Response, error) {
	user, _, _ := r.BasicAuth()
	if !strings.HasPrefix(user, "live_") {
		return lobReply(401, `{"error":{"message":"Your API key is not valid.","status_code":401}}`), nil
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/addresses":
		return lobReply(200, `{"data":[]}`), nil
	case r.Method == "POST" && r.URL.Path == "/v1/us_verifications":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in["primary_line"], "Nowhere") {
			return lobReply(200, `{"deliverability":"undeliverable"}`), nil
		}
		out, _ := json.Marshal(map[string]any{"deliverability": "deliverable", "primary_line": strings.ToUpper(in["primary_line"]),
			"secondary_line": strings.ToUpper(in["secondary_line"]),
			"components":     map[string]string{"city": strings.ToUpper(in["city"]), "state": in["state"], "zip_code": in["zip_code"][:5], "zip_code_plus_4": "1234"}})
		return lobReply(200, string(out)), nil
	case r.Method == "POST" && r.URL.Path == "/v1/letters":
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			f.t.Fatal(err)
		}
		form := map[string]string{"Idempotency-Key": r.Header.Get("Idempotency-Key")}
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			f.t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		f.letters, f.forms, f.files = append(f.letters, r), append(f.forms, form), append(f.files, data)
		id := fmt.Sprintf("ltr_%d", len(f.letters))
		return lobReply(200, `{"id":"`+id+`","url":"https://lob-assets.example/`+id+`.pdf","expected_delivery_date":"2026-11-18","send_date":"2026-11-13T17:00:00Z"}`), nil
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/letters/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/v1/letters/"))
		return lobReply(200, `{"deleted":true}`), nil
	}
	f.t.Fatalf("unexpected Lob call %s %s", r.Method, r.URL)
	return nil, nil
}

func lobReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func lobWebhook(h http.Handler, connector, secret, body string) *httptest.ResponseRecorder {
	stamp := strconv.FormatInt(clock.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "." + body))
	req := httptest.NewRequest("POST", "/webhooks/lob/"+connector, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Lob-Signature-Timestamp", stamp)
	req.Header.Set("Lob-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func lobEvent(id, typ, letter string) string {
	return `{"id":"` + id + `","object":"event","event_type":{"id":"` + typ + `","resource":"letters"},"body":{"id":"` + letter + `","tracking_number":"9400100000000000000000"}}`
}

func mailServer(t *testing.T) (*web.Server, *fakeLob, string) {
	t.Helper()
	s, _ := server(t)
	s.Auth = &auth.Service{DB: s.DB}
	keys, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	lob := &fakeLob{t: t}
	s.Mail = &sending.LobAccounts{DB: s.DB, Keys: keys, PublicURL: "https://api.example.com", Base: "https://lob.test/v1", HTTP: &http.Client{Transport: lob}}
	var ws string
	if err := s.DB.QueryRow(context.Background(), `SELECT id FROM workspaces LIMIT 1`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	return s, lob, ws
}

// twoPagePDF has two page objects, as an uncompressed PDF does.
const twoPagePDF = "%PDF-1.4\n1 0 obj << /Type /Page >> endobj\n2 0 obj << /Type /Page >> endobj\n3 0 obj << /Type /Pages /Count 2 >> endobj\n%%EOF"

func TestALetterGoesOutThroughTheWorkspacesLobAccount(t *testing.T) {
	s, lob, ws := mailServer(t)
	ctx := context.Background()
	h := s.Routes()
	session, _ := consoleSession(t, s, ws, "mail-operator", "ops@example.com")

	// Connect Lob: the key is checked, the return address standardised.
	ret := `"return_address":{"name":"Acme Plumbing","line1":"100 Main St","city":"Phoenix","state":"az","zip":"85001"}`
	if r := consoleCall(h, "PUT", "/api/mail", session, `{"api_key":"pk_wrong",`+ret+`}`); r.Code != 400 {
		t.Fatalf("a publishable key was accepted: %d %s", r.Code, r.Body)
	}
	r := consoleCall(h, "PUT", "/api/mail", session, `{"api_key":"live_secret","webhook_secret":"whsec_1","monthly_cap_cents":500,`+ret+`}`)
	if r.Code != 200 {
		t.Fatalf("connect=%d %s", r.Code, r.Body)
	}
	var settings sending.MailSettings
	json.Unmarshal(r.Body.Bytes(), &settings)
	if settings.Mode != "live" || settings.From != "100 MAIN ST, PHOENIX AZ 85001-1234" || !settings.HasWebhookSecret {
		t.Fatalf("settings=%+v", settings)
	}
	r = consoleCall(h, "GET", "/api/mail", session, "")
	if r.Code != 200 || strings.Contains(r.Body.String(), "live_secret") || strings.Contains(r.Body.String(), "whsec_1") {
		t.Fatalf("mail settings=%d %s", r.Code, r.Body)
	}
	// Changing the limit without re-entering the key keeps it.
	if r := consoleCall(h, "PUT", "/api/mail", session, `{"monthly_cap_cents":500,`+ret+`}`); r.Code != 200 {
		t.Fatalf("update without key=%d %s", r.Code, r.Body)
	}

	// A contact's address is checked before it is saved.
	var contact string
	if err := s.DB.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Pat Debtor') RETURNING id`, ws).Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if r := consoleCall(h, "POST", "/api/contacts/"+contact+"/addresses", session, `{"name":"Pat Debtor","line1":"1 Nowhere Rd","city":"Mesa","state":"AZ","zip":"85201"}`); r.Code != 400 {
		t.Fatalf("undeliverable address saved: %d %s", r.Code, r.Body)
	}
	r = consoleCall(h, "POST", "/api/contacts/"+contact+"/addresses", session, `{"name":"Pat Debtor","line1":"22 Oak Ave","line2":"Apt 4","city":"Mesa","state":"AZ","zip":"85201"}`)
	if r.Code != 201 {
		t.Fatalf("address=%d %s", r.Code, r.Body)
	}
	var address struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &address)

	// Upload the PDF and price it.
	req := httptest.NewRequest("POST", "/api/fax/documents", strings.NewReader(twoPagePDF))
	req.Header.Set("Content-Type", "application/pdf")
	req.AddCookie(&http.Cookie{Name: "vc_session", Value: session})
	up := httptest.NewRecorder()
	h.ServeHTTP(up, req)
	var doc struct{ ID string }
	json.Unmarshal(up.Body.Bytes(), &doc)
	r = consoleCall(h, "POST", "/api/letters/estimate", session, `{"document_id":"`+doc.ID+`","extra_service":"certified"}`)
	var est struct {
		Pages, Sheets int
		CostCents     int `json:"cost_cents"`
	}
	json.Unmarshal(r.Body.Bytes(), &est)
	if r.Code != 200 || est.Pages != 2 || est.Sheets != 3 || est.CostCents < 500 {
		t.Fatalf("estimate=%d %s", r.Code, r.Body)
	}
	if r := consoleCall(h, "POST", "/api/letters", session, `{"contact_id":"`+contact+`","address_id":"`+address.ID+`","document_id":"`+doc.ID+`","extra_service":"certified"}`); r.Code != 409 {
		t.Fatalf("a letter over the monthly limit was queued: %d %s", r.Code, r.Body)
	}

	// Send it, and run the executor as the job would.
	send := func() (string, string) {
		t.Helper()
		r := consoleCall(h, "POST", "/api/letters", session, `{"contact_id":"`+contact+`","address_id":"`+address.ID+`","document_id":"`+doc.ID+`","double_sided":true}`)
		if r.Code != 202 {
			t.Fatalf("letter=%d %s", r.Code, r.Body)
		}
		var out struct {
			ActionID string `json:"action_id"`
			ThreadID string `json:"thread_id"`
		}
		json.Unmarshal(r.Body.Bytes(), &out)
		client, owner, err := s.Mail.Client(ctx, settings.ConnectorID)
		if err != nil || owner != ws {
			t.Fatal(owner, err)
		}
		if outcome, err := sending.DispatchLetter(ctx, s.DB, client, out.ActionID); err != nil || outcome != sending.Sent {
			t.Fatalf("dispatch=%s %v", outcome, err)
		}
		return out.ActionID, out.ThreadID
	}
	first, thread := send()
	form := lob.forms[0]
	if form["Idempotency-Key"] != first || form["metadata[action_id]"] != first || form["to[address_line1]"] != "22 OAK AVE" ||
		form["to[address_zip]"] != "85201-1234" || form["from[name]"] != "Acme Plumbing" || form["use_type"] != "operational" ||
		form["address_placement"] != "insert_blank_page" || form["double_sided"] != "true" || string(lob.files[0]) != twoPagePDF {
		t.Fatalf("Lob got %v", form)
	}

	// Cancelled from the console; Lob's own "deleted" event adds nothing.
	if r := consoleCall(h, "POST", "/api/letters/"+first+"/cancel", session, `{}`); r.Code != 200 || len(lob.deleted) != 1 || lob.deleted[0] != "ltr_1" {
		t.Fatalf("cancel=%d %s %v", r.Code, r.Body, lob.deleted)
	}
	if r := lobWebhook(h, settings.ConnectorID, "whsec_1", lobEvent("evt_del", "letter.deleted", "ltr_1")); r.Code != 200 {
		t.Fatalf("deleted webhook=%d %s", r.Code, r.Body)
	}
	if r := consoleCall(h, "POST", "/api/letters/"+first+"/cancel", session, `{}`); r.Code != 409 {
		t.Fatalf("cancelled twice: %d", r.Code)
	}

	// A second letter is tracked, then comes back.
	second, _ := send()
	if r := lobWebhook(h, settings.ConnectorID, "wrong", lobEvent("evt_x", "letter.in_transit", "ltr_2")); r.Code != 403 {
		t.Fatalf("bad signature=%d", r.Code)
	}
	for i, typ := range []string{"letter.certified.in_transit", "letter.in_transit", "letter.returned_to_sender"} {
		if r := lobWebhook(h, settings.ConnectorID, "whsec_1", lobEvent(fmt.Sprintf("evt_%d", i), typ, "ltr_2")); r.Code != 200 {
			t.Fatalf("%s=%d %s", typ, r.Code, r.Body)
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT type FROM events WHERE thread_id = $1 ORDER BY id`, thread)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for rows.Next() {
		var typ string
		rows.Scan(&typ)
		types = append(types, typ)
	}
	want := "postal.queued postal.sent postal.cancelled postal.queued postal.sent postal.in_transit postal.in_transit postal.returned"
	if strings.Join(types, " ") != want {
		t.Fatalf("timeline=%v", types)
	}
	var state string
	var returned bool
	if err := s.DB.QueryRow(ctx, `SELECT a.state, p.returned_at IS NOT NULL FROM actions a, postal_addresses p WHERE a.id = $1 AND p.identity_id = $2`, second, address.ID).Scan(&state, &returned); err != nil || state != "failed" || !returned {
		t.Fatalf("state=%s returned=%v err=%v", state, returned, err)
	}
	// No more letters to an address the post office sent back.
	if r := consoleCall(h, "POST", "/api/letters", session, `{"contact_id":"`+contact+`","address_id":"`+address.ID+`","document_id":"`+doc.ID+`"}`); r.Code != 400 {
		t.Fatalf("letter to a returned address=%d %s", r.Code, r.Body)
	}
}

func TestLobEventsOnlyMatchTheirOwnAccountsLetters(t *testing.T) {
	s, _, ws := mailServer(t)
	ctx := context.Background()
	h := s.Routes()
	var other, ours, otherConnector string
	if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		ws  string
		out *string
	}{{ws, &ours}, {other, &otherConnector}} {
		session, _ := consoleSession(t, s, w.ws, "op-"+w.ws, w.ws+"@example.com")
		r := consoleCall(h, "PUT", "/api/mail", session, `{"api_key":"live_k","webhook_secret":"secret-`+w.ws+`","monthly_cap_cents":5000,"return_address":{"name":"X","line1":"1 A St","city":"Mesa","state":"AZ","zip":"85201"}}`)
		var settings sending.MailSettings
		json.Unmarshal(r.Body.Bytes(), &settings)
		*w.out = settings.ConnectorID
	}
	// A letter of ours, accepted by Lob as ltr_ours.
	var contact, thread, action string
	s.DB.QueryRow(ctx, `INSERT INTO contacts (workspace_id) VALUES ($1) RETURNING id`, ws).Scan(&contact)
	s.DB.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel) VALUES ($1, $2, 'postal') RETURNING id`, ws, contact).Scan(&thread)
	var principal string
	s.DB.QueryRow(ctx, `SELECT id FROM principals WHERE workspace_id = $1 LIMIT 1`, ws).Scan(&principal)
	if err := s.DB.QueryRow(ctx, `INSERT INTO actions (workspace_id, thread_id, requested_by, connector_id, action, payload, payload_sha256, state, idempotency_key)
		VALUES ($1, $2, $3, $4, 'postal.send', '{}', '\x00', 'accepted', 'letter:test') RETURNING id`, ws, thread, principal, ours).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO delivery_attempts (action_id, connector_id, provider_message_id, outcome, provider_response) VALUES ($1, $2, 'ltr_ours', 'accepted', '{}')`, action, ours); err != nil {
		t.Fatal(err)
	}
	// The other workspace's account signs an event naming our letter.
	if r := lobWebhook(h, otherConnector, "secret-"+other, lobEvent("evt_spoof", "letter.returned_to_sender", "ltr_ours")); r.Code != 200 {
		t.Fatalf("webhook=%d %s", r.Code, r.Body)
	}
	if err := sending.ReconcileEvents(ctx, s.DB); err != nil {
		t.Fatal(err)
	}
	var state string
	s.DB.QueryRow(ctx, `SELECT state FROM actions WHERE id = $1`, action).Scan(&state)
	if state != "accepted" {
		t.Fatalf("another account's event changed our letter: %s", state)
	}
	if r := lobWebhook(h, ours, "secret-"+ws, lobEvent("evt_real", "letter.processed_for_delivery", "ltr_ours")); r.Code != 200 {
		t.Fatalf("webhook=%d", r.Code)
	}
	s.DB.QueryRow(ctx, `SELECT state FROM actions WHERE id = $1`, action).Scan(&state)
	if state != "delivered" {
		t.Fatalf("state=%s", state)
	}
}
