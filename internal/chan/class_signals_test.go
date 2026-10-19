package chananalysis

import "testing"

func TestClass2RequiresFirstLowerLevelRetestAndParentLevelRecovery(t *testing.T) {
	parent := V4Signal{SignalID: "parent-buy", Level: 1, Kind: "class1_buy", Date: "10", ObservedAt: "11", ConfirmedAt: "12", Invalidation: ptr(90), StructureRefs: []int{0, 1, 2}}
	parentLevel := LevelAnalysis{Level: 1, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 1, Direction: "up", Start: "01", End: "10", Confirmed: true, ConfirmedAt: "11"},
		{Chain: 3, Level: 1, Direction: "down", Start: "10", End: "16", Confirmed: true, ConfirmedAt: "16"},
		{Chain: 3, Level: 1, Direction: "up", Start: "16", End: "17", Confirmed: true, ConfirmedAt: "17"},
	}}
	lower := LevelAnalysis{Level: 0, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 0, Direction: "up", Start: "12", End: "14", Low: 90, High: 110, EndPrice: 110, Confirmed: true, ConfirmedAt: "14"},  // first countertrend move
		{Chain: 3, Level: 0, Direction: "down", Start: "14", End: "16", Low: 96, High: 110, EndPrice: 96, Confirmed: true, ConfirmedAt: "16"}, // first valid retest
		{Chain: 3, Level: 0, Direction: "up", Start: "16", End: "17", Low: 96, High: 105, EndPrice: 105, Confirmed: true, ConfirmedAt: "17"},
		{Chain: 3, Level: 0, Direction: "down", Start: "17", End: "18", Low: 95, High: 105, EndPrice: 95, Confirmed: true, ConfirmedAt: "18"}, // repeated retest must not replace first
	}}
	levels := []LevelAnalysis{parentLevel, lower}
	waiting := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, {Level: 0, Status: "awaiting_lower_level"}})
	if waiting.Status != "awaiting_lower_level" || waiting.LowerLevelStatus != "awaiting_lower_level" || len(waiting.Missing) == 0 {
		t.Fatalf("missing lower-level status=%+v, want explicit waiting state", waiting)
	}
	candidate := class2Signal(Frame{}, parent, levels)
	if candidate.Kind != "class2_buy" || candidate.Status != "confirmed" || candidate.Date != "16" || candidate.ObservedAt != "16" || candidate.ConfirmedAt != "17" || len(candidate.StructureIDs) != 4 || candidate.RelatedSignalID != parent.SignalID {
		t.Fatalf("class-2 candidate=%+v, want first lower-level retest and next parent-level recovery", candidate)
	}
}

func TestClass2IgnoresLowerSegmentsStartedBeforeParentConfirmation(t *testing.T) {
	parent := V4Signal{SignalID: "parent-buy", Level: 1, Kind: "class1_buy", Date: "10", ObservedAt: "11", ConfirmedAt: "15", Invalidation: ptr(90), StructureRefs: []int{0}}
	parentLevel := LevelAnalysis{Level: 1, Segments: []Segment{{Chain: 1, Direction: "down", Start: "01", End: "10"}}}
	lower := LevelAnalysis{Level: 0, Status: "ready", Segments: []Segment{
		{Chain: 1, Level: 0, Direction: "up", Start: "12", End: "14", Confirmed: true, ConfirmedAt: "14"}, // began before parent confirmation
		{Chain: 1, Level: 0, Direction: "down", Start: "14", End: "16", Confirmed: true, ConfirmedAt: "16"},
	}}
	got := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, lower})
	if got.Status != "awaiting_lower_level" || len(got.Missing) == 0 || len(got.StructureIDs) != 1 {
		t.Fatalf("class2 used a lower-level segment that started before class1 confirmation: %+v", got)
	}
}

func TestClass2RejectsBoundaryTouchOnFirstRetest(t *testing.T) {
	parent := V4Signal{SignalID: "parent-sell", Level: 1, Kind: "class1_sell", Date: "10", ObservedAt: "11", ConfirmedAt: "12", Invalidation: ptr(120), StructureRefs: []int{0}}
	levels := []LevelAnalysis{
		{Level: 1, Status: "ready", Segments: []Segment{{Chain: 1, Direction: "down", Start: "01", End: "10"}}},
		{Level: 0, Status: "ready", Segments: []Segment{
			{Chain: 1, Level: 0, Direction: "down", Start: "12", End: "14", Low: 90, High: 120, Confirmed: true, ConfirmedAt: "14"},
			{Chain: 1, Level: 0, Direction: "up", Start: "14", End: "16", Low: 90, High: 120, Confirmed: true, ConfirmedAt: "16"},
			{Chain: 1, Level: 0, Direction: "up", Start: "16", End: "18", Low: 91, High: 115, Confirmed: true, ConfirmedAt: "18"},
		}},
	}
	candidate := class2Signal(Frame{}, parent, levels)
	if candidate.Status != "invalidated" || candidate.Date != "16" || candidate.ConfirmedAt != "16" {
		t.Fatalf("boundary retest=%+v, first retest touching invalidation must invalidate", candidate)
	}
}

