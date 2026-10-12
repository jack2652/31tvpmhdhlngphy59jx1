package valuation

import "testing"

func nextGapPtr(value float64) *float64 { return &value }

func TestMobilityGrowthUsesAnalystTargetAsEligibilitySignalLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Software Infrastructure",
		LongBusinessSummary: "Builds robotaxi fleets and humanoid robotics platforms.",
		MarketCap:           nextGapPtr(1e11), MarketCapDataQuality: "consistent",
		Revenue: nextGapPtr(10e9), SharesOutstanding: nextGapPtr(1e9),
		TargetMeanPrice: nextGapPtr(30),
	})

	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "移动科技成长" {
		t.Fatalf("lifecycle=%v, want Python target-price eligibility to select mobility growth; method=%q", result.DecisionTree, result.Method)
	}
	if result.Method != "移动科技成长：自动驾驶/机器人远期 P/S" {
		t.Fatalf("method=%q, want Python mobility forward-revenue P/S model", result.Method)
	}
	if got := result.Methods["移动科技远期 P/S"]; got != 94.5 {
		t.Fatalf("mobility valuation=%v, want 94.5 from 10B revenue x 1.05 growth x 9x P/S / 1B shares", got)
	}
}
