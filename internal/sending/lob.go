package sending

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/postal"
	"github.com/VisiOffice/VisiComms/internal/secrets"
)

// LobAPI is Lob's API root.
const LobAPI = "https://api.lob.com/v1"

// Lob prints and mails letters through one Lob account.
type Lob struct {
	Key string
	// Base is the API root; empty means LobAPI. Tests replace it.
	Base string
	HTTP *http.Client
}

// Test is whether the key is a test key: Lob renders the letter but never
// mails it.
func (l *Lob) Test() bool { return strings.HasPrefix(l.Key, "test_") }

func (l *Lob) base() string {
	if l.Base != "" {
		return l.Base
	}
	return LobAPI
}

// do calls Lob. A 4xx is Lob refusing (Rejected, with Lob's own words); a
// 5xx, timeout or rate limit leaves the outcome unknown.
func (l *Lob) do(ctx context.Context, method, path, contentType string, body io.Reader, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, l.base()+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(l.Key, "")
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if l.HTTP != nil {
		*client = *l.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		reason := fmt.Sprintf("Lob returned HTTP %d", res.StatusCode)
		var detail struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Error.Message != "" {
			reason += ": " + detail.Error.Message
		}
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 && res.StatusCode != 429 {
			return &Rejected{Reason: reason}
		}
		return errors.New(reason)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Check confirms the key works.
func (l *Lob) Check(ctx context.Context) error {
	return l.do(ctx, http.MethodGet, "/addresses?limit=1", "", nil, nil, nil)
}

// Verify standardises a US address and says whether USPS can deliver to
// it. With a test key Lob only returns sample data, so the address is kept
// as entered and marked unchecked.
func (l *Lob) Verify(ctx context.Context, a postal.Address) (postal.Address, string, error) {
	if l.Test() {
		return a, "unchecked", nil
	}
	body, _ := json.Marshal(map[string]string{
		"recipient": a.Name, "primary_line": a.Line1, "secondary_line": a.Line2,
		"city": a.City, "state": a.State, "zip_code": a.Zip,
	})
	var out struct {
		Deliverability string `json:"deliverability"`
		PrimaryLine    string `json:"primary_line"`
		SecondaryLine  string `json:"secondary_line"`
		Components     struct {
			City      string `json:"city"`
			State     string `json:"state"`
			ZipCode   string `json:"zip_code"`
			ZipCodeP4 string `json:"zip_code_plus_4"`
		} `json:"components"`
	}
	if err := l.do(ctx, http.MethodPost, "/us_verifications", "application/json", bytes.NewReader(body), nil, &out); err != nil {
		return a, "", err
	}
	if out.Deliverability == "" {
		return a, "", errors.New("Lob did not say whether the address is deliverable")
	}
	if out.PrimaryLine != "" && out.Components.City != "" && out.Components.ZipCode != "" {
		a.Line1, a.Line2, a.City, a.State, a.Zip = out.PrimaryLine, out.SecondaryLine, out.Components.City, out.Components.State, out.Components.ZipCode
		if out.Components.ZipCodeP4 != "" {
			a.Zip += "-" + out.Components.ZipCodeP4
		}
	}
	return a, out.Deliverability, nil
}

// LetterRequest is one letter to print and mail.
type LetterRequest struct {
	To, From postal.Address
	PDF      []byte
	Options  postal.Options
	// ActionID is Lob's idempotency key and the letter's metadata, so a
	// retry after a timeout returns the same letter.
	ActionID, WorkspaceID string
}

// Letter is Lob's record of a letter it accepted.
type Letter struct {
	ID                   string `json:"id"`
	URL                  string `json:"url"`
	ExpectedDeliveryDate string `json:"expected_delivery_date"`
	SendDate             string `json:"send_date"`
	TrackingNumber       string `json:"tracking_number"`
}

// CreateLetter sends the PDF itself, never a link to it.
func (l *Lob) CreateLetter(ctx context.Context, r LetterRequest) (Letter, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	field := func(k, v string) {
		if v != "" {
			w.WriteField(k, v)
		}
	}
	address := func(prefix string, a postal.Address) {
		field(prefix+"[name]", a.Name)
		field(prefix+"[company]", a.Company)
		field(prefix+"[address_line1]", a.Line1)
		field(prefix+"[address_line2]", a.Line2)
		field(prefix+"[address_city]", a.City)
		field(prefix+"[address_state]", a.State)
		field(prefix+"[address_zip]", a.Zip)
		field(prefix+"[address_country]", "US")
	}
	address("to", r.To)
	address("from", r.From)
	field("color", strconv.FormatBool(r.Options.Color))
	field("double_sided", strconv.FormatBool(r.Options.DoubleSided))
	// Lob adds a blank first page for the address, so any PDF works without
	// leaving the envelope window clear.
	field("address_placement", "insert_blank_page")
	field("mail_type", "usps_first_class")
	field("extra_service", r.Options.ExtraService)
	field("use_type", "operational")
	field("metadata[action_id]", r.ActionID)
	field("metadata[workspace_id]", r.WorkspaceID)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="letter.pdf"`)
	h.Set("Content-Type", "application/pdf")
	part, err := w.CreatePart(h)
	if err != nil {
		return Letter{}, err
	}
	part.Write(r.PDF)
	if err := w.Close(); err != nil {
		return Letter{}, err
	}
	var out Letter
	if err := l.do(ctx, http.MethodPost, "/letters", w.FormDataContentType(), &body, map[string]string{"Idempotency-Key": r.ActionID}, &out); err != nil {
		return out, err
	}
	if out.ID == "" {
		return out, errors.New("Lob accepted the letter without an ID")
	}
	return out, nil
}

// CancelLetter stops a letter before it is printed. Lob refuses once its
// send date has passed.
func (l *Lob) CancelLetter(ctx context.Context, id string) error {
	return l.do(ctx, http.MethodDelete, "/letters/"+url.PathEscape(id), "", nil, nil, nil)
}

// LobAccounts finds each workspace's own Lob account. The key and webhook
// secret are sealed with the workspace data key; nothing returns them.
// There is no installation account to fall back on: a workspace without its
// own key cannot send letters.
type LobAccounts struct {
	DB   *pgxpool.Pool
	Keys *secrets.Keyring
	// PublicURL is where Lob sends webhooks.
	PublicURL string
	// Base and HTTP may be replaced by tests.
	Base string
	HTTP *http.Client
}

var ErrNoLob = errors.New("connect a Lob account under Connections → Mail before sending letters")

type lobCredentials struct {
	APIKey        string `json:"api_key"`
	WebhookSecret string `json:"webhook_secret"`
}

// MailSettings is a workspace's Lob account, without its secrets.
type MailSettings struct {
	ConnectorID string         `json:"connector_id"`
	Mode        string         `json:"mode"`
	Return      postal.Address `json:"return_address"`
	// From is the return address's one-line form; letters carry it.
	From      string `json:"from"`
	CapCents  int    `json:"monthly_cap_cents"`
	Health    string `json:"health"`
	LastError string `json:"last_error,omitempty"`
	// HasWebhookSecret says status updates can be received.
	HasWebhookSecret bool   `json:"has_webhook_secret"`
	WebhookURL       string `json:"webhook_url"`
}

// DefaultCapCents is a new account's monthly limit.
const DefaultCapCents = 5000

func (a *LobAccounts) webhookURL(connector string) string {
	return a.PublicURL + "/webhooks/lob/" + connector
}

// Settings is the workspace's Lob account; ok is false when there is none.
func (a *LobAccounts) Settings(ctx context.Context, workspaceID string) (MailSettings, bool, error) {
	var s MailSettings
	var settings map[string]string
	err := a.DB.QueryRow(ctx, `SELECT id::text, settings, health, coalesce(last_error, '') FROM connectors WHERE workspace_id = $1 AND kind = 'lob'`, workspaceID).
		Scan(&s.ConnectorID, &settings, &s.Health, &s.LastError)
	if db.NotFound(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	s.Mode, s.From = settings["mode"], settings["from_address"]
	s.CapCents, _ = strconv.Atoi(settings["cap_cents"])
	s.HasWebhookSecret = settings["has_webhook_secret"] == "true"
	s.Return = returnAddress(settings)
	s.WebhookURL = a.webhookURL(s.ConnectorID)
	return s, true, nil
}

// MailConnect is what a person enters. An empty APIKey or WebhookSecret
// keeps the stored one.
type MailConnect struct {
	APIKey        string         `json:"api_key"`
	WebhookSecret string         `json:"webhook_secret"`
	Return        postal.Address `json:"return_address"`
	CapCents      int            `json:"monthly_cap_cents"`
}

// Connect checks the key and return address with Lob and stores them.
func (a *LobAccounts) Connect(ctx context.Context, workspaceID string, in MailConnect) (MailSettings, error) {
	if a.Keys == nil {
		return MailSettings{}, errors.New("set VISICOMMS_MASTER_KEY before connecting a Lob account")
	}
	in.APIKey, in.WebhookSecret = strings.TrimSpace(in.APIKey), strings.TrimSpace(in.WebhookSecret)
	if in.CapCents < 0 || in.CapCents > 10_000_00 {
		return MailSettings{}, &Rejected{Reason: "the monthly limit must be between $0 and $10,000"}
	}
	ret, err := in.Return.Clean()
	if err != nil {
		return MailSettings{}, &Rejected{Reason: "return address: " + err.Error()}
	}
	current, exists, err := a.Settings(ctx, workspaceID)
	if err != nil {
		return MailSettings{}, err
	}
	creds := lobCredentials{APIKey: in.APIKey, WebhookSecret: in.WebhookSecret}
	if exists && (creds.APIKey == "" || creds.WebhookSecret == "") {
		_, stored, err := a.open(ctx, current.ConnectorID)
		if err != nil {
			return MailSettings{}, err
		}
		if creds.APIKey == "" {
			creds.APIKey = stored.APIKey
		}
		if creds.WebhookSecret == "" {
			creds.WebhookSecret = stored.WebhookSecret
		}
	}
	if !strings.HasPrefix(creds.APIKey, "test_") && !strings.HasPrefix(creds.APIKey, "live_") {
		return MailSettings{}, &Rejected{Reason: "paste a Lob secret API key: it starts with test_ or live_"}
	}
	lob := &Lob{Key: creds.APIKey, Base: a.Base, HTTP: a.HTTP}
	if err := lob.Check(ctx); err != nil {
		return MailSettings{}, fmt.Errorf("checking the Lob key: %w", err)
	}
	ret, deliverability, err := lob.Verify(ctx, ret)
	if err != nil {
		return MailSettings{}, fmt.Errorf("checking the return address: %w", err)
	}
	if deliverability == "undeliverable" {
		return MailSettings{}, &Rejected{Reason: "USPS cannot deliver to the return address"}
	}
	mode := "live"
	if lob.Test() {
		mode = "test"
	}
	settings := map[string]string{
		"mode": mode, "from_address": ret.OneLine(), "cap_cents": strconv.Itoa(in.CapCents),
		"has_webhook_secret": strconv.FormatBool(creds.WebhookSecret != ""),
		"return_name":        ret.Name, "return_company": ret.Company, "return_line1": ret.Line1, "return_line2": ret.Line2,
		"return_city": ret.City, "return_state": ret.State, "return_zip": ret.Zip,
	}
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return MailSettings{}, err
	}
	id := current.ConnectorID
	if id == "" {
		id = uuid.NewString()
	}
	err = db.Tx(ctx, a.DB, func(tx pgx.Tx) error {
		sealed, err := a.Keys.Seal(ctx, tx, workspaceID, id, plaintext)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO connectors (id, workspace_id, kind, name, settings, credentials_ciphertext, health, last_error, checked_at)
			VALUES ($1, $2, 'lob', 'Lob', $3, $4, 'ok', NULL, now())
			ON CONFLICT (id) DO UPDATE SET settings = $3, credentials_ciphertext = $4, health = 'ok', last_error = NULL, checked_at = now(), updated_at = now()`,
			id, workspaceID, settings, sealed)
		return err
	})
	if err != nil {
		return MailSettings{}, err
	}
	out, _, err := a.Settings(ctx, workspaceID)
	return out, err
}

