package voice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func TestValidateSaysWhatToFix(t *testing.T) {
	cases := map[string]Flow{
		"not available yet":       {Answer: Action{Mode: AI}},
		"forward_to":              {Answer: Action{Mode: Forward, ForwardTo: "555-0100"}},
		"needs a menu":            {Answer: Action{Mode: IVR}},
		"cannot open another":     {Answer: Action{Mode: IVR}, Menu: &Menu{Prompt: "p", Options: []Option{{Digit: "1", Action: Action{Mode: IVR}}}, NoChoice: Action{Mode: Voicemail}}},
		"its own digit":           {Answer: Action{Mode: IVR}, Menu: &Menu{Prompt: "p", Options: []Option{{Digit: "1", Action: Action{Mode: Voicemail}}, {Digit: "1", Action: Action{Mode: Voicemail}}}, NoChoice: Action{Mode: Voicemail}}},
		"ring_seconds":            {Answer: Action{Mode: Voicemail}, RingSeconds: 90},
		"menu is only for answer": {Answer: Action{Mode: Voicemail}, Menu: &Menu{}},
		"then only follows":       {Answer: Action{Mode: Voicemail, Then: &Action{Mode: Voicemail}}},
		"at most 4 steps": {Answer: Action{Mode: RingApp, Then: &Action{Mode: RingApp, Then: &Action{Mode: RingApp, Then: &Action{Mode: RingApp,
			Then: &Action{Mode: Voicemail}}}}}},
		"forward_to: ": {Answer: Action{Mode: RingApp, Then: &Action{Mode: Forward, ForwardTo: "nope"}}},
	}
	for want, f := range cases {
		if err := f.Validate(); !errors.Is(err, ErrInvalidFlow) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	f := Flow{Answer: Action{Mode: Forward, ForwardTo: "+1 (310) 555-0100"}}
	if err := f.Validate(); err != nil || f.Answer.ForwardTo != "+13105550100" || f.RingSeconds != 20 || f.Greeting == "" {
		t.Fatalf("defaults: %+v %v", f, err)
	}
}

func TestFallbackChains(t *testing.T) {
	f := Flow{Answer: Action{Mode: RingApp, Then: &Action{Mode: Forward, ForwardTo: "+1 (310) 555-0100", Then: &Action{Mode: Voicemail}}}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := f.Describe(); got != "ring in the app, then forward to +13105550100, then voicemail" {
		t.Fatalf("describe = %q", got)
	}
	if a := f.At(RouteAnswer, 1); a == nil || a.ForwardTo != "+13105550100" {
		t.Fatalf("step 1 = %+v", a)
	}
	if f.At(RouteAnswer, 3) != nil || f.At(RouteKey("1"), 0) != nil {
		t.Fatal("steps past the chain or a missing route exist")
	}
}

func TestPendingChangesWaitForAPerson(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws, other, connector string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Birch Dental', 'UTC') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO connectors (workspace_id, kind, name, settings, health) VALUES ($1, 'signalwire', 'n', '{}', 'ok') RETURNING id`, ws).Scan(&connector); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO phone_numbers (number, workspace_id, connector_id, provider_project_id) VALUES ('+13105550100', $1, $2, 'p')`, ws, connector); err != nil {
		t.Fatal(err)
	}
	const n = "+13105550100"
	forward := Flow{Answer: Action{Mode: Forward, ForwardTo: "+13105550111"}}

	if _, err := Save(ctx, pool, other, n, "", forward, true); !errors.Is(err, ErrUnknownNumber) {
		t.Fatalf("another workspace changed the number: %v", err)
	}
	if _, err := Activate(ctx, pool, ws, n, ""); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("activated nothing: %v", err)
	}
	first, err := Save(ctx, pool, ws, n, "", forward, false)
	if err != nil || first.Status != "pending" {
		t.Fatalf("Save=%+v %v", first, err)
	}
	// A newer pending change replaces the older one.
	second, err := Save(ctx, pool, ws, n, "", Flow{Answer: Action{Mode: Voicemail}, Greeting: "Leave a message"}, false)
	if err != nil || second.Version != 2 {
		t.Fatalf("Save=%+v %v", second, err)
	}
	flows, _ := Get(ctx, pool, ws, n)
	if flows.Pending == nil || flows.Pending.Version != 2 || flows.Active.Version != 0 {
		t.Fatalf("flows=%+v", flows)
	}
	if v, err := Activate(ctx, pool, ws, n, ""); err != nil || v.Version != 2 {
		t.Fatalf("Activate=%+v %v", v, err)
	}
	flows, _ = Get(ctx, pool, ws, n)
	if flows.Pending != nil || flows.Active.Flow.Greeting != "Leave a message" {
		t.Fatalf("flows=%+v", flows)
	}
	// A person's change replaces both the active flow and anything waiting.
	if _, err := Save(ctx, pool, ws, n, "", forward, false); err != nil {
		t.Fatal(err)
	}
	if v, err := Save(ctx, pool, ws, n, "", forward, true); err != nil || v.Version != 4 {
		t.Fatalf("Save=%+v %v", v, err)
	}
	if flows, _ = Get(ctx, pool, ws, n); flows.Pending != nil || flows.Active.Version != 4 {
		t.Fatalf("flows=%+v", flows)
	}
	if err := Discard(ctx, pool, ws, n); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("Discard=%v", err)
	}
}
