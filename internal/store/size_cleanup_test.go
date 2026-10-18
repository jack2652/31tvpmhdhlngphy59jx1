package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenWithLowMemoryAppliesSQLitePragmasAndSingleConnection(t *testing.T) {
	repository, err := OpenWithOptions(filepath.Join(t.TempDir(), "low-memory.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if got := repository.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max open connections=%d want 1", got)
	}
	for pragma, want := range map[string]int64{
		"PRAGMA cache_size":         -256,
		"PRAGMA mmap_size":          0,
		"PRAGMA temp_store":         1, // SQLITE_TEMP_STORE=FILE
		"PRAGMA wal_autocheckpoint": 200,
	} {
		var got int64
		if err := repository.db.QueryRow(pragma).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("%s=%d want %d", pragma, got, want)
		}
	}
}

func TestCleanupBySizeSkipsVacuumInLowMemoryMode(t *testing.T) {
	repository, err := OpenWithOptions(filepath.Join(t.TempDir(), "low-memory-cleanup.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"large":"payload"}')`, stamp); err != nil {
		t.Fatal(err)
	}
	previousOutput := log.Writer()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)
	result, err := repository.CleanupBySize(1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["raw_json_cleared"] != int64(1) || result["vacuumed"] != false {
		t.Fatalf("low-memory cleanup should prune redundant payload but skip VACUUM: %#v", result)
	}
	if !strings.Contains(logs.String(), "low-memory protection: skipped VACUUM") {
		t.Fatalf("low-memory VACUUM skip was not logged: %q", logs.String())
	}
	var raw *string
	if err := repository.db.QueryRow(`SELECT raw_json FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("redundant raw JSON remains after low-memory cleanup: %q", *raw)
	}
}

func TestFreeBytesMatchesPythonDiskUsageUnits(t *testing.T) {
	if got, want := availableBytes(7, 4096), int64(7*4096); got != want {
		t.Fatalf("availableBytes()=%d, want %d (f_bavail * f_frsize)", got, want)
	}

	path := t.TempDir()
	got, err := freeBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Fatalf("freeBytes=%d, want positive available space", got)
	}
}

func TestAvailableDiskSpaceUsesPythonFreeSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-space.db")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	got, err := repository.AvailableDiskSpaceMB()
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Fatalf("AvailableDiskSpaceMB()=%d, want positive available space", got)
	}
}

func TestCheckpointReaderBusySkipsVacuum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint-reader.db")
	cleaner, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleaner.Close()
	cleaner.db.SetMaxOpenConns(1)
	cleaner.db.SetMaxIdleConns(1)
	if _, err := cleaner.db.Exec(`PRAGMA busy_timeout=50`); err != nil {
		t.Fatal(err)
	}

	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	stamp := storedTimestamp(time.Now().UTC())
	if _, err := writer.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('QQQ', ?, 'test')`, stamp); err != nil {
		t.Fatal(err)
	}
	readTx, err := reader.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer readTx.Rollback()
	var snapshotRows int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM quote_snapshots`).Scan(&snapshotRows); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('QQQ', ?, 'test')`, storedTimestamp(time.Now().UTC().Add(time.Second))); err != nil {
		t.Fatal(err)
	}

	vacuumed, err := cleaner.checkpointAndVacuum()
	if err != nil {
		t.Fatal(err)
	}
	if vacuumed {
		t.Fatal("checkpoint blocked by an active reader was reported as vacuumed")
	}
	var readerRows int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM quote_snapshots`).Scan(&readerRows); err != nil {
		t.Fatal(err)
	}
	if readerRows != snapshotRows {
		t.Fatalf("reader snapshot changed across checkpoint: rows=%d want %d", readerRows, snapshotRows)
	}
	if err := readTx.Commit(); err != nil {
		t.Fatal(err)
	}

	vacuumed, err = cleaner.checkpointAndVacuum()
	if err != nil {
		t.Fatal(err)
	}
	if !vacuumed {
		t.Fatal("VACUUM was not attempted after the reader released its snapshot")
	}
}

func TestVacuumSpaceThresholdMatchesPythonFloatComparison(t *testing.T) {
	for _, test := range []struct {
		databaseBytes int64
		freeBytes     int64
		want          bool
	}{
		{databaseBytes: 5, freeBytes: 6, want: false},
		{databaseBytes: 6, freeBytes: 7, want: true},
		{databaseBytes: 6, freeBytes: 8, want: false},
	} {
		if got := vacuumSpaceInsufficient(test.databaseBytes, test.freeBytes); got != test.want {
			t.Errorf("vacuumSpaceInsufficient(%d, %d)=%t, want %t",
				test.databaseBytes, test.freeBytes, got, test.want)
		}
	}
}

func TestPruneLegacyRawJSONIsBoundedAndIdempotent(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "legacy-raw-json.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, price, raw_json)
VALUES ('QQQ', ?, 'python', 401.25, '{"legacy":"quote-1"}'),
       ('SPY', ?, 'python', 502.50, '{"legacy":"quote-2"}'),
       ('IWM', ?, 'python', 198.75, '{"legacy":"quote-3"}');
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, provider, raw_json)
VALUES ('QQQ', '2030-01-18', ?, 'QQQ300118C00100000', 'call', 'python', '{"legacy":"option-1"}'),
       ('QQQ', '2030-01-18', ?, 'QQQ300118P00100000', 'put', 'python', '{"legacy":"option-2"}')`, stamp, stamp, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	// A zero budget is a bounded no-op, even when payloads are present.
	cleared, err := repository.PruneLegacyRawJSON(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 0 {
		t.Fatalf("zero-budget prune cleared %d rows, want 0", cleared)
	}
	cleared, err = repository.PruneLegacyRawJSON(1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 5 {
		t.Fatalf("cleared=%d, want 5", cleared)
	}
	if cleared, err := repository.PruneLegacyRawJSON(1, time.Second); err != nil || cleared != 0 {
		t.Fatalf("second prune cleared=%d err=%v, want idempotent zero", cleared, err)
	}

	var rows, payloads int
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&rows, &payloads); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || payloads != 0 {
		t.Fatalf("quote rows=%d payloads=%d, want 3 and 0", rows, payloads)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM option_snapshots`).Scan(&rows, &payloads); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || payloads != 0 {
		t.Fatalf("option rows=%d payloads=%d, want 2 and 0", rows, payloads)
	}
	var price float64
	if err := repository.db.QueryRow(`SELECT price FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&price); err != nil {
		t.Fatal(err)
	}
	if price != 401.25 {
		t.Fatalf("business fields changed during raw_json pruning: price=%v", price)
	}
}

func TestCleanupBySizeDisabledDoesNotDeleteData(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"large":"payload"}')`, stamp); err != nil {
		t.Fatal(err)
	}
	result, err := repository.CleanupBySize(0, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["raw_json_cleared"] != int64(0) || result["superseded_quotes"] != int64(0) || result["vacuumed"] != false {
		t.Fatalf("disabled cleanup changed data: %#v", result)
	}
	var raw string
	if err := repository.db.QueryRow(`SELECT raw_json FROM quote_snapshots`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("disabled cleanup cleared raw_json")
	}
}

