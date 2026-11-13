package web

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/postal"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/sending"
)

// Postal mail: each workspace connects its own Lob account, people send PDFs
// as letters, and Lob's webhooks put each letter's progress on the thread.

func (s *Server) mailRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/mail", s.signedIn(s.mailSettings))
	mux.Handle("PUT /api/mail", s.signedIn(s.connectMail))
	mux.Handle("GET /api/contacts/{id}/addresses", s.signedIn(s.contactAddresses))
	mux.Handle("POST /api/contacts/{id}/addresses", s.signedIn(s.addContactAddress))
	mux.Handle("POST /api/letters/estimate", s.signedIn(s.estimateLetter))
	mux.Handle("POST /api/letters", s.signedIn(s.sendLetter))
	mux.Handle("POST /api/letters/{id}/cancel", s.signedIn(s.cancelLetter))
}

func (s *Server) mailAvailable() bool { return s.Mail != nil && s.Mail.Keys != nil }

func (s *Server) mailSettings(w http.ResponseWriter, r *http.Request, p auth.Person) {
	out := map[string]any{"available": s.mailAvailable(), "connected": false}
	if !s.mailAvailable() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	settings, ok, err := s.Mail.Settings(r.Context(), p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading mail settings", err)
		return
	}
	spent, err := postal.MonthSpend(r.Context(), s.DB, p.WorkspaceID, "")
	if err != nil {
		s.fail(w, "loading mail spend", err)
		return
	}
	out["connected"], out["month_spent_cents"], out["default_cap_cents"] = ok, spent, sending.DefaultCapCents
	if ok {
		out["settings"] = settings
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) connectMail(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if !s.mailAvailable() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the server operator must set VISICOMMS_MASTER_KEY before Lob can be connected"})
		return
	}
	var in sending.MailConnect
	if !decode(w, r, &in) {
		return
	}
	settings, err := s.Mail.Connect(r.Context(), p.WorkspaceID, in)
	var rejected *sending.Rejected
	switch {
	case errors.As(err, &rejected):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil && errors.Is(err, sending.ErrNoLob):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		s.fail(w, "connecting Lob", err)
	default:
		writeJSON(w, http.StatusOK, settings)
	}
}

func (s *Server) contactAddresses(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	list, err := postal.Addresses(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if err != nil {
		s.fail(w, "listing addresses", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": orEmpty(list)})
}

// addContactAddress checks the address with the workspace's Lob account
// before saving it. Lob's standardised form is what is stored.
func (s *Server) addContactAddress(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	var in postal.Address
	if !decode(w, r, &in) {
		return
	}
	in, err := in.Clean()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	lob, ok := s.workspaceLob(w, r, p)
	if !ok {
		return
	}
	checked, deliverability, err := lob.Verify(r.Context(), in)
	if err != nil {
		if !s.lobRefused(w, err) {
			s.fail(w, "checking an address with Lob", err)
		}
		return
	}
	saved, err := review.SaveAddress(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"), checked, deliverability)
	switch {
	case errors.Is(err, postal.ErrAddress), errors.Is(err, postal.ErrUndeliverable):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, postal.ErrAddressTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case s.decided(w, err, "saving an address"):
		writeJSON(w, http.StatusCreated, saved)
	}
}

func (s *Server) estimateLetter(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in struct {
		DocumentID string `json:"document_id"`
		postal.Options
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := uuid.Parse(in.DocumentID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a PDF document"})
		return
	}
	est, err := review.EstimateLetter(r.Context(), s.DB, p.WorkspaceID, in.DocumentID, in.Options)
	switch {
	case errors.Is(err, postal.ErrOptions), errors.Is(err, review.ErrFaxDocument):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil:
		s.fail(w, "estimating a letter", err)
	default:
		writeJSON(w, http.StatusOK, est)
	}
}

func (s *Server) sendLetter(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req review.LetterRequest
	if !decode(w, r, &req) {
		return
	}
	for _, id := range []string{req.ContactID, req.AddressID, req.DocumentID} {
		if _, err := uuid.Parse(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a contact, one of their addresses and a PDF"})
			return
		}
	}
	if req.RequestID != "" {
		if _, err := uuid.Parse(req.RequestID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid letter request ID"})
			return
		}
	}
	result, err := review.SendLetter(r.Context(), s.DB, s.reviewer(r.Context(), p), req)
	switch {
	case errors.Is(err, postal.ErrOptions), errors.Is(err, review.ErrFaxDocument), errors.Is(err, review.ErrMailAddress), errors.Is(err, review.ErrLetterPages):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, review.ErrNoMail), errors.Is(err, review.ErrOverMailLimit):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case s.decided(w, err, "queuing a letter"):
		writeJSON(w, http.StatusAccepted, result)
	}
}

// cancelLetter asks Lob to stop a letter before it is printed. Lob refuses
// once the letter's send date has passed, and says so.
func (s *Server) cancelLetter(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	letter, err := review.CancellableLetter(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if errors.Is(err, review.ErrNotCancellable) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.fail(w, "finding a letter", err)
		return
	}
	if !s.mailAvailable() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": review.ErrNoMail.Error()})
		return
	}
	lob, owner, err := s.Mail.Client(r.Context(), letter.ConnectorID)
	if err != nil || owner != p.WorkspaceID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the Lob account this letter went through is no longer connected"})
		return
	}
	if err := lob.CancelLetter(r.Context(), letter.LobID); err != nil {
		if !s.lobRefused(w, err) {
			s.fail(w, "cancelling a letter", err)
		}
		return
	}
	if err := review.LetterCancelled(r.Context(), s.DB, s.reviewer(r.Context(), p), letter); err != nil {
		s.fail(w, "recording a cancelled letter", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelled"})
}

// workspaceLob is the person's workspace's Lob account, or a response saying
// there is none.
func (s *Server) workspaceLob(w http.ResponseWriter, r *http.Request, p auth.Person) (*sending.Lob, bool) {
	if !s.mailAvailable() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": review.ErrNoMail.Error()})
		return nil, false
	}
	settings, ok, err := s.Mail.Settings(r.Context(), p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading mail settings", err)
		return nil, false
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": review.ErrNoMail.Error()})
		return nil, false
	}
	lob, _, err := s.Mail.Client(r.Context(), settings.ConnectorID)
	if err != nil {
		s.fail(w, "opening the Lob account", err)
		return nil, false
	}
	return lob, true
}

// lobRefused answers with Lob's own words when Lob turned the request down.
func (s *Server) lobRefused(w http.ResponseWriter, err error) bool {
	var rejected *sending.Rejected
	if errors.As(err, &rejected) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": rejected.Reason})
		return true
	}
	return false
}

// lobWebhook receives Lob's events for one workspace's account, signed with
// that account's webhook secret.
func (s *Server) lobWebhook(w http.ResponseWriter, r *http.Request) {
	connector := r.PathValue("connector")
	if _, err := uuid.Parse(connector); err != nil || !s.mailAvailable() {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too large"})
		return
	}
	err = s.Mail.VerifyWebhook(r.Context(), connector, r.Header.Get("Lob-Signature-Timestamp"), r.Header.Get("Lob-Signature"), body, s.now())
	switch {
	case errors.Is(err, sending.ErrNoLob):
		http.NotFound(w, r)
		return
	case errors.Is(err, sending.ErrLobSignature):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad signature"})
		return
	case err != nil:
		s.fail(w, "verifying a Lob webhook", err)
		return
	}
	if err := sending.ApplyLobEvent(r.Context(), s.DB, connector, body); err != nil {
		s.fail(w, "applying a Lob event", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
