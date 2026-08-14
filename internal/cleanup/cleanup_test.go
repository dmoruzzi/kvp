package cleanup

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dmoruzzi/decikvp/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kvp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type runEvent struct {
	kind, result string
}

type recording struct {
	mu      sync.Mutex
	runs    []runEvent
	deleted map[string]int64
}

func newRecording() *recording { return &recording{deleted: map[string]int64{}} }

func (r *recording) CleanupRun(kind, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runEvent{kind, result})
}

func (r *recording) CleanupDeleted(kind string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted[kind] += n
}

func (r *recording) had(kind, result string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.runs {
		if e.kind == kind && e.result == result {
			return true
		}
	}
	return false
}

type stubStore struct{}

func (stubStore) DeleteExpired(context.Context) (int64, error)  { return 0, nil }
func (stubStore) EvictOldest(context.Context, int64, int, int) (int64, error) {
	return 0, nil
}
func (stubStore) Size(context.Context) (int64, error)     { return 0, nil }
func (stubStore) IncrementalVacuum(context.Context, int) error { return nil }
func (stubStore) Backup(context.Context, string) (string, error) { return "", nil }
func (stubStore) RetainBackups(string, int) (int, error)  { return 0, nil }

func newTestEvictor(evict func(context.Context) (int64, error), size func(context.Context) (int64, error)) *Evictor {
	return &Evictor{
		st:       stubStore{},
		limit:    1000,
		evict:    evict,
		size:     size,
		throttle: time.Minute,
		now:      time.Now,
		logger:   discardLogger(),
		metrics:  noopMetrics{},
	}
}

func TestExpiryRunOnce(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })

	// Two rows expire at now+1h; one is refreshed to now+3h.
	st.Put(ctx, "a", []byte("v"), time.Hour)
	st.Put(ctx, "b", []byte("v"), time.Hour)
	st.SetClock(func() time.Time { return now.Add(30 * time.Minute) })
	st.Put(ctx, "c", []byte("v"), 3*time.Hour)

	st.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
	rec := newRecording()
	r := NewExpiryRunner(st, time.Hour, discardLogger(), rec)

	deleted, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 2 {
		t.Errorf("RunOnce deleted = %d, want 2", deleted)
	}
	if !rec.had("expiry", "ok") {
		t.Error("missing CleanupRun(expiry, ok)")
	}
	if rec.deleted["expiry"] != 2 {
		t.Errorf("CleanupDeleted(expiry) = %d, want 2", rec.deleted["expiry"])
	}
	rows, _ := st.RowCount(ctx)
	if rows != 1 {
		t.Errorf("RowCount = %d, want 1", rows)
	}
}

func TestExpiryRunOnceErrorRecorded(t *testing.T) {
	st := newStore(t)
	st.Close()
	rec := newRecording()
	r := NewExpiryRunner(st, time.Hour, discardLogger(), rec)
	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Error("RunOnce on closed store: nil error, want error")
	}
	if !rec.had("expiry", "error") {
		t.Error("missing CleanupRun(expiry, error)")
	}
}

func TestEvictorSkipsWhenUnderLimit(t *testing.T) {
	evictions := 0
	e := newTestEvictor(func(context.Context) (int64, error) { evictions++; return 0, nil }, func(context.Context) (int64, error) {
		return 100, nil // under limit
	})
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if evictions != 0 {
		t.Errorf("evict called %d times, want 0 (size under limit)", evictions)
	}
}

func TestEvictorRunOnceRecordsDeleted(t *testing.T) {
	rec := newRecording()
	e := newTestEvictor(func(context.Context) (int64, error) { return 7, nil }, func(context.Context) (int64, error) {
		return 999999, nil // over limit
	})
	e.metrics = rec
	deleted, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 7 {
		t.Errorf("deleted = %d, want 7", deleted)
	}
	if !rec.had("size", "ok") {
		t.Error("missing CleanupRun(size, ok)")
	}
	if rec.deleted["size"] != 7 {
		t.Errorf("CleanupDeleted(size) = %d, want 7", rec.deleted["size"])
	}
}

func TestEvictorErrorRecorded(t *testing.T) {
	rec := newRecording()
	e := newTestEvictor(func(context.Context) (int64, error) { return 0, errBoom }, func(context.Context) (int64, error) {
		return 999999, nil
	})
	e.metrics = rec
	if _, err := e.RunOnce(context.Background()); err == nil {
		t.Error("RunOnce: nil error, want error")
	}
	if !rec.had("size", "error") {
		t.Error("missing CleanupRun(size, error)")
	}
}

func TestEvictorThrottle(t *testing.T) {
	evictions := 0
	e := newTestEvictor(func(context.Context) (int64, error) { evictions++; return 0, nil }, func(context.Context) (int64, error) {
		return 999999, nil
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	if _, err := e.MaybeEvict(context.Background()); err != nil {
		t.Fatalf("MaybeEvict 1: %v", err)
	}
	if _, err := e.MaybeEvict(context.Background()); err != nil {
		t.Fatalf("MaybeEvict 2 (throttled): %v", err)
	}
	if evictions != 1 {
		t.Errorf("evictions after throttled second call = %d, want 1", evictions)
	}

	now = now.Add(2 * time.Minute)
	if _, err := e.MaybeEvict(context.Background()); err != nil {
		t.Fatalf("MaybeEvict 3: %v", err)
	}
	if evictions != 2 {
		t.Errorf("evictions after throttle elapsed = %d, want 2", evictions)
	}
}

func TestEvictorSingleflightGuard(t *testing.T) {
	evictions := 0
	var mu sync.Mutex
	e := newTestEvictor(func(ctx context.Context) (int64, error) {
		mu.Lock()
		evictions++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return 0, nil
	}, func(context.Context) (int64, error) { return 999999, nil })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.MaybeEvict(context.Background())
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if evictions != 1 {
		t.Errorf("concurrent MaybeEvict ran %d evictions, want 1", evictions)
	}
}

func TestBackupRunOnce(t *testing.T) {
	st := newStore(t)
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	st.Put(context.Background(), "a", []byte("b"), time.Hour)

	rec := newRecording()
	b := NewBackuper(st, dir, 24*time.Hour, 3, discardLogger(), rec)
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !rec.had("backup", "ok") {
		t.Error("missing CleanupRun(backup, ok)")
	}

	files, err := filepath.Glob(filepath.Join(dir, "kvp-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("backup files = %d, want 1", len(files))
	}
}

func TestBackupRetention(t *testing.T) {
	st := newStore(t)
	dir := t.TempDir()
	rec := newRecording()
	b := NewBackuper(st, dir, 24*time.Hour, 2, discardLogger(), rec)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		st.SetClock(func() time.Time { return now.Add(time.Duration(i) * time.Hour) })
		if err := b.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "kvp-*.db"))
	if len(files) != 2 {
		t.Errorf("backup files after retention = %d, want 2", len(files))
	}
}

func TestBackupErrorRecorded(t *testing.T) {
	st := newStore(t)
	rec := newRecording()
	b := NewBackuper(st, "", 24*time.Hour, 2, discardLogger(), rec)
	if err := b.RunOnce(context.Background()); err == nil {
		t.Error("RunOnce with empty dir: nil error, want error")
	}
	if !rec.had("backup", "error") {
		t.Error("missing CleanupRun(backup, error)")
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

