package usagehistory

import (
	"context"
	"testing"
	"time"
)

// TestSqliteDegradedStillServesCommittedRows pins the management-query fallback
// contract used by GetUsageHistory: when the async SQLite writer is flagged
// degraded (its last flush exhausted retries, so the most recent records may be
// missing) HasSqliteStore() flips to false so the handler does not treat usg.db
// as authoritative — but SqliteConfigured() stays true and SqliteQueryHistory
// still returns every committed row. The handler serves those rows with a
// "degraded": true annotation rather than falling through to an empty JSONL
// store; this test guards that the read path itself keeps working while
// degraded.
func TestSqliteDegradedStillServesCommittedRows(t *testing.T) {
	s := newTestSqliteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Two commits land durably in usg.db before any degradation.
	committed := []PgRecord{
		{EventID: "d-1", CreatedAt: now.Add(-1 * time.Minute), Provider: "p", Model: "m", TotalTokens: 1},
		{EventID: "d-2", CreatedAt: now.Add(-2 * time.Minute), Provider: "p", Model: "m", TotalTokens: 2},
	}
	if err := s.InsertBatch(ctx, committed); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// Wire the store through a (unstarted) Writer exactly as main.go does, then
	// reset all package-global SQLite state on cleanup so this test cannot leak
	// into others sharing the package vars.
	w := NewWriter(s, 4, 2, time.Second)
	SetSqliteWriter(w)
	t.Cleanup(func() {
		StopSqliteWriter()
		SetSqliteWriter(nil) // resets sqliteWriter=nil and sqliteDegraded=false
	})

	if !HasSqliteStore() {
		t.Fatal("HasSqliteStore = false before degradation, want true")
	}
	if !SqliteConfigured() {
		t.Fatal("SqliteConfigured = false, want true")
	}
	if SqliteDegraded() {
		t.Fatal("SqliteDegraded = true before degradation")
	}

	// Simulate the degrade hook firing from the writer's flush loop.
	MarkSqliteDegraded()

	// Degradation flips the "healthy/authoritative" view but NOT "configured".
	if HasSqliteStore() {
		t.Fatal("HasSqliteStore = true while degraded, want false (handler must not treat usg.db as authoritative)")
	}
	if !SqliteConfigured() {
		t.Fatal("SqliteConfigured = false while degraded, want true (handler needs it for the committed-row fallback)")
	}
	if !SqliteDegraded() {
		t.Fatal("SqliteDegraded = false after MarkSqliteDegraded")
	}

	// The query path must still serve every committed row even while degraded:
	// the async backlog being incomplete only risks the latest unflushed batch,
	// not rows already committed.
	got, err := SqliteQueryHistory(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("SqliteQueryHistory while degraded: %v", err)
	}
	if len(got) != len(committed) {
		t.Fatalf("degraded query returned %d records, want %d (committed rows must remain readable)", len(got), len(committed))
	}
	// Newest first, as the handler's response ordering assumes.
	if got[0].EventID != "d-1" {
		t.Fatalf("degraded query ordering wrong: first = %q, want d-1", got[0].EventID)
	}
}
