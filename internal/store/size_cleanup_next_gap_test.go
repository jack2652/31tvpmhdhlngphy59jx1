package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupBySizeVacuumOnlyWhenWALPushesPhysicalSizeOverLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-only-over-limit.db")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	repository.db.SetMaxOpenConns(1)
	if _, err := repository.db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}

	stamp := storedTimestamp(time.Now().UTC().Add(-48 * time.Hour))
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('WAL-OVER-LIMIT', ?, 'test', ?)`, stamp, strings.Repeat("x", 2<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`DELETE FROM quote_snapshots WHERE symbol='WAL-OVER-LIMIT'`); err != nil {
		t.Fatal(err)
	}
	old := storedTimestamp(time.Now().UTC().Add(-48 * time.Hour))
	recent := storedTimestamp(time.Now().UTC().Add(-time.Hour))
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('KEEP', ?, 'test', '{"keep":"old"}'), ('KEEP', ?, 'test', '{"keep":"recent"}')`, old, recent); err != nil {
		t.Fatal(err)
	}
	mainInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if walInfo.Size() <= 1<<20 {
		t.Fatalf("fixture WAL=%d bytes, want more than 1 MiB", walInfo.Size())
	}
	physical, err := repository.DatabaseSizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	limit := physical - 4096
	if mainInfo.Size() >= limit || physical <= limit {
		t.Fatalf("fixture does not isolate WAL-triggered limit: main=%d physical=%d limit=%d", mainInfo.Size(), physical, limit)
	}
	live, err := repository.DatabaseLiveBytes()
	if err != nil {
		t.Fatal(err)
	}
	target := int64(float64(limit) * sizeCleanupTargetRatio)
	if live > target {
		t.Fatalf("fixture live bytes=%d, want within cleanup target %d", live, target)
	}
	free, err := freeBytes(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if vacuumSpaceInsufficient(mainInfo.Size(), free) {
		t.Skipf("temporary filesystem has insufficient space for VACUUM: database=%d free=%d", mainInfo.Size(), free)
	}

	result, err := repository.CleanupBySize(limit, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["before_bytes"] != physical {
		t.Fatalf("before_bytes=%v, want main+WAL+SHM size %d", result["before_bytes"], physical)
	}
	for _, key := range []string{"raw_json_cleared", "superseded_options", "superseded_quotes", "stale_runs"} {
		if result[key] != int64(0) {
			t.Errorf("result[%s]=%v, want 0 because live bytes are already under target", key, result[key])
		}
	}
	if result["vacuumed"] != true {
		t.Fatalf("vacuumed=%v, want VACUUM to reclaim WAL-only physical excess", result["vacuumed"])
	}
	if after := result["after_bytes"].(int64); after > limit {
		t.Errorf("after_bytes=%d, want WAL reclaimed below limit %d", after, limit)
	}
	var rows, payloads int
	if err := repository.db.QueryRow(`SELECT COUNT(*), COUNT(raw_json) FROM quote_snapshots WHERE symbol='KEEP'`).Scan(&rows, &payloads); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || payloads != 2 {
		t.Fatalf("retained snapshots=%d payloads=%d, want both business rows and their payloads intact", rows, payloads)
	}
}
