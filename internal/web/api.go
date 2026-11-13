package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/sending"
)

const sessionCookie = "vc_session"

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/invites/{token}", s.getInvite)
	mux.HandleFunc("POST /api/auth/register/begin", s.registerBegin)
	mux.HandleFunc("POST /api/auth/register/finish", s.registerFinish)
	mux.HandleFunc("POST /api/auth/login/begin", s.loginBegin)
	mux.HandleFunc("POST /api/auth/login/finish", s.loginFinish)
	mux.HandleFunc("POST /api/auth/logout", s.logout)

	mux.Handle("GET /api/me", s.signedIn(s.me))
	mux.Handle("GET /api/workspaces", s.signedIn(s.workspaces))
	mux.Handle("POST /api/workspaces/{id}/switch", s.signedIn(s.switchWorkspace))
	mux.Handle("POST /api/workspaces/link/begin", s.signedIn(s.linkBegin))
	mux.Handle("POST /api/workspaces/link/finish", s.signedIn(s.linkFinish))
	mux.Handle("POST /api/invites/{token}/accept", s.signedIn(s.acceptInvite))
	mux.Handle("GET /api/attention", s.signedIn(s.attention))
	mux.Handle("GET /api/threads/{id}", s.signedIn(s.thread))
	mux.Handle("POST /api/threads/{id}/take-over", s.signedIn(s.takeOver))
	mux.Handle("POST /api/threads/{id}/hand-back", s.signedIn(s.handBack))
	mux.Handle("POST /api/threads/{id}/resolve", s.signedIn(s.resolve))
	mux.Handle("POST /api/threads/{id}/reopen", s.signedIn(s.reopen))
	mux.Handle("POST /api/threads/{id}/reply", s.signedIn(s.reply))
	mux.Handle("POST /api/threads/{id}/text-back", s.signedIn(s.textBack))
	mux.Handle("POST /api/threads/{id}/call-back", s.signedIn(s.callBack))
	mux.Handle("POST /api/proposals/{id}/approve", s.signedIn(s.approve))
	mux.Handle("POST /api/proposals/{id}/reject", s.signedIn(s.reject))
	mux.Handle("GET /api/approved-info", s.signedIn(s.approvedInfo))
	mux.Handle("POST /api/approved-info/{id}/approve", s.signedIn(s.approveInfo))
	mux.Handle("GET /api/connections", s.signedIn(s.connections))
	mux.Handle("GET /api/setup", s.signedIn(s.setup))
	mux.Handle("POST /api/setup/inbox", s.signedIn(s.createInbox))
	mux.Handle("POST /api/setup/sandbox", s.signedIn(s.useSandbox))
	mux.Handle("POST /api/invites", s.signedIn(s.invite))
	mux.Handle("GET /api/agents", s.signedIn(s.listAgents))
	mux.Handle("POST /api/agents", s.signedIn(s.createAgent))
	mux.Handle("POST /api/agents/{id}/revoke", s.signedIn(s.revokeAgent))
	mux.Handle("POST /api/agents/{id}/sending", s.signedIn(s.setAgentSending))
	mux.Handle("GET /api/fax", s.signedIn(s.faxSettings))
	mux.Handle("POST /api/fax/documents", s.signedIn(s.uploadFaxDocument))
	mux.Handle("GET /api/fax/documents/{id}", s.signedIn(s.downloadFaxDocument))
	mux.Handle("GET /api/calls/{id}/recording", s.signedIn(s.callRecording))
	mux.Handle("POST /api/faxes", s.signedIn(s.sendFax))
	mux.Handle("GET /api/sms", s.signedIn(s.smsSettings))
	s.callFlowRoutes(mux)
	s.entranceRoutes(mux)
	s.browserPhoneRoutes(mux)
	s.notificationRoutes(mux)
	s.chatConsoleRoutes(mux)
	s.slackConsoleRoutes(mux)
	s.mailRoutes(mux)
	mux.Handle("GET /api/sending", s.signedIn(s.sendingStatus))
	mux.Handle("POST /api/sending/domains", s.signedIn(s.addDomain))
	mux.Handle("POST /api/sending/domains/{id}/check", s.signedIn(s.checkDomain))
	mux.Handle("POST /api/sending/from", s.signedIn(s.setFrom))
}

