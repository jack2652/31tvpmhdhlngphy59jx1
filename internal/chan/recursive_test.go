package chananalysis

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestV4SignalJSONContractForClassesAndWaitingState(t *testing.T) {
	tests := []V4Signal{
		{SignalID: "v4|1|class1_buy|date", Symbol: "QQQ", Level: 1, Kind: "class1_buy", Side: "buy", Status: "observing", Phase: "observation", PhaseAt: "2026-01-12", PhaseHistory: []SignalPhase{{Phase: "observation", At: "2026-01-12"}}, RuleVersion: RecursivePreviewVersion, Date: "2026-01-09", ObservedAt: "2026-01-12", StructureRefs: []int{1, 2}, StructureIDs: []string{"segment-a"}, Conditions: []string{"结构条件"}, Missing: []string{"等待反向确认"}, Invalidation: ptr(90), MarketFilter: "unfiltered", LowerLevelStatus: "awaiting_lower_level"},
		{SignalID: "v4|1|class2_buy|date", Symbol: "QQQ", Level: 1, Kind: "class2_buy", Side: "buy", Status: "awaiting_lower_level", Phase: "awaiting_lower_level", PhaseAt: "2026-01-12", PhaseHistory: []SignalPhase{{Phase: "awaiting_lower_level", At: "2026-01-12"}}, RuleVersion: RecursivePreviewVersion, Date: "2026-01-09", ObservedAt: "2026-01-12", StructureRefs: []int{1, 2}, StructureIDs: []string{"class1-id"}, RelatedSignalID: "class1-id", Conditions: []string{"第一次回试严格高于失效边界"}, Missing: []string{"等待直接下一级别结构"}, Invalidation: ptr(90), MarketFilter: "unfiltered", LowerLevelStatus: "awaiting_lower_level"},
		{SignalID: "v4|1|class3_sell|date", Symbol: "QQQ", Level: 1, Kind: "class3_sell", Side: "sell", Status: "confirmed", Phase: "class3_confirmed", PhaseAt: "2026-01-15", PhaseHistory: []SignalPhase{{Phase: "class3_confirmed", At: "2026-01-15"}}, RuleVersion: RecursivePreviewVersion, Date: "2026-01-12", ObservedAt: "2026-01-15", ConfirmedAt: "2026-01-15", StructureRefs: []int{3, 4}, StructureIDs: []string{"center-id", "departure-id", "retest-id"}, RelatedCenterID: "center-id", Conditions: []string{"回试高点严格低于 ZD"}, Missing: []string{}, Invalidation: ptr(110), MarketFilter: "unfiltered", LowerLevelStatus: "unconfirmed"},
	}
	for _, signal := range tests {
		encoded, err := json.Marshal(signal)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"signal_id", "symbol", "level", "kind", "side", "status", "phase", "phase_at", "phase_history", "rule_version", "date", "observed_at", "structure_refs", "structure_ids", "conditions", "missing_conditions", "invalidation_price", "market_filter", "lower_level_status"} {
			if _, ok := got[key]; !ok {
				t.Fatalf("%s JSON missing %q: %s", signal.Kind, key, encoded)
			}
		}
		if signal.Kind == "class2_buy" && (got["related_signal_id"] != signal.RelatedSignalID || got["lower_level_status"] != "awaiting_lower_level") {
			t.Fatalf("class2 linkage/wait status=%s", encoded)
		}
		if signal.Kind == "class3_sell" && got["related_center_id"] != signal.RelatedCenterID {
			t.Fatalf("class3 center linkage=%s", encoded)
		}
		if got["symbol"] != "QQQ" || got["rule_version"] != RecursivePreviewVersion {
			t.Fatalf("signal identity=%s", encoded)
		}
	}
}

