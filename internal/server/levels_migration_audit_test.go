package server

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestLevelsAuditStableCandidateAnchorAcrossLiveSpotSnapshots(t *testing.T) {
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	strike, volume := 99.75, int64(100)
	row := model.OptionRow{Expiration: "2026-11-04", ContractType: "call", Strike: &strike, Volume: &volume}
	anchor := 100.0

	for _, snapshot := range []struct {
		spot     float64
		supportN int
		resistN  int
	}{
		{spot: 99.5, supportN: 0, resistN: 0},
		{spot: 99.8, supportN: 1, resistN: 0},
		{spot: 99.9, supportN: 1, resistN: 0},
		{spot: 100.2, supportN: 1, resistN: 0},
	} {
		spot := snapshot.spot
		result := buildCachedLevelsAt("FIX", row.Expiration, nil, []model.OptionRow{row}, model.Quote{}, &spot, &anchor, nil,
			map[string]any{}, map[string]any{}, map[string]any{}, now, nil)
		if result == nil {
			t.Fatalf("spot %.2f returned nil response", spot)
		}
		support := pythonGoldenLevels(t, result["support"], "snapshot support")
		resistance := pythonGoldenLevels(t, result["resistance"], "snapshot resistance")
		if len(support) != snapshot.supportN || len(resistance) != snapshot.resistN {
			t.Errorf("spot %.2f support/resistance counts=%d/%d, want %d/%d with candidate anchor %.2f",
				spot, len(support), len(resistance), snapshot.supportN, snapshot.resistN, anchor)
		}
		if len(support) == 1 && support[0]["price"] != strike {
			t.Errorf("spot %.2f support candidate=%v, want stable strike %.2f", spot, support[0]["price"], strike)
		}
	}
}

func TestLevelsAuditInvalidSpotFallsBackToLatestClose(t *testing.T) {
	bars := []map[string]any{
		{"date": "2026-10-07", "close": 98.0},
		{"date": "2026-10-08", "close": 100.0},
	}
	invalidSpots := []*float64{nil, auditFloatPointer(math.NaN()), auditFloatPointer(math.Inf(1))}
	for _, spot := range invalidSpots {
		candidate := math.Inf(-1)
		result := buildCachedLevelsAt("FIX", "", bars, nil, model.Quote{}, spot, &candidate, nil,
			map[string]any{}, map[string]any{}, map[string]any{}, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), nil)
		if result == nil || result["spot"] != 100.0 || result["candidate_spot"] != 100.0 {
			t.Errorf("spot/candidate fallback for %v = %#v, want latest close 100", spot, result)
		}
	}
}

func TestLevelsAuditOptionSortTieAtPerSideCapUsesDescendingStrike(t *testing.T) {
	rows := make([]model.OptionRow, 13)
	for index := range rows {
		strike, volume := float64(101+index), int64(10)
		rows[index] = model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volume}
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" || len(levels) != cachedOptionLimit {
		t.Fatalf("metric/count=%q/%d, want volume/%d", metric, len(levels), cachedOptionLimit)
	}
	for index, level := range levels {
		want := float64(113 - index)
		if level.price != want {
			t.Errorf("tie-ranked level[%d]=%.2f, want %.2f from Python reverse tuple ordering", index, level.price, want)
		}
	}
}

func TestLevelsAuditNaNWeightMatchesPythonCompositeClamp(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: math.NaN(), factor: "筹码密集", factors: []string{"筹码密集"}},
	}, 100, true, 1, .5, 10, 1)
	if len(levels) != 1 {
		t.Fatalf("NaN weight produced %d levels, want one clamped candidate", len(levels))
	}
	if levels[0]["score"] != 0.25 || levels[0]["model_score"] != 0.25 {
		t.Fatalf("NaN weight scores=%#v/%#v, want Python's composite score 0.25", levels[0]["score"], levels[0]["model_score"])
	}
}

func auditFloatPointer(value float64) *float64 { return &value }