func TestCleanupBySizeZeroBudgetDoesNotMutateRows(t *testing.T) {
	repository, err := OpenWithOptions(filepath.Join(t.TempDir(), "size-cleanup-zero-budget.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	old := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	newest := time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"legacy":"old"}'), ('QQQ', ?, 'test', '{"legacy":"new"}');
INSERT INTO refresh_runs(symbol, started_at, status)
VALUES ('QQQ', ?, 'success')`, old, newest, old); err != nil {
		t.Fatal(err)
	}

	result, err := repository.CleanupBySize(1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"raw_json_cleared", "superseded_quotes", "superseded_options", "stale_runs"} {
		if got := result[key]; got != int64(0) {
			t.Errorf("result[%s]=%v, want 0 with an expired budget", key, got)
		}
	}
	var quotes, payloads, runs int
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&quotes, &payloads); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM refresh_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if quotes != 2 || payloads != 2 || runs != 1 {
		t.Fatalf("zero-budget cleanup changed rows: quotes=%d payloads=%d runs=%d", quotes, payloads, runs)
	}
}

func TestCleanupBySizeExpiredBudgetSkipsRowChangesButStillVacuumChecks(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-expired-budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"legacy":true}'), ('QQQ', ?, 'test', '{"legacy":true}')`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	result, err := repository.CleanupBySize(1, 1, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"raw_json_cleared", "superseded_quotes", "superseded_options", "stale_runs"} {
		if got := result[key]; got != int64(0) {
			t.Errorf("result[%s]=%v, want 0 with an expired budget", key, got)
		}
	}
	if result["vacuumed"] != true {
		t.Fatalf("expired-budget cleanup should still follow Python's size-triggered VACUUM path: %#v", result)
	}
	var rows, payloads int
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&rows, &payloads); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || payloads != 2 {
		t.Fatalf("expired-budget cleanup changed rows: rows=%d payloads=%d", rows, payloads)
	}
}

func TestCleanupBySizeAllowsConcurrentSnapshotReaderAndWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "size-cleanup-concurrent-io.db")
	cleaner, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleaner.Close()
	workload, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer workload.Close()

	const seedRows = 2000
	tx, err := cleaner.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range seedRows {
		stamp := storedTimestamp(base.Add(time.Duration(i) * time.Second))
		if _, err := tx.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('QQQ', ?, 'test')`, stamp); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	cleanupResult := make(chan error, 1)
	writerResult := make(chan error, 1)
	readerResult := make(chan error, 1)
	go func() {
		<-start
		_, cleanupErr := cleaner.CleanupBySize(1, 25, 30*time.Second)
		cleanupResult <- cleanupErr
	}()
	go func() {
		<-start
		writeTime := time.Now().UTC()
		for i := range 100 {
			stamp := storedTimestamp(writeTime.Add(time.Duration(i) * time.Microsecond))
			if _, writeErr := workload.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, price)
VALUES ('QQQ', ?, 'concurrent-test', ?)`, stamp, float64(i+1)); writeErr != nil {
				writerResult <- writeErr
				return
			}
		}
		writerResult <- nil
	}()
	go func() {
		<-start
		for range 300 {
			var rows int
			if readErr := workload.db.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&rows); readErr != nil {
				readerResult <- readErr
				return
			}
			if rows == 0 {
				readerResult <- fmt.Errorf("concurrent cleanup temporarily removed every quote snapshot")
				return
			}
		}
		readerResult <- nil
	}()
	close(start)

	if err := <-writerResult; err != nil {
		t.Fatalf("concurrent writer failed: %v", err)
	}
	if err := <-readerResult; err != nil {
		t.Fatalf("concurrent reader failed: %v", err)
	}
	if err := <-cleanupResult; err != nil {
		t.Fatalf("concurrent cleanup failed: %v", err)
	}

	var remaining int
	var latestPrice float64
	if err := workload.db.QueryRow(`SELECT COUNT(*), price FROM quote_snapshots
WHERE symbol='QQQ' AND fetched_at=(SELECT MAX(fetched_at) FROM quote_snapshots WHERE symbol='QQQ')`).Scan(&remaining, &latestPrice); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || latestPrice != 100 {
		t.Fatalf("latest concurrent write was not preserved: matching rows=%d price=%v", remaining, latestPrice)
	}
}

