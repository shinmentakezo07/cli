package usagehistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var (
	enabled    atomic.Bool
	pgDegraded atomic.Bool
	store      *Store  // existing JSONL store
	pgWriter   *Writer // async TimescaleDB writer (nil when Postgres not configured)

	sqliteDegraded atomic.Bool
	sqliteWriter   *Writer // async SQLite writer (nil when SQLite not configured)
)

func init() {
	coreusage.RegisterPlugin(&historyPlugin{})
}

// SetEnabled toggles the usage history plugin at runtime.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// Enabled returns whether the usage history plugin is active.
func Enabled() bool {
	return enabled.Load()
}

// InitStore initializes the global store with the given directory.
func InitStore(dir string) {
	if store != nil {
		_ = store.Close()
	}
	store = NewStore(dir)
}

// CloseStore closes the global store. Call on shutdown.
func CloseStore() {
	if store != nil {
		_ = store.Close()
	}
}

// SetPgWriter sets the async TimescaleDB writer. Must be called before any usage records are processed.
func SetPgWriter(w *Writer) {
	pgWriter = w
	pgDegraded.Store(false)
	if w != nil {
		w.SetDegradeHook(MarkPgDegraded)
	}
}

// StopPgWriter stops the async TimescaleDB writer, flushing remaining records.
func StopPgWriter() {
	if pgWriter != nil {
		pgWriter.Stop()
	}
}

// MarkPgDegraded forces management history queries to use JSONL fallback because
// the Postgres backlog is known to be incomplete.
func MarkPgDegraded() {
	pgDegraded.Store(true)
}

func PgDegraded() bool {
	return pgDegraded.Load()
}

// HasPgStore returns true if the TimescaleDB backend is available.
func HasPgStore() bool {
	return !PgDegraded() && pgWriter != nil && pgWriter.store != nil
}

// QueryHistory queries the TimescaleDB store for historical records.
// Returns an error if PgStore is not initialized.
func QueryHistory(ctx context.Context, since time.Time, limit int) ([]JSONLRecord, error) {
	if pgWriter == nil || pgWriter.store == nil {
		return nil, fmt.Errorf("usagehistory: TimescaleDB store not initialized")
	}
	return pgWriter.store.QueryHistory(ctx, since, limit)
}

// SetSqliteWriter sets the async SQLite writer. Must be called before any usage records are processed.
func SetSqliteWriter(w *Writer) {
	sqliteWriter = w
	sqliteDegraded.Store(false)
	if w != nil {
		w.SetDegradeHook(MarkSqliteDegraded)
	}
}

// StopSqliteWriter stops the async SQLite writer, flushing remaining records.
func StopSqliteWriter() {
	if sqliteWriter != nil {
		sqliteWriter.Stop()
	}
}

// MarkSqliteDegraded forces management history queries to fall back because the
// SQLite backlog is known to be incomplete.
func MarkSqliteDegraded() {
	sqliteDegraded.Store(true)
}

func SqliteDegraded() bool {
	return sqliteDegraded.Load()
}

// HasSqliteStore returns true if the SQLite backend is available and healthy
// (not degraded). Use SqliteConfigured for the degraded-tolerant variant.
func HasSqliteStore() bool {
	return !SqliteDegraded() && SqliteConfigured()
}

// SqliteConfigured returns true if the SQLite backend is present, regardless of
// whether its async writer is currently degraded. Committed rows remain
// queryable from usg.db even when degraded — only the latest unflushed batch is
// at risk — so the management handler serves them with a "degraded" annotation
// rather than falling through to an empty JSONL store.
func SqliteConfigured() bool {
	return sqliteWriter != nil && sqliteWriter.store != nil
}

// SqliteQueryHistory queries the SQLite store for historical records.
// Returns an error if the SQLite store is not initialized.
func SqliteQueryHistory(ctx context.Context, since time.Time, limit int) ([]JSONLRecord, error) {
	if sqliteWriter == nil || sqliteWriter.store == nil {
		return nil, fmt.Errorf("usagehistory: sqlite store not initialized")
	}
	return sqliteWriter.store.QueryHistory(ctx, since, limit)
}

