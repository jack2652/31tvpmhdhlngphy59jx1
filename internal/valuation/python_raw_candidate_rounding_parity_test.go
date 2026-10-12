package valuation

import "testing"

func TestProviderCandidateSelectionUsesPythonPreRoundingValidity(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }

	// Python v39 golden: MarketDataProvider._conservative_earnings({},
	// forward_eps=0.0001, growth_hint=0.0) keeps the raw $0.002 Forward EPS x PE
	// candidate, returns value 0.0 after display rounding, and selects the
	// ordinary forward-PE model.
	got := Calculate(Fundamentals{
		ProviderBacked: true,
		ForwardEPS:     ptr(0.0001),
		GrowthRate:     ptr(0),
	})
	if got.Status != StatusReady {
		t.Fatalf("status=%q, want ready for a positive raw candidate; warning=%q", got.Status, got.Warning)
	}
	if got.Method != MethodForwardPE {
		t.Fatalf("method=%q, want %q", got.Method, MethodForwardPE)
	}
	if got.FairValue == nil || *got.FairValue != 0 {
		t.Fatalf("fair value=%v, want Python-rounded 0.00", got.FairValue)
	}
	if candidate, ok := got.Methods[MethodForwardPE]; !ok || candidate != 0 {
		t.Fatalf("display method candidate=%v (present=%v), want rounded 0.00", candidate, ok)
	}
}

func TestProviderNonPositiveForwardEPSDoesNotCreateCandidate(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }
	for _, forwardEPS := range []float64{0, -0.0001} {
		got := Calculate(Fundamentals{
			ProviderBacked: true,
			ForwardEPS:     ptr(forwardEPS),
			GrowthRate:     ptr(0),
		})
		if got.Status != StatusUnavailable || got.FairValue != nil {
			t.Errorf("forward EPS %v: status=%q fair value=%v, want Python unavailable with no value", forwardEPS, got.Status, got.FairValue)
		}
	}
}
