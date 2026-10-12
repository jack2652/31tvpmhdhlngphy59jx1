package valuation

import (
	"math"
	"testing"
)

func lifecycleCompletionPtr(value float64) *float64 { return &value }

func TestGenericSlowdownDiscountsModeledForwardEPSLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software Infrastructure",
		LongBusinessSummary: "Provides workflow software and analytics.",
		NormalizedEPS:       lifecycleCompletionPtr(10),
		GrowthRate:          lifecycleCompletionPtr(.10),
		AIRevenueShare:      lifecycleCompletionPtr(.10),
		Regime:              RegimeSlowdown,
	})

	if result.ForwardEPS == nil || math.Abs(*result.ForwardEPS-7.7) > 1e-9 {
		t.Fatalf("modeled slowdown forward EPS=%v, want 7.7 (10 x 1.10 x 0.70); method=%q lifecycle=%v", result.ForwardEPS, result.Method, result.DecisionTree)
	}
	if !result.AIExposed {
		t.Fatal("generic AI exposure was not retained on the result")
	}
	if result.ForwardEPSSource != "模型估计远期EPS（放缓调整70%）" {
		t.Fatalf("forward EPS source=%q", result.ForwardEPSSource)
	}
}

func TestMobilitySlowdownDiscountsEPSFallbackLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Auto Manufacturers", IndustryKey: "auto-manufacturers",
		LongBusinessSummary:  "Builds autonomous robotaxi vehicles and robotics platforms.",
		CurrentPrice:         lifecycleCompletionPtr(200),
		SharesOutstanding:    lifecycleCompletionPtr(1e9),
		MarketCap:            lifecycleCompletionPtr(200e9),
		MarketCapDataQuality: "consistent",
		TrailingEPS:          lifecycleCompletionPtr(10),
		ForwardEPS:           lifecycleCompletionPtr(10),
		ForwardEPSObserved:   true,
		GrowthRate:           lifecycleCompletionPtr(.10),
		Regime:               RegimeSlowdown,
	})

	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("method=%q, want mobility growth model", result.Method)
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 7 {
		t.Fatalf("forward EPS=%v, want Python slowdown value 7", result.ForwardEPS)
	}
	if got := result.Optimistic["forward_eps"]; got != 7.0 {
		t.Fatalf("optimistic forward EPS=%v, want 7", got)
	}
	if got := result.Optimistic["low"]; got != 154.0 {
		t.Fatalf("slowdown optimistic low=%v, want 154 (7 x 22)", got)
	}
}

func TestDataQualityDeductionsMatchPythonCombinedInput(t *testing.T) {
	result := Calculate(Fundamentals{
		MarketCap:            lifecycleCompletionPtr(4e9),
		MarketCapDataQuality: "unit_mismatch",
		AnnualRevenue:        []float64{50, -1},
		AnalystCount:         lifecycleCompletionPtr(2),
	})

	// Python deductions: small cap 10, uncertain market cap 15, invalid revenue
	// history 15, and fewer than three analysts 20.
	if result.DataQualityScore != 40 {
		t.Fatalf("data quality score=%v, want 40", result.DataQualityScore)
	}
}

