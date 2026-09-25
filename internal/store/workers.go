package store

import (
	"context"
	"fmt"

	"github.com/1morshed1/task-scheduler/internal/models"
)

// RegisterWorker inserts a row for a newly-started worker and returns its
// generated id. The worker holds onto this id for the rest of its life and
// stamps it into locked_by when it claims tasks.
func (s *Store) RegisterWorker(ctx context.Context, hostname string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO workers (hostname) VALUES ($1) RETURNING id`,
		hostname).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("register worker: %w", err)
	}
	return id, nil
}

// Heartbeat updates a worker's last_heartbeat to now(). Called on a timer. This
// is the liveness signal the reaper checks: a worker that stops heartbeating is
// eventually declared dead and its in-flight tasks are re-queued. Keeping this a
// single cheap UPDATE means heartbeating adds negligible load even with many
// workers.
func (s *Store) Heartbeat(ctx context.Context, workerID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE workers SET last_heartbeat = now() WHERE id = $1`, workerID)
	return err
}

// SetWorkerStatus flips a worker between active/draining/dead. The worker sets
// itself 'draining' on SIGTERM; the reaper sets crashed workers 'dead'.
func (s *Store) SetWorkerStatus(ctx context.Context, workerID string, status models.WorkerStatus) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE workers SET status = $2 WHERE id = $1`, workerID, string(status))
	return err
}

// DeregisterWorker removes a worker row on clean shutdown. We delete rather than
// mark dead because a gracefully-departed worker leaves no orphaned tasks and
// needn't clutter the health view. (Crashed workers are marked 'dead' by the
// reaper instead, so their failure stays visible.)
func (s *Store) DeregisterWorker(ctx context.Context, workerID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM workers WHERE id = $1`, workerID)
	return err
}

// ListWorkers returns all known workers for the dashboard, most recently
// heartbeated first.
func (s *Store) ListWorkers(ctx context.Context) ([]*models.Worker, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, hostname, status, last_heartbeat, tasks_completed, tasks_failed, started_at
		FROM workers
		ORDER BY last_heartbeat DESC`)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()

	var out []*models.Worker
	for rows.Next() {
		var w models.Worker
		if err := rows.Scan(&w.ID, &w.Hostname, &w.Status, &w.LastHeartbeat,
			&w.TasksCompleted, &w.TasksFailed, &w.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}