func TestMACDSeriesUsesVersionedInitializationAndWarmup(t *testing.T) {
	closes := []float64{100, 110, 90}
	bars := make([]Bar, 37)
	for index := range bars {
		price := 100.0
		if index < len(closes) {
			price = closes[index]
		}
		bars[index] = Bar{Date: fmt.Sprintf("2026-01-%02d", index+1), Close: price}
	}
	points := macdSeries(bars)
	if len(points) != len(bars) {
		t.Fatalf("MACD points=%d, want %d", len(points), len(bars))
	}
	first := points[0]
	if first.DIF != 0 || first.DEA != 0 || first.Histogram != 0 || !first.Warmup || first.Version != macdVersion {
		t.Fatalf("first MACD point=%+v, want zero seeded values, warmup and %q", first, macdVersion)
	}
	second := points[1]
	wantDIF := 20.0/13 - 20.0/27
	wantDEA := wantDIF * 0.2
	wantHistogram := 2 * (wantDIF - wantDEA)
	if math.Abs(second.DIF-wantDIF) > 1e-12 || math.Abs(second.DEA-wantDEA) > 1e-12 || math.Abs(second.Histogram-wantHistogram) > 1e-12 {
		t.Fatalf("second MACD point=%+v, want DIF=%0.15f DEA=%0.15f histogram=%0.15f", second, wantDIF, wantDEA, wantHistogram)
	}
	if !points[macdWarmupBars-1].Warmup || points[macdWarmupBars].Warmup || points[macdWarmupBars].Version != macdVersion {
		t.Fatalf("MACD warmup boundary: index %d=%+v index %d=%+v", macdWarmupBars-1, points[macdWarmupBars-1], macdWarmupBars, points[macdWarmupBars])
	}
	parameters := RecursiveParameters()["macd"].(map[string]any)
	if parameters["fast"] != 12 || parameters["slow"] != 26 || parameters["signal"] != 9 || parameters["warmup_bars"] != macdWarmupBars || parameters["version"] != macdVersion {
		t.Fatalf("versioned MACD parameters=%#v", parameters)
	}
}

func TestBuildStrengthsIntegratesSameLevelDirectionalAreasAndExposesWindow(t *testing.T) {
	bars := make([]Bar, 42)
	for index := range bars {
		price := 100 + 8*math.Sin(float64(index)*0.37) + 2*math.Sin(float64(index)*0.11)
		bars[index] = Bar{Date: fmt.Sprintf("%02d", index), Close: price}
	}
	segments := []Segment{
		{Direction: "up", Level: 2, Start: "30", End: "36", StartPrice: bars[30].Close, EndPrice: bars[36].Close, Low: bars[30].Close, High: bars[36].Close, Confirmed: true, ConfirmedAt: "37"},
		{Direction: "down", Level: 2, Start: "35", End: "41", StartPrice: bars[35].Close, EndPrice: bars[41].Close, Low: bars[41].Close, High: bars[35].Close, Confirmed: true, ConfirmedAt: "42"},
		{Direction: "up", Level: 1, Start: "35", End: "41", StartPrice: bars[35].Close, EndPrice: bars[41].Close, Low: bars[35].Close, High: bars[41].Close, Confirmed: true, ConfirmedAt: "42"},
		{Direction: "down", Level: 2, Start: "35", End: "41", Confirmed: false, Forming: true},
	}
	strengths := buildStrengths(bars, []LevelAnalysis{{Level: 2, Segments: segments[:2]}, {Level: 1, Segments: segments[2:3]}})
	if len(strengths) != 3 {
		t.Fatalf("strength count=%d, want confirmed segments only across both levels: %+v", len(strengths), strengths)
	}
	macd := macdSeries(bars)
	wantArea := func(direction string, start, end int) float64 {
		area := 0.0
		for index := maxInt(start, macdWarmupBars); index <= end; index++ {
			if direction == "up" {
				area += math.Max(0, macd[index].Histogram)
			} else {
				area += math.Max(0, -macd[index].Histogram)
			}
		}
		return area
	}
	if strengths[0].Level != 2 || strengths[0].Direction != "up" || strengths[0].StartIndex != 30 || strengths[0].EndIndex != 36 || strengths[0].WarmupComplete || strengths[0].WarmupBars != 35 || strengths[0].MACDVersion != macdVersion || strengths[0].MACDArea != wantArea("up", 30, 36) {
		t.Fatalf("prewarm directional strength=%+v", strengths[0])
	}
	if strengths[1].Level != 2 || strengths[1].Direction != "down" || !strengths[1].WarmupComplete || strengths[1].MACDArea != wantArea("down", 35, 41) || strengths[1].Start != "35" || strengths[1].End != "41" {
		t.Fatalf("downward strength provenance/area=%+v", strengths[1])
	}
	if strengths[2].Level != 1 || strengths[2].Direction != "up" || strengths[2].MACDArea != wantArea("up", 35, 41) {
		t.Fatalf("same dates at a different recursive level were mixed: %+v", strengths[2])
	}
	for index, strength := range strengths {
		segment := []Segment{segments[0], segments[1], segments[2]}[index]
		duration := strength.EndIndex - strength.StartIndex
		wantSlope := 100 * (segment.EndPrice/segment.StartPrice - 1) / float64(maxInt(1, duration))
		wantAmplitude := 100 * (segment.High - segment.Low) / segment.StartPrice
		if math.Abs(strength.SlopePercent-wantSlope) > 1e-12 || strength.DurationBars != maxInt(1, duration) || math.Abs(strength.Amplitude-wantAmplitude) > 1e-12 {
			t.Fatalf("auxiliary metrics[%d]=%+v want slope=%v percent/bar, duration=%d bars, amplitude=%v percent", index, strength, wantSlope, maxInt(1, duration), wantAmplitude)
		}
	}
	if strengths[0].Amplitude != 100*(segments[0].High-segments[0].Low)/segments[0].StartPrice {
		t.Fatal("amplitude should remain an independently reported metric")
	}
}

