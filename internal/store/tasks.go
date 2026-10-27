package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1morshed1/task-scheduler/internal/models"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a lookup by id matches no row. Callers (the API)
// translate this into a 404. Defining a sentinel error keeps the store from
// needing to know about HTTP status codes.
var ErrNotFound = errors.New("not found")

// CreateTaskParams is the input for enqueueing a task. Kept separate from the
// stored models.Task because the caller only supplies a subset of fields; the
// database fills in id, timestamps, and status.
type CreateTaskParams struct {
	Type        string
	Payload     []byte // raw JSON; validated as JSONB by Postgres on insert
	Priority    int
	ScheduledAt time.Time // when the task first becomes claimable
	MaxRetries  int
}

// CreateTask inserts a new pending task and returns the full stored row.
func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (*models.Task, error) {
	const q = `
		INSERT INTO tasks (type, payload, priority, scheduled_at, max_retries)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + taskColumns
	row := s.pool.QueryRow(ctx, q, p.Type, p.Payload, p.Priority, p.ScheduledAt, p.MaxRetries)
	return scanTask(row)
}

// ClaimTask is THE core operation of the whole system. It atomically finds the
// next due task and marks it running by this worker — with a guarantee that no
// two workers ever claim the same task.
//
// How the SQL works, from the inside out:
//
//	SELECT id ... WHERE status='pending' AND scheduled_at<=now()
//	ORDER BY priority DESC, scheduled_at ASC
//	FOR UPDATE SKIP LOCKED
//	LIMIT 1
//
// FOR UPDATE takes a row lock on the chosen row for the duration of the
// statement. SKIP LOCKED is the magic word: if another worker's transaction
// already holds that row's lock, this query does not block waiting for it — it
// simply *skips* that row and considers the next candidate. So ten workers
// running this simultaneously each grab ten *different* tasks, with no blocking
// and no double-claiming.
//
// We wrap the SELECT in an UPDATE ... WHERE id = (subquery) ... RETURNING so the
// find-and-claim is a single atomic statement (see ADR-002). The moment the
// UPDATE commits, the row's status is 'running' and it will never match another
// worker's pending-filter again.
//
// Returns (nil, nil) when there is no work — a normal, expected condition the
// worker loop treats as "sleep then poll again", not an error.
func (s *Store) ClaimTask(ctx context.Context, workerID string) (*models.ClaimedTask, error) {
	const q = `
		UPDATE tasks
		SET status     = 'running',
		    locked_by  = $1,
		    locked_at  = now(),
		    started_at = now(),
		    updated_at = now()
		WHERE id = (
		    SELECT id
		    FROM tasks
		    WHERE status = 'pending'
		      AND scheduled_at <= now()
		    ORDER BY priority DESC, scheduled_at ASC
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		)
		RETURNING id, type, payload, retry_count, max_retries`

	var t models.ClaimedTask
	err := s.pool.QueryRow(ctx, q, workerID).
		Scan(&t.ID, &t.Type, &t.Payload, &t.RetryCount, &t.MaxRetries)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no work available right now
	}
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	return &t, nil
}

// CompleteTask marks a running task as completed and stores its result. It also
// bumps the worker's completed counter in the same transaction, so the two facts
// (task done, worker credited) can never drift apart.
func (s *Store) CompleteTask(ctx context.Context, taskID, workerID string, result []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks
			SET status = 'completed',
			    result = $2,
			    last_error = NULL,
			    locked_by = NULL,
			    locked_at = NULL,
			    completed_at = now(),
			    updated_at = now()
			WHERE id = $1`, taskID, result); err != nil {
			return fmt.Errorf("mark completed: %w", err)
		}
		_, err := tx.Exec(ctx,
			`UPDATE workers SET tasks_completed = tasks_completed + 1 WHERE id = $1`,
			workerID)
		return err
	})
}

