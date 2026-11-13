// Package postal holds what letters need on both sides of an approval:
// mailing addresses, the options a letter is sent with, and its estimated
// cost. Sending through Lob is in internal/sending.
package postal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Address is a US mailing address.
type Address struct {
	Name    string `json:"name"`
	Company string `json:"company,omitempty"`
	Line1   string `json:"line1"`
	Line2   string `json:"line2,omitempty"`
	City    string `json:"city"`
	State   string `json:"state"`
	Zip     string `json:"zip"`
}

var (
	ErrAddress = errors.New("an address needs a name (40 characters at most), street, city, two-letter state and ZIP code")
	// ErrAddressTaken means another contact already has this address.
	ErrAddressTaken  = errors.New("another contact already has this mailing address")
	ErrUndeliverable = errors.New("USPS cannot deliver to this address; check it and save it again")
)

var (
	stateCode = regexp.MustCompile(`^[A-Z]{2}$`)
	zipCode   = regexp.MustCompile(`^\d{5}(-\d{4})?$`)
)

// Clean trims an address and checks it has what a letter needs.
func (a Address) Clean() (Address, error) {
	for _, f := range []*string{&a.Name, &a.Company, &a.Line1, &a.Line2, &a.City, &a.State, &a.Zip} {
		*f = strings.Join(strings.Fields(*f), " ")
	}
	a.State = strings.ToUpper(a.State)
	switch {
	case a.Name == "" || utf8.RuneCountInString(a.Name) > 40 || utf8.RuneCountInString(a.Company) > 40,
		a.Line1 == "" || utf8.RuneCountInString(a.Line1) > 64 || utf8.RuneCountInString(a.Line2) > 64,
		a.City == "", !stateCode.MatchString(a.State), !zipCode.MatchString(a.Zip):
		return a, ErrAddress
	}
	return a, nil
}

// OneLine is the address without the addressee, upper-cased the way USPS
// writes it, e.g. "123 MAIN ST APT 4, LOS ANGELES CA 90066-1234". It is the
// postal identity's address.
func (a Address) OneLine() string {
	street := a.Line1
	if a.Line2 != "" {
		street += " " + a.Line2
	}
	return strings.ToUpper(fmt.Sprintf("%s, %s %s %s", street, a.City, a.State, a.Zip))
}

// Saved is a contact's mailing address as stored.
type Saved struct {
	IdentityID string `json:"id"`
	Address
	Deliverability string `json:"deliverability"`
	Returned       bool   `json:"returned"`
	// Verified is a person's confirmation that the address is the
	// contact's; Lob's check alone never sets it.
	Verified bool `json:"verified"`
}

// Deliverable is whether letters may go to the address.
func (s Saved) Deliverable() bool {
	return !s.Returned && s.Deliverability != "undeliverable"
}

// Save adds an address to a contact, or updates it. a has been checked
// (and standardised by Lob when the key is live); deliverability is Lob's
// verdict, or "unchecked" with a test key. A person saving it confirms it.
func Save(ctx context.Context, tx pgx.Tx, workspaceID, contactID string, a Address, deliverability string) (Saved, error) {
	a, err := a.Clean()
	if err != nil {
		return Saved{}, err
	}
	if deliverability == "undeliverable" {
		return Saved{}, ErrUndeliverable
	}
	line := a.OneLine()
	var id, owner string
	err = tx.QueryRow(ctx, `INSERT INTO identities (workspace_id, contact_id, channel, address, verified)
		VALUES ($1, $2, 'postal', $3, true)
		ON CONFLICT (workspace_id, channel, address) DO UPDATE SET verified = identities.verified OR identities.contact_id = EXCLUDED.contact_id
		RETURNING id, contact_id`, workspaceID, contactID, line).Scan(&id, &owner)
	if err != nil {
		return Saved{}, err
	}
	if owner != contactID {
		return Saved{}, ErrAddressTaken
	}
	_, err = tx.Exec(ctx, `INSERT INTO postal_addresses (identity_id, workspace_id, name, company, line1, line2, city, state, zip, deliverability)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), $7, $8, $9, $10)
		ON CONFLICT (identity_id) DO UPDATE SET name = EXCLUDED.name, company = EXCLUDED.company, line1 = EXCLUDED.line1, line2 = EXCLUDED.line2,
			city = EXCLUDED.city, state = EXCLUDED.state, zip = EXCLUDED.zip, deliverability = EXCLUDED.deliverability,
			checked_at = now(), returned_at = NULL`,
		id, workspaceID, a.Name, a.Company, a.Line1, a.Line2, a.City, a.State, a.Zip, deliverability)
	if err != nil {
		return Saved{}, err
	}
	return Saved{IdentityID: id, Address: a, Deliverability: deliverability, Verified: true}, nil
}

const savedColumns = `i.id::text, p.name, coalesce(p.company, ''), p.line1, coalesce(p.line2, ''), p.city, p.state, p.zip,
	p.deliverability, p.returned_at IS NOT NULL, i.verified`

