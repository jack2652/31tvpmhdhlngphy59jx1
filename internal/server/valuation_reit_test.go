package server

import (
	"testing"

	"option-scope-go/internal/provider"
	"option-scope-go/internal/valuation"
)

func TestValuationInputsREITFixtures(t *testing.T) {
	fixtures := []struct {
		name         string
		fundamentals provider.YahooFundamentals
		wantREIT     bool
		wantMethod   string
		wantMargin   float64
	}{
		{
			name: "stable industry key with reported metrics",
			fundamentals: provider.YahooFundamentals{
				IndustryKey: "reit-residential", Shares: floatPtrFixture(10),
				FFOPerShare: floatPtrFixture(2.5), AFFOPerShare: floatPtrFixture(2), NAVPerShare: floatPtrFixture(25),
			},
			wantREIT: true, wantMethod: valuation.MethodREIT, wantMargin: 0.25,
		},
		{
			name: "display name fallback with derived FFO",
			fundamentals: provider.YahooFundamentals{
				Sector: "Real Estate", Industry: "Real Estate Investment Trust",
				Shares: floatPtrFixture(10), NetIncome: floatPtrFixture(100), DepreciationAndAmortization: floatPtrFixture(-20),
			},
			wantREIT: true, wantMethod: valuation.MethodREIT, wantMargin: 0.25,
		},
		{
			name: "high beta adds five percentage points",
			fundamentals: provider.YahooFundamentals{
				IndustryKey: "reit-residential", Beta: floatPtrFixture(1.8), FFOPerShare: floatPtrFixture(2.5),
			},
			wantREIT: true, wantMethod: valuation.MethodREIT, wantMargin: 0.30,
		},
		{
			name: "ordinary real estate uses financial industry filter",
			fundamentals: provider.YahooFundamentals{
				Sector: "Real Estate", Industry: "Real Estate Services", ForwardEPS: floatPtrFixture(5),
				BookValue: floatPtrFixture(40),
			},
			wantREIT: false, wantMethod: "金融/地产行业过滤：DDM + P/B", wantMargin: 0.30,
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			inputs := valuationInputs("FIX", floatPtrFixture(100), fixture.fundamentals)
			if got := inputs.IsREIT; got != fixture.wantREIT {
				t.Fatalf("IsREIT = %v, want %v; inputs=%#v", got, fixture.wantREIT, inputs)
			}
			if fixture.wantREIT && (inputs.SafetyMargin == nil || *inputs.SafetyMargin != fixture.wantMargin) {
				t.Fatalf("input safety margin = %v, want %v for REIT", inputs.SafetyMargin, fixture.wantMargin)
			}
			if !fixture.wantREIT && inputs.SafetyMargin != nil {
				t.Fatalf("ordinary company safety margin = %v, want classification default", *inputs.SafetyMargin)
			}
			result := valuation.Calculate(inputs)
			if result.Method != fixture.wantMethod {
				t.Fatalf("method = %q, want %q; result=%#v", result.Method, fixture.wantMethod, result)
			}
			if fixture.wantREIT && result.SafetyMargin != fixture.wantMargin {
				t.Fatalf("result safety margin = %v, want %v", result.SafetyMargin, fixture.wantMargin)
			}
		})
	}
}

func TestValuationInputsPassesYahooIndustryContext(t *testing.T) {
	defenseGrowth := 0.12
	fundamentals := provider.YahooFundamentals{
		DefenseRevenueGrowth: &defenseGrowth,
		LongBusinessSummary:  "defense contractor with recurring government programs",
	}

	inputs := valuationInputs("FIX", floatPtrFixture(100), fundamentals)
	if inputs.DefenseRevenueGrowth == nil || *inputs.DefenseRevenueGrowth != defenseGrowth {
		t.Fatalf("DefenseRevenueGrowth = %v, want %v", inputs.DefenseRevenueGrowth, defenseGrowth)
	}
	if inputs.LongBusinessSummary != fundamentals.LongBusinessSummary {
		t.Fatalf("LongBusinessSummary = %q, want %q", inputs.LongBusinessSummary, fundamentals.LongBusinessSummary)
	}
}

