package valuation

import (
	"math"
	"strings"
	"testing"
)

func TestCalculateREITFixtures(t *testing.T) {
	fixtures := []struct {
		name         string
		fundamentals Fundamentals
		wantStatus   string
		wantMethod   string
		wantValue    float64
		wantLow      float64
		wantHigh     float64
		wantWarn     string
	}{
		{
			name: "reported FFO AFFO and NAV",
			fundamentals: Fundamentals{
				IsREIT: true, FFOPerShare: ptr(2.50), AFFOPerShare: ptr(2.00), NAVPerShare: ptr(25),
			},
			wantStatus: StatusReady, wantMethod: MethodREIT, wantValue: 30, wantLow: 24, wantHigh: 36,
			wantWarn: "property-level data was not available",
		},
		{
			name: "derive FFO and estimate AFFO",
			fundamentals: Fundamentals{
				IsREIT: true, SharesOutstanding: ptr(10), NetIncome: ptr(100), DepreciationAndAmortization: ptr(-20),
			},
			wantStatus: StatusReady, wantMethod: MethodREIT, wantValue: 153, wantLow: 122.40, wantHigh: 183.60,
			wantWarn: "AFFO was unavailable",
		},
		{
			name:         "NAV only",
			fundamentals: Fundamentals{IsREIT: true, NAVPerShare: ptr(100)},
			wantStatus:   StatusReady, wantMethod: MethodREIT, wantValue: 100, wantLow: 85, wantHigh: 115,
			wantWarn: "property-level data was not available",
		},
		{
			name: "missing REIT metrics is unavailable",
			fundamentals: Fundamentals{
				IsREIT: true, ForwardEPS: ptr(5), NormalizedEPS: ptr(4),
				NormalizedEPSSource: "历史 EPS 中位数", CurrentPrice: ptr(10),
				SharesOutstanding: ptr(10e9), MarketCap: ptr(100e9),
				Revenue: ptr(90e9), AnnualRevenue: []float64{80e9, 90e9},
			},
			wantStatus: StatusUnavailable, wantWarn: "REIT-specific FFO/AFFO/NAV data unavailable",
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			result := Calculate(fixture.fundamentals)
			if result.Status != fixture.wantStatus {
				t.Fatalf("status = %q, want %q; result=%#v", result.Status, fixture.wantStatus, result)
			}
			if result.Method != fixture.wantMethod {
				t.Fatalf("method = %q, want %q; result=%#v", result.Method, fixture.wantMethod, result)
			}
			if fixture.wantStatus == StatusUnavailable {
				if result.FairValue != nil || result.FairValueLow != nil || result.FairValueHigh != nil ||
					result.FairValueBuyLow != nil || result.FairValueBuyHigh != nil ||
					len(result.Methods) != 0 || result.Defensive != nil || result.Optimistic != nil {
					t.Fatalf("unavailable REIT must not expose a valuation: %#v", result)
				}
				if result.ForwardEPS == nil || *result.ForwardEPS != 5 ||
					result.NormalizedEPSSource != "历史 EPS 中位数" ||
					result.MarketCapDataQuality != "consistent" || result.DataQualityScore != 90 || !result.IndustryModel {
					t.Fatalf("REIT diagnostics were not preserved: %#v", result)
				}
				if result.DecisionTree == nil || result.DecisionTree.IndustryFilter != "reit" || result.DecisionTree.Lifecycle != "reit" {
					t.Fatalf("unavailable REIT lost specialist classification: %#v", result.DecisionTree)
				}
				if !strings.Contains(strings.Join(result.Warnings, " "), fixture.wantWarn) {
					t.Fatalf("warnings = %#v, want substring %q", result.Warnings, fixture.wantWarn)
				}
				return
			}
			assertFloat(t, result.FairValue, fixture.wantValue)
			assertFloat(t, result.FairValueLow, fixture.wantLow)
			assertFloat(t, result.FairValueHigh, fixture.wantHigh)
			if !strings.Contains(strings.Join(result.Warnings, " "), fixture.wantWarn) {
				t.Fatalf("warnings = %#v, want substring %q", result.Warnings, fixture.wantWarn)
			}
		})
	}
}

func TestCalculateREITRejectsNonFiniteMetrics(t *testing.T) {
	result := Calculate(Fundamentals{IsREIT: true, FFOPerShare: ptr(math.NaN())})
	if result.Status != StatusInvalid || len(result.Errors) == 0 {
		t.Fatalf("result = %#v, want invalid non-finite REIT input", result)
	}
}
