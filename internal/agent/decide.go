package agent

import (
	"context"
	"errors"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// ErrNoPerson is an agent with no person behind it who may decide.
var ErrNoPerson = errors.New("only a connection a person made with OAuth can decide, and only while they are a member of this workspace; ask them to decide in the console")

// Person is who an OAuth connection acts for in the agent's workspace: the
// member who approved the connection, or their membership here under the
// same login. Agent tokens have no person, so they cannot decide anything.
func Person(ctx context.Context, q db.Querier, a Agent) (id, name string, err error) {
	err = q.QueryRow(ctx, `SELECT h.id, h.name FROM principals ag
		JOIN oauth_grants g ON g.id = ag.oauth_grant_id AND g.revoked_at IS NULL
		JOIN principals ap ON ap.id = g.approved_by
		JOIN principals h ON h.workspace_id = ag.workspace_id AND h.kind = 'human' AND h.revoked_at IS NULL
			AND (h.id = ap.id OR h.login_id = ap.login_id)
		WHERE ag.id = $1 AND ag.workspace_id = $2
		ORDER BY h.id = ap.id DESC LIMIT 1`, a.ID, a.WorkspaceID).Scan(&id, &name)
	if db.NotFound(err) {
		return "", "", ErrNoPerson
	}
	return id, name, err
}
