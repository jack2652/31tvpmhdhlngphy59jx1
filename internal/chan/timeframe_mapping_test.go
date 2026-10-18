package chananalysis

import "testing"

func TestTimeframeRelationLadderIsExplicitAndDirect(t *testing.T) {
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
	if higher.TimeframeMapping.LowerTimeframe != "1h" || higher.TimeframeMapping.LowerTimeframeStatus != "awaiting_lower_timeframe" || higher.SignalsV4[0].TimeframeConfirmation.Status != "awaiting_lower_timeframe" {
		t.Fatalf("missing 1h data not exposed: %+v signal=%+v", higher.TimeframeMapping, higher.SignalsV4[0].TimeframeConfirmation)
	}
}

func TestFourHourAnalyzeIntradayKeepsSignalsAndMarksAwaiting15MinuteConfirmation(t *testing.T) {
	frame := Frame{Timeframe: "4h", AsOf: "2026-01-16 16:00", BarCount: 100, Quality: DataQuality{PriceBasis: "raw"}, SignalsV4: []V4Signal{{SignalID: "4h-buy", Side: "buy", ObservedAt: "2026-01-15 16:00"}}}
	lower := Frame{Timeframe: "15m", BarCount: 200, Quality: DataQuality{PriceBasis: "raw"}, Segments: []Segment{{Level: 0, Direction: "down", Start: "2026-01-16 09:30", End: "2026-01-16 11:00", Chain: 1, Confirmed: true, ConfirmedAt: "2026-01-16 11:15"}, {Level: 0, Direction: "up", Start: "2026-01-16 11:00", End: "2026-01-16 14:00", Chain: 1, Confirmed: true, ConfirmedAt: "2026-01-16 14:15"}}}
	AttachMappedTimeframe(&frame, lower)
	if frame.TimeframeMapping.LowerTimeframeStatus != "ready" || len(frame.SignalsV4) != 1 {
		t.Fatalf("4h frame mapping/signals=%+v %+v", frame.TimeframeMapping, frame.SignalsV4)
	}
	if got := frame.SignalsV4[0].TimeframeConfirmation; got == nil || got.Status != "confirmed" || got.LowerTimeframe != "15m" {
		t.Fatalf("4h signal lower timeframe confirmation=%+v", got)
	}
}