func (a *LobAccounts) open(ctx context.Context, connectorID string) (string, lobCredentials, error) {
	var creds lobCredentials
	if a == nil || a.Keys == nil {
		return "", creds, ErrNoLob
	}
	var workspaceID string
	var sealed []byte
	err := a.DB.QueryRow(ctx, `SELECT workspace_id, credentials_ciphertext FROM connectors WHERE id::text = $1 AND kind = 'lob'`, connectorID).Scan(&workspaceID, &sealed)
	if db.NotFound(err) {
		return "", creds, ErrNoLob
	}
	if err != nil {
		return "", creds, err
	}
	plaintext, err := a.Keys.Open(ctx, a.DB, workspaceID, connectorID, sealed)
	if err != nil {
		return "", creds, err
	}
	return workspaceID, creds, json.Unmarshal(plaintext, &creds)
}

// Client is the Lob account behind a connector, and the workspace it
// belongs to.
func (a *LobAccounts) Client(ctx context.Context, connectorID string) (*Lob, string, error) {
	ws, creds, err := a.open(ctx, connectorID)
	if err != nil {
		return nil, "", err
	}
	return &Lob{Key: creds.APIKey, Base: a.Base, HTTP: a.HTTP}, ws, nil
}

// ErrLobSignature means a webhook did not come from the connector's Lob
// account.
var ErrLobSignature = errors.New("Lob webhook signature does not verify")

