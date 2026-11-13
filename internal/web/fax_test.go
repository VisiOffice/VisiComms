package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/files"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFaxWebhookReceiveAndRetry(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(ctx, `UPDATE phone_numbers SET capabilities='{"fax":true}' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	form := url.Values{"AccountSid": {smsProjectID}, "From": {smsCustomer}, "To": {smsOurNumber}, "FaxSid": {"FX-in"}}
	if r := signedSMS(h, "/webhooks/signalwire/fax", form, "wrong", smsPublicURL); r.Code != 403 {
		t.Fatalf("bad signature=%d", r.Code)
	}
	if r := signedSMS(h, "/webhooks/signalwire/fax", form, "signing-key", smsPublicURL); r.Code != 200 || !strings.Contains(r.Body.String(), `<Receive action="https://comms.example.com/webhooks/signalwire/fax/received"`) {
		t.Fatalf("bootstrap=%d %s", r.Code, r.Body)
	}
	downloads := 0
	s.SignalWire.Installation.HTTP = &http.Client{Transport: resendHTTP(func(r *http.Request) (*http.Response, error) {
		downloads++
		if r.URL.Host != "files.signalwire.com" {
			t.Fatalf("unexpected media host %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("%PDF-1.4\nfixture\n%%EOF"))}, nil
	})}
	form.Set("FaxStatus", "received")
	form.Set("NumPages", "2")
	form.Set("MediaUrl", "https://files.signalwire.com/received.pdf")
	for range 2 {
		if r := signedSMS(h, "/webhooks/signalwire/fax/received", form, "signing-key", smsPublicURL); r.Code != 200 {
			t.Fatalf("receive=%d %s", r.Code, r.Body)
		}
	}
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM documents WHERE workspace_id=$1`, ws).Scan(&n); err != nil || n != 1 || downloads != 1 {
		t.Fatalf("documents=%d downloads=%d err=%v", n, downloads, err)
	}
	form.Set("FaxSid", "FX-evil")
	form.Set("MediaUrl", "https://attacker.example/fax.pdf")
	if r := signedSMS(h, "/webhooks/signalwire/fax/received", form, "signing-key", smsPublicURL); r.Code != 500 || downloads != 1 {
		t.Fatalf("external media fetched: status=%d downloads=%d", r.Code, downloads)
	}
	for _, path := range []string{"/api/fax/documents/00000000-0000-0000-0000-000000000000", "/fax-media/" + strings.Repeat("0", 64)} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code == 200 {
			t.Fatalf("private document accessible at %s", path)
		}
	}
}

func TestFaxUploadDownloadAndSendAreWorkspaceScoped(t *testing.T) {
	s, ws := smsServer(t)
	ctx := context.Background()
	s.Auth = &auth.Service{DB: s.DB}
	if _, err := s.DB.Exec(ctx, `UPDATE phone_numbers SET capabilities='{"fax":true}' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	var person, other string
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals(workspace_id,kind,name) VALUES($1,'human','Fax operator') RETURNING id`, ws).Scan(&person); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("fax-session"))
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_sha256,principal_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, digest[:], person); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	call := func(method, path, contentType, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Filename", "quote.pdf")
		req.AddCookie(&http.Cookie{Name: "vc_session", Value: "fax-session"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	r := call("POST", "/api/fax/documents", "application/pdf", "%PDF-1.4\nfixture\n%%EOF")
	if r.Code != 201 {
		t.Fatalf("upload=%d %s", r.Code, r.Body)
	}
	var doc files.Document
	if err := json.Unmarshal(r.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	r = call("GET", "/api/fax/documents/"+doc.ID, "", "")
	if r.Code != 200 || r.Header().Get("Content-Type") != "application/pdf" || !strings.Contains(r.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download=%d %s", r.Code, r.Body)
	}
	if r = call("POST", "/api/fax/documents", "application/pdf", "not a PDF"); r.Code != 400 {
		t.Fatalf("bad PDF=%d", r.Code)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces(name,timezone) VALUES('Other','UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	otherDoc, err := files.Store(ctx, s.DB, other, "private.pdf", []byte("%PDF-private"))
	if err != nil {
		t.Fatal(err)
	}
	if r = call("GET", "/api/fax/documents/"+otherDoc.ID, "", ""); r.Code != 404 {
		t.Fatalf("other workspace download=%d", r.Code)
	}
	payload := func(id string) string {
		b, _ := json.Marshal(map[string]string{"from": smsOurNumber, "to": smsCustomer, "document_id": id, "request_id": "11111111-1111-4111-8111-111111111111"})
		return string(b)
	}
	if r = call("POST", "/api/faxes", "application/json", payload(otherDoc.ID)); r.Code != 400 {
		t.Fatalf("other workspace send=%d %s", r.Code, r.Body)
	}
	for range 2 {
		if r = call("POST", "/api/faxes", "application/json", payload(doc.ID)); r.Code != 202 {
			t.Fatalf("send=%d %s", r.Code, r.Body)
		}
	}
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM actions WHERE workspace_id=$1 AND action='fax.send'`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("queued=%d %v", n, err)
	}
}
