// Package dashboard serves the live web UI. It has two parts:
//
//  1. A tiny static page (embedded into the binary) that renders the numbers.
//  2. A Server-Sent Events (SSE) stream that pushes a fresh stats snapshot to
//     every connected browser every couple of seconds.
//
// We use SSE rather than WebSockets on purpose — the data only ever flows
// server→browser, which is exactly what SSE is for, and it needs no third-party
// library. See docs/09 for the full rationale (ADR-005).
package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/1morshed1/task-scheduler/internal/store"
)

// Hub polls the store on a timer and fan-outs each stats snapshot to all
// connected SSE clients. One goroutine does the querying; clients just receive.
// This is the classic pub/sub "hub" pattern, kept to ~40 lines.
type Hub struct {
	store    *store.Store
	log      *slog.Logger
	interval time.Duration

	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
	last        []byte // most recent snapshot, sent immediately to new clients
}

// NewHub builds a dashboard hub that refreshes every `interval`.
func NewHub(s *store.Store, log *slog.Logger, interval time.Duration) *Hub {
	return &Hub{
		store:       s,
		log:         log,
		interval:    interval,
		subscribers: make(map[chan []byte]struct{}),
	}
}

// Run is the broadcaster loop: every tick it queries a snapshot and pushes it to
// every subscriber. Runs until ctx is cancelled. Start it once at server boot.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	h.publish(ctx) // publish once immediately so a fresh page isn't blank
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.publish(ctx)
		}
	}
}

func (h *Hub) publish(ctx context.Context) {
	snap, err := h.store.Snapshot(ctx)
	if err != nil {
		h.log.Warn("dashboard: snapshot failed", "err", err)
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		h.log.Warn("dashboard: marshal snapshot failed", "err", err)
		return
	}

	h.mu.Lock()
	h.last = data
	for ch := range h.subscribers {
		// Non-blocking send: if a slow client's buffer is full, we drop this
		// frame for them rather than stalling the whole broadcast. The next tick
		// will carry fresh data anyway, so a dropped frame is harmless.
		select {
		case ch <- data:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *Hub) subscribe() chan []byte {
	ch := make(chan []byte, 4) // small buffer to tolerate brief write stalls
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	last := h.last
	h.mu.Unlock()
	// Prime the new subscriber with the latest snapshot so its page fills in
	// instantly instead of waiting for the next tick.
	if last != nil {
		ch <- last
	}
	return ch
}

func (h *Hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
}

// Routes mounts the dashboard: the SSE stream and the static page.
func (h *Hub) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/events", h.handleSSE)
	mux.HandleFunc("GET /", h.handleIndex)
}

// handleSSE streams stats to one browser as Server-Sent Events. The wire format
// is dead simple: each message is the line "data: <json>\n\n". The browser's
// EventSource API parses that and fires an event; it also auto-reconnects if the
// stream drops, which we get entirely for free.
func (h *Hub) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.subscribe()
	defer h.unsubscribe(ch)

	// Stream until the client disconnects (r.Context() is cancelled when they do).
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			// Write the SSE frame and flush so the browser sees it immediately.
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleIndex serves the embedded dashboard page.
func (h *Hub) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Only the exact root path serves the page; anything else is a 404 (this
	// pattern otherwise catches every unmatched path).
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
