package sending

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // SignalWire Compatibility callbacks use HMAC-SHA1.
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/phone"
)

// SMSMessage is the approved payload; the originating number is hash-bound.
type SMSMessage struct {
	From string `json:"from"`
	Body string `json:"body"`
}

type SMSSender interface {
	SendSMS(context.Context, SMSMessage, string, string) (string, error)
	ProjectID() string
}

// SignalWire uses the Compatibility API (LaML).
// PublicURL is the canonical public origin; webhook signatures never trust Host
// or forwarded headers. HTTP may be replaced by tests.
type SignalWire struct {
	Space, Project, Token, SigningKey, PublicURL string
	HTTP                                         *http.Client
}

var signalWireSpace = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.signalwire\.com$`)
var signalWireProject = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

func NewSignalWire(space, project, token, signingKey, publicURL string) (*SignalWire, error) {
	space = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(space)), "https://"), "/")
	if !signalWireSpace.MatchString(space) || !signalWireProject.MatchString(project) || token == "" || signingKey == "" {
		return nil, errors.New("SignalWire requires a space hostname, project ID, API token and signing key")
	}
	publicURL, err := CheckPublicURL(publicURL)
	if err != nil {
		return nil, err
	}
	return &SignalWire{Space: space, Project: project, Token: token, SigningKey: signingKey, PublicURL: publicURL}, nil
}

// CheckPublicURL validates SIGNALWIRE_PUBLIC_URL and trims its trailing slash.
func CheckPublicURL(publicURL string) (string, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("SIGNALWIRE_PUBLIC_URL must be the public HTTPS origin, without a path or query")
	}
	return strings.TrimRight(publicURL, "/"), nil
}
func (*SignalWire) Kind() string         { return "signalwire" }
func (s *SignalWire) ProjectID() string  { return s.Project }
func (s *SignalWire) InboundURL() string { return s.PublicURL + "/webhooks/signalwire/sms" }
func (s *SignalWire) StatusURL() string  { return s.PublicURL + "/webhooks/signalwire/sms/status" }

func (s *SignalWire) request(ctx context.Context, method, path string, values url.Values, output any) error {
	return signalWireDo(ctx, s.HTTP, method, "https://"+s.Space+"/api/laml/2010-04-01/Accounts/"+s.Project+path, s.Project, s.Token, values, output)
}

// signalWireDo makes one authenticated call. Redirects are refused so
// credentials never reach another host, and a 4xx is a Rejected.
func signalWireDo(ctx context.Context, hc *http.Client, method, endpoint, user, password string, values url.Values, output any) error {
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	return signalWireSend(ctx, hc, method, endpoint, user, password, "application/x-www-form-urlencoded", body, output)
}

// signalWireJSON is signalWireDo for the JSON project APIs.
func signalWireJSON(ctx context.Context, hc *http.Client, method, endpoint, user, password string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return signalWireSend(ctx, hc, method, endpoint, user, password, "application/json", bytes.NewReader(data), output)
}

func signalWireSend(ctx context.Context, hc *http.Client, method, endpoint, user, password, contentType string, body io.Reader, output any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	if hc != nil {
		*client = *hc
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("SignalWire response is too large")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		reason := fmt.Sprintf("SignalWire returned HTTP %d", res.StatusCode)
		// Compatibility errors say what was wrong; keep it for whoever reads
		// the log.
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Message != "" {
			reason += ": " + detail.Message
		}
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 {
			return &Rejected{Reason: reason}
		}
		return errors.New(reason)
	}
	if output != nil {
		return json.Unmarshal(data, output)
	}
	return nil
}
func (s *SignalWire) SendSMS(ctx context.Context, msg SMSMessage, to, actionID string) (string, error) {
	if _, err := phone.Normalize(msg.From); err != nil {
		return "", &Rejected{Reason: err.Error()}
	}
	if _, err := phone.Normalize(to); err != nil {
		return "", &Rejected{Reason: err.Error()}
	}
	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	// No transport retries: a lost response may follow a successful send.
	err := s.request(ctx, http.MethodPost, "/Messages.json", url.Values{"From": {msg.From}, "To": {to}, "Body": {msg.Body}, "StatusCallback": {s.StatusURL()}}, &result)
	if err != nil {
		return "", err
	}
	if result.SID == "" {
		return "", errors.New("SignalWire accepted the request without a message SID")
	}
	if result.Status == "failed" || result.Status == "undelivered" {
		return result.SID, &Rejected{Reason: "SignalWire reported an immediate delivery failure"}
	}
	return result.SID, nil
}

type SignalWireNumber struct {
	Number       string          `json:"phone_number"`
	SID          string          `json:"sid"`
	Account      string          `json:"account_sid"`
	Capabilities map[string]bool `json:"capabilities"`
}

var ErrFaxNumber = errors.New("this number is not fax-capable in the workspace's SignalWire project")

var ErrSMSNumber = errors.New("this number is not SMS-capable in the workspace's SignalWire project")

func (s *SignalWire) CheckNumber(ctx context.Context, number string, capabilities ...string) (SignalWireNumber, error) {
	capability := "sms"
	if len(capabilities) > 0 {
		capability = capabilities[0]
	}
	var page struct {
		Numbers []SignalWireNumber `json:"incoming_phone_numbers"`
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	err := s.request(ctx, http.MethodGet, "/IncomingPhoneNumbers.json?"+url.Values{"PhoneNumber": {number}, "PageSize": {"1000"}}.Encode(), nil, &page)
	if err != nil {
		return SignalWireNumber{}, err
	}
	for _, n := range page.Numbers {
		if n.Number == number && n.Account == s.Project && n.Capabilities[capability] {
			return n, nil
		}
	}
	if capability == "fax" {
		return SignalWireNumber{}, ErrFaxNumber
	}
	return SignalWireNumber{}, ErrSMSNumber
}

// Verify checks Compatibility form callbacks, using the project's signing key.
// SWML JSON callbacks use another protocol and are deliberately not accepted.
func (s *SignalWire) Verify(path, signature string, values url.Values) bool {
	if s.SigningKey == "" || signature == "" {
		return false
	}
	var payload strings.Builder
	payload.WriteString(s.PublicURL + path)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string{}, values[k]...)
		sort.Strings(vals)
		previous := ""
		for i, v := range vals {
			if i > 0 && v == previous {
				continue
			}
			payload.WriteString(k)
			payload.WriteString(v)
			previous = v
		}
	}
	mac := hmac.New(sha1.New, []byte(s.SigningKey))
	mac.Write([]byte(payload.String()))
	got, err := base64.StdEncoding.DecodeString(signature)
	return err == nil && hmac.Equal(mac.Sum(nil), got)
}

// RedirectCall moves a live call to new markup at path under the public URL.
func (s *SignalWire) RedirectCall(ctx context.Context, callSID, path string) error {
	return s.request(ctx, http.MethodPost, "/Calls/"+url.PathEscape(callSID)+".json",
		url.Values{"Url": {s.PublicURL + path}, "Method": {"POST"}}, nil)
}

// BrowserToken lets a browser phone register as resource and take calls
// dialled to it. It lasts an hour; the console asks for a new one before
// then.
func (s *SignalWire) BrowserToken(ctx context.Context, resource string) (string, error) {
	var out struct {
		JWT string `json:"jwt_token"`
	}
	err := signalWireJSON(ctx, s.HTTP, http.MethodPost, "https://"+s.Space+"/api/relay/rest/jwt", s.Project, s.Token,
		map[string]any{"resource": resource, "expires_in": 60}, &out)
	if err == nil && out.JWT == "" {
		err = errors.New("SignalWire returned no browser token")
	}
	return out.JWT, err
}
