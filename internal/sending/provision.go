package sending

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// tokenPermissions are what a workspace's project token may do: send and
// receive messages, faxes and calls, and look up and buy numbers.
var tokenPermissions = []string{"messaging", "fax", "calling", "numbers"}

// Subproject is a project as SignalWire's project API returns it. SigningKey
// is present only when the project is created or its key rotated.
type Subproject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Parent     string `json:"parent_project_id"`
	SigningKey string `json:"signing_key"`
}

func (p *SignalWireParent) project(ctx context.Context, path string, input, output any) error {
	return signalWireJSON(ctx, p.HTTP, http.MethodPost, "https://"+p.Space+"/api/"+path, p.Project, p.Token, input, output)
}

// CreateSubproject makes a subproject of the parent. Its signing key is in
// this response only.
func (p *SignalWireParent) CreateSubproject(ctx context.Context, name string) (Subproject, error) {
	var s Subproject
	if err := p.project(ctx, "projects", map[string]any{"name": name}, &s); err != nil {
		return s, fmt.Errorf("creating the subproject: %w", err)
	}
	return s, p.check(s, "")
}

// RotateSigningKey replaces a subproject's signing key. The old key stops
// verifying within a minute or two, so anything else still checking callbacks
// with it (a previous system, during a move) fails from then on.
func (p *SignalWireParent) RotateSigningKey(ctx context.Context, subproject string) (Subproject, error) {
	var s Subproject
	if !signalWireProject.MatchString(subproject) {
		return s, errors.New("invalid subproject ID")
	}
	if err := p.project(ctx, "projects/"+subproject+"/signing-key/rotate", map[string]any{}, &s); err != nil {
		return s, fmt.Errorf("rotating the signing key: %w", err)
	}
	return s, p.check(s, subproject)
}

func (p *SignalWireParent) check(s Subproject, want string) error {
	switch {
	case !signalWireProject.MatchString(s.ID) || (want != "" && s.ID != want):
		return errors.New("SignalWire did not return this subproject")
	case s.Parent != p.Project:
		return errors.New("SignalWire project " + s.ID + " is not a subproject of the parent")
	case s.SigningKey == "":
		return errors.New("SignalWire did not return the subproject's signing key")
	}
	return nil
}

// CreateToken mints an API token for a subproject of the parent. The token is
// in this response only.
func (p *SignalWireParent) CreateToken(ctx context.Context, subproject, name string) (string, error) {
	if !signalWireProject.MatchString(subproject) {
		return "", errors.New("invalid subproject ID")
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := p.project(ctx, "project/tokens", map[string]any{"name": name, "permissions": tokenPermissions, "subproject_id": subproject}, &t); err != nil {
		return "", fmt.Errorf("minting the subproject's token: %w", err)
	}
	if t.Token == "" {
		return "", errors.New("SignalWire did not return the new token")
	}
	return t.Token, nil
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Provision gives a workspace a new subproject of the parent and connects it:
// SignalWire hands over the signing key and a token once, and both go
// straight into the workspace's sealed connector. Nobody sees either.
func (p *SignalWireProjects) Provision(ctx context.Context, parent *SignalWireParent, workspaceID string) (Subproject, error) {
	var name string
	var connected bool
	err := p.DB.QueryRow(ctx, `SELECT name, EXISTS (SELECT 1 FROM connectors WHERE workspace_id = w.id AND kind = 'signalwire_project')
		FROM workspaces w WHERE id = $1`, workspaceID).Scan(&name, &connected)
	if db.NotFound(err) {
		return Subproject{}, errors.New("no such workspace")
	}
	if err != nil {
		return Subproject{}, err
	}
	if connected {
		return Subproject{}, errors.New("this workspace already has a SignalWire project; use adopt to give it a new token")
	}
	if p.Keys == nil {
		return Subproject{}, errors.New("set VISICOMMS_MASTER_KEY before provisioning a workspace's SignalWire project")
	}
	sub, err := parent.CreateSubproject(ctx, "visicomms-"+strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-"))
	if err != nil {
		return sub, err
	}
	// From here the subproject exists. Its signing key cannot be read again,
	// but adopt --rotate issues a new one, so a failure is recoverable.
	if err := p.connectSubproject(ctx, parent, workspaceID, sub.ID, sub.SigningKey); err != nil {
		return sub, fmt.Errorf("subproject %s was created but not connected (%w); finish with: visicomms signalwire adopt %s %s --rotate", sub.ID, err, workspaceID, sub.ID)
	}
	return sub, nil
}

// Adopt connects an existing subproject of the parent to a workspace with a
// newly minted token. SignalWire only reveals a signing key when it makes
// one, so either signingKey is the current key, copied from the dashboard, or
// it is empty and the key is rotated: right only once nothing else still
// verifies this subproject's callbacks with the old key.
func (p *SignalWireProjects) Adopt(ctx context.Context, parent *SignalWireParent, workspaceID, subproject, signingKey string) error {
	if p.Keys == nil {
		return errors.New("set VISICOMMS_MASTER_KEY before connecting a workspace's own SignalWire project")
	}
	if !signalWireProject.MatchString(subproject) {
		return errors.New("invalid subproject ID")
	}
	// Refuse before calling SignalWire: a rotation cannot be undone, and a
	// minted token would be left live.
	if err := p.connectable(ctx, workspaceID, subproject); err != nil {
		return err
	}
	if signingKey == "" {
		sub, err := parent.RotateSigningKey(ctx, subproject)
		if err != nil {
			return err
		}
		signingKey = sub.SigningKey
	}
	return p.connectSubproject(ctx, parent, workspaceID, subproject, signingKey)
}

func (p *SignalWireProjects) connectSubproject(ctx context.Context, parent *SignalWireParent, workspaceID, subproject, signingKey string) error {
	token, err := parent.CreateToken(ctx, subproject, "visicomms")
	if err != nil {
		return err
	}
	sw, err := NewSignalWire(parent.Space, subproject, token, signingKey, p.PublicURL)
	if err != nil {
		return err
	}
	sw.HTTP = p.HTTP
	return p.ConnectProject(ctx, workspaceID, sw)
}

// connectable is ConnectProject's refusals, checked without connecting.
func (p *SignalWireProjects) connectable(ctx context.Context, workspaceID, project string) error {
	if p.Installation != nil && project == p.Installation.Project {
		return errors.New("this is the installation's SignalWire project; workspaces without their own project already use it")
	}
	var exists, elsewhere, numbers bool
	err := p.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1),
			EXISTS (SELECT 1 FROM connectors WHERE kind = 'signalwire_project' AND settings->>'project_id' = $2 AND workspace_id <> $1),
			EXISTS (SELECT 1 FROM phone_numbers n JOIN connectors c ON c.workspace_id = n.workspace_id AND c.kind = 'signalwire_project'
				WHERE n.workspace_id = $1 AND n.provider_project_id = c.settings->>'project_id' AND c.settings->>'project_id' <> $2)`,
		workspaceID, project).Scan(&exists, &elsewhere, &numbers)
	switch {
	case err != nil:
		return err
	case !exists:
		return errors.New("no such workspace")
	case elsewhere:
		return errors.New("this SignalWire project is already connected to another workspace")
	case numbers:
		return errors.New("this workspace still has numbers connected through another SignalWire project")
	}
	return nil
}
