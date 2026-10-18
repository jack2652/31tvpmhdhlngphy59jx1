package chananalysis

import "testing"

// These mirrored fixtures exercise both directions through the same frozen
// signal rules. A missing lower timeframe remains an explicit waiting state;
// missing market data remains unfiltered rather than being treated as passed.
func TestSignalFixturesCoverClassOneDirectionsWaitingMarketAndInvalidation(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level, strengths := classOneFixture(1)
			if side == "buy" {
				level, strengths = mirrorSignalFixture(level, strengths)
			}
			levels := []LevelAnalysis{level, {Level: 0, Status: "awaiting_lower_level"}}
			wantKind := "class1_sell"
			invalidationBar := Bar{Date: "09", High: 16, Low: 12}
			if side == "buy" {
				wantKind = "class1_buy"
				invalidationBar = Bar{Date: "09", High: 88, Low: 84}
			}

			waiting := buildV4Signals(Frame{}, levels, strengths)
			class1, class2 := findV4Signal(waiting, wantKind), findV4Signal(waiting, "class2_"+side)
			if class1 == nil || class1.Status != "reversal_confirmed" || class1.MarketFilter != "unfiltered" || class1.LowerLevelStatus != "awaiting_lower_level" {
				t.Fatalf("class-1 %s fixture=%+v, want confirmed structure with missing market/lower-level data explicit", side, waiting)
			}
			if class2 == nil || class2.Status != "awaiting_lower_level" || class2.LowerLevelStatus != "awaiting_lower_level" || len(class2.Missing) == 0 {
				t.Fatalf("class-2 %s fixture=%+v, want explicit lower-level wait", side, waiting)
			}

			invalidated := buildV4Signals(Frame{Candles: []Bar{invalidationBar}}, levels, strengths)
			invalid := findV4Signal(invalidated, wantKind)
			if invalid == nil || invalid.Status != "invalidated" || invalid.Phase != "invalidated" || invalid.PhaseAt != "09" || invalid.ConfirmedAt != "" {
				t.Fatalf("class-1 %s invalidation fixture=%+v, want first later boundary touch to invalidate", side, invalidated)
			}
		})
	}
}

func TestSignalFixturesCoverClassTwoAndThreeInBothDirections(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run("class2_"+side, func(t *testing.T) {
			parent, parentLevel, lower := classTwoFixture()
			if side == "sell" {
				parent.Kind = "class1_sell"
				parent.Side = "sell"
				parent.Invalidation = ptr(10)
				parentLevel, _ = mirrorSignalFixture(parentLevel, nil)
				lower, _ = mirrorSignalFixture(lower, nil)
			}
			got := class2Signal(Frame{}, parent, []LevelAnalysis{parentLevel, lower})
			wantKind := "class2_" + side
			if got.Kind != wantKind || got.Status != "confirmed" || got.ConfirmedAt != "17" || got.RelatedSignalID != parent.SignalID || got.MarketFilter != "unfiltered" || len(got.Missing) != 0 {
				t.Fatalf("class-2 %s fixture=%+v, want first retest and parent recovery confirmed", side, got)
			}
		})

		t.Run("class3_"+side, func(t *testing.T) {
			level := classThreeFixture()
			if side == "sell" {
				level, _ = mirrorSignalFixture(level, nil)
			}
			got := class3Signals(Frame{}, []LevelAnalysis{level})
			wantKind := "class3_" + side
			if len(got) != 1 || got[0].Kind != wantKind || got[0].Status != "confirmed" || got[0].RelatedCenterID != "center-1" || got[0].MarketFilter != "unfiltered" || len(got[0].Missing) != 0 {
				t.Fatalf("class-3 %s fixture=%+v, want first retest strictly outside the center", side, got)
			}
		})
	}
}

func TestSignalFixturesDoNotDeriveClassTwoFromInvalidatedClassOne(t *testing.T) {
	parent, parentLevel, lower := classTwoFixture()
	parent.Status = "invalidated"
	parent.Phase = "invalidated"
	parent.PhaseAt = "13"
	parent.PhaseHistory = []SignalPhase{{Phase: "reversal_confirmed", At: "12"}, {Phase: "invalidated", At: "13"}}
	got := buildClass23Signals(Frame{}, []V4Signal{parent}, []LevelAnalysis{parentLevel, lower})
	for _, signal := range got {
		if signal.Kind == "class2_buy" || signal.Kind == "class2_sell" {
			t.Fatalf("invalidated parent generated class-2 signal: %+v", signal)
		}
	}
}

