package valuation

import (
	"math"
	"testing"
)

func valuationGapCompletionPtr(value float64) *float64 { return &value }

func TestCloudStorageSummaryUsesPythonDefaultAIExposure(t *testing.T) {
	result := Calculate(Fundamentals{
		CurrentPrice:         valuationGapCompletionPtr(100),
		SharesOutstanding:    valuationGapCompletionPtr(10),
		MarketCap:            valuationGapCompletionPtr(1000),
		MarketCapDataQuality: "consistent",
		Revenue:              valuationGapCompletionPtr(100),
		AnnualRevenue:        []float64{100},
		FreeCashFlow:         valuationGapCompletionPtr(20),
		GrowthRate:           valuationGapCompletionPtr(.10),
		RevenueGrowth:        valuationGapCompletionPtr(.10),
		NormalizedEPS:        valuationGapCompletionPtr(10),
		ForwardEPS:           valuationGapCompletionPtr(12),
		ForwardEPSObserved:   true,
		AnalystCount:         valuationGapCompletionPtr(4),
		Sector:               "Technology",
		Industry:             "Software Infrastructure",
		IndustryKey:          "software-infrastructure",
		LongBusinessSummary:  "Provides cloud data storage services.",
		Regime:               RegimeSlowdown,
	})

	if !result.AIExposed {
		t.Fatal("cloud-storage summary should use Python's default 30% AI exposure")
	}
	if result.ForwardEPS == nil || math.Abs(*result.ForwardEPS-8.4) > 1e-9 {
		t.Fatalf("forward EPS=%v, want Python v39 golden 8.4 after slowdown adjustment", derefValuationGap(result.ForwardEPS))
	}
	if result.ForwardEPSSource != "Yahoo远期EPS（放缓调整70%）" {
		t.Fatalf("forward EPS source=%q, want Python's slowdown-adjusted Yahoo source", result.ForwardEPSSource)
	}
	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "普通" {
		t.Fatalf("decision tree=%#v, cloud storage software must not become a storage-hardware lifecycle", result.DecisionTree)
	}
}

func TestDefenseStorageSummaryDoesNotUseDefaultAIExposure(t *testing.T) {
	f := Fundamentals{
		CurrentPrice:         valuationGapCompletionPtr(100),
		SharesOutstanding:    valuationGapCompletionPtr(10),
		MarketCap:            valuationGapCompletionPtr(1000),
		MarketCapDataQuality: "consistent",
		Revenue:              valuationGapCompletionPtr(100),
		AnnualRevenue:        []float64{100},
		FreeCashFlow:         valuationGapCompletionPtr(20),
		GrowthRate:           valuationGapCompletionPtr(.05),
		RevenueGrowth:        valuationGapCompletionPtr(.05),
		NormalizedEPS:        valuationGapCompletionPtr(10),
		ForwardEPS:           valuationGapCompletionPtr(12),
		ForwardEPSObserved:   true,
		AnalystCount:         valuationGapCompletionPtr(4),
		Sector:               "Technology",
		Industry:             "Software Infrastructure",
		IndustryKey:          "software-infrastructure",
		LongBusinessSummary:  "Provides directed energy storage management software.",
		Regime:               RegimeSlowdown,
	}

	result := Calculate(f)
	if result.AIExposed {
		t.Fatal("Python excludes default AI-revenue exposure for defense-tagged companies")
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 12 {
		t.Fatalf("forward EPS=%v, defense-tagged storage should not receive the 70%% AI slowdown haircut", derefValuationGap(result.ForwardEPS))
	}
}

func derefValuationGap(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
