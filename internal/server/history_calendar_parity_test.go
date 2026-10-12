package server

import (
	"testing"
	"time"
)

func TestHistoryLatestDateRejectsMalformedLexicallyNewestBar(t *testing.T) {
	bars := []map[string]any{
		{"date": "2025-01-03", "close": 100.0},
		{"date": "2025-99-99", "close": 101.0},
	}

	if got := historyLatestDate(bars); !got.IsZero() {
		t.Fatalf("latest date = %s, want zero for malformed lexically newest date", got)
	}

	sevenDaysLater := time.Date(2025, time.January, 10, 12, 0, 0, 0, time.UTC)
	if historyWithinQuoteAlignmentWindow(historyLatestDate(bars), sevenDaysLater) {
		t.Fatal("malformed latest history date must not qualify for quote alignment")
	}
}
