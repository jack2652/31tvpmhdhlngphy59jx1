package server

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/gamma"
	"option-scope-go/internal/model"
)

func TestCachedBuyerEligibilityUsesPythonTradingHorizonAndCalendarDTE(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Friday's five-trading-day horizon ends the following Friday at the
	// same local time, skipping the weekend. Expiration on that date is excluded.
	now := time.Date(2026, time.October, 9, 16, 0, 0, 0, location)
	cases := []struct {
		expiration string
		wantDTE    int
		eligible   bool
	}{
		{"2026-10-16", 7, false},
		{"2026-10-17", 8, true},
		{"2026-10-19", 10, true},
		{"2026-10-10", 1, false},
		{"2026-10-08", 0, false},
		{"2026-10-09", 0, false},
	}
	for _, test := range cases {
		dte, eligible, valid := cachedBuyerExpirationEligibility(test.expiration, now, 5)
		if dte != test.wantDTE || eligible != test.eligible || !valid {
			t.Errorf("eligibility(%q)=(%d,%v,valid=%v), want (%d,%v,valid=true)", test.expiration, dte, eligible, valid, test.wantDTE, test.eligible)
		}
	}
	if _, _, valid := cachedBuyerExpirationEligibility("2026-10-1", now, 5); valid {
		t.Fatal("non-ISO expiration should be invalid")
	}
	wantHorizon := time.Date(2026, time.October, 16, 16, 0, 0, 0, location)
	if got := cachedBuyerHorizonEnd(now, 5); !got.Equal(wantHorizon) {
		t.Fatalf("horizon end=%s want %s", got, wantHorizon)
	}
	// The horizon boundary is strictly excluded, even on the same calendar date;
	// an expiration one calendar day later is eligible.
	boundaryDTE, boundaryEligible, boundaryValid := cachedBuyerExpirationEligibility("2026-10-16", now.Add(time.Hour), 5)
	if boundaryDTE != 7 || boundaryEligible || !boundaryValid {
		t.Fatalf("same-day expiry after horizon clock should be excluded: dte=%d eligible=%v", boundaryDTE, boundaryEligible)
	}
}

func TestCachedBuyerStructuresExcludesHorizonExpiryButAllowsFartherFallback(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(location)
	strike, bid, ask := 100.0, 1.0, 1.1
	soon := now.AddDate(0, 0, 2).Format("2006-01-02")
	short := cachedBuyerStructures([]model.OptionRow{{Expiration: soon, ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask}}, 100, map[string]string{"action": "hold"}, nil, nil)
	if short["available"] != false || short["reason"] != "可用到期日都落在未来 5 个交易日窗口内，暂不生成买方结构" {
		t.Fatalf("near-horizon contract should be excluded with explicit reason: %#v", short)
	}

	// Python falls back to non-sweet expirations when no 10–45 DTE options
	// qualify; it does not impose an arbitrary 90-day maximum.
	far := now.AddDate(0, 0, 100).Format("2006-01-02")
	long := cachedBuyerStructures([]model.OptionRow{{Expiration: far, ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask}}, 100, map[string]string{"action": "hold"}, nil, nil)
	items, ok := long["items"].([]map[string]any)
	if long["available"] != true || !ok || len(items) != 1 || items[0]["expiration"] != far {
		t.Fatalf("far expiry should remain eligible when no sweet expiry exists: %#v", long)
	}
}

func TestCachedBuyerStructuresKeepsFarOutShortLegForValidVertical(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 100).Format("2006-01-02")
	longStrike, shortStrike := 108.0, 123.0
	longBid, longAsk, shortBid, shortAsk := 5.0, 5.2, 0.96, 1.04
	rows := []model.OptionRow{
		{Expiration: expiration, ContractType: "call", Strike: &longStrike, Bid: &longBid, Ask: &longAsk},
		{Expiration: expiration, ContractType: "call", Strike: &shortStrike, Bid: &shortBid, Ask: &shortAsk},
	}
	result := cachedBuyerStructures(rows, 100, map[string]string{"action": "hold"}, nil, nil)
	items, ok := result["items"].([]map[string]any)
	if !ok {
		t.Fatalf("items have unexpected type: %#v", result["items"])
	}
	for _, item := range items {
		strikes, _ := item["strikes"].([]float64)
		if item["kind"] == "vertical" && len(strikes) == 2 && strikes[0] == longStrike && strikes[1] == shortStrike {
			return
		}
	}
	t.Fatalf("expected Python-compatible call vertical with a short strike outside 15%% moneyness filter, got %#v", items)
}

