package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
	"option-scope-go/internal/valuation"
)

func TestApplyValuationDiagnosticsPreservesResultWarningsAndErrors(t *testing.T) {
	quote := model.Quote{}
	result := valuation.Result{
		Status:   valuation.StatusUnavailable,
		Warning:  "no usable valuation candidate: insufficient positive owner earnings, forward EPS, or free cash flow",
		Warnings: []string{"required return defaulted to 10.0%", "no usable valuation candidate"},
		Errors:   []string{"fundamentals unavailable"},
	}

	applyValuationDiagnostics(&quote, result)
	if quote.FairValueStatus != valuation.StatusUnavailable || quote.FairValuePending {
		t.Fatalf("status/pending = %q/%v, want unavailable/false", quote.FairValueStatus, quote.FairValuePending)
	}
	if quote.FairValueWarning != result.Warning {
		t.Fatalf("warning = %q, want %q", quote.FairValueWarning, result.Warning)
	}
	if len(quote.FairValueWarnings) != len(result.Warnings) || len(quote.FairValueErrors) != len(result.Errors) {
		t.Fatalf("diagnostics = %#v/%#v, want %#v/%#v", quote.FairValueWarnings, quote.FairValueErrors, result.Warnings, result.Errors)
	}
	// The quote owns its diagnostic slices; later result reuse must not mutate it.
	result.Warnings[0] = "mutated"
	result.Errors[0] = "mutated"
	if quote.FairValueWarnings[0] == "mutated" || quote.FairValueErrors[0] == "mutated" {
		t.Fatalf("diagnostic slices alias valuation result: %#v/%#v", quote.FairValueWarnings, quote.FairValueErrors)
	}

	payload, err := json.Marshal(quote)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fair_value_status", "fair_value_warning", "fair_value_warnings", "fair_value_errors"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("JSON omitted diagnostic field %q: %s", key, payload)
		}
	}
}

func TestApplyValuationDiagnosticsDefaultsMissingStatusToReady(t *testing.T) {
	quote := model.Quote{FairValuePending: true}
	applyValuationDiagnostics(&quote, valuation.Result{Warning: "used fallback"})
	if quote.FairValueStatus != valuation.StatusReady || quote.FairValuePending {
		t.Fatalf("status/pending = %q/%v, want ready/false", quote.FairValueStatus, quote.FairValuePending)
	}
}

func TestAPIValuationStatusMapsInvalidToFailed(t *testing.T) {
	result := valuation.Result{Status: valuation.StatusInvalid}
	if got := apiValuationStatus(result.Status); got != "failed" {
		t.Fatalf("API status=%q, want failed", got)
	}

	quote := model.Quote{}
	applyValuationDiagnostics(&quote, result)
	if quote.FairValueStatus != "failed" || quote.FairValuePending {
		t.Fatalf("direct quote status/pending=%q/%v, want failed/false", quote.FairValueStatus, quote.FairValuePending)
	}
	if got := valuationCachePayload(result, valuation.Fundamentals{})["status"]; got != "failed" {
		t.Fatalf("cached result status=%v, want failed", got)
	}

	quote = model.Quote{}
	applyValuationPayloadDiagnostics(&quote, map[string]any{"status": valuation.StatusInvalid})
	if quote.FairValueStatus != "failed" || quote.FairValuePending {
		t.Fatalf("legacy cache status/pending=%q/%v, want failed/false", quote.FairValueStatus, quote.FairValuePending)
	}
}

func TestValuationRetryableErrorMatchesPythonTransientFailures(t *testing.T) {
	for _, err := range []error{
		errors.New("Yahoo returned HTTP 408"),
		errors.New("Yahoo returned HTTP 425"),
		errors.New("Yahoo returned HTTP 500"),
		errors.New("Yahoo returned HTTP 502"),
		errors.New("Yahoo returned HTTP 503"),
		errors.New("Yahoo returned HTTP 504"),
		context.DeadlineExceeded,
		errors.New("connection aborted"),
	} {
		if !valuationRetryableError(err) {
			t.Errorf("transient error %q was not retryable", err)
		}
	}
	if valuationRetryableError(errors.New("Yahoo returned HTTP 404")) {
		t.Fatal("permanent HTTP 404 was classified as retryable")
	}
}

