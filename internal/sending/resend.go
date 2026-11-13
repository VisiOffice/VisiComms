package sending

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/email"
)

// Resend uses one installation-level API key. It never retries a send. HTTP
// is injectable for tests; production requests only go to api.resend.com.
type Resend struct {
	key  string
	HTTP *http.Client
}

func NewResend(key string) (*Resend, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("RESEND_API_KEY is required")
	}
	return &Resend{key: key}, nil
}
func (*Resend) Kind() string { return "resend" }

func (r *Resend) request(ctx context.Context, method, path string, input, output any, idempotencyKey string) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.resend.com"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VisiComms")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if r.HTTP != nil {
		*client = *r.HTTP
	}
	// Redirecting a POST can change its method or duplicate the operation.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	const maxResponse = 12 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponse {
		return errors.New("Resend response exceeds size limit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Do not include arbitrary response bodies, which may contain customer data.
		reason := fmt.Sprintf("Resend returned HTTP %d", res.StatusCode)
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 && res.StatusCode != 409 {
			return &Rejected{Reason: reason}
		}
		return errors.New(reason)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decoding Resend response: %w", err)
		}
	}
	return nil
}

// messageBodies returns the plain-text and HTML versions of a built message.
func messageBodies(msg *mail.Message) (text, htmlBody string, err error) {
	mediaType, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mediaType != "multipart/alternative" {
		var body io.Reader = msg.Body
		if strings.EqualFold(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			body = quotedprintable.NewReader(body)
		}
		data, err := io.ReadAll(body)
		return string(data), "", err
	}
	parts := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := parts.NextPart() // decodes quoted-printable
		if errors.Is(err, io.EOF) {
			return text, htmlBody, nil
		}
		if err != nil {
			return "", "", err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return "", "", err
		}
		switch partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type")); partType {
		case "text/plain":
			text = string(data)
		case "text/html":
			htmlBody = string(data)
		}
	}
}

func (r *Resend) Send(ctx context.Context, raw []byte, tags map[string]string) (string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", &Rejected{Reason: "Invalid email message"}
	}
	to, err := msg.Header.AddressList("To")
	if err != nil {
		return "", &Rejected{Reason: "Invalid email recipients"}
	}
	decoder := new(mime.WordDecoder)
	subject, err := decoder.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		return "", &Rejected{Reason: "Invalid email subject"}
	}
	text, htmlBody, err := messageBodies(msg)
	if err != nil {
		return "", &Rejected{Reason: "Invalid email body"}
	}
	recipients := make([]string, len(to))
	for i, addr := range to {
		recipients[i] = addr.Address
	}
	headers := map[string]string{}
	for _, name := range []string{"In-Reply-To", "References"} {
		if value := msg.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	type tag struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	emailTags := []tag{}
	for name, value := range tags {
		emailTags = append(emailTags, tag{name, value})
	}
	input := map[string]any{"from": msg.Header.Get("From"), "to": recipients, "subject": subject, "text": text, "headers": headers, "tags": emailTags}
	if htmlBody != "" {
		input["html"] = htmlBody
	}
	var out struct {
		ID string `json:"id"`
	}
	key := ""
	if actionID := tags["action_id"]; actionID != "" {
		key = "visicomms/" + actionID
	}
	if err := r.request(ctx, http.MethodPost, "/emails", input, &out, key); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("Resend accepted the request without a message ID")
	}
	return out.ID, nil
}

type resendDomain struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Capabilities struct {
		Sending string `json:"sending"`
	} `json:"capabilities"`
	Records []struct {
		Record   string `json:"record"`
		Type     string `json:"type"`
		Name     string `json:"name"`
		Value    string `json:"value"`
		Status   string `json:"status"`
		Priority *int   `json:"priority"`
	} `json:"records"`
}

var resendID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (r *Resend) findDomain(ctx context.Context, name string) (string, error) {
	path := "/domains?limit=100"
	for {
		var page struct {
			Data    []resendDomain `json:"data"`
			HasMore bool           `json:"has_more"`
		}
		if err := r.request(ctx, http.MethodGet, path, nil, &page, ""); err != nil {
			return "", err
		}
		for _, d := range page.Data {
			if strings.EqualFold(d.Name, name) {
				return d.ID, nil
			}
		}
		if !page.HasMore {
			return "", nil
		}
		if len(page.Data) == 0 {
			return "", errors.New("invalid Resend domain pagination")
		}
		next := "/domains?limit=100&after=" + url.QueryEscape(page.Data[len(page.Data)-1].ID)
		if next == path {
			return "", errors.New("Resend domain pagination did not advance")
		}
		path = next
	}
}

