package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/sending"
)

// settle is how long an event waits before an instant notification, so the
// transcript that follows a voicemail can go with it.
const settle = 20 * time.Second

// digestHour is when, in the workspace's time zone, the daily summary goes.
const digestHour = 8

// Notifier sends notifications. Email needs a sender and From address; text
// needs the workspace's SignalWire project and one of its SMS numbers.
// Either may be missing, and that channel is then skipped.
type Notifier struct {
	DB         *pgxpool.Pool
	Logger     *slog.Logger
	Email      sending.Sender
	From       string
	SignalWire *sending.SignalWireProjects
	AppURL     string
}

// Channels says which channels this server can deliver.
func (n *Notifier) Channels() map[string]bool {
	return map[string]bool{"email": n != nil && n.Email != nil && n.From != "", "sms": n != nil && n.SignalWire != nil}
}

// item is an event worth telling someone about.
type item struct {
	EventID     int64
	WorkspaceID string
	Workspace   string
	ThreadID    string
	Type        string
	Contact     string
	Payload     map[string]any
	At          time.Time
}

func (it item) category() string { return categoryOf(it.Type) }

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// headline is one line about the event.
func (it item) headline() string {
	who := it.Contact
	switch it.Type {
	case "voice.voicemail":
		secs, _ := it.Payload["duration_seconds"].(float64)
		return fmt.Sprintf("Voicemail from %s (%d:%02d)", who, int(secs)/60, int(secs)%60)
	case "call.missed":
		return "Missed call from " + who
	case "call.ai_handled":
		if _, ok := it.Payload["message"]; ok {
			return "Message from " + who + ", taken by the AI"
		}
		return "The AI answered a call from " + who
	case "call.answered":
		return "Call from " + who + " answered"
	case "sms.received":
		return "Text from " + who
	case "chat.received":
		return "Website chat from " + who
	case "email.received":
		return "Email from " + who
	case "fax.received":
		return "Fax from " + who
	case "proposal.held":
		return "A reply to " + who + " is waiting for approval"
	case "thread.escalated":
		return "An agent needs you on " + who + "’s thread"
	}
	return "Could not deliver to " + who
}

// detail is the content worth reading without opening the console. Contact
// words are passed to a person, never to a model.
func (it item) detail() string {
	switch it.Type {
	case "sms.received", "chat.received":
		return str(it.Payload, "text")
	case "email.received":
		return str(it.Payload, "subject")
	case "voice.voicemail":
		return str(it.Payload, "transcript")
	case "call.ai_handled":
		return str(it.Payload, "summary")
	case "thread.escalated":
		return str(it.Payload, "note")
	}
	return ""
}

func (n *Notifier) link(it item) string {
	return strings.TrimRight(n.AppURL, "/") + "/threads/" + it.ThreadID
}

const itemsSQL = `SELECT e.id, e.workspace_id, w.name, e.thread_id, e.type, coalesce(c.display_name, ''), e.payload, e.occurred_at,
		(SELECT t.payload->>'text' FROM events t WHERE t.thread_id = e.thread_id AND t.type = 'voice.transcript'
			AND t.payload->>'call_id' = e.payload->>'call_id' LIMIT 1)
	FROM events e JOIN workspaces w ON w.id = e.workspace_id LEFT JOIN contacts c ON c.id = e.contact_id`

func scanItems(rows pgx.Rows) ([]item, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		var raw []byte
		var transcript *string
		if err := r.Scan(&it.EventID, &it.WorkspaceID, &it.Workspace, &it.ThreadID, &it.Type, &it.Contact, &raw, &it.At, &transcript); err != nil {
			return it, err
		}
		_ = json.Unmarshal(raw, &it.Payload)
		if it.Payload == nil {
			it.Payload = map[string]any{}
		}
		if transcript != nil {
			it.Payload["transcript"] = *transcript
		}
		if it.Contact == "" {
			it.Contact = "a contact"
		}
		return it, nil
	})
}

// recipient is a person with an instant setting for a category.
type recipient struct {
	ID, Email, Phone string
	Channels         []string
}

