// Package api implements the REST API: submit tasks, query their status, inspect
// stats, and retry from the dead-letter queue. It uses only net/http — no web
// framework — with the Go 1.22+ ServeMux that supports method+path patterns like
// "POST /api/v1/tasks/{id}". See docs/ADR-003 for why no framework.
//
// The handlers are thin: they parse the request, call the store, and encode the
// response. All the real logic lives in the store. That keeps HTTP concerns and
// data logic cleanly separated (and each easy to test).
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/1morshed1/task-scheduler/internal/store"
)

// Server holds the dependencies the handlers need.
type Server struct {
	store *store.Store
	log   *slog.Logger
}

// New builds an API server.
func New(s *store.Store, log *slog.Logger) *Server {
	return &Server{store: s, log: log}
}

// Routes registers the API endpoints on the given mux. We take a mux rather than
// creating one so cmd/server can mount the dashboard on the same mux/port.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)
	mux.HandleFunc("GET /api/v1/dlq", s.handleListDLQ)
	mux.HandleFunc("POST /api/v1/dlq/{id}/retry", s.handleRetryDLQ)

	// Prometheus scrape endpoint (see metrics.go).
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Liveness/readiness probes for Kubernetes. healthz = "process is up";
	// readyz = "process can serve, i.e. the DB is reachable".
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
}

// --- JSON helpers -----------------------------------------------------------

// writeJSON encodes v as JSON with the given status code. Centralizing this
// keeps every handler consistent (content-type, encoding errors logged once).
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("encode response", "err", err)
	}
}

// errorResponse is the uniform error envelope so clients can rely on a shape.
type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, errorResponse{Error: msg})
}

// --- middleware -------------------------------------------------------------

// LoggingMiddleware wraps a handler to log method, path, status, and duration.
// We write our own because we chose no framework; it's ~20 lines and you see
// exactly what a framework's logger would do for you.
func LoggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// statusRecorder captures the status code the handler writes, which the
		// bare ResponseWriter does not expose after the fact.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer if it supports flushing. This lets the
// SSE handler (which needs to flush each event) work even when wrapped by the
// logging middleware.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
