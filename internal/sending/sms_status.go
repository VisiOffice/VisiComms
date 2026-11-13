package sending

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// SMSReadiness is why a workspace's texts do or do not go out: its numbers,
// and its 10DLC brand and campaigns as SignalWire's registry reports them
// now. States are SignalWire's own words; NextStep says what to do.
type SMSReadiness struct {
	Project   string     `json:"project,omitempty"`
	Numbers   []string   `json:"texting_numbers"`
	Brand     *Brand     `json:"brand,omitempty"`
	Campaigns []Campaign `json:"campaigns,omitempty"`
	NextStep  string     `json:"next_step"`
	// Unread is set when the registry could not be read, and why.
	Unread string `json:"registry_unread,omitempty"`
}

// ReadSMSStatus reads a workspace's SMS readiness. parent may be nil (a
// self-hosted installation), in which case the registry is not read.
func ReadSMSStatus(ctx context.Context, pool *pgxpool.Pool, parent *SignalWireParent, workspaceID string) (SMSReadiness, error) {
	var out SMSReadiness
	rows, err := pool.Query(ctx, `SELECT number FROM phone_numbers WHERE workspace_id = $1 AND capabilities->>'sms' = 'true' ORDER BY number`, workspaceID)
	if err != nil {
		return out, err
	}
	if out.Numbers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return out, err
	}
	if out.Numbers == nil {
		out.Numbers = []string{}
	}
	var brandID string
	var campaignIDs []string
	err = pool.QueryRow(ctx, `SELECT coalesce(settings->>'project_id', ''), coalesce(settings->>'brand_id', ''),
			coalesce(ARRAY(SELECT jsonb_array_elements_text(coalesce(settings->'campaign_ids', '[]'))), '{}')
		FROM connectors WHERE workspace_id = $1 AND kind = 'signalwire_project'`, workspaceID).Scan(&out.Project, &brandID, &campaignIDs)
	if err != nil && !db.NotFound(err) {
		return out, err
	}
	switch {
	case out.Project == "":
		out.NextStep = "This workspace has no SignalWire project of its own, so its texting is set up by whoever runs this VisiComms installation. Ask them."
		return out, nil
	case brandID == "":
		out.NextStep = "No 10DLC brand is recorded for this workspace. Register the business as a brand and a campaign, then record them (visicomms signalwire registry). US carriers block texts from unregistered numbers."
		return out, nil
	case parent == nil:
		out.Unread = "this installation has no SignalWire parent project to read the registry with"
		out.NextStep = "Check brand " + brandID + " and its campaigns in SignalWire."
		return out, nil
	}
	brand, err := parent.Brand(ctx, brandID)
	if err != nil {
		out.Unread = "SignalWire's registry could not be read just now: " + err.Error()
		out.NextStep = "Try again later, or check brand " + brandID + " in SignalWire."
		return out, nil
	}
	out.Brand = &brand
	all, err := parent.Campaigns(ctx, brandID)
	if err != nil {
		out.Unread = "SignalWire's registry could not be read just now: " + err.Error()
		out.NextStep = "Try again later."
		return out, nil
	}
	for _, c := range all {
		for _, id := range campaignIDs {
			if c.ID == id {
				out.Campaigns = append(out.Campaigns, c)
			}
		}
	}
	out.NextStep = nextSMSStep(brand, out.Campaigns, out.Numbers)
	return out, nil
}

// approvedLike is a registry state that lets texts through. SignalWire's
// words vary by stage, so this errs towards "not yet".
func approvedLike(state string) bool {
	switch strings.ToLower(state) {
	case "active", "approved", "verified", "completed", "registered":
		return true
	}
	return false
}

func nextSMSStep(brand Brand, campaigns []Campaign, numbers []string) string {
	if !approvedLike(brand.State) {
		return "The brand " + brand.Name + " is " + brand.State + " with the carriers' registry. Campaigns cannot be approved until it is; nothing to do but wait unless SignalWire asked for changes."
	}
	if len(campaigns) == 0 {
		return "The brand is " + brand.State + " but no campaign is recorded for this workspace. Create a campaign under it and record it (visicomms signalwire registry)."
	}
	for _, c := range campaigns {
		if approvedLike(c.State) {
			if len(numbers) == 0 {
				return "Campaign " + c.Name + " is " + c.State + ", but no texting number is connected. Connect one (visicomms sms connect)."
			}
			return "Campaign " + c.Name + " is " + c.State + ". Texts can go out; if one fails, check the number is assigned to this campaign in SignalWire."
		}
	}
	return "Campaign " + campaigns[0].Name + " is " + campaigns[0].State + ": waiting on carrier review. Texts to US numbers will not be delivered until it is approved."
}
