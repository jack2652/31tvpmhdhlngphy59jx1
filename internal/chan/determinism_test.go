package chananalysis

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

func TestAdjustedAnalysisProducesStableConfirmedStructuresForSameInputAndRule(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	day := time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	bars := make([]Bar, 0, 160)
	for len(bars) < 160 {
		if marketcalendar.IsTradingDay(day) {
			index := float64(len(bars))
			close := 100 + 0.015*index + 5*math.Sin(index*0.31) + 1.3*math.Sin(index*0.077)
			bar := Bar{Date: day.Format(time.DateOnly), Open: close - 0.2, High: close + 0.8, Low: close - 0.9, Close: close, Volume: 1000 + index}
			bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose = bar.Open, bar.High, bar.Low, bar.Close
			bar.AdjustmentFactor = 1
			bars = append(bars, bar)
		}
		day = day.AddDate(0, 0, 1)
	}
	cutoff := day.Add(12 * time.Hour)
	first := AnalyzeAdjusted("STABLE", bars, nil, cutoff)
	second := AnalyzeAdjusted("STABLE", bars, nil, cutoff)
	if first.RuleVersion != second.RuleVersion || first.RuleVersion != RecursivePreviewVersion {
		t.Fatalf("rule versions differ or are unexpected: %q / %q", first.RuleVersion, second.RuleVersion)
	}
	for _, timeframe := range []struct {
		name string
		a, b Frame
	}{{"daily", first.Daily, second.Daily}, {"weekly", first.Weekly, second.Weekly}} {
		left, err := json.Marshal(timeframe.a)
		if err != nil {
			t.Fatal(err)
		}
		right, err := json.Marshal(timeframe.b)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s confirmed/derived frame differs for identical input and rule version", timeframe.name)
		}
	}
}
