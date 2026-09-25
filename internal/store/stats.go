package store

import (
	"context"
	"fmt"
	"time"

	"github.com/1morshed1/task-scheduler/internal/models"
)

// TaskStatusCounts returns the number of tasks in each status. Powers the
// dashboard's headline numbers and the /stats endpoint.
//
// We GROUP BY status in a single query rather than issuing one COUNT per status.
// One round-trip, and Postgres scans the table once. Statuses with zero rows
// simply won't appear in the map; the caller fills in zeros for display.
func (s *Store) TaskStatusCounts(ctx context.Context) (models.StatusCounts, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT status, count(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("status counts: %w", err)
	}
	defer rows.Close()

	counts := models.StatusCounts{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

// DLQDepth returns how many tasks are currently sitting in the dead-letter
// queue — a key at-a-glance health number.
func (s *Store) DLQDepth(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dead_letter_queue`).Scan(&n)
	return n, err
}

// Stats bundles everything the dashboard needs in one shot, so the SSE
// broadcaster and the /stats endpoint can render a full snapshot from a single
// call.
type Stats struct {
	TaskCounts  models.StatusCounts `json:"task_counts"`
	DLQDepth    int64               `json:"dlq_depth"`
	Workers     []*models.Worker    `json:"workers"`
	GeneratedAt time.Time           `json:"generated_at"`
}

// Snapshot gathers a complete stats picture. Three quick queries; fine to run on
// the dashboard's refresh tick.
func (s *Store) Snapshot(ctx context.Context) (*Stats, error) {
	counts, err := s.TaskStatusCounts(ctx)
	if err != nil {
		return nil, err
	}
	depth, err := s.DLQDepth(ctx)
	if err != nil {
		return nil, err
	}
	workers, err := s.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	return &Stats{
		TaskCounts:  counts,
		DLQDepth:    depth,
		Workers:     workers,
		GeneratedAt: time.Now().UTC(),
	}, nil
}
