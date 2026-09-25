// Package db owns the PostgreSQL connection pool and the migration runner.
//
// We use pgxpool (from jackc/pgx/v5) rather than the standard database/sql
// package. Reasons, in order of importance:
//
//  1. pgx speaks the native Postgres wire protocol, so it supports Postgres
//     types (JSONB, arrays, etc.) directly and is measurably faster.
//  2. pgxpool is a purpose-built connection pool with good defaults.
//  3. It exposes RETURNING, batch, and COPY cleanly — handy for a queue.
//
// A connection *pool* matters here because every worker poll and every API
// request needs a connection, and opening a fresh TCP+TLS+auth connection per
// query would dominate our latency. The pool keeps a set of warm connections
// and hands them out.
package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles embeds the .sql files in ../../migrations into the binary at
// compile time. This is a lovely Go feature: the migrations travel *inside* the
// executable, so there is no "did you copy the SQL files to the server?" class
// of bug. The path is relative to THIS source file.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Connect opens a pgx connection pool and verifies it with a ping. It retries a
// few times because in a container world (docker-compose, k8s) the app often
// starts a beat before the database is ready to accept connections. Failing
// fast on the very first attempt would make the whole system fragile to boot
// ordering, so we back off and retry instead.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Pool sizing: for a learning system these defaults are plenty. In
	// production you'd tune MaxConns against your Postgres max_connections and
	// the number of worker/server replicas.
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 1 * time.Hour

	var pool *pgxpool.Pool
	// Retry with a simple linear backoff: 1s, 2s, 3s ... up to ~15s total.
	for attempt := 1; attempt <= 5; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			// NewWithConfig is lazy; Ping forces a real connection so we learn
			// now (not on the first query) whether the DB is reachable.
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			} else {
				err = pingErr
				pool.Close()
			}
		}

		wait := time.Duration(attempt) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("connect to database after retries: %w", err)
}

// Migrate applies any embedded migration files that have not yet been applied.
//
// This is a deliberately tiny, transparent migration runner rather than a
// dependency like golang-migrate. For a project this size that keeps the moving
// parts down and — more importantly for a learning repo — makes the mechanism
// completely visible: it is just a table of applied versions plus "run the ones
// we haven't run, in order, each in its own transaction".
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// 1. Ensure the bookkeeping table exists.
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// 2. Read which versions are already applied.
	applied := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	// 3. List embedded migration files and sort them so they apply in order.
	//    We name files like 0001_init.sql, 0002_....sql — lexical sort == order.
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// 4. Apply each unapplied migration in its own transaction. Running each in
	//    a transaction means a migration is all-or-nothing: a syntax error
	//    halfway through file 3 rolls back file 3 entirely, and files 1-2 stay
	//    applied. We stop on the first failure.
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}

		sqlBytes, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if err := applyOne(ctx, pool, version, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// applyOne runs a single migration file and records it, atomically.
func applyOne(ctx context.Context, pool *pgxpool.Pool, version, sqlText string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, sqlText); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version)
		return err
	})
}
