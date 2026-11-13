package voice

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// OpenAI answers calls in AI mode. SignalWire dials the project's SIP address;
// OpenAI announces the call with a signed webhook, VisiComms accepts it with
// the workspace's instructions, then follows the conversation over a
// websocket to run tools and keep the transcript. The audio never passes
// through VisiComms.
type OpenAI struct {
	APIKey, ProjectID, WebhookSecret string
	// API is which of OpenAI's voice APIs answers: APILive (GPT-Live) or
	// APIRealtime. Empty is APIRealtime.
	API string
	// BaseURL and SocketURL default to OpenAI's https and wss API roots
	// (ending /v1); tests point them at fakes.
	BaseURL, SocketURL string
	HTTP               *http.Client
}

// OpenAI's voice APIs. The same SIP call can be announced by both; only the
// one VisiComms is set to use answers it.
const (
	APILive     = "live"
	APIRealtime = "realtime"
)

func (o *OpenAI) api() string {
	if o.API == APILive {
		return APILive
	}
	return APIRealtime
}

// UsesLive reports whether calls are answered by GPT-Live.
func (o *OpenAI) UsesLive() bool { return o.api() == APILive }

// Model is the realtime model calls are answered with; LiveModel is
// GPT-Live's, and BackendModel is the model GPT-Live hands tasks to.
const (
	Model        = "gpt-realtime-2.1"
	LiveModel    = "gpt-live-1"
	BackendModel = "gpt-6-luna"
)

// Voices OpenAI can speak calls in. The realtime model speaks only
// realtimeVoices; another choice falls back to the default there.
var Voices = []string{"marin", "cedar", "alloy", "ash", "ballad", "coral", "echo", "sage", "shimmer", "verse",
	"gleam", "meridian", "delta", "cinder", "quartz", "ripple", "vesper", "willow", "stone", "beacon"}

var realtimeVoices = []string{"alloy", "ash", "ballad", "coral", "echo", "sage", "shimmer", "verse", "marin", "cedar"}

const defaultVoice = "marin"

// webhookTolerance is how far a webhook's timestamp may be from now.
const webhookTolerance = 5 * time.Minute

// callHeader carries the VisiComms call ID from SignalWire to OpenAI and back
// in the webhook, so the webhook can be matched to the call.
const callHeader = "X-VisiComms-Call"

// SIPAddress is where SignalWire sends a call for the AI to answer.
func (o *OpenAI) SIPAddress(callID string) string {
	return "sip:" + o.ProjectID + "@sip.api.openai.com;transport=tls?" + callHeader + "=" + url.QueryEscape(callID)
}

var ErrWebhookSignature = errors.New("OpenAI webhook signature does not verify")

// Verify checks a webhook signed in the Standard Webhooks format.
func (o *OpenAI) Verify(body []byte, id, timestamp, signature string, now time.Time) error {
	if id == "" || timestamp == "" || signature == "" {
		return ErrWebhookSignature
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrWebhookSignature
	}
	if d := now.Sub(time.Unix(secs, 0)); d > webhookTolerance || d < -webhookTolerance {
		return ErrWebhookSignature
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(o.WebhookSecret, "whsec_"))
	if err != nil {
		key = []byte(o.WebhookSecret)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%s.%s", id, timestamp, body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, entry := range strings.Fields(signature) {
		version, value, _ := strings.Cut(entry, ",")
		if version == "v1" && hmac.Equal([]byte(value), []byte(expected)) {
			return nil
		}
	}
	return ErrWebhookSignature
}

// IncomingAI is the webhook announcing a call.
type IncomingAI struct {
	// API is which voice API announced it, APILive or APIRealtime.
	API string
	// OpenAICallID names the call in that API: a realtime call ID or a Live
	// session ID.
	OpenAICallID string
	// CallID is the VisiComms call it was dialled for, from the SIP header.
	CallID string
}

// ParseWebhook reads a verified webhook. ok is false for any event other
// than an incoming SIP call.
func ParseWebhook(body []byte) (in IncomingAI, ok bool, err error) {
	var ev struct {
		Type string `json:"type"`
		Data struct {
			CallID     string `json:"call_id"`
			SessionID  string `json:"session_id"`
			Transport  string `json:"type"`
			SIPHeaders []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"sip_headers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return in, false, err
	}
	switch {
	case ev.Type == "realtime.call.incoming":
		in.API, in.OpenAICallID = APIRealtime, ev.Data.CallID
	// live.call.incoming is the deprecated name, without a transport type.
	case ev.Type == "live.transport.incoming" && ev.Data.Transport == "sip", ev.Type == "live.call.incoming":
		in.API, in.OpenAICallID = APILive, ev.Data.SessionID
	default:
		return in, false, nil
	}
	for _, h := range ev.Data.SIPHeaders {
		if strings.EqualFold(h.Name, callHeader) {
			in.CallID = h.Value
		}
	}
	return in, true, nil
}

// post sends a call action. A nil body sends none.
func (o *OpenAI) post(ctx context.Context, callID, verb string, body any) error {
	base := o.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	path := "/realtime/calls/"
	if o.UsesLive() {
		path = "/live/sessions/"
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path+url.PathEscape(callID)+"/"+verb, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("OpenAI %s returned %d: %s", verb, resp.StatusCode, detail)
	}
	return nil
}

// Accept answers a call with a session. GPT-Live takes it wrapped in
// {"session": ...}; the realtime API takes it as the body.
func (o *OpenAI) Accept(ctx context.Context, callID string, session map[string]any) error {
	if o.UsesLive() {
		return o.post(ctx, callID, "accept", map[string]any{"session": session})
	}
	return o.post(ctx, callID, "accept", session)
}

// Reject turns away a call VisiComms did not dial.
func (o *OpenAI) Reject(ctx context.Context, callID string) error {
	return o.post(ctx, callID, "reject", map[string]any{"status_code": 603})
}

// Refer transfers the caller to a phone number.
func (o *OpenAI) Refer(ctx context.Context, callID, number string) error {
	return o.post(ctx, callID, "refer", map[string]any{"target_uri": "tel:" + number})
}

// Hangup ends a call.
func (o *OpenAI) Hangup(ctx context.Context, callID string) error {
	if o.UsesLive() {
		return o.post(ctx, callID, "hangup", nil)
	}
	return o.post(ctx, callID, "hangup", map[string]any{})
}

// Connect opens the websocket that follows an accepted call.
func (o *OpenAI) Connect(ctx context.Context, callID string) (*websocket.Conn, error) {
	base := o.SocketURL
	if base == "" {
		base = "wss://api.openai.com/v1"
	}
	address := base + "/realtime?call_id=" + url.QueryEscape(callID)
	if o.UsesLive() {
		address = base + "/live/sessions/" + url.PathEscape(callID) + "/attach"
	}
	conn, resp, err := websocket.Dial(ctx, address, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + o.APIKey}},
	})
	if err != nil {
		// OpenAI says why in the refused handshake's body.
		if resp != nil && resp.Body != nil {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if len(detail) > 0 {
				err = fmt.Errorf("%w: %s", err, detail)
			}
		}
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	return conn, nil
}
