// Package store implements SQLite persistence for the KVP store: schema
// migration, CRUD, TTL expiry, oldest-first size eviction, incremental vacuum
// and VACUUM INTO backups.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	journalMode   = "WAL"
	synchronous   = "NORMAL"
	busyTimeoutMS = 5000

	// entryOverhead approximates per-entry memory cost beyond key+value bytes
	// (map slot, slice header, time.Time).
	entryOverhead = 96

	// maxDeleteParams bounds the placeholders per DELETE IN statement
	// (SQLite's default host-parameter limit is 999).
	maxDeleteParams = 500
)

// entry is a value resident in the memory layer.
type entry struct {
	value     []byte
	expiresAt time.Time
}

func entrySize(key string, e entry) int64 {
	return int64(len(key) + len(e.value) + entryOverhead)
}

// Store wraps the SQLite database and, when enabled, an in-memory layer that
// serves reads (spec §4): memory is authoritative while the process runs and
// SQLite provides durability across restarts. Every write goes to SQLite first
// and is mirrored into memory; evictions and expiry sweeps remove from both so
// the two stay consistent. A memLimit of 0 disables the memory layer entirely
// (SQLite-only mode).
type Store struct {
	db  *sql.DB
	now func() time.Time

	mu         sync.RWMutex
	cache      map[string]entry
	cacheBytes int64
	memLimit   int64
}

// GetResult is the outcome of a key lookup.
type GetResult struct {
	Found   bool
	Expired bool
	Value   []byte
}

