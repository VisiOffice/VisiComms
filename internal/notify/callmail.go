package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

const (
	// maxBubbles keeps a long call under the size where mail apps clip a
	// message; the rest is in the console.
	maxBubbles = 60
	// maxBubbleText is the most one bubble shows.
	maxBubbleText = 1000
)

// bubble is one turn of a call, drawn as a text message.
type bubble struct {
	FromAI bool
	Label  string // who is speaking, shown the first time each side speaks
	Text   string
}

// callEmail is what the call notification template draws.
type callEmail struct {
	Workspace string
	Headline  string
	Summary   string
	Bubbles   []bubble
	More      int
	Link      string
	Settings  string
}

// isCall says whether the event is a phone call, which gets the HTML email.
func (it item) isCall() bool {
	switch it.Type {
	case "voice.voicemail", "call.missed", "call.ai_handled", "call.answered":
		return true
	}
	return false
}

// bubbles is the call as a text conversation: the AI on one side and the
// caller on the other. A voicemail is the caller alone.
func (it item) bubbles() []bubble {
	var out []bubble
	switch it.Type {
	case "voice.voicemail":
		if t := clip(str(it.Payload, "transcript"), maxBubbleText); t != "" {
			out = append(out, bubble{Text: t})
		}
	case "call.ai_handled":
		lines, _ := it.Payload["transcript"].([]any)
		for _, l := range lines {
			line, _ := l.(map[string]any)
			if t := clip(str(line, "text"), maxBubbleText); t != "" {
				out = append(out, bubble{FromAI: str(line, "speaker") != "caller", Text: t})
			}
		}
	}
	return out
}

// callerName is how the conversation labels the caller.
func (it item) callerName() string {
	if it.Contact == "a contact" {
		return "Caller"
	}
	return it.Contact
}

// callSummary is the line above the conversation. When no message was taken
// the stored summary only repeats what the caller said, which the
// conversation already shows.
func (it item) callSummary(bubbles []bubble) string {
	if it.Type != "call.ai_handled" {
		return ""
	}
	summary := str(it.Payload, "summary")
	if len(bubbles) > 0 && strings.HasPrefix(summary, "No message taken.") {
		return "No message taken."
	}
	return clip(summary, 2000)
}

// transcriptText is the conversation for mail apps that show plain text.
func (it item) transcriptText() string {
	if it.Type != "call.ai_handled" {
		return ""
	}
	bubbles := it.bubbles()
	var b strings.Builder
	for i, m := range bubbles {
		if i == maxBubbles {
			fmt.Fprintf(&b, "(%d more in VisiComms)\n", len(bubbles)-maxBubbles)
			break
		}
		who := "Caller"
		if m.FromAI {
			who = "AI"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, m.Text)
	}
	return b.String()
}

// callHTML draws the call. Everything from the call goes through
// html/template, so a caller's words can never become markup.
func (n *Notifier) callHTML(it item) (string, error) {
	bubbles := it.bubbles()
	mail := callEmail{
		Workspace: it.Workspace,
		Headline:  it.headline(),
		Summary:   it.callSummary(bubbles),
		Link:      n.link(it),
		Settings:  strings.TrimRight(n.AppURL, "/") + "/settings/notifications",
	}
	if len(bubbles) > maxBubbles {
		mail.More = len(bubbles) - maxBubbles
		bubbles = bubbles[:maxBubbles]
	}
	var seenAI, seenCaller bool
	for i := range bubbles {
		switch {
		case bubbles[i].FromAI && !seenAI:
			bubbles[i].Label, seenAI = "AI", true
		case !bubbles[i].FromAI && !seenCaller:
			bubbles[i].Label, seenCaller = it.callerName(), true
		}
	}
	mail.Bubbles = bubbles
	var b bytes.Buffer
	if err := callTemplate.Execute(&b, mail); err != nil {
		return "", err
	}
	return b.String(), nil
}

// callTemplate uses nested tables and inline styles because mail apps ignore
// most other layout. Rounded corners fall back to square in desktop Outlook.
var callTemplate = template.Must(template.New("call").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>{{.Headline}}</title>
</head>
<body style="margin:0;padding:0;background-color:#f2f2f7;font-family:-apple-system,BlinkMacSystemFont,'Helvetica Neue',Helvetica,Arial,sans-serif;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#f2f2f7"><tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff" style="max-width:480px;background-color:#ffffff;border-radius:16px;">
<tr><td style="padding:24px 20px 4px 20px;">
<div style="font-size:13px;line-height:18px;color:#6e6e73;">{{.Workspace}}</div>
<div style="font-size:19px;line-height:25px;font-weight:600;color:#1c1c1e;padding-top:2px;">{{.Headline}}</div>
{{- if .Summary}}
<div style="font-size:15px;line-height:21px;color:#3a3a3c;padding-top:10px;">{{.Summary}}</div>
{{- end}}
</td></tr>
{{- if .Bubbles}}
<tr><td style="padding:14px 16px 0 16px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
{{- range .Bubbles}}
<tr><td align="{{if .FromAI}}right{{else}}left{{end}}" style="padding:3px 0;">
{{- if .Label}}
<div style="font-size:12px;line-height:16px;color:#8e8e93;padding:6px 12px 3px 12px;text-align:{{if .FromAI}}right{{else}}left{{end}};">{{.Label}}</div>
{{- end}}
<table role="presentation" align="{{if .FromAI}}right{{else}}left{{end}}" cellpadding="0" cellspacing="0" border="0" style="max-width:80%;"><tr>
{{- if .FromAI}}
<td bgcolor="#007aff" style="background-color:#007aff;color:#ffffff;border-radius:18px 18px 4px 18px;padding:8px 13px;font-size:15px;line-height:20px;word-break:break-word;">{{.Text}}</td>
{{- else}}
<td bgcolor="#e9e9eb" style="background-color:#e9e9eb;color:#000000;border-radius:18px 18px 18px 4px;padding:8px 13px;font-size:15px;line-height:20px;word-break:break-word;">{{.Text}}</td>
{{- end}}
</tr></table>
</td></tr>
{{- end}}
</table>
</td></tr>
{{- if .More}}
<tr><td align="center" style="padding:10px 20px 0 20px;font-size:13px;line-height:18px;color:#8e8e93;">{{.More}} more in VisiComms</td></tr>
{{- end}}
{{- end}}
<tr><td align="center" style="padding:22px 20px 24px 20px;">
<a href="{{.Link}}" style="display:inline-block;background-color:#007aff;color:#ffffff;font-size:15px;line-height:20px;font-weight:600;text-decoration:none;padding:11px 22px;border-radius:22px;">Open the call</a>
</td></tr>
</table>
<div style="max-width:480px;font-size:12px;line-height:17px;color:#8e8e93;padding:14px 20px 0 20px;"><a href="{{.Settings}}" style="color:#8e8e93;">Change what you are told about</a></div>
</td></tr></table>
</body>
</html>
`))