// VerifyWebhook checks a webhook against the connector's secret: HMAC-SHA256
// of the timestamp, ".", and the raw body, in hex. A timestamp more than
// five minutes off is refused when it can be read.
func (a *LobAccounts) VerifyWebhook(ctx context.Context, connectorID, timestamp, signature string, body []byte, now time.Time) error {
	_, creds, err := a.open(ctx, connectorID)
	if err != nil {
		return err
	}
	if creds.WebhookSecret == "" {
		return ErrLobSignature
	}
	return verifyLobSignature(creds.WebhookSecret, timestamp, signature, body, now)
}

func verifyLobSignature(secret, timestamp, signature string, body []byte, now time.Time) error {
	if timestamp == "" || signature == "" {
		return ErrLobSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(signature))) {
		return ErrLobSignature
	}
	if at, ok := lobTime(timestamp); ok && (now.Sub(at) > 5*time.Minute || at.Sub(now) > 5*time.Minute) {
		return ErrLobSignature
	}
	return nil
}

// lobTime reads the signature timestamp as Unix seconds, Unix milliseconds
// or RFC 3339, whichever it is.
func lobTime(s string) (time.Time, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// lobEvent is a Lob webhook, stored with the connector it arrived on.
type lobEvent struct {
	Connector string `json:"connector_id"`
	Event     struct {
		ID        string `json:"id"`
		EventType struct {
			ID string `json:"id"`
		} `json:"event_type"`
		Body struct {
			ID             string `json:"id"`
			URL            string `json:"url"`
			TrackingNumber string `json:"tracking_number"`
			Expected       string `json:"expected_delivery_date"`
		} `json:"body"`
	} `json:"event"`
}

// ApplyLobEvent durably receives a verified Lob webhook for a letter.
// Events about anything else are ignored.
func ApplyLobEvent(ctx context.Context, pool *pgxpool.Pool, connectorID string, body []byte) error {
	stored, err := json.Marshal(map[string]any{"connector_id": connectorID, "event": json.RawMessage(body)})
	if err != nil {
		return err
	}
	var e lobEvent
	if err := json.Unmarshal(stored, &e); err != nil {
		return err
	}
	if !strings.HasPrefix(e.Event.EventType.ID, "letter.") {
		return nil
	}
	return receiveDeliveryEvent(ctx, pool, "lob", e.Event.ID, stored)
}

// decodeLob maps a letter event to the thread event and action state it
// means. Certified mail's events are named like normal ones under
// "letter.certified.".
func decodeLob(body []byte) (deliveryEvent, error) {
	var e lobEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return deliveryEvent{}, err
	}
	out := deliveryEvent{Channel: "postal", MessageID: e.Event.Body.ID, Connector: e.Connector, Detail: map[string]any{"lob_event": e.Event.EventType.ID}}
	if e.Event.Body.TrackingNumber != "" {
		out.Detail["tracking_number"] = e.Event.Body.TrackingNumber
	}
	name := strings.TrimPrefix(e.Event.EventType.ID, "letter.")
	name = strings.TrimPrefix(name, "certified.")
	switch name {
	case "rendered_pdf":
		out.Type = "postal.rendered"
		if e.Event.Body.URL != "" {
			out.Detail["url"] = e.Event.Body.URL
		}
	case "mailed":
		out.Type = "postal.mailed"
	case "in_transit", "in_local_area", "re-routed":
		out.Type = "postal.in_transit"
		out.Detail["scan"] = name
	case "processed_for_delivery":
		out.Type, out.State = "postal.processed_for_delivery", "delivered"
	case "delivered":
		out.Type, out.State = "postal.delivered", "delivered"
	case "returned_to_sender":
		out.Type, out.State, out.Reason = "postal.returned", "failed", "returned to sender"
	case "pickup_available", "issue":
		out.Type = "postal.notice"
		out.Detail["scan"] = name
	case "failed":
		out.Type, out.State, out.Reason = "postal.failed", "failed", "Lob could not produce the letter"
	case "deleted":
		out.Type, out.State = "postal.cancelled", "cancelled"
	}
	return out, nil
}

// LetterSender creates letters; *Lob is one.
type LetterSender interface {
	CreateLetter(context.Context, LetterRequest) (Letter, error)
	Test() bool
}
