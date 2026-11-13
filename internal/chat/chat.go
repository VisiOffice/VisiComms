// Package chat is web chat: a widget on a customer's own site whose
// conversations are threads on the chat channel, in the same inbox and under
// the same rules as email and SMS.
//
// Two rules shape everything here. Nothing is stored until a visitor sends a
// message, so opening the widget leaves no contact or thread behind. And the
// visitor's token is their only key: every request for a conversation proves
// the token, and the workspace always comes from the widget, never from
// anything the browser says.
package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/VisiOffice/VisiComms/internal/untrusted"
)

// MaxMessage is the longest message, in characters, either side can send.
const MaxMessage = 4000

var (
	// ErrNotFound is an unknown or disabled widget, or a token that does not
	// belong to it. All look the same from outside.
	ErrNotFound = errors.New("not found")
	ErrEmpty    = errors.New("write a message first")
	ErrTooLong  = errors.New("messages can be at most 4000 characters")
	// ErrInvalid is a pre-chat form problem; the message says which field.
	ErrInvalid = errors.New("invalid")
	// ErrQuickReplies is a bad set of quick-reply buttons.
	ErrQuickReplies = errors.New("quick replies: at most 4 buttons of up to 40 characters each")
)

// CleanQuickReplies trims and checks quick-reply buttons, dropping blanks
// and repeats.
func CleanQuickReplies(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		if utf8.RuneCountInString(r) > 40 {
			return nil, ErrQuickReplies
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) > 4 {
		return nil, ErrQuickReplies
	}
	return out, nil
}

// FieldError is a pre-chat form field the visitor must fix.
type FieldError struct{ Field, Message string }

func (e *FieldError) Error() string { return e.Message }
func (e *FieldError) Unwrap() error { return ErrInvalid }

// Widget is one chat widget and its settings.
type Widget struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"-"`
	PublicID          string    `json:"public_id"`
	Name              string    `json:"name"`
	Color             string    `json:"color"`
	Position          string    `json:"position"`
	Greeting          string    `json:"greeting"`
	ReturningGreeting string    `json:"returning_greeting"`
	ReplyTime         string    `json:"reply_time"`
	OfflineMessage    string    `json:"offline_message"`
	AskName           string    `json:"ask_name"`
	AskEmail          string    `json:"ask_email"`
	AskPhone          string    `json:"ask_phone"`
	AllowedOrigins    []string  `json:"allowed_origins"`
	QuickReplies      []string  `json:"quick_replies"`
	Disabled          bool      `json:"disabled"`
	CreatedAt         time.Time `json:"created_at"`
}

const widgetColumns = `id, workspace_id, public_id, name, color, position, greeting, returning_greeting, reply_time,
	offline_message, ask_name, ask_email, ask_phone, allowed_origins, quick_replies, disabled_at IS NOT NULL, created_at`

func scanWidget(row pgx.Row) (Widget, error) {
	var w Widget
	err := row.Scan(&w.ID, &w.WorkspaceID, &w.PublicID, &w.Name, &w.Color, &w.Position, &w.Greeting, &w.ReturningGreeting,
		&w.ReplyTime, &w.OfflineMessage, &w.AskName, &w.AskEmail, &w.AskPhone, &w.AllowedOrigins, &w.QuickReplies, &w.Disabled, &w.CreatedAt)
	if db.NotFound(err) {
		return w, ErrNotFound
	}
	return w, err
}

// Load finds an enabled widget by the id in its script tag.
func Load(ctx context.Context, q db.Querier, publicID string) (Widget, error) {
	if !validPublicID(publicID) {
		return Widget{}, ErrNotFound
	}
	return scanWidget(q.QueryRow(ctx, `SELECT `+widgetColumns+` FROM chat_widgets WHERE public_id = $1 AND disabled_at IS NULL`, publicID))
}

func validPublicID(id string) bool {
	if len(id) != 26 || !strings.HasPrefix(id, "w_") {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}

// OriginAllowed says whether a page on origin may use the widget. An empty
// list allows any site. This stops another site embedding the widget by
// accident; it is not security, because any client can send any Origin.
func (w Widget) OriginAllowed(origin string) bool {
	if len(w.AllowedOrigins) == 0 {
		return true
	}
	o, err := NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	for _, allowed := range w.AllowedOrigins {
		if allowed == o {
			return true
		}
	}
	return false
}

// NormalizeOrigin reduces an origin or URL to scheme://host[:port], lower case.
func NormalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", errors.New("not a site address")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// Config is what the widget script needs to draw itself. It is public.
type Config struct {
	Name              string `json:"name"`
	Color             string `json:"color"`
	Position          string `json:"position"`
	Greeting          string `json:"greeting"`
	ReturningGreeting string `json:"returning_greeting"`
	ReplyTime         string `json:"reply_time"`
	OfflineMessage    string `json:"offline_message"`
	// Staffed is whether the workspace's business hours are on now.
	Staffed bool `json:"staffed"`
	Form    struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"form"`
	MaxMessage   int      `json:"max_message"`
	QuickReplies []string `json:"quick_replies"`
	// Popups are the active pop-up messages, in order.
	Popups []PublicPopup `json:"popups"`
}

