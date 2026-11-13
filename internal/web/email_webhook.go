package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/VisiOffice/VisiComms/internal/email"
	"github.com/VisiOffice/VisiComms/internal/ingest"
)

// maxEmailBody bounds a webhook body. Attachments arrive as storage keys,
// not bytes, so real messages are far smaller.
const maxEmailBody = 10 << 20

// signatureTolerance is how far a webhook's timestamp may be from now, so a
// captured request cannot be replayed later.
const signatureTolerance = 5 * time.Minute

// emailWebhook receives inbound email from the Cloudflare email worker
// (inbound/): POST /webhooks/email.
func (s *Server) emailWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEmailBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	if err := s.verifyInboundEmail(r, body); err != nil {
		s.Logger.Warn("email webhook rejected", "reason", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// The worker wraps the message in "payload".
	data, ok := envelope["payload"].(map[string]any)
	if !ok {
		data = envelope
	}

	in := email.Parse(data, time.Now())
	workspaceID, err := ingest.Workspace(r.Context(), s.DB, in.To)
	if errors.Is(err, ingest.ErrUnknownRecipient) {
		s.Logger.Warn("email for unknown address", "to", in.To)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown recipient"})
		return
	}
	if err != nil {
		s.fail(w, "resolving workspace", err)
		return
	}

	res, err := ingest.Email(r.Context(), s.DB, workspaceID, in)
	if errors.Is(err, ingest.ErrNoSender) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.fail(w, "storing email", err)
		return
	}
	s.Logger.Info("email received", "workspace", workspaceID, "thread", res.ThreadID, "event", res.EventID,
		"duplicate", res.Duplicate, "kind", res.Classification.Kind, "opt_out", res.Classification.OptOut,
		"suspicious", len(res.Suspicious) > 0)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "event_id": res.EventID, "duplicate": res.Duplicate})
}

// verifyInboundEmail checks X-VisiComms-Signature, the hex HMAC-SHA256 of
// "<timestamp>.<body>", and that X-VisiComms-Timestamp is recent. A missing
// secret rejects everything rather than turning the check off.
func (s *Server) verifyInboundEmail(r *http.Request, body []byte) error {
	if s.InboundEmailSecret == "" {
		return errors.New("INBOUND_EMAIL_SECRET is not set")
	}
	sig := r.Header.Get("X-VisiComms-Signature")
	ts := r.Header.Get("X-VisiComms-Timestamp")
	if sig == "" || ts == "" {
		return errors.New("missing signature headers")
	}
	mac := hmac.New(sha256.New, []byte(s.InboundEmailSecret))
	mac.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	at, err := parseTimestamp(ts)
	if err != nil {
		return err
	}
	if d := s.now().Sub(at); d > signatureTolerance || d < -signatureTolerance {
		return errors.New("timestamp outside tolerance")
	}
	return nil
}

// parseTimestamp accepts Unix seconds or milliseconds.
func parseTimestamp(ts string) (time.Time, error) {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, errors.New("timestamp is not a number")
	}
	if n > 1e12 {
		return time.UnixMilli(n), nil
	}
	return time.Unix(n, 0), nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Logger.Error(what, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