func (r *Resend) RegisterDomain(ctx context.Context, domain string) (Identity, error) {
	id, err := r.findDomain(ctx, domain)
	if err != nil {
		return Identity{}, err
	}
	if id != "" {
		return r.getDomain(ctx, id)
	}
	var d resendDomain
	if err := r.request(ctx, http.MethodPost, "/domains", map[string]any{"name": domain}, &d, ""); err != nil {
		return Identity{}, err
	}
	if !resendID.MatchString(d.ID) {
		return Identity{}, errors.New("Resend returned an invalid domain ID")
	}
	return resendIdentity(d), nil
}

func (r *Resend) DomainStatus(ctx context.Context, domain string) (Identity, error) {
	id, err := r.findDomain(ctx, domain)
	if err != nil {
		return Identity{}, err
	}
	if !resendID.MatchString(id) {
		return Identity{}, errors.New("domain is missing from the Resend account")
	}
	if err := r.request(ctx, http.MethodPost, "/domains/"+id+"/verify", nil, nil, ""); err != nil {
		return Identity{}, err
	}
	return r.getDomain(ctx, id)
}
func (r *Resend) getDomain(ctx context.Context, id string) (Identity, error) {
	if !resendID.MatchString(id) {
		return Identity{}, errors.New("invalid Resend domain ID")
	}
	var d resendDomain
	if err := r.request(ctx, http.MethodGet, "/domains/"+id, nil, &d, ""); err != nil {
		return Identity{}, err
	}
	return resendIdentity(d), nil
}
func resendIdentity(d resendDomain) Identity {
	id := Identity{Status: strings.ToUpper(d.Status), Records: []Record{}}
	if d.Status == "verified" && d.Capabilities.Sending != "disabled" {
		id.Status = "SUCCESS"
	}
	for _, record := range d.Records {
		name := strings.TrimSuffix(record.Name, ".")
		if name == "@" || name == "" {
			name = d.Name
		} else if name != d.Name && !strings.HasSuffix(name, "."+d.Name) {
			name += "." + d.Name
		}
		id.Records = append(id.Records, Record{Type: record.Type, Name: name, Value: record.Value, Priority: record.Priority,
			Purpose: "Resend " + record.Record, OK: record.Status == "verified"})
	}
	return id
}

// ReceivedEmail includes the recipient metadata returned by Resend.
// Attachments retain provider locators; no short-lived signed URLs are stored.
type ReceivedEmail struct {
	ID          string            `json:"id"`
	From        string            `json:"from"`
	To          []string          `json:"to"`
	ReceivedFor []string          `json:"received_for"`
	ReplyTo     []string          `json:"reply_to"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	MessageID   string            `json:"message_id"`
	CreatedAt   time.Time         `json:"created_at"`
	Headers     map[string]string `json:"headers"`
	Attachments []struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
}

func (r *Resend) Receive(ctx context.Context, id string) (ReceivedEmail, error) {
	var received ReceivedEmail
	if !resendID.MatchString(id) {
		return received, errors.New("invalid Resend email ID")
	}
	err := r.request(ctx, http.MethodGet, "/emails/receiving/"+id+"?html_format=cid", nil, &received, "")
	if err == nil && received.ID != id {
		err = errors.New("Resend returned a different email ID")
	}
	return received, err
}
func (r ReceivedEmail) Inbound(to string, now time.Time) email.Inbound {
	if !r.CreatedAt.IsZero() {
		now = r.CreatedAt
	}
	headers := map[string]any{}
	normalized := map[string]string{}
	for name, value := range r.Headers {
		headers[name] = value
		normalized[strings.ToLower(name)] = value
	}
	replyTo := normalized["reply-to"]
	if len(r.ReplyTo) > 0 {
		replyTo = r.ReplyTo[0]
	}
	in := email.Parse(map[string]any{"message_id": r.MessageID, "from": r.From, "to": to, "subject": r.Subject,
		"text": r.Text, "html": r.HTML, "headers": headers, "reply_to": replyTo,
		"in_reply_to": normalized["in-reply-to"], "references": normalized["references"], "date": normalized["date"]}, now)
	in.Provider = "resend"
	if in.MessageID == "" {
		in.MessageID = "<" + r.ID + "@resend.invalid>"
	}
	if in.Text == "" && in.HTML != "" {
		in.Text = email.HTMLText(in.HTML)
	}
	for _, attachment := range r.Attachments {
		in.Attachments = append(in.Attachments, email.Attachment{Key: "resend:" + r.ID + "/" + attachment.ID,
			Filename: attachment.Filename, ContentType: attachment.ContentType, ByteSize: attachment.Size})
	}
	return in
}
