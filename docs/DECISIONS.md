# Decision Log (ADR-lite)

This file records notable engineering decisions as we make them, in the style of
lightweight Architecture Decision Records. Each entry: the decision, the context,
the alternatives, and the trade-off we accepted. Newest at the bottom so you can
read it as a story.

The point of this file is to teach you *how to think about trade-offs*, not just
to document what we picked. When you build your own systems, keeping a log like
this is one of the highest-leverage habits you can form.

---

## ADR-001 — Use PostgreSQL as the queue (no Redis/RabbitMQ/Kafka)

**Context.** We need a durable place to store tasks and a way for many workers
to pull work without stepping on each other.

**Decision.** Use a plain Postgres table as the queue, claimed with
`SELECT ... FOR UPDATE SKIP LOCKED`.

**Alternatives considered.**
- *Redis* (e.g. `BRPOPLPUSH`, or Redis Streams): very fast, but durability is
  weaker and we'd need a second datastore for task metadata anyway.
- *RabbitMQ / Kafka*: purpose-built, scale further, but add a large operational
  surface and hide the locking mechanics we specifically want to learn.

**Trade-off accepted.** Postgres-as-a-queue tops out at maybe a few thousand
tasks/sec — far beyond what this project needs and beyond most real apps. In
exchange we get: one datastore, full ACID durability, transactional enqueue
alongside business data, and a crystal-clear view of the locking primitive.
See `docs/04-locking-skip-locked.md`.

---

## ADR-002 — Claim a task in ONE atomic UPDATE, not BEGIN/SELECT/UPDATE/COMMIT

**Context.** The spec sketches the claim as an explicit transaction: `BEGIN;
SELECT ... FOR UPDATE SKIP LOCKED; UPDATE ...; COMMIT;`. That works, but it
holds a transaction open across two round-trips.

**Decision.** Collapse it into a single statement:
`UPDATE tasks SET ... WHERE id = (SELECT id ... FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING ...`.

**Trade-off accepted.** Slightly less obvious to a SQL beginner, so we explain
it carefully in the locking doc. In return: one network round-trip, no
application-managed transaction to leak, and the row is locked and updated in
the same breath — impossible to "forget to commit".

---

## ADR-003 — Standard library HTTP, no web framework

**Context.** We need a REST API and a dashboard.

**Decision.** Use `net/http` with the Go 1.22+ `ServeMux` that supports
`"POST /api/v1/tasks/{id}"`-style patterns.

**Trade-off accepted.** No middleware ecosystem out of the box, so we write a
little of our own (logging, JSON helpers). In return: zero dependency risk on
the HTTP layer, and you learn what a framework is actually doing for you.

---

## ADR-004 — Reaper increments retry_count when it re-queues a stale task

**Context.** When a worker crashes mid-task, the task is stuck in `running` with
a lock that will never be released. The reaper re-queues it. Question: does an
interruption "count" as a failed attempt?

**Decision.** Yes — the reaper increments `retry_count` when re-queuing.

**Alternatives considered.** Treat interruption as blameless and re-queue
without counting. Cleaner in theory (the task didn't fail, the worker did).

**Trade-off accepted.** A task that *reliably crashes its worker* (segfault,
OOM, infinite loop that gets OOM-killed) would, under the blameless policy, loop
forever: crash, reap, crash, reap. By counting the attempt, such a
"poison task" eventually exhausts its retries and lands in the DLQ where a human
sees it. We trade a little bookkeeping purity for guaranteed forward progress.
See `docs/07-heartbeats-and-reaper.md`.

---

## ADR-005 — Server-Sent Events (SSE) for the dashboard, not WebSockets

**Context.** The spec says "Dashboard (Go + WebSocket)". The dashboard only ever
pushes data *from* server *to* browser (counts, worker health). The browser
never streams data back over the same channel.

**Decision.** Use SSE (`text/event-stream`) instead of WebSockets.

**Trade-off accepted.** SSE is one-directional and text-only. That is exactly
our use case. In return: no third-party WebSocket library, it's plain HTTP (works
through every proxy), auto-reconnects in the browser for free, and the server
side is ~30 lines of standard library. Actions that *do* go browser→server (the
DLQ "retry" button) use an ordinary `POST`. See
`docs/09-dashboard-sse-vs-websocket.md`.
