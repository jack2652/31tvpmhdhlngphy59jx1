package chananalysis

import (
	"strings"
	"testing"
)

func TestClass2RequiresFirstLowerLevelRetestAndParentLevelRecovery(t *testing.T) {
	parent := V4Signal{SignalID: "parent-buy", Level: 1, Kind: "class1_buy", Date: "10", ObservedAt: "11", ConfirmedAt: "12", Invalidation: ptr(90), StructureRefs: []int{0, 1, 2}}
	parentLevel := LevelAnalysis{Level: 1, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 1, Direction: "up", Start: "01", End: "10", Confirmed: true, ConfirmedAt: "11"},
		{Chain: 3, Level: 1, Direction: "down", Start: "10", End: "16", Confirmed: true, ConfirmedAt: "16"},
		{Chain: 3, Level: 1, Direction: "up", Start: "16", End: "17", Confirmed: true, ConfirmedAt: "17"},
	}}
	lower := LevelAnalysis{Level: 0, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 0, Direction: "up", Start: "12", End: "14", StartPrice: 100, EndPrice: 110, Low: 90, High: 110, Confirmed: true, ConfirmedAt: "14"},  // 第一段逆趋势走势
		{Chain: 3, Level: 0, Direction: "down", Start: "14", End: "16", StartPrice: 110, EndPrice: 96, Low: 96, High: 110, Confirmed: true, ConfirmedAt: "16"}, // 首次有效回试
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

func TestClass2AcceptsFloatNoiseAtContinuousRetestEndpoint(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	lower.Segments[1].StartPrice = lower.Segments[0].EndPrice + 1e-9

	got := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, lower})
	if got.Kind != "class2_buy" || got.Status != "confirmed" || got.Date != "16" {
		t.Fatalf("class-2=%+v, a sub-tolerance endpoint difference must preserve the continuous first retest", got)
	}
}

func TestClass2UsesMatchingRecursiveSourceWhenL0HasTwoInputs(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	parent.SourceKind, parentLevel.SourceKind, lower.SourceKind = "recursive", "recursive", "recursive"
	penLower := LevelAnalysis{Level: 0, SourceKind: "pen", Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 0, Direction: "down", Start: "12", End: "14", Confirmed: true, ConfirmedAt: "14"},
	}}
	got := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, penLower, lower})
	if got.Status != "confirmed" || got.Kind != "class2_buy" || len(got.StructureIDs) != 4 {
		t.Fatalf("class-2 with duplicate level zero=%+v, want the recursive L0 impulse/retest rather than the pen L0 input", got)
	}
	for _, id := range got.StructureIDs[1:3] {
		if !strings.Contains(id, "|recursive|") {
			t.Fatalf("lower structure ID=%q, want its recursive source", id)
		}
	}
}

func TestClass2RejectsDiscontinuousFirstRetestInsteadOfUsingLaterStructure(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	lower.Segments[1].Start = "15"
	lower.Segments = append(lower.Segments,
		Segment{Chain: 3, Level: 0, Direction: "down", Start: "16", End: "18", StartPrice: 96, EndPrice: 95, Low: 95, High: 108, Confirmed: true, ConfirmedAt: "18"},
	)
	got := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, lower})
	if got.Status != "awaiting_lower_level" || got.Price != nil || len(got.StructureIDs) != 1 {
		t.Fatalf("class-2=%+v, a discontinuous first retest must not be skipped for a later candidate", got)
	}
}

func TestBuildClass23SignalsShowsClass2WaitingForUnconfirmedClass1(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			kind, waitingKind, missing := "class1_buy", "class2_buy", "等待一买反向结构确认"
			if side == "sell" {
				kind, waitingKind, missing = "class1_sell", "class2_sell", "等待一卖反向结构确认"
			}
			parent := V4Signal{SignalID: "parent-" + side, Level: 1, Kind: kind, Side: side, Date: "10", ObservedAt: "11", Status: "observing", Invalidation: ptr(120)}
			got := buildClass23Signals(Frame{}, []V4Signal{parent}, []LevelAnalysis{{Level: 1, Status: "ready"}})
			var waiting *V4Signal
			for index := range got {
				if got[index].Kind == waitingKind {
					waiting = &got[index]
					break
				}
			}
			if waiting == nil || waiting.Status != "awaiting_class1_confirmation" || waiting.Price != nil {
				t.Fatalf("signals=%+v, want a non-plottable class-2 %s waiting on first-point confirmation", got, side)
			}
			if len(waiting.Missing) != 1 || waiting.Missing[0] != missing {
				t.Fatalf("missing=%v, want the first-point confirmation dependency", waiting.Missing)
			}
		})
	}
}

