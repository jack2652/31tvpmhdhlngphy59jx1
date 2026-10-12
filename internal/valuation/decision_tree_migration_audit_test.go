package valuation

import "testing"

func TestSpecializedIndustryWithoutModelInputsDoesNotUseGenericOwnerEarnings(t *testing.T) {
	result := Calculate(Fundamentals{
		IndustryKey:       "biotechnology",
		OwnerEarnings:     floatPointer(100),
		SharesOutstanding: floatPointer(10),
	})

	if result.Status != StatusUnavailable {
		t.Fatalf("status=%q method=%q, want unavailable for a specialized industry without model inputs", result.Status, result.Method)
	}
	if result.Method != "生物科技：已上市药品 DCF + 管线风险调整 NPV" {
		t.Fatalf("method=%q, want the identified biotechnology model", result.Method)
	}
	if !result.IndustryModel {
		t.Fatal("industry model marker was lost on the unavailable result")
	}
}

func TestSlowdownAdjustsForwardEPSSourceForAIExposedChipModel(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Semiconductors", IndustryKey: "semiconductors",
		Revenue: floatPointer(100), SharesOutstanding: floatPointer(10),
		NormalizedEPS: floatPointer(5), ForwardEPS: floatPointer(10), ForwardEPSObserved: true,
		GrowthRate: floatPointer(.30), Regime: RegimeSlowdown,
	})

	if result.ForwardEPS == nil || *result.ForwardEPS != 7 {
		t.Fatalf("forward EPS=%v, want 7 after the Python 70%% slowdown adjustment", result.ForwardEPS)
	}
	if result.ForwardEPSSource != "Yahoo远期EPS（放缓调整70%）" {
		t.Fatalf("forward EPS source=%q, want slowdown-adjusted Yahoo source", result.ForwardEPSSource)
	}
}