func TestAPIValuationConfidenceMatchesPythonLabelsAndThresholds(t *testing.T) {
	if got := confidenceScorePercent(.8425); got != 84.2 {
		t.Fatalf("confidence score decimal rounding=%v, want Python round result 84.2", got)
	}
	for _, test := range []struct {
		score float64
		want  string
	}{{.44, "低"}, {.45, "中"}, {.75, "高"}} {
		gotScore, gotLabel := apiValuationConfidence(valuation.Fundamentals{}, valuation.Result{
			FairValue: floatPointer(100), Status: valuation.StatusReady, Confidence: test.score,
		})
		if gotScore != test.score*100 || gotLabel != test.want {
			t.Errorf("confidence %.2f => %.1f/%q, want %.1f/%q", test.score, gotScore, gotLabel, test.score*100, test.want)
		}
	}

	inputs := valuation.Fundamentals{
		AnnualRevenue:       []float64{100},
		MarketCap:           floatPointer(10e9),
		LongBusinessSummary: "semiconductor",
	}
	result := valuation.Result{
		FairValue: floatPointer(100), Status: valuation.StatusReady, Method: "generic",
		DataQualityScore: 100, Regime: valuation.RegimeNormal,
		Defensive:  map[string]any{"value": 100.0, "low": 0.0},
		Optimistic: map[string]any{"high": 289.1666666667},
	}
	gotScore, gotLabel := apiValuationConfidence(inputs, result)
	if math.Abs(gotScore-65.3) > .001 || gotLabel != "中" {
		t.Fatalf("semiconductor text alone reduced confidence: %.1f/%q, want 65.3/中", gotScore, gotLabel)
	}
	result.Warnings = []string{"缺乏分析师远期 EPS，已自动降级为保守模型"}
	gotScore, gotLabel = apiValuationConfidence(inputs, result)
	if math.Abs(gotScore-45.3) > .001 || gotLabel != "中" {
		t.Fatalf("missing-forward-EPS diagnostic was not applied: %.1f/%q, want 45.3/中", gotScore, gotLabel)
	}
	result.Warnings = nil
	inputs.Beta = floatPointer(4)
	gotScore, _ = apiValuationConfidence(inputs, result)
	if math.Abs(gotScore-65.3) > .001 {
		t.Fatalf("out-of-range beta should normalize to 1 for confidence: %.1f, want 65.3", gotScore)
	}
	inputs.Beta = floatPointer(2)
	gotScore, _ = apiValuationConfidence(inputs, result)
	if math.Abs(gotScore-53.3) > .001 {
		t.Fatalf("valid high beta confidence penalty=%.1f, want 53.3", gotScore)
	}
}

func TestValuationRequiredReturnMatchesPythonCAPMAndStableClamp(t *testing.T) {
	for _, test := range []struct {
		name    string
		rate    float64
		beta    *float64
		company provider.YahooFundamentals
		want    float64
	}{
		{name: "decimal units", rate: .04, beta: floatPointer(2), want: .15},
		{name: "percentage units", rate: 4, beta: floatPointer(2), want: .15},
		{name: "minimum clamp", rate: .01, beta: floatPointer(.2), want: .085},
		{name: "maximum clamp", rate: .12, beta: floatPointer(3), want: .18},
		{name: "invalid beta fallback", rate: .04, beta: floatPointer(4), want: .095},
		{name: "stable company cap", rate: .04, beta: floatPointer(3), company: provider.YahooFundamentals{Sector: "Consumer Defensive"}, want: .09},
		{name: "stable company floor", rate: .02, beta: floatPointer(.2), company: provider.YahooFundamentals{Industry: "Beverages"}, want: .085},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.company.Beta = test.beta
			if got := valuationRequiredReturnWithRate(test.company, test.rate); math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("required return=%v, want %v", got, test.want)
			}
		})
	}
	for _, test := range []struct{ input, want float64 }{{.04, .04}, {4, .04}, {.01, .02}, {.12, .08}} {
		if got := normalizeRiskFreeRate(test.input); got != test.want {
			t.Errorf("normalizeRiskFreeRate(%v)=%v, want %v", test.input, got, test.want)
		}
	}
}