// RunInstant sends instant notifications for events since the last run.
// Each (person, event, channel) is recorded before it is sent, so a crash
// can lose one notification but never repeats one.
func (n *Notifier) RunInstant(ctx context.Context) error {
	// Without a cursor, start from now: never a flood of old events.
	if _, err := n.DB.Exec(ctx, `INSERT INTO notification_cursor (last_event_id) SELECT coalesce(max(id), 0) FROM events ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	var cursor int64
	if err := n.DB.QueryRow(ctx, `SELECT last_event_id FROM notification_cursor`).Scan(&cursor); err != nil {
		return err
	}
	// Look only up to the first event that has not settled, so none is
	// skipped however events interleave.
	wait := fmt.Sprintf("%d seconds", int(settle.Seconds()))
	var last int64
	if err := n.DB.QueryRow(ctx, `SELECT coalesce((SELECT min(id) - 1 FROM events WHERE id > $1 AND occurred_at >= now() - $2::interval),
		(SELECT coalesce(max(id), $1) FROM events))`, cursor, wait).Scan(&last); err != nil {
		return err
	}
	rows, err := n.DB.Query(ctx, itemsSQL+` WHERE e.id > $1 AND e.id <= $2 AND e.type = ANY($3) ORDER BY e.id LIMIT 200`, cursor, last, eventTypes())
	if err != nil {
		return err
	}
	items, err := scanItems(rows)
	if err != nil {
		return err
	}
	if len(items) == 200 {
		last = items[len(items)-1].EventID
	}
	for _, it := range items {
		if it.Type == "chat.received" {
			// A live chat is many short messages: tell people when a visitor
			// starts waiting, not about every line they type after that.
			var waiting bool
			if err := n.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events e WHERE e.thread_id = $1 AND e.type = 'chat.received'
				AND e.id < $2 AND e.id > coalesce((SELECT max(id) FROM events s WHERE s.thread_id = $1 AND s.type = 'chat.sent' AND s.id < $2), 0))`,
				it.ThreadID, it.EventID).Scan(&waiting); err != nil {
				return err
			}
			if waiting {
				continue
			}
		}
		people, err := n.recipients(ctx, it.WorkspaceID, it.category())
		if err != nil {
			return err
		}
		for _, p := range people {
			for _, ch := range p.Channels {
				n.send(ctx, it, p, ch)
			}
		}
	}
	_, err = n.DB.Exec(ctx, `UPDATE notification_cursor SET last_event_id = $1 WHERE last_event_id < $1`, last)
	return err
}

func (n *Notifier) recipients(ctx context.Context, workspaceID, category string) ([]recipient, error) {
	def := Off
	for _, c := range Categories {
		if c.Key == category {
			def = c.Default
		}
	}
	rows, err := n.DB.Query(ctx, `SELECT p.id, coalesce(p.email, ''), coalesce(p.notify_phone, ''), coalesce(s.channels, '{email}')
		FROM principals p LEFT JOIN notification_settings s ON s.principal_id = p.id AND s.category = $2
		WHERE p.workspace_id = $1 AND p.kind = 'human' AND p.revoked_at IS NULL AND coalesce(s.delivery, $3) = 'instant'`, workspaceID, category, def)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (recipient, error) {
		var p recipient
		return p, r.Scan(&p.ID, &p.Email, &p.Phone, &p.Channels)
	})
}

