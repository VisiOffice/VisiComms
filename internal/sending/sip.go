package sending

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SIPEndpoint is a SIP account a device, such as a door intercom, signs in to
// SignalWire with. Calls it places are handed to VisiComms like calls to a
// number.
type SIPEndpoint struct {
	ID       string
	Username string
	// Server is where the device signs in.
	Server string
}

// SIPServer is the space's SIP domain, e.g. example.sip.signalwire.com.
func (s *SignalWire) SIPServer() string {
	name, ok := strings.CutSuffix(s.Space, ".signalwire.com")
	if !ok || name == "" {
		return ""
	}
	return name + ".sip.signalwire.com"
}

// ErrNoSIPServer means the space's SIP domain cannot be worked out.
var ErrNoSIPServer = errors.New("this SignalWire space has no SIP domain")

// CreateSIPEndpoint makes a SIP account whose calls are handed to callURL.
// statusURL hears how they end. The password is the caller's to show once;
// SignalWire keeps it, VisiComms does not.
func (s *SignalWire) CreateSIPEndpoint(ctx context.Context, username, password, callerID, callURL, statusURL string) (SIPEndpoint, error) {
	server := s.SIPServer()
	if server == "" {
		return SIPEndpoint{}, ErrNoSIPServer
	}
	var out struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	err := signalWireJSON(ctx, s.HTTP, http.MethodPost, "https://"+s.Space+"/api/relay/rest/endpoints/sip", s.Project, s.Token, map[string]any{
		"username":  username,
		"password":  password,
		"caller_id": callerID,
		// Intercoms rarely encrypt their audio.
		"encryption":                  "optional",
		"call_handler":                "laml_webhooks",
		"call_request_url":            callURL,
		"call_request_method":         "POST",
		"call_fallback_url":           callURL,
		"call_fallback_method":        "POST",
		"call_status_callback_url":    statusURL,
		"call_status_callback_method": "POST",
	}, &out)
	if err != nil {
		return SIPEndpoint{}, err
	}
	if out.ID == "" {
		return SIPEndpoint{}, errors.New("SignalWire did not return the SIP endpoint's id")
	}
	return SIPEndpoint{ID: out.ID, Username: username, Server: server}, nil
}

// DeleteSIPEndpoint removes a SIP account, so the device can no longer sign
// in. One that is already gone is not an error.
func (s *SignalWire) DeleteSIPEndpoint(ctx context.Context, id string) error {
	err := signalWireSend(ctx, s.HTTP, http.MethodDelete, "https://"+s.Space+"/api/relay/rest/endpoints/sip/"+url.PathEscape(id), s.Project, s.Token, "application/json", nil, nil)
	var rejected *Rejected
	if errors.As(err, &rejected) && strings.Contains(rejected.Reason, "HTTP 404") {
		return nil
	}
	return err
}