func TestMatureGrowthDerivesMissingMarketCapFromPriceAndSharesLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: lifecycleCompletionPtr(100), SharesOutstanding: lifecycleCompletionPtr(10e9),
		Revenue: lifecycleCompletionPtr(100e9), FreeCashFlow: lifecycleCompletionPtr(30e9),
		RevenueGrowth: lifecycleCompletionPtr(.09), GrowthRate: lifecycleCompletionPtr(.09),
		NormalizedEPS: lifecycleCompletionPtr(10), ForwardEPS: lifecycleCompletionPtr(12), ForwardEPSObserved: true,
		AnalystCount: lifecycleCompletionPtr(4),
	})

	if result.Method != "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("method=%q, want Python's mature-growth model from implied $1T market cap; lifecycle=%#v", result.Method, result.DecisionTree)
	}
	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "普通" {
		t.Fatalf("decision tree=%#v, want Python's ordinary lifecycle label for a mature-growth model", result.DecisionTree)
	}
	if result.MarketCapDataQuality != "derived" || result.DataQualityScore != 100 {
		t.Fatalf("market cap quality/score=%q/%v, want Python's derived/100", result.MarketCapDataQuality, result.DataQualityScore)
	}

	explicitZero := Fundamentals{
		CurrentPrice: lifecycleCompletionPtr(100), SharesOutstanding: lifecycleCompletionPtr(10e9),
		MarketCap: lifecycleCompletionPtr(0), Revenue: lifecycleCompletionPtr(100e9),
		FreeCashFlow: lifecycleCompletionPtr(30e9), RevenueGrowth: lifecycleCompletionPtr(.09),
		GrowthRate: lifecycleCompletionPtr(.09), NormalizedEPS: lifecycleCompletionPtr(10),
		ForwardEPS: lifecycleCompletionPtr(12), ForwardEPSObserved: true,
	}
	if result := Calculate(explicitZero); result.Method == "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("explicit zero market cap should not be replaced with implied market cap: %#v", result)
	}
}

func TestMobilityLifecycleDerivesMissingMarketCapLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software Infrastructure",
		LongBusinessSummary: "Builds robotaxi fleets and humanoid robotics platforms.",
		CurrentPrice:        lifecycleCompletionPtr(100), SharesOutstanding: lifecycleCompletionPtr(1e9),
		Revenue: lifecycleCompletionPtr(10e9), GrowthRate: lifecycleCompletionPtr(.05),
		ForwardEPS: lifecycleCompletionPtr(5), ForwardEPSObserved: true,
	})
	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "移动科技成长" {
		t.Fatalf("lifecycle=%#v, want mobility growth from an implied $100B market cap; method=%q", result.DecisionTree, result.Method)
	}
}

func TestFinancialDataQualityThresholdsMatchPython(t *testing.T) {
	base := Fundamentals{
		MarketCap: lifecycleCompletionPtr(5e9), Revenue: lifecycleCompletionPtr(100),
		AnnualRevenue: []float64{100}, AnalystCount: lifecycleCompletionPtr(3),
	}
	if result := Calculate(base); result.DataQualityScore != 100 {
		t.Fatalf("exact small-cap and analyst thresholds score=%v, want 100", result.DataQualityScore)
	}
	tests := []struct {
		name string
		edit func(*Fundamentals)
		want float64
	}{
		{name: "below small-cap threshold", edit: func(f *Fundamentals) { f.MarketCap = lifecycleCompletionPtr(5e9 - 1) }, want: 90},
		{name: "below analyst threshold", edit: func(f *Fundamentals) { f.AnalystCount = lifecycleCompletionPtr(3 - 1e-9) }, want: 80},
		{name: "invalid revenue observation despite current revenue", edit: func(f *Fundamentals) { f.AnnualRevenue = []float64{100, 0} }, want: 85},
		{name: "missing analysts and target", edit: func(f *Fundamentals) { f.AnalystCount = nil }, want: 90},
		{name: "target replaces missing analyst coverage", edit: func(f *Fundamentals) { f.AnalystCount, f.TargetMeanPrice = nil, lifecycleCompletionPtr(20) }, want: 100},
		{name: "uncertain market-cap quality", edit: func(f *Fundamentals) { f.MarketCapDataQuality = "market_cap_preferred" }, want: 85},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := base
			test.edit(&f)
			if result := Calculate(f); result.DataQualityScore != test.want {
				t.Fatalf("data quality score=%v, want %v", result.DataQualityScore, test.want)
			}
		})
	}
}

