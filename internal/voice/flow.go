// Package voice answers calls to a workspace's numbers. Each number has a call
// flow, chosen in the console or by an agent: ring people in the app, forward
// to a phone, an IVR menu, or the AI. Whatever happens, an unanswered call
// ends in voicemail, and every call is an event on the caller's thread.
package voice

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
)

// Modes a call can be answered in. AI needs OpenAI configured (EnableAI);
// until then, Available says so and Validate refuses it.
const (
	Forward   = "forward"
	IVR       = "ivr"
	AI        = "ai"
	RingApp   = "ring_app"
	Voicemail = "voicemail"
)

// Available is which modes this build can run.
var Available = map[string]bool{Forward: true, IVR: true, RingApp: true, Voicemail: true}

// Action is one way to answer: the flow's own, or an IVR option's.
type Action struct {
	Mode      string `json:"mode"`
	ForwardTo string `json:"forward_to,omitempty"`
	// Then is tried when a ring or forward goes unanswered (or nobody is
	// online to ring); without it, the caller leaves a message.
	Then *Action `json:"then,omitempty"`
}

// maxChain is the most steps an action and its fallbacks can have.
const maxChain = 4

// rings reports whether a mode can go unanswered and so fall through.
func rings(mode string) bool { return mode == Forward || mode == RingApp }

// Option is one key in an IVR menu.
type Option struct {
	Digit  string `json:"digit"`
	Label  string `json:"label"`
	Action Action `json:"action"`
}

// Menu is an IVR: a prompt, the keys, and what happens when the caller
// presses nothing (never a hang-up).
type Menu struct {
	Prompt   string   `json:"prompt"`
	Options  []Option `json:"options"`
	NoChoice Action   `json:"no_choice"`
}

// Flow is what a number does with a call.
type Flow struct {
	Answer Action `json:"answer"`
	Menu   *Menu  `json:"menu,omitempty"`
	// RingSeconds is how long a forward or ring rings before voicemail.
	RingSeconds int `json:"ring_seconds"`
	// Greeting is read before the voicemail beep.
	Greeting string `json:"greeting"`
	// Agent is how the AI answers; required when any action is AI.
	Agent *Agent `json:"agent,omitempty"`
}

// Agent is the AI's setup for a number. What it knows comes from the
// workspace's approved info, so a fact a person has not approved never
// reaches a caller.
type Agent struct {
	// Opening is the AI's first line, spoken as soon as it picks up.
	Opening string `json:"opening"`
	// Instructions are this business's rules for calls, on top of the
	// built-in phone manner.
	Instructions string `json:"instructions"`
	Voice        string `json:"voice"`
	// TransferTo is a person the AI can hand the caller to; empty, it takes
	// a message instead.
	TransferTo string `json:"transfer_to,omitempty"`
}

func (a *Agent) validate() error {
	a.Opening = strings.TrimSpace(a.Opening)
	a.Instructions = strings.TrimSpace(a.Instructions)
	if a.Opening == "" || utf8.RuneCountInString(a.Opening) > 300 {
		return invalid("agent.opening is required, at most 300 characters")
	}
	if utf8.RuneCountInString(a.Instructions) > 20000 {
		return invalid("agent.instructions is longer than 20000 characters")
	}
	if a.Voice == "" {
		a.Voice = defaultVoice
	}
	if !slices.Contains(Voices, a.Voice) {
		return invalid("agent.voice must be one of %s", strings.Join(Voices, ", "))
	}
	if a.TransferTo != "" {
		number, err := phone.Normalize(a.TransferTo)
		if err != nil {
			return invalid("agent.transfer_to: %v", err)
		}
		a.TransferTo = number
	}
	return nil
}

// UsesAI reports whether any way through the flow reaches the AI.
func (f Flow) UsesAI() bool {
	for _, start := range f.starts() {
		for a := start; a != nil; a = a.Then {
			if a.Mode == AI {
				return true
			}
		}
	}
	return false
}

