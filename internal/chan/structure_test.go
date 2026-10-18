package chananalysis

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestMergeBarsAppliesDirectionAndPreservesRawMapping(t *testing.T) {
	bars := []Bar{
		{Date: "01", High: 10, Low: 5, Chain: 0, SourceIndex: 10, HasSourceIndex: true},
		{Date: "02", High: 12, Low: 7, Chain: 0, SourceIndex: 11, HasSourceIndex: true}, // establishes upward direction
		{Date: "03", High: 11, Low: 8, Chain: 0, SourceIndex: 12, HasSourceIndex: true}, // inclusion raises the lower edge
		{Date: "04", High: 11, Low: 9, Chain: 0, SourceIndex: 13, HasSourceIndex: true}, // inclusion raises it again
		{Date: "05", High: 12, Low: 9, Chain: 0, SourceIndex: 14, HasSourceIndex: true}, // equal extremes use later source bar
	}
	merged := mergeBars(bars)
	if len(merged) != 2 {
		t.Fatalf("merged count=%d, want 2: %+v", len(merged), merged)
	}
	last := merged[1]
	if last.high != 12 || last.low != 9 || last.start != 1 || last.end != 4 || last.sourceStart != 11 || last.sourceEnd != 14 || last.sourceHigh != 14 || last.sourceLow != 14 {
		t.Fatalf("merged inclusion/mapping=%+v, want [9,12], raw bars 1..4 and later equal-extreme sources", last)
	}
	output := mergedCandleOutput(merged)
	if output[1].RawStart != 1 || output[1].RawEnd != 4 || output[1].SourceRawStart != 11 || output[1].SourceRawEnd != 14 || output[1].HighRawBar != 14 || output[1].LowRawBar != 14 {
		t.Fatalf("merged candle output=%+v, want raw and source mappings preserved", output[1])
	}
}

func TestMergeBarsDoesNotJoinInclusionsAcrossChains(t *testing.T) {
	bars := []Bar{
		{Date: "01", High: 10, Low: 5, Chain: 0},
		{Date: "02", High: 9, Low: 6, Chain: 0},
		{Date: "03", High: 9, Low: 6, Chain: 1},
	}
	merged := mergeBars(bars)
	if len(merged) != 2 || merged[0].start != 0 || merged[0].end != 1 || merged[1].start != 2 || merged[1].end != 2 || merged[0].chain == merged[1].chain {
		t.Fatalf("merged chain result=%+v, want first two bars combined and new chain separate", merged)
	}
}

func TestDirectionInitializationKeepsUnknownInclusionUntilAnUnambiguousPair(t *testing.T) {
	bars := []Bar{
		{Date: "01", High: 10, Low: 5, Chain: 0},
		{Date: "02", High: 9, Low: 6, Chain: 0},  // contained while direction is unknown: retain union
		{Date: "03", High: 11, Low: 6, Chain: 0}, // establishes upward direction
		{Date: "04", High: 10, Low: 7, Chain: 0}, // upward inclusion raises the lower edge
	}
	merged := mergeBars(bars)
	if len(merged) != 2 {
		t.Fatalf("merged=%+v, want first two unknown-direction bars combined", merged)
	}
	if merged[0].high != 10 || merged[0].low != 5 || merged[0].start != 0 || merged[0].end != 1 {
		t.Fatalf("unknown-direction inclusion=%+v, want union [5,10] over raw bars 0..1", merged[0])
	}
	if merged[1].high != 11 || merged[1].low != 7 || merged[1].start != 2 || merged[1].end != 3 {
		t.Fatalf("upward inclusion=%+v, want directional bounds [7,11] over raw bars 2..3", merged[1])
	}
}

func TestDirectionFromPairUsesTheMovingEdgeWhenTheOtherEdgeIsEqual(t *testing.T) {
	cases := []struct {
		name                                         string
		previousHigh, previousLow, nextHigh, nextLow float64
		want                                         int
	}{
		{name: "equal highs move upward by low", previousHigh: 10, previousLow: 5, nextHigh: 10, nextLow: 6, want: 1},
		{name: "equal highs move downward by low", previousHigh: 10, previousLow: 6, nextHigh: 10, nextLow: 5, want: -1},
		{name: "equal lows move upward by high", previousHigh: 10, previousLow: 5, nextHigh: 11, nextLow: 5, want: 1},
		{name: "equal lows move downward by high", previousHigh: 11, previousLow: 5, nextHigh: 10, nextLow: 5, want: -1},
		{name: "identical range remains unknown", previousHigh: 10, previousLow: 5, nextHigh: 10, nextLow: 5, want: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := directionFromPair(testCase.previousHigh, testCase.previousLow, testCase.nextHigh, testCase.nextLow); got != testCase.want {
				t.Fatalf("directionFromPair()=%d, want %d", got, testCase.want)
			}
		})
	}
}

