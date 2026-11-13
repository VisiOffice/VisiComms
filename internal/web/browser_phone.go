package web

import (
	"encoding/json"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

// The console's phone: a browser registers at SignalWire with a token, says
// it is online, and shows who is calling when it rings.
func (s *Server) browserPhoneRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/phone/token", s.signedIn(s.phoneToken))
	mux.Handle("PUT /api/phone/presence", s.signedIn(s.phonePresence))
	mux.Handle("GET /api/phone/calls", s.signedIn(s.phoneCalls))
}

func (s *Server) phoneToken(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.SignalWire == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": "This server has no SignalWire."})
		return
	}
	sw, err := s.SignalWire.ForWorkspace(r.Context(), p.WorkspaceID)
	if err != nil || sw == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": "This workspace has no SignalWire project yet."})
		return
	}
	resource := voice.Resource(p.ID)
	token, err := sw.BrowserToken(r.Context(), resource)
	if err != nil {
		s.Logger.Error("minting a browser phone token", "person", p.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": "SignalWire would not issue a phone token: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "project": sw.Project, "token": token, "resource": resource})
}

func (s *Server) phonePresence(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var body struct {
		Online bool `json:"online"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {\"online\": true|false}"})
		return
	}
	if err := voice.SetPresence(r.Context(), s.DB, p.WorkspaceID, p.ID, body.Online); err != nil {
		s.fail(w, "saving phone presence", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"online": body.Online})
}

func (s *Server) phoneCalls(w http.ResponseWriter, r *http.Request, p auth.Person) {
	calls, err := voice.Live(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "listing live calls", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": orEmpty(calls)})
}
