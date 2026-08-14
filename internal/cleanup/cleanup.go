// Package cleanup runs the background maintenance jobs from the spec §8:
// the expiry sweep, the throttled size-based eviction, and the vacuum/backup
// job. Every job runs in its own goroutine under a shared cancellable context
// and never fails silently — errors are logged and metered.
package cleanup

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Metrics is the telemetry surface jobs emit through; implemented by
// internal/telemetry.
type Metrics interface {
	CleanupRun(kind, result string)
	CleanupDeleted(kind string, n int64)
}

type noopMetrics struct{}

func (noopMetrics) CleanupRun(string, string)      {}
func (noopMetrics) CleanupDeleted(string, int64)   {}

// Store is the persistence surface the jobs need. *store.Store satisfies it.
type Store interface {
	DeleteExpired(ctx context.Context) (int64, error)
	EvictOldest(ctx context.Context, limit int64, batchSize, maxRuns int) (int64, error)
	Size(ctx context.Context) (int64, error)
	IncrementalVacuum(ctx context.Context, n int) error
	Backup(ctx context.Context, dir string) (string, error)
	RetainBackups(dir string, n int) (int, error)
}

// ExpiryRunner deletes expired rows every interval (§8.1).
type ExpiryRunner struct {
	st       Store
	interval time.Duration
	logger   *slog.Logger
	metrics  Metrics
}

func NewExpiryRunner(st Store, interval time.Duration, logger *slog.Logger, m Metrics) *ExpiryRunner {
	if logger == nil {
		logger = slog.Default()
	}
	if m == nil {
		m = noopMetrics{}
	}
	return &ExpiryRunner{st: st, interval: interval, logger: logger, metrics: m}
}

// RunOnce performs a single sweep and returns the deleted count.
func (r *ExpiryRunner) RunOnce(ctx context.Context) (int64, error) {
	start := time.Now()
	n, err := r.st.DeleteExpired(ctx)
	if err != nil {
		r.metrics.CleanupRun("expiry", "error")
		r.logger.Error("expiry sweep failed", "error", err)
		return n, err
	}
	r.metrics.CleanupRun("expiry", "ok")
	r.metrics.CleanupDeleted("expiry", n)
	r.logger.Info("expiry sweep", "deleted", n, "duration_ms", time.Since(start).Milliseconds())
	return n, nil
}

// Run loops the sweep until the context is cancelled, running once immediately.
func (r *ExpiryRunner) Run(ctx context.Context) {
	r.RunOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// Evictor enforces the DB size budget (§8.2). Runs are throttled to at most one
// per throttle window and guarded against concurrent overlap (singleflight).
type Evictor struct {
	st         Store
	limit      int64
	batchSize  int
	maxRuns    int
	throttle   time.Duration
	logger     *slog.Logger
	metrics    Metrics
	evict      func(ctx context.Context) (int64, error)
	size       func(ctx context.Context) (int64, error)
	now        func() time.Time

	mu      sync.Mutex
	running bool
	lastRun time.Time
}

func NewEvictor(st Store, limit int64, batchSize, maxRuns int, throttle time.Duration, logger *slog.Logger, m Metrics) *Evictor {
	if logger == nil {
		logger = slog.Default()
	}
	if m == nil {
		m = noopMetrics{}
	}
	e := &Evictor{
		st: st, limit: limit, batchSize: batchSize, maxRuns: maxRuns,
		throttle: throttle, logger: logger, metrics: m, now: time.Now,
	}
	e.evict = func(ctx context.Context) (int64, error) {
		return st.EvictOldest(ctx, limit, batchSize, maxRuns)
	}
	e.size = func(ctx context.Context) (int64, error) { return st.Size(ctx) }
	return e
}

// RunOnce checks the size budget and runs one eviction pass if over it,
// followed by an incremental vacuum (spec §8.4).
func (e *Evictor) RunOnce(ctx context.Context) (int64, error) {
	sz, err := e.size(ctx)
	if err != nil {
		e.metrics.CleanupRun("size", "error")
		e.logger.Error("size check failed", "error", err)
		return 0, err
	}
	if sz < e.limit {
		return 0, nil
	}
	start := time.Now()
	n, err := e.evict(ctx)
	if err != nil {
		e.metrics.CleanupRun("size", "error")
		e.logger.Error("size eviction failed", "error", err)
		return n, err
	}
	if n > 0 {
		if vacErr := e.st.IncrementalVacuum(ctx, e.batchSize); vacErr != nil {
			e.logger.Error("incremental vacuum failed", "error", vacErr)
		}
	}
	e.metrics.CleanupRun("size", "ok")
	e.metrics.CleanupDeleted("size", n)
	e.logger.Info("size eviction", "deleted", n, "db_bytes", sz, "duration_ms", time.Since(start).Milliseconds())
	return n, nil
}

// MaybeEvict runs a single eviction pass subject to the throttle and the
// singleflight guard. It is safe for concurrent callers (background loop and
// the POST trigger).
func (e *Evictor) MaybeEvict(ctx context.Context) (int64, error) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return 0, nil
	}
	if e.now().Sub(e.lastRun) < e.throttle {
		e.mu.Unlock()
		return 0, nil
	}
	e.running = true
	e.lastRun = e.now()
	e.mu.Unlock()

	n, err := e.RunOnce(ctx)

	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
	return n, err
}

// RunLoop ticks the eviction check every throttle window for the background job.
func (e *Evictor) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(e.throttle)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.MaybeEvict(ctx)
		}
	}
}

// Backuper writes VACUUM INTO snapshots and enforces retention (§8.4).
type Backuper struct {
	st        Store
	dir       string
	interval  time.Duration
	retention int
	logger    *slog.Logger
	metrics   Metrics
}

func NewBackuper(st Store, dir string, interval time.Duration, retention int, logger *slog.Logger, m Metrics) *Backuper {
	if logger == nil {
		logger = slog.Default()
	}
	if m == nil {
		m = noopMetrics{}
	}
	return &Backuper{st: st, dir: dir, interval: interval, retention: retention, logger: logger, metrics: m}
}

// RunOnce writes one backup and prunes old files to the retention limit.
func (b *Backuper) RunOnce(ctx context.Context) error {
	start := time.Now()
	path, err := b.st.Backup(ctx, b.dir)
	if err != nil {
		b.metrics.CleanupRun("backup", "error")
		b.logger.Error("backup failed", "error", err)
		return err
	}
	if _, err := b.st.RetainBackups(b.dir, b.retention); err != nil {
		b.metrics.CleanupRun("backup", "error")
		b.logger.Error("backup retention failed", "error", err)
		return err
	}
	b.metrics.CleanupRun("backup", "ok")
	b.logger.Info("backup completed", "path", path, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// Run loops backups every interval; no-op when the backup dir is unset.
func (b *Backuper) Run(ctx context.Context) {
	if b.dir == "" {
		b.logger.Info("backup job disabled (KVP_BACKUP_DIR unset)")
		return
	}
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.RunOnce(ctx)
		}
	}
}