func TestCleanupBySizeStopsAfterRawJSONMeetsLiveTarget(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-target-stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	const rowCount = 12
	const payloadSize = 64 * 1024
	payload := strings.Repeat("x", payloadSize)
	tx, err := repository.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range rowCount {
		stamp := time.Date(2025, time.January, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', ?)`, stamp, payload); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	beforeLive, err := repository.DatabaseLiveBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`UPDATE quote_snapshots SET raw_json=NULL`); err != nil {
		t.Fatal(err)
	}
	afterRawLive, err := repository.DatabaseLiveBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`UPDATE quote_snapshots SET raw_json=?`, payload); err != nil {
		t.Fatal(err)
	}

	maxBytes := (afterRawLive*5)/4 + 4096
	target := int64(float64(maxBytes) * sizeCleanupTargetRatio)
	if target < afterRawLive || maxBytes >= beforeLive {
		t.Fatalf("fixture does not separate raw-only and snapshot cleanup: before=%d after_raw=%d limit=%d target=%d",
			beforeLive, afterRawLive, maxBytes, target)
	}
	result, err := repository.CleanupBySize(maxBytes, 2, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["raw_json_cleared"] != int64(rowCount) || result["superseded_quotes"] != int64(0) {
		t.Fatalf("cleanup did not stop after raw JSON met target: %#v", result)
	}
	var quotes, payloads int
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&quotes, &payloads); err != nil {
		t.Fatal(err)
	}
	if quotes != rowCount || payloads != 0 {
		t.Fatalf("snapshot history changed after raw-only cleanup: quotes=%d payloads=%d, want %d and 0", quotes, payloads, rowCount)
	}
}

