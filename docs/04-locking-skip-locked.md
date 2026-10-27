# 04 · The Heart of the System — `FOR UPDATE SKIP LOCKED`

If you take one idea away from this project, make it this one. Everything else is
plumbing around this single mechanism.

## The problem

We have one table of pending tasks and *many* worker processes. We need:

> Every task is executed by **exactly one** worker. Two workers must never grab
> the same task at the same time. And workers must not block each other while
> competing for work.

Those requirements pull in opposite directions. Mutual exclusion usually means
locking, and locking usually means *waiting* — but we explicitly don't want
workers waiting on each other; we want them all busy.

## The naive approaches (and why they fail)

**Attempt 1: `SELECT` then `UPDATE`.**
```sql
SELECT id FROM tasks WHERE status='pending' LIMIT 1;   -- worker A and B both read task #7
UPDATE tasks SET status='running' WHERE id = 7;         -- both update it
```
Classic race. A and B both read task #7 before either updates it. Both run it.
**Double execution.** ❌

**Attempt 2: `SELECT ... FOR UPDATE` (plain).**
`FOR UPDATE` locks the selected row. Now if A locks task #7, B's identical query
*blocks* — it waits for A's transaction to finish. Correct (no double claim) but
**slow**: with 20 workers, 19 of them queue up behind the one holding the lock,
all trying for the same row. You've serialized your whole worker pool. ❌

## The fix: `SKIP LOCKED`

```sql
SELECT id
FROM tasks
WHERE status = 'pending' AND scheduled_at <= now()
ORDER BY priority DESC, scheduled_at ASC
FOR UPDATE SKIP LOCKED
LIMIT 1;
```

`SKIP LOCKED` changes one thing: instead of *waiting* for a row another
transaction has locked, Postgres **skips over it** and returns the next
unlocked candidate. So:

- Worker A's query locks and returns task #7.
- Worker B's *simultaneous* query sees #7 is locked, skips it, locks and returns
  task #8.
- ...and so on. Twenty workers → twenty different tasks, no waiting, no
  double-claims.

That is the entire trick. Correct **and** non-blocking. It turns a plain
Postgres table into a high-quality work queue.

## Our exact statement (and why it's one UPDATE)

The plan sketches this as `BEGIN; SELECT ... FOR UPDATE SKIP LOCKED; UPDATE ...;
COMMIT;`. We collapse it into a single atomic statement (see `store/tasks.go`,
`ClaimTask`, and ADR-002):

```sql
UPDATE tasks
SET status='running', locked_by=$1, locked_at=now(), started_at=now(), updated_at=now()
WHERE id = (
    SELECT id FROM tasks
    WHERE status='pending' AND scheduled_at <= now()
    ORDER BY priority DESC, scheduled_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, type, payload, retry_count, max_retries;
```

The inner `SELECT ... FOR UPDATE SKIP LOCKED` picks and locks one unlocked due
task. The outer `UPDATE` flips it to `running` and stamps the worker id, then
`RETURNING` hands the worker exactly the fields it needs. It is **one statement,
one round-trip, atomic**. The row is locked and mutated in the same breath —
there is no window in which we hold a lock but haven't updated, and no
application-managed transaction that could be left uncommitted.

The instant that `UPDATE` commits, the row's `status` is `running`, so it no
longer matches the `status='pending'` filter and can never be handed to a second
worker.

## Why the ORDER BY matters here

`ORDER BY priority DESC, scheduled_at ASC` gives us priority scheduling and FIFO
fairness within a priority, essentially for free. And because of the **partial
index** `idx_tasks_claim ON tasks(priority DESC, scheduled_at ASC) WHERE
status='pending'` (see `docs/02`), Postgres can satisfy both the filter and the
ordering straight from the index — it walks the index in order and returns the
first unlocked row. No sorting a huge table on every poll.

## No leader, no coordination service

Notice what we did *not* build: no leader election, no ZooKeeper/etcd, no
distributed lock manager, no worker-to-worker communication. The database's row
locks are the coordination. Every worker is identical and equal; they discover
their disjoint slices of work purely by racing on `SKIP LOCKED`. This is why the
architecture in `docs/01` has no "leader" box. Fewer moving parts, less to get
wrong.

## Proof it actually works

`internal/store/store_test.go::TestClaimIsExclusive` creates 50 tasks and turns
20 goroutines loose claiming them concurrently, recording every claim. It
asserts each task was claimed **exactly once**. Run it:

```bash
go test ./internal/store/ -run TestClaimIsExclusive -v
```

That test is the executable version of the guarantee at the top of this page.

## Where this pattern's limits are

`SKIP LOCKED` on Postgres realistically scales to low thousands of
claims/second. Past that you'd shard the table, batch-claim multiple rows per
query, or move to a dedicated broker. For the vast majority of real systems —
and certainly for everything this project targets — it is more than enough, and
it keeps the whole design to a single datastore. (Recorded as ADR-001.)
