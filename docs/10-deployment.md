# 10 · Deployment — Docker & Kubernetes

How the two processes become running infrastructure. Files: `Dockerfile`,
`docker-compose.yml`, `deploy/k8s/`.

## One image, two roles

Both `server` and `worker` are built into a **single Docker image** (multi-stage
`Dockerfile`). Which one runs is chosen by the container's command
(`/app/server` vs `/app/worker`). Why one image and not two?

- The build is identical up to the final binary; sharing it halves build time
  and registry storage.
- There's never a version-skew question of "is the worker image the same commit
  as the server image?" — they're literally the same image.

The multi-stage build compiles with the Go toolchain in stage 1, then copies
just the two static binaries into a ~10MB Alpine runtime in stage 2. Because the
migrations and the dashboard HTML are `//go:embed`ded, **there are no other files
to ship** — the image is essentially just the executables. Static binaries
(`CGO_ENABLED=0`) mean no libc dependency to worry about.

## Local: docker-compose

```bash
docker compose up --build
```

Brings up Postgres + 1 server + 3 workers. The `depends_on ... condition:
service_healthy` gates the app on Postgres's healthcheck, and the app's own
connect-with-retry (`docs/03`) covers any remaining startup race. Open
http://localhost:8080, then `make seed` to fire in some demo tasks and watch
them spread across the three workers live.

Scale workers on the fly:
```bash
docker compose up --build --scale worker=6
```

Note `stop_grace_period: 35s` on the worker service — the compose equivalent of
Kubernetes' termination grace period, giving the graceful drain (`docs/08`) room
to finish before compose force-kills.

## Kubernetes

`deploy/k8s/` maps the architecture straight onto k8s primitives:

| File | What | Why |
|---|---|---|
| `secret.yaml` | `DATABASE_URL` | one place for the connection string |
| `postgres.yaml` | demo Postgres | replace with managed DB in prod |
| `migrate-job.yaml` | `server -migrate-only` | schema owned by one Job, run before workers |
| `server.yaml` | Deployment + Service | API/dashboard/reaper, 2 replicas, probes |
| `worker.yaml` | Deployment | the scalable worker pool |

Three details worth studying:

**1. Health probes wire to our endpoints.** `livenessProbe → /healthz`
("process up?"), `readinessProbe → /readyz` ("DB reachable?"). Kubernetes only
routes traffic to a pod once readiness passes and restarts a pod whose liveness
fails. Our two endpoints exist precisely to answer these two different questions
(`internal/api/handlers.go`).

**2. `terminationGracePeriodSeconds: 40` ≥ `SHUTDOWN_GRACE: 30s`.** This is the
deployment half of graceful shutdown. On a rolling update, k8s sends the worker
SIGTERM and waits this long before SIGKILL. It **must** exceed the worker's own
`SHUTDOWN_GRACE`, or k8s would hard-kill the worker while it's still draining —
defeating the whole mechanism from `docs/08`. The 10s headroom absorbs
scheduling delays. Getting this ordering wrong is a classic production
foot-gun; the manifest comments call it out.

**3. Migrations as a Job, run before workers.** Rather than have every worker (or
racing server replicas) try to migrate, a dedicated one-shot Job applies the
schema. This gives schema changes a single owner and a clear ordering in the
deploy pipeline. (The server also auto-migrates on boot for the simple local
case, but in k8s the Job is the intended path.)

**Multiple reaper replicas are fine.** The server runs the reaper, and we run 2
server replicas — so 2 reapers. They don't coordinate, and they don't need to:
re-queuing an already-requeued task is a no-op, and marking an already-dead
worker dead is a no-op. Idempotent operations make redundancy free. (Contrast a
design that needed a *single* leader reaper — that would drag in leader election,
which our `SKIP LOCKED` approach let us avoid entirely; see `docs/04`.)

## Scaling knobs, recapped

- **Throughput** → more worker replicas and/or higher `WORKER_CONCURRENCY`.
- **API capacity** → more server replicas behind the Service.
- **Recovery speed vs. false reaps** → `LOCK_TTL` (`docs/07`).
- **Retry aggressiveness** → `max_retries` per task and the backoff curve
  (`docs/06`).

All of them are environment variables or per-task fields — nothing requires a
rebuild. That's the 12-factor payoff from `docs/03`.
