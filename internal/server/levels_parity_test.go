package server

import (
	"math"
	"strings"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestMergeCachedLevelsRoundsScoresAndZones(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{{
		price: 110.123456, score: 0.5, factors: []string{"看涨持仓"},
	}}, 100, true, 0.33334, 0.01, 10, 1)
	if len(levels) != 1 {
		t.Fatalf("got %d merged levels, want one", len(levels))
	}
	level := levels[0]
	if level["score"] != 0.41 || level["model_score"] != 0.41 {
		t.Errorf("scores=%v/%v, want Python-rounded score 0.41", level["score"], level["model_score"])
	}
	if level["zone_low"] != 109.7901 || level["zone_high"] != 110.4568 {
		t.Errorf("zone=%v..%v, want Python-rounded boundaries 109.7901..110.4568", level["zone_low"], level["zone_high"])
	}
	for _, field := range []string{"raw_low", "raw_high", "priority"} {
		if _, exists := level[field]; exists {
			t.Errorf("internal merge field %q leaked into the Python-compatible level payload", field)
		}
	}
}

func TestMergeCachedLevelsCutsOverlappingZonesLikePythonGolden(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: .3, factor: "筹码密集", factors: []string{"筹码密集"}},
		{price: 103, score: .5, factor: "筹码密集", factors: []string{"筹码密集"}},
	}, 100, true, 1.5, .1, 10, 1)
	if len(levels) != 2 {
		t.Fatalf("overlap merge returned %d levels, want Python's two candidates: %#v", len(levels), levels)
	}
	for index, want := range []struct {
		price, score, low, high float64
	}{{101, .34, 99.5, 102}, {103, .41, 102, 104.5}} {
		level := levels[index]
		if level["price"] != want.price || level["score"] != want.score || level["zone_low"] != want.low || level["zone_high"] != want.high {
			t.Errorf("level[%d]=%#v, want Python merge_candidates output price=%v score=%v zone=%v..%v", index, level, want.price, want.score, want.low, want.high)
		}
	}
}

func TestMergeCachedLevelsUsesPythonPriorityArithmetic(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 98, score: 0.4032258064516129, factor: "far", factors: []string{"far"}},
		{price: 99, score: 0, factor: "near", factors: []string{"near"}},
	}, 100, false, 0.01, 0, 1, 1)
	if len(levels) != 1 || levels[0]["price"] != 99.0 {
		t.Fatalf("Python's ratio-based priority arithmetic should rank 99 above 98, got %#v", levels)
	}
}

func TestMergeCachedLevelsZeroToleranceUsesPythonDefault(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 100.4, score: 0.5, factor: "筹码密集", factors: []string{"筹码密集"}},
		{price: 100.7, score: 0.4, factor: "承接位", factors: []string{"承接位"}},
	}, 100, true, 0.25, 0, 10, 1)
	if len(levels) != 1 {
		t.Fatalf("zero tolerance should use Python's 0.5%% default and merge 0.3%%-apart candidates, got %#v", levels)
	}
	factors, ok := levels[0]["factors"].([]string)
	if !ok || len(factors) != 2 {
		t.Fatalf("merged factors=%#v, want both independent candidates", levels[0]["factors"])
	}
}

func TestMergeCachedLevelsNegativeToleranceDoesNotUsePythonDefault(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 100.4, score: 0.5, factor: "筹码密集", factors: []string{"筹码密集"}},
		{price: 100.7, score: 0.4, factor: "承接位", factors: []string{"承接位"}},
	}, 100, true, 0.25, -1, 10, 1)
	if len(levels) != 2 {
		t.Fatalf("negative tolerance should clamp to zero instead of using the default, got %#v", levels)
	}
}

func TestMergeCachedLevelsNonfiniteToleranceUsesPythonDefault(t *testing.T) {
	for _, tolerance := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		levels := mergeCachedLevelsWeighted([]cachedLevel{
			{price: 100.4, score: 0.5, factor: "筹码密集", factors: []string{"筹码密集"}},
			{price: 100.7, score: 0.4, factor: "承接位", factors: []string{"承接位"}},
		}, 100, true, 0.25, tolerance, 10, 1)
		if len(levels) != 1 {
			t.Errorf("non-finite tolerance %v should use Python's 0.5%% default, got %#v", tolerance, levels)
		}
	}
}

