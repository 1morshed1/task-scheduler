# 00 · Project Overview & How to Read These Docs

Welcome. This folder is your **textbook**. The code in this repo is a working
distributed task scheduler, and every non-obvious decision that went into it is
explained here in plain language, in the order it was built.

If you read these files top to bottom, you will understand not just *what* the
code does but *why* it is shaped the way it is — which is the part that actually
transfers to your next project.

## What we are building

A **distributed task scheduler**. Think of a simplified version of Celery
(Python), Sidekiq (Ruby), or Temporal (Go). The job of the system:

1. Accept **task definitions** over an HTTP API (`type`, `payload`, `schedule`,
   `max_retries`).
2. Store them durably in **PostgreSQL**.
3. Let a **pool of worker processes** pick tasks up and run them — with the
   guarantee that *two workers never run the same task at the same time*.
4. **Retry** failed tasks with **exponential backoff + jitter**.
5. When a task has failed too many times, move it to a **dead-letter queue
   (DLQ)** so a human can inspect it and retry it later.
6. Detect **crashed workers** and re-queue the tasks they were holding.
7. Shut workers down **gracefully** so an in-flight task is never killed
   mid-execution during a deploy.
8. Show all of this live on a **dashboard**.

## The hard problems this forces you to solve

This project was chosen precisely because it is *not* a CRUD app. To make it
correct you have to confront the classic distributed-systems questions:

| Problem | Where it shows up | Doc |
|---|---|---|
| **Mutual exclusion** — two workers, one task | Claiming a task | `04-locking-skip-locked.md` |
| **At-least-once vs exactly-once** | Retries + crashes | `05-delivery-semantics.md` |
| **Failure recovery** | Backoff + DLQ | `06-retries-and-dlq.md` |
| **Liveness detection** | Heartbeats + reaper | `07-heartbeats-and-reaper.md` |
| **Clean deploys** | Graceful shutdown | `08-graceful-shutdown.md` |

## Technology choices (short version — long version in `01-architecture.md`)

- **Go** — great concurrency primitives (goroutines, channels, `context`), a
  strong standard library HTTP server, and it compiles to a single static
  binary that is trivial to containerize.
- **PostgreSQL** — the star of the show. We deliberately use Postgres *as the
  queue itself* via `SELECT ... FOR UPDATE SKIP LOCKED`. No Redis, no RabbitMQ,
  no Kafka required. One fewer moving part, full transactional durability, and
  it teaches the locking concept better than a purpose-built broker that hides
  it. `04-locking-skip-locked.md` explains this at length.
- **Standard library only for HTTP** — no web framework. Go 1.22+ `http.ServeMux`
  has method-aware routing (`"POST /api/v1/tasks"`), which is all we need.
- **`pgx/v5`** — the one third-party dependency. It is the de-facto Postgres
  driver + connection pool for Go and is faster and more capable than the
  `database/sql` + `lib/pq` combination.
- **Server-Sent Events (SSE)** for the live dashboard instead of WebSockets —
  see `09-dashboard-sse-vs-websocket.md` for why the simpler tool is the right
  one here.

## Repository map

```
cmd/
  server/     the API + dashboard process (you run one or a few of these)
  worker/     the worker process (you run many of these)
internal/
  config/     read configuration from environment variables
  db/         connection pool + migration runner
  models/     the plain data types (Task, Worker, DeadLetter)
  store/      ALL the SQL lives here — the data access layer
  api/        HTTP handlers for the REST API
  worker/     the worker loop: claim, execute, retry, heartbeat, shutdown
  reaper/     background job that recovers tasks from dead workers
  dashboard/  the SSE hub + stats broadcaster
migrations/   versioned SQL schema files
web/          the dashboard's static HTML/JS
docs/         <- you are here
```

## How to run it (once built)

```bash
make db-up        # start postgres (or use your own — see .env.example)
make migrate      # create the tables
make run-server   # terminal 1: the API + dashboard on :8080
make run-worker   # terminal 2: a worker (run this in several terminals)
```

Then open http://localhost:8080 for the dashboard, and submit a task:

```bash
curl -X POST localhost:8080/api/v1/tasks \
  -H 'content-type: application/json' \
  -d '{"type":"flaky","payload":{"n":1},"max_retries":5}'
```

## Reading order

1. `01-architecture.md` — the big picture and why the pieces are split this way
2. `02-data-model.md` — the database schema, column by column
3. `03-config-and-db.md` — configuration and the connection pool
4. `04-locking-skip-locked.md` — **the heart of the system**
5. `05-delivery-semantics.md` — what guarantees we actually provide
6. `06-retries-and-dlq.md` — exponential backoff and the dead-letter queue
7. `07-heartbeats-and-reaper.md` — detecting and recovering from crashes
8. `08-graceful-shutdown.md` — surviving deploys without dropping work
9. `09-dashboard-sse-vs-websocket.md` — live UI with the simplest tool
10. `DECISIONS.md` — a running log of every notable decision and its trade-off
