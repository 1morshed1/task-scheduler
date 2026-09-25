package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/1morshed1/task-scheduler/internal/db"
	"github.com/1morshed1/task-scheduler/internal/store"
)

// newTestServer wires a real Server over the test database and returns an
// httptest.Server. These are integration tests: they exercise the HTTP layer
// AND the store AND Postgres together, which is where the interesting bugs
// (JSON shape, status codes, SQL) actually live. Skips if no DB is reachable.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("no database available: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE tasks, workers, dead_letter_queue`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	mux := http.NewServeMux()
	New(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() { ts.Close(); pool.Close() })
	return ts
}

func TestCreateAndGetTask(t *testing.T) {
	ts := newTestServer(t)

	// Create.
	res, err := http.Post(ts.URL+"/api/v1/tasks", "application/json",
		strings.NewReader(`{"type":"noop","payload":{"x":1},"max_retries":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}
	var created struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Status     string `json:"status"`
		MaxRetries int    `json:"max_retries"`
	}
	json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if created.ID == "" || created.Type != "noop" || created.Status != "pending" || created.MaxRetries != 2 {
		t.Fatalf("unexpected created task: %+v", created)
	}

	// Get it back.
	res, _ = http.Get(ts.URL + "/api/v1/tasks/" + created.ID)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
}

func TestCreateTaskValidation(t *testing.T) {
	ts := newTestServer(t)
	// Missing 'type' must be a 400.
	res, _ := http.Post(ts.URL+"/api/v1/tasks", "application/json",
		strings.NewReader(`{"payload":{}}`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing type status = %d, want 400", res.StatusCode)
	}
	res.Body.Close()

	// Malformed JSON must be a 400, not a 500.
	res, _ = http.Post(ts.URL+"/api/v1/tasks", "application/json",
		strings.NewReader(`{not json`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
}

func TestGetMissingTaskIs404(t *testing.T) {
	ts := newTestServer(t)
	res, _ := http.Get(ts.URL + "/api/v1/tasks/11111111-1111-1111-1111-111111111111")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	res.Body.Close()
}

func TestListReturnsEmptyArrayNotNull(t *testing.T) {
	ts := newTestServer(t)
	res, _ := http.Get(ts.URL + "/api/v1/tasks")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	// Must be "[]", never "null" — so clients can iterate without a nil check.
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("empty list body = %q, want []", string(body))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	// Create a task so at least one gauge is non-zero.
	http.Post(ts.URL+"/api/v1/tasks", "application/json",
		strings.NewReader(`{"type":"noop"}`))

	res, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	// Spot-check the Prometheus exposition format: HELP/TYPE headers and a
	// labeled sample must be present.
	for _, want := range []string{
		"# TYPE tasks_total gauge",
		`tasks_total{status="pending"}`,
		"dlq_depth",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
