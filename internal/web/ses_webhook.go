package web

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/sending"
)

// sesWebhook receives SES delivery events through Amazon SNS:
// POST /webhooks/ses. Every message must carry a valid SNS signature and
// come from the configured topic; anything else is refused.
func (s *Server) sesWebhook(w http.ResponseWriter, r *http.Request) {
	if s.SNS == nil || s.SESEventsTopic == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "SES events are not configured"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	var m sending.SNSMessage
	if err := json.Unmarshal(body, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if m.TopicArn != s.SESEventsTopic {
		s.Logger.Warn("SNS message for another topic", "topic", m.TopicArn)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": sending.ErrWrongTopic.Error()})
		return
	}
	if err := s.SNS.Verify(r.Context(), m); err != nil {
		s.Logger.Warn("SNS signature rejected", "err", err)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "signature does not verify"})
		return
	}
	switch m.Type {
	case "SubscriptionConfirmation":
		if err := s.SNS.Confirm(r.Context(), m); err != nil {
			s.fail(w, "confirming SNS subscription", err)
			return
		}
		s.Logger.Info("SES events subscription confirmed", "topic", m.TopicArn)
	case "Notification":
		// A 5xx makes SNS retry, which suits a database hiccup; redeliveries
		// are deduped by the SNS message ID.
		if err := sending.ApplyEvent(r.Context(), s.DB, m.MessageID, m.Message); err != nil {
			s.fail(w, "applying SES event", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