func TestCleanupBySizeNoopReportsPythonCompatiblePhysicalSize(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-noop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := repository.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 80 {
		if _, err := tx.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', zeroblob(32768))`, stamp); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`DELETE FROM quote_snapshots`); err != nil {
		t.Fatal(err)
	}

	physicalBytes, err := repository.DatabaseSizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	liveBytes, err := repository.DatabaseLiveBytes()
	if err != nil {
		t.Fatal(err)
	}
	if physicalBytes <= liveBytes {
		t.Fatalf("fixture must have reusable SQLite pages: physical=%d live=%d", physicalBytes, liveBytes)
	}

	result, err := repository.CleanupBySize(0, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["before_bytes"] != physicalBytes || result["live_bytes"] != physicalBytes {
		t.Fatalf("no-op size metrics=%#v, want both before_bytes and live_bytes to report Python-compatible physical size %d", result, physicalBytes)
	}
}

func TestCleanupBySizeClearsRawJSONDeletesStaleRunsAndThinsSnapshots(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	now := time.Now().UTC()
	oldest := now.Add(-72 * time.Hour).Format(time.RFC3339Nano)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"oldest":true}'), ('QQQ', ?, 'test', '{"old":true}'), ('QQQ', ?, 'test', '{"recent":true}');
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, provider, raw_json)
VALUES ('QQQ', '2030-01-18', ?, 'QQQ300118C00100000', 'call', 'test', '{"oldest":true}'),
       ('QQQ', '2030-01-18', ?, 'QQQ300118C00100000', 'call', 'test', '{"old":true}'),
       ('QQQ', '2030-01-18', ?, 'QQQ300118C00100000', 'call', 'test', '{"recent":true}');
`, oldest, old, recent, oldest, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO refresh_runs(symbol, started_at, status)
VALUES ('QQQ', ?, 'success'), ('QQQ', ?, 'success')`, oldest, recent); err != nil {
		t.Fatal(err)
	}

	result, err := repository.CleanupBySize(1, 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{
		"raw_json_cleared":   6,
		"superseded_quotes":  2,
		"superseded_options": 2,
		"stale_runs":         1,
	} {
		if got := result[key]; got != want {
			t.Errorf("result[%s]=%v, want %d; result=%#v", key, got, want, result)
		}
	}
	if result["vacuumed"] != true {
		t.Errorf("vacuumed=%v, want true", result["vacuumed"])
	}

	var count int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM quote_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("quote row count=%d, want one latest row", count)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM option_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("option row count=%d, want one latest row", count)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM refresh_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("refresh run count=%d, want one recent row", count)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE raw_json IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("quote raw_json count=%d, want zero", count)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM option_snapshots WHERE raw_json IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("option raw_json count=%d, want zero", count)
	}
}

