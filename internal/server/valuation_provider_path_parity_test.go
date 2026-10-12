package server

import (
	"math"
	"testing"

	"option-scope-go/internal/provider"
	"option-scope-go/internal/valuation"
)

func TestProviderValuationWithoutIndustryKeyUsesTextRiskClassification(t *testing.T) {
	fundamentals := provider.YahooFundamentals{
		Sector: "Technology", Beta: floatPtrFixture(2), ForwardEPS: floatPtrFixture(2),
	}
	inputs := valuationInputs("FIX", floatPtrFixture(20), fundamentals)
	if !inputs.ProviderBacked {
		t.Fatal("Yahoo adapter did not mark provider-backed valuation input")
	}
	if inputs.IndustryKey != "" {
		t.Fatalf("fixture unexpectedly has industryKey %q", inputs.IndustryKey)
	}
	if inputs.SafetyMargin != nil {
		t.Fatalf("ordinary provider margin=%v, want lifecycle classifier to choose it", *inputs.SafetyMargin)
	}
	result := valuation.Calculate(inputs)
	if result.Status != valuation.StatusReady || result.SafetyMargin != .35 {
		t.Fatalf("provider-backed text classification margin/status=%v/%q, want high-beta default 0.35/ready; result=%#v", result.SafetyMargin, result.Status, result)
	}
	confidenceInputs := inputs
	confidenceInputs.FreeCashFlow = floatPtrFixture(-10)
	confidenceResult := valuation.Calculate(confidenceInputs)
	confidence, _ := apiValuationConfidence(confidenceInputs, confidenceResult)
	withoutRiskSignals := confidenceInputs
	withoutRiskSignals.Beta = nil
	withoutRiskSignals.FreeCashFlow = floatPtrFixture(0)
	baselineConfidence, _ := apiValuationConfidence(withoutRiskSignals, confidenceResult)
	if math.Abs(baselineConfidence-confidence-30) > 1e-9 {
		t.Fatalf("provider confidence omitted beta/negative-FCF penalties: baseline=%v risky=%v, want 30 point difference", baselineConfidence, confidence)
	}

	// Provider-independent callers retain the old 30% default when the marker
	// and stable industry key are both absent.
	legacy := valuation.Calculate(valuation.Fundamentals{
		Sector: "Technology", Beta: floatPtrFixture(2), ForwardEPS: floatPtrFixture(2), ForwardEPSObserved: true,
	})
	if legacy.SafetyMargin != .30 {
		t.Fatalf("provider-independent legacy margin=%v, want unchanged 0.30", legacy.SafetyMargin)
	}
}
