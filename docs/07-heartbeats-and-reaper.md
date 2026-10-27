# 07 · Heartbeats & the Reaper — Surviving Worker Crashes

`docs/08` handles the *polite* exit (SIGTERM). This doc handles the *impolite*
one: a worker that dies without warning — OOM-killed, segfault, power loss, node
evicted. When that happens, the tasks it had claimed are stuck in `running` with
a lock nobody will ever release. Left alone, they'd never run again. The
**heartbeat + reaper** pair fixes this automatically. Code:
`internal/worker/worker.go` (heartbeat), `internal/reaper/reaper.go` and
`internal/store/reaper.go` (recovery).

## Heartbeats: the liveness signal

Every worker updates its `last_heartbeat` timestamp every `HEARTBEAT_INTERVAL`
(default 10s). It's a single cheap `UPDATE workers SET last_heartbeat = now()`.
The presence of recent heartbeats is how the rest of the system knows a worker is
alive. A worker that has stopped heartbeating is, from the outside, ind
distinguishable from dead — so we treat it as dead.

Crucially, each claimed **task** also carries its own `locked_at` timestamp (set
at claim time). The reaper keys off *the task's* `locked_at`, not the worker row —
so task recovery works even if the worker table were somehow inconsistent. The
worker row's heartbeat is used to update the *dashboard's* view of worker health.

## The reaper: bounded, automatic recovery

The reaper runs inside the server process every `REAPER_INTERVAL` (default 15s).
Each sweep does two things (`store.RequeueOrphanedTasks`, `store.MarkDeadWorkers`):

**1. Recover orphaned tasks.** Find tasks that are `status='running'` with
`locked_at` older than `LOCK_TTL` (default 30s) — i.e. claimed by a worker that
has since gone silent — and:
- if the task still has retries left → set it back to `pending`, bump
  `retry_count`, make it runnable now. Another worker picks it up.
- if it's already out of retries → mark it `dead` and snapshot it into the DLQ.

**2. Mark dead workers.** Flip `workers` rows whose `last_heartbeat` is older than
`LOCK_TTL` to `status='dead'`, so the dashboard stops showing them as alive.
(Cosmetic — task recovery already happened via step 1.)

An immediate sweep runs on server startup too, so recovery after a mass outage
doesn't wait a full interval.

## The `LOCK_TTL` ≥ 2×`HEARTBEAT_INTERVAL` rule (why config enforces it)

This is the subtle correctness knob, enforced in `config.Load()`. Consider a
worker that is perfectly healthy but busy running a legitimately slow task. Its
heartbeat goroutine keeps firing every 10s. Now suppose `LOCK_TTL` were also 10s:
a single slightly-late heartbeat (GC pause, scheduler jitter) could make the
task's lock look "stale" for a moment, and the reaper would **steal a task that is
still actively running** — now two workers run it. Duplicate work, from a false
positive.

By requiring `LOCK_TTL` to be comfortably larger than the heartbeat interval (we
enforce ≥2×, recommend 3×), we guarantee a worker must miss *several* heartbeats
in a row before it's declared dead. Only a genuinely gone worker does that. This
is a lovely example of encoding a correctness invariant directly into config
validation — the app literally refuses to start in a mis-tuned state.

### The trade-off in `LOCK_TTL`

- **Too low** → false positives: live-but-busy workers get their tasks reaped →
  duplicate execution.
- **Too high** → slow recovery: after a real crash, tasks sit stuck for longer
  before being re-queued.

30s (with a 10s heartbeat) is a good default: 3 missed heartbeats before reaping,
recovery within ~half a minute.

## ADR-004: reaped tasks count as an attempt

When the reaper re-queues an orphaned task it **increments `retry_count`**. Why
count an interruption the task didn't cause? Because of *poison tasks*: a task
whose execution reliably kills its worker (triggers an OOM, an infinite loop that
gets the process killed). Under a "blameless" policy that task would loop
forever — run, crash the worker, get reaped, run, crash... By counting the
attempt, a poison task eventually exhausts its retries and lands in the DLQ where
a human notices it. We trade a little accounting purity for guaranteed forward
progress. (Full ADR in `docs/DECISIONS.md`.)

## Proof

`store_test.go::TestReaperRequeuesOrphans` claims a task, back-dates its
`locked_at` to simulate a dead worker, runs the reaper, and asserts the task is
`pending` again with `retry_count = 1`. That's the crash-recovery guarantee as an
executable test.
