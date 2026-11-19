package chananalysis

import (
	"strings"
	"testing"
)

func TestTimeframeRelationLadderIsExplicitAndDirect(t *testing.T) {
	if TimeframeMappingVersion != "chan-timeframe-map-v5" {
		t.Fatalf("timeframe mapping version=%q, want v5", TimeframeMappingVersion)
	}
	want := map[string]timeframeRelation{
		"15m": {higher: "1h", role: "lowest"},
		"1h":  {higher: "2h", lower: "15m", role: "intermediate"},
		"2h":  {higher: "4h", lower: "1h", role: "intermediate"},
		"4h":  {higher: "1d", lower: "2h", role: "intermediate"},
		"1d":  {higher: "1wk", lower: "4h", role: "intermediate"},
		"1wk": {lower: "1d", role: "highest"},
	}
	for period, expected := range want {
		actual, ok := timeframeRelationFor(period)
		if !ok || actual != expected {
			t.Fatalf("relation %s=%+v,%v want %+v", period, actual, ok, expected)
		}
	}
	if _, ok := timeframeRelationFor("30m"); ok {
		t.Fatal("unconfigured 30m timeframe relation unexpectedly accepted")
	}
}

func TestMappedTimeframeConfirmationRequiresOrderedLowerStructures(t *testing.T) {
	higher := Frame{Timeframe: "1wk", AsOf: "2026-01-16", Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, SignalsV4: []V4Signal{{SignalID: "weekly-buy", Side: "buy", Level: 1, ObservedAt: "2026-01-12"}}}
	lower := Frame{Timeframe: "1d", BarCount: 5, Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, RecursiveLevels: []LevelAnalysis{{Level: 0, Segments: []Segment{
		{Chain: 1, Level: 0, Direction: "down", Start: "2026-01-13", End: "2026-01-14", Confirmed: true, ConfirmedAt: "2026-01-14"},
		{Chain: 1, Level: 0, Direction: "up", Start: "2026-01-14", End: "2026-01-15", Confirmed: true, ConfirmedAt: "2026-01-15"},
	}}}}
	AttachMappedTimeframe(&higher, lower)
	got := higher.SignalsV4[0].TimeframeConfirmation
	if got == nil || got.Status != "confirmed" || got.HigherTimeframe != "1wk" || got.LowerTimeframe != "1d" || got.ConfirmedAt != "2026-01-15" || len(got.StructureIDs) != 2 {
		t.Fatalf("valid daily confirmation=%+v", got)
	}
	if higher.TimeframeMapping.LowerTimeframeStatus != "ready" {
		t.Fatalf("weekly mapping=%+v", higher.TimeframeMapping)
	}
}

func TestMappedTimeframeConfirmationDoesNotUseFutureOrSameDayData(t *testing.T) {
	for _, test := range []struct {
		name      string
		observed  string
		start     string
		turn      string
		confirm   string
		wantState string
	}{
		{name: "same-day daily lower bar", observed: "2026-01-12", start: "2026-01-12", turn: "2026-01-13", confirm: "2026-01-14", wantState: "awaiting_confirmation"},
		{name: "after weekly as-of", observed: "2026-01-09", start: "2026-01-13", turn: "2026-01-14", confirm: "2026-01-16", wantState: "awaiting_confirmation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			higher := Frame{Timeframe: "1wk", AsOf: "2026-01-15", Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, SignalsV4: []V4Signal{{SignalID: "buy", Side: "buy", ObservedAt: test.observed}}}
			lower := Frame{Timeframe: "1d", BarCount: 2, Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, Segments: []Segment{
				{Level: 0, Chain: 1, Direction: "down", Start: test.start, End: test.turn, Confirmed: true, ConfirmedAt: test.turn},
				{Level: 0, Chain: 1, Direction: "up", Start: test.turn, End: test.confirm, Confirmed: true, ConfirmedAt: test.confirm},
			}}
			AttachMappedTimeframe(&higher, lower)
			if got := higher.SignalsV4[0].TimeframeConfirmation.Status; got != test.wantState {
				t.Fatalf("future leakage state=%q want %q, confirmation=%+v", got, test.wantState, higher.SignalsV4[0].TimeframeConfirmation)
			}
		})
	}
}

