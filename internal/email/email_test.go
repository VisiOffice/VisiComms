package email

import (
	"encoding/base64"
	"reflect"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)

func TestParsePrefersReplyToForTheContact(t *testing.T) {
	in := Parse(map[string]any{
		"from":    "Website Form <noreply@acmeplumbing.example>",
		"replyTo": "marcus@roofco.example",
		"to":      "In <in+acmeplumbing@in.visicomms.app>",
	}, now)
	if in.Contact.Email != "marcus@roofco.example" {
		t.Fatalf("contact = %q, want the Reply-To address", in.Contact.Email)
	}
	if in.Contact.Name != "Website Form" {
		t.Fatalf("contact name = %q, want From's name when Reply-To has none", in.Contact.Name)
	}
	if in.From.Email != "noreply@acmeplumbing.example" {
		t.Fatalf("from = %q", in.From.Email)
	}
}

func TestParseLowercasesAndAcceptsLooseAddresses(t *testing.T) {
	cases := map[string]Address{
		"Dana Whitfield <Dana@Example.COM>":   {Name: "Dana Whitfield", Email: "dana@example.com"},
		"Lee, Marcus <marcus@roofco.example>": {Name: "Lee, Marcus", Email: "marcus@roofco.example"},
		"plain@example.com":                   {Email: "plain@example.com"},
	}
	for raw, want := range cases {
		if got := ParseAddress(raw); got != want {
			t.Errorf("ParseAddress(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

func TestParseDecodesQuotedPrintableAndBase64(t *testing.T) {
	qp := Parse(map[string]any{"text": "Price for a water heater =3D a week?=\nThanks caf=C3=A9"}, now)
	if qp.Text != "Price for a water heater = a week?Thanks café" {
		t.Errorf("quoted-printable text = %q", qp.Text)
	}
	body := "Hi, I need a repair for a garage cleanout in Mesa next week please."
	b64 := Parse(map[string]any{"text": base64.StdEncoding.EncodeToString([]byte(body))}, now)
	if b64.Text != body {
		t.Errorf("base64 text = %q", b64.Text)
	}
	plain := Parse(map[string]any{"text": "Do you have a water heater? Thanks, Marcus."}, now)
	if plain.Text != "Do you have a water heater? Thanks, Marcus." {
		t.Errorf("plain text changed: %q", plain.Text)
	}
}

func TestParseTakesTextFromHTMLSentAsText(t *testing.T) {
	in := Parse(map[string]any{"text": "<html><body><p>Need a water heater</p><style>p{}</style></body></html>"}, now)
	if in.HTML == "" || in.Text != "Need a water heater" {
		t.Fatalf("text = %q, html = %q", in.Text, in.HTML)
	}
}

func TestParseReferencesHeadersAndDate(t *testing.T) {
	in := Parse(map[string]any{
		"inReplyTo":  "<b@x>",
		"references": "<a@x> <b@x>",
		"headers":    []any{map[string]any{"name": "Auto-Submitted", "value": "auto-replied"}},
		"date":       "Thu, 08 Oct 2026 09:14:00 -0700",
		"attachments": []any{
			map[string]any{"key": "r2/abc", "byte_size": float64(1200)},
			map[string]any{"filename": "no-key.pdf"},
		},
	}, now)
	if !reflect.DeepEqual(in.References, []string{"<b@x>", "<a@x>"}) {
		t.Errorf("references = %v", in.References)
	}
	if in.Headers["auto-submitted"] != "auto-replied" {
		t.Errorf("headers = %v", in.Headers)
	}
	if !in.Date.Equal(time.Date(2026, 10, 8, 16, 14, 0, 0, time.UTC)) {
		t.Errorf("date = %v", in.Date)
	}
	if len(in.Attachments) != 1 || in.Attachments[0].Filename != "attachment" || in.Attachments[0].ByteSize != 1200 {
		t.Errorf("attachments = %+v", in.Attachments)
	}
	if text := Parse(map[string]any{"headers": "Precedence: bulk\nX-Other: 1"}, now).Headers; text["precedence"] != "bulk" {
		t.Errorf("raw header text = %v", text)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		in     map[string]any
		kind   Kind
		optOut bool
	}{
		{"customer inquiry", map[string]any{"from": "marcus@roofco.example", "subject": "water heater for Tuesday?", "text": "What does a week cost?"}, KindMessage, false},
		{"mailer daemon", map[string]any{"from": "MAILER-DAEMON@mx.example.com", "subject": "Hi"}, KindBounce, false},
		{"bounce subject", map[string]any{"from": "x@y.com", "subject": "Undeliverable: your quote"}, KindBounce, false},
		{"bounce quoting unsubscribe", map[string]any{"from": "postmaster@y.com", "subject": "unsubscribe"}, KindBounce, false},
		{"auto-submitted header", map[string]any{"from": "x@y.com", "headers": map[string]any{"Auto-Submitted": "auto-replied"}}, KindAutoReply, false},
		{"auto-submitted no", map[string]any{"from": "x@y.com", "headers": map[string]any{"Auto-Submitted": "no"}}, KindMessage, false},
		{"out of office", map[string]any{"from": "x@y.com", "subject": "Out of Office: back Monday"}, KindAutoReply, false},
		{"no-reply sender", map[string]any{"from": "notifications@crm.example.com", "subject": "New lead"}, KindAutomated, false},
		{"unsubscribe subject", map[string]any{"from": "x@y.com", "subject": "Re: Unsubscribe"}, KindMessage, true},
		{"stop first line", map[string]any{"from": "x@y.com", "subject": "Re: your quote", "text": "STOP\n\nOn Tue, Acme Plumbing wrote:\n> Hi"}, KindMessage, true},
		{"remove me", map[string]any{"from": "x@y.com", "text": "Please remove me."}, KindMessage, true},
		{"stop in a sentence", map[string]any{"from": "x@y.com", "text": "Stop by Tuesday if you can, we need the water heater."}, KindMessage, false},
		{"unsubscribe only in quote", map[string]any{"from": "x@y.com", "text": "Yes please book it\n> unsubscribe"}, KindMessage, false},
	}
	for _, c := range cases {
		got := Classify(Parse(c.in, now))
		if got.Kind != c.kind || got.OptOut != c.optOut {
			t.Errorf("%s: got %+v, want kind %s opt-out %v", c.name, got, c.kind, c.optOut)
		}
	}
}

func TestSplitHidden(t *testing.T) {
	visible, hidden := SplitHidden(`<p>Need a repair</p><div style="display:none">secret <b>words</b></div><span hidden>also</span><script>x()</script>`)
	if visible != "Need a repair" || hidden != "secret words also" {
		t.Fatalf("visible = %q, hidden = %q", visible, hidden)
	}
}