func TestRecursiveInputsUseConsistentPriceAndMidpointDates(t *testing.T) {
	segments := withConfirmedSegmentEndpoints([]Segment{
		{Direction: "up", Chain: 0, Start: "2026-01-05", End: "2026-01-09", StartPrice: 10, EndPrice: 20, Low: 10, High: 20, Confirmed: true, ConfirmedAt: "2026-01-12"},
		{Direction: "down", Chain: 0, Start: "2026-01-09", End: "2026-01-15", StartPrice: 20, EndPrice: 12, Low: 11, High: 20, Confirmed: true, ConfirmedAt: "2026-01-16"},
		{Direction: "up", Chain: 0, Start: "2026-01-15", End: "2026-01-23", StartPrice: 12, EndPrice: 22, Low: 12, High: 22, Confirmed: true, ConfirmedAt: "2026-01-26"},
		{Direction: "down", Chain: 0, Start: "2026-01-23", End: "2026-01-27", StartPrice: 22, EndPrice: 14, Low: 13, High: 22, Confirmed: true, ConfirmedAt: "2026-01-28"},
		{Direction: "up", Chain: 0, Start: "2026-01-27", End: "2026-01-30", StartPrice: 14, EndPrice: 18, Low: 14, High: 18, Confirmed: true, ConfirmedAt: "2026-02-02"},
		{Direction: "down", Chain: 0, Start: "2026-01-30", End: "2026-02-03", StartPrice: 18, EndPrice: 11, Low: 10, High: 18, Confirmed: true, ConfirmedAt: "2026-02-04"},
	})
	inputs := recursiveInputStrokes(segments, 1)
	if len(inputs) != 3 {
		t.Fatalf("recursive inputs=%+v, want continuous higher-level strokes", inputs)
	}
	if inputs[0].Start.Date != "2026-01-12" || inputs[0].Start.Price != 16 || inputs[0].End.Date != "2026-01-19" || inputs[0].End.Price != 17 {
		t.Fatalf("first recursive input endpoints=%+v, want midpoint date/price", inputs[0])
	}
	if inputs[0].ConfirmedAt != "2026-01-28" || !inputs[0].Confirmed || inputs[1].Start.Date != inputs[0].End.Date || inputs[1].Start.Price != inputs[0].End.Price {
		t.Fatalf("recursive confirmation/continuity=%+v, want confirmed continuous pivots", inputs[:2])
	}
}

