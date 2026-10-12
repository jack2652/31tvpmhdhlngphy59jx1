package server

import (
	"math"
	"testing"

	"option-scope-go/internal/provider"
	"option-scope-go/internal/valuation"
)

func TestValuationAdapterPrefersFreshFundamentalsPrice(t *testing.T) {
	quotePrice := 50.0
	fundamentals := provider.YahooFundamentals{
		Price: float64Pointer(120), MarketCap: float64Pointer(1200),
		Shares: float64Pointer(10), SharesSource: "balance_sheet",
	}
	inputs := valuationInputs("TEST", valuationCurrentPrice(fundamentals, &quotePrice), fundamentals)
	if inputs.CurrentPrice == nil || *inputs.CurrentPrice != 120 {
		t.Fatalf("valuation price=%v, want fresh fundamentals price 120", inputs.CurrentPrice)
	}
	if inputs.MarketCap == nil || *inputs.MarketCap != 1200 || inputs.MarketCapDataQuality != "consistent" {
		t.Fatalf("market cap reconciliation used stale quote price: cap=%v quality=%q", inputs.MarketCap, inputs.MarketCapDataQuality)
	}

	invalid := math.NaN()
	fundamentals.Price = &invalid
	fallback := valuationCurrentPrice(fundamentals, &quotePrice)
	if fallback == nil || *fallback != quotePrice {
		t.Fatalf("invalid fundamentals price fallback=%v, want quote price %v", fallback, quotePrice)
	}
	if got := valuationCurrentPrice(provider.YahooFundamentals{}, nil); got != nil {
		t.Fatalf("missing prices should remain unavailable, got %v", *got)
	}
}

func TestValuationAdapterTrustsInfoSharesForMarketCapReconciliation(t *testing.T) {
	fundamentals := provider.YahooFundamentals{
		Price:        float64Pointer(10),
		MarketCap:    float64Pointer(1000),
		Shares:       float64Pointer(10),
		SharesSource: "info",
	}
	inputs := valuationInputs("TEST", fundamentals.Price, fundamentals)
	if inputs.MarketCap == nil || *inputs.MarketCap != 100 {
		t.Fatalf("market cap=%v, want info shares reconciliation to 100", inputs.MarketCap)
	}
	if inputs.MarketCapDataQuality != "reconciled" {
		t.Fatalf("market cap quality=%q, want reconciled", inputs.MarketCapDataQuality)
	}
	result := valuation.Calculate(inputs)
	want := "market_cap 与股本×现价偏差超过 20%，已使用info×现价校正市值"
	for _, warning := range result.Warnings {
		if warning == want {
			return
		}
	}
	t.Fatalf("market cap warning=%#v, want %q", result.Warnings, want)
}

func TestAPIValuationConfidenceUsesRawInterestCoverageThreshold(t *testing.T) {
	fairValue := 100.0
	roundedCoverage := 2.0
	base := valuation.Result{
		FairValue:        &fairValue,
		Status:           valuation.StatusReady,
		InterestCoverage: &roundedCoverage,
		DataQualityScore: 100,
		Defensive:        map[string]any{"value": 100.0, "low": 100.0},
		Optimistic:       map[string]any{"high": 100.0},
	}
	above := valuation.Fundamentals{
		OperatingIncome: float64Pointer(2.001), InterestExpense: float64Pointer(1), Revenue: float64Pointer(1),
	}
	below := valuation.Fundamentals{
		OperatingIncome: float64Pointer(1.999), InterestExpense: float64Pointer(1), Revenue: float64Pointer(1),
	}
	high, _ := apiValuationConfidence(above, base)
	low, _ := apiValuationConfidence(below, base)
	if high != 100 || low != 85 {
		t.Fatalf("confidence above/below raw 2x threshold=%v/%v, want 100/85", high, low)
	}
}

func TestValuationAdapterMatureGrowthLifecycleThresholds(t *testing.T) {
	base := func() provider.YahooFundamentals {
		return provider.YahooFundamentals{
			Sector: "Technology", Industry: "Software Application",
			MarketCap: float64Pointer(500e9), Revenue: float64Pointer(100e9),
			FreeCashflow: float64Pointer(20e9), RevenueGrowth: float64Pointer(.06),
			EPS: float64Pointer(5), ForwardEPS: float64Pointer(6), Shares: float64Pointer(10e9),
		}
	}
	tests := []struct {
		name       string
		mutate     func(*provider.YahooFundamentals)
		wantMature bool
	}{
		{name: "inclusive threshold boundaries", wantMature: true},
		{name: "market cap below 500 billion", mutate: func(f *provider.YahooFundamentals) { *f.MarketCap = 500e9 - 1 }, wantMature: false},
		{name: "FCF margin below 20 percent", mutate: func(f *provider.YahooFundamentals) { *f.FreeCashflow = 20e9 - 1 }, wantMature: false},
		{name: "growth below 6 percent", mutate: func(f *provider.YahooFundamentals) { *f.RevenueGrowth = .06 - 1e-9 }, wantMature: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fundamentals := base()
			if test.mutate != nil {
				test.mutate(&fundamentals)
			}
			result := valuation.Calculate(valuationInputs("TEST", nil, fundamentals))
			got := result.Method == "成熟增长型巨头：远期正常化 EPS × PE"
			if got != test.wantMature {
				t.Fatalf("mature-growth=%v, want %v; result=%#v", got, test.wantMature, result)
			}
		})
	}
}

