## Project 4 (Bonus): Distributed Task Scheduler with Dead-Letter Recovery

### Concept & Why It Matters

This is a project I am adding because it fills a gap the other three do not cover: building a core infrastructure component from scratch. You are building a distributed cron/task scheduler — something like a simplified Temporal, Celery, or Sidekiq. It accepts task definitions via an API, schedules them across a pool of workers, handles retries with exponential backoff, routes permanently failed tasks to a dead-letter queue, and provides a dashboard showing task states and worker health.

This is valuable because it forces you to solve hard distributed systems problems: leader election, exactly-once execution, worker heartbeats, and graceful shutdown.

### Architecture Diagram

```
  Clients                                                                    
   │                                                                         
   │  POST /api/v1/tasks                                                     
   │  { type, payload, schedule, max_retries }                               
   ▼                                                                         
┌────────────────────────────────────────────────────────────────────────────┐
│                        KUBERNETES CLUSTER                                  │
│                                                                            │
│  ┌────────────────┐     ┌───────────────────────────────────────────┐       │
│  │  Go Scheduler  │     │  PostgreSQL                               │      │
│  │  API           │────▶│                                           │      │
│  │                │     │  tasks (id, type, payload, status,        │      │
│  │  - Accept      │     │         scheduled_at, locked_by,          │      │
│  │    tasks       │     │         retry_count, max_retries,         │      │
│  │  - View        │     │         last_error, created_at)           │      │
│  │    status      │     │                                           │      │
│  └────────────────┘     │  workers (id, last_heartbeat, status,     │      │
│                         │          tasks_completed, tasks_failed)   │      │
│                         │                                           │      │
│                         │  dead_letter_queue (id, task_id,          │      │
│                         │         final_error, exhausted_at)        │      │
│                         └───────────────────────────────────────────┘      │
│                                                                            │
│  ┌──────────────────────────────────────────────────────┐                   │
│  │  Worker Pool (Go)                                    │                  │
│  │                                                      │                  │
│  │  Worker 1 ──┐                                        │                  │
│  │  Worker 2 ──┤── Poll DB for tasks with               │                  │
│  │  Worker 3 ──┤   SELECT ... FOR UPDATE SKIP LOCKED    │                  │
│  │  Worker N ──┘                                        │                  │
│  │                                                      │                  │
│  │  Each worker:                                        │                  │
│  │  - Sends heartbeat every 10s                         │                  │
│  │  - Executes task handler                             │                  │
│  │  - On success: mark completed                        │                  │
│  │  - On failure: increment retry_count                 │                  │
│  │  - On max retries: move to dead_letter_queue         │                  │
│  │  - Graceful shutdown on SIGTERM                      │                  │
│  └──────────────────────────────────────────────────────┘                   │
│                                                                            │
│  ┌──────────────────────────────────────────────────────┐                   │
│  │  Kafka (optional, for high-throughput variant)       │                  │
│  │  topic: task-events  (task lifecycle events)         │                  │
│  │  topic: dlq-events   (dead-letter notifications)    │                  │
│  └──────────────────────────────────────────────────────┘                   │
│                                                                            │
│  ┌──────────────────────────────────────────────────────┐                   │
│  │  Dashboard (Go + WebSocket)                          │                  │
│  │  - Live task counts by status                        │                  │
│  │  - Worker health (last heartbeat, tasks/min)         │                  │
│  │  - Dead-letter queue browser with retry button       │                  │
│  │  - Task execution time percentiles                   │                  │
│  └──────────────────────────────────────────────────────┘                   │
│                                                                            │
│  Observability: Prometheus + Grafana + Jaeger            │                 │
└────────────────────────────────────────────────────────────────────────────┘
```

### Key Implementation Details

**Task Locking with `SELECT ... FOR UPDATE SKIP LOCKED`:**

This is the core mechanism. Multiple workers compete for tasks. PostgreSQL's `SKIP LOCKED` ensures that if Worker 1 has already locked a row, Worker 2 skips it instead of blocking:

```sql
-- Each worker runs this in a loop
BEGIN;
SELECT id, type, payload
FROM tasks
WHERE status = 'pending'
  AND scheduled_at <= NOW()
  AND (locked_by IS NULL OR last_heartbeat < NOW() - INTERVAL '30 seconds')
ORDER BY scheduled_at ASC
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- If a row is returned, lock it
UPDATE tasks SET status = 'running', locked_by = $worker_id, last_heartbeat = NOW()
WHERE id = $task_id;
COMMIT;
```

**Exponential Backoff with Jitter:**

```go
func nextRetryDelay(retryCount int) time.Duration {
    base := time.Second * time.Duration(math.Pow(2, float64(retryCount)))
    jitter := time.Duration(rand.Int63n(int64(base / 2)))
    return base + jitter
    // Retry 1: 2-3s, Retry 2: 4-6s, Retry 3: 8-12s, etc.
}
```

**Dead-Letter Queue Recovery:**

When a task exhausts its retries, it is moved to the `dead_letter_queue` table with the final error message. The dashboard API exposes a `POST /api/v1/dlq/:id/retry` endpoint that moves the task back to `pending` with a reset retry count. This lets operators inspect failures and manually retry after fixing the underlying issue.

**Worker Heartbeats:**

Each worker sends a heartbeat (updates `last_heartbeat` in the `workers` table) every 10 seconds. If a worker crashes without releasing its locks, the scheduler detects stale heartbeats (older than 30 seconds) and reassigns those tasks to other workers. This prevents tasks from being stuck forever.

**Graceful Shutdown:**

When a worker receives SIGTERM (which Kubernetes sends before killing a pod), it stops accepting new tasks, waits for the currently running task to complete (up to a 30-second deadline), and then exits. This prevents tasks from being interrupted mid-execution during deployments.

---
---
