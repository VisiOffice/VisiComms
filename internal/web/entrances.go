package web

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// Entrances: door intercoms whose calls the AI answers as visitors at the
// gate. A person sets them up in the console.
func (s *Server) entranceRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/entrances", s.signedIn(s.listEntrances))
	mux.Handle("POST /api/entrances", s.signedIn(s.createEntrance))
	mux.Handle("PUT /api/entrances/{id}", s.signedIn(s.updateEntrance))
	mux.Handle("DELETE /api/entrances/{id}", s.signedIn(s.deleteEntrance))
	mux.Handle("POST /api/entrances/{id}/sip", s.signedIn(s.createEntranceSIP))
	mux.Handle("DELETE /api/entrances/{id}/sip", s.signedIn(s.deleteEntranceSIP))
}

type entrancesResponse struct {
	Entrances []voice.Entrance `json:"entrances"`
	// SIPAvailable says whether the workspace's SignalWire project can make
	// SIP accounts for intercoms.
	SIPAvailable bool            `json:"sip_available"`
	Available    map[string]bool `json:"available_modes"`
	Voices       []string        `json:"voices"`
	Default      voice.Flow      `json:"default_flow"`
}

func (s *Server) listEntrances(w http.ResponseWriter, r *http.Request, p auth.Person) {
	entrances, err := voice.Entrances(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "listing entrances", err)
		return
	}
	sw, _ := s.SignalWire.ForWorkspace(r.Context(), p.WorkspaceID)
	writeJSON(w, http.StatusOK, entrancesResponse{
		Entrances:    entrances,
		SIPAvailable: sw != nil && sw.SIPServer() != "",
		Available:    voice.Available,
		Voices:       voice.Voices,
		Default:      voice.DefaultEntranceFlow(),
	})
}

// entranceSaved reports the outcome of a change; false means it already
// answered.
func (s *Server) entranceSaved(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, voice.ErrInvalidFlow):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, voice.ErrUnknownEntrance):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, voice.ErrCallerIDTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		s.fail(w, "saving an entrance", err)
	}
	return false
}

func (s *Server) createEntrance(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var e voice.Entrance
	if !decode(w, r, &e) {
		return
	}
	if e.Flow.Answer.Mode == "" {
		e.Flow = voice.DefaultEntranceFlow()
	}
	out, err := voice.CreateEntrance(r.Context(), s.DB, p.WorkspaceID, e)
	if s.entranceSaved(w, err) {
		writeJSON(w, http.StatusCreated, out)
	}
}

func (s *Server) updateEntrance(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var e voice.Entrance
	if !decode(w, r, &e) {
		return
	}
	out, err := voice.UpdateEntrance(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), e)
	if s.entranceSaved(w, err) {
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) deleteEntrance(w http.ResponseWriter, r *http.Request, p auth.Person) {
	endpoint, project, err := voice.DeleteEntrance(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if !s.entranceSaved(w, err) {
		return
	}
	s.removeSIPEndpoint(r.Context(), project, endpoint)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// createEntranceSIP makes the entrance a SIP account, replacing any it had,
// and returns it with its password, which is never shown again.
func (s *Server) createEntranceSIP(w http.ResponseWriter, r *http.Request, p auth.Person) {
	ctx := r.Context()
	e, err := voice.GetEntrance(ctx, s.DB, p.WorkspaceID, r.PathValue("id"))
	if !s.entranceSaved(w, err) {
		return
	}
	sw, err := s.SignalWire.ForWorkspace(ctx, p.WorkspaceID)
	if err != nil || sw.SIPServer() == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Connect SignalWire for this workspace before making a SIP account."})
		return
	}
	username := "gate-" + randomString(12, "abcdefghijklmnopqrstuvwxyz0123456789")
	password := randomString(24, "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789")
	endpoint, err := sw.CreateSIPEndpoint(ctx, username, password, e.Name, sw.PublicURL+voice.PathIncoming, sw.PublicURL+voice.PathStatus)
	if err != nil {
		s.fail(w, "making a SIP account at SignalWire", err)
		return
	}
	oldEndpoint, oldProject, err := voice.SetEntranceSIP(ctx, s.DB, p.WorkspaceID, e.ID, username, endpoint.ID, sw.Project, endpoint.Server)
	if err != nil {
		s.removeSIPEndpoint(ctx, sw.Project, endpoint.ID)
		s.entranceSaved(w, err)
		return
	}
	s.removeSIPEndpoint(ctx, oldProject, oldEndpoint)
	s.Logger.Info("entrance SIP account made", "entrance", e.ID, "username", username, "person", p.ID)
	writeJSON(w, http.StatusOK, voice.SIPAccount{
		Server: endpoint.Server, Port: voice.SIPPort, Transport: voice.SIPTransport,
		Username: username, Password: password, Recipient: voice.SIPRecipient,
	})
}

func (s *Server) deleteEntranceSIP(w http.ResponseWriter, r *http.Request, p auth.Person) {
	oldEndpoint, oldProject, err := voice.SetEntranceSIP(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), "", "", "", "")
	if !s.entranceSaved(w, err) {
		return
	}
	s.removeSIPEndpoint(r.Context(), oldProject, oldEndpoint)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// removeSIPEndpoint deletes a SIP account at SignalWire. A failure is logged:
// the account no longer names any entrance, so a call from it is refused.
func (s *Server) removeSIPEndpoint(ctx context.Context, project, endpoint string) {
	if endpoint == "" {
		return
	}
	sw, _, err := s.SignalWire.ForProject(ctx, project)
	if err == nil {
		err = sw.DeleteSIPEndpoint(ctx, endpoint)
	}
	if err != nil {
		s.Logger.Error("removing a SIP account at SignalWire", "endpoint", endpoint, "err", err)
	}
}

// randomString is n characters drawn uniformly from alphabet.
func randomString(n int, alphabet string) string {
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		_, _ = rand.Read(buf)
		// Reject bytes past the last whole multiple, so no character is
		// likelier than another.
		if int(buf[0]) >= 256-256%len(alphabet) {
			continue
		}
		out[i] = alphabet[int(buf[0])%len(alphabet)]
		i++
	}
	return string(out)
}
