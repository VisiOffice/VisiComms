package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
)

func agentRequest(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentAPINeedsALiveToken(t *testing.T) {
	s, h := server(t)
	ctx := context.Background()
	var ws, thread string
	if err := s.DB.QueryRow(ctx, `SELECT workspace_id FROM inbound_addresses LIMIT 1`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `WITH c AS (INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Ann') RETURNING id)
		INSERT INTO threads (workspace_id, contact_id, channel, subject) SELECT $1, id, 'email', 'Hi' FROM c RETURNING id`, ws).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	a, token, err := agent.Create(ctx, s.DB, ws, "Claude", nil)
	if err != nil {
		t.Fatal(err)
	}

	if rec := agentRequest(h, "GET", "/api/v1/threads", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}
	if rec := agentRequest(h, "GET", "/api/v1/threads", "vca_wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", rec.Code)
	}
	rec := agentRequest(h, "GET", "/api/v1/threads", token, "")
	var list struct {
		Threads []agent.ThreadSummary `json:"threads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); rec.Code != http.StatusOK || err != nil || len(list.Threads) != 1 {
		t.Fatalf("threads = %d %s", rec.Code, rec.Body)
	}

	// A form post is refused: drafts are JSON only.
	req := httptest.NewRequest("POST", "/api/v1/threads/"+thread+"/drafts", strings.NewReader("body=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	form := httptest.NewRecorder()
	h.ServeHTTP(form, req)
	if form.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post = %d", form.Code)
	}
	if rec := agentRequest(h, "POST", "/api/v1/threads/nope/drafts", token, `{"body":"hi"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown thread = %d", rec.Code)
	}
	// Ann has no email address, so the draft is denied, not stored.
	rec = agentRequest(h, "POST", "/api/v1/threads/"+thread+"/drafts", token, `{"body":"hi"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"denied"`) {
		t.Fatalf("draft = %d %s", rec.Code, rec.Body)
	}

	if err := agent.Revoke(ctx, s.DB, ws, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec := agentRequest(h, "GET", "/api/v1/me", token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d", rec.Code)
	}
}

func TestMCPEndpointNeedsATokenAndChecksOrigin(t *testing.T) {
	s, h := server(t)
	s.AppURL = "https://app.example.com"
	h = s.Routes()
	ctx := context.Background()
	var ws string
	if err := s.DB.QueryRow(ctx, `SELECT workspace_id FROM inbound_addresses LIMIT 1`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	_, token, err := agent.Create(ctx, s.DB, ws, "Claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if rec := agentRequest(h, "POST", "/mcp", "", ping); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token = %d", rec.Code)
	}
	if rec := agentRequest(h, "POST", "/mcp", token, ping); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result":{}`) {
		t.Fatalf("ping = %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(ping))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin = %d", rec.Code)
	}
	if rec := agentRequest(h, "GET", "/mcp", token, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
}
