package valuation

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func ptr(value float64) *float64 { return &value }

func TestValuationRoundingMatchesPythonDecimalRound(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  float64
		places int
		want   float64
	}{
		{"currency just below decimal half", 289.575, 2, 289.57},
		{"currency just above decimal half", 452.925, 2, 452.93},
		{"exact half to even down", 1.125, 2, 1.12},
		{"exact half to even up", 1.375, 2, 1.38},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := roundPython(test.value, test.places); got != test.want {
				t.Fatalf("roundPython(%v, %d)=%v, want Python result %v", test.value, test.places, got, test.want)
			}
		})
	}
}

func TestCalculateDerivesOwnerEarningsAndRanges(t *testing.T) {
	result := Calculate(Fundamentals{
		NetIncome: ptr(100), DepreciationAndAmortization: ptr(20),
		CapitalExpenditure: ptr(-40), MaintenanceCapexRatio: ptr(0.50),
		ChangeInWorkingCapital: ptr(5), SharesOutstanding: ptr(10),
		RequiredReturn: ptr(0.10), TerminalGrowth: ptr(0.02), SafetyMargin: ptr(0.30),
	})
	if result.Status != StatusReady || result.Method != MethodOwnerEarnings {
		t.Fatalf("status/method = %q/%q, want ready/%q", result.Status, result.Method, MethodOwnerEarnings)
	}
	assertFloat(t, result.OwnerEarningsPerShare, 9.5)
	assertFloat(t, result.FairValue, 118.75)
	assertFloat(t, result.FairValueLow, 95.00)
	assertFloat(t, result.FairValueHigh, 142.50)
	assertFloat(t, result.FairValueBuyLow, 66.50)
	assertFloat(t, result.FairValueBuyHigh, 99.75)
	if result.Confidence != 0.85 || result.ConfidenceLabel != "high" {
		t.Fatalf("confidence = %v/%q, want 0.85/high", result.Confidence, result.ConfidenceLabel)
	}
	if result.Warning != "" {
		t.Fatalf("explicit-input owner earnings warning = %q, want empty", result.Warning)
	}
	assertFloat(t, result.RequiredReturn, .10)
	if result.RiskFreeRate != nil {
		t.Fatalf("risk-free rate=%v, want nil when no provider value was supplied", *result.RiskFreeRate)
	}
}

func TestCalculateCarriesValuationRates(t *testing.T) {
	result := Calculate(Fundamentals{
		RiskFreeRate: ptr(.04), RequiredReturn: ptr(.095), ForwardEPS: ptr(5),
	})
	assertFloat(t, result.RiskFreeRate, .04)
	assertFloat(t, result.RequiredReturn, .095)
}

func TestCalculateOwnerEarningsTakesPriorityAndRecordsOtherCandidates(t *testing.T) {
	result := Calculate(Fundamentals{
		OwnerEarnings: ptr(100), SharesOutstanding: ptr(10), ForwardEPS: ptr(5),
		FreeCashFlow: ptr(80), GrowthRate: ptr(0.03), RequiredReturn: ptr(0.10), TerminalGrowth: ptr(0.02),
	})
	if result.Method != MethodOwnerEarnings {
		t.Fatalf("method = %q, want %q", result.Method, MethodOwnerEarnings)
	}
	if _, ok := result.Methods[MethodForwardPE]; !ok {
		t.Fatalf("methods = %#v, missing forward PE", result.Methods)
	}
	if _, ok := result.Methods[MethodFCFDCF]; !ok {
		t.Fatalf("methods = %#v, missing FCF DCF", result.Methods)
	}
	if result.Warning != "" {
		t.Fatalf("warning = %q, want empty", result.Warning)
	}
}

func TestProviderValuationSelectsPythonCandidateMedianAfterUnitFiltering(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "software-application", Sector: "Technology", Industry: "Software Application",
		CurrentPrice: ptr(30), MarketCap: ptr(300), SharesOutstanding: ptr(10),
		TrailingEPS: ptr(1), NormalizedEPS: ptr(1), ForwardEPS: ptr(1), ForwardEPSObserved: true,
		GrowthRate: ptr(.03), FreeCashFlow: ptr(20), FreeCashFlowPerShare: ptr(2),
		NetIncome: ptr(200), Revenue: ptr(1000), AnnualRevenue: []float64{1000, 900},
		AnalystCount: ptr(5), RequiredReturn: ptr(.095), TerminalGrowth: ptr(.015),
	})
	if result.Status != StatusReady || result.Method != MethodOwnerEarnings {
		t.Fatalf("status/method=%q/%q, want Python-compatible provider valuation with owner earnings as model label", result.Status, result.Method)
	}
	// Python produces owner earnings $250, PE $20 and DCF $27.04; the owner
	// earnings outlier is filtered against the EPS anchor, leaving $27.04.
	assertFloat(t, result.FairValue, 27.04)
	assertFloat(t, result.FairValueLow, 22.17)
	assertFloat(t, result.FairValueHigh, 31.91)
	assertFloat(t, result.FairValueBuyLow, 15.52)
	assertFloat(t, result.FairValueBuyHigh, 22.34)
	if result.Defensive["value"] != 27.04 || result.Defensive["low"] != 21.09 || result.Defensive["high"] != 32.99 || result.Defensive["safety_margin"] != .30 {
		t.Fatalf("defensive scenario=%#v, want Python v39 defensive golden", result.Defensive)
	}
	if result.Defensive["buy_low"] != 14.76 || result.Defensive["buy_high"] != 23.09 {
		t.Fatalf("defensive buy range=%#v, want Python v39 buy range", result.Defensive)
	}
	if result.Optimistic["value"] != 22.0 || result.Optimistic["low"] != 18.7 || result.Optimistic["high"] != 25.3 || result.Optimistic["safety_margin"] != .30 {
		t.Fatalf("optimistic scenario=%#v, want Python v39 optimistic golden", result.Optimistic)
	}
	if result.Optimistic["buy_low"] != 13.09 || result.Optimistic["buy_high"] != 17.71 {
		t.Fatalf("optimistic buy range=%#v, want Python v39 buy range", result.Optimistic)
	}
	if got := result.Methods[MethodOwnerEarnings]; got != 250 {
		t.Fatalf("owner earnings diagnostic=%v, want 250 retained as a rejected candidate diagnostic", got)
	}
}

func TestOwnerEarningsAllowsMissingWorkingCapitalAndDAAndFallsBackToFCF(t *testing.T) {
	derived := Calculate(Fundamentals{NetIncome: ptr(100), SharesOutstanding: ptr(10), CapitalExpenditure: ptr(-20), MaintenanceCapexRatio: ptr(.5)})
	if derived.Method != MethodOwnerEarnings {
		t.Fatalf("missing optional D&A/WC must still derive owner earnings: %#v", derived)
	}
	assertFloat(t, derived.OwnerEarningsPerShare, 9)
	fallback := Calculate(Fundamentals{FreeCashFlow: ptr(50), SharesOutstanding: ptr(10), RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02)})
	if fallback.Method != MethodOwnerEarnings {
		t.Fatalf("Python owner earnings fallback to FCF should be available: %#v", fallback)
	}
	assertFloat(t, fallback.OwnerEarningsPerShare, 5)
}

func TestCalculateIncludesScenarioAndIndependentQualityMetadata(t *testing.T) {
	result := Calculate(Fundamentals{
		OwnerEarnings: ptr(100), SharesOutstanding: ptr(10), CurrentPrice: ptr(50),
		ForwardEPS: ptr(5), FreeCashFlow: ptr(80), GrowthRate: ptr(.03),
		MarketCap: ptr(500), AnnualRevenue: []float64{1000, 900}, AnnualEPS: []float64{3, 4},
		Buyback: ptr(-10), DividendRate: ptr(1), AnalystCount: ptr(5),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02), SafetyMargin: ptr(.30),
	})
	if result.DataQualityScore != 90 {
		t.Fatalf("independent data quality score=%v, want 90", result.DataQualityScore)
	}
	if result.Confidence == result.DataQualityScore/100 {
		t.Fatalf("data quality score should not be an alias for method confidence: %#v", result)
	}
	if result.Defensive == nil || result.Optimistic == nil {
		t.Fatalf("scenario results missing: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
	if got := result.ShareholderTotalReturnYield; got == nil || math.Abs(*got-.04) > 1e-9 {
		t.Fatalf("shareholder return yield=%v, want 0.04", got)
	}
	if result.NormalizedEPSSource != "历史 EPS 中位数" || result.MarketCapDataQuality != "consistent" {
		t.Fatalf("metadata source or market-cap quality mismatch: %#v", result)
	}
}

func TestInterestCoverageUsesUnroundedRatioToTightenOptimisticScenario(t *testing.T) {
	result := Calculate(Fundamentals{
		ForwardEPS: ptr(10), OperatingIncome: ptr(1.999), InterestExpense: ptr(1),
	})
	if result.Status != StatusReady || result.Optimistic == nil {
		t.Fatalf("result=%#v, want a ready valuation with an optimistic scenario", result)
	}
	assertFloat(t, result.InterestCoverage, 2)
	if got := result.Optimistic["high"]; got != 255.0 {
		t.Fatalf("optimistic high=%v, want 255 after raw 1.999x coverage applies the Python 15%% haircut", got)
	}
	if got := result.Optimistic["buy_high"]; got != 204.0 {
		t.Fatalf("optimistic buy_high=%v, want 204 after the same Python 15%% haircut", got)
	}
	foundWarning := false
	for _, warning := range result.Warnings {
		if warning == "利息覆盖率低于 2 倍，乐观估值已收紧" {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("warnings=%#v, want low-interest-coverage warning for raw ratio 1.999", result.Warnings)
	}
}

func TestCalculateDataQualityPenalizesIncompleteInputsSeparately(t *testing.T) {
	result := Calculate(Fundamentals{ForwardEPS: ptr(5), RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02)})
	if result.DataQualityScore != 75 {
		t.Fatalf("missing data quality score=%v, want 75", result.DataQualityScore)
	}
	if result.Confidence != .65 {
		t.Fatalf("method confidence=%v should remain independent of data quality", result.Confidence)
	}
}

func TestDataQualityScoreUsesPositiveRevenueFallbackWhenAnnualSeriesMissing(t *testing.T) {
	withFallback := Calculate(Fundamentals{
		Revenue: ptr(100), AnalystCount: ptr(5), ForwardEPS: ptr(5),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	if withFallback.DataQualityScore != 100 {
		t.Fatalf("positive current revenue should replace missing annual series: score=%v, want 100", withFallback.DataQualityScore)
	}

	withoutFallback := Calculate(Fundamentals{
		Revenue: ptr(0), AnalystCount: ptr(5), ForwardEPS: ptr(5),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	if withoutFallback.DataQualityScore != 85 {
		t.Fatalf("nonpositive current revenue should retain missing-revenue penalty: score=%v, want 85", withoutFallback.DataQualityScore)
	}
}

func TestCalculateFallsBackToForwardEPSPE(t *testing.T) {
	result := Calculate(Fundamentals{ForwardEPS: ptr(5), ForwardPELow: ptr(10), ForwardPEHigh: ptr(20), SafetyMargin: ptr(0.25)})
	if result.Status != StatusReady || result.Method != MethodForwardPE {
		t.Fatalf("status/method = %q/%q, want ready/%q", result.Status, result.Method, MethodForwardPE)
	}
	assertFloat(t, result.FairValue, 75)
	assertFloat(t, result.FairValueLow, 60)
	assertFloat(t, result.FairValueHigh, 90)
	assertFloat(t, result.FairValueBuyLow, 45)
	assertFloat(t, result.FairValueBuyHigh, 67.5)
	if !strings.Contains(result.Warning, "owner earnings unavailable") {
		t.Fatalf("warning = %q, want fallback warning", result.Warning)
	}
	if result.ConfidenceLabel != "medium" || result.Confidence != 0.65 {
		t.Fatalf("confidence = %v/%q, want 0.65/medium", result.Confidence, result.ConfidenceLabel)
	}
}

func TestCalculateGenericModelsForwardEPSFromNormalizedEPSWithoutGrowthHistory(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "software-application", NormalizedEPS: ptr(5),
	})
	if result.Status != StatusReady || result.Method != MethodForwardPE {
		t.Fatalf("status/method = %q/%q, want ready/%q", result.Status, result.Method, MethodForwardPE)
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 5 {
		t.Fatalf("modeled forward EPS=%v, want normalized EPS 5 when growth is unavailable", result.ForwardEPS)
	}
	if result.Methods[MethodForwardPE] != 100 {
		t.Fatalf("forward PE candidate=%v, want normalized EPS 5 x default 20 PE", result.Methods[MethodForwardPE])
	}
}

func TestCalculateCapsGenericForwardEPSAgainstTrailingEPS(t *testing.T) {
	forward, trailing := 100.0, 10.0
	result := Calculate(Fundamentals{ForwardEPS: &forward, TrailingEPS: &trailing})
	if result.ForwardEPS == nil || *result.ForwardEPS != 30 {
		t.Fatalf("capped forward EPS=%v want 30", result.ForwardEPS)
	}
	if forward != 100 {
		t.Fatalf("input ForwardEPS mutated to %v", forward)
	}
	if result.Methods[MethodForwardPE] != 600 {
		t.Fatalf("PE valuation=%v want 600", result.Methods[MethodForwardPE])
	}
	if !strings.Contains(result.Warning, "capped at 3x trailing EPS") {
		t.Fatalf("missing cap warning: %q", result.Warning)
	}
	boundary := Calculate(Fundamentals{ForwardEPS: ptr(30), TrailingEPS: ptr(10)})
	if boundary.ForwardEPS == nil || *boundary.ForwardEPS != 30 || strings.Contains(boundary.Warning, "capped") {
		t.Fatalf("boundary result unexpectedly capped: %#v", boundary)
	}
	missing := Calculate(Fundamentals{ForwardEPS: ptr(100)})
	if missing.ForwardEPS == nil || *missing.ForwardEPS != 100 {
		t.Fatalf("missing trailing EPS should not alter forward EPS: %#v", missing)
	}
	nonpositive := Calculate(Fundamentals{ForwardEPS: ptr(100), TrailingEPS: ptr(-10)})
	if nonpositive.ForwardEPS == nil || *nonpositive.ForwardEPS != 100 {
		t.Fatalf("nonpositive trailing EPS should not cap: %#v", nonpositive)
	}
}

func TestCalculateCapsGenericForwardEPSAgainstNormalizedEPSWhenAvailable(t *testing.T) {
	result := Calculate(Fundamentals{
		ForwardEPS: ptr(100), TrailingEPS: ptr(10), NormalizedEPS: ptr(20),
	})
	if result.ForwardEPS == nil || *result.ForwardEPS != 60 {
		t.Fatalf("forward EPS cap=%v, want 3x normalized EPS (60)", result.ForwardEPS)
	}
}

func TestCalculateMatureGrowthRequiresPythonCashFlowAndGrowthSignals(t *testing.T) {
	base := Fundamentals{
		IndustryKey: "software-application", MarketCap: ptr(6e11), Revenue: ptr(100e9),
		FreeCashFlow: ptr(25e9), RevenueGrowth: ptr(.07), GrowthRate: ptr(.07),
		GrowthHintObserved: true, NormalizedEPS: ptr(10), ForwardEPS: ptr(20),
		ForwardEPSObserved: true,
	}
	result := Calculate(base)
	if result.Method != "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("qualified mature-growth company selected %q, want mature-growth model", result.Method)
	}
	if result.Warning != "" || len(result.Warnings) != 0 {
		t.Fatalf("mature-growth model has a spurious industry warning: warning=%q warnings=%#v", result.Warning, result.Warnings)
	}
	if got := result.Methods["成熟增长 PE"]; got != 371.25 {
		t.Fatalf("mature-growth PE candidate=%v, want normalized EPS floor capped at 1.35x: 371.25", got)
	}
	assertFloat(t, result.FairValueLow, 289.57)
	assertFloat(t, result.FairValueHigh, 452.93)
	assertFloat(t, result.FairValueBuyLow, 231.66)
	assertFloat(t, result.FairValueBuyHigh, 362.34)
	if result.SafetyMargin != .20 || result.Defensive == nil || result.Defensive["value"] != 287.5 || result.Defensive["safety_margin"] != .20 {
		t.Fatalf("mature-growth defensive scenario=%#v margin=%v", result.Defensive, result.SafetyMargin)
	}
	if result.Optimistic == nil || result.Optimistic["low"] != 337.5 || result.Optimistic["high"] != 472.5 || result.Optimistic["safety_margin"] != .18 {
		t.Fatalf("mature-growth optimistic scenario=%#v", result.Optimistic)
	}

	belowCap := base
	belowCap.MarketCap = ptr(4e11)
	if result := Calculate(belowCap); result.Method == "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("sub-$500B company incorrectly selected mature-growth model: %#v", result)
	}

	weakCashFlow := base
	weakCashFlow.MarketCap = ptr(1.1e12)
	weakCashFlow.FreeCashFlow = ptr(19e9)
	if result := Calculate(weakCashFlow); result.Method == "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("company below 20%% FCF margin incorrectly selected mature-growth model: %#v", result)
	}
}

func TestStableCashflowLifecycleMatchesPythonClassificationText(t *testing.T) {
	tests := []struct {
		name       string
		industry   string
		summary    string
		wantStable bool
	}{
		{name: "industry context", industry: "Restaurants", wantStable: true},
		{name: "business summary only", industry: "Software Application", summary: "Operates a restaurant and retail platform.", wantStable: true},
		{name: "discount stores is not stable cashflow keyword", industry: "Software Application", summary: "Provides services to discount stores.", wantStable: false},
		{name: "discount stores industry", industry: "Discount Stores", wantStable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(Fundamentals{
				Sector: "Technology", Industry: test.industry, IndustryKey: "software-application",
				LongBusinessSummary: test.summary, Revenue: ptr(100), FreeCashFlow: ptr(20),
				GrowthRate: ptr(.10), ForwardEPS: ptr(2), ForwardEPSObserved: true,
			})
			gotStable := result.Method == "稳定现金流：正常化 EPS × 20–35 倍 PE"
			if gotStable != test.wantStable {
				t.Fatalf("method=%q stable-cashflow=%v, want %v; result=%#v", result.Method, gotStable, test.wantStable, result)
			}
			if result.DecisionTree == nil {
				t.Fatalf("missing decision tree: %#v", result)
			}
			wantLifecycle := "普通"
			if test.wantStable {
				wantLifecycle = "稳定现金流"
			}
			if result.DecisionTree.Lifecycle != wantLifecycle {
				t.Fatalf("lifecycle=%q, want %q", result.DecisionTree.Lifecycle, wantLifecycle)
			}
		})
	}
}

