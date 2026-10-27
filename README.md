# Distributed Task Scheduler with Dead-Letter Recovery

A from-scratch distributed cron/task scheduler in Go — a simplified cousin of
Celery, Sidekiq, or Temporal. It accepts tasks over an HTTP API, spreads them
across a pool of workers with an **exactly-once claim**, retries failures with
**exponential backoff + jitter**, routes exhausted tasks to a **dead-letter
queue**, recovers work from **crashed workers**, shuts down **gracefully** on
deploys, and shows it all on a **live dashboard**.

> **This repo is built to be read.** Every non-obvious decision is explained in
> [`docs/`](docs/) — start with [`docs/00-overview.md`](docs/00-overview.md).
> If you want to *learn* distributed systems, read the docs alongside the code.

## Why this project is interesting

It's deliberately not a CRUD app. Making it correct forces you to solve the
classic distributed-systems problems, and each one has a doc:

- **Mutual exclusion** — two workers must never run the same task →
  [`04-locking-skip-locked.md`](docs/04-locking-skip-locked.md)
- **Delivery semantics** — at-least-once, and why exactly-once is a myth →
  [`05-delivery-semantics.md`](docs/05-delivery-semantics.md)
- **Failure recovery** — backoff, retries, dead-letter →
  [`06-retries-and-dlq.md`](docs/06-retries-and-dlq.md)
- **Liveness & crash recovery** — heartbeats + reaper →
  [`07-heartbeats-and-reaper.md`](docs/07-heartbeats-and-reaper.md)
- **Clean deploys** — graceful shutdown →
  [`08-graceful-shutdown.md`](docs/08-graceful-shutdown.md)

## The big idea

We use **PostgreSQL as the queue itself**, via `SELECT ... FOR UPDATE SKIP
LOCKED`. No Redis, no RabbitMQ, no Kafka, and — because row locks *are* the
coordination — **no leader election**. Every worker is equal and discovers its
own slice of work by racing on the lock. One datastore, full durability, and the
core mechanism stays visible instead of hidden inside a broker. (See
[ADR-001](docs/DECISIONS.md) and [`docs/04`](docs/04-locking-skip-locked.md).)

## Architecture at a glance

```
 client ──POST /api/v1/tasks──▶ ┌─────────────┐        ┌──────────────┐
                                │   server    │◀──────▶│  PostgreSQL  │
 browser ──dashboard (SSE)────▶ │ API+dash+   │        │  tasks       │
                                │ reaper      │        │  workers     │
                                └─────────────┘        │  dead_letter │
                                                       └──────┬───────┘
                                         claim/heartbeat/complete
                                   ┌────────────┬───────────────┐
                                ┌──┴───┐    ┌───┴──┐        ┌────┴─┐
                                │worker│    │worker│  ...   │worker│
                                └──────┘    └──────┘        └──────┘
```

Two processes, one datastore. Full walkthrough:
[`docs/01-architecture.md`](docs/01-architecture.md).

## Quick start

**Option A — Docker (everything at once):**
```bash
docker compose up --build      # postgres + server + 3 workers
make seed                      # fire some demo tasks
open http://localhost:8080     # watch the live dashboard
```

**Option B — locally with Go:**
```bash
# 1. a Postgres reachable at the default DSN (see .env.example), then:
make run-server                # terminal 1: API + dashboard on :8080
make run-worker                # terminal 2 (and 3, 4, ...): a worker each
```

Submit a task and watch it flow:
```bash
curl -XPOST localhost:8080/api/v1/tasks \
  -H 'content-type: application/json' \
  -d '{"type":"flaky","payload":{},"max_retries":5}'
```

## HTTP API

| Method & path | Purpose |
|---|---|
| `POST /api/v1/tasks` | enqueue a task (`type`, `payload`, `priority`, `scheduled_at`, `max_retries`) |
| `GET /api/v1/tasks` | list tasks (`?status=`, `?limit=`) |
| `GET /api/v1/tasks/{id}` | fetch one task |
| `GET /api/v1/stats` | status counts + worker health + DLQ depth |
| `GET /api/v1/dlq` | browse the dead-letter queue |
| `POST /api/v1/dlq/{id}/retry` | re-enqueue a dead-lettered task |
| `GET /api/v1/events` | live stats stream (Server-Sent Events) |
| `GET /metrics` | Prometheus metrics (backlog, DLQ depth, per-worker throughput) |
| `GET /healthz`, `GET /readyz` | liveness / readiness probes |

## Built-in demo task types

Registered in every worker so the system is explorable out of the box
(`internal/worker/handlers.go`):

- `noop` — succeeds instantly.
- `sleep` — sleeps `payload.ms` milliseconds (great for watching graceful drain).
- `fail_always` — always fails (watch the retry → backoff → dead-letter pipeline).
- `flaky` — fails ~60% of the time (watch retries push it through).

Register your own by adding a `Handler` to the registry — one function, no other
changes.

## Configuration

All via environment variables with sane defaults; see
[`.env.example`](.env.example) and [`docs/03`](docs/03-config-and-db.md). Key
knobs: `WORKER_CONCURRENCY`, `HEARTBEAT_INTERVAL`, `LOCK_TTL` (must be ≥ 2×
heartbeat — enforced at startup), `SHUTDOWN_GRACE`, `POLL_INTERVAL`,
`REAPER_INTERVAL`.

## Testing

```bash
make test          # full suite (needs a reachable Postgres)
make test-short    # fast, DB-free unit tests only
```

The headline test is
[`TestClaimIsExclusive`](internal/store/store_test.go): 50 tasks, 20 concurrent
claimers, asserting each task is claimed *exactly once* — the executable proof of
the core correctness guarantee.

## Project layout

```
cmd/server, cmd/worker   the two process entrypoints
internal/config          env configuration
internal/db              pgx pool + embedded migration runner
internal/models          dependency-free shared types
internal/store           ALL SQL lives here (the data-access layer)
internal/api             REST handlers (net/http only)
internal/worker          claim loop, backoff, retries, graceful shutdown
internal/reaper          crash recovery
internal/dashboard       SSE hub + embedded live UI
deploy/k8s               Kubernetes manifests
docs                     the learning material — read this
```

## Deployment

Docker Compose for local, Kubernetes manifests in
[`deploy/k8s/`](deploy/k8s/). Full guide:
[`docs/10-deployment.md`](docs/10-deployment.md).

## Learn the trade-offs

The [decision log](docs/DECISIONS.md) records every notable choice as a
lightweight ADR — Postgres-as-queue, the single-statement claim, no framework,
the reaper's retry accounting, SSE over WebSockets — with the alternatives and
the trade-off accepted. That file is arguably the most useful thing here for
your own future systems.
