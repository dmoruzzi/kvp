package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

// newCommitTestStore opens a cached store at an explicit path so tests can
// close it and reopen the same file to verify persistence.
func newCommitTestStore(t *testing.T, memLimit int64) (string, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kvp.db")
	s, err := Open(path, memLimit)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return path, s
}

// reopen closes s and reopens the same file SQLite-only, simulating a restart:
// only durable rows are visible afterwards.
func reopen(t *testing.T, s *Store, path string) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	return s2
}

func get(t *testing.T, s *Store, key string) GetResult {
	t.Helper()
	res, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	return res
}

func TestPutSyncPersistsImmediately(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	if err := s.Put(context.Background(), "k", []byte("v1"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if s.DirtyCount() != 0 {
		t.Errorf("DirtyCount after sync put = %d, want 0", s.DirtyCount())
	}
	if n, err := s.Flush(context.Background()); err != nil || n != 0 {
		t.Fatalf("Flush after sync put = %d, %v; want 0, nil", n, err)
	}
	if got := get(t, reopen(t, s, path), "k"); !got.Found || !bytes.Equal(got.Value, []byte("v1")) {
		t.Errorf("reopened value = %v found=%v, want v1", got.Value, got.Found)
	}
}

func TestPutAsyncDefersPersistence(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	if err := s.PutWithMode(ctx, "k", []byte("v1"), time.Hour, PutAsync); err != nil {
		t.Fatalf("PutWithMode async: %v", err)
	}

	// Authoritative in memory, but nothing durable yet.
	if got := get(t, s, "k"); !got.Found || !bytes.Equal(got.Value, []byte("v1")) {
		t.Errorf("async read = %v found=%v, want v1", got.Value, got.Found)
	}
	if n := s.DirtyCount(); n != 1 {
		t.Errorf("DirtyCount = %d, want 1", n)
	}
	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("RowCount before flush = %d, want 0", rows)
	}

	if n, err := s.Flush(ctx); err != nil || n != 1 {
		t.Fatalf("Flush = %d, %v; want 1, nil", n, err)
	}
	if n := s.DirtyCount(); n != 0 {
		t.Errorf("DirtyCount after flush = %d, want 0", n)
	}
	if got := get(t, reopen(t, s, path), "k"); !got.Found || !bytes.Equal(got.Value, []byte("v1")) {
		t.Errorf("reopened value after flush = %v found=%v, want v1", got.Value, got.Found)
	}
}

func TestAsyncCoalescesLatestValue(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	for _, v := range []string{"v1", "v2", "v3"} {
		if err := s.PutWithMode(ctx, "k", []byte(v), time.Hour, PutAsync); err != nil {
			t.Fatalf("PutWithMode(%s): %v", v, err)
		}
	}
	if n := s.DirtyCount(); n != 1 {
		t.Errorf("DirtyCount = %d, want 1 (same key coalesces)", n)
	}
	if n, err := s.Flush(ctx); err != nil || n != 1 {
		t.Fatalf("Flush = %d, %v; want 1, nil", n, err)
	}
	if got := get(t, reopen(t, s, path), "k"); !got.Found || !bytes.Equal(got.Value, []byte("v3")) {
		t.Errorf("reopened value = %v found=%v, want v3 (latest wins)", got.Value, got.Found)
	}
}

func TestPutMemoryNeverPersists(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	if err := s.Put(ctx, "k", []byte("v1"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutWithMode(ctx, "k", []byte("v2"), time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode memory: %v", err)
	}

	// Memory is authoritative for reads; the durable copy keeps the old value.
	if got := get(t, s, "k"); !got.Found || !bytes.Equal(got.Value, []byte("v2")) {
		t.Errorf("memory read = %v found=%v, want v2", got.Value, got.Found)
	}
	if n, err := s.Flush(ctx); err != nil || n != 0 {
		t.Fatalf("Flush = %d, %v; want 0, nil (memory mode never dirties)", n, err)
	}
	if got := get(t, reopen(t, s, path), "k"); !got.Found || !bytes.Equal(got.Value, []byte("v1")) {
		t.Errorf("reopened value = %v found=%v, want v1 (persisted shadow resurfaces)", got.Value, got.Found)
	}
}

func TestMemoryAfterAsyncClearsDirty(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	if err := s.PutWithMode(ctx, "k", []byte("v1"), time.Hour, PutAsync); err != nil {
		t.Fatalf("PutWithMode async: %v", err)
	}
	if err := s.PutWithMode(ctx, "k", []byte("v2"), time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode memory: %v", err)
	}
	if n := s.DirtyCount(); n != 0 {
		t.Errorf("DirtyCount = %d, want 0 (memory mode cancels the pending flush)", n)
	}
	if n, err := s.Flush(ctx); err != nil || n != 0 {
		t.Fatalf("Flush = %d, %v; want 0, nil", n, err)
	}
	if got := get(t, reopen(t, s, path), "k"); got.Found {
		t.Errorf("reopened value = %v, want not found (client said never persist)", got.Value)
	}
}

func TestSyncAfterAsyncClearsDirty(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	if err := s.PutWithMode(ctx, "k", []byte("v1"), time.Hour, PutAsync); err != nil {
		t.Fatalf("PutWithMode async: %v", err)
	}
	if err := s.Put(ctx, "k", []byte("v2"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n := s.DirtyCount(); n != 0 {
		t.Errorf("DirtyCount = %d, want 0 (sync write persists the key)", n)
	}
	if n, err := s.Flush(ctx); err != nil || n != 0 {
		t.Fatalf("Flush = %d, %v; want 0, nil", n, err)
	}
	if got := get(t, reopen(t, s, path), "k"); !got.Found || !bytes.Equal(got.Value, []byte("v2")) {
		t.Errorf("reopened value = %v found=%v, want v2", got.Value, got.Found)
	}
}

func TestFlushWithoutMemoryLayerIsNoop(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.Flush(context.Background()); err != nil || n != 0 {
		t.Fatalf("Flush on SQLite-only store = %d, %v; want 0, nil", n, err)
	}
}

func TestPutWithModeDowngradesWithoutMemoryLayer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Async and memory degrade to sync when the memory layer is disabled.
	if err := s.PutWithMode(ctx, "k", []byte("v1"), time.Hour, PutAsync); err != nil {
		t.Fatalf("PutWithMode async: %v", err)
	}
	if err := s.PutWithMode(ctx, "k2", []byte("v2"), time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode memory: %v", err)
	}
	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("RowCount = %d, want 2 (both persisted)", rows)
	}
}

func TestParsePutMode(t *testing.T) {
	cases := []struct {
		in   string
		want PutMode
		ok   bool
	}{
		{"sync", PutSync, true},
		{"ASYNC", PutAsync, true},
		{"Memory", PutMemory, true},
		{"", PutSync, false},
		{"1", PutSync, false},
		{"instant", PutSync, false},
	}
	for _, tc := range cases {
		got, ok := ParsePutMode(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParsePutMode(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEvictOldestPurgesVolatileFirst(t *testing.T) {
	_, s := newCommitTestStore(t, 1<<20)
	st := s
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.now = fixed(now)
	ctx := context.Background()

	// One durable row (small), two volatile memory-only rows (large).
	if err := st.Put(ctx, "durable", []byte("d"), time.Hour); err != nil {
		t.Fatalf("Put durable: %v", err)
	}
	if err := st.PutWithMode(ctx, "m1", bytes.Repeat([]byte("x"), 1024), time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode m1: %v", err)
	}
	if err := st.PutWithMode(ctx, "m2", bytes.Repeat([]byte("y"), 1024), 2*time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode m2: %v", err)
	}

	// Budget fits only the durable entry: volatile entries must go first, and
	// the durable row must survive (never fully emptied).
	limit := entrySize("durable", entry{value: []byte("d")}) + 1
	deleted, err := st.EvictOldest(ctx, limit, 10, 8)
	if err != nil {
		t.Fatalf("EvictOldest: %v", err)
	}
	if deleted != 2 {
		t.Errorf("EvictOldest deleted = %d, want 2", deleted)
	}
	if got := get(t, st, "durable"); !got.Found || !bytes.Equal(got.Value, []byte("d")) {
		t.Errorf("durable entry lost: found=%v value=%v", got.Found, got.Value)
	}
	for _, key := range []string{"m1", "m2"} {
		if got := get(t, st, key); got.Found {
			t.Errorf("volatile entry %s survived eviction", key)
		}
	}
	if n := st.CacheBytes(); n >= limit {
		t.Errorf("CacheBytes = %d, want < %d after eviction", n, limit)
	}
	rows, err := st.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1 (durable row untouched)", rows)
	}
}

func TestEvictOldestRemovesShadowRowsOfVolatileVictims(t *testing.T) {
	path, s := newCommitTestStore(t, 1<<20)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)

	if err := s.Put(ctx, "k", []byte("v1"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutWithMode(ctx, "k", bytes.Repeat([]byte("x"), 2048), time.Hour, PutMemory); err != nil {
		t.Fatalf("PutWithMode memory: %v", err)
	}
	// Evict everything volatile; the shadowed stale row must not resurface.
	limit := int64(1)
	if _, err := s.EvictOldest(ctx, limit, 10, 8); err != nil {
		t.Fatalf("EvictOldest: %v", err)
	}
	if got := get(t, s, "k"); got.Found {
		t.Errorf("volatile entry survived eviction: %v", got.Value)
	}
	if got := get(t, reopen(t, s, path), "k"); got.Found {
		t.Errorf("stale shadow row survived eviction: %v", got.Value)
	}
}
