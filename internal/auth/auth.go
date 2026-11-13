// Package auth signs people in with passkeys. There are no passwords: a
// person is invited, registers a passkey from the invite link, and signs in
// with it from then on. Passkeys are discoverable, so sign-in needs no email.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

const (
	InviteTTL    = 7 * 24 * time.Hour
	SessionTTL   = 30 * 24 * time.Hour
	ceremonyTTL  = 5 * time.Minute
	tokenEntropy = 32
)

var (
	// ErrDenied covers every failed ceremony or unknown session alike, so a
	// caller learns nothing about which part failed.
	ErrDenied = errors.New("not signed in")
	// ErrInviteInvalid means the invite is unknown, used or expired.
	ErrInviteInvalid = errors.New("this invite link is no longer valid")
	// ErrAlreadyMember means the signed-in person already belongs to the
	// workspace they tried to join or link.
	ErrAlreadyMember = errors.New("you already belong to that workspace")
	// ErrNoWorkspace means the signed-in person cannot switch to that
	// workspace.
	ErrNoWorkspace = errors.New("you don't belong to that workspace")
)

// Config is the relying party: the site passkeys are bound to.
type Config struct {
	// RPID is the domain passkeys belong to, e.g. "visicomms.app".
	RPID string
	// Origins are the page origins allowed to run ceremonies, e.g.
	// "https://app.visicomms.app".
	Origins []string
}

type Service struct {
	DB       *pgxpool.Pool
	webauthn *webauthn.WebAuthn
}

func New(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	w, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: "VisiComms",
		RPOrigins:     cfg.Origins,
	})
	if err != nil {
		return nil, fmt.Errorf("passkey config: %w", err)
	}
	return &Service{DB: pool, webauthn: w}, nil
}

// Person is a signed-in human.
type Person struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
}

// Invite is shown on the invite page before a passkey is made.
type Invite struct {
	WorkspaceName string `json:"workspace_name"`
	Name          string `json:"name"`
	Email         string `json:"email"`
}

// CreateInvite returns a one-time token for name and email to join a
// workspace. Only its hash is stored.
func (s *Service) CreateInvite(ctx context.Context, workspaceID, email, name string, createdBy *string) (string, error) {
	token := randomToken()
	_, err := s.DB.Exec(ctx, `INSERT INTO invites (token_sha256, workspace_id, email, name, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		digest(token), workspaceID, strings.ToLower(strings.TrimSpace(email)), strings.TrimSpace(name), createdBy,
		time.Now().Add(InviteTTL))
	return token, err
}

// LookupInvite describes a usable invite.
func (s *Service) LookupInvite(ctx context.Context, token string) (Invite, error) {
	var in Invite
	err := s.DB.QueryRow(ctx, `SELECT w.name, i.name, i.email FROM invites i JOIN workspaces w ON w.id = i.workspace_id
		WHERE i.token_sha256 = $1 AND i.used_at IS NULL AND i.expires_at > now()`, digest(token)).
		Scan(&in.WorkspaceName, &in.Name, &in.Email)
	if db.NotFound(err) {
		return in, ErrInviteInvalid
	}
	return in, err
}

// person adapts a principal to go-webauthn.
type person struct {
	id          []byte
	name, email string
	creds       []webauthn.Credential
}

func (p person) WebAuthnID() []byte                         { return p.id }
func (p person) WebAuthnName() string                       { return p.email }
func (p person) WebAuthnDisplayName() string                { return p.name }
func (p person) WebAuthnCredentials() []webauthn.Credential { return p.creds }

// Ceremony is what the browser needs to run navigator.credentials.
type Ceremony struct {
	ID      string `json:"ceremony_id"`
	Options any    `json:"options"`
}

// BeginRegistration starts making a passkey for an invite.
func (s *Service) BeginRegistration(ctx context.Context, inviteToken string) (Ceremony, error) {
	in, err := s.LookupInvite(ctx, inviteToken)
	if err != nil {
		return Ceremony{}, err
	}
	user := person{id: randomBytes(tokenEntropy), name: in.Name, email: in.Email}
	options, session, err := s.webauthn.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		return Ceremony{}, err
	}
	id, err := s.saveCeremony(ctx, s.DB, "register", session, digest(inviteToken))
	return Ceremony{ID: id, Options: options}, err
}

// FinishRegistration stores the new passkey, creates the person and signs
// them in. The invite is used up in the same transaction.
func (s *Service) FinishRegistration(ctx context.Context, ceremonyID string, response json.RawMessage) (Person, string, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return Person{}, "", ErrDenied
	}
	var p Person
	var token string
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		session, inviteHash, err := s.consumeCeremony(ctx, tx, ceremonyID, "register")
		if err != nil {
			return err
		}
		var workspaceID, email, name string
		err = tx.QueryRow(ctx, `SELECT workspace_id, email, name FROM invites
			WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now() FOR UPDATE`, inviteHash).
			Scan(&workspaceID, &email, &name)
		if db.NotFound(err) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		user := person{id: session.UserID, name: name, email: email}
		cred, err := s.webauthn.CreateCredential(user, session, parsed)
		if err != nil {
			return ErrDenied
		}
		if err := tx.QueryRow(ctx, `WITH l AS (INSERT INTO logins DEFAULT VALUES RETURNING id)
			INSERT INTO principals (workspace_id, kind, name, email, webauthn_user_id, login_id)
			SELECT $1, 'human', $2, $3, $4, l.id FROM l RETURNING id`, workspaceID, name, email, session.UserID).Scan(&p.ID); err != nil {
			return err
		}
		if err := insertPasskey(ctx, tx, p.ID, cred); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invites SET used_at = now(), principal_id = $2 WHERE token_sha256 = $1`,
			inviteHash, p.ID); err != nil {
			return err
		}
		p.WorkspaceID, p.Name, p.Email = workspaceID, name, email
		token, err = createSession(ctx, tx, p.ID)
		return err
	})
	return p, token, err
}

