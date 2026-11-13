package web

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/sending"
)

// verifiedSMSForm checks a Compatibility callback against the project it names:
// the client it returns is that project's, and every later lookup is scoped to
// the same AccountSid. An unknown project gets the same 403 as a bad signature.
func (s *Server) verifiedSMSForm(w http.ResponseWriter, r *http.Request) (url.Values, *sending.SignalWire, bool) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		writeJSON(w, 415, map[string]string{"error": "send a Compatibility form callback"})
		return nil, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid form"})
		return nil, nil, false
	}
	for _, key := range []string{"MessageSid", "AccountSid", "From", "To", "Body", "MessageStatus", "FaxSid", "FaxStatus", "MediaUrl", "NumPages"} {
		if len(r.PostForm[key]) > 1 {
			writeJSON(w, 400, map[string]string{"error": "ambiguous form"})
			return nil, nil, false
		}
	}
	sw, _, err := s.SignalWire.ForProject(r.Context(), r.PostForm.Get("AccountSid"))
	if err != nil && !errors.Is(err, sending.ErrUnknownProject) {
		s.fail(w, "loading SignalWire project", err)
		return nil, nil, false
	}
	signature := r.Header.Get("X-SignalWire-Signature")
	if signature == "" {
		signature = r.Header.Get("X-Twilio-Signature")
	}
	if sw == nil || !sw.Verify(r.URL.RequestURI(), signature, r.PostForm) {
		writeJSON(w, 403, map[string]string{"error": "signature or project does not verify"})
		return nil, nil, false
	}
	return r.PostForm, sw, true
}

func (s *Server) smsWebhook(w http.ResponseWriter, r *http.Request) {
	form, _, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	mediaCount, _ := strconv.Atoi(form.Get("NumMedia"))
	result, err := ingest.ReceiveSMS(r.Context(), s.DB, ingest.SMS{SID: form.Get("MessageSid"), From: form.Get("From"), To: form.Get("To"), Body: form.Get("Body"), MediaCount: mediaCount, Project: form.Get("AccountSid")})
	if errors.Is(err, ingest.ErrUnknownRecipient) {
		writeJSON(w, 404, map[string]string{"error": "unknown recipient"})
		return
	}
	if errors.Is(err, ingest.ErrSMSMessage) {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.fail(w, "receiving SMS", err)
		return
	}
	s.Logger.Info("SMS received", "thread", result.ThreadID, "duplicate", result.Duplicate)
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response></Response>`))
}

func (s *Server) smsStatusWebhook(w http.ResponseWriter, r *http.Request) {
	form, _, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	status := sending.SMSStatus{SID: form.Get("MessageSid"), Status: strings.ToLower(form.Get("MessageStatus")), ErrorCode: form.Get("ErrorCode"), Project: form.Get("AccountSid")}
	if status.SID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing message SID"})
		return
	}
	if err := sending.ApplySMSStatus(r.Context(), s.DB, status); err != nil {
		s.fail(w, "applying SMS delivery event", err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