func TestMergeCachedLevelsFloorsZoneWidthLikePython(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 105, score: 0.5, factor: "筹码密集", factors: []string{"筹码密集"}},
	}, 100, true, 0, 0.1, 10, 1)
	if len(levels) != 1 {
		t.Fatalf("got %d levels, want one", len(levels))
	}
	if levels[0]["zone_low"] != 104.75 || levels[0]["zone_high"] != 105.25 {
		t.Fatalf("zone=%v..%v, want Python minimum-width bounds 104.75..105.25", levels[0]["zone_low"], levels[0]["zone_high"])
	}
}

func TestMergeCachedLevelsRejectsNonpositiveSpotLikePython(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: 0.5, factor: "筹码密集", factors: []string{"筹码密集"}},
	}, 0, true, 1, 0.5, 10, 1)
	if len(levels) != 0 {
		t.Fatalf("nonpositive spot should produce no levels, got %#v", levels)
	}
}

func TestMergeCachedLevelsExplicitNonpositiveLimitMatchesPython(t *testing.T) {
	values := []cachedLevel{{price: 101, score: .5, factors: []string{"筹码密集"}}}
	for _, limit := range []int{0, -1} {
		if got := mergeCachedLevels(values, 100, true, 1, .5, limit); len(got) != 0 {
			t.Errorf("explicit limit %d returned %d levels, want empty", limit, len(got))
		}
		if got := mergeCachedLevelsWeighted(values, 100, true, 1, .5, limit, 1); len(got) != 0 {
			t.Errorf("weighted explicit limit %d returned %d levels, want empty", limit, len(got))
		}
	}
}

func TestCachedATRReadsNumericStringsLikePythonNumberAdapter(t *testing.T) {
	bars := []map[string]any{
		{"high": "11", "low": "9", "close": "10"},
		{"high": "12", "low": "10", "close": "11"},
	}
	if got := cachedATR(bars, 2); got != 2 {
		t.Fatalf("ATR from numeric strings=%v, want 2 like Python _number", got)
	}
}

func TestNumberValueMatchesPythonFloatCoercionsAndFiniteGuard(t *testing.T) {
	for _, test := range []struct {
		value any
		want  float64
	}{
		{true, 1},
		{false, 0},
		{int32(-7), -7},
		{uint16(12), 12},
		{" 12.5 ", 12.5},
	} {
		got := numberValue(test.value)
		if got == nil || *got != test.want {
			t.Errorf("numberValue(%#v)=%v, want %v", test.value, got, test.want)
		}
	}
	for _, value := range []any{nil, "", "NaN", "+Inf", "invalid", math.NaN(), math.Inf(1)} {
		if got := numberValue(value); got != nil {
			t.Errorf("numberValue(%#v)=%v, want nil", value, *got)
		}
	}
}

func TestBuildCachedLevelsUsesOnlyLatestCloseForSpotFallback(t *testing.T) {
	bars := []map[string]any{
		{"date": "2026-10-07", "open": 99.0, "high": 101.0, "low": 98.0, "close": 100.0},
		{"date": "2026-10-08", "open": 100.0, "high": 102.0, "low": 99.0},
	}
	result := buildCachedLevels(
		"TEST", "", bars, nil, model.Quote{}, nil, nil, nil,
		map[string]any{}, map[string]any{}, map[string]any{},
	)
	if result != nil {
		t.Fatalf("Python falls back only to the latest close; Go used an older close and built %#v", result["spot"])
	}
}

func TestRoundCachedUsesPythonTiesToEven(t *testing.T) {
	for _, test := range []struct {
		value float64
		want  float64
	}{{2.5, 2}, {3.5, 4}, {-2.5, -2}} {
		if got := roundCached(test.value, 0); got != test.want {
			t.Errorf("roundCached(%v, 0)=%v, want Python round result %v", test.value, got, test.want)
		}
	}
}

