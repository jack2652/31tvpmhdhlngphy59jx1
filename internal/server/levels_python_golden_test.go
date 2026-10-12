package server

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func pythonGoldenFloat(value float64) *float64 { return &value }

func pythonGoldenInt(value int64) *int64 { return &value }

func pythonGoldenFixture() ([]map[string]any, []model.OptionRow) {
	bars := make([]map[string]any, 180)
	previous := 100.0
	start := time.Date(2025, time.December, 1, 0, 0, 0, 0, time.UTC)
	for index := range bars {
		i := float64(index)
		close := 0.0
		switch {
		case index < 60:
			close = 100 + 2*math.Sin(i*.42)
		case index < 120:
			close = 88 + float64(index-60)*.1 + .8*math.Sin(i*.38)
		default:
			close = 94 + float64(index-120)*.11 + .8*math.Sin(i*.44)
		}
		open := previous
		high := math.Max(open, close) + .55 + float64(index%3)*.07
		low := math.Min(open, close) - .52 - float64(index%4)*.04
		bars[index] = map[string]any{
			"date": start.AddDate(0, 0, index).Format("2006-01-02"), "open": open,
			"high": high, "low": low, "close": close, "volume": int64(500000 + (index%7)*80000),
		}
		previous = close
	}

	strikes := []float64{90, 94, 97, 100, 103, 106, 110}
	openInterests := []int64{1600, 900, 1200, 2800, 1900, 1400, 2200}
	volumes := []int64{260, 180, 210, 440, 300, 250, 390}
	gammas := []float64{
		.017308911349825963, .019687429197265714, .020874872435234445,
		.021493064852288076, .021543493682642693, .02107006702942301, .019757265043808744,
	}
	rows := make([]model.OptionRow, 0, len(strikes)*2)
	for index, strike := range strikes {
		iv := .32 + math.Abs(strike-100)*.006
		premium := math.Max(1.2, 100*iv*.18)
		for _, contractType := range []string{"call", "put"} {
			intrinsic := math.Max(100-strike, 0)
			if contractType == "put" {
				intrinsic = math.Max(strike-100, 0)
			}
			mid := intrinsic + premium
			bid := math.Round(mid*.98*10000) / 10000
			ask := math.Round(mid*1.02*10000) / 10000
			rows = append(rows, model.OptionRow{
				Expiration: "2026-11-04", ContractType: contractType, Strike: pythonGoldenFloat(strike),
				Bid: pythonGoldenFloat(bid), Ask: pythonGoldenFloat(ask), LastPrice: pythonGoldenFloat(math.Round(mid*10000) / 10000),
				Volume: pythonGoldenInt(volumes[index]), OpenInterest: pythonGoldenInt(openInterests[index]),
				ModelIV: pythonGoldenFloat(.642318), ModelGamma: pythonGoldenFloat(gammas[index]), ModelIVSource: "price",
			})
		}
	}
	return bars, rows
}

func pythonGoldenMap(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	switch result := value.(type) {
	case map[string]any:
		return result
	case map[string]string:
		converted := make(map[string]any, len(result))
		for key, item := range result {
			converted[key] = item
		}
		return converted
	default:
		t.Fatalf("%s has type %T, want a string-keyed map", context, value)
	}
	return nil
}

func pythonGoldenLevels(t *testing.T, value any, context string) []map[string]any {
	t.Helper()
	result, ok := value.([]map[string]any)
	if !ok {
		t.Fatalf("%s has type %T, want []map[string]any", context, value)
	}
	return result
}

func pythonGoldenNumber(t *testing.T, value any, want, tolerance float64, context string) {
	t.Helper()
	var got float64
	ok := true
	switch number := value.(type) {
	case float64:
		got = number
	case int:
		got = float64(number)
	case int64:
		got = float64(number)
	default:
		ok = false
	}
	if !ok || math.Abs(got-want) > tolerance {
		t.Errorf("%s=%#v, want %.10f (+/- %.1g)", context, value, want, tolerance)
	}
}

func TestCachedFibonacciCandidatesMatchPythonGolden(t *testing.T) {
	bars, _ := pythonGoldenFixture()
	got := cachedFibonacciLevels(bars)
	want := []cachedLevel{
		{price: 98.07829966107954, score: .6, factor: "斐波那契 23.6%"},
		{price: 95.92257280914413, score: .9, factor: "斐波那契 38.2%"},
		{price: 94.18027302470318, score: 1, factor: "斐波那契 50%"},
		{price: 92.43797324026222, score: 1, factor: "斐波那契 61.8%"},
		{price: 89.95741083529545, score: .6, factor: "斐波那契 78.6%"},
	}
	if len(got) != len(want) {
		t.Fatalf("Fibonacci candidates=%d, want %d", len(got), len(want))
	}
	for index, expected := range want {
		if math.Abs(got[index].price-expected.price) > 1e-11 || math.Abs(got[index].score-expected.score) > 1e-12 || got[index].factor != expected.factor {
			t.Errorf("Fibonacci candidate[%d]=%#v, want Python golden %#v", index, got[index], expected)
		}
	}
}

