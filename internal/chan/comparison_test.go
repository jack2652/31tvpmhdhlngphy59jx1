package chananalysis

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAlgorithmComparisonExplainsLegacyAndRecursiveStructures(t *testing.T) {
	legacy := Frame{Timeframe: "1d", Fractals: []Fractal{{Date: "2026-01-02", Kind: "top", Price: 12}}, Strokes: []Stroke{{Direction: "up", Start: Fractal{Date: "01", Kind: "bottom", Price: 10}, End: Fractal{Date: "02", Kind: "top", Price: 12}}}, Centers: []Center{{Level: 0, InputKind: "stroke", Start: "01", End: "02", Lower: 10, Upper: 12}}, Signals: []Signal{{Kind: "third_buy_candidate", Date: "02", Status: "candidate"}}}
	recursive := Frame{Timeframe: "1d", FractalsV4: []Fractal{{Date: "2026-01-03", Kind: "top", Price: 13}}, StrokesV4: []Stroke{{Level: 0, Direction: "up", Start: Fractal{Date: "01", Kind: "bottom", Price: 10}, End: Fractal{Date: "03", Kind: "top", Price: 13}}}, Segments: []Segment{{Level: 0, Direction: "up", Start: "01", End: "03", Confirmed: false}}, RecursiveCenters: []RecursiveCenter{{Level: 1, InputKind: "recursive_trend", Start: "01", End: "03", Lower: 10, Upper: 13, Forming: true}}, SignalsV4: []V4Signal{{Level: 1, Kind: "class1_buy", Date: "03", Status: "observing"}}}
	comparison := compareAlgorithmFrames(legacy, recursive)
	if comparison.Legacy.Strokes != 1 || comparison.Recursive.Strokes != 1 || comparison.Recursive.Segments != 1 || comparison.Recursive.Centers != 1 || comparison.Recursive.Signals != 1 {
		t.Fatalf("comparison counts=%+v", comparison)
	}
	if len(comparison.Differences) != 5 || len(comparison.Differences[0].LegacyOnly) != 1 || len(comparison.Differences[0].RecursiveOnly) != 1 {
		t.Fatalf("comparison detail=%+v", comparison.Differences)
	}
	encoded, err := json.Marshal(comparison)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"legacy_rule_version", "recursive_rule_version", "legacy_only", "recursive_only", "explanation"} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("comparison JSON missing %q: %s", field, encoded)
		}
	}
}

func TestAlgorithmComparisonCountsAllRecursiveLevels(t *testing.T) {
	levelOne := LevelAnalysis{Level: 1, Strokes: []Stroke{{Level: 1}}, Segments: []Segment{{Level: 1, Direction: "up", Start: "01", End: "02", Confirmed: true}}, Centers: []RecursiveCenter{{Level: 1, InputKind: "segment", Start: "01", End: "02", Lower: 10, Upper: 12, ConfirmedAt: "03"}}}
	levelTwo := LevelAnalysis{Level: 2, Strokes: []Stroke{{Level: 2}}, Segments: []Segment{{Level: 2, Direction: "down", Start: "03", End: "04", Confirmed: false}}, Centers: []RecursiveCenter{{Level: 2, InputKind: "recursive_trend", Start: "03", End: "04", Lower: 9, Upper: 11, Forming: true}}}
	comparison := compareAlgorithmFrames(Frame{Timeframe: "1d"}, Frame{Timeframe: "1d", RecursiveLevels: []LevelAnalysis{levelOne, levelTwo}})
	if comparison.Recursive.Segments != 2 || comparison.Recursive.Strokes != 2 || comparison.Recursive.Centers != 2 {
		t.Fatalf("recursive counts must include higher levels: %+v", comparison.Recursive)
	}
	if len(comparison.Differences[2].RecursiveOnly) != 2 {
		t.Fatalf("recursive level segment examples=%v", comparison.Differences[2].RecursiveOnly)
	}
}
