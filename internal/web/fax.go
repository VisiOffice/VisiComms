package web

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/google/uuid"
)

func (s *Server) faxSettings(w http.ResponseWriter, r *http.Request, p auth.Person) {
	numbers, err := sending.SMSNumbers(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "listing fax numbers", err)
		return
	}
	faxNumbers := []sending.PhoneNumber{}
	for _, n := range numbers {
		if n.Capabilities["fax"] {
			faxNumbers = append(faxNumbers, n)
		}
	}
	available, err := s.signalWireAvailable(r.Context(), p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading SignalWire project", err)
		return
	}
	inbound := ""
	if available {
		inbound = s.SignalWire.Webhooks().FaxInboundURL()
	}
	writeJSON(w, 200, map[string]any{"available": available, "numbers": faxNumbers, "inbound_url": inbound})
}
func (s *Server) uploadFaxDocument(w http.ResponseWriter, r *http.Request, p auth.Person) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, files.MaxPDF))
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "PDF exceeds 10 MB"})
		return
	}
	filename, err := url.PathUnescape(r.Header.Get("X-Filename"))
	if err != nil {
		filename = "document.pdf"
	}
	doc, err := files.Store(r.Context(), s.DB, p.WorkspaceID, filename, data)
	if errors.Is(err, files.ErrPDF) {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.fail(w, "storing fax PDF", err)
		return
	}
	writeJSON(w, 201, doc)
}
func (s *Server) downloadFaxDocument(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	doc, err := files.Load(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if db.NotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "loading PDF", err)
		return
	}
	servePDF(w, doc)
}
func (s *Server) faxMedia(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if len(token) != 64 {
		http.NotFound(w, r)
		return
	}
	doc, err := files.LoadGrant(r.Context(), s.DB, token)
	if db.NotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "loading provider fax media", err)
		return
	}
	servePDF(w, doc)
}
func servePDF(w http.ResponseWriter, doc files.Document) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": doc.Filename}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", strconv.Itoa(len(doc.Content)))
	w.Write(doc.Content)
}
func (s *Server) sendFax(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req review.FaxRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.DocumentID); err != nil {
		writeJSON(w, 400, map[string]string{"error": "choose a PDF document"})
		return
	}
	if req.RequestID != "" {
		if _, err := uuid.Parse(req.RequestID); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid fax request ID"})
			return
		}
	}
	result, err := review.SendFax(r.Context(), s.DB, s.reviewer(r.Context(), p), req)
	switch {
	case errors.Is(err, phone.ErrNumber), errors.Is(err, review.ErrFaxDocument):
		writeJSON(w, 400, map[string]string{"error": err.Error()})
	case s.decided(w, err, "queuing fax"):
		writeJSON(w, 202, result)
	}
}
func (s *Server) faxInbound(w http.ResponseWriter, r *http.Request) {
	form, sw, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	to, err := phone.Normalize(form.Get("To"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid fax number"})
		return
	}
	var owned bool
	err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM phone_numbers WHERE number=$1 AND provider_project_id=$2 AND capabilities->>'fax'='true')`, to, sw.ProjectID()).Scan(&owned)
	if err != nil {
		s.fail(w, "resolving fax number", err)
		return
	}
	if !owned {
		http.NotFound(w, r)
		return
	}
	var target bytes.Buffer
	xml.EscapeText(&target, []byte(sw.FaxReceivedURL()))
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Receive action="%s"/></Response>`, target.String())
}
func (s *Server) faxReceived(w http.ResponseWriter, r *http.Request) {
	form, sw, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	status := strings.ToLower(form.Get("FaxStatus"))
	if status != "received" && status != "failed" {
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	pages, _ := strconv.Atoi(form.Get("NumPages"))
	in := ingest.Fax{SID: form.Get("FaxSid"), From: form.Get("From"), To: form.Get("To"), Project: form.Get("AccountSid"), Status: status, ErrorCode: form.Get("ErrorCode"), Pages: pages}
	if in.SID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing fax SID"})
		return
	}
	if _, err := phone.Normalize(in.From); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid fax sender"})
		return
	}
	// Resolve ownership before any media fetch and acknowledge saved redeliveries
	// even if the provider media URL has expired since the original receipt.
	to, err := phone.Normalize(in.To)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid fax recipient"})
		return
	}
	var ws string
	err = s.DB.QueryRow(r.Context(), `SELECT workspace_id FROM phone_numbers WHERE number=$1 AND provider_project_id=$2 AND capabilities->>'fax'='true'`, to, in.Project).Scan(&ws)
	if db.NotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "resolving incoming fax", err)
		return
	}
	var duplicate bool
	err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=$1 AND provider='signalwire_fax_in' AND provider_ref=$2)`, ws, in.SID+":"+status).Scan(&duplicate)
	if err != nil {
		s.fail(w, "checking fax receipt", err)
		return
	}
	if duplicate {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if status == "received" {
		in.PDF, err = sw.DownloadFax(r.Context(), form.Get("MediaUrl"))
		if err != nil {
			s.fail(w, "retrieving received fax PDF", err)
			return
		}
	}
	if _, err := ingest.ReceiveFax(r.Context(), s.DB, in); err != nil {
		if errors.Is(err, ingest.ErrFax) {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		s.fail(w, "storing fax receipt", err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *Server) faxStatus(w http.ResponseWriter, r *http.Request) {
	form, _, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	if form.Get("FaxSid") == "" {
		writeJSON(w, 400, map[string]string{"error": "missing fax SID"})
		return
	}
	pages, _ := strconv.Atoi(form.Get("NumPages"))
	status := sending.FaxStatus{SID: form.Get("FaxSid"), Status: strings.ToLower(form.Get("FaxStatus")), Project: form.Get("AccountSid"), ErrorCode: form.Get("ErrorCode"), Pages: pages}
	if err := sending.ApplyFaxStatus(r.Context(), s.DB, status); err != nil {
		s.fail(w, "applying fax status", err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
