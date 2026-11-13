package voice

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
)

// Entrance is a building's door intercom. A visitor pressing its button
// calls the workspace, and the call runs the entrance's flow, not the
// number's: the AI greets them as someone standing at the gate.
//
// A call is from the entrance when it comes from the entrance's fixed caller
// ID (an on-site bridge dialling one of the workspace's numbers), or from
// the entrance's SIP account (the intercom calling SignalWire directly).
// Nothing a visitor says is used to decide.
type Entrance struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CallerID  string `json:"caller_id,omitempty"`
	Flow      Flow   `json:"flow"`
	ContactID string `json:"contact_id,omitempty"`
	// SIP is the account the intercom signs in with, once one is made.
	SIP *SIPAccount `json:"sip,omitempty"`
}

// SIPAccount is what an intercom is set up with to call SignalWire
// directly. Password is filled in only when the account is just made: it is
// shown once and never stored.
type SIPAccount struct {
	Server    string `json:"server"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
	Username  string `json:"username"`
	Password  string `json:"password,omitempty"`
	// Recipient is the number the intercom is told to dial. SignalWire hands
	// the call to VisiComms whatever it is.
	Recipient string `json:"recipient"`
}

// SIP sign-in settings that every intercom is given.
const (
	SIPPort      = 5060
	SIPTransport = "UDP"
	SIPRecipient = "1000"
)

var ErrUnknownEntrance = errors.New("no such entrance in this workspace")

// ErrCallerIDTaken means another entrance in the workspace already uses the
// caller ID.
var ErrCallerIDTaken = errors.New("another entrance already has this caller ID")

// DefaultEntranceFlow is how a new entrance answers: the AI, as a gatekeeper,
// when this server can run it; otherwise the visitor leaves a message.
func DefaultEntranceFlow() Flow {
	f := Flow{
		Answer:      Action{Mode: Voicemail},
		RingSeconds: 20,
		Greeting:    "Sorry, nobody can come to the gate right now. Please leave a message after the tone.",
	}
	if Available[AI] {
		f.Answer = Action{Mode: AI}
		f.Agent = &Agent{
			Opening: "Hello, you've reached the front gate. Who are you here to see?",
			Instructions: "Delivery drivers: ask them to leave packages at the gate and to contact the customer through their app.\n" +
				"Guests and vendors: take a message with their name, who they are here for, and why.",
			Voice: defaultVoice,
		}
	}
	return f
}

func (e *Entrance) validate() error {
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" || utf8.RuneCountInString(e.Name) > 80 {
		return invalid("name is required, at most 80 characters")
	}
	e.CallerID = strings.TrimSpace(e.CallerID)
	if e.CallerID != "" {
		n, err := phone.Normalize(e.CallerID)
		if err != nil {
			return invalid("caller_id: %v", err)
		}
		e.CallerID = n
	}
	return e.Flow.Validate()
}

const entranceColumns = `e.id::text, e.name, coalesce(e.caller_id, ''), e.flow, e.contact_id::text,
	coalesce(e.sip_username, ''), coalesce(e.sip_server, '')`

func scanEntrance(row pgx.Row) (Entrance, error) {
	var e Entrance
	var user, server string
	if err := row.Scan(&e.ID, &e.Name, &e.CallerID, &e.Flow, &e.ContactID, &user, &server); err != nil {
		return e, err
	}
	if user != "" {
		e.SIP = &SIPAccount{Server: server, Port: SIPPort, Transport: SIPTransport, Username: user, Recipient: SIPRecipient}
	}
	return e, nil
}

// Entrances lists a workspace's entrances.
func Entrances(ctx context.Context, q db.Querier, workspaceID string) ([]Entrance, error) {
	rows, err := q.Query(ctx, `SELECT `+entranceColumns+` FROM entrances e WHERE e.workspace_id = $1 ORDER BY e.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entrance, error) { return scanEntrance(r) })
}

// GetEntrance loads one of a workspace's entrances.
func GetEntrance(ctx context.Context, q db.Querier, workspaceID, id string) (Entrance, error) {
	e, err := scanEntrance(q.QueryRow(ctx, `SELECT `+entranceColumns+` FROM entrances e WHERE e.workspace_id = $1 AND e.id::text = $2`, workspaceID, id))
	if db.NotFound(err) {
		return e, ErrUnknownEntrance
	}
	return e, err
}

// CreateEntrance adds an entrance. Its calls go on one contact named after
// it: the contact that already called from its caller ID, if any, so the
// gate's earlier calls stay on the same thread.
func CreateEntrance(ctx context.Context, pool *pgxpool.Pool, workspaceID string, e Entrance) (Entrance, error) {
	if err := e.validate(); err != nil {
		return e, err
	}
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if err := callerIDFree(ctx, tx, workspaceID, "", e.CallerID); err != nil {
			return err
		}
		contact, err := entranceContact(ctx, tx, workspaceID, e.Name, e.CallerID, "")
		if err != nil {
			return err
		}
		e.ContactID = contact
		return tx.QueryRow(ctx, `INSERT INTO entrances (workspace_id, contact_id, name, caller_id, flow) VALUES ($1, $2, $3, NULLIF($4, ''), $5) RETURNING id::text`,
			workspaceID, contact, e.Name, e.CallerID, e.Flow).Scan(&e.ID)
	})
	return e, err
}