func usageEventID(record coreusage.Record, endpoint, requestID string) string {
	parts := []string{
		strings.TrimSpace(record.Provider),
		strings.TrimSpace(record.Model),
		strings.TrimSpace(record.Alias),
		strings.TrimSpace(endpoint),
		strings.TrimSpace(record.AuthType),
		strings.TrimSpace(requestID),
		strings.TrimSpace(record.Source),
		strings.TrimSpace(record.AuthIndex),
		fmt.Sprintf("%d/%d/%d/%d/%d/%d/%d", record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.ReasoningTokens, record.Detail.CachedTokens, record.Detail.CacheReadTokens, record.Detail.CacheCreationTokens, record.Detail.TotalTokens),
		fmt.Sprintf("%t/%d", record.Failed, record.Fail.StatusCode),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

type historyPlugin struct{}

func (p *historyPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !Enabled() {
		return
	}
	// Nothing to do if no durable sink is configured. When the SQLite backend is
	// enabled the JSONL store is intentionally left uninitialized, so store may
	// be nil — that is valid as long as the SQLite writer is present.
	if store == nil && sqliteWriter == nil {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	alias := strings.TrimSpace(record.Alias)
	if alias == "" {
		alias = model
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}

	totalTokens := record.Detail.TotalTokens
	if totalTokens == 0 {
		totalTokens = record.Detail.InputTokens + record.Detail.OutputTokens
	}
	if totalTokens == 0 {
		totalTokens = record.Detail.CachedTokens
	}

	endpoint := strings.TrimSpace(logging.GetEndpoint(ctx))
	requestID := strings.TrimSpace(logging.GetRequestID(ctx))

	rec := JSONLRecord{
		EventID:         usageEventID(record, endpoint, requestID),
		Provider:        provider,
		Model:           model,
		Alias:           alias,
		Endpoint:        endpoint,
		AuthType:        authType,
		APIKey:          strings.TrimSpace(record.APIKey),
		RequestID:       requestID,
		ReasoningEffort: strings.TrimSpace(record.ReasoningEffort),
		Timestamp:       timestamp,
		LatencyMs:       record.Latency.Milliseconds(),
		Source:          record.Source,
		AuthIndex:       record.AuthIndex,
		Tokens: TokenStats{
			InputTokens:         record.Detail.InputTokens,
			OutputTokens:        record.Detail.OutputTokens,
			ReasoningTokens:     record.Detail.ReasoningTokens,
			CachedTokens:        record.Detail.CachedTokens,
			CacheReadTokens:     record.Detail.CacheReadTokens,
			CacheCreationTokens: record.Detail.CacheCreationTokens,
			TotalTokens:         totalTokens,
		},
		Failed: record.Failed,
		Fail: FailDetail{
			StatusCode: record.Fail.StatusCode,
			Body:       strings.TrimSpace(record.Fail.Body),
		},
	}

	// SQLite backend replaces JSONL when enabled: usg.db is the durable store.
	// The Postgres parallel writer (if also configured) stays independent.
	if sqliteWriter != nil {
		pgRec := fromJSONLRecord(&rec)
		sqliteWriter.Write(pgRec)
		if pgWriter != nil {
			pgWriter.Write(pgRec)
		}
		return
	}

	if store == nil {
		return
	}
	if err := store.Write(rec); err != nil {
		log.WithError(err).Warn("usagehistory: failed to write record")
		return
	}

	// Write to TimescaleDB via async writer (if configured). Records reach the
	// Postgres writer only after JSONL persistence succeeds, so JSONL remains the
	// durable fallback if the Postgres backlog is later degraded.
	if pgWriter != nil {
		pgWriter.Write(fromJSONLRecord(&rec))
	}
}
