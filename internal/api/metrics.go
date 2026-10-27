package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/1morshed1/task-scheduler/internal/models"
)

// handleMetrics exposes a Prometheus-compatible /metrics endpoint. We emit the
// text exposition format BY HAND rather than pulling in the Prometheus client
// library — for a handful of metrics it's a few lines, adds no dependency, and
// shows you exactly what the format is (a big part of the learning goal). A real
// high-cardinality service would use the client library; here, transparency wins.
//
// The format is line-based:
//
//	# HELP <name> <description>
//	# TYPE <name> <gauge|counter>
//	<name>{label="value"} <number>
//
// Point Prometheus at this endpoint and these become time series you can graph
// in Grafana (task backlog over time, DLQ depth, per-worker throughput, ...).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.Snapshot(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not gather metrics")
		return
	}

	var b strings.Builder

	// Task counts by status (gauge — a point-in-time backlog measurement).
	b.WriteString("# HELP tasks_total Number of tasks in each status.\n")
	b.WriteString("# TYPE tasks_total gauge\n")
	// Always emit all five statuses (even at 0) so graphs don't have gaps.
	for _, st := range []models.TaskStatus{
		models.StatusPending, models.StatusRunning, models.StatusCompleted,
		models.StatusFailed, models.StatusDead,
	} {
		fmt.Fprintf(&b, "tasks_total{status=%q} %d\n", st, snap.TaskCounts[string(st)])
	}

	// Dead-letter queue depth (gauge — a key alerting signal: page someone if
	// this climbs).
	b.WriteString("# HELP dlq_depth Number of tasks currently in the dead-letter queue.\n")
	b.WriteString("# TYPE dlq_depth gauge\n")
	fmt.Fprintf(&b, "dlq_depth %d\n", snap.DLQDepth)

	// Live worker count (gauge).
	b.WriteString("# HELP workers_total Number of registered workers.\n")
	b.WriteString("# TYPE workers_total gauge\n")
	fmt.Fprintf(&b, "workers_total %d\n", len(snap.Workers))

	// Per-worker lifetime counters. Sorting keeps the output stable/diffable.
	workers := append([]*models.Worker(nil), snap.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
	b.WriteString("# HELP worker_tasks_completed_total Tasks completed by a worker.\n")
	b.WriteString("# TYPE worker_tasks_completed_total counter\n")
	for _, wk := range workers {
		fmt.Fprintf(&b, "worker_tasks_completed_total{worker=%q,host=%q} %d\n",
			wk.ID, wk.Hostname, wk.TasksCompleted)
	}
	b.WriteString("# HELP worker_tasks_failed_total Failed attempts recorded by a worker.\n")
	b.WriteString("# TYPE worker_tasks_failed_total counter\n")
	for _, wk := range workers {
		fmt.Fprintf(&b, "worker_tasks_failed_total{worker=%q,host=%q} %d\n",
			wk.ID, wk.Hostname, wk.TasksFailed)
	}

	// Prometheus expects this specific content type.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
