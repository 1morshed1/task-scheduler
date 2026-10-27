# 08 · Graceful Shutdown

Deploys happen constantly. Every deploy stops the old worker pods and starts new
ones. If stopping a worker killed whatever task it was running, every deploy
would leave a trail of half-finished work to be retried — noisy at best,
duplicate side-effects at worst. Graceful shutdown makes a *planned* stop
produce **zero** interrupted tasks. Code: `internal/worker/worker.go`.

## How Kubernetes (and Docker) stop a process

They don't `kill -9` first. They send **SIGTERM** and wait (Kubernetes'
`terminationGracePeriodSeconds`, default 30s) before escalating to SIGKILL. That
grace window is our chance to finish up cleanly. Our job: catch SIGTERM, stop
taking new work, let in-flight work finish, then exit — all inside that window.

## The mechanism, step by step

`cmd/worker` creates a context that is cancelled on SIGTERM/SIGINT
(`signal.NotifyContext`). `Worker.Run` blocks on that context and, when it fires,
runs this sequence:

```
1. close(shutdownCh)          → claim loops stop picking up NEW tasks
2. SetWorkerStatus(draining)  → dashboard shows this worker as draining
3. wait for in-flight tasks   → up to ShutdownGrace (default 30s)
       ├─ all finish in time  → clean shutdown, zero interruptions ✅
       └─ grace expires        → cancelTasks(): cooperative handlers get
                                   ctx cancellation; whatever is still stuck
                                   will be reaped from the DB later
4. stop heartbeat, Deregister → leave no 'dead' row behind
5. return → process exits
```

### The two-context trick (the crux)

There are **two** contexts in play, and keeping them separate is what makes
shutdown graceful rather than abrupt:

- The **signal context** (`ctx`) tells `Run` *when to begin* shutting down. Claim
  loops watch a derived `shutdownCh` to stop claiming.
- The **task context** (`taskCtx`) is what handlers actually receive. It is
  **deliberately NOT derived from the signal context.** If it were, SIGTERM would
  instantly cancel the running handler — the opposite of graceful. Instead
  `taskCtx` stays alive so the in-flight task runs to completion, and we only
  cancel it *ourselves* as a last resort if the grace period expires.

New engineers almost always wire the handler's context straight to the signal
context and are surprised their "graceful" shutdown kills tasks instantly. This
separation is the fix.

### Why keep heartbeating while draining?

If a worker stopped heartbeating the moment it started draining, the reaper could
declare it dead and re-queue the very task it is busy finishing — creating a
duplicate. So we keep the heartbeat goroutine running until *after* the in-flight
tasks are done and we're about to exit. Liveness must outlast the drain.

### Clean deregister vs. crash

A graceful exit calls `DeregisterWorker`, which **deletes** the worker row: a
worker that left politely has no orphaned tasks and needn't linger in the health
view. Contrast a *crash*: no deregister happens, the row goes stale, and the
reaper marks it `dead` and re-queues its tasks. The two paths are intentionally
different so the dashboard can distinguish "left cleanly" from "died".

## How this ties back to delivery semantics

`docs/05` explained we're at-least-once, and that duplicates come from *unplanned*
crashes. Graceful shutdown is why **planned** stops (the overwhelmingly common
case — every deploy) produce no duplicates at all: the task finishes and records
`completed` before the process exits. We can't make crashes impossible, but we
make the routine case clean.

## Trying it yourself

1. Submit a `sleep` task with a long duration:
   `curl ... -d '{"type":"sleep","payload":{"ms":15000}}'`
2. Watch a worker claim it (logs show "task ... running").
3. Send that worker SIGTERM (Ctrl-C once, or `kill -TERM <pid>`).
4. Observe: it logs "draining", does **not** exit immediately, waits for the
   15s sleep to finish, logs "task completed" and "all in-flight tasks finished
   cleanly", then exits. The task ends up `completed`, never retried.
5. For contrast, `kill -9` a worker mid-sleep: no graceful path runs, and ~30s
   later the reaper re-queues the orphaned task (`docs/07`).
