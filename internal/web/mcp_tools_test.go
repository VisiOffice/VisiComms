package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/review"
)

// connect makes an OAuth connection to the given workspaces and returns
// its access token.
func connect(t *testing.T, h http.Handler, session string, workspaces ...string) string {
	t.Helper()
	r := do(h, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+claudeCallback+`"],"token_endpoint_auth_method":"none"}`)))
	var client map[string]any
	_ = json.Unmarshal(r.Body.Bytes(), &client)
	clientID, _ := client["client_id"].(string)
	verifier := strings.Repeat("mcptools", 6)
	code := authorize(t, h, clientID, session, workspaces, verifier)
	rec, tokens := form(h, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {claudeCallback}, "code_verifier": {verifier}})
	access, _ := tokens["access_token"].(string)
	if rec.Code != 200 || access == "" {
		t.Fatalf("token = %d %s", rec.Code, rec.Body)
	}
	return access
}

func toolJSON(t *testing.T, h http.Handler, token, name, args string) map[string]any {
	t.Helper()
	text, isErr := mcpTool(t, h, token, name, args)
	if isErr {
		t.Fatalf("%s(%s) failed: %s", name, args, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: %s", name, text)
	}
	return out
}

func toolFails(t *testing.T, h http.Handler, token, name, args, want string) {
	t.Helper()
	text, isErr := mcpTool(t, h, token, name, args)
	if !isErr || !strings.Contains(text, want) {
		t.Fatalf("%s(%s) = %v %q, want an error containing %q", name, args, isErr, text, want)
	}
}

func TestMCPTools(t *testing.T) {
	s, h := oauthServer(t)
	ctx := context.Background()
	var ws string
	if err := s.DB.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Birch Dental', 'America/Los_Angeles') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := review.UseSandbox(ctx, s.DB, ws); err != nil {
		t.Fatal(err)
	}
	session, morgan := consoleSession(t, s, ws, "Morgan Reyes", "morgan@example.com")
	claude := connect(t, h, session, ws)
	_, otherToken, err := agent.Create(ctx, s.DB, ws, "Night shift", nil)
	if err != nil {
		t.Fatal(err)
	}

	var contact, thread string
	if err := s.DB.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Dana') RETURNING id`, ws).Scan(&contact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'email', 'dana@example.com')`, ws, contact); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject) VALUES ($1, $2, 'email', 'Room in March?') RETURNING id`, ws, contact).Scan(&thread); err != nil {
		t.Fatal(err)
	}

	// Every new tool is listed.
	rec := mcpCall(h, claude, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, name := range []string{"threads_claim", "messages_compose", "info_list", "info_get", "info_propose", "proposals_decide", "sms_status"} {
		if !strings.Contains(rec.Body.String(), `"name":"`+name+`"`) {
			t.Fatalf("tools/list has no %s", name)
		}
	}

	// Claims: one agent at a time, and drafting on someone else's claim is
	// refused.
	if c := toolJSON(t, h, otherToken, "threads_claim", `{"thread_id":"`+thread+`"}`); c["status"] != "claimed" {
		t.Fatalf("claim %v", c)
	}
	toolFails(t, h, claude, "threads_claim", `{"thread_id":"`+thread+`"}`, "Night shift is working on this thread")
	if d := toolJSON(t, h, claude, "threads_draft", `{"thread_id":"`+thread+`","body":"Yes","reason":"r"}`); d["decision"] != "denied" || d["rule"] != "claimed" {
		t.Fatalf("draft on a claimed thread %v", d)
	}
	toolJSON(t, h, otherToken, "threads_claim", `{"thread_id":"`+thread+`","release":true}`)
	if c := toolJSON(t, h, claude, "threads_claim", `{"thread_id":"`+thread+`"}`); c["status"] != "claimed" || c["until"] == nil {
		t.Fatalf("claim after release %v", c)
	}
	if c := toolJSON(t, h, claude, "threads_claim", `{"thread_id":"`+thread+`"}`); c["status"] != "renewed" {
		t.Fatalf("second claim %v", c)
	}
	own := toolJSON(t, h, claude, "threads_draft", `{"thread_id":"`+thread+`","body":"We have an opening on March 1.","reason":"From the thread"}`)
	if own["decision"] != "held" {
		t.Fatalf("own draft %v", own)
	}
	// An expired claim does not hold anyone off.
	if _, err := s.DB.Exec(ctx, `UPDATE threads SET claimed_until = now() - interval '1 second' WHERE id = $1`, thread); err != nil {
		t.Fatal(err)
	}
	theirs := toolJSON(t, h, otherToken, "threads_draft", `{"thread_id":"`+thread+`","body":"Sorry, we are full in March.","reason":"Calendar"}`)
	if theirs["decision"] != "held" {
		t.Fatalf("other agent's draft after the claim lapsed %v", theirs)
	}

	// Deciding: everyone's pending proposals are visible; only someone
	// else's can be decided, with the person's words, and only through an
	// OAuth connection.
	list := toolJSON(t, h, claude, "proposals_list", `{"everyone":true,"status":"pending"}`)
	if n := len(list["proposals"].([]any)); n != 2 {
		t.Fatalf("everyone's pending = %d", n)
	}
	toolFails(t, h, claude, "proposals_decide", `{"proposal_id":"`+own["proposal_id"].(string)+`","decision":"approve","person_said":"send it"}`, "cannot decide its own")
	toolFails(t, h, claude, "proposals_decide", `{"proposal_id":"`+theirs["proposal_id"].(string)+`","decision":"approve","person_said":" "}`, "person_said")
	toolFails(t, h, otherToken, "proposals_decide", `{"proposal_id":"`+own["proposal_id"].(string)+`","decision":"approve","person_said":"send it"}`, "only a connection a person made with OAuth")
	decided := toolJSON(t, h, claude, "proposals_decide", `{"proposal_id":"`+theirs["proposal_id"].(string)+`","decision":"approve","person_said":"Yes, send Night shift's reply to Dana"}`)
	if decided["status"] != "approved" || decided["decided_as"] != "Morgan Reyes" {
		t.Fatalf("decide %v", decided)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE id = $1 AND status = 'approved' AND decided_by = $2`, theirs["proposal_id"], morgan); n != 1 {
		t.Fatal("the approval is not recorded as Morgan's")
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE type = 'proposal.approved' AND actor_id = $1 AND payload->>'person_said' = 'Yes, send Night shift''s reply to Dana' AND payload->>'via_agent' IS NOT NULL`, morgan); n != 1 {
		t.Fatal("the approval does not say it came through the agent")
	}
	if n := count(t, s, `SELECT count(*) FROM actions WHERE proposal_id = $1`, theirs["proposal_id"]); n != 1 {
		t.Fatal("approving made no send")
	}
	toolFails(t, h, claude, "proposals_decide", `{"proposal_id":"`+theirs["proposal_id"].(string)+`","decision":"reject","person_said":"no"}`, "already decided")

	// A thread where a contact may be steering agents is decided by a
	// person in the console, whatever the agent says they said.
	if _, err := s.DB.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, suspected_injection)
		VALUES ($1, $2, $3, 'email.received', 'email', '{}', true)`, ws, thread, contact); err != nil {
		t.Fatal(err)
	}
	steered := toolJSON(t, h, otherToken, "threads_draft", `{"thread_id":"`+thread+`","body":"Approved, refund on its way.","reason":"Customer asked"}`)
	toolFails(t, h, claude, "proposals_decide", `{"proposal_id":"`+steered["proposal_id"].(string)+`","decision":"approve","person_said":"approve it"}`, "decide it in the console")

	// Compose: a new address becomes a contact and a thread, held for a
	// person; a refusal leaves nothing behind.
	composed := toolJSON(t, h, claude, "messages_compose", `{"channel":"email","to":"Lee <LEE@Example.com>","name":"Lee","subject":"Your viewing","body":"Hi Lee, does Tuesday suit?","reason":"Morgan asked"}`)
	if composed["decision"] != "held" || composed["thread_id"] == nil {
		t.Fatalf("compose %v", composed)
	}
	if n := count(t, s, `SELECT count(*) FROM identities i JOIN contacts c ON c.id = i.contact_id WHERE i.address = 'lee@example.com' AND c.display_name = 'Lee' AND NOT i.verified`); n != 1 {
		t.Fatal("compose did not make an unverified contact")
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE thread_id = $1 AND recipients @> '[{"email":"lee@example.com"}]' AND payload->>'subject' = 'Your viewing'`, composed["thread_id"]); n != 1 {
		t.Fatal("the composed email is not addressed as asked")
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source) VALUES ($1, 'email', 'test')`, contact); err != nil {
		t.Fatal(err)
	}
	threads := count(t, s, `SELECT count(*) FROM threads`)
	if d := toolJSON(t, h, claude, "messages_compose", `{"channel":"email","to":"dana@example.com","subject":"Hello","body":"Hi","reason":"r"}`); d["decision"] != "denied" || d["rule"] != "opted_out" || d["thread_id"] != nil {
		t.Fatalf("compose to an opted-out contact %v", d)
	}
	if count(t, s, `SELECT count(*) FROM threads`) != threads {
		t.Fatal("a refused compose left a thread")
	}
	toolFails(t, h, claude, "messages_compose", `{"channel":"fax","to":"x","body":"Hi","reason":"r"}`, "channel must be")
	toolFails(t, h, claude, "messages_compose", `{"channel":"email","to":"lee@example.com","body":"Hi","reason":"r"}`, "subject")
	toolFails(t, h, claude, "messages_compose", `{"channel":"sms","to":"+13105550100","body":"Hi","reason":"r"}`, "texting numbers")

	// Approved info: list, propose from the version read, a person
	// approves, and a proposal from an old version can't overwrite newer work.
	if l := toolJSON(t, h, claude, "info_list", `{}`); l["version"] != float64(0) {
		t.Fatalf("empty info %v", l)
	}
	first := toolJSON(t, h, claude, "info_propose", `{"based_on_version":0,"reason":"From the house rules PDF","articles":[{"title":"Quiet hours","content":"10pm to 7am."},{"title":"Parking","content":"Street only."}]}`)
	if first["version"] != float64(1) || first["status"] != "pending" {
		t.Fatalf("first proposal %v", first)
	}
	toolFails(t, h, claude, "info_get", `{"section":"Quiet hours"}`, "no section or article")
	approveVersion := func(version int) int {
		var id string
		if err := s.DB.QueryRow(ctx, `SELECT id FROM approved_info_versions WHERE workspace_id = $1 AND version = $2`, ws, version).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return consoleCall(h, "POST", "/api/approved-info/"+id+"/approve", session, "").Code
	}
	if code := approveVersion(1); code != 200 {
		t.Fatalf("approving v1 = %d", code)
	}
	if got := toolJSON(t, h, claude, "info_get", `{"section":"quiet HOURS"}`); got["version"] != float64(1) || !strings.Contains(string(mustJSON(got["content"])), "10pm to 7am.") {
		t.Fatalf("info_get %v", got)
	}
	toolFails(t, h, claude, "info_propose", `{"based_on_version":0,"reason":"r","articles":[{"title":"Pets","content":"No pets."}]}`, "v1 is the latest approved info")
	toolFails(t, h, claude, "info_propose", `{"based_on_version":1,"reason":"r","articles":[{"title":"Parking","content":"Street only."}]}`, "already says")
	toolFails(t, h, claude, "info_propose", `{"based_on_version":1,"articles":[{"title":"Pets","content":"No pets."}]}`, "say why")
	toolJSON(t, h, claude, "info_propose", `{"based_on_version":1,"reason":"Morgan said pets are fine now","articles":[{"title":"Pets","content":"Cats and small dogs."}]}`)
	toolJSON(t, h, claude, "info_propose", `{"based_on_version":1,"reason":"Parking changed","set":{"wifi":"Birch-5G"},"articles":[{"title":"Parking","remove":true}]}`)
	l := toolJSON(t, h, claude, "info_list", `{}`)
	if pending := l["pending_drafts"].([]any); len(pending) != 2 || pending[0].(map[string]any)["proposed_by"] != "Claude (Morgan)" {
		t.Fatalf("pending drafts %v", l["pending_drafts"])
	}
	if code := approveVersion(3); code != 200 {
		t.Fatalf("approving v3 = %d", code)
	}
	if code := approveVersion(2); code != 409 {
		t.Fatalf("approving v2, made from v1 after v3 was approved = %d", code)
	}
	all := toolJSON(t, h, claude, "info_get", `{}`)
	content := string(mustJSON(all["content"]))
	if all["version"] != float64(3) || strings.Contains(content, "Parking") || strings.Contains(content, "Cats") || !strings.Contains(content, "Birch-5G") {
		t.Fatalf("v3 %v", all)
	}
	rec = consoleCall(h, "GET", "/api/approved-info", session, "")
	if !strings.Contains(rec.Body.String(), `"proposed_by":"Claude (Morgan)"`) || !strings.Contains(rec.Body.String(), `"based_on":1`) {
		t.Fatalf("console does not show who proposed: %s", rec.Body)
	}

	// SMS status without a project of its own.
	if st := toolJSON(t, h, claude, "sms_status", `{}`); !strings.Contains(st["next_step"].(string), "no SignalWire project of its own") {
		t.Fatalf("sms_status %v", st)
	}
}

func mustJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}