// Routes name where an action starts: the flow's answer, an IVR key, or the
// menu's no-choice action.
const (
	RouteAnswer   = "answer"
	RouteNoChoice = "none"
)

// RouteKey is the route of the IVR option for a key.
func RouteKey(digit string) string { return "key" + digit }

func (f Flow) starts() map[string]*Action {
	out := map[string]*Action{RouteAnswer: &f.Answer}
	if f.Menu != nil {
		out[RouteNoChoice] = &f.Menu.NoChoice
		for i := range f.Menu.Options {
			out[RouteKey(f.Menu.Options[i].Digit)] = &f.Menu.Options[i].Action
		}
	}
	return out
}

// At is the action depth fallbacks into route, or nil.
func (f Flow) At(route string, depth int) *Action {
	a := f.starts()[route]
	for ; a != nil && depth > 0; depth-- {
		a = a.Then
	}
	return a
}

// DefaultFlow is what a number without a flow does: take a message.
func DefaultFlow() Flow {
	return Flow{Answer: Action{Mode: Voicemail}, RingSeconds: 20, Greeting: defaultGreeting}
}

const defaultGreeting = "Sorry we missed you. Please leave a message after the tone and we will call you back."

var ErrInvalidFlow = errors.New("invalid call flow")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFlow, fmt.Sprintf(format, a...))
}

// Validate checks a flow and fills its defaults. Errors wrap ErrInvalidFlow
// and say what to fix, for a person or an agent.
func (f *Flow) Validate() error {
	if err := f.validate(); err != nil {
		return err
	}
	if f.UsesAI() && f.Agent == nil {
		return invalid("mode ai needs agent settings: an opening line at least")
	}
	if f.Agent != nil {
		return f.Agent.validate()
	}
	return nil
}

func (f *Flow) validate() error {
	if f.RingSeconds == 0 {
		f.RingSeconds = 20
	}
	if f.RingSeconds < 5 || f.RingSeconds > 60 {
		return invalid("ring_seconds must be between 5 and 60")
	}
	f.Greeting = strings.TrimSpace(f.Greeting)
	if f.Greeting == "" {
		f.Greeting = defaultGreeting
	}
	if utf8.RuneCountInString(f.Greeting) > 500 {
		return invalid("greeting is longer than 500 characters")
	}
	if err := f.Answer.validate(true); err != nil {
		return err
	}
	if f.Answer.Mode != IVR {
		if f.Menu != nil {
			return invalid("menu is only for answer mode ivr")
		}
		return nil
	}
	m := f.Menu
	if m == nil {
		return invalid("answer mode ivr needs a menu")
	}
	m.Prompt = strings.TrimSpace(m.Prompt)
	if m.Prompt == "" || utf8.RuneCountInString(m.Prompt) > 500 {
		return invalid("the menu needs a prompt of at most 500 characters")
	}
	if len(m.Options) == 0 || len(m.Options) > 9 {
		return invalid("the menu needs between 1 and 9 options")
	}
	seen := map[string]bool{}
	for i := range m.Options {
		o := &m.Options[i]
		if len(o.Digit) != 1 || o.Digit < "0" || o.Digit > "9" || seen[o.Digit] {
			return invalid("each option needs its own digit, 0 to 9")
		}
		seen[o.Digit] = true
		o.Label = strings.TrimSpace(o.Label)
		if utf8.RuneCountInString(o.Label) > 80 {
			return invalid("option labels are at most 80 characters")
		}
		if err := o.Action.validate(false); err != nil {
			return err
		}
	}
	return m.NoChoice.validate(false)
}

func (a *Action) validate(top bool) error {
	steps := 0
	for step := a; step != nil; step = step.Then {
		if steps++; steps > maxChain {
			return invalid("an action can have at most %d steps", maxChain)
		}
		if err := step.validateStep(top && step == a); err != nil {
			return err
		}
		if step.Then != nil && !rings(step.Mode) {
			return invalid("then only follows forward or ring_app; %s does not go unanswered", step.Mode)
		}
	}
	return nil
}