func TestBuildClass23SignalsKeepsClass2ThatFormedBeforeParentInvalidation(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	parent.Status, parent.Phase, parent.PhaseAt = "invalidated", "invalidated", "19"
	parent.PhaseHistory = []SignalPhase{{Phase: "divergence_confirmed", At: "12"}, {Phase: "invalidated", At: "19"}}
	frame := Frame{Candles: []Bar{{Date: "19", Low: 89}}}
	signals := buildClass23Signals(frame, []V4Signal{parent}, []LevelAnalysis{parentLevel, lower})
	var second *V4Signal
	for index := range signals {
		if signals[index].Kind == "class2_buy" {
			second = &signals[index]
			break
		}
	}
	if second == nil || second.Status != "invalidated" || second.Date != "16" || second.ObservedAt != "16" || second.ConfirmedAt != "17" || second.PhaseAt != "19" || len(second.PhaseHistory) != 3 || second.PhaseHistory[1] != (SignalPhase{Phase: "class2_confirmed", At: "17"}) || second.PhaseHistory[2] != (SignalPhase{Phase: "invalidated", At: "19"}) {
		t.Fatalf("class-2 history=%+v, want second-point confirmation retained before later parent invalidation", signals)
	}
}

func TestBuildClass23SignalsDoesNotCreateClass2AfterParentInvalidation(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	parent.Status, parent.Phase, parent.PhaseAt = "invalidated", "invalidated", "15"
	parent.PhaseHistory = []SignalPhase{{Phase: "divergence_confirmed", At: "12"}, {Phase: "invalidated", At: "15"}}
	signals := buildClass23Signals(Frame{Candles: []Bar{{Date: "15", Low: 89}}}, []V4Signal{parent}, []LevelAnalysis{parentLevel, lower})
	if hasSignalKind(signals, "class2_buy") {
		t.Fatalf("signals=%+v, a lower-level retest after parent invalidation must not create a second point", signals)
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
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "05", EndPrice: 15, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "05"},                   // 中枢离开段
		{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", StartPrice: 15, EndPrice: 12, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "06"}, // 低点触及 ZG，候选失效
	}
	if got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}}); len(got) != 1 || got[0].Kind != "class3_buy" || got[0].Status != "invalidated" {
		t.Fatalf("boundary-touch class-3 signals=%+v, want retained invalidated candidate", got)
	}
	segments[4].Low, segments[4].EndPrice = 11, 11
	segments[4].StartPrice = 15 + 1e-9
	got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Kind != "class3_buy" || got[0].Status != "confirmed" || got[0].Date != "06" || got[0].ObservedAt != "06" || got[0].RelatedCenterID != center.CenterID || len(got[0].StructureIDs) != 3 {
		t.Fatalf("strictly outside class-3 candidate=%+v", got)
	}
}

func TestRecursiveCentersTreatSubToleranceBoundaryDifferenceAsTouch(t *testing.T) {
	segments := centerRelationFixture(10+1e-8, 12)
	centers := buildRecursiveCenters(segments)
	if len(centers) != 2 || centers[1].RelationToPrevious != "touch" || centers[1].Kind != "newborn" {
		t.Fatalf("centers=%+v, a sub-tolerance boundary difference must be classified as touch", centers)
	}
}

func TestClass3RecognizesDepartureAtLastCenterExtension(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "center-extension-" + side, Lower: 5, Upper: 10, End: "05", ConfirmedAt: "06", EndSegment: 3}
			segments := []Segment{
				{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", Low: 4, High: 10, Confirmed: true, ConfirmedAt: "02"},
				{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "03", Low: 3, High: 9, Confirmed: true, ConfirmedAt: "03"},
				{Level: 0, Chain: 0, Direction: "up", Start: "03", End: "04", Low: 5, High: 12, Confirmed: true, ConfirmedAt: "04"},
				{Level: 0, Chain: 0, Direction: "down", Start: "04", End: "05", StartPrice: 12, EndPrice: 3, Low: 3, High: 12, Confirmed: true, ConfirmedAt: "05"}, // 最后一段中枢延伸本身离开中枢
				{Level: 0, Chain: 0, Direction: "up", Start: "05", End: "06", StartPrice: 3, EndPrice: 4, Low: 3, High: 4, Confirmed: true, ConfirmedAt: "06"},     // 首次回试并确认中枢闭合
			}
			if side == "buy" {
				level, _ := mirrorSignalFixture(LevelAnalysis{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}, nil)
				segments, center = level.Segments, level.Centers[0]
			}
			got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
			wantKind := "class3_" + side
			if len(got) != 1 || got[0].Kind != wantKind || got[0].Status != "confirmed" || got[0].Date != "06" || got[0].ObservedAt != "06" || got[0].ConfirmedAt != "06" || len(got[0].StructureRefs) != 2 || got[0].StructureRefs[0] != 3 || got[0].StructureRefs[1] != 4 {
				t.Fatalf("last-extension class-3 %s=%+v, want the extension departure and first outside retest confirmed at center close", side, got)
			}
		})
	}
}

