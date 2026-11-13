// Package email parses inbound email as the Cloudflare email worker posts it,
// and classifies what must not wake an agent.
package email

import (
	"bytes"
	"encoding/base64"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Inbound is one received email, decoded.
type Inbound struct {
	// Provider identifies the adapter; empty means the legacy Cloudflare path.
	Provider  string
	MessageID string
	// To is the address the worker received it at; it selects the workspace.
	To      string
	From    Address
	ReplyTo *Address
	// Contact is who to reply to: Reply-To when present (lead forms send
	// From a no-reply address with the person in Reply-To), else From.
	Contact Address
	Subject string
	Text    string
	HTML    string
	// InReplyTo and References, in order, without duplicates.
	References  []string
	Headers     map[string]string
	Attachments []Attachment
	Date        time.Time
}

type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Attachment is a file the worker already stored; the event keeps the pointer.
type Attachment struct {
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum,omitempty"`
}

// Parse reads the worker's JSON fields. Both camelCase and snake_case keys
// are accepted, as the worker has sent both.
func Parse(p map[string]any, now time.Time) Inbound {
	in := Inbound{
		MessageID: str(p, "messageId", "message_id"),
		To:        str(p, "to"),
		From:      ParseAddress(str(p, "from")),
		Subject:   str(p, "subject"),
		Headers:   normalizeHeaders(p["headers"]),
		Date:      now,
	}
	if rt := str(p, "replyTo", "reply_to"); rt != "" {
		a := ParseAddress(rt)
		in.ReplyTo = &a
	}
	in.Contact = in.From
	if in.ReplyTo != nil && in.ReplyTo.Email != "" {
		in.Contact = *in.ReplyTo
		if in.Contact.Name == "" {
			in.Contact.Name = in.From.Name
		}
	}

	in.Text = decodeContent(str(p, "text", "body"))
	in.HTML = decodeContent(str(p, "html"))
	if in.HTML == "" && looksLikeHTML(in.Text) {
		in.HTML = in.Text
		in.Text = HTMLText(in.HTML)
	}

	in.References = references(str(p, "inReplyTo", "in_reply_to"), str(p, "references"))
	in.Attachments = attachments(p["attachments"])
	if d := str(p, "date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			in.Date = t
		}
	}
	return in
}

func str(p map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := p[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ParseAddress reads "Name <email>" or a bare address; the email is lower-cased.
func ParseAddress(s string) Address {
	s = strings.TrimSpace(s)
	if s == "" {
		return Address{}
	}
	if a, err := mail.ParseAddress(s); err == nil {
		return Address{Name: a.Name, Email: strings.ToLower(a.Address)}
	}
	// Loose fallback for headers net/mail rejects, e.g. unquoted commas.
	if i, j := strings.LastIndex(s, "<"), strings.LastIndex(s, ">"); i >= 0 && j > i {
		name := strings.Trim(strings.TrimSpace(s[:i]), `"'`)
		return Address{Name: name, Email: strings.ToLower(strings.TrimSpace(s[i+1 : j]))}
	}
	return Address{Email: strings.ToLower(s)}
}

// normalizeHeaders accepts a map, a list of {name, value}, or raw header
// text, and returns lower-cased names.
func normalizeHeaders(v any) map[string]string {
	out := map[string]string{}
	switch h := v.(type) {
	case map[string]any:
		for k, val := range h {
			if s, ok := val.(string); ok {
				out[strings.ToLower(k)] = s
			}
		}
	case []any:
		for _, item := range h {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			value, _ := m["value"].(string)
			if name != "" {
				out[strings.ToLower(name)] = value
			}
		}
	case string:
		for _, line := range strings.Split(h, "\n") {
			if k, val, ok := strings.Cut(line, ":"); ok {
				out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(val)
			}
		}
	}
	return out
}

func references(inReplyTo, refs string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range append([]string{inReplyTo}, strings.Fields(refs)...) {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func attachments(v any) []Attachment {
	list, _ := v.([]any)
	var out []Attachment
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		a := Attachment{Key: str(m, "key"), Filename: str(m, "filename"), ContentType: str(m, "content_type"), Checksum: str(m, "checksum")}
		if a.Key == "" {
			continue
		}
		if a.Filename == "" {
			a.Filename = "attachment"
		}
		if a.ContentType == "" {
			a.ContentType = "application/octet-stream"
		}
		if n, ok := m["byte_size"].(float64); ok {
			a.ByteSize = int64(n)
		}
		out = append(out, a)
	}
	return out
}

var (
	qpHex        = regexp.MustCompile(`=[0-9A-Fa-f]{2}`)
	qpSoftBreak  = regexp.MustCompile(`=\r?\n`)
	base64Only   = regexp.MustCompile(`^[A-Za-z0-9+/]+=*$`)
	whitespace   = regexp.MustCompile(`\s+`)
	htmlMarkers  = regexp.MustCompile(`(?i)<!DOCTYPE\s+html|<html[\s>]|<body[\s>]|<div[\s>]|<p[\s>]|<br\s*/?>|<table[\s>]`)
	threeLetters = regexp.MustCompile(`[a-zA-Z]{3,}`)
)

// decodeContent undoes base64 or quoted-printable bodies the worker passed
// through still encoded. The checks are heuristics that have handled
// real mail; well-formed text passes through unchanged.
func decodeContent(s string) string {
	if s == "" {
		return ""
	}
	if looksLikeBase64(s) {
		if b, err := base64.StdEncoding.DecodeString(whitespace.ReplaceAllString(s, "")); err == nil {
			return toUTF8(b)
		}
		return s
	}
	if qpHex.MatchString(s) {
		decoded := qpSoftBreak.ReplaceAllString(s, "")
		var buf bytes.Buffer
		for i := 0; i < len(decoded); i++ {
			if decoded[i] == '=' && i+2 < len(decoded) && isHex(decoded[i+1]) && isHex(decoded[i+2]) {
				buf.WriteByte(unhex(decoded[i+1])<<4 | unhex(decoded[i+2]))
				i += 2
				continue
			}
			buf.WriteByte(decoded[i])
		}
		return toUTF8(buf.Bytes())
	}
	return s
}

func looksLikeBase64(s string) bool {
	clean := whitespace.ReplaceAllString(s, "")
	if len(clean) < 50 || !base64Only.MatchString(clean) {
		return false
	}
	if strings.Contains(s, ". ") || strings.Contains(s, ", ") || strings.Contains(s, "<") {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return false
	}
	return bytes.ContainsAny(b, "<\n") || threeLetters.Match(b)
}

// toUTF8 keeps valid UTF-8 and otherwise reads the bytes as ISO-8859-1,
// which every byte sequence is.
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

func looksLikeHTML(s string) bool { return s != "" && htmlMarkers.MatchString(s) }

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
