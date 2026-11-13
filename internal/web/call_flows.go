package web

import (
	"errors"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// Call flows: a person sets one in the console, an agent over the API or MCP.
func (s *Server) callFlowRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/numbers", s.signedIn(s.listCallFlows))
	mux.Handle("PUT /api/numbers/{number}/call-flow", s.signedIn(s.setCallFlow))
	mux.Handle("POST /api/numbers/{number}/call-flow/activate", s.signedIn(s.activateCallFlow))
	mux.Handle("POST /api/numbers/{number}/call-flow/discard", s.signedIn(s.discardCallFlow))
}

func (s *Server) agentCallFlowRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/numbers", s.asAgent(s.agentCallFlows))
	mux.Handle("PUT /api/v1/numbers/{number}/call-flow", s.asAgent(s.agentSetCallFlow))
}

type numbersResponse struct {
	Numbers   []voice.Flows   `json:"numbers"`
	Available map[string]bool `json:"available_modes"`
	Voices    []string        `json:"voices"`
}

func (s *Server) callFlowsFor(w http.ResponseWriter, r *http.Request, workspaceID string) {
	flows, err := voice.List(r.Context(), s.DB, workspaceID)
	if err != nil {
		s.fail(w, "listing call flows", err)
		return
	}
	writeJSON(w, http.StatusOK, numbersResponse{Numbers: flows, Available: voice.Available, Voices: voice.Voices})
}

// callFlowSaved reports the outcome of a change; false means it already
// answered.
func (s *Server) callFlowSaved(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, voice.ErrInvalidFlow):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, voice.ErrUnknownNumber):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, voice.ErrNothingPending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, voice.ErrObserver):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		s.fail(w, "saving a call flow", err)
	}
	return false
}

func (s *Server) listCallFlows(w http.ResponseWriter, r *http.Request, p auth.Person) {
	s.callFlowsFor(w, r, p.WorkspaceID)
}

func (s *Server) setCallFlow(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var f voice.Flow
	if !decode(w, r, &f) {
		return
	}
	v, err := voice.Save(r.Context(), s.DB, p.WorkspaceID, r.PathValue("number"), p.ID, f, true)
	if s.callFlowSaved(w, err) {
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *Server) activateCallFlow(w http.ResponseWriter, r *http.Request, p auth.Person) {
	v, err := voice.Activate(r.Context(), s.DB, p.WorkspaceID, r.PathValue("number"), p.ID)
	if s.callFlowSaved(w, err) {
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *Server) discardCallFlow(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if !s.callFlowSaved(w, voice.Discard(r.Context(), s.DB, p.WorkspaceID, r.PathValue("number"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
}

func (s *Server) agentCallFlows(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	s.callFlowsFor(w, r, a.WorkspaceID)
}

func (s *Server) agentSetCallFlow(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	var f voice.Flow
	if !decode(w, r, &f) {
		return
	}
	v, err := voice.SaveAs(r.Context(), s.DB, a.WorkspaceID, r.PathValue("number"), a.ID, a.Autonomy, f)
	if s.callFlowSaved(w, err) {
		writeJSON(w, http.StatusOK, v)
	}
}
