package server

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestCachedLevelsOptionCandidatesCrossLiveSpotLikePython(t *testing.T) {
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 100.5, 100.0
	rows := []model.OptionRow{
		{Expiration: "2026-11-04", ContractType: "call", Strike: pythonGoldenFloat(99.5), Volume: pythonGoldenInt(100)},
		{Expiration: "2026-11-04", ContractType: "call", Strike: pythonGoldenFloat(100.25), Volume: pythonGoldenInt(100)},
	}
	result := buildCachedLevelsAt("FIX", "2026-11-04", nil, rows, model.Quote{}, &spot, &candidateSpot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, nil)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	if result["options_metric"] != "volume" {
		t.Fatalf("options_metric=%v, want Python volume fallback", result["options_metric"])
	}
	support := pythonGoldenLevels(t, result["support"], "cross-live-spot support")
	if len(support) != 1 {
		t.Fatalf("support=%#v, want only the 99.5 option candidate", support)
	}
	pythonGoldenNumber(t, support[0]["price"], 99.5, 0, "cross-live-spot support price")
	if resistance := pythonGoldenLevels(t, result["resistance"], "cross-live-spot resistance"); len(resistance) != 0 {
		t.Errorf("candidate 100.25 crossed live spot but was reclassified as resistance: %#v", resistance)
	}
}

func TestCachedLevelsDoesNotUseOtherExpiryIVForSelectedExpiry(t *testing.T) {
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	spot, candidateSpot := 100.0, 100.0
	rows := []model.OptionRow{
		{Expiration: "2026-12-04", ContractType: "call", Strike: pythonGoldenFloat(95), Volume: pythonGoldenInt(100)},
		{Expiration: "2026-12-04", ContractType: "call", Strike: pythonGoldenFloat(105), Volume: pythonGoldenInt(100)},
	}
	modelByExpiry := map[string]any{
		"2026-12-04": map[string]any{"iv": .65, "samples": 2, "source": "provider"},
	}
	result := buildCachedLevelsAt("FIX", "2026-11-04", nil, rows, model.Quote{}, &spot, &candidateSpot, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, modelByExpiry)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	if result["expiration"] != "2026-11-04" {
		t.Fatalf("expiration=%v, want selected expiry 2026-11-04", result["expiration"])
	}
	for _, side := range []string{"support", "resistance"} {
		levels := pythonGoldenLevels(t, result[side], "other-expiry-IV "+side)
		if len(levels) != 1 {
			t.Fatalf("%s=%#v, want one option candidate", side, levels)
		}
		if levels[0]["probability"] != nil {
			t.Errorf("%s probability=%v, want nil because selected-expiry IV is missing", side, levels[0]["probability"])
		}
	}
}

