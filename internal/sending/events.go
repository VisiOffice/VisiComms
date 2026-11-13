package sending

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SNS SignatureVersion 1 is SHA1withRSA; version 2 is used when the topic asks for it.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

var (
	// ErrBadSignature means the message did not come from Amazon SNS.
	ErrBadSignature = errors.New("SNS signature does not verify")
	// ErrWrongTopic means a genuine SNS message for a topic we don't listen to.
	ErrWrongTopic = errors.New("not the configured SES events topic")
)

// snsHost is the only place SNS certificates and confirmation links may point.
var snsHost = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com$`)

// SNSMessage is an Amazon SNS HTTP(S) delivery.
type SNSMessage struct {
	Type             string
	MessageID        string `json:"MessageId"`
	TopicArn         string
	Subject          string
	Message          string
	Timestamp        string
	SignatureVersion string
	Signature        string
	SigningCertURL   string
	SubscribeURL     string
	Token            string
}

// SNSVerifier checks SNS signatures, fetching each signing certificate once.
type SNSVerifier struct {
	// HTTP fetches certificates and confirmation links; nil means a client
	// with a 10 s timeout.
	HTTP  *http.Client
	mu    sync.Mutex
	certs map[string]*x509.Certificate
}

func (v *SNSVerifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func amazonURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !snsHost.MatchString(u.Hostname()) {
		return nil, fmt.Errorf("%w: untrusted URL %q", ErrBadSignature, raw)
	}
	return u, nil
}

func (v *SNSVerifier) cert(ctx context.Context, raw string) (*x509.Certificate, error) {
	u, err := amazonURL(raw)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, ".pem") {
		return nil, fmt.Errorf("%w: certificate URL is not a .pem", ErrBadSignature)
	}
	v.mu.Lock()
	if c, ok := v.certs[raw]; ok {
		v.mu.Unlock()
		return c, nil
	}
	v.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	res, err := v.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: no certificate", ErrBadSignature)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	if v.certs == nil {
		v.certs = map[string]*x509.Certificate{}
	}
	v.certs[raw] = c
	v.mu.Unlock()
	return c, nil
}

// signedString is the text SNS signs, per message type.
func (m SNSMessage) signedString() string {
	var b strings.Builder
	add := func(k, val string) { b.WriteString(k + "\n" + val + "\n") }
	add("Message", m.Message)
	add("MessageId", m.MessageID)
	if m.Type == "Notification" {
		if m.Subject != "" {
			add("Subject", m.Subject)
		}
	} else {
		add("SubscribeURL", m.SubscribeURL)
	}
	add("Timestamp", m.Timestamp)
	if m.Type != "Notification" {
		add("Token", m.Token)
	}
	add("TopicArn", m.TopicArn)
	add("Type", m.Type)
	return b.String()
}

// Verify checks that m was signed by Amazon SNS.
func (v *SNSVerifier) Verify(ctx context.Context, m SNSMessage) error {
	c, err := v.cert(ctx, m.SigningCertURL)
	if err != nil {
		return err
	}
	pub, ok := c.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: not an RSA key", ErrBadSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	var hash crypto.Hash
	var digest []byte
	switch m.SignatureVersion {
	case "1":
		sum := sha1.Sum([]byte(m.signedString())) //nolint:gosec
		hash, digest = crypto.SHA1, sum[:]
	case "2":
		sum := sha256.Sum256([]byte(m.signedString()))
		hash, digest = crypto.SHA256, sum[:]
	default:
		return fmt.Errorf("%w: unknown signature version %q", ErrBadSignature, m.SignatureVersion)
	}
	if err := rsa.VerifyPKCS1v15(pub, hash, digest, sig); err != nil {
		return ErrBadSignature
	}
	return nil
}

// Confirm visits a subscription's confirmation link, which must be SNS's own.
func (v *SNSVerifier) Confirm(ctx context.Context, m SNSMessage) error {
	if _, err := amazonURL(m.SubscribeURL); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.SubscribeURL, nil)
	res, err := v.client().Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("confirming subscription: status %d", res.StatusCode)
	}
	return nil
}

// sesEvent is the part of an SES event-publishing record we act on.
type sesEvent struct {
	EventType string `json:"eventType"`
	Mail      struct {
		MessageID string `json:"messageId"`
	} `json:"mail"`
	Bounce *struct {
		BounceType        string `json:"bounceType"`
		BouncedRecipients []struct {
			EmailAddress   string `json:"emailAddress"`
			DiagnosticCode string `json:"diagnosticCode"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint *struct {
		FeedbackType string `json:"complaintFeedbackType"`
	} `json:"complaint"`
	DeliveryDelay *struct {
		DelayType string `json:"delayType"`
	} `json:"deliveryDelay"`
	Reject *struct {
		Reason string `json:"reason"`
	} `json:"reject"`
}