func TestCleanupBySizePrunesOldChanAdjustedHistoryAfterSizeLimit(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-chan-adjusted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fetchedAt := time.Now().UTC()
	start := time.Date(2010, 1, 4, 0, 0, 0, 0, time.UTC)
	bars := make([]map[string]any, 0, 100)
	for index := 0; index < 100; index++ {
		date := start.AddDate(0, 0, index).Format(time.DateOnly)
		bars = append(bars, map[string]any{
			"date": date, "open": 50.0, "high": 55.0, "low": 45.0, "close": 52.0, "volume": 1000.0,
			"raw_open": 100.0, "raw_high": 110.0, "raw_low": 90.0, "raw_close": 104.0, "raw_volume": 1000.0, "adjustment_factor": 0.5,
		})
	}
	if err := repository.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", bars, fetchedAt); err != nil {
		t.Fatal(err)
	}
	result, err := repository.CleanupBySize(1, 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := result["chan_adjusted_bars_pruned"]; got != int64(len(bars)) {
		t.Fatalf("pruned adjusted bars=%v, want %d (cleanup result=%#v)", got, len(bars), result)
	}
	stored, err := repository.LatestChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", 3000)
	if err != nil || len(stored["bars"].([]map[string]any)) != 0 {
		t.Fatalf("adjusted history remains after size cleanup: stored=%#v err=%v", stored, err)
	}
}

