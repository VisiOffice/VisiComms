package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// InboundDomain receives forwarded customer email. Mail to any address here
// reaches POST /webhooks/email through the Cloudflare email worker.
const InboundDomain = "in.visicomms.com"

// Setup step states.
const (
	StepTodo    = "todo"
	StepPartial = "partial"
	StepDone    = "done"
)

// SetupStep is one thing a workspace needs before agents can do real work.
// Every state is read from what is actually configured, so the checklist
// cannot claim something is set up when it is not.
type SetupStep struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	// Optional steps don't hold up "set up".
	Optional bool `json:"optional"`
	// Value is something to copy, such as the forwarding address.
	Value string `json:"value,omitempty"`
}

type SetupStatus struct {
	Steps []SetupStep `json:"steps"`
	// Complete when every required step is done.
	Complete bool `json:"complete"`
}

// Setup reads what the workspace has configured.
func Setup(ctx context.Context, q db.Querier, workspaceID string, providers ...string) (SetupStatus, error) {
	provider := ""
	if len(providers) > 0 {
		provider = providers[0]
	}
	var inbox *string
	var realSenders, sandboxSenders, approvedInfo, draftInfo, agents, people int
	err := q.QueryRow(ctx, `SELECT
			(SELECT min(address) FROM inbound_addresses WHERE workspace_id = $1),
			(SELECT count(*) FROM connectors WHERE workspace_id = $1 AND kind IN ('ses', 'resend', 'postmark') AND ($2 = '' OR kind = $2) AND health = 'ok'),
			(SELECT count(*) FROM connectors WHERE workspace_id = $1 AND kind = 'sandbox'),
			(SELECT count(*) FROM approved_info_versions WHERE workspace_id = $1 AND approved_at IS NOT NULL),
			(SELECT count(*) FROM approved_info_versions WHERE workspace_id = $1 AND approved_at IS NULL),
			(SELECT count(*) FROM principals WHERE workspace_id = $1 AND kind = 'agent' AND revoked_at IS NULL),
			(SELECT count(*) FROM principals WHERE workspace_id = $1 AND kind = 'human' AND revoked_at IS NULL)`,
		workspaceID, provider).Scan(&inbox, &realSenders, &sandboxSenders, &approvedInfo, &draftInfo, &agents, &people)
	if err != nil {
		return SetupStatus{}, err
	}

	steps := []SetupStep{
		{Key: "inbox", Title: "Forward your inquiries", State: StepTodo,
			Detail: "Get an address to forward customer email to. Every message becomes a thread here."},
		{Key: "sending", Title: "Send as your domain", State: StepTodo,
			Detail: "Replies go out from your own address, like quotes@yourbusiness.com, after you verify the domain."},
		{Key: "approved_info", Title: "Add approved info", State: StepTodo,
			Detail: "The facts agents may quote: hours, services, service area and policies. A person approves each version."},
		{Key: "agent", Title: "Add an agent", State: StepTodo,
			Detail: "The agent drafts replies from approved info. Nothing it writes is sent until you approve it."},
		{Key: "team", Title: "Invite your team", State: StepTodo, Optional: true,
			Detail: "Anyone who should review held drafts or take over threads."},
	}
	if inbox != nil {
		steps[0].State = StepDone
		steps[0].Detail = "Forward customer email here. Every message becomes a thread."
		steps[0].Value = *inbox
	}
	switch {
	case realSenders > 0:
		steps[1].State = StepDone
		steps[1].Detail = "Replies go out from your own domain."
	case sandboxSenders > 0:
		steps[1].State = StepPartial
		steps[1].Detail = "Using the sandbox: approved replies are recorded but reach nobody. Verify your domain to reach customers."
	}
	switch {
	case approvedInfo > 0:
		steps[2].State = StepDone
		steps[2].Detail = "Agents quote only from the approved version."
	case draftInfo > 0:
		steps[2].State = StepPartial
		steps[2].Detail = "A draft is waiting for approval. Agents can't quote anything until a version is approved."
	}
	if agents > 0 {
		steps[3].State = StepDone
		steps[3].Detail = "Drafts arrive in Needs attention for you to approve."
	}
	if people > 1 {
		steps[4].State = StepDone
		steps[4].Detail = "Your team can review held drafts."
	}

	complete := true
	for _, s := range steps {
		if !s.Optional && s.State != StepDone {
			complete = false
		}
	}
	return SetupStatus{Steps: steps, Complete: complete}, nil
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// NewInboxAddress makes a forwarding address for a workspace: readable, from
// its name, and unguessable, so nobody can post mail into another workspace
// by guessing its address.
func NewInboxAddress(workspaceName string, domains ...string) string {
	slug := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(workspaceName), "-"), "-")
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "-")
	}
	if slug == "" {
		slug = "inbox"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	domain := InboundDomain
	if len(domains) > 0 {
		domain = domains[0]
	}
	return "in+" + slug + "-" + hex.EncodeToString(b) + "@" + domain
}

// CreateInbox gives the workspace a forwarding address, or returns the one it
// already has.
func CreateInbox(ctx context.Context, q db.Querier, workspaceID string, domains ...string) (string, error) {
	domain := InboundDomain
	if len(domains) > 0 {
		domain = domains[0]
	}
	if err := ValidateInboundDomain(domain); err != nil {
		return "", err
	}
	var address string
	err := q.QueryRow(ctx, `SELECT min(address) FROM inbound_addresses WHERE workspace_id = $1 HAVING count(*) > 0`, workspaceID).Scan(&address)
	if err == nil {
		return address, nil
	}
	if !db.NotFound(err) {
		return "", err
	}
	var name string
	if err := q.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&name); err != nil {
		return "", err
	}
	address = NewInboxAddress(name, domain)
	_, err = q.Exec(ctx, `INSERT INTO inbound_addresses (address, workspace_id) VALUES ($1, $2)`, address, workspaceID)
	return address, err
}

// UseSandbox adds the sandbox sender, which records approved replies as sent
// and contacts nobody. It does nothing when the workspace already has one.
func UseSandbox(ctx context.Context, q db.Querier, workspaceID string) error {
	_, err := q.Exec(ctx, `INSERT INTO connectors (workspace_id, kind, name, health, checked_at)
		SELECT $1, 'sandbox', 'Sandbox email (nothing is sent)', 'ok', now()
		WHERE NOT EXISTS (SELECT 1 FROM connectors WHERE workspace_id = $1 AND kind = 'sandbox')`, workspaceID)
	return err
}

var inboundDomainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func ValidateInboundDomain(domain string) error {
	if !inboundDomainPattern.MatchString(domain) {
		return errors.New("INBOUND_EMAIL_DOMAIN must be a valid domain, without a URL or address")
	}
	return nil
}