func TestValuationAdapterTreatsNonFiniteRevenueGrowthAsMissing(t *testing.T) {
	base := provider.YahooFundamentals{
		Sector: "Technology", Industry: "Software Application",
		MarketCap: float64Pointer(1.1e12), Revenue: float64Pointer(100e9),
		FreeCashflow: float64Pointer(25e9), AnnualEPS: []float64{10.2, 10},
	}
	withoutHint := valuationInputs("TEST", nil, base)
	if withoutHint.RevenueGrowth != nil || withoutHint.GrowthHintObserved {
		t.Fatalf("unreported revenue growth should remain absent: revenue=%v observed=%v", withoutHint.RevenueGrowth, withoutHint.GrowthHintObserved)
	}
	want := valuation.Calculate(withoutHint)
	if want.Method != "成熟增长型巨头：远期正常化 EPS × PE" {
		t.Fatalf("1T company with no reported growth hint should use the Python mature-growth fallback: %#v", want)
	}

	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		fundamentals := base
		fundamentals.RevenueGrowth = &invalid
		inputs := valuationInputs("TEST", nil, fundamentals)
		if inputs.RevenueGrowth != nil || inputs.GrowthHintObserved {
			t.Fatalf("non-finite revenue growth %v was not normalized to missing: %#v", invalid, inputs)
		}
		got := valuation.Calculate(inputs)
		if got.Method != want.Method || got.DecisionTree == nil || want.DecisionTree == nil || got.DecisionTree.Lifecycle != want.DecisionTree.Lifecycle {
			t.Fatalf("non-finite revenue growth %v changed missing-growth fallback: got=%#v want=%#v", invalid, got, want)
		}
	}
}

func TestValuationAdapterStableCashflowUsesStrictPythonBoundaries(t *testing.T) {
	base := func() provider.YahooFundamentals {
		return provider.YahooFundamentals{
			Sector: "Consumer Defensive", Industry: "Restaurants",
			Revenue: float64Pointer(100), FreeCashflow: float64Pointer(16),
			EarningsGrowth: float64Pointer(.149999), EPS: float64Pointer(2), ForwardEPS: float64Pointer(2.2),
		}
	}
	tests := []struct {
		name       string
		mutate     func(*provider.YahooFundamentals)
		wantStable bool
	}{
		{name: "just inside strict boundaries", wantStable: true},
		{name: "growth at 15 percent is excluded", mutate: func(f *provider.YahooFundamentals) { *f.EarningsGrowth = .15 }, wantStable: false},
		{name: "FCF margin at 15 percent is excluded", mutate: func(f *provider.YahooFundamentals) { *f.FreeCashflow = 15 }, wantStable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fundamentals := base()
			if test.mutate != nil {
				test.mutate(&fundamentals)
			}
			result := valuation.Calculate(valuationInputs("TEST", nil, fundamentals))
			got := result.DecisionTree != nil && result.DecisionTree.Lifecycle == "稳定现金流"
			if got != test.wantStable {
				t.Fatalf("stable-cashflow=%v, want %v; result=%#v", got, test.wantStable, result)
			}
		})
	}
}

func TestValuationAdapterKeepsREITPriorityWhenSpecialistInputsAreMissing(t *testing.T) {
	fundamentals := provider.YahooFundamentals{
		Sector: "Real Estate", Industry: "REIT - Retail", IndustryKey: "reit-retail",
		Revenue: float64Pointer(100), Shares: float64Pointer(10), FreeCashflow: float64Pointer(-20),
		EarningsGrowth: float64Pointer(.35),
	}
	result := valuation.Calculate(valuationInputs("TEST", nil, fundamentals))
	if result.Status != valuation.StatusUnavailable || result.FairValue != nil || result.Warning != "REIT-specific FFO/AFFO/NAV data unavailable; valuation was not generated" {
		t.Fatalf("missing REIT metrics should remain unavailable under the specialist model: %#v", result)
	}
	if !result.IndustryModel {
		t.Fatalf("REIT specialist classification was lost to generic lifecycle signals: %#v", result)
	}

	withAFFO := fundamentals
	withAFFO.AFFOPerShare = float64Pointer(2)
	result = valuation.Calculate(valuationInputs("TEST", nil, withAFFO))
	if result.Method != valuation.MethodREIT || result.DecisionTree == nil || result.DecisionTree.Lifecycle != "reit" {
		t.Fatalf("REIT specialist model should outrank growth and negative-FCF signals: %#v", result)
	}
}
