package web

import (
	"errors"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/notify"
)

type notificationsResponse struct {
	People     []notify.Person   `json:"people"`
	Categories []notify.Category `json:"categories"`
	// Channels says which channels this server can deliver.
	Channels map[string]bool `json:"channels"`
}

func (s *Server) notificationRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/notifications", s.signedIn(s.myNotifications))
	mux.Handle("PUT /api/notifications", s.signedIn(s.setMyNotifications))
}

func (s *Server) agentNotificationRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/notifications", s.asAgent(s.agentNotifications))
	mux.Handle("PUT /api/v1/notifications/{person}", s.asAgent(s.agentSetNotifications))
}

func (s *Server) notifications(w http.ResponseWriter, r *http.Request, workspaceID, onlyPerson string) {
	people, err := notify.People(r.Context(), s.DB, workspaceID)
	if err != nil {
		s.fail(w, "loading notification settings", err)
		return
	}
	if onlyPerson != "" {
		mine := []notify.Person{}
		for _, p := range people {
			if p.ID == onlyPerson {
				mine = append(mine, p)
			}
		}
		people = mine
	}
	writeJSON(w, http.StatusOK, notificationsResponse{People: people, Categories: notify.Categories, Channels: s.Notifier.Channels()})
}

func (s *Server) notificationsSaved(w http.ResponseWriter, p notify.Person, err error) {
	switch {
	case errors.Is(err, notify.ErrInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, notify.ErrUnknownPerson):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		s.fail(w, "saving notification settings", err)
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Server) myNotifications(w http.ResponseWriter, r *http.Request, p auth.Person) {
	s.notifications(w, r, p.WorkspaceID, p.ID)
}

func (s *Server) setMyNotifications(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var ch notify.Change
	if !decode(w, r, &ch) {
		return
	}
	person, err := notify.Update(r.Context(), s.DB, p.WorkspaceID, p.ID, p.ID, ch)
	s.notificationsSaved(w, person, err)
}

func (s *Server) agentNotifications(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	s.notifications(w, r, a.WorkspaceID, "")
}

func (s *Server) agentSetNotifications(w http.ResponseWriter, r *http.Request, a agent.Agent) {
	if a.Autonomy == "observe" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": notify.ErrObserver.Error()})
		return
	}
	var ch notify.Change
	if !decode(w, r, &ch) {
		return
	}
	person, err := notify.Update(r.Context(), s.DB, a.WorkspaceID, r.PathValue("person"), a.ID, ch)
	s.notificationsSaved(w, person, err)
}