// UpdateEntrance changes an entrance's name, caller ID and flow. Its SIP
// account is unchanged.
func UpdateEntrance(ctx context.Context, pool *pgxpool.Pool, workspaceID, id string, in Entrance) (Entrance, error) {
	if err := in.validate(); err != nil {
		return in, err
	}
	var out Entrance
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		current, err := GetEntrance(ctx, tx, workspaceID, id)
		if err != nil {
			return err
		}
		if err := callerIDFree(ctx, tx, workspaceID, id, in.CallerID); err != nil {
			return err
		}
		if _, err := entranceContact(ctx, tx, workspaceID, in.Name, in.CallerID, current.ContactID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE entrances SET name = $3, caller_id = NULLIF($4, ''), flow = $5, updated_at = now() WHERE workspace_id = $1 AND id::text = $2`,
			workspaceID, id, in.Name, in.CallerID, in.Flow); err != nil {
			return err
		}
		out, err = GetEntrance(ctx, tx, workspaceID, id)
		return err
	})
	return out, err
}

func callerIDFree(ctx context.Context, tx pgx.Tx, workspaceID, id, callerID string) error {
	if callerID == "" {
		return nil
	}
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM entrances WHERE workspace_id = $1 AND caller_id = $2 AND id::text <> $3)`, workspaceID, callerID, id).Scan(&taken)
	if err == nil && taken {
		err = ErrCallerIDTaken
	}
	return err
}

// entranceContact names the entrance's contact after it, and gives it the
// caller ID as a calling address. With no contact yet, it is the one who
// already called from the caller ID, or a new one.
func entranceContact(ctx context.Context, tx pgx.Tx, workspaceID, name, callerID, contact string) (string, error) {
	if contact == "" && callerID != "" {
		err := tx.QueryRow(ctx, `SELECT contact_id::text FROM identities WHERE workspace_id = $1 AND channel = 'voice' AND address = $2`, workspaceID, callerID).Scan(&contact)
		if err != nil && !db.NotFound(err) {
			return "", err
		}
	}
	if contact == "" {
		if err := tx.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, $2) RETURNING id::text`, workspaceID, name).Scan(&contact); err != nil {
			return "", err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE contacts SET display_name = $2, updated_at = now() WHERE id::text = $1`, contact, name); err != nil {
		return "", err
	}
	if callerID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address) VALUES ($1, $2, 'voice', $3)
			ON CONFLICT (workspace_id, channel, address) DO NOTHING`, workspaceID, contact, callerID); err != nil {
			return "", err
		}
	}
	return contact, nil
}

// DeleteEntrance removes an entrance. Its thread and contact stay. It returns
// the SIP endpoint to remove at SignalWire, if it had one.
func DeleteEntrance(ctx context.Context, pool *pgxpool.Pool, workspaceID, id string) (endpointID, project string, err error) {
	err = pool.QueryRow(ctx, `DELETE FROM entrances WHERE workspace_id = $1 AND id::text = $2
		RETURNING coalesce(sip_endpoint_id, ''), coalesce(provider_project_id, '')`, workspaceID, id).Scan(&endpointID, &project)
	if db.NotFound(err) {
		err = ErrUnknownEntrance
	}
	return endpointID, project, err
}

// SetEntranceSIP records the entrance's SIP account, returning the one it
// replaces (to remove at SignalWire), if any.
func SetEntranceSIP(ctx context.Context, pool *pgxpool.Pool, workspaceID, id, username, endpointID, project, server string) (oldEndpoint, oldProject string, err error) {
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT coalesce(sip_endpoint_id, ''), coalesce(provider_project_id, '') FROM entrances WHERE workspace_id = $1 AND id::text = $2 FOR UPDATE`,
			workspaceID, id).Scan(&oldEndpoint, &oldProject)
		if db.NotFound(err) {
			return ErrUnknownEntrance
		}
		if err != nil {
			return err
		}
		var user, endpoint, proj, srv *string
		if username != "" {
			user, endpoint, proj, srv = &username, &endpointID, &project, &server
		}
		_, err = tx.Exec(ctx, `UPDATE entrances SET sip_username = $3, sip_endpoint_id = $4, provider_project_id = $5, sip_server = $6, updated_at = now()
			WHERE workspace_id = $1 AND id::text = $2`, workspaceID, id, user, endpoint, proj, srv)
		return err
	})
	return oldEndpoint, oldProject, err
}

// sipUser is the account name in a SIP address such as
// sip:gate-abc@example.sip.signalwire.com.
var sipAddress = regexp.MustCompile(`^sips?:([A-Za-z0-9._-]{1,64})@[A-Za-z0-9.-]+(:\d+)?(;.*)?$`)

func sipUser(from string) (string, bool) {
	m := sipAddress.FindStringSubmatch(strings.TrimSpace(from))
	if m == nil {
		return "", false
	}
	return m[1], true
}
