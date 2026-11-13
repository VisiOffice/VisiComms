package web_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/secrets"
	"github.com/VisiOffice/VisiComms/internal/slack"
	"github.com/VisiOffice/VisiComms/internal/testutil"
	"github.com/VisiOffice/VisiComms/internal/web"
)

type slackPost struct{ Channel, Text, ThreadTS, TS string }

// fakeSlack answers the Web API calls the connector makes.
type fakeSlack struct {
	mu     sync.Mutex
	posts  []slackPost
	emails map[string]string
	next   int
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer xoxb-good" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "auth.test":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T1", "team": "Acme Plumbing", "user_id": "UBOT"})
	case "chat.postMessage":
		f.next++
		ts := fmt.Sprintf("1700000000.%06d", f.next)
		f.posts = append(f.posts, slackPost{r.Form.Get("channel"), r.Form.Get("text"), r.Form.Get("thread_ts"), ts})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
	case "users.info":
		id := r.Form.Get("user")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"name": id, "real_name": "Luis Ortega",
			"profile": map[string]any{"email": f.emails[id]}}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fakeSlack) all() []slackPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slackPost(nil), f.posts...)
}

func slackServer(t *testing.T) (*web.Server, http.Handler, chat.Widget, *fakeSlack, *slack.Mirror) {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Acme Plumbing', 'America/Los_Angeles') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSlack{emails: map[string]string{}}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &slack.Store{DB: pool, Keys: keys, API: &slack.Client{BaseURL: api.URL}}
	s := &web.Server{
		DB: pool, Logger: logger, Auth: &auth.Service{DB: pool}, PublicURL: "https://api.example.com",
		Slack: store, SlackEvents: &slack.Events{Store: store, Logger: logger},
	}
	h := s.Routes()
	w, err := chat.Create(ctx, pool, ws, "Acme Plumbing")
	if err != nil {
		t.Fatal(err)
	}
	return s, h, w, fake, &slack.Mirror{Store: store, Logger: logger, AppURL: "https://app.example.com"}
}

