package sending

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ResendEvent is the verified webhook envelope. Content is fetched separately
// for email.received; delivery events use the same durable receipt machinery.
type ResendEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID   string   `json:"email_id"`
		MessageID string   `json:"message_id"`
		To        []string `json:"to"`
		Bounce    struct {
			Message string `json:"message"`
		} `json:"bounce"`
		Failed struct {
			Reason string `json:"reason"`
		} `json:"failed"`
		Suppressed struct {
			Reason string `json:"reason"`
		} `json:"suppressed"`
	} `json:"data"`
}

type deliveryEvent struct {
	Type, MessageID, RFCMessageID, State, Reason, OptOut, Channel, Project string
	Pages                                                                  int
	// Connector is the connector a webhook arrived on, when the provider has
	// one account per workspace (Lob): only its own letters match.
	Connector string
	// Detail is added to the thread event.
	Detail map[string]any
}

func ApplyResendEvent(ctx context.Context, pool *pgxpool.Pool, eventID string, body []byte) error {
	return receiveDeliveryEvent(ctx, pool, "resend", eventID, json.RawMessage(body))
}

func decodeDeliveryEvent(provider string, body []byte) (deliveryEvent, error) {
	out := deliveryEvent{Channel: "email"}
	switch provider {
	case "ses":
		var e sesEvent
		if err := json.Unmarshal(body, &e); err != nil {
			return out, err
		}
		out.MessageID = e.Mail.MessageID
		switch e.EventType {
		case "Delivery":
			out.Type, out.State = "email.delivered", "delivered"
		case "Bounce":
			out.Type = "email.delayed"
			if e.Bounce != nil {
				if len(e.Bounce.BouncedRecipients) > 0 {
					out.Reason = e.Bounce.BouncedRecipients[0].DiagnosticCode
				}
				if e.Bounce.BounceType == "Permanent" {
					out.Type, out.State, out.OptOut = "email.bounced", "failed", "hard bounce"
				}
			}
		case "Complaint":
			out.Type, out.OptOut = "email.complained", "spam complaint"
		case "DeliveryDelay":
			out.Type = "email.delayed"
			if e.DeliveryDelay != nil {
				out.Reason = e.DeliveryDelay.DelayType
			}
		case "Reject":
			out.Type, out.State = "email.failed", "failed"
			if e.Reject != nil {
				out.Reason = "SES rejected the message: " + e.Reject.Reason
			}
		}
	case "resend":
		var e ResendEvent
		if err := json.Unmarshal(body, &e); err != nil {
			return out, err
		}
		out.MessageID, out.RFCMessageID = e.Data.EmailID, e.Data.MessageID
		switch e.Type {
		case "email.sent":
			out.Type = "email.accepted"
		case "email.delivered":
			out.Type, out.State = "email.delivered", "delivered"
		case "email.bounced":
			out.Type, out.State, out.OptOut, out.Reason = "email.bounced", "failed", "hard bounce", e.Data.Bounce.Message
		case "email.complained":
			out.Type, out.OptOut = "email.complained", "spam complaint"
		case "email.delivery_delayed":
			out.Type = "email.delayed"
		case "email.failed":
			out.Type, out.State, out.Reason = "email.failed", "failed", e.Data.Failed.Reason
		case "email.suppressed":
			out.Type, out.State, out.Reason = "email.failed", "failed", e.Data.Suppressed.Reason
		}
	case "signalwire":
		var e SMSStatus
		if err := json.Unmarshal(body, &e); err != nil {
			return out, err
		}
		out.Channel, out.MessageID, out.Project = "sms", e.SID, e.Project
		switch e.Status {
		case "queued", "sending", "sent":
			out.Type = "sms.delivery_update"
		case "delivered":
			out.Type, out.State = "sms.delivered", "delivered"
		case "failed", "undelivered":
			out.Type, out.State, out.Reason = "sms.failed", "failed", "SignalWire error "+e.ErrorCode
		}
	case "signalwire_fax":
		var e FaxStatus
		if err := json.Unmarshal(body, &e); err != nil {
			return out, err
		}
		out.Channel, out.MessageID, out.Project = "fax", e.SID, e.Project
		out.Pages = e.Pages
		switch e.Status {
		case "queued", "processing", "sending":
			out.Type = "fax.delivery_update"
		case "delivered":
			out.Type, out.State = "fax.delivered", "delivered"
		case "failed", "busy", "no-answer", "canceled":
			out.Type, out.State, out.Reason = "fax.failed", "failed", e.Status+" "+e.ErrorCode
		}
	case "lob":
		return decodeLob(body)
	default:
		return out, fmt.Errorf("unknown delivery provider %q", provider)
	}
	return out, nil
}

// SMSStatus is stored only after the form callback has been authenticated.
type SMSStatus struct {
	SID       string `json:"sid"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
	Project   string `json:"project"`
}

func ApplySMSStatus(ctx context.Context, pool *pgxpool.Pool, status SMSStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return receiveDeliveryEvent(ctx, pool, "signalwire", status.SID+":"+status.Status, body)
}

type FaxStatus struct {
	SID       string `json:"sid"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
	Project   string `json:"project"`
	Pages     int    `json:"pages"`
}

func ApplyFaxStatus(ctx context.Context, pool *pgxpool.Pool, status FaxStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return receiveDeliveryEvent(ctx, pool, "signalwire_fax", status.SID+":"+status.Status, body)
}
