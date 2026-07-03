package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newTestSqliteTokenStore(t *testing.T) (*SqliteTokenStore, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := SQLiteStoreConfig{
		Path:     filepath.Join(dir, "usg.db"),
		SpoolDir: filepath.Join(dir, "sqlitestore"),
	}
	s, err := NewSqliteTokenStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSqliteTokenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Bootstrap(context.Background(), ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return s, dir
}

func TestSqliteTokenStoreSaveListDelete(t *testing.T) {
	s, _ := newTestSqliteTokenStore(t)
	ctx := context.Background()

	auth := &cliproxyauth.Auth{
		ID:       "claude-1",
		FileName: "claude-1.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":  "claude",
			"email": "a@b.com",
		},
	}
	if _, err := s.Save(ctx, auth); err != nil {
		t.Fatalf("Save: %v", err)
	}

	listed, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d, want 1", len(listed))
	}
	got := listed[0]
	if got.Provider != "claude" || got.FileName != "claude-1.json" {
		t.Fatalf("auth metadata mismatch: %+v", got)
	}
	if got.Attributes["email"] != "a@b.com" {
		t.Fatalf("email attr missing: %+v", got.Attributes)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not scanned from sqlite TEXT column")
	}

	// PersistConfig round-trips the (empty) spool config.yaml into the config row.
	if err := s.PersistConfig(ctx); err != nil {
		t.Fatalf("PersistConfig: %v", err)
	}

	if err := s.Delete(ctx, "claude-1.json"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	listed, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("after delete List returned %d, want 0", len(listed))
	}
}

func TestSqliteTokenStoreBootstrapsFromDatabaseOnRestart(t *testing.T) {
	s1, dir := newTestSqliteTokenStore(t)
	ctx := context.Background()

	auth := &cliproxyauth.Auth{
		ID:       "gemini-1",
		FileName: "gemini-1.json",
		Provider: "gemini",
		Metadata: map[string]any{"type": "gemini"},
	}
	if _, err := s1.Save(ctx, auth); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_ = s1.Close()

	// A fresh store opening the SAME usg.db + spool must reconstruct the auth file.
	cfg := SQLiteStoreConfig{
		Path:     filepath.Join(dir, "usg.db"),
		SpoolDir: filepath.Join(dir, "sqlitestore"),
	}
	s2, err := NewSqliteTokenStore(ctx, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Bootstrap(ctx, ""); err != nil {
		t.Fatalf("Bootstrap reopen: %v", err)
	}

	// Auth file should have been rewritten from the DB by syncAuthFromDatabase.
	restoredPath := filepath.Join(cfg.SpoolDir, "auths", "gemini-1.json")
	if _, err := os.Stat(restoredPath); err != nil {
		t.Fatalf("auth file not restored on bootstrap: %v", err)
	}
	listed, err := s2.List(ctx)
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(listed) != 1 || listed[0].Provider != "gemini" {
		t.Fatalf("auth not restored from usg.db: %+v", listed)
	}
}
