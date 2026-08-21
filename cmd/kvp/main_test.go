package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freePort grabs an ephemeral port for a test listener. There is a small race
// between closing the probe listener and the server binding it, which is
// acceptable inside a test process.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRunEndToEnd wires the real config, telemetry, store, cleanup jobs and
// both HTTP listeners through run(), exercises the public API and admin
// endpoints over TCP, then cancels the lifetime context and asserts a clean
// shutdown. It is the regression net for cross-package wiring: any signature
// or behavioral break between packages fails here even if unit tests pass.
func TestRunEndToEnd(t *testing.T) {
	pub, adm := freePort(t), freePort(t)
	dbPath := filepath.Join(t.TempDir(), "kvp.db")

	t.Setenv("KVP_PORT", fmt.Sprintf("127.0.0.1:%d", pub))
	t.Setenv("KVP_METRICS_PORT", fmt.Sprintf("127.0.0.1:%d", adm))
	t.Setenv("KVP_DB_PATH", dbPath)
	t.Setenv("KVP_MEMORY_CACHE_MB", "1") // exercise the memory layer end to end
	t.Setenv("KVP_CLEANUP_INTERVAL", "100ms")
	t.Setenv("KVP_LOG_LEVEL", "error")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", pub)
	admin := fmt.Sprintf("http://127.0.0.1:%d", adm)

	waitForStatus(t, "readyz", admin+"/readyz", http.StatusOK)

	// Public API round trip through the full middleware chain.
	res, err := http.Post(base+"/it-test", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST /it-test: %v", err)
	}
	gotBody, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated || string(gotBody) != "stored" {
		t.Fatalf("POST /it-test = %d %q, want 201 %q", res.StatusCode, gotBody, "stored")
	}

	res, err = http.Get(base + "/it-test")
	if err != nil {
		t.Fatalf("GET /it-test: %v", err)
	}
	gotBody, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(gotBody) != "hello" {
		t.Errorf("GET /it-test = %d %q, want 200 %q", res.StatusCode, gotBody, "hello")
	}

	waitForStatus(t, "healthz", admin+"/healthz", http.StatusOK)

	// Metrics endpoint renders Prometheus text on the admin port.
	res, err = http.Get(admin + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics = %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not exit after context cancellation")
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file missing after run: %v", err)
	}
}

func waitForStatus(t *testing.T, name, url string, want int) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		res, err := client.Get(url)
		if err == nil {
			last = res.StatusCode
			_ = res.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s at %s never returned %d (last %d)", name, url, want, last)
}