func TestValuationInputsExposeCAPMFallbackAndCacheRates(t *testing.T) {
	inputs := valuationInputs("MCD", nil, provider.YahooFundamentals{
		Sector: "Consumer Defensive", Industry: "Restaurants", Beta: floatPointer(2),
	})
	if inputs.RiskFreeRate == nil || *inputs.RiskFreeRate != .04 || inputs.RequiredReturn == nil || *inputs.RequiredReturn != .09 {
		t.Fatalf("stable-company CAPM inputs=%#v, want 4%% fallback and 9%% return", inputs)
	}
	result := valuation.Calculate(inputs)
	payload := valuationCachePayload(result, inputs)
	if payload["risk_free_rate"] != .04 || payload["required_return"] != .09 {
		t.Fatalf("cached CAPM rates=%#v, want 0.04 and 0.09", payload)
	}
}

func TestValuationInputsUseYahooRiskFreeRateAndNormalizePercentUnits(t *testing.T) {
	inputs := valuationInputs("TEST", nil, provider.YahooFundamentals{
		RiskFreeRate: floatPointer(5), Beta: floatPointer(1.2),
	})
	if inputs.RiskFreeRate == nil || *inputs.RiskFreeRate != .05 || inputs.RequiredReturn == nil || *inputs.RequiredReturn != .116 {
		t.Fatalf("Yahoo CAPM inputs=%#v, want 5%% rate and 11.6%% required return", inputs)
	}
	result := valuation.Calculate(inputs)
	payload := valuationCachePayload(result, inputs)
	if payload["risk_free_rate"] != .05 || payload["required_return"] != .116 {
		t.Fatalf("cached Yahoo CAPM rates=%#v, want 0.05 and 0.116", payload)
	}
	if got := valuationRiskFreeRate(provider.YahooFundamentals{RiskFreeRate: floatPointer(0)}); got != .04 {
		t.Fatalf("zero Yahoo rate fallback=%v, want 0.04", got)
	}
}

func TestValuationInputsPreserveTimeseriesRetryForPSFallback(t *testing.T) {
	inputs := valuationInputs("TEST", nil, provider.YahooFundamentals{
		ValuationUpstreamError: true, ValuationUpstreamRetryable: true,
		Revenue: floatPointer(1000), Shares: floatPointer(10), PriceToSales: floatPointer(2),
	})
	if !inputs.UpstreamError {
		t.Fatal("valuation inputs dropped the provider's optional-upstream failure signal")
	}
	result := valuation.Calculate(inputs)
	if result.Status != valuation.StatusReady || result.Method != "信息页 P/S 低置信度兜底" {
		t.Fatalf("retryable info-page inputs produced %q/%q, want ready/P/S fallback", result.Status, result.Method)
	}
	applyTimeseriesRetryStatus(provider.YahooFundamentals{ValuationUpstreamRetryable: true}, &result)
	if result.Status != valuation.StatusReady || result.Warning != "" {
		t.Fatalf("retry status overrode a usable P/S fallback: %#v", result)
	}
}

func TestRunValuationUsesFreshFundamentalsPriceWithoutRewritingQuotePrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-current-price.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	quotePrice, fetchedAt := 50.0, time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "TEST", Price: &quotePrice, FetchedAt: &fetchedAt, Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	fundamentals := provider.YahooFundamentals{
		Price: floatPointer(120), EPS: floatPointer(10), ForwardEPS: floatPointer(10),
		MarketCap: floatPointer(1200), Shares: floatPointer(10), SharesSource: "balance_sheet",
		Revenue: floatPointer(1000), AnnualRevenue: []float64{1000, 900},
		FreeCashflow: floatPointer(100), AnnualEPS: []float64{10, 9},
	}
	service := NewWithProvider(config.Config{
		DatabasePath: path, AnalysisCacheEntries: 16, UpstreamTimeout: time.Second,
	}, repository, &testMarketProvider{fundamentals: fundamentals})
	defer service.Close()

	service.runValuation(context.Background(), "TEST")
	latest, found, err := repository.LatestQuote("TEST")
	if err != nil || !found {
		t.Fatalf("read completed quote: found=%v err=%v", found, err)
	}
	if latest.Price == nil || *latest.Price != quotePrice {
		t.Fatalf("valuation rewrote market quote price to %v; want %v", latest.Price, quotePrice)
	}
	if latest.FairValueMarketCapDataQuality == nil || *latest.FairValueMarketCapDataQuality != "consistent" {
		t.Fatalf("valuation market-cap quality=%v; want consistent at fresh price 120", latest.FairValueMarketCapDataQuality)
	}
}

