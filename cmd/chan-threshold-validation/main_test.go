package main

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	chananalysis "option-scope-go/internal/chan"
)

func TestNumberReadsSQLiteNullableFloatValues(t *testing.T) {
	value := 12.5
	if got := number(&value); got != value {
		t.Fatalf("number(*float64)=%v, want %v", got, value)
	}
	if got := number((*float64)(nil)); got != 0 {
		t.Fatalf("number(nil)=%v, want 0", got)
	}
	if got := number(float64(8)); got != 8 {
		t.Fatalf("number(float64)=%v, want 8", got)
	}
}

func TestValidationDefaultsToAboutFiveYearsOfDailyBars(t *testing.T) {
	if defaultValidationBarLimit != 1300 {
		t.Fatalf("default validation bar limit = %d, want 1300 (about five years)", defaultValidationBarLimit)
	}
}

func TestParseSymbolsTrimsDeduplicatesAndRejectsEmptyEntries(t *testing.T) {
	got, err := parseSymbols(" QQQ, SPY,QQQ,^GSPC ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"QQQ", "SPY", "^GSPC"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSymbols=%v, want %v", got, want)
	}
	for _, invalid := range []string{"", "QQQ,,SPY", ",QQQ", "QQQ,"} {
		if _, err := parseSymbols(invalid); err == nil {
			t.Errorf("parseSymbols(%q) should reject an empty symbol", invalid)
		}
	}
}

func TestBatchReportKeepsExplicitPerSymbolFailures(t *testing.T) {
	output := batchReport{Status: "partial", Reports: []report{
		{Status: "error", Symbol: "MISSING", Error: "no adjusted history"},
		{Status: "ok", Symbol: "QQQ", Bars: 3000, Conclusion: "no_conclusion_no_observed_signals"},
	}}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded batchReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != "partial" || len(decoded.Reports) != 2 || decoded.Reports[0].Status != "error" || decoded.Reports[0].Error == "" || decoded.Reports[1].Status != "ok" {
		t.Fatalf("batch JSON lost per-symbol status/error: %s", encoded)
	}
}

func TestReportJSONDisclosesScopeCostsAndThresholdConclusion(t *testing.T) {
	value := 1.25
	wins := 24
	winRate := .8
	ci := &chananalysis.WilsonInterval{Lower: .63, Upper: .90, Confidence: .95}
	output := report{Symbol: "QQQ", Bars: 1255, Start: "2021-10-18", End: "2026-10-16", RuleVersion: chananalysis.RecursivePreviewVersion, Scope: "directional diagnostic", Method: "5/10/20 bars, 70/30 chronological", Conclusion: "descriptive_results_only_not_a_strategy_backtest", Benchmark: benchmarkReport{Symbol: "^GSPC", Bars: 1255, Start: "2021-10-18", End: "2026-10-16", Method: "aligned buy-and-hold", ReturnPercent: 12, MaxDrawdownPercent: -8}, Costs: costDisclosure{Commission: "not modeled", Slippage: "not modeled", Execution: "not a strategy backtest"}, Limitations: []string{"signal-path drawdown is a window diagnostic, not portfolio drawdown"}, Calibration: []chananalysis.ThresholdCalibration{{Threshold: .8, Train: chananalysis.CalibrationPartition{Horizons: map[int]chananalysis.HorizonMeasure{5: {Samples: 30, Wins: &wins, WinRate: &winRate, WinRateCI95: ci, Mean: &value, Median: &value, P10: &value, P90: &value, MaxDrawdown: &value, Inference: "descriptive_wilson_95"}}}, OutOfSample: chananalysis.CalibrationPartition{Horizons: map[int]chananalysis.HorizonMeasure{5: {Samples: 0, Inference: "no_outcomes"}}}}}}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"symbol", "bars", "start", "end", "rule_version", "benchmark", "costs", "scope", "method", "threshold_conclusion", "calibration", "limitations"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("report missing %q: %s", key, encoded)
		}
	}
	benchmark := fields["benchmark"].(map[string]any)
	for _, key := range []string{"symbol", "bars", "start", "end", "method", "buy_hold_return_percent", "buy_hold_max_drawdown_percent"} {
		if _, ok := benchmark[key]; !ok {
			t.Errorf("benchmark report missing %q: %s", key, encoded)
		}
	}
	calibration := fields["calibration"].([]any)[0].(map[string]any)
	for _, split := range []string{"train", "out_of_sample"} {
		if _, ok := calibration[split]; !ok {
			t.Errorf("calibration report missing %q partition: %s", split, encoded)
		}
	}
	horizon := calibration["train"].(map[string]any)["horizons"].(map[string]any)["5"].(map[string]any)
	for _, key := range []string{"samples", "wins", "directional_win_rate", "directional_win_rate_ci95", "mean_directional_return_percent", "median_directional_return_percent", "p10_directional_return_percent", "p90_directional_return_percent", "max_directional_drawdown_percent", "inference"} {
		if _, ok := horizon[key]; !ok {
			t.Errorf("horizon report missing %q: %s", key, encoded)
		}
	}
	limitations := fields["limitations"].([]any)
	if len(limitations) == 0 || !strings.Contains(limitations[0].(string), "portfolio drawdown") {
		t.Errorf("report must distinguish signal-path drawdown from portfolio drawdown: %s", encoded)
	}
}

