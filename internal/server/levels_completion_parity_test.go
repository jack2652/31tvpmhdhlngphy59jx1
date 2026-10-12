package server

import (
	"math"
	"testing"

	"option-scope-go/internal/model"
)

func TestCachedOptionCandidatesAggregateAcrossSnapshotsLikePython(t *testing.T) {
	option := func(expiration, contractType string, strike float64, volume int64) model.OptionRow {
		return model.OptionRow{
			Expiration: expiration, ContractType: contractType,
			Strike: &strike, Volume: &volume,
		}
	}
	rows := []model.OptionRow{
		option("2026-11-20", "call", 90, 8),
		option("2026-11-20", "put", 90, 2),
		option("2026-11-20", "call", 95, 5),
		option("2026-11-20", "put", 95, 5),
		option("2026-11-20", "call", 99, 50),
		option("2026-12-18", "call", 101, 20),
		option("2026-12-18", "call", 105, 10),
		option("2026-12-18", "call", 110, 5),
		option("2026-12-18", "put", 110, 5),
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" {
		t.Fatalf("metric=%q, want Python's volume fallback", metric)
	}
	want := []struct {
		price  float64
		score  float64
		factor string
	}{
		{101, 1, "看涨墙"}, {110, 1, "看涨持仓"}, {105, 1, "看涨持仓"},
		{99, 1, "看涨墙"}, {95, 0.6757142857142856, "看涨持仓"}, {90, 0.6757142857142856, "看涨持仓"},
	}
	if len(levels) != len(want) {
		t.Fatalf("multi-snapshot candidates=%#v, want %d Python candidates", levels, len(want))
	}
	for index, expected := range want {
		got := levels[index]
		if got.price != expected.price || math.Abs(got.score-expected.score) > 1e-12 || got.factor != expected.factor {
			t.Errorf("candidate[%d]=%#v, want Python aggregate %#v", index, got, expected)
		}
	}
}

func TestCachedSupportPlanMatchesPythonCountBoundaries(t *testing.T) {
	for _, test := range []struct {
		count, buyCount, addCount int
		wantFirstAdd              float64
	}{
		{1, 1, 0, 0},
		{19, 10, 9, 89},
		{20, 10, 10, 89},
		{21, 10, 10, 88},
		{30, 10, 10, 79},
	} {
		values := make([]map[string]any, test.count)
		for index := range values {
			rank := float64(index + 1)
			values[index] = map[string]any{"price": 100 - rank, "score": rank / 100}
		}
		buy, add := splitCachedSupport(values, 100)
		if len(buy) != test.buyCount || len(add) != test.addCount {
			t.Errorf("count=%d plan sizes=%d/%d, want Python split %d/%d", test.count, len(buy), len(add), test.buyCount, test.addCount)
			continue
		}
		if test.wantFirstAdd > 0 && add[0]["price"] != test.wantFirstAdd {
			t.Errorf("count=%d first add price=%v, want Python score-ranked selection %v", test.count, add[0]["price"], test.wantFirstAdd)
		}
	}
}

func TestCachedHistoryHoldoutFormationBoundaryMatchesPython(t *testing.T) {
	makeBars := func(count int) []map[string]any {
		bars := make([]map[string]any, count)
		for index := range bars {
			bars[index] = map[string]any{"high": 101.0, "low": 99.0, "close": 100.0}
		}
		return bars
	}
	level := func() map[string]any {
		return map[string]any{"price": 100.0, "zone_low": 99.0, "zone_high": 101.0, "score": 0.8, "factors": []string{"筹码密集", "看跌持仓"}}
	}

	short := annotateCachedLevelHistory([]map[string]any{level()}, makeBars(125), "support")[0]
	if short["validation_method"] != "insufficient_history" || short["history_samples"] != 0 {
		t.Fatalf("125 bars=%#v, want Python insufficient-history behavior at 120-bar holdout boundary", short)
	}
	boundary := annotateCachedLevelHistory([]map[string]any{level()}, makeBars(126), "support")[0]
	if boundary["validation_method"] != "holdout_price_action" || boundary["history_samples"] != 1 || boundary["history_hold_rate"] != 1.0 {
		t.Fatalf("126 bars=%#v, want one valid holdout touch after reserving the last 120 bars", boundary)
	}
}