func TestSNDKStorageCycleCoreValuesMatchPythonV39Golden(t *testing.T) {
	result := valuation.Calculate(valuation.Fundamentals{
		CurrentPrice: floatPointer(1719.11),
		Sector:       "Technology", Industry: "Computer Hardware Storage",
		TrailingEPS: floatPointer(100), NormalizedEPS: floatPointer(90),
		NormalizedEPSSource: "历史 EPS 中位数", AnnualEPS: []float64{100, 90, 80, 70, 60},
		ForwardEPS: floatPointer(527), ForwardEPSObserved: true,
		GrowthRate: floatPointer(.5), GrowthHintObserved: true, Beta: floatPointer(1.2),
		Revenue: floatPointer(1000), FreeCashFlow: floatPointer(250),
		OperatingCashflow: floatPointer(250), OperatingIncome: floatPointer(300),
		NetIncome: floatPointer(200), DepreciationAndAmortization: floatPointer(50),
		CapitalExpenditure: floatPointer(50), MarketCap: floatPointer(10e9),
		PriceToSales: floatPointer(12), TargetMeanPrice: floatPointer(2137),
		RiskFreeRate: floatPointer(.04), RequiredReturn: floatPointer(.106),
		Regime: valuation.RegimeNormal, RegimeSignals: map[string]any{"market": "normal"},
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	const pythonV39 = `{"buy_high":769.32,"buy_low":512.88,"confidence":"高","confidence_score":87.8,"data_quality":"complete","data_quality_score":100.0,"decision_tree":{"industry_filter":"普通行业","lifecycle":"AI存储/硬件周期","model":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE"},"defensive":{"buy_high":641.1,"buy_low":512.88,"high":1068.5,"low":854.8,"model":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE 压力测试","safety_margin":0.4,"value":961.65},"forward_eps":61.0571,"forward_eps_source":"Yahoo远期EPS","growth_rate":0.5,"high":1282.2,"historical_valuation_percentiles":null,"interest_coverage":null,"low":854.8,"market_cap_data_quality":"unknown","model":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","model_label":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","model_under_regime":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","normalized_eps":90.0,"normalized_eps_source":"历史 EPS 中位数","optimistic":{"buy_high":1099.03,"buy_low":732.69,"forward_eps":61.0571,"growth_rate":0.5,"high":1831.71,"low":1221.14,"model":"AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE","safety_margin":0.4,"value":1526.43},"owner_earnings_maintenance_ratio":0.5,"owner_earnings_ratio_source":"科技/消费","pe_high":30.0,"pe_low":20.0,"quarterly_momentum":null,"regime":"NORMAL","regime_signals":{"market":"normal"},"required_return":0.106,"risk_free_rate":0.04,"safety_margin":0.4,"shareholder_total_return_yield":null,"shares_source":null,"source":"valuation_v39","validation":{"median":null,"methods":{}},"value":1068.5,"warnings":["AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际"]}`
	var pythonResult, goResult map[string]any
	if err := json.Unmarshal([]byte(pythonV39), &pythonResult); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &goResult); err != nil {
		t.Fatal(err)
	}
	pythonToGo := map[string]string{
		"value": "fair_value", "low": "fair_value_low", "high": "fair_value_high",
		"buy_low": "fair_value_buy_low", "buy_high": "fair_value_buy_high", "model": "method",
		"risk_free_rate": "risk_free_rate", "required_return": "required_return",
		"regime": "regime", "regime_signals": "regime_signals",
		"defensive": "defensive", "optimistic": "optimistic", "data_quality_score": "data_quality_score",
		"normalized_eps_source": "normalized_eps_source", "market_cap_data_quality": "market_cap_data_quality",
		"safety_margin": "safety_margin", "warnings": "warnings",
	}
	sharedPython, sharedGo := make(map[string]any, len(pythonToGo)), make(map[string]any, len(pythonToGo))
	for pythonKey, goKey := range pythonToGo {
		sharedPython[pythonKey] = pythonResult[pythonKey]
		sharedGo[pythonKey] = goResult[goKey]
	}
	if !reflect.DeepEqual(sharedGo, sharedPython) {
		t.Fatalf("shared fields differ from Python v39 JSON golden:\n Go: %#v\nPython: %#v", sharedGo, sharedPython)
	}
	if result.ForwardEPS == nil || math.Round(*result.ForwardEPS*1e4)/1e4 != pythonResult["forward_eps"] {
		t.Fatalf("rounded forward EPS=%v, want Python v39 value %v", result.ForwardEPS, pythonResult["forward_eps"])
	}
	const want = `{"fair_value":1068.5,"fair_value_low":854.8,"fair_value_high":1282.2,"fair_value_buy_low":512.88,"fair_value_buy_high":769.32,"risk_free_rate":0.04,"required_return":0.106,"method":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","methods":{"周期调整远期 EPS × PE":1068.5},"regime":"NORMAL","regime_signals":{"market":"normal"},"model_under_regime":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","defensive":{"buy_high":641.1,"buy_low":512.88,"high":1068.5,"low":854.8,"model":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE 压力测试","safety_margin":0.4,"value":961.65},"optimistic":{"buy_high":1099.03,"buy_low":732.69,"forward_eps":61.0571,"growth_rate":0.5,"high":1831.71,"low":1221.14,"model":"AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE","safety_margin":0.4,"value":1526.43},"data_quality_score":100,"normalized_eps_source":"历史 EPS 中位数","market_cap_data_quality":"unknown","forward_eps":61.0571,"safety_margin":0.4,"confidence":0.7,"confidence_label":"medium","status":"ready","warnings":["AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际"]}`
	if string(encoded) != want {
		t.Fatalf("full storage-cycle result differs from golden:\n got: %s\nwant: %s", encoded, want)
	}
	if result.Method != "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE" {
		t.Fatalf("method=%q, want Python v39 storage-cycle branch", result.Method)
	}
	if result.FairValue == nil || *result.FairValue != 1068.5 {
		t.Fatalf("fair value=%v, want Python v39 golden 1068.5", result.FairValue)
	}
	if result.ForwardEPS == nil || math.Abs(*result.ForwardEPS-61.0571) > .0001 {
		t.Fatalf("reported forward EPS=%v, want Python v39 cap 61.0571", result.ForwardEPS)
	}
}

func TestValuationCachePayloadValidationAndTTL(t *testing.T) {
	ready := map[string]any{"source": goFairValueSource, "status": valuation.StatusReady, "value": 12.0}
	if _, ok := validValuationPayload(ready); !ok {
		t.Fatal("ready payload rejected")
	}
	if valuationCacheFresh(ready, time.Now().Add(-7*time.Hour)) {
		t.Fatal("ready payload older than six hours considered fresh")
	}
	retry := map[string]any{"source": goFairValueSource, "status": "retry", "value": nil}
	if _, ok := validValuationPayload(retry); !ok || !valuationCacheFresh(retry, time.Now().Add(-time.Second)) {
		t.Fatal("retry payload should be valid and fresh for its short TTL")
	}
	unavailable := map[string]any{"source": goFairValueSource, "status": valuation.StatusUnavailable, "value": nil}
	if _, ok := validValuationPayload(unavailable); !ok {
		t.Fatal("unavailable payload rejected")
	}
	if valuationCacheFresh(unavailable, time.Now().Add(-16*time.Second)) {
		t.Fatal("terminal payload older than short TTL considered fresh")
	}
	for _, malformed := range []map[string]any{
		{"source": "valuation_v39", "status": valuation.StatusReady, "value": 12.0},
		{"source": goFairValueSource, "status": valuation.StatusReady, "value": 0.0},
		{"source": goFairValueSource, "status": valuation.StatusReady, "value": "12"},
	} {
		if _, ok := validValuationPayload(malformed); ok {
			t.Fatalf("malformed payload accepted: %#v", malformed)
		}
	}
}

func TestOldGoValuationSourceIsRefreshable(t *testing.T) {
	if !isGoFairValueSource("valuation_go_v1") || !isGoFairValueSource(goFairValueSource) {
		t.Fatalf("Go valuation sources should be recognized across cache revisions")
	}
	if isGoFairValueSource("valuation_v39") || isGoFairValueSource("python-valuation") {
		t.Fatalf("non-Go valuation source incorrectly recognized as Go")
	}
	if goFairValueCacheVersion != "go_v10" || goFairValueSource != "valuation_go_v10" {
		t.Fatalf("Go valuation cache/source version did not advance: version=%q source=%q", goFairValueCacheVersion, goFairValueSource)
	}
}

func TestETFValuationCachePersistsUnavailableStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etf-valuation.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	service := NewWithProvider(config.Config{AnalysisCacheEntries: 16}, repository, nil)
	result := valuation.Calculate(valuation.Fundamentals{InstrumentType: "ETF"})
	if result.Status != valuation.StatusUnavailable {
		t.Fatalf("ETF valuation status = %q", result.Status)
	}
	service.persistValuationCache("QQQ", valuationCachePayload(result, valuation.Fundamentals{InstrumentType: "ETF"}))
	status, _, found, err := repository.GetAnalysisCacheEntry(fairValueStatusCacheKey("QQQ"))
	if err != nil || !found {
		t.Fatalf("ETF status cache found=%v err=%v", found, err)
	}
	payload, ok := status.(map[string]any)
	if !ok || payload["status"] != valuation.StatusUnavailable {
		t.Fatalf("cached ETF status = %#v, want unavailable", status)
	}
	quote := model.Quote{Symbol: "QQQ", FairValueStatus: "pending", FairValuePending: true}
	service.mergeCachedValuation("QQQ", &quote)
	if quote.FairValueStatus != valuation.StatusUnavailable || quote.FairValuePending || quote.FairValue != nil {
		t.Fatalf("merged ETF quote = %#v, want terminal unavailable without value", quote)
	}
}

