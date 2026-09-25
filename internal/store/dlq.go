package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/1morshed1/task-scheduler/internal/models"
	"github.com/jackc/pgx/v5"
)

// ListDeadLetters returns the dead-letter queue, most recent failures first.
func (s *Store) ListDeadLetters(ctx context.Context, limit int) ([]*models.DeadLetter, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, task_id, task_type, payload, final_error, retry_count, exhausted_at
		FROM dead_letter_queue
		ORDER BY exhausted_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list dlq: %w", err)
	}
	defer rows.Close()

	var out []*models.DeadLetter
	for rows.Next() {
		var d models.DeadLetter
		if err := rows.Scan(&d.ID, &d.TaskID, &d.TaskType, &d.Payload,
			&d.FinalError, &d.RetryCount, &d.ExhaustedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RetryDeadLetter is the operator's "try again" action. It takes a DLQ entry id,
// re-enqueues a FRESH task from the captured snapshot (retry_count reset to 0,
// runnable immediately), and removes the DLQ entry — all in one transaction so
// we can never both re-enqueue AND leave the stale DLQ row behind.
//
// We create a new task row rather than resurrecting the old 'dead' one. That
// keeps the historical record of the death intact (the old task stays 'dead')
// and gives the retry a clean lifecycle. Returns the new task's id.
func (s *Store) RetryDeadLetter(ctx context.Context, dlqID string) (string, error) {
	var newTaskID string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			taskType string
			payload  []byte
		)
		err := tx.QueryRow(ctx,
			`SELECT task_type, payload FROM dead_letter_queue WHERE id = $1`,
			dlqID).Scan(&taskType, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read dlq entry: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO tasks (type, payload, status, scheduled_at)
			VALUES ($1, $2, 'pending', now())
			RETURNING id`, taskType, payload).Scan(&newTaskID); err != nil {
			return fmt.Errorf("re-enqueue task: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`DELETE FROM dead_letter_queue WHERE id = $1`, dlqID); err != nil {
			return fmt.Errorf("remove dlq entry: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newTaskID, nil
}