func TestMappedTimeframeConfirmationRejectsUnconfirmedOrOutOfSnapshotSegments(t *testing.T) {
	for _, test := range []struct {
		name                string
		counterConfirmedAt  string
		recoveryEnd         string
		recoveryConfirmedAt string
		higherAsOf          string
		lowerAsOf           string
	}{
		{name: "counter move lacks confirmation time", counterConfirmedAt: "", recoveryEnd: "2026-01-15", recoveryConfirmedAt: "2026-01-15", higherAsOf: "2026-01-16", lowerAsOf: "2026-01-16"},
		{name: "recovery ends after higher snapshot", counterConfirmedAt: "2026-01-14", recoveryEnd: "2026-01-16", recoveryConfirmedAt: "2026-01-15", higherAsOf: "2026-01-15", lowerAsOf: "2026-01-16"},
		{name: "recovery ends after lower snapshot", counterConfirmedAt: "2026-01-14", recoveryEnd: "2026-01-15", recoveryConfirmedAt: "2026-01-14", higherAsOf: "2026-01-16", lowerAsOf: "2026-01-14"},
		{name: "recovery confirmation precedes its end", counterConfirmedAt: "2026-01-14", recoveryEnd: "2026-01-16", recoveryConfirmedAt: "2026-01-15", higherAsOf: "2026-01-17", lowerAsOf: "2026-01-17"},
	} {
		t.Run(test.name, func(t *testing.T) {
			higher := Frame{Timeframe: "1wk", AsOf: test.higherAsOf, Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, SignalsV4: []V4Signal{{SignalID: "buy", Side: "buy", ObservedAt: "2026-01-12"}}}
			lower := Frame{Timeframe: "1d", AsOf: test.lowerAsOf, BarCount: 2, Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, Segments: []Segment{
				{Level: 0, Chain: 1, Direction: "down", Start: "2026-01-13", End: "2026-01-14", Confirmed: true, ConfirmedAt: test.counterConfirmedAt},
				{Level: 0, Chain: 1, Direction: "up", Start: "2026-01-14", End: test.recoveryEnd, Confirmed: true, ConfirmedAt: test.recoveryConfirmedAt},
			}}
			AttachMappedTimeframe(&higher, lower)
			if got := higher.SignalsV4[0].TimeframeConfirmation.Status; got != "awaiting_confirmation" {
				t.Fatalf("inconsistent or out-of-snapshot structures confirmed the signal: %+v", higher.SignalsV4[0].TimeframeConfirmation)
			}
		})
	}
}

func TestMappedTimeframeConfirmationRejectsDiscontinuousPriceEndpoint(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			firstDirection, secondDirection := "down", "up"
			firstStart, firstEnd, secondStart, secondEnd := 110.0, 100.0, 99.0, 108.0
			if side == "sell" {
				firstDirection, secondDirection = "up", "down"
				firstStart, firstEnd, secondStart, secondEnd = 100, 110, 111, 102
			}
			higher := Frame{Timeframe: "1wk", AsOf: "2026-01-16", Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, SignalsV4: []V4Signal{{SignalID: "weekly-" + side, Side: side, ObservedAt: "2026-01-12"}}}
			lower := Frame{Timeframe: "1d", BarCount: 5, Quality: DataQuality{PriceBasis: "split_dividend_adjusted"}, RecursiveLevels: []LevelAnalysis{{Level: 0, Segments: []Segment{
				{Chain: 1, Level: 0, Direction: firstDirection, Start: "2026-01-13", End: "2026-01-14", StartPrice: firstStart, EndPrice: firstEnd, Confirmed: true, ConfirmedAt: "2026-01-14"},
				{Chain: 1, Level: 0, Direction: secondDirection, Start: "2026-01-14", End: "2026-01-15", StartPrice: secondStart, EndPrice: secondEnd, Confirmed: true, ConfirmedAt: "2026-01-15"},
			}}}}
			AttachMappedTimeframe(&higher, lower)
			if got := higher.SignalsV4[0].TimeframeConfirmation; got == nil || got.Status != "awaiting_confirmation" {
				t.Fatalf("discontinuous mapped structures confirmed a signal: %+v", got)
			}
		})
	}
}

