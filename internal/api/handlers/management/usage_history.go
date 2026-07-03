package management

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagehistory"
)

// GetUsageHistory reads usage records from the TimescaleDB backend when available,
// falling back to JSONL files. Supports ?days=N and ?limit=N query parameters.
func (h *Handler) GetUsageHistory(c *gin.Context) {
	if !usagehistory.Enabled() {
		c.JSON(http.StatusOK, gin.H{"records": []interface{}{}})
		return
	}

	// Parse query parameters.
	days := 7
	if d := c.Query("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil && parsed > 0 {
			days = parsed
		}
	}
	// 0 = no limit (return every record in the window).
	limit := 0
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed >= 0 {
			limit = parsed
		}
	}

	// If TimescaleDB backend is available, query it directly.
	if usagehistory.HasPgStore() {
		since := time.Now().AddDate(0, 0, -days)
		records, err := usagehistory.QueryHistory(c.Request.Context(), since, limit)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"records": records, "source": "postgres"})
			return
		}
		// Fall through to SQLite / JSONL on error.
	}
	if usagehistory.HasSqliteStore() {
		// Embedded SQLite backend (usg.db) — the durable store when
		// usage-history-sqlite-enabled is on (in which case JSONL is skipped).
		since := time.Now().AddDate(0, 0, -days)
		records, err := usagehistory.SqliteQueryHistory(c.Request.Context(), since, limit)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"records": records, "source": "sqlite"})
			return
		}
		// Fall through to degraded-query / JSONL on error.
	} else if usagehistory.SqliteConfigured() {
		// Degraded: the async writer's last flush exhausted retries, so the most
		// recent records may be missing. Every committed row is still readable
		// from usg.db, though, and serving them with a "degraded" flag is strictly
		// more useful than falling through to the JSONL fallback — which is empty
		// once SQLite replaces JSONL as the durable store.
		since := time.Now().AddDate(0, 0, -days)
		records, err := usagehistory.SqliteQueryHistory(c.Request.Context(), since, limit)
		body := gin.H{"records": []interface{}{}, "source": "sqlite", "degraded": true}
		if err == nil {
			body["records"] = records
		}
		c.JSON(http.StatusOK, body)
		return
	}

	// Fallback: read from JSONL files.
	dir := h.cfg.UsageHistoryDir
	if dir == "" {
		dir = "usage-history"
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"records": []interface{}{}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read history directory"})
		return
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "usage-") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	var rawRecords []json.RawMessage
	for _, path := range files {
		readJSONLFile(path, &rawRecords)
	}

	// Apply limit and days filter to JSONL results.
	since := time.Now().AddDate(0, 0, -days)
	var filtered []json.RawMessage
	for _, raw := range rawRecords {
		var rec struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			filtered = append(filtered, raw)
			continue
		}
		if rec.Timestamp.IsZero() || rec.Timestamp.After(since) || rec.Timestamp.Equal(since) {
			filtered = append(filtered, raw)
		}
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{"records": filtered, "source": "jsonl"})
}

func readJSONLFile(path string, out *[]json.RawMessage) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		*out = append(*out, raw)
	}
}
