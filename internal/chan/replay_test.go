package chananalysis

import (
	"testing"
	"time"
)

func TestReplayAdjustedUsesOnlyChronologicalClosedPrefixes(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bars := []Bar{}
	for index := 0; index < 60; index++ {
		date := time.Date(2026, 1, 2, 0, 0, 0, 0, location).AddDate(0, 0, index)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		price := 100 + float64(len(bars)%9) - float64(len(bars)%5)*0.3
		bar := Bar{Date: date.Format(time.DateOnly), Open: price, High: price + 1, Low: price - 1, Close: price, Volume: 100}
		bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose, bar.AdjustmentFactor = bar.Open, bar.High, bar.Low, bar.Close, 1
		bars = append(bars, bar)
	}
	points, err := ReplayAdjusted("REPLAY", bars, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != len(bars) {
		t.Fatalf("replay points=%d want one per closed bar prefix=%d", len(points), len(bars))
	}
	for index, point := range points {
		if point.AsOf != bars[index].Date || point.Rule != RecursivePreviewVersion {
			t.Fatalf("replay point[%d]=%+v, want as-of=%s rule=%s", index, point, bars[index].Date, RecursivePreviewVersion)
		}
		for _, signal := range point.Signals {
			if signal.ObservedAt == "" || signal.ObservedAt > point.AsOf {
				t.Fatalf("signal visible beyond prefix[%d] %s: %+v", index, point.AsOf, signal)
			}
		}
		match, err := SameReplayFrame(bars[:index+1], time.Time{}, point, "REPLAY")
		if err != nil || !match {
			t.Fatalf("prefix[%d] replay differs from full recomputation: match=%v err=%v", index, match, err)
		}
	}
}

func TestReplayAdjustedRejectsDuplicateOrOutOfOrderInput(t *testing.T) {
	bars := []Bar{{Date: "2026-01-06"}, {Date: "2026-01-05"}}
	if _, err := ReplayAdjusted("REPLAY", bars, time.Now()); err == nil {
		t.Fatal("out-of-order input should be rejected rather than replayed with lookahead")
	}
}