func TestCachedBuyerBelowIntrinsicMatchesPythonSlack(t *testing.T) {
	if !cachedBuyerBelowIntrinsic(.95, 100, 99, "call") {
		t.Fatal("call quote below 98% intrinsic should be rejected")
	}
	if cachedBuyerBelowIntrinsic(.99, 100, 99, "call") {
		t.Fatal("call quote at 99% intrinsic should be retained")
	}
	if !cachedBuyerBelowIntrinsic(.95, 100, 101, "put") {
		t.Fatal("put quote below 98% intrinsic should be rejected")
	}
	if cachedBuyerBelowIntrinsic(1, 100, 101, "put") {
		t.Fatal("put quote at intrinsic should be retained")
	}
}

func TestCachedBuyerQuoteDedupKeepsNarrowestSpread(t *testing.T) {
	strike := 100.0
	rows := []model.OptionRow{
		{Expiration: "2030-01-18", ContractType: "call", Strike: &strike, Bid: float64Pointer(1), Ask: float64Pointer(1.4)},
		{Expiration: "2030-01-18", ContractType: "call", Strike: &strike, Bid: float64Pointer(1.1), Ask: float64Pointer(1.2)},
		{Expiration: "2030-01-18", ContractType: "call", Strike: &strike, Bid: float64Pointer(1.05), Ask: float64Pointer(1.25)},
		{Expiration: "2030-01-18", ContractType: "put", Strike: &strike, Bid: float64Pointer(1), Ask: float64Pointer(1.2)},
	}
	got := dedupeCachedBuyerQuotes(rows)
	if len(got) != 2 || got[0].Bid == nil || *got[0].Bid != 1.1 || got[1].ContractType != "put" {
		t.Fatalf("dedupe should keep narrowest call spread separately from put: %#v", got)
	}
}

func TestCachedOptionLevelTiesAreDeterministicAndMatchPythonOrder(t *testing.T) {
	strikes := []float64{103, 105, 109}
	rows := make([]model.OptionRow, 0, len(strikes))
	for _, strike := range strikes {
		volume := int64(100)
		rows = append(rows, model.OptionRow{ContractType: "call", Strike: &strike, Volume: &volume})
	}
	for iteration := 0; iteration < 20; iteration++ {
		levels, metric := cachedOptionLevels(rows, 100)
		if metric != "volume" || len(levels) != len(strikes) {
			t.Fatalf("iteration %d metric=%q levels=%#v", iteration, metric, levels)
		}
		for index, want := range []float64{109, 105, 103} {
			if levels[index].price != want {
				t.Fatalf("iteration %d level[%d]=%v want %v (ties must follow Python reverse tuple ordering)", iteration, index, levels[index].price, want)
			}
		}
	}
}

func TestCachedOptionLevelsIgnoreRawProviderGammaWithoutModelGamma(t *testing.T) {
	spot, strike := 100.0, 105.0
	oi, providerGamma := int64(1000), 0.02
	rows := []model.OptionRow{{
		ContractType: "call", Strike: &strike, OpenInterest: &oi,
		Gamma: &providerGamma, Expiration: "invalid-expiration",
	}}
	gamma.Annotate(rows, &spot)
	if rows[0].ModelGamma != nil {
		t.Fatalf("invalid expiration unexpectedly produced model gamma: %#v", rows[0].ModelGamma)
	}
	levels, metric := cachedOptionLevels(rows, spot)
	if metric != "gex" {
		t.Fatalf("metric=%q want gex", metric)
	}
	if len(levels) != 0 {
		t.Fatalf("provider gamma without model gamma must not create a GEX candidate: %#v", levels)
	}
}