func TestCachedLevelStrengthTierFallsBackToRawHistoryRates(t *testing.T) {
	level := map[string]any{
		"score": .8, "model_score": .8, "factors": []string{"承接位"},
		"history_samples": 6, "history_hold_rate": .8, "history_break_rate": .2,
		"history_adjusted_hold_rate": nil, "history_adjusted_break_rate": nil,
	}
	if got := cachedLevelStrengthTier(level); got != "strong" {
		t.Fatalf("tier=%q, want strong using Python raw-rate and sample-confidence fallbacks", got)
	}
}

func TestCachedLevelStrengthTierKeepsHighCompositeEvidenceReinforced(t *testing.T) {
	level := map[string]any{
		"score": .78, "model_score": .86,
		"factors":         []string{"筹码密集", "斐波那契 78.6%", "看涨持仓"},
		"history_samples": 8, "history_confidence": .7273,
		"history_adjusted_hold_rate": .7273, "history_adjusted_break_rate": .2727,
		"history_reaction_rate": .8,
	}
	if got := cachedLevelStrengthTier(level); got != "reinforced" {
		t.Fatalf("tier=%q, want reinforced for high-score independent evidence that misses strong break-rate threshold", got)
	}
}

func TestCachedLevelStrengthTierReinforcesNearStrongOptionWall(t *testing.T) {
	level := map[string]any{
		"price": 550.0, "score": .69, "model_score": .70,
		"factors":         []string{"看涨墙"},
		"history_samples": 2, "history_confidence": .4,
		"history_adjusted_hold_rate": .8, "history_adjusted_break_rate": .2,
	}
	if got := cachedLevelStrengthTier(level); got != "reinforced" {
		t.Fatalf("tier=%q, want reinforced for qualifying option wall below strong score with limited history", got)
	}
}

func TestCachedLevelStrengthTierRejectsBrokenOptionWallDespiteLowSampleCount(t *testing.T) {
	level := map[string]any{
		"score": .69, "model_score": .72, "factors": []string{"看涨墙"},
		"history_samples": 2, "history_confidence": .4,
		"history_adjusted_hold_rate": .39, "history_adjusted_break_rate": .61,
		"history_reaction_rate": .5,
	}
	if got := cachedLevelStrengthTier(level); got != "normal" {
		t.Fatalf("tier=%q, want normal when the identified wall already shows poor reactions", got)
	}
}

func TestCachedLevelStrengthTierUsesBacktestCalibratedFocusThreshold(t *testing.T) {
	level := map[string]any{
		"price": 140.8123, "score": .59, "model_score": .69,
		"factors":         []string{"斐波那契 23.6%", "看跌持仓"},
		"history_samples": 10, "history_confidence": .7692,
		"history_adjusted_hold_rate": .5385, "history_adjusted_break_rate": .4615,
		"history_reaction_rate": .9,
	}
	if got := cachedLevelStrengthTier(level); got != "reinforced" {
		t.Fatalf("tier=%q, want reinforced at the calibrated historical cutoff", got)
	}
}

func TestCachedLevelStrengthTierRejectsBelowCalibratedBoundaries(t *testing.T) {
	base := map[string]any{
		"score": .55, "model_score": .55, "factors": []string{"筹码密集", "看跌持仓"},
		"history_samples": 5, "history_confidence": .625,
		"history_adjusted_hold_rate": .4, "history_adjusted_break_rate": .6,
		"history_reaction_rate": .9,
	}
	tests := []struct {
		name  string
		field string
		value any
	}{
		{"model score", "model_score", .54},
		{"sample count", "history_samples", 4},
		{"reaction rate", "history_reaction_rate", .79},
		{"hold rate", "history_adjusted_hold_rate", .39},
		{"break rate", "history_adjusted_break_rate", .61},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			level := make(map[string]any, len(base))
			for key, value := range base {
				level[key] = value
			}
			level[test.field] = test.value
			if got := cachedLevelStrengthTier(level); got != "normal" {
				t.Fatalf("tier=%q, want normal below calibrated %s boundary", got, test.name)
			}
		})
	}
}

