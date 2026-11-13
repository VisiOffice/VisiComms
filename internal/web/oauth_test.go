package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/oauth"
	"github.com/VisiOffice/VisiComms/internal/testutil"
	"github.com/VisiOffice/VisiComms/internal/web"
)

const claudeCallback = "https://claude.ai/api/mcp/auth_callback"

func oauthServer(t *testing.T) (*web.Server, http.Handler) {
	t.Helper()
	pool := testutil.DB(t)
	s := &web.Server{
		DB: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: &auth.Service{DB: pool},
		AppURL: "https://app.example.com",
		OAuth:  &oauth.Server{DB: pool, Issuer: "https://api.example.com", AppURL: "https://app.example.com"},
	}
	return s, s.Routes()
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func form(h http.Handler, path string, values url.Values) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(h, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func mcpCall(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(h, req)
}

// mcpTool calls a tool and returns its text and whether it is an error.
func mcpTool(t *testing.T, h http.Handler, token, name, args string) (string, bool) {
	t.Helper()
	rec := mcpCall(h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Result.Content) != 1 {
		t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
	}
	return out.Result.Content[0].Text, out.Result.IsError
}

// authorize runs the browser half and returns the code.
func authorize(t *testing.T, h http.Handler, clientID, session string, workspaces []string, verifier string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {claudeCallback}, "state": {"st8"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"resource": {"https://api.example.com/mcp"}, "scope": {"mcp"}}
	rec := do(h, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
	consent := rec.Header().Get("Location")
	if rec.Code != 302 || !strings.HasPrefix(consent, "https://app.example.com/authorize/") {
		t.Fatalf("authorize = %d %s", rec.Code, consent)
	}
	request := strings.TrimPrefix(consent, "https://app.example.com/authorize/")
	body, _ := json.Marshal(map[string]any{"workspaces": workspaces})
	rec = consoleCall(h, "POST", "/api/oauth/requests/"+request+"/approve", session, string(body))
	var out struct{ Redirect string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	u, err := url.Parse(out.Redirect)
	if rec.Code != 200 || err != nil || !strings.HasPrefix(out.Redirect, claudeCallback+"?") || u.Query().Get("state") != "st8" ||
		u.Query().Get("iss") != "https://api.example.com" {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	return u.Query().Get("code")
}

func TestOAuthForMCP(t *testing.T) {
	s, h := oauthServer(t)
	ctx := context.Background()

	// Two workspaces for one login, and someone else's.
	var bins, pine, other, login string
	for _, w := range []struct {
		name string
		id   *string
	}{{"Acme Plumbing", &bins}, {"Birch Dental", &pine}, {"Someone Else", &other}} {
		if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ($1, 'America/Los_Angeles') RETURNING id`, w.name).Scan(w.id); err != nil {
			t.Fatal(err)
		}
	}
	session, morgan := consoleSession(t, s, bins, "Morgan Reyes", "morgan@example.com")
	if err := s.DB.QueryRow(ctx, `SELECT login_id FROM principals WHERE id = $1`, morgan).Scan(&login); err != nil || login == "" {
		if err := s.DB.QueryRow(ctx, `INSERT INTO logins DEFAULT VALUES RETURNING id`).Scan(&login); err != nil {
			t.Fatal(err)
		}
		_, _ = s.DB.Exec(ctx, `UPDATE principals SET login_id = $2 WHERE id = $1`, morgan, login)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO principals (workspace_id, kind, name, login_id) VALUES ($1, 'human', 'Morgan Reyes', $2)`, pine, login); err != nil {
		t.Fatal(err)
	}

	// Discovery: an unauthenticated MCP call says where to sign in.
	rec := mcpCall(h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `resource_metadata="https://api.example.com/.well-known/oauth-protected-resource"`) {
		t.Fatalf("unauthenticated = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rec = do(h, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource/mcp", nil))
	if !strings.Contains(rec.Body.String(), `"resource":"https://api.example.com/mcp"`) {
		t.Fatalf("resource metadata %s", rec.Body)
	}
	rec = do(h, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
	var meta map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta["token_endpoint"] != "https://api.example.com/oauth/token" || meta["registration_endpoint"] != "https://api.example.com/oauth/register" {
		t.Fatalf("metadata %s", rec.Body)
	}

	// Registration: HTTPS or loopback redirects only.
	reg := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		r := do(h, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &out)
		return r, out
	}
	if r, _ := reg(`{"client_name":"Evil","redirect_uris":["http://evil.example/cb"]}`); r.Code != 400 {
		t.Fatalf("http redirect registered: %d", r.Code)
	}
	r, client := reg(`{"client_name":"Claude","redirect_uris":["` + claudeCallback + `"],"token_endpoint_auth_method":"none"}`)
	clientID, _ := client["client_id"].(string)
	if r.Code != 201 || !strings.HasPrefix(clientID, "vcoc_") || client["client_secret"] != nil {
		t.Fatalf("register = %d %s", r.Code, r.Body)
	}

	// A redirect the client did not register is never redirected to.
	rec = do(h, httptest.NewRequest("GET", "/oauth/authorize?response_type=code&client_id="+clientID+"&redirect_uri=https://evil.example/cb", nil))
	if rec.Code != 400 || rec.Header().Get("Location") != "" {
		t.Fatalf("bad redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Without PKCE the client is told why.
	rec = do(h, httptest.NewRequest("GET", "/oauth/authorize?response_type=code&state=s&client_id="+clientID+"&redirect_uri="+url.QueryEscape(claudeCallback), nil))
	if loc := rec.Header().Get("Location"); rec.Code != 302 || !strings.Contains(loc, "error=invalid_request") || !strings.Contains(loc, "state=s") {
		t.Fatalf("no PKCE = %d %q", rec.Code, loc)
	}

	// The consent page lists only the person's workspaces; an id that is
	// not theirs is ignored.
	sum := sha256.Sum256([]byte("v"))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {claudeCallback},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	loc := do(h, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil)).Header().Get("Location")
	pending := consoleCall(h, "GET", "/api/oauth/requests/"+strings.TrimPrefix(loc, "https://app.example.com/authorize/"), session, "")
	if !strings.Contains(pending.Body.String(), "Birch Dental") || !strings.Contains(pending.Body.String(), "Acme Plumbing") ||
		strings.Contains(pending.Body.String(), "Someone Else") || !strings.Contains(pending.Body.String(), `"redirect_host":"claude.ai"`) {
		t.Fatalf("pending %s", pending.Body)
	}
	if rec := consoleCall(h, "POST", "/api/oauth/requests/"+strings.TrimPrefix(loc, "https://app.example.com/authorize/")+"/approve", session, `{"workspaces":["`+other+`"]}`); rec.Code != 400 {
		t.Fatalf("approving only someone else's workspace = %d", rec.Code)
	}

	verifier := strings.Repeat("abcdefgh", 6)
	code := authorize(t, h, clientID, session, []string{bins, pine, other}, verifier)
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {claudeCallback}}
	exchange.Set("code_verifier", strings.Repeat("x", 48))
	if rec, out := form(h, "/oauth/token", exchange); rec.Code != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier = %d %v", rec.Code, out)
	}
	exchange.Set("code_verifier", verifier)
	rec, tokens := form(h, "/oauth/token", exchange)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if rec.Code != 200 || !strings.HasPrefix(access, "vcoa_") || !strings.HasPrefix(refresh, "vcor_") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d %s", rec.Code, rec.Body)
	}
	if n := count(t, s, `SELECT count(*) FROM principals WHERE kind = 'agent' AND oauth_grant_id IS NOT NULL AND autonomy = 'approve' AND name = 'Claude (Morgan)'`); n != 2 {
		t.Fatalf("%d agents, want one per allowed workspace", n)
	}

	// Two workspaces: every tool needs one, and says which exist.
	text, _ := mcpTool(t, h, access, "workspaces_list", `{}`)
	if !strings.Contains(text, "Acme Plumbing") || !strings.Contains(text, "Birch Dental") || strings.Contains(text, "Someone Else") ||
		!strings.Contains(text, `"workspace_required":true`) {
		t.Fatalf("workspaces_list %s", text)
	}
	if text, isErr := mcpTool(t, h, access, "threads_list", `{}`); !isErr || !strings.Contains(text, "workspace is required") {
		t.Fatalf("no workspace: %v %s", isErr, text)
	}
	if text, isErr := mcpTool(t, h, access, "threads_list", `{"workspace":"`+other+`"}`); !isErr || !strings.Contains(text, "not one you have access to") {
		t.Fatalf("someone else's workspace: %v %s", isErr, text)
	}
	if text, isErr := mcpTool(t, h, access, "whoami", `{"workspace":"birch dental"}`); isErr || !strings.Contains(text, `"workspace":"Birch Dental"`) {
		t.Fatalf("by name: %v %s", isErr, text)
	}
	if text, isErr := mcpTool(t, h, access, "threads_list", `{"workspace":"`+bins+`","waiting":true}`); isErr || !strings.Contains(text, `"threads":[]`) {
		t.Fatalf("by id: %v %s", isErr, text)
	}
	list := mcpCall(h, access, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).Body.String()
	if !strings.Contains(list, `"required":["workspace"`) {
		t.Fatalf("tools/list does not require workspace: %.300s", list)
	}

	// Refresh rotates: the old refresh token stops working.
	rec, again := form(h, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if rec.Code != 200 || again["access_token"] == access {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := form(h, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); rec.Code != 400 {
		t.Fatalf("old refresh token reused = %d", rec.Code)
	}
	access = again["access_token"].(string)

	// Revoking the agent in one workspace leaves the other; with one left,
	// no workspace argument is needed.
	var pineAgent string
	_ = s.DB.QueryRow(ctx, `SELECT id FROM principals WHERE workspace_id = $1 AND oauth_grant_id IS NOT NULL`, pine).Scan(&pineAgent)
	_, _ = s.DB.Exec(ctx, `UPDATE principals SET revoked_at = now() WHERE id = $1`, pineAgent)
	if text, isErr := mcpTool(t, h, access, "whoami", `{}`); isErr || !strings.Contains(text, "Acme Plumbing") {
		t.Fatalf("one workspace left: %v %s", isErr, text)
	}

	// A code used twice ends the whole grant.
	if rec, _ := form(h, "/oauth/token", exchange); rec.Code != 400 {
		t.Fatalf("code reused = %d", rec.Code)
	}
	if rec := mcpCall(h, access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); rec.Code != 401 {
		t.Fatalf("after code reuse = %d", rec.Code)
	}

	// An agent token still works as before, over the same endpoint.
	_, agentToken, err := agent.Create(ctx, s.DB, bins, "Desk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := mcpTool(t, h, agentToken, "whoami", `{}`); isErr || !strings.Contains(text, `"name":"Desk"`) {
		t.Fatalf("agent token: %s", text)
	}

	// A new approval for one workspace.
	code = authorize(t, h, clientID, session, []string{bins}, verifier)
	exchange.Set("code", code)
	_, tokens = form(h, "/oauth/token", exchange)
	access = tokens["access_token"].(string)
	if text, isErr := mcpTool(t, h, access, "workspaces_list", `{}`); isErr || !strings.Contains(text, `"workspace_required":false`) {
		t.Fatalf("single workspace: %s", text)
	}
	// Revocation by the client.
	if rec, _ := form(h, "/oauth/revoke", url.Values{"token": {tokens["refresh_token"].(string)}, "client_id": {clientID}}); rec.Code != 200 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := mcpCall(h, access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); rec.Code != 401 {
		t.Fatalf("after revoke = %d", rec.Code)
	}
}