func TestTimeseriesRateLimitProducesRetryDiagnosticsAndShortTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-timeseries-retry.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	price, now := 100.0, time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	upstream := &testMarketProvider{fundamentals: provider.YahooFundamentals{TimeseriesRetryable: true}}
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, upstream)
	defer service.Close()
	service.runValuation(context.Background(), "QQQ")

	payload, fresh := service.cachedValuation("QQQ")
	if !fresh {
		t.Fatal("rate-limited valuation should be cached for its short retry window")
	}
	if payload["status"] != "retry" || payload["warning"] != "Yahoo 数据源暂时限流，估值将在稍后自动重试" {
		t.Fatalf("retry payload diagnostics = %#v", payload)
	}
	if !valuationCacheFresh(payload, time.Now().Add(-time.Second)) || valuationCacheFresh(payload, time.Now().Add(-3*time.Second)) {
		t.Fatal("rate-limited valuation should use the two-second retry TTL")
	}
	quote := model.Quote{Symbol: "QQQ"}
	service.mergeCachedValuation("QQQ", &quote)
	if quote.FairValueStatus != "retry" || !quote.FairValuePending || quote.FairValueWarning != payload["warning"] {
		t.Fatalf("retry state was not surfaced to the quote: %#v", quote)
	}

	ready := valuation.Result{FairValue: floatPointer(120), Status: valuation.StatusReady, Warning: "existing candidate warning"}
	applyTimeseriesRetryStatus(provider.YahooFundamentals{TimeseriesRetryable: true}, &ready)
	if ready.Status != valuation.StatusReady || ready.Warning != "existing candidate warning" {
		t.Fatalf("partial timeseries failure changed a valid candidate: %#v", ready)
	}
}

