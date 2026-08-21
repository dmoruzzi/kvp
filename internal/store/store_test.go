package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kvp.db"), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newCachedTestStore(t *testing.T, memLimit int64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kvp.db"), memLimit)
	if err != nil {
		t.Fatalf("Open cached: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func fixed(now time.Time) func() time.Time { return func() time.Time { return now } }

func TestOpenAppliesSchemaAndPragmas(t *testing.T) {
	s := newTestStore(t)

	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var bt int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil {
		t.Fatal(err)
	}
	if bt != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", bt)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='kv_store'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("kv_store table count = %d, want 1", n)
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_kv_store_expires_at'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("idx_kv_store_expires_at count = %d, want 1", n)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)

	if err := s.Put(context.Background(), "foo", []byte("bar"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found {
		t.Error("Get: Found = false, want true")
	}
	if got.Expired {
		t.Error("Get: Expired = true, want false")
	}
	if !bytes.Equal(got.Value, []byte("bar")) {
		t.Errorf("Get: Value = %q, want %q", got.Value, "bar")
	}
}

func TestPutBinaryValue(t *testing.T) {
	s := newTestStore(t)
	payload := []byte{0x00, 0x01, 0xFF, 0xFE, 0x80, 'a'}
	if err := s.Put(context.Background(), "bin", payload, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Value, payload) {
		t.Errorf("binary round-trip mismatch: got %v want %v", got.Value, payload)
	}
}

func TestPutUpsert(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)

	if err := s.Put(context.Background(), "k", []byte("v1"), time.Hour); err != nil {
		t.Fatal(err)
	}
	// Upsert later: value and expiry both replaced.
	s.now = fixed(now.Add(10 * time.Minute))
	if err := s.Put(context.Background(), "k", []byte("v2"), time.Hour); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Value, []byte("v2")) {
		t.Errorf("after upsert Value = %q, want v2", got.Value)
	}

	rows, err := s.RowCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1 after upsert", rows)
	}
}

func TestGetMissingKey(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Found {
		t.Error("Get missing: Found = true, want false")
	}
}

func TestGetExpiredLazyDeletes(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	if err := s.Put(context.Background(), "k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}

	s.now = fixed(now.Add(2 * time.Hour))
	got, err := s.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found {
		t.Error("Get expired: Found = false, want true (row existed)")
	}
	if !got.Expired {
		t.Error("Get expired: Expired = false, want true")
	}

	rows, err := s.RowCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("RowCount after lazy delete = %d, want 0", rows)
	}
}

func TestDeleteExpiredSweep(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	// A later write stays valid past the sweep time.
	s.now = fixed(now.Add(30 * time.Minute))
	if err := s.Put(ctx, "k3", []byte("v"), 2*time.Hour); err != nil {
		t.Fatal(err)
	}

	s.now = fixed(now.Add(2 * time.Hour))
	deleted, err := s.DeleteExpired(ctx, 0)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteExpired deleted = %d, want 2", deleted)
	}

	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount after sweep = %d, want 1", rows)
	}

	// Running again deletes nothing.
	deleted, err = s.DeleteExpired(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("second sweep deleted = %d, want 0", deleted)
	}
}

func TestDeleteExpiredNoopWhenNothingExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), 10*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	// Advance time by 1h — well within the 10h TTL, so nothing expires.
	s.now = fixed(now.Add(1 * time.Hour))
	deleted, err := s.DeleteExpired(ctx, 0)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 0 {
		t.Errorf("DeleteExpired deleted = %d, want 0 (probe should short-circuit)", deleted)
	}

	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("RowCount = %d, want 3 (no rows should be removed)", rows)
	}
}

func TestDeleteExpiredEmptyTable(t *testing.T) {
	s := newTestStore(t)
	s.now = fixed(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	deleted, err := s.DeleteExpired(context.Background(), 0)
	if err != nil {
		t.Fatalf("DeleteExpired on empty table: %v", err)
	}
	if deleted != 0 {
		t.Errorf("DeleteExpired on empty table deleted = %d, want 0", deleted)
	}
}

func TestDeleteExpiredProbeErrorWrapped(t *testing.T) {
	s := newTestStore(t)
	_ = s.Close()

	_, err := s.DeleteExpired(context.Background(), 0)
	if err == nil {
		t.Fatal("DeleteExpired on closed store: nil error, want error")
	}
}

func TestDeleteExpiredRespectsSweepLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	// Insert 5 rows all set to expire in 1h.
	for i := 1; i <= 5; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	// Advance past expiry.
	s.now = fixed(now.Add(2 * time.Hour))

	// Sweep with limit=2 should delete only the 2 oldest.
	deleted, err := s.DeleteExpired(ctx, 2)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteExpired deleted = %d, want 2", deleted)
	}

	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("RowCount = %d, want 3", rows)
	}
}

