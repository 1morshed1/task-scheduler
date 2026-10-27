// Package reaper is the background janitor that recovers work from workers that
// died without cleaning up after themselves. It runs inside the server process
// on a timer. Read docs/07 for the full story.
//
// The reaper is what makes the system self-healing: without it, a single worker
// crash would strand every task that worker was holding in the 'running' state
// forever. With it, those tasks are automatically returned to the queue (or
// dead-lettered if they're out of retries) a short, bounded time after the
// worker goes silent.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/1morshed1/task-scheduler/internal/config"
	"github.com/1morshed1/task-scheduler/internal/store"
)

// Reaper periodically scans for stale locks and dead workers.
type Reaper struct {
	store *store.Store
	cfg   *config.Config
	log   *slog.Logger
}

// New builds a Reaper.
func New(s *store.Store, cfg *config.Config, log *slog.Logger) *Reaper {
	return &Reaper{store: s, cfg: cfg, log: log}
}

// Run ticks every ReaperInterval until ctx is cancelled, running one sweep per
// tick. We run an immediate sweep on startup too, so a server that boots after a
// mass worker outage recovers right away rather than waiting a full interval.
func (r *Reaper) Run(ctx context.Context) {
	r.log.Info("reaper started",
		"interval", r.cfg.ReaperInterval, "lock_ttl", r.cfg.LockTTL)

	r.sweep(ctx) // immediate first pass

	ticker := time.NewTicker(r.cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("reaper stopped")
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep runs a single recovery pass. It is deliberately tolerant of errors: a
// transient DB hiccup should not kill the reaper, it should just be retried on
// the next tick.
func (r *Reaper) sweep(ctx context.Context) {
	requeued, dead, err := r.store.RequeueOrphanedTasks(ctx, r.cfg.LockTTL)
	if err != nil {
		r.log.Warn("reaper: requeue sweep failed", "err", err)
	} else if requeued > 0 || dead > 0 {
		r.log.Info("reaper: recovered orphaned tasks",
			"requeued", requeued, "dead_lettered", dead)
	}

	deadWorkers, err := r.store.MarkDeadWorkers(ctx, r.cfg.LockTTL)
	if err != nil {
		r.log.Warn("reaper: mark-dead-workers failed", "err", err)
	} else if deadWorkers > 0 {
		r.log.Info("reaper: marked dead workers", "count", deadWorkers)
	}
}