func TestClass2InvalidationPreservesChronologicalConfirmationHistory(t *testing.T) {
	t.Run("invalidation before recovery confirmation", func(t *testing.T) {
		parent, parentLevel, lower := classTwoFixture()
		parentLevel.Segments[2].End = "18"
		parentLevel.Segments[2].ConfirmedAt = "18"
		got := class2Signal(Frame{Candles: []Bar{{Date: "17", Low: 89}}}, parent, []LevelAnalysis{parentLevel, lower})
		if got.Status != "invalidated" || got.PhaseAt != "17" || got.ConfirmedAt != "" || len(got.PhaseHistory) != 2 || got.PhaseHistory[0].Phase != "first_retest" || got.PhaseHistory[1].Phase != "invalidated" {
			t.Fatalf("pre-confirmation invalidation=%+v, want no later confirmation event", got)
		}
		if got.Detail != "二类候选在本级恢复确认前触及一类点失效边界，候选失效" {
			t.Fatalf("pre-confirmation detail=%q", got.Detail)
		}
	})

	t.Run("invalidation after recovery confirmation", func(t *testing.T) {
		parent, parentLevel, lower := classTwoFixture()
		got := class2Signal(Frame{Candles: []Bar{{Date: "18", Low: 89}}}, parent, []LevelAnalysis{parentLevel, lower})
		if got.Status != "invalidated" || got.PhaseAt != "18" || got.ConfirmedAt != "17" || len(got.PhaseHistory) != 3 {
			t.Fatalf("post-confirmation invalidation=%+v, want confirmation retained before invalidation", got)
		}
		if got.PhaseHistory[1] != (SignalPhase{Phase: "class2_confirmed", At: "17"}) || got.PhaseHistory[2] != (SignalPhase{Phase: "invalidated", At: "18"}) {
			t.Fatalf("post-confirmation phase history=%+v", got.PhaseHistory)
		}
		if got.Detail != "二类点已确认，之后触及一类点失效边界，信号失效" {
			t.Fatalf("post-confirmation detail=%q", got.Detail)
		}
	})
}

func TestClass3RequiresFirstRetestStrictlyOutsideCenter(t *testing.T) {
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "center-1", Lower: 5, Upper: 10, ConfirmedAt: "03", EndSegment: 2}
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", Confirmed: true, ConfirmedAt: "02"},
		{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "03", Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Chain: 0, Direction: "up", Start: "03", End: "04", Confirmed: true, ConfirmedAt: "04"},
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "05", EndPrice: 15, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "05"},   // departure
		{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", EndPrice: 12, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "06"}, // equals ZG: invalidated
	}
	if got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}}); len(got) != 1 || got[0].Kind != "class3_buy" || got[0].Status != "invalidated" {
		t.Fatalf("boundary-touch class-3 signals=%+v, want retained invalidated candidate", got)
	}
	segments[4].Low, segments[4].EndPrice = 11, 11
	got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Kind != "class3_buy" || got[0].Status != "confirmed" || got[0].Date != "06" || got[0].ObservedAt != "06" || got[0].RelatedCenterID != center.CenterID || len(got[0].StructureIDs) != 3 {
		t.Fatalf("strictly outside class-3 candidate=%+v", got)
	}
}

func TestClass3RetestTouchingOrEnteringCenterIsRetainedAsInvalidated(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			level := classThreeFixture()
			level.Segments[4].Low, level.Segments[4].EndPrice = 10, 10
			if side == "sell" {
				level, _ = mirrorSignalFixture(level, nil)
			}
			// The canonical buy retest touches ZG; the mirrored sell retest
			// touches ZD. Both are invalid because equality is not strictly outside.
			got := class3Signals(Frame{}, []LevelAnalysis{level})
			wantKind := "class3_" + side
			if len(got) != 1 || got[0].Kind != wantKind || got[0].Status != "invalidated" || got[0].Phase != "invalidated" || got[0].PhaseAt != "06" || got[0].ConfirmedAt != "" || len(got[0].Missing) != 0 || len(got[0].StructureIDs) != 3 {
				t.Fatalf("boundary-touch class-3 %s=%+v, want retained invalidated first-retest evidence", side, got)
			}
		})
	}
}

func TestClass3ReportsDepartureAndFormingFirstRetestAsWaiting(t *testing.T) {
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "center-1", Lower: 5, Upper: 10, ConfirmedAt: "03", End: "04", EndSegment: 2}
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", Confirmed: true, ConfirmedAt: "02"},
		{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "03", Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Chain: 0, Direction: "up", Start: "03", End: "04", Confirmed: true, ConfirmedAt: "04"},
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "05", EndPrice: 15, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "05"},
	}
	level := LevelAnalysis{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}
	got := class3Signals(Frame{}, []LevelAnalysis{level})
	if len(got) != 1 || got[0].Status != "observing" || got[0].Phase != "awaiting_first_retest" || len(got[0].Missing) == 0 {
		t.Fatalf("confirmed departure without retest=%+v, want explicit waiting signal", got)
	}
	formingRetest := Segment{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", EndPrice: 12, Low: 11, High: 15, Forming: true}
	level.Segments = append(level.Segments, formingRetest)
	got = class3Signals(Frame{}, []LevelAnalysis{level})
	if len(got) != 1 || got[0].Status != "observing" || got[0].Phase != "awaiting_first_retest" || got[0].Date != "06" {
		t.Fatalf("forming first retest=%+v, want waiting candidate with forming endpoint date", got)
	}
}

func TestClass3MarksLaterCenterReturnAsInvalidated(t *testing.T) {
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "center-1", Lower: 5, Upper: 10, ConfirmedAt: "03", EndSegment: 2}
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", Confirmed: true, ConfirmedAt: "02"},
		{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "03", Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Chain: 0, Direction: "up", Start: "03", End: "04", Confirmed: true, ConfirmedAt: "04"},
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "05", EndPrice: 15, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "05"},
		{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", EndPrice: 12, Low: 11, High: 15, Confirmed: true, ConfirmedAt: "06"},
	}
	frame := Frame{Candles: []Bar{{Date: "07", Low: 10, High: 12}}}
	got := class3Signals(frame, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Status != "invalidated" || got[0].PhaseAt != "07" || got[0].ConfirmedAt != "06" {
		t.Fatalf("invalidated class-3 candidate=%+v", got)
	}
}
