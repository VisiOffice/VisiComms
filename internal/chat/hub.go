package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Hub wakes open widget streams when their thread changes. Writers call
// Notify inside their transaction; Postgres delivers the notification on
// commit to the one connection the hub listens on, and the hub tells every
// stream on that thread to read again. A missed wake-up costs at most one
// heartbeat interval, because streams also re-read on their heartbeat.
type Hub struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	mu     sync.Mutex
	subs   map[string]map[chan struct{}]struct{}
	typing map[string]*typingState
	// changes names each thread that changed, for one other reader (the
	// Slack mirror). Full means a reader is behind; it sweeps instead.
	changes chan string
}

// typingState is who was last seen typing on a thread. It lives in memory
// only: it is worth nothing a few seconds later, and one server runs the API.
type typingState struct {
	visitor, team time.Time
	teamName      string
}

// TypingFor is how long a typing signal counts; clients repeat it sooner.
const TypingFor = 6 * time.Second

func NewHub(pool *pgxpool.Pool, logger *slog.Logger) *Hub {
	return &Hub{pool: pool, logger: logger, subs: map[string]map[chan struct{}]struct{}{}, typing: map[string]*typingState{},
		changes: make(chan string, 256)}
}

// Changes receives the id of each chat thread that changed.
func (h *Hub) Changes() <-chan string { return h.changes }

// VisitorTyping records that the visitor on thread is typing.
func (h *Hub) VisitorTyping(threadID string) {
	h.mu.Lock()
	h.typingOf(threadID).visitor = time.Now()
	h.mu.Unlock()
}

// TeamTyping records that someone on the team, shown to the visitor by first
// name, is typing on thread, and wakes the visitor's stream.
func (h *Hub) TeamTyping(threadID, name string) {
	h.mu.Lock()
	t := h.typingOf(threadID)
	t.team, t.teamName = time.Now(), name
	h.mu.Unlock()
	h.wake(threadID)
}

// IsVisitorTyping says whether the visitor typed in the last few seconds.
func (h *Hub) IsVisitorTyping(threadID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.typing[threadID]
	return t != nil && time.Since(t.visitor) < TypingFor
}

// TeamTypingSince is the team's latest typing signal on thread, if any.
func (h *Hub) TeamTypingSince(threadID string) (string, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.typing[threadID]; t != nil {
		return t.teamName, t.team
	}
	return "", time.Time{}
}

// typingOf needs h.mu held.
func (h *Hub) typingOf(threadID string) *typingState {
	if len(h.typing) > 1000 {
		for id, t := range h.typing {
			if time.Since(t.visitor) > time.Minute && time.Since(t.team) > time.Minute {
				delete(h.typing, id)
			}
		}
	}
	t := h.typing[threadID]
	if t == nil {
		t = &typingState{}
		h.typing[threadID] = t
	}
	return t
}

// Run listens until ctx ends, reconnecting after a failure.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.listen(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("chat hub: listening failed, retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	pooled, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// LISTEN state belongs to this connection; never hand it back to the pool.
	conn := pooled.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `LISTEN chat`); err != nil {
		return err
	}
	// Anything that happened while not listening: wake everyone once.
	h.wakeAll()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.wake(n.Payload)
		select {
		case h.changes <- n.Payload:
		default:
		}
	}
}

// Subscribe returns a channel that receives when thread changes.
func (h *Hub) Subscribe(threadID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[threadID] == nil {
		h.subs[threadID] = map[chan struct{}]struct{}{}
	}
	h.subs[threadID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[threadID], ch)
		if len(h.subs[threadID]) == 0 {
			delete(h.subs, threadID)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wake(threadID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[threadID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.subs {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}