func TestRecursiveInputsRejectDisconnectedSegments(t *testing.T) {
	segments := withConfirmedSegmentEndpoints([]Segment{
		{Direction: "up", Chain: 0, Start: "2026-01-05", End: "2026-01-07", StartPrice: 10, EndPrice: 20, Confirmed: true, ConfirmedAt: "2026-01-08"},
		{Direction: "down", Chain: 0, Start: "2026-01-09", End: "2026-01-11", StartPrice: 20, EndPrice: 12, Confirmed: true, ConfirmedAt: "2026-01-12"},
		{Direction: "up", Chain: 0, Start: "2026-01-11", End: "2026-01-13", StartPrice: 12, EndPrice: 22, Confirmed: true, ConfirmedAt: "2026-01-14"},
	})
	if got := recursiveInputStrokes(segments, 1); len(got) != 0 {
		t.Fatalf("recursive inputs=%+v, want disconnected triple rejected", got)
	}
}

func TestBuildRecursiveLevelsDoesNotPromoteFormingLowerObjects(t *testing.T) {
	baseStrokes := []Stroke{{Confirmed: true}, {Confirmed: false}}
	baseSegments := []Segment{
		{Chain: 0, Level: 0, Direction: "up", Start: "01", End: "02", StartKind: "bottom", EndKind: "top", Low: 1, High: 10, Confirmed: true},
		{Chain: 0, Level: 0, Direction: "down", Start: "02", End: "03", StartKind: "top", EndKind: "bottom", Low: 4, High: 12, Confirmed: false, Forming: true},
		{Chain: 0, Level: 0, Direction: "up", Start: "03", End: "04", StartKind: "bottom", EndKind: "top", Low: 3, High: 11, Confirmed: true},
	}
	levels := buildRecursiveLevels(baseStrokes, baseSegments)
	if len(levels) < 2 || levels[0].Status != "forming" || levels[0].WaitingFor == "" || levels[1].Status != "awaiting_lower_level" || levels[1].InputCount != 0 {
		t.Fatalf("levels=%+v, want forming level 0 and empty awaiting level 1", levels)
	}
}

func withConfirmedSegmentEndpoints(segments []Segment) []Segment {
	for index := range segments {
		if segments[index].Direction == "up" {
			segments[index].StartKind = "bottom"
			segments[index].EndKind = "top"
		} else {
			segments[index].StartKind = "top"
			segments[index].EndKind = "bottom"
		}
	}
	return segments
}

