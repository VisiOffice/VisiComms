package notify

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/VisiOffice/VisiComms/internal/testutil"
)

type inbox struct{ sent []string }

func (b *inbox) Send(_ context.Context, raw []byte, _ map[string]string) (string, error) {
	b.sent = append(b.sent, string(raw))
	return "id", nil
}

func TestInstantAndDailyNotifications(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws, person, contact, thread string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO notification_cursor (last_event_id) VALUES (0)`)
	must(err)
	must(pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Birch Dental', 'America/Los_Angeles') RETURNING id`).Scan(&ws))
	must(pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name, email) VALUES ($1, 'human', 'Morgan', 'morgan@example.com') RETURNING id`, ws).Scan(&person))
	must(pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Jane Doe') RETURNING id`, ws).Scan(&contact))
	must(pool.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel) VALUES ($1, $2, 'voice') RETURNING id`, ws, contact).Scan(&thread))
	add := func(typ, payload, age string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, occurred_at)
			VALUES ($1, $2, $3, $4, 'voice', $5, now() - $6::interval)`, ws, thread, contact, typ, payload, age)
		must(err)
	}
	box := &inbox{}
	n := &Notifier{DB: pool, Logger: slog.Default(), Email: box, From: "notify@example.com", AppURL: "https://app.example.com"}

	add("voice.voicemail", `{"call_id":"c1","duration_seconds":42}`, "1 minute")
	add("voice.transcript", `{"call_id":"c1","text":"Is Tuesday still free?"}`, "1 minute")
	add("call.answered", `{"call_id":"c2"}`, "1 minute")
	add("sms.received", `{"text":"too new to send yet"}`, "0 seconds")
	must(n.RunInstant(ctx))
	if len(box.sent) != 1 || !strings.Contains(box.sent[0], "Voicemail from Jane Doe (0:42)") || !strings.Contains(box.sent[0], "Is Tuesday still free?") ||
		!strings.Contains(box.sent[0], "https://app.example.com/threads/"+thread) {
		t.Fatalf("instant = %q", box.sent)
	}
	must(n.RunInstant(ctx))
	if len(box.sent) != 1 {
		t.Fatalf("sent again: %d", len(box.sent))
	}

	// The new text has not settled, so the cursor stops before it.
	var cursor, newest int64
	must(pool.QueryRow(ctx, `SELECT last_event_id FROM notification_cursor`).Scan(&cursor))
	must(pool.QueryRow(ctx, `SELECT max(id) FROM events`).Scan(&newest))
	if cursor >= newest {
		t.Fatalf("cursor %d passed an unsettled event %d", cursor, newest)
	}

	// Turned off, nobody is told about texts; by default someone is.
	if people, err := n.recipients(ctx, ws, "sms"); err != nil || len(people) != 1 {
		t.Fatalf("default sms recipients = %v %v", people, err)
	}
	_, err = Update(ctx, pool, ws, person, person, Change{Settings: []Setting{{Category: "sms", Delivery: Off}}})
	must(err)
	if people, err := n.recipients(ctx, ws, "sms"); err != nil || len(people) != 0 {
		t.Fatalf("off sms recipients = %v %v", people, err)
	}

	// Answered calls are daily by default: the summary goes after 8am local,
	// once a day.
	morning := time.Date(2026, 10, 31, 7, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	must(n.RunDigests(ctx, morning))
	if len(box.sent) != 1 {
		t.Fatal("summary sent before 8am")
	}
	must(n.RunDigests(ctx, morning.Add(2*time.Hour)))
	if len(box.sent) != 2 || !strings.Contains(box.sent[1], "Daily summary: 1 item") || !strings.Contains(box.sent[1], "Call from Jane Doe answered") {
		t.Fatalf("summary = %q", box.sent[1:])
	}
	must(n.RunDigests(ctx, morning.Add(3*time.Hour)))
	if len(box.sent) != 2 {
		t.Fatal("two summaries in a day")
	}

	if _, err := Update(ctx, pool, ws, person, person, Change{Settings: []Setting{{Category: "nope", Delivery: Off}}}); err == nil {
		t.Fatal("unknown category accepted")
	}
	bad := "555"
	if _, err := Update(ctx, pool, ws, person, person, Change{Phone: &bad}); err == nil {
		t.Fatal("bad phone accepted")
	}
	if _, err := Update(ctx, pool, ws, "00000000-0000-0000-0000-000000000000", person, Change{}); err != ErrUnknownPerson {
		t.Fatalf("unknown person: %v", err)
	}
}

// A live chat is many short messages: one notification when the visitor
// starts waiting, and another only after someone has replied.
func TestChatNotifiesWhenTheVisitorStartsWaiting(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws, contact, thread string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO notification_cursor (last_event_id) VALUES (0)`)
	must(err)
	must(pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Acme Plumbing', 'America/Los_Angeles') RETURNING id`).Scan(&ws))
	_, err = pool.Exec(ctx, `INSERT INTO principals (workspace_id, kind, name, email) VALUES ($1, 'human', 'Morgan', 'morgan@example.com')`, ws)
	must(err)
	must(pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Website visitor') RETURNING id`, ws).Scan(&contact))
	must(pool.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel) VALUES ($1, $2, 'chat') RETURNING id`, ws, contact).Scan(&thread))
	add := func(typ, text string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, occurred_at)
			VALUES ($1, $2, $3, $4, 'chat', jsonb_build_object('text', $5::text), now() - interval '1 minute')`, ws, thread, contact, typ, text)
		must(err)
	}
	box := &inbox{}
	n := &Notifier{DB: pool, Logger: slog.Default(), Email: box, From: "notify@example.com", AppURL: "https://app.example.com"}
	add("chat.received", "Hi")
	add("chat.received", "Do you deliver to Glendale?")
	add("chat.sent", "Yes we do.")
	add("chat.received", "Great, Friday?")
	add("chat.received", "Morning if possible")
	must(n.RunInstant(ctx))
	if len(box.sent) != 2 || !strings.Contains(box.sent[0], "Website chat from Website visitor") || !strings.Contains(box.sent[0], "Hi") ||
		!strings.Contains(box.sent[1], "Great, Friday?") {
		t.Fatalf("sent %d: %q", len(box.sent), box.sent)
	}
}