func TestCachedLevelStrengthTierRejectsWeakHistoryDespiteSeveralFactors(t *testing.T) {
	level := map[string]any{
		"score": .72, "model_score": .82,
		"factors":         []string{"筹码密集", "斐波那契 50%", "看跌持仓"},
		"history_samples": 4, "history_confidence": .5714,
		"history_adjusted_hold_rate": .39, "history_adjusted_break_rate": .61,
		"history_reaction_rate": .4,
		"recent_samples":        3, "recent_reactions": 2,
	}
	if got := cachedLevelStrengthTier(level); got != "normal" {
		t.Fatalf("tier=%q, want normal when poor hold/break history defeats model score and factor count", got)
	}
}

func TestCachedLevelStrengthTierDoesNotUseRecentReactionsWithoutHistoricalCalibration(t *testing.T) {
	base := map[string]any{
		"score": .68, "model_score": .72,
		"factors":         []string{"筹码密集", "斐波那契 61.8%"},
		"history_samples": 3, "history_confidence": .5,
		"history_adjusted_hold_rate": .7, "history_adjusted_break_rate": .3,
		"history_reaction_rate": .5,
		"recent_samples":        3, "recent_reactions": 3,
	}
	if got := cachedLevelStrengthTier(base); got != "normal" {
		t.Fatalf("tier=%q, want normal until the full historical calibration sample is met", got)
	}
	base["history_samples"] = 5
	base["history_reaction_rate"] = .8
	base["history_adjusted_hold_rate"] = .4
	base["history_adjusted_break_rate"] = .6
	if got := cachedLevelStrengthTier(base); got != "reinforced" {
		t.Fatalf("tier=%q, want reinforced after historical calibration passes", got)
	}
}

func TestCachedLevelHistoryParsesPythonNumbersAndRejectsInvalidLevels(t *testing.T) {
	bars := make([]map[string]any, 140)
	for i := range bars {
		bars[i] = map[string]any{"high": 101.0, "low": 99.0, "close": 100.0}
	}
	levels := []map[string]any{
		{"price": "100", "zone_low": "99", "zone_high": "101", "score": .7},
		{"price": "bad", "zone_low": 99.0, "zone_high": 101.0, "score": .7},
		{"price": 100.0, "zone_low": math.NaN(), "zone_high": 101.0, "score": .7},
		{"price": 0.0, "zone_low": 99.0, "zone_high": 101.0, "score": .7},
	}
	got := annotateCachedLevelHistory(levels, bars, "support")
	if got[0]["validation_method"] != "holdout_price_action" {
		t.Errorf("numeric strings should be accepted like Python: %#v", got[0])
	}
	for index := 1; index < len(got); index++ {
		if got[index]["validation_method"] != "invalid_level" || got[index]["history_samples"] != 0 || got[index]["history_confidence"] != 0.0 {
			t.Errorf("invalid level %d should receive empty invalid_level history: %#v", index, got[index])
		}
	}
}

func TestCachedLevelStrengthTierAcceptsJSONStyleFactorsAndCounts(t *testing.T) {
	level := map[string]any{
		"score": .6, "model_score": .6, "factors": []any{"筹码密集", "承接位"},
		"history_samples": "5", "history_reaction_rate": "0.8",
		"history_adjusted_hold_rate": "0.4", "history_adjusted_break_rate": "0.6",
	}
	if got := cachedLevelStrengthTier(level); got != "reinforced" {
		t.Fatalf("tier=%q, want reinforced using Python list and numeric-string behavior", got)
	}
}

func TestMergeCachedLevelsAppliesPythonTrendWeightBeforeAndAfterComposite(t *testing.T) {
	makeLevel := func(trendWeight float64) map[string]any {
		levels := mergeCachedLevelsWeighted([]cachedLevel{{
			price: 110, score: 0.5, factors: []string{"看涨持仓"},
		}}, 100, true, 1, 1, 10, trendWeight)
		if len(levels) != 1 {
			t.Fatalf("got %d merged levels for trend weight %v, want one", len(levels), trendWeight)
		}
		return levels[0]
	}
	if got := makeLevel(0.9)["model_score"]; got != 0.37 {
		t.Errorf("0.9 trend weight score=%v, want Python-compatible 0.37", got)
	}
	if got := makeLevel(1.15)["model_score"]; got != 0.45 {
		t.Errorf("1.15 trend weight score=%v, want Python-compatible 0.45", got)
	}
}

