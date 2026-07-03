package usagehistory

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite" // register "sqlite" driver for database/sql
)

// isoTimeFormat is the storage format for timestamp columns. It is ISO-8601 UTC
// with millisecond precision, so lexical ordering matches chronological ordering
// and range queries (WHERE created_at >= ?) behave like a TIMESTAMPTZ column.
const isoTimeFormat = "2006-01-02T15:04:05.000Z"

// SqliteStore mirrors PgStore on top of an embedded SQLite database (usg.db).
// It satisfies HistoryStore so it can be driven by the shared async Writer and
// queried by the management API exactly like the TimescaleDB backend.
type SqliteStore struct {
	db *sql.DB

	// retentionCancel stops the background retention sweeper; retentionWG waits
	// for it to exit. Unlike TimescaleDB, SQLite has no automatic chunk drop, so
	// without a periodic sweep usg.db grows unbounded between restarts.
	retentionCancel context.CancelFunc
	retentionWG     sync.WaitGroup
}

// NewSqliteStore opens (creating if necessary) the SQLite database at dbPath,
// applies concurrency pragmas, and pings it.
func NewSqliteStore(ctx context.Context, dbPath string) (*SqliteStore, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("usagehistory: sqlite path is required")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("usagehistory: open sqlite: %w", err)
	}
	// WAL allows the async writer to flush while management handlers read
	// concurrently. busy_timeout makes a contending connection wait instead of
	// returning SQLITE_BUSY immediately. A small pool is enough: the only writer
	// is the serial flush goroutine.
	db.SetMaxOpenConns(4)
	if err = applySqlitePragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usagehistory: ping sqlite: %w", err)
	}
	return &SqliteStore{db: db}, nil
}

func applySqlitePragmas(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("usagehistory: sqlite pragma %q: %w", p, err)
		}
	}
	return nil
}

