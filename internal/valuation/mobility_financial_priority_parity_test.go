package valuation

import "testing"

func TestFinancialIndustryFilterPrecedesMobilityNarrativeLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Financial Services", Industry: "Auto Manufacturers",
		LongBusinessSummary: "Develops autonomous self-driving robotaxi technology.",
		CurrentPrice:        floatPointer(100), SharesOutstanding: floatPointer(2e9),
		MarketCap: floatPointer(200e9), MarketCapDataQuality: "consistent",
		Revenue: floatPointer(50e9), GrowthRate: floatPointer(.15),
		BookValue: floatPointer(10), DividendRate: floatPointer(1),
	})

	if result.Method != "金融/地产行业过滤：DDM + P/B" {
		t.Fatalf("method=%q, want Python financial industry filter despite mobility narrative", result.Method)
	}
	if _, ok := result.Methods["DDM"]; !ok {
		t.Fatalf("financial candidates=%#v, want DDM", result.Methods)
	}
	if _, ok := result.Methods["移动科技远期 P/S"]; ok {
		t.Fatalf("mobility candidate escaped the Python financial industry filter: %#v", result.Methods)
	}
	if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "金融/地产" {
		t.Fatalf("decision tree=%#v, want financial industry filter", result.DecisionTree)
	}
}