func TestCachedChipCandidatesMatchPythonGolden(t *testing.T) {
	bars, _ := pythonGoldenFixture()
	got := cachedChipPeaks(bars)
	want := []cachedLevel{
		{price: 89.50278737056153, score: 1, factor: "筹码密集"},
		{price: 91.04500116332366, score: .9437144047702432, factor: "筹码密集"},
		{price: 92.77228061121725, score: .9308667956517406, factor: "筹码密集"},
		{price: 93.20410047319065, score: .8878671940574028, factor: "筹码密集"},
		{price: 97.21385633437221, score: .8153698111868661, factor: "筹码密集"},
		{price: 93.75929743858502, score: .8153095627530377, factor: "筹码密集"},
	}
	if len(got) != len(want) {
		t.Fatalf("chip candidates=%d, want %d", len(got), len(want))
	}
	for index, expected := range want {
		if math.Abs(got[index].price-expected.price) > 1e-10 || math.Abs(got[index].score-expected.score) > 1e-11 || got[index].factor != expected.factor {
			t.Errorf("chip candidate[%d]=%#v, want Python golden %#v", index, got[index], expected)
		}
	}
}

func TestCachedMergePriorityUsesUnroundedCompositeScorePythonGolden(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: .5, factor: "看涨持仓", factors: []string{"看涨持仓"}},
		{price: 101.0001, score: .5003225806451613, factor: "看涨持仓", factors: []string{"看涨持仓"}},
	}, 100, true, .1, .00005, 1, 1)
	if len(levels) != 1 || levels[0]["price"] != 101.0001 {
		t.Fatalf("priority winner=%#v, want farther candidate 101.0001 from Python's unrounded score ranking", levels)
	}
	if levels[0]["score"] != .41 || levels[0]["model_score"] != .41 {
		t.Errorf("rounded score/model_score=%v/%v, want both candidates' Python-rounded 0.41", levels[0]["score"], levels[0]["model_score"])
	}
}

func TestCachedMergeKeepsStablePriorityTieAtCapWithPythonStrengthPropagation(t *testing.T) {
	// Python gives 101.5 and 102 the same unrounded priority (0.2304) after
	// propagation; stable sorting keeps 101.5 in the capped result.
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: .1, factor: "看涨墙", factors: []string{"看涨墙"}},
		{price: 101.5, score: .1, factor: "筹码密集", factors: []string{"筹码密集"}},
		{price: 102, score: .6, factor: "筹码密集", factors: []string{"筹码密集"}},
		{price: 102, score: .37161290322580645, factor: "斐波那契 23.6%", factors: []string{"斐波那契 23.6%"}},
		{price: 106, score: .5, factor: "筹码密集", factors: []string{"筹码密集"}},
	}, 100, true, .1, .01, 2, 1)
	if len(levels) != 2 || levels[0]["price"] != 101.0 || levels[1]["price"] != 101.5 {
		t.Fatalf("cap selected %#v, want Python's stable priority-tie order [101 101.5]", levels)
	}
	if levels[1]["score"] != .58 || levels[1]["model_score"] != .28 {
		t.Errorf("propagated level score/model_score=%v/%v, want Python-rounded 0.58/0.28", levels[1]["score"], levels[1]["model_score"])
	}
}

func TestCachedOptionLevelsMatchPythonUnknownContractDefault(t *testing.T) {
	strike, volume := 95.0, int64(100)
	rows := []model.OptionRow{{ContractType: "CALL", Strike: &strike, Volume: &volume}}
	got, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" {
		t.Fatalf("metric=%q, want volume fallback when the chain has no open interest", metric)
	}
	if len(got) != 1 || got[0].price != strike || got[0].factor != "看跌持仓" {
		t.Fatalf("unknown/case-variant type should follow Python's non-call -> put branch: %#v", got)
	}
}