func TestBuildCachedLevelsDowntrendMixedFactorsMatchesPythonGolden(t *testing.T) {
	bars := make([]map[string]any, 60)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := range bars {
		close := 130 - float64(index)*.5 + .5*float64(index%5-2)
		bars[index] = map[string]any{
			"date": start.AddDate(0, 0, index).Format("2006-01-02"),
			"high": close + 1, "low": close - 1, "close": close, "volume": 1000,
		}
	}
	expiration := "2026-12-18"
	callStrike, putStrike := 102.0, 98.0
	callVolume, putVolume := int64(100), int64(100)
	callOI, putOI := int64(0), int64(0)
	providerIV := .5
	rows := []model.OptionRow{
		{Expiration: expiration, ContractType: "call", Strike: &callStrike, Volume: &callVolume, OpenInterest: &callOI, ImpliedVolatility: &providerIV},
		{Expiration: expiration, ContractType: "put", Strike: &putStrike, Volume: &putVolume, OpenInterest: &putOI, ImpliedVolatility: &providerIV},
	}
	spot, anchor := 101.0, 100.0
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	modelByExpiry := map[string]any{expiration: map[string]any{"iv": providerIV, "samples": 2, "source": "provider"}}
	result := buildCachedLevelsAt("DOWN", expiration, bars, rows, model.Quote{}, &spot, &anchor, nil,
		map[string]any{}, map[string]any{}, map[string]any{}, now, modelByExpiry)
	if result == nil {
		t.Fatal("buildCachedLevelsAt returned nil")
	}
	trend := pythonGoldenMap(t, result["trend"], "downtrend")
	if trend["direction"] != "down" || trend["label"] != "下行通道 · 下跌趋势" || trend["background_direction"] != "down" || trend["reversal_confirmed"] != false {
		t.Fatalf("downtrend metadata=%#v, want Python descending-channel golden", trend)
	}
	pythonGoldenNumber(t, trend["slope_percent"], -.431, .000001, "downtrend slope")
	pythonGoldenNumber(t, trend["upper"], 101.6555, .0001, "downtrend upper channel")
	pythonGoldenNumber(t, trend["lower"], 99.5412, .0001, "downtrend lower channel")

	if result["options_metric"] != "volume" {
		t.Fatalf("options_metric=%v, want volume fallback for zero-OI chain", result["options_metric"])
	}
	support := pythonGoldenLevels(t, result["support"], "downtrend support")
	resistance := pythonGoldenLevels(t, result["resistance"], "downtrend resistance")
	if len(support) != 1 || len(resistance) == 0 {
		t.Fatalf("support/resistance counts=%d/%d, want one support and mixed-factor resistance candidates", len(support), len(resistance))
	}
	pythonGoldenNumber(t, support[0]["price"], 98, 0, "downtrend support price")
	pythonGoldenNumber(t, support[0]["score"], .51, .0001, "downtrend support score")
	pythonGoldenNumber(t, resistance[0]["price"], 101.5448, .0001, "mixed-factor resistance price")
	pythonGoldenNumber(t, resistance[0]["score"], .72, .0001, "mixed-factor resistance score")
	if got := cachedFactorStrings(resistance[0]["factors"]); len(got) != 2 || got[0] != "筹码密集" || got[1] != "看涨持仓" {
		t.Errorf("mixed-factor resistance factors=%v, want [筹码密集 看涨持仓]", got)
	}
	recommendation := pythonGoldenMap(t, result["recommendation"], "downtrend recommendation")
	if recommendation["action"] != "sell" || recommendation["reason"] != "下行趋势，现价接近压力位" {
		t.Errorf("downtrend recommendation=%#v, want Python sell signal", recommendation)
	}
	tradePoints := pythonGoldenMap(t, result["trade_points"], "downtrend trade points")
	sell := pythonGoldenMap(t, tradePoints["sell"], "downtrend sell point")
	pythonGoldenNumber(t, sell["price"], 101.5448, .0001, "downtrend sell point price")
	pythonGoldenNumber(t, sell["confidence"], .8095, .0001, "downtrend sell point confidence")
	plan := pythonGoldenMap(t, result["plan"], "downtrend plan")
	planSell := pythonGoldenLevels(t, plan["sell"], "downtrend plan sell")
	if len(planSell) == 0 || planSell[0]["price"] != resistance[0]["price"] {
		t.Errorf("downtrend plan sell=%#v, want nearest mixed-factor resistance first", planSell)
	}
}

func TestCachedLevelBuilderCapsCandidatesAtPythonThirty(t *testing.T) {
	candidates := make([]cachedLevel, 35)
	for index := range candidates {
		candidates[index] = cachedLevel{
			price: float64(index + 101), score: .2,
			factor: "看涨持仓", factors: []string{"看涨持仓"},
		}
	}
	got := mergeCachedLevelsWeighted(candidates, 100, true, .1, .01, cachedCandidateLimit, 1)
	if len(got) != 30 {
		t.Fatalf("candidate count=%d, want Python's per-side 30 candidate cap", len(got))
	}
	for index, level := range got {
		pythonGoldenNumber(t, level["price"], float64(index+101), 0, "capped candidate price")
	}
}

func TestCachedFibonacciFallbackKeepsZeroLowLikePython(t *testing.T) {
	bars := make([]map[string]any, 5)
	for index := range bars {
		bars[index] = map[string]any{"high": float64(10 + index), "low": float64(index), "close": float64(5 + index)}
	}

	got := cachedFibonacciLevels(bars)
	want := []float64{10.696, 8.652, 7, 5.348, 2.996}
	if len(got) != len(want) {
		t.Fatalf("Fibonacci candidates=%#v, want five Python fallback levels", got)
	}
	for index, price := range want {
		pythonGoldenNumber(t, got[index].price, price, 1e-12, "zero-low Fibonacci price")
	}
}