func TestMappedTimeframeBuildsLevelZeroClass2FromFirstRetestAndParentRecovery(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			higher, lower := mappedClass2Fixture(side)

			AttachMappedTimeframe(&higher, lower)

			var class2 *V4Signal
			for index := range higher.SignalsV4 {
				if higher.SignalsV4[index].Kind == "class2_"+side {
					class2 = &higher.SignalsV4[index]
					break
				}
			}
			if class2 == nil || class2.Status != "confirmed" || class2.Date != "2026-01-06" || class2.ObservedAt != "2026-01-06 16:00" || class2.ConfirmedAt != "2026-01-09" || class2.RelatedSignalID != "daily-parent-"+side || len(class2.StructureIDs) != 4 || !strings.HasPrefix(class2.StructureIDs[1], "4h|") || !strings.HasPrefix(class2.StructureIDs[2], "4h|") {
				t.Fatalf("mapped class-2 %s=%+v, want lower first retest followed by higher recovery confirmation", side, class2)
			}
		})
	}
}

func TestMappedClass2RejectsSameDayLowerStructureAndKeepsWaiting(t *testing.T) {
	higher, lower := mappedClass2Fixture("buy")
	lower.Segments[0].Start, lower.Segments[0].End, lower.Segments[0].ConfirmedAt = "2026-01-05 09:30", "2026-01-05 13:00", "2026-01-05 13:00"
	lower.Segments[1].Start, lower.Segments[1].End, lower.Segments[1].ConfirmedAt = "2026-01-05 13:00", "2026-01-05 16:00", "2026-01-05 16:00"
	AttachMappedTimeframe(&higher, lower)
	class2 := mappedClass2Signal(higher, "class2_buy")
	if class2 == nil || class2.Status != "awaiting_lower_level" || class2.Price != nil {
		t.Fatalf("same-day mapped class-2=%+v, want waiting because lower bars were not observable after daily close", class2)
	}
}

func TestMappedClass2DoesNotUseStructuresAfterLowerFrameAsOf(t *testing.T) {
	higher, lower := mappedClass2Fixture("buy")
	lower.AsOf = "2026-01-05"
	AttachMappedTimeframe(&higher, lower)

	class2 := mappedClass2Signal(higher, "class2_buy")
	if class2 == nil || class2.Status != "awaiting_lower_level" || class2.Price != nil {
		t.Fatalf("mapped class-2=%+v, want waiting because the lower frame ends before its impulse and retest", class2)
	}
}

func TestMappedClass2RejectsRetestTouchAndLowerChainBreak(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Frame)
		status string
	}{
		{name: "touches invalidation", mutate: func(lower *Frame) { lower.Segments[1].Low = 90 }, status: "invalidated"},
		{name: "lower chain break", mutate: func(lower *Frame) { lower.Segments[1].Chain++ }, status: "awaiting_lower_level"},
		{name: "missing impulse confirmation time", mutate: func(lower *Frame) { lower.Segments[0].ConfirmedAt = "" }, status: "awaiting_lower_level"},
	} {
		t.Run(test.name, func(t *testing.T) {
			higher, lower := mappedClass2Fixture("buy")
			test.mutate(&lower)
			AttachMappedTimeframe(&higher, lower)
			class2 := mappedClass2Signal(higher, "class2_buy")
			if class2 == nil || class2.Status != test.status {
				t.Fatalf("mapped class-2=%+v, want status %q", class2, test.status)
			}
		})
	}
}

func TestMappedClass2WaitsForHigherTimeframeRecovery(t *testing.T) {
	higher, lower := mappedClass2Fixture("buy")
	higher.StrokesV4 = higher.StrokesV4[:1]
	AttachMappedTimeframe(&higher, lower)
	class2 := mappedClass2Signal(higher, "class2_buy")
	if class2 == nil || class2.Status != "observing" || class2.Date != "2026-01-06" || class2.ConfirmedAt != "" {
		t.Fatalf("mapped class-2=%+v, want an observed first retest awaiting daily recovery", class2)
	}
}