// signedIn requires a session. Requests that change anything must also be
// JSON: a cross-site form post cannot set that content type, so together with
// the SameSite cookie this keeps other sites from acting as the person.
func (s *Server) signedIn(next func(http.ResponseWriter, *http.Request, auth.Person)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		pdfUpload := r.URL.Path == "/api/fax/documents" && contentType == "application/pdf"
		if r.Method != http.MethodGet && !isJSON(r) && !pdfUpload {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		p, err := s.Auth.Session(r.Context(), c.Value)
		if err != nil {
			s.clearSession(w)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		next(w, r, p)
	})
}

func isJSON(r *http.Request) bool {
	t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && t == "application/json"
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionTTL / time.Second),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSON(r) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	return true
}

// --- sign-in ---

func (s *Server) getInvite(w http.ResponseWriter, r *http.Request) {
	in, err := s.Auth.LookupInvite(r.Context(), r.PathValue("token"))
	if errors.Is(err, auth.ErrInviteInvalid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.fail(w, "invite lookup", err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Invite string `json:"invite"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, err := s.Auth.BeginRegistration(r.Context(), req.Invite)
	s.ceremony(w, c, err)
}

type finishRequest struct {
	CeremonyID string          `json:"ceremony_id"`
	Credential json.RawMessage `json:"credential"`
}

func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	var req finishRequest
	if !decode(w, r, &req) {
		return
	}
	p, token, err := s.Auth.FinishRegistration(r.Context(), req.CeremonyID, req.Credential)
	s.signedInAs(w, p, token, err)
}

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
		return
	}
	c, err := s.Auth.BeginLogin(r.Context())
	s.ceremony(w, c, err)
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	var req finishRequest
	if !decode(w, r, &req) {
		return
	}
	p, token, err := s.Auth.FinishLogin(r.Context(), req.CeremonyID, req.Credential)
	s.signedInAs(w, p, token, err)
}

func (s *Server) ceremony(w http.ResponseWriter, c auth.Ceremony, err error) {
	switch {
	case errors.Is(err, auth.ErrInviteInvalid):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		s.fail(w, "starting passkey ceremony", err)
	default:
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Server) signedInAs(w http.ResponseWriter, p auth.Person, token string, err error) {
	switch {
	case errors.Is(err, auth.ErrDenied):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That passkey didn't work. Try again."})
	case errors.Is(err, auth.ErrInviteInvalid):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		s.fail(w, "finishing passkey ceremony", err)
	default:
		s.setSession(w, token)
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.Auth.EndSession(r.Context(), c.Value)
	}
	s.clearSession(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// --- console ---

// reviewer acts for p. SMSProject is the workspace's SignalWire project;
// without one, SMS and fax sends are refused as having no sender.
func (s *Server) reviewer(ctx context.Context, p auth.Person) review.Reviewer {
	project, err := s.SignalWire.ProjectID(ctx, p.WorkspaceID)
	if err != nil && !errors.Is(err, sending.ErrNoSignalWire) {
		s.Logger.Error("loading SignalWire project", "workspace", p.WorkspaceID, "err", err)
	}
	return review.Reviewer{SMSProject: project, ID: p.ID, WorkspaceID: p.WorkspaceID, Enqueue: s.Enqueue, Provider: sending.ProviderKind(s.Sending)}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var workspace string
	if err := s.DB.QueryRow(r.Context(), `SELECT name FROM workspaces WHERE id = $1`, p.WorkspaceID).Scan(&workspace); err != nil {
		s.fail(w, "loading workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"person": p, "workspace": map[string]string{"id": p.WorkspaceID, "name": workspace}})
}

func (s *Server) workspaces(w http.ResponseWriter, r *http.Request, p auth.Person) {
	list, err := s.Auth.Workspaces(r.Context(), p)
	if err != nil {
		s.fail(w, "listing workspaces", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": orEmpty(list)})
}

// sessionToken is the caller's cookie; signedIn has already checked it.
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) switchWorkspace(w http.ResponseWriter, r *http.Request, _ auth.Person) {
	p, err := s.Auth.Switch(r.Context(), sessionToken(r), r.PathValue("id"))
	s.membership(w, p, err, "switching workspace")
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request, _ auth.Person) {
	p, err := s.Auth.AcceptInvite(r.Context(), sessionToken(r), r.PathValue("token"))
	s.membership(w, p, err, "accepting invite")
}

func (s *Server) membership(w http.ResponseWriter, p auth.Person, err error, what string) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, p)
	case errors.Is(err, auth.ErrNoWorkspace), errors.Is(err, auth.ErrInviteInvalid):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrAlreadyMember):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrDenied):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	default:
		s.fail(w, what, err)
	}
}

// linkBegin starts a passkey sign-in whose answer links that passkey's
// workspace to the signed-in person instead of opening a new session.
func (s *Server) linkBegin(w http.ResponseWriter, r *http.Request, _ auth.Person) {
	c, err := s.Auth.BeginLogin(r.Context())
	s.ceremony(w, c, err)
}

func (s *Server) linkFinish(w http.ResponseWriter, r *http.Request, _ auth.Person) {
	var req finishRequest
	if !decode(w, r, &req) {
		return
	}
	list, err := s.Auth.FinishLink(r.Context(), sessionToken(r), req.CeremonyID, req.Credential)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"workspaces": orEmpty(list)})
	case errors.Is(err, auth.ErrAlreadyMember):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "That passkey is for a workspace you can already switch to."})
	case errors.Is(err, auth.ErrDenied):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That passkey didn't work. Try again."})
	default:
		s.fail(w, "linking a workspace", err)
	}
}

func (s *Server) attention(w http.ResponseWriter, r *http.Request, p auth.Person) {
	items, err := review.Attention(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading attention", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(items)})
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request, p auth.Person) {
	v, err := review.Thread(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if err == nil && v.Channel == "chat" && s.Chat != nil {
		v.VisitorTyping = s.Chat.IsVisitorTyping(v.ID)
	}
	if s.decided(w, err, "loading thread") {
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *Server) takeOver(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.decided(w, review.TakeOver(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id")), "taking over") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "taken over"})
	}
}

func (s *Server) handBack(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.decided(w, review.HandBack(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id")), "handing back") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "handed back"})
	}
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.decided(w, review.Resolve(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id")), "marking a thread done") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	}
}

func (s *Server) reopen(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.decided(w, review.Reopen(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id")), "reopening a thread") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "open"})
	}
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req review.EmailPayload
	if !decode(w, r, &req) {
		return
	}
	res, err := review.Reply(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"), req)
	if s.decided(w, err, "replying") {
		writeJSON(w, http.StatusOK, res)
	}
}

// textBack answers a call by texting the caller from the number they called.
func (s *Server) textBack(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := review.TextBack(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"), req.Body)
	if s.decided(w, err, "texting a caller back") {
		writeJSON(w, http.StatusOK, res)
	}
}

// callBack records that a person is calling a caller back and returns the
// numbers for the browser phone to dial.
func (s *Server) callBack(w http.ResponseWriter, r *http.Request, p auth.Person) {
	from, to, err := review.CallBack(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"))
	if s.decided(w, err, "calling a caller back") {
		writeJSON(w, http.StatusOK, map[string]string{"from": from, "to": to})
	}
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Edit *review.EmailPayload `json:"edit"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.decided(w, review.Approve(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"), req.Edit), "approving") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
	}
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.decided(w, review.Reject(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"), req.Note), "rejecting") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
	}
}

func (s *Server) approvedInfo(w http.ResponseWriter, r *http.Request, p auth.Person) {
	list, err := review.ApprovedInfoVersions(r.Context(), s.DB, p.WorkspaceID)
	if s.decided(w, err, "loading approved info") {
		writeJSON(w, http.StatusOK, map[string]any{"versions": orEmpty(list)})
	}
}

func (s *Server) approveInfo(w http.ResponseWriter, r *http.Request, p auth.Person) {
	voided, err := review.ApproveInfo(r.Context(), s.DB, s.reviewer(r.Context(), p), r.PathValue("id"))
	if s.decided(w, err, "approving info") {
		writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "voided_drafts": voided})
	}
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request, p auth.Person) {
	c, err := review.ListConnections(r.Context(), s.DB, p.WorkspaceID)
	if s.decided(w, err, "loading connections") {
		writeJSON(w, http.StatusOK, c)
	}
}

// decided maps review errors to responses; true means carry on.
func (s *Server) decided(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, review.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, review.ErrEmptyReply), errors.Is(err, review.ErrSMSLength), errors.Is(err, review.ErrChatLength), errors.Is(err, chat.ErrQuickReplies):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, review.ErrConflict), errors.Is(err, review.ErrOptedOut), errors.Is(err, review.ErrHeldDraft), errors.Is(err, review.ErrStaleInfo),
		errors.Is(err, review.ErrNoSender), errors.Is(err, review.ErrNoAddress):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		s.fail(w, what, err)
	}
	return false
}