func TestCachedChipCandidatesUseBothOHLCBoundsLikePython(t *testing.T) {
	// Python derives the bucket range from every high and low, then swaps an
	// inverted bar's endpoints while distributing its volume.
	bars := make([]map[string]any, 12)
	for index := range bars {
		bars[index] = map[string]any{
			"low": float64(101 + index), "high": float64(99 + index),
			"close": float64(100 + index), "volume": 100.0,
		}
	}

	got := cachedChipPeaks(bars)
	want := []cachedLevel{
		{price: 108.99375, score: 1, factor: "筹码密集"},
		{price: 106.98958333333333, score: .999605697271662, factor: "筹码密集"},
		{price: 104.98541666666667, score: .9992197266624371, factor: "筹码密集"},
		{price: 102.98125, score: .9988418631209914, factor: "筹码密集"},
		{price: 101.03125, score: .9911157357595116, factor: "筹码密集"},
		{price: 101.95208333333333, score: .9785073040825996, factor: "筹码密集"},
	}
	if len(got) != len(want) {
		t.Fatalf("chip candidates=%#v, want %d Python golden peaks", got, len(want))
	}
	for index, expected := range want {
		if math.Abs(got[index].price-expected.price) > 1e-10 || math.Abs(got[index].score-expected.score) > 1e-11 || got[index].factor != expected.factor {
			t.Errorf("chip candidate[%d]=%#v, want Python golden %#v", index, got[index], expected)
		}
	}
}

func TestCachedLevelHistoryUsesATRAtHistoricalTouch(t *testing.T) {
	base := map[string]any{"high": 98.0, "low": 97.0, "close": 97.5, "volume": 1000}
	touch := map[string]any{"high": 101.0, "low": 99.0, "close": 100.5, "volume": 1000}
	future := map[string]any{"high": 100.0, "low": 98.0, "close": 98.5, "volume": 1000}
	spike := map[string]any{"high": 120.0, "low": 110.0, "close": 115.0, "volume": 1000}
	bars := make([]map[string]any, 0, 145)
	for range 5 {
		bars = append(bars, base)
	}
	bars = append(bars, touch, future)
	for range 14 {
		bars = append(bars, spike)
	}
	for range 120 {
		bars = append(bars, base)
	}
	levels := []map[string]any{{
		"price": 100.0, "zone_low": 99.0, "zone_high": 101.0,
		"score": 0.8, "factors": []string{"筹码密集", "看跌持仓"},
	}}

	got := annotateCachedLevelHistory(levels, bars, "support")[0]
	if got["history_samples"] != 1 || got["history_break_rate"] != 1.0 || got["history_confidence"] != 0.25 {
		t.Fatalf("history metrics=%#v, want one touch broken at its historical ATR and confidence 0.25", got)
	}
	pythonGoldenNumber(t, got["score"], 0.77, 0, "historical-ATR calibrated score")
	if got["validation_method"] != "holdout_price_action" || got["recent_samples"] != 1 || got["recent_reactions"] != 1 {
		t.Errorf("history metadata=%#v, want holdout sample and recent reaction", got)
	}
}

func TestCachedFibonacciIgnoresSingleWickLikePython(t *testing.T) {
	bars := make([]map[string]any, 40)
	for index := range bars {
		price := 100 + float64(index)*0.5
		high := price + 0.4
		if index == 20 {
			high = 180
		}
		bars[index] = map[string]any{"high": high, "low": price - 0.4, "close": price}
	}

	got := cachedFibonacciLevels(bars)
	want := []float64{115.1092, 112.1454, 109.75, 107.3546, 103.9442}
	if len(got) != len(want) {
		t.Fatalf("Fibonacci candidates=%#v, want five confirmed-swing levels", got)
	}
	for index, price := range want {
		pythonGoldenNumber(t, got[index].price, price, 1e-10, "single-wick Fibonacci price")
	}
}

