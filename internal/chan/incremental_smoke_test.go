package chananalysis

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

func TestIncrementalAnalyzerMatchesEveryAdjustedPrefix(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	day := time.Date(2025, 1, 2, 16, 0, 0, 0, location)
	bars := make([]Bar, 0, 100)
	for len(bars) < 100 {
		if marketcalendar.IsTradingDay(day) {
			i := len(bars)
			center := 100 + 4*math.Sin(float64(i)*0.41) + float64(i)*0.01
			b := Bar{Date: day.Format(time.DateOnly), Open: center, High: center + 0.8, Low: center - 0.8, Close: center, Volume: 1000, AdjustmentFactor: 1}
			if i%17 == 9 {
				b.High = center + 1.4
				b.Low = center - 0.5
			} // inclusion and directional merge transitions
			b.RawOpen, b.RawHigh, b.RawLow, b.RawClose = b.Open, b.High, b.Low, b.Close
			bars = append(bars, b)
		}
		day = day.AddDate(0, 0, 1)
	}
	state := NewIncrementalAnalyzer("INCR")
	for i, b := range bars {
		cutoff := dayForBarClose(b.Date, location)
		got, err := state.Append(b, cutoff)
		if err != nil {
			t.Fatalf("append %d %s: %v", i, b.Date, err)
		}
		want := AnalyzeAdjusted("INCR", bars[:i+1], nil, cutoff)
		if !sameRecursiveFrame(got.Daily, want.Daily) {
			t.Fatalf("prefix %d %s differs from full adjusted recomputation", i, b.Date)
		}
	}
	metrics := state.Metrics()
	if metrics.IncrementalUpdates != uint64(len(bars)) || metrics.ComponentStateReuses != uint64(len(bars)) || metrics.ResultCacheHits != 0 || metrics.MACDPoints != len(bars) {
		t.Fatalf("unexpected incremental metrics %+v", metrics)
	}
}

func TestIncrementalAnalyzerRejectsNonAppendUpdatesWithoutMutation(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bar := adjustedReplayBar("2026-01-02", 100)
	state := NewIncrementalAnalyzer("INCR")
	if _, err := state.Append(bar, dayForBarClose(bar.Date, location)); err != nil {
		t.Fatal(err)
	}
	before := state.Metrics()
	if _, err := state.Append(bar, dayForBarClose(bar.Date, location)); err == nil {
		t.Fatal("duplicate append was accepted")
	}
	if got := state.Metrics(); got != before {
		t.Fatalf("rejected update mutated state: before=%+v after=%+v", before, got)
	}
	if _, ok := state.ReuseLastResult(); !ok || state.Metrics().ResultCacheHits != 1 {
		t.Fatalf("existing result should count as a result-cache hit; metrics=%+v", state.Metrics())
	}
}

func sameRecursiveFrame(a, b Frame) bool {
	left, err := json.Marshal(struct {
		Candles   []Bar
		Merged    []MergedCandle
		Fractals  []Fractal
		Strokes   []Stroke
		Segments  []Segment
		Centers   []RecursiveCenter
		Levels    []LevelAnalysis
		Strengths []Strength
		Signals   []V4Signal
		MACD      []MACDPoint
		Trend     string
		Context   TrendContext
	}{a.Candles, a.MergedCandles, a.FractalsV4, a.StrokesV4, a.Segments, a.RecursiveCenters, a.RecursiveLevels, a.Strengths, a.SignalsV4, a.MACD, a.TrendState, a.TrendContext})
	if err != nil {
		return false
	}
	right, err := json.Marshal(struct {
		Candles   []Bar
		Merged    []MergedCandle
		Fractals  []Fractal
		Strokes   []Stroke
		Segments  []Segment
		Centers   []RecursiveCenter
		Levels    []LevelAnalysis
		Strengths []Strength
		Signals   []V4Signal
		MACD      []MACDPoint
		Trend     string
		Context   TrendContext
	}{b.Candles, b.MergedCandles, b.FractalsV4, b.StrokesV4, b.Segments, b.RecursiveCenters, b.RecursiveLevels, b.Strengths, b.SignalsV4, b.MACD, b.TrendState, b.TrendContext})
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

func dayForBarClose(value string, location *time.Location) time.Time {
	day, _ := time.ParseInLocation(time.DateOnly, value, location)
	return day.Add(16 * time.Hour)
}

func adjustedReplayBar(value string, price float64) Bar {
	return Bar{Date: value, Open: price, High: price + 1, Low: price - 1, Close: price, Volume: 100, RawOpen: price, RawHigh: price + 1, RawLow: price - 1, RawClose: price, AdjustmentFactor: 1}
}