// ApplyEvent durably receives a verified SES notification before applying it.
// Unmatched events remain pending: they may precede the send's database commit.
func ApplyEvent(ctx context.Context, pool *pgxpool.Pool, snsMessageID, body string) error {
	var e sesEvent
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return fmt.Errorf("parsing SES event: %w", err)
	}
	return receiveDeliveryEvent(ctx, pool, "ses", snsMessageID, json.RawMessage(body))
}

func receiveDeliveryEvent(ctx context.Context, pool *pgxpool.Pool, provider, eventID string, body json.RawMessage) error {
	e, err := decodeDeliveryEvent(provider, body)
	if err != nil {
		return err
	}
	if e.Type == "" {
		return nil
	}
	if eventID == "" || e.MessageID == "" {
		return errors.New("delivery event requires notification and message IDs")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_events (provider, event_id, message_id, payload)
  VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, provider, eventID, e.MessageID, body); err != nil {
		return err
	}
	return applyStoredEvent(ctx, pool, provider, eventID)
}

// ReconcileEvents applies a bounded batch of pending events with matching sends.
// Unrelated mail cannot block the batch. Called after dispatch and by the sweep,
// so a crash after receipt or after recording a send does not lose its outcome.
func ReconcileEvents(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `SELECT p.provider, p.event_id FROM provider_events p
		WHERE p.provider IN ('ses', 'resend', 'signalwire', 'signalwire_fax', 'lob') AND p.processed_at IS NULL AND EXISTS (
			SELECT 1 FROM delivery_attempts d JOIN connectors c ON c.id = d.connector_id JOIN actions a ON a.id=d.action_id
			WHERE d.provider_message_id = p.message_id AND c.kind = CASE WHEN p.provider='signalwire_fax' THEN 'signalwire' ELSE p.provider END
 AND a.action=CASE WHEN p.provider='signalwire_fax' THEN 'fax.send' WHEN p.provider='signalwire' THEN 'sms.send' WHEN p.provider='lob' THEN 'postal.send' ELSE 'email.send' END
 AND (p.provider <> 'lob' OR c.id::text = p.payload->>'connector_id'))
		ORDER BY p.received_at, p.event_id LIMIT 100`)
	if err != nil {
		return err
	}
	type receipt struct{ Provider, ID string }
	ids, err := pgx.CollectRows(rows, pgx.RowToStructByPos[receipt])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := applyStoredEvent(ctx, pool, id.Provider, id.ID); err != nil {
			return err
		}
	}
	return nil
}

