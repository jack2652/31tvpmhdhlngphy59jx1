package server

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func levelsMatrixFixture() ([]map[string]any, []model.OptionRow) {
	bars := make([]map[string]any, 126)
	bars[0] = map[string]any{"high": 96.0, "low": 94.5, "close": 95.0, "volume": 500.0}
	for index := 1; index < 6; index++ {
		bars[index] = map[string]any{"high": 98.0, "low": 96.0, "close": 97.0, "volume": 100.0}
	}
	for index := 6; index < len(bars); index++ {
		bars[index] = map[string]any{"high": 101.0, "low": 99.0, "close": 100.0, "volume": 100.0}
	}
	rows := []model.OptionRow{
		{Expiration: "2026-11-20", ContractType: "put", Strike: pythonGoldenFloat(90), Volume: pythonGoldenInt(5), ImpliedVolatility: pythonGoldenFloat(.4)},
		{Expiration: "2026-11-20", ContractType: "call", Strike: pythonGoldenFloat(95), Volume: pythonGoldenInt(10), ImpliedVolatility: pythonGoldenFloat(.4)},
		{Expiration: "2026-11-20", ContractType: "put", Strike: pythonGoldenFloat(105), Volume: pythonGoldenInt(10), ImpliedVolatility: pythonGoldenFloat(.4)},
		{Expiration: "2026-11-20", ContractType: "call", Strike: pythonGoldenFloat(110), Volume: pythonGoldenInt(5), ImpliedVolatility: pythonGoldenFloat(.4)},
	}
	return bars, rows
}

func TestBuildCachedLevelsMatrixMatchesPythonAcrossSpotProbabilityAndPlan(t *testing.T) {
	bars, rows := levelsMatrixFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	anchor := 100.0
	// Golden probabilities come from Python build_levels with its expiry helpers fixed to now.
	cases := []struct {
		name                  string
		spot                  float64
		supportProbability    []float64
		resistancePrices      []float64
		resistanceProbability []float64
		buyPrices, addPrices  []float64
		sellPrices            []float64
	}{
		{"below-anchor", 99, []float64{.772, .5031}, []float64{100.0042, 105, 110}, []float64{.9435, .6793, .4591}, []float64{95}, []float64{90}, []float64{100.0042, 105, 110}},
		{"at-anchor", 100, []float64{.7185, .4591}, []float64{100.0042, 105, 110}, []float64{.9998, .7317, .5031}, []float64{95}, []float64{90}, []float64{100.0042, 105, 110}},
		{"above-chip-anchor", 101, []float64{.667, .4178}, []float64{105, 110}, []float64{.7849, .5487}, []float64{95}, []float64{90}, []float64{105, 110}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spot := test.spot
			result := buildCachedLevelsAt("MATRIX", "2026-11-20", bars, rows, model.Quote{}, &spot, &anchor, nil,
				map[string]any{}, map[string]any{}, map[string]any{}, now, nil)
			if result == nil || result["options_metric"] != "volume" || result["candidate_spot"] != anchor || result["spot"] != spot {
				t.Fatalf("spot/anchor/metric=%v/%v/%v, want %v/%v/volume", result["spot"], result["candidate_spot"], result["options_metric"], spot, anchor)
			}
			if got := result["generated_at"].(time.Time); !got.Equal(now) {
				t.Fatalf("generated_at=%s, want fixed oracle time %s", got, now)
			}
			for _, side := range []struct {
				name, planKey string
				prices        []float64
				probabilities []float64
			}{
				{"support", "buy", []float64{95, 90}, test.supportProbability},
				{"resistance", "sell", test.resistancePrices, test.resistanceProbability},
			} {
				levels := pythonGoldenLevels(t, result[side.name], side.name)
				if len(levels) != len(side.prices) || len(levels) != len(side.probabilities) {
					t.Fatalf("%s count=%d, want %d Python levels", side.name, len(levels), len(side.prices))
				}
				for index, wantPrice := range side.prices {
					level := levels[index]
					pythonGoldenNumber(t, level["price"], wantPrice, .0001, side.name+" price")
					pythonGoldenNumber(t, level["probability"], side.probabilities[index], .0001, side.name+" probability")
					pythonGoldenNumber(t, level["score"], .56, 0, side.name+" score")
					if side.name == "support" && wantPrice == 95 {
						if level["history_samples"] != 1 || level["history_hold_rate"] != 1.0 || level["validation_method"] != "holdout_price_action" {
							t.Errorf("95 support history=%#v, want one held holdout touch", level)
						}
					}
				}
				plan := pythonGoldenMap(t, result["plan"], "plan")
				planLevels := pythonGoldenLevels(t, plan[side.planKey], "plan."+side.planKey)
				if side.planKey == "buy" {
					assertLevelsMatrixPrices(t, "plan.buy", planLevels, test.buyPrices)
					add := pythonGoldenLevels(t, plan["add"], "plan.add")
					assertLevelsMatrixPrices(t, "plan.add", add, test.addPrices)
				} else {
					assertLevelsMatrixPrices(t, "plan.sell", planLevels, test.sellPrices)
				}
			}
		})
	}
}