// EnsureSchema creates the usage_records table and indexes if they do not exist.
// The columns mirror migrations/001_create_usage_records.sql exactly (names and
// semantics); bigint columns become INTEGER and the boolean column becomes
// INTEGER 0/1 under SQLite's flexible typing.
func (s *SqliteStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_records (
			event_id            TEXT NOT NULL DEFAULT '',
			created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			provider            TEXT NOT NULL DEFAULT 'unknown',
			model               TEXT NOT NULL DEFAULT 'unknown',
			alias               TEXT NOT NULL DEFAULT '',
			endpoint            TEXT NOT NULL DEFAULT '',
			auth_type           TEXT NOT NULL DEFAULT 'unknown',
			api_key             TEXT NOT NULL DEFAULT '',
			request_id          TEXT NOT NULL DEFAULT '',
			reasoning_effort    TEXT NOT NULL DEFAULT '',
			latency_ms          INTEGER NOT NULL DEFAULT 0,
			source              TEXT NOT NULL DEFAULT '',
			auth_index          TEXT NOT NULL DEFAULT '',
			input_tokens        INTEGER NOT NULL DEFAULT 0,
			output_tokens       INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
			cached_tokens       INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens        INTEGER NOT NULL DEFAULT 0,
			failed              INTEGER NOT NULL DEFAULT 0,
			fail_status_code    INTEGER NOT NULL DEFAULT 0,
			fail_body           TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("usagehistory: sqlite create table: %w", err)
	}
	indexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_records_event_id_created_at
			ON usage_records (event_id, created_at) WHERE event_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_provider_model_key
			ON usage_records (provider, model, api_key, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_created_at
			ON usage_records (created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_api_key
			ON usage_records (api_key, created_at DESC)`,
	}
	for _, idx := range indexes {
		if _, err := s.db.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("usagehistory: sqlite create index: %w", err)
		}
	}
	return nil
}

// SetRetentionPolicy deletes rows older than days. TimescaleDB's chunk drop has
// no SQLite equivalent; a plain DELETE run at startup (and periodically) keeps
// the file bounded.
func (s *SqliteStore) SetRetentionPolicy(ctx context.Context, days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format(isoTimeFormat)
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage_records WHERE created_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("usagehistory: sqlite retention delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.WithField("rows", n).Info("usagehistory: sqlite retention delete")
	}
	return nil
}

const sqliteInsertUsageRecord = `
	INSERT INTO usage_records (
		event_id, created_at, provider, model, alias, endpoint, auth_type, api_key,
		request_id, reasoning_effort, latency_ms, source, auth_index,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens,
		cache_read_tokens, cache_creation_tokens, total_tokens,
		failed, fail_status_code, fail_body
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?
	)
	ON CONFLICT DO NOTHING`

// InsertBatch inserts records in a single transaction for throughput and
// idempotency. It satisfies the BatchInserter contract used by the shared Writer.
func (s *SqliteStore) InsertBatch(ctx context.Context, records []PgRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usagehistory: sqlite begin tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, sqliteInsertUsageRecord)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("usagehistory: sqlite prepare insert: %w", err)
	}
	for i := range records {
		r := &records[i]
		if _, err := stmt.ExecContext(ctx,
			pgRecordEventID(r), r.CreatedAt.UTC().Format(isoTimeFormat), r.Provider, r.Model, r.Alias, r.Endpoint, r.AuthType, r.APIKey,
			r.RequestID, r.ReasoningEffort, r.LatencyMs, r.Source, r.AuthIndex,
			r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens,
			r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
			boolToInt(r.Failed), r.FailStatusCode, r.FailBody,
		); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("usagehistory: sqlite insert record %d: %w", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("usagehistory: sqlite close stmt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usagehistory: sqlite commit: %w", err)
	}
	return nil
}

// QueryHistory returns records within the time window, newest first. The result
// is converted to JSONLRecord so the management API response matches the
// Postgres backend byte-for-byte.
func (s *SqliteStore) QueryHistory(ctx context.Context, since time.Time, limit int) ([]JSONLRecord, error) {
	sinceStr := since.UTC().Format(isoTimeFormat)
	query := `
		SELECT event_id, created_at, provider, model, alias, endpoint, auth_type, api_key,
			request_id, reasoning_effort, latency_ms, source, auth_index,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			cache_read_tokens, cache_creation_tokens, total_tokens,
			failed, fail_status_code, fail_body
		FROM usage_records
		WHERE created_at >= ?
		ORDER BY created_at DESC`
	args := []any{sinceStr}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usagehistory: sqlite query: %w", err)
	}
	defer rows.Close()

	var records []JSONLRecord
	for rows.Next() {
		var (
			r          PgRecord
			createdStr string
			failInt    int64
		)
		if err := rows.Scan(
			&r.EventID, &createdStr, &r.Provider, &r.Model, &r.Alias, &r.Endpoint, &r.AuthType, &r.APIKey,
			&r.RequestID, &r.ReasoningEffort, &r.LatencyMs, &r.Source, &r.AuthIndex,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens,
			&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens,
			&failInt, &r.FailStatusCode, &r.FailBody,
		); err != nil {
			return nil, fmt.Errorf("usagehistory: sqlite scan: %w", err)
		}
		r.Failed = failInt != 0
		r.CreatedAt = parseISOTime(createdStr)
		records = append(records, r.toJSONLRecord())
	}
	return records, rows.Err()
}

// StartRetentionSweeper launches a background goroutine that periodically deletes
// rows older than the retention window. TimescaleDB drops old chunks on its own;
// SQLite has no equivalent, so without this usg.db only gets trimmed once at
// startup and grows for the rest of the process lifetime. Safe to call at most
// once; no-op if days <= 0 or interval <= 0.
func (s *SqliteStore) StartRetentionSweeper(ctx context.Context, days int, interval time.Duration) {
	if s == nil || days <= 0 || interval <= 0 {
		return
	}
	sweepCtx, cancel := context.WithCancel(ctx)
	s.retentionCancel = cancel
	s.retentionWG.Add(1)
	go func() {
		defer s.retentionWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if err := s.SetRetentionPolicy(sweepCtx, days); err != nil {
					log.WithError(err).Warn("usagehistory: sqlite retention sweep failed")
				}
			}
		}
	}()
}

// Close stops the retention sweeper (if running) and closes the underlying
// database connection. Idempotent.
func (s *SqliteStore) Close() {
	if s == nil {
		return
	}
	if s.retentionCancel != nil {
		s.retentionCancel()
		s.retentionWG.Wait()
		s.retentionCancel = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func parseISOTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(isoTimeFormat, s); err == nil {
		return t
	}
	return time.Time{}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
