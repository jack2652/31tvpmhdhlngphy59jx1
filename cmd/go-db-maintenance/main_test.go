package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"option-scope-go/internal/config"
	"option-scope-go/internal/store"
)

var configEnvKeys = []string{
	"HOST", "PORT", "DATABASE_PATH", "ACCESS_KEY", "MARKET_PROXY",
	"DEFAULT_SYMBOLS", "RAW_RETENTION_DAYS", "DATABASE_MAX_MB", "SCHEDULER_ENABLED",
	"HISTORY_MAX_AGE_SECONDS", "EXTREMES_MAX_AGE_SECONDS", "UPSTREAM_TIMEOUT_SECONDS",
	"CBOE_TIMEOUT_SECONDS", "UPSTREAM_WAIT_SECONDS", "UPSTREAM_CONCURRENCY", "LOW_MEMORY",
	"ANALYSIS_CACHE_ENTRIES", "ALPACA_API_KEY", "ALPACA_API_SECRET",
	"SIZE_CLEANUP_TIME_BUDGET_SECONDS",
}

func loadConfigFromTempEnv(t *testing.T, values string) (config.Config, string) {
	t.Helper()
	projectDir := t.TempDir()
	databasePath := filepath.Join(projectDir, "isolated-options.db")
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(fmt.Sprintf("DATABASE_PATH=%s\n%s", databasePath, values)), 0o600); err != nil {
		t.Fatal(err)
	}
	type priorValue struct {
		value   string
		present bool
	}
	prior := make(map[string]priorValue, len(configEnvKeys))
	for _, key := range configEnvKeys {
		value, present := os.LookupEnv(key)
		prior[key] = priorValue{value: value, present: present}
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	restore := func() {
		for key, previous := range prior {
			if previous.present {
				_ = os.Setenv(key, previous.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	defer restore()
	return config.LoadFromDir(projectDir), databasePath
}

func initializeDatabase(t *testing.T, path string) {
	t.Helper()
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

func insertQuoteFixture(t *testing.T, path, symbol string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO quote_snapshots (symbol, fetched_at, provider) VALUES (?, ?, 'fixture')`, symbol, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func unusedLocalPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCleanupOfflineUsesTempConfigAndRetentionAndSizeSettings(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "RAW_RETENTION_DAYS=1\nDATABASE_MAX_MB=1\nSIZE_CLEANUP_TIME_BUDGET_SECONDS=5\nLOW_MEMORY=true\n")
	if cfg.DatabasePath != databasePath {
		t.Fatalf("loaded database path = %q, want isolated path %q", cfg.DatabasePath, databasePath)
	}
	initializeDatabase(t, databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	current := time.Now().UTC().Format(time.RFC3339Nano)
	for _, fetchedAt := range []string{old, current} {
		if _, err := database.Exec(`INSERT INTO quote_snapshots (symbol, fetched_at, provider) VALUES ('TEST', ?, 'fixture')`, fetchedAt); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"cleanup-offline"}, cfg, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Deleted map[string]int64 `json:"deleted"`
		Size    struct {
			LimitBytes int64 `json:"limit_bytes"`
		} `json:"size"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode cleanup result %q: %v", output.String(), err)
	}
	if result.Deleted["quotes"] != 1 {
		t.Fatalf("deleted quotes = %d, want 1", result.Deleted["quotes"])
	}
	if result.Size.LimitBytes != 1<<20 {
		t.Fatalf("size limit bytes = %d, want %d", result.Size.LimitBytes, 1<<20)
	}
	database, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var remaining int
	if err := database.QueryRow("SELECT COUNT(*) FROM quote_snapshots").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining quotes = %d, want 1", remaining)
	}
	leaseProbe, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseProbe.Close()
	if acquired, err := leaseProbe.TryAcquireServiceLease(housekeepingLeaseName, "post-cleanup-probe", time.Minute); err != nil || !acquired {
		t.Fatalf("cleanup lease was not released: acquired=%v err=%v", acquired, err)
	}
	if err := leaseProbe.ReleaseServiceLease(housekeepingLeaseName, "post-cleanup-probe"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupOfflinePrunesLegacyRawJSONWithoutSizeLimit(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	if cfg.DatabaseMaxMB != 0 {
		t.Fatalf("database size limit = %d MB, want unlimited", cfg.DatabaseMaxMB)
	}
	initializeDatabase(t, databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'fixture', '{"legacy":"quote"}');
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, provider, raw_json)
VALUES ('QQQ', '2030-01-18', ?, 'QQQ300118C00100000', 'call', 'fixture', '{"legacy":"option"}')`, stamp, stamp); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run([]string{"cleanup-offline"}, cfg, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Deleted map[string]int64 `json:"deleted"`
		Size    struct {
			LimitBytes     int64 `json:"limit_bytes"`
			RawJSONCleared int64 `json:"raw_json_cleared"`
		} `json:"size"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode cleanup result %q: %v", output.String(), err)
	}
	if result.Size.LimitBytes != 0 {
		t.Fatalf("size limit bytes = %d, want unlimited", result.Size.LimitBytes)
	}
	if result.Size.RawJSONCleared != 2 {
		t.Fatalf("raw_json_cleared = %d, want 2", result.Size.RawJSONCleared)
	}

	database, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, table := range []string{"quote_snapshots", "option_snapshots"} {
		var rows, payloads int
		if err := database.QueryRow("SELECT COUNT(*), COUNT(raw_json) FROM "+table).Scan(&rows, &payloads); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || payloads != 0 {
			t.Errorf("%s rows=%d raw_json payloads=%d, want one row and no payload", table, rows, payloads)
		}
	}
}

func TestCleanupOfflineRejectsActiveHousekeepingLeaseWithoutDeletingRows(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "RAW_RETENTION_DAYS=1\nLOW_MEMORY=true\n")
	initializeDatabase(t, databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	if _, err := database.Exec(`INSERT INTO quote_snapshots (symbol, fetched_at, provider) VALUES ('LEASED', ?, 'fixture')`, old); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	leaseOwner, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseOwner.Close()
	if acquired, err := leaseOwner.TryAcquireServiceLease(housekeepingLeaseName, "active-housekeeper", 48*time.Hour); err != nil || !acquired {
		t.Fatalf("failed to seed active housekeeping lease: acquired=%v err=%v", acquired, err)
	}
	defer leaseOwner.ReleaseServiceLease(housekeepingLeaseName, "active-housekeeper")

	var output bytes.Buffer
	err = cleanupOffline(cfg, &output)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("cleanupOffline() error=%v, want active-maintenance rejection", err)
	}
	checkDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()
	var remaining int
	if err := checkDB.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='LEASED'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("rejected cleanup deleted fixture rows: remaining=%d, want 1", remaining)
	}
}

func TestCleanupOnlineUsesConfiguredKeyAndLocalAPI(t *testing.T) {
	cfg, _ := loadConfigFromTempEnv(t, "ACCESS_KEY=temp-access-key\nSIZE_CLEANUP_TIME_BUDGET_SECONDS=5\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/cleanup" {
			t.Errorf("request = %s %s, want POST /api/cleanup", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Access-Key"); got != "temp-access-key" {
			t.Errorf("X-Access-Key = %q, want configured key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":{"quotes":0},"size":{"limit_bytes":0}}`))
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = port
	var output bytes.Buffer
	if err := run([]string{"cleanup-online"}, cfg, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"quotes":0`) {
		t.Fatalf("output = %q, missing API response", output.String())
	}
}

func TestCleanupOnlineRetriesLeaseBusyThenWritesOriginalJSON(t *testing.T) {
	cfg, _ := loadConfigFromTempEnv(t, "ACCESS_KEY=temp-access-key\nSIZE_CLEANUP_TIME_BUDGET_SECONDS=5\n")
	attempts := 0
	successBody := `{"deleted":{"quotes":2},"size":{"before_bytes":10,"after_bytes":8}}` + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Method != http.MethodPost || r.URL.Path != "/api/cleanup" {
			t.Errorf("request = %s %s, want POST /api/cleanup", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Access-Key"); got != "temp-access-key" {
			t.Errorf("X-Access-Key = %q, want configured key", got)
		}
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"detail":"`+cleanupLeaseBusyDetail+`"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successBody)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = port
	var output bytes.Buffer
	if err := run([]string{"cleanup-online"}, cfg, &output); err != nil {
		t.Fatalf("cleanup-online failed after lease became available: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("cleanup request attempts=%d, want 3", attempts)
	}
	if output.String() != successBody {
		t.Fatalf("cleanup output=%q, want original JSON %q", output.String(), successBody)
	}
}

func TestCleanupOnlineDoesNotRetryOtherUnavailableResponses(t *testing.T) {
	cfg, _ := loadConfigFromTempEnv(t, "SIZE_CLEANUP_TIME_BUDGET_SECONDS=5\n")
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"detail":"maintenance temporarily unavailable"}`)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = port
	var output bytes.Buffer
	err = run([]string{"cleanup-online"}, cfg, &output)
	if err == nil || !strings.Contains(err.Error(), "maintenance temporarily unavailable") {
		t.Fatalf("cleanup-online error=%v, want non-lease 503 failure", err)
	}
	if attempts != 1 {
		t.Fatalf("cleanup request attempts=%d, want no retry", attempts)
	}
}

func TestCleanupOnlineBoundsLeaseBusyRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"detail":"`+cleanupLeaseBusyDetail+`"}`)
	}))
	defer server.Close()
	var output bytes.Buffer
	var delays []time.Duration
	err := cleanupOnlineWithRetry("", server.URL, &output, server.Client(), func(delay time.Duration) {
		delays = append(delays, delay)
	})
	if err == nil || !strings.Contains(err.Error(), cleanupLeaseBusyDetail) {
		t.Fatalf("cleanup error=%v, want lease-busy error after retry limit", err)
	}
	if attempts != cleanupOnlineMaxAttempts {
		t.Fatalf("cleanup request attempts=%d, want maximum %d", attempts, cleanupOnlineMaxAttempts)
	}
	if len(delays) != cleanupOnlineMaxAttempts-1 {
		t.Fatalf("retry delays=%v, want %d delays", delays, cleanupOnlineMaxAttempts-1)
	}
	for _, delay := range delays {
		if delay != cleanupRetryMaxDelay {
			t.Fatalf("retry delay=%s, want bounded maximum %s", delay, cleanupRetryMaxDelay)
		}
	}
	if output.Len() != 0 {
		t.Fatalf("cleanup output=%q, want no output on failure", output.String())
	}
}