func TestFractalCarriesRawRangeLevelAndFirstObservation(t *testing.T) {
	bars := []Bar{
		{Date: "01", SourceIndex: 100, HasSourceIndex: true},
		{Date: "02", SourceIndex: 101, HasSourceIndex: true},
		{Date: "03", SourceIndex: 102, HasSourceIndex: true},
		{Date: "04", SourceIndex: 103, HasSourceIndex: true},
		{Date: "05", SourceIndex: 104, HasSourceIndex: true},
	}
	merged := []mergedBar{
		{high: 10, low: 5, highIndex: 0, lowIndex: 0, start: 0, end: 0, sourceHigh: 100, sourceLow: 100, sourceStart: 100, sourceEnd: 100, chain: 2},
		{high: 11, low: 6, highIndex: 1, lowIndex: 1, start: 1, end: 1, sourceHigh: 101, sourceLow: 101, sourceStart: 101, sourceEnd: 101, chain: 2},
		{high: 15, low: 10, highIndex: 2, lowIndex: 2, start: 2, end: 2, sourceHigh: 102, sourceLow: 102, sourceStart: 102, sourceEnd: 102, chain: 2},
		{high: 12, low: 8, highIndex: 3, lowIndex: 3, start: 3, end: 3, sourceHigh: 103, sourceLow: 103, sourceStart: 103, sourceEnd: 103, chain: 2},
		{high: 13, low: 9, highIndex: 4, lowIndex: 4, start: 4, end: 4, sourceHigh: 104, sourceLow: 104, sourceStart: 104, sourceEnd: 104, chain: 2},
	}
	fractals := findFractals(bars, merged)
	if len(fractals) != 1 {
		t.Fatalf("fractals=%+v, want one fixed top", fractals)
	}
	fractal := fractals[0]
	if fractal.Kind != "top" || fractal.Date != "03" || fractal.Price != 15 || fractal.Level != 0 || fractal.FirstObservedAt != "05" || fractal.ConfirmedAt != "05" || fractal.RawStartIndex != 2 || fractal.RawEndIndex != 2 || fractal.SourceRawIndex != 102 || fractal.SourceRawStart != 102 || fractal.SourceRawEnd != 102 || fractal.Chain != 2 {
		t.Fatalf("fractal provenance=%+v, want exact raw range and observation metadata", fractal)
	}
}

func TestFractalsRequireStrictHighAndLowExtremesAndStayWithinOneChain(t *testing.T) {
	makeBars := func(highs, lows []float64, chains []int) ([]Bar, []mergedBar) {
		bars := make([]Bar, len(highs))
		merged := make([]mergedBar, len(highs))
		for index := range highs {
			bars[index] = Bar{Date: fmt.Sprintf("%02d", index)}
			merged[index] = mergedBar{high: highs[index], low: lows[index], highIndex: index, lowIndex: index, start: index, end: index, chain: chains[index]}
		}
		return bars, merged
	}
	cases := []struct {
		name        string
		highs, lows []float64
		chains      []int
	}{
		{name: "equal high does not make a top", highs: []float64{10, 15, 15, 12}, lows: []float64{5, 9, 8, 6}, chains: []int{0, 0, 0, 0}},
		{name: "equal low does not make a bottom", highs: []float64{15, 12, 13, 14}, lows: []float64{10, 5, 5, 7}, chains: []int{0, 0, 0, 0}},
		{name: "chain boundary cannot confirm a top", highs: []float64{10, 15, 11, 12}, lows: []float64{5, 9, 8, 6}, chains: []int{0, 0, 0, 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			bars, merged := makeBars(testCase.highs, testCase.lows, testCase.chains)
			if fractals := findFractals(bars, merged); len(fractals) != 0 {
				t.Fatalf("fractals=%+v, want none", fractals)
			}
		})
	}
}

