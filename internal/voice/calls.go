package voice

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/VisiOffice/VisiComms/internal/untrusted"
)

// ErrUnknownCall means a callback names a call this project never began.
var ErrUnknownCall = errors.New("no such call")

// Call is a call in progress, as the webhooks need it.
type Call struct {
	ID, WorkspaceID, ThreadID, ContactID string
	From, To                             string
	// CallSID and Project name the call at SignalWire.
	CallSID, Project string
	Flow             Flow
	// Entrance is the name of the door intercom the call is from, if it is;
	// the visitor is standing there, not calling from a phone of their own.
	Entrance string
	Outcome  string
	// AIAnswered is set once the AI has started speaking; from then the
	// AI's conversation records how the call ended.
	AIAnswered bool
	// Space and Browsers are filled in before ringing the app: the
	// project's SignalWire space and the browser phones online to ring.
	Space    string
	Browsers []string
}

// Incoming is SignalWire's first callback for a call.
type Incoming struct {
	Project, CallSID, From, To string
}

// Begin records an incoming call on the caller's voice thread and returns the
// flow it runs. A repeated callback for the same call returns the same call.
func Begin(ctx context.Context, pool *pgxpool.Pool, in Incoming) (Call, error) {
	if user, ok := sipUser(in.From); ok {
		return beginFromSIP(ctx, pool, in, user)
	}
	c := Call{To: in.To, CallSID: in.CallSID, Project: in.Project}
	to, err := phone.Normalize(in.To)
	if err != nil || in.CallSID == "" {
		return c, ErrUnknownNumber
	}
	c.To = to
	// Withheld and malformed caller IDs share one "unknown caller" contact.
	from, err := phone.Normalize(in.From)
	known := err == nil
	if !known {
		from = "unknown"
	}
	c.From = from
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM phone_numbers WHERE number = $1 AND provider_project_id = $2`, to, in.Project).Scan(&c.WorkspaceID); err != nil {
			if db.NotFound(err) {
				return ErrUnknownNumber
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.WorkspaceID+":voice:"+from); err != nil {
			return err
		}
		if done, err := resume(ctx, tx, &c); done || err != nil {
			return err
		}
		var entranceID *string
		if known {
			var id string
			err := tx.QueryRow(ctx, `SELECT id::text, contact_id::text, name, flow FROM entrances WHERE workspace_id = $1 AND caller_id = $2`,
				c.WorkspaceID, from).Scan(&id, &c.ContactID, &c.Entrance, &c.Flow)
			if err == nil {
				entranceID = &id
			} else if !db.NotFound(err) {
				return err
			}
		}
		if entranceID == nil {
			if err := contactFor(ctx, tx, &c, known); err != nil {
				return err
			}
		}
		// A call from an entrance moves its number's thread to the
		// entrance's contact.
		if err := tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject, phone_number, remote_address)
			VALUES ($1, $2, 'voice', $3, $4, $3)
			ON CONFLICT (workspace_id, remote_address, phone_number) WHERE channel = 'voice'
			DO UPDATE SET state = 'open', version = threads.version + 1, updated_at = now(),
				contact_id = CASE WHEN $5 THEN EXCLUDED.contact_id ELSE threads.contact_id END RETURNING id`,
			c.WorkspaceID, c.ContactID, from, to, entranceID != nil).Scan(&c.ThreadID); err != nil {
			return err
		}
		var flowID *string
		if entranceID == nil {
			c.Flow = DefaultFlow()
			var id string
			err := tx.QueryRow(ctx, `SELECT id, flow FROM call_flows WHERE number = $1 AND status = 'active'`, to).Scan(&id, &c.Flow)
			if err == nil {
				flowID = &id
			} else if !db.NotFound(err) {
				return err
			}
		}
		return record(ctx, tx, &c, &to, flowID, entranceID)
	})
	return c, err
}

