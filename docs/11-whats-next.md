# 11 · What's Next — Scope, Observability, and Extensions

This doc does two things: it honestly states **what's in the box vs. what the
plan sketched as optional**, and it gives you a guided tour of the extensions
you'd build next — so you know how, even where we stopped short on purpose.

## What's fully built (the core)

Everything the spec lists as a *hard problem* is implemented, working, and
tested against a real Postgres:

- ✅ Task submission API + status queries
- ✅ Multi-worker execution with **exactly-once claiming** (`SKIP LOCKED`)
- ✅ **Retries** with exponential backoff + jitter
- ✅ **Dead-letter queue** + operator retry
- ✅ **Worker heartbeats** + **reaper** crash recovery
- ✅ **Graceful shutdown** on SIGTERM
- ✅ **Live dashboard** (SSE) with worker health and DLQ browser
- ✅ Docker + Kubernetes deployment
- ✅ **Prometheus `/metrics`** endpoint (see below)

## Observability: what's here and what you'd add

The spec's diagram footnotes "Prometheus + Grafana + Jaeger". Here's the honest
state and the path forward:

### Metrics (partially here)

`GET /metrics` (in `internal/api/metrics.go`) already emits Prometheus text
format: task backlog per status, DLQ depth, worker count, and per-worker
completed/failed counters. We hand-wrote the exposition format (no client
library) so you can *see* what Prometheus consumes. To complete the loop:

1. Point a Prometheus server at `/metrics` (a `scrape_config` job).
2. Import the series into Grafana; graph `tasks_total{status="pending"}` (backlog
   trend), `dlq_depth` (alert if it climbs), and
   `rate(worker_tasks_completed_total[5m])` (throughput).

**Good first extension:** add a **task-latency histogram**. Store each task's
execution duration (you already compute `dur` in `worker.execute`; persist it to
a column), then expose `task_duration_seconds` as Prometheus histogram buckets.
That unlocks the "execution time percentiles" the spec's dashboard mentions
(p50/p95/p99), which we intentionally left out to keep the schema lean.

### Distributed tracing (Jaeger) — not built, here's how

Tracing shines when a request crosses many services. Our path is short
(API → DB, worker → DB), so tracing is lower-value here than in a microservice
mesh — which is why it's out of scope. If you want to learn it:

1. Add OpenTelemetry (`go.opentelemetry.io/otel`).
2. Start a span when a task is claimed; propagate a trace id by storing it in the
   task's `payload` or a new column at submission time.
3. Export to Jaeger. You'll be able to follow a single task from submission
   through every retry to completion or dead-letter.

## Kafka (the "high-throughput variant") — a deliberate non-goal

The spec marks Kafka "optional, for high-throughput". We deliberately **did not**
use it, and understanding *why* is itself the lesson (ADR-001):

- Postgres-as-a-queue handles low-thousands of tasks/sec — beyond what most real
  systems need. Adding Kafka would add a major operational component to solve a
  scale problem we don't have.
- Keeping one datastore keeps the **locking mechanism visible**, which is the
  whole point of the project.

**When you'd actually reach for Kafka:** sustained tens-of-thousands of
tasks/sec, or a need to fan the same task lifecycle events out to many
independent consumers (analytics, audit, notifications). At that point you'd keep
Postgres for task *state* and publish *lifecycle events* (`task.created`,
`task.completed`, `task.dead_lettered`) to Kafka topics for consumers — which is
exactly the `task-events` / `dlq-events` topics the spec's diagram shows. The
worker's `execute` method is the natural place to emit those events.

## Other worthwhile extensions

- **Recurring/cron tasks.** Add a `cron_expr` column; a small scheduler goroutine
  in the server inserts the next `pending` task each time one completes. Our
  `scheduled_at` mechanism (`docs/02`) already supports the "run at a future
  time" half.
- **Task cancellation.** A `POST /tasks/{id}/cancel` that sets `pending` tasks to
  a `cancelled` status (and signals the worker for `running` ones via a flag it
  polls).
- **Priority fairness / rate limiting.** Per-task-type concurrency caps so one
  noisy type can't starve others (a `WHERE type NOT IN (types at capacity)`
  clause on the claim).
- **Payload/result size limits & archival.** Move very large payloads to blob
  storage and keep a reference, and periodically archive old `completed` rows to
  keep the hot table small.
- **AuthN/AuthZ on the API.** Right now the API is open; add an auth middleware
  (API keys or JWT) before exposing it beyond a trusted network.

## The meta-lesson

Notice the pattern in what we built vs. skipped: we implemented every piece that
teaches a **distributed-systems fundamental** (locking, delivery semantics,
failure recovery, liveness, clean shutdown) and stopped at pieces that are mostly
**operational scale-out** (Kafka, full tracing). For this project, depth on the
fundamentals beats breadth of infrastructure. When you extend this, keep asking:
*what's the smallest change that solves my actual problem?* — the same question
that led to Postgres-as-a-queue in the first place.