func TestBuildCachedLevelsMatchesPythonEmptyMarketDataDefaults(t *testing.T) {
	spot := 100.0
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	result := buildCachedLevelsAt("EMPTY", "", nil, nil, model.Quote{}, &spot, &spot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, map[string]any{})
	if result == nil {
		t.Fatal("valid spot with empty bars and chain should still produce the Python empty-data analysis")
	}
	pythonGoldenNumber(t, result["spot"], 100, 0, "empty-data spot")
	pythonGoldenNumber(t, result["candidate_spot"], 100, 0, "empty-data candidate spot")
	pythonGoldenNumber(t, result["history_bars"], 0, 0, "empty-data history bars")
	trend, trendIsMap := result["trend"].(map[string]any)
	if result["options_metric"] != "volume" || !trendIsMap || trend != nil {
		t.Errorf("empty-data defaults metric/trend=%v/%#v, want volume/nil", result["options_metric"], result["trend"])
	}
	if levels := pythonGoldenLevels(t, result["support"], "empty-data support"); len(levels) != 0 {
		t.Errorf("empty-data support=%#v, want empty", levels)
	}
	if levels := pythonGoldenLevels(t, result["resistance"], "empty-data resistance"); len(levels) != 0 {
		t.Errorf("empty-data resistance=%#v, want empty", levels)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal empty-data result: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal empty-data result: %v", err)
	}
	for _, key := range []string{"support", "resistance"} {
		if levels, ok := payload[key].([]any); !ok || len(levels) != 0 {
			t.Errorf("serialized %s=%#v, want Python-compatible empty array", key, payload[key])
		}
	}
	recommendation := pythonGoldenMap(t, result["recommendation"], "empty-data recommendation")
	if recommendation["action"] != "hold" || recommendation["reason"] != "历史趋势或价位数据不足，暂不形成明确买卖信号" {
		t.Errorf("empty-data recommendation=%#v", recommendation)
	}
	buyer := pythonGoldenMap(t, result["buyer_structures"], "empty-data buyer structures")
	if buyer["available"] != false || buyer["status"] != "unavailable" || buyer["reason"] != "当前缓存缺少有效买卖报价，暂不生成买方结构" {
		t.Errorf("empty-data buyer defaults=%#v", buyer)
	}
}

func TestBuildCachedLevelsFiltersCandidateAnchorAtLiveSpotPythonGolden(t *testing.T) {
	bars, _ := pythonGoldenFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 96.5, 100.0
	result := buildCachedLevelsAt("FIX", "", bars, nil, model.Quote{}, &spot, &candidateSpot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, map[string]any{})
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	pythonGoldenNumber(t, result["spot"], 96.5, 0, "live spot")
	pythonGoldenNumber(t, result["candidate_spot"], 100, 0, "candidate spot")
	support := pythonGoldenLevels(t, result["support"], "live-spot support")
	wantSupport := []float64{95.9226, 94.1803, 93.2041, 92.438, 91.045, 89.5028}
	if len(support) != len(wantSupport) {
		t.Fatalf("live-spot support count=%d, want Python-filtered count %d", len(support), len(wantSupport))
	}
	for index, want := range wantSupport {
		pythonGoldenNumber(t, support[index]["price"], want, .0001, "live-spot support["+strconv.Itoa(index)+"].price")
		if support[index]["price"].(float64) >= spot {
			t.Errorf("support[%d]=%v crossed live spot %v", index, support[index]["price"], spot)
		}
	}
	if resistance := pythonGoldenLevels(t, result["resistance"], "live-spot resistance"); len(resistance) != 0 {
		t.Errorf("candidate levels removed from support were reclassified into resistance: %#v", resistance)
	}
}

func TestBuildCachedLevelsMissingOrInvalidExpiryIVMatchesPythonGolden(t *testing.T) {
	bars, _ := pythonGoldenFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 101.0, 100.0
	for _, expiration := range []string{"2026-11-04", "not-a-date"} {
		t.Run(expiration, func(t *testing.T) {
			result := buildCachedLevelsAt("FIX", expiration, bars, nil, model.Quote{}, &spot, &candidateSpot, nil,
				map[string]any{}, map[string]any{}, map[string]any{}, now, map[string]any{})
			if result == nil {
				t.Fatal("buildCachedLevelsAt returned nil")
			}
			if result["expiration"] != expiration || result["options_metric"] != "volume" {
				t.Errorf("expiration/options_metric=%v/%v, want %q/volume", result["expiration"], result["options_metric"], expiration)
			}
			support := pythonGoldenLevels(t, result["support"], "missing-IV support")
			for index, level := range support {
				if level["probability"] != nil {
					t.Errorf("support[%d] probability=%v, want Python None without selected-expiry IV", index, level["probability"])
				}
			}
			tradePoints := pythonGoldenMap(t, result["trade_points"], "missing-IV trade_points")
			buy := pythonGoldenMap(t, tradePoints["buy"], "missing-IV trade_points.buy")
			pythonGoldenNumber(t, buy["price"], 97.7778, .0001, "missing-IV buy price")
			pythonGoldenNumber(t, buy["confidence"], .7799, .0001, "missing-IV buy confidence")
			pythonGoldenNumber(t, buy["model_confidence"], .7544, .0001, "missing-IV buy model_confidence")
		})
	}
}