func TestDecisionTreeFinancialSectorMatchesPythonV39(t *testing.T) {
	result := Calculate(Fundamentals{Sector: "Financial", Industry: "Asset Management"})
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "金融/地产" || result.DecisionTree.Lifecycle != "金融/地产" {
		t.Fatalf("financial sector decision tree=%#v, want Python v39 financial filter and lifecycle", result.DecisionTree)
	}

	result = Calculate(Fundamentals{IndustryKey: "real_estate_services"})
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "金融/地产" || result.DecisionTree.Lifecycle != "金融/地产" {
		t.Fatalf("normalized real-estate industry key decision tree=%#v, want Python v39 financial filter and lifecycle", result.DecisionTree)
	}
}

func TestDigitalRetailDecisionTreeKeepsStableCashflowLifecycle(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Retail", MarketCap: ptr(2e11),
		ForwardEPS: ptr(5), ForwardEPSObserved: true, FreeCashFlow: ptr(20),
		Revenue: ptr(100), SharesOutstanding: ptr(1e9), GrowthRate: ptr(.10),
	})
	if result.Method != "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("method=%q, want digital-retail model; result=%#v", result.Method, result)
	}
	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "稳定现金流" {
		t.Fatalf("decision tree=%#v, want Python v39 stable-cashflow lifecycle", result.DecisionTree)
	}
}

func TestCalculateSpecializedClassificationMatchesPythonKeywordOrder(t *testing.T) {
	tests := []struct {
		name        string
		fundamental Fundamentals
		wantMethod  string
	}{
		{
			name: "property and casualty summary selects insurance",
			fundamental: Fundamentals{
				Sector: "Technology", Industry: "Software Application",
				LongBusinessSummary:   "Underwrites property & casualty policies.",
				EmbeddedValuePerShare: ptr(10), NewBusinessValuePerShare: ptr(2),
			},
			wantMethod: "保险：内含价值 EV + 新业务价值 NBV",
		},
		{
			name: "REIT precedes biotech when both appear in summary",
			fundamental: Fundamentals{
				Sector: "Technology", Industry: "Other",
				LongBusinessSummary: "A real estate investment trust focused on biotechnology.",
				AFFOPerShare:        ptr(1), NAVPerShare: ptr(10),
			},
			wantMethod: MethodREIT,
		},
		{
			name: "energy precedes utility when both appear in summary",
			fundamental: Fundamentals{
				Sector: "Technology", Industry: "Other",
				LongBusinessSummary: "An energy producer operating regulated utilities.",
				NAVPerShare:         ptr(10), DividendRate: ptr(1), RABPerShare: ptr(8),
			},
			wantMethod: "能源/矿业：有限寿命 NAV + EV/EBITDA",
		},
		{
			name: "energy specialist precedes generic financial filter",
			fundamental: Fundamentals{
				Sector: "Financial Services", Industry: "Other",
				LongBusinessSummary: "An energy exploration company.", NAVPerShare: ptr(10),
			},
			wantMethod: "能源/矿业：有限寿命 NAV + EV/EBITDA",
		},
		{
			name: "business summary banking signal outranks mapped energy key",
			fundamental: Fundamentals{
				IndustryKey: "gold", LongBusinessSummary: "A regional bank with gold mining operations.",
				BookValue: ptr(10), ROE: ptr(.10),
			},
			wantMethod: "银行：P/B-ROE + 剩余收益 RI",
		},
		{
			name: "utility summary signal outranks mapped internet key",
			fundamental: Fundamentals{
				IndustryKey:         "internet-content-information",
				LongBusinessSummary: "A regulated utility operator.",
				DividendRate:        ptr(1), RABPerShare: ptr(10),
			},
			wantMethod: "公用事业：DDM + RAB",
		},
		{
			name: "specialized internet model precedes semiconductor growth signal",
			fundamental: Fundamentals{
				IndustryKey: "internet-content-information", LongBusinessSummary: "Builds a semiconductor platform.",
				GrowthRate: ptr(.30), ForwardEPS: ptr(10), ForwardEPSObserved: true, PriceToSales: ptr(12),
				Revenue: ptr(100), SharesOutstanding: ptr(10),
			},
			wantMethod: "互联网平台：用户质量 + 远期 P/S",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.fundamental)
			if result.Method != test.wantMethod {
				t.Fatalf("method=%q, want %q; result=%#v", result.Method, test.wantMethod, result)
			}
		})
	}
}

func TestDefenseTransitionKeepsPythonSemiconductorEPSCap(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Semiconductors", GrowthRate: ptr(.30),
		NormalizedEPS: ptr(10), ForwardEPS: ptr(100), ForwardEPSObserved: true,
		DefenseRevenueGrowth: ptr(.30), PriceToSales: ptr(12),
		Revenue: ptr(100), SharesOutstanding: ptr(10),
	})
	if !strings.Contains(result.Method, "国防订单驱动转型") {
		t.Fatalf("method=%q, want defense-transition model: %#v", result.Method, result)
	}
	assertFloat(t, result.ForwardEPS, 50)
}

func TestDefenseTransitionDefensiveScenarioMatchesPythonAnchors(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(1103.35), Sector: "Technology", Industry: "Semiconductors",
		GrowthRate: ptr(.30), RevenueGrowth: ptr(.30), DefenseRevenueGrowth: ptr(.30),
		Revenue: ptr(100), SharesOutstanding: ptr(10),
		TargetMeanPrice: ptr(1161.96), TargetLowPrice: ptr(820),
	})
	if result.Method != "国防订单驱动转型：远期 P/S + 分析师共识" {
		t.Fatalf("method=%q, want defense transition", result.Method)
	}
	if result.Defensive["value"] != 820.0 || result.Defensive["low"] != 639.6 || result.Defensive["high"] != 1000.4 {
		t.Fatalf("defensive scenario=%#v, want Python aligned anchor 820 with ±22%%", result.Defensive)
	}
}

func TestDefenseTransitionIncludesAnalystTargetLowCandidate(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Communication Equipment",
		GrowthRate: ptr(.30), RevenueGrowth: ptr(.30),
		DefenseRevenueGrowth: ptr(.30),
		Revenue:              ptr(100), SharesOutstanding: ptr(10),
		TargetMeanPrice: ptr(1161.96), TargetLowPrice: ptr(820),
	})
	if got := result.Methods["分析师目标下沿"]; got != 820.0 {
		t.Fatalf("defense candidates=%#v, analyst target low=%v; want 820 included in Python-compatible median", result.Methods, got)
	}
	if result.Defensive["value"] != 820.0 {
		t.Fatalf("defensive scenario=%#v, want the $820 target-low anchor", result.Defensive)
	}
}

func TestSpecializedIndustryPrecedesMissingForwardEPSFallback(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "internet-content-information", LongBusinessSummary: "Builds a semiconductor platform.",
		GrowthRate: ptr(.30), PriceToSales: ptr(12), Revenue: ptr(100), SharesOutstanding: ptr(10),
	})
	if result.Method != "互联网平台：用户质量 + 远期 P/S" || !result.IndustryModel {
		t.Fatalf("specialized model was replaced by missing-forward fallback: %#v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "缺乏分析师远期 EPS") {
		t.Fatalf("missing-forward-EPS warning not preserved: %#v", result.Warnings)
	}
}

func TestSpecializedIndustryDefenseSignalDoesNotChangePublicValuation(t *testing.T) {
	base := Fundamentals{
		IndustryKey: "internet-content-information", LongBusinessSummary: "Builds a semiconductor platform.",
		NormalizedEPS: ptr(2), ForwardEPS: ptr(2), ForwardEPSObserved: true,
		CurrentPrice: ptr(100), Revenue: ptr(1000), SharesOutstanding: ptr(10), PlatformUsers: ptr(1000), ARPU: ptr(2),
	}
	withoutDefense := Calculate(base)
	base.DefenseRevenueGrowth = ptr(.30)
	withDefense := Calculate(base)
	if withDefense.Method != "互联网平台：用户质量 + 远期 P/S" || !withDefense.IndustryModel {
		t.Fatalf("specialized model was replaced by defense transition: %#v", withDefense)
	}
	if withDefense.Methods["互联网平台远期 P/S"] != 550 || withDefense.Methods["用户价值校验"] != 100 {
		t.Fatalf("internet platform candidates=%#v, want 550 and 100", withDefense.Methods)
	}
	if !reflect.DeepEqual(withDefense, withoutDefense) {
		t.Fatalf("defense signal changed specialized public valuation: without=%#v with=%#v", withoutDefense, withDefense)
	}
}