func TestCachedBuyerReferenceIVUsesNearestPairedATMQuote(t *testing.T) {
	contract := func(strike, iv float64, source string) *cachedBuyerContract {
		return &cachedBuyerContract{row: model.OptionRow{Strike: &strike}, iv: iv, ivSource: source}
	}
	groups := []*cachedBuyerGroup{
		{expiration: "2030-01-18", dte: 10, rows: []*cachedBuyerContract{
			contract(100.25, 0.2, "contract"),
			contract(101, 0.3, "contract"),
			contract(101, 0.35, "contract"),
			contract(104, 0.9, "contract"),
		}},
		{expiration: "2030-01-25", dte: 17, rows: []*cachedBuyerContract{
			contract(100, 0.8, "contract"),
			contract(100, 0.82, "contract"),
		}},
	}
	if got := cachedBuyerReferenceIV(groups, 100); math.Abs(got-0.325) > 1e-12 {
		t.Fatalf("reference IV=%v, want paired nearest-ATM mean 0.325", got)
	}
}

func TestCachedBuyerReferenceIVUsesCrossExpiryMedianFallback(t *testing.T) {
	firstStrike, secondStrike := 100.0, 100.0
	groups := []*cachedBuyerGroup{
		{expiration: "2030-01-18", dte: 10, rows: []*cachedBuyerContract{
			{row: model.OptionRow{Strike: &firstStrike}, iv: 0.5, ivSource: "expiration"},
			{row: model.OptionRow{Strike: &firstStrike}, iv: 0.1, ivSource: "expiration"},
		}},
		{expiration: "2030-01-25", dte: 17, rows: []*cachedBuyerContract{
			{row: model.OptionRow{Strike: &secondStrike}, iv: 0.3, ivSource: "expiration"},
			{row: model.OptionRow{Strike: &secondStrike}, iv: 0.2, ivSource: "expiration"},
		}},
	}
	if got := cachedBuyerReferenceIV(groups, 100); got != 0.3 {
		t.Fatalf("reference IV=%v, want upper median across expirations 0.3", got)
	}
}

func TestCachedBuyerSelectDiverseMatchesPythonSimilarityAndKindFallback(t *testing.T) {
	item := func(kind, expiration string, strikes []float64, expected, score float64) map[string]any {
		return map[string]any{
			"kind": kind, "expiration": expiration, "strikes": strikes,
			"expected_return": expected, "score": score,
		}
	}
	verticals := []map[string]any{
		item("vertical", "2030-01-18", []float64{100, 105}, 0.9, 0.9),
		item("vertical", "2030-01-18", []float64{100.5, 105.5}, 0.85, 0.8),
		item("vertical", "2030-01-18", []float64{102, 107}, 0.8, 0.7),
		item("vertical", "2030-01-18", []float64{104, 109}, 0.7, 0.6),
		item("vertical", "2030-01-18", []float64{106, 111}, 0.6, 0.5),
		item("vertical", "2030-01-18", []float64{108, 113}, 0.5, 0.4),
	}
	single := item("single", "2030-01-18", []float64{100}, 0.4, 0.3)
	ranked := append(verticals, single)
	selected := cachedBuyerSelectDiverse(ranked, 5, 100)
	if len(selected) != 5 {
		t.Fatalf("selected %d structures, want 5", len(selected))
	}
	if selected[0]["expected_return"] != 0.9 || selected[1]["expected_return"] != 0.8 {
		t.Fatalf("near-duplicate vertical should be skipped: %#v", selected)
	}
	if selected[len(selected)-1]["kind"] != "single" {
		t.Fatalf("single-leg structure should replace lowest-ranked vertical: %#v", selected)
	}
}

func TestCachedBestTradePointAcceptsMissingTrend(t *testing.T) {
	price := 100.0
	levels := []map[string]any{{"price": 95.0, "score": 0.8, "factors": []string{"支撑"}}}
	if got := cachedBestTradePoint(nil, levels, price, 0.25, 0.5, true); got == nil {
		t.Fatal("expected a trade point with no trend metadata")
	}
}

