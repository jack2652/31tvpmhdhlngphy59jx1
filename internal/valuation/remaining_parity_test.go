package valuation

import (
	"strings"
	"testing"
)

func TestSlowdownAIStorageCycleKeepsPythonMainPEBand(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductor-memory", Industry: "Semiconductor Memory", Sector: "Technology",
		ForwardEPS: ptr(10), ForwardEPSObserved: true, TrailingEPS: ptr(2),
		GrowthRate: ptr(.25), Regime: RegimeSlowdown,
	})
	if result.PELow == nil || *result.PELow != 20 || result.PEHigh == nil || *result.PEHigh != 30 {
		t.Fatalf("main PE band=%v-%v, want Python's 20-30 under slowdown", result.PELow, result.PEHigh)
	}
}

func TestMaturePremiumPEBandUsesRevenueGrowthLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Software Infrastructure", IndustryKey: "software-infrastructure", Sector: "Technology",
		MarketCap: ptr(1e12), MarketCapDataQuality: "consistent", Revenue: ptr(100e9), FreeCashFlow: ptr(30e9),
		NormalizedEPS: ptr(8), ForwardEPS: ptr(9), ForwardEPSObserved: true,
		GrowthRate: ptr(.05), RevenueGrowth: ptr(.10),
	})
	if !strings.Contains(result.Method, "成熟增长型巨头") {
		t.Fatalf("method=%q, want mature-growth model", result.Method)
	}
	if result.PELow == nil || *result.PELow != 30 || result.PEHigh == nil || *result.PEHigh != 38 {
		t.Fatalf("mature PE band=%v-%v, want Python's 30-38 from 10%% revenue growth", result.PELow, result.PEHigh)
	}
}

func TestMemoryWithoutAnalystForwardEPSUsesCyclicalMedianLikePython(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey: "semiconductor-memory", Industry: "Semiconductor Memory", Sector: "Technology",
		TrailingEPS: ptr(8), AnnualEPS: []float64{8, 6, 4, 3}, GrowthRate: ptr(.05),
	})
	if result.Method != "周期中值 EPS × PE" {
		t.Fatalf("method=%q, want Python's cyclical median EPS model", result.Method)
	}
	if result.PELow == nil || *result.PELow != 10 || result.PEHigh == nil || *result.PEHigh != 15 {
		t.Fatalf("memory fallback PE band=%v-%v, want 10-15", result.PELow, result.PEHigh)
	}
	if result.ForwardEPS != nil {
		t.Fatalf("forward EPS=%v, want nil without an analyst estimate", *result.ForwardEPS)
	}
}

func TestGenericAIExposureDoesNotTreatNarrativeKeywordsAsRevenueExposure(t *testing.T) {
	result := Calculate(Fundamentals{
		Industry: "Software Infrastructure", IndustryKey: "software-infrastructure", Sector: "Technology",
		LongBusinessSummary: "Provides artificial intelligence tools, autonomous workflow software, and robotics research.",
		TrailingEPS:         ptr(10), ForwardEPS: ptr(12), ForwardEPSObserved: true,
		GrowthRate: ptr(.10), Regime: RegimeSlowdown,
	})
	if result.AIExposed {
		t.Fatal("ordinary software narrative keywords alone must not establish AI revenue exposure")
	}
	if result.ForwardEPS == nil || *result.ForwardEPS != 12 {
		t.Fatalf("forward EPS=%v, want 12 without an AI slowdown haircut", result.ForwardEPS)
	}
}