// Open opens (creating if needed) the SQLite database at path, applies the
// schema migration and the required PRAGMAs from the spec §4. When memLimit is
// positive, the in-memory layer is enabled and preloaded with all non-expired
// rows; a memLimit of 0 keeps every read on SQLite.
func Open(path string, memLimit int64) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty db path")
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// database/sql does not retry failed connections; configure limits.
	db.SetMaxOpenConns(1) // WAL serializes writes anyway; single writer avoids SQLITE_BUSY storms
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, now: time.Now, memLimit: memLimit}
	if memLimit > 0 {
		s.cache = make(map[string]entry)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if memLimit > 0 {
		if err := s.loadCache(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// loadCache populates the memory layer from persisted rows. Rows already past
// their expiry are skipped and left for the sweep/lazy delete to remove.
func (s *Store) loadCache() error {
	rows, err := s.db.Query(`SELECT key, value, expires_at FROM kv_store`)
	if err != nil {
		return fmt.Errorf("store: load cache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := s.now()
	for rows.Next() {
		var key, expiresStr string
		var value []byte
		if err := rows.Scan(&key, &value, &expiresStr); err != nil {
			return fmt.Errorf("store: load cache scan: %w", err)
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, expiresStr)
		if err != nil {
			return fmt.Errorf("store: load cache %q: parse expires_at %q: %w", key, expiresStr, err)
		}
		if now.After(expiresAt) {
			continue
		}
		e := entry{value: value, expiresAt: expiresAt}
		s.cache[key] = e
		s.cacheBytes += entrySize(key, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: load cache: %w", err)
	}
	return nil
}

func (s *Store) migrate() error {
	pragmas := []string{
		"PRAGMA journal_mode=" + journalMode,
		fmt.Sprintf("PRAGMA synchronous=%s", synchronous),
		"PRAGMA auto_vacuum=INCREMENTAL",
		fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS),
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("store: apply %s: %w", p, err)
		}
	}
	const schema = `
CREATE TABLE IF NOT EXISTS kv_store (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kv_store_expires_at ON kv_store(expires_at);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// SetClock overrides the time source used for TTL computation and lazy expiry
// (test/clock-injection hook; production uses time.Now).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Put upserts value under key, setting expiry to now + ttl. The write hits
// SQLite first; only on success is the value mirrored into the memory layer.
func (s *Store) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	expiresAt := s.now().Add(ttl)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO kv_store (key, value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, expires_at=excluded.expires_at`,
		key, value, expiresAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: put %q: %w", key, err)
	}
	if s.caching() {
		s.cachePut(key, value, expiresAt)
	}
	return nil
}

func (s *Store) cachePut(key string, value []byte, expiresAt time.Time) {
	e := entry{value: value, expiresAt: expiresAt}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.cache[key]; ok {
		s.cacheBytes -= entrySize(key, old)
	}
	s.cache[key] = e
	s.cacheBytes += entrySize(key, e)
}

// Get retrieves value for key. With the memory layer enabled the lookup never
// touches SQLite — memory is authoritative (misses are misses); a row past its
// expiry is removed from both layers on the spot and reported as Expired.
// Without the memory layer it reads SQLite directly.
func (s *Store) Get(ctx context.Context, key string) (GetResult, error) {
	if s.caching() {
		return s.getCached(ctx, key)
	}
	return s.getDB(ctx, key)
}

func (s *Store) getCached(ctx context.Context, key string) (GetResult, error) {
	var res GetResult

	s.mu.RLock()
	e, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return res, nil
	}

	res.Found = true
	if !s.now().After(e.expiresAt) {
		res.Value = e.value
		return res, nil
	}

	// Expired: drop from memory unless a concurrent Put refreshed the entry,
	// then delete the matching (stale) row from SQLite in the background.
	s.mu.Lock()
	if cur, ok := s.cache[key]; ok && cur.expiresAt.Equal(e.expiresAt) {
		s.cacheBytes -= entrySize(key, cur)
		delete(s.cache, key)
	}
	s.mu.Unlock()

	expiresStr := e.expiresAt.Format(time.RFC3339Nano)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Best-effort: guarded by expires_at so a concurrent refresh survives;
		// the expiry sweep removes any straggler.
		_, _ = s.db.ExecContext(ctx,
			`DELETE FROM kv_store WHERE key = ? AND expires_at = ?`, key, expiresStr)
	}()

	res.Expired = true
	return res, nil
}

func (s *Store) getDB(ctx context.Context, key string) (GetResult, error) {
	var res GetResult
	row := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at FROM kv_store WHERE key = ?`, key)

	var expiresStr string
	err := row.Scan(&res.Value, &expiresStr)
	if errors.Is(err, sql.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("store: get %q: %w", key, err)
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, expiresStr)
	if err != nil {
		return res, fmt.Errorf("store: get %q: parse expires_at %q: %w", key, expiresStr, err)
	}

	res.Found = true
	if s.now().After(expiresAt) {
		res.Expired = true
		if _, err := s.db.ExecContext(ctx, `DELETE FROM kv_store WHERE key = ?`, key); err != nil {
			return res, fmt.Errorf("store: lazy delete %q: %w", key, err)
		}
	}
	return res, nil
}

// DeleteExpired removes rows past their expiry — from the memory layer and
// SQLite — and returns the deleted count. When limit > 0 at most limit rows
// are deleted per call; when limit <= 0 all expired rows are deleted in a
// single statement. The memory layer always purges every expired entry: it is
// authoritative for reads, and any DB rows left behind by the limit are
// unreachable and removed on a later sweep.
func (s *Store) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	now := s.now()
	nowStr := now.Format(time.RFC3339Nano)

	if s.caching() {
		s.mu.Lock()
		for key, e := range s.cache {
			if now.After(e.expiresAt) {
				s.cacheBytes -= entrySize(key, e)
				delete(s.cache, key)
			}
		}
		s.mu.Unlock()
	}

	var probe int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM kv_store WHERE expires_at < ? LIMIT 1`, nowStr).Scan(&probe)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: probe expired: %w", err)
	}

	var res sql.Result
	if limit > 0 {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM kv_store WHERE key IN (
				SELECT key FROM kv_store WHERE expires_at < ? ORDER BY expires_at ASC LIMIT ?
			)`, nowStr, limit)
	} else {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM kv_store WHERE expires_at < ?`, nowStr)
	}
	if err != nil {
		return 0, fmt.Errorf("store: delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// EvictOldest runs the size-based eviction described in the spec §8.2. With the
// memory layer enabled the budget applies to cache bytes; otherwise to the
// real on-disk size. See evictOldest for the algorithm.
func (s *Store) EvictOldest(ctx context.Context, limit int64, batchSize, maxRuns int) (int64, error) {
	if s.caching() {
		return s.evictOldest(ctx, limit, batchSize, maxRuns, func() (int64, error) {
			return s.CacheBytes(), nil
		})
	}
	return s.evictOldest(ctx, limit, batchSize, maxRuns, func() (int64, error) {
		return s.Size(ctx)
	})
}

// evictOldest deletes oldest-expiring rows in bounded batches until the size
// function reports a value below limit, a batch affects 0 rows, maxRuns batches
// have run, or deleting a batch would empty the table (the table is never fully
// emptied — regression guard for v1 bug #1).
func (s *Store) evictOldest(ctx context.Context, limit int64, batchSize, maxRuns int, size func() (int64, error)) (int64, error) {
	if batchSize <= 0 || maxRuns <= 0 {
		return 0, errors.New("store: evict: batchSize and maxRuns must be positive")
	}
	var deleted int64
	for run := 0; run < maxRuns; run++ {
		sz, err := size()
		if err != nil {
			return deleted, fmt.Errorf("store: evict: size: %w", err)
		}
		if sz < limit {
			return deleted, nil
		}

		// Never fully empty the table.
		total, err := s.RowCount(ctx)
		if err != nil {
			return deleted, err
		}
		batch := batchSize
		if total <= int64(batchSize) {
			batch = int(total) - 1
		}
		if batch <= 0 {
			return deleted, nil
		}

		var n int64
		if s.caching() {
			n, err = s.deleteOldestBatchCached(ctx, batch)
		} else {
			var res sql.Result
			res, err = s.db.ExecContext(ctx, `
DELETE FROM kv_store
WHERE key IN (SELECT key FROM kv_store ORDER BY expires_at ASC LIMIT ?)`, batch)
			if err == nil {
				n, err = res.RowsAffected()
			}
		}
		if err != nil {
			return deleted, fmt.Errorf("store: evict batch: %w", err)
		}
		if n == 0 {
			return deleted, nil
		}
		deleted += n
	}
	return deleted, nil
}

// deleteOldestBatchCached evicts one oldest-expiring batch from both layers:
// it selects the victim keys, deletes exactly those rows from SQLite and drops
// them from memory. Selecting the keys first keeps the two layers in lockstep.
func (s *Store) deleteOldestBatchCached(ctx context.Context, batch int) (int64, error) {
	keys := make([]string, 0, batch)
	rows, err := s.db.QueryContext(ctx,
		`SELECT key FROM kv_store ORDER BY expires_at ASC LIMIT ?`, batch)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}

	var deleted int64
	for start := 0; start < len(keys); start += maxDeleteParams {
		end := start + maxDeleteParams
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		res, err := s.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM kv_store WHERE key IN (%s)`, placeholders(len(chunk))),
			chunkToArgs(chunk)...)
		if err != nil {
			return deleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}

	s.mu.Lock()
	for _, key := range keys {
		if e, ok := s.cache[key]; ok {
			s.cacheBytes -= entrySize(key, e)
			delete(s.cache, key)
		}
	}
	s.mu.Unlock()

	return deleted, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func chunkToArgs(chunk []string) []any {
	args := make([]any, len(chunk))
	for i, k := range chunk {
		args[i] = k
	}
	return args
}

// caching reports whether the in-memory layer is enabled.
func (s *Store) caching() bool { return s.memLimit > 0 }

// CacheBytes returns the approximate memory used by the in-memory layer
// (always 0 when the layer is disabled).
func (s *Store) CacheBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cacheBytes
}

// Usage reports the footprint the eviction budget applies to: cache bytes when
// the memory layer is enabled, otherwise the on-disk database size.
func (s *Store) Usage(ctx context.Context) (int64, error) {
	if s.caching() {
		return s.CacheBytes(), nil
	}
	return s.Size(ctx)
}

// Size returns the on-disk footprint of the database including WAL files.
func (s *Store) Size(ctx context.Context) (int64, error) {
	// The main DB file size is cheap and deterministic enough for eviction
	// decisions. WAL pages land in -wal until a checkpoint.
	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("store: page_count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("store: page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// RowCount returns the number of stored keys.
func (s *Store) RowCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

// IncrementalVacuum reclaims up to n free pages (spec §8.4).
func (s *Store) IncrementalVacuum(ctx context.Context, n int) error {
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", n)); err != nil {
		return fmt.Errorf("store: incremental_vacuum: %w", err)
	}
	return nil
}

// Backup writes a VACUUM INTO snapshot into dir named kvp-<timestamp>.db and
// returns its path. The backup is a self-contained database file.
func (s *Store) Backup(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("store: backup dir not configured")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("store: mkdir backup dir: %w", err)
	}
	path := filepath.Join(dir, "kvp-"+s.now().UTC().Format("20060102T150405.000Z")+".db")
	sql := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(path, "'", "''"))
	if _, err := s.db.ExecContext(ctx, sql); err != nil {
		return "", fmt.Errorf("store: backup: %w", err)
	}
	return path, nil
}

// RetainBackups deletes all backup files in dir except the newest n (sorted by
// name, which is timestamp-ordered). Returns the number of deleted files.
func (s *Store) RetainBackups(dir string, n int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("store: read backup dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "kvp-") && strings.HasSuffix(e.Name(), ".db") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	excess := len(files) - n
	if excess <= 0 {
		return 0, nil
	}
	deleted := 0
	for _, f := range files[:excess] {
		if err := os.Remove(f); err != nil {
			return deleted, fmt.Errorf("store: remove backup %s: %w", f, err)
		}
		deleted++
	}
	return deleted, nil
}