func consoleSession(t *testing.T, s *web.Server, workspace, name, email string) (string, string) {
	t.Helper()
	var person string
	if err := s.DB.QueryRow(context.Background(), `INSERT INTO principals (workspace_id, kind, name, email) VALUES ($1, 'human', $2, $3) RETURNING id`,
		workspace, name, email).Scan(&person); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(name))
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO sessions (token_sha256, principal_id, expires_at) VALUES ($1, $2, now() + interval '1 day')`,
		digest[:], person); err != nil {
		t.Fatal(err)
	}
	return name, person
}

func consoleCall(h http.Handler, method, path, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "vc_session", Value: session})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func slackEvent(h http.Handler, connector, secret, body string, at time.Time) *httptest.ResponseRecorder {
	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	req := httptest.NewRequest("POST", "/slack/events/"+connector, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func slackMessage(eventID, user, text, threadTS, ts string) string {
	b, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "T1", "event_id": eventID,
		"event": map[string]any{"type": "message", "user": user, "text": text, "channel": "C0BINS001", "thread_ts": threadTS, "ts": ts}})
	return string(b)
}

func TestSlackMirror(t *testing.T) {
	s, h, w, fake, mirror := slackServer(t)
	ctx := context.Background()
	session, morgan := consoleSession(t, s, w.WorkspaceID, "Morgan Reyes", "morgan@acmeplumbing.example")

	// A chat from before Slack was connected is never copied.
	chatSay(t, h, w, "", `{"text":"Before Slack"}`)
	time.Sleep(10 * time.Millisecond)

	// Connecting checks the token and posts a test message.
	if rec := consoleCall(h, "PUT", "/api/slack", session, `{"bot_token":"xoxb-bad","signing_secret":"sec","channel_id":"C0BINS001"}`); rec.Code != 400 ||
		!strings.Contains(rec.Body.String(), "bot token is not valid") {
		t.Fatalf("bad token = %d %s", rec.Code, rec.Body)
	}
	rec := consoleCall(h, "PUT", "/api/slack", session, `{"bot_token":"xoxb-good","signing_secret":"shh","channel_id":"C0BINS001","mention":"@here"}`)
	if rec.Code != 200 {
		t.Fatalf("connect = %d %s", rec.Code, rec.Body)
	}
	var view struct {
		Connected bool   `json:"connected"`
		EventsURL string `json:"events_url"`
		Settings  slack.Settings
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if !view.Connected || view.Settings.Mention != "<!here>" || !strings.HasPrefix(view.EventsURL, "https://api.example.com/slack/events/") {
		t.Fatalf("view %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "xoxb-good") || strings.Contains(rec.Body.String(), "shh") {
		t.Fatal("credentials returned to the browser")
	}
	connector := strings.TrimPrefix(view.EventsURL, "https://api.example.com/slack/events/")
	if p := fake.all(); len(p) != 1 || p[0].Channel != "C0BINS001" {
		t.Fatalf("test message %+v", p)
	}

	var old string
	_ = s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors`).Scan(&old)
	if err := mirror.Sync(ctx, old); err != nil || len(fake.all()) != 1 {
		t.Fatalf("a chat from before Slack was copied: %v %+v", err, fake.all())
	}

	// A new chat starts a Slack thread; the visitor's words cannot ping
	// the channel.
	first := chatSay(t, h, w, "", `{"text":"<!channel> water heater this week?","page":"https://www.acmeplumbing.example/repairs","attribution":{"gclid":"abc"}}`)
	var thread string
	_ = s.DB.QueryRow(ctx, `SELECT thread_id FROM chat_visitors WHERE thread_id <> $1`, old).Scan(&thread)
	if err := mirror.Sync(ctx, thread); err != nil {
		t.Fatal(err)
	}
	posts := fake.all()
	root := posts[len(posts)-1]
	if root.ThreadTS != "" || !strings.HasPrefix(root.Text, "<!here>\n*New website chat* from a visitor") ||
		!strings.Contains(root.Text, "from google_ads") || !strings.Contains(root.Text, "&lt;!channel&gt; water heater") ||
		!strings.Contains(root.Text, "<https://app.example.com/threads/"+thread+"|Open in VisiComms>") {
		t.Fatalf("root %q", root.Text)
	}

	// A reply from the console is copied into the Slack thread, once.
	if _, err := review.Reply(ctx, s.DB, review.Reviewer{ID: morgan, WorkspaceID: w.WorkspaceID}, thread, review.EmailPayload{Body: "Yes, Friday."}); err != nil {
		t.Fatal(err)
	}
	_ = mirror.Sync(ctx, thread)
	_ = mirror.Sync(ctx, thread)
	posts = fake.all()
	if p := posts[len(posts)-1]; len(posts) != 3 || p.ThreadTS != root.TS || p.Text != "*Morgan Reyes:* Yes, Friday." {
		t.Fatalf("console reply %+v", posts)
	}

	// Slack's URL check.
	rec = slackEvent(h, connector, "shh", `{"type":"url_verification","challenge":"xyz"}`, time.Now())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"xyz"`) {
		t.Fatalf("challenge = %d %s", rec.Code, rec.Body)
	}
	// A wrong signature, or an old one, is refused.
	if rec := slackEvent(h, connector, "wrong", slackMessage("Ev0", "UANT", "hi", root.TS, "1.1"), time.Now()); rec.Code != 401 {
		t.Fatalf("bad signature = %d", rec.Code)
	}
	if rec := slackEvent(h, connector, "shh", slackMessage("Ev0", "UANT", "hi", root.TS, "1.1"), time.Now().Add(-10*time.Minute)); rec.Code != 401 {
		t.Fatalf("old signature = %d", rec.Code)
	}

	// Someone not linked: nothing goes to the visitor; the bot says why.
	if rec := slackEvent(h, connector, "shh", slackMessage("Ev1", "UNOBODY", "We can do Friday", root.TS, "1.2"), time.Now()); rec.Code != 200 {
		t.Fatalf("unlinked = %d", rec.Code)
	}
	posts = fake.all()
	if p := posts[len(posts)-1]; !strings.Contains(p.Text, "<@UNOBODY> Not sent") || p.ThreadTS != root.TS {
		t.Fatalf("unlinked notice %+v", p)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE thread_id = $1 AND type = 'chat.sent'`, thread); n != 1 {
		t.Fatalf("unlinked reply was sent: %d", n)
	}

	// A member whose Slack email matches is linked and replies as
	// themselves; Slack's retry of the same event does nothing more.
	_, _ = consoleSession(t, s, w.WorkspaceID, "Luis Ortega", "luis@acmeplumbing.example")
	fake.mu.Lock()
	fake.emails["UANT"] = "Luis@AcmePlumbing.example"
	fake.mu.Unlock()
	before := len(fake.all())
	for range 2 {
		if rec := slackEvent(h, connector, "shh", slackMessage("Ev2", "UANT", "Friday works &amp; I will be there &lt;10 am&gt; <https://acmeplumbing.example/pay|pay here>", root.TS, "1.3"), time.Now()); rec.Code != 200 {
			t.Fatalf("reply = %d %s", rec.Code, rec.Body)
		}
	}
	history := chatCall(h, "GET", "/chat/w/"+w.PublicID+"/messages", first.Token, site, "")
	var got struct{ Messages []chat.Message }
	_ = json.Unmarshal(history.Body.Bytes(), &got)
	last := got.Messages[len(got.Messages)-1]
	if len(got.Messages) != 3 || last.Name != "Luis" || last.Text != "Friday works & I will be there <10 am> https://acmeplumbing.example/pay" {
		t.Fatalf("visitor sees %+v", got.Messages)
	}
	if n := count(t, s, `SELECT count(*) FROM principals WHERE slack_user_id = 'UANT'`); n != 1 {
		t.Fatal("Slack user not linked")
	}
	// It is already in Slack, so it is not posted back.
	_ = mirror.Sync(ctx, thread)
	if len(fake.all()) != before {
		t.Fatalf("Slack reply echoed back: %+v", fake.all()[before:])
	}

	// The visitor's next message lands in the same Slack thread; the
	// bot's own messages and edits are ignored.
	chatSay(t, h, w, first.Token, `{"text":"Great, book it"}`)
	_ = mirror.Sync(ctx, thread)
	posts = fake.all()
	if p := posts[len(posts)-1]; p.ThreadTS != root.TS || p.Text != "*Visitor:* Great, book it" {
		t.Fatalf("visitor follow-up %+v", p)
	}
	bot := strings.Replace(slackMessage("Ev3", "UBOT", "echo", root.TS, "1.4"), `"user":"UBOT"`, `"user":"UBOT","bot_id":"B1"`, 1)
	slackEvent(h, connector, "shh", bot, time.Now())
	if n := count(t, s, `SELECT count(*) FROM events WHERE thread_id = $1 AND type = 'chat.sent'`, thread); n != 2 {
		t.Fatalf("bot message was sent to the visitor: %d", n)
	}

	// Linking by hand, and a bad id.
	if rec := consoleCall(h, "PUT", "/api/slack/members/"+morgan, session, `{"slack_user_id":"nope"}`); rec.Code != 400 {
		t.Fatalf("bad member id = %d", rec.Code)
	}
	if rec := consoleCall(h, "PUT", "/api/slack/members/"+morgan, session, `{"slack_user_id":"UROB1234"}`); rec.Code != 204 {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	if rec := consoleCall(h, "PUT", "/api/slack/members/"+morgan, session, `{"slack_user_id":"UANT"}`); rec.Code != 400 {
		t.Fatalf("linking a Slack user twice = %d", rec.Code)
	}

	// Disconnecting stops callbacks.
	if rec := consoleCall(h, "DELETE", "/api/slack", session, ""); rec.Code != 204 {
		t.Fatalf("disconnect = %d", rec.Code)
	}
	if rec := slackEvent(h, connector, "shh", slackMessage("Ev9", "UANT", "hi", root.TS, "1.9"), time.Now()); rec.Code != 404 {
		t.Fatalf("after disconnect = %d", rec.Code)
	}
}
