package chananalysis

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

// This fixture uses real adjusted OHLC bars and the production structure pipeline.
func TestIncrementalAnalyzerMatchesFullResultThroughRecursiveCenter(t *testing.T) {
	bars := recursiveCenterAdjustedBars(t)
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	state := NewIncrementalAnalyzer("RECURSIVE-PARITY")
	maxStrokes, maxSegments, maxConfirmedSegments, maxCenters := 0, 0, 0, 0
	for index, bar := range bars {
		cutoff := dayForBarClose(bar.Date, location)
		got, err := state.Append(bar, cutoff)
		if err != nil {
			t.Fatalf("append prefix %d (%s): %v", index+1, bar.Date, err)
		}
		want := AnalyzeAdjusted("RECURSIVE-PARITY", bars[:index+1], nil, cutoff)
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("incremental result differs from full recompute at prefix %d (%s); incremental bytes=%d, full bytes=%d", index+1, bar.Date, len(gotJSON), len(wantJSON))
		}
		if len(want.Daily.StrokesV4) > maxStrokes {
			maxStrokes = len(want.Daily.StrokesV4)
		}
		if len(want.Daily.Segments) > maxSegments {
			maxSegments = len(want.Daily.Segments)
		}
		confirmed := 0
		for _, segment := range want.Daily.Segments {
			if segment.Confirmed {
				confirmed++
			}
		}
		if confirmed > maxConfirmedSegments {
			maxConfirmedSegments = confirmed
		}
		if len(want.Daily.RecursiveCenters) > maxCenters {
			maxCenters = len(want.Daily.RecursiveCenters)
		}
	}
	if maxStrokes < 20 || maxSegments < 4 || maxConfirmedSegments < 3 || maxCenters < 1 {
		t.Fatalf("fixture lacks recursive coverage: strokes=%d segments=%d confirmed_segments=%d recursive_centers=%d", maxStrokes, maxSegments, maxConfirmedSegments, maxCenters)
	}
	t.Logf("matched %d prefixes: max strokes=%d segments=%d confirmed_segments=%d recursive_centers=%d", len(bars), maxStrokes, maxSegments, maxConfirmedSegments, maxCenters)
}

func recursiveCenterAdjustedBars(t *testing.T) []Bar {
	t.Helper()
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	pivots := []float64{9, 4, 5, 14, 10, 11, 7, 13, 9, 15, 6, 6.8, 6.2, 12, 5.5, 11, 4, 13, 12.5, 12.9, 7, 14, 9, 15, 6, 6.5, 6.4, 5.5, 6.5}
	day := time.Date(2020, 1, 2, 16, 0, 0, 0, location)
	bars := make([]Bar, 0, (len(pivots)-1)*6+1)
	appendBar := func(price, spread float64) {
		for !marketcalendar.IsTradingDay(day) {
			day = day.AddDate(0, 0, 1)
		}
		bar := Bar{Date: day.Format(time.DateOnly), Open: price, High: price + spread, Low: price - spread, Close: price, Volume: 100, AdjustmentFactor: 1}
		bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose = bar.Open, bar.High, bar.Low, bar.Close
		bars = append(bars, bar)
		day = day.AddDate(0, 0, 1)
	}
	for index := 0; index+1 < len(pivots); index++ {
		for step := 0; step < 6; step++ {
			ratio := math.Pow(float64(step)/6, 2)
			point := pivots[index] + (pivots[index+1]-pivots[index])*ratio
			appendBar(100+.5*point, .002)
		}
	}
	appendBar(100+.5*pivots[len(pivots)-1], .2)
	return bars
}
