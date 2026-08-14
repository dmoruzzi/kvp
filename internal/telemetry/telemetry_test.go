package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsGoldenNames(t *testing.T) {
	tel, err := New(Config{ServiceName: "golden"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tel.Shutdown(context.Background())

	// Record at least one sample per instrument (§14 golden metrics test).
	m := tel.Metrics
	m.Request("GET", "index", "/", "200")
	m.Request("POST", "kvp", "/some-key", "201")
	m.RequestDuration("GET", "index", "/", 5*time.Millisecond)
	m.RequestInFlight("GET", "index", "/", 1)
	m.RequestInFlight("GET", "index", "/", -1)
	m.DBQuery("get", 2*time.Millisecond)
	m.KeyStored()
	m.KeyExpired()
	m.Error("kvp", "/some-key", "404")
	m.CleanupRun("expiry", "ok")
	m.CleanupRun("size", "error")
	m.CleanupDeleted("size", 7)
	m.SetDBSize(1234)
	m.SetDBRows(42)

	rr := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rr.Code)
	}
	out := rr.Body.String()

	want := []string{
		"kvp_http_requests_total",
		"kvp_http_request_duration_seconds",
		"kvp_http_request_in_flight",
		"kvp_db_query_duration_seconds",
		"kvp_db_size_bytes",
		"kvp_db_rows",
		"kvp_keys_stored_total",
		"kvp_keys_expired_total",
		"kvp_cleanup_runs_total",
		"kvp_cleanup_deleted_keys_total",
		"kvp_http_errors_total",
	}
	for _, name := range want {
		if !strings.Contains(out, name) {
			t.Errorf("golden metric %q missing from /metrics output", name)
		}
	}

	// The raw key path is present as a label (product requirement).
	if !strings.Contains(out, `path="/some-key"`) {
		t.Errorf("raw key path label missing; output:\n%s", out)
	}
	// Cleanup runs carry kind and result labels (Prometheus sorts labels
	// alphabetically: kind, result).
	if !strings.Contains(out, `kind="size"`) || !strings.Contains(out, `result="error"`) {
		t.Errorf("cleanup kind/result labels missing; output:\n%s", out)
	}
}

func TestLoggerLevel(t *testing.T) {
	tel, err := New(Config{ServiceName: "kvp", LogLevel: "error"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tel.Shutdown(context.Background())
	if tel.Logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("logger at level error should not be enabled for info")
	}
	if !tel.Logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("logger at level error should be enabled for error")
	}
}

func TestHealth(t *testing.T) {
	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errDB }

	// Liveness is 200 while the process runs.
	h := NewHealth(ok)
	rr := httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Errorf("/healthz body = %q", rr.Body.String())
	}

	// Readiness is 200 when ping succeeds.
	rr = httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/readyz (ok) = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"status":"ready"`) {
		t.Errorf("/readyz body = %q", rr.Body.String())
	}

	// Readiness is 503 when ping fails.
	h = NewHealth(fail)
	rr = httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz (db down) = %d, want 503", rr.Code)
	}

	// Readiness is 503 while draining even with a healthy DB.
	h = NewHealth(ok)
	h.SetDraining(true)
	rr = httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz (draining) = %d, want 503", rr.Code)
	}

	// Non-GET on health endpoints is 405.
	rr = httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/healthz", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", rr.Code)
	}
}

type dbErr struct{}

func (dbErr) Error() string { return "db unavailable" }

var errDB = dbErr{}
