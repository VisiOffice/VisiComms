package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// ReplyFunc sends a person's reply to a chat, through the same path as the
// console. It returns an error in words the person can act on.
type ReplyFunc func(ctx context.Context, workspaceID, principalID, threadID, text string) error

// Events handles Slack's Events API callbacks for one installation.
type Events struct {
	Store  *Store
	Logger *slog.Logger
	Reply  ReplyFunc
	// Now may be replaced by tests.
	Now func() time.Time
}

type callback struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	TeamID    string `json:"team_id"`
	EventID   string `json:"event_id"`
	Event     struct {
		Type     string `json:"type"`
		Subtype  string `json:"subtype"`
		BotID    string `json:"bot_id"`
		User     string `json:"user"`
		Text     string `json:"text"`
		Channel  string `json:"channel"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"event"`
}

// Handle verifies a callback for a connection and acts on it. It returns
// the url_verification challenge when Slack asks for one. ErrSignature and
// ErrNotConnected mean the request is refused; nothing is stored.
func (e *Events) Handle(ctx context.Context, connectorID, timestamp, signature string, body []byte) (string, error) {
	conn, err := e.Store.ByID(ctx, e.Store.DB, connectorID)
	if err != nil {
		return "", err
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	if err := Verify(conn.SigningSecret(), timestamp, signature, body, now()); err != nil {
		return "", err
	}
	var cb callback
	if err := json.Unmarshal(body, &cb); err != nil {
		return "", err
	}
	if cb.Type == "url_verification" {
		return cb.Challenge, nil
	}
	if cb.Type != "event_callback" || cb.EventID == "" {
		return "", nil
	}
	if conn.TeamID != "" && cb.TeamID != "" && cb.TeamID != conn.TeamID {
		return "", nil
	}
	ev := cb.Event
	// Only a person's new message in a chat's Slack thread: not the bot,
	// not edits, joins or other subtypes, not the thread's first message.
	if ev.Type != "message" || ev.Subtype != "" || ev.BotID != "" || ev.User == "" || ev.User == conn.BotUserID ||
		ev.Channel != conn.ChannelID || ev.ThreadTS == "" || ev.ThreadTS == ev.TS {
		return "", nil
	}
	// Each event once: Slack retries anything not acknowledged in 3 seconds.
	tag, err := e.Store.DB.Exec(ctx, `INSERT INTO provider_events (provider, event_id, message_id, payload)
		VALUES ('slack', $1, $2, $3) ON CONFLICT DO NOTHING`, conn.ID+":"+cb.EventID, ev.ThreadTS, body)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", nil
	}
	var threadID string
	err = e.Store.DB.QueryRow(ctx, `SELECT thread_id FROM slack_threads WHERE connector_id = $1 AND channel_id = $2 AND ts = $3`,
		conn.ID, ev.Channel, ev.ThreadTS).Scan(&threadID)
	if db.NotFound(err) {
		return "", nil // a thread VisiComms did not start
	}
	if err != nil {
		return "", err
	}
	cl := conn.Client(e.Store.API)
	notice := func(text string) {
		if _, err := cl.Post(ctx, ev.Channel, text, ev.ThreadTS); err != nil {
			e.Logger.Warn("slack: posting a notice", "thread", threadID, "err", err)
		}
	}
	principalID, err := e.member(ctx, conn, cl, ev.User)
	if err != nil {
		return "", err
	}
	if principalID == "" {
		notice("<@" + ev.User + "> Not sent to the visitor: your Slack account is not linked to a VisiComms member. " +
			"Ask an admin to link it under Connections, Slack, or make sure your Slack email is the one you sign in to VisiComms with.")
		return "", nil
	}
	text := strings.TrimSpace(Plain(ev.Text, nil))
	if text == "" {
		return "", nil
	}
	if err := e.Reply(ctx, conn.WorkspaceID, principalID, threadID, text); err != nil {
		notice("<@" + ev.User + "> Not sent to the visitor: " + Escape(err.Error()))
		return "", nil
	}
	return "", nil
}

// member finds the workspace person a Slack user replies as: by the linked
// id, or the first time by matching email, which is then remembered.
func (e *Events) member(ctx context.Context, conn *Connection, cl *Client, slackUser string) (string, error) {
	var id string
	err := e.Store.DB.QueryRow(ctx, `SELECT id FROM principals WHERE workspace_id = $1 AND slack_user_id = $2
		AND kind = 'human' AND revoked_at IS NULL`, conn.WorkspaceID, slackUser).Scan(&id)
	if err == nil || !db.NotFound(err) {
		return id, err
	}
	u, err := cl.UserInfo(ctx, slackUser)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			e.Logger.Warn("slack: looking up a user", "user", slackUser, "err", err)
			return "", nil
		}
		return "", err
	}
	if u.Email == "" {
		return "", nil
	}
	err = e.Store.DB.QueryRow(ctx, `UPDATE principals SET slack_user_id = $3
		WHERE id = (SELECT id FROM principals WHERE workspace_id = $1 AND kind = 'human' AND revoked_at IS NULL
			AND slack_user_id IS NULL AND lower(email) = lower($2) LIMIT 1)
		RETURNING id`, conn.WorkspaceID, u.Email, slackUser).Scan(&id)
	if db.NotFound(err) {
		return "", nil
	}
	return id, err
}
