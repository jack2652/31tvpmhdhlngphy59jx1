package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRetainsSixMonthsOfIntradayHistory(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "intraday-retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	now := time.Now().UTC()
	retained := now.AddDate(0, 0, -200).Format("2006-01-02 15:04")
	expired := now.AddDate(0, 0, -211).Format("2006-01-02 15:04")
	if _, err := repository.db.Exec(`INSERT INTO intraday_history(symbol, interval, bar_time, open, high, low, close, volume, fetched_at)
VALUES ('AAPL', '15m', ?, 100, 102, 99, 101, 1000, '2026-10-23T00:00:00Z'),
       ('AAPL', '15m', ?, 100, 102, 99, 101, 1000, '2026-10-23T00:00:00Z')`, retained, expired); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.Cleanup(1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted["intraday_history"] != 1 {
		t.Fatalf("deleted intraday rows=%d, want only the row older than 210 days", deleted["intraday_history"])
	}
	var count int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM intraday_history WHERE symbol='AAPL'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining intraday rows=%d, want one row within the 210-day retention window", count)
	}
}
