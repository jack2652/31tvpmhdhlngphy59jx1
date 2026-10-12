package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestLatestPayloadTreatsInvalidPythonCachePayloadsAsMissing(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "invalid-payloads.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	cases := []struct {
		table   string
		payload string
	}{
		{"price_extremes", "{"},
		{"price_extremes", "null"},
		{"price_extremes", "[]"},
		{"price_extremes", `"text"`},
		{"beta_snapshots", "{"},
		{"beta_snapshots", "null"},
		{"beta_snapshots", "[]"},
		{"beta_snapshots", "42"},
		{"earnings_snapshots", "{"},
		{"earnings_snapshots", "null"},
		{"earnings_snapshots", "[]"},
		{"earnings_snapshots", `{"dates":"2030-01-18"}`},
		{"earnings_snapshots", `{"other":[]}`},
	}
	for index, test := range cases {
		t.Run(fmt.Sprintf("%s/%d", test.table, index), func(t *testing.T) {
			symbol := fmt.Sprintf("BAD%d", index)
			if _, err := repository.db.Exec(`INSERT INTO `+test.table+`(symbol, payload, fetched_at) VALUES (?, ?, ?)`, symbol, test.payload, "2030-01-18T15:00:00Z"); err != nil {
				t.Fatal(err)
			}
			got, err := repository.LatestPayload(test.table, symbol)
			if err != nil {
				t.Fatalf("LatestPayload returned an error for an invalid cache entry: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("LatestPayload=%#v, want empty map cache miss", got)
			}
		})
	}
}
