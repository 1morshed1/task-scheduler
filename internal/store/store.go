// Package store is the data-access layer. EVERY SQL statement in the entire
// system lives in this package and nowhere else. That single rule is the most
// valuable architectural boundary in the project: if you want to know how data
// can possibly change, there is exactly one place to read.
//
// The store speaks in terms of models.* types and takes a context.Context on
// every method (so callers can cancel/timeout DB work). It never imports
// net/http or anything above it — data flows up to the handlers, HTTP concerns
// never leak down here.
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the connection pool. Methods hang off it, grouped across files:
//
//	store.go   - constructor + shared helpers
//	tasks.go   - task CRUD, the SKIP LOCKED claim, completion/failure paths
//	workers.go - worker registration, heartbeat, counters
//	dlq.go     - dead-letter queue browse + retry
//	stats.go   - aggregate counts for the API/dashboard
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store over an existing pool. We inject the pool rather than
// creating it here so that main owns the connection lifecycle (open once, close
// on shutdown) and tests can share one pool across cases.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for the rare caller that needs it (e.g. a
// health check that pings). Kept narrow on purpose.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
