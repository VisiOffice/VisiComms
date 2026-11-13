package web

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/oauth"
)

// OAuth for MCP clients (package oauth). The discovery documents,
// registration and token endpoints are called by the client's servers, with
// no cookies; the consent page is in the console, which calls /api/oauth.

func (s *Server) oauthRoutes(mux *http.ServeMux) {
	public := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// No cookies are read here, so any origin may call (browser-based
			// MCP clients do).
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if s.OAuth == nil {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	resource := public(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.OAuth.ProtectedResource()) })
	metadata := public(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.OAuth.Metadata()) })
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		mux.HandleFunc("GET "+p, resource)
		mux.HandleFunc("OPTIONS "+p, resource)
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp", "/.well-known/openid-configuration"} {
		mux.HandleFunc("GET "+p, metadata)
		mux.HandleFunc("OPTIONS "+p, metadata)
	}
	mux.HandleFunc("POST /oauth/register", public(s.oauthRegister))
	mux.HandleFunc("OPTIONS /oauth/register", public(s.oauthRegister))
	mux.HandleFunc("GET /oauth/authorize", s.oauthAuthorize)
	mux.HandleFunc("POST /oauth/token", public(s.oauthToken))
	mux.HandleFunc("OPTIONS /oauth/token", public(s.oauthToken))
	mux.HandleFunc("POST /oauth/revoke", public(s.oauthRevoke))

	mux.Handle("GET /api/oauth/requests/{id}", s.signedIn(s.oauthPending))
	mux.Handle("POST /api/oauth/requests/{id}/approve", s.signedIn(s.oauthApprove))
	mux.Handle("POST /api/oauth/requests/{id}/deny", s.signedIn(s.oauthDeny))
}

func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request) {
	var in oauth.Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, oauth.Error{Code: "invalid_client_metadata", Description: "send the registration as JSON"})
		return
	}
	out, err := s.OAuth.Register(r.Context(), in)
	if s.oauthFailed(w, err, "registering an OAuth client") {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// oauthAuthorize sends the browser to the console's consent page, or back
// to the client with an error. An unknown client or redirect gets a page
// here instead, never a redirect.
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	to, err := s.OAuth.Authorize(r.Context(), r.URL.Query())
	if errors.Is(err, oauth.ErrBadRedirect) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>VisiComms</title><p>This connection request is not valid: " +
			html.EscapeString(err.Error()) + ". Remove the connector and add it again.</p>"))
		return
	}
	if err != nil {
		s.fail(w, "authorizing an OAuth client", err)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauth.Error{Code: "invalid_request", Description: "send the request as a form"})
		return
	}
	id, secret, _ := r.BasicAuth()
	res, err := s.OAuth.Token(r.Context(), r.PostForm, id, secret)
	if s.oauthFailed(w, err, "issuing an OAuth token") {
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauth.Error{Code: "invalid_request", Description: "send the request as a form"})
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if s.oauthFailed(w, s.OAuth.Revoke(r.Context(), r.PostForm.Get("token"), id, secret), "revoking an OAuth token") {
		return
	}
	w.WriteHeader(http.StatusOK)
}

// oauthFailed writes an OAuth error response; true means it did.
func (s *Server) oauthFailed(w http.ResponseWriter, err error, what string) bool {
	if err == nil {
		return false
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		status := http.StatusBadRequest
		if oe.Code == "invalid_client" {
			status = http.StatusUnauthorized
		}
		writeJSON(w, status, oe)
		return true
	}
	s.Logger.Error(what, "err", err)
	writeJSON(w, http.StatusInternalServerError, oauth.Error{Code: "server_error", Description: "try again"})
	return true
}

func (s *Server) oauthPending(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	req, err := s.OAuth.Pending(r.Context(), r.PathValue("id"), p.ID)
	if s.consentFailed(w, err) {
		return
	}
	req.Workspaces = orEmpty(req.Workspaces)
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) oauthApprove(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Workspaces []string `json:"workspaces"`
	}
	if !decode(w, r, &in) {
		return
	}
	to, err := s.OAuth.Approve(r.Context(), r.PathValue("id"), p.ID, in.Workspaces)
	if s.consentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": to})
}

func (s *Server) oauthDeny(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	to, err := s.OAuth.Deny(r.Context(), r.PathValue("id"))
	if s.consentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": to})
}

func (s *Server) consentFailed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, oauth.ErrNoRequest):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, oauth.ErrNoWorkspace):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		s.fail(w, "OAuth consent", err)
	}
	return true
}

// asMCP accepts an agent token or an OAuth access token. Without either it
// answers 401 with where to find the authorization server, which is how a
// client such as Claude starts the OAuth flow.
func (s *Server) asMCP(next func(http.ResponseWriter, *http.Request, []agent.Agent)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := `Bearer realm="visicomms"`
		if s.OAuth != nil {
			challenge += `, resource_metadata="` + s.OAuth.Issuer + `/.well-known/oauth-protected-resource"`
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in: connect with OAuth, or send an agent token as Authorization: Bearer"})
			return
		}
		var agents []agent.Agent
		var err error
		if oauth.IsToken(token) && s.OAuth != nil {
			agents, err = s.OAuth.Authenticate(r.Context(), token)
		} else {
			var a agent.Agent
			if a, err = agent.Authenticate(r.Context(), s.DB, token); err == nil {
				agents = []agent.Agent{a}
			}
		}
		if errors.Is(err, agent.ErrDenied) {
			w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the token is invalid, expired or revoked"})
			return
		}
		if err != nil {
			s.fail(w, "checking MCP token", err)
			return
		}
		next(w, r, agents)
	})
}