func TestStrokeBuilderKeepsOnlyTheMostExtremeSameKindFractal(t *testing.T) {
	merged := make([]mergedBar, 9)
	for index := range merged {
		merged[index] = mergedBar{high: 20, low: 10, highIndex: index, lowIndex: index, start: index, end: index, chain: 0}
	}
	bars := make([]Bar, len(merged))
	fractals := []Fractal{
		{Kind: "bottom", Date: "01", Price: 10, MergedIndex: 0, RawIndex: 0, Chain: 0},
		{Kind: "top", Date: "05", Price: 20, MergedIndex: 4, RawIndex: 4, Chain: 0},
		{Kind: "bottom", Date: "08", Price: 12, MergedIndex: 7, RawIndex: 7, Chain: 0}, // too close to form a pen
		{Kind: "top", Date: "09", Price: 22, MergedIndex: 8, RawIndex: 8, Chain: 0},
	}
	strokes, _ := buildStrokes(fractals, merged, bars)
	if len(strokes) != 1 || strokes[0].Start.Price != 10 || strokes[0].End.Price != 22 || strokes[0].End.Date != "09" || strokes[0].Confirmed {
		t.Fatalf("strokes=%+v, want one forming pen ending at the more-extreme top 22", strokes)
	}
}

func TestStrokeBuilderKeepsTheEarlierFractalWhenSameKindPricesTie(t *testing.T) {
	cases := []struct {
		name         string
		fractals     []Fractal
		setRanges    func([]mergedBar)
		wantFirstEnd string
	}{
		{name: "equal tops keep the earlier one", fractals: []Fractal{
			{Kind: "bottom", Date: "01", Price: 10, MergedIndex: 0, RawIndex: 0, Chain: 0},
			{Kind: "top", Date: "05", Price: 20, MergedIndex: 4, RawIndex: 4, Chain: 0},
			{Kind: "top", Date: "07", Price: 20, MergedIndex: 6, RawIndex: 6, Chain: 0},
		}, setRanges: func(merged []mergedBar) {
			for index := 0; index < len(merged); index++ {
				merged[index].low, merged[index].high = 10, 20
			}
		}, wantFirstEnd: "05"},
		{name: "equal bottoms keep the earlier one", fractals: []Fractal{
			{Kind: "top", Date: "01", Price: 20, MergedIndex: 0, RawIndex: 0, Chain: 0},
			{Kind: "bottom", Date: "05", Price: 10, MergedIndex: 4, RawIndex: 4, Chain: 0},
			{Kind: "bottom", Date: "07", Price: 10, MergedIndex: 6, RawIndex: 6, Chain: 0},
		}, setRanges: func(merged []mergedBar) {
			for index := 0; index < len(merged); index++ {
				merged[index].low, merged[index].high = 10, 20
			}
		}, wantFirstEnd: "05"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			merged := make([]mergedBar, 7)
			for index := range merged {
				merged[index] = mergedBar{high: 22, low: 8, highIndex: index, lowIndex: index, start: index, end: index, chain: 0}
			}
			testCase.setRanges(merged)
			bars := make([]Bar, len(merged))
			strokes, _ := buildStrokes(testCase.fractals, merged, bars)
			if len(strokes) != 1 || strokes[0].End.Date != testCase.wantFirstEnd || strokes[0].Confirmed {
				t.Fatalf("strokes=%+v, want first endpoint date %s", strokes, testCase.wantFirstEnd)
			}
		})
	}
}

func TestFastReversalRestartsPenChainWithoutJoiningTheNewEndpoint(t *testing.T) {
	merged := make([]mergedBar, 16)
	for index := range merged {
		merged[index] = mergedBar{high: 20, low: 10, highIndex: index, lowIndex: index, start: index, end: index, chain: 0}
	}
	for index := 4; index <= 8; index++ {
		merged[index].low = 12
	}
	merged[9].low = 11 // invalidate the last bottom before a minimum-gap top can form
	merged[11].high = 22
	for index := 12; index < len(merged); index++ {
		merged[index].high, merged[index].low = 18, 8
	}
	bars := make([]Bar, len(merged))
	fractals := []Fractal{
		{Kind: "bottom", Date: "01", Price: 10, MergedIndex: 0, RawIndex: 0, Chain: 0},
		{Kind: "top", Date: "05", Price: 20, MergedIndex: 4, RawIndex: 4, Chain: 0},
		{Kind: "bottom", Date: "09", Price: 12, MergedIndex: 8, RawIndex: 8, Chain: 0},
		{Kind: "top", Date: "12", Price: 22, MergedIndex: 11, RawIndex: 11, Chain: 0}, // less than four merged bars after the bottom
		{Kind: "bottom", Date: "16", Price: 8, MergedIndex: 15, RawIndex: 15, Chain: 0},
	}
	strokes, restartedAt := buildStrokes(fractals, merged, bars)
	if restartedAt != "12" {
		t.Fatalf("restart date=%q, want 12", restartedAt)
	}
	if len(strokes) != 2 || strokes[0].Chain != 0 || !strokes[0].Confirmed || strokes[0].Start.Date != "01" || strokes[0].End.Date != "05" || strokes[1].Chain != -1 || strokes[1].Start.Date != "12" || strokes[1].End.Date != "16" || strokes[1].Confirmed {
		t.Fatalf("strokes=%+v, want preserved confirmed pen and a separate forming pen after restart", strokes)
	}
}