func TestStableCashflowLifecycleSourceParityCompletion(t *testing.T) {
	discountStore := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Discount Stores", IndustryKey: "discount-stores",
		Revenue: lifecycleCompletionPtr(100), FreeCashFlow: lifecycleCompletionPtr(20),
		ForwardEPS: lifecycleCompletionPtr(5), ForwardEPSObserved: true, GrowthRate: lifecycleCompletionPtr(.10),
		MarketCap: lifecycleCompletionPtr(50e9),
	})
	if discountStore.Method == "稳定现金流：正常化 EPS × 20–35 倍 PE" {
		t.Fatalf("method=%q, discount stores alone are not a Python stable-cashflow signal", discountStore.Method)
	}
	if discountStore.DecisionTree == nil || discountStore.DecisionTree.Lifecycle != "普通" {
		t.Fatalf("decision tree=%#v, want ordinary lifecycle without Python stable-cashflow signals", discountStore.DecisionTree)
	}

	softwareServingRestaurants := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software Infrastructure", IndustryKey: "software-infrastructure",
		LongBusinessSummary: "Provides workflow software to restaurants and food businesses.",
		Revenue:             lifecycleCompletionPtr(100), FreeCashFlow: lifecycleCompletionPtr(20),
		ForwardEPS: lifecycleCompletionPtr(5), ForwardEPSObserved: true, GrowthRate: lifecycleCompletionPtr(.10),
	})
	if softwareServingRestaurants.Method != "稳定现金流：正常化 EPS × 20–35 倍 PE" {
		t.Fatalf("method=%q, Python classification text includes restaurant/food business-summary signals", softwareServingRestaurants.Method)
	}
	if softwareServingRestaurants.DecisionTree == nil || softwareServingRestaurants.DecisionTree.Lifecycle != "稳定现金流" {
		t.Fatalf("decision tree=%#v, want stable-cashflow lifecycle from Python classification text", softwareServingRestaurants.DecisionTree)
	}
}

func TestFinancialScenarioRangesMatchPythonDDMPBAndTargets(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial", Industry: "Asset Management",
		CurrentPrice: lifecycleCompletionPtr(20), ForwardEPS: lifecycleCompletionPtr(5), ForwardEPSObserved: true,
		BookValue: lifecycleCompletionPtr(18), DividendRate: lifecycleCompletionPtr(1),
		TargetMeanPrice: lifecycleCompletionPtr(24), TargetLowPrice: lifecycleCompletionPtr(12), TargetHighPrice: lifecycleCompletionPtr(35),
		GrowthRate: lifecycleCompletionPtr(.10), RequiredReturn: lifecycleCompletionPtr(.10),
	})
	if result.FairValue == nil || *result.FairValue != 19.8 {
		t.Fatalf("financial fair value=%v, want DDM/PB/consensus median 19.8", result.FairValue)
	}
	if result.FairValueLow == nil || *result.FairValueLow != 16.24 || result.FairValueHigh == nil || *result.FairValueHigh != 23.36 {
		t.Fatalf("financial main range=%v-%v, want Python's 18%% three-candidate spread", result.FairValueLow, result.FairValueHigh)
	}
	assertScenario(t, result.Defensive, 19.8, 15.84, 23.76, .25)
	if result.Defensive["model"] != "金融/地产行业过滤：DDM + P/B 压力测试" {
		t.Fatalf("defensive model=%v, want Python financial stress-test label", result.Defensive["model"])
	}
	assertScenario(t, result.Optimistic, 23.5, 12, 35, .25)
	if result.Optimistic["model"] != "金融/地产：DDM + P/B" {
		t.Fatalf("optimistic model=%v, want Python financial DDM/PB label", result.Optimistic["model"])
	}
}

func TestFinancialWithoutDDMPBHasNoPythonOptimisticScenario(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial", Industry: "Asset Management",
		CurrentPrice: lifecycleCompletionPtr(20), ForwardEPS: lifecycleCompletionPtr(5), ForwardEPSObserved: true,
		TargetMeanPrice: lifecycleCompletionPtr(24),
	})
	if result.Status != StatusReady || result.Optimistic != nil {
		t.Fatalf("financial result=%#v, want consensus-only estimate without an optimistic DDM/PB scenario", result)
	}
}