func TestCachedBestTradePointParsesPythonNumericStringsAndFactors(t *testing.T) {
	levels := []map[string]any{{
		"price": "95", "score": "0.8", "model_score": "0.8",
		"factors": []any{"筹码密集", "承接位"}, "probability": "0.4",
		"history_samples": "3", "history_confidence": "0.5",
	}}
	point := cachedBestTradePoint(nil, levels, 100, 0, 0, true)
	if point == nil || point["price"] != 95.0 || point["history_samples"] != 3 || point["reason"] != "多因子共振" {
		t.Fatalf("numeric strings and JSON-style factors should match Python parsing: %#v", point)
	}
	factors, ok := point["factors"].([]string)
	if !ok || len(factors) != 2 || factors[0] != "筹码密集" || factors[1] != "承接位" {
		t.Fatalf("factors=%#v, want both labels", point["factors"])
	}
}

func TestCachedBuyerTargetCanUseFartherStrongCandidate(t *testing.T) {
	resistance := []map[string]any{
		{"price": 108.0, "score": 0.55},
		{"price": 125.0, "score": 0.95},
	}
	target, source, _, _ := cachedBuyerTarget("call", 100, nil, resistance, 0, 0)
	if target != 125 || source != "综合压力位" {
		t.Fatalf("buyer target=%v/%q, want farther strong candidate 125/综合压力位", target, source)
	}
}

func TestCachedPayloadAvailableRejectsMalformedShapes(t *testing.T) {
	invalid := []any{
		struct{}{},
		map[string]any{"dates": []any{struct{}{}}},
		map[string]any{"beta": math.Inf(1)},
		[]any{map[string]any{"value": math.NaN()}},
	}
	for _, value := range invalid {
		if cachedPayloadAvailable(value) {
			t.Errorf("malformed cached payload %#v should not be available", value)
		}
	}
	if !cachedPayloadAvailable(float64(0)) {
		t.Fatal("finite numeric payload should be available")
	}
}

func TestLevelsAnalysisCacheVersionInvalidatesPreviousAlgorithmResults(t *testing.T) {
	key := levelsAnalysisCacheKey("ORCL", "2026-10-16", nil, nil, model.Quote{}, nil, nil, nil, nil, nil, nil, nil)
	if !strings.HasPrefix(key, "levels-v25:") {
		t.Fatalf("levels cache key=%q, want current v21 namespace", key)
	}
	if strings.HasPrefix(key, "levels-v20:") {
		t.Fatalf("levels cache key reused stale v20 namespace: %q", key)
	}
}

func TestCachedOptionLevelsUsesProviderIVWhenModelGammaUnavailable(t *testing.T) {
	spot, strike := 100.0, 105.0
	bid, ask := 4.0, 5.0
	openInterest := int64(1000)
	iv := 0.3
	expiration := time.Now().AddDate(0, 0, 30).In(time.FixedZone("America/New_York", -5*60*60)).Format("2006-01-02")
	for _, modelGamma := range []*float64{nil, float64Pointer(0), float64Pointer(math.NaN()), float64Pointer(math.Inf(1))} {
		rows := []model.OptionRow{{ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask, OpenInterest: &openInterest, ImpliedVolatility: &iv, ModelGamma: modelGamma, Expiration: expiration}}
		levels, metric := cachedOptionLevels(rows, spot)
		if metric != "gex" {
			t.Fatalf("metric=%q, want gex", metric)
		}
		if len(levels) != 1 || levels[0].price != strike || math.IsNaN(levels[0].score) || math.IsInf(levels[0].score, 0) {
			t.Fatalf("provider IV fallback should produce a finite GEX candidate at strike %.2f: %#v", strike, levels)
		}
	}
}