func TestStrokeCarriesFractalAndInputBarProvenance(t *testing.T) {
	merged := make([]mergedBar, 9)
	bars := make([]Bar, 9)
	for index := range merged {
		merged[index] = mergedBar{high: 20, low: 10, highIndex: index, lowIndex: index, start: index, end: index, sourceHigh: 200 + index, sourceLow: 200 + index, sourceStart: 200 + index, sourceEnd: 200 + index, chain: 4}
		bars[index] = Bar{Date: string(rune('a' + index)), Close: 15}
	}
	points := []Fractal{
		{Kind: "bottom", Date: "a", Price: 10, Level: 0, ConfirmedAt: "c", MergedIndex: 0, RawIndex: 0, SourceRawIndex: 200, Chain: 4},
		{Kind: "top", Date: "e", Price: 20, Level: 0, ConfirmedAt: "f", MergedIndex: 4, RawIndex: 4, SourceRawIndex: 204, Chain: 4},
		{Kind: "bottom", Date: "i", Price: 10, Level: 0, ConfirmedAt: "j", MergedIndex: 8, RawIndex: 8, SourceRawIndex: 208, Chain: 4},
	}
	strokes, _ := buildStrokes(points, merged, bars)
	if len(strokes) != 2 || !strokes[0].Confirmed || strokes[0].ConfirmedAt != "j" {
		t.Fatalf("strokes=%+v, want two strokes and first confirmed by third fractal", strokes)
	}
	stroke := strokes[0]
	if stroke.Direction != "up" || stroke.Level != 0 || stroke.Start.MergedIndex != 0 || stroke.End.MergedIndex != 4 || stroke.Start.SourceRawIndex != 200 || stroke.End.SourceRawIndex != 204 || stroke.StartBar != 0 || stroke.EndBar != 4 || stroke.StartRawBar != 200 || stroke.EndRawBar != 204 || stroke.InputStartBar != 0 || stroke.InputEndBar != 4 {
		t.Fatalf("stroke provenance=%+v, want fractal, level, raw and input ranges", stroke)
	}
}

func TestConfirmedStructuresRemainStableAsBarsAppend(t *testing.T) {
	bars := make([]Bar, 180)
	for index := range bars {
		price := 100 + 12*math.Sin(float64(index)*0.31) + 5*math.Sin(float64(index)*0.071)
		bars[index] = Bar{Date: fmt.Sprintf("%04d", index), Open: price, High: price + 0.7, Low: price - 0.7, Close: price + 0.05*math.Sin(float64(index)), Volume: 100, Chain: 0}
	}
	var previous Frame
	for end := 20; end <= len(bars); end++ {
		current := addRecursiveAnalysis(Frame{}, bars[:end])
		if end > 20 {
			asOf := bars[end-2].Date
			if !reflect.DeepEqual(confirmedFractals(previous.FractalsV4, asOf), confirmedFractals(current.FractalsV4, asOf)) {
				old, next := confirmedFractals(previous.FractalsV4, asOf), confirmedFractals(current.FractalsV4, asOf)
				t.Fatalf("confirmed fractals changed after append at prefix=%d old=%+v new=%+v", end, old, next)
			}
			if !reflect.DeepEqual(confirmedStrokes(previous.StrokesV4, asOf), confirmedStrokes(current.StrokesV4, asOf)) {
				t.Fatalf("confirmed strokes changed after append at prefix=%d", end)
			}
			if !reflect.DeepEqual(confirmedSegments(previous.Segments, asOf), confirmedSegments(current.Segments, asOf)) {
				t.Fatalf("confirmed segments changed after append at prefix=%d", end)
			}
			if !reflect.DeepEqual(confirmedRecursiveCenters(previous.RecursiveCenters, asOf), confirmedRecursiveCenters(current.RecursiveCenters, asOf)) {
				t.Fatalf("confirmed centers changed after append at prefix=%d", end)
			}
		}
		previous = current
	}
	if len(previous.FractalsV4) < 3 || len(confirmedStrokes(previous.StrokesV4, bars[len(bars)-1].Date)) < 2 {
		t.Fatalf("fixture did not exercise confirmed fractals/strokes: fractals=%d strokes=%d", len(previous.FractalsV4), len(confirmedStrokes(previous.StrokesV4, bars[len(bars)-1].Date)))
	}
}

