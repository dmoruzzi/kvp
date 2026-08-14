package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kvp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
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
	deleted, err := s.DeleteExpired(ctx)
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
	deleted, err = s.DeleteExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("second sweep deleted = %d, want 0", deleted)
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
	b, err := Open(path)
	if err != nil {
		t.Fatalf("opening backup: %v", err)
	}
	defer b.Close()
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
