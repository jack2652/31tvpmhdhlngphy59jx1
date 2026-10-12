package valuation

import "testing"

func TestStableCashflowSafetyMarginMatchesPythonBetaConfig(t *testing.T) {
	base := Fundamentals{
		Sector: "Consumer Defensive", Industry: "Restaurants",
		Revenue: floatPointer(100), FreeCashFlow: floatPointer(20),
		ForwardEPS: floatPointer(5), ForwardEPSObserved: true, GrowthRate: floatPointer(.10),
	}

	ordinary := Calculate(base)
	if ordinary.DecisionTree == nil || ordinary.DecisionTree.Lifecycle != "稳定现金流" || ordinary.SafetyMargin != .18 {
		t.Fatalf("ordinary beta result=%#v, want stable cashflow lifecycle with 0.18 safety margin", ordinary)
	}

	highBeta := base
	highBeta.Beta = floatPointer(1.8)
	result := Calculate(highBeta)
	if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "稳定现金流" || result.SafetyMargin != .25 {
		t.Fatalf("high-beta result=%#v, want stable_high_beta 0.20 plus the Python 0.05 beta adjustment", result)
	}
}
