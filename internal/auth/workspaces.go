package auth

import (
	"context"
	"encoding/json"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Membership is one workspace a signed-in person can switch to.
type Membership struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Current     bool   `json:"current"`
}

// Workspaces lists every workspace the person's login belongs to, the
// current one included.
func (s *Service) Workspaces(ctx context.Context, p Person) ([]Membership, error) {
	rows, err := s.DB.Query(ctx, `SELECT w.id, w.name, q.id = $1
		FROM principals me JOIN principals q ON q.id = me.id OR q.login_id = me.login_id
		JOIN workspaces w ON w.id = q.workspace_id
		WHERE me.id = $1 AND q.kind = 'human' AND q.revoked_at IS NULL
		ORDER BY lower(w.name), w.id`, p.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Membership])
}

// Switch moves a session to the same login's person in another workspace.
func (s *Service) Switch(ctx context.Context, token, workspaceID string) (Person, error) {
	var p Person
	err := s.DB.QueryRow(ctx, `WITH target AS (
			SELECT q.id, q.workspace_id, q.name, coalesce(q.email, '') AS email
			FROM sessions s JOIN principals me ON me.id = s.principal_id
			JOIN principals q ON q.login_id = me.login_id
			WHERE s.token_sha256 = $1 AND s.expires_at > now() AND me.revoked_at IS NULL
			  AND q.workspace_id::text = $2 AND q.kind = 'human' AND q.revoked_at IS NULL)
		UPDATE sessions s SET principal_id = target.id, last_seen_at = now() FROM target
		WHERE s.token_sha256 = $1
		RETURNING target.id, target.workspace_id, target.name, target.email`, digest(token), workspaceID).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Email)
	if db.NotFound(err) {
		return p, ErrNoWorkspace
	}
	return p, err
}

// AcceptInvite joins the invite's workspace as the signed-in person, under
// the same login, without a new passkey, and switches the session to it.
func (s *Service) AcceptInvite(ctx context.Context, token, inviteToken string) (Person, error) {
	var p Person
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var loginID *string
		if err := tx.QueryRow(ctx, `SELECT me.login_id FROM sessions s JOIN principals me ON me.id = s.principal_id
			WHERE s.token_sha256 = $1 AND s.expires_at > now() AND me.revoked_at IS NULL FOR UPDATE OF s`, digest(token)).
			Scan(&loginID); err != nil {
			return ErrDenied
		}
		if loginID == nil {
			return ErrDenied
		}
		err := tx.QueryRow(ctx, `SELECT workspace_id, name, email FROM invites
			WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now() FOR UPDATE`, digest(inviteToken)).
			Scan(&p.WorkspaceID, &p.Name, &p.Email)
		if db.NotFound(err) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE login_id = $1 AND workspace_id = $2)`,
			*loginID, p.WorkspaceID).Scan(&member); err != nil {
			return err
		}
		if member {
			return ErrAlreadyMember
		}
		if err := tx.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name, email, login_id)
			VALUES ($1, 'human', $2, $3, $4) RETURNING id`, p.WorkspaceID, p.Name, p.Email, *loginID).Scan(&p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invites SET used_at = now(), principal_id = $2 WHERE token_sha256 = $1`,
			digest(inviteToken), p.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sessions SET principal_id = $2 WHERE token_sha256 = $1`, digest(token), p.ID)
		return err
	})
	return p, err
}

// FinishLink checks a passkey from another workspace, started with
// BeginLogin, and joins its login to the signed-in person's: holding both
// passkeys shows they are the same human. The session stays where it is.
func (s *Service) FinishLink(ctx context.Context, token, ceremonyID string, response json.RawMessage) ([]Membership, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return nil, ErrDenied
	}
	var me Person
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var mine *string
		if err := tx.QueryRow(ctx, `SELECT me.id, me.login_id FROM sessions s JOIN principals me ON me.id = s.principal_id
			WHERE s.token_sha256 = $1 AND s.expires_at > now() AND me.revoked_at IS NULL`, digest(token)).
			Scan(&me.ID, &mine); err != nil || mine == nil {
			return ErrDenied
		}
		other, err := s.validatePasskey(ctx, tx, ceremonyID, parsed)
		if err != nil {
			return err
		}
		var theirs *string
		if err := tx.QueryRow(ctx, `SELECT login_id FROM principals WHERE id = $1`, other.ID).Scan(&theirs); err != nil {
			return err
		}
		if theirs == nil {
			// A person made before logins existed; give them this login.
			var clash bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE login_id = $1 AND workspace_id = $2)`,
				*mine, other.WorkspaceID).Scan(&clash); err != nil {
				return err
			}
			if clash {
				return ErrAlreadyMember
			}
			_, err := tx.Exec(ctx, `UPDATE principals SET login_id = $2 WHERE id = $1`, other.ID, *mine)
			return err
		}
		if *theirs == *mine {
			return ErrAlreadyMember
		}
		var clash bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals a JOIN principals b ON a.workspace_id = b.workspace_id
			WHERE a.login_id = $1 AND b.login_id = $2)`, *mine, *theirs).Scan(&clash); err != nil {
			return err
		}
		if clash {
			return ErrAlreadyMember
		}
		if _, err := tx.Exec(ctx, `UPDATE principals SET login_id = $1 WHERE login_id = $2`, *mine, *theirs); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM logins WHERE id = $1`, *theirs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Workspaces(ctx, me)
}
