// Package agent is what an outside agent can see and do: read the inbox,
// draft replies for a person to approve, resolve or escalate a thread. An
// agent is a principal with a bearer token; the token decides which workspace
// it works in, and nothing it sends goes out without the policy that holds
// every draft for a person, unless a person allowed that token to send.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// TokenPrefix marks a VisiComms agent token, so a leaked one is recognisable
// in logs and secret scanners.
const TokenPrefix = "vca_"

var (
	// ErrDenied covers a missing, unknown or revoked token alike.
	ErrDenied = errors.New("invalid or revoked agent token")
	// ErrNotFound is a thread, proposal or agent outside the workspace.
	ErrNotFound = errors.New("not found")
	ErrBadName  = errors.New("give the agent a name")
)

// Agent is an agent principal. The token itself is never stored or returned
// after creation.
type Agent struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Autonomy    string `json:"autonomy"`
	// MaySend is a person's permission for this token to send new messages
	// over REST without approval.
	MaySend    bool       `json:"may_send"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Create makes an agent in the workspace and returns its token, which is
// shown once. New agents start in approve mode: everything they draft waits
// for a person.
func Create(ctx context.Context, q db.Querier, workspaceID, name string, createdBy *string) (Agent, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Agent{}, "", ErrBadName
	}
	token := TokenPrefix + randomToken()
	var a Agent
	err := q.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name, token_sha256, autonomy, created_by)
		VALUES ($1, 'agent', $2, $3, 'approve', $4)
		RETURNING id, workspace_id, name, autonomy, created_at`, workspaceID, name, digest(token), createdBy).
		Scan(&a.ID, &a.WorkspaceID, &a.Name, &a.Autonomy, &a.CreatedAt)
	return a, token, err
}

// List is the workspace's token-holding agents, newest first.
func List(ctx context.Context, q db.Querier, workspaceID string) ([]Agent, error) {
	rows, err := q.Query(ctx, `SELECT id, workspace_id, name, autonomy, may_send, created_at, last_used_at, revoked_at
		FROM principals WHERE workspace_id = $1 AND kind = 'agent' AND (token_sha256 IS NOT NULL OR oauth_grant_id IS NOT NULL)
		ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Name, &a.Autonomy, &a.MaySend, &a.CreatedAt, &a.LastUsedAt, &a.RevokedAt); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// Revoke stops an agent at once. Its drafts stay for a person to decide, and
// its history stays attributed to it.
func Revoke(ctx context.Context, q db.Querier, workspaceID, agentID string) error {
	if !validID(agentID) {
		return ErrNotFound
	}
	tag, err := q.Exec(ctx, `UPDATE principals SET revoked_at = coalesce(revoked_at, now())
		WHERE id = $1 AND workspace_id = $2 AND kind = 'agent'`, agentID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate finds the agent a bearer token belongs to.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, token string) (Agent, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return Agent{}, ErrDenied
	}
	var a Agent
	// last_used_at is for people, so a minute's precision is enough and a busy
	// agent doesn't rewrite its row on every call.
	err := pool.QueryRow(ctx, `UPDATE principals SET last_used_at = CASE
			WHEN last_used_at IS NULL OR last_used_at < now() - interval '1 minute' THEN now() ELSE last_used_at END
		WHERE token_sha256 = $1 AND kind = 'agent' AND revoked_at IS NULL
		RETURNING id, workspace_id, name, autonomy, may_send, created_at, last_used_at`, digest(token)).
		Scan(&a.ID, &a.WorkspaceID, &a.Name, &a.Autonomy, &a.MaySend, &a.CreatedAt, &a.LastUsedAt)
	if db.NotFound(err) {
		return Agent{}, ErrDenied
	}
	return a, err
}

// validID reports whether id could be a row id, so a malformed one is "not
// found" rather than a database error.
func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