func TestMergeCachedValuationKeepsValueAndOverlaysLatestAttemptDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-overlay.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, &testMarketProvider{})
	defer service.Close()
	service.persistValuationCache("QQQ", map[string]any{
		"value": 125.0, "low": 100.0, "high": 150.0, "source": goFairValueSource,
		"status": valuation.StatusReady, "model": "owner_earnings",
	})
	service.persistValuationCache("QQQ", map[string]any{
		"value": nil, "source": goFairValueSource, "status": "retry",
		"warning": "temporary upstream failure", "warnings": []string{"retrying"}, "errors": []string{"429"},
	})
	quote := model.Quote{Symbol: "QQQ"}
	service.mergeCachedValuation("QQQ", &quote)
	if quote.FairValue != 125.0 || quote.FairValueSource != goFairValueSource {
		t.Fatalf("ready value was lost: %#v", quote)
	}
	if quote.FairValueStatus != "retry" || !quote.FairValuePending || quote.FairValueWarning != "temporary upstream failure" {
		t.Fatalf("retry diagnostics not overlaid: %#v", quote)
	}
}

func TestDecorateStatusQuotePreservesGoRetryStateAlongsidePriorValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-status.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	price, value, now := 100.0, 125.0, time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, FairValue: value, FairValueSource: goFairValueSource, FairValueStatus: "ready", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, &testMarketProvider{})
	defer service.Close()
	service.persistValuationCache("QQQ", map[string]any{
		"value": nil, "source": goFairValueSource, "status": "retry",
		"warning": "temporary upstream failure", "warnings": []string{"retrying"}, "errors": []string{"429"},
	})
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found {
		t.Fatalf("load quote found=%v err=%v", found, err)
	}
	decorated := service.decorateStatusQuote(context.Background(), quote)
	if got := positiveNumeric(decorated.FairValue); got == nil || *got != value || decorated.FairValueStatus != "retry" || !decorated.FairValuePending {
		t.Fatalf("status normalization lost retry state/value: %#v", decorated)
	}
}