func TestBuildCachedLevelsMatchesPythonGolden(t *testing.T) {
	bars, rows := pythonGoldenFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 101.0, 100.0
	modelByExpiry := map[string]any{
		"2026-11-04": map[string]any{"iv": .642318, "samples": 4, "source": "price"},
	}
	result := buildCachedLevelsAt("FIX", "2026-11-04", bars, rows, model.Quote{}, &spot, &candidateSpot, nil, map[string]any{}, map[string]any{}, map[string]any{}, now, modelByExpiry)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	if got := result["generated_at"].(time.Time); !got.Equal(now) {
		t.Errorf("generated_at=%s, want %s", got, now)
	}
	pythonGoldenNumber(t, result["spot"], 101, 0, "spot")
	pythonGoldenNumber(t, result["candidate_spot"], 100, 0, "candidate_spot")
	pythonGoldenNumber(t, result["history_bars"], 180, 0, "history_bars")
	pythonGoldenNumber(t, result["options_horizon_days"], 45, 0, "options_horizon_days")
	if result["options_metric"] != "gex" {
		t.Errorf("options_metric=%v, want gex", result["options_metric"])
	}

	trend := pythonGoldenMap(t, result["trend"], "trend")
	if trend["direction"] != "up" {
		t.Errorf("trend direction=%v, want up", trend["direction"])
	}
	if trend["background_direction"] != "up" || trend["reversal_confirmed"] != false {
		t.Errorf("trend metadata=%#v, want up background without reversal", trend)
	}
	pythonGoldenNumber(t, trend["slope_percent"], .118, .0005, "trend slope_percent")
	pythonGoldenNumber(t, trend["upper"], 101.4569, .0001, "trend upper")
	pythonGoldenNumber(t, trend["lower"], 99.8066, .0001, "trend lower")
	pythonGoldenNumber(t, trend["bars"], 60, 0, "trend bars")
	rsi := pythonGoldenMap(t, trend["rsi"], "trend rsi")
	pythonGoldenNumber(t, rsi["value"], 69, .1, "trend rsi.value")
	pythonGoldenNumber(t, rsi["period"], 14, 0, "trend rsi.period")

	recommendation := pythonGoldenMap(t, result["recommendation"], "recommendation")
	if recommendation["action"] != "hold" || recommendation["reason"] != "趋势方向与支撑/压力信号未形成明确共振" {
		t.Errorf("recommendation=%#v, want Python hold recommendation", recommendation)
	}

	support := pythonGoldenLevels(t, result["support"], "support")
	resistance := pythonGoldenLevels(t, result["resistance"], "resistance")
	if len(support) == 0 || len(resistance) == 0 {
		t.Fatalf("support/resistance lengths=%d/%d, want nonempty", len(support), len(resistance))
	}
	assertLevelNumbers := func(context, field string, levels []map[string]any, expected []float64) {
		t.Helper()
		if len(levels) != len(expected) {
			t.Fatalf("%s count=%d, want %d", context, len(levels), len(expected))
		}
		for index, want := range expected {
			pythonGoldenNumber(t, levels[index][field], want, .0001, context+"["+strconv.Itoa(index)+"]."+field)
		}
	}
	assertLevelNumbers("support", "price", support, []float64{97.7778, 97, 95.9226, 94, 93.2041, 92.438, 91.045, 89.5028})
	assertLevelNumbers("support", "score", support, []float64{.79, .71, .58, .92, .74, .78, .69, .86})
	assertLevelNumbers("support", "model_score", support, []float64{.78, .72, .58, .92, .58, .78, .58, .86})
	assertLevelNumbers("support", "probability", support, []float64{.8607, .8269, .7801, .6975, .6638, .6317, .5744, .5131})
	assertLevelNumbers("support", "zone_low", support, []float64{97.3889, 96.5666, 95.4892, 93.602, 92.821, 92.0046, 90.6116, 89.0694})
	assertLevelNumbers("support", "zone_high", support, []float64{98.5117, 97.3889, 96.356, 94.6137, 93.602, 92.821, 91.4784, 90.4334})
	assertLevelNumbers("resistance", "price", resistance, []float64{103, 106, 110})
	assertLevelNumbers("resistance", "score", resistance, []float64{.55, .51, .51})
	assertLevelNumbers("resistance", "model_score", resistance, []float64{.51, .51, .51})
	assertLevelNumbers("resistance", "probability", resistance, []float64{.9155, .7937, .6441})
	assertLevelNumbers("resistance", "zone_low", resistance, []float64{102.5666, 105.5666, 109.5666})
	assertLevelNumbers("resistance", "zone_high", resistance, []float64{103.4334, 106.4334, 110.4334})
	for index, want := range []int{7, 2, 0, 0, 0, 0, 0, 0} {
		pythonGoldenNumber(t, support[index]["history_samples"], float64(want), 0, "support history_samples")
	}
	for index, want := range []int{4, 0, 0} {
		pythonGoldenNumber(t, resistance[index]["history_samples"], float64(want), 0, "resistance history_samples")
	}
	pythonGoldenNumber(t, support[0]["price"], 97.7778, .0001, "first support price")
	pythonGoldenNumber(t, support[0]["score"], .79, .0001, "first support score")
	pythonGoldenNumber(t, support[0]["model_score"], .78, .0001, "first support model_score")
	pythonGoldenNumber(t, support[0]["probability"], .8607, .0001, "first support probability")
	if support[0]["history_samples"] != 7 || support[0]["validation_method"] != "holdout_price_action" {
		t.Errorf("first support history metadata=%#v", support[0])
	}
	pythonGoldenNumber(t, support[0]["history_hold_rate"], 1, 0, "first support history_hold_rate")
	pythonGoldenNumber(t, support[0]["history_adjusted_hold_rate"], .9, .0001, "first support adjusted hold rate")
	pythonGoldenNumber(t, support[0]["history_confidence"], .7, .0001, "first support history confidence")
	pythonGoldenNumber(t, resistance[0]["price"], 103, .0001, "first resistance price")
	pythonGoldenNumber(t, resistance[0]["score"], .55, .0001, "first resistance score")
	pythonGoldenNumber(t, resistance[0]["model_score"], .51, .0001, "first resistance model_score")
	if resistance[0]["history_samples"] != 4 || resistance[0]["validation_method"] != "holdout_price_action" {
		t.Errorf("first resistance history metadata=%#v", resistance[0])
	}
	pythonGoldenNumber(t, resistance[0]["history_adjusted_hold_rate"], .8571, .0001, "first resistance adjusted hold rate")
	pythonGoldenNumber(t, resistance[0]["history_confidence"], .5714, .0001, "first resistance history confidence")

	tradePoints := pythonGoldenMap(t, result["trade_points"], "trade_points")
	buy := pythonGoldenMap(t, tradePoints["buy"], "trade_points.buy")
	sell := pythonGoldenMap(t, tradePoints["sell"], "trade_points.sell")
	pythonGoldenNumber(t, buy["price"], 94, .0001, "buy trade point price")
	pythonGoldenNumber(t, buy["zone_low"], 93.602, .0001, "buy trade point zone_low")
	pythonGoldenNumber(t, buy["zone_high"], 94.6137, .0001, "buy trade point zone_high")
	pythonGoldenNumber(t, buy["confidence"], .8474, .0001, "buy trade point confidence")
	pythonGoldenNumber(t, sell["price"], 103, .0001, "sell trade point price")
	pythonGoldenNumber(t, sell["zone_low"], 102.5666, .0001, "sell trade point zone_low")
	pythonGoldenNumber(t, sell["zone_high"], 103.4334, .0001, "sell trade point zone_high")
	pythonGoldenNumber(t, sell["confidence"], .6179, .0001, "sell trade point confidence")
	pythonGoldenNumber(t, sell["model_confidence"], .578, .0001, "sell trade point model confidence")
	pythonGoldenNumber(t, sell["history_weight"], .1429, .0001, "sell trade point history weight")
	if sell["history_samples"] != 4 {
		t.Errorf("sell trade point history_samples=%v, want 4", sell["history_samples"])
	}
	stopLoss := pythonGoldenMap(t, result["stop_loss"], "stop_loss")
	pythonGoldenNumber(t, stopLoss["price"], 99.3016, .0001, "stop loss price")
	pythonGoldenNumber(t, stopLoss["anchor_price"], 99.8066, .0001, "stop loss anchor")
	if stopLoss["source"] != "趋势通道下轨" {
		t.Errorf("stop loss source=%v, want trend channel lower bound", stopLoss["source"])
	}

	validation := pythonGoldenMap(t, result["history_validation"], "history_validation")
	if validation["method"] != "holdout_price_action" || validation["lookahead_bars"] != 5 || validation["excluded_recent_bars"] != 120 {
		t.Errorf("history_validation=%#v", validation)
	}
	plan := pythonGoldenMap(t, result["plan"], "plan")
	assertLevelNumbers("plan.buy", "price", pythonGoldenLevels(t, plan["buy"], "plan.buy"), []float64{97.7778, 97, 95.9226, 94})
	assertLevelNumbers("plan.add", "price", pythonGoldenLevels(t, plan["add"], "plan.add"), []float64{93.2041, 92.438, 91.045, 89.5028})
	assertLevelNumbers("plan.sell", "price", pythonGoldenLevels(t, plan["sell"], "plan.sell"), []float64{103, 106, 110})

	buyer := pythonGoldenMap(t, result["buyer_structures"], "buyer_structures")
	if buyer["available"] != true || buyer["status"] != "ok" || buyer["direction"] != "neutral" || buyer["primary_direction"] != nil ||
		buyer["direction_label"] != "中性对比：同时观察买入看涨 / 买入看跌" || buyer["horizon_trading_days"] != 5 ||
		buyer["horizon_label"] != "未来 5 个交易日" || buyer["method"] != "布莱克-斯科尔斯模型估算" ||
		buyer["quote_source"] != "server" || buyer["iv_source"] != "server_quote" || buyer["quote_method"] != "仅使用有效买卖价中间价，不使用最新成交价代替成本" {
		t.Fatalf("buyer_structures status=%#v, want available neutral structures", buyer)
	}
	if buyer["recommendation"] != "趋势方向与支撑/压力信号未形成明确共振" {
		t.Errorf("buyer recommendation=%v", buyer["recommendation"])
	}
	pythonGoldenNumber(t, buyer["reference_iv"], .4986101330835363, .0000001, "buyer reference IV")
	targets := pythonGoldenMap(t, buyer["targets"], "buyer targets")
	callTarget := pythonGoldenMap(t, targets["call"], "call target")
	putTarget := pythonGoldenMap(t, targets["put"], "put target")
	if callTarget["source"] != "综合压力位" || putTarget["source"] != "综合支撑位" {
		t.Errorf("buyer target sources=%v/%v", callTarget["source"], putTarget["source"])
	}
	pythonGoldenNumber(t, callTarget["price"], 103, .0001, "call target price")
	pythonGoldenNumber(t, callTarget["touch_probability"], .7764, .0001, "call target touch")
	pythonGoldenNumber(t, putTarget["price"], 97.7778, .0001, "put target price")
	pythonGoldenNumber(t, putTarget["touch_probability"], .6387, .0001, "put target touch")
	items := pythonGoldenLevels(t, buyer["items"], "buyer items")
	if len(items) == 0 {
		t.Fatal("buyer structures are available but contain no items")
	}
	first := items[0]
	if first["kind"] != "vertical" || first["direction"] != "call" || first["expiration"] != "2026-11-04" ||
		first["is_primary"] != false || first["direction_label"] != "看涨方案" || first["target_source"] != "综合压力位" ||
		first["scenario_profitable"] != true || first["model_iv_source"] != "合约中间价反解" ||
		first["quote_method"] != "双腿买卖价中间价" {
		t.Errorf("first buyer structure identity=%#v", first)
	}
	if strikes, ok := first["strikes"].([]float64); !ok || len(strikes) != 2 || strikes[0] != 97 || strikes[1] != 110 {
		t.Errorf("first buyer structure strikes=%#v, want [97 110]", first["strikes"])
	}
	pythonGoldenNumber(t, first["dte"], 30, 0, "first buyer structure dte")
	pythonGoldenNumber(t, first["iv"], .7476367462, .0000001, "first buyer structure IV")
	pythonGoldenNumber(t, first["delta"], .2069068568, .0000001, "first buyer structure delta")
	pythonGoldenNumber(t, first["target_price"], 103, .0001, "first buyer structure target")
	pythonGoldenNumber(t, first["scenario_return"], .4043851121, .0000001, "first buyer structure scenario return")
	pythonGoldenNumber(t, first["expected_return"], .2133700404, .0000001, "first buyer structure expected return")
	pythonGoldenNumber(t, first["score"], .6664569653, .0000001, "first buyer structure score")
	pythonGoldenNumber(t, first["touch_probability"], .7764, .0001, "first buyer structure touch probability")
	pythonGoldenNumber(t, first["max_loss"], 224.4, .0001, "first buyer structure max loss")
	pythonGoldenNumber(t, first["max_profit"], 1075.6, .0001, "first buyer structure max profit")
	pythonGoldenNumber(t, first["breakeven"], 99.244, .0001, "first buyer structure breakeven")
}

