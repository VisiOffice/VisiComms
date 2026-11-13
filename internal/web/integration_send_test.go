package web_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/review"
)

func TestIntegrationSend(t *testing.T) {
	s, h := oauthServer(t)
	ctx := context.Background()
	var ws string
	if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Cedar Court', 'America/Los_Angeles') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := review.UseSandbox(ctx, s.DB, ws); err != nil {
		t.Fatal(err)
	}
	session, morgan := consoleSession(t, s, ws, "Morgan Reyes", "morgan@example.com")
	a, token, err := agent.Create(ctx, s.DB, ws, "VisiRent", nil)
	if err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := s.DB.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	actions := func() int { return count(`SELECT count(*) FROM actions WHERE workspace_id = $1`, ws) }
	post := func(body string) (int, map[string]any) {
		t.Helper()
		rec := agentRequest(h, "POST", "/api/v1/messages", token, body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	const reminder = `{"channel":"email","to":"Tenant@Example.com","name":"Tia","subject":"Rent due","body":"Rent is due on the 1st.","send":true,"idempotency_key":"rent-2026-12-lease-1"}`

	// Not granted: refused, and nothing is left behind.
	if code, out := post(reminder); code != 403 || !strings.Contains(out["error"].(string), "Connections") {
		t.Fatalf("no permission = %d %v", code, out)
	}
	if n := count(`SELECT count(*) FROM threads WHERE workspace_id = $1`, ws); n != 0 {
		t.Fatalf("refused send left %d threads", n)
	}

	// Without send it is held, as over MCP.
	code, out := post(`{"channel":"email","to":"held@example.com","subject":"Hello","body":"Hi"}`)
	if code != 200 || out["decision"] != "held" || out["proposal_id"] == nil || actions() != 0 {
		t.Fatalf("held = %d %v, %d actions", code, out, actions())
	}

	// A token cannot grant itself the permission.
	if rec := agentRequest(h, "POST", "/api/agents/"+a.ID+"/sending", token, `{"allowed":true}`); rec.Code != 401 {
		t.Fatalf("token granting itself = %d %s", rec.Code, rec.Body)
	}
	if rec := consoleCall(h, "POST", "/api/agents/"+a.ID+"/sending", session, `{"allowed":true}`); rec.Code != 200 {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	if n := count(`SELECT count(*) FROM principals WHERE id = $1 AND may_send AND may_send_by = $2`, a.ID, morgan); n != 1 {
		t.Fatal("the grant did not record who made it")
	}

	if code, out := post(`{"channel":"email","to":"a@example.com","subject":"Hi","body":"Hi","send":true}`); code != 400 {
		t.Fatalf("no key = %d %v", code, out)
	}

	code, out = post(reminder)
	if code != 200 || out["decision"] != "allowed" || out["state"] != "delivered" || out["message_id"] == nil || actions() != 1 {
		t.Fatalf("send = %d %v, %d actions", code, out, actions())
	}
	id, thread := out["message_id"].(string), out["thread_id"].(string)

	// A retry sends nothing and reports the first send.
	code, again := post(reminder)
	if code != 200 || again["replayed"] != true || again["message_id"] != id || again["thread_id"] != thread || actions() != 1 {
		t.Fatalf("retry = %d %v, %d actions", code, again, actions())
	}
	if n := count(`SELECT count(*) FROM threads WHERE workspace_id = $1`, ws); n != 2 {
		t.Fatalf("retry made a thread: %d", n)
	}
	different := strings.Replace(reminder, "Rent is due", "Rent is late", 1)
	if code, out := post(different); code != 409 {
		t.Fatalf("reused key = %d %v", code, out)
	}

	rec := agentRequest(h, "GET", "/api/v1/messages/"+id, token, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"delivered"`) {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	_, otherToken, err := agent.Create(ctx, s.DB, ws, "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := agentRequest(h, "GET", "/api/v1/messages/"+id, otherToken, ""); rec.Code != 404 {
		t.Fatalf("another agent's message = %d", rec.Code)
	}

	// The same request arriving several times at once still sends once.
	var wg sync.WaitGroup
	before := actions()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentRequest(h, "POST", "/api/v1/messages", token, `{"channel":"email","to":"race@example.com","subject":"Hi","body":"Hi","send":true,"idempotency_key":"race"}`)
		}()
	}
	wg.Wait()
	if actions() != before+1 {
		t.Fatalf("concurrent retries made %d sends", actions()-before)
	}

	// An opt-out still refuses, with nothing sent.
	var contact string
	if err := s.DB.QueryRow(ctx, `SELECT contact_id FROM threads WHERE id = $1`, thread).Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source) VALUES ($1, 'email', 'test')`, contact); err != nil {
		t.Fatal(err)
	}
	optedOut := strings.Replace(reminder, "rent-2026-12-lease-1", "rent-2027-01-lease-1", 1)
	if code, out := post(optedOut); code != 200 || out["decision"] != "denied" || out["rule"] != "opted_out" || actions() != before+1 {
		t.Fatalf("opted out = %d %v, %d actions", code, out, actions())
	}

	// The hourly cap stops a runaway job.
	if _, err := s.DB.Exec(ctx, `INSERT INTO actions (workspace_id, thread_id, requested_by, action, payload, payload_sha256, idempotency_key)
		SELECT $1, $2, $3, 'email.send', '{}', '\x00', 'cap:' || g FROM generate_series(1, $4::int) g`, ws, thread, a.ID, agent.MaxSendsPerHour); err != nil {
		t.Fatal(err)
	}
	capped := `{"channel":"email","to":"new@example.com","subject":"Hi","body":"Hi","send":true,"idempotency_key":"over"}`
	if code, out := post(capped); code != 200 || out["decision"] != "denied" || out["rule"] != "send_rate" {
		t.Fatalf("over the cap = %d %v", code, out)
	}

	// Over MCP the same token still only drafts; send is not an option there.
	before = actions()
	composed := toolJSON(t, h, token, "messages_compose", `{"channel":"email","to":"mcp@example.com","subject":"Hi","body":"Hi","send":true,"idempotency_key":"mcp"}`)
	if composed["decision"] != "held" || actions() != before {
		t.Fatalf("MCP compose = %v, %d actions", composed, actions())
	}

	// Withdrawing it takes effect on the next request.
	if rec := consoleCall(h, "POST", "/api/agents/"+a.ID+"/sending", session, `{"allowed":false}`); rec.Code != 200 {
		t.Fatalf("withdraw = %d %s", rec.Code, rec.Body)
	}
	if code, _ := post(strings.Replace(reminder, "rent-2026-12-lease-1", "again", 1)); code != 403 {
		t.Fatalf("after withdrawing = %d", code)
	}

	// A connection made through claude.ai can never be given this.
	claude := connect(t, h, session, ws)
	_ = claude
	var oauthAgent string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM principals WHERE workspace_id = $1 AND oauth_grant_id IS NOT NULL`, ws).Scan(&oauthAgent); err != nil {
		t.Fatal(err)
	}
	if rec := consoleCall(h, "POST", "/api/agents/"+oauthAgent+"/sending", session, `{"allowed":true}`); rec.Code != 404 {
		t.Fatalf("granting an OAuth connection = %d", rec.Code)
	}
}