func TestCachedMergeUsesFirstPriceAsNonChainingClusterAnchor(t *testing.T) {
	got := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: 0.2, factor: "看涨持仓", factors: []string{"看涨持仓"}},
		{price: 101.9, score: 0.3, factor: "看涨持仓", factors: []string{"看涨持仓"}},
		{price: 102.8, score: 0.4, factor: "看涨持仓", factors: []string{"看涨持仓"}},
	}, 100, true, 0.25, 1, 10, 1)
	if len(got) != 2 {
		t.Fatalf("clustered levels=%#v, want 101/101.9 together and 102.8 separate", got)
	}
	if got[0]["price"] != 101.9 || got[1]["price"] != 102.8 {
		t.Fatalf("cluster representatives=%v/%v, want 101.9 and 102.8", got[0]["price"], got[1]["price"])
	}
	pythonGoldenNumber(t, got[0]["score"], 0.41, 0, "first cluster score")
	pythonGoldenNumber(t, got[1]["score"], 0.37, 0, "second cluster score")
	if got[0]["zone_low"] != 100.75 || got[0]["zone_high"] != 102.15 {
		t.Errorf("first cluster zone=%v..%v, want Python bounds 100.75..102.15", got[0]["zone_low"], got[0]["zone_high"])
	}
}

func TestCachedAbsorptionKeepsHeldDipsAndExcludesBrokenOrFlatLows(t *testing.T) {
	bars := make([]map[string]any, 0, 27)
	for range 10 {
		bars = append(bars, map[string]any{"high": 111.0, "low": 108.0, "close": 110.0, "volume": 100.0})
	}
	bars = append(bars, map[string]any{"high": 110.0, "low": 100.0, "close": 109.5, "volume": 300.0})
	for range 6 {
		bars = append(bars, map[string]any{"high": 116.0, "low": 109.0, "close": 115.0, "volume": 120.0})
	}
	bars = append(bars,
		map[string]any{"high": 115.0, "low": 104.0, "close": 114.5, "volume": 300.0},
		map[string]any{"high": 113.0, "low": 102.0, "close": 108.0, "volume": 150.0},
		map[string]any{"high": 112.0, "low": 103.0, "close": 109.0, "volume": 150.0},
	)
	for range 2 {
		bars = append(bars, map[string]any{"high": 110.0, "low": 103.0, "close": 109.0, "volume": 150.0})
	}
	for range 6 {
		bars = append(bars, map[string]any{"high": 114.0, "low": 109.0, "close": 113.0, "volume": 120.0})
	}

	got := cachedAbsorptionLevels(bars, 113)
	if len(got) != 2 {
		t.Fatalf("absorption candidates=%#v, want only held pivots at 102 and 100", got)
	}
	for index, want := range []struct{ price, score float64 }{{102, 0.6}, {100, 0.72}} {
		pythonGoldenNumber(t, got[index].price, want.price, 0, "absorption price")
		pythonGoldenNumber(t, got[index].score, want.score, 1e-12, "absorption score")
		if got[index].factor != "承接位" {
			t.Errorf("absorption factor[%d]=%q, want 承接位", index, got[index].factor)
		}
	}
}

func TestCachedOptionLevelsUsesPythonNonCallGEXSignForCaseVariant(t *testing.T) {
	spot, strike, modelGamma := 100.0, 105.0, 0.02
	providerIV := 0.5
	openInterest := int64(1000)
	rows := []model.OptionRow{{
		Expiration: "2027-12-17", ContractType: "CALL", Strike: &strike, OpenInterest: &openInterest,
		ImpliedVolatility: &providerIV, ModelGamma: &modelGamma,
	}}

	levels, metric := cachedOptionLevels(rows, spot)
	if metric != "gex" {
		t.Fatalf("metric=%q, want gex with valid open interest", metric)
	}
	if len(levels) != 1 {
		t.Fatalf("case-variant contract type should follow Python's non-call put GEX branch, got %#v", levels)
	}
	if levels[0].price != strike || levels[0].factor != "看跌持仓" {
		t.Fatalf("case-variant GEX candidate=%#v, want put candidate at strike %v", levels[0], strike)
	}
	if levels[0].score != 1 {
		t.Errorf("case-variant GEX score=%v, want Python's single-candidate score 1", levels[0].score)
	}
}