func TestBuildCachedLevelsBuyRecommendationAndBuyerOrderMatchPythonGolden(t *testing.T) {
	bars, rows := pythonGoldenFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 98.0, 100.0
	modelByExpiry := map[string]any{
		"2026-11-04": map[string]any{"iv": .642318, "samples": 4, "source": "price"},
	}
	result := buildCachedLevelsAt("FIX", "2026-11-04", bars, rows, model.Quote{}, &spot, &candidateSpot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, modelByExpiry)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	recommendation := pythonGoldenMap(t, result["recommendation"], "buy recommendation")
	if recommendation["action"] != "buy" || recommendation["reason"] != "上行趋势，现价接近支撑位" {
		t.Fatalf("recommendation=%#v, want Python buy recommendation", recommendation)
	}
	tradePoints := pythonGoldenMap(t, result["trade_points"], "buy trade points")
	buyPoint := pythonGoldenMap(t, tradePoints["buy"], "buy trade point")
	pythonGoldenNumber(t, buyPoint["price"], 94, 0, "buy trade point price")
	pythonGoldenNumber(t, buyPoint["confidence"], .8944, .0001, "buy trade point confidence")

	buyer := pythonGoldenMap(t, result["buyer_structures"], "buy buyer structures")
	if buyer["available"] != true || buyer["direction"] != "call" || buyer["primary_direction"] != "call" || buyer["direction_label"] != "主方向：买入看涨 · 同时对比买入看跌" {
		t.Fatalf("buyer direction metadata=%#v", buyer)
	}
	pythonGoldenNumber(t, buyer["reference_iv"], .6779856021702289, .0000001, "buyer reference IV")
	targets := pythonGoldenMap(t, buyer["targets"], "buyer targets")
	callTarget := pythonGoldenMap(t, targets["call"], "call target")
	putTarget := pythonGoldenMap(t, targets["put"], "put target")
	pythonGoldenNumber(t, callTarget["price"], 103, 0, "call target price")
	pythonGoldenNumber(t, callTarget["touch_probability"], .5961, .0001, "call target touch probability")
	pythonGoldenNumber(t, putTarget["price"], 97.7778, .0001, "put target price")
	pythonGoldenNumber(t, putTarget["touch_probability"], .9807, .0001, "put target touch probability")
	items := pythonGoldenLevels(t, buyer["items"], "buy buyer items")
	want := []struct {
		kind, direction string
		strikes         []float64
		primary         bool
		expected, score float64
	}{
		{"vertical", "call", []float64{97, 110}, true, .24949032, .65040668},
		{"single", "call", []float64{100}, true, .11834872, .81886237},
		{"vertical", "call", []float64{97, 106}, true, .10463306, .72927647},
		{"single", "call", []float64{97}, true, .06405216, .79281971},
		{"single", "call", []float64{103}, true, .05292407, .82826774},
		{"vertical", "put", []float64{103, 90}, false, .09862232, .66833707},
		{"vertical", "put", []float64{103, 94}, false, .00265836, .72527106},
		{"vertical", "put", []float64{106, 94}, false, -.03179586, .73319606},
		{"vertical", "put", []float64{106, 97}, false, -.06092764, .72946935},
		{"single", "put", []float64{106}, false, -.07836846, .7711978},
	}
	if len(items) != len(want) {
		t.Fatalf("buyer item count=%d, want Python's %d sorted structures", len(items), len(want))
	}
	for index, expected := range want {
		item := items[index]
		if item["kind"] != expected.kind || item["direction"] != expected.direction || item["is_primary"] != expected.primary {
			t.Errorf("buyer item[%d] identity=%#v, want kind=%s direction=%s primary=%v", index, item, expected.kind, expected.direction, expected.primary)
		}
		strikes, ok := item["strikes"].([]float64)
		if !ok || len(strikes) != len(expected.strikes) {
			t.Errorf("buyer item[%d] strikes=%#v, want %v", index, item["strikes"], expected.strikes)
			continue
		}
		for strikeIndex, strike := range expected.strikes {
			if strikes[strikeIndex] != strike {
				t.Errorf("buyer item[%d] strike[%d]=%v, want %v", index, strikeIndex, strikes[strikeIndex], strike)
			}
		}
		pythonGoldenNumber(t, item["expected_return"], expected.expected, .0000001, "buyer item["+strconv.Itoa(index)+"].expected_return")
		pythonGoldenNumber(t, item["score"], expected.score, .0000001, "buyer item["+strconv.Itoa(index)+"].score")
	}
}