func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request, p auth.Person) {
	st, err := review.Setup(r.Context(), s.DB, p.WorkspaceID, sending.ProviderKind(s.Sending))
	if s.decided(w, err, "loading setup") {
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *Server) createInbox(w http.ResponseWriter, r *http.Request, p auth.Person) {
	domain := s.InboundDomain
	if domain == "" {
		domain = review.InboundDomain
	}
	address, err := review.CreateInbox(r.Context(), s.DB, p.WorkspaceID, domain)
	if s.decided(w, err, "creating inbox") {
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
	}
}

func (s *Server) useSandbox(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.decided(w, review.UseSandbox(r.Context(), s.DB, p.WorkspaceID), "adding sandbox") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "sandbox on"})
	}
}

// invite lets anyone in a workspace invite someone else into it. The link is
// returned to the inviter to pass on; nothing is emailed yet.
func (s *Server) invite(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Email, req.Name = strings.TrimSpace(req.Email), strings.TrimSpace(req.Name)
	if req.Name == "" || !strings.Contains(req.Email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Enter a name and an email address."})
		return
	}
	token, err := s.Auth.CreateInvite(r.Context(), p.WorkspaceID, req.Email, req.Name, &p.ID)
	if s.decided(w, err, "creating invite") {
		writeJSON(w, http.StatusOK, map[string]string{"url": s.AppURL + "/invite/" + token})
	}
}