func TestDeleteExpiredSweepLimitZeroDeletesAll(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	s.now = fixed(now.Add(2 * time.Hour))

	// limit=0 means unlimited — all 5 expired rows should be deleted.
	deleted, err := s.DeleteExpired(ctx, 0)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 5 {
		t.Errorf("DeleteExpired deleted = %d, want 5", deleted)
	}

	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("RowCount = %d, want 0", rows)
	}
}

func TestEvictOldestOrderingBoundedNeverEmpty(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	// 5 rows whose expiry increases with suffix: k1 oldest ... k5 newest.
	for i := 1; i <= 5; i++ {
		key := "k" + string(rune('0'+i))
		ttl := time.Duration(i) * time.Hour
		if err := s.Put(ctx, key, []byte("v"), ttl); err != nil {
			t.Fatal(err)
		}
	}

	// Fake size always over the limit so the loop runs on stop conditions.
	overLimit := func() (int64, error) { return 999999, nil }
	deleted, err := s.evictOldest(ctx, 100, 2, 10, overLimit)
	if err != nil {
		t.Fatalf("evictOldest: %v", err)
	}

	// Guard must never empty the table: 5 rows, batches of 2, leaves 1.
	if deleted != 4 {
		t.Errorf("evictOldest deleted = %d, want 4 (never empty)", deleted)
	}
	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1 (table never fully emptied)", rows)
	}

	// Oldest-expiring rows went first: only k5 remains.
	got, err := s.Get(ctx, "k5")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found {
		t.Error("k5 should remain (newest expiry)")
	}
	for i := 1; i <= 4; i++ {
		key := "k" + string(rune('0'+i))
		got, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got.Found {
			t.Errorf("%s should have been evicted first", key)
		}
	}
}

func TestEvictOldestStopsAtMaxRuns(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	for i := 1; i <= 10; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Duration(i)*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	overLimit := func() (int64, error) { return 999999, nil }
	deleted, err := s.evictOldest(ctx, 100, 2, 2, overLimit)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Errorf("evictOldest deleted = %d, want 4 (2 batches of 2)", deleted)
	}
	rows, _ := s.RowCount(ctx)
	if rows != 6 {
		t.Errorf("RowCount = %d, want 6 after maxRuns cap", rows)
	}
}

func TestEvictOldestStopsWhenBatchAffectsZeroRows(t *testing.T) {
	s := newTestStore(t)
	overLimit := func() (int64, error) { return 999999, nil }
	deleted, err := s.evictOldest(context.Background(), 100, 100, 10, overLimit)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("evictOldest on empty table deleted = %d, want 0", deleted)
	}
}

func TestEvictOldestStopsWhenSizeBelowLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Duration(i)*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	// Size is over the limit on the first probe, then under — the loop must
	// stop after the first batch.
	calls := 0
	size := func() (int64, error) {
		calls++
		if calls == 1 {
			return 999999, nil
		}
		return 1, nil
	}
	deleted, err := s.evictOldest(ctx, 100, 1, 10, size)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("evictOldest deleted = %d, want 1 (size below limit after first batch)", deleted)
	}
}

func TestEvictOldestSingleRowNeverEmptied(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()
	if err := s.Put(ctx, "solo", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	overLimit := func() (int64, error) { return 999999, nil }
	deleted, err := s.evictOldest(ctx, 100, 100, 10, overLimit)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("single row eviction deleted = %d, want 0", deleted)
	}
	rows, _ := s.RowCount(ctx)
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1", rows)
	}
}

func TestSizeAndRowCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)

	before, err := s.Size(ctx)
	if err != nil {
		t.Fatalf("Size before: %v", err)
	}
	if before <= 0 {
		t.Errorf("Size before = %d, want > 0", before)
	}

	for i := 0; i < 10; i++ {
		if err := s.Put(ctx, "key", []byte("a value that takes space"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	after, err := s.Size(ctx)
	if err != nil {
		t.Fatalf("Size after: %v", err)
	}
	if after < before {
		t.Errorf("Size after = %d, want >= %d", after, before)
	}

	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1", rows)
	}
}

func TestPing(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestIncrementalVacuum(t *testing.T) {
	s := newTestStore(t)
	if err := s.IncrementalVacuum(context.Background(), 100); err != nil {
		t.Errorf("IncrementalVacuum: %v", err)
	}
}

func TestBackup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	if err := s.Put(ctx, "a", []byte("b"), time.Hour); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path, err := s.Backup(ctx, dir)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if path == "" {
		t.Fatal("Backup returned empty path")
	}

	// Backup file is a valid DB containing the row.
	b, err := Open(path, 0)
	if err != nil {
		t.Fatalf("opening backup: %v", err)
	}
	defer func() { _ = b.Close() }()
	got, err := b.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || string(got.Value) != "b" {
		t.Errorf("backup Get = %+v, want found b", got)
	}
}

func TestBackupRejectsEmptyDir(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Backup(context.Background(), ""); err == nil {
		t.Error("Backup with empty dir: nil error, want error")
	}
}

// --- in-memory layer (spec §4: memory-first reads, SQLite persistence) ---

func TestCachedPutGetRoundTrip(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)

	if err := s.Put(context.Background(), "foo", []byte("bar"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found || got.Expired || !bytes.Equal(got.Value, []byte("bar")) {
		t.Errorf("Get = %+v, want found bar", got)
	}
	if s.CacheBytes() != entrySize("foo", entry{value: []byte("bar")}) {
		t.Errorf("CacheBytes = %d, want %d", s.CacheBytes(), entrySize("foo", entry{value: []byte("bar")}))
	}
}

func TestCachedGetServesFromMemoryOnly(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	ctx := context.Background()
	if err := s.Put(ctx, "k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	// Remove the persisted row behind the cache's back: memory is
	// authoritative while the process runs, so the read must still succeed.
	if _, err := s.db.Exec(`DELETE FROM kv_store WHERE key = 'k'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found || string(got.Value) != "v" {
		t.Errorf("Get = %+v, want found v served from memory", got)
	}
}

func TestCachedMissingKey(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	got, err := s.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Found {
		t.Error("Get missing: Found = true, want false")
	}
}

func TestCachedRestartPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kvp.db")
	ctx := context.Background()

	s1, err := Open(path, 1<<20)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s1.Put(ctx, "k", []byte("persisted"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !got.Found || !bytes.Equal(got.Value, []byte("persisted")) {
		t.Errorf("Get after restart = %+v, want found persisted", got)
	}
}

func TestCachedStartupLoadSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kvp.db")
	ctx := context.Background()

	s1, err := Open(path, 1<<20)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s1.Put(ctx, "dead", []byte("x"), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if err := s1.Put(ctx, "alive", []byte("y"), time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // let "dead" lapse
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.Get(ctx, "alive")
	if err != nil || !got.Found || string(got.Value) != "y" {
		t.Errorf("Get alive = %+v err %v, want found y", got, err)
	}
	got, err = s2.Get(ctx, "dead")
	if err != nil {
		t.Fatalf("Get dead: %v", err)
	}
	if got.Found {
		t.Error("expired row was loaded into the memory layer")
	}
}

func TestCachedGetExpiredEvictsBothLayers(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	s.now = fixed(now.Add(2 * time.Hour))

	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found || !got.Expired {
		t.Errorf("Get expired = %+v, want found+expired", got)
	}
	if s.CacheBytes() != 0 {
		t.Errorf("CacheBytes after lazy expire = %d, want 0", s.CacheBytes())
	}

	// The persisted row is removed asynchronously; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := s.RowCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rows == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lazy DB delete did not run; RowCount = %d", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCachedConcurrentPutRefreshBeatsLazyExpire(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("old"), time.Hour); err != nil {
		t.Fatal(err)
	}
	s.now = fixed(now.Add(2 * time.Hour))
	expired, err := s.Get(ctx, "k")
	if err != nil || !expired.Expired {
		t.Fatalf("Get expired = %+v err %v, want expired", expired, err)
	}

	// Refresh right after the lazy expire observed the stale entry: the
	// refreshed row must survive in both layers.
	if err := s.Put(ctx, "k", []byte("new"), time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the async stale delete fire

	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Found || string(got.Value) != "new" {
		t.Errorf("Get after refresh = %+v, want found new", got)
	}
	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("RowCount after refresh = %d, want 1 (refreshed row survived)", rows)
	}
}

func TestCachedDeleteExpiredPurgesMemory(t *testing.T) {
	s := newCachedTestStore(t, 1<<20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	if err := s.Put(ctx, "a", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "b", []byte("v"), 3*time.Hour); err != nil {
		t.Fatal(err)
	}

	s.now = fixed(now.Add(2 * time.Hour))
	deleted, err := s.DeleteExpired(ctx, 0)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteExpired deleted = %d, want 1", deleted)
	}
	if s.CacheBytes() == 0 {
		t.Error("memory layer emptied entirely; only 'a' should have been purged")
	}
	got, err := s.Get(ctx, "b")
	if err != nil || !got.Found {
		t.Errorf("Get b = %+v err %v, want found", got, err)
	}
	got, err = s.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Error("expired 'a' still resident in memory after sweep")
	}
}

func TestCachedEvictOldestTrimsToBudget(t *testing.T) {
	s := newCachedTestStore(t, 300) // fits ~3 entries of 99 bytes
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		key := "k" + string(rune('0'+i))
		if err := s.Put(ctx, key, []byte("v"), time.Duration(i)*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := s.EvictOldest(ctx, 300, 2, 10)
	if err != nil {
		t.Fatalf("EvictOldest: %v", err)
	}
	if deleted != 2 {
		t.Errorf("EvictOldest deleted = %d, want 2", deleted)
	}
	if usage, err := s.Usage(ctx); err != nil || usage >= 300 {
		t.Errorf("Usage = %d err %v, want < 300 (cache budget)", usage, err)
	}

	// Oldest-expiring went first, from both layers.
	for _, key := range []string{"k1", "k2"} {
		got, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got.Found {
			t.Errorf("%s should have been evicted from memory", key)
		}
	}
	for _, key := range []string{"k3", "k4", "k5"} {
		got, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Found {
			t.Errorf("%s should have survived eviction", key)
		}
	}
	rows, err := s.RowCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("RowCount = %d, want 3 (both layers trimmed identically)", rows)
	}
}

func TestSQLiteOnlyModeHasNoMemoryLayer(t *testing.T) {
	s := newTestStore(t) // memLimit 0
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = fixed(now)
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if s.CacheBytes() != 0 {
		t.Errorf("CacheBytes = %d, want 0 in SQLite-only mode", s.CacheBytes())
	}
	usage, err := s.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	size, err := s.Size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage != size {
		t.Errorf("Usage = %d, want on-disk size %d in SQLite-only mode", usage, size)
	}
	got, err := s.Get(ctx, "k")
	if err != nil || !got.Found || string(got.Value) != "v" {
		t.Errorf("Get = %+v err %v, want found v", got, err)
	}
}

func TestRetainBackups(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("kvp-20260101T000000.000Z.db")
	write("kvp-20260102T000000.000Z.db")
	write("kvp-20260103T000000.000Z.db")
	write("notes.txt")   // not a backup: must survive
	write("kvp-old.bak") // wrong extension: must survive

	deleted, err := st.RetainBackups(dir, 2)
	if err != nil {
		t.Fatalf("RetainBackups: %v", err)
	}
	if deleted != 1 {
		t.Errorf("RetainBackups deleted = %d, want 1", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "kvp-20260101T000000.000Z.db")); !os.IsNotExist(err) {
		t.Error("oldest backup still exists after retention")
	}
	for _, name := range []string{
		"kvp-20260102T000000.000Z.db",
		"kvp-20260103T000000.000Z.db",
		"notes.txt",
		"kvp-old.bak",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have been retained: %v", name, err)
		}
	}

	// Retention larger than the file count is a no-op.
	deleted, err = st.RetainBackups(dir, 10)
	if err != nil {
		t.Fatalf("RetainBackups(10): %v", err)
	}
	if deleted != 0 {
		t.Errorf("RetainBackups(10) deleted = %d, want 0", deleted)
	}
}

func TestRetainBackupsMissingDirErrors(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RetainBackups(filepath.Join(t.TempDir(), "nope"), 2); err == nil {
		t.Error("RetainBackups on missing dir: nil error, want error")
	}
}