func (n *Notifier) send(ctx context.Context, it item, p recipient, channel string) {
	if !n.Channels()[channel] || (channel == "email" && p.Email == "") || (channel == "sms" && p.Phone == "") {
		return
	}
	tag, err := n.DB.Exec(ctx, `INSERT INTO notifications_sent (principal_id, event_id, channel) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, p.ID, it.EventID, channel)
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	if channel == "email" {
		body := it.headline() + "\n"
		if d := it.detail(); d != "" {
			body += "\n" + clip(d, 2000) + "\n"
		}
		if t := it.transcriptText(); t != "" {
			body += "\n" + t
		}
		body += "\n" + n.link(it) + "\n\nChange what you are told about in VisiComms: " + strings.TrimRight(n.AppURL, "/") + "/settings/notifications\n"
		htmlBody := ""
		if it.isCall() {
			// A drawing failure must not cost the notification: send it plain.
			if htmlBody, err = n.callHTML(it); err != nil {
				n.Logger.Warn("call email not drawn", "event", it.EventID, "err", err)
				htmlBody = ""
			}
		}
		err = n.email(ctx, p.Email, "["+it.Workspace+"] "+it.headline(), body, htmlBody)
	} else {
		text := it.headline()
		if d := it.detail(); d != "" {
			text += ": " + clip(d, 200)
		}
		err = n.text(ctx, it.WorkspaceID, p.Phone, text+" "+n.link(it))
	}
	if err != nil {
		n.Logger.Warn("notification not sent", "event", it.EventID, "channel", channel, "err", err)
		_, _ = n.DB.Exec(ctx, `UPDATE notifications_sent SET error = $4 WHERE principal_id = $1 AND event_id = $2 AND channel = $3`, p.ID, it.EventID, channel, err.Error())
	}
}

func (n *Notifier) email(ctx context.Context, to, subject, body, htmlBody string) error {
	raw := sending.Message{FromName: "VisiComms", FromAddress: n.From, To: []string{to}, Subject: subject, Body: body, HTML: htmlBody, Date: time.Now()}.Build()
	_, err := n.Email.Send(ctx, raw, map[string]string{"kind": "notification"})
	return err
}

// text sends from the workspace's first SMS number. A person's phone is
// theirs, not a contact's, so no opt-out or thread applies.
func (n *Notifier) text(ctx context.Context, workspaceID, to, body string) error {
	numbers, err := sending.SMSNumbers(ctx, n.DB, workspaceID)
	if err != nil {
		return err
	}
	if len(numbers) == 0 {
		return fmt.Errorf("the workspace has no SMS number to send from")
	}
	sw, err := n.SignalWire.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	_, err = sw.SendSMS(ctx, sending.SMSMessage{From: numbers[0].Number, Body: clip(body, 600)}, to, "")
	return err
}

// RunDigests sends each person with any daily category their summary once a
// day, after digestHour in the workspace's time zone, covering everything
// since their last one (at most a week).
func (n *Notifier) RunDigests(ctx context.Context, now time.Time) error {
	if !n.Channels()["email"] {
		return nil
	}
	rows, err := n.DB.Query(ctx, `SELECT p.id, p.workspace_id, w.timezone, coalesce(p.email, ''),
			coalesce((SELECT max(covered_to) FROM notification_digests d WHERE d.principal_id = p.id), 0)
		FROM principals p JOIN workspaces w ON w.id = p.workspace_id
		WHERE p.kind = 'human' AND p.revoked_at IS NULL AND p.email IS NOT NULL`)
	if err != nil {
		return err
	}
	type person struct {
		ID, Workspace, TZ, Email string
		From                     int64
	}
	people, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (person, error) {
		var p person
		return p, r.Scan(&p.ID, &p.Workspace, &p.TZ, &p.Email, &p.From)
	})
	if err != nil {
		return err
	}
	for _, p := range people {
		loc, err := time.LoadLocation(p.TZ)
		if err != nil {
			loc = time.UTC
		}
		local := now.In(loc)
		if local.Hour() < digestHour {
			continue
		}
		if err := n.digest(ctx, p.ID, p.Workspace, p.Email, p.From, local); err != nil {
			n.Logger.Warn("daily summary not sent", "person", p.ID, "err", err)
		}
	}
	return nil
}

func (n *Notifier) digest(ctx context.Context, personID, workspaceID, email string, from int64, local time.Time) error {
	people, err := People(ctx, n.DB, workspaceID)
	if err != nil {
		return err
	}
	var daily []string
	for _, p := range people {
		if p.ID == personID {
			for _, s := range p.Settings {
				if s.Delivery == Daily {
					for _, c := range Categories {
						if c.Key == s.Category {
							daily = append(daily, c.events...)
						}
					}
				}
			}
		}
	}
	if len(daily) == 0 {
		return nil
	}
	day := local.Format("2006-01-02")
	var to int64
	if err := n.DB.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events`).Scan(&to); err != nil {
		return err
	}
	// Claim the day first: a crash loses a summary rather than sending two.
	tag, err := n.DB.Exec(ctx, `INSERT INTO notification_digests (principal_id, day, covered_to) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, personID, day, to)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	rows, err := n.DB.Query(ctx, itemsSQL+` WHERE e.workspace_id = $1 AND e.id > $2 AND e.id <= $3 AND e.type = ANY($4)
		AND e.occurred_at > now() - interval '7 days' ORDER BY e.id`, workspaceID, from, to, daily)
	if err != nil {
		return err
	}
	items, err := scanItems(rows)
	if err != nil || len(items) == 0 {
		return err
	}
	var b strings.Builder
	counts := map[string]int{}
	for _, it := range items {
		counts[it.category()]++
	}
	fmt.Fprintf(&b, "Since your last summary:\n\n")
	for _, c := range Categories {
		if counts[c.Key] > 0 {
			fmt.Fprintf(&b, "  %s: %d\n", c.Label, counts[c.Key])
		}
	}
	b.WriteString("\n")
	for _, it := range items {
		line := it.At.In(local.Location()).Format("Mon 3:04pm") + "  " + it.headline()
		if d := it.detail(); d != "" {
			line += ": " + clip(d, 120)
		}
		fmt.Fprintf(&b, "%s\n  %s\n", line, n.link(it))
	}
	b.WriteString("\nChange what you are told about in VisiComms: " + strings.TrimRight(n.AppURL, "/") + "/settings/notifications\n")
	subject := fmt.Sprintf("[%s] Daily summary: %d %s", items[0].Workspace, len(items), map[bool]string{true: "item", false: "items"}[len(items) == 1])
	return n.email(ctx, email, subject, b.String(), "")
}
