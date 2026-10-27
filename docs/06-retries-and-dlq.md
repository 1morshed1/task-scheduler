# 06 · Retries, Exponential Backoff, and the Dead-Letter Queue

Tasks fail. Networks blip, dependencies restart, inputs are briefly invalid. A
good scheduler treats failure as normal and has a disciplined policy for it.
Ours has three stages: **retry with backoff**, then **dead-letter**, then
**operator retry**. Code: `internal/worker/{backoff.go,worker.go}` and
`internal/store/{tasks.go,dlq.go}`.

## Stage 1 — Retry with exponential backoff + jitter

When a handler returns an error and retries remain, the worker does *not* try
again immediately. It reschedules the task for the future using
`NextRetryDelay(retryCount+1)`:

```
retry 1 → ~2–3s   (2^1 + jitter)
retry 2 → ~4–6s   (2^2 + jitter)
retry 3 → ~8–12s  (2^3 + jitter)
retry 4 → ~16–24s ... capped at 5 minutes
```

Two ideas are stacked here:

**Exponential** (double each time). If a downstream dependency is down, retrying
every second just piles on load and slows its recovery. Backing off gives it
room to heal while still recovering fast from momentary blips.

**Jitter** (a random add-on). Imagine 500 tasks all fail at the same instant
because the database went down for everyone. Without jitter they'd *all* retry at
exactly +2s, then all at +4s — synchronized stampedes that re-crash the
dependency the moment it recovers. Jitter smears the retries across time so the
load is smooth. This is AWS's well-known "exponential backoff **and jitter**"
result; the naming in `backoff.go` calls it out.

### How a retry re-enters the queue

`RetryTask` sets the task back to `status='pending'` and
`scheduled_at = now() + delay`. That's it. The *same* claim query
(`WHERE status='pending' AND scheduled_at<=now()`) will pick it up once the delay
elapses. **Retry and scheduling are the same mechanism** — we didn't build a
separate retry timer. (See `docs/02` on `scheduled_at` doing double duty.)

The failed attempt is also recorded: `retry_count++` and `last_error` is stored
so you can see *why* it's retrying, in the API and dashboard.

## The retry-budget arithmetic

`max_retries` is the number of *retries*, not total attempts. With the default
`max_retries = 3`:

```
attempt 1 (initial)  retry_count=0  fail → schedule retry, retry_count→1
attempt 2 (retry 1)  retry_count=1  fail → schedule retry, retry_count→2
attempt 3 (retry 2)  retry_count=2  fail → schedule retry, retry_count→3
attempt 4 (retry 3)  retry_count=3  fail → retries exhausted → DEAD-LETTER
```

So total attempts = `max_retries + 1`. The decision in `worker.execute` is
simply: `if retry_count >= max_retries → dead-letter, else retry`. Set
`max_retries: 0` when you submit a task and it gets exactly one shot.

## Stage 2 — Dead-letter

When retries are exhausted, `DeadLetterTask` does three things **in one
transaction** (so they can't diverge):

1. Marks the task `status='dead'` (terminal).
2. Inserts a **snapshot** into `dead_letter_queue` — the type, payload,
   `final_error`, and `retry_count` at the moment of death.
3. Increments the worker's `tasks_failed` counter.

Why a snapshot in a separate table rather than just `status='dead'`? Because the
DLQ is meant to be a **human's inbox of failures**: "show me everything that gave
up, and why". It's a first-class operator surface, and it survives even if old
`tasks` rows are later pruned. (See `docs/02`.)

## Stage 3 — Operator retry (recovery)

The whole point of a dead-letter queue is that a human can look, *fix the
underlying problem* (deploy a bug fix, restore the dependency), and try again.
`POST /api/v1/dlq/{id}/retry` (handler in `internal/api`, logic in
`store.RetryDeadLetter`) does this atomically:

1. Read the DLQ snapshot.
2. Insert a **fresh** `pending` task from it with `retry_count = 0` (a clean
   slate — full retry budget again).
3. Delete the DLQ entry.

We create a *new* task rather than resurrecting the dead one, so the historical
record of the original failure stays intact. The dashboard exposes this as a
"Retry" button next to each DLQ entry (`docs/09`).

## Robustness detail: panicking handlers

`worker.runHandler` wraps the handler call in a `recover()`. If a handler panics
(nil-pointer, etc.), we convert the panic into an ordinary failed attempt instead
of letting it crash the worker process — which would also kill every sibling task
that worker is running concurrently. A single bad task should degrade to a
retry/dead-letter, never take down the fleet.

## See it end to end

- Submit a `fail_always` task with `max_retries: 2` and watch the logs: two
  retries with growing backoff, then a dead-letter.
- Submit a `flaky` task (fails ~60%): usually succeeds within a couple of
  retries, demonstrating that backoff pushes most transient failures through.
- Then hit the DLQ retry button (or the API) on a dead-lettered task and watch a
  fresh task get enqueued.
