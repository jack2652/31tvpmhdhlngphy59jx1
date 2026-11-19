package chananalysis

import (
	"fmt"
	"math"
	"testing"
)

func TestFeatureSequenceBuildsFormingAndConfirmedSegments(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	segments := buildSegments(strokes)
	if len(segments) != 2 {
		t.Fatalf("segments = %#v, want first confirmed segment and second forming segment", segments)
	}
	if !segments[0].Confirmed || segments[0].Forming || segments[0].ConfirmedAt != "10" || segments[0].BreakStroke != 7 || segments[0].ConfirmStroke != 8 || segments[0].StartStroke != 0 || segments[0].EndStroke != 6 || segments[0].EndPointStroke != 6 || segments[0].End != "08" || segments[0].EndPrice != 15 {
		t.Fatalf("first segment confirmation/break indices incorrect: %+v", segments[0])
	}
	if segments[0].Direction != "up" || segments[0].Level != 0 || segments[0].Start != "01" || segments[0].StartPrice != 5 || segments[0].Low != 5 || segments[0].High != 15 || segments[0].StartRawBar != 0 || segments[0].EndRawBar != 70 || segments[0].StartInputBar != 0 || segments[0].EndInputBar != 14 {
		t.Fatalf("segment structure/provenance incorrect: %+v", segments[0])
	}
	if segments[1].Confirmed || !segments[1].Forming || segments[1].StartStroke != 7 || segments[1].EndStroke != 8 || segments[1].Direction != "down" {
		t.Fatalf("last segment should remain forming: %+v", segments[1])
	}
}

func TestFirstKindFeatureSequenceConfirmsThroughOnePlusOne(t *testing.T) {
	prices := [][2]float64{
		{90.6201, 81.9843}, {81.9843, 97.1998}, {97.1998, 80.1524}, {80.1524, 83.9774},
		{83.9774, 73.7053}, {73.7053, 78.5928}, {78.5928, 65.8817}, {65.8817, 79.2870},
		{79.2870, 62.8339}, {62.8339, 70.4809}, {70.4809, 60.0463}, {60.0463, 68.5033},
		{68.5033, 64.5433}, {64.5433, 73.9368},
	}
	strokes := make([]Stroke, len(prices))
	for index, price := range prices {
		direction, startKind, endKind := "down", "top", "bottom"
		if index%2 == 1 {
			direction, startKind, endKind = "up", "bottom", "top"
		}
		strokes[index] = Stroke{
			Chain: 0, Direction: direction,
			Start: Fractal{Kind: startKind, Date: fmt.Sprintf("%02d", index+1), Price: price[0]},
			End:   Fractal{Kind: endKind, Date: fmt.Sprintf("%02d", index+2), Price: price[1]},
			Low:   math.Min(price[0], price[1]), High: math.Max(price[0], price[1]),
			Confirmed: true, ConfirmedAt: fmt.Sprintf("%02d", index+2),
		}
	}
	for _, mirrored := range []bool{false, true} {
		input := append([]Stroke(nil), strokes...)
		if mirrored {
			for index := range input {
				stroke := &input[index]
				stroke.Start.Price, stroke.End.Price = 200-stroke.Start.Price, 200-stroke.End.Price
				stroke.Low, stroke.High = 200-stroke.High, 200-stroke.Low
				if stroke.Direction == "up" {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "down", "top", "bottom"
				} else {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "up", "bottom", "top"
				}
			}
		}
		candidate, evidence, mode, ok := segmentEnd(input, 0)
		if !ok || candidate != 11 || evidence != 13 || mode != 1 {
			t.Fatalf("mirrored=%v one-plus-one termination=(%d,%d,%d,%v), want break at stroke 11 confirmed at stroke 13", mirrored, candidate, evidence, mode, ok)
		}
		segments := buildSegments(input)
		if len(segments) < 2 || !segments[0].Confirmed || segments[0].BreakStroke != 11 || segments[0].ConfirmStroke != 13 || segments[0].EndPointStroke != 10 {
			t.Fatalf("mirrored=%v segments=%+v, want the first segment to close at its stroke-10 endpoint", mirrored, segments)
		}
	}
}