func TestConfirmedSegmentsRemainStableAsStrokesAppend(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	var previousSegments []Segment
	var previousCenters []RecursiveCenter
	for end := 3; end <= len(strokes); end++ {
		currentSegments := buildSegments(strokes[:end])
		currentCenters := buildRecursiveCenters(currentSegments)
		asOf := strokes[end-2].ConfirmedAt
		if end > 3 {
			if !reflect.DeepEqual(confirmedSegments(previousSegments, asOf), confirmedSegments(currentSegments, asOf)) {
				t.Fatalf("confirmed segments changed after stroke append end=%d asof=%s old=%+v new=%+v", end, asOf, confirmedSegments(previousSegments, asOf), confirmedSegments(currentSegments, asOf))
			}
			if !reflect.DeepEqual(confirmedRecursiveCenters(previousCenters, asOf), confirmedRecursiveCenters(currentCenters, asOf)) {
				t.Fatalf("confirmed centers changed after stroke append end=%d", end)
			}
		}
		previousSegments, previousCenters = currentSegments, currentCenters
	}
	if len(confirmedSegments(previousSegments, strokes[len(strokes)-1].ConfirmedAt)) == 0 {
		t.Fatalf("fixture did not exercise a confirmed segment: %+v", previousSegments)
	}
}

func TestConfirmedCentersRemainStableAsSegmentsAppend(t *testing.T) {
	segments := withAlternatingEndpointKinds([]Segment{
		{Chain: 0, Level: 1, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 1, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
		{Chain: 0, Level: 1, Direction: "down", Start: "04", End: "05", Low: 5, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Chain: 0, Level: 1, Direction: "up", Start: "05", End: "06", Low: 11, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Chain: 0, Level: 1, Direction: "down", Start: "06", End: "07", Low: 10, High: 13, Confirmed: true, ConfirmedAt: "07"},
		{Chain: 0, Level: 1, Direction: "up", Start: "07", End: "08", Low: 9, High: 12, Confirmed: true, ConfirmedAt: "08"},
		{Chain: 0, Level: 1, Direction: "down", Start: "08", End: "09", Low: 11, High: 14, Confirmed: true, ConfirmedAt: "09"},
	})
	var previous []RecursiveCenter
	for end := 3; end <= len(segments); end++ {
		current := buildRecursiveCenters(segments[:end])
		if end > 3 {
			asOf := segments[end-2].ConfirmedAt
			if !reflect.DeepEqual(confirmedRecursiveCenters(previous, asOf), confirmedRecursiveCenters(current, asOf)) {
				t.Fatalf("confirmed centers changed after segment append end=%d asof=%s old=%+v new=%+v", end, asOf, confirmedRecursiveCenters(previous, asOf), confirmedRecursiveCenters(current, asOf))
			}
		}
		previous = current
	}
	if len(confirmedRecursiveCenters(previous, segments[len(segments)-1].ConfirmedAt)) == 0 {
		t.Fatalf("fixture did not produce a closed confirmed center: %+v", previous)
	}
}

func confirmedFractals(input []Fractal, asOf string) []Fractal {
	result := []Fractal{}
	for _, item := range input {
		if item.ConfirmedAt <= asOf {
			result = append(result, item)
		}
	}
	return result
}

func confirmedStrokes(input []Stroke, asOf string) []Stroke {
	result := []Stroke{}
	for _, item := range input {
		if item.Confirmed && item.ConfirmedAt <= asOf {
			result = append(result, item)
		}
	}
	return result
}

func confirmedSegments(input []Segment, asOf string) []Segment {
	result := []Segment{}
	for _, item := range input {
		if item.Confirmed && item.ConfirmedAt <= asOf {
			result = append(result, item)
		}
	}
	return result
}

func confirmedRecursiveCenters(input []RecursiveCenter, asOf string) []RecursiveCenter {
	result := []RecursiveCenter{}
	for _, item := range input {
		if !item.Forming && item.ConfirmedAt != "" && item.ConfirmedAt <= asOf {
			result = append(result, item)
		}
	}
	return result
}