// RetryTask records a failed attempt and reschedules the task to run again after
// `delay`. The worker calls this when a handler failed but retries remain. The
// backoff delay is computed by the caller (see internal/worker/backoff.go) and
// applied here as scheduled_at = now() + delay. Setting status back to 'pending'
// with a future scheduled_at is exactly how a retry re-enters the queue.
func (s *Store) RetryTask(ctx context.Context, taskID, workerID, errMsg string, delay time.Duration) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks
			SET status = 'pending',
			    retry_count = retry_count + 1,
			    last_error = $2,
			    scheduled_at = now() + $3::interval,
			    locked_by = NULL,
			    locked_at = NULL,
			    updated_at = now()
			WHERE id = $1`,
			taskID, errMsg, intervalString(delay)); err != nil {
			return fmt.Errorf("mark for retry: %w", err)
		}
		_, err := tx.Exec(ctx,
			`UPDATE workers SET tasks_failed = tasks_failed + 1 WHERE id = $1`,
			workerID)
		return err
	})
}

// DeadLetterTask handles the terminal-failure path: the task has exhausted its
// retries. In ONE transaction we (a) mark the task 'dead', (b) copy a snapshot
// into dead_letter_queue, and (c) credit the worker's failed counter. Doing all
// three atomically means we can never end up with a dead task that isn't in the
// DLQ, or a DLQ entry for a task still marked running.
func (s *Store) DeadLetterTask(ctx context.Context, taskID, workerID, finalError string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Mark dead and capture the columns we need for the snapshot.
		var (
			taskType   string
			payload    []byte
			retryCount int
		)
		err := tx.QueryRow(ctx, `
			UPDATE tasks
			SET status = 'dead',
			    last_error = $2,
			    locked_by = NULL,
			    locked_at = NULL,
			    updated_at = now()
			WHERE id = $1
			RETURNING type, payload, retry_count`,
			taskID, finalError).Scan(&taskType, &payload, &retryCount)
		if err != nil {
			return fmt.Errorf("mark dead: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO dead_letter_queue
			    (task_id, task_type, payload, final_error, retry_count)
			VALUES ($1, $2, $3, $4, $5)`,
			taskID, taskType, payload, finalError, retryCount); err != nil {
			return fmt.Errorf("insert dlq: %w", err)
		}

		_, err = tx.Exec(ctx,
			`UPDATE workers SET tasks_failed = tasks_failed + 1 WHERE id = $1`,
			workerID)
		return err
	})
}

// GetTask fetches a single task by id, returning ErrNotFound if absent.
func (s *Store) GetTask(ctx context.Context, id string) (*models.Task, error) {
	const q = `SELECT ` + taskColumns + ` FROM tasks WHERE id = $1`
	t, err := scanTask(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListTasks returns tasks, optionally filtered by status, newest first. `limit`
// caps the page size. A blank status means "all statuses".
func (s *Store) ListTasks(ctx context.Context, status string, limit int) ([]*models.Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100 // guardrail against unbounded result sets
	}

	// We build the query with an optional status filter. Using a nullable
	// parameter ($1 IS NULL OR status = $1) keeps it to one prepared statement
	// while supporting both "all" and "filtered".
	const q = `
		SELECT ` + taskColumns + `
		FROM tasks
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, status, limit)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var out []*models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- shared scan helpers ---

// taskColumns is the canonical column list, in the order scanTask expects. Kept
// in one place so INSERT ... RETURNING and SELECT stay in lockstep — add a
// column here and both paths update together.
const taskColumns = `
	id, type, payload, status, priority, scheduled_at,
	locked_by, locked_at, retry_count, max_retries,
	last_error, result, created_at, updated_at, started_at, completed_at`

// row is the small interface shared by pgx.Row (single) and pgx.Rows (iterated),
// so scanTask works for both QueryRow and Query loops.
type row interface {
	Scan(dest ...any) error
}

func scanTask(r row) (*models.Task, error) {
	var t models.Task
	err := r.Scan(
		&t.ID, &t.Type, &t.Payload, &t.Status, &t.Priority, &t.ScheduledAt,
		&t.LockedBy, &t.LockedAt, &t.RetryCount, &t.MaxRetries,
		&t.LastError, &t.Result, &t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// intervalString renders a Go duration as a Postgres interval literal like
// '12.500 seconds'. We pass it as text and cast with ::interval in SQL. Using
// seconds (a single unit) avoids any ambiguity in interval parsing.
func intervalString(d time.Duration) string {
	return fmt.Sprintf("%f seconds", d.Seconds())
}