func TestRecursiveCenterExtensionKeepsInitialBoundaries(t *testing.T) {
	segments := withAlternatingEndpointKinds([]Segment{
		{Chain: 0, Level: 1, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 1, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
		{Chain: 0, Level: 1, Direction: "down", Start: "04", End: "05", Low: 5, High: 9, Confirmed: true, ConfirmedAt: "05"},
	})
	centers := buildRecursiveCenters(segments)
	if len(centers) != 1 || centers[0].Level != 1 || centers[0].Kind != "initial" || centers[0].InputKind != "recursive_trend" || centers[0].Chain != 0 || centers[0].Lower != 4 || centers[0].Upper != 10 || centers[0].ExtensionCount != 1 || centers[0].StartSegment != 0 || centers[0].EndSegment != 3 || centers[0].Start != "01" || centers[0].End != "05" || centers[0].FormedAt != "04" || centers[0].ConfirmedAt != "" || !centers[0].Forming || centers[0].ClosedAt != "" {
		t.Fatalf("centers = %#v, want initial [4,10] bounds and one extension", centers)
	}
}

func TestRecursiveCenterOnlyBecomesConfirmedAfterDeparture(t *testing.T) {
	segments := withAlternatingEndpointKinds([]Segment{
		{Chain: 0, Level: 1, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 1, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
		{Chain: 0, Level: 1, Direction: "down", Start: "04", End: "05", Low: 0, High: 3, Confirmed: true, ConfirmedAt: "05"},
	})
	centers := buildRecursiveCenters(segments)
	if len(centers) != 1 || centers[0].Forming || centers[0].FormedAt != "04" || centers[0].ClosedAt != "05" || centers[0].ConfirmedAt != "05" || centers[0].End != "04" || centers[0].EndSegment != 2 {
		t.Fatalf("centers=%+v, want center closed at departure segment confirmation without moving its end", centers)
	}
}

func TestPenAndRecursiveCentersExposeDistinctTypesAndLevels(t *testing.T) {
	strokes := []Stroke{
		{Chain: 0, Direction: "up", Level: 0, Start: Fractal{Kind: "bottom", Date: "01"}, End: Fractal{Kind: "top", Date: "02"}, Low: 1, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Direction: "down", Level: 0, Start: Fractal{Kind: "top", Date: "02"}, End: Fractal{Kind: "bottom", Date: "03"}, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Direction: "up", Level: 0, Start: Fractal{Kind: "bottom", Date: "03"}, End: Fractal{Kind: "top", Date: "04"}, Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
	}
	penCenters := buildCenters(strokes)
	if len(penCenters) != 1 || penCenters[0].Level != 0 || penCenters[0].Kind != "pen" || penCenters[0].InputKind != "stroke" {
		t.Fatalf("pen centers=%+v, want explicitly level-0 pen center from strokes", penCenters)
	}
	trendCenters := buildRecursiveCenters(withAlternatingEndpointKinds([]Segment{
		{Chain: 0, Level: 2, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 2, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 2, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
	}))
	if len(trendCenters) != 1 || trendCenters[0].Level != 2 || trendCenters[0].InputKind != "recursive_trend" {
		t.Fatalf("recursive centers=%+v, want level-2 recursive trend center", trendCenters)
	}
}

func TestRecursiveCenterRequiresStrictOverlapAndAlternatingConfirmedInputs(t *testing.T) {
	cases := []struct {
		name     string
		segments []Segment
	}{
		{name: "touching bounds", segments: []Segment{
			{Chain: 0, Direction: "up", Low: 1, High: 10, Confirmed: true},
			{Chain: 0, Direction: "down", Low: 10, High: 12, Confirmed: true},
			{Chain: 0, Direction: "up", Low: 8, High: 14, Confirmed: true},
		}},
		{name: "nonalternating directions", segments: []Segment{
			{Chain: 0, Direction: "up", Low: 1, High: 10, Confirmed: true},
			{Chain: 0, Direction: "up", Low: 4, High: 12, Confirmed: true},
			{Chain: 0, Direction: "down", Low: 3, High: 11, Confirmed: true},
		}},
		{name: "unconfirmed input", segments: []Segment{
			{Chain: 0, Direction: "up", Low: 1, High: 10, Confirmed: true},
			{Chain: 0, Direction: "down", Low: 4, High: 12, Confirmed: false},
			{Chain: 0, Direction: "up", Low: 3, High: 11, Confirmed: true},
		}},
		{name: "mixed levels", segments: []Segment{
			{Chain: 0, Level: 0, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true},
			{Chain: 0, Level: 0, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true},
			{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true},
		}},
		{name: "disconnected segment endpoints", segments: []Segment{
			{Chain: 0, Direction: "up", Start: "01", End: "02", Low: 1, High: 10, Confirmed: true},
			{Chain: 0, Direction: "down", Start: "03", End: "04", Low: 4, High: 12, Confirmed: true},
			{Chain: 0, Direction: "up", Start: "04", End: "05", Low: 3, High: 11, Confirmed: true},
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if centers := buildRecursiveCenters(testCase.segments); len(centers) != 0 {
				t.Fatalf("centers=%+v, want no center", centers)
			}
		})
	}
}

func TestRecursiveCenterRejectsOverlapNarrowerThanPriceTolerance(t *testing.T) {
	segments := withAlternatingEndpointKinds([]Segment{
		{Chain: 0, Level: 1, Direction: "up", Start: "01", End: "02", Low: 1, High: 10 + 1e-9, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 1, Direction: "down", Start: "02", End: "03", Low: 10, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 8, High: 14, Confirmed: true, ConfirmedAt: "04"},
	})
	if centers := buildRecursiveCenters(segments); len(centers) != 0 {
		t.Fatalf("centers=%+v, an overlap narrower than source-price tolerance must normalize to a touch", centers)
	}
}

func TestRecursiveCentersClassifyOverlapTouchAndSeparation(t *testing.T) {
	targets := []struct {
		name     string
		lower    float64
		upper    float64
		relation string
		kind     string
	}{
		{name: "overlap expands previous center", lower: 9, upper: 12, relation: "overlap", kind: "expansion"},
		{name: "touching boundary creates adjacent center", lower: 10, upper: 12, relation: "touch", kind: "newborn"},
		{name: "separate above creates new center", lower: 11, upper: 14, relation: "separate_up", kind: "newborn"},
		{name: "separate below creates new center", lower: 1, upper: 3, relation: "separate_down", kind: "newborn"},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			segments := centerRelationFixture(target.lower, target.upper)
			centers := buildRecursiveCenters(segments)
			if len(centers) != 2 {
				t.Fatalf("centers=%+v, want previous and adjacent center", centers)
			}
			current := centers[1]
			wantRelation := "new_after"
			if target.kind == "expansion" {
				wantRelation = "expands"
			}
			if current.Lower != target.lower || current.Upper != target.upper || current.Kind != target.kind || current.RelationToPrevious != target.relation || current.Relation != wantRelation || current.RelatedCenter == nil || *current.RelatedCenter != 0 || current.CenterID == "" || current.PreviousCenterID != centers[0].CenterID || current.StartSegment != 5 || current.EndSegment != 7 {
				t.Fatalf("current center=%+v, want bounds [%v,%v], kind=%s relation=%s and range 5..7", current, target.lower, target.upper, target.kind, target.relation)
			}
		})
	}
}

func centerRelationFixture(lower, upper float64) []Segment {
	segments := []Segment{
		{Chain: 0, Level: 1, Direction: "up", Start: "01", End: "02", Low: 2, High: 10, Confirmed: true, ConfirmedAt: "02"},
		{Chain: 0, Level: 1, Direction: "down", Start: "02", End: "03", Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Chain: 0, Level: 1, Direction: "up", Start: "03", End: "04", Low: 3, High: 11, Confirmed: true, ConfirmedAt: "04"},
		{Chain: 0, Level: 1, Direction: "down", Start: "04", End: "05", Low: 5, High: 9, Confirmed: true, ConfirmedAt: "05"},
	}
	separator := [2]float64{100, 101}
	if lower < 5 {
		separator = [2]float64{0, 1}
	}
	segments = append(segments, Segment{Chain: 0, Level: 1, Direction: "up", Start: "05", End: "06", Low: separator[0], High: separator[1], Confirmed: true, ConfirmedAt: "06"})
	intervals := [][2]float64{{lower, upper + 2}, {lower - 1, upper}, {lower - 2, upper + 1}}
	for index, interval := range intervals {
		start := fmt.Sprintf("%02d", 6+index)
		end := fmt.Sprintf("%02d", 7+index)
		direction := "down"
		switch index % 2 {
		case 1:
			direction = "up"
		}
		segments = append(segments, Segment{Chain: 0, Level: 1, Direction: direction, Start: start, End: end, StartPrice: interval[1], EndPrice: interval[0], Low: interval[0], High: interval[1], Confirmed: true, ConfirmedAt: end})
	}
	segments = withAlternatingEndpointKinds(segments)
	for index := 1; index < len(segments); index++ {
		segments[index].StartPrice = segments[index-1].EndPrice
	}
	return segments
}

func withAlternatingEndpointKinds(segments []Segment) []Segment {
	for index := range segments {
		switch segments[index].Direction {
		case "up":
			segments[index].StartKind = "bottom"
			segments[index].EndKind = "top"
		case "down":
			segments[index].StartKind = "top"
			segments[index].EndKind = "bottom"
		}
	}
	return segments
}

func TestBrokenFeatureExtremeUsesDirectionalExtreme(t *testing.T) {
	features := []featureRange{
		{Low: 10, High: 14},
		{Low: 7, High: 11},
		{Low: 9, High: 13},
	}
	if extreme, reclaim, ok := brokenFeatureExtreme(features, Stroke{Low: 6, High: 10}, "up"); !ok || extreme != 7 || reclaim != 11 {
		t.Fatalf("up break extreme=%v reclaim=%v ok=%v, want broken low 7 and reclaim high 11", extreme, reclaim, ok)
	}
	features = []featureRange{
		{Low: 10, High: 14},
		{Low: 14, High: 18},
		{Low: 11, High: 16},
	}
	if extreme, reclaim, ok := brokenFeatureExtreme(features, Stroke{Low: 15, High: 19}, "down"); !ok || extreme != 18 || reclaim != 14 {
		t.Fatalf("down break extreme=%v reclaim=%v ok=%v, want broken high 18 and reclaim low 14", extreme, reclaim, ok)
	}
}

func TestFeatureSequenceMergesIncludedElementsByTrendDirection(t *testing.T) {
	var upwardFeatures, downwardFeatures []featureRange
	upwardDirection := 0
	upwardFeatures = appendFeature(upwardFeatures, &upwardDirection, Stroke{Direction: "down", Chain: 2, Low: 20, High: 30, Start: Fractal{Date: "01"}, End: Fractal{Date: "02"}}, 1)
	upwardFeatures = appendFeature(upwardFeatures, &upwardDirection, Stroke{Direction: "down", Low: 15, High: 25, Start: Fractal{Date: "02"}, End: Fractal{Date: "03"}}, 3)
	upwardFeatures = appendFeature(upwardFeatures, &upwardDirection, Stroke{Direction: "down", Low: 17, High: 22, Start: Fractal{Date: "03"}, End: Fractal{Date: "04"}}, 5)
	if upwardDirection != -1 || len(upwardFeatures) != 2 || upwardFeatures[1].Low != 15 || upwardFeatures[1].High != 22 || upwardFeatures[1].StartStroke != 3 || upwardFeatures[1].EndStroke != 5 || upwardFeatures[1].Start != "02" || upwardFeatures[1].End != "04" {
		t.Fatalf("upward segment feature inclusion direction=%d ranges=%+v, want downward sequence inclusion [15,22] with provenance", upwardDirection, upwardFeatures)
	}
	downwardDirection := 0
	downwardFeatures = appendFeature(downwardFeatures, &downwardDirection, Stroke{Direction: "up", Chain: 4, Low: 10, High: 20, Start: Fractal{Date: "04"}, End: Fractal{Date: "05"}}, 2)
	downwardFeatures = appendFeature(downwardFeatures, &downwardDirection, Stroke{Direction: "up", Low: 15, High: 25, Start: Fractal{Date: "05"}, End: Fractal{Date: "06"}}, 4)
	downwardFeatures = appendFeature(downwardFeatures, &downwardDirection, Stroke{Direction: "up", Low: 17, High: 22, Start: Fractal{Date: "06"}, End: Fractal{Date: "07"}}, 6)
	if downwardDirection != 1 || len(downwardFeatures) != 2 || downwardFeatures[1].Low != 17 || downwardFeatures[1].High != 25 || downwardFeatures[1].StartStroke != 4 || downwardFeatures[1].EndStroke != 6 || downwardFeatures[1].Start != "05" || downwardFeatures[1].End != "07" {
		t.Fatalf("downward segment feature inclusion direction=%d ranges=%+v, want upward sequence inclusion [17,25] with provenance", downwardDirection, downwardFeatures)
	}
}

func TestFeatureBreakRequiresStrictPenetration(t *testing.T) {
	upFeatures := []featureRange{
		{Low: 10, High: 14},
		{Low: 7, High: 11},
		{Low: 9, High: 13},
	}
	if _, _, broken := brokenFeatureExtreme(upFeatures, Stroke{Low: 7}, "up"); broken {
		t.Fatal("touching the feature low must not count as a strict break")
	}
	downFeatures := []featureRange{
		{Low: 10, High: 14},
		{Low: 14, High: 18},
		{Low: 11, High: 16},
	}
	if _, _, broken := brokenFeatureExtreme(downFeatures, Stroke{High: 18}, "down"); broken {
		t.Fatal("touching the feature high must not count as a strict break")
	}
}

func TestFeatureBreakReclaimedByOriginalDirectionRemainsForming(t *testing.T) {
	strokes := featureBreakFixture(12)
	segments := buildSegments(strokes)
	if len(segments) != 1 || segments[0].Confirmed || !segments[0].Forming || segments[0].BreakStroke != -1 || segments[0].ConfirmStroke != -1 || segments[0].EndPointStroke != 6 || segments[0].End != "08" || segments[0].EndPrice != 15 {
		t.Fatalf("reclaimed feature break must remain a forming segment at its highest endpoint: %+v", segments)
	}
}

func TestReclaimedFeatureBreakDoesNotHideLaterConfirmedBreak(t *testing.T) {
	// 第一次破坏被反弹收回后，必须继续检查后续破坏，不能提前结束整条线段扫描。
	strokes := featureBreakFixture(12)
	for _, price := range [][2]float64{{12, 4}, {4, 8}} {
		index := len(strokes)
		direction, startKind, endKind := "down", "top", "bottom"
		if price[1] > price[0] {
			direction, startKind, endKind = "up", "bottom", "top"
		}
		strokes = append(strokes, Stroke{Chain: 0, Direction: direction,
			Start: Fractal{Kind: startKind, Date: fmt.Sprintf("%02d", index+1), Price: price[0]},
			End:   Fractal{Kind: endKind, Date: fmt.Sprintf("%02d", index+2), Price: price[1]},
			Low:   math.Min(price[0], price[1]), High: math.Max(price[0], price[1]),
			Confirmed: true, ConfirmedAt: fmt.Sprintf("%02d", index+2)})
	}
	for _, mirrored := range []bool{false, true} {
		input := append([]Stroke(nil), strokes...)
		if mirrored {
			for index := range input {
				stroke := &input[index]
				stroke.Start.Price, stroke.End.Price = 30-stroke.Start.Price, 30-stroke.End.Price
				stroke.Low, stroke.High = 30-stroke.High, 30-stroke.Low
				if stroke.Direction == "up" {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "down", "top", "bottom"
				} else {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "up", "bottom", "top"
				}
			}
		}
		segments := buildSegments(input)
		if len(segments) == 0 || !segments[0].Confirmed || segments[0].BreakStroke != 9 || segments[0].ConfirmStroke != 10 || segments[0].EndPointStroke != 6 {
			t.Fatalf("mirrored=%v segments=%+v, want the later break confirmed after the earlier candidate was reclaimed", mirrored, segments)
		}
	}
}

func TestFeatureBreakRequiresFollowingStrokeToConfirmAndBacktracksEndpoint(t *testing.T) {
	// The rebound ends above the broken feature low (7) but below the
	// feature element high (11), so it must not erase the break.
	segments := buildSegments(featureBreakFixture(8))
	if len(segments) != 2 {
		t.Fatalf("segments=%+v, want confirmed segment followed by forming segment", segments)
	}
	segment := segments[0]
	if !segment.Confirmed || segment.Forming || segment.BreakStroke != 7 || segment.ConfirmStroke != 8 || segment.ConfirmedAt != "10" {
		t.Fatalf("confirmation metadata=%+v, want break at 7 and confirmation at stroke 8", segment)
	}
	if segment.StartStroke != 0 || segment.EndStroke != 6 || segment.EndPointStroke != 6 || segment.Start != "01" || segment.End != "08" || segment.EndPrice != 15 || segment.StartPrice != 5 {
		t.Fatalf("segment endpoint/range=%+v, want endpoint backtracked to stroke 6 while ranges end before break", segment)
	}
	if segment.StartRawBar != 0 || segment.EndRawBar != 70 || segment.StartInputBar != 0 || segment.EndInputBar != 14 || len(segment.Feature) != 3 || segment.Feature[1].StartStroke != 3 || segment.Feature[1].EndStroke != 3 {
		t.Fatalf("segment source/input provenance=%+v, want endpoint/raw ranges and source feature stroke", segment)
	}
}

func TestNextSegmentStartsAfterBacktrackedEndpointNotLateFeatureBreak(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	// 最强端点位于第 4 笔，第 5 笔候选由第 7 笔确认；后续线段应从第 5 笔开始。
	strokes[4].End.Price, strokes[4].High = 17, 17
	strokes[5].Start.Price, strokes[5].High = 17, 17
	strokes[5].End.Price = 9
	strokes[5].Low = 9
	strokes[6].Start.Price, strokes[6].Low = 9, 9
	strokes[6].End.Price, strokes[6].High = 12, 12
	strokes[7].Start.Price, strokes[7].High = 12, 12
	strokes[7].End.Price, strokes[7].Low = 6, 6
	strokes[8].Start.Price, strokes[8].Low = 6, 6
	segments := buildSegments(strokes)
	if len(segments) < 2 {
		t.Fatalf("segments=%+v, want a following counter-direction segment", segments)
	}
	first, second := segments[0], segments[1]
	if !first.Confirmed || first.BreakStroke != 5 || first.ConfirmStroke != 7 || first.EndPointStroke != 4 || first.End != strokes[4].End.Date {
		t.Fatalf("first segment=%+v, want the 1+1 candidate confirmed at stroke 7 with endpoint backtracked to stroke 4", first)
	}
	if second.StartStroke != first.EndPointStroke+1 || second.Direction != "down" || second.Start != first.End || strokes[second.StartStroke].Start.Date != first.End {
		t.Fatalf("second segment=%+v first=%+v, want continuous counter move from backtracked endpoint", second, first)
	}
}

func TestFeatureSequenceBreakAndConfirmationAreSymmetricForDownSegments(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	for index := range strokes {
		strokes[index].Low, strokes[index].High = -strokes[index].High, -strokes[index].Low
		strokes[index].Start.Price = -strokes[index].Start.Price
		strokes[index].End.Price = -strokes[index].End.Price
		if strokes[index].Direction == "up" {
			strokes[index].Direction = "down"
		} else {
			strokes[index].Direction = "up"
		}
		if strokes[index].Direction == "up" {
			strokes[index].Start.Kind = "bottom"
			strokes[index].End.Kind = "top"
		} else {
			strokes[index].Start.Kind = "top"
			strokes[index].End.Kind = "bottom"
		}
	}
	segments := buildSegments(strokes)
	if len(segments) != 2 {
		t.Fatalf("segments=%+v, want confirmed downward segment and following forming segment", segments)
	}
	segment := segments[0]
	if segment.Direction != "down" || !segment.Confirmed || segment.Forming || segment.BreakStroke != 7 || segment.ConfirmStroke != 8 || segment.ConfirmedAt != "10" || segment.StartStroke != 0 || segment.EndStroke != 6 || segment.EndPointStroke != 6 || segment.End != "08" || segment.EndPrice != -15 {
		t.Fatalf("downward segment=%+v, want symmetric break confirmation and endpoint backtracking", segment)
	}
}

func TestFeatureBreakWithoutConfirmedReversalRemainsForming(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	strokes[8].Confirmed = false
	segments := buildSegments(strokes)
	if len(segments) != 1 {
		t.Fatalf("segments=%+v, want one forming segment until reversal stroke confirms", segments)
	}
	segment := segments[0]
	if segment.Confirmed || !segment.Forming || segment.BreakStroke != 7 || segment.ConfirmStroke != -1 || segment.EndStroke != 6 || segment.EndPointStroke != 6 || segment.End != "08" || segment.EndPrice != 15 || segment.ConfirmedAt != "" {
		t.Fatalf("unconfirmed reversal state=%+v, want forming segment with backtracked endpoint and no confirm time", segment)
	}
}

func TestGappedFeatureSequenceTerminatesThroughSecondFeatureFractal(t *testing.T) {
	for _, mirrored := range []bool{false, true} {
		strokes := gappedFeatureBreakFixture()
		if mirrored {
			for index := range strokes {
				stroke := &strokes[index]
				stroke.Start.Price, stroke.End.Price = 30-stroke.Start.Price, 30-stroke.End.Price
				stroke.Low, stroke.High = 30-stroke.High, 30-stroke.Low
				if stroke.Direction == "up" {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "down", "top", "bottom"
				} else {
					stroke.Direction, stroke.Start.Kind, stroke.End.Kind = "up", "bottom", "top"
				}
			}
		}
		candidate, evidence, mode, ok := segmentEnd(strokes, 0)
		if !ok || candidate != 3 || evidence != 8 || mode != 2 {
			t.Fatalf("mirrored=%v gapped feature termination=(%d,%d,%d,%v), want break at stroke 3 confirmed by second-sequence fractal at stroke 8", mirrored, candidate, evidence, mode, ok)
		}
		segments := buildSegments(strokes)
		if len(segments) < 2 || !segments[0].Confirmed || segments[0].BreakStroke != 3 || segments[0].ConfirmStroke != 8 || segments[0].ConfirmedAt != "10" || segments[0].EndPointStroke != 2 {
			t.Fatalf("mirrored=%v segments=%+v, want confirmed first segment at the gap break", mirrored, segments)
		}
	}
}

func gappedFeatureBreakFixture() []Stroke {
	prices := [][2]float64{{10, 20}, {20, 15}, {15, 25}, {25, 21}, {21, 24}, {24, 18}, {18, 20}, {20, 19}, {19, 23}}
	strokes := make([]Stroke, len(prices))
	for index, price := range prices {
		direction, startKind, endKind := "up", "bottom", "top"
		if index%2 == 1 {
			direction, startKind, endKind = "down", "top", "bottom"
		}
		strokes[index] = Stroke{
			Chain: 0, Direction: direction,
			Start: Fractal{Kind: startKind, Date: fmt.Sprintf("%02d", index+1), Price: price[0]},
			End:   Fractal{Kind: endKind, Date: fmt.Sprintf("%02d", index+2), Price: price[1]},
			Low:   math.Min(price[0], price[1]), High: math.Max(price[0], price[1]),
			Confirmed: true, ConfirmedAt: fmt.Sprintf("%02d", index+2),
		}
	}
	return strokes
}

func TestSegmentsDoNotCrossChainBoundaries(t *testing.T) {
	strokes := featureBreakFixture(6.5)
	for index := range strokes {
		strokes[index].Chain = 3
	}
	strokes = append(strokes, Stroke{Chain: 4, Direction: "down", Start: Fractal{Date: "20", Price: 10}, End: Fractal{Date: "21", Price: 5}, Low: 5, High: 10, Confirmed: true, ConfirmedAt: "21"})
	segments := buildSegments(strokes)
	for _, segment := range segments {
		for index := segment.StartStroke; index <= segment.EndStroke; index++ {
			if strokes[index].Chain != segment.Chain {
				t.Fatalf("segment crossed chain boundary: %+v input=%+v", segment, strokes[segment.StartStroke:segment.EndStroke+1])
			}
		}
	}
}

func featureBreakFixture(finalUpEnd float64) []Stroke {
	prices := []struct {
		start, end float64
	}{
		{5, 14}, {14, 10}, {10, 11}, {11, 7}, {7, 13}, {13, 9}, {9, 15}, {15, 6}, {6, finalUpEnd},
	}
	directions := []string{"up", "down", "up", "down", "up", "down", "up", "down", "up"}
	strokes := make([]Stroke, len(prices))
	for index, price := range prices {
		low, high := math.Min(price.start, price.end), math.Max(price.start, price.end)
		startKind, endKind := "bottom", "top"
		if directions[index] == "down" {
			startKind, endKind = "top", "bottom"
		}
		strokes[index] = Stroke{Chain: 0, Direction: directions[index], Start: Fractal{Kind: startKind, Date: fmt.Sprintf("%02d", index+1), Price: price.start}, End: Fractal{Kind: endKind, Date: fmt.Sprintf("%02d", index+2), Price: price.end}, Low: low, High: high, Confirmed: true, ConfirmedAt: fmt.Sprintf("%02d", index+2), Level: 0, StartBar: index, EndBar: index + 1, StartRawBar: index * 10, EndRawBar: (index + 1) * 10, InputStartBar: index * 2, InputEndBar: (index + 1) * 2}
	}
	return strokes
}

func TestDailyChainSplitPreservesEarlierBars(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-06", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-08", Open: 10, High: 11, Low: 9, Close: 10},
	}
	result, warnings := chainDailyBars(bars)
	if len(result) != 3 || result[0].Chain != 0 || result[1].Chain != 0 || result[2].Chain != 1 || len(warnings) != 1 {
		t.Fatalf("chain result=%+v warnings=%v", result, warnings)
	}
}

