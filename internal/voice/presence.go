package voice

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// presenceWindow is how recently a browser must have checked in to be rung.
const presenceWindow = 75 * time.Second

// Resource is the name a person's browser phone registers as at SignalWire.
func Resource(principalID string) string { return "visicomms-" + principalID }

// SetPresence records whether a person's browser phone can take calls.
func SetPresence(ctx context.Context, q db.Querier, workspaceID, principalID string, online bool) error {
	_, err := q.Exec(ctx, `INSERT INTO phone_presence (principal_id, workspace_id, online) VALUES ($1, $2, $3)
		ON CONFLICT (principal_id) DO UPDATE SET workspace_id = $2, online = $3, seen_at = now()`, principalID, workspaceID, online)
	return err
}

// Online lists the browser phones to ring for a workspace's call.
func Online(ctx context.Context, q db.Querier, workspaceID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT pp.principal_id::text FROM phone_presence pp JOIN principals p ON p.id = pp.principal_id
		WHERE pp.workspace_id = $1 AND pp.online AND pp.seen_at > now() - make_interval(secs => $2) AND p.revoked_at IS NULL
		ORDER BY pp.principal_id`, workspaceID, presenceWindow.Seconds())
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for i := range ids {
		ids[i] = Resource(ids[i])
	}
	return ids, err
}

// Ringing is a live call in the workspace, for the console's phone to show
// who is calling.
type Ringing struct {
	CallID   string    `json:"call_id"`
	ThreadID string    `json:"thread_id"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Contact  string    `json:"contact"`
	Started  time.Time `json:"started_at"`
}

// Live lists calls in progress, newest first.
func Live(ctx context.Context, q db.Querier, workspaceID string) ([]Ringing, error) {
	rows, err := q.Query(ctx, `SELECT c.id::text, c.thread_id::text, c.remote, coalesce(c.number, ''), coalesce(ct.display_name, ''), c.started_at
		FROM calls c LEFT JOIN contacts ct ON ct.id = c.contact_id
		WHERE c.workspace_id = $1 AND c.outcome IS NULL AND c.ai_call_id IS NULL AND c.started_at > now() - interval '5 minutes'
		ORDER BY c.started_at DESC LIMIT 5`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Ringing])
}