func mappedClass2Signal(frame Frame, kind string) *V4Signal {
	for index := range frame.SignalsV4 {
		if frame.SignalsV4[index].Kind == kind {
			return &frame.SignalsV4[index]
		}
	}
	return nil
}

func mappedClass2Fixture(side string) (Frame, Frame) {
	impulseDirection, retestDirection, recoveryDirection := "up", "down", "up"
	previousDirection, previousStartKind, previousEndKind := "down", "top", "bottom"
	recoveryStartKind, recoveryEndKind := "bottom", "top"
	impulseStart, impulseEnd, retestEnd, recoveryEnd := 100.0, 110.0, 101.0, 108.0
	impulseLow, impulseHigh, retestLow, retestHigh := 100.0, 110.0, 95.0, 110.0
	recoveryLow, recoveryHigh := 101.0, 108.0
	invalidation := 90.0
	if side == "sell" {
		impulseDirection, retestDirection, recoveryDirection = "down", "up", "down"
		previousDirection, previousStartKind, previousEndKind = "up", "bottom", "top"
		recoveryStartKind, recoveryEndKind = "top", "bottom"
		impulseStart, impulseEnd, retestEnd, recoveryEnd = 100, 90, 109, 98
		impulseLow, impulseHigh, retestLow, retestHigh = 90, 100, 90, 115
		recoveryLow, recoveryHigh = 98, 109
		invalidation = 120
	}
	parent := V4Signal{
		SignalID: "daily-parent-" + side, Level: 0, SourceKind: "pen", Kind: "class1_" + side, Side: side,
		Status: "confirmed", Date: "2026-01-02", ObservedAt: "2026-01-04", ConfirmedAt: "2026-01-05",
		StructureRefs: []int{0}, Invalidation: ptr(invalidation),
	}
	higher := Frame{
		Timeframe: "1d", BarCount: 3, AsOf: "2026-01-09", Quality: DataQuality{PriceBasis: "split_dividend_adjusted"},
		StrokesV4: []Stroke{
			{Direction: previousDirection, Chain: 3, Level: 0, Start: Fractal{Kind: previousStartKind, Date: "2026-01-01", Price: 100}, End: Fractal{Kind: previousEndKind, Date: "2026-01-02", Price: 90}, Low: 90, High: 100, Confirmed: true, ConfirmedAt: "2026-01-03"},
			{Direction: recoveryDirection, Chain: 3, Level: 0, Start: Fractal{Kind: recoveryStartKind, Date: "2026-01-07", Price: retestEnd}, End: Fractal{Kind: recoveryEndKind, Date: "2026-01-09", Price: recoveryEnd}, Low: recoveryLow, High: recoveryHigh, Confirmed: true, ConfirmedAt: "2026-01-09"},
		},
		SignalsV4: []V4Signal{parent, class2SignalBase(parent)},
	}
	lower := Frame{
		Timeframe: "4h", BarCount: 2, AsOf: "2026-01-09", Quality: DataQuality{PriceBasis: "split_dividend_adjusted"},
		Segments: []Segment{
			{Level: 0, Chain: 7, Direction: impulseDirection, Start: "2026-01-06 09:30", End: "2026-01-06 13:00", StartPrice: impulseStart, EndPrice: impulseEnd, Low: impulseLow, High: impulseHigh, Confirmed: true, ConfirmedAt: "2026-01-06 13:00"},
			{Level: 0, Chain: 7, Direction: retestDirection, Start: "2026-01-06 13:00", End: "2026-01-06 16:00", StartPrice: impulseEnd, EndPrice: retestEnd, Low: retestLow, High: retestHigh, Confirmed: true, ConfirmedAt: "2026-01-06 16:00"},
		},
	}
	return higher, lower
}

