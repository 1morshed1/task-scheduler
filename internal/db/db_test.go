package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// testDSN returns the database connection string for tests. It honors
// DATABASE_URL so CI can point it at its own Postgres, and falls back to the
// local development database otherwise. Tests that need a DB are skipped when it
// is unreachable, so `go test ./...` never fails just because Postgres is down.
func testDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable"
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := Connect(ctx, testDSN())
	if err != nil {
		t.Skipf("no database available, skipping: %v", err)
	}
	defer pool.Close()

	// Running Migrate twice must succeed both times: the second run should find
	// every migration already applied and do nothing. This guards the core
	// promise of the runner — it is safe to run on every boot.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate (idempotency): %v", err)
	}

	// Sanity: the tables we expect should now exist.
	for _, table := range []string{"tasks", "workers", "dead_letter_queue", "schema_migrations"} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("expected table %q to exist after migrate", table)
		}
	}
}