func TestCachedOptionLevelsRecomputesGammaFromIVLikePython(t *testing.T) {
	strikeNear, strikeFar := 120.0, 150.0
	openInterestNear, openInterestFar := int64(1000), int64(1000)
	modelIV := 0.5
	staleGammaNear, staleGammaFar := 0.000001, 1.0
	rows := []model.OptionRow{
		{Expiration: "2027-12-17", ContractType: "call", Strike: &strikeNear, OpenInterest: &openInterestNear, ModelIV: &modelIV, ModelGamma: &staleGammaNear},
		{Expiration: "2027-12-17", ContractType: "call", Strike: &strikeFar, OpenInterest: &openInterestFar, ModelIV: &modelIV, ModelGamma: &staleGammaFar},
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "gex" || len(levels) != 2 {
		t.Fatalf("metric/levels=%q/%#v, want two GEX candidates", metric, levels)
	}
	if levels[0].price != strikeNear {
		t.Fatalf("GEX ranking=%v then %v, want the near-ATM strike first after recomputing Gamma from IV", levels[0].price, levels[1].price)
	}
}

func TestCachedOptionLevelsUseInjectedClockForPythonGEXGolden(t *testing.T) {
	rows := make([]model.OptionRow, 0, 6)
	for _, item := range []struct {
		strike float64
		oi     int64
		kind   string
	}{
		{97, 1000, "put"}, {98, 500, "put"}, {99, 300, "put"},
		{101, 1000, "call"}, {102, 500, "call"}, {103, 300, "call"},
	} {
		strike, oi, iv, volume := item.strike, item.oi, 0.35, int64(10)
		rows = append(rows, model.OptionRow{
			Expiration: "2026-11-04", ContractType: item.kind, Strike: &strike,
			OpenInterest: &oi, Volume: &volume, ModelIV: &iv,
		})
	}
	now := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC)
	levels, metric := cachedOptionLevelsAt(rows, 100, now)
	if metric != "gex" || len(levels) != 6 {
		t.Fatalf("metric/levels=%q/%#v, want six GEX candidates", metric, levels)
	}
	want := []struct {
		price, score float64
		factor       string
	}{
		{101, 1, "看涨墙"}, {102, 1, "看涨持仓"}, {103, .8165911537624794, "看涨持仓"},
		{97, 1, "看跌墙"}, {98, 1, "看跌持仓"}, {99, .8679807425878487, "看跌持仓"},
	}
	for index, expected := range want {
		got := levels[index]
		if got.price != expected.price || got.factor != expected.factor || math.Abs(got.score-expected.score) > 1e-12 {
			t.Errorf("candidate[%d]=%#v, want fixed-clock Python GEX golden %#v", index, got, expected)
		}
	}
}

func TestCachedOptionWallThresholdBoundariesMatchPython(t *testing.T) {
	makeRows := func(strikes []float64, volumes []int64) []model.OptionRow {
		rows := make([]model.OptionRow, len(strikes))
		for index, strike := range strikes {
			strike, volume := strike, volumes[index]
			rows[index] = model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volume}
		}
		return rows
	}
	assertPeakFactor := func(name string, rows []model.OptionRow, want string) {
		t.Helper()
		levels, metric := cachedOptionLevels(rows, 100)
		if metric != "volume" || len(levels) == 0 {
			t.Fatalf("%s metric/levels=%q/%#v, want volume candidates", name, metric, levels)
		}
		if levels[0].factor != want {
			t.Errorf("%s peak factor=%q, want %q", name, levels[0].factor, want)
		}
	}

	assertPeakFactor("fewer than three strikes", makeRows([]float64{105, 110}, []int64{10, 18}), "看涨持仓")
	assertPeakFactor("prominence threshold equality", makeRows([]float64{103, 105, 109}, []int64{10, 10, 18}), "看涨墙")

	strikes := make([]float64, 24)
	volumes := make([]int64, 24)
	for index := range strikes {
		strikes[index] = float64(101 + index)
		volumes[index] = 50
	}
	volumes[len(volumes)-1] = 100
	assertPeakFactor("8 percent share threshold equality", makeRows(strikes, volumes), "看涨墙")
}

