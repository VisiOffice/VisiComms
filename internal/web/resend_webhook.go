package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/sending"
)

// verifyResend implements the Svix v1 signature scheme, including freshness.
// See https://docs.svix.com/receiving/verifying-payloads/how-manual .
func verifyResend(secret string, headers http.Header, body []byte, now time.Time) error {
	if secret == "" {
		return errors.New("Resend webhook secret is missing")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 {
		return errors.New("invalid Resend webhook secret")
	}
	id, ts := headers.Get("svix-id"), headers.Get("svix-timestamp")
	if id == "" || ts == "" {
		return errors.New("missing signature headers")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("invalid signature timestamp")
	}
	delta := now.Sub(time.Unix(unix, 0))
	if delta > signatureTolerance || delta < -signatureTolerance {
		return errors.New("timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	for _, sig := range strings.Fields(headers.Get("svix-signature")) {
		version, value, ok := strings.Cut(sig, ",")
		if !ok || version != "v1" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err == nil && hmac.Equal(mac.Sum(nil), decoded) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

func (s *Server) resendWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Resend == nil || s.ResendWebhookSecret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Resend webhooks are not configured"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	if err := verifyResend(s.ResendWebhookSecret, r.Header, body, s.now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature does not verify"})
		return
	}
	var event sending.ResendEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if event.Type == "email.received" {
		s.receiveResend(w, r, event)
		return
	}
	if err := sending.ApplyResendEvent(r.Context(), s.DB, r.Header.Get("svix-id"), body); err != nil {
		s.fail(w, "applying Resend delivery event", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) receiveResend(w http.ResponseWriter, r *http.Request, event sending.ResendEvent) {
	// Fetch authenticated content, never trust a body supplied in a webhook.
	received, err := s.Resend.Receive(r.Context(), event.Data.EmailID)
	if err != nil {
		s.fail(w, "retrieving Resend email", err)
		return
	}
	// Forwarded mail may name the original mailbox in received_for while
	// the webhook names our generated forwarding address in to. Consider
	// both, but only addresses explicitly assigned to a workspace can route.
	recipients := append([]string{}, event.Data.To...)
	recipients = append(recipients, received.To...)
	recipients = append(recipients, received.ReceivedFor...)
	seen := map[string]bool{}
	stored := 0
	for _, to := range recipients {
		workspaceID, err := ingest.Workspace(r.Context(), s.DB, to)
		if errors.Is(err, ingest.ErrUnknownRecipient) {
			continue
		}
		if err != nil {
			s.fail(w, "resolving Resend recipient", err)
			return
		}
		if seen[workspaceID] {
			continue
		}
		seen[workspaceID] = true
		if _, err := ingest.Email(r.Context(), s.DB, workspaceID, received.Inbound(to, s.now())); err != nil {
			s.fail(w, "storing Resend email", err)
			return
		}
		stored++
	}
	if stored == 0 {
		s.Logger.Warn("Resend email has no configured recipient", "email_id", event.Data.EmailID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "workspaces": stored})
}
