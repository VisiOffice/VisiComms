package voice

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func sign(secret []byte, id string, ts int64, body string) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s.%d.%s", id, ts, body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestOpenAIWebhooksVerify(t *testing.T) {
	secret := []byte("0123456789abcdef")
	o := &OpenAI{WebhookSecret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}
	now := time.Unix(1_800_000_000, 0)
	body := `{"type":"realtime.call.incoming"}`
	good := sign(secret, "wh_1", now.Unix(), body)
	ts := fmt.Sprint(now.Unix())
	if err := o.Verify([]byte(body), "wh_1", ts, "v1,bad "+good, now); err != nil {
		t.Fatalf("a valid signature among several: %v", err)
	}
	for name, err := range map[string]error{
		"tampered body": o.Verify([]byte(body+" "), "wh_1", ts, good, now),
		"other id":      o.Verify([]byte(body), "wh_2", ts, good, now),
		"stale":         o.Verify([]byte(body), "wh_1", ts, good, now.Add(6*time.Minute)),
		"unsigned":      o.Verify([]byte(body), "wh_1", ts, "", now),
	} {
		if err == nil {
			t.Errorf("%s verified", name)
		}
	}
}

func TestKnowledgeKeepsPhoneArticles(t *testing.T) {
	got := KnowledgeText([]byte(`{"articles":[
		{"title":"Closed offices","content":"The Oak Lane office is closed.","channels":["phone"]},
		{"title":"Patient email","content":"Email only.","channels":["email"]},
		{"title":"Insurance","content":"We take most plans.","channels":[]}]}`))
	if !strings.Contains(got, "### Closed offices\nThe Oak Lane office is closed.") || !strings.Contains(got, "### Insurance") || strings.Contains(got, "Patient email") {
		t.Fatalf("knowledge = %q", got)
	}
	if got := KnowledgeText([]byte(`{"sizes":[10,20]}`)); !strings.Contains(got, `"sizes"`) {
		t.Fatalf("other approved info = %q", got)
	}
}

func TestInstructionsPutTheBusinessRulesFirst(t *testing.T) {
	a := AICall{Business: "Birch Dental", Knowledge: "### Appointments\nMornings only.", Agent: Agent{Instructions: "Never promise an appointment time."}}
	got := a.Instructions()
	for _, want := range []string{"You answer the phone for Birch Dental.", "## Birch Dental's rules", "Never promise an appointment time.", "### Appointments\nMornings only.", "take a message"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
}

// An AI that never starts speaking sends the caller to voicemail rather than
// leaving them on a silent line.
func TestSilentAIFallsBackToVoicemail(t *testing.T) {
	defer func(d time.Duration) { greetingTimeout = d }(greetingTimeout)
	greetingTimeout = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	var fellBack atomic.Int32
	c := &Conversation{
		Call:     AICall{OpenAICallID: "rtc_1", Agent: Agent{Opening: "Hello"}},
		OpenAI:   &OpenAI{SocketURL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fallback: func(context.Context) error { fellBack.Add(1); return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if fellBack.Load() != 1 {
		t.Fatalf("fell back %d times", fellBack.Load())
	}
}

func TestWebhooksSayWhichAPIAnnouncedTheCall(t *testing.T) {
	for body, want := range map[string]IncomingAI{
		`{"type":"realtime.call.incoming","data":{"call_id":"rtc_1","sip_headers":[{"name":"x-visicomms-call","value":"c1"}]}}`:                   {API: APIRealtime, OpenAICallID: "rtc_1", CallID: "c1"},
		`{"type":"live.transport.incoming","data":{"type":"sip","session_id":"live_1","sip_headers":[{"name":"X-VisiComms-Call","value":"c1"}]}}`: {API: APILive, OpenAICallID: "live_1", CallID: "c1"},
		`{"type":"live.call.incoming","data":{"session_id":"live_2"}}`:                                                                            {API: APILive, OpenAICallID: "live_2"},
	} {
		got, ok, err := ParseWebhook([]byte(body))
		if err != nil || !ok || got != want {
			t.Errorf("%s = %+v %v %v", body, got, ok, err)
		}
	}
	if _, ok, _ := ParseWebhook([]byte(`{"type":"live.transport.incoming","data":{"type":"webrtc","session_id":"live_3"}}`)); ok {
		t.Error("a WebRTC session was taken for a call")
	}
}

func TestLivePromptsSplitSpeechFromTools(t *testing.T) {
	a := AICall{Business: "Birch Dental", Knowledge: "### Appointments\nMornings only.",
		Agent: Agent{Opening: "Hi, thanks for calling Cedar Court.", Instructions: "Never mention other patients.", Voice: "gleam", TransferTo: "+13105550100"}}
	live := a.LiveInstructions()
	for _, want := range []string{`"This call may be recorded. Hi, thanks for calling Cedar Court."`, "The call is recorded.", "Never mention other patients.", "### Appointments\nMornings only.", "Delegation policy:", "Transfers:", "speakerphone", "Never wait in silence", "letter by letter", "Delegate once"} {
		if !strings.Contains(live, want) {
			t.Errorf("live instructions lack %q", want)
		}
	}
	if g := a.Greeting(); !strings.Contains(g, "This call may be recorded. Hi, thanks") {
		t.Errorf("greeting = %q", g)
	}
	if backend := a.BackendInstructions(); !strings.Contains(backend, "transfer_to_person") || strings.Contains(backend, "Mornings only") {
		t.Errorf("backend instructions = %q", backend)
	}
	// The realtime model cannot speak GPT-Live's newer voices.
	if v := a.Session()["audio"].(map[string]any)["output"].(map[string]any)["voice"]; v != "marin" {
		t.Errorf("realtime voice = %v", v)
	}
}
