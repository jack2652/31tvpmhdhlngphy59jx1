package chananalysis

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

func TestCalibrateDivergenceThresholdsUsesChronologicalPartitions(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bars := make([]Bar, 0, 180)
	day := time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	for len(bars) < 180 {
		if marketcalendar.IsTradingDay(day) {
			index := float64(len(bars))
			close := 100 + index*.018 + 6*math.Sin(index*.31) + 1.5*math.Sin(index*.077)
			bar := Bar{Date: day.Format(time.DateOnly), Open: close - .15, High: close + 1, Low: close - 1, Close: close, Volume: 1000}
			bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose, bar.AdjustmentFactor = bar.Open, bar.High, bar.Low, bar.Close, 1
			bars = append(bars, bar)
		}
		day = day.AddDate(0, 0, 1)
	}
	got, err := CalibrateDivergenceThresholds("QQQ", bars, []float64{0.7, 0.8, 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("threshold reports=%d, want 3", len(got))
	}
	for _, report := range got {
		if report.Threshold <= 0 || report.Threshold >= 1 || report.Train.Horizons[5].Samples > report.Train.Signals || report.OutOfSample.Horizons[5].Samples > report.OutOfSample.Signals {
			t.Fatalf("invalid calibration report=%+v", report)
		}
	}
}

func TestFormingLifecycleCountsResolvedEpisodesAndLeavesOpenCandidate(t *testing.T) {
	candidates := map[string]*formingCandidateState{
		"confirmed":      {LastSeen: 7, Resolved: "confirmed"},
		"withdrawn":      {LastSeen: 6, Resolved: "withdrawn"},
		"still-forming":  {LastSeen: 9},
		"second-episode": {LastSeen: 8, Resolved: "withdrawn"},
	}
	got := classifyFormingLifecycle(candidates)
	if got != (FormingLifecycle{FirstObserved: 4, Confirmed: 1, Withdrawn: 2, StillForming: 1}) {
		t.Fatalf("forming lifecycle=%+v, want confirmed/withdrawn/still-forming episodes = 1/2/1", got)
	}
}

func TestTrackFormingCandidateEpisodesSeparatesWithdrawAndReappearance(t *testing.T) {
	active := map[string]*formingCandidateState{}
	episodes := map[string]*formingCandidateState{}
	counts := map[string]int{}
	trackFormingCandidateEpisodes(active, episodes, counts, nil, map[string]string{"A": "buy"}, "2026-01-05", 0)
	trackFormingCandidateEpisodes(active, episodes, counts, nil, nil, "2026-01-06", 1)
	trackFormingCandidateEpisodes(active, episodes, counts, nil, map[string]string{"A": "buy"}, "2026-01-07", 2)
	trackFormingCandidateEpisodes(active, episodes, counts, map[string]bool{"A": true}, nil, "2026-01-08", 3)
	got := classifyFormingLifecycle(episodes)
	if got != (FormingLifecycle{FirstObserved: 2, Confirmed: 1, Withdrawn: 1, StillForming: 0}) {
		t.Fatalf("forming lifecycle=%+v, want two episodes resolved as withdrawn then confirmed", got)
	}
}

func TestCalibrateIntradayDivergenceThresholdsUsesClosedBarPrefixes(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bars := make([]Bar, 0, 120)
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, location)
	for len(bars) < 120 {
		if marketcalendar.IsTradingDay(day) {
			for slot := 0; slot < 6 && len(bars) < 120; slot++ {
				index := float64(len(bars))
				close := 100 + 2*math.Sin(index*.27) + .015*index
				stamp := day.Add(9*time.Hour + 30*time.Minute + time.Duration(slot)*time.Hour)
				bars = append(bars, Bar{Date: stamp.Format("2006-01-02 15:04"), Open: close - .1, High: close + 1, Low: close - 1, Close: close, Volume: 100})
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	got, err := CalibrateIntradayDivergenceThresholds("60m", bars, []float64{.8}, CalibrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Threshold != .8 || got[0].Train.Horizons[5].Samples > got[0].Train.Signals {
		t.Fatalf("intraday calibration=%+v, want one time-forward 60m report", got)
	}
}

func TestCalibrateDivergenceThresholdsRejectsInvalidSample(t *testing.T) {
	if _, err := CalibrateDivergenceThresholds("QQQ", []Bar{{Date: "2026-01-01"}}, []float64{0.8}); err == nil {
		t.Fatal("short sample should be rejected")
	}
	bars := make([]Bar, 100)
	for i := range bars {
		bars[i].Date = fmt.Sprintf("2026-01-%02d", i+1)
	}
	if _, err := CalibrateDivergenceThresholds("QQQ", bars, []float64{1}); err == nil {
		t.Fatal("invalid threshold should be rejected")
	}
}

func TestCalibrateDivergenceThresholdsSkipsRejectedAdjustedBarsWithoutAborting(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bars := make([]Bar, 0, 180)
	day := time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	for len(bars) < 180 {
		if marketcalendar.IsTradingDay(day) {
			index := float64(len(bars))
			close := 100 + index*.018 + 6*math.Sin(index*.31) + 1.5*math.Sin(index*.077)
			bar := Bar{Date: day.Format(time.DateOnly), Open: close - .15, High: close + 1, Low: close - 1, Close: close, Volume: 1000}
			bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose, bar.AdjustmentFactor = bar.Open, bar.High, bar.Low, bar.Close, 1
			bars = append(bars, bar)
		}
		day = day.AddDate(0, 0, 1)
	}
	// A provider factor mismatch should be dropped by the production quality
	// gate and split the structure chain; it must not make calibration fail.
	bars[90].AdjustmentFactor = .5
	if _, err := CalibrateDivergenceThresholds("QQQ", bars, []float64{0.8}); err != nil {
		t.Fatalf("calibration rejected a series containing a production-rejected bar: %v", err)
	}
}

func TestCalibrateDivergenceThresholdSignalClassificationUsesEachCandidate(t *testing.T) {
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: 0, Start: "01", End: "02", StartPrice: 4, EndPrice: 11, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: 0, Start: "02", End: "03", StartPrice: 11, EndPrice: 7, Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, Start: "03", End: "04", StartPrice: 7, EndPrice: 9, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: 0, Start: "04", End: "05", StartPrice: 9, EndPrice: 6, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: 0, Start: "05", End: "06", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: 0, Start: "06", End: "07", StartPrice: 14, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "07"},
		{Direction: "up", Chain: 0, Level: 0, Start: "07", End: "08", StartPrice: 12, EndPrice: 16, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "08"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, Lower: 5, Upper: 10, Start: "01", End: "04", FormedAt: "04", ConfirmedAt: "05", EndSegment: 2}
	levels := []LevelAnalysis{{Level: 0, Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}}
	strengths := []Strength{{Direction: "up", Level: 0, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true}, {Direction: "up", Level: 0, Start: "07", End: "08", MACDArea: 8, WarmupComplete: true}}
	frame := Frame{Candles: []Bar{{Date: "09", High: 17, Low: 13, Close: 16}}}
	if signals := buildV4SignalsWithDivergenceRatio(frame, levels, strengths, 0.7); hasSignalKind(signals, "class1_sell") {
		t.Fatal("0.8 strength ratio must be excluded by candidate threshold 0.7")
	}
	if signals := buildV4SignalsWithDivergenceRatio(frame, levels, strengths, 0.9); !hasSignalKind(signals, "class1_sell") {
		t.Fatal("0.8 strength ratio must be included by candidate threshold 0.9")
	}
}

func TestMeasureCalibrationRetainsZeroWinsAndSuppressesSmallSampleRate(t *testing.T) {
	bars := make([]Bar, 40)
	observations := make([]calibrationObservation, 30)
	for index := range bars {
		bars[index] = Bar{Date: time.Date(2025, 1, 1+index, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), Close: 100 + float64(index)}
		if index < len(observations) {
			observations[index] = calibrationObservation{date: bars[index].Date, side: "sell"}
		}
	}

	partition := measureCalibration(observations, bars, len(bars), true)
	measure := partition.Horizons[5]
	if measure.Samples != 30 || measure.Wins == nil || *measure.Wins != 0 {
		t.Fatalf("zero-win count must be explicit: %+v", measure)
	}
	if measure.WinRate == nil || *measure.WinRate != 0 || measure.WinRateCI95 == nil {
		t.Fatalf("sufficient sample should report zero win rate and interval: %+v", measure)
	}
	encoded, err := json.Marshal(measure)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if value, exists := fields["wins"]; !exists || value != float64(0) {
		t.Fatalf("JSON must include explicit wins: %s", encoded)
	}

	smallSample := measureCalibration(observations[:1], bars, len(bars), true).Horizons[5]
	if smallSample.Samples != 1 || smallSample.Wins == nil || *smallSample.Wins != 0 {
		t.Fatalf("small sample should expose descriptive win count: %+v", smallSample)
	}
	if smallSample.WinRate != nil || smallSample.WinRateCI95 != nil || smallSample.Inference != "insufficient_sample_no_win_rate_claim" {
		t.Fatalf("small sample must not claim a win rate: %+v", smallSample)
	}
}

func TestMeasureCalibrationStartsAfterObservedClose(t *testing.T) {
	bars := []Bar{{Date: "2025-01-02", Close: 100}, {Date: "2025-01-03", Close: 110}, {Date: "2025-01-06", Close: 90}, {Date: "2025-01-07", Close: 120}, {Date: "2025-01-08", Close: 125}, {Date: "2025-01-09", Close: 130}, {Date: "2025-01-12", Close: 140}}
	observations := []calibrationObservation{{date: bars[0].Date, side: "buy"}}
	measure := measureCalibration(observations, bars, len(bars), true).Horizons[5]
	want := (140.0/110.0 - 1) * 100
	if measure.Samples != 1 || measure.Mean == nil || math.Abs(*measure.Mean-want) > 1e-9 || measure.Median == nil || math.Abs(*measure.Median-want) > 1e-9 || measure.P10 == nil || math.Abs(*measure.P10-want) > 1e-9 || measure.P90 == nil || math.Abs(*measure.P90-want) > 1e-9 {
		t.Fatalf("return distribution=%+v, want next-bar close baseline return %.4f%%", measure, want)
	}
	if measure.MaxDrawdown == nil || math.Abs(*measure.MaxDrawdown-(-100.0/5.5)) > 1e-9 {
		t.Fatalf("directional max drawdown=%+v, want approximately -18.1818%% from the next-bar baseline", measure.MaxDrawdown)
	}
}

func TestMeasureCalibrationKeepsForwardWindowsInsideTheirTimePartition(t *testing.T) {
	bars := make([]Bar, 80)
	for index := range bars {
		bars[index] = Bar{Date: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, index).Format(time.DateOnly), Close: 100 + float64(index)}
	}
	cutoff := 35
	observations := []calibrationObservation{
		{date: bars[25].Date, side: "buy"}, // 20-bar forward outcome would cross cutoff.
		{date: bars[30].Date, side: "buy"}, // starts in training, but 5-bar outcome remains in training.
		{date: bars[33].Date, side: "buy"}, // next-bar baseline is already out of sample.
		{date: bars[35].Date, side: "buy"}, // first valid out-of-sample observation.
	}
	train := measureCalibration(observations, bars, cutoff, true)
	if train.Signals != 3 || train.Horizons[5].Samples != 1 || train.Horizons[10].Samples != 0 || train.Horizons[20].Samples != 0 {
		t.Fatalf("training partition included a crossing window: %+v", train)
	}
	outOfSample := measureCalibration(observations, bars, cutoff, false)
	if outOfSample.Signals != 1 || outOfSample.Horizons[5].Samples != 1 || outOfSample.Horizons[10].Samples != 1 || outOfSample.Horizons[20].Samples != 1 {
		t.Fatalf("out-of-sample partition included a training-period outcome or missed valid window: %+v", outOfSample)
	}
}
