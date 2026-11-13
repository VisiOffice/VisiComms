package sending

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Routing is where SignalWire sends a number's calls and texts.
type Routing struct {
	Number         string `json:"phone_number"`
	SID            string `json:"sid"`
	VoiceURL       string `json:"voice_url"`
	VoiceMethod    string `json:"voice_method"`
	StatusCallback string `json:"status_callback"`
	SMSURL         string `json:"sms_url"`
	SMSMethod      string `json:"sms_method"`
	// VoiceReceiveMode is "fax" when the number answers calls as faxes.
	VoiceReceiveMode string `json:"voice_receive_mode,omitempty"`
}

// TakesFaxes reports whether the number answers calls as faxes, so its call
// webhook is the fax handler.
func (r Routing) TakesFaxes() bool { return r.VoiceReceiveMode == "fax" }

var ErrNotInProject = errors.New("the number is not in this SignalWire project")

// Routing reads a number's webhooks.
func (s *SignalWire) Routing(ctx context.Context, number string) (Routing, error) {
	var page struct {
		Numbers []struct {
			Routing
			Account string `json:"account_sid"`
		} `json:"incoming_phone_numbers"`
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.request(ctx, http.MethodGet, "/IncomingPhoneNumbers.json?"+url.Values{"PhoneNumber": {number}}.Encode(), nil, &page); err != nil {
		return Routing{}, err
	}
	for _, n := range page.Numbers {
		if n.Number == number && n.Account == s.Project {
			return n.Routing, nil
		}
	}
	return Routing{}, ErrNotInProject
}

// SetRouting points a number's calls and texts at r's URLs. An empty URL is
// left as it is.
func (s *SignalWire) SetRouting(ctx context.Context, r Routing) error {
	v := url.Values{}
	set := func(key, value, methodKey, method string) {
		if value != "" {
			v.Set(key, value)
			if methodKey != "" {
				v.Set(methodKey, method)
			}
		}
	}
	set("VoiceUrl", r.VoiceURL, "VoiceMethod", cmp.Or(r.VoiceMethod, "POST"))
	set("StatusCallback", r.StatusCallback, "StatusCallbackMethod", "POST")
	set("SmsUrl", r.SMSURL, "SmsMethod", cmp.Or(r.SMSMethod, "POST"))
	if len(v) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return s.request(ctx, http.MethodPost, "/IncomingPhoneNumbers/"+url.PathEscape(r.SID)+".json", v, nil)
}
