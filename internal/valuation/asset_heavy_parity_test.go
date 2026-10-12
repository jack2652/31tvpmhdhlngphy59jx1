package valuation

import "testing"

func TestAssetHeavyTransitionScenariosMatchPython(t *testing.T) {
	for _, test := range []struct {
		name              string
		beta              *float64
		wantMargin        float64
		wantDefensiveMarg float64
	}{
		{name: "ordinary beta", wantMargin: .42, wantDefensiveMarg: .45},
		{name: "high beta", beta: ptr(2), wantMargin: .52, wantDefensiveMarg: .50},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(Fundamentals{
				Sector: "Technology", Industry: "Software Infrastructure",
				CurrentPrice: ptr(50), SharesOutstanding: ptr(10), MarketCap: ptr(500),
				MarketCapDataQuality: "consistent", Beta: test.beta,
				Revenue: ptr(100), FreeCashFlow: ptr(-10),
			})
			if result.Method != "重资产转型：核心业务 PE + 制造/云业务 P/S" {
				t.Fatalf("method=%q, want asset-heavy transition", result.Method)
			}
			assertFloat(t, result.FairValue, 20)
			assertFloat(t, result.FairValueLow, 15.6)
			assertFloat(t, result.FairValueHigh, 24.4)
			assertFloat(t, floatPointer(result.SafetyMargin), test.wantMargin)
			if result.DecisionTree == nil || result.DecisionTree.Lifecycle != "重资产转型" {
				t.Fatalf("decision tree=%#v, want 重资产转型", result.DecisionTree)
			}
			assertScenario(t, result.Defensive, 20, 15.6, 24.4, test.wantDefensiveMarg)
			assertScenario(t, result.Optimistic, 22, 17.6, 26.4, .40)
		})
	}
}

func assertScenario(t *testing.T, got map[string]any, value, low, high, margin float64) {
	t.Helper()
	for key, want := range map[string]float64{
		"value": value, "low": low, "high": high, "safety_margin": margin,
	} {
		actual, ok := got[key].(float64)
		if !ok || actual != want {
			t.Errorf("scenario[%q]=%v, want %v", key, got[key], want)
		}
	}
}