func TestCleanupBySizePrunesOldChanStructureRevisionsButKeepsRecentAudit(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-chan-revisions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	oldAt := time.Now().UTC().AddDate(0, 0, -3*365)
	base := ChanStructureSnapshot{Symbol: "QQQ", Timeframe: "1d", RuleVersion: "preview.1", InputFingerprint: "old-input", BarFingerprints: map[string]string{"2010-01-04": "bar-a"}, ConfirmedStructures: map[string]json.RawMessage{"segment|level=0|start=2010-01-04|end=2010-01-04": json.RawMessage(`{"end_price":100}`)}, UpdatedAt: oldAt}
	if _, err := repository.RecordChanStructureSnapshot(base); err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.InputFingerprint = "old-input-revised"
	changed.BarFingerprints = map[string]string{"2010-01-04": "bar-b"}
	changed.ConfirmedStructures = map[string]json.RawMessage{"segment|level=0|start=2010-01-04|end=2010-01-04": json.RawMessage(`{"end_price":101}`)}
	changed.UpdatedAt = oldAt.Add(time.Minute)
	if revision, err := repository.RecordChanStructureSnapshot(changed); err != nil || revision == nil {
		t.Fatalf("create old revision=%#v err=%v", revision, err)
	}
	changed.InputFingerprint = "recent-input"
	changed.BarFingerprints = map[string]string{"2010-01-04": "bar-c"}
	changed.ConfirmedStructures = map[string]json.RawMessage{"segment|level=0|start=2010-01-04|end=2010-01-04": json.RawMessage(`{"end_price":102}`)}
	changed.UpdatedAt = time.Now().UTC()
	if revision, err := repository.RecordChanStructureSnapshot(changed); err != nil || revision == nil {
		t.Fatalf("create recent revision=%#v err=%v", revision, err)
	}
	newRule := changed
	newRule.RuleVersion = "preview.2"
	newRule.InputFingerprint = "new-rule-input"
	newRule.UpdatedAt = time.Now().UTC()
	if _, err := repository.RecordChanStructureSnapshot(newRule); err != nil {
		t.Fatalf("preserve new rule snapshot: %v", err)
	}
	result, err := repository.CleanupBySize(1, 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["chan_structure_revisions_pruned"] != int64(1) {
		t.Fatalf("revision prune count=%v, want 1: %#v", result["chan_structure_revisions_pruned"], result)
	}
	var count int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM chan_structure_revisions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("revision rows=%d, want the recent audit preserved", count)
	}
	var snapshots int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM chan_structure_snapshots WHERE symbol='QQQ' AND timeframe='1d'`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 {
		t.Fatalf("structure snapshot count=%d, want one snapshot for each of two rule versions", snapshots)
	}
}

func TestThinSupersededMatchesPythonRetentionLadder(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-ladder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	now := time.Now().UTC()
	old := now.Add(-30 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	newest := now.Format(time.RFC3339Nano)
	for _, stamp := range []string{old, recent, newest} {
		if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
			VALUES ('QQQ', ?, 'test')`, stamp); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(time.Minute)
	floor := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	deleted, err := repository.thinSuperseded("quote_snapshots", []string{"symbol"}, floor, 10, 5000, deadline)
	if err != nil || deleted != 1 {
		t.Fatalf("protected cleanup deleted=%d err=%v, want only the row older than the protection floor", deleted, err)
	}
	deleted, err = repository.thinSuperseded("quote_snapshots", []string{"symbol"}, floor, 13, 5000, deadline)
	if err != nil || deleted != 0 {
		t.Fatalf("second protected cleanup deleted=%d err=%v, want 0", deleted, err)
	}
	deleted, err = repository.thinSuperseded("quote_snapshots", []string{"symbol"}, noCleanupFloor, 0, 5000, deadline)
	if err != nil || deleted != 1 {
		t.Fatalf("unprotected cleanup deleted=%d err=%v, want 1", deleted, err)
	}
	var remaining int
	var remainingStamp string
	if err := repository.db.QueryRow(`SELECT COUNT(*), MAX(fetched_at) FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&remaining, &remainingStamp); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || remainingStamp != newest {
		t.Fatalf("remaining quotes=%d newest=%q, want exactly latest %q", remaining, remainingStamp, newest)
	}
}

func TestDatabaseSizeBytesIncludesSQLiteSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar-size.db")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	tx, err := repository.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		if _, err := tx.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', zeroblob(32768))`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty WAL sidecar, info=%v err=%v", info, err)
	}

	var want int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err == nil {
			want += info.Size()
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	got, err := repository.DatabaseSizeBytes()
	if err != nil || got != want {
		t.Fatalf("DatabaseSizeBytes()=%d err=%v, want main+WAL+SHM total %d", got, err, want)
	}
}

func TestConcurrentCleanupBySizeAcrossSQLiteHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-size-cleanup.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	tx, err := first.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range 40 {
		stamp := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', zeroblob(16384))`, stamp); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	type cleanupResult struct {
		result map[string]any
		err    error
	}
	start := make(chan struct{})
	results := make(chan cleanupResult, 2)
	for _, repository := range []*SQLite{first, second} {
		go func(repository *SQLite) {
			<-start
			result, err := repository.CleanupBySize(1, 3, 30*time.Second)
			results <- cleanupResult{result: result, err: err}
		}(repository)
	}
	close(start)

	var cleared, deleted int64
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent CleanupBySize() failed: %v", result.err)
		}
		cleared += result.result["raw_json_cleared"].(int64)
		deleted += result.result["superseded_quotes"].(int64)
	}
	if cleared != 40 || deleted != 39 {
		t.Fatalf("concurrent cleanup cleared=%d deleted=%d, want 40 and 39", cleared, deleted)
	}
	var remaining, payloads int
	if err := first.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&remaining, &payloads); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || payloads != 0 {
		t.Fatalf("remaining quotes=%d payloads=%d, want one latest row with no raw JSON", remaining, payloads)
	}
}
