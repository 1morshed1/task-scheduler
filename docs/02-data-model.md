# 02 · The Data Model

The whole system is three tables. Getting them right is 80% of getting the
system right, because in this design **the database schema *is* the protocol**
that the independent processes coordinate through. The authoritative definition
is `internal/db/migrations/0001_init.sql` (heavily commented); this doc explains
the thinking.

## `tasks` — the queue

A row in `tasks` is simultaneously a stored record *and* a queue entry. Its
`status` column drives everything.

### The status lifecycle

```
                         ┌─────────────────────────────────────┐
                         │                                     │ retry
                         ▼                                     │ (backoff)
   create ──▶ pending ──────▶ running ──┬──▶ completed         │
                  ▲                     │                      │
                  │                     ├──▶ failed ───────────┘
     reaper       │                     │        │
  (worker died)   └─────────────────────┘        │ retries exhausted
                                                  ▼
                                                dead ──▶ (row copied into
                                                          dead_letter_queue)
```

- **pending** — claimable. The claim query looks for `status='pending' AND
  scheduled_at <= now()`.
- **running** — a worker holds it (`locked_by`, `locked_at` set).
- **completed** — success. Terminal.
- **failed** — a *single* attempt failed. The worker immediately decides the
  next state: back to `pending` with a future `scheduled_at` (retry), or `dead`.
  In practice a task rarely lingers in `failed`; it is the moment of decision.
- **dead** — retries exhausted. We also copy a snapshot into
  `dead_letter_queue`. Terminal until a human retries it.

We enforce these five values with a SQL `CHECK` constraint. A `CHECK` is a
cheap, permanent guardrail: no application bug, no manual `psql` edit, nothing
can ever put an invalid status in the table.

### Two clever uses of `scheduled_at`

`scheduled_at` does double duty, and this is worth internalizing:

1. **Delayed tasks.** Submit with a future `scheduled_at` → it simply isn't
   claimable until then.
2. **Retry backoff.** When an attempt fails, we set `status='pending'` and
   `scheduled_at = now() + backoff`. The *exact same* claim query that handles
   normal scheduling now also implements "wait 8 seconds then try again". One
   mechanism, two features. See `docs/06`.

### `locked_by` / `locked_at`

These record *who* is running a task and *since when*. `locked_at` is what the
reaper reads: if a task has been `running` longer than `LOCK_TTL` with no fresh
heartbeat, the worker is presumed dead and the task is re-queued (`docs/07`).

### Indexing: the partial index

```sql
CREATE INDEX idx_tasks_claim
    ON tasks (priority DESC, scheduled_at ASC)
    WHERE status = 'pending';
```

The `WHERE status = 'pending'` makes this a **partial index** — it only contains
rows that are actually claimable. Over time the table fills with millions of
`completed` rows, but those are *not in this index*, so the claim query stays
fast forever and the index stays small. This is the kind of detail that
separates a queue that works in a demo from one that works at 3am six months in.

## `workers` — health & observability

One row per live worker. Powers the dashboard's "worker health" panel and gives
the reaper a place to mark a worker `dead`. `last_heartbeat` is the liveness
signal; `tasks_completed` / `tasks_failed` are running counters for
throughput display.

Note: the worker table is for *observability*. The actual task-stealing
protection does **not** depend on it — that comes from the row lock plus
`locked_at` on the task itself. This separation means even if the workers table
were wiped, task correctness would hold.

## `dead_letter_queue` — the failure inbox

Why a separate table instead of just leaving dead tasks in `tasks` with
`status='dead'`?

1. **It's a first-class operator surface.** "Show me everything that has
   failed permanently" is a single `SELECT * FROM dead_letter_queue` — no
   filtering the huge tasks table.
2. **It's a snapshot.** We copy `payload`, `task_type`, the `final_error`, and
   the `retry_count` at the moment of death. Even if you later prune old `tasks`
   rows, the DLQ retains the forensic record of what failed and why.
3. **Retry is explicit.** `POST /api/v1/dlq/{id}/retry` reads the snapshot,
   inserts a fresh `pending` task with `retry_count = 0`, and removes the DLQ
   entry. A human decision, not an automatic one — which is the whole point of a
   dead-letter queue.
