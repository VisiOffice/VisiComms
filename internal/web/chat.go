package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
)

// The widget API is public: any site's visitors call it, cross-origin and
// without cookies. A visitor proves who they are with the token they were
// given on their first message, in the Authorization header; nothing here
// accepts a thread, contact or workspace id from the browser.

func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /chat/widget.js", s.chatScript)
	mux.HandleFunc("OPTIONS /chat/w/{widget}/{rest...}", s.chatPreflight)
	mux.HandleFunc("GET /chat/w/{widget}/config", s.chatConfig)
	mux.HandleFunc("POST /chat/w/{widget}/messages", s.chatSend)
	mux.HandleFunc("GET /chat/w/{widget}/messages", s.chatHistory)
	mux.HandleFunc("GET /chat/w/{widget}/stream", s.chatStream)
	mux.HandleFunc("POST /chat/w/{widget}/typing", s.chatVisitorTyping)
	mux.HandleFunc("POST /chat/w/{widget}/popups/{popup}/shown", s.chatPopupShown)
}

func (s *Server) chatConsoleRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/chat/widgets", s.signedIn(s.listChatWidgets))
	mux.Handle("POST /api/chat/widgets", s.signedIn(s.createChatWidget))
	mux.Handle("PUT /api/chat/widgets/{id}", s.signedIn(s.updateChatWidget))
	mux.Handle("GET /api/chat/widgets/{id}/popups", s.signedIn(s.listPopups))
	mux.Handle("POST /api/chat/widgets/{id}/popups", s.signedIn(s.createPopup))
	mux.Handle("PUT /api/chat/popups/{id}", s.signedIn(s.updatePopup))
	mux.Handle("DELETE /api/chat/popups/{id}", s.signedIn(s.deletePopup))
	mux.Handle("POST /api/threads/{id}/typing", s.signedIn(s.teamTyping))
}

// chatWidget loads the widget a request names and applies its CORS rules. It
// answers the request itself, and returns false, when the widget is unknown
// or the page's site is not allowed.
func (s *Server) chatWidget(w http.ResponseWriter, r *http.Request) (chat.Widget, bool) {
	h := w.Header()
	h.Set("Vary", "Origin")
	h.Set("Cache-Control", "no-store")
	widget, err := chat.Load(r.Context(), s.DB, r.PathValue("widget"))
	if errors.Is(err, chat.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such chat"})
		return widget, false
	}
	if err != nil {
		s.fail(w, "loading chat widget", err)
		return widget, false
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		if !widget.OriginAllowed(origin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this site isn't allowed to use this chat"})
			return widget, false
		}
		// No credentials: the token travels in a header, never a cookie.
		h.Set("Access-Control-Allow-Origin", origin)
	} else if len(widget.AllowedOrigins) > 0 && r.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this site isn't allowed to use this chat"})
		return widget, false
	}
	return widget, true
}

func (s *Server) chatPreflight(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.chatWidget(w, r); !ok {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(v, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// clientIP is the address kamal-proxy saw. It appends the peer to
// X-Forwarded-For, so the last entry is the one a client cannot forge.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) chatConfig(w http.ResponseWriter, r *http.Request) {
	widget, ok := s.chatWidget(w, r)
	if !ok {
		return
	}
	c, err := chat.PublicConfig(r.Context(), s.DB, widget, s.now())
	if err != nil {
		s.fail(w, "chat config", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	widget, ok := s.chatWidget(w, r)
	if !ok {
		return
	}
	token := bearer(r)
	ip := clientIP(r)
	// Starting a chat is free to an attacker, so new conversations are
	// limited per address, and messages per address and per visitor.
	if !s.chatLimits().allow("msg:"+ip, 30, time.Minute) ||
		(token == "" && !s.chatLimits().allow("new:"+ip, 10, time.Hour)) ||
		(token != "" && !s.chatLimits().allow("tok:"+token, 15, time.Minute)) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many messages. Wait a minute and try again."})
		return
	}
	var in chat.Inbound
	if !decodeLimited(w, r, &in, 32<<10) {
		return
	}
	res, err := chat.Receive(r.Context(), s.DB, widget, token, in)
	var field *chat.FieldError
	switch {
	case errors.As(err, &field):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": field.Message, "field": field.Field})
		return
	case errors.Is(err, chat.ErrEmpty), errors.Is(err, chat.ErrTooLong):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, chat.ErrNotFound):
		// The token is unknown here: the widget starts a new conversation.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "conversation not found"})
		return
	case err != nil:
		s.fail(w, "receiving chat message", err)
		return
	}
	if res.New {
		s.Logger.Info("chat started", "widget", widget.PublicID, "thread", res.Visitor.ThreadID)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) chatVisitor(w http.ResponseWriter, r *http.Request) (chat.Visitor, bool) {
	widget, ok := s.chatWidget(w, r)
	if !ok {
		return chat.Visitor{}, false
	}
	v, err := chat.Authenticate(r.Context(), s.DB, widget, bearer(r))
	if errors.Is(err, chat.ErrNotFound) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "conversation not found"})
		return v, false
	}
	if err != nil {
		s.fail(w, "chat visitor", err)
		return v, false
	}
	return v, true
}