func TestMappedTimeframeRequiresDirectPeriodAndMatchingPriceBasis(t *testing.T) {
	t.Run("wrong adjacent period", func(t *testing.T) {
		higher := Frame{Timeframe: "1d", SignalsV4: []V4Signal{{Side: "buy"}}}
		AttachMappedTimeframe(&higher, Frame{Timeframe: "1wk", BarCount: 1})
		if higher.TimeframeMapping.LowerTimeframeStatus != "awaiting_lower_timeframe" || higher.SignalsV4[0].TimeframeConfirmation.LowerTimeframe != "4h" {
			t.Fatalf("accepted non-direct lower period: %+v signal=%+v", higher.TimeframeMapping, higher.SignalsV4[0].TimeframeConfirmation)
		}
	})
	t.Run("different price basis", func(t *testing.T) {
		higher := Frame{Timeframe: "1d", SignalsV4: []V4Signal{{Side: "buy"}}}
		lower := Frame{Timeframe: "4h", BarCount: 100, Quality: DataQuality{PriceBasis: "raw"}}
		AttachMappedTimeframe(&higher, lower)
		if higher.TimeframeMapping.LowerTimeframeStatus != "incompatible_price_basis" || higher.SignalsV4[0].TimeframeConfirmation.Status != "incompatible_price_basis" {
			t.Fatalf("mixed price basis was accepted: %+v signal=%+v", higher.TimeframeMapping, higher.SignalsV4[0].TimeframeConfirmation)
		}
	})
}

func TestTimeframeMappingExplicitlyMarksUnavailableLowerFrame(t *testing.T) {
	higher := Frame{Timeframe: "4h", SignalsV4: []V4Signal{{Side: "sell"}}}
	AttachMappedTimeframe(&higher, Frame{})
	if higher.TimeframeMapping.LowerTimeframe != "2h" || higher.TimeframeMapping.LowerTimeframeStatus != "awaiting_lower_timeframe" || higher.SignalsV4[0].TimeframeConfirmation.Status != "awaiting_lower_timeframe" {
		t.Fatalf("missing 2h data not exposed: %+v signal=%+v", higher.TimeframeMapping, higher.SignalsV4[0].TimeframeConfirmation)
	}
}

func TestFourHourAnalyzeIntradayUsesCanonicalTwoHourConfirmation(t *testing.T) {
	frame := Frame{Timeframe: "4h", AsOf: "2026-01-16 16:00", BarCount: 100, Quality: DataQuality{PriceBasis: "raw"}, SignalsV4: []V4Signal{{SignalID: "4h-buy", Side: "buy", ObservedAt: "2026-01-15 16:00"}}}
	lower := Frame{Timeframe: "2h", BarCount: 200, Quality: DataQuality{PriceBasis: "raw"}, Segments: []Segment{{Level: 0, Direction: "down", Start: "2026-01-16 09:30", End: "2026-01-16 11:00", Chain: 1, Confirmed: true, ConfirmedAt: "2026-01-16 11:15"}, {Level: 0, Direction: "up", Start: "2026-01-16 11:00", End: "2026-01-16 14:00", Chain: 1, Confirmed: true, ConfirmedAt: "2026-01-16 14:15"}}}
	AttachMappedTimeframe(&frame, lower)
	if frame.TimeframeMapping.LowerTimeframeStatus != "ready" || len(frame.SignalsV4) != 1 {
		t.Fatalf("4h frame mapping/signals=%+v %+v", frame.TimeframeMapping, frame.SignalsV4)
	}
	if got := frame.SignalsV4[0].TimeframeConfirmation; got == nil || got.Status != "confirmed" || got.LowerTimeframe != "2h" {
		t.Fatalf("4h signal canonical lower timeframe confirmation=%+v", got)
	}

	frame.SignalsV4[0].TimeframeConfirmation = nil
	AttachMappedTimeframe(&frame, Frame{Timeframe: "15m", BarCount: 200, Quality: DataQuality{PriceBasis: "raw"}, Segments: lower.Segments})
	if frame.TimeframeMapping.LowerTimeframeStatus != "awaiting_lower_timeframe" || frame.SignalsV4[0].TimeframeConfirmation.Status != "awaiting_lower_timeframe" {
		t.Fatalf("accepted non-direct 15m confirmation: mapping=%+v confirmation=%+v", frame.TimeframeMapping, frame.SignalsV4[0].TimeframeConfirmation)
	}
}