func TestCachedOptionLevelsKeepPythonMinimumTimeForExpiredValidContract(t *testing.T) {
	spot, strike, iv := 100.0, 105.0, 0.3
	oi := int64(1000)
	expiration := "2000-01-01"
	rows := []model.OptionRow{{
		ContractType: "call", Strike: &strike, OpenInterest: &oi,
		ImpliedVolatility: &iv, Expiration: expiration,
	}}
	gamma.Annotate(rows, &spot)
	levels, metric := cachedOptionLevels(rows, spot)
	if metric != "gex" {
		t.Fatalf("metric=%q, want gex from available open interest", metric)
	}
	if len(levels) != 1 || rows[0].ModelGamma == nil {
		t.Fatalf("parseable expired contract should retain Python's minimum-time GEX: levels=%#v row=%#v", levels, rows[0])
	}

	modelIV := .3
	rows[0].ModelIV = &modelIV
	_, years := cachedExpiryIVAt(rows, expiration, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	wantYears := 30.0 / (365 * 24 * 60)
	if math.Abs(years-wantYears) > 1e-12 {
		t.Fatalf("expired contract years=%v, want Python minimum %v", years, wantYears)
	}
}

func TestCachedExpiryIVMatchesPythonAcrossMarketSessions(t *testing.T) {
	modelIV := 0.3
	cases := []struct {
		name       string
		expiration string
		now        time.Time
		wantYears  float64
	}{
		// Times are UTC instants corresponding to New York market-session states.
		{"premarket", "2026-10-06", time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC), 0.0009132420091324201},
		{"after-hours", "2026-10-06", time.Date(2026, time.October, 6, 22, 0, 0, 0, time.UTC), 5.7077625570776254e-05},
		{"overnight", "2026-10-06", time.Date(2026, time.October, 7, 2, 0, 0, 0, time.UTC), 5.7077625570776254e-05},
		{"closed-sunday", "2026-09-28", time.Date(2026, time.September, 27, 18, 0, 0, 0, time.UTC), 0.0029680365296803654},
		// The interval crosses the spring DST transition: 53 elapsed hours, not 54.
		{"dst-weekend", "2026-03-09", time.Date(2026, time.March, 7, 15, 0, 0, 0, time.UTC), 0.006050228310502283},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rows := []model.OptionRow{{Expiration: test.expiration, ModelIV: &modelIV}}
			gotIV, gotYears := cachedExpiryIVAt(rows, test.expiration, test.now)
			if gotIV != modelIV {
				t.Errorf("IV=%v, want %v", gotIV, modelIV)
			}
			if math.Abs(gotYears-test.wantYears) > 1e-12 {
				t.Errorf("years to expiry=%.16g, want Python golden %.16g", gotYears, test.wantYears)
			}
		})
	}
}

func TestCachedOptionGEXUsesModeledGammaWhenProviderGammaMissing(t *testing.T) {
	spot, strike, mid, bid, ask := 100.0, 100.0, 5.0, 4.9, 5.1
	oi := int64(1000)
	rows := []model.OptionRow{{ContractType: "call", Strike: &strike, LastPrice: &mid, Bid: &bid, Ask: &ask, OpenInterest: &oi, Expiration: "2099-01-16"}}
	gamma.Annotate(rows, &spot)
	_, metric := cachedOptionLevels(rows, spot)
	if metric != "gex" {
		t.Fatalf("option metric = %q, want gex", metric)
	}
	if rows[0].ModelGamma == nil || *rows[0].ModelGamma <= 0 {
		t.Fatalf("annotation did not supply model gamma: %#v", rows[0])
	}
}

