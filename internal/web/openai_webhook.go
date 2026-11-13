package web

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/voice"
)

// openAIRealtime answers a call OpenAI announces: one VisiComms dialled for
// the AI is accepted with the workspace's instructions and followed until it
// ends; any other is turned away.
func (s *Server) openAIRealtime(w http.ResponseWriter, r *http.Request) {
	if s.OpenAI == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "body too large"})
		return
	}
	if err := s.OpenAI.Verify(body, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), s.now()); err != nil {
		writeJSON(w, 401, map[string]string{"error": err.Error()})
		return
	}
	in, ok, err := voice.ParseWebhook(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid event"})
		return
	}
	if !ok || in.OpenAICallID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// OpenAI can announce one SIP call through both voice APIs; the one
	// VisiComms is not set to use leaves the decision to the other. Logging
	// both shows which APIs the project announces calls through.
	answers := (in.API == voice.APILive) == s.OpenAI.UsesLive()
	s.Logger.Info("OpenAI announced a call", "api", in.API, "openai_call", in.OpenAICallID, "call", in.CallID, "answering", answers)
	if !answers {
		w.WriteHeader(http.StatusOK)
		return
	}
	call, err := voice.ClaimAI(r.Context(), s.DB, in.CallID, in.OpenAICallID)
	switch {
	case errors.Is(err, voice.ErrAlreadyClaimed):
		w.WriteHeader(http.StatusOK)
		return
	case errors.Is(err, voice.ErrUnknownCall):
		s.Logger.Warn("OpenAI call matches no call waiting for the AI", "openai_call", in.OpenAICallID)
		if err := s.OpenAI.Reject(r.Context(), in.OpenAICallID); err != nil {
			s.Logger.Warn("rejecting an OpenAI call", "err", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		s.fail(w, "claiming an AI call", err)
		return
	}
	conv := &voice.Conversation{Call: call, OpenAI: s.OpenAI, DB: s.DB, Logger: s.Logger, Fallback: s.aiFallback(call.Call)}
	// The call outlives this request.
	ctx := context.WithoutCancel(r.Context())
	if err := s.OpenAI.Accept(r.Context(), in.OpenAICallID, call.SessionFor(s.OpenAI)); err != nil {
		s.Logger.Error("accepting an AI call", "call", call.ID, "err", err)
		if err := conv.Fallback(ctx); err != nil {
			s.Logger.Error("sending an AI call to voicemail", "call", call.ID, "err", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	s.Logger.Info("AI answered a call", "call", call.ID, "openai_call", in.OpenAICallID)
	go conv.Run(ctx)
	w.WriteHeader(http.StatusOK)
}

// aiFallback redirects the call at SignalWire to take a message.
func (s *Server) aiFallback(c voice.Call) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if s.SignalWire == nil {
			return errors.New("SignalWire is not configured")
		}
		sw, _, err := s.SignalWire.ForProject(ctx, c.Project)
		if err != nil {
			return err
		}
		return sw.RedirectCall(ctx, c.CallSID, voice.PathMessage)
	}
}
