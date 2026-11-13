package sending

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

var (
	ErrBadDomain      = errors.New("enter a domain like example.com")
	ErrDomainNotReady = errors.New("that domain isn't verified for this workspace yet")
	ErrBadAddress     = errors.New("enter an email address on one of your verified domains")
)

// LookupTXT resolves TXT records; tests replace it.
var LookupTXT = func(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// Domain is a sending domain with the DNS records the business adds.
type Domain struct {
	ID               string     `json:"id"`
	Domain           string     `json:"domain"`
	OwnershipChecked bool       `json:"ownership_verified"`
	SESStatus        string     `json:"ses_status,omitempty"`
	Provider         string     `json:"provider"`
	Status           string     `json:"status"`
	Verified         bool       `json:"verified"`
	Records          []Record   `json:"records"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
}

type Record struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Purpose  string `json:"purpose"`
	OK       bool   `json:"ok"`
	Priority *int   `json:"priority,omitempty"`
}

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func normalizeDomain(d string) (string, bool) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	return d, domainPattern.MatchString(d)
}

// AddDomain starts verifying a domain for a workspace: it registers the
// domain with the provider (or finds it already there) and creates the workspace's
// own ownership token.
func AddDomain(ctx context.Context, q db.Querier, p DomainProvider, workspaceID, domain string) (Domain, error) {
	domain, ok := normalizeDomain(domain)
	if !ok {
		return Domain{}, ErrBadDomain
	}
	id, err := p.RegisterDomain(ctx, domain)
	if err != nil {
		return Domain{}, err
	}
	records, err := json.Marshal(identityRecords(id, domain))
	if err != nil {
		return Domain{}, err
	}
	token := randomHex(16)
	var domainID string
	err = q.QueryRow(ctx, `INSERT INTO sending_domains (workspace_id, domain, ownership_token, provider_status, dkim_tokens, checked_at, provider, dns_records)
		VALUES ($1, $2, $3, $4, $5, now(), $6, $7)
		ON CONFLICT (workspace_id, provider, domain) DO UPDATE SET provider_status = EXCLUDED.provider_status, dkim_tokens = EXCLUDED.dkim_tokens, dns_records = EXCLUDED.dns_records, checked_at = now()
		RETURNING id`, workspaceID, domain, token, id.Status, nonNilTokens(id.DKIMTokens), ProviderKind(p), json.RawMessage(records)).Scan(&domainID)
	if err != nil {
		return Domain{}, err
	}
	return getDomain(ctx, q, workspaceID, domainID)
}

// CheckDomain re-reads the provider's status and looks for the ownership record.
func CheckDomain(ctx context.Context, q db.Querier, p DomainProvider, workspaceID, domainID string) (Domain, error) {
	var domain, token string
	err := q.QueryRow(ctx, `SELECT domain, ownership_token FROM sending_domains WHERE id = $1 AND workspace_id = $2 AND provider = $3`,
		domainID, workspaceID, ProviderKind(p)).Scan(&domain, &token)
	if db.NotFound(err) {
		return Domain{}, ErrDomainNotReady
	}
	if err != nil {
		return Domain{}, err
	}
	id, err := p.DomainStatus(ctx, domain)
	if err != nil {
		return Domain{}, err
	}
	records, err := json.Marshal(identityRecords(id, domain))
	if err != nil {
		return Domain{}, err
	}
	owned := hasOwnershipRecord(ctx, domain, token)
	_, err = q.Exec(ctx, `UPDATE sending_domains SET provider_status = $3, dkim_tokens = $4, dns_records = $6, checked_at = now(),
			ownership_verified_at = CASE WHEN $5 THEN coalesce(ownership_verified_at, now()) ELSE NULL END
		WHERE id = $1 AND workspace_id = $2`, domainID, workspaceID, id.Status, nonNilTokens(id.DKIMTokens), owned, json.RawMessage(records))
	if err != nil {
		return Domain{}, err
	}
	return getDomain(ctx, q, workspaceID, domainID)
}

func hasOwnershipRecord(ctx context.Context, domain, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	txts, err := LookupTXT(ctx, "_visicomms."+domain)
	if err != nil {
		return false
	}
	for _, t := range txts {
		if strings.TrimSpace(t) == "visicomms-verify="+token {
			return true
		}
	}
	return false
}

// ListDomains returns the workspace's sending domains.
func ListDomains(ctx context.Context, q db.Querier, workspaceID string, providers ...string) ([]Domain, error) {
	provider := ""
	if len(providers) > 0 {
		provider = providers[0]
	}
	rows, err := q.Query(ctx, `SELECT id FROM sending_domains WHERE workspace_id = $1 AND ($2 = '' OR provider = $2) ORDER BY created_at`, workspaceID, provider)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := []Domain{}
	for _, id := range ids {
		d, err := getDomain(ctx, q, workspaceID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func getDomain(ctx context.Context, q db.Querier, workspaceID, id string) (Domain, error) {
	var d Domain
	var token string
	var ownedAt *time.Time
	var dkim []string
	var records []byte
	err := q.QueryRow(ctx, `SELECT id, domain, ownership_token, ownership_verified_at, provider_status, dkim_tokens, checked_at, provider, dns_records
		FROM sending_domains WHERE id = $1 AND workspace_id = $2`, id, workspaceID).
		Scan(&d.ID, &d.Domain, &token, &ownedAt, &d.Status, &dkim, &d.CheckedAt, &d.Provider, &records)
	if err != nil {
		return d, err
	}
	d.OwnershipChecked = ownedAt != nil
	dkimOK := d.Status == "SUCCESS"
	if d.Provider == "ses" {
		d.SESStatus = d.Status
	}
	d.Verified = d.OwnershipChecked && dkimOK
	d.Records = append(d.Records, Record{Type: "TXT", Name: "_visicomms." + d.Domain, Value: "visicomms-verify=" + token,
		Purpose: "Proves this workspace owns the domain", OK: d.OwnershipChecked})
	var providerRecords []Record
	if err := json.Unmarshal(records, &providerRecords); err != nil {
		return d, err
	}
	if len(providerRecords) == 0 && d.Provider == "ses" {
		providerRecords = identityRecords(Identity{Status: d.Status, DKIMTokens: dkim}, d.Domain)
	}
	d.Records = append(d.Records, providerRecords...)
	return d, nil
}

// From is who mail appears to come from.
type From struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// SetFrom points the workspace's selected provider connector at an address on one of its
// verified domains. The connector is created the first time.
func SetFrom(ctx context.Context, q db.Querier, workspaceID string, f From, providers ...string) error {
	provider := "ses"
	if len(providers) > 0 {
		provider = providers[0]
	}
	if provider != "ses" && provider != "resend" {
		return ErrDomainNotReady
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(f.Address))
	if err != nil {
		return ErrBadAddress
	}
	address := strings.ToLower(addr.Address)
	_, domain, _ := strings.Cut(address, "@")
	var verified bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sending_domains WHERE workspace_id = $1 AND domain = $2
		AND ownership_verified_at IS NOT NULL AND provider_status = 'SUCCESS' AND provider = $3)`, workspaceID, domain, provider).Scan(&verified)
	if err != nil {
		return err
	}
	if !verified {
		return ErrDomainNotReady
	}
	settings := map[string]string{"from_name": strings.TrimSpace(f.Name), "from_address": address}
	tag, err := q.Exec(ctx, `UPDATE connectors SET settings = $2, health = 'ok', last_error = NULL, checked_at = now(), updated_at = now()
		WHERE workspace_id = $1 AND kind = $3`, workspaceID, settings, provider)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = q.Exec(ctx, `INSERT INTO connectors (workspace_id, kind, name, settings, health, checked_at)
			VALUES ($1, $3, $4, $2, 'ok', now())`, workspaceID, settings, provider, map[string]string{"ses": "Amazon SES", "resend": "Resend"}[provider])
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func nonNilTokens(tokens []string) []string {
	if tokens == nil {
		return []string{}
	}
	return tokens
}

func identityRecords(id Identity, domain string) []Record {
	if id.Records != nil {
		return id.Records
	}
	records := []Record{}
	for _, token := range id.DKIMTokens {
		records = append(records, Record{Type: "CNAME", Name: token + "._domainkey." + domain,
			Value: token + ".dkim.amazonses.com", Purpose: "Lets Amazon SES sign mail as this domain (DKIM)", OK: id.Status == "SUCCESS"})
	}
	return records
}