func TestCachedMergePropagatesStrongLevelScoreBeforeRanking(t *testing.T) {
	values := []cachedLevel{{price: 101, score: .1, factor: "看涨墙", factors: []string{"看涨墙"}}, {price: 101.5, score: .1, factor: "筹码密集", factors: []string{"筹码密集"}}}
	levels := mergeCachedLevelsWeighted(values, 100, true, 0.1, .1, 30, 1)
	if len(levels) != 2 {
		t.Fatalf("levels=%#v", levels)
	}
	var strong, near map[string]any
	for _, item := range levels {
		if item["price"] == 101.0 {
			strong = item
		}
		if item["price"] == 101.5 {
			near = item
		}
	}
	if strong == nil || strong["score"].(float64) < .70 {
		t.Fatalf("wall candidate should remain strong: %#v", strong)
	}
	if near == nil || near["score"].(float64) < strong["score"].(float64)*.8-1e-9 || near["model_score"].(float64) >= .70 {
		t.Fatalf("nearby score propagation should affect priority but preserve base model score: %#v", near)
	}
}

func TestCachedMergeKeepsUnroundedScoresThroughClusterPropagation(t *testing.T) {
	levels := mergeCachedLevelsWeighted([]cachedLevel{
		{price: 101, score: 1, factors: []string{"看涨持仓", "筹码密集"}},
		{price: 101.5, score: .9, factors: []string{"看涨持仓"}},
	}, 100, true, .1, .1, 10, 1.04)
	var near map[string]any
	for _, level := range levels {
		if level["price"] == 101.5 {
			near = level
			break
		}
	}
	if near == nil {
		t.Fatalf("nearby level missing from result: %#v", levels)
	}
	if near["model_score"] != .55 || near["score"] != .56 {
		t.Errorf("nearby score/model_score=%v/%v, want Python-rounded propagated score 0.56 and base model score 0.55", near["score"], near["model_score"])
	}
}

func TestCachedMergeScoresIndependentEvidenceAndWall(t *testing.T) {
	values := []cachedLevel{
		{price: 105, score: .75, factor: "看涨墙", factors: []string{"看涨墙"}},
		{price: 105.4, score: .55, factor: "斐波那契 50%", factors: []string{"斐波那契 50%"}},
	}
	levels := mergeCachedLevelsWeighted(values, 100, true, 1, 1, 30, 1)
	if len(levels) != 1 {
		t.Fatalf("expected one merged level, got %#v", levels)
	}
	level := levels[0]
	factors, ok := level["factors"].([]string)
	if !ok || len(factors) != 2 {
		t.Fatalf("merged factors=%#v", level["factors"])
	}
	if level["strength_tier"] != "strong" || level["score"].(float64) < .8 {
		t.Fatalf("independent wall/Fibonacci evidence did not strengthen level: %#v", level)
	}
}

func TestCachedChipPeakTiesMatchPythonReverseTupleOrder(t *testing.T) {
	peaks := []cachedPeakItem{{value: 10, index: 3}, {value: 10, index: 8}, {value: 12, index: 1}, {value: 10, index: 5}}
	sortCachedPeakItems(peaks)
	want := []cachedPeakItem{{value: 12, index: 1}, {value: 10, index: 8}, {value: 10, index: 5}, {value: 10, index: 3}}
	for index := range want {
		if peaks[index] != want[index] {
			t.Fatalf("peak order=%#v want Python reverse tuple order %#v", peaks, want)
		}
	}
}

func TestCachedTrendMatchesPythonChannelGolden(t *testing.T) {
	bars := make([]map[string]any, 60)
	for index := range bars {
		close := 100 + float64(index)*.123456 + float64(index%5)*.017
		bars[index] = map[string]any{"close": close}
	}
	trend := cachedTrend(bars)
	if trend == nil {
		t.Fatal("expected trend for 60 valid closes")
	}
	if trend["direction"] != "up" || trend["label"] != "上行通道 · 上涨趋势" || trend["background_direction"] != "up" || trend["reversal_confirmed"] != false || trend["bars"] != 60 {
		t.Fatalf("trend metadata differs from Python golden: %#v", trend)
	}
	for field, want := range map[string]float64{"slope_percent": .119, "upper": 107.3572, "lower": 107.2853} {
		got, ok := trend[field].(float64)
		if !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s=%v, want Python golden %v", field, trend[field], want)
		}
	}
	rsi, ok := trend["rsi"].(map[string]any)
	if !ok || rsi["value"] != 100.0 || rsi["state"] != "overbought" {
		t.Errorf("RSI differs from Python golden: %#v", trend["rsi"])
	}
}

