package web

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/recordings"
	"github.com/VisiOffice/VisiComms/internal/voice"
	"github.com/google/uuid"
)

// callRecording streams a call's recording to a signed-in person in the
// call's workspace. The provider URL and credentials never reach the browser.
func (s *Server) callRecording(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil || s.SignalWire == nil {
		http.NotFound(w, r)
		return
	}
	kept, err := recordings.Find(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), s.now())
	if errors.Is(err, recordings.ErrExpired) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "Recordings are deleted a year after the call."})
		return
	}
	if err != nil && !db.NotFound(err) {
		s.fail(w, "finding a recording", err)
		return
	}
	if err == nil && kept.Archived {
		if s.Recordings == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Recordings storage is not configured."})
			return
		}
		obj, err := s.Recordings.Get(r.Context(), kept.ObjectKey, r.Header.Get("Range"))
		if err != nil {
			s.Logger.Error("reading a stored recording", "call", kept.CallID, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The recording could not be read from storage."})
			return
		}
		defer obj.Body.Close()
		w.Header().Set("Content-Type", obj.ContentType)
		if obj.ContentLength > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(obj.ContentLength, 10))
		}
		if obj.ContentRange != "" {
			w.Header().Set("Content-Range", obj.ContentRange)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(obj.Status())
		io.Copy(w, obj.Body)
		return
	}
	// Not copied yet: play SignalWire's.
	raw, err := voice.RecordingURL(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if db.NotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "finding a recording", err)
		return
	}
	sw, err := s.SignalWire.ForWorkspace(r.Context(), p.WorkspaceID)
	if err != nil || sw == nil {
		http.NotFound(w, r)
		return
	}
	res, err := sw.OpenRecording(r.Context(), raw, r.Header.Get("Range"))
	if err != nil {
		s.Logger.Error("fetching a recording", "call", r.PathValue("id"), "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "SignalWire would not return the recording."})
		return
	}
	defer res.Body.Close()
	kind := res.Header.Get("Content-Type")
	if !strings.HasPrefix(kind, "audio/") {
		kind = "audio/mpeg"
	}
	w.Header().Set("Content-Type", kind)
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}
