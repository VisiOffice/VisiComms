package web

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/slack"
)

// Slack: chats copied into a channel, and replies typed there sent to the
// visitor. Slack calls /slack/events/{id}
// with each message; the connector id picks the signing secret.

func (s *Server) slackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /slack/events/{connector}", s.slackEvents)
}

func (s *Server) slackConsoleRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/slack", s.signedIn(s.slackSettings))
	mux.Handle("PUT /api/slack", s.signedIn(s.saveSlack))
	mux.Handle("DELETE /api/slack", s.signedIn(s.deleteSlack))
	mux.Handle("PUT /api/slack/members/{id}", s.signedIn(s.linkSlackMember))
}

func (s *Server) slackEvents(w http.ResponseWriter, r *http.Request) {
	if s.SlackEvents == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	challenge, err := s.SlackEvents.Handle(r.Context(), r.PathValue("connector"),
		r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body)
	switch {
	case errors.Is(err, slack.ErrNotConnected):
		http.NotFound(w, r)
	case errors.Is(err, slack.ErrSignature):
		http.Error(w, "bad signature", http.StatusUnauthorized)
	case err != nil:
		// Acknowledged anyway: a retry would hit the same error, and the
		// event is recorded once already.
		s.Logger.Error("slack: handling an event", "err", err)
		w.WriteHeader(http.StatusOK)
	case challenge != "":
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// slackReply sends a reply typed in Slack as the linked person, with the
// console's rules.
func (s *Server) slackReply(ctx context.Context, workspaceID, principalID, threadID, text string) error {
	r := review.Reviewer{ID: principalID, WorkspaceID: workspaceID, Enqueue: s.Enqueue}
	_, err := review.Reply(ctx, s.DB, r, threadID, review.EmailPayload{Body: text, Via: "slack"})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, review.ErrConflict):
		return errors.New("someone else has taken over this chat in VisiComms")
	case errors.Is(err, review.ErrChatLength), errors.Is(err, review.ErrEmptyReply), errors.Is(err, review.ErrOptedOut):
		return err
	case errors.Is(err, review.ErrNotFound), errors.Is(err, review.ErrNoAddress):
		return errors.New("this chat no longer exists in VisiComms")
	}
	s.Logger.Error("slack: sending a reply", "thread", threadID, "err", err)
	return errors.New("something went wrong; reply from VisiComms instead")
}

type slackView struct {
	Connected bool           `json:"connected"`
	Available bool           `json:"available"`
	Settings  slack.Settings `json:"settings"`
	Health    string         `json:"health"`
	LastError string         `json:"last_error"`
	// EventsURL goes in the Slack app's Event Subscriptions.
	EventsURL string         `json:"events_url"`
	Members   []slack.Member `json:"members"`
}

func (s *Server) slackSettings(w http.ResponseWriter, r *http.Request, p auth.Person) {
	v := slackView{Available: s.Slack != nil && s.Slack.Keys != nil}
	members, err := slack.Members(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "loading members", err)
		return
	}
	v.Members = orEmpty(members)
	if v.Available {
		conn, err := s.Slack.Load(r.Context(), s.DB, p.WorkspaceID)
		switch {
		case err == nil:
			v.Connected, v.Settings, v.Health, v.LastError = true, conn.Settings, conn.Health, conn.LastError
			v.EventsURL = s.PublicURL + "/slack/events/" + conn.ID
		case !errors.Is(err, slack.ErrNotConnected):
			s.fail(w, "loading Slack", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) saveSlack(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.Slack == nil || s.Slack.Keys == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": slack.ErrNoKeys.Error()})
		return
	}
	var in slack.Input
	if !decode(w, r, &in) {
		return
	}
	_, err := s.Slack.Save(r.Context(), p.WorkspaceID, in)
	var field *slack.FieldError
	if errors.As(err, &field) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": field.Msg})
		return
	}
	if err != nil {
		s.fail(w, "saving Slack", err)
		return
	}
	s.slackSettings(w, r, p)
}

func (s *Server) deleteSlack(w http.ResponseWriter, r *http.Request, p auth.Person) {
	if s.Slack != nil {
		if err := s.Slack.Delete(r.Context(), p.WorkspaceID); err != nil {
			s.fail(w, "disconnecting Slack", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkSlackMember(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in struct {
		SlackUserID string `json:"slack_user_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	err := slack.Link(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), in.SlackUserID)
	var field *slack.FieldError
	switch {
	case errors.As(err, &field):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": field.Msg})
	case errors.Is(err, slack.ErrNotConnected):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		s.fail(w, "linking Slack member", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