func TestCleanupRetryDelayUsesBoundedBackoffWhenHeaderIsMissing(t *testing.T) {
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		cleanupRetryMaxDelay,
	}
	for attempt, expected := range want {
		if got := cleanupRetryDelay("", attempt+1); got != expected {
			t.Errorf("cleanupRetryDelay(attempt=%d)=%s, want %s", attempt+1, got, expected)
		}
	}
}

func TestBackupUsesVacuumIntoAndPreservesRows(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "")
	initializeDatabase(t, databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO quote_snapshots (symbol, fetched_at, provider) VALUES ('BACKUP', ?, 'fixture')`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backup", "copy.db")
	var output bytes.Buffer
	if err := run([]string{"backup", destination}, cfg, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	copyDB, err := sql.Open("sqlite", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM quote_snapshots WHERE symbol='BACKUP'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("copied rows = %d, want 1", count)
	}
}

func TestBackupIncludesUncheckpointedWALAtReservedSourcePath(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "options?account#fragment%name.db")
	initializeDatabase(t, source)
	database, err := sql.Open("sqlite", sqliteFileDSN(source, false))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	conn, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('WAL-BACKUP', '2030-01-18T15:00:00Z', 'fixture')`); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(source + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("fixture did not leave data in WAL: info=%v err=%v", walInfo, err)
	}

	destination := filepath.Join(directory, "backup.db")
	var output bytes.Buffer
	if err := backupDatabase(source, destination, &output); err != nil {
		t.Fatalf("backup database at a reserved-character path: %v", err)
	}
	backup, err := sql.Open("sqlite", sqliteFileDSN(destination, true))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var count int
	if err := backup.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='WAL-BACKUP'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backup rows=%d, want WAL-resident row", count)
	}
}

