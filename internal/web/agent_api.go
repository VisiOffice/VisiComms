package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/mcp"
	"github.com/VisiOffice/VisiComms/internal/review"
)

// agentRoutes are the API for outside agents, authenticated by an agent
// token instead of a session. An agent reads its workspace and drafts; only
// an integration a person has allowed to can send without approval.
func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me", s.asAgent(s.agentMe))
	mux.Handle("GET /api/v1/threads", s.asAgent(s.agentThreads))
	mux.Handle("GET /api/v1/threads/{id}", s.asAgent(s.agentThread))
	mux.Handle("POST /api/v1/threads/{id}/drafts", s.asAgent(s.agentDraft))
	mux.Handle("POST /api/v1/threads/{id}/resolve", s.asAgent(s.agentResolve))
	mux.Handle("POST /api/v1/threads/{id}/escalate", s.asAgent(s.agentEscalate))
	mux.Handle("GET /api/v1/proposals", s.asAgent(s.agentProposals))
	mux.Handle("GET /api/v1/proposals/{id}", s.asAgent(s.agentProposal))
	mux.Handle("POST /api/v1/messages", s.asAgent(s.agentMessage))
	mux.Handle("GET /api/v1/messages/{id}", s.asAgent(s.agentSentMessage))
	mux.Handle("GET /api/v1/contacts", s.asAgent(s.agentContacts))
	mux.Handle("GET /api/v1/events", s.asAgent(s.agentEvents))
	s.agentCallFlowRoutes(mux)
	s.agentNotificationRoutes(mux)

	// The same tools over MCP. Stateless: no server-sent stream to open.
	tools := &mcp.Server{DB: s.DB, Logger: s.Logger, Registry: s.SMSRegistry,
		Reviewer: func(ctx context.Context, workspaceID, personID string) review.Reviewer {
			return s.reviewer(ctx, auth.Person{ID: personID, WorkspaceID: workspaceID})
		}}
	mux.Handle("POST /mcp", s.asMCP(func(w http.ResponseWriter, r *http.Request, agents []agent.Agent) {
		// A browser page cannot reach this with an agent's token unless the
		// token was given to it, but MCP asks servers to check Origin anyway,
		// against DNS rebinding.
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.AppURL {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
			return
		}
		tools.Serve(w, r, agents...)
	}))
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

// asAgent requires a valid agent token in the Authorization header.
func (s *Server) asAgent(next func(http.ResponseWriter, *http.Request, agent.Agent)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="visicomms"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "send an agent token as Authorization: Bearer"})
			return
		}
		a, err := agent.Authenticate(r.Context(), s.DB, strings.TrimSpace(token))
		if errors.Is(err, agent.ErrDenied) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="visicomms", error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			s.fail(w, "checking agent token", err)
			return
		}
		next(w, r, a)
	})
}

func (s *Server) agentMe(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var workspace string
	if err := s.DB.QueryRow(r.Context(), `SELECT name FROM workspaces WHERE id = $1`, a.WorkspaceID).Scan(&workspace); err != nil {
		s.fail(w, "loading workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": a, "workspace": map[string]string{"id": a.WorkspaceID, "name": workspace}})
}

func (s *Server) agentThreads(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := agent.Threads(r.Context(), s.DB, a.WorkspaceID, agent.ThreadFilter{
		State: q.Get("state"), Waiting: q.Get("waiting") == "true", Limit: limit,
	})
	if s.agentDecided(w, err, "listing threads") {
		writeJSON(w, http.StatusOK, map[string]any{"threads": orEmpty(list)})
	}
}

