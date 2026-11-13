package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Mirror copies chat threads into Slack. Each thread is copied in event
// order from where it last stopped, so a missed wake-up or a Slack outage
// only delays a message; the sweep catches up.
type Mirror struct {
	Store  *Store
	Logger *slog.Logger
	// AppURL is the console, for the link to each chat.
	AppURL string
	// Sweep is how often every connected workspace is checked; default 1m.
	Sweep time.Duration
}

// Run copies a thread whenever wake names it, and sweeps all chats
// regularly for anything missed.
func (m *Mirror) Run(ctx context.Context, wake <-chan string) {
	every := m.Sweep
	if every == 0 {
		every = time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	m.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-wake:
			if err := m.Sync(ctx, id); err != nil {
				m.Logger.Warn("slack: copying chat", "thread", id, "err", err)
			}
		case <-tick.C:
			m.sweep(ctx)
		}
	}
}

// sweep syncs chats with something not yet copied: started after Slack was
// connected, in the last week.
func (m *Mirror) sweep(ctx context.Context) {
	rows, err := m.Store.DB.Query(ctx, `SELECT t.id FROM threads t
		JOIN connectors c ON c.workspace_id = t.workspace_id AND c.kind = 'slack'
		LEFT JOIN slack_threads s ON s.thread_id = t.id
		WHERE t.channel = 'chat' AND t.created_at >= c.created_at AND t.updated_at > now() - interval '7 days'
		AND EXISTS (SELECT 1 FROM events e WHERE e.thread_id = t.id AND e.id > coalesce(s.last_event_id, 0)
			AND e.type IN ('chat.received', 'chat.sent', 'thread.resolved', 'thread.reopened'))
		LIMIT 200`)
	if err != nil {
		m.Logger.Warn("slack: sweep", "err", err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		m.Logger.Warn("slack: sweep", "err", err)
		return
	}
	for _, id := range ids {
		if err := m.Sync(ctx, id); err != nil {
			m.Logger.Warn("slack: copying chat", "thread", id, "err", err)
		}
	}
}

type mirrorEvent struct {
	ID      int64
	Type    string
	Payload struct {
		Text     string `json:"text"`
		Sender   string `json:"sender"`
		PopupID  string `json:"popup_id"`
		Via      string `json:"via"`
		Page     string `json:"page"`
		Reason   string `json:"reason"`
		Approved string `json:"approved_by"`
	}
	Attribution map[string]struct {
		Value string `json:"value"`
	}
	Actor string
}

// Sync copies whatever a chat has that Slack does not yet. It holds the
// thread's row while posting, so two callers never post the same event.
func (m *Mirror) Sync(ctx context.Context, threadID string) error {
	if !validUUID(threadID) {
		return nil
	}
	var postErr error
	var connectorID string
	err := db.Tx(ctx, m.Store.DB, func(tx pgx.Tx) error {
		var workspaceID, channel string
		var created time.Time
		err := tx.QueryRow(ctx, `SELECT workspace_id, channel, created_at FROM threads WHERE id = $1`, threadID).Scan(&workspaceID, &channel, &created)
		if db.NotFound(err) || (err == nil && channel != "chat") {
			return nil
		}
		if err != nil {
			return err
		}
		conn, err := m.Store.Load(ctx, tx, workspaceID)
		if errors.Is(err, ErrNotConnected) {
			return nil
		}
		if err != nil {
			return err
		}
		connectorID = conn.ID
		var connected time.Time
		if err := tx.QueryRow(ctx, `SELECT created_at FROM connectors WHERE id = $1`, conn.ID).Scan(&connected); err != nil {
			return err
		}
		if created.Before(connected) {
			return nil // started before Slack was connected
		}
		if _, err := tx.Exec(ctx, `INSERT INTO slack_threads (thread_id, connector_id, channel_id) VALUES ($1, $2, $3)
			ON CONFLICT (thread_id) DO NOTHING`, threadID, conn.ID, conn.ChannelID); err != nil {
			return err
		}
		var ts *string
		var last int64
		var channelID, mirroredBy string
		if err := tx.QueryRow(ctx, `SELECT ts, last_event_id, channel_id, connector_id FROM slack_threads WHERE thread_id = $1 FOR UPDATE`,
			threadID).Scan(&ts, &last, &channelID, &mirroredBy); err != nil {
			return err
		}
		if mirroredBy != conn.ID {
			return nil // copied under an earlier connection; leave it there
		}
		rows, err := tx.Query(ctx, `SELECT e.id, e.type, e.payload, coalesce(e.attribution, '{}'), coalesce(p.name, '')
			FROM events e LEFT JOIN principals p ON p.id = e.actor_id
			WHERE e.thread_id = $1 AND e.id > $2 AND e.type IN ('chat.received', 'chat.sent', 'thread.resolved', 'thread.reopened')
			ORDER BY e.id LIMIT 50`, threadID, last)
		if err != nil {
			return err
		}
		var events []mirrorEvent
		for rows.Next() {
			var ev mirrorEvent
			var payload, attribution []byte
			if err := rows.Scan(&ev.ID, &ev.Type, &payload, &attribution, &ev.Actor); err != nil {
				rows.Close()
				return err
			}
			_ = json.Unmarshal(payload, &ev.Payload)
			_ = json.Unmarshal(attribution, &ev.Attribution)
			events = append(events, ev)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}

		cl := conn.Client(m.Store.API)
		advance := func(id int64) error {
			last = id
			_, err := tx.Exec(ctx, `UPDATE slack_threads SET last_event_id = $2 WHERE thread_id = $1`, threadID, id)
			return err
		}
		// The Slack thread starts at the visitor's first message; a pop-up
		// line before it is posted into the thread after.
		if ts == nil {
			first := -1
			for i, ev := range events {
				if ev.Type == "chat.received" {
					first = i
					break
				}
			}
			if first < 0 {
				return nil
			}
			root, err := m.rootText(ctx, tx, conn, threadID, events[first])
			if err != nil {
				return err
			}
			newTS, err := cl.Post(ctx, channelID, root, "")
			if err != nil {
				postErr = err
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE slack_threads SET ts = $2 WHERE thread_id = $1`, threadID, newTS); err != nil {
				return err
			}
			ts = &newTS
			// The root carries the first message. Anything before it is
			// the pop-up the visitor answered: posted once, best effort,
			// since reposting it later would put it out of order.
			if err := advance(events[first].ID); err != nil {
				return err
			}
			for _, ev := range events[:first] {
				if text := m.replyText(conn, ev); text != "" {
					if _, err := cl.Post(ctx, channelID, text, newTS); err != nil {
						postErr = err
						return nil
					}
				}
			}
			events = events[first+1:]
		}
		for _, ev := range events {
			if text := m.replyText(conn, ev); text != "" {
				if _, err := cl.Post(ctx, channelID, text, *ts); err != nil {
					postErr = err
					return nil
				}
			}
			if err := advance(ev.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if connectorID != "" {
		SetHealth(ctx, m.Store.DB, connectorID, postErr)
	}
	return postErr
}

func (m *Mirror) rootText(ctx context.Context, tx pgx.Tx, conn *Connection, threadID string, first mirrorEvent) (string, error) {
	var name, email, phone string
	if err := tx.QueryRow(ctx, `SELECT coalesce(v.name, ''), coalesce(v.email, ''), coalesce(v.phone, '')
		FROM chat_visitors v WHERE v.thread_id = $1`, threadID).Scan(&name, &email, &phone); err != nil && !db.NotFound(err) {
		return "", err
	}
	var lines []string
	if conn.Mention != "" && conn.MentionOn != MentionNever {
		lines = append(lines, conn.Mention)
	}
	lines = append(lines, "*New website chat* from "+Escape(visitorLabel(name, email, phone)))
	var about []string
	if first.Payload.Page != "" {
		about = append(about, "on "+Escape(first.Payload.Page))
	}
	if src := first.Attribution["source"].Value; src != "" {
		s := src
		if c := first.Attribution["campaign"].Value; c != "" {
			s += " / " + c
		}
		about = append(about, "from "+Escape(s))
	}
	if len(about) > 0 {
		lines = append(lines, strings.Join(about, ", "))
	}
	if m.AppURL != "" {
		lines = append(lines, fmt.Sprintf("<%s/threads/%s|Open in VisiComms>", m.AppURL, threadID))
	}
	lines = append(lines, "", "*Visitor:* "+Escape(first.Payload.Text))
	return strings.Join(lines, "\n"), nil
}

// replyText is how one event reads in the Slack thread; "" posts nothing.
func (m *Mirror) replyText(conn *Connection, ev mirrorEvent) string {
	switch ev.Type {
	case "chat.received":
		text := "*Visitor:* " + Escape(ev.Payload.Text)
		if conn.Mention != "" && conn.MentionOn == MentionEvery {
			text = conn.Mention + " " + text
		}
		return text
	case "chat.sent":
		if ev.Payload.Via == "slack" {
			return "" // already in Slack, typed by the person
		}
		if ev.Payload.PopupID != "" {
			return "_Pop-up message they answered, shown as " + Escape(ev.Payload.Sender) + ":_ " + Escape(ev.Payload.Text)
		}
		who := ev.Actor
		if who == "" {
			who = ev.Payload.Sender
		}
		return "*" + Escape(who) + ":* " + Escape(ev.Payload.Text)
	case "thread.resolved":
		if ev.Actor != "" {
			return "_Marked done by " + Escape(ev.Actor) + "_"
		}
		return "_Marked done_"
	case "thread.reopened":
		return "_Reopened_"
	}
	return ""
}

func visitorLabel(name, email, phone string) string {
	switch {
	case name != "" && email != "":
		return name + " (" + email + ")"
	case name != "" && phone != "":
		return name + " (" + phone + ")"
	case name != "":
		return name
	case email != "":
		return email
	case phone != "":
		return phone
	}
	return "a visitor"
}
