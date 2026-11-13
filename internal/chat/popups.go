package chat

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Popup is a message the widget shows a visitor who has not written yet, to
// start the conversation. It is held, not stored: showing one saves nothing
// about the visitor. If they reply, its text opens the new thread.
type Popup struct {
	ID            string   `json:"id"`
	WidgetID      string   `json:"widget_id"`
	Position      int      `json:"position"`
	Name          string   `json:"name"`
	Text          string   `json:"text"`
	SenderName    string   `json:"sender_name"`
	Trigger       string   `json:"trigger"`
	Seconds       int      `json:"seconds"`
	ScrollPercent int      `json:"scroll_percent"`
	PagePattern   string   `json:"page_pattern"`
	OpensWindow   bool     `json:"opens_window"`
	Sound         bool     `json:"sound"`
	Devices       string   `json:"devices"`
	Frequency     string   `json:"frequency"`
	OnlyIfNone    bool     `json:"only_if_none"`
	QuickReplies  []string `json:"quick_replies"`
	Active        bool     `json:"active"`
	// Shown and Chats are the last 30 days: how often it showed, and how
	// often a chat started from it.
	Shown int `json:"shown"`
	Chats int `json:"chats"`
}

const popupColumns = `p.id, p.widget_id, p.position, p.name, p.text, coalesce(p.sender_name, ''), p.trigger, p.seconds,
	p.scroll_percent, coalesce(p.page_pattern, ''), p.opens_window, p.sound, p.devices, p.frequency, p.only_if_none,
	p.quick_replies, p.active,
	coalesce((SELECT sum(shown) FROM chat_popup_stats s WHERE s.popup_id = p.id AND s.day > current_date - 30), 0)::int,
	coalesce((SELECT sum(chats) FROM chat_popup_stats s WHERE s.popup_id = p.id AND s.day > current_date - 30), 0)::int`

func scanPopup(row pgx.Row) (Popup, error) {
	var p Popup
	err := row.Scan(&p.ID, &p.WidgetID, &p.Position, &p.Name, &p.Text, &p.SenderName, &p.Trigger, &p.Seconds,
		&p.ScrollPercent, &p.PagePattern, &p.OpensWindow, &p.Sound, &p.Devices, &p.Frequency, &p.OnlyIfNone,
		&p.QuickReplies, &p.Active, &p.Shown, &p.Chats)
	if db.NotFound(err) {
		return p, ErrNotFound
	}
	return p, err
}

