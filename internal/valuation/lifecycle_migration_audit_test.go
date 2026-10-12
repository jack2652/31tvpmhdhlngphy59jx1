package valuation

import "testing"

func lifecycleAuditPtr(value float64) *float64 { return &value }

func TestDecisionTreeMobilityLifecycleMatchesPythonGrowthAndWinterGates(t *testing.T) {
	ev := func(marketCap, growth float64) Fundamentals {
		return Fundamentals{
			Sector: "Consumer Cyclical", Industry: "Electric Vehicles", IndustryKey: "electric-vehicles",
			MarketCap: lifecycleAuditPtr(marketCap), MarketCapDataQuality: "consistent",
			Revenue: lifecycleAuditPtr(100e9), FreeCashFlow: lifecycleAuditPtr(0),
			TrailingEPS: lifecycleAuditPtr(4), ForwardEPS: lifecycleAuditPtr(5), ForwardEPSObserved: true,
			GrowthRate: lifecycleAuditPtr(growth),
		}
	}
	tests := []struct {
		name string
		f    Fundamentals
		want string
	}{
		{name: "500 billion and 5 percent boundaries in winter without AI exposure", f: func() Fundamentals {
			f := ev(5e11, .05)
			f.Regime = RegimeWinter
			return f
		}(), want: "移动科技成长"},
		{name: "just below large EV market cap boundary", f: ev(5e11-1, .05), want: "普通"},
		{name: "just below minimum growth boundary", f: ev(5e11, .05-1e-9), want: "普通"},
		{name: "AI exposed winter disables mobility lifecycle", f: func() Fundamentals {
			f := ev(5e11, .05)
			f.Regime, f.AIRevenueShare = RegimeWinter, lifecycleAuditPtr(.10)
			return f
		}(), want: "强周期"},
		{name: "negative FCF transition yields to eligible mobility growth", f: func() Fundamentals {
			f := ev(5e11, .05)
			f.FreeCashFlow = lifecycleAuditPtr(-1)
			return f
		}(), want: "移动科技成长"},
		{name: "AI storage cycle precedes electric mobility growth", f: func() Fundamentals {
			f := ev(5e11, .20)
			f.Industry = "Electric Vehicles and Enterprise Storage"
			f.LongBusinessSummary = "Manufactures battery electric vehicles and enterprise storage systems."
			return f
		}(), want: "AI存储/硬件周期"},
		{name: "storage growth without observed forward EPS falls back to ordinary", f: Fundamentals{
			Sector: "Technology", Industry: "Computer Hardware Storage",
			LongBusinessSummary: "Manufactures enterprise storage systems.",
			GrowthRate:          lifecycleAuditPtr(.20), TrailingEPS: lifecycleAuditPtr(2),
		}, want: "普通"},
		{name: "strong mobility signals use the inclusive 100 billion threshold", f: Fundamentals{
			Sector: "Technology", Industry: "Software Infrastructure",
			LongBusinessSummary: "Builds robotaxi fleets and humanoid robotics platforms.",
			MarketCap:           lifecycleAuditPtr(1e11), MarketCapDataQuality: "consistent",
			GrowthRate: lifecycleAuditPtr(.05), ForwardEPS: lifecycleAuditPtr(2), ForwardEPSObserved: true,
		}, want: "移动科技成长"},
		{name: "strong mobility signals below 100 billion do not qualify", f: Fundamentals{
			Sector: "Technology", Industry: "Software Infrastructure",
			LongBusinessSummary: "Builds robotaxi fleets and humanoid robotics platforms.",
			MarketCap:           lifecycleAuditPtr(1e11 - 1), MarketCapDataQuality: "consistent",
			GrowthRate: lifecycleAuditPtr(.05), ForwardEPS: lifecycleAuditPtr(2), ForwardEPSObserved: true,
		}, want: "普通"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.f)
			if result.DecisionTree == nil || result.DecisionTree.Lifecycle != test.want {
				t.Fatalf("lifecycle=%v, want %q; method=%q", result.DecisionTree, test.want, result.Method)
			}
		})
	}
}

func TestDecisionTreeLifecycleUsesPythonPriorityForOverlappingSignals(t *testing.T) {
	tests := []struct {
		name string
		f    Fundamentals
		want string
	}{
		{
			name: "defense transition precedes AI storage cycle",
			f: Fundamentals{
				Sector: "Technology", Industry: "Semiconductor Memory", IndustryKey: "semiconductor-memory",
				LongBusinessSummary: "Defense contractor producing NAND memory chips and storage hardware.",
				GrowthRate:          lifecycleAuditPtr(.30), Revenue: lifecycleAuditPtr(100), FreeCashFlow: lifecycleAuditPtr(20),
				TrailingEPS: lifecycleAuditPtr(10), ForwardEPS: lifecycleAuditPtr(12), ForwardEPSObserved: true,
				DefenseRevenueGrowth: lifecycleAuditPtr(.30),
			},
			want: "国防订单驱动转型",
		},
		{
			name: "asset-heavy transition precedes ordinary cyclicality",
			f: Fundamentals{
				Sector: "Consumer Defensive", Industry: "Airlines",
				LongBusinessSummary: "Also operates a restaurant and food business.",
				GrowthRate:          lifecycleAuditPtr(.10), Revenue: lifecycleAuditPtr(100), FreeCashFlow: lifecycleAuditPtr(-10),
				ForwardEPS: lifecycleAuditPtr(2), ForwardEPSObserved: true,
			},
			want: "重资产转型",
		},
		{
			name: "cyclical industry precedes stable cashflow business text",
			f: Fundamentals{
				Sector: "Consumer Defensive", Industry: "Airlines",
				LongBusinessSummary: "Also operates a restaurant and food business.",
				GrowthRate:          lifecycleAuditPtr(.10), Revenue: lifecycleAuditPtr(100), FreeCashFlow: lifecycleAuditPtr(20),
				ForwardEPS: lifecycleAuditPtr(2), ForwardEPSObserved: true,
			},
			want: "强周期",
		},
		{
			name: "stable cashflow without cyclical industry signal",
			f: Fundamentals{
				Sector: "Consumer Defensive", Industry: "Restaurants",
				GrowthRate: lifecycleAuditPtr(.10), Revenue: lifecycleAuditPtr(100), FreeCashFlow: lifecycleAuditPtr(20),
				ForwardEPS: lifecycleAuditPtr(2), ForwardEPSObserved: true,
			},
			want: "稳定现金流",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Calculate(test.f)
			if result.DecisionTree == nil || result.DecisionTree.Lifecycle != test.want {
				t.Fatalf("lifecycle=%v, want %q; method=%q", result.DecisionTree, test.want, result.Method)
			}
		})
	}
}