func TestCachedOptionLevelsSumsSideValuesInPythonStrikeOrder(t *testing.T) {
	rows := make([]model.OptionRow, 0, 3)
	for index, volume := range []int64{1, 1, 1 << 53} {
		strike := float64(101 + index)
		volumeValue := volume
		rows = append(rows, model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volumeValue})
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" || len(levels) != 3 {
		t.Fatalf("metric/levels=%q/%#v, want three volume candidates", metric, levels)
	}
	if levels[0].price != 103 || levels[1].price != 102 {
		t.Fatalf("candidate ordering=%v/%v, want descending (exposure, strike)", levels[0].price, levels[1].price)
	}

	// Python sums ascending-strike values [1, 1, 2^53] before sorting by
	// exposure. Summing after ranking loses the two small values in float64.
	peak := float64(int64(1) << 53)
	sideTotal := float64(1) + float64(1) + peak
	share := float64(1) / sideTotal
	want := 0.7/peak + 0.3*share/cachedOptionWallShare
	pythonGoldenNumber(t, levels[1].score, want, 0, "small option candidate score")
}

func TestCachedOptionMetricUsesAggregatedStrikeOrderLikePython(t *testing.T) {
	largeStrike, middleStrike, smallStrike := 90.0, 100.0, 110.0
	largeVolume, middleVolume, smallVolume := int64(9007199254740894), int64(99), int64(2)
	openInterest, providerIV := int64(1801439850948199), 0.5
	expiration := "2030-01-18"
	rows := []model.OptionRow{
		{Expiration: expiration, ContractType: "call", Strike: &smallStrike, Volume: &smallVolume, ImpliedVolatility: &providerIV},
		{Expiration: expiration, ContractType: "call", Strike: &middleStrike, Volume: &middleVolume, ImpliedVolatility: &providerIV},
		{Expiration: expiration, ContractType: "call", Strike: &largeStrike, Volume: &largeVolume, ImpliedVolatility: &providerIV},
	}
	rows[0].OpenInterest = &openInterest

	pythonVolume := float64(largeVolume)
	pythonVolume += float64(middleVolume)
	pythonVolume += float64(smallVolume)
	inputOrderVolume := float64(smallVolume)
	inputOrderVolume += float64(middleVolume)
	inputOrderVolume += float64(largeVolume)
	if pythonVolume != 9007199254740994 || inputOrderVolume != 9007199254740996 ||
		float64(openInterest) < pythonVolume*0.2 || float64(openInterest) >= inputOrderVolume*0.2 {
		t.Fatalf("fixture does not straddle Python's 20%% threshold: Python=%v input-order=%v OI=%v", pythonVolume, inputOrderVolume, openInterest)
	}

	_, metric := cachedOptionLevelsAt(rows, 100, time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC))
	if metric != "gex" {
		t.Fatalf("metric=%q, want gex after per-strike aggregation and ascending-strike summation", metric)
	}
}

