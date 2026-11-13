package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

func (s *Server) voiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+voice.PathIncoming, s.voiceIncoming)
	mux.HandleFunc("POST "+voice.PathMenu, s.voiceStep(s.voiceMenu))
	mux.HandleFunc("POST "+voice.PathDial, s.voiceStep(s.voiceDial))
	mux.HandleFunc("POST "+voice.PathRecorded, s.voiceStep(s.voiceRecorded))
	mux.HandleFunc("POST "+voice.PathTranscript, s.voiceStep(s.voiceTranscript))
	mux.HandleFunc("POST "+voice.PathStatus, s.voiceStep(s.voiceStatus))
	mux.HandleFunc("POST "+voice.PathAIDial, s.voiceStep(s.voiceAIDial))
	mux.HandleFunc("POST "+voice.PathMessage, s.voiceStep(s.voiceMessage))
	mux.HandleFunc("POST "+voice.PathAIRecording, s.voiceStep(s.voiceAIRecording))
	mux.HandleFunc("POST /webhooks/openai/realtime", s.openAIRealtime)
}

func cxml(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(body))
}

func (s *Server) voiceIncoming(w http.ResponseWriter, r *http.Request) {
	form, sw, ok := s.verifiedSMSForm(w, r)
	if !ok {
		return
	}
	c, err := voice.Begin(r.Context(), s.DB, voice.Incoming{Project: form.Get("AccountSid"), CallSID: form.Get("CallSid"), From: form.Get("From"), To: form.Get("To")})
	if errors.Is(err, voice.ErrUnknownNumber) {
		cxml(w, voice.Hangup())
		return
	}
	if err != nil {
		// Nothing was recorded, but the caller can still leave a message.
		s.Logger.Error("starting a call", "err", err)
		cxml(w, voice.TakeMessage(sw.PublicURL, voice.DefaultFlow()))
		return
	}
	s.Logger.Info("call received", "call", c.ID, "thread", c.ThreadID, "flow", c.Flow.Describe())
	s.ringable(r, sw, &c)
	cxml(w, voice.Answer(sw.PublicURL, c))
}

type voiceHandler func(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call)

// voiceStep verifies a later callback and loads its call, scoped to the
// project that signed it.
func (s *Server) voiceStep(next voiceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, sw, ok := s.verifiedSMSForm(w, r)
		if !ok {
			return
		}
		c, err := voice.Find(r.Context(), s.DB, form.Get("AccountSid"), form.Get("CallSid"))
		if errors.Is(err, voice.ErrUnknownCall) {
			cxml(w, voice.Hangup())
			return
		}
		if err != nil {
			s.Logger.Error("loading a call", "err", err)
			cxml(w, voice.TakeMessage(sw.PublicURL, voice.DefaultFlow()))
			return
		}
		s.ringable(r, sw, &c)
		next(w, r, sw, c)
	}
}

// ringable fills in the browser phones a ring-in-the-app step can ring. If
// they cannot be loaded, the step falls through as if nobody were online.
func (s *Server) ringable(r *http.Request, sw *sending.SignalWire, c *voice.Call) {
	c.Space = sw.Space
	online, err := voice.Online(r.Context(), s.DB, c.WorkspaceID)
	if err != nil {
		s.Logger.Error("loading browser phones", "call", c.ID, "err", err)
	}
	c.Browsers = online
}

func (s *Server) voiceMenu(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	cxml(w, voice.Choose(sw.PublicURL, c, r.PostForm.Get("Digits")))
}

// voiceDial follows a ring or forward: answered ends the call, anything else
// moves on to the step's fallback, and past the last one takes a message.
func (s *Server) voiceDial(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	if strings.ToLower(r.PostForm.Get("DialCallStatus")) == "completed" {
		if err := voice.Answered(r.Context(), s.DB, c, r.PostForm.Get("To")); err != nil {
			s.Logger.Error("recording an answered call", "call", c.ID, "err", err)
		}
		cxml(w, voice.Hangup())
		return
	}
	depth, err := strconv.Atoi(r.URL.Query().Get("depth"))
	if err != nil {
		cxml(w, voice.TakeMessage(sw.PublicURL, c.Flow))
		return
	}
	cxml(w, voice.Act(sw.PublicURL, c, r.URL.Query().Get("route"), depth+1))
}

// voiceAIDial follows the leg handed to the AI. Once the AI has spoken, its
// conversation records the call; if it never did (OpenAI did not answer, or
// answered and stayed silent), the caller leaves a message.
func (s *Server) voiceAIDial(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	if c.AIAnswered {
		cxml(w, voice.Hangup())
		return
	}
	cxml(w, voice.TakeMessage(sw.PublicURL, c.Flow))
}

// voiceMessage takes a message from a call the AI could not start on.
func (s *Server) voiceMessage(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	cxml(w, voice.TakeMessage(sw.PublicURL, c.Flow))
}

func (s *Server) voiceRecorded(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	seconds, _ := strconv.Atoi(r.PostForm.Get("RecordingDuration"))
	recording := r.PostForm.Get("RecordingUrl")
	var err error
	if recording == "" || seconds < 1 {
		err = voice.Missed(r.Context(), s.DB, c)
	} else {
		err = voice.LeftMessage(r.Context(), s.DB, c, recording, seconds)
	}
	if err != nil {
		s.Logger.Error("recording a voicemail", "call", c.ID, "err", err)
	}
	cxml(w, voice.Hangup())
}

// voiceAIRecording keeps the recording of a call the AI answered. It is a
// status callback, so it answers with nothing to do.
func (s *Server) voiceAIRecording(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	seconds, _ := strconv.Atoi(r.PostForm.Get("RecordingDuration"))
	recording := r.PostForm.Get("RecordingUrl")
	status := r.PostForm.Get("RecordingStatus")
	if recording != "" && (status == "" || strings.EqualFold(status, "completed")) {
		if err := voice.Recorded(r.Context(), s.DB, c, recording, r.PostForm.Get("RecordingSid"), seconds); err != nil {
			s.Logger.Error("keeping a call recording", "call", c.ID, "err", err)
		}
	}
	cxml(w, voice.Empty())
}

func (s *Server) voiceTranscript(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	text := strings.TrimSpace(r.PostForm.Get("TranscriptionText"))
	if strings.EqualFold(r.PostForm.Get("TranscriptionStatus"), "completed") && text != "" {
		if err := voice.Transcript(r.Context(), s.DB, c, text); err != nil {
			s.Logger.Error("recording a transcript", "call", c.ID, "err", err)
		}
	}
	cxml(w, voice.Empty())
}

// voiceStatus closes a call that ended without an outcome: the caller hung up
// during a ring or a greeting.
func (s *Server) voiceStatus(w http.ResponseWriter, r *http.Request, sw *sending.SignalWire, c voice.Call) {
	var err error
	switch status := strings.ToLower(r.PostForm.Get("CallStatus")); status {
	case "completed", "busy", "no-answer", "canceled":
		if c.AIAnswered {
			// The AI's conversation records how the call ended.
			break
		}
		err = voice.Missed(r.Context(), s.DB, c)
	case "failed":
		err = voice.Failed(r.Context(), s.DB, c, status)
	}
	if err != nil {
		s.Logger.Error("closing a call", "call", c.ID, "err", err)
	}
	cxml(w, voice.Empty())
}
