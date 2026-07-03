package usagehistory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestSqliteStore(t *testing.T) *SqliteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usg.db")
	s, err := NewSqliteStore(context.Background(), path)
	if err != nil {
		t.Fatalf("NewSqliteStore: %v", err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return s
}

func TestSqliteStoreInsertAndQueryRoundTrip(t *testing.T) {
	s := newTestSqliteStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	records := []PgRecord{
		{
			EventID:     "evt-1",
			CreatedAt:   now,
			Provider:    "claude",
			Model:       "claude-sonnet",
			Alias:       "sonnet",
			Endpoint:    "/v1/messages",
			AuthType:    "oauth",
			InputTokens: 12,
			TotalTokens: 34,
			Failed:      false,
		},
		{
			EventID:        "evt-2",
			CreatedAt:      now.Add(-1 * time.Hour),
			Provider:       "gemini",
			Model:          "gemini-2.5",
			TotalTokens:    56,
			Failed:         true,
			FailStatusCode: 500,
			FailBody:       "boom",
		},
	}

	if err := s.InsertBatch(ctx, records); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	got, err := s.QueryHistory(ctx, now.Add(-2*time.Hour), 0)
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	// Newest first.
	if got[0].EventID != "evt-1" {
		t.Fatalf("first record = %q, want evt-1", got[0].EventID)
	}
	// Round-trip of token + fail fields.
	if got[0].Tokens.InputTokens != 12 || got[0].Tokens.TotalTokens != 34 {
		t.Fatalf("token round-trip mismatch: %+v", got[0].Tokens)
	}
	if got[1].Failed != true || got[1].Fail.StatusCode != 500 || got[1].Fail.Body != "boom" {
		t.Fatalf("fail round-trip mismatch: %+v", got[1])
	}
	// Timestamp round-trips as UTC time.
	if got[0].Timestamp.IsZero() {
		t.Fatal("timestamp not parsed")
	}
}

func TestSqliteStoreDedupByEventID(t *testing.T) {
	s := newTestSqliteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	rec := PgRecord{EventID: "dup", CreatedAt: now, Provider: "p", Model: "m", TotalTokens: 1}
	if err := s.InsertBatch(ctx, []PgRecord{rec}); err != nil {
		t.Fatalf("InsertBatch 1: %v", err)
	}
	if err := s.InsertBatch(ctx, []PgRecord{rec}); err != nil {
		t.Fatalf("InsertBatch 2: %v", err)
	}
	got, err := s.QueryHistory(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("dedup failed: got %d records, want 1", len(got))
	}
}

func TestSqliteStoreRetentionDelete(t *testing.T) {
	s := newTestSqliteStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Millisecond)
	fresh := time.Now().UTC()
	if err := s.InsertBatch(ctx, []PgRecord{
		{EventID: "old", CreatedAt: old, Provider: "p", Model: "m", TotalTokens: 1},
		{EventID: "fresh", CreatedAt: fresh, Provider: "p", Model: "m", TotalTokens: 1},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	// 30-day retention keeps the "fresh" record and the 72h-old record.
	if err := s.SetRetentionPolicy(ctx, 30); err != nil {
		t.Fatalf("SetRetentionPolicy 30: %v", err)
	}
	got, err := s.QueryHistory(ctx, old.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("30-day retention deleted too much: got %d, want 2", len(got))
	}
	// 1-day retention drops the 72h-old record only.
	if err := s.SetRetentionPolicy(ctx, 1); err != nil {
		t.Fatalf("SetRetentionPolicy 1: %v", err)
	}
	got, err = s.QueryHistory(ctx, old.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("QueryHistory after retention: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "fresh" {
		t.Fatalf("1-day retention result wrong: %+v", got)
	}
}

func TestSqliteStoreHistoryStoreInterface(t *testing.T) {
	// Compile-time assertion that *SqliteStore satisfies the shared Writer
	// contract, so it can be driven by NewWriter like *PgStore.
	var _ HistoryStore = (*SqliteStore)(nil)
}

func TestSqliteStoreDrivenByWriter(t *testing.T) {
	s := newTestSqliteStore(t)
	ctx := context.Background()

	w := NewWriter(s, 0, 50, 5*time.Millisecond)
	w.Start(ctx)
	defer w.Stop()

	now := time.Now().UTC()
	for i := 0; i < 120; i++ {
		if !w.Write(PgRecord{EventID: "", CreatedAt: now, Provider: "p", Model: "m", TotalTokens: int64(i)}) {
			t.Fatalf("Write returned false at %d", i)
		}
	}

	w.Stop() // final flush drains everything

	got, err := s.QueryHistory(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("writer+sqlite dropped records: got %d, want 120", len(got))
	}
}
