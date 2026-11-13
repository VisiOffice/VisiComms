package chat

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func TestStaffed(t *testing.T) {
	hours := []byte(`{"mon": [["08:00","12:00"],["13:00","17:00"]]}`)
	la, _ := time.LoadLocation("America/Los_Angeles")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, la)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for when, want := range map[string]bool{
		"2026-11-09 08:00": true,  // Monday, opening
		"2026-11-09 12:30": false, // lunch
		"2026-11-09 16:59": true,
		"2026-11-09 17:00": false,
		"2026-11-10 10:00": false, // Tuesday is not listed: closed
	} {
		if got := Staffed(hours, "America/Los_Angeles", at(when).UTC()); got != want {
			t.Errorf("%s: staffed=%v", when, got)
		}
	}
	if !Staffed([]byte(`{}`), "America/Los_Angeles", time.Now()) {
		t.Error("no hours set means always staffed")
	}
}

func TestAttributionRecord(t *testing.T) {
	cases := []struct {
		in          Attribution
		source, how string
		medium      string
	}{
		{Attribution{Source: "newsletter", Medium: "email", GCLID: "x"}, "newsletter", "utm", "email"},
		{Attribution{GBRAID: "x"}, "google_ads", "click_id", "cpc"},
		{Attribution{MSCLKID: "x"}, "microsoft_ads", "click_id", "cpc"},
		{Attribution{Referrer: "https://www.bing.com/search?q=plumber"}, "bing.com", "referrer", ""},
	}
	for _, c := range cases {
		r := c.in.record()
		src, _ := r["source"].(map[string]string)
		med, _ := r["medium"].(map[string]string)
		if src["value"] != c.source || src["how"] != c.how || med["value"] != c.medium {
			t.Errorf("%+v: %v", c.in, r)
		}
	}
	if len(Attribution{}.record()) != 0 {
		t.Error("nothing known records nothing")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"acmeplumbing.example":                   "https://acmeplumbing.example",
		"https://WWW.AcmePlumbing.example/a?b=1": "https://www.acmeplumbing.example",
		"http://localhost:4321":                  "http://localhost:4321",
	} {
		if got, err := NormalizeOrigin(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"ftp://x.com", "https://user@x.com", "https://"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestHubWakesOnCommitOnly(t *testing.T) {
	pool := testutil.DB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wake, stop := hub.Subscribe("thread-1")
	defer stop()
	go hub.Run(ctx)
	// The first wake-up is the hub catching up after it starts listening.
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("hub never started")
	}

	// A rolled-back transaction's notification is never delivered.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Notify(ctx, tx, "thread-1"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	select {
	case <-wake:
		t.Fatal("woken by a rolled-back transaction")
	case <-time.After(300 * time.Millisecond):
	}

	if err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if err := Notify(ctx, tx, "thread-2"); err != nil {
			return err
		}
		return Notify(ctx, tx, "thread-1")
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("not woken by a commit")
	}
}
