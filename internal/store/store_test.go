package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/1morshed1/task-scheduler/internal/db"
	"github.com/1morshed1/task-scheduler/internal/models"
)

// testStore spins up a Store against the test database, runs migrations, and
// truncates the tables so each test starts clean. If no DB is reachable the
// caller's test is skipped rather than failed.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("no database available, skipping: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Clean slate. RESTART IDENTITY not needed (UUID keys); CASCADE is harmless.
	if _, err := pool.Exec(ctx, `TRUNCATE tasks, workers, dead_letter_queue`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return New(pool)
}

func mustCreate(t *testing.T, s *Store, typ string) *models.Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), CreateTaskParams{
		Type:        typ,
		Payload:     []byte(`{}`),
		ScheduledAt: time.Now(),
		MaxRetries:  3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// TestClaimIsExclusive is the most important test in the repo. It proves the
// core correctness guarantee: when many workers race to claim from a pool of N
// tasks, each task is claimed by exactly ONE worker — never zero, never two.
//
// We create 50 tasks and fire 20 goroutines that all hammer ClaimTask until the
// queue is empty, recording every id they claim. Then we assert there are no
// duplicate claims and that all 50 tasks were claimed. If SKIP LOCKED or our
// atomic-UPDATE claim were wrong, this test would flake with duplicates.
func TestClaimIsExclusive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const numTasks = 50
	const numWorkers = 20
	for i := 0; i < numTasks; i++ {
		mustCreate(t, s, "noop")
	}

	var mu sync.Mutex
	claimedBy := map[string]int{} // task id -> how many times claimed

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerNum int) {
			defer wg.Done()
			// Each goroutine registers a worker so locked_by is a real FK-ish id.
			wid, err := s.RegisterWorker(ctx, "test-host")
			if err != nil {
				t.Errorf("register: %v", err)
				return
			}
			for {
				task, err := s.ClaimTask(ctx, wid)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if task == nil {
					return // queue drained
				}
				mu.Lock()
				claimedBy[task.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(claimedBy) != numTasks {
		t.Fatalf("expected %d distinct tasks claimed, got %d", numTasks, len(claimedBy))
	}
	for id, n := range claimedBy {
		if n != 1 {
			t.Errorf("task %s claimed %d times (want exactly 1) — exclusivity broken!", id, n)
		}
	}
}

// TestRetryReschedules verifies the retry path: retry_count increments, the
// error is stored, and the task becomes pending again with a future
// scheduled_at (so it is not immediately re-claimable).
func TestRetryReschedules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task := mustCreate(t, s, "flaky")
	wid, _ := s.RegisterWorker(ctx, "h")

	if _, err := s.ClaimTask(ctx, wid); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.RetryTask(ctx, task.ID, wid, "boom", 30*time.Second); err != nil {
		t.Fatalf("retry: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != models.StatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.LastError == nil || *got.LastError != "boom" {
		t.Errorf("last_error = %v, want boom", got.LastError)
	}
	if !got.ScheduledAt.After(time.Now().Add(20 * time.Second)) {
		t.Errorf("scheduled_at %v should be ~30s in the future (backoff not applied)", got.ScheduledAt)
	}
	// And because scheduled_at is in the future, it must NOT be claimable now.
	claimed, _ := s.ClaimTask(ctx, wid)
	if claimed != nil {
		t.Errorf("task became claimable before its backoff elapsed")
	}
}

// TestDeadLetterAndRetry verifies the terminal-failure path and operator retry:
// DeadLetterTask marks dead + inserts a DLQ snapshot, and RetryDeadLetter
// re-enqueues a fresh pending task and clears the DLQ entry.
func TestDeadLetterAndRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task := mustCreate(t, s, "fail_always")
	wid, _ := s.RegisterWorker(ctx, "h")
	if _, err := s.ClaimTask(ctx, wid); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := s.DeadLetterTask(ctx, task.ID, wid, "final boom"); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}
	dead, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dead.Status != models.StatusDead {
		t.Errorf("status = %s, want dead", dead.Status)
	}

	dlq, err := s.ListDeadLetters(ctx, 10)
	if err != nil {
		t.Fatalf("list dlq: %v", err)
	}
	if len(dlq) != 1 {
		t.Fatalf("dlq depth = %d, want 1", len(dlq))
	}

	newID, err := s.RetryDeadLetter(ctx, dlq[0].ID)
	if err != nil {
		t.Fatalf("retry dlq: %v", err)
	}
	revived, err := s.GetTask(ctx, newID)
	if err != nil {
		t.Fatalf("get revived: %v", err)
	}
	if revived.Status != models.StatusPending || revived.RetryCount != 0 {
		t.Errorf("revived task = %s/retry %d, want pending/0", revived.Status, revived.RetryCount)
	}
	// DLQ should now be empty.
	after, _ := s.ListDeadLetters(ctx, 10)
	if len(after) != 0 {
		t.Errorf("dlq depth after retry = %d, want 0", len(after))
	}
}

// TestReaperRequeuesOrphans proves crash recovery: a task left 'running' with a
// stale lock is returned to the queue by the reaper (retries remaining) and its
// retry_count is bumped per ADR-004.
func TestReaperRequeuesOrphans(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task := mustCreate(t, s, "noop")
	wid, _ := s.RegisterWorker(ctx, "h")
	if _, err := s.ClaimTask(ctx, wid); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Simulate the worker having died a while ago by back-dating locked_at.
	if _, err := s.pool.Exec(ctx,
		`UPDATE tasks SET locked_at = now() - interval '5 minutes' WHERE id = $1`,
		task.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	requeued, dead, err := s.RequeueOrphanedTasks(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if requeued != 1 || dead != 0 {
		t.Fatalf("reap result requeued=%d dead=%d, want 1/0", requeued, dead)
	}
	got, _ := s.GetTask(ctx, task.ID)
	if got.Status != models.StatusPending {
		t.Errorf("orphan status = %s, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("orphan retry_count = %d, want 1 (ADR-004)", got.RetryCount)
	}
}
