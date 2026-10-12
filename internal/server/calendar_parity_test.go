package server

import (
	"testing"
	"time"
)

func TestMarketRefreshIntervalUsesNYSECalendar(t *testing.T) {
	tests := []struct {
		instant string
		want    time.Duration
	}{
		{"2025-01-09T10:00:00-05:00", 5 * time.Minute},  // Carter national day of mourning
		{"2021-12-31T10:00:00-05:00", 20 * time.Second}, // Saturday New Year's Day did not close Friday
		{"2026-11-27T12:59:00-05:00", 20 * time.Second}, // early close day, before 13:00
		{"2026-11-27T13:00:00-05:00", time.Minute},      // regular session has ended
		{"1992-11-27T13:59:00-05:00", 20 * time.Second}, // XNYS historical 14:00 early close
		{"1992-11-27T14:00:00-05:00", time.Minute},      // regular session has ended
	}
	for _, test := range tests {
		instant, err := time.Parse(time.RFC3339, test.instant)
		if err != nil {
			t.Fatalf("parse %q: %v", test.instant, err)
		}
		if got := marketRefreshInterval(instant); got != test.want {
			t.Errorf("marketRefreshInterval(%s) = %s, want %s", test.instant, got, test.want)
		}
	}
}
