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
	"time"

	_ "modernc.org/sqlite"
)

const (
	journalMode   = "WAL"
	synchronous   = "NORMAL"
	busyTimeoutMS = 5000
)

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// GetResult is the outcome of a key lookup.
type GetResult struct {
	Found   bool
	Expired bool
	Value   []byte
}

// Open opens (creating if needed) the SQLite database at path, applies the
// schema migration and the required PRAGMAs from the spec §4.
func Open(path string) (*Store, error) {
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

	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
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

// Put upserts value under key, setting expiry to now + ttl.
func (s *Store) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	expiresAt := s.now().Add(ttl)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO kv_store (key, value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, expires_at=excluded.expires_at`,
		key, value, expiresAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: put %q: %w", key, err)
	}
	return nil
}

// Get retrieves value for key. A row past its expiry is deleted on the spot and
// reported as Expired.
func (s *Store) Get(ctx context.Context, key string) (GetResult, error) {
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

// DeleteExpired removes every row past its expiry and returns the deleted count.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM kv_store WHERE expires_at < ?`, s.now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// EvictOldest runs the size-based eviction described in the spec §8.2 using the
// real on-disk size. See evictOldest for the algorithm.
func (s *Store) EvictOldest(ctx context.Context, limit int64, batchSize, maxRuns int) (int64, error) {
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

		res, err := s.db.ExecContext(ctx, `
DELETE FROM kv_store
WHERE key IN (SELECT key FROM kv_store ORDER BY expires_at ASC LIMIT ?)`, batch)
		if err != nil {
			return deleted, fmt.Errorf("store: evict batch: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		if n == 0 {
			return deleted, nil
		}
		deleted += n
	}
	return deleted, nil
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
