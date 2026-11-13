package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/web"
)

const site = "https://www.acmeplumbing.example"

func chatServer(t *testing.T) (*web.Server, http.Handler, chat.Widget) {
	t.Helper()
	s, h := server(t)
	var ws string
	if err := s.DB.QueryRow(context.Background(), `SELECT workspace_id FROM inbound_addresses LIMIT 1`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	w, err := chat.Create(context.Background(), s.DB, ws, "Acme Plumbing")
	if err != nil {
		t.Fatal(err)
	}
	return s, h, w
}

func chatCall(h http.Handler, method, path, token, origin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.RemoteAddr = "203.0.113.7:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type sent struct {
	Token   string       `json:"token"`
	Message chat.Message `json:"message"`
}

func chatSay(t *testing.T, h http.Handler, w chat.Widget, token, body string) sent {
	t.Helper()
	rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", token, site, body)
	if rec.Code != 200 {
		t.Fatalf("send=%d %s", rec.Code, rec.Body)
	}
	var out sent
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, s *web.Server, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChatConfigStoresNothing(t *testing.T) {
	s, h, w := chatServer(t)
	rec := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/config", "", site, "")
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != site {
		t.Fatalf("config=%d cors=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	var c chat.Config
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if c.Name != "Acme Plumbing" || c.Greeting == "" || !c.Staffed {
		t.Fatalf("config %+v", c)
	}
	if rec := chatCall(h, "OPTIONS", "/chat/w/"+w.PublicID+"/messages", "", site, ""); rec.Code != 204 ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight=%d %v", rec.Code, rec.Header())
	}
	if rec := chatCall(h, "GET", "/chat/w/w_000000000000000000000000/config", "", site, ""); rec.Code != 404 {
		t.Fatalf("unknown widget=%d", rec.Code)
	}
	if n := count(t, s, `SELECT (SELECT count(*) FROM contacts) + (SELECT count(*) FROM threads) + (SELECT count(*) FROM chat_visitors)`); n != 0 {
		t.Fatalf("opening the widget stored %d rows", n)
	}
}

func TestChatAllowedSites(t *testing.T) {
	s, h, w := chatServer(t)
	settings := chat.Settings{Name: w.Name, Color: w.Color, Position: w.Position, Greeting: w.Greeting, ReturningGreeting: w.ReturningGreeting,
		ReplyTime: w.ReplyTime, OfflineMessage: w.OfflineMessage, AskName: "off", AskEmail: "off", AskPhone: "off",
		AllowedOrigins: []string{"acmeplumbing.example", "https://www.acmeplumbing.example/contact"}}
	w, err := chat.Update(context.Background(), s.DB, w.WorkspaceID, w.ID, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.AllowedOrigins) != 2 || w.AllowedOrigins[0] != "https://acmeplumbing.example" || w.AllowedOrigins[1] != site {
		t.Fatalf("origins %v", w.AllowedOrigins)
	}
	if rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", "", "https://evil.example", `{"text":"hi"}`); rec.Code != 403 {
		t.Fatalf("other site=%d", rec.Code)
	}
	if rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", "", "", `{"text":"hi"}`); rec.Code != 403 {
		t.Fatalf("no origin=%d", rec.Code)
	}
	chatSay(t, h, w, "", `{"text":"hi"}`)
}

func TestChatFirstMessageCreatesTheConversation(t *testing.T) {
	s, h, w := chatServer(t)
	first := chatSay(t, h, w, "", `{"text":"Do you have a water heater Friday?","page":"https://www.acmeplumbing.example/repairs",
		"attribution":{"gclid":"Cj0KCQ","landing_page":"https://www.acmeplumbing.example/?gclid=Cj0KCQ","referrer":"https://www.google.com/"}}`)
	if len(first.Token) < 40 || first.Message.From != "visitor" {
		t.Fatalf("first %+v", first)
	}
	var thread, channel, subject string
	var attribution []byte
	var untrusted bool
	if err := s.DB.QueryRow(context.Background(), `SELECT t.id, t.channel, t.subject, e.attribution, e.untrusted FROM threads t
		JOIN events e ON e.thread_id = t.id AND e.type = 'chat.received'`).Scan(&thread, &channel, &subject, &attribution, &untrusted); err != nil {
		t.Fatal(err)
	}
	if channel != "chat" || subject != "Chat on acmeplumbing.example" || !untrusted {
		t.Fatalf("thread %s %q untrusted=%v", channel, subject, untrusted)
	}
	var attr map[string]map[string]string
	_ = json.Unmarshal(attribution, &attr)
	if attr["source"]["value"] != "google_ads" || attr["source"]["how"] != "click_id" || attr["medium"]["value"] != "cpc" || attr["gclid"]["value"] != "Cj0KCQ" {
		t.Fatalf("attribution %s", attribution)
	}

	// The same visitor keeps their thread; the token is not issued again.
	again := chatSay(t, h, w, first.Token, `{"text":"Also a water heater?"}`)
	if again.Token != "" {
		t.Fatal("token issued twice")
	}
	if n := count(t, s, `SELECT count(*) FROM threads`); n != 1 {
		t.Fatalf("%d threads", n)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE thread_id = $1 AND type = 'chat.received' AND attribution IS NULL`, thread); n != 1 {
		t.Fatal("attribution belongs on the first message only")
	}

	// It is on the needs-attention list until someone answers.
	items, err := review.Attention(context.Background(), s.DB, w.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ThreadID != thread || items[0].Kind != review.Unanswered {
		t.Fatalf("attention %+v", items)
	}
}

// The token is the visitor's only key: nothing but the right token, on the
// right widget, reaches a conversation.
func TestChatTokenIsTheOnlyKey(t *testing.T) {
	s, h, w := chatServer(t)
	a := chatSay(t, h, w, "", `{"text":"I am A"}`)
	b := chatSay(t, h, w, "", `{"text":"I am B"}`)
	other, err := chat.Create(context.Background(), s.DB, w.WorkspaceID, "Other site")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ widget, token string }{
		"no token":         {w.PublicID, ""},
		"made-up token":    {w.PublicID, strings.Repeat("x", 43)},
		"another widget's": {other.PublicID, a.Token},
	} {
		if rec := chatCall(h, "GET", "/chat/w/"+tc.widget+"/messages", tc.token, site, ""); rec.Code != 401 {
			t.Errorf("%s: history=%d", name, rec.Code)
		}
		if tc.token != "" {
			if rec := chatCall(h, "POST", "/chat/w/"+tc.widget+"/messages", tc.token, site, `{"text":"x"}`); rec.Code != 401 {
				t.Errorf("%s: send=%d", name, rec.Code)
			}
		}
	}
	rec := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", b.Token, site, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "I am A") || !strings.Contains(rec.Body.String(), "I am B") {
		t.Fatalf("B's history=%d %s", rec.Code, rec.Body)
	}
}

func TestChatPreChatForm(t *testing.T) {
	s, h, w := chatServer(t)
	settings := chat.Settings{Name: w.Name, Color: w.Color, Position: w.Position, Greeting: w.Greeting, ReturningGreeting: w.ReturningGreeting,
		ReplyTime: w.ReplyTime, OfflineMessage: w.OfflineMessage, AskName: "required", AskEmail: "optional", AskPhone: "optional"}
	w, err := chat.Update(context.Background(), s.DB, w.WorkspaceID, w.ID, settings)
	if err != nil {
		t.Fatal(err)
	}
	rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", "", site, `{"text":"hi"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"field":"name"`) {
		t.Fatalf("missing name=%d %s", rec.Code, rec.Body)
	}
	rec = chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", "", site, `{"text":"hi","name":"Dana","phone":"12"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"field":"phone"`) {
		t.Fatalf("bad phone=%d %s", rec.Code, rec.Body)
	}
	chatSay(t, h, w, "", `{"text":"hi","name":"Dana Ruiz","email":"Dana@Example.com","phone":"(415) 555-0100"}`)
	var name, email, phone string
	if err := s.DB.QueryRow(context.Background(), `SELECT c.display_name, v.email, v.phone FROM chat_visitors v JOIN contacts c ON c.id = v.contact_id`).
		Scan(&name, &email, &phone); err != nil {
		t.Fatal(err)
	}
	if name != "Dana Ruiz" || email != "dana@example.com" || phone != "+14155550100" {
		t.Fatalf("visitor %q %q %q", name, email, phone)
	}
	// Typed details are unverified: they never become the contact's identities.
	if n := count(t, s, `SELECT count(*) FROM identities`); n != 0 {
		t.Fatalf("%d identities from a typed form", n)
	}
}

func TestChatReplies(t *testing.T) {
	s, h, w := chatServer(t)
	ctx := context.Background()
	first := chatSay(t, h, w, "", `{"text":"Hello?"}`)
	var thread, person, agentID string
	if err := s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Morgan Reyes') RETURNING id`, w.WorkspaceID).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'agent', 'Desk') RETURNING id`, w.WorkspaceID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	r := review.Reviewer{ID: person, WorkspaceID: w.WorkspaceID}

	res, err := review.Reply(ctx, s.DB, r, thread, review.EmailPayload{Body: "Yes, we do. Which address?"})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "delivered" {
		t.Fatalf("person's reply %s", res.State)
	}

	d, err := agent.Propose(ctx, s.DB, agent.Agent{ID: agentID, WorkspaceID: w.WorkspaceID, Autonomy: "approve"}, thread,
		agent.Draft{Body: "We deliver Monday to Saturday."})
	if err != nil || d.Decision != "held" {
		t.Fatalf("draft %+v %v", d, err)
	}
	if err := review.Approve(ctx, s.DB, r, d.ProposalID, nil); err != nil {
		t.Fatal(err)
	}

	rec := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", first.Token, site, "")
	var got struct{ Messages []chat.Message }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Messages) != 3 {
		t.Fatalf("history %s", rec.Body)
	}
	if m := got.Messages[1]; m.From != "team" || m.Name != "Morgan" || m.Text != "Yes, we do. Which address?" {
		t.Fatalf("person's reply as the visitor sees it %+v", m)
	}
	if m := got.Messages[2]; m.Name != "Acme Plumbing" {
		t.Fatalf("an agent's reply goes out under the widget's name, got %+v", m)
	}
	var state string
	_ = s.DB.QueryRow(ctx, `SELECT state FROM threads WHERE id = $1`, thread).Scan(&state)
	if state != "waiting" {
		t.Fatalf("thread %s after a reply", state)
	}
	items, _ := review.Attention(ctx, s.DB, w.WorkspaceID)
	if len(items) != 0 {
		t.Fatalf("answered chat still needs attention: %+v", items)
	}
	// After a given id, only newer messages.
	rec = chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages?after="+itoa(got.Messages[1].ID), first.Token, site, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Messages) != 1 {
		t.Fatalf("after: %s", rec.Body)
	}
}

func TestChatStream(t *testing.T) {
	s, h, w := chatServer(t)
	first := chatSay(t, h, w, "", `{"text":"Anyone there?"}`)
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/chat/w/"+w.PublicID+"/stream?after="+itoa(first.Message.ID), nil)
	req.Header.Set("Authorization", "Bearer "+first.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream=%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var thread, person string
	_ = s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors`).Scan(&thread)
	_ = s.DB.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Sam') RETURNING id`, w.WorkspaceID).Scan(&person)
	if _, err := review.Reply(ctx, s.DB, review.Reviewer{ID: person, WorkspaceID: w.WorkspaceID}, thread, review.EmailPayload{Body: "Hi, I'm here."}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var m chat.Message
			_ = json.Unmarshal([]byte(line), &m)
			if m.Text != "Hi, I'm here." || m.Name != "Sam" {
				t.Fatalf("streamed %+v", m)
			}
			return
		}
	}
	t.Fatalf("stream ended without the reply: %v", scanner.Err())
}