func TestCachedBuyerStructuresUsesProviderIVAndBuildsScenarioFields(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	strike := 100.0
	bid, ask, providerIV := 0.0098, 0.0102, 0.4
	rows := []model.OptionRow{{
		ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask,
		ImpliedVolatility: &providerIV, Expiration: expiration,
	}}
	result := cachedBuyerStructures(rows, 100, map[string]string{"action": "buy"}, nil, nil)
	if result["available"] != true {
		t.Fatalf("expected available structures, got %#v", result)
	}
	items, ok := result["items"].([]map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected at least one structure, got %#v", result["items"])
	}
	item := items[0]
	for _, key := range []string{"delta", "target_price", "target_source", "scenario_return", "scenario_profitable", "touch_probability", "expected_return"} {
		if _, exists := item[key]; !exists {
			t.Errorf("structure missing %q: %#v", key, item)
		}
	}
	for _, key := range []string{"theta_per_day", "vega"} {
		if _, exists := item[key]; exists {
			t.Errorf("single-leg response added Python-incompatible field %q: %#v", key, item)
		}
	}
	if source, _ := item["model_iv_source"].(string); source != "合约中间价反解" && source != "到期日模型估算" {
		t.Errorf("model_iv_source=%q, want recognized source", source)
	}
	if item["iv"] != providerIV {
		if iv, ok := item["iv"].(float64); !ok || math.Abs(iv-providerIV) > 1e-6 {
			t.Errorf("iv=%#v, want provider fallback %v", item["iv"], providerIV)
		}
	}
}

func TestBuildCachedLevelsReusesIVEstimatedFromFullChain(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().In(location).AddDate(0, 0, 30).Format("2006-01-02")
	spot := 100.0
	strike := 100.0
	rows := []model.OptionRow{
		{Expiration: expiration, ContractType: "call", Strike: &strike, Bid: floatPointer(.009), Ask: floatPointer(.011), ImpliedVolatility: floatPointer(.8)},
		{Expiration: expiration, ContractType: "call", Strike: &strike, Bid: floatPointer(.0099), Ask: floatPointer(.0101), ImpliedVolatility: floatPointer(.2)},
		{Expiration: expiration, ContractType: "call", Strike: &strike, Bid: floatPointer(.0095), Ask: floatPointer(.0105), ImpliedVolatility: floatPointer(.9)},
	}
	result := buildCachedLevels(
		"TEST", expiration, nil, rows, model.Quote{}, &spot, &spot, nil,
		map[string]any{}, map[string]any{}, map[string]any{},
	)
	if result == nil {
		t.Fatal("expected levels response")
	}
	structures, ok := result["buyer_structures"].(map[string]any)
	if !ok {
		t.Fatalf("buyer_structures has type %T", result["buyer_structures"])
	}
	items, ok := structures["items"].([]map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected buyer structure, got %#v", structures)
	}
	if got := items[0]["iv"]; got != .8 {
		t.Fatalf("buyer IV=%v, want full-chain provider median .8 (not deduped-row IV .2)", got)
	}
}

func TestCachedBuyerEmptyResponseMatchesPythonShape(t *testing.T) {
	result := cachedBuyerStructures(nil, 100, nil, nil, nil, nil)
	for _, key := range []string{"available", "status", "direction", "horizon_trading_days", "horizon_label", "items", "reason", "method", "quote_source", "iv_source", "quote_method"} {
		if _, exists := result[key]; !exists {
			t.Errorf("empty response missing Python field %q: %#v", key, result)
		}
	}
	for _, key := range []string{"primary_direction", "direction_label", "targets", "reference_iv"} {
		if _, exists := result[key]; exists {
			t.Errorf("empty response added Python-incompatible field %q: %#v", key, result)
		}
	}
}

func TestCachedBuyerStructuresExcludesWideSpreadContracts(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 30).In(time.FixedZone("America/New_York", -5*60*60)).Format("2006-01-02")
	goodStrike, wideStrike := 100.0, 105.0
	goodBid, goodAsk := 4.9, 5.1
	wideBid, wideAsk := 0.9, 1.2
	rows := []model.OptionRow{
		{ContractType: "call", Strike: &goodStrike, Bid: &goodBid, Ask: &goodAsk, Expiration: expiration},
		{ContractType: "call", Strike: &wideStrike, Bid: &wideBid, Ask: &wideAsk, Expiration: expiration},
	}
	result := cachedBuyerStructures(rows, 100, map[string]string{"action": "buy"}, nil, nil)
	if result["available"] != true {
		t.Fatalf("expected the narrow-spread contract to remain usable: %#v", result)
	}
	items, ok := result["items"].([]map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected buyer candidates, got %#v", result["items"])
	}
	for _, item := range items {
		strikes, _ := item["strikes"].([]float64)
		for _, strike := range strikes {
			if strike == wideStrike {
				t.Errorf("contract with spread above 10%% entered a buyer structure: %#v", item)
			}
		}
	}
}