func TestCachedMedianTrueRangeMatchesPythonEvenAndOddSamples(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{name: "even uses upper middle", values: []float64{4, 1, 3, 2}, want: 3},
		{name: "odd uses middle", values: []float64{5, 1, 3}, want: 3},
		{name: "empty sample", values: nil, want: 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := cachedMedianTrueRange(test.values); got != test.want {
				t.Fatalf("cachedMedianTrueRange(%v)=%v want Python-compatible %v", test.values, got, test.want)
			}
		})
	}
}

func TestCachedATRSeriesMatchesPythonRollingWindowGolden(t *testing.T) {
	bars := []map[string]any{
		{"high": 10.21, "low": 9.91, "close": 10.02},
		{"high": 10.47, "low": 10.02, "close": 10.39},
		{"high": 10.44, "low": 10.11, "close": 10.17},
		{"high": 10.95, "low": 10.13, "close": 10.9},
		{"high": 10.91, "low": 10.65, "close": 10.71},
		{"high": 11.37, "low": 10.70, "close": 11.33},
		{"high": 11.41, "low": 10.96, "close": 11.02},
	}
	want := []float64{.3000000000000007, .3750000000000009, .3600000000000006, .5333333333333332, .4699999999999995, .5833333333333327, .4599999999999997}
	got := cachedATRSeries(bars, 3)
	for index := range want {
		if math.Abs(got[index]-want[index]) > 1e-14 {
			t.Fatalf("ATR[%d]=%.17g, want Python rolling-window golden %.17g", index, got[index], want[index])
		}
	}
}

func TestCachedATRSeriesMatchesPrefixATR(t *testing.T) {
	bars := make([]map[string]any, 70)
	for i := range bars {
		close := float64(100 + i%9)
		bars[i] = map[string]any{"high": close + 2, "low": close - 1, "close": close}
	}
	series := cachedATRSeries(bars, 14)
	if len(series) != len(bars) {
		t.Fatalf("ATR series length=%d want=%d", len(series), len(bars))
	}
	for i := range bars {
		want := cachedATR(bars[:i+1], 14)
		if math.Abs(series[i]-want) > 1e-9 {
			t.Fatalf("ATR[%d]=%f want %f", i, series[i], want)
		}
	}
}

func TestCachedVisibleSelectionKeepsFarStrongLevel(t *testing.T) {
	values := make([]map[string]any, 7)
	for index := range values {
		values[index] = map[string]any{"price": float64(99 - index), "score": .4, "strength_tier": "normal"}
	}
	values[6] = map[string]any{"price": 70.0, "score": .95, "strength_tier": "strong"}
	visible := selectVisibleCachedLevels(values, 100, 6)
	if len(visible) != 6 {
		t.Fatalf("visible count=%d want 6", len(visible))
	}
	found := false
	for _, level := range visible {
		if level["price"] == 70.0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("far strong level was dropped: %#v", visible)
	}
}

func TestCachedVisibleSelectionInfersMissingStrengthTierLikePython(t *testing.T) {
	values := make([]map[string]any, 7)
	for index := range values {
		values[index] = map[string]any{"price": float64(99 - index), "score": .4, "strength_tier": "normal"}
	}
	values[6] = map[string]any{"price": 70.0, "score": .95}
	visible := selectVisibleCachedLevels(values, 100, 6)
	for _, level := range visible {
		if level["price"] == 70.0 {
			return
		}
	}
	t.Fatalf("missing strength_tier should be inferred from score like Python: %#v", visible)
}

func TestCachedVisibleSelectionReturnsEmptyForNonPositiveLimit(t *testing.T) {
	values := []map[string]any{{"price": 99.0}, {"price": 101.0}}
	for _, limit := range []int{0, -1} {
		if got := selectVisibleCachedLevels(values, 100, limit); len(got) != 0 {
			t.Errorf("limit %d returned %d levels, want empty like Python", limit, len(got))
		}
	}
}

