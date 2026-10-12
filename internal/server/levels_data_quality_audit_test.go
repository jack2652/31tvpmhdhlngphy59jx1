package server

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestLevelsDataQualityJSONNumbersMatchPythonAtFibonacciWindowBoundary(t *testing.T) {
	makeBars := func(count int, jsonNumbers bool) []map[string]any {
		bars := make([]map[string]any, count)
		for index := range bars {
			close := 100.0 + float64(index)*0.1
			high, low := close+1, close-1
			bar := map[string]any{"high": high, "low": low, "close": close}
			if jsonNumbers {
				bar["high"] = json.Number(formatAuditNumber(high))
				bar["low"] = json.Number(formatAuditNumber(low))
				bar["close"] = json.Number(formatAuditNumber(close))
			}
			bars[index] = bar
		}
		return bars
	}

	floatWindow := makeBars(120, false)
	jsonWindow := makeBars(120, true)
	withIgnoredOldBar := append([]map[string]any{{"high": "bad", "low": nil, "close": "bad"}}, jsonWindow...)
	want := cachedFibonacciLevels(floatWindow)
	for name, bars := range map[string][]map[string]any{"json-number window": jsonWindow, "121 bars with invalid excluded prefix": withIgnoredOldBar} {
		got := cachedFibonacciLevels(bars)
		if len(got) != len(want) {
			t.Fatalf("%s candidates=%#v, want %d levels from Python's last-120-bar window", name, got, len(want))
		}
		for index := range want {
			if math.Abs(got[index].price-want[index].price) > 1e-12 || got[index].score != want[index].score || got[index].factor != want[index].factor {
				t.Errorf("%s candidate[%d]=%#v, want %#v", name, index, got[index], want[index])
			}
		}
	}
	withMissingInteriorValue := makeBars(120, true)
	withMissingInteriorValue[60]["close"] = nil
	if got := cachedFibonacciLevels(withMissingInteriorValue); len(got) != 0 {
		t.Fatalf("missing close inside the selected window should reject Fibonacci candidates like Python, got %#v", got)
	}
}

func TestLevelsDataQualityJSONNumbersFeedATRAndChipCandidates(t *testing.T) {
	floatBars := make([]map[string]any, 12)
	jsonBars := make([]map[string]any, 12)
	for index := range floatBars {
		low := float64(90 + index%3)
		high := float64(100 + index%5)
		close := (low + high) / 2
		volume := float64(100 + index*10)
		floatBars[index] = map[string]any{"high": high, "low": low, "close": close, "volume": volume}
		jsonBars[index] = map[string]any{
			"high": json.Number(formatAuditNumber(high)), "low": json.Number(formatAuditNumber(low)),
			"close": json.Number(formatAuditNumber(close)), "volume": json.Number(formatAuditNumber(volume)),
		}
	}
	if got, want := cachedATR(jsonBars, cachedATRPeriod), cachedATR(floatBars, cachedATRPeriod); got != want {
		t.Fatalf("ATR from json.Number=%v, want %v like Python float coercion", got, want)
	}
	got, want := cachedChipPeaks(jsonBars), cachedChipPeaks(floatBars)
	if len(got) != len(want) {
		t.Fatalf("chip peaks from json.Number=%d, want %d like Python float coercion", len(got), len(want))
	}
	for index := range want {
		if math.Abs(got[index].price-want[index].price) > 1e-12 || math.Abs(got[index].score-want[index].score) > 1e-12 {
			t.Errorf("chip peak[%d]=%#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestLevelsDataQualityInvalidOptionRowsAreExcludedLikePython(t *testing.T) {
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, anchor := 100.0, 100.0
	validStrike, invalidStrike := 105.0, math.NaN()
	volume := int64(100)
	rows := []model.OptionRow{
		{Expiration: "not-a-date", ContractType: "call", Strike: &validStrike, Volume: &volume},
		{Expiration: "2026-11-04", ContractType: "call", Strike: &invalidStrike, Volume: &volume},
		{Expiration: "2026-11-04", ContractType: "call", Strike: nil, Volume: &volume},
	}
	result := buildCachedLevelsAt("FIX", "not-a-date", nil, rows, model.Quote{}, &spot, &anchor, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, nil)
	if result == nil || result["options_metric"] != "volume" {
		t.Fatalf("invalid chain result=%#v, want valid-strike volume fallback", result)
	}
	resistance := pythonGoldenLevels(t, result["resistance"], "invalid chain resistance")
	if len(resistance) != 1 || resistance[0]["price"] != validStrike {
		t.Fatalf("invalid strikes were not discarded: %#v, want only %.2f", resistance, validStrike)
	}
}

func formatAuditNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
