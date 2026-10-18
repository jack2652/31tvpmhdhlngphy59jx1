package chananalysis

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

// TestIncrementalAnalyzerMatchesCompleteAdjustedResult compares the entire
// public result for every append-only prefix. This intentionally includes both
// daily and weekly frames, legacy compatibility fields, warnings, parameters,
// and result metadata; checking a hand-picked subset can miss stale derived
// fields even when the primary recursive frame happens to match.
func TestIncrementalAnalyzerMatchesCompleteAdjustedResult(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2024, 1, 2, 16, 0, 0, 0, location)
	bars := make([]Bar, 0, 220)
	for len(bars) < 220 {
		if marketcalendar.IsTradingDay(day) {
			i := len(bars)
			base := 100 + 12*math.Sin(float64(i)*0.31) + 5*math.Sin(float64(i)*0.071) + 0.01*float64(i)
			bar := Bar{Date: day.Format(time.DateOnly), Open: base - 0.15, High: base + 0.9, Low: base - 0.9, Close: base + 0.1, Volume: 1000 + float64(i), AdjustmentFactor: 1}
			switch i % 23 {
			case 7:
				bar.High = base + 1.7 // expand a containment candidate
			case 15:
				bar.Low = base - 1.8 // reverse the containment direction
			}
			bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose = bar.Open, bar.High, bar.Low, bar.Close
			bars = append(bars, bar)
		}
		day = day.AddDate(0, 0, 1)
	}

	state := NewIncrementalAnalyzer("PARITY")
	maxDailyFractals, maxDailyStrokes, maxDailySegments, maxCenters, maxSignals := 0, 0, 0, 0, 0
	maxWeeklyBars := 0
	for i, bar := range bars {
		cutoff := dayForBarClose(bar.Date, location)
		got, err := state.Append(bar, cutoff)
		if err != nil {
			t.Fatalf("append prefix %d (%s): %v", i+1, bar.Date, err)
		}
		want := AnalyzeAdjusted("PARITY", bars[:i+1], nil, cutoff)
		if len(want.Daily.FractalsV4) > maxDailyFractals {
			maxDailyFractals = len(want.Daily.FractalsV4)
		}
		if len(want.Daily.StrokesV4) > maxDailyStrokes {
			maxDailyStrokes = len(want.Daily.StrokesV4)
		}
		if len(want.Daily.Segments) > maxDailySegments {
			maxDailySegments = len(want.Daily.Segments)
		}
		if len(want.Daily.RecursiveCenters) > maxCenters {
			maxCenters = len(want.Daily.RecursiveCenters)
		}
		if len(want.Daily.SignalsV4) > maxSignals {
			maxSignals = len(want.Daily.SignalsV4)
		}
		if len(want.Weekly.Candles) > maxWeeklyBars {
			maxWeeklyBars = len(want.Weekly.Candles)
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("incremental result differs from full adjusted recomputation at prefix %d (%s)\nincremental: %s\nfull:        %s", i+1, bar.Date, gotJSON, wantJSON)
		}
	}
	t.Logf("parity fixture maxima: fractals=%d strokes=%d segments=%d weekly_bars=%d recursive_centers=%d signals=%d", maxDailyFractals, maxDailyStrokes, maxDailySegments, maxWeeklyBars, maxCenters, maxSignals)
	if maxDailyFractals == 0 || maxDailyStrokes == 0 || maxDailySegments == 0 || maxWeeklyBars == 0 {
		t.Fatalf("parity fixture did not exercise key derived structures: fractals=%d strokes=%d segments=%d weekly_bars=%d recursive_centers=%d signals=%d", maxDailyFractals, maxDailyStrokes, maxDailySegments, maxWeeklyBars, maxCenters, maxSignals)
	}
}