func TestBuildCachedLevelsMatrixOHLCVQualityEdgesMatchPython(t *testing.T) {
	baseBars, rows := levelsMatrixFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, anchor := 100.0, 100.0
	cases := []struct {
		name              string
		mutate            func([]map[string]any)
		wantChip, wantFib bool
	}{
		{"in-window-missing-close", func(bars []map[string]any) { bars[20]["close"] = nil }, true, false},
		{"all-zero-volume", func(bars []map[string]any) {
			for _, bar := range bars {
				bar["volume"] = 0.0
			}
		}, false, false},
		{"invalid-volume-row", func(bars []map[string]any) { bars[80]["volume"] = "bad" }, true, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bars := make([]map[string]any, len(baseBars))
			for index, bar := range baseBars {
				copied := make(map[string]any, len(bar))
				for key, value := range bar {
					copied[key] = value
				}
				bars[index] = copied
			}
			test.mutate(bars)
			result := buildCachedLevelsAt("MATRIX", "2026-11-20", bars, rows, model.Quote{}, &spot, &anchor, nil,
				map[string]any{}, map[string]any{}, map[string]any{}, now, nil)
			if result == nil {
				t.Fatal("buildCachedLevelsAt returned nil")
			}
			all := append(pythonGoldenLevels(t, result["support"], "support"), pythonGoldenLevels(t, result["resistance"], "resistance")...)
			hasChip, hasFib := false, false
			for _, level := range all {
				for _, factor := range cachedFactorStrings(level["factors"]) {
					hasChip = hasChip || factor == "筹码密集"
					hasFib = hasFib || len(factor) >= len("斐波那契") && factor[:len("斐波那契")] == "斐波那契"
				}
			}
			if hasChip != test.wantChip || hasFib != test.wantFib {
				t.Errorf("factor presence chip/fib=%v/%v, want Python %v/%v", hasChip, hasFib, test.wantChip, test.wantFib)
			}
			if result["history_bars"] != 126 || result["options_metric"] != "volume" {
				t.Errorf("history bars/metric=%v/%v, want 126/volume", result["history_bars"], result["options_metric"])
			}
		})
	}
}

func TestCachedLevelMatrixPlanBoundariesAndNearbyPriorityOrder(t *testing.T) {
	for _, count := range []int{1, 2, 9, 10, 11, 19, 20, 21, 30} {
		values := make([]map[string]any, count)
		for index := range values {
			values[index] = map[string]any{"price": 100 - float64(index+1), "score": .56}
		}
		buy, add := splitCachedSupport(values, 100)
		wantBuy := min(cachedLevelLimit, max(1, (count+1)/2))
		wantAdd := min(cachedLevelLimit, count-wantBuy)
		if len(buy) != wantBuy || len(add) != wantAdd {
			t.Errorf("count=%d plan sizes=%d/%d, want Python %d/%d", count, len(buy), len(add), wantBuy, wantAdd)
		}
		if len(buy) > 0 && buy[0]["price"] != 99.0 {
			t.Errorf("count=%d first buy=%v, want nearest price 99", count, buy[0]["price"])
		}
	}
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: .2, factors: []string{"看涨持仓"}},
		{price: 102, score: .2, factors: []string{"看涨持仓"}},
		{price: 103, score: .2, factors: []string{"看涨持仓"}},
	}, 100, true, .1, 0, 2, 1)
	if len(levels) != 2 || levels[0]["price"] != 101.0 || levels[1]["price"] != 102.0 {
		t.Fatalf("priority-ranked candidates=%#v, want nearest Python candidates [101 102] at cap", levels)
	}
}

func assertLevelsMatrixPrices(t *testing.T, context string, levels []map[string]any, want []float64) {
	t.Helper()
	if len(levels) != len(want) {
		t.Fatalf("%s count=%d, want %d prices", context, len(levels), len(want))
	}
	for index, price := range want {
		got, ok := levels[index]["price"].(float64)
		if !ok || math.Abs(got-price) > .0001 {
			t.Errorf("%s[%d].price=%v, want %v", context, index, levels[index]["price"], price)
		}
	}
}