func TestCachedRecommendationMatchesPythonChannelAndRangeRules(t *testing.T) {
	up := map[string]any{"direction": "up", "lower": 95.0, "upper": 105.0}
	result := cachedRecommendation(up, nil, []map[string]any{{"price": 104.0}}, 103)
	if result["action"] != "sell" || result["reason"] != "上行通道上沿附近，现价接近压力位" {
		t.Fatalf("upper-channel recommendation=%#v", result)
	}
	rangeTrend := map[string]any{"direction": "range", "lower": 95.0, "upper": 105.0}
	result = cachedRecommendation(rangeTrend, []map[string]any{{"price": 99.0}}, nil, 100)
	if result["action"] != "buy" || result["reason"] != "震荡区间下沿，现价接近支撑位" {
		t.Fatalf("range support recommendation=%#v", result)
	}
	result = cachedRecommendation(rangeTrend, []map[string]any{{"price": 99.0}}, []map[string]any{{"price": 100.5}}, 100)
	if result["action"] != "sell" {
		t.Fatalf("closer range resistance should sell: %#v", result)
	}
	result = cachedRecommendation(up, []map[string]any{{"price": 102.0}}, nil, 103)
	if result["action"] != "hold" {
		t.Fatalf("up-channel upper position should not buy: %#v", result)
	}
}

func TestCachedStopLossUsesNearestHighestAnchorAndATRBuffer(t *testing.T) {
	trend := map[string]any{"lower": 97.0}
	buy := map[string]any{"zone_low": 95.0}
	support := []map[string]any{{"zone_low": 99.0, "price": 99.5}}
	result := cachedStopLoss(trend, buy, support, 100, 4)
	if result == nil || result["anchor_price"] != 99.0 || result["price"] != 98.0 || result["buffer"] != 1.0 || result["source"] != "最近支撑位" {
		t.Fatalf("ATR stop loss=%#v", result)
	}
	result = cachedStopLoss(trend, buy, nil, 100, 0)
	if result == nil || result["anchor_price"] != 97.0 || result["buffer"] != .5 {
		t.Fatalf("fallback stop loss=%#v", result)
	}
}

func TestAbsorptionDistanceMatchesPythonATRAndFallbackRules(t *testing.T) {
	cases := []struct {
		name                           string
		price, atr, atrRatio, fallback float64
		want                           float64
	}{
		{name: "missing ATR uses configured price fallback", price: 100, atr: 0, atrRatio: .25, fallback: .002, want: .2},
		{name: "valid ATR obeys minimum zone floor", price: 100, atr: .2, atrRatio: .5, fallback: .01, want: .25},
		{name: "valid ATR ratio dominates floor", price: 100, atr: 2, atrRatio: .5, fallback: .01, want: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := absorptionDistance(test.price, test.atr, test.atrRatio, test.fallback); math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("absorptionDistance()=%v want Python-compatible %v", got, test.want)
			}
		})
	}
}

func TestCachedAbsorptionRejectsIncompleteOHLCWindowLikePython(t *testing.T) {
	bars := make([]map[string]any, 20)
	for i := range bars {
		bars[i] = map[string]any{"high": 100.1, "low": 99.9, "close": 100.0, "volume": 100.0}
	}
	bars[9] = map[string]any{"high": 100.2, "low": 99.0, "close": 99.0, "volume": 100.0}
	for index := 10; index <= 14; index++ {
		bars[index] = map[string]any{"high": 99.7, "low": 99.4, "close": 99.495, "volume": 100.0}
	}
	if got := cachedAbsorptionLevels(bars, 100); len(got) == 0 {
		t.Fatal("complete OHLC fixture should yield its confirmed absorption candidate")
	}
	delete(bars[0], "high")
	if got := cachedAbsorptionLevels(bars, 100); len(got) != 0 {
		t.Fatalf("Python rejects an absorption window with missing OHLC; Go returned %#v", got)
	}
}