func (a *Action) validateStep(top bool) error {
	switch {
	case a.Mode == IVR && !top:
		return invalid("a menu option cannot open another menu")
	case a.Mode != IVR && !Available[a.Mode]:
		if a.Mode == AI || a.Mode == RingApp {
			return invalid("mode %s is not available yet", a.Mode)
		}
		return invalid("mode must be forward, ivr, ai, ring_app or voicemail")
	}
	if a.Mode != Forward {
		if a.ForwardTo != "" {
			return invalid("forward_to is only for mode forward")
		}
		return nil
	}
	number, err := phone.Normalize(a.ForwardTo)
	if err != nil {
		return invalid("forward_to: %v", err)
	}
	a.ForwardTo = number
	return nil
}

// Version is a stored flow.
type Version struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	Version   int    `json:"version"`
	Status    string `json:"status"`
	Flow      Flow   `json:"flow"`
	CreatedBy string `json:"created_by,omitempty"`
	// Summary is Flow.Describe, filled in for agents.
	Summary string `json:"summary,omitempty"`
}

// Flows is a number's active flow (DefaultFlow when it has none) and the
// version waiting for a person, if any.
type Flows struct {
	Number  string   `json:"number"`
	Active  Version  `json:"active"`
	Pending *Version `json:"pending,omitempty"`
}

var ErrUnknownNumber = errors.New("no such number in this workspace")

// ErrNothingPending means there is no waiting change to activate or discard.
var ErrNothingPending = errors.New("no change is waiting for this number")