func applyStoredEvent(ctx context.Context, pool *pgxpool.Pool, provider, snsMessageID string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var body []byte
		err := tx.QueryRow(ctx, `SELECT payload FROM provider_events
			WHERE provider = $2 AND event_id = $1 AND processed_at IS NULL FOR UPDATE`, snsMessageID, provider).Scan(&body)
		if db.NotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		e, err := decodeDeliveryEvent(provider, body)
		if err != nil {
			return err
		}
		var actionID, workspaceID, threadID, contactID, state string
		err = tx.QueryRow(ctx, `SELECT a.id, a.workspace_id, a.thread_id, t.contact_id, a.state
			FROM delivery_attempts d JOIN actions a ON a.id = d.action_id JOIN threads t ON t.id = a.thread_id
			JOIN connectors c ON c.id = d.connector_id
			WHERE d.provider_message_id = $1 AND c.kind = $2 AND ($3 = '' OR c.settings->>'project_id' = $3) AND a.action=$4||'.send' AND t.channel=$4
				AND ($5 = '' OR c.id::text = $5) ORDER BY d.sent_at DESC LIMIT 1 FOR UPDATE OF a`, e.MessageID, deliveryConnector(provider), e.Project, e.Channel, e.Connector).
			Scan(&actionID, &workspaceID, &threadID, &contactID, &state)
		if db.NotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		evType, newState, optOut := e.Type, e.State, e.OptOut
		if newState == "cancelled" && state == "cancelled" {
			// Cancelled from the console, which recorded it already.
			_, err = tx.Exec(ctx, `UPDATE provider_events SET processed_at = now() WHERE provider = $2 AND event_id = $1`, snsMessageID, provider)
			return err
		}
		payload := map[string]any{"action_id": actionID, "provider_message_id": e.MessageID}
		for k, v := range e.Detail {
			payload[k] = v
		}
		if e.Channel == "fax" {
			payload["pages"] = e.Pages
		}
		if e.Reason != "" {
			payload["reason"] = e.Reason
		}
		if e.RFCMessageID != "" {
			payload["message_id"] = e.RFCMessageID
		}

		tag, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, channel, payload, provider, provider_ref)
			VALUES ($1, $2, $3, $4, $8, $5, $7, $6) ON CONFLICT DO NOTHING`,
			workspaceID, threadID, contactID, evType, payload, snsMessageID, provider, e.Channel)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			_, err = tx.Exec(ctx, `UPDATE provider_events SET processed_at = now() WHERE provider = $2 AND event_id = $1`, snsMessageID, provider)
			return err // already applied
		}
		if newState == "delivered" {
			// Delivery also settles an earlier "unknown": the provider did take it.
			_, err = tx.Exec(ctx, `UPDATE actions SET state = 'delivered', updated_at = now() WHERE id = $1 AND state IN ('accepted', 'unknown', 'dispatching')`, actionID)
		} else if newState == "failed" {
			_, err = tx.Exec(ctx, `UPDATE actions SET state = 'failed', updated_at = now() WHERE id = $1 AND ($2 NOT IN ('sms','fax') OR state <> 'delivered')`, actionID, e.Channel)
			if err == nil && e.Type == "postal.returned" {
				// No more letters to an address the post office sent back.
				_, err = tx.Exec(ctx, `UPDATE postal_addresses SET returned_at = now()
					WHERE identity_id::text = (SELECT recipients->0->>'postal_identity_id' FROM actions WHERE id = $1)`, actionID)
			}
		} else if newState == "cancelled" {
			_, err = tx.Exec(ctx, `UPDATE actions SET state = 'cancelled', updated_at = now() WHERE id = $1 AND state IN ('accepted', 'unknown')`, actionID)
		}
		if err != nil {
			return err
		}
		if optOut != "" {
			// A hard bounce or complaint must stop all further mail to this
			// contact, or the shared SES account's reputation suffers.
			_, err = tx.Exec(ctx, `INSERT INTO opt_outs (contact_id, channel, source) VALUES ($1, 'email', $2) ON CONFLICT DO NOTHING`, contactID, optOut)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE provider_events SET processed_at = now() WHERE provider = $2 AND event_id = $1`, snsMessageID, provider)
		return err
	})
}

func deliveryConnector(provider string) string {
	if provider == "signalwire_fax" {
		return "signalwire"
	}
	return provider
}
