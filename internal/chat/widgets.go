package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// List is the workspace's widgets, oldest first.
func List(ctx context.Context, q db.Querier, workspaceID string) ([]Widget, error) {
	rows, err := q.Query(ctx, `SELECT `+widgetColumns+` FROM chat_widgets WHERE workspace_id = $1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Widget{}
	for rows.Next() {
		w, err := scanWidget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Get is one of the workspace's widgets, enabled or not.
func Get(ctx context.Context, q db.Querier, workspaceID, id string) (Widget, error) {
	if !validUUID(id) {
		return Widget{}, ErrNotFound
	}
	return scanWidget(q.QueryRow(ctx, `SELECT `+widgetColumns+` FROM chat_widgets WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
}

// Create adds a widget with the default settings.
func Create(ctx context.Context, pool *pgxpool.Pool, workspaceID, name string) (Widget, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return Widget{}, &FieldError{"name", "Give the widget a name of up to 80 characters."}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return Widget{}, err
	}
	return scanWidget(pool.QueryRow(ctx, `INSERT INTO chat_widgets (workspace_id, public_id, name) VALUES ($1, $2, $3)
		RETURNING `+widgetColumns, workspaceID, "w_"+hex.EncodeToString(b), name))
}

// Settings is everything a person can change on a widget.
type Settings struct {
	Name              string   `json:"name"`
	Color             string   `json:"color"`
	Position          string   `json:"position"`
	Greeting          string   `json:"greeting"`
	ReturningGreeting string   `json:"returning_greeting"`
	ReplyTime         string   `json:"reply_time"`
	OfflineMessage    string   `json:"offline_message"`
	AskName           string   `json:"ask_name"`
	AskEmail          string   `json:"ask_email"`
	AskPhone          string   `json:"ask_phone"`
	AllowedOrigins    []string `json:"allowed_origins"`
	QuickReplies      []string `json:"quick_replies"`
	Disabled          bool     `json:"disabled"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Settings) validate() error {
	text := func(field string, v *string, max int, required bool) error {
		*v = strings.TrimSpace(*v)
		if (required && *v == "") || utf8.RuneCountInString(*v) > max {
			return &FieldError{field, "Keep this under " + strconv.Itoa(max) + " characters" + map[bool]string{true: ", and don't leave it empty.", false: "."}[required]}
		}
		return nil
	}
	for _, f := range []struct {
		field    string
		v        *string
		max      int
		required bool
	}{
		{"name", &s.Name, 80, true},
		{"greeting", &s.Greeting, 300, true},
		{"returning_greeting", &s.ReturningGreeting, 300, true},
		{"reply_time", &s.ReplyTime, 120, false},
		{"offline_message", &s.OfflineMessage, 500, true},
	} {
		if err := text(f.field, f.v, f.max, f.required); err != nil {
			return err
		}
	}
	if !hexColor.MatchString(s.Color) {
		return &FieldError{"color", "Pick a colour like #2563eb."}
	}
	if s.Position != "right" && s.Position != "left" {
		return &FieldError{"position", "Put the widget on the right or the left."}
	}
	for field, v := range map[string]string{"ask_name": s.AskName, "ask_email": s.AskEmail, "ask_phone": s.AskPhone} {
		if v != "off" && v != "optional" && v != "required" {
			return &FieldError{field, "Choose off, optional or required."}
		}
	}
	if len(s.AllowedOrigins) > 20 {
		return &FieldError{"allowed_origins", "List at most 20 sites."}
	}
	seen := map[string]bool{}
	origins := []string{}
	for _, raw := range s.AllowedOrigins {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		o, err := NormalizeOrigin(raw)
		if err != nil {
			return &FieldError{"allowed_origins", strings.TrimSpace(raw) + " isn't a site address. Use one like https://example.com."}
		}
		if !seen[o] {
			seen[o] = true
			origins = append(origins, o)
		}
	}
	s.AllowedOrigins = origins
	replies, err := CleanQuickReplies(s.QuickReplies)
	if err != nil {
		return &FieldError{"quick_replies", "Use at most 4 buttons, each up to 40 characters."}
	}
	s.QuickReplies = replies
	return nil
}

// Update replaces a widget's settings.
func Update(ctx context.Context, pool *pgxpool.Pool, workspaceID, id string, s Settings) (Widget, error) {
	if !validUUID(id) {
		return Widget{}, ErrNotFound
	}
	if err := s.validate(); err != nil {
		return Widget{}, err
	}
	return scanWidget(pool.QueryRow(ctx, `UPDATE chat_widgets SET name = $3, color = $4, position = $5, greeting = $6,
			returning_greeting = $7, reply_time = $8, offline_message = $9, ask_name = $10, ask_email = $11, ask_phone = $12,
			allowed_origins = $13, disabled_at = CASE WHEN $14 THEN coalesce(disabled_at, now()) END, quick_replies = $15, updated_at = now()
		WHERE id = $1 AND workspace_id = $2 RETURNING `+widgetColumns,
		id, workspaceID, s.Name, s.Color, s.Position, s.Greeting, s.ReturningGreeting, s.ReplyTime, s.OfflineMessage,
		s.AskName, s.AskEmail, s.AskPhone, s.AllowedOrigins, s.Disabled, s.QuickReplies))
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(id string) bool { return uuidPattern.MatchString(id) }
