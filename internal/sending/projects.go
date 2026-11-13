package sending

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/secrets"
)

// ErrNoSignalWire means a workspace has no project of its own and the
// installation has none to fall back on.
var ErrNoSignalWire = errors.New("SignalWire is not connected for this workspace")

// ErrUnknownProject means a callback or number names a project that is neither
// the installation's nor any workspace's.
var ErrUnknownProject = errors.New("unknown SignalWire project")

// SignalWireProjects finds the project a workspace, callback or queued send
// uses. A workspace's own project is a signalwire_project connector whose token
// and signing key are sealed with the workspace data key. A workspace without
// one uses the installation's SIGNALWIRE_* project, which is how a
// single-business self-host runs. A nil *SignalWireProjects has no projects.
type SignalWireProjects struct {
	DB *pgxpool.Pool
	// Keys opens workspace credentials; nil means only the installation
	// project is available.
	Keys *secrets.Keyring
	// Installation is the SIGNALWIRE_* project; nil when unset.
	Installation *SignalWire
	// PublicURL is the origin SignalWire calls back; every project shares it.
	PublicURL string
	// HTTP may be replaced by tests.
	HTTP *http.Client
}

type signalWireCredentials struct {
	Token      string `json:"token"`
	SigningKey string `json:"signing_key"`
}

// Webhooks carries only the public URL, for showing the callback URLs.
func (p *SignalWireProjects) Webhooks() *SignalWire { return &SignalWire{PublicURL: p.PublicURL} }

// ForProject returns the client for a project and the workspace that owns it,
// "" for the installation project.
func (p *SignalWireProjects) ForProject(ctx context.Context, project string) (*SignalWire, string, error) {
	if p == nil || project == "" {
		return nil, "", ErrUnknownProject
	}
	if p.Installation != nil && project == p.Installation.Project {
		return p.Installation, "", nil
	}
	if p.Keys == nil {
		return nil, "", ErrUnknownProject
	}
	var id, workspaceID, space string
	var sealed []byte
	err := p.DB.QueryRow(ctx, `SELECT id, workspace_id, settings->>'space', credentials_ciphertext FROM connectors
		WHERE kind = 'signalwire_project' AND settings->>'project_id' = $1`, project).Scan(&id, &workspaceID, &space, &sealed)
	if db.NotFound(err) {
		return nil, "", ErrUnknownProject
	}
	if err != nil {
		return nil, "", err
	}
	plaintext, err := p.Keys.Open(ctx, p.DB, workspaceID, id, sealed)
	if err != nil {
		return nil, "", err
	}
	var creds signalWireCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, "", err
	}
	sw, err := NewSignalWire(space, project, creds.Token, creds.SigningKey, p.PublicURL)
	if err != nil {
		return nil, "", err
	}
	sw.HTTP = p.HTTP
	return sw, workspaceID, nil
}

// ProjectID is the project a workspace sends and receives through, without
// opening any credentials.
func (p *SignalWireProjects) ProjectID(ctx context.Context, workspaceID string) (string, error) {
	if p == nil {
		return "", ErrNoSignalWire
	}
	var project string
	err := p.DB.QueryRow(ctx, `SELECT settings->>'project_id' FROM connectors
		WHERE workspace_id = $1 AND kind = 'signalwire_project'`, workspaceID).Scan(&project)
	switch {
	case err == nil:
		return project, nil
	case !db.NotFound(err):
		return "", err
	case p.Installation != nil:
		return p.Installation.Project, nil
	}
	return "", ErrNoSignalWire
}

// ForWorkspace returns the client for the workspace's project.
func (p *SignalWireProjects) ForWorkspace(ctx context.Context, workspaceID string) (*SignalWire, error) {
	project, err := p.ProjectID(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	sw, owner, err := p.ForProject(ctx, project)
	if err != nil {
		return nil, err
	}
	if owner != "" && owner != workspaceID {
		return nil, ErrUnknownProject
	}
	return sw, nil
}

// ConnectProject stores a workspace's own SignalWire project, after checking
// the credentials against SignalWire. Reconnecting the same project replaces
// its token and signing key and keeps its recorded brand and campaigns; switching a workspace to another project is
// refused while numbers are still connected through the old one.
func (p *SignalWireProjects) ConnectProject(ctx context.Context, workspaceID string, sw *SignalWire) error {
	if p.Keys == nil {
		return errors.New("set VISICOMMS_MASTER_KEY before connecting a workspace's own SignalWire project")
	}
	if p.Installation != nil && sw.Project == p.Installation.Project {
		return errors.New("this is the installation's SignalWire project; workspaces without their own project already use it")
	}
	// Checked with a call within a workspace token's own scope (numbers):
	// reading the account itself needs Management, which it should not have.
	var page struct {
		Numbers []struct {
			AccountSID string `json:"account_sid"`
		} `json:"incoming_phone_numbers"`
	}
	if err := sw.request(ctx, http.MethodGet, "/IncomingPhoneNumbers.json?PageSize=1", nil, &page); err != nil {
		return fmt.Errorf("checking the project's token: %w", err)
	}
	for _, n := range page.Numbers {
		if n.AccountSID != sw.Project {
			return errors.New("SignalWire did not confirm this project ID for these credentials")
		}
	}
	creds, err := json.Marshal(signalWireCredentials{Token: sw.Token, SigningKey: sw.SigningKey})
	if err != nil {
		return err
	}
	settings := map[string]string{"space": sw.Space, "project_id": sw.Project}
	err = db.Tx(ctx, p.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "signalwire-project:"+workspaceID); err != nil {
			return err
		}
		var id, current string
		err := tx.QueryRow(ctx, `SELECT id, settings->>'project_id' FROM connectors
			WHERE workspace_id = $1 AND kind = 'signalwire_project'`, workspaceID).Scan(&id, &current)
		if err != nil && !db.NotFound(err) {
			return err
		}
		if err == nil && current != sw.Project {
			var numbers int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM phone_numbers WHERE workspace_id = $1 AND provider_project_id = $2`, workspaceID, current).Scan(&numbers); err != nil {
				return err
			}
			if numbers > 0 {
				return errors.New("this workspace still has numbers connected through SignalWire project " + current)
			}
		}
		if id == "" {
			id = uuid.NewString()
		}
		sealed, err := p.Keys.Seal(ctx, tx, workspaceID, id, creds)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO connectors (id, workspace_id, kind, name, settings, credentials_ciphertext, health, last_error, checked_at)
			VALUES ($1, $2, 'signalwire_project', 'SignalWire project', $3::jsonb, $4, 'ok', NULL, now())
			ON CONFLICT (id) DO UPDATE SET settings = CASE WHEN connectors.settings->>'project_id' = $3::jsonb->>'project_id'
				THEN connectors.settings || $3::jsonb ELSE $3::jsonb END, credentials_ciphertext = $4, health = 'ok', last_error = NULL, checked_at = now(), updated_at = now()`,
			id, workspaceID, settings, sealed)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "connectors_signalwire_project" {
		return errors.New("this SignalWire project is already connected to another workspace")
	}
	return err
}