func (s *Server) sendingStatus(w http.ResponseWriter, r *http.Request, p auth.Person) {
	domains, err := sending.ListDomains(r.Context(), s.DB, p.WorkspaceID, sending.ProviderKind(s.Sending))
	if err != nil {
		s.fail(w, "listing domains", err)
		return
	}
	var from *sending.From
	var f sending.From
	err = s.DB.QueryRow(r.Context(), `SELECT coalesce(settings->>'from_name', ''), coalesce(settings->>'from_address', '')
		FROM connectors WHERE workspace_id = $1 AND kind = $2`, p.WorkspaceID, sending.ProviderKind(s.Sending)).Scan(&f.Name, &f.Address)
	if err == nil {
		from = &f
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": domains, "from": from, "available": s.Sending != nil, "provider": sending.ProviderKind(s.Sending)})
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Domain string `json:"domain"`
	}
	if !decode(w, r, &req) || !s.sendingAvailable(w) {
		return
	}
	d, err := sending.AddDomain(r.Context(), s.DB, s.Sending, p.WorkspaceID, req.Domain)
	if s.sendingDecided(w, err, "adding domain") {
		writeJSON(w, http.StatusOK, d)
	}
}

func (s *Server) checkDomain(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if !s.sendingAvailable(w) {
		return
	}
	d, err := sending.CheckDomain(r.Context(), s.DB, s.Sending, p.WorkspaceID, r.PathValue("id"))
	if s.sendingDecided(w, err, "checking domain") {
		writeJSON(w, http.StatusOK, d)
	}
}

func (s *Server) setFrom(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req sending.From
	if !decode(w, r, &req) || !s.sendingAvailable(w) {
		return
	}
	if s.sendingDecided(w, sending.SetFrom(r.Context(), s.DB, p.WorkspaceID, req, sending.ProviderKind(s.Sending)), "setting from") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	}
}

func (s *Server) sendingAvailable(w http.ResponseWriter) bool {
	if s.Sending == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Sending isn't configured on this server yet."})
		return false
	}
	return true
}

func (s *Server) sendingDecided(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, sending.ErrBadDomain), errors.Is(err, sending.ErrBadAddress):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, sending.ErrDomainNotReady):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		s.fail(w, what, err)
	}
	return false
}

func (s *Server) smsSettings(w http.ResponseWriter, r *http.Request, p auth.Person) {
	numbers, err := sending.SMSNumbers(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "listing SMS numbers", err)
		return
	}
	smsNumbers := []sending.PhoneNumber{}
	for _, n := range numbers {
		if n.Capabilities["sms"] {
			smsNumbers = append(smsNumbers, n)
		}
	}
	available, err := s.signalWireAvailable(r.Context(), p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading SignalWire project", err)
		return
	}
	inbound, status := "", ""
	if available {
		inbound, status = s.SignalWire.Webhooks().InboundURL(), s.SignalWire.Webhooks().StatusURL()
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": available, "numbers": smsNumbers, "inbound_url": inbound, "status_url": status})
}

// signalWireAvailable reports whether the workspace has a SignalWire project,
// its own or the installation's.
func (s *Server) signalWireAvailable(ctx context.Context, workspaceID string) (bool, error) {
	_, err := s.SignalWire.ProjectID(ctx, workspaceID)
	if errors.Is(err, sending.ErrNoSignalWire) {
		return false, nil
	}
	return err == nil, err
}