// BeginLogin starts a sign-in with any passkey for this site.
func (s *Service) BeginLogin(ctx context.Context) (Ceremony, error) {
	options, session, err := s.webauthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return Ceremony{}, err
	}
	id, err := s.saveCeremony(ctx, s.DB, "login", session, nil)
	return Ceremony{ID: id, Options: options}, err
}

// FinishLogin checks the passkey's signature and opens a session.
func (s *Service) FinishLogin(ctx context.Context, ceremonyID string, response json.RawMessage) (Person, string, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return Person{}, "", ErrDenied
	}
	var p Person
	var token string
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if p, err = s.validatePasskey(ctx, tx, ceremonyID, parsed); err != nil {
			return err
		}
		token, err = createSession(ctx, tx, p.ID)
		return err
	})
	return p, token, err
}

// validatePasskey answers a login ceremony: it checks the signature, uses up
// the challenge and returns whose passkey it was.
func (s *Service) validatePasskey(ctx context.Context, tx pgx.Tx, ceremonyID string, parsed *protocol.ParsedCredentialAssertionData) (Person, error) {
	var p Person
	session, _, err := s.consumeCeremony(ctx, tx, ceremonyID, "login")
	if err != nil {
		return p, err
	}
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		return loadPerson(ctx, tx, userHandle, &p)
	}
	_, cred, err := s.webauthn.ValidatePasskeyLogin(handler, session, parsed)
	if err != nil {
		return p, ErrDenied
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return p, err
	}
	// The sign count moves on every use; storing it lets a cloned
	// authenticator be noticed.
	_, err = tx.Exec(ctx, `UPDATE passkeys SET credential = $2, last_used_at = now() WHERE credential_id = $1`, cred.ID, data)
	return p, err
}

func loadPerson(ctx context.Context, tx pgx.Tx, handle []byte, p *Person) (webauthn.User, error) {
	err := tx.QueryRow(ctx, `SELECT id, workspace_id, name, coalesce(email, '') FROM principals
		WHERE webauthn_user_id = $1 AND kind = 'human' AND revoked_at IS NULL`, handle).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Email)
	if err != nil {
		return nil, ErrDenied
	}
	rows, err := tx.Query(ctx, `SELECT credential FROM passkeys WHERE principal_id = $1`, p.ID)
	if err != nil {
		return nil, err
	}
	creds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (webauthn.Credential, error) {
		var c webauthn.Credential
		var raw []byte
		if err := r.Scan(&raw); err != nil {
			return c, err
		}
		return c, json.Unmarshal(raw, &c)
	})
	if err != nil {
		return nil, err
	}
	return person{id: handle, name: p.Name, email: p.Email, creds: creds}, nil
}

func insertPasskey(ctx context.Context, tx pgx.Tx, principalID string, cred *webauthn.Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO passkeys (principal_id, credential_id, credential) VALUES ($1, $2, $3)`,
		principalID, cred.ID, data)
	return err
}

func (s *Service) saveCeremony(ctx context.Context, q db.Querier, kind string, session *webauthn.SessionData, inviteHash []byte) (string, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	var id string
	err = q.QueryRow(ctx, `INSERT INTO auth_ceremonies (kind, session, invite_token_sha256, expires_at)
		VALUES ($1, $2, $3, $4) RETURNING id`, kind, data, inviteHash, time.Now().Add(ceremonyTTL)).Scan(&id)
	return id, err
}

// consumeCeremony deletes the ceremony as it reads it, so a challenge can
// only ever be answered once.
func (s *Service) consumeCeremony(ctx context.Context, tx pgx.Tx, id, kind string) (webauthn.SessionData, []byte, error) {
	var session webauthn.SessionData
	var raw, inviteHash []byte
	err := tx.QueryRow(ctx, `DELETE FROM auth_ceremonies WHERE id = $1 AND kind = $2 AND expires_at > now()
		RETURNING session, invite_token_sha256`, id, kind).Scan(&raw, &inviteHash)
	if err != nil {
		return session, nil, ErrDenied
	}
	return session, inviteHash, json.Unmarshal(raw, &session)
}

func createSession(ctx context.Context, q db.Querier, principalID string) (string, error) {
	token := randomToken()
	_, err := q.Exec(ctx, `INSERT INTO sessions (token_sha256, principal_id, expires_at) VALUES ($1, $2, $3)`,
		digest(token), principalID, time.Now().Add(SessionTTL))
	return token, err
}

// Session returns the person a session token belongs to.
func (s *Service) Session(ctx context.Context, token string) (Person, error) {
	var p Person
	err := s.DB.QueryRow(ctx, `UPDATE sessions s SET last_seen_at = now()
		FROM principals p
		WHERE s.token_sha256 = $1 AND s.expires_at > now() AND p.id = s.principal_id AND p.revoked_at IS NULL
		RETURNING p.id, p.workspace_id, p.name, coalesce(p.email, '')`, digest(token)).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Email)
	if err != nil {
		return p, ErrDenied
	}
	return p, nil
}

// EndSession signs a session out.
func (s *Service) EndSession(ctx context.Context, token string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM sessions WHERE token_sha256 = $1`, digest(token))
	return err
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randomToken() string { return base64.RawURLEncoding.EncodeToString(randomBytes(tokenEntropy)) }

func digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
