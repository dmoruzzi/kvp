package server

import (
	"context"
	"net/http"
	"testing"
)

func TestCommitModeInvalidHeader(t *testing.T) {
	h, _ := newTestServerCached(t, Options{})
	for _, v := range []string{"instant", "1", "SYNC-FAST", ""} {
		if v == "" {
			continue // empty header means the server default, not an error
		}
		rr := doReq(t, h, "POST", "/k", "v", map[string]string{"X-Commit-Mode": v})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("POST with X-Commit-Mode: %s = %d, want 400", v, rr.Code)
		}
		if body(t, rr) != `{"error":"invalid X-Commit-Mode"}` {
			t.Errorf("body = %q", body(t, rr))
		}
	}
}

func TestCommitModeSyncDefault(t *testing.T) {
	h, st := newTestServerCached(t, Options{})
	rr := doReq(t, h, "POST", "/k", "v", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", rr.Code)
	}
	if body(t, rr) != "stored" {
		t.Errorf("body = %q, want stored", body(t, rr))
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "sync" {
		t.Errorf("X-Commit-Mode = %q, want sync", got)
	}
	if got := rr.Header().Get("X-Commit-Mode-Warning"); got != "" {
		t.Errorf("X-Commit-Mode-Warning = %q, want empty", got)
	}
	// Sync writes are durable without a flush.
	if rows, err := st.RowCount(context.Background()); err != nil || rows != 1 {
		t.Errorf("RowCount = %d, %v; want 1, nil", rows, err)
	}
}

func TestCommitModeAsync(t *testing.T) {
	h, st := newTestServerCached(t, Options{})
	rr := doReq(t, h, "POST", "/k", "v", map[string]string{"X-Commit-Mode": "async"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", rr.Code)
	}
	if body(t, rr) != "accepted" {
		t.Errorf("body = %q, want accepted", body(t, rr))
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "async" {
		t.Errorf("X-Commit-Mode = %q, want async", got)
	}
	if got := rr.Header().Get("X-Commit-Mode-Warning"); got != "" {
		t.Errorf("X-Commit-Mode-Warning = %q, want empty", got)
	}
	// Memory is authoritative for reads immediately.
	rr = doReq(t, h, "GET", "/k", "", nil)
	if rr.Code != http.StatusOK || body(t, rr) != "v" {
		t.Errorf("GET = %d body %q, want 200 v", rr.Code, body(t, rr))
	}
	if st.DirtyCount() != 1 {
		t.Errorf("DirtyCount = %d, want 1 (persistence deferred to the flush job)", st.DirtyCount())
	}
	if n, err := st.Flush(context.Background()); err != nil || n != 1 {
		t.Errorf("Flush = %d, %v; want 1, nil", n, err)
	}
}

func TestCommitModeCaseInsensitive(t *testing.T) {
	h, _ := newTestServerCached(t, Options{})
	rr := doReq(t, h, "POST", "/k", "v", map[string]string{"X-Commit-Mode": "ASYNC"})
	if rr.Code != http.StatusAccepted {
		t.Errorf("POST X-Commit-Mode: ASYNC = %d, want 202", rr.Code)
	}
	rr = doReq(t, h, "POST", "/k2", "v", map[string]string{"X-Commit-Mode": "Memory"})
	if rr.Code != http.StatusCreated {
		t.Errorf("POST X-Commit-Mode: Memory = %d, want 201", rr.Code)
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "memory" {
		t.Errorf("X-Commit-Mode = %q, want memory", got)
	}
}

func TestCommitModeMemory(t *testing.T) {
	h, st := newTestServerCached(t, Options{})
	// Durable value first so the shadow semantics are observable.
	if rr := doReq(t, h, "POST", "/k", "v1", nil); rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", rr.Code)
	}
	rr := doReq(t, h, "POST", "/k", "v2", map[string]string{"X-Commit-Mode": "memory"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST memory = %d, want 201", rr.Code)
	}
	if body(t, rr) != "stored" {
		t.Errorf("body = %q, want stored", body(t, rr))
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "memory" {
		t.Errorf("X-Commit-Mode = %q, want memory", got)
	}
	if got := rr.Header().Get("X-Commit-Mode-Warning"); got != "" {
		t.Errorf("X-Commit-Mode-Warning = %q, want empty", got)
	}
	// Read serves the memory value.
	rr = doReq(t, h, "GET", "/k", "", nil)
	if body(t, rr) != "v2" {
		t.Errorf("GET body = %q, want v2", body(t, rr))
	}
	// Memory writes never dirty the flush set.
	if n, err := st.Flush(context.Background()); err != nil || n != 0 {
		t.Errorf("Flush = %d, %v; want 0, nil", n, err)
	}
}

func TestCommitModeDowngradeWithoutMemoryCache(t *testing.T) {
	// newTestServer opens a SQLite-only store: async/memory degrade to sync
	// with a warning header (§5.2).
	h, _ := newTestServer(t, Options{})
	for _, v := range []string{"async", "memory"} {
		rr := doReq(t, h, "POST", "/"+v, "v", map[string]string{"X-Commit-Mode": v})
		if rr.Code != http.StatusCreated {
			t.Errorf("POST memory disabled, mode %s = %d, want 201", v, rr.Code)
		}
		if body(t, rr) != "stored" {
			t.Errorf("body = %q, want stored", body(t, rr))
		}
		if got := rr.Header().Get("X-Commit-Mode"); got != "sync" {
			t.Errorf("X-Commit-Mode = %q, want sync", got)
		}
		if got := rr.Header().Get("X-Commit-Mode-Warning"); got != "memory-cache-disabled" {
			t.Errorf("X-Commit-Mode-Warning = %q, want memory-cache-disabled", got)
		}
	}
}

func TestCommitModeServerDefaultOption(t *testing.T) {
	h, _ := newTestServerCached(t, Options{CommitMode: "async"})
	// No header: the server default applies.
	rr := doReq(t, h, "POST", "/k", "v", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", rr.Code)
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "async" {
		t.Errorf("X-Commit-Mode = %q, want async", got)
	}
	// Header still wins over the server default.
	rr = doReq(t, h, "POST", "/k2", "v", map[string]string{"X-Commit-Mode": "sync"})
	if rr.Code != http.StatusCreated {
		t.Errorf("POST = %d, want 201", rr.Code)
	}
	if got := rr.Header().Get("X-Commit-Mode"); got != "sync" {
		t.Errorf("X-Commit-Mode = %q, want sync", got)
	}
}