func TestClass3UsesRecursiveCenterOwnerMoveIndexWhenItIsZero(t *testing.T) {
	center := RecursiveCenter{Level: 1, Chain: 0, CenterID: "center-owner-zero", Lower: 5, Upper: 10, Start: "00", End: "01", ConfirmedAt: "03", OwnerMoveIndex: 0, EndSegment: 2}
	segments := []Segment{
		{Level: 1, Chain: 0, Direction: "up", Start: "01", End: "04", StartPrice: 9, EndPrice: 12, Low: 8, High: 12, Confirmed: true, ConfirmedAt: "04"},
		{Level: 1, Chain: 0, Direction: "down", Start: "04", End: "06", StartPrice: 12, EndPrice: 11, Low: 11, High: 12, Confirmed: true, ConfirmedAt: "06"},
	}
	got := class3Signals(Frame{}, []LevelAnalysis{{Level: 1, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Kind != "class3_buy" || got[0].Status != "confirmed" || got[0].Date != "06" {
		t.Fatalf("class-3 signals=%+v, want owner move at index zero followed by the first outside retest", got)
	}
}

func TestClass3RecognizesSPY2022DownwardCenterDeparture(t *testing.T) {
	center := RecursiveCenter{
		Level: 0, Chain: 0, CenterID: "L0-C0-S0-E3", Lower: 419.8759779470982, Upper: 435.04083957177517,
		End: "2022-06-17", ConfirmedAt: "2022-10-28", EndSegment: 3,
	}
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "2021-12-03", End: "2022-01-04", StartPrice: 419.8759779470982, EndPrice: 450.50356938921004, Low: 419.8759779470982, High: 450.50356938921004, Confirmed: true, ConfirmedAt: "2022-06-30"},
		{Level: 0, Chain: 0, Direction: "down", Start: "2022-01-04", End: "2022-02-24", StartPrice: 450.50356938921004, EndPrice: 385.42193057202616, Low: 385.42193057202616, High: 450.50356938921004, Confirmed: true, ConfirmedAt: "2022-04-11"},
		{Level: 0, Chain: 0, Direction: "up", Start: "2022-02-24", End: "2022-03-29", StartPrice: 385.42193057202616, EndPrice: 435.04083957177517, Low: 385.42193057202616, High: 435.04083957177517, Confirmed: true, ConfirmedAt: "2022-06-30"},
		{Level: 0, Chain: 0, Direction: "down", Start: "2022-03-29", End: "2022-06-17", StartPrice: 435.04083957177517, EndPrice: 342.45755440518343, Low: 342.45755440518343, High: 435.04083957177517, Confirmed: true, ConfirmedAt: "2022-08-29"},
		{Level: 0, Chain: 0, Direction: "up", Start: "2022-06-17", End: "2022-08-16", StartPrice: 342.45755440518343, EndPrice: 408.2314590393534, Low: 342.45755440518343, High: 408.2314590393534, Confirmed: true, ConfirmedAt: "2022-10-28"},
	}
	got := class3Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Kind != "class3_sell" || got[0].Status != "confirmed" || got[0].Date != "2022-08-16" || got[0].ObservedAt != "2022-10-28" || got[0].ConfirmedAt != "2022-10-28" || got[0].Price == nil || *got[0].Price != 408.2314590393534 || got[0].Invalidation == nil || *got[0].Invalidation != center.Lower {
		t.Fatalf("SPY 2022 class-3 signal=%+v, want the 2022-08-16 outside retest observed when center close confirms on 2022-10-28", got)
	}
}

func TestUnifiedSignalScanIncludesPenLevelClass3AndKeepsRecursiveLevels(t *testing.T) {
	strokes := []Stroke{
		{Direction: "down", Chain: 0, Level: 0, Start: Fractal{Kind: "top", Date: "2022-01-01", Price: 442}, End: Fractal{Kind: "bottom", Date: "2022-01-02", Price: 419}, Low: 419, High: 442, Confirmed: true, ConfirmedAt: "2022-01-02"},
		{Direction: "up", Chain: 0, Level: 0, Start: Fractal{Kind: "bottom", Date: "2022-01-02", Price: 419}, End: Fractal{Kind: "top", Date: "2022-01-03", Price: 450}, Low: 419, High: 450, Confirmed: true, ConfirmedAt: "2022-01-03"},
		{Direction: "down", Chain: 0, Level: 0, Start: Fractal{Kind: "top", Date: "2022-01-03", Price: 450}, End: Fractal{Kind: "bottom", Date: "2022-01-04", Price: 394}, Low: 394, High: 450, Confirmed: true, ConfirmedAt: "2022-01-04"},
		{Direction: "up", Chain: 0, Level: 0, Start: Fractal{Kind: "bottom", Date: "2022-01-04", Price: 394}, End: Fractal{Kind: "top", Date: "2022-01-05", Price: 430}, Low: 394, High: 430, Confirmed: true, ConfirmedAt: "2022-01-05"},
		{Direction: "down", Chain: 0, Level: 0, Start: Fractal{Kind: "top", Date: "2022-01-05", Price: 430}, End: Fractal{Kind: "bottom", Date: "2022-01-06", Price: 385}, Low: 385, High: 430, Confirmed: true, ConfirmedAt: "2022-01-06"},
		{Direction: "up", Chain: 0, Level: 0, Start: Fractal{Kind: "bottom", Date: "2022-01-06", Price: 385}, End: Fractal{Kind: "top", Date: "2022-01-07", Price: 414}, Low: 385, High: 414, Confirmed: true, ConfirmedAt: "2022-01-07"},
	}
	frame := Frame{StrokesV4: strokes}
	recursive := []LevelAnalysis{
		{Level: 0, SourceKind: "segments", Segments: []Segment{{Level: 0, Chain: 0, Direction: "down", Start: "01", End: "02"}}},
		{Level: 1, Segments: []Segment{{Level: 1, Chain: 0, Direction: "down", Start: "01", End: "02"}}},
	}
	inputs := buildSignalScanLevels(frame, recursive)
	if len(inputs) != 3 || inputs[0].Level != 0 || inputs[0].SourceKind != "pen" || inputs[1].Level != 0 || inputs[1].SourceKind != "recursive" || inputs[2].Level != 1 || inputs[2].SourceKind != "recursive" {
		t.Fatalf("normalized levels=%+v, want pen and recursive level 0 followed by recursive level 1", inputs)
	}
	if len(inputs[0].Centers) != 1 || inputs[0].Centers[0].FormedAt != "2022-01-04" || inputs[0].Centers[0].ClosedAt != "2022-01-07" || inputs[0].Centers[0].ConfirmedAt != "2022-01-07" || inputs[0].Centers[0].EndSegment != 4 {
		t.Fatalf("pen center lifecycle=%+v, want third-stroke formation and first outside-stroke closure", inputs[0].Centers)
	}
	signals := buildV4Signals(Frame{}, inputs, nil)
	var sell *V4Signal
	for index := range signals {
		if signals[index].Kind == "class3_sell" && signals[index].SourceKind == "pen" {
			sell = &signals[index]
			break
		}
	}
	if sell == nil || sell.Status != "confirmed" || sell.Date != "2022-01-07" || sell.ObservedAt != "2022-01-07" || sell.RelatedCenterID != inputs[0].Centers[0].CenterID || len(sell.StructureIDs) != 3 {
		t.Fatalf("canonical signals=%+v, want pen-level three-sell using the closed center and first outside retest", signals)
	}
}

func TestUnifiedSignalScanFallsBackToRecursiveLevelZeroWithoutPenInput(t *testing.T) {
	recursive := []LevelAnalysis{
		{Level: 0, Segments: []Segment{{Level: 0, Chain: 1, Direction: "up", Start: "01", End: "02"}}},
		{Level: 1, Segments: []Segment{{Level: 1, Chain: 1, Direction: "down", Start: "01", End: "02"}}},
	}
	inputs := buildSignalScanLevels(Frame{}, recursive)
	if len(inputs) != 2 || inputs[0].Level != 0 || inputs[0].SourceKind != "recursive" || len(inputs[0].Segments) != 1 || inputs[1].Level != 1 {
		t.Fatalf("normalized levels=%+v, want both recursive levels preserved when no pen input exists", inputs)
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

func TestClass3DoesNotConnectCenterDepartureAcrossSameChainGap(t *testing.T) {
	level := classThreeFixture()
	level.Segments[3].Start = "04-gap"
	if got := class3Signals(Frame{}, []LevelAnalysis{level}); hasSignalKind(got, "class3_buy") {
		t.Fatalf("signals=%+v, a departure after a structural gap must not belong to the prior center", got)
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
	formingRetest := Segment{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", StartPrice: 15, EndPrice: 12, Low: 11, High: 15, Forming: true}
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
		{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", StartPrice: 15, EndPrice: 12, Low: 11, High: 15, Confirmed: true, ConfirmedAt: "06"},
	}
	frame := Frame{Candles: []Bar{{Date: "07", Low: 10, High: 12}}}
	got := class3Signals(frame, []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}})
	if len(got) != 1 || got[0].Status != "invalidated" || got[0].PhaseAt != "07" || got[0].ConfirmedAt != "06" {
		t.Fatalf("invalidated class-3 candidate=%+v", got)
	}
}
