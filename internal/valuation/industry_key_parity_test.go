package valuation

import (
	"strings"
	"testing"
)

func TestIndustryKeyCategoryMapMatchesPythonV39(t *testing.T) {
	want := map[string]string{
		"semiconductor-memory":         "memory_chip",
		"semiconductors":               "semiconductor",
		"software-infrastructure":      "software",
		"software-application":         "software",
		"auto-manufacturers":           "automotive",
		"computer-hardware":            "computer_hardware",
		"aerospace-defense":            "defense",
		"internet-content-information": "internet_platform",
		"internet-retail":              "internet_platform",
		"reit-residential":             "reit",
		"reit-retail":                  "reit",
		"reit-office":                  "reit",
		"reit-diversified":             "reit",
		"banks-diversified":            "banking",
		"insurance-diversified":        "insurance",
		"biotechnology":                "biotech",
		"utilities-regulated-electric": "utility",
	}
	if len(industryKeyCategoryMap) != len(want) {
		t.Fatalf("industry key map has %d entries, want %d", len(industryKeyCategoryMap), len(want))
	}
	for key, expected := range want {
		if got := industryKeyCategory(key); got != expected {
			t.Errorf("industryKeyCategory(%q)=%q, want %q", key, got, expected)
		}
		underscore := strings.ReplaceAll(key, "-", "_")
		if got := industryKeyCategory(underscore); got != expected {
			t.Errorf("industryKeyCategory(%q)=%q, want %q after underscore normalization", underscore, got, expected)
		}
	}
}

func TestIndustryKeyCategoryDrivesSpecializedRoutingWithoutTextFields(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }
	result := Calculate(Fundamentals{
		IndustryKey: "reit_residential", FFOPerShare: ptr(2), AFFOPerShare: ptr(1.7), NAVPerShare: ptr(20),
	})
	if result.Method != MethodREIT || result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "reit" {
		t.Fatalf("REIT key routing=%#v, want the Python reit branch", result)
	}

	result = Calculate(Fundamentals{
		IndustryKey: "semiconductor_memory", ForwardEPS: ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(2),
	})
	if !strings.Contains(result.Method, "存储") {
		t.Fatalf("memory key routing method=%q, want storage-cycle valuation", result.Method)
	}
}

func TestIndustryKeyCategoryClassificationMatchesPythonWithoutTextFields(t *testing.T) {
	tests := []struct {
		key       string
		category  string
		filter    string
		lifecycle string
		aiExposed bool
	}{
		{key: "semiconductor-memory", category: "memory_chip", lifecycle: "强周期", aiExposed: true},
		{key: "semiconductors", category: "semiconductor", aiExposed: true},
		{key: "software-infrastructure", category: "software"},
		{key: "software-application", category: "software"},
		{key: "auto-manufacturers", category: "automotive"},
		{key: "computer-hardware", category: "computer_hardware"},
		{key: "aerospace-defense", category: "defense"},
		{key: "internet-content-information", category: "internet_platform", filter: "internet_platform", lifecycle: "internet_platform"},
		{key: "internet-retail", category: "internet_platform", filter: "internet_platform", lifecycle: "internet_platform"},
		{key: "reit-residential", category: "reit", filter: "reit", lifecycle: "reit"},
		{key: "reit-retail", category: "reit", filter: "reit", lifecycle: "reit"},
		{key: "reit-office", category: "reit", filter: "reit", lifecycle: "reit"},
		{key: "reit-diversified", category: "reit", filter: "reit", lifecycle: "reit"},
		{key: "banks-diversified", category: "banking", filter: "banking", lifecycle: "banking"},
		{key: "insurance-diversified", category: "insurance", filter: "insurance", lifecycle: "insurance"},
		{key: "biotechnology", category: "biotech", filter: "biotech", lifecycle: "biotech"},
		{key: "utilities-regulated-electric", category: "utility", filter: "utility", lifecycle: "utility"},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			result := Calculate(Fundamentals{IndustryKey: test.key})
			if got := industryKeyCategory(test.key); got != test.category {
				t.Fatalf("category=%q, want Python category %q", got, test.category)
			}
			if result.DecisionTree == nil {
				t.Fatal("decision tree is missing")
			}
			wantFilter := test.filter
			if wantFilter == "" {
				wantFilter = "普通行业"
				if test.category == "internet_platform" {
					wantFilter = "internet_platform"
				}
				if test.category == "biotech" {
					wantFilter = "biotech"
				}
				if test.category == "utility" {
					wantFilter = "utility"
				}
			}
			if result.DecisionTree.IndustryFilter != wantFilter {
				t.Errorf("industry filter=%q, want Python v39 category result %q", result.DecisionTree.IndustryFilter, wantFilter)
			}
			if result.Status != StatusUnavailable {
				t.Errorf("status=%q without model inputs, want unavailable", result.Status)
			}
			wantLifecycle := test.lifecycle
			if wantLifecycle == "" {
				wantLifecycle = "普通"
				if test.category == "internet_platform" {
					wantLifecycle = "internet_platform"
				}
				if test.category == "biotech" {
					wantLifecycle = "biotech"
				}
				if test.category == "utility" {
					wantLifecycle = "utility"
				}
			}
			if result.DecisionTree.Lifecycle != wantLifecycle {
				t.Errorf("lifecycle=%q, want Python v39 category result %q", result.DecisionTree.Lifecycle, wantLifecycle)
			}
			if result.AIExposed != test.aiExposed {
				t.Errorf("AIExposed=%t, want Python valuation value %t", result.AIExposed, test.aiExposed)
			}
		})
	}
}

func TestMarketCapQualityWarningsReachValuationResult(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }
	unitMismatch := Calculate(Fundamentals{
		CurrentPrice: ptr(100), SharesOutstanding: ptr(10), MarketCap: ptr(100001),
		MarketCapDataQuality: "unit_mismatch", SharesSource: "info", ForwardEPS: ptr(5),
	})
	if !containsWarning(unitMismatch.Warnings, "market_cap 与股本×现价存在数量级差异") {
		t.Fatalf("unit-mismatch warning missing from result: %#v", unitMismatch.Warnings)
	}

	reconciled := Calculate(Fundamentals{
		CurrentPrice: ptr(100), SharesOutstanding: ptr(10), MarketCap: ptr(1500),
		MarketCapDataQuality: "reconciled", SharesSource: "balance_sheet", ForwardEPS: ptr(5),
	})
	if !containsWarning(reconciled.Warnings, "market_cap 与股本×现价偏差超过 20%") {
		t.Fatalf("reconciled warning missing from result: %#v", reconciled.Warnings)
	}
}

func containsWarning(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}