// Get returns a number's flows.
func Get(ctx context.Context, q db.Querier, workspaceID, number string) (Flows, error) {
	out := Flows{Number: number, Active: Version{Number: number, Status: "active", Flow: DefaultFlow()}}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM phone_numbers WHERE number = $1 AND workspace_id = $2)`, number, workspaceID).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrUnknownNumber
	}
	rows, err := q.Query(ctx, `SELECT id, number, version, status, flow, coalesce(created_by::text, '') FROM call_flows
		WHERE number = $1 AND workspace_id = $2 AND status IN ('active', 'pending')`, number, workspaceID)
	if err != nil {
		return out, err
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Version, error) {
		var v Version
		err := row.Scan(&v.ID, &v.Number, &v.Version, &v.Status, &v.Flow, &v.CreatedBy)
		return v, err
	})
	if err != nil {
		return out, err
	}
	for _, v := range versions {
		if v.Status == "active" {
			out.Active = v
		} else {
			out.Pending = &v
		}
	}
	return out, nil
}

// Save stores a new version of a number's flow. Active makes it the one calls
// run, superseding the old; otherwise it waits for a person (replacing any
// version already waiting).
func Save(ctx context.Context, pool *pgxpool.Pool, workspaceID, number, actorID string, f Flow, active bool) (Version, error) {
	if err := f.Validate(); err != nil {
		return Version{}, err
	}
	var v Version
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "call-flow:"+number); err != nil {
			return err
		}
		if _, err := Get(ctx, tx, workspaceID, number); err != nil {
			return err
		}
		retire := "pending"
		status := "pending"
		if active {
			retire, status = "active", "active"
			// An activated change also settles whatever was waiting.
			if _, err := tx.Exec(ctx, `UPDATE call_flows SET status = 'discarded' WHERE number = $1 AND status = 'pending'`, number); err != nil {
				return err
			}
		}
		next := map[string]string{"active": "superseded", "pending": "discarded"}[retire]
		if _, err := tx.Exec(ctx, `UPDATE call_flows SET status = $2 WHERE number = $1 AND status = $3`, number, next, retire); err != nil {
			return err
		}
		v = Version{Number: number, Status: status, Flow: f, CreatedBy: actorID}
		return tx.QueryRow(ctx, `INSERT INTO call_flows (workspace_id, number, version, status, flow, created_by, activated_by, activated_at)
			SELECT $1, $2, coalesce(max(version), 0) + 1, $3, $4, NULLIF($5, '')::uuid, CASE WHEN $3 = 'active' THEN NULLIF($5, '')::uuid END, CASE WHEN $3 = 'active' THEN now() END
			FROM call_flows WHERE number = $2 RETURNING id, version`,
			workspaceID, number, status, f, actorID).Scan(&v.ID, &v.Version)
	})
	return v, err
}

// Activate makes the waiting version the one calls run. Only a person does
// this.
func Activate(ctx context.Context, pool *pgxpool.Pool, workspaceID, number, personID string) (Version, error) {
	var v Version
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "call-flow:"+number); err != nil {
			return err
		}
		flows, err := Get(ctx, tx, workspaceID, number)
		if err != nil {
			return err
		}
		if flows.Pending == nil {
			return ErrNothingPending
		}
		if _, err := tx.Exec(ctx, `UPDATE call_flows SET status = 'superseded' WHERE number = $1 AND status = 'active'`, number); err != nil {
			return err
		}
		v = *flows.Pending
		v.Status = "active"
		_, err = tx.Exec(ctx, `UPDATE call_flows SET status = 'active', activated_by = NULLIF($2, '')::uuid, activated_at = now() WHERE id = $1`, v.ID, personID)
		return err
	})
	return v, err
}

// Discard drops the waiting version.
func Discard(ctx context.Context, pool *pgxpool.Pool, workspaceID, number string) error {
	tag, err := pool.Exec(ctx, `UPDATE call_flows SET status = 'discarded' WHERE number = $1 AND workspace_id = $2 AND status = 'pending'`, number, workspaceID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNothingPending
	}
	return err
}

// Describe is a one-line summary of a flow, for people and agents.
func (f Flow) Describe() string {
	one := func(a Action) string {
		switch a.Mode {
		case Forward:
			return "forward to " + a.ForwardTo
		case AI:
			return "AI answers"
		case RingApp:
			return "ring in the app"
		}
		return "voicemail"
	}
	say := func(a Action) string {
		parts := []string{one(a)}
		for t := a.Then; t != nil; t = t.Then {
			parts = append(parts, one(*t))
		}
		return strings.Join(parts, ", then ")
	}
	if f.Answer.Mode != IVR || f.Menu == nil {
		return say(f.Answer)
	}
	parts := []string{}
	for _, o := range f.Menu.Options {
		parts = append(parts, o.Digit+": "+say(o.Action))
	}
	return "menu (" + strings.Join(parts, ", ") + "; no choice: " + say(f.Menu.NoChoice) + ")"
}

// List returns the flows of every number in the workspace.
func List(ctx context.Context, q db.Querier, workspaceID string) ([]Flows, error) {
	rows, err := q.Query(ctx, `SELECT number FROM phone_numbers WHERE workspace_id = $1 ORDER BY number`, workspaceID)
	if err != nil {
		return nil, err
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := []Flows{}
	for _, n := range numbers {
		f, err := Get(ctx, q, workspaceID, n)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// ErrObserver means an agent in observe mode tried to change a flow.
var ErrObserver = errors.New("an agent in observe mode cannot change call flows")

// SaveAs stores a change from an agent: applied at once for an autonomous
// agent, waiting for a person for one that needs approval, refused for an
// observer.
func SaveAs(ctx context.Context, pool *pgxpool.Pool, workspaceID, number, agentID, autonomy string, f Flow) (Version, error) {
	switch autonomy {
	case "autonomous":
		return Save(ctx, pool, workspaceID, number, agentID, f, true)
	case "approve":
		return Save(ctx, pool, workspaceID, number, agentID, f, false)
	}
	return Version{}, ErrObserver
}
