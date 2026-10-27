# 03 · Configuration & the Database Connection

Two small but load-bearing pieces of plumbing: how the app is configured, and
how it talks to Postgres. Files: `internal/config/config.go`, `internal/db/db.go`.

## Configuration: environment variables, with defaults

We follow the [12-factor](https://12factor.net/config) rule: **config lives in
the environment.** The same compiled binary runs in dev, staging, and prod; only
the env vars differ. `config.Load()` reads them, applies sensible development
defaults, and validates.

Knobs you can turn (all optional — defaults shown):

| Env var | Default | What it controls |
|---|---|---|
| `DATABASE_URL` | local dev DSN | Postgres connection string |
| `HTTP_ADDR` | `:8080` | server listen address |
| `WORKER_CONCURRENCY` | `5` | tasks one worker runs at once |
| `POLL_INTERVAL` | `1s` | wait between empty claim attempts |
| `HEARTBEAT_INTERVAL` | `10s` | how often a worker proves it's alive |
| `SHUTDOWN_GRACE` | `30s` | max wait for in-flight tasks on SIGTERM |
| `REAPER_INTERVAL` | `15s` | how often we scan for dead workers |
| `LOCK_TTL` | `30s` | stale-lock threshold before re-queuing |

### The one validation that matters

`Load()` refuses to start if `LOCK_TTL < 2 * HEARTBEAT_INTERVAL`. Why so strict?

If the lock TTL were, say, equal to the heartbeat interval, then a perfectly
healthy worker that is *busy running a long task* could miss a single heartbeat
by a hair and have its live task reaped and handed to another worker — now two
workers run the same task. Requiring a comfortable margin (we suggest 3×, we
enforce ≥2×) means only a genuinely dead worker gets reaped. This is a great
example of a config constraint that encodes a correctness invariant. See
`docs/07`.

## The connection pool

We use `pgxpool`. A pool keeps a handful of warm connections and lends them out,
because establishing a Postgres connection (TCP + TLS + auth) is expensive
relative to a query. Every worker poll and API request borrows a connection and
returns it.

### Connect-with-retry

`db.Connect` pings the database and **retries with backoff** if it isn't ready.
In containers the app frequently boots a second or two before Postgres is
accepting connections. Without retry, boot ordering becomes a source of flaky
crashes and you end up writing `depends_on` gymnastics or restart loops. With
retry, the app just waits calmly for the DB to come up. Robust startup is a
feature.

## The migration runner

Rather than pull in a migration library, `db.Migrate` is ~60 lines you can read
in one sitting. It:

1. Creates a `schema_migrations` bookkeeping table if absent.
2. Reads which versions already ran.
3. Uses Go's `//go:embed` to bundle the `.sql` files **into the binary** — so
   there is never a "the SQL files weren't deployed" problem.
4. Applies each not-yet-applied file, in lexical order (hence the `0001_`,
   `0002_` naming), **each in its own transaction** so a migration is
   all-or-nothing.

`//go:embed` is worth calling out: the migration files physically live inside
the executable after compilation. `go build` produces one artifact that carries
its own schema. That is a lovely property for a containerized service.

### Why embed migrations under `internal/db/`?

`//go:embed` can only see files at or below the directory of the Go source file
declaring it. So the SQL lives at `internal/db/migrations/*.sql`, next to the
code that embeds it, rather than at the repo root. A minor placement quirk with
a big payoff: self-contained binaries.
