package valuation

import "testing"

func TestFinancialSectorUsesPythonFinancialFilterModel(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial", Industry: "Asset Management",
		ForwardEPS: ptr(5), FreeCashFlowPerShare: ptr(4), CurrentPrice: ptr(20),
		BookValue: ptr(18), DividendRate: ptr(1), TargetMeanPrice: ptr(24),
	})
	if result.Status != StatusReady || result.Method != "金融/地产行业过滤：DDM + P/B" {
		t.Fatalf("financial sector should select the Python DDM/P/B filter model: %#v", result)
	}
	for _, method := range []string{"DDM", "P/B", "分析师共识"} {
		if _, ok := result.Methods[method]; !ok {
			t.Errorf("financial model candidates=%#v, missing %q", result.Methods, method)
		}
	}
	for _, method := range []string{MethodOwnerEarnings, MethodForwardPE, MethodFCFDCF} {
		if _, ok := result.Methods[method]; ok {
			t.Errorf("financial valuation unexpectedly includes generic candidate %q: %#v", method, result.Methods)
		}
	}
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "金融/地产" || result.DecisionTree.Lifecycle != "金融/地产" {
		t.Fatalf("decision tree=%#v, want Python financial sector filter", result.DecisionTree)
	}
}