func TestBuildCachedLevelsSellRecommendationAndBuyerOrderMatchPythonGolden(t *testing.T) {
	bars, rows := pythonGoldenFixture()
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 105.0, 100.0
	modelByExpiry := map[string]any{
		"2026-11-04": map[string]any{"iv": .642318, "samples": 4, "source": "price"},
	}
	result := buildCachedLevelsAt("FIX", "2026-11-04", bars, rows, model.Quote{}, &spot, &candidateSpot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, modelByExpiry)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	recommendation := pythonGoldenMap(t, result["recommendation"], "sell recommendation")
	if recommendation["action"] != "sell" || recommendation["reason"] != "上行通道上沿附近，现价接近压力位" {
		t.Fatalf("recommendation=%#v, want Python sell recommendation", recommendation)
	}
	tradePoints := pythonGoldenMap(t, result["trade_points"], "sell trade points")
	sellPoint := pythonGoldenMap(t, tradePoints["sell"], "sell trade point")
	pythonGoldenNumber(t, sellPoint["price"], 106, 0, "sell trade point price")
	pythonGoldenNumber(t, sellPoint["confidence"], .5964, .0001, "sell trade point confidence")

	buyer := pythonGoldenMap(t, result["buyer_structures"], "sell buyer structures")
	if buyer["available"] != true || buyer["direction"] != "put" || buyer["primary_direction"] != "put" || buyer["direction_label"] != "主方向：买入看跌 · 同时对比买入看涨" {
		t.Fatalf("buyer direction metadata=%#v", buyer)
	}
	pythonGoldenNumber(t, buyer["reference_iv"], .7784482681777367, .0000001, "buyer reference IV")
	targets := pythonGoldenMap(t, buyer["targets"], "buyer targets")
	callTarget := pythonGoldenMap(t, targets["call"], "call target")
	putTarget := pythonGoldenMap(t, targets["put"], "put target")
	pythonGoldenNumber(t, callTarget["price"], 106, 0, "call target price")
	pythonGoldenNumber(t, callTarget["touch_probability"], .9299, .0001, "call target touch probability")
	pythonGoldenNumber(t, putTarget["price"], 97.7778, .0001, "put target price")
	pythonGoldenNumber(t, putTarget["touch_probability"], .5086, .0001, "put target touch probability")
	items := pythonGoldenLevels(t, buyer["items"], "sell buyer items")
	want := []struct {
		kind, direction string
		strikes         []float64
		primary         bool
		expected, score float64
	}{
		{"vertical", "put", []float64{103, 90}, true, .25810395, .64912486},
		{"vertical", "put", []float64{103, 94}, true, .09777658, .70668701},
		{"single", "put", []float64{100}, true, .09192875, .78237905},
		{"single", "put", []float64{103}, true, .04151752, .76401819},
		{"single", "put", []float64{97}, true, .02624058, .76147656},
		{"vertical", "call", []float64{97, 110}, false, .37374163, .77286738},
		{"vertical", "call", []float64{97, 106}, false, .20181802, .83735988},
		{"single", "call", []float64{100}, false, .0801882, .76730605},
		{"vertical", "call", []float64{97, 103}, false, .073419, .75662517},
		{"single", "call", []float64{97}, false, .03279145, .70711124},
	}
	if len(items) != len(want) {
		t.Fatalf("buyer item count=%d, want Python's %d sorted structures", len(items), len(want))
	}
	for index, expected := range want {
		item := items[index]
		if item["kind"] != expected.kind || item["direction"] != expected.direction || item["is_primary"] != expected.primary {
			t.Errorf("buyer item[%d] identity=%#v, want kind=%s direction=%s primary=%v", index, item, expected.kind, expected.direction, expected.primary)
		}
		strikes, ok := item["strikes"].([]float64)
		if !ok || len(strikes) != len(expected.strikes) {
			t.Errorf("buyer item[%d] strikes=%#v, want %v", index, item["strikes"], expected.strikes)
			continue
		}
		for strikeIndex, strike := range expected.strikes {
			if strikes[strikeIndex] != strike {
				t.Errorf("buyer item[%d] strike[%d]=%v, want %v", index, strikeIndex, strikes[strikeIndex], strike)
			}
		}
		pythonGoldenNumber(t, item["expected_return"], expected.expected, .0000001, "buyer item["+strconv.Itoa(index)+"].expected_return")
		pythonGoldenNumber(t, item["score"], expected.score, .0000001, "buyer item["+strconv.Itoa(index)+"].score")
	}
}