// beginFromSIP starts a call placed by an entrance's intercom signed in to
// its SIP account. The account, not anything the visitor says, names the
// entrance; a SIP call from any other account is refused.
func beginFromSIP(ctx context.Context, pool *pgxpool.Pool, in Incoming, user string) (Call, error) {
	c := Call{From: "sip:" + user, CallSID: in.CallSID, Project: in.Project}
	if in.CallSID == "" {
		return c, ErrUnknownNumber
	}
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var entranceID string
		err := tx.QueryRow(ctx, `SELECT workspace_id::text, id::text, contact_id::text, name, flow FROM entrances WHERE sip_username = $1 AND provider_project_id = $2`,
			user, in.Project).Scan(&c.WorkspaceID, &entranceID, &c.ContactID, &c.Entrance, &c.Flow)
		if db.NotFound(err) {
			return ErrUnknownNumber
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.WorkspaceID+":voice:"+c.From); err != nil {
			return err
		}
		if done, err := resume(ctx, tx, &c); done || err != nil {
			return err
		}
		// No number was dialled, so the thread is found by its SIP address.
		err = tx.QueryRow(ctx, `UPDATE threads SET state = 'open', version = version + 1, updated_at = now(), contact_id = $3
			WHERE id = (SELECT id FROM threads WHERE workspace_id = $1 AND channel = 'voice' AND remote_address = $2 AND phone_number IS NULL ORDER BY created_at LIMIT 1)
			RETURNING id`, c.WorkspaceID, c.From, c.ContactID).Scan(&c.ThreadID)
		if db.NotFound(err) {
			err = tx.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel, subject, remote_address) VALUES ($1, $2, 'voice', $3, $4) RETURNING id`,
				c.WorkspaceID, c.ContactID, c.Entrance, c.From).Scan(&c.ThreadID)
		}
		if err != nil {
			return err
		}
		return record(ctx, tx, &c, nil, nil, &entranceID)
	})
	return c, err
}

// resume finds a call SignalWire has already told us about, so a repeated
// first callback returns the same call.
func resume(ctx context.Context, tx pgx.Tx, c *Call) (bool, error) {
	var outcome *string
	err := tx.QueryRow(ctx, `SELECT c.id, c.thread_id, c.contact_id, c.outcome, coalesce(f.flow, e.flow, 'null'::jsonb), coalesce(e.name, '')
		FROM calls c LEFT JOIN call_flows f ON f.id = c.flow_id LEFT JOIN entrances e ON e.id = c.entrance_id
		WHERE c.call_sid = $1 AND c.provider_project_id = $2`, c.CallSID, c.Project).Scan(&c.ID, &c.ThreadID, &c.ContactID, &outcome, &c.Flow, &c.Entrance)
	if db.NotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if outcome != nil {
		c.Outcome = *outcome
	}
	if c.Flow.Answer.Mode == "" {
		c.Flow = DefaultFlow()
	}
	return true, nil
}

// record stores a new call and its call.received event.
func record(ctx context.Context, tx pgx.Tx, c *Call, number, flowID, entranceID *string) error {
	if err := tx.QueryRow(ctx, `INSERT INTO calls (workspace_id, thread_id, contact_id, call_sid, provider_project_id, number, remote, flow_id, entrance_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		c.WorkspaceID, c.ThreadID, c.ContactID, c.CallSID, c.Project, number, c.From, flowID, entranceID).Scan(&c.ID); err != nil {
		return err
	}
	payload := map[string]any{"call_id": c.ID, "from": c.From, "flow": c.Flow.Describe()}
	if number != nil {
		payload["to"] = *number
	}
	if c.Entrance != "" {
		payload["entrance"] = c.Entrance
	}
	return event(ctx, tx, *c, "call.received", payload, false)
}

func contactFor(ctx context.Context, tx pgx.Tx, c *Call, known bool) error {
	if known {
		// The same number texting and calling is the same person.
		err := tx.QueryRow(ctx, `SELECT contact_id FROM identities WHERE workspace_id = $1 AND channel IN ('voice', 'sms') AND address = $2
			ORDER BY channel = 'voice' DESC LIMIT 1`, c.WorkspaceID, c.From).Scan(&c.ContactID)
		if err == nil || !db.NotFound(err) {
			return err
		}
	} else {
		err := tx.QueryRow(ctx, `SELECT contact_id FROM threads WHERE workspace_id = $1 AND channel = 'voice' AND remote_address = 'unknown' LIMIT 1`, c.WorkspaceID).Scan(&c.ContactID)
		if err == nil || !db.NotFound(err) {
			return err
		}
	}
	name := c.From
	if !known {
		name = "Unknown caller"
	}
	if err := tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id`, c.WorkspaceID, name).Scan(&c.ContactID); err != nil {
		return err
	}
	if !known {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'voice', $3)`, c.WorkspaceID, c.ContactID, c.From)
	return err
}

// Find loads a call from a later callback, scoped to the project that
// signed it.
func Find(ctx context.Context, q db.Querier, project, callSID string) (Call, error) {
	c := Call{CallSID: callSID, Project: project}
	var outcome *string
	err := q.QueryRow(ctx, `SELECT c.id, c.workspace_id, c.thread_id, c.contact_id, c.remote, coalesce(c.number, ''), c.outcome,
			coalesce(f.flow, e.flow, 'null'::jsonb), coalesce(e.name, ''), c.ai_greeted_at IS NOT NULL
		FROM calls c LEFT JOIN call_flows f ON f.id = c.flow_id LEFT JOIN entrances e ON e.id = c.entrance_id
		WHERE c.call_sid = $1 AND c.provider_project_id = $2`,
		callSID, project).Scan(&c.ID, &c.WorkspaceID, &c.ThreadID, &c.ContactID, &c.From, &c.To, &outcome, &c.Flow, &c.Entrance, &c.AIAnswered)
	if db.NotFound(err) {
		return c, ErrUnknownCall
	}
	if outcome != nil {
		c.Outcome = *outcome
	}
	if c.Flow.Answer.Mode == "" {
		c.Flow = DefaultFlow()
	}
	return c, err
}

