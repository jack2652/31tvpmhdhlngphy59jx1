package chananalysis

import (
	"encoding/json"
	"fmt"
	"time"

	"option-scope-go/internal/marketcalendar"
)

// ReplayPoint is one time-forward result computed only from the input bars
// visible through AsOf. Signals are tagged with the same visibility boundary.
type ReplayPoint struct {
	AsOf      string             `json:"as_of"`
	Rule      string             `json:"rule_version"`
	Signals   []V4Signal         `json:"signals"`
	Segments  []Segment          `json:"segments"`
	Centers   []RecursiveCenter  `json:"centers"`
	MACD      []MACDPoint        `json:"macd"`
	Merged    []MergedCandle     `json:"merged_candles"`
	Fractals  []Fractal          `json:"fractals"`
	StrokesV4 []Stroke           `json:"strokes_v4"`
	Strengths []Strength         `json:"strengths"`
	Levels    []LevelAnalysis    `json:"levels"`
	Metrics   IncrementalMetrics `json:"incremental_metrics"`
}

// ReplayAdjusted advances one closed-bar prefix at a time. It intentionally
// calls the production analyzer on each prefix so the replay's information
// boundary is explicit and comparable to a full recomputation at that point.
func ReplayAdjusted(symbol string, input []Bar, cutoff time.Time) ([]ReplayPoint, error) {
	if !isChronologicalUnique(input) {
		return nil, fmt.Errorf("replay input bars must be unique and chronological")
	}
	points := make([]ReplayPoint, 0, len(input))
	state := NewIncrementalAnalyzer(symbol)
	for end := 1; end <= len(input); end++ {
		prefix := input[:end]
		location, _ := time.LoadLocation("America/New_York")
		if location == nil {
			location = time.UTC
		}
		pointCutoff := cutoff
		if parsed, err := time.ParseInLocation(time.DateOnly, prefix[len(prefix)-1].Date, location); err == nil {
			barClose := parsed.Add(time.Duration(marketcalendar.RegularCloseMinute(parsed)) * time.Minute)
			if pointCutoff.IsZero() || pointCutoff.After(barClose) {
				pointCutoff = barClose
			}
		}
		result := Result{}
		var err error
		accepted, _, _, _, _, _ := validatedAdjustedBars(prefix, pointCutoff)
		if len(accepted) > len(state.bars) {
			newBar := accepted[len(accepted)-1]
			result, err = state.Append(newBar, pointCutoff)
		} else if len(state.bars) > 0 {
			if cached, ok := state.ReuseLastResult(); ok {
				result = cached
			}
		} else {
			result = AnalyzeAdjusted(symbol, nil, nil, pointCutoff)
		}
		if err != nil {
			return nil, fmt.Errorf("incremental prefix %s: %w", prefix[len(prefix)-1].Date, err)
		}
		visible := make([]V4Signal, 0, len(result.Daily.SignalsV4))
		for _, signal := range result.Daily.SignalsV4 {
			if signal.ObservedAt == "" || signal.ObservedAt > prefix[len(prefix)-1].Date {
				return nil, fmt.Errorf("signal observed after replay prefix %s: %+v", prefix[len(prefix)-1].Date, signal)
			}
			visible = append(visible, signal)
		}
		points = append(points, ReplayPoint{AsOf: prefix[len(prefix)-1].Date, Rule: result.RuleVersion, Signals: visible, Segments: result.Daily.Segments, Centers: result.Daily.RecursiveCenters, MACD: result.Daily.MACD, Merged: result.Daily.MergedCandles, Fractals: result.Daily.FractalsV4, StrokesV4: result.Daily.StrokesV4, Strengths: result.Daily.Strengths, Levels: result.Daily.RecursiveLevels, Metrics: state.Metrics()})
	}
	return points, nil
}

func isChronologicalUnique(bars []Bar) bool {
	previous := ""
	for _, bar := range bars {
		if bar.Date == "" || previous != "" && bar.Date <= previous {
			return false
		}
		previous = bar.Date
	}
	return true
}

// SameReplayFrame reports whether the serialized signal and structure frame
// match a full recomputation over the exact same visible input prefix.
func SameReplayFrame(prefix []Bar, cutoff time.Time, replay ReplayPoint, symbol string) (bool, error) {
	pointCutoff := cutoff
	if len(prefix) > 0 {
		location, _ := time.LoadLocation("America/New_York")
		if location == nil {
			location = time.UTC
		}
		if day, err := time.ParseInLocation(time.DateOnly, prefix[len(prefix)-1].Date, location); err == nil {
			close := day.Add(time.Duration(marketcalendar.RegularCloseMinute(day)) * time.Minute)
			if pointCutoff.IsZero() || pointCutoff.After(close) {
				pointCutoff = close
			}
		}
	}
	result := AnalyzeAdjusted(symbol, prefix, nil, pointCutoff)
	if result.RuleVersion != replay.Rule {
		return false, nil
	}
	encoded, err := json.Marshal(struct {
		Signals   []V4Signal        `json:"signals"`
		Segments  []Segment         `json:"segments"`
		Centers   []RecursiveCenter `json:"centers"`
		MACD      []MACDPoint       `json:"macd"`
		Merged    []MergedCandle    `json:"merged_candles"`
		Fractals  []Fractal         `json:"fractals"`
		StrokesV4 []Stroke          `json:"strokes_v4"`
		Strengths []Strength        `json:"strengths"`
		Levels    []LevelAnalysis   `json:"levels"`
	}{replay.Signals, replay.Segments, replay.Centers, replay.MACD, replay.Merged, replay.Fractals, replay.StrokesV4, replay.Strengths, replay.Levels})
	if err != nil {
		return false, err
	}
	full, err := json.Marshal(struct {
		Signals   []V4Signal        `json:"signals"`
		Segments  []Segment         `json:"segments"`
		Centers   []RecursiveCenter `json:"centers"`
		MACD      []MACDPoint       `json:"macd"`
		Merged    []MergedCandle    `json:"merged_candles"`
		Fractals  []Fractal         `json:"fractals"`
		StrokesV4 []Stroke          `json:"strokes_v4"`
		Strengths []Strength        `json:"strengths"`
		Levels    []LevelAnalysis   `json:"levels"`
	}{result.Daily.SignalsV4, result.Daily.Segments, result.Daily.RecursiveCenters, result.Daily.MACD, result.Daily.MergedCandles, result.Daily.FractalsV4, result.Daily.StrokesV4, result.Daily.Strengths, result.Daily.RecursiveLevels})
	if err != nil {
		return false, err
	}
	return string(encoded) == string(full), nil
}
