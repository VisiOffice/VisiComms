package review

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// ErrStaleInfo is a draft of approved info made before a newer version was
// approved.
var ErrStaleInfo = errors.New("a newer version was approved after this draft was made; ask for it again on top of the current version")

type ApprovedInfo struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Content    json.RawMessage `json:"content"`
	ApprovedBy *string         `json:"approved_by,omitempty"`
	ApprovedAt *time.Time      `json:"approved_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	// PendingQuoting counts held drafts that quoted an older version; approving
	// this one voids them.
	PendingQuoting int `json:"pending_quoting"`
	// An agent's proposal: who, why, and the approved version it changes.
	ProposedBy *string `json:"proposed_by,omitempty"`
	Reason     *string `json:"reason,omitempty"`
	BasedOn    *int    `json:"based_on,omitempty"`
}

// ApprovedInfoVersions lists versions newest first.
func ApprovedInfoVersions(ctx context.Context, q db.Querier, workspaceID string) ([]ApprovedInfo, error) {
	rows, err := q.Query(ctx, `SELECT v.id, v.version, v.content, p.name, v.approved_at, v.created_at,
			CASE WHEN v.approved_at IS NULL THEN (SELECT count(*) FROM proposals x WHERE x.workspace_id = v.workspace_id
				AND x.status = 'pending' AND x.approved_info_version_id IS NOT NULL AND x.approved_info_version_id <> v.id)::int ELSE 0 END,
			CASE WHEN c.kind = 'agent' THEN c.name END, v.reason, v.based_on
		FROM approved_info_versions v LEFT JOIN principals p ON p.id = v.approved_by LEFT JOIN principals c ON c.id = v.created_by
		WHERE v.workspace_id = $1 ORDER BY v.version DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ApprovedInfo])
}

// ApproveInfo approves a draft version of approved info. Held drafts that
// quoted an older version are voided, because their approval would cover
// facts the business no longer stands behind.
//
// A draft an agent proposed on top of an approved version is refused once a
// later version has been approved, since approving it would quietly undo
// that later change.
func ApproveInfo(ctx context.Context, pool *pgxpool.Pool, r Reviewer, versionID string) (voided int, err error) {
	err = db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, r.WorkspaceID+":approved_info"); err != nil {
			return err
		}
		var stale bool
		err := tx.QueryRow(ctx, `SELECT v.based_on IS NOT NULL AND EXISTS (SELECT 1 FROM approved_info_versions n
				WHERE n.workspace_id = v.workspace_id AND n.approved_at IS NOT NULL AND n.version > v.based_on)
			FROM approved_info_versions v WHERE v.id = $1 AND v.workspace_id = $2`, versionID, r.WorkspaceID).Scan(&stale)
		if db.NotFound(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if stale {
			return ErrStaleInfo
		}
		tag, err := tx.Exec(ctx, `UPDATE approved_info_versions SET approved_by = $3, approved_at = now()
			WHERE id = $1 AND workspace_id = $2 AND approved_at IS NULL`, versionID, r.WorkspaceID, r.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		rows, err := tx.Query(ctx, `UPDATE proposals SET status = 'voided', decision_note = 'Approved info changed'
			WHERE workspace_id = $1 AND status = 'pending' AND approved_info_version_id IS NOT NULL AND approved_info_version_id <> $2
			RETURNING id, thread_id`, r.WorkspaceID, versionID)
		if err != nil {
			return err
		}
		type voidedProposal struct{ ID, ThreadID string }
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[voidedProposal])
		if err != nil {
			return err
		}
		for _, v := range list {
			if err := event(ctx, tx, r, v.ThreadID, "proposal.voided", map[string]any{
				"proposal_id": v.ID, "reason": "Approved info changed",
			}); err != nil {
				return err
			}
		}
		voided = len(list)
		return nil
	})
	return voided, err
}

type Connections struct {
	Connectors []Connector    `json:"connectors"`
	Inboxes    []string       `json:"inboxes"`
	Systems    []LinkedSystem `json:"systems"`
	Agents     []Agent        `json:"agents"`
}

type Connector struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Health    string     `json:"health"`
	LastError *string    `json:"last_error,omitempty"`
	FixOwner  *string    `json:"fix_owner,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// LinkedSystem is a business system that pushes records in.
type LinkedSystem struct {
	System     string     `json:"system"`
	Records    int        `json:"records"`
	LastPushed *time.Time `json:"last_pushed,omitempty"`
}

type Agent struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Autonomy      string     `json:"autonomy"`
	ConfigVersion *string    `json:"config_version,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	MaySend       bool       `json:"may_send"`
	// CanSend is whether sending can be turned on: tokens only, not a
	// connection a person made through claude.ai.
	CanSend bool `json:"can_send"`
}

// ListConnections is everything the workspace is connected to. Credentials
// are never read here.
func ListConnections(ctx context.Context, q db.Querier, workspaceID string) (Connections, error) {
	var c Connections
	rows, err := q.Query(ctx, `SELECT id, kind, name, health, last_error, fix_owner, checked_at
		FROM connectors WHERE workspace_id = $1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return c, err
	}
	if c.Connectors, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Connector]); err != nil {
		return c, err
	}
	rows, err = q.Query(ctx, `SELECT address FROM inbound_addresses WHERE workspace_id = $1 ORDER BY address`, workspaceID)
	if err != nil {
		return c, err
	}
	if c.Inboxes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return c, err
	}
	rows, err = q.Query(ctx, `SELECT system, count(*)::int, max(pushed_at) FROM links WHERE workspace_id = $1
		GROUP BY system ORDER BY system`, workspaceID)
	if err != nil {
		return c, err
	}
	if c.Systems, err = pgx.CollectRows(rows, pgx.RowToStructByPos[LinkedSystem]); err != nil {
		return c, err
	}
	rows, err = q.Query(ctx, `SELECT id, name, autonomy, config_version, revoked_at, may_send,
			token_sha256 IS NOT NULL AND oauth_grant_id IS NULL AND revoked_at IS NULL FROM principals
		WHERE workspace_id = $1 AND kind = 'agent' ORDER BY created_at`, workspaceID)
	if err != nil {
		return c, err
	}
	c.Agents, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Agent])
	return c, err
}

// DraftApprovedInfo adds a new version of a workspace's approved info for a
// person to approve. Nothing quotes it until they do.
func DraftApprovedInfo(ctx context.Context, pool *pgxpool.Pool, workspaceID string, content json.RawMessage) (int, error) {
	var obj map[string]any
	if err := json.Unmarshal(content, &obj); err != nil {
		return 0, errors.New("approved info must be a JSON object")
	}
	var version int
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, workspaceID+":approved_info"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO approved_info_versions (workspace_id, version, content)
			VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM approved_info_versions WHERE workspace_id = $1), $2) RETURNING version`,
			workspaceID, content).Scan(&version)
	})
	return version, err
}
