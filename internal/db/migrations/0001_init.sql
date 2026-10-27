-- Migration 0001: initial schema for the distributed task scheduler.
--
-- Read docs/02-data-model.md for a column-by-column explanation. The comments
-- here focus on the WHY behind the less obvious choices.

-- gen_random_uuid() lives in pgcrypto on older Postgres; it is built in from
-- PG 13+. CREATE EXTENSION IF NOT EXISTS is a harmless no-op when built in.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- tasks: the queue itself. A row IS a unit of work.
-- ---------------------------------------------------------------------------
CREATE TABLE tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- 'type' names which handler runs this task (e.g. "send_email"). The worker
    -- looks it up in its handler registry. Free-form text so new task types need
    -- no schema change.
    type          TEXT NOT NULL,

    -- 'payload' is the task's input, opaque to the scheduler. JSONB (not TEXT)
    -- so it is validated as JSON on write and queryable if we ever need to be.
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Lifecycle state. We enforce the allowed values with a CHECK so a bug can
    -- never write a nonsense status.
    --   pending   -> waiting to be claimed (scheduled_at <= now())
    --   running   -> claimed by a worker, in flight
    --   completed -> finished successfully (terminal)
    --   failed    -> a single attempt failed; will become pending again or dead
    --   dead      -> exhausted retries, copied into dead_letter_queue (terminal)
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','running','completed','failed','dead')),

    -- Higher priority runs first among tasks that are due. Lets urgent work
    -- jump the line without a separate queue.
    priority      INT NOT NULL DEFAULT 0,

    -- The earliest time this task may run. For "run now" tasks it defaults to
    -- now(); for scheduled/retried tasks it is a future timestamp. The claim
    -- query filters on scheduled_at <= now(), which is how both delayed tasks
    -- AND backoff-retries are implemented with one mechanism.
    scheduled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Which worker currently holds this task (NULL when not running). Useful for
    -- the dashboard and for debugging "who is running this?".
    locked_by     UUID,

    -- When the current lock was taken. The reaper uses (now() - locked_at) to
    -- decide a worker died and re-queue the task. See docs/07.
    locked_at     TIMESTAMPTZ,

    retry_count   INT NOT NULL DEFAULT 0,
    max_retries   INT NOT NULL DEFAULT 3,

    -- The error message from the most recent failed attempt. Surfaced in the
    -- API and dashboard so you can see *why* something is retrying.
    last_error    TEXT,

    -- The handler's successful output, if any. Lets callers read a result back.
    result        JSONB,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ
);

-- The single most important index in the whole system. The claim query filters
-- by status = 'pending' and scheduled_at <= now(), ordered by priority then
-- scheduled_at. This partial index (WHERE status = 'pending') keeps the index
-- small — it only contains rows that are actually claimable, not the millions
-- of completed rows that may accumulate — and lets Postgres find the next task
-- to run without a full table scan. See docs/04.
CREATE INDEX idx_tasks_claim
    ON tasks (priority DESC, scheduled_at ASC)
    WHERE status = 'pending';

-- Supports the reaper's scan for stale running tasks.
CREATE INDEX idx_tasks_running_locked_at
    ON tasks (locked_at)
    WHERE status = 'running';

-- ---------------------------------------------------------------------------
-- workers: one row per live worker process, for health/observability.
-- ---------------------------------------------------------------------------
CREATE TABLE workers (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname         TEXT NOT NULL,

    -- active   -> claiming and running tasks
    -- draining -> received SIGTERM, finishing in-flight work, not claiming new
    -- dead     -> reaper marked it dead after a missed heartbeat
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','draining','dead')),

    last_heartbeat   TIMESTAMPTZ NOT NULL DEFAULT now(),
    tasks_completed  BIGINT NOT NULL DEFAULT 0,
    tasks_failed     BIGINT NOT NULL DEFAULT 0,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- dead_letter_queue: tasks that exhausted their retries. This is a SEPARATE
-- table (not just status='dead' on tasks) so the DLQ is a first-class thing an
-- operator can browse, and so it captures a snapshot of the task at the moment
-- it died — even if the original task row is later cleaned up.
-- ---------------------------------------------------------------------------
CREATE TABLE dead_letter_queue (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       UUID NOT NULL,
    task_type     TEXT NOT NULL,
    payload       JSONB NOT NULL,
    final_error   TEXT,
    retry_count   INT NOT NULL,
    exhausted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dlq_exhausted_at ON dead_letter_queue (exhausted_at DESC);
