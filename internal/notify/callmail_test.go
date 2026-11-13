package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func aiCall(t *testing.T, payload string) item {
	t.Helper()
	it := item{Workspace: "Birch Dental", ThreadID: "t1", Type: "call.ai_handled", Contact: "Jane Doe"}
	if err := json.Unmarshal([]byte(payload), &it.Payload); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestCallEmailDrawsTheConversation(t *testing.T) {
	n := &Notifier{AppURL: "https://app.example.com/"}
	it := aiCall(t, `{"summary":"Wants an appointment. Call back: +15551234567","message":{"reason":"Wants an appointment"},"transcript":[
		{"speaker":"ai","text":"Hi, this is Birch Dental."},
		{"speaker":"caller","text":"Is Tuesday free?"},
		{"speaker":"ai","text":"Let me take a message."}]}`)
	got, err := n.callHTML(it)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Hi, this is Birch Dental.", "Is Tuesday free?", "#007aff", "#e9e9eb", "Wants an appointment. Call back:", `href="https://app.example.com/threads/t1"`, "Jane Doe"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(it.transcriptText(), "Caller: Is Tuesday free?\nAI: Let me take a message.\n") {
		t.Errorf("plain transcript = %q", it.transcriptText())
	}
	// Each side is named once, the first time it speaks.
	if c := strings.Count(got, ">AI</div>"); c != 1 {
		t.Errorf("AI labelled %d times", c)
	}
}

func TestCallEmailEscapesCallerWords(t *testing.T) {
	n := &Notifier{AppURL: "https://app.example.com"}
	it := aiCall(t, `{"summary":"<img src=x>","transcript":[{"speaker":"caller","text":"<script>alert(1)</script><a href='https://evil.test'>click</a>"}]}`)
	it.Contact = "<b>Mallory</b>"
	got, err := n.callHTML(it)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script>", "<img", "<b>Mallory", "<a href='https://evil.test'", `<a href="https://evil.test`} {
		if strings.Contains(got, bad) {
			t.Errorf("unescaped %q", bad)
		}
	}
}

func TestCallEmailDoesNotRepeatWhatTheCallerSaid(t *testing.T) {
	it := aiCall(t, `{"summary":"No message taken. The caller said: hello there","transcript":[{"speaker":"caller","text":"hello there"}]}`)
	if got := it.callSummary(it.bubbles()); got != "No message taken." {
		t.Fatalf("summary = %q", got)
	}
}

func TestCallEmailCapsLongCalls(t *testing.T) {
	var lines []string
	for i := range maxBubbles + 5 {
		lines = append(lines, fmt.Sprintf(`{"speaker":"ai","text":"line %d"}`, i))
	}
	it := aiCall(t, `{"summary":"s","transcript":[`+strings.Join(lines, ",")+`]}`)
	got, err := (&Notifier{AppURL: "https://a.test"}).callHTML(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "5 more in VisiComms") || strings.Contains(got, fmt.Sprintf("line %d<", maxBubbles)) {
		t.Fatal("long call was not capped")
	}
	if !strings.Contains(it.transcriptText(), "(5 more in VisiComms)") {
		t.Fatal("plain transcript was not capped")
	}
}

func TestVoicemailAndMissedCallEmails(t *testing.T) {
	vm := item{Workspace: "Birch Dental", ThreadID: "t", Type: "voice.voicemail", Contact: "a contact", Payload: map[string]any{"transcript": "Call me back", "duration_seconds": float64(9)}}
	b := vm.bubbles()
	if len(b) != 1 || b[0].FromAI || b[0].Text != "Call me back" {
		t.Fatalf("voicemail bubbles = %+v", b)
	}
	missed := item{Workspace: "Birch Dental", ThreadID: "t", Type: "call.missed", Contact: "Jane", Payload: map[string]any{}}
	got, err := (&Notifier{AppURL: "https://a.test"}).callHTML(missed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "#e9e9eb") || !strings.Contains(got, "Missed call from Jane") || !strings.Contains(got, "/threads/t") {
		t.Fatal("missed call email is wrong")
	}
}