// Answered records that a forwarded call was picked up.
func Answered(ctx context.Context, pool *pgxpool.Pool, c Call, by string) error {
	return finish(ctx, pool, c, "answered", "call.answered", map[string]any{"call_id": c.ID, "answered_by": by})
}

// Missed records a call that ended before anyone answered or a message was
// left. It waits for a person like a message does.
func Missed(ctx context.Context, pool *pgxpool.Pool, c Call) error {
	return finish(ctx, pool, c, "missed", "call.missed", map[string]any{"call_id": c.ID, "from": c.From, "classification": map[string]any{"kind": "message"}})
}

// Failed records a call SignalWire could not complete.
func Failed(ctx context.Context, pool *pgxpool.Pool, c Call, status string) error {
	return finish(ctx, pool, c, "failed", "call.failed", map[string]any{"call_id": c.ID, "status": status})
}

// LeftMessage records a message left by the caller.
func LeftMessage(ctx context.Context, pool *pgxpool.Pool, c Call, recordingURL string, seconds int) error {
	return finish(ctx, pool, c, "voicemail", "voice.voicemail", map[string]any{
		"call_id": c.ID, "from": c.From, "recording_url": recordingURL, "duration_seconds": seconds,
		"classification": map[string]any{"kind": "message"},
	})
}

// Recorded attaches the recording of a call the AI answered. It leaves the
// call's outcome alone, and a repeated callback adds nothing.
func Recorded(ctx context.Context, pool *pgxpool.Pool, c Call, recordingURL, recordingSID string, seconds int) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE thread_id = $1 AND type = 'call.recorded' AND payload->>'call_id' = $2)`, c.ThreadID, c.ID).Scan(&exists); err != nil || exists {
			return err
		}
		return event(ctx, tx, c, "call.recorded", map[string]any{
			"call_id": c.ID, "recording_url": recordingURL, "recording_sid": recordingSID, "duration_seconds": seconds,
		}, false)
	})
}

// RecordingURL finds the provider URL of a call's recording, from the AI
// call recording or a voicemail, within one workspace.
func RecordingURL(ctx context.Context, pool *pgxpool.Pool, workspaceID, callID string) (string, error) {
	var u string
	err := pool.QueryRow(ctx, `SELECT payload->>'recording_url' FROM events
		WHERE workspace_id = $1 AND type IN ('call.recorded', 'voice.voicemail') AND payload->>'call_id' = $2
		  AND coalesce(payload->>'recording_url', '') <> ''
		ORDER BY occurred_at DESC LIMIT 1`, workspaceID, callID).Scan(&u)
	return u, err
}

// finish sets the call's outcome once; a later callback for the same call
// changes nothing.
func finish(ctx context.Context, pool *pgxpool.Pool, c Call, outcome, eventType string, payload map[string]any) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		// A voicemail whose recording arrives after the hang-up replaces the
		// missed call the status callback recorded first.
		tag, err := tx.Exec(ctx, `UPDATE calls SET outcome = $2, ended_at = now()
			WHERE id = $1 AND (outcome IS NULL OR (outcome = 'missed' AND $2 = 'voicemail'))`, c.ID, outcome)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return event(ctx, tx, c, eventType, payload, false)
	})
}

// Transcript records what the caller said in a voicemail. It is the
// caller's words, so it is untrusted like any message.
func Transcript(ctx context.Context, pool *pgxpool.Pool, c Call, text string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE thread_id = $1 AND type = 'voice.transcript' AND payload->>'call_id' = $2)`, c.ThreadID, c.ID).Scan(&exists); err != nil || exists {
			return err
		}
		return event(ctx, tx, c, "voice.transcript", map[string]any{"call_id": c.ID, "text": text}, len(untrusted.SuspiciousReasons(text, "")) > 0)
	})
}

func event(ctx context.Context, tx pgx.Tx, c Call, typ string, payload map[string]any, suspicious bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, provider, untrusted, suspected_injection)
		VALUES ($1, $2, $3, $4, 'voice', $5, 'signalwire_voice', $6, $7)`,
		c.WorkspaceID, c.ThreadID, c.ContactID, typ, payload, typ == "voice.transcript", suspicious)
	return err
}