func TestExpiredRetryAttemptDoesNotGetHiddenByPriorReadyCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-retry-expired.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, &testMarketProvider{})
	defer service.Close()
	ready := map[string]any{"value": 125.0, "source": goFairValueSource, "status": valuation.StatusReady}
	retry := map[string]any{"value": nil, "source": goFairValueSource, "status": "retry", "warning": "temporary error"}
	if err := repository.PutAnalysisCache(fairValueCacheKey("QQQ"), ready, 32); err != nil {
		t.Fatal(err)
	}
	if err := repository.PutAnalysisCache(fairValueStatusCacheKey("QQQ"), retry, 32); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339Nano)
	if _, err := admin.Exec(`UPDATE api_analysis_cache SET created_at=? WHERE cache_key=?`, ancient, fairValueStatusCacheKey("QQQ")); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := admin.Close(); err != nil {
		t.Fatal(err)
	}
	if _, fresh := service.cachedValuation("QQQ"); fresh {
		t.Fatal("expired retry state incorrectly considered fresh due to older ready value")
	}
	quote := model.Quote{Symbol: "QQQ"}
	service.mergeCachedValuation("QQQ", &quote)
	if got := positiveNumeric(quote.FairValue); got == nil || *got != 125 || quote.FairValueStatus != valuation.StatusReady {
		t.Fatalf("expired retry should fall back to stale ready display state: %#v", quote)
	}
}

func TestMergeCachedValuationPrefersNewReadyAttemptDuringTwoKeyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-ready-overlay.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, &testMarketProvider{})
	defer service.Close()
	service.persistValuationCache("QQQ", map[string]any{
		"value": 125.0, "source": goFairValueSource, "status": valuation.StatusReady,
	})
	// Simulate the exact process-interruption window after the new status-key
	// write but before persistValuationCache updates the long-lived value key.
	if err := repository.PutAnalysisCache(fairValueStatusCacheKey("QQQ"), map[string]any{
		"value": 145.0, "source": goFairValueSource, "status": valuation.StatusReady,
	}, 32); err != nil {
		t.Fatal(err)
	}
	quote := model.Quote{Symbol: "QQQ"}
	service.mergeCachedValuation("QQQ", &quote)
	if got := positiveNumeric(quote.FairValue); got == nil || *got != 145 {
		t.Fatalf("newer completed value not selected: %#v", quote)
	}
}

func TestMergeCachedValuationNeverOverwritesPythonValueOrDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-python-authority.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32}, repository, &testMarketProvider{})
	defer service.Close()
	service.persistValuationCache("QQQ", map[string]any{
		"value": 99.0, "source": goFairValueSource, "status": valuation.StatusReady,
	})
	service.persistValuationCache("QQQ", map[string]any{
		"value": nil, "source": goFairValueSource, "status": "retry", "warning": "Go retry must not leak into Python quote",
	})
	pythonValue := 123.45
	quote := model.Quote{
		Symbol: "QQQ", FairValue: pythonValue, FairValueSource: "valuation_v39",
		FairValueStatus: valuation.StatusReady,
	}
	service.mergeCachedValuation("QQQ", &quote)
	if got := positiveNumeric(quote.FairValue); got == nil || *got != pythonValue || quote.FairValueSource != "valuation_v39" || quote.FairValueStatus != valuation.StatusReady || quote.FairValueWarning != "" {
		t.Fatalf("Go cache replaced authoritative Python valuation: %#v", quote)
	}
}