func TestCachedOptionGEXMetricThresholdEqualityMatchesPython(t *testing.T) {
	strikeA, strikeB, modelGamma := 105.0, 110.0, 0.01
	volumeA, volumeB := int64(100), int64(10)
	openInterestA, openInterestB := int64(10), int64(12)
	expiration, providerIV := "2030-01-18", 0.5
	rows := []model.OptionRow{
		{Expiration: expiration, ContractType: "call", Strike: &strikeA, Volume: &volumeA, OpenInterest: &openInterestA, ImpliedVolatility: &providerIV, ModelGamma: &modelGamma},
		{Expiration: expiration, ContractType: "call", Strike: &strikeB, Volume: &volumeB, OpenInterest: &openInterestB, ImpliedVolatility: &providerIV, ModelGamma: &modelGamma},
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "gex" {
		t.Fatalf("OI/volume ratio exactly 20%% selected metric %q, want gex", metric)
	}
	if len(levels) != 2 || levels[0].price != strikeB {
		t.Fatalf("GEX threshold equality should rank the larger-OI 110 strike first, got %#v", levels)
	}

	belowThreshold := int64(11)
	rows[1].OpenInterest = &belowThreshold
	levels, metric = cachedOptionLevels(rows, 100)
	if metric != "volume" {
		t.Fatalf("OI/volume ratio just below 20%% selected metric %q, want volume", metric)
	}
	if len(levels) != 2 || levels[0].price != strikeA {
		t.Fatalf("below-threshold volume fallback should rank the 105 strike first, got %#v", levels)
	}
	if math.Abs(levels[1].score-0.4109090909090909) > 1e-12 {
		t.Errorf("below-threshold 110 score=%.16g, want Python volume score %.16g", levels[1].score, 0.4109090909090909)
	}
}

func TestCachedOptionLevelsCapsEachSideAtPythonTwelve(t *testing.T) {
	rows := make([]model.OptionRow, 15)
	for index := range rows {
		strike := float64(101 + index)
		volume := int64(index + 1)
		rows[index] = model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volume}
	}
	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" || len(levels) != 12 {
		t.Fatalf("metric/level count=%q/%d, want volume/12", metric, len(levels))
	}
	wantLevels := []struct{ strike, score float64 }{
		{115, 1}, {114, 1}, {113, 1}, {112, 0.935}, {111, 0.8570833333333332}, {110, 0.7791666666666666},
		{109, 0.70125}, {108, 0.6233333333333333}, {107, 0.5454166666666665}, {106, 0.4675},
		{105, 0.3895833333333333}, {104, 0.31166666666666665},
	}
	for index, want := range wantLevels {
		wantStrike := want.strike
		if levels[index].price != wantStrike {
			t.Errorf("option level[%d]=%v, want %v after Python per-side cap", index, levels[index].price, wantStrike)
		}
		pythonGoldenNumber(t, levels[index].score, want.score, 1e-12, "capped option level score")
	}
}

func TestCachedOptionLevelsRetainsSignedAggregateInputsLikePython(t *testing.T) {
	strikeA, strikeB := 105.0, 110.0
	positiveVolume, negativeVolume, otherVolume := int64(10), int64(-15), int64(6)
	rows := []model.OptionRow{
		{ContractType: "call", Strike: &strikeA, Volume: &positiveVolume},
		{ContractType: "call", Strike: &strikeA, Volume: &negativeVolume},
		{ContractType: "call", Strike: &strikeB, Volume: &otherVolume},
	}

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" || len(levels) != 1 {
		t.Fatalf("signed volume aggregation returned metric/levels=%q/%#v, want volume and only positive-net strike", metric, levels)
	}
	if levels[0].price != strikeB || levels[0].factor != "看涨持仓" || levels[0].score != 1 {
		t.Fatalf("signed aggregate candidate=%#v, want only strike 110 at score 1", levels[0])
	}

	strikeC, strikeD := 105.0, 110.0
	volumeC, volumeD := int64(25), int64(5)
	openInterestC, openInterestD := int64(30), int64(-25)
	modelGamma := 0.01
	rows = []model.OptionRow{
		{ContractType: "call", Strike: &strikeC, Volume: &volumeC, OpenInterest: &openInterestC, ModelGamma: &modelGamma},
		{ContractType: "call", Strike: &strikeD, Volume: &volumeD, OpenInterest: &openInterestD, ModelGamma: &modelGamma},
	}
	levels, metric = cachedOptionLevels(rows, 100)
	if metric != "volume" || len(levels) != 2 || levels[0].price != strikeC {
		t.Fatalf("signed OI total selected metric/levels=%q/%#v, want volume with strike 105 first", metric, levels)
	}
	pythonGoldenNumber(t, levels[1].score, 0.765, 1e-12, "signed-OI volume fallback score")
}

func TestCachedOptionLevelsRejectsNonfiniteStrikesLikePython(t *testing.T) {
	volume, validStrike := int64(100), 105.0
	invalidStrikes := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	rows := make([]model.OptionRow, 0, len(invalidStrikes)+1)
	for _, strike := range invalidStrikes {
		strike := strike
		rows = append(rows, model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volume})
	}
	rows = append(rows, model.OptionRow{ContractType: "call", Strike: &validStrike, Volume: &volume})

	levels, metric := cachedOptionLevels(rows, 100)
	if metric != "volume" {
		t.Fatalf("metric=%q, want volume", metric)
	}
	if len(levels) != 1 || levels[0].price != validStrike || levels[0].factor != "看涨持仓" {
		t.Fatalf("nonfinite strikes should be discarded like Python; got %#v, want only strike %v", levels, validStrike)
	}
}