func TestBuildV4SignalsRequiresConfirmedDepartureRetestAndWeakeningExtreme(t *testing.T) {
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: 0, Start: "01", End: "02", StartPrice: 4, EndPrice: 11, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: 0, Start: "02", End: "03", StartPrice: 11, EndPrice: 7, Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, Start: "03", End: "04", StartPrice: 7, EndPrice: 9, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "04"},
		// First non-overlapping reverse segment closes the center.
		{Direction: "down", Chain: 0, Level: 0, Start: "04", End: "05", StartPrice: 9, EndPrice: 6, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: 0, Start: "05", End: "06", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: 0, Start: "06", End: "07", StartPrice: 14, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "07"},
		{Direction: "up", Chain: 0, Level: 0, Start: "07", End: "08", StartPrice: 12, EndPrice: 16, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "08"},
		{Direction: "down", Chain: 0, Level: 0, Start: "08", End: "09", StartPrice: 16, EndPrice: 13, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "09"},
		{Direction: "up", Chain: 0, Level: 0, Start: "09", End: "10", StartPrice: 13, EndPrice: 15, Low: 13, High: 15, Confirmed: true, ConfirmedAt: "10"},
		{Direction: "down", Chain: 0, Level: 0, Start: "10", End: "11", StartPrice: 15, EndPrice: 11, Low: 11, High: 15, Confirmed: true, ConfirmedAt: "11"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, Lower: 5, Upper: 10, Start: "01", End: "04", FormedAt: "04", ConfirmedAt: "05", StartSegment: 0, EndSegment: 2, Forming: false}
	strengths := []Strength{
		{Direction: "up", Level: 0, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true},
		{Direction: "up", Level: 0, Start: "07", End: "08", MACDArea: 7.9, WarmupComplete: true},
	}
	levels := []LevelAnalysis{{Level: 0, Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}}
	signals := buildV4Signals(Frame{}, levels, strengths)
	var signal *V4Signal
	for index := range signals {
		if signals[index].Kind == "class1_sell" {
			signal = &signals[index]
			break
		}
	}
	if signal == nil {
		t.Fatalf("signals=%+v, want one divergence candidate from two confirmed pushes around an outside retest", signals)
	}
	if signal.Status != "reversal_confirmed" || signal.Phase != "reversal_confirmed" || signal.Date != "08" || signal.ObservedAt != "08" || signal.ConfirmedAt != "09" || signal.PhaseAt != "11" || len(signal.PhaseHistory) != 3 || signal.Price == nil || *signal.Price != 16 || signal.Invalidation == nil || *signal.Invalidation != 16 || signal.StrengthRatio == nil || math.Abs(*signal.StrengthRatio-0.79) > 1e-12 {
		t.Fatalf("divergence state=%+v, want endpoint date, observation, divergence confirmation, reversal confirmation, price and ratio", *signal)
	}
	if len(signal.Missing) != 0 || len(signal.Conditions) < 5 {
		t.Fatalf("signal evidence=%+v, want all required evidence and no missing conditions after reversal confirmation", *signal)
	}
	for _, state := range []struct {
		segments []Segment
		status   string
		phase    string
		wantHist int
	}{
		{segments: segments[:7], status: "observing", phase: "observation", wantHist: 1},
		{segments: segments[:8], status: "divergence_confirmed", phase: "divergence_confirmed", wantHist: 2},
		{segments: segments, status: "reversal_confirmed", phase: "reversal_confirmed", wantHist: 3},
	} {
		candidate := buildV4Signals(Frame{}, []LevelAnalysis{{Level: 0, Segments: state.segments, Centers: []RecursiveCenter{center}}}, strengths)
		var divergence *V4Signal
		for index := range candidate {
			if candidate[index].Kind == "class1_sell" {
				divergence = &candidate[index]
				break
			}
		}
		if divergence == nil || divergence.Status != state.status || divergence.Phase != state.phase || len(divergence.PhaseHistory) != state.wantHist {
			t.Fatalf("lifecycle for %s=%+v, want phase %s history length %d", state.status, candidate, state.phase, state.wantHist)
		}
	}
	invalidated := buildV4Signals(Frame{Candles: []Bar{{Date: "09", High: 16, Low: 12}}}, levels, strengths)
	var invalidatedDivergence *V4Signal
	for index := range invalidated {
		if invalidated[index].Kind == "class1_sell" {
			invalidatedDivergence = &invalidated[index]
			break
		}
	}
	if invalidatedDivergence == nil || invalidatedDivergence.Status != "invalidated" || invalidatedDivergence.Phase != "invalidated" || invalidatedDivergence.PhaseAt != "09" || invalidatedDivergence.ConfirmedAt != "" || invalidatedDivergence.PhaseHistory[len(invalidatedDivergence.PhaseHistory)-1] != (SignalPhase{Phase: "invalidated", At: "09"}) {
		t.Fatalf("invalidated lifecycle=%+v, want post-observation invalidation preserving chronological history", invalidated)
	}
}

func TestV4SignalInvalidationStartsAfterObservationAndKeepsPhaseHistory(t *testing.T) {
	bars := []Bar{
		{Date: "07", High: 20, Low: 12}, // before observation; ignored
		{Date: "08", High: 16, Low: 12}, // observation bar; ignored
		{Date: "09", High: 16, Low: 12}, // first later touch of low boundary
	}
	if got := firstV4InvalidationAt(bars, "08", "up", 16); got != "09" {
		t.Fatalf("invalidation date=%q, want first post-observation touch at 09", got)
	}
	if got := firstV4InvalidationAt(bars, "08", "down", 12); got != "09" {
		t.Fatalf("down invalidation date=%q, want first post-observation touch at 09", got)
	}
	if got := firstV4InvalidationAt(bars[:2], "08", "up", 16); got != "" {
		t.Fatalf("invalidation date=%q, observation bar must not invalidate itself", got)
	}
}