// Popups lists a widget's pop-ups in the order they are tried.
func Popups(ctx context.Context, q db.Querier, workspaceID, widgetID string) ([]Popup, error) {
	if !validUUID(widgetID) {
		return nil, ErrNotFound
	}
	rows, err := q.Query(ctx, `SELECT `+popupColumns+` FROM chat_popups p
		WHERE p.widget_id = $1 AND p.workspace_id = $2 ORDER BY p.position, p.created_at`, widgetID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Popup{}
	for rows.Next() {
		p, err := scanPopup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PopupSettings is what a person sets on a pop-up.
type PopupSettings struct {
	Position      int      `json:"position"`
	Name          string   `json:"name"`
	Text          string   `json:"text"`
	SenderName    string   `json:"sender_name"`
	Trigger       string   `json:"trigger"`
	Seconds       int      `json:"seconds"`
	ScrollPercent int      `json:"scroll_percent"`
	PagePattern   string   `json:"page_pattern"`
	OpensWindow   bool     `json:"opens_window"`
	Sound         bool     `json:"sound"`
	Devices       string   `json:"devices"`
	Frequency     string   `json:"frequency"`
	OnlyIfNone    bool     `json:"only_if_none"`
	QuickReplies  []string `json:"quick_replies"`
	Active        bool     `json:"active"`
}

func (s *PopupSettings) validate() error {
	s.Name, s.Text, s.SenderName, s.PagePattern = strings.TrimSpace(s.Name), strings.TrimSpace(s.Text),
		strings.TrimSpace(s.SenderName), strings.TrimSpace(s.PagePattern)
	switch {
	case s.Name == "" || utf8.RuneCountInString(s.Name) > 80:
		return &FieldError{"name", "Give the pop-up a name of up to 80 characters, for your own list."}
	case s.Text == "" || utf8.RuneCountInString(s.Text) > 500:
		return &FieldError{"text", "Write the message, up to 500 characters."}
	case utf8.RuneCountInString(s.SenderName) > 60:
		return &FieldError{"sender_name", "Keep the sender's name under 60 characters."}
	}
	switch s.Trigger {
	case "delay", "page", "exit", "scroll", "idle":
	default:
		return &FieldError{"trigger", "Choose when the pop-up shows."}
	}
	if s.Seconds < 0 || s.Seconds > 3600 {
		return &FieldError{"seconds", "Use between 0 and 3600 seconds."}
	}
	if (s.Trigger == "delay" || s.Trigger == "idle") && s.Seconds < 1 {
		return &FieldError{"seconds", "Wait at least one second."}
	}
	if s.ScrollPercent == 0 {
		s.ScrollPercent = 50
	}
	if s.ScrollPercent < 1 || s.ScrollPercent > 100 {
		return &FieldError{"scroll_percent", "Use a percentage from 1 to 100."}
	}
	if s.Trigger == "page" {
		if s.PagePattern == "" || !strings.HasPrefix(s.PagePattern, "/") || len(s.PagePattern) > 300 {
			return &FieldError{"page_pattern", "Enter a page path starting with /, such as /services or /repairs/*."}
		}
	} else {
		s.PagePattern = ""
	}
	if s.Devices == "" {
		s.Devices = "all"
	}
	if s.Devices != "all" && s.Devices != "desktop" && s.Devices != "mobile" {
		return &FieldError{"devices", "Choose all devices, desktop or mobile."}
	}
	if s.Frequency == "" {
		s.Frequency = "session"
	}
	if s.Frequency != "session" && s.Frequency != "first_visit" {
		return &FieldError{"frequency", "Choose once per visit or first visit only."}
	}
	replies, err := CleanQuickReplies(s.QuickReplies)
	if err != nil {
		return &FieldError{"quick_replies", "Use at most 4 buttons, each up to 40 characters."}
	}
	s.QuickReplies = replies
	return nil
}

// CreatePopup adds a pop-up to one of the workspace's widgets.
func CreatePopup(ctx context.Context, pool *pgxpool.Pool, workspaceID, widgetID string, s PopupSettings) (Popup, error) {
	if !validUUID(widgetID) {
		return Popup{}, ErrNotFound
	}
	if err := s.validate(); err != nil {
		return Popup{}, err
	}
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO chat_popups (workspace_id, widget_id, position, name, text, sender_name, trigger, seconds,
			scroll_percent, page_pattern, opens_window, sound, devices, frequency, only_if_none, quick_replies, active)
		SELECT workspace_id, id, $3, $4, $5, nullif($6, ''), $7, $8, $9, nullif($10, ''), $11, $12, $13, $14, $15, $16, $17
		FROM chat_widgets WHERE id = $1 AND workspace_id = $2 RETURNING id`,
		widgetID, workspaceID, s.Position, s.Name, s.Text, s.SenderName, s.Trigger, s.Seconds, s.ScrollPercent, s.PagePattern,
		s.OpensWindow, s.Sound, s.Devices, s.Frequency, s.OnlyIfNone, s.QuickReplies, s.Active).Scan(&id)
	if db.NotFound(err) {
		return Popup{}, ErrNotFound
	}
	if err != nil {
		return Popup{}, err
	}
	return getPopup(ctx, pool, workspaceID, id)
}

// UpdatePopup replaces a pop-up's settings.
func UpdatePopup(ctx context.Context, pool *pgxpool.Pool, workspaceID, id string, s PopupSettings) (Popup, error) {
	if !validUUID(id) {
		return Popup{}, ErrNotFound
	}
	if err := s.validate(); err != nil {
		return Popup{}, err
	}
	tag, err := pool.Exec(ctx, `UPDATE chat_popups SET position = $3, name = $4, text = $5, sender_name = nullif($6, ''), trigger = $7,
			seconds = $8, scroll_percent = $9, page_pattern = nullif($10, ''), opens_window = $11, sound = $12, devices = $13,
			frequency = $14, only_if_none = $15, quick_replies = $16, active = $17, updated_at = now()
		WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, s.Position, s.Name, s.Text, s.SenderName, s.Trigger, s.Seconds, s.ScrollPercent, s.PagePattern,
		s.OpensWindow, s.Sound, s.Devices, s.Frequency, s.OnlyIfNone, s.QuickReplies, s.Active)
	if err != nil {
		return Popup{}, err
	}
	if tag.RowsAffected() == 0 {
		return Popup{}, ErrNotFound
	}
	return getPopup(ctx, pool, workspaceID, id)
}

// DeletePopup removes a pop-up and its counts. Threads it started keep its
// words, which were copied into them.
func DeletePopup(ctx context.Context, pool *pgxpool.Pool, workspaceID, id string) error {
	if !validUUID(id) {
		return ErrNotFound
	}
	tag, err := pool.Exec(ctx, `DELETE FROM chat_popups WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func getPopup(ctx context.Context, q db.Querier, workspaceID, id string) (Popup, error) {
	return scanPopup(q.QueryRow(ctx, `SELECT `+popupColumns+` FROM chat_popups p WHERE p.id = $1 AND p.workspace_id = $2`, id, workspaceID))
}

// PublicPopup is what the widget needs to decide when to show a pop-up.
type PublicPopup struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Sender        string   `json:"sender"`
	Trigger       string   `json:"trigger"`
	Seconds       int      `json:"seconds"`
	ScrollPercent int      `json:"scroll_percent"`
	PagePattern   string   `json:"page_pattern,omitempty"`
	OpensWindow   bool     `json:"opens_window"`
	Sound         bool     `json:"sound"`
	Devices       string   `json:"devices"`
	Frequency     string   `json:"frequency"`
	OnlyIfNone    bool     `json:"only_if_none"`
	QuickReplies  []string `json:"quick_replies"`
}

func activePopups(ctx context.Context, q db.Querier, w Widget) ([]PublicPopup, error) {
	rows, err := q.Query(ctx, `SELECT id, text, coalesce(sender_name, $2), trigger, seconds, scroll_percent, coalesce(page_pattern, ''),
			opens_window, sound, devices, frequency, only_if_none, quick_replies
		FROM chat_popups WHERE widget_id = $1 AND active ORDER BY position, created_at`, w.ID, w.Name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PublicPopup])
}

// SeenPopup is the widget's report that a pop-up was shown.
type SeenPopup struct {
	ID      string    `json:"id"`
	ShownAt time.Time `json:"shown_at"`
}

// PopupShown counts a showing. The widget calls it before it shows the
// pop-up; the caller limits how often one visitor can.
func PopupShown(ctx context.Context, pool *pgxpool.Pool, w Widget, popupID string) error {
	if !validUUID(popupID) {
		return ErrNotFound
	}
	tag, err := pool.Exec(ctx, `INSERT INTO chat_popup_stats (popup_id, day, shown)
		SELECT id, current_date, 1 FROM chat_popups WHERE id = $1 AND widget_id = $2 AND active
		ON CONFLICT (popup_id, day) DO UPDATE SET shown = chat_popup_stats.shown + 1`, popupID, w.ID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// replayPopup writes the pop-up a new visitor answered into their thread,
// before their message, at the time it was shown: an agent reading the
// thread sees what was said to them first. Its words come from the widget's
// own pop-up, never from the browser; an unknown or deleted pop-up is
// skipped, because a conversation that starts without its opening line is
// better than a message that fails to send.
func replayPopup(ctx context.Context, tx pgx.Tx, w Widget, v Visitor, seen SeenPopup) error {
	if !validUUID(seen.ID) {
		return nil
	}
	var text, sender string
	err := tx.QueryRow(ctx, `SELECT text, coalesce(sender_name, $3) FROM chat_popups WHERE id = $1 AND widget_id = $2`,
		seen.ID, w.ID, w.Name).Scan(&text, &sender)
	if db.NotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now()
	at := seen.ShownAt
	if at.IsZero() || at.After(now) || at.Before(now.Add(-24*time.Hour)) {
		at = now
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, occurred_at)
		VALUES ($1, $2, $3, 'chat.sent', 'chat', $4, $5)`, v.WorkspaceID, v.ThreadID, v.ContactID,
		map[string]any{"text": text, "sender": sender, "popup_id": seen.ID}, at); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_popup_stats (popup_id, day, chats) VALUES ($1, current_date, 1)
		ON CONFLICT (popup_id, day) DO UPDATE SET chats = chat_popup_stats.chats + 1`, seen.ID)
	return err
}
