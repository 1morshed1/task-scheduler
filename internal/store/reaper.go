package store

import (
	"context"
	"fmt"
	"time"
)

// ReapResult reports what a single reaper pass did, purely for logging/metrics.
type ReapResult struct {
	TasksRequeued int64 // orphaned running tasks put back to pending
	TasksDead     int64 // orphaned tasks that had no retries left -> dead+DLQ
	WorkersDead   int64 // workers marked dead for missing heartbeats
}

// RequeueOrphanedTasks finds tasks stuck in 'running' whose lock is older than
// lockTTL — meaning the worker that held them has almost certainly died without
// releasing them — and returns them to the queue.
//
// This is the counterpart to the claim: claiming takes a lock, and if the holder
// vanishes, THIS restores the task so another worker can pick it up. Without it,
// a single worker crash would strand its in-flight tasks in 'running' forever.
//
// The decision embedded here (see ADR-004): a reaped task has its retry_count
// incremented, exactly as if the attempt had failed. Rationale — a task that
// reliably crashes its worker (OOM, panic that kills the process) would
// otherwise loop forever: run, crash, reap, run, crash... Counting the attempt
// means such a "poison task" eventually exhausts retries and lands in the DLQ
// for a human to inspect. We accept slightly pessimistic accounting to guarantee
// forward progress.
//
// Two sub-cases, handled in one statement each:
//   - retries remain  -> back to 'pending', retry_count++, runnable now
//   - retries exhausted -> straight to 'dead' AND snapshot into the DLQ
func (s *Store) RequeueOrphanedTasks(ctx context.Context, lockTTL time.Duration) (requeued, dead int64, err error) {
	staleBefore := time.Now().Add(-lockTTL)

	// Case 1: orphaned tasks that still have retries left -> requeue.
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET status = 'pending',
		    retry_count = retry_count + 1,
		    last_error = 'worker lock expired (presumed crashed)',
		    locked_by = NULL,
		    locked_at = NULL,
		    scheduled_at = now(),
		    updated_at = now()
		WHERE status = 'running'
		  AND locked_at < $1
		  AND retry_count < max_retries`, staleBefore)
	if err != nil {
		return 0, 0, fmt.Errorf("requeue orphaned: %w", err)
	}
	requeued = tag.RowsAffected()

	// Case 2: orphaned tasks with no retries left -> dead + DLQ snapshot.
	// A single INSERT ... SELECT copies all qualifying tasks into the DLQ, then
	// an UPDATE marks them dead. Both run in the same implicit statement scope;
	// we wrap them in a transaction so the copy and the mark can't diverge.
	tag2, err := s.deadLetterOrphans(ctx, staleBefore)
	if err != nil {
		return requeued, 0, err
	}
	dead = tag2
	return requeued, dead, nil
}

func (s *Store) deadLetterOrphans(ctx context.Context, staleBefore time.Time) (int64, error) {
	// Insert snapshots for orphaned, retries-exhausted tasks that aren't already
	// in the DLQ (the NOT EXISTS guards against a double-insert if a prior pass
	// was interrupted between the INSERT and the UPDATE).
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO dead_letter_queue (task_id, task_type, payload, final_error, retry_count)
		SELECT id, type, payload,
		       'worker lock expired and retries exhausted', retry_count
		FROM tasks t
		WHERE t.status = 'running'
		  AND t.locked_at < $1
		  AND t.retry_count >= t.max_retries
		  AND NOT EXISTS (
		      SELECT 1 FROM dead_letter_queue d WHERE d.task_id = t.id
		  )`, staleBefore); err != nil {
		return 0, fmt.Errorf("dlq orphans insert: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET status = 'dead',
		    last_error = 'worker lock expired and retries exhausted',
		    locked_by = NULL,
		    locked_at = NULL,
		    updated_at = now()
		WHERE status = 'running'
		  AND locked_at < $1
		  AND retry_count >= max_retries`, staleBefore)
	if err != nil {
		return 0, fmt.Errorf("mark orphans dead: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkDeadWorkers flips workers whose last_heartbeat is older than lockTTL to
// 'dead' status. This is cosmetic/observability only — the task recovery above
// keys off the task's own locked_at, not the worker row — but it keeps the
// dashboard honest about which workers are actually alive. Returns the count.
func (s *Store) MarkDeadWorkers(ctx context.Context, lockTTL time.Duration) (int64, error) {
	staleBefore := time.Now().Add(-lockTTL)
	tag, err := s.pool.Exec(ctx, `
		UPDATE workers
		SET status = 'dead'
		WHERE status <> 'dead'
		  AND last_heartbeat < $1`, staleBefore)
	if err != nil {
		return 0, fmt.Errorf("mark dead workers: %w", err)
	}
	return tag.RowsAffected(), nil
}