func scanSaved(row pgx.Row) (Saved, error) {
	var s Saved
	err := row.Scan(&s.IdentityID, &s.Name, &s.Company, &s.Line1, &s.Line2, &s.City, &s.State, &s.Zip, &s.Deliverability, &s.Returned, &s.Verified)
	return s, err
}

// Addresses lists a contact's mailing addresses.
func Addresses(ctx context.Context, q db.Querier, workspaceID, contactID string) ([]Saved, error) {
	rows, err := q.Query(ctx, `SELECT `+savedColumns+` FROM identities i JOIN postal_addresses p ON p.identity_id = i.id
		WHERE i.workspace_id = $1 AND i.contact_id::text = $2 AND i.channel = 'postal' ORDER BY p.created_at`, workspaceID, contactID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Saved, error) { return scanSaved(r) })
}

// Get loads one of a contact's mailing addresses.
func Get(ctx context.Context, q db.Querier, workspaceID, contactID, identityID string) (Saved, error) {
	return scanSaved(q.QueryRow(ctx, `SELECT `+savedColumns+` FROM identities i JOIN postal_addresses p ON p.identity_id = i.id
		WHERE i.workspace_id = $1 AND i.contact_id::text = $2 AND i.id::text = $3 AND i.channel = 'postal'`, workspaceID, contactID, identityID))
}

// Options is how a letter is printed and mailed.
type Options struct {
	// ExtraService is "", "certified" or "certified_return_receipt".
	ExtraService string `json:"extra_service"`
	Color        bool   `json:"color"`
	DoubleSided  bool   `json:"double_sided"`
}

var ErrOptions = errors.New("extra service must be none, certified or certified_return_receipt")

func (o Options) Check() error {
	switch o.ExtraService {
	case "", "certified", "certified_return_receipt":
		return nil
	}
	return ErrOptions
}

// MaxPages is Lob's limit for a domestic letter's PDF.
func MaxPages(doubleSided bool) int {
	if doubleSided {
		return 120
	}
	return 60
}

// Estimated prices, in cents, from Lob's published per-piece rates. Lob's
// API does not return a price; its bill is final. Update these from the
// Lob invoice.
const (
	priceLetter        = 85  // first class, black and white, first sheet
	priceLetterColor   = 105 // first class, colour, first sheet
	priceExtraSheet    = 12
	priceExtraSheetCol = 25
	priceCertified     = 525
	priceReturnReceipt = 300 // on top of certified
)

// Estimate is what a letter will cost, before it is sent.
type Estimate struct {
	Pages int `json:"pages"`
	// Sheets includes the blank address page Lob adds in front.
	Sheets    int `json:"sheets"`
	CostCents int `json:"cost_cents"`
}

// Estimate prices a letter of pages pages.
func (o Options) Estimate(pages int) Estimate {
	if pages < 1 {
		pages = 1
	}
	sheets := pages
	if o.DoubleSided {
		sheets = (pages + 1) / 2
	}
	sheets++ // the address page
	first, extra := priceLetter, priceExtraSheet
	if o.Color {
		first, extra = priceLetterColor, priceExtraSheetCol
	}
	cost := first + (sheets-1)*extra
	switch o.ExtraService {
	case "certified":
		cost += priceCertified
	case "certified_return_receipt":
		cost += priceCertified + priceReturnReceipt
	}
	return Estimate{Pages: pages, Sheets: sheets, CostCents: cost}
}

var pageObject = regexp.MustCompile(`/Type\s*/Page[^s]`)

// Pages counts a PDF's pages from its page objects. A PDF that keeps them in
// compressed object streams counts as one page, so the estimate is a floor
// there; Lob counts the real pages.
func Pages(pdf []byte) int {
	n := len(pageObject.FindAllIndex(pdf, -1))
	if n == 0 && bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return 1
	}
	return n
}

// MonthSpend is the estimated cost of this month's letters: every letter
// not cancelled, and failed ones only if Lob took them (a returned letter
// is still charged). except leaves out one action, the one being checked.
func MonthSpend(ctx context.Context, q db.Querier, workspaceID, except string) (int, error) {
	var cents int
	err := q.QueryRow(ctx, `SELECT coalesce(sum((a.payload->>'cost_cents')::int), 0) FROM actions a
		WHERE a.workspace_id = $1 AND a.action = 'postal.send' AND a.id::text <> $2 AND a.created_at >= date_trunc('month', now())
		AND a.state <> 'cancelled'
		AND NOT (a.state = 'failed' AND NOT EXISTS (SELECT 1 FROM delivery_attempts d WHERE d.action_id = a.id AND d.outcome = 'accepted'))`,
		workspaceID, except).Scan(&cents)
	return cents, err
}

// LetterPayload is a letter action's approved content. The return address
// (From) and the estimated cost are part of what was approved.
type LetterPayload struct {
	DocumentID string `json:"document_id"`
	SHA256     string `json:"sha256"`
	From       string `json:"from"`
	Options
	Pages     int `json:"pages"`
	CostCents int `json:"cost_cents"`
}

// LetterRecipient is a letter action's one recipient.
type LetterRecipient struct {
	IdentityID string `json:"postal_identity_id"`
	Name       string `json:"name"`
	Address    string `json:"address"`
}