func TestCachedAbsorptionReboundUsesATRZoneFloor(t *testing.T) {
	bars := make([]map[string]any, 20)
	for i := range bars {
		close := 100.0
		bars[i] = map[string]any{"high": 100.1, "low": 99.9, "close": close, "volume": 100.0}
	}
	bars[9] = map[string]any{"high": 100.2, "low": 99.0, "close": 99.0, "volume": 100.0}
	for index := 10; index <= 14; index++ {
		bars[index] = map[string]any{"high": 99.7, "low": 99.4, "close": 99.495, "volume": 100.0}
	}
	levels := cachedAbsorptionLevels(bars, 100)
	found := false
	for _, level := range levels {
		if math.Abs(level.price-99.0) < 1e-9 {
			found = true
		}
	}
	if !found {
		t.Fatalf("0.5%% rebound above ATR floor did not qualify: %#v", levels)
	}
}

func TestCachedBestTradePointUsesFiveDayTouchAndPythonHistoryFallbacks(t *testing.T) {
	volatility := .8
	levels := []map[string]any{
		{"price": 90.0, "zone_low": 89.0, "zone_high": 91.0, "score": .6, "model_score": .6, "factors": []string{"筹码密集"}, "probability": .9, "history_samples": 20, "history_hold_rate": .9, "history_break_rate": .1, "history_adjusted_hold_rate": nil, "history_adjusted_break_rate": nil, "history_confidence": nil},
	}
	point := cachedBestTradePoint(map[string]any{"direction": "range"}, levels, 100, volatility, 1.0, true)
	if point == nil {
		t.Fatal("expected support trade point")
	}
	wantTouch := cachedTouchProbability(90, 100, volatility, 5.0/252.0).(float64)
	wantModel := .6*.4 + (1.0/3)*.2 + wantTouch*.15 + .5*.1 + .78*.15
	if math.Abs(point["model_confidence"].(float64)-wantModel) > 0.0001 {
		t.Fatalf("model confidence=%v want %.4f (five-day touch %v)", point["model_confidence"], wantModel, wantTouch)
	}
	if point["history_weight"] != 0.0 || point["history_hold_rate"] != .9 || point["history_break_rate"] != .1 || point["history_sample_confidence"] != 0.0 {
		t.Fatalf("history fields should fall back to raw rates without inferring confidence: %#v", point)
	}

	// If explicit history confidence is absent but adjusted hold is present,
	// the Python formula uses confidence=0 and therefore does not blend history.
	levels[0]["history_adjusted_hold_rate"] = .95
	levels[0]["history_adjusted_break_rate"] = .05
	point = cachedBestTradePoint(nil, levels, 100, 0, 3.0, true)
	if point["history_weight"] != 0.0 || point["history_sample_confidence"] != 0.0 {
		t.Fatalf("missing history confidence should not be derived from counts: %#v", point)
	}
}

func TestCachedBestTradePointNormalizesPythonOutputFields(t *testing.T) {
	price := 95.123456
	levels := []map[string]any{{
		"price": price, "zone_low": 0.0, "zone_high": 0.0,
		"model_score": .8, "factors": nil, "history_samples": 4.9,
		"history_confidence": 1.234567,
	}}
	point := cachedBestTradePoint(nil, levels, 100, 0, 0, true)
	if point == nil {
		t.Fatal("expected support trade point")
	}
	if point["zone_low"] != 95.1235 || point["zone_high"] != 95.1235 {
		t.Errorf("missing zone bounds=%v..%v, want price fallback rounded to 4 places", point["zone_low"], point["zone_high"])
	}
	if point["history_samples"] != 4 {
		t.Errorf("history_samples=%v, want Python int conversion 4", point["history_samples"])
	}
	if point["history_sample_confidence"] != 1.0 {
		t.Errorf("history_sample_confidence=%v, want clamped and rounded 1.0", point["history_sample_confidence"])
	}
	if factors, ok := point["factors"].([]string); !ok || factors == nil || len(factors) != 0 {
		t.Errorf("missing factors=%#v, want empty array", point["factors"])
	}

	levels[0]["zone_low"], levels[0]["zone_high"] = 91.22226, 89.11114
	levels[0]["history_confidence"] = .123456
	point = cachedBestTradePoint(nil, levels, 100, 0, 0, true)
	if point["zone_low"] != 89.1111 || point["zone_high"] != 91.2223 {
		t.Errorf("reversed zone bounds=%v..%v, want normalized rounded 89.1111..91.2223", point["zone_low"], point["zone_high"])
	}
	if point["history_sample_confidence"] != .1235 {
		t.Errorf("history_sample_confidence=%v, want rounded 0.1235", point["history_sample_confidence"])
	}
}

