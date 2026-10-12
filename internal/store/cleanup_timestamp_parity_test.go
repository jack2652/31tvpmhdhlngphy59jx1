package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRetentionPreservesPythonMicrosecondComparison(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "cleanup-microseconds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	cutoff := "2026-10-10T00:00:00.000001+00:00"
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('EXPIRED', '2026-10-10T00:00:00.000000+00:00', 'test'),
       ('AT_CUTOFF', '2026-10-10T00:00:00.000001+00:00', 'test')`); err != nil {
		t.Fatal(err)
	}

	deleted, err := repository.cleanupBefore(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if deleted["quotes"] != 1 {
		t.Fatalf("deleted quotes=%d, want the Python-ISO timestamp one microsecond before cutoff", deleted["quotes"])
	}
	if _, found, err := repository.LatestQuote("EXPIRED"); err != nil || found {
		t.Fatalf("expired quote found=%v err=%v, want deleted", found, err)
	}
	if _, found, err := repository.LatestQuote("AT_CUTOFF"); err != nil || !found {
		t.Fatalf("at-cutoff quote found=%v err=%v, want retained", found, err)
	}
}

func TestSizeCleanupProtectionFloorMatchesPythonISOOffset(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "size-cleanup-microseconds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	floor := "2026-10-10T12:00:00.000001+00:00"
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('QQQ', '2026-10-10T12:00:00.000001+00:00', 'test'),
       ('QQQ', '2026-10-10T13:00:00.000001+00:00', 'test')`); err != nil {
		t.Fatal(err)
	}

	deleted, err := repository.thinSuperseded("quote_snapshots", []string{"symbol"}, floor, 13, 10, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted rows=%d, want the exact Python-ISO protection-floor row retained", deleted)
	}
	var rows int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("remaining rows=%d, want the protected boundary row and later bucket representative", rows)
	}
}

func TestPythonISOTimestampMatchesUTCDateTimeISOFormat(t *testing.T) {
	input := time.Date(2026, 10, 10, 12, 30, 45, 123456789, time.FixedZone("UTC+8", 8*60*60))
	if got, want := pythonISOTimestamp(input), "2026-10-10T04:30:45.123456+00:00"; got != want {
		t.Fatalf("pythonISOTimestamp()=%q, want %q", got, want)
	}
}