func classOneFixture(levelNumber int) (LevelAnalysis, []Strength) {
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: levelNumber, Start: "01", End: "02", StartPrice: 4, EndPrice: 11, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: levelNumber, Start: "02", End: "03", StartPrice: 11, EndPrice: 7, Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: levelNumber, Start: "03", End: "04", StartPrice: 7, EndPrice: 9, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: levelNumber, Start: "04", End: "05", StartPrice: 9, EndPrice: 6, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: levelNumber, Start: "05", End: "06", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: levelNumber, Start: "06", End: "07", StartPrice: 14, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "07"},
		{Direction: "up", Chain: 0, Level: levelNumber, Start: "07", End: "08", StartPrice: 12, EndPrice: 16, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "08"},
		{Direction: "down", Chain: 0, Level: levelNumber, Start: "08", End: "09", StartPrice: 16, EndPrice: 13, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "09"},
		{Direction: "up", Chain: 0, Level: levelNumber, Start: "09", End: "10", StartPrice: 13, EndPrice: 15, Low: 13, High: 15, Confirmed: true, ConfirmedAt: "10"},
		{Direction: "down", Chain: 0, Level: levelNumber, Start: "10", End: "11", StartPrice: 15, EndPrice: 11, Low: 11, High: 15, Confirmed: true, ConfirmedAt: "11"},
	}
	center := RecursiveCenter{Level: levelNumber, Chain: 0, Lower: 5, Upper: 10, Start: "01", End: "04", FormedAt: "04", ConfirmedAt: "05", EndSegment: 2}
	strengths := []Strength{
		{Direction: "up", Level: levelNumber, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true},
		{Direction: "up", Level: levelNumber, Start: "07", End: "08", MACDArea: 7.9, WarmupComplete: true},
	}
	return LevelAnalysis{Level: levelNumber, Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}, strengths
}

func classTwoFixture() (V4Signal, LevelAnalysis, LevelAnalysis) {
	parent := V4Signal{SignalID: "parent-buy", Level: 1, Kind: "class1_buy", Side: "buy", Date: "10", ObservedAt: "11", ConfirmedAt: "12", Invalidation: ptr(90), StructureRefs: []int{0, 1, 2}, MarketFilter: "unfiltered"}
	parentLevel := LevelAnalysis{Level: 1, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 1, Direction: "up", Start: "01", End: "10", Confirmed: true, ConfirmedAt: "11"},
		{Chain: 3, Level: 1, Direction: "down", Start: "10", End: "16", Confirmed: true, ConfirmedAt: "16"},
		{Chain: 3, Level: 1, Direction: "up", Start: "16", End: "17", Confirmed: true, ConfirmedAt: "17"},
	}}
	lower := LevelAnalysis{Level: 0, Status: "ready", Segments: []Segment{
		{Chain: 3, Level: 0, Direction: "up", Start: "12", End: "14", Low: 90, High: 110, EndPrice: 110, Confirmed: true, ConfirmedAt: "14"},
		{Chain: 3, Level: 0, Direction: "down", Start: "14", End: "16", Low: 96, High: 110, EndPrice: 96, Confirmed: true, ConfirmedAt: "16"},
	}}
	return parent, parentLevel, lower
}

func classThreeFixture() LevelAnalysis {
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "center-1", Lower: 5, Upper: 10, ConfirmedAt: "03", EndSegment: 2}
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", Confirmed: true, ConfirmedAt: "02"},
		{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "03", Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Chain: 0, Direction: "up", Start: "03", End: "04", Confirmed: true, ConfirmedAt: "04"},
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "05", EndPrice: 15, Low: 10, High: 15, Confirmed: true, ConfirmedAt: "05"},
		{Level: 0, Chain: 0, Direction: "down", Start: "05", End: "06", EndPrice: 12, Low: 11, High: 15, Confirmed: true, ConfirmedAt: "06"},
	}
	return LevelAnalysis{Level: 0, Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}
}

func mirrorSignalFixture(level LevelAnalysis, strengths []Strength) (LevelAnalysis, []Strength) {
	invertDirection := func(direction string) string {
		if direction == "up" {
			return "down"
		}
		if direction == "down" {
			return "up"
		}
		return direction
	}
	for index := range level.Segments {
		segment := &level.Segments[index]
		segment.Direction = invertDirection(segment.Direction)
		segment.StartPrice, segment.EndPrice = 100-segment.StartPrice, 100-segment.EndPrice
		segment.Low, segment.High = 100-segment.High, 100-segment.Low
	}
	for index := range level.Centers {
		center := &level.Centers[index]
		center.Lower, center.Upper = 100-center.Upper, 100-center.Lower
	}
	for index := range strengths {
		strengths[index].Direction = invertDirection(strengths[index].Direction)
	}
	return level, strengths
}

func findV4Signal(signals []V4Signal, kind string) *V4Signal {
	for index := range signals {
		if signals[index].Kind == kind {
			return &signals[index]
		}
	}
	return nil
}