func TestCachedSupportPlanBuysNearestThenRanksDeeperAdds(t *testing.T) {
	values := []map[string]any{
		{"price": 98.0, "score": .3},
		{"price": 90.0, "score": .99},
		{"price": 95.0, "score": .4},
		{"price": 85.0, "score": .8},
	}
	buy, add := splitCachedSupport(values, 100)
	if len(buy) != 2 || buy[0]["price"] != 98.0 || buy[1]["price"] != 95.0 {
		t.Fatalf("buy plan should be nearest half: %#v", buy)
	}
	if len(add) != 2 || add[0]["price"] != 90.0 || add[1]["price"] != 85.0 {
		t.Fatalf("add plan should rank deeper candidates by score: %#v", add)
	}
}

func TestCachedBuyerShortFallbackMatchesPythonNearestBeforeWidthCheck(t *testing.T) {
	longStrike, nearTargetStrike, bestTargetStrike := 108.0, 125.0, 145.0
	long := &cachedBuyerContract{row: model.OptionRow{Strike: &longStrike}}
	group := &cachedBuyerGroup{rows: []*cachedBuyerContract{
		{row: model.OptionRow{ContractType: "call", Strike: &nearTargetStrike}},
		{row: model.OptionRow{ContractType: "call", Strike: &bestTargetStrike}},
	}}
	if got := cachedBuyerShortRows(group, long, "call", 140, 100); len(got) != 0 {
		t.Fatalf("Python picks the closest target strike first, then rejects it for width; got fallback %#v", got)
	}
}

func TestCachedBuyerStructuresRejectsNonfiniteQuotesLikePython(t *testing.T) {
	spot, strike := 100.0, 100.0
	quote := math.NaN()
	expiration := time.Now().AddDate(0, 0, 100).Format("2006-01-02")
	result := cachedBuyerStructuresWithIV([]model.OptionRow{{
		Expiration: expiration, ContractType: "call", Strike: &strike,
		Bid: &quote, Ask: &quote,
	}}, spot, map[string]string{"action": "hold"}, nil, nil, map[string]any{})
	if result["available"] != false || result["reason"] != "当前缓存缺少有效买卖报价，暂不生成买方结构" {
		t.Fatalf("nonfinite bid/ask should be rejected by Python-style numeric parsing: %#v", result)
	}
}

func TestCachedBuyerStructuresDoesNotMutateSharedRows(t *testing.T) {
	spot, strike, bid, ask := 100.0, 100.0, 5.0, 5.1
	expiration := time.Now().AddDate(0, 0, 100).Format("2006-01-02")
	rows := []model.OptionRow{{
		Expiration: expiration, ContractType: "CALL", Strike: &strike,
		Bid: &bid, Ask: &ask,
	}}
	cachedBuyerStructuresWithIV(rows, spot, map[string]string{"action": "hold"}, nil, nil, map[string]any{})
	if rows[0].ContractType != "CALL" || rows[0].ModelIV != nil || rows[0].ModelGamma != nil {
		t.Fatalf("buyer analysis mutated shared option rows: %#v", rows[0])
	}
}

func TestCachedBuyerStructuresDoesNotUseRawProviderIVWithoutExpiryModel(t *testing.T) {
	spot, strike, bid, ask, providerIV := 100.0, 100.0, .0097, .0103, .3
	expiration := time.Now().AddDate(0, 0, 100).Format("2006-01-02")
	result := cachedBuyerStructuresWithIV([]model.OptionRow{{
		Expiration: expiration, ContractType: "call", Strike: &strike,
		Bid: &bid, Ask: &ask, ImpliedVolatility: &providerIV,
	}}, spot, map[string]string{"action": "hold"}, nil, nil, map[string]any{})
	if result["available"] != false || result["reason"] != "当前期权链没有满足报价和流动性条件的候选结构" {
		t.Fatalf("Python drops a contract when both modeled IV fallbacks are missing: %#v", result)
	}
}
