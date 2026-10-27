// Package config loads all runtime configuration from environment variables.
//
// Why environment variables? Because this service is designed to run in
// containers (Docker / Kubernetes). The "12-factor app" methodology
// (https://12factor.net/config) tells us to keep configuration in the
// environment, not in code or committed files — so the same built image can run
// in dev, staging, and prod just by changing env vars. That is exactly what our
// docker-compose.yml and Kubernetes manifests do.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the fully-resolved configuration for any of our processes (server
// or worker). Not every field is used by every process, and that's fine — it is
// simpler to have one config type than to split it and duplicate the parsing.
type Config struct {
	// DatabaseURL is a standard Postgres connection string, e.g.
	// postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable
	DatabaseURL string

	// HTTPAddr is the address the API/dashboard server listens on, e.g. ":8080".
	HTTPAddr string

	// --- Worker tuning knobs ---

	// WorkerConcurrency is how many tasks a single worker process runs at once.
	// Each is handled by its own goroutine. More concurrency = more throughput
	// per process, but also more simultaneous load on task handlers and the DB.
	WorkerConcurrency int

	// PollInterval is how long a worker waits before asking the DB for work
	// again *when it found nothing last time*. This is the classic polling
	// trade-off: shorter = lower latency to pick up new tasks, but more empty
	// queries hammering the database. 1s is a sane default for a learning system.
	PollInterval time.Duration

	// HeartbeatInterval is how often a worker updates its last_heartbeat.
	HeartbeatInterval time.Duration

	// ShutdownGrace is the maximum time a worker will wait for in-flight tasks to
	// finish after receiving SIGTERM before it exits anyway.
	ShutdownGrace time.Duration

	// --- Reaper tuning knobs (used by the server process) ---

	// ReaperInterval is how often the reaper scans for dead workers / stale locks.
	ReaperInterval time.Duration

	// LockTTL is how long a task may stay 'running' with no fresh heartbeat from
	// its worker before the reaper considers the worker dead and re-queues it.
	// This MUST be comfortably larger than HeartbeatInterval, or a healthy but
	// briefly-busy worker could have its live task stolen. Rule of thumb:
	// LockTTL >= 3 * HeartbeatInterval.
	LockTTL time.Duration
}

// Load reads configuration from the environment, applying sensible defaults so
// the app runs out of the box in development. It returns an error only for
// values that cannot be defaulted safely (currently none, but the door is open).
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:       getenv("DATABASE_URL", "postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable"),
		HTTPAddr:          getenv("HTTP_ADDR", ":8080"),
		WorkerConcurrency: getenvInt("WORKER_CONCURRENCY", 5),
		PollInterval:      getenvDuration("POLL_INTERVAL", time.Second),
		HeartbeatInterval: getenvDuration("HEARTBEAT_INTERVAL", 10*time.Second),
		ShutdownGrace:     getenvDuration("SHUTDOWN_GRACE", 30*time.Second),
		ReaperInterval:    getenvDuration("REAPER_INTERVAL", 15*time.Second),
		LockTTL:           getenvDuration("LOCK_TTL", 30*time.Second),
	}

	// A tiny bit of validation to catch the one misconfiguration that would
	// silently corrupt behaviour: a lock TTL that isn't safely larger than the
	// heartbeat. We warn loudly by returning an error rather than letting
	// workers steal each other's live tasks.
	if c.LockTTL < 2*c.HeartbeatInterval {
		return nil, fmt.Errorf(
			"LOCK_TTL (%s) must be at least 2x HEARTBEAT_INTERVAL (%s); "+
				"otherwise a busy but healthy worker's task can be reaped",
			c.LockTTL, c.HeartbeatInterval)
	}

	return c, nil
}

// getenv returns the env var or a fallback.
func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getenvInt parses an int env var, falling back on missing/invalid input.
func getenvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// getenvDuration parses a Go duration string (e.g. "10s", "1m") from an env var.
func getenvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
