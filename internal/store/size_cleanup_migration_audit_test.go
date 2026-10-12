package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupBySizeBudgetInterruptsBlockedWriteWithoutChangingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-budget-lock.db")
	cleaner, err := OpenWithOptions(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleaner.Close()
	workload, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer workload.Close()

	if _, err := cleaner.db.Exec(`PRAGMA busy_timeout=400`); err != nil {
		t.Fatal(err)
	}
	if _, err := cleaner.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'test', '{"legacy":true}')`, storedTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	tx, err := workload.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO refresh_runs(symbol, started_at, status)
VALUES ('SPY', ?, 'running')`, storedTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	result, err := cleaner.CleanupBySize(1, 1, 25*time.Millisecond)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("CleanupBySize() error = %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("CleanupBySize() took %s for a 25ms budget while blocked by a writer", elapsed)
	}
	var busyTimeout int
	if err := cleaner.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 30000 {
		t.Fatalf("cleanup left busy_timeout=%dms, want the default 30000ms", busyTimeout)
	}
	if result["raw_json_cleared"] != int64(0) || result["stale_runs"] != int64(0) {
		t.Fatalf("cleanup changed rows after its deadline: %#v", result)
	}
	var rows, payloads int
	if err := cleaner.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots`).Scan(&rows, &payloads); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || payloads != 1 {
		t.Fatalf("quote rows=%d payloads=%d, want the original row and payload preserved", rows, payloads)
	}
}
