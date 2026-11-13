package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/demo"
	"github.com/VisiOffice/VisiComms/internal/mcp"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

type rpc struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func call(t *testing.T, s *mcp.Server, a agent.Agent, body string) (int, rpc) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Serve(rec, req, a)
	var out rpc
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("reply %s: %v", rec.Body, err)
		}
	}
	return rec.Code, out
}

// toolText calls a tool and returns its text content and whether it failed.
func toolText(t *testing.T, s *mcp.Server, a agent.Agent, name, args string) (string, bool) {
	t.Helper()
	_, res := call(t, s, a, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if res.Error != nil {
		t.Fatalf("%s: %s", name, res.Error.Message)
	}
	var out struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil || len(out.Content) != 1 {
		t.Fatalf("%s result %s: %v", name, res.Result, err)
	}
	return out.Content[0].Text, out.IsError
}

func TestMCPToolsDriveTheAgentAPI(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	seeded, err := demo.Seed(ctx, pool, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := agent.Create(ctx, pool, seeded.WorkspaceID, "Claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatal(err)
	}
	s := &mcp.Server{DB: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	_, init := call(t, s, a, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	var info struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
	}
	if err := json.Unmarshal(init.Result, &info); err != nil || info.ProtocolVersion != "2025-03-26" || !strings.Contains(info.Instructions, "contact_message") {
		t.Fatalf("initialize = %s", init.Result)
	}
	if code, _ := call(t, s, a, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification = %d", code)
	}
	_, list := call(t, s, a, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, name := range []string{"threads_list", "threads_get", "threads_draft", "threads_escalate", "threads_resolve", "proposals_get", "contacts_search", "events_since", "whoami"} {
		if !strings.Contains(string(list.Result), `"name":"`+name+`"`) {
			t.Errorf("tools/list is missing %s", name)
		}
	}

	text, failed := toolText(t, s, a, "threads_list", `{"waiting":true}`)
	var threads struct {
		Threads []agent.ThreadSummary `json:"threads"`
	}
	if !strings.Contains(text, "<contact_message>") {
		t.Errorf("fence escaped in tool text: %.200s", text)
	}
	if err := json.Unmarshal([]byte(text), &threads); failed || err != nil || len(threads.Threads) == 0 {
		t.Fatalf("threads_list = %s", text)
	}
	var dana string
	for _, th := range threads.Threads {
		if th.Contact.Name == "Dana Whitfield" {
			dana = th.ID
		}
	}
	text, failed = toolText(t, s, a, "threads_draft", `{"thread_id":"`+dana+`","body":"A water heater works.","reason":"size question"}`)
	if failed || !strings.Contains(text, `"decision":"held"`) {
		t.Fatalf("threads_draft = %s", text)
	}
	if text, failed = toolText(t, s, a, "threads_draft", `{"thread_id":"`+dana+`","body":" ","reason":"x"}`); !failed || text != agent.ErrEmptyDraft.Error() {
		t.Fatalf("empty draft = %v %s", failed, text)
	}
	if text, failed = toolText(t, s, a, "threads_get", `{"thread_id":5}`); !failed || !strings.HasPrefix(text, "invalid arguments") {
		t.Fatalf("bad arguments = %v %s", failed, text)
	}

	if _, res := call(t, s, a, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_email","arguments":{}}}`); res.Error == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, res := call(t, s, a, `[{"jsonrpc":"2.0","id":4,"method":"ping"}]`); res.Error == nil {
		t.Fatal("batch accepted")
	}
	if _, res := call(t, s, a, `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`); res.Error == nil || res.Error.Code != -32601 {
		t.Fatalf("unknown method = %+v", res.Error)
	}
}