func TestLifecycleFlagConflictReferenceProbes(t *testing.T) {
	base := Fundamentals{
		Sector: "Technology", Industry: "Semiconductors", IndustryKey: "internet-content-information",
		CurrentPrice: ptr(100), MarketCap: ptr(1e12), MarketCapDataQuality: "consistent",
		SharesOutstanding: ptr(1e10), Revenue: ptr(1e11), FreeCashFlow: ptr(3e10),
		PlatformUsers: ptr(1000), ARPU: ptr(2),
		NormalizedEPS: ptr(2), ForwardEPS: ptr(20), ForwardEPSObserved: true,
		GrowthRate: ptr(.30), RevenueGrowth: ptr(.30), AIRevenueShare: ptr(.60),
		AnnualEPS: []float64{2, 2, 2}, RequiredReturn: ptr(.095), RiskFreeRate: ptr(.04),
	}
	for _, test := range []struct {
		name string
		f    Fundamentals
		want string
	}{
		{name: "specialized internet + memory + winter", f: func() Fundamentals {
			f := base
			f.LongBusinessSummary = "Online platform provides semiconductor NAND memory storage hardware."
			f.Regime = RegimeWinter
			return f
		}(), want: `{"fair_value":55,"fair_value_low":30,"fair_value_high":80,"fair_value_buy_low":18,"fair_value_buy_high":48,"risk_free_rate":0.04,"required_return":0.095,"method":"互联网平台：用户质量 + 远期 P/S","methods":{"互联网平台远期 P/S":55,"用户价值校验":0},"regime":"WINTER","model_under_regime":"互联网平台：用户质量 + 远期 P/S (WINTER)","defensive":{"buy_high":40.26,"buy_low":25.74,"high":67.1,"low":42.9,"model":"互联网平台：用户质量 + 远期 P/S 压力测试","safety_margin":0.4,"value":55},"optimistic":{"buy_high":48,"buy_low":18,"forward_eps":12,"growth_rate":0.3,"high":80,"low":30,"model":"互联网平台：用户质量 + 远期 P/S","safety_margin":0.4,"value":55},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","forward_eps":12,"safety_margin":0.4,"confidence":0.7,"confidence_label":"medium","status":"ready","warning":"行业专用估值模型已启用，仍需结合行业指标与安全边际判断","warnings":["行业专用估值模型已启用，仍需结合行业指标与安全边际判断"]}`},
		{name: "specialized internet + chip + defense + mature", f: func() Fundamentals {
			f := base
			f.Industry = "Internet Services"
			f.LongBusinessSummary = "Online platform builds semiconductor chips."
			f.DefenseRevenueGrowth = ptr(.35)
			return f
		}(), want: `{"fair_value":55,"fair_value_low":30,"fair_value_high":80,"fair_value_buy_low":18,"fair_value_buy_high":48,"risk_free_rate":0.04,"required_return":0.095,"method":"互联网平台：用户质量 + 远期 P/S","methods":{"互联网平台远期 P/S":55,"用户价值校验":0},"regime":"NORMAL","model_under_regime":"互联网平台：用户质量 + 远期 P/S","defensive":{"buy_high":40.26,"buy_low":25.74,"high":67.1,"low":42.9,"model":"互联网平台：用户质量 + 远期 P/S 压力测试","safety_margin":0.4,"value":55},"optimistic":{"buy_high":48,"buy_low":18,"forward_eps":12,"growth_rate":0.3,"high":80,"low":30,"model":"互联网平台：用户质量 + 远期 P/S","safety_margin":0.4,"value":55},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","forward_eps":12,"safety_margin":0.4,"confidence":0.7,"confidence_label":"medium","status":"ready","warning":"行业专用估值模型已启用，仍需结合行业指标与安全边际判断","warnings":["行业专用估值模型已启用，仍需结合行业指标与安全边际判断"]}`},
		{name: "memory + defense, normal", f: func() Fundamentals {
			f := base
			f.Industry = "Semiconductor Memory"
			f.IndustryKey = "semiconductor-memory"
			f.LongBusinessSummary = "Defense contractor producing memory chips and NAND storage."
			f.DefenseRevenueGrowth = ptr(.35)
			return f
		}(), want: `{"fair_value":210,"fair_value_low":168,"fair_value_high":252,"fair_value_buy_low":100.8,"fair_value_buy_high":151.2,"risk_free_rate":0.04,"required_return":0.095,"method":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","methods":{"周期调整远期 EPS × PE":210},"regime":"NORMAL","model_under_regime":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE","defensive":{"buy_high":126,"buy_low":100.8,"high":210,"low":168,"model":"AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE 压力测试","safety_margin":0.4,"value":189},"optimistic":{"buy_high":216,"buy_low":144,"forward_eps":12,"growth_rate":0.3,"high":360,"low":240,"model":"AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE","safety_margin":0.4,"value":300},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","forward_eps":12,"safety_margin":0.4,"confidence":0.7,"confidence_label":"medium","status":"ready","warning":"估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动","warnings":["AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际","估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动"]}`},
		{name: "memory + defense, winter", f: func() Fundamentals {
			f := base
			f.Industry = "Semiconductor Memory"
			f.IndustryKey = "semiconductor-memory"
			f.LongBusinessSummary = "Defense contractor producing memory chips and NAND storage."
			f.DefenseRevenueGrowth = ptr(.35)
			f.Regime = RegimeWinter
			return f
		}(), want: `{"fair_value":8,"fair_value_low":6.24,"fair_value_high":9.76,"fair_value_buy_low":2.37,"fair_value_buy_high":3.71,"risk_free_rate":0.04,"required_return":0.095,"method":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","methods":{"AI寒冬周期中值 PE":8},"regime":"WINTER","model_under_regime":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 (WINTER)","defensive":{"buy_high":3.9,"buy_low":2.5,"forward_eps":null,"growth_rate":0.3,"high":9.76,"low":6.24,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 压力测试","safety_margin":0.6,"value":8},"optimistic":{"buy_high":3.84,"buy_low":2.56,"forward_eps":null,"growth_rate":0.3,"high":9.6,"low":6.4,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","safety_margin":0.6,"value":8},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","forward_eps":12,"safety_margin":0.62,"confidence":0.7,"confidence_label":"medium","status":"ready","warning":"估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动","warnings":["市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线","估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动"]}`},
		{name: "chip + winter", f: func() Fundamentals {
			f := base
			f.Industry = "Semiconductors"
			f.IndustryKey = "semiconductors"
			f.LongBusinessSummary = "Semiconductor chip designer."
			f.Regime = RegimeWinter
			return f
		}(), want: `{"fair_value":8,"fair_value_low":6.24,"fair_value_high":9.76,"fair_value_buy_low":2.37,"fair_value_buy_high":3.71,"risk_free_rate":0.04,"required_return":0.095,"method":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","methods":{"AI寒冬周期中值 PE":8},"regime":"WINTER","model_under_regime":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 (WINTER)","defensive":{"buy_high":3.9,"buy_low":2.5,"forward_eps":null,"growth_rate":0.3,"high":9.76,"low":6.24,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 压力测试","safety_margin":0.6,"value":8},"optimistic":{"buy_high":3.84,"buy_low":2.56,"forward_eps":null,"growth_rate":0.3,"high":9.6,"low":6.4,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","safety_margin":0.6,"value":8},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","forward_eps":12,"safety_margin":0.62,"confidence":0.7,"confidence_label":"medium","status":"ready","warnings":["市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线"]}`},
		{name: "energy specialist + memory + missing EPS", f: func() Fundamentals {
			f := base
			f.Industry = "Oil & Gas Integrated"
			f.IndustryKey = "oil-gas-integrated"
			f.LongBusinessSummary = "Oil producer also makes NAND memory storage hardware."
			f.ForwardEPS = nil
			f.ForwardEPSObserved = false
			f.ForwardEBITDA = ptr(2e10)
			return f
		}(), want: `{"fair_value":13,"fair_value_low":10,"fair_value_high":16,"fair_value_buy_low":6.5,"fair_value_buy_high":10.4,"risk_free_rate":0.04,"required_return":0.095,"method":"能源/矿业：有限寿命 NAV + EV/EBITDA","methods":{"EV/EBITDA":13},"regime":"NORMAL","model_under_regime":"能源/矿业：有限寿命 NAV + EV/EBITDA","defensive":{"buy_high":10.31,"buy_low":6.59,"high":15.86,"low":10.14,"model":"能源/矿业：有限寿命 NAV + EV/EBITDA 压力测试","safety_margin":0.35,"value":13},"optimistic":{"buy_high":10.4,"buy_low":6.5,"forward_eps":null,"growth_rate":0.3,"high":16,"low":10,"model":"能源/矿业：有限寿命 NAV + EV/EBITDA","safety_margin":0.35,"value":13},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","safety_margin":0.35,"confidence":0.7,"confidence_label":"medium","status":"ready","warning":"行业专用估值模型已启用，仍需结合行业指标与安全边际判断","warnings":["缺乏分析师远期 EPS，已自动降级为保守模型","行业专用估值模型已启用，仍需结合行业指标与安全边际判断"]}`},
		{name: "storage winter + missing EPS", f: func() Fundamentals {
			f := base
			f.Industry = "Semiconductor Memory"
			f.IndustryKey = "semiconductor-memory"
			f.LongBusinessSummary = "NAND memory chip and storage hardware manufacturer."
			f.ForwardEPS = nil
			f.ForwardEPSObserved = false
			f.Regime = RegimeWinter
			return f
		}(), want: `{"fair_value":8,"fair_value_low":6.24,"fair_value_high":9.76,"fair_value_buy_low":2.37,"fair_value_buy_high":3.71,"risk_free_rate":0.04,"required_return":0.095,"method":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","methods":{"AI寒冬周期中值 PE":8},"regime":"WINTER","model_under_regime":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 (WINTER)","defensive":{"buy_high":3.9,"buy_low":2.5,"forward_eps":null,"growth_rate":0.3,"high":9.76,"low":6.24,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线 压力测试","safety_margin":0.6,"value":8},"optimistic":{"buy_high":3.84,"buy_low":2.56,"forward_eps":null,"growth_rate":0.3,"high":9.6,"low":6.4,"model":"AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线","safety_margin":0.6,"value":8},"data_quality_score":90,"normalized_eps_source":"历史 EPS 中位数","shareholder_total_return_yield":0,"market_cap_data_quality":"consistent","safety_margin":0.62,"confidence":0.7,"confidence_label":"medium","status":"ready","warnings":["缺乏分析师远期 EPS，已自动降级为保守模型","市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(Calculate(test.f))
			if err != nil {
				t.Fatal(err)
			}
			var got any
			var want any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("full public Result mismatch:\n got: %s\nwant: %s", encoded, test.want)
			}
		})
	}
}

func TestDataQualityScorePenalizesAnyTruthySubThresholdMarketCap(t *testing.T) {
	result := Calculate(Fundamentals{
		MarketCap: ptr(-1), Revenue: ptr(100), AnnualRevenue: []float64{100}, AnalystCount: ptr(3),
	})
	if result.DataQualityScore != 90 {
		t.Fatalf("negative market-cap quality score=%v, want 90 per Python truthy/sub-threshold rule", result.DataQualityScore)
	}
}

func TestCalculateMatureGrowthPremiumAndMissingHintFallback(t *testing.T) {
	base := Fundamentals{
		IndustryKey: "software-application", MarketCap: ptr(1.1e12), Revenue: ptr(100e9),
		FreeCashFlow: ptr(30e9), RevenueGrowth: ptr(.09), GrowthRate: ptr(.09),
		GrowthHintObserved: true, NormalizedEPS: ptr(10), ForwardEPS: ptr(12),
		ForwardEPSObserved: true,
	}
	result := Calculate(base)
	if result.Method != "成熟增长型巨头：远期正常化 EPS × PE" || result.Methods["成熟增长 PE"] != 408 {
		t.Fatalf("premium mature-growth valuation=%#v, want 12 EPS x 34 PE", result)
	}
	assertFloat(t, result.FairValueLow, 318.24)
	assertFloat(t, result.FairValueHigh, 497.76)
	assertFloat(t, result.FairValueBuyLow, 254.59)
	assertFloat(t, result.FairValueBuyHigh, 398.21)
	if result.Defensive == nil || result.Defensive["value"] != 368.0 || result.Optimistic == nil || result.Optimistic["low"] != 360.0 || result.Optimistic["high"] != 456.0 {
		t.Fatalf("premium mature-growth scenarios differ from Python PE anchors: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}

	highBeta := base
	highBeta.Beta = ptr(2)
	result = Calculate(highBeta)
	if result.SafetyMargin != .30 || result.Defensive == nil || result.Defensive["safety_margin"] != .30 {
		t.Fatalf("high-beta mature-growth margin=%v defensive=%#v, want 30%%", result.SafetyMargin, result.Defensive)
	}

	missingHint := base
	missingHint.RevenueGrowth = nil
	missingHint.GrowthRate = ptr(.02) // Derived from history, not an observed Yahoo growth hint.
	missingHint.GrowthHintObserved = false
	if result := Calculate(missingHint); result.Method != "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("$1T company without a reported growth hint should use the mature-growth fallback: %#v", result)
	}

	reportedSlowGrowth := missingHint
	reportedSlowGrowth.GrowthHintObserved = true
	if result := Calculate(reportedSlowGrowth); result.Method == "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("reported sub-6%% growth incorrectly used the missing-hint fallback: %#v", result)
	}
}

func TestV39SNDKDecisionTreeAndModelDiagnostics(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(1719.11), Sector: "Technology", Industry: "Computer Hardware Storage",
		TrailingEPS: ptr(100), NormalizedEPS: ptr(90), NormalizedEPSSource: "历史 EPS 中位数",
		AnnualEPS: []float64{100, 90, 80, 70, 60}, ForwardEPS: ptr(527), ForwardEPSObserved: true,
		GrowthRate: ptr(.5), GrowthHintObserved: true, Beta: ptr(1.2), Revenue: ptr(1000),
		FreeCashFlow: ptr(250), OperatingCashflow: ptr(250), OperatingIncome: ptr(300),
		NetIncome: ptr(200), DepreciationAndAmortization: ptr(50), CapitalExpenditure: ptr(50),
		MarketCap: ptr(10e9), PriceToSales: ptr(12), TargetMeanPrice: ptr(2137),
		RiskFreeRate: ptr(.04), RequiredReturn: ptr(.106), Regime: RegimeNormal,
	})
	if result.NormalizedEPS == nil || *result.NormalizedEPS != 90 {
		t.Fatalf("normalized EPS=%v, want 90", result.NormalizedEPS)
	}
	if result.ForwardEPS == nil || math.Abs(*result.ForwardEPS-61.0571) > .0001 || result.ForwardEPSSource != "Yahoo远期EPS" {
		t.Fatalf("forward EPS/source=%v/%q, want 61.0571/Yahoo远期EPS", result.ForwardEPS, result.ForwardEPSSource)
	}
	assertFloat(t, result.PELow, 20)
	assertFloat(t, result.PEHigh, 30)
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "普通行业" ||
		result.DecisionTree.Lifecycle != "AI存储/硬件周期" || result.DecisionTree.Model != result.Method {
		t.Fatalf("decision tree=%#v, want ordinary industry / AI storage cycle / selected model", result.DecisionTree)
	}
	if len(result.Validation.Methods) != 0 || result.Validation.Median != nil {
		t.Fatalf("validation=%#v, want empty methods and null median without shares", result.Validation)
	}
}

func TestV39CrossValidationBuildsPerShareCandidatesAndMedian(t *testing.T) {
	result := Calculate(Fundamentals{
		SharesOutstanding: ptr(10), FreeCashFlow: ptr(100), OwnerEarnings: ptr(80),
		Debt: ptr(20), Cash: ptr(10), OperatingIncome: ptr(50), RequiredReturn: ptr(.10),
	})
	want := map[string]float64{
		"所有者收益":   144,
		"FCF收益率":  166.67,
		"EV/EBIT": 74,
	}
	for key, value := range want {
		if result.Validation.Methods[key] != value {
			t.Errorf("validation method %q=%v, want %v", key, result.Validation.Methods[key], value)
		}
	}
	if _, ok := result.Validation.Methods["DCF"]; !ok {
		t.Fatalf("validation methods=%#v, missing DCF", result.Validation.Methods)
	}
	assertFloat(t, result.Validation.Median, 144)
}

func TestCalculateGenericHighGrowthUsesPythonPEGAndScenarios(t *testing.T) {
	base := Fundamentals{
		CurrentPrice: ptr(200), SharesOutstanding: ptr(1e9), IndustryKey: "software-application",
		Industry: "Software Infrastructure", Sector: "Technology", MarketCap: ptr(200e9), MarketCapDataQuality: "consistent",
		Revenue: ptr(100e9), FreeCashFlow: ptr(10e9), NormalizedEPS: ptr(8), ForwardEPS: ptr(10),
		ForwardEPSObserved: true, GrowthRate: ptr(.30), RequiredReturn: ptr(.095), TerminalGrowth: ptr(.035),
	}
	result := Calculate(base)
	if result.Method != "Forward EPS × PEG" {
		t.Fatalf("qualified generic growth company selected %q, want Python Forward PEG", result.Method)
	}
	if result.Methods["Forward PEG"] != 315 {
		t.Fatalf("Forward PEG candidate=%v, want 10 EPS at 26.5-36.5x midpoint", result.Methods["Forward PEG"])
	}
	assertFloat(t, result.FairValue, 315)
	assertFloat(t, result.FairValueLow, 245.7)
	assertFloat(t, result.FairValueHigh, 384.3)
	if result.SafetyMargin != .45 {
		t.Fatalf("generic high-growth safety margin=%v, want 45%%", result.SafetyMargin)
	}
	if result.Optimistic == nil || result.Optimistic["value"] != 450.0 || result.Optimistic["low"] != 369.0 || result.Optimistic["high"] != 531.0 || result.Optimistic["safety_margin"] != .48 {
		t.Fatalf("generic high-growth optimistic scenario=%#v", result.Optimistic)
	}
	if result.Optimistic["forward_eps"] != 10.0 || result.Optimistic["growth_rate"] != .3 {
		t.Fatalf("generic high-growth optimistic inputs=%#v", result.Optimistic)
	}
	if result.Defensive == nil || result.Defensive["value"] != 166.67 || result.Defensive["safety_margin"] != .45 {
		t.Fatalf("generic high-growth defensive scenario=%#v", result.Defensive)
	}
}

func TestCalculateGenericHighGrowthRequiresGrowthAndNonCyclicalIndustry(t *testing.T) {
	base := Fundamentals{
		IndustryKey: "software-application", Industry: "Software Infrastructure", Sector: "Technology",
		ForwardEPS: ptr(10), ForwardEPSObserved: true, NormalizedEPS: ptr(8), GrowthRate: ptr(.17),
	}
	if result := Calculate(base); result.Method == "Forward EPS × PEG" {
		t.Fatalf("17%% growth incorrectly selected Forward PEG: %#v", result)
	}

	cyclical := base
	cyclical.IndustryKey = ""
	cyclical.Industry = "Airlines"
	cyclical.GrowthRate = ptr(.30)
	if result := Calculate(cyclical); result.Method == "Forward EPS × PEG" {
		t.Fatalf("cyclical airline incorrectly selected Forward PEG: %#v", result)
	}
}

func TestCalculateUsesTrimmedHistoricalEPSToValueCyclicalCompanies(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Shipping", Sector: "Industrials", TrailingEPS: ptr(4), NormalizedEPS: ptr(5),
		AnnualEPS: []float64{3, 6}, ForwardEPS: ptr(30), ForwardEPSObserved: true, GrowthRate: ptr(.30),
	})
	if result.Method != "周期中值 EPS × PE" {
		t.Fatalf("cyclical company method=%q, want cycle EPS model", result.Method)
	}
	if result.Methods["周期中值PE"] != 50 {
		t.Fatalf("cycle EPS candidate=%v, want 4 EPS × 12.5 PE", result.Methods["周期中值PE"])
	}
	assertFloat(t, result.FairValue, 50)
	assertFloat(t, result.FairValueLow, 40)
	assertFloat(t, result.FairValueHigh, 60)
	if result.SafetyMargin != .62 {
		t.Fatalf("cyclical safety margin=%v, want 62%%", result.SafetyMargin)
	}
	defensiveValue, valueOK := result.Defensive["value"].(float64)
	defensiveModel, modelOK := result.Defensive["model"].(string)
	if !valueOK || defensiveValue != 52 || !modelOK || defensiveModel != "周期中值 EPS × PE 压力测试" {
		t.Fatalf("cyclical defensive scenario=%#v, want cycle EPS 4 × defensive PE 13", result.Defensive)
	}
	if result.Optimistic != nil {
		t.Fatalf("ordinary cyclical company should not have a forward optimistic scenario: %#v", result.Optimistic)
	}
	if _, ok := result.Methods["Forward PEG"]; ok {
		t.Fatalf("cyclical peak must not use PEG: %#v", result.Methods)
	}
}

func TestCalculateCyclicalDefensiveIncludesOwnerEarningsCandidate(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Shipping", Sector: "Industrials", TrailingEPS: ptr(4), NormalizedEPS: ptr(5),
		AnnualEPS: []float64{3, 6}, SharesOutstanding: ptr(10), OwnerEarnings: ptr(80),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	defensiveValue, ok := result.Defensive["value"].(float64)
	if !ok || defensiveValue != 100 {
		t.Fatalf("cyclical defensive value=%#v, want owner earnings candidate median 100", result.Defensive)
	}
}

func TestCalculateCyclicalDefensiveUsesOwnerEarningsAndDCFMedian(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Shipping", Sector: "Industrials", TrailingEPS: ptr(4), NormalizedEPS: ptr(5),
		AnnualEPS: []float64{3, 6}, ForwardEPS: ptr(30), ForwardEPSObserved: true, GrowthRate: ptr(.30),
		SharesOutstanding: ptr(10), OwnerEarnings: ptr(80), FreeCashFlowPerShare: ptr(.10),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	defensiveValue, ok := result.Defensive["value"].(float64)
	if result.Defensive == nil || !ok || defensiveValue != 52 {
		t.Fatalf("cyclical defensive median=%#v, want the median of DCF, cycle EPS x 13, and owner earnings", result.Defensive)
	}
	if result.Optimistic != nil {
		t.Fatalf("ordinary cyclical company should not have a forward optimistic scenario: %#v", result.Optimistic)
	}
}

func TestCalculateCyclicalEPSFallsBackToNormalizedEPSWithShortHistory(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Shipping", Sector: "Industrials", TrailingEPS: ptr(4), NormalizedEPS: ptr(5),
		AnnualEPS: []float64{3}, ForwardEPS: ptr(20), ForwardEPSObserved: true, GrowthRate: ptr(.30),
	})
	if result.Method != "周期中值 EPS × PE" || result.Methods["周期中值PE"] != 62.5 {
		t.Fatalf("short-history cyclical model=%#v, want normalized EPS 5 × 12.5 PE", result)
	}
	if _, ok := result.Methods["Forward PEG"]; ok {
		t.Fatalf("cyclical peak must not use PEG: %#v", result.Methods)
	}
}

func TestCyclicalEPSUsesOnlyFiveAnnualObservations(t *testing.T) {
	got := cyclicalEPS(Fundamentals{AnnualEPS: []float64{1, 2, 3, 4, 5, 100}}, ptr(9))
	if got == nil || *got != 3 {
		t.Fatalf("cycle EPS=%v, want median 3 after limiting annual history to five values", got)
	}
}

func TestCalculateGenericHighGrowthCanUseModeledForwardEPS(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "software-application", Industry: "Software Infrastructure", Sector: "Technology",
		NormalizedEPS: ptr(8), GrowthRate: ptr(.30),
	})
	if result.Method != "Forward EPS × PEG" {
		t.Fatalf("missing analyst EPS should use modeled forward EPS on generic growth branch: %#v", result)
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 10.4 || result.Methods["Forward PEG"] != 327.6 {
		t.Fatalf("modeled forward EPS result=%#v", result)
	}
}

func TestCalculateDerivesGrowthInPythonV39FallbackOrder(t *testing.T) {
	base := Fundamentals{
		CurrentPrice: ptr(100), SharesOutstanding: ptr(10), IndustryKey: "software-application",
		Industry: "Software Infrastructure", Sector: "Technology", Revenue: ptr(100),
		NormalizedEPS: ptr(10), FreeCashFlow: ptr(20), OperatingCashflow: ptr(30), CapitalExpenditure: ptr(10),
	}
	for _, test := range []struct {
		name           string
		fundamentals   Fundamentals
		wantValue      float64
		wantForwardEPS float64
		wantPELow      float64
		wantPEHigh     float64
	}{
		{
			name: "reported revenue growth precedes histories",
			fundamentals: func() Fundamentals {
				f := base
				f.RevenueGrowth = ptr(.30)
				f.AnnualEPS = []float64{12, 10, 8}
				return f
			}(),
			wantValue: 409.5, wantForwardEPS: 13, wantPELow: 26.5, wantPEHigh: 36.5,
		},
		{
			name: "annual EPS CAGR precedes revenue CAGR",
			fundamentals: func() Fundamentals {
				f := base
				f.AnnualEPS = []float64{12, 10, 8}
				f.AnnualRevenue = []float64{140, 100, 80}
				return f
			}(),
			wantValue: 289.02, wantForwardEPS: 12.2474, wantPELow: 18.6, wantPEHigh: 28.6,
		},
		{
			name: "annual revenue CAGR fallback",
			fundamentals: func() Fundamentals {
				f := base
				f.AnnualRevenue = []float64{120, 100, 80}
				return f
			}(),
			wantValue: 289.02, wantForwardEPS: 12.2474, wantPELow: 18.6, wantPEHigh: 28.6,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.fundamentals)
			if result.Method != "Forward EPS × PEG" {
				t.Fatalf("method=%q, want Python v39 Forward PEG; result=%#v", result.Method, result)
			}
			assertFloat(t, result.FairValue, test.wantValue)
			if result.ForwardEPS == nil || math.Abs(*result.ForwardEPS-test.wantForwardEPS) > .0001 {
				t.Fatalf("modeled forward EPS=%v, want %.4f", result.ForwardEPS, test.wantForwardEPS)
			}
			assertFloat(t, result.PELow, test.wantPELow)
			assertFloat(t, result.PEHigh, test.wantPEHigh)
			if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "普通" {
				t.Fatalf("decision tree=%#v, want ordinary lifecycle", result.DecisionTree)
			}
		})
	}
}

func TestCalculateGenericHighGrowthCapsExtremeOptimisticScenario(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "software-application", Industry: "Software Infrastructure", Sector: "Technology",
		NormalizedEPS: ptr(2), ForwardEPS: ptr(10), ForwardEPSObserved: true, GrowthRate: ptr(.50),
	})
	if result.Method != "Forward EPS × PEG" {
		t.Fatalf("high-growth company selected %q", result.Method)
	}
	if result.Optimistic == nil || result.Optimistic["model"] != "混合估值锚点：共识 + 基本面交叉验证" || result.Optimistic["value"] != 96.0 || result.Optimistic["safety_margin"] != .35 {
		t.Fatalf("extreme optimistic scenario was not capped against the defensive anchor: %#v", result.Optimistic)
	}
	if !strings.Contains(result.Warning, "混合估值锚点") {
		t.Fatalf("missing hybrid-anchor warning: %q", result.Warning)
	}
}

func TestStableCashflowCompanyUsesPythonEPSAnchorsAndScenarios(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Defensive", Industry: "Beverages", CurrentPrice: ptr(100),
		TrailingEPS: ptr(3), NormalizedEPS: ptr(4), ForwardEPS: ptr(5), GrowthRate: ptr(.08),
		Revenue: ptr(100), FreeCashFlow: ptr(20), SharesOutstanding: ptr(10),
	})
	if result.Method != "稳定现金流：正常化 EPS × 20–35 倍 PE" {
		t.Fatalf("stable company model=%q", result.Method)
	}
	assertFloat(t, result.FairValue, 137.5)
	assertFloat(t, result.FairValueLow, 116.88)
	assertFloat(t, result.FairValueHigh, 158.12)
	if result.Defensive == nil || result.Defensive["value"] != 96.0 || result.Defensive["safety_margin"] != .20 {
		t.Fatalf("stable defensive scenario=%#v", result.Defensive)
	}
	if result.Optimistic == nil || result.Optimistic["value"] != 110.0 || result.Optimistic["safety_margin"] != .30 {
		t.Fatalf("stable optimistic scenario=%#v", result.Optimistic)
	}
}

func TestDCFDerivesBoundedTerminalGrowthFromCompanyGrowth(t *testing.T) {
	cases := []struct {
		name   string
		growth float64
		want   float64
	}{
		{"negative growth floor", -.15, .015},
		{"zero growth floor", 0, .015},
		{"moderate growth", .08, .02},
		{"high growth cap", .50, .035},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(Fundamentals{
				FreeCashFlowPerShare: ptr(10), GrowthRate: ptr(test.growth),
				RequiredReturn: ptr(.10),
			})
			want := dcfPerShare(10, clamp(test.growth, -.02, .14), .10, test.want)
			if got, ok := result.Methods[MethodFCFDCF]; !ok || math.Abs(got-roundCurrency(want)) > 1e-9 {
				t.Fatalf("DCF=%v, want growth-derived terminal growth %.4f result %.2f", got, test.want, roundCurrency(want))
			}
			if !strings.Contains(strings.Join(result.Warnings, " "), "derived from growth rate") {
				t.Fatalf("missing derived terminal-growth disclosure: %#v", result.Warnings)
			}
		})
	}
}

func TestMissingGrowthUsesPythonTerminalGrowthFloor(t *testing.T) {
	result := Calculate(Fundamentals{
		FreeCashFlowPerShare: ptr(10), RequiredReturn: ptr(.10),
	})
	want := dcfPerShare(10, 0, .10, .015)
	if got, ok := result.Methods[MethodFCFDCF]; !ok || math.Abs(got-roundCurrency(want)) > 1e-9 {
		t.Fatalf("missing growth should use zero near-term and 1.5%% terminal growth: got %v want %.2f", got, roundCurrency(want))
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "terminal growth defaulted to 1.5%") {
		t.Fatalf("missing-growth terminal floor should be documented: %#v", result.Warnings)
	}
}

func TestWinterFloorEPSMatchesPythonWindowAndFallback(t *testing.T) {
	if got := winterFloorEPS(Fundamentals{AnnualEPS: []float64{20, 18, 10, 8, 6, 3, 2, 1}}, 99); math.Abs(got-11.0/3) > 1e-12 {
		t.Fatalf("winter floor=%v, want mean of lowest three values after Python seven-year/two-peak filter", got)
	}
	if got := winterFloorEPS(Fundamentals{AnnualEPS: []float64{6, 2}}, 99); got != 4 {
		t.Fatalf("short annual history floor=%v, want mean of available annual EPS without fallback mixing", got)
	}
	if got := winterFloorEPS(Fundamentals{}, 7); got != 7 {
		t.Fatalf("empty annual history floor=%v, want caller fallback 7", got)
	}
}

func TestAIWinterAppliesRevenueShareDefaultsAndDiscounts(t *testing.T) {
	annual := []float64{1, 2, 3, 4, 5, 6, 7}
	cases := []struct {
		name string
		f    Fundamentals
		want float64
	}{
		{
			name: "AI chip default share",
			f: Fundamentals{
				Sector: "Technology", Industry: "Semiconductors", Regime: RegimeWinter,
				AnnualEPS: annual, NormalizedEPS: ptr(5), ForwardEPS: ptr(30), ForwardEPSObserved: true,
				GrowthRate: ptr(.50), PriceToSales: ptr(12), Beta: ptr(1.5),
			},
			want: 8,
		},
		{
			name: "semiconductor default share",
			f:    Fundamentals{Sector: "Technology", Industry: "Semiconductors", Regime: RegimeWinter, AnnualEPS: annual, GrowthRate: ptr(.05), PriceToSales: ptr(5)},
			want: 8,
		},
		{
			name: "storage default share",
			f:    Fundamentals{Sector: "Technology", Industry: "Computer Hardware Storage", Regime: RegimeWinter, AnnualEPS: annual},
			want: 16,
		},
		{
			name: "percentage share input",
			f:    Fundamentals{Sector: "Technology", Industry: "Software", Regime: RegimeWinter, AnnualEPS: annual, AIRevenueShare: ptr(40)},
			want: 16,
		},
		{
			name: "ratio share input at medium threshold",
			f:    Fundamentals{Sector: "Technology", Industry: "Software", Regime: RegimeWinter, AnnualEPS: annual, AIRevenueShare: ptr(.10)},
			want: 16,
		},
		{
			name: "low share has no discount",
			f:    Fundamentals{Sector: "Technology", Industry: "Software", Regime: RegimeWinter, AnnualEPS: annual, AIRevenueShare: ptr(.05)},
			want: 20,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.f)
			if !strings.Contains(result.Method, "AI寒冬") {
				t.Fatalf("model=%q, want AI winter model", result.Method)
			}
			assertFloat(t, result.FairValue, test.want)
			assertFloat(t, floatPointer(result.Methods["AI寒冬周期中值 PE"]), test.want)
			if result.Methods["寒冬 P/B 底线"] != 0 || result.Methods["寒冬净现金保护"] != 0 {
				t.Fatalf("unexpected winter candidates: %#v", result.Methods)
			}
			if result.Defensive["low"] != roundCurrency(test.want*.78) || result.Defensive["high"] != roundCurrency(test.want*1.22) ||
				result.Optimistic["low"] != roundCurrency(test.want*.80) || result.Optimistic["high"] != roundCurrency(test.want*1.20) {
				t.Fatalf("winter scenarios do not match Python defensive +/-22%% and optimistic +/-20%%: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
			}
			if result.SafetyMargin != .62 {
				t.Fatalf("safety margin=%v, want 0.62 from the winter cyclical baseline", result.SafetyMargin)
			}
		})
	}
}

func TestAIWinterKeepsSpecializedIndustryPriority(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial Services", Industry: "Banks Diversified", Regime: RegimeWinter,
		BookValue: ptr(10), ROE: ptr(.12), AIRevenueShare: ptr(.80),
	})
	if strings.Contains(result.Method, "AI寒冬") || !strings.Contains(result.Method, "银行") {
		t.Fatalf("specialized banking model was overridden by AI winter: %q", result.Method)
	}
}

func TestAIWinterTakesCandidateMedianAndUsesEBITDAOnlyAsFallback(t *testing.T) {
	base := Fundamentals{
		Sector: "Technology", Industry: "Software", Regime: RegimeWinter,
		AIRevenueShare: ptr(.25), AnnualEPS: []float64{1, 2, 3, 4, 5, 6, 7},
	}
	withAnchors := base
	withAnchors.BookValue = ptr(40)
	withAnchors.Cash = ptr(30)
	withAnchors.SharesOutstanding = ptr(3)
	result := Calculate(withAnchors)
	assertFloat(t, result.FairValue, 16)
	if result.Methods["AI寒冬周期中值 PE"] != 16 || result.Methods["寒冬 P/B 底线"] != 32 || result.Methods["寒冬净现金保护"] != 10 {
		t.Fatalf("winter candidates=%#v, want EPS=16, P/B=32, net cash=10", result.Methods)
	}

	fallback := Fundamentals{
		Sector: "Technology", Industry: "Software", Regime: RegimeWinter,
		AIRevenueShare: ptr(.25), SharesOutstanding: ptr(10), EBITDA: ptr(100),
	}
	result = Calculate(fallback)
	assertFloat(t, result.FairValue, 60)
	if result.Methods["寒冬 EV/EBITDA 兜底"] != 60 || len(result.Methods) != 1 {
		t.Fatalf("winter EBITDA should be the sole fallback candidate: %#v", result.Methods)
	}
}

func TestAIWinterRaisesSafetyMarginForHighBeta(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software", Regime: RegimeWinter,
		AIRevenueShare: ptr(.80), AnnualEPS: []float64{1, 2, 3, 4, 5}, Beta: ptr(1.6),
	})
	if result.SafetyMargin != .67 {
		t.Fatalf("high-beta AI winter safety margin=%v, want 0.67", result.SafetyMargin)
	}
}

func TestExplicitTerminalGrowthOverridesGrowthDerivedDefault(t *testing.T) {
	result := Calculate(Fundamentals{
		FreeCashFlowPerShare: ptr(10), GrowthRate: ptr(.50),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	want := dcfPerShare(10, .14, .10, .02)
	if got, ok := result.Methods[MethodFCFDCF]; !ok || math.Abs(got-roundCurrency(want)) > 1e-9 {
		t.Fatalf("explicit terminal growth was not honored: got %v want %.2f", got, roundCurrency(want))
	}
	if strings.Contains(strings.Join(result.Warnings, " "), "derived from growth rate") {
		t.Fatalf("explicit terminal-growth input should not be reported as derived: %#v", result.Warnings)
	}
}

func TestCalculateFallsBackToFCFDCF(t *testing.T) {
	result := Calculate(Fundamentals{FreeCashFlowPerShare: ptr(10), GrowthRate: ptr(0.05), RequiredReturn: ptr(0.10), TerminalGrowth: ptr(0.02), SafetyMargin: ptr(0.20)})
	want := 0.0
	for year := 1; year <= 5; year++ {
		want += 10 * math.Pow(1.05, float64(year)) / math.Pow(1.10, float64(year))
	}
	want += 10 * math.Pow(1.05, 5) * 1.02 / 0.08 / math.Pow(1.10, 5)
	want = math.Round(want*100) / 100
	if result.Status != StatusReady || result.Method != MethodFCFDCF {
		t.Fatalf("status/method = %q/%q, want ready/%q", result.Status, result.Method, MethodFCFDCF)
	}
	assertFloat(t, result.FairValue, want)
	assertFloat(t, result.FairValueLow, math.Round(want*0.8*100)/100)
	assertFloat(t, result.FairValueHigh, math.Round(want*1.2*100)/100)
	if !strings.Contains(result.Warning, "used FCF DCF") {
		t.Fatalf("warning = %q, want DCF fallback warning", result.Warning)
	}
	if result.ConfidenceLabel != "medium" || result.Confidence != 0.60 {
		t.Fatalf("confidence = %v/%q, want 0.60/medium", result.Confidence, result.ConfidenceLabel)
	}
}

func TestCalculateUnavailableWhenNoPositiveCandidate(t *testing.T) {
	result := Calculate(Fundamentals{OwnerEarnings: ptr(-10), ForwardEPS: ptr(-2), FreeCashFlow: ptr(-100), SharesOutstanding: ptr(10)})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", result.Status, StatusUnavailable)
	}
	if result.FairValue != nil || result.FairValueLow != nil || result.FairValueBuyHigh != nil {
		t.Fatalf("unavailable result contains values: %#v", result)
	}
	if result.Confidence != 0 || result.ConfidenceLabel != "low" || result.Warning == "" {
		t.Fatalf("unavailable metadata = confidence %v/%q warning %q", result.Confidence, result.ConfidenceLabel, result.Warning)
	}
}

func TestCalculateDoesNotApplyCompanyModelsToETF(t *testing.T) {
	result := Calculate(Fundamentals{
		InstrumentType: "etf", CurrentPrice: ptr(751.27), SharesOutstanding: ptr(1e9),
		TrailingEPS: ptr(25), NormalizedEPS: ptr(24), ForwardEPS: ptr(30),
		FreeCashFlow: ptr(1e12), RevenueGrowth: ptr(.25), GrowthRate: ptr(.30),
	})
	if result.Status != StatusUnavailable || result.FairValue != nil || result.Defensive != nil || result.Optimistic != nil {
		t.Fatalf("ETF received company valuation: %#v", result)
	}
	if !strings.Contains(result.Warning, "not an operating company") {
		t.Fatalf("ETF warning = %q, want clear unsupported-model explanation", result.Warning)
	}
}

func TestValidateRejectsNonFiniteAndInvalidRanges(t *testing.T) {
	fundamentals := Fundamentals{CurrentPrice: ptr(math.NaN()), SharesOutstanding: ptr(0), MaintenanceCapexRatio: ptr(1.5), ForwardPELow: ptr(30), ForwardPEHigh: ptr(20), SafetyMargin: ptr(1), RequiredReturn: ptr(-0.01)}
	err := fundamentals.Validate()
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T %v, want *ValidationError", err, err)
	}
	if len(validationErr.Problems) < 5 {
		t.Fatalf("validation problems = %#v, want all invalid fields", validationErr.Problems)
	}
	result := Calculate(fundamentals)
	if result.Status != StatusInvalid || len(result.Errors) < 5 {
		t.Fatalf("invalid result = %#v, want invalid status and errors", result)
	}
	if !strings.Contains(result.Warning, "current_price must be finite") {
		t.Fatalf("warning = %q, missing finite-value diagnostic", result.Warning)
	}
}

func TestValidateAllowsMissingFallbackInputs(t *testing.T) {
	if err := (Fundamentals{}).Validate(); err != nil {
		t.Fatalf("missing optional fundamentals rejected: %v", err)
	}
	result := Calculate(Fundamentals{})
	if result.Status != StatusUnavailable {
		t.Fatalf("empty fundamentals status = %q, want %q", result.Status, StatusUnavailable)
	}
}

func assertFloat(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		if got == nil {
			t.Errorf("value = <nil>, want %.2f", want)
		} else {
			t.Errorf("value = %.10f, want %.10f", *got, want)
		}
	}
}

func TestCalculateClassifiesRecognizedIndustryWithoutIndustryKey(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Banks - Regional", Sector: "Financial Services", BookValue: ptr(20),
		ROE: ptr(.15), SharesOutstanding: ptr(10), GrowthRate: ptr(.25),
		ForwardEPS: ptr(5), CurrentPrice: ptr(20),
	})
	if result.Status != StatusReady || result.Method != "银行：P/B-ROE + 剩余收益 RI" {
		t.Fatalf("recognized sector/industry should select the banking model without industryKey: %#v", result)
	}
	if _, ok := result.Methods[MethodForwardPE]; ok {
		t.Fatalf("banking valuation must not include generic forward PE: %#v", result.Methods)
	}
	if _, ok := result.Methods[MethodFCFDCF]; ok {
		t.Fatalf("banking valuation must not include generic DCF: %#v", result.Methods)
	}
}

func TestCalculateBankingScenarioMatchesPythonV39(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial Services", Industry: "Banks - Diversified", IndustryKey: "banks-diversified",
		BookValue: ptr(100), ROE: ptr(10), Beta: ptr(1), RiskFreeRate: ptr(.04), RequiredReturn: ptr(.095),
	})
	if result.Method != "银行：P/B-ROE + 剩余收益 RI" || result.DecisionTree == nil ||
		result.DecisionTree.IndustryFilter != "banking" || result.DecisionTree.Lifecycle != "banking" {
		t.Fatalf("bank classification differs from Python v39: method=%q tree=%#v", result.Method, result.DecisionTree)
	}
	assertFloat(t, result.FairValue, 100)
	assertFloat(t, result.FairValueLow, 80)
	assertFloat(t, result.FairValueHigh, 120)
	wantDefensive := map[string]any{
		"value": 100.0, "low": 78.0, "high": 122.0, "buy_low": 58.5, "buy_high": 91.5,
		"model": "银行：P/B-ROE + 剩余收益 RI 压力测试", "safety_margin": .25,
	}
	wantOptimistic := map[string]any{
		"value": 100.0, "low": 80.0, "high": 120.0, "buy_low": 60.0, "buy_high": 90.0,
		"model": "银行：P/B-ROE + 剩余收益 RI", "safety_margin": .25,
		"forward_eps": nil, "growth_rate": 0.0,
	}
	if !reflect.DeepEqual(result.Defensive, wantDefensive) || !reflect.DeepEqual(result.Optimistic, wantOptimistic) {
		t.Fatalf("bank scenarios differ from Python v39 golden: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
}

func TestCalculateUnknownIndustryUsesGenericFallback(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Specialty Components", Sector: "Industrials",
		ForwardEPS: ptr(5), CurrentPrice: ptr(20),
	})
	if result.Status != StatusReady || result.Method != MethodForwardPE {
		t.Fatalf("unrecognized industry should retain generic fallback: %#v", result)
	}
}

func TestCalculateHealthcarePlansUsesHealthcareEPSMultipleInsteadOfInsuranceEV(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(431.87), Sector: "Healthcare", Industry: "Health Care Plans",
		IndustryKey: "healthcare-plans", LongBusinessSummary: "Offers health insurance plans and Medicare services.",
		NormalizedEPS: ptr(20.5999), ForwardEPS: ptr(16.9657), ForwardEPSObserved: true,
		GrowthRate: ptr(.271),
	})
	if result.Status != StatusReady || result.Method != "医疗保险计划：正常化 EPS × PE" {
		t.Fatalf("healthcare plan valuation=%#v, want healthcare earnings model", result)
	}
	if result.FairValue == nil || math.Abs(*result.FairValue-411.998) > .01 {
		t.Fatalf("fair value=%v, want normalized EPS × 20 PE near 412", result.FairValue)
	}
	if result.Defensive["low"] != 329.6 || result.Defensive["high"] != 391.4 {
		t.Fatalf("defensive range=%#v, want normalized earnings range 329.60-391.40", result.Defensive)
	}
	if result.Optimistic["low"] != 391.4 || result.Optimistic["high"] != 494.4 {
		t.Fatalf("optimistic range=%#v, want normalized earnings range 391.40-494.40", result.Optimistic)
	}
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "healthcare_plans" || result.DecisionTree.Lifecycle != "healthcare_plans" {
		t.Fatalf("healthcare plan classification=%#v, want healthcare_plans", result.DecisionTree)
	}
	if _, ok := result.Methods["保险内含价值 + NBV"]; ok {
		t.Fatalf("healthcare plan incorrectly used life/property insurance EV: %#v", result.Methods)
	}
}

func TestCalculateFinancialClassificationFiltersGenericModels(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Capital Markets", Sector: "Financial Services",
		LongBusinessSummary: "Provides real estate data products for institutional clients.",
		ForwardEPS:          ptr(5), FreeCashFlowPerShare: ptr(4), CurrentPrice: ptr(20),
		BookValue: ptr(18), DividendRate: ptr(1), TargetMeanPrice: ptr(24),
	})
	if result.Status != StatusReady || result.Method != "金融/地产行业过滤：DDM + P/B" {
		t.Fatalf("financial classification should select the filtered model: %#v", result)
	}
	if result.Methods["P/B"] != 19.8 || result.Methods["DDM"] <= 0 {
		t.Fatalf("financial model candidates = %#v, want P/B and DDM", result.Methods)
	}
	for _, genericMethod := range []string{MethodOwnerEarnings, MethodForwardPE, MethodFCFDCF} {
		if _, ok := result.Methods[genericMethod]; ok {
			t.Fatalf("financial valuation unexpectedly includes %q: %#v", genericMethod, result.Methods)
		}
	}
}

func TestCalculateFinancialClassificationDoesNotFallBackToDCFWithoutAnchors(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Real Estate Services", Sector: "Real Estate",
		ForwardEPS: ptr(5), FreeCashFlowPerShare: ptr(4), CurrentPrice: ptr(20),
	})
	if result.Status != StatusUnavailable {
		t.Fatalf("financial company without DDM/PB/consensus anchors should remain unavailable: %#v", result)
	}
	if result.Methods == nil || result.ConfidenceLabel != "low" || result.DataQualityScore != 75 || result.ForwardEPS == nil {
		t.Fatalf("unavailable financial result lost valuation diagnostics: %#v", result)
	}
	if _, ok := result.Methods[MethodFCFDCF]; ok {
		t.Fatalf("financial company unexpectedly fell back to DCF: %#v", result.Methods)
	}
}

func TestCalculateDoesNotTreatFinancialCustomersAsFinancialCompany(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Specialty Components", Sector: "Industrials",
		LongBusinessSummary: "Provides software and services to financial services customers.",
		ForwardEPS:          ptr(5), CurrentPrice: ptr(20),
	})
	if result.Status != StatusReady || result.Method != MethodForwardPE {
		t.Fatalf("business-summary reference should not activate financial filtering: %#v", result)
	}
}

func TestCalculateCruiseUsesEBITDAAndForwardEarnings(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Cruise Lines", ForwardEPS: ptr(5), EBITDA: ptr(100), ForwardEBITDA: ptr(120),
		SharesOutstanding: ptr(10), Debt: ptr(200), Cash: ptr(50), CurrentPrice: ptr(100),
	})
	if result.Status != StatusReady || result.Method != "重资产消费服务：EV/EBITDA + 远期 EPS" {
		t.Fatalf("cruise company should use its industry model: %#v", result)
	}
	if result.Methods["邮轮 EV/EBITDA 10x"] != 105 || result.Methods["邮轮 EV/EBITDA 12x"] != 129 {
		t.Fatalf("EV/EBITDA candidates = %#v", result.Methods)
	}
	if result.Methods["邮轮远期 PE 12x"] != 60 || result.Methods["邮轮远期 PE 15x"] != 75 {
		t.Fatalf("forward PE candidates = %#v", result.Methods)
	}
	assertFloat(t, result.FairValue, 105)
	assertFloat(t, result.FairValueLow, 86.10)
	assertFloat(t, result.FairValueHigh, 123.90)
	if result.SafetyMargin != .30 {
		t.Fatalf("cruise main safety margin=%v, want default risk margin .30", result.SafetyMargin)
	}
	if result.Defensive == nil || result.Defensive["value"] != 93.0 || result.Defensive["low"] != 81.0 ||
		result.Defensive["high"] != 105.0 || result.Defensive["buy_low"] != 60.75 || result.Defensive["buy_high"] != 78.75 ||
		result.Defensive["safety_margin"] != .25 {
		t.Fatalf("cruise defensive scenario differs from Python golden: %#v", result.Defensive)
	}
	if result.Optimistic == nil || result.Optimistic["low"] != 105.0 || result.Optimistic["high"] != 129.0 || result.Optimistic["safety_margin"] != .25 {
		t.Fatalf("cruise scenarios do not match EV and PE bounds: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
	earningsOnly := Calculate(Fundamentals{
		Industry: "Cruise Lines", ForwardEPS: ptr(5), SharesOutstanding: ptr(10),
	})
	if earningsOnly.Optimistic == nil || earningsOnly.Optimistic["low"] != 60.0 || earningsOnly.Optimistic["high"] != 75.0 {
		t.Fatalf("cruise PE-only optimistic case missing: %#v", earningsOnly.Optimistic)
	}
}

func TestCalculateDefenseModelRequiresTransitionSignals(t *testing.T) {
	stable := Calculate(Fundamentals{
		IndustryKey: "aerospace-defense", ForwardEPS: ptr(5), GrowthRate: ptr(.05),
		RevenueGrowth: ptr(.03), Revenue: ptr(100), SharesOutstanding: ptr(10),
	})
	if stable.Method != MethodForwardPE {
		t.Fatalf("mature defense company should stay on generic earnings path: %#v", stable)
	}
	if _, ok := stable.Methods["国防远期 P/S 6x"]; ok {
		t.Fatalf("mature defense company unexpectedly used the transition model: %#v", stable.Methods)
	}

	transition := Calculate(Fundamentals{
		IndustryKey: "aerospace-defense", ForwardEPS: ptr(5), GrowthRate: ptr(.12),
		RevenueGrowth: ptr(.12), Revenue: ptr(100), SharesOutstanding: ptr(10),
	})
	if transition.Method != "国防订单驱动转型：远期 P/S + 分析师共识" {
		t.Fatalf("growing defense business should activate its transition model: %#v", transition)
	}
	if !strings.Contains(strings.Join(transition.Warnings, " "), "缺乏分析师远期 EPS") {
		t.Fatalf("missing analyst EPS warning not carried into confidence inputs: %#v", transition.Warnings)
	}
	if transition.Methods["国防远期 P/S 6x"] != 72 || transition.Methods["国防远期 P/S 8x"] != 96 {
		t.Fatalf("defense transition candidates = %#v", transition.Methods)
	}
	assertFloat(t, transition.FairValue, 96)
	assertFloat(t, transition.FairValueLow, 78.72)
	assertFloat(t, transition.FairValueHigh, 113.28)
	if transition.SafetyMargin != .30 || transition.Defensive == nil || transition.Defensive["value"] != 96.0 ||
		transition.Defensive["low"] != 74.88 || transition.Defensive["high"] != 117.12 || transition.Defensive["safety_margin"] != .45 || transition.Optimistic["low"] != 72.0 ||
		transition.Optimistic["high"] != 96.0 || transition.Optimistic["safety_margin"] != .45 {
		t.Fatalf("defense scenarios do not match Python anchors: defensive=%#v optimistic=%#v", transition.Defensive, transition.Optimistic)
	}

	consensusOnly := Calculate(Fundamentals{
		IndustryKey: "aerospace-defense", TargetMeanPrice: ptr(40),
		GrowthRate: ptr(.12), RevenueGrowth: ptr(.12),
	})
	if consensusOnly.Optimistic == nil || consensusOnly.Optimistic["low"] != 34.8 || consensusOnly.Optimistic["high"] != 46.0 {
		t.Fatalf("defense target-price fallback missing: %#v", consensusOnly.Optimistic)
	}
}

func TestCalculateDefenseSummaryOverridesSemiconductorKey(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Sector: "Technology", Industry: "Semiconductors",
		LongBusinessSummary: "Defense contractor designing aerospace lasers and directed energy systems.",
		GrowthRate:          ptr(.12), RevenueGrowth: ptr(.12), Revenue: ptr(100), SharesOutstanding: ptr(10),
	})
	if result.Method != "国防订单驱动转型：远期 P/S + 分析师共识" {
		t.Fatalf("defense business summary should override semiconductor classification: %#v", result)
	}
}

func TestCalculateEnergyPrefersReportedNAVOverEBITDAProxy(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "gold", NAVPerShare: ptr(10), EBITDA: ptr(100),
		ForwardEBITDA: ptr(120), SharesOutstanding: ptr(10), Debt: ptr(200), Cash: ptr(50),
	})
	if result.Method != "能源/矿业：有限寿命 NAV + EV/EBITDA" {
		t.Fatalf("energy company should use asset valuation: %#v", result)
	}
	if result.Methods["资源 NAV"] != 10 {
		t.Fatalf("NAV candidate = %#v", result.Methods)
	}
	if _, ok := result.Methods["EV/EBITDA"]; ok {
		t.Fatalf("NAV-anchored company should not also receive an EBITDA proxy: %#v", result.Methods)
	}
	assertFloat(t, result.FairValue, 9.5)
	assertFloat(t, result.FairValueLow, 7.5)
	assertFloat(t, result.FairValueHigh, 11.5)
}

func TestInsuranceAndBiotechFallbacksMatchPythonModeledForwardEPS(t *testing.T) {
	tests := []struct {
		name         string
		fundamentals Fundamentals
		method       string
		value        float64
		low          float64
		high         float64
		defensive    map[string]any
		optimistic   map[string]any
	}{
		{
			name: "insurance earnings proxy",
			fundamentals: Fundamentals{
				Sector: "Financial Services", Industry: "Insurance - Diversified", IndustryKey: "insurance-diversified",
				NormalizedEPS: ptr(10), GrowthRate: ptr(.10),
			},
			method: "保险：内含价值 EV + 新业务价值 NBV", value: 165, low: 132, high: 198,
			defensive: map[string]any{
				"value": 165.0, "low": 128.7, "high": 201.3, "buy_low": 83.66, "buy_high": 130.84,
				"model": "保险：内含价值 EV + 新业务价值 NBV 压力测试", "safety_margin": .35,
			},
			optimistic: map[string]any{
				"value": 165.0, "low": 132.0, "high": 198.0, "buy_low": 85.8, "buy_high": 128.7,
				"model": "保险：内含价值 EV + 新业务价值 NBV", "safety_margin": .35,
				"forward_eps": 11.0, "growth_rate": .1,
			},
		},
		{
			name: "biotech approved-drug proxy",
			fundamentals: Fundamentals{
				Sector: "Healthcare", Industry: "Biotechnology", IndustryKey: "biotechnology",
				NormalizedEPS: ptr(10), GrowthRate: ptr(.10),
			},
			method: "生物科技：已上市药品 DCF + 管线风险调整 NPV", value: 183.15, low: 148.5, high: 217.8,
			defensive: map[string]any{
				"value": 183.15, "low": 142.86, "high": 223.44, "buy_low": 78.57, "buy_high": 122.89,
				"model": "生物科技：已上市药品 DCF + 管线风险调整 NPV 压力测试", "safety_margin": .45,
			},
			optimistic: map[string]any{
				"value": 183.15, "low": 148.5, "high": 217.8, "buy_low": 81.68, "buy_high": 119.79,
				"model": "生物科技：已上市药品 DCF + 管线风险调整 NPV", "safety_margin": .45,
				"forward_eps": 11.0, "growth_rate": .1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.fundamentals)
			if result.Method != test.method {
				t.Fatalf("method=%q, want %q", result.Method, test.method)
			}
			assertFloat(t, result.ForwardEPS, 11)
			assertFloat(t, result.FairValue, test.value)
			assertFloat(t, result.FairValueLow, test.low)
			assertFloat(t, result.FairValueHigh, test.high)
			if !reflect.DeepEqual(result.Defensive, test.defensive) || !reflect.DeepEqual(result.Optimistic, test.optimistic) {
				t.Fatalf("scenario differs from Python v39 golden: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
			}
			if result.ForwardEPSSource != "模型估计远期EPS" || result.DecisionTree == nil || result.DecisionTree.Model != test.method {
				t.Fatalf("forward EPS source / decision tree mismatch: source=%q tree=%#v", result.ForwardEPSSource, result.DecisionTree)
			}
		})
	}
}

func TestCalculateIndustryMarginsMatchPythonV39(t *testing.T) {
	for _, test := range []struct {
		name         string
		fundamentals Fundamentals
		wantMargin   float64
	}{
		{
			name:         "banking",
			fundamentals: Fundamentals{IndustryKey: "banks-diversified", BookValue: ptr(100), ROE: ptr(.10)},
			wantMargin:   .25,
		},
		{
			name:         "insurance",
			fundamentals: Fundamentals{IndustryKey: "insurance-diversified", EmbeddedValuePerShare: ptr(100), NewBusinessValuePerShare: ptr(10)},
			wantMargin:   .35,
		},
		{
			name:         "biotech",
			fundamentals: Fundamentals{IndustryKey: "biotechnology", ApprovedDrugValue: ptr(100), PipelineRNPV: ptr(20)},
			wantMargin:   .45,
		},
		{
			name:         "energy mining",
			fundamentals: Fundamentals{IndustryKey: "gold", NAVPerShare: ptr(100)},
			wantMargin:   .35,
		},
		{
			name:         "coal industry",
			fundamentals: Fundamentals{Industry: "Coal", NAVPerShare: ptr(100)},
			wantMargin:   .35,
		},
		{
			name:         "utility",
			fundamentals: Fundamentals{Sector: "Industrials", Industry: "Electric Utility", RABPerShare: ptr(100)},
			wantMargin:   .18,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.fundamentals)
			if result.Status != StatusReady || result.SafetyMargin != test.wantMargin {
				t.Fatalf("status/margin=%q/%v, want ready/%v; result=%#v", result.Status, result.SafetyMargin, test.wantMargin, result)
			}
			assertFloat(t, result.FairValueBuyLow, *result.FairValueLow*(1-test.wantMargin))
			if result.Defensive == nil || result.Defensive["safety_margin"] != test.wantMargin {
				t.Fatalf("defensive margin=%#v, want %v", result.Defensive, test.wantMargin)
			}
		})
	}

	bankInputs := Fundamentals{
		IndustryKey: "banks-diversified", BookValue: ptr(100), ROE: ptr(.10), Beta: ptr(2),
	}
	bank := Calculate(bankInputs)
	if bank.SafetyMargin != .30 {
		t.Fatalf("high-beta banking margin=%v, want base 25%% plus Python's 5%% beta adjustment", bank.SafetyMargin)
	}
	bankInputs.Beta = ptr(4)
	bank = Calculate(bankInputs)
	if bank.SafetyMargin != .25 {
		t.Fatalf("out-of-range beta banking margin=%v, want normalized beta=1 with no high-beta adjustment", bank.SafetyMargin)
	}
}

func TestCalculateLargeDigitalRetailPrecedesNegativeFCFTransition(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Discount Stores", MarketCap: ptr(2e11),
		ForwardEPS: ptr(5), ForwardEPSObserved: true, FreeCashFlow: ptr(-1), Revenue: ptr(100),
		SharesOutstanding: ptr(1e9), SafetyMargin: ptr(.30),
	})
	if result.Method != "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("large digital retailer should keep its lifecycle branch despite negative FCF: %#v", result)
	}
	if _, ok := result.Methods["传统零售远期 PE"]; !ok {
		t.Fatalf("digital retail candidate missing: %#v", result.Methods)
	}
	assertFloat(t, result.FairValue, 90)
	assertFloat(t, result.FairValueLow, 76.5)
	assertFloat(t, result.FairValueHigh, 103.5)
	assertFloat(t, result.FairValueBuyLow, 62.73)
	assertFloat(t, result.FairValueBuyHigh, 84.87)
	if result.SafetyMargin != .18 {
		t.Fatalf("main safety margin=%v, want Python stable-company margin 0.18", result.SafetyMargin)
	}
	if result.Defensive["value"] != 120.0 || result.Defensive["low"] != 110.0 || result.Defensive["high"] != 130.0 || result.Defensive["safety_margin"] != .20 {
		t.Fatalf("digital retail defensive scenario=%#v, want center 120, range 110-130, margin 0.20", result.Defensive)
	}
	if result.Optimistic["value"] != 130.0 || result.Optimistic["low"] != 120.0 || result.Optimistic["high"] != 140.0 ||
		result.Optimistic["buy_low"] != 96.0 || result.Optimistic["buy_high"] != 112.0 || result.Optimistic["safety_margin"] != .20 {
		t.Fatalf("digital retail optimistic scenario=%#v, want center 130, range 120-140, buy range 96-112, margin 0.20", result.Optimistic)
	}
	if result.Optimistic["model"] != "防御性数字化零售：远期 EPS × 15–20 + 数字化溢价" || result.Optimistic["forward_eps"] != 5.0 || result.Optimistic["growth_rate"] != .03 {
		t.Fatalf("digital retail optimistic metadata=%#v, want Python model, EPS 5 and growth floor 0.03", result.Optimistic)
	}
}

func TestDigitalRetailWithoutEPSFallsBackWithoutSyntheticRetailScenario(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Discount Stores", MarketCap: ptr(2e11),
		SharesOutstanding: ptr(1e9), Revenue: ptr(100e9), FreeCashFlow: ptr(20e9),
		RequiredReturn: ptr(.10), TerminalGrowth: ptr(.02),
	})
	if result.Status != StatusReady || result.Method == "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("missing retail EPS should use an available generic fallback: %#v", result)
	}
	if result.Defensive != nil {
		if model, _ := result.Defensive["model"].(string); strings.Contains(model, "数字化零售") {
			t.Fatalf("missing retail EPS generated a synthetic digital-retail scenario: %#v", result.Defensive)
		}
	}
}

func TestDigitalRetailEstimatesForwardEPSFromNormalizedEPSAndGrowth(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Discount Stores", MarketCap: ptr(2e11),
		SharesOutstanding: ptr(1e9), NormalizedEPS: ptr(5), GrowthRate: ptr(.10),
	})
	if result.Method != "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("modeled-forward-EPS retail valuation selected %q, want digital retail", result.Method)
	}
	if result.Methods["传统零售远期 PE"] != 99 {
		t.Fatalf("retail candidate=%v, want (5 EPS x 1.10) x 18", result.Methods["传统零售远期 PE"])
	}
	if result.Defensive["value"] != 129.0 {
		t.Fatalf("defensive center=%v, want modeled retail EPS x 18 + 30", result.Defensive["value"])
	}
	if result.Optimistic["value"] != 139.0 || result.Optimistic["low"] != 129.0 || result.Optimistic["high"] != 149.0 ||
		result.Optimistic["forward_eps"] != 5.5 || result.Optimistic["growth_rate"] != .1 {
		t.Fatalf("optimistic scenario=%#v, want modeled retail EPS 5.5 with Python range and metadata", result.Optimistic)
	}
}

func TestCalculateDoesNotClassifyLargeFoodCompanyAsDigitalRetail(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer Cyclical", Industry: "Beverages", MarketCap: ptr(2e11),
		ForwardEPS: ptr(5), ForwardEPSObserved: true, FreeCashFlow: ptr(20), Revenue: ptr(100),
	})
	if result.Method == "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("food/beverage category without retail classification should not activate digital retail: %#v", result)
	}
}

func TestBusinessSummaryRetailCustomersDoNotTriggerDigitalRetailModel(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software Infrastructure",
		LongBusinessSummary: "Provides payment and inventory tools to retail customers.",
		MarketCap:           ptr(2e11), ForwardEPS: ptr(5), ForwardEPSObserved: true,
		FreeCashFlow: ptr(30), Revenue: ptr(100), GrowthRate: ptr(.05),
	})
	if strings.Contains(result.Method, "数字化零售") || result.Methods["传统零售远期 PE"] != 0 {
		t.Fatalf("business-summary retail mention incorrectly selected digital-retail lifecycle model: %#v", result)
	}
}

func TestCalculateNormalizesUnderscoresInIndustryLifecycleText(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Consumer_Cyclical", Industry: "Discount_Stores", MarketCap: ptr(2e11),
		ForwardEPS: ptr(5), ForwardEPSObserved: true, FreeCashFlow: ptr(20), Revenue: ptr(100),
		SharesOutstanding: ptr(1e9),
	})
	if result.Method != "防御性数字化零售：远期 EPS + 数字化溢价" {
		t.Fatalf("underscore industry fields should match Python normalization: %#v", result)
	}
}

func TestCalculateInternetPlatformUsesCappedUserRevenueAnchor(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "internet-content-information", Revenue: ptr(1000),
		SharesOutstanding: ptr(10), PlatformUsers: ptr(1000), ARPU: ptr(5), ChurnRate: ptr(10),
	})
	if result.Method != "互联网平台：用户质量 + 远期 P/S" {
		t.Fatalf("internet company should use its platform model: %#v", result)
	}
	if !result.IndustryModel || result.AIExposed {
		t.Fatalf("specialized internet model confidence flags = industry %v, AI %v", result.IndustryModel, result.AIExposed)
	}
	if result.Methods["用户价值校验"] != 90 || result.Methods["互联网平台远期 P/S"] != 495 {
		t.Fatalf("internet valuation candidates = %#v", result.Methods)
	}
	assertFloat(t, result.FairValue, 630)
	assertFloat(t, result.FairValueLow, 360)
	assertFloat(t, result.FairValueHigh, 900)
}

func TestCalculateIndustryKeyREITCarriesIndustryConfidenceFlag(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "reit-residential", FFOPerShare: ptr(2), AFFOPerShare: ptr(1.7),
	})
	if !result.IndustryModel {
		t.Fatalf("REIT classification omitted industry confidence flag: %#v", result)
	}
}

func TestCalculateREITScenariosMatchPythonV39Boundaries(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "reit-residential", FFOPerShare: ptr(2.5), AFFOPerShare: ptr(2), NAVPerShare: ptr(25),
	})
	assertFloat(t, result.FairValue, 30)
	assertFloat(t, result.FairValueLow, 24)
	assertFloat(t, result.FairValueHigh, 36)
	wantDefensive := map[string]any{
		"value": 30.0, "low": 23.4, "high": 36.6, "buy_low": 17.55, "buy_high": 27.45,
		"model": "REIT：P/FFO + P/AFFO + NAV 压力测试", "safety_margin": .25,
	}
	wantOptimistic := map[string]any{
		"value": 30.0, "low": 24.0, "high": 36.0, "buy_low": 18.0, "buy_high": 27.0,
		"model": "REIT：P/FFO + P/AFFO + NAV", "safety_margin": .25,
		"forward_eps": nil, "growth_rate": 0.0,
	}
	if !reflect.DeepEqual(result.Defensive, wantDefensive) || !reflect.DeepEqual(result.Optimistic, wantOptimistic) {
		t.Fatalf("REIT scenarios differ from Python v39 golden: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
}

func TestCalculateNormalizesUnderscoreIndustryKeysLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "reit_residential", FFOPerShare: ptr(2), AFFOPerShare: ptr(1.7),
	})
	if result.Method != MethodREIT || !result.IndustryModel {
		t.Fatalf("underscore industry key should match Python normalization and select REIT valuation: %#v", result)
	}
}

func TestCalculateClassifiesPythonInternetPlatformVariants(t *testing.T) {
	cases := []struct {
		name, sector, industry, summary string
	}{
		{name: "e-commerce industry", sector: "Consumer Cyclical", industry: "E-Commerce"},
		{name: "internet services summary", sector: "Technology", industry: "Software", summary: "Provides internet services."},
		{name: "communication services interactive media", sector: "Communication Services", industry: "Technology", summary: "Operates an interactive creator platform."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Calculate(Fundamentals{
				Sector: tc.sector, Industry: tc.industry, LongBusinessSummary: tc.summary,
				Revenue: ptr(1000), SharesOutstanding: ptr(10), PlatformUsers: ptr(1000), ARPU: ptr(5),
			})
			if result.Method != "互联网平台：用户质量 + 远期 P/S" {
				t.Fatalf("Python-classified internet platform fell through to %q: %#v", result.Method, result)
			}
		})
	}
}

func TestCalculateInternetAIPremiumMatchesPythonKeywordSet(t *testing.T) {
	keywords := []string{
		"large language model", "ad ranking", "personalized ads", "ai-powered", "ai powered",
		"ai-driven", "ai driven", "ai models", "llama",
	}
	for _, keyword := range keywords {
		t.Run(keyword, func(t *testing.T) {
			result := Calculate(Fundamentals{
				IndustryKey: "internet-content-information", LongBusinessSummary: "Builds " + keyword + " products.",
				MarketCap: ptr(1.2e12), Revenue: ptr(100e9), SharesOutstanding: ptr(10e9),
			})
			model, _ := result.Optimistic["model"].(string)
			if !strings.Contains(model, "AI 互联网平台") {
				t.Fatalf("Python AI platform keyword %q did not select the premium scenario: %#v", keyword, result.Optimistic)
			}
		})
	}
}

func TestCalculateMegaCapAIInternetPlatformScenarioMatchesPython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Communication Services", Industry: "Internet Content & Information",
		IndustryKey:         "internet-content-information",
		LongBusinessSummary: "AI-powered ad ranking and recommendation platform with generative AI infrastructure",
		MarketCap:           ptr(1.5e12), Revenue: ptr(130e9), SharesOutstanding: ptr(3e9),
		GrowthRate: ptr(.18), RevenueGrowth: ptr(.18),
	})
	if result.Method != "AI 互联网平台：广告/推荐基础设施 + P/S" {
		t.Fatalf("model=%q, want Python AI internet-platform model", result.Method)
	}
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "internet_platform" ||
		result.DecisionTree.Lifecycle != "internet_platform" || result.DecisionTree.Model != result.Method {
		t.Fatalf("decision tree differs from Python AI platform classification: %#v", result.DecisionTree)
	}
	assertFloat(t, result.FairValue, 238.33)
	if !reflect.DeepEqual(result.Optimistic, map[string]any{
		"value": 352.95, "low": 185.9, "high": 520.0,
		"buy_low": 111.54, "buy_high": 312.0,
		"model": "AI 互联网平台：基础 P/S + AI 溢价 P/S 上限", "safety_margin": .40,
		"forward_revenue_per_share": 43.3333, "growth_rate": .18,
	}) {
		t.Fatalf("AI platform optimistic scenario differs from Python golden: %#v", result.Optimistic)
	}
}

func TestCalculateHighGrowthChipUsesForwardEPSAndCrossChecks(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(100), ForwardEPSObserved: true, TrailingEPS: ptr(4), GrowthRate: ptr(.30),
		Revenue: ptr(100), EBITDA: ptr(20), SharesOutstanding: ptr(10), Debt: ptr(10), Cash: ptr(5),
		PriceToSales: ptr(20), EnterpriseToEBITDA: ptr(30), TargetMeanPrice: ptr(350),
	})
	if result.Method != "高增长芯片：远期盈利 + EV/EBITDA / P/S" {
		t.Fatalf("method = %q, want high-growth chip model", result.Method)
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 10 {
		t.Fatalf("forward EPS cap = %v, want 10", result.ForwardEPS)
	}
	if result.Methods["远期 EPS × PE"] != 275 || result.Methods["远期 EV/EBITDA"] != 72.1 || result.Methods["远期 P/S"] != 129.5 {
		t.Fatalf("high-growth chip candidates = %#v", result.Methods)
	}
	assertFloat(t, result.FairValue, 129.5)
	assertFloat(t, result.FairValueLow, 106.19)
	assertFloat(t, result.FairValueHigh, 152.81)
	if result.Defensive["low"] != 200.0 || result.Defensive["high"] != 250.0 || result.Optimistic["low"] != 300.0 || result.Optimistic["high"] != 350.0 {
		t.Fatalf("chip scenarios mismatch: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
}

func TestCalculateOrdinarySemiconductorDoesNotUseChipSpecialtyByIndustryKey(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(6), ForwardEPSObserved: true, TrailingEPS: ptr(5), GrowthRate: ptr(.10),
		Revenue: ptr(100), FreeCashFlow: ptr(20), EBITDA: ptr(25), SharesOutstanding: ptr(10),
		PriceToSales: ptr(8), MarketCap: ptr(240e9), MarketCapDataQuality: "consistent",
	})
	if strings.Contains(result.Method, "高增长芯片") || strings.Contains(result.Method, "AI 存储/硬件周期") {
		t.Fatalf("ordinary semiconductor used a specialty chip model: %#v", result)
	}
	if _, ok := result.Methods["远期 EV/EBITDA"]; ok {
		t.Fatalf("ordinary semiconductor received a chip EV/EBITDA candidate: %#v", result.Methods)
	}
	if result.SafetyMargin != .30 {
		t.Fatalf("ordinary semiconductor safety margin=%v, want generic 30%%", result.SafetyMargin)
	}
}

func TestCalculateHighGrowthChipFallsBackWithoutObservedForwardEPS(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(10), TrailingEPS: ptr(4), GrowthRate: ptr(.30),
		Revenue: ptr(100), EBITDA: ptr(20), SharesOutstanding: ptr(10),
		PriceToSales: ptr(20),
	})
	if strings.Contains(result.Method, "高增长芯片") {
		t.Fatalf("unobserved analyst EPS should disable high-growth chip valuation: %#v", result)
	}
	if result.Status != StatusUnavailable || result.ForwardEPS != nil {
		t.Fatalf("missing analyst EPS must not be re-estimated on the chip fallback: %#v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "缺乏分析师远期 EPS") {
		t.Fatalf("missing analyst EPS warning was lost: %#v", result.Warnings)
	}
}

func TestCalculateTreatsNonpositiveObservedForwardEPSAsMissing(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(-1), ForwardEPSObserved: true, TrailingEPS: ptr(4), GrowthRate: ptr(.30),
		Revenue: ptr(100), EBITDA: ptr(20), SharesOutstanding: ptr(10), PriceToSales: ptr(20),
	})
	if result.Status != StatusUnavailable || result.Method != "" || result.ForwardEPS != nil {
		t.Fatalf("nonpositive Yahoo EPS must follow Python's missing-EPS fallback: %#v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "缺乏分析师远期 EPS") {
		t.Fatalf("missing analyst EPS warning was lost: %#v", result.Warnings)
	}
}

func TestCalculateMemoryClassificationPrecedesHighGrowthChip(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductor Memory", Sector: "Technology",
		ForwardEPS: ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(2), GrowthRate: ptr(.40),
	})
	if !strings.Contains(result.Method, "存储") {
		t.Fatalf("memory semiconductor should use cyclical memory model: %#v", result)
	}
}

func TestCalculateMobilityTechnologyPrecedesKeywordIndustryModels(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(354.54), SharesOutstanding: ptr(3.2e9),
		Industry: "Auto Manufacturers", Sector: "Consumer Cyclical",
		LongBusinessSummary: "Tesla develops electric vehicles, autonomous self driving robotaxi, robotics, humanoid Optimus, artificial intelligence and energy storage for utilities. It also provides automotive insurance services.",
		ForwardEPS:          ptr(1.26), ForwardEPSObserved: true, TrailingEPS: ptr(1.20), GrowthRate: ptr(.12),
		Revenue: ptr(100e9), FreeCashFlow: ptr(8e9), Debt: ptr(5e9), Cash: ptr(25e9),
		MarketCap: ptr(1.13e12), MarketCapDataQuality: "consistent", Beta: ptr(2.0),
	})
	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("mobility company was captured by a keyword industry model: %#v", result)
	}
	if result.IndustryModel || !result.AIExposed {
		t.Fatalf("mobility confidence flags = industry %v, AI %v", result.IndustryModel, result.AIExposed)
	}
	if model, _ := result.Optimistic["model"].(string); !strings.Contains(model, "移动科技成长") {
		t.Fatalf("missing mobility upside scenario: %#v", result.Optimistic)
	}
	if result.SafetyMargin != .35 || result.Defensive["value"] != 22.05 || result.Defensive["low"] != 17.20 ||
		result.Defensive["high"] != 26.90 || result.Defensive["safety_margin"] != .35 {
		t.Fatalf("mobility main and automobile-floor margins differ from Python: margin=%v defensive=%#v", result.SafetyMargin, result.Defensive)
	}
}

func TestCalculateOrdinaryAutoManufacturerDoesNotUseMobilityModel(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(80), SharesOutstanding: ptr(3e9),
		IndustryKey: "auto-manufacturers", Industry: "Auto Manufacturers", Sector: "Consumer Cyclical",
		LongBusinessSummary: "Manufactures passenger vehicles and commercial trucks.",
		ForwardEPS:          ptr(4), ForwardEPSObserved: true, TrailingEPS: ptr(3.5), GrowthRate: ptr(.10),
		Revenue: ptr(80e9), FreeCashFlow: ptr(4e9), MarketCap: ptr(240e9), MarketCapDataQuality: "consistent",
	})
	if strings.Contains(result.Method, "移动科技") || strings.Contains(result.Method, "汽车制造") {
		t.Fatalf("ordinary auto manufacturer entered mobility valuation: %#v", result)
	}
	if _, ok := result.Methods["汽车制造远期 EPS × PE"]; ok {
		t.Fatalf("ordinary auto manufacturer received an automotive floor: %#v", result.Methods)
	}
}

func TestCalculateLargeElectricVehicleMakerUsesMobilityGrowthModel(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(200), SharesOutstanding: ptr(3e9),
		IndustryKey: "electric-vehicles", Industry: "Electric Vehicles", Sector: "Consumer Cyclical",
		LongBusinessSummary: "Designs and manufactures battery electric vehicles.",
		ForwardEPS:          ptr(3), ForwardEPSObserved: true, TrailingEPS: ptr(2.5), GrowthRate: ptr(.10),
		Revenue: ptr(100e9), FreeCashFlow: ptr(4e9), MarketCap: ptr(600e9), MarketCapDataQuality: "consistent",
	})
	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("large EV maker failed the configured mobility growth path: %#v", result)
	}
	if result.SafetyMargin != .30 || result.Defensive["value"] != 52.5 || result.Defensive["safety_margin"] != .30 {
		t.Fatalf("mobility main and defensive margins differ from Python: margin=%v defensive=%#v", result.SafetyMargin, result.Defensive)
	}
	if model, _ := result.Optimistic["model"].(string); !strings.Contains(model, "移动科技成长") {
		t.Fatalf("missing mobility upside scenario: %#v", result.Optimistic)
	}
}

func TestCalculateCapsMobilityForwardEPSAgainstNormalizedEPS(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(200), SharesOutstanding: ptr(1e9), IndustryKey: "auto-manufacturers",
		Industry: "Auto Manufacturers", LongBusinessSummary: "Autonomous robotaxi and robotics technology.",
		ForwardEPS: ptr(100), ForwardEPSObserved: true, TrailingEPS: ptr(10), GrowthRate: ptr(.12),
		Revenue: ptr(100e9), MarketCap: ptr(200e9), MarketCapDataQuality: "consistent",
	})
	if result.ForwardEPS == nil || *result.ForwardEPS != 30 {
		t.Fatalf("mobility forward EPS=%v, want the normal 3x cap of 30", result.ForwardEPS)
	}
}

func TestCalculateMobilityTechnologyCanUseTwoStrongSignalsWithoutIndustryContext(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(354.54), SharesOutstanding: ptr(3.2e9),
		Industry: "Software Infrastructure", Sector: "Technology",
		LongBusinessSummary: "Develops robotaxi fleets and an autopilot platform.",
		ForwardEPS:          ptr(1.26), ForwardEPSObserved: true, TrailingEPS: ptr(1.20), GrowthRate: ptr(.12),
		Revenue: ptr(100e9), FreeCashFlow: ptr(8e9), Debt: ptr(5e9), Cash: ptr(25e9),
		MarketCap: ptr(1.13e12), MarketCapDataQuality: "consistent",
	})
	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("two strong mobility signals should classify a large company: %#v", result)
	}
}

func TestCalculateMobilityOptimisticRevenueUsesSlowdownMultiplesAndDerivedShares(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(200), IndustryKey: "auto-manufacturers", Industry: "Auto Manufacturers",
		LongBusinessSummary: "Builds autonomous robotaxi vehicles and robotics platforms.",
		ForwardEPS:          ptr(2), ForwardEPSObserved: true, GrowthRate: ptr(.10), Revenue: ptr(100e9),
		MarketCap: ptr(200e9), MarketCapDataQuality: "consistent", Regime: RegimeSlowdown,
	})
	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("mobility growth model not selected: %#v", result)
	}
	if result.Optimistic["low"] != 440.0 || result.Optimistic["high"] != 770.0 {
		t.Fatalf("slowdown P/S range=%#v, want 440-770", result.Optimistic)
	}
	if result.Optimistic["safety_margin"] != .45 || result.Optimistic["forward_revenue"] != 110e9 {
		t.Fatalf("slowdown assumptions=%#v", result.Optimistic)
	}
	if model, _ := result.Optimistic["model"].(string); model != "移动科技成长：远期收入 P/S 4–7x（放缓）" {
		t.Fatalf("slowdown model=%q", model)
	}
}

func TestCalculateMobilityOptimisticFallsBackToModeledForwardEPSWithoutRevenue(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(200), IndustryKey: "auto-manufacturers", Industry: "Auto Manufacturers",
		LongBusinessSummary: "Builds autonomous robotaxi vehicles and robotics platforms.",
		TrailingEPS:         ptr(2), AnnualEPS: []float64{1, 2, 3}, GrowthRate: ptr(.20),
		MarketCap: ptr(200e9), MarketCapDataQuality: "consistent",
	})
	if result.Optimistic["low"] != 72.0 || result.Optimistic["high"] != 120.0 {
		t.Fatalf("modeled EPS range=%#v, want 72-120", result.Optimistic)
	}
	if result.Optimistic["forward_eps"] != 2.4 || result.Optimistic["safety_margin"] != .35 {
		t.Fatalf("modeled EPS assumptions=%#v", result.Optimistic)
	}
	if model, _ := result.Optimistic["model"].(string); model != "移动科技成长：远期 EPS × 30–50 倍 PE" {
		t.Fatalf("normal EPS model=%q", model)
	}
}

func TestCalculateMobilityTechnologyIgnoresEnterpriseSoftwareLanguage(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(137.34), SharesOutstanding: ptr(2.88e9),
		Industry: "Software Infrastructure", Sector: "Technology",
		LongBusinessSummary: "Provides autonomous database products and robotic process automation for enterprise customers.",
		ForwardEPS:          ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(6.38), GrowthRate: ptr(.12),
		Revenue: ptr(60e9), FreeCashFlow: ptr(10e9),
		MarketCap: ptr(417.7e9), MarketCapDataQuality: "consistent",
	})
	optimisticModel, _ := result.Optimistic["model"].(string)
	if strings.Contains(result.Method, "移动科技成长") || strings.Contains(optimisticModel, "移动科技成长") {
		t.Fatalf("enterprise software terms triggered mobility valuation: %#v", result)
	}
}

func TestCalculateAssetHeavyChipTransitionPrecedesHighGrowthChip(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductors", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(4), GrowthRate: ptr(.30),
		Revenue: ptr(100), EBITDA: ptr(20), SharesOutstanding: ptr(10), FreeCashFlow: ptr(-5),
	})
	if !strings.Contains(result.Method, "重资产转型") {
		t.Fatalf("negative FCF should give asset-heavy transition priority: %#v", result)
	}
	if result.Methods["制造/云业务 P/S"] != 15 {
		t.Fatalf("semiconductor asset P/S=%v, want 1.5x revenue per Python lifecycle rule", result.Methods["制造/云业务 P/S"])
	}
}

func TestCalculateAIStorageCycleAppliesEPSHaircutAndSeparateScenarios(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductor-memory", Industry: "Semiconductors", Sector: "Technology",
		ForwardEPS: ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(2), GrowthRate: ptr(.25),
	})
	if result.Method != "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE" {
		t.Fatalf("method = %q, want AI storage cycle model", result.Method)
	}
	if result.Methods["周期调整远期 EPS × PE"] != 175 {
		t.Fatalf("cycle-adjusted EPS candidate = %#v, want 175", result.Methods)
	}
	assertFloat(t, result.FairValue, 175)
	assertFloat(t, result.FairValueLow, 140)
	assertFloat(t, result.FairValueHigh, 210)
	if result.Defensive["low"] != 140.0 || result.Defensive["high"] != 175.0 || result.Optimistic["low"] != 200.0 || result.Optimistic["high"] != 300.0 {
		t.Fatalf("storage cycle scenarios mismatch: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
}

func TestCalculateClassifiesFlashStorageLanguageLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		LongBusinessSummary: "Designs flash storage products for data centers.",
		Revenue:             ptr(100), SharesOutstanding: ptr(10),
		NormalizedEPS: ptr(2), ForwardEPS: ptr(3), ForwardEPSObserved: true,
	})
	if result.Method != "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE" {
		t.Fatalf("plain flash keyword should match Python's memory classification, got %#v", result)
	}
}

func TestCalculateStructuralAIStorageUsesGrowthPEBand(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "computer-hardware", Industry: "Computer Hardware", Sector: "Technology",
		LongBusinessSummary: "Manufacturer of hard disk drives and enterprise storage systems.",
		ForwardEPS:          ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(2), GrowthRate: ptr(.35),
		Revenue: ptr(100), FreeCashFlow: ptr(20),
	})
	if result.Method != "AI 存储结构性增长：远期 EPS × 35–55 倍 PE" {
		t.Fatalf("method = %q, want structural AI storage model", result.Method)
	}
	if result.Methods["AI 存储远期 EPS × PE"] != 450 {
		t.Fatalf("storage growth candidate = %#v, want 450", result.Methods)
	}
	assertFloat(t, result.FairValue, 450)
	assertFloat(t, result.FairValueLow, 360)
	assertFloat(t, result.FairValueHigh, 540)
	if result.Defensive["low"] != 350.0 || result.Defensive["high"] != 450.0 || result.Optimistic["low"] != 350.0 || result.Optimistic["high"] != 550.0 {
		t.Fatalf("structural storage scenarios mismatch: defensive=%#v optimistic=%#v", result.Defensive, result.Optimistic)
	}
}

func TestCalculateOrdinaryComputerHardwareUsesGenericModel(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "computer-hardware", Industry: "Computer Hardware", Sector: "Technology",
		LongBusinessSummary: "Designs enterprise servers and workstation computers.",
		ForwardEPS:          ptr(6), ForwardEPSObserved: true, TrailingEPS: ptr(5), GrowthRate: ptr(.10),
		Revenue: ptr(100), FreeCashFlow: ptr(12), EBITDA: ptr(20), SharesOutstanding: ptr(10),
	})
	if strings.Contains(result.Method, "AI 存储/硬件周期") || strings.Contains(result.Method, "AI 存储结构性增长") {
		t.Fatalf("ordinary computer hardware used a storage-specific model: %#v", result)
	}
	if _, ok := result.Methods["远期 EV/EBITDA"]; ok {
		t.Fatalf("ordinary computer hardware received an industry-specific EV/EBITDA candidate: %#v", result.Methods)
	}
	if result.SafetyMargin != .30 {
		t.Fatalf("ordinary computer hardware safety margin=%v, want generic 30%%", result.SafetyMargin)
	}
}

func TestCalculateSpecializedModelFallsBackToPriceAnchorForExtremeValue(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice: ptr(100), Sector: "Technology", Industry: "Computer Hardware Storage",
		ForwardEPS: ptr(527), ForwardEPSObserved: true, TrailingEPS: ptr(100), NormalizedEPS: ptr(100),
		GrowthRate: ptr(.50), Beta: ptr(1.2), Revenue: ptr(1000),
		OperatingCashflow: ptr(250), OperatingIncome: ptr(300), NetIncome: ptr(200),
		DepreciationAndAmortization: ptr(50), CapitalExpenditure: ptr(50),
		MarketCap: ptr(10e9), PriceToSales: ptr(12), TargetMeanPrice: ptr(2137),
	})
	if result.Method != "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE" {
		t.Fatalf("method=%q, want specialized storage cycle model", result.Method)
	}
	assertFloat(t, result.FairValue, 150)
	if !strings.Contains(result.Warning, "Yahoo 财务数据口径异常") {
		t.Fatalf("warning=%q, want Yahoo unit-anomaly fallback warning", result.Warning)
	}
	foundWarning := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "已降级到现价锚定") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("warnings=%#v, missing price-anchor fallback warning", result.Warnings)
	}
}

func TestCalculateFiltersUnitAnomalyCandidatesBeforeMedian(t *testing.T) {
	tests := []struct {
		name            string
		currentPrice    float64
		targetMeanPrice float64
		wantFairValue   float64
	}{
		{"EPS anchor removes outlier", 10000, 100000, 3500},
		{"price anchor removes outlier", 1000, 25000, 3500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(Fundamentals{
				IndustryKey: "software-application", Sector: "Technology", Industry: "Software Infrastructure",
				CurrentPrice: ptr(test.currentPrice), SharesOutstanding: ptr(10),
				NormalizedEPS: ptr(500), ForwardEPS: ptr(500), FreeCashFlow: ptr(-1),
				Revenue: ptr(15000), EBITDA: ptr(3500.0 / 12 * 10), TargetMeanPrice: ptr(test.targetMeanPrice),
			})
			if result.Method != "重资产转型：核心业务 PE + 制造/云业务 P/S" {
				t.Fatalf("method=%q, want asset-heavy multi-candidate model", result.Method)
			}
			assertFloat(t, result.FairValue, test.wantFairValue)
			assertFloat(t, result.FairValueLow, 2870)
			assertFloat(t, result.FairValueHigh, 4130)
			if result.Methods["分析师共识"] != test.targetMeanPrice {
				t.Fatalf("candidate details changed after filtering: %#v", result.Methods)
			}
		})
	}
}

func TestFilterUnitAnomalyCandidatesKeepsOriginalWhenNoReplacementFits(t *testing.T) {
	tests := []struct {
		name          string
		epsAnchor     float64
		priceAnchor   float64
		skipEPSFilter bool
		candidates    []float64
	}{
		{"no EPS-plausible candidate", 500, 0, false, []float64{10, 20}},
		{"no price-plausible candidate", 0, 1000, true, []float64{10, 20}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := filterUnitAnomalyCandidates(test.candidates, test.epsAnchor, test.priceAnchor, test.skipEPSFilter)
			if len(got) != len(test.candidates) {
				t.Fatalf("candidate count=%d, want original count %d: %v", len(got), len(test.candidates), got)
			}
			for i, want := range test.candidates {
				if got[i] != want {
					t.Fatalf("candidate[%d]=%v, want original value %v", i, got[i], want)
				}
			}
		})
	}
}

func TestCalculateUsesInformationPagePSFallbackOnlyAfterRetryableNoCandidate(t *testing.T) {
	eligible := Fundamentals{
		TimeseriesRetryable: true, Revenue: ptr(1000), SharesOutstanding: ptr(10),
		PriceToSales: ptr(2), Debt: ptr(100), Cash: ptr(50), SafetyMargin: ptr(.30),
	}
	result := Calculate(eligible)
	if result.Status != StatusReady || result.Method != "信息页 P/S 低置信度兜底" {
		t.Fatalf("status/method=%q/%q, want ready/information-page P/S fallback", result.Status, result.Method)
	}
	assertFloat(t, result.FairValue, 125)
	assertFloat(t, result.FairValueLow, 97.5)
	assertFloat(t, result.FairValueHigh, 152.5)
	assertFloat(t, result.FairValueBuyLow, 68.25)
	assertFloat(t, result.FairValueBuyHigh, 106.75)
	if result.Methods[result.Method] != 125 || result.Warning != "" {
		t.Fatalf("fallback methods/warning=%#v/%q, want candidate with warning in warnings only", result.Methods, result.Warning)
	}
	foundPartialWarning := false
	for _, warning := range result.Warnings {
		if warning == "部分 Yahoo 财务接口暂时不可用，已使用可用字段生成估值" {
			foundPartialWarning = true
		}
	}
	if !foundPartialWarning {
		t.Fatalf("warnings=%#v, missing partial-upstream fallback warning", result.Warnings)
	}
	withExistingCandidate := applyInformationPagePSFallback(eligible, Result{
		Methods: map[string]float64{"existing candidate": 0},
	})
	if withExistingCandidate.FairValue != nil || withExistingCandidate.Method != "" {
		t.Fatalf("fallback replaced an existing candidate set: %#v", withExistingCandidate)
	}

	tests := []struct {
		name         string
		fundamentals Fundamentals
	}{
		{
			name:         "retry flag absent",
			fundamentals: Fundamentals{Revenue: ptr(1000), SharesOutstanding: ptr(10), PriceToSales: ptr(2)},
		},
		{
			name:         "revenue absent",
			fundamentals: Fundamentals{TimeseriesRetryable: true, SharesOutstanding: ptr(10), PriceToSales: ptr(2)},
		},
		{
			name:         "shares absent",
			fundamentals: Fundamentals{TimeseriesRetryable: true, Revenue: ptr(1000), PriceToSales: ptr(2)},
		},
		{
			name: "debt makes candidate nonpositive",
			fundamentals: Fundamentals{
				TimeseriesRetryable: true, Revenue: ptr(1000), SharesOutstanding: ptr(10),
				PriceToSales: ptr(2), Debt: ptr(2000),
			},
		},
		{
			name: "regular model candidate takes precedence",
			fundamentals: Fundamentals{
				TimeseriesRetryable: true, Revenue: ptr(1000), SharesOutstanding: ptr(10),
				PriceToSales: ptr(2), ForwardEPS: ptr(5),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Calculate(test.fundamentals)
			if got.Method == "信息页 P/S 低置信度兜底" {
				t.Fatalf("unexpected P/S fallback for ineligible input: %#v", got)
			}
		})
	}
}

func TestInformationPagePSFallbackUsesPriceAnchorAndStableMargin(t *testing.T) {
	result := Calculate(Fundamentals{
		UpstreamError: true, CurrentPrice: ptr(10), Revenue: ptr(1000), SharesOutstanding: ptr(10),
		PriceToSales: ptr(2), Sector: "Consumer_Defensive", Industry: "Restaurants", Beta: ptr(1.8),
		SafetyMargin: ptr(.30),
	})
	if result.Method != "信息页 P/S 低置信度兜底" || result.Status != StatusReady {
		t.Fatalf("status/method=%q/%q, want ready/information-page P/S fallback", result.Status, result.Method)
	}
	if result.Methods[result.Method] != 130 {
		t.Fatalf("candidate value=%v, want 130", result.Methods[result.Method])
	}
	assertFloat(t, result.FairValue, 15)
	if result.SafetyMargin != .25 {
		t.Fatalf("stable high-beta margin=%v, want 0.25", result.SafetyMargin)
	}
	assertFloat(t, result.FairValueBuyLow, 9.56)
	assertFloat(t, result.FairValueBuyHigh, 12.94)
	if len(result.Warnings) == 0 || result.Warnings[len(result.Warnings)-1] != "模型估值远高于现价，Yahoo 财务数据口径异常，已降级到现价锚定" {
		t.Fatalf("price-anchor warnings=%#v, want the overriding unit-anomaly warning", result.Warnings)
	}
}

func TestInformationFallbackSafetyMarginMatchesPythonClassification(t *testing.T) {
	tests := []struct {
		name        string
		fundamental Fundamentals
		want        float64
	}{
		{name: "banking industry", fundamental: Fundamentals{IndustryKey: "banks-diversified"}, want: .25},
		{name: "insurance industry", fundamental: Fundamentals{IndustryKey: "insurance-diversified"}, want: .35},
		{name: "REIT industry", fundamental: Fundamentals{IndustryKey: "reit-residential"}, want: .25},
		{name: "biotech industry", fundamental: Fundamentals{IndustryKey: "biotechnology"}, want: .45},
		{name: "energy mining industry", fundamental: Fundamentals{IndustryKey: "gold"}, want: .35},
		{name: "utility industry", fundamental: Fundamentals{IndustryKey: "utilities-regulated-electric"}, want: .18},
		{name: "internet industry", fundamental: Fundamentals{IndustryKey: "internet-content-information"}, want: .40},
		{name: "generic financial real estate", fundamental: Fundamentals{Sector: "Financial Services"}, want: .25},
		{name: "insurance classification from summary", fundamental: Fundamentals{LongBusinessSummary: "Provides life insurance products."}, want: .35},
		{name: "AI storage cycle", fundamental: Fundamentals{IndustryKey: "semiconductor-memory", ForwardEPS: ptr(10), ForwardEPSObserved: true}, want: .40},
		{name: "AI storage structural growth", fundamental: Fundamentals{
			Industry: "Hard Disk Drives", LongBusinessSummary: "Enterprise storage hardware",
			Revenue: ptr(100), FreeCashFlow: ptr(20), GrowthRate: ptr(.35), ForwardEPS: ptr(5), ForwardEPSObserved: true,
		}, want: .40},
		{name: "high growth chip", fundamental: Fundamentals{
			Sector: "Technology", Industry: "Semiconductors", GrowthRate: ptr(.30), ForwardEPS: ptr(10),
			ForwardEPSObserved: true, PriceToSales: ptr(12),
		}, want: .30},
		{name: "mature growth", fundamental: Fundamentals{
			MarketCap: ptr(600e9), Revenue: ptr(100), RevenueGrowth: ptr(.10), FreeCashFlow: ptr(25),
		}, want: .20},
		{name: "mature growth high beta", fundamental: Fundamentals{
			MarketCap: ptr(600e9), Revenue: ptr(100), RevenueGrowth: ptr(.10), FreeCashFlow: ptr(25), Beta: ptr(2),
		}, want: .30},
		{name: "negative cash flow transition", fundamental: Fundamentals{FreeCashFlow: ptr(-10)}, want: .42},
		{name: "high beta transition", fundamental: Fundamentals{FreeCashFlow: ptr(-10), Beta: ptr(2)}, want: .52},
		{name: "capex transition", fundamental: Fundamentals{OperatingCashflow: ptr(100), CapitalExpenditure: ptr(-75)}, want: .42},
		{name: "cyclical", fundamental: Fundamentals{Industry: "Shipping", FreeCashFlow: ptr(10)}, want: .62},
		{name: "generic high growth", fundamental: Fundamentals{GrowthRate: ptr(.20), NormalizedEPS: ptr(2)}, want: .45},
		{name: "defense transition negative cash flow", fundamental: Fundamentals{
			IndustryKey: "aerospace-defense", FreeCashFlow: ptr(-10), GrowthRate: ptr(.10),
		}, want: .38},
		{name: "stable", fundamental: Fundamentals{Sector: "Consumer Defensive"}, want: .18},
		{name: "stable high beta", fundamental: Fundamentals{Sector: "Consumer Staples", Beta: ptr(2)}, want: .25},
		{name: "mobility business summary does not become energy industry", fundamental: Fundamentals{
			IndustryKey: "auto-manufacturers", Industry: "Auto Manufacturers", MarketCap: ptr(600e9),
			Revenue: ptr(100), FreeCashFlow: ptr(25), GrowthRate: ptr(.10),
			LongBusinessSummary: "Develops autonomous driving and energy storage products.",
		}, want: .30},
		{name: "cruise has no dedicated margin", fundamental: Fundamentals{Industry: "Cruise Lines", FreeCashFlow: ptr(10)}, want: .30},
		{name: "AI winter remains cyclical", fundamental: Fundamentals{
			Sector: "Technology", Industry: "Semiconductors", Regime: RegimeWinter, GrowthRate: ptr(.30),
			ForwardEPS: ptr(10), ForwardEPSObserved: true, PriceToSales: ptr(12),
		}, want: .62},
		{name: "default", fundamental: Fundamentals{}, want: .30},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := informationFallbackSafetyMargin(test.fundamental); got != test.want {
				t.Fatalf("safety margin=%v, want Python classification margin %v", got, test.want)
			}
		})
	}
}