func TestZeroOutcomeCalibrationRetainsExplicitNoConclusion(t *testing.T) {
	measure := chananalysis.HorizonMeasure{Samples: 0, Inference: "no_outcomes"}
	if measure.Samples != 0 || measure.Inference != "no_outcomes" || measure.WinRate != nil || measure.WinRateCI95 != nil {
		t.Fatalf("zero outcome measure must not imply a hit rate: %+v", measure)
	}
	if got := thresholdConclusion([]chananalysis.ThresholdCalibration{{Threshold: .8, Train: chananalysis.CalibrationPartition{Horizons: map[int]chananalysis.HorizonMeasure{5: measure}}, OutOfSample: chananalysis.CalibrationPartition{Horizons: map[int]chananalysis.HorizonMeasure{5: measure}}}}); got != "no_conclusion_no_observed_signals" {
		t.Fatalf("zero-signal report conclusion=%q", got)
	}
}

func TestThresholdConclusionDoesNotPoolRepeatedSignalsAcrossThresholds(t *testing.T) {
	calibration := []chananalysis.ThresholdCalibration{
		{Threshold: .6, Train: chananalysis.CalibrationPartition{Signals: 18}, OutOfSample: chananalysis.CalibrationPartition{Signals: 7}},
		{Threshold: .7, Train: chananalysis.CalibrationPartition{Signals: 18}, OutOfSample: chananalysis.CalibrationPartition{Signals: 7}},
		{Threshold: .8, Train: chananalysis.CalibrationPartition{Signals: 18}, OutOfSample: chananalysis.CalibrationPartition{Signals: 7}},
		{Threshold: .9, Train: chananalysis.CalibrationPartition{Signals: 18}, OutOfSample: chananalysis.CalibrationPartition{Signals: 7}},
	}
	if got := thresholdConclusion(calibration); got != "descriptive_only_insufficient_sample" {
		t.Fatalf("threshold conclusion=%q; repeated structures across threshold sweeps must not be pooled into an apparently adequate sample", got)
	}
}

func TestBenchmarkStatsUsesAlignedSP500Bars(t *testing.T) {
	target := []chananalysis.Bar{{Date: "2026-01-02", Close: 50}, {Date: "2026-01-05", Close: 55}, {Date: "2026-01-06", Close: 60}}
	benchmark := []chananalysis.Bar{{Date: "2026-01-01", Close: 100}, {Date: "2026-01-02", Close: 200}, {Date: "2026-01-05", Close: 180}, {Date: "2026-01-06", Close: 220}, {Date: "2026-01-07", Close: 300}}
	count, start, end, change, drawdown, err := benchmarkStats(target, benchmark)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || start != target[0].Date || end != target[len(target)-1].Date {
		t.Fatalf("benchmark range=%d %s..%s, want 3 bars aligned to target", count, start, end)
	}
	if math.Abs(change-10) > 1e-9 || math.Abs(drawdown-(-10)) > 1e-9 {
		t.Fatalf("benchmark change/drawdown=%.4f/%.4f, want 10/-10; target close path would be +20%%", change, drawdown)
	}
}

func TestBenchmarkStatsRejectsIncompletePeriodCoverage(t *testing.T) {
	target := []chananalysis.Bar{{Date: "2026-01-02", Close: 50}, {Date: "2026-01-06", Close: 60}}
	benchmark := []chananalysis.Bar{{Date: "2026-01-02", Close: 100}, {Date: "2026-01-05", Close: 110}}
	_, _, _, _, _, err := benchmarkStats(target, benchmark)
	if err == nil || !strings.Contains(err.Error(), "missing or invalid for target trading date 2026-01-06") {
		t.Fatalf("expected explicit coverage error, got %v", err)
	}
}
