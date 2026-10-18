package chananalysis

import (
	"testing"
)

// These fixtures exercise the complete adjusted OHLC pair used by recursive
// analysis. A verified split, reverse split, or cash adjustment must preserve
// valid price ranges and must not break the normalized structure chain.
func TestCorporateActionFixturesPreserveAdjustedOHLCAndChains(t *testing.T) {
	tests := []struct {
		name     string
		bars     []Bar
		wantKind string
	}{
		{
			name: "forward split",
			bars: []Bar{
				{Date: "2026-01-05", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 100, RawHigh: 102, RawLow: 98, RawClose: 100, AdjustmentFactor: .5},
				{Date: "2026-01-06", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 50, RawHigh: 51, RawLow: 49, RawClose: 50, AdjustmentFactor: 1},
			},
			wantKind: "split_candidate",
		},
		{
			name: "reverse split",
			bars: []Bar{
				{Date: "2026-01-05", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 50, RawHigh: 51, RawLow: 49, RawClose: 50, AdjustmentFactor: 1},
				{Date: "2026-01-06", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 100, RawHigh: 102, RawLow: 98, RawClose: 100, AdjustmentFactor: .5},
			},
			wantKind: "split_candidate",
		},
		{
			name: "cash dividend adjustment",
			bars: []Bar{
				{Date: "2026-01-05", Open: 99, High: 99.99, Low: 98.01, Close: 99, RawOpen: 100, RawHigh: 101, RawLow: 99, RawClose: 100, AdjustmentFactor: .99},
				{Date: "2026-01-06", Open: 99, High: 100, Low: 98, Close: 99, RawOpen: 99, RawHigh: 100, RawLow: 98, RawClose: 99, AdjustmentFactor: 1},
			},
			wantKind: "dividend_or_cash_adjustment_candidate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, bar := range test.bars {
				if !(bar.Low <= bar.Open && bar.Open <= bar.High && bar.Low <= bar.Close && bar.Close <= bar.High) {
					t.Fatalf("adjusted OHLC range is invalid: %+v", bar)
				}
				if !validAdjustedPair(bar) {
					t.Fatalf("adjusted and raw OHLC do not agree with factor: %+v", bar)
				}
			}

			quality := dailyQuality(test.bars, test.bars, 0, 0, 0, 0)
			if quality.CorporateActionSuspected || quality.Status != "ok" {
				t.Fatalf("verified event degraded normalized history: %+v", quality)
			}
			if len(quality.CorporateActionEvents) != 1 {
				t.Fatalf("events=%+v, want one classified action", quality.CorporateActionEvents)
			}
			event := quality.CorporateActionEvents[0]
			if event.Kind != test.wantKind || !event.FactorVerified || event.Status != "factor_adjusted" {
				t.Fatalf("event=%+v, want verified %s", event, test.wantKind)
			}
			chained, warnings := chainDailyBars(test.bars)
			if len(chained) != 2 || chained[0].Chain != chained[1].Chain || len(warnings) != 0 {
				t.Fatalf("chain=%+v warnings=%v, verified action must not break normalized history", chained, warnings)
			}
		})
	}
}