// PublicConfig is the widget's settings and whether someone is expected to
// be around, from the workspace's business hours.
func PublicConfig(ctx context.Context, q db.Querier, w Widget, now time.Time) (Config, error) {
	var tz string
	var hours []byte
	if err := q.QueryRow(ctx, `SELECT timezone, business_hours FROM workspaces WHERE id = $1`, w.WorkspaceID).Scan(&tz, &hours); err != nil {
		return Config{}, err
	}
	c := Config{
		Name: w.Name, Color: w.Color, Position: w.Position, Greeting: w.Greeting, ReturningGreeting: w.ReturningGreeting,
		ReplyTime: w.ReplyTime, OfflineMessage: w.OfflineMessage, Staffed: Staffed(hours, tz, now), MaxMessage: MaxMessage,
	}
	c.Form.Name, c.Form.Email, c.Form.Phone = w.AskName, w.AskEmail, w.AskPhone
	c.QuickReplies = w.QuickReplies
	popups, err := activePopups(ctx, q, w)
	if err != nil {
		return c, err
	}
	c.Popups = popups
	return c, nil
}

// Staffed reads business hours as {"mon": [["08:00","17:00"]], ...} in the
// workspace's time zone. No hours at all means always staffed; a day missing
// from hours that are set means closed that day.
func Staffed(hours []byte, tz string, now time.Time) bool {
	var days map[string][][2]string
	if err := json.Unmarshal(hours, &days); err != nil || len(days) == 0 {
		return true
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	day := strings.ToLower(local.Weekday().String()[:3])
	clock := local.Format("15:04")
	for _, span := range days[day] {
		if clock >= span[0] && clock < span[1] {
			return true
		}
	}
	return false
}

// Visitor is someone who has sent at least one message.
type Visitor struct {
	ID, WorkspaceID, WidgetID, ContactID, ThreadID string
}

// HashToken is how a token is stored and looked up.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Authenticate finds the visitor a token belongs to, on this widget only.
func Authenticate(ctx context.Context, q db.Querier, w Widget, token string) (Visitor, error) {
	if len(token) < 40 || len(token) > 64 {
		return Visitor{}, ErrNotFound
	}
	v := Visitor{WidgetID: w.ID}
	err := q.QueryRow(ctx, `SELECT id, workspace_id, contact_id, thread_id FROM chat_visitors
		WHERE token_sha256 = $1 AND widget_id = $2`, HashToken(token), w.ID).Scan(&v.ID, &v.WorkspaceID, &v.ContactID, &v.ThreadID)
	if db.NotFound(err) {
		return v, ErrNotFound
	}
	return v, err
}

// Attribution is where the visitor came from, as the widget saw it on the
// page they landed on.
type Attribution struct {
	Source      string `json:"utm_source"`
	Medium      string `json:"utm_medium"`
	Campaign    string `json:"utm_campaign"`
	Term        string `json:"utm_term"`
	Content     string `json:"utm_content"`
	LandingPage string `json:"landing_page"`
	Referrer    string `json:"referrer"`
	GCLID       string `json:"gclid"`
	GBRAID      string `json:"gbraid"`
	WBRAID      string `json:"wbraid"`
	FBCLID      string `json:"fbclid"`
	MSCLKID     string `json:"msclkid"`
}

// record is the attribution as stored on the first event: each value with
// how it was obtained. A click id with no UTM source names the source.
func (a Attribution) record() map[string]any {
	out := map[string]any{}
	put := func(key, value, how string) {
		value = clip(strings.TrimSpace(value), 500)
		if value != "" {
			out[key] = map[string]string{"value": value, "how": how}
		}
	}
	source, medium, how := a.Source, a.Medium, "utm"
	if source == "" {
		switch {
		case a.GCLID != "" || a.GBRAID != "" || a.WBRAID != "":
			source, how = "google_ads", "click_id"
		case a.MSCLKID != "":
			source, how = "microsoft_ads", "click_id"
		case a.FBCLID != "":
			source, how = "facebook_ads", "click_id"
		}
		if how == "click_id" && medium == "" {
			medium = "cpc"
		}
	}
	if source == "" && a.Referrer != "" {
		if u, err := url.Parse(a.Referrer); err == nil && u.Host != "" {
			source, how = strings.TrimPrefix(strings.ToLower(u.Host), "www."), "referrer"
		}
	}
	put("source", source, how)
	put("medium", medium, how)
	put("campaign", a.Campaign, "utm")
	put("keyword", a.Term, "utm")
	put("content", a.Content, "utm")
	put("landing_page", a.LandingPage, "widget")
	put("referrer", a.Referrer, "widget")
	put("gclid", a.GCLID, "url")
	put("gbraid", a.GBRAID, "url")
	put("wbraid", a.WBRAID, "url")
	put("fbclid", a.FBCLID, "url")
	put("msclkid", a.MSCLKID, "url")
	return out
}

// Inbound is one message from a visitor, with the pre-chat form and page
// details the widget sends alongside it.
type Inbound struct {
	Text string `json:"text"`
	// Popup is the pop-up message the visitor was shown before writing, if
	// any. Only its id and when it showed come from the browser; its words
	// are read from the widget's own pop-ups.
	Popup       *SeenPopup  `json:"popup"`
	Name        string      `json:"name"`
	Email       string      `json:"email"`
	Phone       string      `json:"phone"`
	Page        string      `json:"page"`
	Attribution Attribution `json:"attribution"`
}

// Message is one line of the conversation as the visitor sees it.
type Message struct {
	ID           int64     `json:"id"`
	From         string    `json:"from"` // "visitor" or "team"
	Name         string    `json:"name,omitempty"`
	Text         string    `json:"text"`
	At           time.Time `json:"at"`
	QuickReplies []string  `json:"quick_replies,omitempty"`
}

// Received is what storing a message did. Token is set only when this
// message created the visitor; it is never shown again.
type Received struct {
	Token   string  `json:"token,omitempty"`
	Message Message `json:"message"`
	Visitor Visitor `json:"-"`
	New     bool    `json:"-"`
}

// Receive stores a visitor's message. With no token it is the visitor's first
// message, and only now are the contact, thread and visitor created.
func Receive(ctx context.Context, pool *pgxpool.Pool, w Widget, token string, in Inbound) (Received, error) {
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		return Received{}, ErrEmpty
	}
	if utf8.RuneCountInString(in.Text) > MaxMessage {
		return Received{}, ErrTooLong
	}
	in.Page = clip(strings.TrimSpace(in.Page), 2000)
	var res Received
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		visitor := map[string]string{}
		var attribution map[string]any
		if token == "" {
			details, err := form(w, in)
			if err != nil {
				return err
			}
			visitor = details
			res.Token, res.New = newToken(), true
			res.Visitor = Visitor{WorkspaceID: w.WorkspaceID, WidgetID: w.ID}
			name := details["name"]
			if name == "" {
				name = "Website visitor"
			}
			if err := tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`,
				w.WorkspaceID, name).Scan(&res.Visitor.ContactID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject) VALUES ($1, $2, 'chat', $3) RETURNING id`,
				w.WorkspaceID, res.Visitor.ContactID, subject(w, in.Page)).Scan(&res.Visitor.ThreadID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO chat_visitors (workspace_id, widget_id, token_sha256, contact_id, thread_id, name, email, phone)
				VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, '')) RETURNING id`,
				w.WorkspaceID, w.ID, HashToken(res.Token), res.Visitor.ContactID, res.Visitor.ThreadID,
				details["name"], details["email"], details["phone"]).Scan(&res.Visitor.ID); err != nil {
				return err
			}
			attribution = in.Attribution.record()
			if len(attribution) == 0 {
				attribution = nil
			}
			if in.Popup != nil {
				if err := replayPopup(ctx, tx, w, res.Visitor, *in.Popup); err != nil {
					return err
				}
			}
		} else {
			v := Visitor{WidgetID: w.ID}
			err := tx.QueryRow(ctx, `SELECT id, workspace_id, contact_id, thread_id FROM chat_visitors
				WHERE token_sha256 = $1 AND widget_id = $2 FOR UPDATE`, HashToken(token), w.ID).
				Scan(&v.ID, &v.WorkspaceID, &v.ContactID, &v.ThreadID)
			if db.NotFound(err) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			res.Visitor = v
			// Details the site passes later (setUser) fill in what is missing;
			// they never replace what the visitor already gave.
			later, err := form(Widget{AskName: "optional", AskEmail: "optional", AskPhone: "optional"}, in)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE chat_visitors SET last_seen_at = now(),
					name = coalesce(name, nullif($2, '')), email = coalesce(email, nullif($3, '')), phone = coalesce(phone, nullif($4, ''))
				WHERE id = $1`, v.ID, later["name"], later["email"], later["phone"]); err != nil {
				return err
			}
			if later["name"] != "" {
				if _, err := tx.Exec(ctx, `UPDATE contacts SET display_name = $2, updated_at = now()
					WHERE id = $1 AND display_name = 'Website visitor'`, v.ContactID, later["name"]); err != nil {
					return err
				}
			}
			if len(later) > 0 {
				visitor = later
			}
			if _, err := tx.Exec(ctx, `UPDATE threads SET state = 'open', version = version + 1, updated_at = now() WHERE id = $1`, v.ThreadID); err != nil {
				return err
			}
		}
		payload := map[string]any{"text": in.Text, "page": in.Page, "classification": map[string]any{"kind": "message"}}
		if len(visitor) > 0 {
			payload["visitor"] = visitor
		}
		suspicious := untrusted.SuspiciousReasons(in.Text, "")
		res.Message = Message{From: "visitor", Text: in.Text}
		if err := tx.QueryRow(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, untrusted, suspected_injection, attribution)
			VALUES ($1, $2, $3, 'chat.received', 'chat', $4, true, $5, $6) RETURNING id, occurred_at`,
			res.Visitor.WorkspaceID, res.Visitor.ThreadID, res.Visitor.ContactID, payload, len(suspicious) > 0, attribution).
			Scan(&res.Message.ID, &res.Message.At); err != nil {
			return err
		}
		return Notify(ctx, tx, res.Visitor.ThreadID)
	})
	return res, err
}

// form checks the pre-chat form against the widget's settings and returns
// what the visitor gave, cleaned up. Nothing here is verified.
func form(w Widget, in Inbound) (map[string]string, error) {
	out := map[string]string{}
	name := clip(strings.TrimSpace(in.Name), 200)
	if name == "" && w.AskName == "required" {
		return nil, &FieldError{"name", "Enter your name."}
	}
	if name != "" {
		out["name"] = name
	}
	email := strings.TrimSpace(in.Email)
	if email == "" && w.AskEmail == "required" {
		return nil, &FieldError{"email", "Enter your email address."}
	}
	if email != "" {
		a, err := mail.ParseAddress(email)
		if err != nil || len(a.Address) > 254 || !strings.Contains(a.Address, ".") {
			return nil, &FieldError{"email", "Enter a valid email address."}
		}
		out["email"] = strings.ToLower(a.Address)
	}
	ph := strings.TrimSpace(in.Phone)
	if ph == "" && w.AskPhone == "required" {
		return nil, &FieldError{"phone", "Enter your phone number."}
	}
	if ph != "" {
		n, err := phone.Normalize(nanp(ph))
		if err != nil {
			return nil, &FieldError{"phone", "Enter a phone number with its area code, or + and the country code."}
		}
		out["phone"] = n
	}
	return out, nil
}

// nanp reads a 10-digit number, or 11 digits starting with 1, as a North
// American one. Visitors type numbers the way they say them; anything else
// must start with + and its country code.
func nanp(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), "+") {
		return s
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	switch {
	case len(digits) == 10:
		return "+1" + digits
	case len(digits) == 11 && digits[0] == '1':
		return "+" + digits
	}
	return s
}

func subject(w Widget, page string) string {
	if u, err := url.Parse(page); err == nil && u.Host != "" {
		return "Chat on " + strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	}
	return "Chat on " + w.Name
}

// Messages is the conversation as the visitor sees it, after the given id.
func Messages(ctx context.Context, q db.Querier, v Visitor, after int64) ([]Message, error) {
	rows, err := q.Query(ctx, `SELECT id, CASE WHEN type = 'chat.received' THEN 'visitor' ELSE 'team' END,
			coalesce(payload->>'sender', ''), coalesce(payload->>'text', ''), occurred_at,
			coalesce(ARRAY(SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(payload->'quick_replies') = 'array'
				THEN payload->'quick_replies' ELSE '[]' END)), '{}')
		FROM events WHERE thread_id = $1 AND type IN ('chat.received', 'chat.sent') AND id > $2
		ORDER BY id LIMIT 200`, v.ThreadID, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// Notify tells listening widgets that thread has something new. It is sent
// when the transaction commits, so a listener never reads ahead of the data.
func Notify(ctx context.Context, tx pgx.Tx, threadID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify('chat', $1)`, threadID)
	return err
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