func (s *Server) agentThread(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	t, err := agent.Thread(r.Context(), s.DB, a.WorkspaceID, r.PathValue("id"), r.URL.Query().Get("all") == "true")
	if s.agentDecided(w, err, "loading thread") {
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) agentDraft(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var d agent.Draft
	if !decode(w, r, &d) {
		return
	}
	res, err := agent.Propose(r.Context(), s.DB, a, r.PathValue("id"), d)
	if s.agentDecided(w, err, "drafting") {
		writeJSON(w, http.StatusOK, res)
	}
}

// agentMessage starts a thread with a new email or text. It is held for a
// person unless the request says send and a person has allowed this token to.
func (s *Server) agentMessage(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var req struct {
		agent.Message
		Send           bool   `json:"send"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &req) {
		return
	}
	var res agent.Composed
	var err error
	if req.Send {
		reviewer := s.reviewer(r.Context(), auth.Person{ID: a.ID, WorkspaceID: a.WorkspaceID})
		res, err = agent.ComposeAndSend(r.Context(), s.DB, a, reviewer, req.Message, req.IdempotencyKey)
	} else {
		res, err = agent.Compose(r.Context(), s.DB, a, req.Message)
	}
	if s.agentDecided(w, err, "composing") {
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) agentSentMessage(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	m, err := agent.GetSentMessage(r.Context(), s.DB, a, r.PathValue("id"))
	if s.agentDecided(w, err, "loading message") {
		writeJSON(w, http.StatusOK, m)
	}
}

type agentNote struct {
	Note string `json:"note"`
}

func (s *Server) agentResolve(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var req agentNote
	if !decode(w, r, &req) {
		return
	}
	if s.agentDecided(w, agent.Resolve(r.Context(), s.DB, a, r.PathValue("id"), req.Note), "resolving") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	}
}

func (s *Server) agentEscalate(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var req agentNote
	if !decode(w, r, &req) {
		return
	}
	if s.agentDecided(w, agent.Escalate(r.Context(), s.DB, a, r.PathValue("id"), req.Note), "escalating") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "escalated"})
	}
}

func (s *Server) agentProposals(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	list, err := agent.Proposals(r.Context(), s.DB, a, r.URL.Query().Get("status"), r.URL.Query().Get("everyone") == "true")
	if s.agentDecided(w, err, "listing proposals") {
		writeJSON(w, http.StatusOK, map[string]any{"proposals": orEmpty(list)})
	}
}

func (s *Server) agentProposal(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	p, err := agent.GetProposal(r.Context(), s.DB, a, r.PathValue("id"))
	if s.agentDecided(w, err, "loading proposal") {
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Server) agentContacts(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	list, err := agent.SearchContacts(r.Context(), s.DB, a.WorkspaceID, r.URL.Query().Get("q"))
	if s.agentDecided(w, err, "searching contacts") {
		writeJSON(w, http.StatusOK, map[string]any{"contacts": orEmpty(list)})
	}
}

func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, next, err := agent.Events(r.Context(), s.DB, a.WorkspaceID, after, limit)
	if s.agentDecided(w, err, "listing events") {
		writeJSON(w, http.StatusOK, map[string]any{"events": orEmpty(list), "next_after": next})
	}
}

func (s *Server) agentDecided(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, agent.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, agent.ErrEmptyDraft), errors.Is(err, agent.ErrSMSLength), errors.Is(err, agent.ErrChatLength), errors.Is(err, chat.ErrQuickReplies),
		errors.Is(err, agent.ErrNoteNeeded), errors.Is(err, agent.ErrBadName),
		errors.Is(err, agent.ErrComposeChannel), errors.Is(err, agent.ErrComposeTo), errors.Is(err, agent.ErrComposeSubject), errors.Is(err, agent.ErrComposeFrom),
		errors.Is(err, agent.ErrSendKey):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, agent.ErrSendNotAllowed):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, agent.ErrKeyReused), errors.Is(err, review.ErrNoSender):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		s.fail(w, what, err)
	}
	return false
}

// --- managing agents from the console ---

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, p auth.Person) {
	list, err := agent.List(r.Context(), s.DB, p.WorkspaceID)
	if s.agentDecided(w, err, "listing agents") {
		writeJSON(w, http.StatusOK, map[string]any{"agents": orEmpty(list)})
	}
}

// createAgent returns the new agent's token. It is shown this once; only its
// hash is kept.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	a, token, err := agent.Create(r.Context(), s.DB, p.WorkspaceID, req.Name, &p.ID)
	if s.agentDecided(w, err, "creating agent") {
		writeJSON(w, http.StatusOK, map[string]any{"agent": a, "token": token})
	}
}

// setAgentSending is a person allowing a token to send without approval, or
// withdrawing that. A session only: no token can reach it.
func (s *Server) setAgentSending(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req struct {
		Allowed bool `json:"allowed"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.agentDecided(w, agent.SetMaySend(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), p.ID, req.Allowed), "changing sending") {
		writeJSON(w, http.StatusOK, map[string]bool{"allowed": req.Allowed})
	}
}

func (s *Server) revokeAgent(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.agentDecided(w, agent.Revoke(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id")), "revoking agent") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}
