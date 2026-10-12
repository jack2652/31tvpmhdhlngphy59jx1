package server

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
	"option-scope-go/internal/valuation"
)

func TestSNDKValuationDiagnosticsSurviveCacheToQuoteJSON(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "sndk-valuation-diagnostics.db")
	repository, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := NewWithProvider(config.Config{DatabasePath: databasePath, AnalysisCacheEntries: 16}, repository, &testMarketProvider{})
	defer service.Close()

	inputs := valuation.Fundamentals{
		CurrentPrice: float64Pointer(1719.11),
		Sector:       "Technology", Industry: "Computer Hardware Storage",
		TrailingEPS: float64Pointer(100), NormalizedEPS: float64Pointer(90),
		NormalizedEPSSource: "历史 EPS 中位数", AnnualEPS: []float64{100, 90, 80, 70, 60},
		ForwardEPS: float64Pointer(527), ForwardEPSObserved: true,
		GrowthRate: float64Pointer(.5), GrowthHintObserved: true,
		Revenue: float64Pointer(1000), FreeCashFlow: float64Pointer(250),
		OperatingCashflow: float64Pointer(250), OperatingIncome: float64Pointer(300),
		NetIncome: float64Pointer(200), DepreciationAndAmortization: float64Pointer(50),
		CapitalExpenditure: float64Pointer(50), MarketCap: float64Pointer(10e9),
		PriceToSales: float64Pointer(12), TargetMeanPrice: float64Pointer(2137),
		RiskFreeRate: float64Pointer(.04), RequiredReturn: float64Pointer(.106),
		Regime: valuation.RegimeNormal,
	}
	result := valuation.Calculate(inputs)
	service.persistValuationCache("SNDK", valuationCachePayload(result, inputs))
	quote := model.Quote{Symbol: "SNDK"}
	service.mergeCachedValuation("SNDK", &quote)

	encoded, err := json.Marshal(quoteAPIValue(quote))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"fair_value_normalized_eps": 90,
		"fair_value_forward_eps":    61.0571,
		"fair_value_pe_low":         20,
		"fair_value_pe_high":        30,
	} {
		if got[key] != want {
			t.Errorf("%s=%v, want %v; JSON=%s", key, got[key], want, encoded)
		}
	}
	if got["fair_value_forward_eps_source"] != "Yahoo远期EPS" {
		t.Errorf("forward EPS source=%v, want Yahoo远期EPS", got["fair_value_forward_eps_source"])
	}
	tree, ok := got["fair_value_decision_tree"].(map[string]any)
	if !ok || tree["industry_filter"] != "普通行业" || tree["lifecycle"] != "AI存储/硬件周期" ||
		tree["model"] != result.Method {
		t.Errorf("decision tree=%#v, want Python v39 SNDK labels", got["fair_value_decision_tree"])
	}
	validation, ok := got["fair_value_validation"].(map[string]any)
	if !ok || validation["median"] != nil {
		t.Errorf("validation=%#v, want null median", got["fair_value_validation"])
	}
	methods, ok := validation["methods"].(map[string]any)
	if !ok || len(methods) != 0 {
		t.Errorf("validation methods=%#v, want empty map", validation["methods"])
	}

	service.persistValuationCache("SNDK", map[string]any{
		"value": nil, "source": goFairValueSource, "status": "retry", "warning": "temporary failure",
	})
	withRetry := model.Quote{Symbol: "SNDK"}
	service.mergeCachedValuation("SNDK", &withRetry)
	if withRetry.FairValueStatus != "retry" || withRetry.FairValueNormalizedEPS == nil || *withRetry.FairValueNormalizedEPS != 90 ||
		withRetry.FairValueDecisionTree == nil || withRetry.FairValueValidation == nil {
		t.Errorf("retry overlay lost ready SNDK diagnostics or status: %#v", withRetry)
	}
}

func TestNonFiniteGrowthHintDoesNotBlockMatureGrowthModel(t *testing.T) {
	invalidGrowth := math.NaN()
	fundamentals := provider.YahooFundamentals{
		EPS: float64Pointer(4), AnnualEPS: []float64{3, 2, 1},
		EarningsGrowth: &invalidGrowth,
		Sector:         "Technology", Industry: "Software Infrastructure",
		MarketCap: float64Pointer(1.2e12), Revenue: float64Pointer(100), FreeCashflow: float64Pointer(25),
	}
	inputs := valuationInputs("MEGA", float64Pointer(100), fundamentals)
	if inputs.GrowthHintObserved {
		t.Fatal("non-finite Yahoo growth must not count as an observed growth hint")
	}
	result := valuation.Calculate(inputs)
	if result.Method != "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("method=%q, want Python mature-growth branch; inputs=%#v", result.Method, inputs)
	}
}