func TestChatLimitsNewConversations(t *testing.T) {
	_, h, w := chatServer(t)
	for i := 0; i < 10; i++ {
		chatSay(t, h, w, "", `{"text":"spam"}`)
	}
	if rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/messages", "", site, `{"text":"spam"}`); rec.Code != 429 {
		t.Fatalf("11th new chat from one address=%d", rec.Code)
	}
}

func TestChatScript(t *testing.T) {
	_, h, _ := chatServer(t)
	rec := chatCall(h, "GET", "/chat/widget.js", "", "", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(rec.Body.String(), "data-widget") {
		t.Fatalf("script=%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func popupSettings(text string) chat.PopupSettings {
	return chat.PopupSettings{Name: "Services nudge", Text: text, Trigger: "delay", Seconds: 10, Active: true,
		QuickReplies: []string{"Book a visit", "Opening hours"}}
}

// A pop-up is held, not stored: showing it saves nothing about the visitor.
// When they reply, the pop-up's words (from the server, never the browser)
// open the thread at the time it was shown.
func TestChatPopups(t *testing.T) {
	s, h, w := chatServer(t)
	ctx := context.Background()
	p, err := chat.CreatePopup(ctx, s.DB, w.WorkspaceID, w.ID, popupSettings("Need a repair this week? Ask us."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.CreatePopup(ctx, s.DB, w.WorkspaceID, w.ID, chat.PopupSettings{Name: "x", Text: "y", Trigger: "page", PagePattern: "services", Active: true}); err == nil {
		t.Fatal("a page pop-up needs a path starting with /")
	}
	rec := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/config", "", site, "")
	var c chat.Config
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if len(c.Popups) != 1 || c.Popups[0].Sender != "Acme Plumbing" || c.Popups[0].Seconds != 10 || len(c.Popups[0].QuickReplies) != 2 {
		t.Fatalf("config popups %+v", c.Popups)
	}

	visit := strings.Repeat("a", 32)
	shown := "/chat/w/" + w.PublicID + "/popups/" + p.ID + "/shown?visit=" + visit
	if rec := chatCall(h, "POST", shown, "", site, ""); rec.Code != 204 {
		t.Fatalf("shown=%d %s", rec.Code, rec.Body)
	}
	if rec := chatCall(h, "POST", shown, "", site, ""); rec.Code != 429 {
		t.Fatalf("shown twice in one visit=%d", rec.Code)
	}
	if n := count(t, s, `SELECT (SELECT count(*) FROM contacts) + (SELECT count(*) FROM threads) + (SELECT count(*) FROM chat_visitors)`); n != 0 {
		t.Fatalf("showing a pop-up stored %d rows", n)
	}

	shownAt := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
	first := chatSay(t, h, w, "", `{"text":"Yes, a water heater","popup":{"id":"`+p.ID+`","shown_at":"`+shownAt+`","text":"FORGED"}}`)
	rec = chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", first.Token, site, "")
	var got struct{ Messages []chat.Message }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Messages) != 2 || got.Messages[0].Text != "Need a repair this week? Ask us." || got.Messages[0].From != "team" ||
		got.Messages[0].Name != "Acme Plumbing" || got.Messages[1].Text != "Yes, a water heater" {
		t.Fatalf("conversation %s", rec.Body)
	}
	if d := time.Since(got.Messages[0].At); d < 2*time.Minute || d > 4*time.Minute {
		t.Fatalf("pop-up kept its time? %v ago", d)
	}
	popups, err := chat.Popups(ctx, s.DB, w.WorkspaceID, w.ID)
	if err != nil || len(popups) != 1 || popups[0].Shown != 1 || popups[0].Chats != 1 {
		t.Fatalf("stats %+v %v", popups, err)
	}
	// Still waiting on a person: the pop-up is not an answer.
	items, _ := review.Attention(ctx, s.DB, w.WorkspaceID)
	if len(items) != 1 || items[0].Kind != review.Unanswered {
		t.Fatalf("attention %+v", items)
	}

	// Another widget's pop-up id is ignored, and the message still goes through.
	other, _ := chat.Create(ctx, s.DB, w.WorkspaceID, "Other")
	theirs, _ := chat.CreatePopup(ctx, s.DB, w.WorkspaceID, other.ID, popupSettings("Not yours"))
	second := chatSay(t, h, w, "", `{"text":"hi","popup":{"id":"`+theirs.ID+`"}}`)
	rec = chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", second.Token, site, "")
	if strings.Contains(rec.Body.String(), "Not yours") {
		t.Fatal("another widget's pop-up was written into this chat")
	}
	if err := chat.DeletePopup(ctx, s.DB, w.WorkspaceID, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestChatQuickRepliesAndLaterDetails(t *testing.T) {
	s, h, w := chatServer(t)
	ctx := context.Background()
	first := chatSay(t, h, w, "", `{"text":"Hello"}`)
	var thread, person string
	_ = s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors`).Scan(&thread)
	_ = s.DB.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Morgan') RETURNING id`, w.WorkspaceID).Scan(&person)
	r := review.Reviewer{ID: person, WorkspaceID: w.WorkspaceID}
	if _, err := review.Reply(ctx, s.DB, r, thread, review.EmailPayload{Body: "Delivery or pickup?", QuickReplies: []string{"Delivery", "Pickup", strings.Repeat("x", 41)}}); err == nil {
		t.Fatal("a 41-character button was accepted")
	}
	if _, err := review.Reply(ctx, s.DB, r, thread, review.EmailPayload{Body: "Delivery or pickup?", QuickReplies: []string{"Delivery", " Pickup ", "Delivery"}}); err != nil {
		t.Fatal(err)
	}
	rec := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", first.Token, site, "")
	var got struct{ Messages []chat.Message }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if m := got.Messages[len(got.Messages)-1]; len(m.QuickReplies) != 2 || m.QuickReplies[1] != "Pickup" {
		t.Fatalf("quick replies %+v", m)
	}

	// setUser after the first message fills in what is missing.
	chatSay(t, h, w, first.Token, `{"text":"Delivery","name":"Dana Ruiz","email":"dana@example.com"}`)
	chatSay(t, h, w, first.Token, `{"text":"thanks","name":"Someone Else"}`)
	var name, email, display string
	_ = s.DB.QueryRow(ctx, `SELECT v.name, v.email, c.display_name FROM chat_visitors v JOIN contacts c ON c.id = v.contact_id`).Scan(&name, &email, &display)
	if name != "Dana Ruiz" || email != "dana@example.com" || display != "Dana Ruiz" {
		t.Fatalf("later details %q %q %q", name, email, display)
	}
}

func TestChatTyping(t *testing.T) {
	s, h, w := chatServer(t)
	s.Chat = chat.NewHub(s.DB, s.Logger)
	ctx := context.Background()
	first := chatSay(t, h, w, "", `{"text":"Hello"}`)
	var thread string
	_ = s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors`).Scan(&thread)
	if rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/typing", "", site, ""); rec.Code != 401 {
		t.Fatalf("typing without a conversation=%d", rec.Code)
	}
	if rec := chatCall(h, "POST", "/chat/w/"+w.PublicID+"/typing", first.Token, site, ""); rec.Code != 204 || !s.Chat.IsVisitorTyping(thread) {
		t.Fatalf("visitor typing=%d", rec.Code)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", srv.URL+"/chat/w/"+w.PublicID+"/stream?after="+itoa(first.Message.ID), nil)
	req.Header.Set("Authorization", "Bearer "+first.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.Chat.TeamTyping(thread, "Morgan")
	}()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if scanner.Text() == "event: typing" {
			scanner.Scan()
			if !strings.Contains(scanner.Text(), `"name":"Morgan"`) {
				t.Fatalf("typing event %q", scanner.Text())
			}
			return
		}
	}
	t.Fatal("no typing event")
}