func TestValuationInputsLeavesGenericMarginToPythonClassification(t *testing.T) {
	fundamentals := provider.YahooFundamentals{
		Sector: "Technology", Industry: "Software Infrastructure", IndustryKey: "software-infrastructure",
		Beta: floatPtrFixture(2), ForwardEPS: floatPtrFixture(2), FreeCashflow: floatPtrFixture(-10),
	}
	inputs := valuationInputs("FIX", floatPtrFixture(20), fundamentals)
	if inputs.SafetyMargin != nil {
		t.Fatalf("generic provider safety margin = %v, want Go classification to choose it", *inputs.SafetyMargin)
	}
	result := valuation.Calculate(inputs)
	if result.SafetyMargin != .52 {
		t.Fatalf("generic negative-FCF high-beta margin = %v, want Python transition margin 0.47 plus beta adjustment 0.05; result=%#v", result.SafetyMargin, result)
	}
}

func TestValuationInputsPassesYahooInstrumentType(t *testing.T) {
	inputs := valuationInputs("QQQ", floatPtrFixture(751.27), provider.YahooFundamentals{
		QuoteType: "ETF", EPS: floatPtrFixture(25), ForwardEPS: floatPtrFixture(30),
		FreeCashflow: floatPtrFixture(1e12), RevenueGrowth: floatPtrFixture(.25),
	})
	if inputs.InstrumentType != "ETF" {
		t.Fatalf("instrument type = %q, want ETF", inputs.InstrumentType)
	}
	result := valuation.Calculate(inputs)
	if result.FairValue != nil || result.Defensive != nil || result.Optimistic != nil || result.Status != valuation.StatusUnavailable {
		t.Fatalf("ETF valuation = %#v, want unavailable company-based valuation", result)
	}
}

func TestOwnerMaintenanceRatioUsesPythonClassificationSignals(t *testing.T) {
	negativeFCF, capex, operating := -10.0, 70.0, 100.0
	tests := []struct {
		name string
		data provider.YahooFundamentals
		want float64
	}{
		{
			name: "memory business summary excludes asset-heavy transition",
			data: provider.YahooFundamentals{
				Sector: "Technology", Industry: "Software Infrastructure",
				LongBusinessSummary: "Provides flash memory technology", FreeCashflow: &negativeFCF,
			},
			want: .50,
		},
		{
			name: "defense signal in business summary excludes asset-heavy transition",
			data: provider.YahooFundamentals{
				Sector: "Technology", Industry: "Software Infrastructure",
				LongBusinessSummary: "Develops directed energy laser systems", FreeCashflow: &negativeFCF,
			},
			want: .50,
		},
		{
			name: "defense revenue growth excludes asset-heavy transition",
			data: provider.YahooFundamentals{
				Sector: "Technology", Industry: "Software Infrastructure",
				DefenseRevenueGrowth: floatPtrFixture(.35), FreeCashflow: &negativeFCF,
			},
			want: .50,
		},
		{
			name: "storage hardware growth excludes asset-heavy transition",
			data: provider.YahooFundamentals{
				Sector: "Technology", Industry: "Computer Hardware",
				LongBusinessSummary: "Builds enterprise storage products", RevenueGrowth: floatPtrFixture(.25),
				FreeCashflow: &negativeFCF,
			},
			want: .50,
		},
		{
			name: "computer hardware industry key activates storage classification",
			data: provider.YahooFundamentals{
				Sector: "Technology", IndustryKey: "computer-hardware",
				LongBusinessSummary: "Provides storage infrastructure", RevenueGrowth: floatPtrFixture(.25),
				FreeCashflow: &negativeFCF,
			},
			want: .50,
		},
		{
			name: "defense growth hint excludes asset-heavy transition",
			data: provider.YahooFundamentals{
				Industry: "Aerospace & Defense", EarningsGrowth: floatPtrFixture(.12),
				CapitalExpenditure: &capex, OperatingCashflow: &operating,
			},
			want: .70,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ownerMaintenanceRatio(test.data); got != test.want {
				t.Fatalf("maintenance ratio=%v, want Python classification result %v", got, test.want)
			}
		})
	}
}

func floatPtrFixture(value float64) *float64 { return &value }