func TestBuildTrendContextReportsCenterRelationAndReturnState(t *testing.T) {
	center := RecursiveCenter{Level: 1, Chain: 2, CenterID: "L1-C2-S0-E2", Lower: 10, Upper: 20, EndSegment: 1, ConfirmedAt: "04", RelationToPrevious: "separate_up"}
	segments := []Segment{
		{Direction: "up", Chain: 2, Level: 1, EndPrice: 14, Low: 10, High: 16, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "down", Chain: 2, Level: 1, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "up", Chain: 2, Level: 1, EndPrice: 25, Low: 20, High: 26, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "down", Chain: 2, Level: 1, EndPrice: 18, Low: 17, High: 25, Confirmed: true, ConfirmedAt: "06"},
	}
	context := buildTrendContext(LevelAnalysis{Level: 1, Segments: segments, Centers: []RecursiveCenter{center}}, "returning")
	if context.Status != "returning" || context.Direction != "down" || context.Level != 1 || context.CenterID != center.CenterID || context.CenterRelation != "separate_up" || context.CenterPosition != "inside" || context.DepartureStatus != "returned_to_center" || context.AsOf != "06" || context.Reason == "" {
		t.Fatalf("trend context=%+v, want explained return into last confirmed center", context)
	}
}

func TestBuildTrendContextIgnoresFormingObjects(t *testing.T) {
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "closed", Lower: 5, Upper: 10, EndSegment: 0, ConfirmedAt: "03"}
	segments := []Segment{
		{Direction: "down", Chain: 0, Level: 0, EndPrice: 8, Low: 7, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, EndPrice: 12, Low: 8, High: 13, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: 0, EndPrice: 9, Low: 7, High: 12, Forming: true},
	}
	context := buildTrendContext(LevelAnalysis{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}, "departing")
	if context.Direction != "up" || context.AsOf != "04" || context.Status != "departing" || context.CenterPosition != "above" {
		t.Fatalf("trend context=%+v, forming segment must not replace last confirmed state", context)
	}
}

func TestBuildV4SignalsRejectsDivergenceWithoutRequiredBackground(t *testing.T) {
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: 0, Start: "01", End: "02", EndPrice: 11, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: 0, Start: "02", End: "03", Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, Start: "03", End: "04", EndPrice: 9, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: 0, Start: "04", End: "05", Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: 0, Start: "05", End: "06", EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: 0, Start: "06", End: "07", Low: 9, High: 14, Confirmed: true, ConfirmedAt: "07"}, // enters center
		{Direction: "up", Chain: 0, Level: 0, Start: "07", End: "08", EndPrice: 16, Low: 9, High: 16, Confirmed: true, ConfirmedAt: "08"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, Lower: 5, Upper: 10, FormedAt: "04", ConfirmedAt: "05", EndSegment: 2}
	strengths := []Strength{{Direction: "up", Level: 0, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true}, {Direction: "up", Level: 0, Start: "07", End: "08", MACDArea: 7, WarmupComplete: true}}
	levels := []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}}
	got := buildV4Signals(Frame{}, levels, strengths)
	for _, signal := range got {
		if signal.Kind == "class1_buy" || signal.Kind == "class1_sell" {
			t.Fatalf("signals=%+v, want no divergence signal because intervening retest entered the center", got)
		}
	}
	if signal := findV4Signal(got, "class3_sell"); signal == nil || signal.Status != "invalidated" {
		t.Fatalf("signals=%+v, want the failed first center retest retained as invalidated", got)
	}
}

func TestRecursiveCenterListRebasesPreviousCenterIndexes(t *testing.T) {
	previous := RecursiveCenter{Level: 0, CenterID: "L0-C0-S0-E2"}
	current := RecursiveCenter{Level: 0, CenterID: "L0-C0-S4-E6", PreviousCenterID: previous.CenterID, RelatedCenter: func() *int { value := 0; return &value }()}
	otherLevel := RecursiveCenter{Level: 1, CenterID: "L1-C0-S0-E2"}
	levels := []LevelAnalysis{
		{Level: 1, Centers: []RecursiveCenter{otherLevel}},
		{Level: 0, Centers: []RecursiveCenter{previous, current}},
	}
	flattened := recursiveCenterList(levels)
	if len(flattened) != 3 || flattened[2].RelatedCenter == nil || *flattened[2].RelatedCenter != 1 || flattened[2].PreviousCenterID != flattened[1].CenterID {
		t.Fatalf("flattened centers=%+v, want stable cross-list relation to flattened index 1", flattened)
	}
}
