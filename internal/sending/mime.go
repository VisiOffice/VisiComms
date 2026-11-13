package sending

import (
	"bytes"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// Message is one email to build: plain text, plus HTML when it has any.
type Message struct {
	FromName, FromAddress string
	To                    []string
	Subject, Body         string
	// HTML, when set, goes alongside Body, which stays as the plain-text
	// version for mail apps that do not show HTML.
	HTML string
	// InReplyTo is the customer's Message-ID being answered, so the reply
	// lands in their thread.
	InReplyTo string
	Date      time.Time
}

// Build renders the message as MIME. SES adds its own Message-ID.
func (m Message) Build() []byte {
	var b bytes.Buffer
	from := mail.Address{Name: m.FromName, Address: m.FromAddress}
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", from.String())
	to := make([]string, len(m.To))
	for i, a := range m.To {
		to[i] = (&mail.Address{Address: a}).String()
	}
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", m.Date.Format(time.RFC1123Z))
	if m.InReplyTo != "" {
		header("In-Reply-To", m.InReplyTo)
		header("References", m.InReplyTo)
	}
	header("MIME-Version", "1.0")
	if m.HTML != "" {
		parts := multipart.NewWriter(&b)
		header("Content-Type", `multipart/alternative; boundary="`+parts.Boundary()+`"`)
		b.WriteString("\r\n")
		// Mail apps show the last part they can display, so HTML goes last.
		writePart(parts, "text/plain", m.Body)
		writePart(parts, "text/html", m.HTML)
		_ = parts.Close()
		return b.Bytes()
	}
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	_ = w.Close()
	return b.Bytes()
}

func writePart(parts *multipart.Writer, mediaType, body string) {
	part, _ := parts.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {mediaType + `; charset="utf-8"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	w := quotedprintable.NewWriter(part)
	_, _ = w.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	_ = w.Close()
}
