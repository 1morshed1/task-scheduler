package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/1morshed1/task-scheduler/internal/models"
	"github.com/1morshed1/task-scheduler/internal/store"
)

// createTaskRequest is the JSON body for POST /api/v1/tasks. Only `type` is
// required; everything else has a sensible default so the simplest possible
// request is just {"type":"noop"}.
type createTaskRequest struct {
	Type string `json:"type"`
	// Payload is arbitrary JSON handed to the handler untouched. Defaults to {}.
	Payload json.RawMessage `json:"payload"`
	// Priority: higher runs first among due tasks. Defaults to 0.
	Priority int `json:"priority"`
	// ScheduledAt: when the task first becomes claimable (RFC3339). Omit to run
	// as soon as possible. This is how you schedule delayed/future work.
	ScheduledAt *time.Time `json:"scheduled_at"`
	// MaxRetries: number of retries before dead-lettering. Defaults to 3.
	MaxRetries *int `json:"max_retries"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	// Cap the body so a giant payload can't exhaust memory.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	var req createTaskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Type == "" {
		s.writeError(w, http.StatusBadRequest, "field 'type' is required")
		return
	}

	// Apply defaults.
	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	scheduledAt := time.Now()
	if req.ScheduledAt != nil {
		scheduledAt = *req.ScheduledAt
	}
	maxRetries := 3
	if req.MaxRetries != nil {
		if *req.MaxRetries < 0 {
			s.writeError(w, http.StatusBadRequest, "max_retries must be >= 0")
			return
		}
		maxRetries = *req.MaxRetries
	}

	task, err := s.store.CreateTask(r.Context(), store.CreateTaskParams{
		Type:        req.Type,
		Payload:     payload,
		Priority:    req.Priority,
		ScheduledAt: scheduledAt,
		MaxRetries:  maxRetries,
	})
	if err != nil {
		s.log.Error("create task", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not create task")
		return
	}
	// 201 Created is the correct status for a successful POST that creates a
	// resource; we return the full task so the client learns its generated id.
	s.writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	// Optional ?status= and ?limit= query params.
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}

	tasks, err := s.store.ListTasks(r.Context(), status, limit)
	if err != nil {
		s.log.Error("list tasks", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not list tasks")
		return
	}
	// Return an empty array (not null) when there are no tasks, so clients can
	// always iterate the response without a nil check.
	if tasks == nil {
		tasks = []*models.Task{}
	}
	s.writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id") // Go 1.22 path wildcard extraction
	task, err := s.store.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		s.log.Error("get task", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not get task")
		return
	}
	s.writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.Snapshot(r.Context())
	if err != nil {
		s.log.Error("stats", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not gather stats")
		return
	}
	s.writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleListDLQ(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	items, err := s.store.ListDeadLetters(r.Context(), limit)
	if err != nil {
		s.log.Error("list dlq", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not list dead letters")
		return
	}
	if items == nil {
		items = []*models.DeadLetter{}
	}
	s.writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleRetryDLQ(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	newID, err := s.store.RetryDeadLetter(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "dead-letter entry not found")
		return
	}
	if err != nil {
		s.log.Error("retry dlq", "err", err)
		s.writeError(w, http.StatusInternalServerError, "could not retry dead letter")
		return
	}
	// Report the id of the freshly-enqueued task so the caller can follow it.
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status":      "requeued",
		"new_task_id": newID,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Liveness: if this code is running at all, the process is alive.
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Readiness: can we actually serve requests? That means the DB is reachable.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Pool().Ping(ctx); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
