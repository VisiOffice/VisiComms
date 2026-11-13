// Package web is the HTTP API. Agents and humans use the same routes; every
// outbound action goes through the policy engine, never straight to a
// connector.
package web

import (
	"context"
	"encoding/json"
	"github.com/VisiOffice/VisiComms/internal/notify"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/oauth"
	"github.com/VisiOffice/VisiComms/internal/recordings"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/slack"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

type Server struct {
	DB     *pgxpool.Pool
	Logger *slog.Logger
	// InboundEmailSecret verifies the email worker's webhooks.
	InboundEmailSecret string
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time
	// Auth signs people in; the console API needs it.
	Auth *auth.Service
	// AppURL is the console's origin, for links such as invites.
	AppURL string
	// Sending is the platform's email provider; nil when not configured.
	Sending sending.Provider
	// SignalWire finds each workspace's project; nil when SMS and fax are off.
	SignalWire *sending.SignalWireProjects
	// Recordings is the bucket call recordings are kept in; nil leaves them
	// at SignalWire.
	Recordings recordings.Store
	// OpenAI answers calls in AI mode; nil when it is not configured.
	OpenAI              *voice.OpenAI
	Resend              *sending.Resend
	ResendWebhookSecret string
	InboundDomain       string
	// SNS verifies SES event notifications; SESEventsTopic is the only topic
	// accepted.
	SNS            *sending.SNSVerifier
	SESEventsTopic string
	// Enqueue hands real sends to the executor (see review.Reviewer).
	Enqueue func(ctx context.Context, tx pgx.Tx, actionID string) error
	// SecureCookies is set when the site is served over HTTPS (everywhere but
	// local development): the session cookie is HTTPS-only and responses carry
	// HSTS.
	SecureCookies bool
	// Notifier says which notification channels work; nil means none.
	Notifier *notify.Notifier
	// Chat wakes open chat widget streams; nil makes them poll.
	Chat *chat.Hub
	// Slack holds each workspace's Slack connection; nil turns Slack off.
	Slack *slack.Store
	// SlackEvents takes Slack's callbacks; its Reply is set by Routes.
	SlackEvents *slack.Events
	// OAuth lets hosted MCP clients connect; nil turns it off.
	OAuth *oauth.Server
	// SMSRegistry reads 10DLC registrations for the sms_status tool; nil
	// leaves them unread.
	SMSRegistry *sending.SignalWireParent
	// PublicURL is the API's public address, where chat widgets load from.
	PublicURL string
	// Mail is each workspace's own Lob account; nil turns letters off.
	Mail *sending.LobAccounts

	chatLimiterMu sync.Mutex
	chatLimiter   *chatLimiter
}

// hstsMaxAge is two years, the preload list's minimum. TLS ends at
// kamal-proxy (or Cloudflare, through the Worker), so the header is set here
// by configuration rather than by looking at the request.
const hstsMaxAge = "max-age=63072000; includeSubDomains"

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /up", s.up)
	mux.HandleFunc("GET /fax-media/{token}", s.faxMedia)
	mux.HandleFunc("POST /webhooks/signalwire/fax", s.faxInbound)
	mux.HandleFunc("POST /webhooks/signalwire/fax/received", s.faxReceived)
	mux.HandleFunc("POST /webhooks/signalwire/fax/status", s.faxStatus)
	mux.HandleFunc("POST /webhooks/lob/{connector}", s.lobWebhook)
	mux.HandleFunc("POST /webhooks/email", s.emailWebhook)
	mux.HandleFunc("POST /webhooks/ses", s.sesWebhook)
	mux.HandleFunc("POST /webhooks/resend", s.resendWebhook)
	mux.HandleFunc("POST /webhooks/signalwire/sms", s.smsWebhook)
	mux.HandleFunc("POST /webhooks/signalwire/sms/status", s.smsStatusWebhook)
	s.voiceRoutes(mux)
	s.chatRoutes(mux)
	if s.SlackEvents != nil && s.SlackEvents.Reply == nil {
		s.SlackEvents.Reply = s.slackReply
	}
	s.slackRoutes(mux)
	s.agentRoutes(mux)
	s.oauthRoutes(mux)
	if s.Auth != nil {
		s.apiRoutes(mux)
	}
	return s.securityHeaders(mux)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if s.SecureCookies {
			h.Set("Strict-Transport-Security", hstsMaxAge)
		}
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// up is the deploy health check: healthy only when the database answers.
func (s *Server) up(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		s.Logger.Error("health check: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
