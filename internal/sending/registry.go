package sending

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SignalWireParent is the hosted service's parent project. 10DLC brands and
// campaigns are registered through it, for every subproject, so only the
// parent can read them. It is configured with SIGNALWIRE_PARENT_*; a
// self-hosted installation has none and files 10DLC in SignalWire itself.
type SignalWireParent struct {
	Space, Project, Token string
	HTTP                  *http.Client
}

func NewSignalWireParent(space, project, token string) (*SignalWireParent, error) {
	space = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(space)), "https://"), "/")
	if !signalWireSpace.MatchString(space) || !signalWireProject.MatchString(project) || token == "" {
		return nil, errors.New("SIGNALWIRE_PARENT_* needs the space hostname, project ID and API token")
	}
	return &SignalWireParent{Space: space, Project: project, Token: token}, nil
}

// Brand and Campaign are what SignalWire's registry reports, read live: state
// is never cached, because the carrier registry changes it.
type Brand struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type Campaign struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	SMSUseCase string `json:"sms_use_case"`
}

func (p *SignalWireParent) registry(ctx context.Context, path string, output any) error {
	return signalWireDo(ctx, p.HTTP, http.MethodGet, "https://"+p.Space+"/api/relay/rest/registry/beta/"+path, p.Project, p.Token, nil, output)
}

func (p *SignalWireParent) Brand(ctx context.Context, id string) (Brand, error) {
	var b Brand
	if !signalWireProject.MatchString(id) {
		return b, errors.New("invalid brand ID")
	}
	if err := p.registry(ctx, "brands/"+id, &b); err != nil {
		return b, err
	}
	if b.ID != id {
		return b, errors.New("SignalWire did not return this brand")
	}
	return b, nil
}

// Brands lists every brand registered through the parent.
func (p *SignalWireParent) Brands(ctx context.Context) ([]Brand, error) {
	var page struct {
		Data []Brand `json:"data"`
	}
	err := p.registry(ctx, "brands", &page)
	return page.Data, err
}

// Campaigns lists a brand's campaigns. Brands have a handful, well within the
// first page.
func (p *SignalWireParent) Campaigns(ctx context.Context, brandID string) ([]Campaign, error) {
	if !signalWireProject.MatchString(brandID) {
		return nil, errors.New("invalid brand ID")
	}
	var page struct {
		Data []Campaign `json:"data"`
	}
	err := p.registry(ctx, "brands/"+brandID+"/campaigns", &page)
	return page.Data, err
}

// AttachRegistry records which brand and campaigns a workspace's project
// sends under, after checking them against the parent's registry. The
// campaigns must belong to the brand; with none named, all of them are
// recorded. Only the IDs are stored.
func AttachRegistry(ctx context.Context, pool *pgxpool.Pool, parent *SignalWireParent, workspaceID, brandID string, campaignIDs []string) (Brand, []Campaign, error) {
	brand, err := parent.Brand(ctx, brandID)
	if err != nil {
		return brand, nil, err
	}
	all, err := parent.Campaigns(ctx, brandID)
	if err != nil {
		return brand, nil, err
	}
	campaigns := all
	if len(campaignIDs) > 0 {
		campaigns = nil
		for _, id := range campaignIDs {
			found := false
			for _, c := range all {
				if c.ID == id {
					campaigns, found = append(campaigns, c), true
				}
			}
			if !found {
				return brand, nil, errors.New("campaign " + id + " is not one of brand " + brand.Name + "'s campaigns")
			}
		}
	}
	ids := []string{}
	for _, c := range campaigns {
		ids = append(ids, c.ID)
	}
	registry, err := json.Marshal(map[string]any{"brand_id": brand.ID, "campaign_ids": ids})
	if err != nil {
		return brand, nil, err
	}
	tag, err := pool.Exec(ctx, `UPDATE connectors SET settings = settings || $2::jsonb, updated_at = now()
		WHERE workspace_id = $1 AND kind = 'signalwire_project'`, workspaceID, registry)
	if err != nil {
		return brand, nil, err
	}
	if tag.RowsAffected() == 0 {
		return brand, nil, errors.New("connect the workspace's own SignalWire project first (visicomms signalwire connect)")
	}
	return brand, campaigns, nil
}
