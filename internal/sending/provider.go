// Package sending executes approved email actions and manages sending domains.
// Provider adapters implement transport and domain setup independently.
package sending

import "context"

// Identity is a domain as the provider sees it. SUCCESS means it can send.
type Identity struct {
	// Status is SES's DKIM status: PENDING, SUCCESS, FAILED, TEMPORARY_FAILURE, NOT_STARTED.
	Status     string
	DKIMTokens []string
	Records    []Record
}

// Sender is the outbound transport. Inbound email is configured separately:
// an installation can receive through Cloudflare and send through SES.
type Sender interface {
	// Send returns a provider message ID. Rejected means definitely not sent;
	// any other error means unknown and must not be retried blindly.
	Send(ctx context.Context, raw []byte, tags map[string]string) (string, error)
}

// DomainProvider manages sending identities and the DNS records needed by
// each adapter. SES DKIM tokens remain supported for existing identities.
type DomainProvider interface {
	RegisterDomain(ctx context.Context, domain string) (Identity, error)
	DomainStatus(ctx context.Context, domain string) (Identity, error)
}

// Provider combines the capabilities used by the existing setup API.
type Provider interface {
	Sender
	DomainProvider
}

// Rejected is a definite refusal: the provider did not accept the message.
type Rejected struct{ Reason string }

func (r *Rejected) Error() string { return r.Reason }

// ProviderKind identifies a configured adapter. Legacy test adapters are SES.
func ProviderKind(p any) string {
	if named, ok := p.(interface{ Kind() string }); ok {
		return named.Kind()
	}
	return "ses"
}