func TestSegmentsAndCentersDoNotCrossDataChains(t *testing.T) {
	strokes := []Stroke{
		{Chain: 0, Direction: "up", Start: Fractal{Date: "01"}, End: Fractal{Date: "02"}, Low: 1, High: 10, Confirmed: true},
		{Chain: 0, Direction: "down", Start: Fractal{Date: "02"}, End: Fractal{Date: "03"}, Low: 4, High: 10, Confirmed: true},
		{Chain: 1, Direction: "up", Start: Fractal{Date: "03"}, End: Fractal{Date: "04"}, Low: 3, High: 11, Confirmed: true},
		{Chain: 1, Direction: "down", Start: Fractal{Date: "04"}, End: Fractal{Date: "05"}, Low: 5, High: 9, Confirmed: true},
	}
	segments := buildSegments(strokes)
	for _, segment := range segments {
		for index := segment.StartStroke; index <= segment.EndStroke; index++ {
			if strokes[index].Chain != segment.Chain {
				t.Fatalf("segment crossed chain boundary: %+v strokes=%+v", segment, strokes)
			}
		}
	}
	if centers := buildRecursiveCenters([]Segment{
		{Chain: 0, Direction: "up", Low: 1, High: 10, Confirmed: true},
		{Chain: 0, Direction: "down", Low: 4, High: 12, Confirmed: true},
		{Chain: 1, Direction: "up", Low: 3, High: 11, Confirmed: true},
	}); len(centers) != 0 {
		t.Fatalf("center crossed chain boundary: %+v", centers)
	}
}