func TestCachedBuyerWideSpreadEmptyReasonMatchesPython(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().In(location).AddDate(0, 0, 30).Format("2006-01-02")
	strike, bid, ask := 100.0, 0.9, 1.2
	result := cachedBuyerStructures([]model.OptionRow{{
		Expiration: expiration, ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask,
	}}, 100, map[string]string{"action": "buy"}, nil, nil)
	if result["available"] != false || result["reason"] != "当前期权链没有满足报价和流动性条件的候选结构" {
		t.Fatalf("wide but valid quote should report no eligible structure like Python: %#v", result)
	}
}

func TestCachedBuyerIVSourceOnlyUsesReturnedExpirationGroups(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(location)
	selectedExpiration := now.AddDate(0, 0, 30).Format("2006-01-02")
	unusedExpiration := now.AddDate(0, 0, 2).Format("2006-01-02")
	strike, bid, ask, providerIV := 100.0, 149.0, 151.0, 0.3
	rows := []model.OptionRow{
		{Expiration: selectedExpiration, ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask, ImpliedVolatility: &providerIV},
		{Expiration: unusedExpiration, ContractType: "call", Strike: &strike},
	}
	result := cachedBuyerStructures(rows, 100, map[string]string{"action": "buy"}, nil, nil)
	if result["available"] != true {
		t.Fatalf("selected provider-IV expiration should produce structures: %#v", result)
	}
	items, ok := result["items"].([]map[string]any)
	if !ok || len(items) == 0 || items[0]["model_iv_source"] != "到期日模型估算" {
		t.Fatalf("expected an expiration-IV candidate from selected expiration: %#v", result["items"])
	}
	if result["iv_source"] != "server" {
		t.Fatalf("unused default-IV expiration leaked into response iv_source=%v, want server", result["iv_source"])
	}
}

func TestCachedBuyerVerticalReportsBoundedRiskAndScenario(t *testing.T) {
	longStrike, shortStrike := 100.0, 105.0
	long := &cachedBuyerContract{
		row: model.OptionRow{ContractType: "call", Strike: &longStrike, Expiration: "2030-01-18"},
		mid: 5, spreadRatio: 0.02, dte: 30, years: 0.1, activityScore: 0.8,
		iv: 0.3, ivSource: "contract", delta: 0.6, thetaPerDay: -0.02, vega: 0.1,
	}
	short := &cachedBuyerContract{
		row: model.OptionRow{ContractType: "call", Strike: &shortStrike, Expiration: "2030-01-18"},
		mid: 2, spreadRatio: 0.02, dte: 30, years: 0.1, activityScore: 0.7,
		iv: 0.3, ivSource: "contract", delta: 0.4, thetaPerDay: -0.01, vega: 0.08,
	}
	item := cachedBuyerVertical(long, short, "call", 105, "综合压力位", 100, 5.0/365, 0.5, true)
	if item == nil {
		t.Fatal("expected valid debit vertical")
	}
	if item["kind"] != "vertical" || item["max_loss"] != 300.0 || item["max_profit"] != 200.0 {
		t.Fatalf("unexpected vertical risk fields: %#v", item)
	}
	for _, key := range []string{"target_price", "target_source", "scenario_return", "scenario_profitable", "touch_probability", "expected_return", "net_theta_per_day", "net_vega"} {
		if _, exists := item[key]; !exists {
			t.Errorf("vertical missing %q: %#v", key, item)
		}
	}
	for _, key := range []string{"theta_per_day", "vega"} {
		if _, exists := item[key]; exists {
			t.Errorf("vertical response added Python-incompatible field %q: %#v", key, item)
		}
	}
}
