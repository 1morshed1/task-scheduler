# 01 · Architecture

## The 10,000-foot view

```
                 ┌──────────────┐
   HTTP client → │   server     │   (1..N replicas, stateless)
   (curl, UI)    │  API + dash  │
                 │  + reaper    │
                 └──────┬───────┘
                        │  reads/writes
                        ▼
                 ┌──────────────┐
                 │  PostgreSQL  │   the single source of truth
                 │  tasks       │   AND the queue AND the coordination layer
                 │  workers     │
                 │  dead_letter │
                 └──────┬───────┘
                        ▲  claim / heartbeat / complete
                        │
        ┌───────────────┼───────────────┐
        │               │               │
   ┌─────────┐     ┌─────────┐     ┌─────────┐
   │ worker  │     │ worker  │     │ worker  │   (1..N replicas)
   └─────────┘     └─────────┘     └─────────┘
```

There are exactly **two kinds of process** and **one datastore**.

### The `server` process (`cmd/server`)
Stateless. Runs the REST API (submit tasks, query status, retry from the DLQ)
and serves the live dashboard. It also hosts the **reaper** goroutine, which
periodically re-queues tasks orphaned by crashed workers. You can run one, or
several behind a load balancer — they share nothing but the database.

### The `worker` process (`cmd/worker`)
Also effectively stateless (its only durable state lives in Postgres). It loops:
claim a due task, run the matching handler, record the outcome, repeat. It sends
a heartbeat on a timer and shuts down gracefully on SIGTERM. You run **many** of
these to scale throughput — they coordinate purely through the database's row
locks, never by talking to each other.

### PostgreSQL
Does triple duty:
1. **Storage** — durable task/worker/DLQ records.
2. **Queue** — the `tasks` table, claimed via `FOR UPDATE SKIP LOCKED`.
3. **Coordination** — row locks *are* the mutual exclusion. There is no separate
   lock service, no ZooKeeper, no leader election. This is the big idea.

## Why split server and worker at all?

They have different scaling and failure profiles:
- The API is bursty and latency-sensitive; workers are throughput-oriented and
  can be slow without users noticing.
- You want to scale workers up and down (even to zero) independently of the API.
- A worker crash must never take the API down, and vice-versa.

Running them as separate binaries makes those independent. In development you
just run them in two terminals; in Kubernetes they are two Deployments.

## Why is coordination-free (no leader election) possible?

The plan mentions leader election as a "hard problem", and it is — *if* you
build a scheduler where one node must own the schedule. We sidestep it: because
claiming is an atomic database operation with `SKIP LOCKED`, **every worker is
equal**. There is no leader to elect. The database is the arbiter. This is a
deliberate simplification and a genuinely good pattern; `docs/04` explains why
it is correct.

## Package dependency direction

Dependencies point *inward* toward plain data, never outward toward HTTP:

```
cmd/server ─┐
            ├─→ internal/api ──────┐
cmd/worker ─┤                      ├─→ internal/store ─→ internal/models
            ├─→ internal/worker ───┤        │
            ├─→ internal/reaper ───┘        └─→ internal/db (pgxpool)
            └─→ internal/dashboard          
```

- `models` knows about nothing (just structs).
- `store` knows about `models` and the DB. **All SQL lives here** — nowhere else
  in the codebase writes a query. This is the single most useful boundary in the
  project: if you want to know how data changes, there is exactly one place to
  look.
- `api`, `worker`, `reaper`, `dashboard` depend on `store`, never on each other.
- `cmd/*` wires everything together (dependency injection by hand in `main`).

Keeping this direction means the interesting logic never touches HTTP or SQL
directly, so it stays testable and easy to reason about.
