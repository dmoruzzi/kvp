package cleanup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmoruzzi/kvp/internal/store"
)

func newCachedStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kvp.db"), 1<<20)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestFlusherRunOncePersistsDirtyKeys(t *testing.T) {
	st := newCachedStore(t)
	ctx := context.Background()
	if err := st.PutWithMode(ctx, "a", []byte("va"), time.Hour, store.PutAsync); err != nil {
		t.Fatalf("PutWithMode: %v", err)
	}
	if err := st.PutWithMode(ctx, "b", []byte("vb"), time.Hour, store.PutAsync); err != nil {
		t.Fatalf("PutWithMode: %v", err)
	}

	rec := newRecording()
	f := NewFlusher(st, time.Second, discardLogger(), rec)
	n, err := f.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("RunOnce persisted = %d, want 2", n)
	}
	if st.DirtyCount() != 0 {
		t.Errorf("DirtyCount after flush = %d, want 0", st.DirtyCount())
	}
	if !rec.had("flush", "ok") {
		t.Error("flush run not metered as ok")
	}

	// A run with no dirty keys is a no-op: no extra metered sample.
	rec2 := newRecording()
	f2 := NewFlusher(st, time.Second, discardLogger(), rec2)
	if n, err := f2.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunOnce (empty) = %d, %v; want 0, nil", n, err)
	}
	if rec2.had("flush", "ok") {
		t.Error("no-op flush run should not be metered")
	}
}

func TestFlusherRunOnceErrorMetered(t *testing.T) {
	// Drive RunOnce against a stub whose Flush always fails.
	rec := newRecording()
	f := NewFlusher(failingFlushStore{}, time.Second, discardLogger(), rec)
	if _, err := f.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce: nil error, want error")
	}
	if !rec.had("flush", "error") {
		t.Error("failed flush run not metered as error")
	}
}

type failingFlushStore struct{ stubStore }

func (failingFlushStore) Flush(context.Context) (int64, error) { return 0, errFlushBoom }

type flushErr struct{}

func (flushErr) Error() string { return "flush boom" }

var errFlushBoom = flushErr{}
