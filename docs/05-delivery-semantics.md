# 05 · Delivery Semantics — What Guarantee Do We Actually Provide?

This is the doc that separates "I wrote a task queue" from "I understand task
queues". The question: when the system runs a task, how many times does it run?

The honest, precise answer for our design is **at-least-once**. Let's unpack why,
why true exactly-once is a myth, and what you (and your task handlers) must do
about it.

## The three possibilities

- **At-most-once:** every task runs 0 or 1 times. Never double-runs, but may be
  *lost* (a crash mid-task means it never completes). Easy; usually unacceptable.
- **At-least-once:** every task runs 1 or more times. Never lost, but may
  *repeat*. This is what we provide, and what almost every real queue provides.
- **Exactly-once:** every task runs precisely 1 time. The holy grail — and, as a
  pure delivery guarantee across process/network crashes, **provably
  impossible**. You can only *approximate* it by making the work itself tolerant
  of repeats.

## Why we are at-least-once

Walk the happy path and then insert a crash:

1. Worker claims task #7 (`status='running'`, `locked_by=me`).
2. Worker runs the handler. **Success.**
3. Worker calls `CompleteTask` → `status='completed'`.

Now crash the worker *between steps 2 and 3* — the handler finished its
side-effect (say, it charged a credit card) but the worker died before recording
`completed`. The task is stuck in `running`. Eventually the reaper (see `docs/07`)
notices the stale lock and re-queues it. Another worker runs it **again** — the
card is charged twice.

There is no way to close this window with a queue alone: between "did the work"
and "recorded that I did the work" there is always a moment where a crash leaves
ambiguity. The database can't know the handler's side-effect happened if the
worker died before telling it. Hence: **at-least-once**.

> The famous line: "exactly-once delivery is impossible; exactly-once
> *processing* is achievable — by making processing idempotent."

## The contract this puts on task handlers

Because a task may run more than once, **handlers must be idempotent**: running
the same task twice must have the same effect as running it once. This is not
optional; it is the price of never losing work. Practical techniques:

- **Natural idempotency.** "Set user 42's status to active" is naturally
  idempotent — doing it twice is the same as once.
- **Idempotency keys.** For non-idempotent effects (charge a card, send an
  email), attach a unique key derived from the task and have the downstream
  system dedupe on it. Stripe, for example, takes an `Idempotency-Key` header.
- **Conditional writes.** "Insert this row if it doesn't already exist"
  (`INSERT ... ON CONFLICT DO NOTHING`).

We document this loudly because it's the single most common thing people get
wrong when they first build on a queue: they assume exactly-once, don't make
handlers idempotent, and get mysterious duplicate side-effects under load or
after a deploy.

## What our design does to *minimize* duplicates

We can't eliminate duplicates, but we keep them rare:

- **Atomic claim** (`docs/04`) means duplicates never happen from *concurrent*
  claiming — only from the crash-after-work window above.
- **Generous `LOCK_TTL`** (≥ 2× heartbeat) means we don't reap a task that's
  merely slow — only one whose worker is genuinely gone. Fewer false-positive
  reaps → fewer re-runs of tasks that were actually still in progress.
- **Graceful shutdown** (`docs/08`) means a *planned* worker stop (deploys, the
  common case) finishes its in-flight task and records the outcome, so a normal
  restart produces **zero** duplicates. Duplicates only come from *unplanned*
  crashes.

## Where "completed" is recorded matters

Notice we mark `completed` **after** the handler returns success, not before. If
we marked it before running, a crash mid-run would lose the task (at-most-once).
By recording completion last, a crash always errs toward *re-running* (at-least-
once). Given the choice between "sometimes lose work" and "sometimes repeat idempotent
work", repeating is almost always the safer default — so we bias that way
deliberately.