func (s *Server) chatHistory(w http.ResponseWriter, r *http.Request) {
	v, ok := s.chatVisitor(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	msgs, err := chat.Messages(r.Context(), s.DB, v, after)
	if err != nil {
		s.fail(w, "chat history", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// chatStream sends new messages as server-sent events. The widget reads it
// with fetch, because EventSource cannot send the Authorization header. A
// stream lasts a few minutes and the widget reconnects from its last id.
func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	v, ok := s.chatVisitor(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	ip := clientIP(r)
	if !s.chatLimits().hold("stream:"+ip, 20) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many open chats"})
		return
	}
	defer s.chatLimits().release("stream:" + ip)

	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil && q > after {
		after = q
	}
	var wake <-chan struct{}
	if s.Chat != nil {
		ch, cancel := s.Chat.Subscribe(v.ThreadID)
		defer cancel()
		wake = ch
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// Without the hub (tests, or a failed listener) the heartbeat is the poll.
	heartbeat := 20 * time.Second
	if s.Chat == nil {
		heartbeat = 3 * time.Second
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	var typingSent time.Time
	end := time.NewTimer(5 * time.Minute)
	defer end.Stop()
	ctx := r.Context()
	for {
		msgs, err := chat.Messages(ctx, s.DB, v, after)
		if err != nil {
			if ctx.Err() == nil {
				s.Logger.Error("chat stream", "err", err)
			}
			return
		}
		for _, m := range msgs {
			data, _ := json.Marshal(m)
			fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", m.ID, data)
			after = m.ID
		}
		if len(msgs) > 0 {
			flusher.Flush()
		}
		if s.Chat != nil {
			if name, at := s.Chat.TeamTypingSince(v.ThreadID); at.After(typingSent) && time.Since(at) < chat.TypingFor {
				data, _ := json.Marshal(map[string]string{"name": name})
				fmt.Fprintf(w, "event: typing\ndata: %s\n\n", data)
				flusher.Flush()
				typingSent = at
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-end.C:
			return
		case <-wake:
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

func decodeLimited(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if !isJSON(r) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	return true
}

func (s *Server) chatScript(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Access-Control-Allow-Origin", "*")
	w.Write(chat.Script)
}

// --- console ---

func (s *Server) listChatWidgets(w http.ResponseWriter, r *http.Request, p auth.Person) {
	widgets, err := chat.List(r.Context(), s.DB, p.WorkspaceID)
	if err != nil {
		s.fail(w, "listing chat widgets", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"widgets": widgets, "script_url": s.chatScriptURL()})
}

func (s *Server) createChatWidget(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	widget, err := chat.Create(r.Context(), s.DB, p.WorkspaceID, in.Name)
	s.chatWidgetSaved(w, widget, err)
}

func (s *Server) updateChatWidget(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in chat.Settings
	if !decode(w, r, &in) {
		return
	}
	widget, err := chat.Update(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), in)
	s.chatWidgetSaved(w, widget, err)
}

func (s *Server) chatWidgetSaved(w http.ResponseWriter, widget chat.Widget, err error) {
	var field *chat.FieldError
	switch {
	case errors.As(err, &field):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": field.Message, "field": field.Field})
	case errors.Is(err, chat.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		s.fail(w, "saving chat widget", err)
	default:
		writeJSON(w, http.StatusOK, widget)
	}
}

// chatScriptURL is where sites load the widget from: the API's own public
// address, so visitors' requests never pass through the console's origin.
func (s *Server) chatScriptURL() string {
	base := strings.TrimRight(s.PublicURL, "/")
	if base == "" {
		base = "http://localhost:8080"
	}
	return base + "/chat/widget.js"
}

// --- limits ---

// chatLimiter counts requests in fixed windows and open streams, in memory.
// One server runs the API, so this is the whole picture.
type chatLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
	open    map[string]int
}

type window struct {
	start time.Time
	count int
}

func (s *Server) chatLimits() *chatLimiter {
	s.chatLimiterMu.Lock()
	defer s.chatLimiterMu.Unlock()
	if s.chatLimiter == nil {
		s.chatLimiter = &chatLimiter{windows: map[string]*window{}, open: map[string]int{}}
	}
	return s.chatLimiter
}

func (l *chatLimiter) allow(key string, max int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.windows) > 50000 {
		for k, w := range l.windows {
			if now.Sub(w.start) > time.Hour {
				delete(l.windows, k)
			}
		}
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= per {
		w = &window{start: now}
		l.windows[key] = w
	}
	w.count++
	return w.count <= max
}

func (l *chatLimiter) hold(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[key] >= max {
		return false
	}
	l.open[key]++
	return true
}

func (l *chatLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[key]--; l.open[key] <= 0 {
		delete(l.open, key)
	}
}

// chatVisitorTyping tells the team the visitor is typing. Only a visitor
// with a conversation can: before the first message nothing is known.
func (s *Server) chatVisitorTyping(w http.ResponseWriter, r *http.Request) {
	v, ok := s.chatVisitor(w, r)
	if !ok {
		return
	}
	if s.chatLimits().allow("typing:"+bearer(r), 40, time.Minute) && s.Chat != nil {
		s.Chat.VisitorTyping(v.ThreadID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatPopupShown is asked before the widget shows a pop-up. It counts the
// showing and allows each pop-up once per visitor per visit, whatever the
// browser asks: before a first message the visitor is only an address and
// the widget's per-visit id, so both are limited.
func (s *Server) chatPopupShown(w http.ResponseWriter, r *http.Request) {
	widget, ok := s.chatWidget(w, r)
	if !ok {
		return
	}
	popup := r.PathValue("popup")
	visit := r.URL.Query().Get("visit")
	if len(visit) < 16 || len(visit) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing visit"})
		return
	}
	ip := clientIP(r)
	if !s.chatLimits().allow("popup:"+popup+":"+visit, 1, 12*time.Hour) ||
		!s.chatLimits().allow("popup-ip:"+popup+":"+ip, 20, time.Hour) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "already shown"})
		return
	}
	err := chat.PopupShown(r.Context(), s.DB, widget, popup)
	if errors.Is(err, chat.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such pop-up"})
		return
	}
	if err != nil {
		s.fail(w, "counting a pop-up", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) teamTyping(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var threadID string
	err := s.DB.QueryRow(r.Context(), `SELECT id FROM threads WHERE id::text = $1 AND workspace_id = $2 AND channel = 'chat'`,
		r.PathValue("id"), p.WorkspaceID).Scan(&threadID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if s.Chat != nil {
		s.Chat.TeamTyping(threadID, strings.Fields(p.Name + " ")[0])
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listPopups(w http.ResponseWriter, r *http.Request, p auth.Person) {
	popups, err := chat.Popups(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if errors.Is(err, chat.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		s.fail(w, "listing pop-ups", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"popups": popups})
}

func (s *Server) createPopup(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in chat.PopupSettings
	if !decode(w, r, &in) {
		return
	}
	popup, err := chat.CreatePopup(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), in)
	s.popupSaved(w, popup, err)
}

func (s *Server) updatePopup(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var in chat.PopupSettings
	if !decode(w, r, &in) {
		return
	}
	popup, err := chat.UpdatePopup(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"), in)
	s.popupSaved(w, popup, err)
}

func (s *Server) deletePopup(w http.ResponseWriter, r *http.Request, p auth.Person) {
	err := chat.DeletePopup(r.Context(), s.DB, p.WorkspaceID, r.PathValue("id"))
	if errors.Is(err, chat.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		s.fail(w, "deleting a pop-up", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) popupSaved(w http.ResponseWriter, popup chat.Popup, err error) {
	var field *chat.FieldError
	switch {
	case errors.As(err, &field):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": field.Message, "field": field.Field})
	case errors.Is(err, chat.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		s.fail(w, "saving a pop-up", err)
	default:
		writeJSON(w, http.StatusOK, popup)
	}
}