func TestRestoreReplacesDatabaseAndKeepsRollbackFiles(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	cfg.Host = "127.0.0.1"
	cfg.Port = unusedLocalPort(t)
	initializeDatabase(t, databasePath)
	insertQuoteFixture(t, databasePath, "BEFORE")

	backupPath := filepath.Join(t.TempDir(), "restore-source.db")
	initializeDatabase(t, backupPath)
	insertQuoteFixture(t, backupPath, "AFTER")

	for suffix, contents := range map[string]string{"-wal": "old-wal", "-shm": "old-shm"} {
		if err := os.WriteFile(databasePath+suffix, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := run([]string{"restore", backupPath}, cfg, &output); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if !strings.Contains(output.String(), "原数据库及 SQLite sidecar 保留在 ") {
		t.Fatalf("restore output does not identify rollback files: %q", output.String())
	}
	rollbackPath := strings.TrimSuffix(strings.SplitN(output.String(), "原数据库及 SQLite sidecar 保留在 ", 2)[1], "。\n")
	if _, err := os.Stat(rollbackPath); err != nil {
		t.Fatalf("previous database was not preserved at %s: %v", rollbackPath, err)
	}
	for suffix, want := range map[string]string{"-wal": "old-wal", "-shm": "old-shm"} {
		got, err := os.ReadFile(rollbackPath + suffix)
		if err != nil || string(got) != want {
			t.Errorf("rollback sidecar %s = %q, %v; want %q", suffix, got, err, want)
		}
	}
	database, err := sql.Open("sqlite", sqliteFileDSN(databasePath, true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var restored, old int
	if err := database.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='AFTER'`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='BEFORE'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if restored != 1 || old != 0 {
		t.Fatalf("restored rows AFTER/BEFORE=%d/%d, want 1/0", restored, old)
	}
}

func TestRestorePreservesExistingDatabaseOwnerAndGroup(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	cfg.Host = "127.0.0.1"
	cfg.Port = unusedLocalPort(t)
	initializeDatabase(t, databasePath)

	fixtureUID, fixtureGID := os.Geteuid(), os.Getegid()
	if fixtureUID == 0 {
		fixtureUID, fixtureGID = 65534, 65534
		if err := os.Chown(databasePath, fixtureUID, fixtureGID); err != nil {
			t.Skipf("cannot set distinct fixture owner/group: %v", err)
		}
	} else {
		groups, err := os.Getgroups()
		if err != nil {
			t.Fatal(err)
		}
		setDistinctGroup := false
		for _, group := range groups {
			if group == fixtureGID {
				continue
			}
			if err := os.Chown(databasePath, fixtureUID, group); err == nil {
				fixtureGID = group
				setDistinctGroup = true
				break
			}
		}
		if !setDistinctGroup {
			t.Skip("cannot set a supplementary group distinct from the staging file group")
		}
	}
	before, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("database file metadata does not expose Linux uid/gid")
	}

	backupPath := filepath.Join(t.TempDir(), "restore-owner-source.db")
	initializeDatabase(t, backupPath)
	if err := restoreDatabase(cfg, backupPath, &bytes.Buffer{}); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	after, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("restored file metadata does not expose Linux uid/gid")
	}
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Fatalf("restored uid/gid = %d/%d, want existing database uid/gid %d/%d", afterStat.Uid, afterStat.Gid, beforeStat.Uid, beforeStat.Gid)
	}
}

func TestRestoreOwnerPreservationFailureLeavesOriginalDatabaseInPlace(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	cfg.Host = "127.0.0.1"
	cfg.Port = unusedLocalPort(t)
	initializeDatabase(t, databasePath)
	insertQuoteFixture(t, databasePath, "KEEP")
	backupPath := filepath.Join(t.TempDir(), "restore-owner-failure-source.db")
	initializeDatabase(t, backupPath)
	insertQuoteFixture(t, backupPath, "REPLACEMENT")

	ownerPreservationErr := errors.New("simulated chown failure")
	chownCalled := false
	err := restoreDatabaseWithChown(cfg, backupPath, &bytes.Buffer{}, func(string, int, int) error {
		chownCalled = true
		return ownerPreservationErr
	})
	if !chownCalled || !errors.Is(err, ownerPreservationErr) {
		t.Fatalf("restore error = %v, chown called=%v; want owner-preservation failure", err, chownCalled)
	}

	database, err := sql.Open("sqlite", sqliteFileDSN(databasePath, true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var kept, replacement int
	if err := database.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='KEEP'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='REPLACEMENT'`).Scan(&replacement); err != nil {
		t.Fatal(err)
	}
	if kept != 1 || replacement != 0 {
		t.Fatalf("database rows after failed owner preservation KEEP/REPLACEMENT=%d/%d, want 1/0", kept, replacement)
	}
	rollbackFiles, err := filepath.Glob(databasePath + ".pre-restore-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(rollbackFiles) != 0 {
		t.Fatalf("owner-preservation failure moved original database to rollback files: %v", rollbackFiles)
	}
}

func TestRestoreRejectsInvalidBackupWithoutChangingDatabase(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	cfg.Host = "127.0.0.1"
	cfg.Port = unusedLocalPort(t)
	initializeDatabase(t, databasePath)
	insertQuoteFixture(t, databasePath, "KEEP")
	badBackup := filepath.Join(t.TempDir(), "not-a-database.db")
	if err := os.WriteFile(badBackup, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(cfg, badBackup, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid restore backup") {
		t.Fatalf("restore error = %v, want invalid backup rejection", err)
	}
	database, err := sql.Open("sqlite", sqliteFileDSN(databasePath, true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='KEEP'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("existing database rows after rejected restore = %d, want 1", count)
	}
}

func TestRestoreRejectsListeningApplication(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "LOW_MEMORY=true\n")
	cfg.Host = "0.0.0.0"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = port
	backupPath := filepath.Join(t.TempDir(), "valid-backup.db")
	initializeDatabase(t, backupPath)
	if err := restoreDatabase(cfg, backupPath, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stop the application") {
		t.Fatalf("restore error = %v, want running-service rejection", err)
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("running-service restore created destination database: stat error=%v", err)
	}
}

func TestMoveDatabaseFilesRestoresPartialMoveOnRollbackCollision(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "options.db")
	rollbackPath := filepath.Join(directory, "options.db.rollback")
	if err := os.WriteFile(databasePath, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("source-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackPath+"-wal", []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := moveDatabaseFiles(databasePath, rollbackPath); err == nil || !strings.Contains(err.Error(), "rollback path already exists") {
		t.Fatalf("moveDatabaseFiles() error = %v, want rollback collision", err)
	}
	if contents, err := os.ReadFile(databasePath); err != nil || string(contents) != "database" {
		t.Fatalf("source database after partial-move rollback = %q, %v", contents, err)
	}
	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Fatalf("partial rollback file remains: stat error=%v", err)
	}
	if contents, err := os.ReadFile(databasePath + "-wal"); err != nil || string(contents) != "source-wal" {
		t.Fatalf("source WAL after partial-move rollback = %q, %v", contents, err)
	}
}

func TestStatsReadsConfiguredDatabase(t *testing.T) {
	cfg, databasePath := loadConfigFromTempEnv(t, "")
	initializeDatabase(t, databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO quote_snapshots (symbol, fetched_at, provider) VALUES ('STATS', ?, 'fixture')`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"stats"}, cfg, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "quote_snapshots") || !strings.Contains(output.String(), "sqlite_sequence") || !strings.Contains(output.String(), "1\n") {
		t.Fatalf("stats output differs from the Python table listing or misses quote count: %q", output.String())
	}
}
