package chananalysis

import (
	"fmt"
	"math"
	"sort"
)

type featureRange struct {
	FeatureElement
	chain int
}

// buildSegments implements the confirmed feature-sequence cases from the
// reference chan-lun implementation. Segment starts require three overlapping
// strokes; termination checks the un-gapped and gapped feature-sequence cases,
// and completed ranges are split at the first counter-direction stroke.
func buildSegments(strokes []Stroke) []Segment {
	segments := []Segment{}
	for index := 0; index < len(strokes); {
		chain, direction := strokes[index].Chain, strokes[index].Direction
		chainEnd := index
		for chainEnd+1 < len(strokes) && strokes[chainEnd+1].Chain == chain {
			chainEnd++
		}
		if index+2 > chainEnd || !validSegmentStart(strokes[index]) || !overlappingStrokeTriple(strokes, index) {
			index++
			continue
		}
		confirmedEnd := chainEnd
		for confirmedEnd >= index && !strokes[confirmedEnd].Confirmed {
			confirmedEnd--
		}
		candidate, evidence, _, complete := segmentEnd(strokes[:confirmedEnd+1], index)
		end, next := -1, chainEnd+1
		if complete {
			end, next = candidate-1, candidate
		} else {
			end = formingSegmentEnd(strokes, index, chainEnd, direction)
			if end < index+2 {
				index++
				continue
			}
			next = end + 1
		}
		segment := makeSegment(strokes, index, end, direction, chain, complete)
		segment.Feature = featureElements(segmentFeatures(strokes, index, end, direction, chain))
		if complete {
			segment.BreakStroke, segment.ConfirmStroke = candidate, evidence
			segment.ConfirmedAt = strokes[evidence].ConfirmedAt
			segment.Forming = false
		}
		segments = append(segments, segment)
		if next <= index {
			next = index + 1
		}
		index = next
	}
	return segments
}

func makeSegment(strokes []Stroke, start, end int, direction string, chain int, confirmed bool) Segment {
	segment := Segment{Direction: direction, Chain: chain, Level: strokes[start].Level, StartStroke: start, EndStroke: end, EndPointStroke: end, BreakStroke: -1, ConfirmStroke: -1,
		Start: strokes[start].Start.Date, End: strokes[end].End.Date, StartKind: strokes[start].Start.Kind, EndKind: strokes[end].End.Kind, StartPrice: strokes[start].Start.Price, EndPrice: strokes[end].End.Price,
		StartRawBar: strokes[start].StartRawBar, EndRawBar: strokes[end].EndRawBar, StartInputBar: strokes[start].InputStartBar, EndInputBar: strokes[end].InputEndBar,
		Low: strokes[start].Low, High: strokes[start].High, Confirmed: confirmed, Forming: !confirmed, Feature: []FeatureElement{}}
	for index := start + 1; index <= end; index++ {
		segment.Low = math.Min(segment.Low, strokes[index].Low)
		segment.High = math.Max(segment.High, strokes[index].High)
	}
	return segment
}

func overlappingStrokeTriple(strokes []Stroke, start int) bool {
	if start+2 >= len(strokes) {
		return false
	}
	first, second, third := strokes[start], strokes[start+1], strokes[start+2]
	lastInChain := start+3 >= len(strokes) || strokes[start+3].Chain != third.Chain
	if !first.Confirmed || !second.Confirmed || (!third.Confirmed && !lastInChain) || first.Chain != second.Chain || second.Chain != third.Chain || first.Direction != third.Direction || first.Direction == second.Direction {
		return false
	}
	return math.Max(first.Low, math.Max(second.Low, third.Low)) <= math.Min(first.High, math.Min(second.High, third.High))
}

func formingSegmentEnd(strokes []Stroke, start, end int, direction string) int {
	best := -1
	for index := start + 2; index <= end; index += 2 {
		if strokes[index].Chain != strokes[start].Chain || strokes[index].Direction != direction {
			continue
		}
		if direction == "up" && strokes[index].End.Price > strokes[start].Start.Price {
			if best < 0 || strokes[index].End.Price > strokes[best].End.Price {
				best = index
			}
		} else if direction == "down" && strokes[index].End.Price < strokes[start].Start.Price {
			if best < 0 || strokes[index].End.Price < strokes[best].End.Price {
				best = index
			}
		}
	}
	return best
}

func segmentEnd(strokes []Stroke, start int) (candidate, evidence, mode int, ok bool) {
	if start >= len(strokes) {
		return -1, -1, 0, false
	}
	up := strokes[start].Direction == "up"
	counter := "down"
	if !up {
		counter = "up"
	}
	features := []featureRange{}
	featureDirection := segmentFeatureDirection(strokes[start].Direction)
	endings := [][3]int{}
	for index := start + 1; index < len(strokes); index++ {
		stroke := strokes[index]
		if stroke.Chain != strokes[start].Chain || !stroke.Confirmed || stroke.Direction != counter {
			continue
		}
		continuation := stroke.Start.Price > strokes[start].Start.Price
		if !up {
			continuation = stroke.Start.Price < strokes[start].Start.Price
		}
		if index >= start+3 && len(features) > 0 && continuation {
			if candidate, evidence, mode, found := candidateSegmentEnd(strokes, start, index, features[len(features)-1]); found {
				endings = append(endings, [3]int{candidate, evidence, mode})
			}
			rawIndex := index - 2
			if rawIndex > start {
				raw := featureRange{FeatureElement: FeatureElement{StartStroke: rawIndex, EndStroke: rawIndex, Low: strokes[rawIndex].Low, High: strokes[rawIndex].High, Start: strokes[rawIndex].Start.Date, End: strokes[rawIndex].End.Date}, chain: stroke.Chain}
				if candidate, evidence, mode, found := candidateSegmentEnd(strokes, start, index, raw); found {
					confirmation := strokes[evidence].End.Price < strokes[start].Start.Price
					if !up {
						confirmation = strokes[evidence].End.Price > strokes[start].Start.Price
					}
					low, high := math.MaxFloat64, -math.MaxFloat64
					for reverseIndex := index; reverseIndex <= evidence; reverseIndex++ {
						low = math.Min(low, strokes[reverseIndex].Low)
						high = math.Max(high, strokes[reverseIndex].High)
					}
					overlap := false
					if evidence-index+1 >= 3 {
						overlapLow := math.Max(strokes[index].Low, math.Max(strokes[index+1].Low, strokes[index+2].Low))
						overlapHigh := math.Min(strokes[index].High, math.Min(strokes[index+1].High, strokes[index+2].High))
						overlap = overlapLow <= overlapHigh
					}
					if confirmation && overlap && low <= high {
						endings = append(endings, [3]int{candidate, evidence, mode})
					}
				}
			}
		}
		appendFeature(&features, &featureDirection, stroke, index)
	}
	if len(endings) == 0 {
		return -1, -1, 0, false
	}
	sort.SliceStable(endings, func(i, j int) bool {
		if endings[i][1] == endings[j][1] {
			return endings[i][0] < endings[j][0]
		}
		return endings[i][1] < endings[j][1]
	})
	return endings[0][0], endings[0][1], endings[0][2], true
}

func candidateSegmentEnd(strokes []Stroke, start, candidate int, left featureRange) (int, int, int, bool) {
	up := strokes[start].Direction == "up"
	counter := strokes[candidate]
	if counter.Direction == strokes[start].Direction {
		return -1, -1, 0, false
	}
	if up && counter.High <= left.High || !up && counter.Low >= left.Low {
		return -1, -1, 0, false
	}
	right := []featureRange{{FeatureElement: FeatureElement{StartStroke: candidate, EndStroke: candidate, Low: counter.Low, High: counter.High, Start: counter.Start.Date, End: counter.End.Date}, chain: counter.Chain}}
	second := []featureRange{}
	rightDirection, secondDirection := segmentFeatureDirection(strokes[start].Direction), segmentFeatureDirection(counter.Direction)
	extreme := counter.Start.Price
	gap := left.High < counter.Low
	if !up {
		gap = left.Low > counter.High
	}
	pendingFirstKind := false
	for index := candidate + 1; index < len(strokes); index++ {
		stroke := strokes[index]
		if stroke.Chain != strokes[start].Chain || !stroke.Confirmed {
			break
		}
		if stroke.Direction == counter.Direction {
			appendFeature(&right, &rightDirection, stroke, index)
		} else {
			appendFeature(&second, &secondDirection, stroke, index)
			if gap && len(right) >= 2 && hasFeatureFractal(second, featureFractalKind(up)) {
				return candidate, index, 2, true
			}
			if pendingFirstKind {
				if _, _, _, following := segmentEnd(strokes[:index+1], candidate); following {
					return candidate, index, 1, true
				}
			}
			newExtreme := stroke.End.Price > extreme
			if !up {
				newExtreme = stroke.End.Price < extreme
			}
			if newExtreme {
				return -1, -1, 0, false
			}
		}
		if len(right) < 2 {
			continue
		}
		a, b := right[0], right[1]
		turned := b.High < a.High && b.Low < a.Low
		if !up {
			turned = b.High > a.High && b.Low > a.Low
		}
		if turned && !gap && stroke.Direction == counter.Direction {
			selected := stroke.End.Price < counter.End.Price
			crossedStart := stroke.End.Price < strokes[start].Start.Price
			if !up {
				selected = stroke.End.Price > counter.End.Price
				crossedStart = stroke.End.Price > strokes[start].Start.Price
			}
			if selected || crossedStart {
				return candidate, index, 1, true
			}
			pendingFirstKind = true
		}
		if turned && gap && hasFeatureFractal(second, featureFractalKind(up)) {
			return candidate, index, 2, true
		}
	}
	return -1, -1, 0, false
}

func featureFractalKind(up bool) string {
	if up {
		return "bottom"
	}
	return "top"
}

func hasFeatureFractal(features []featureRange, kind string) bool {
	if len(features) < 3 {
		return false
	}
	a, b, c := features[len(features)-3], features[len(features)-2], features[len(features)-1]
	if kind == "top" {
		return b.High > a.High && b.High > c.High && b.Low > a.Low && b.Low > c.Low
	}
	return b.Low < a.Low && b.Low < c.Low && b.High < a.High && b.High < c.High
}

func segmentEndpoint(strokes []Stroke, start, end int, direction string) int {
	endpoint := start
	for index := start + 1; index <= end && index < len(strokes); index++ {
		if direction == "up" && strokes[index].End.Price > strokes[endpoint].End.Price || direction == "down" && strokes[index].End.Price < strokes[endpoint].End.Price {
			endpoint = index
		}
	}
	return endpoint
}

func appendFeature(features *[]featureRange, sequenceDirection *int, stroke Stroke, index int) {
	current := featureRange{FeatureElement: FeatureElement{StartStroke: index, EndStroke: index, Low: stroke.Low, High: stroke.High, Start: stroke.Start.Date, End: stroke.End.Date}, chain: stroke.Chain}
	if len(*features) == 0 {
		*features = append(*features, current)
		return
	}
	last := &(*features)[len(*features)-1]
	contains := current.High >= last.High && current.Low <= last.Low || last.High >= current.High && last.Low <= current.Low
	if !contains {
		*features = append(*features, current)
		return
	}
	// The feature sequence's inclusion direction is recalculated from its
	// latest two standardized elements once that pair exists. A direction
	// inferred from the first pair becomes stale after a later reversal.
	if len(*features) > 1 {
		previous := (*features)[len(*features)-2]
		*sequenceDirection = -1
		if last.High > previous.High {
			*sequenceDirection = 1
		}
	}
	if *sequenceDirection < 0 {
		last.Low = math.Min(last.Low, current.Low)
		last.High = math.Min(last.High, current.High)
	} else if *sequenceDirection > 0 {
		last.Low = math.Max(last.Low, current.Low)
		last.High = math.Max(last.High, current.High)
	} else {
		last.Low = math.Min(last.Low, current.Low)
		last.High = math.Max(last.High, current.High)
	}
	last.EndStroke, last.End = index, current.End
}

func segmentFeatures(strokes []Stroke, start, end int, direction string, chain int) []featureRange {
	features := []featureRange{}
	featureDirection := segmentFeatureDirection(direction)
	for i := start + 1; i <= end; i++ {
		stroke := strokes[i]
		if stroke.Chain != chain || stroke.Direction == direction {
			continue
		}
		appendFeature(&features, &featureDirection, stroke, i)
	}
	return features
}

func featureElements(features []featureRange) []FeatureElement {
	result := make([]FeatureElement, 0, len(features))
	for _, feature := range features {
		result = append(result, feature.FeatureElement)
	}
	return result
}

// segmentFeatureDirection is fixed by the hypothesized segment direction.
// Feature-sequence inclusion happens before two non-contained elements may
// exist to infer a direction, so leaving this unknown and taking a union can
// erase or invent the feature pivots that terminate a segment.
func segmentFeatureDirection(direction string) int {
	if direction == "up" {
		return 1
	}
	return -1
}

func brokenFeatureExtreme(features []featureRange, stroke Stroke, direction string) (float64, float64, bool) {
	for index := 1; index+1 < len(features); index++ {
		left, middle, right := features[index-1], features[index], features[index+1]
		var pivot bool
		if direction == "up" {
			pivot = middle.Low < left.Low && middle.High < left.High && middle.Low < right.Low && middle.High < right.High
			if pivot && stroke.Low < middle.Low {
				// A rebound only cancels the break if it recovers through the
				// other edge of the broken feature element. Comparing against
				// middle.Low here makes every positive rebound look reclaimed.
				return middle.Low, middle.High, true
			}
		} else {
			pivot = middle.Low > left.Low && middle.High > left.High && middle.Low > right.Low && middle.High > right.High
			if pivot && stroke.High > middle.High {
				// Symmetric to an upward segment: after an upward break, a
				// pullback must cross the lower edge to cancel the candidate.
				return middle.High, middle.Low, true
			}
		}
	}
	return 0, 0, false
}

func buildRecursiveCenters(segments []Segment) []RecursiveCenter {
	centers := []RecursiveCenter{}
	for index := 0; index+2 < len(segments); {
		a, b, c := segments[index], segments[index+1], segments[index+2]
		if !eligibleCenterTriple(a, b, c) {
			index++
			continue
		}
		zd := math.Max(a.Low, math.Max(b.Low, c.Low))
		zg := math.Min(a.High, math.Min(b.High, c.High))
		if zd >= zg {
			index++
			continue
		}
		inputKind := "segment"
		if a.Level > 0 {
			inputKind = "recursive_trend"
		}
		center := RecursiveCenter{Level: a.Level, Kind: "initial", InputKind: inputKind, Chain: a.Chain, Start: a.Start, End: c.End, Lower: zd, Upper: zg,
			FormedAt: c.ConfirmedAt, ConfirmedAt: "", StartSegment: index, EndSegment: index + 2, ExtensionCount: 0, Forming: true}
		end := index + 3
		for end < len(segments) && eligibleCenterExtension(segments[end-1], segments[end], a.Level) && segments[end].Low < zg && segments[end].High > zd {
			center.End = segments[end].End
			center.EndSegment = end
			center.ExtensionCount++
			end++
		}
		if end < len(segments) && eligibleCenterExtension(segments[end-1], segments[end], a.Level) {
			center.Forming = false
			center.ClosedAt = segments[end].ConfirmedAt
			center.ConfirmedAt = center.ClosedAt
		}
		previousIndex := -1
		for previous := len(centers) - 1; previous >= 0; previous-- {
			if centers[previous].Chain == center.Chain && centers[previous].Level == center.Level {
				previousIndex = previous
				break
			}
		}
		center.CenterID = recursiveCenterID(center)
		if previousIndex < 0 {
			center.Kind = "initial"
		} else {
			previous := centers[previousIndex]
			center.RelatedCenter = &previousIndex
			center.PreviousCenterID = previous.CenterID
			switch {
			case center.Lower < previous.Upper && center.Upper > previous.Lower:
				center.RelationToPrevious, center.Relation, center.Kind = "overlap", "expands", "expansion"
			case center.Lower == previous.Upper || center.Upper == previous.Lower:
				center.RelationToPrevious, center.Relation, center.Kind = "touch", "new_after", "newborn"
			case center.Lower > previous.Upper:
				center.RelationToPrevious, center.Relation, center.Kind = "separate_up", "new_after", "newborn"
			case center.Upper < previous.Lower:
				center.RelationToPrevious, center.Relation, center.Kind = "separate_down", "new_after", "newborn"
			default:
				center.Relation, center.Kind = "new_after", "newborn"
			}
		}
		center.CenterID = recursiveCenterID(center)
		centers = append(centers, center)
		// The first non-overlapping segment ends the old center. The next
		// center's three-segment seed begins after that departure segment.
		index = end + 1
	}
	return centers
}

func recursiveCenterID(center RecursiveCenter) string {
	return fmt.Sprintf("L%d-C%d-S%d-E%d", center.Level, center.Chain, center.StartSegment, center.EndSegment)
}

func eligibleCenterTriple(a, b, c Segment) bool {
	return a.Chain == b.Chain && b.Chain == c.Chain &&
		a.Level == b.Level && b.Level == c.Level &&
		a.Confirmed && b.Confirmed && c.Confirmed &&
		a.Direction != b.Direction && b.Direction != c.Direction && a.Direction == c.Direction &&
		a.EndKind != "" && a.EndKind == b.StartKind && b.EndKind == c.StartKind &&
		a.End == b.Start && b.End == c.Start
}

func validSegmentStart(stroke Stroke) bool {
	if stroke.Direction == "up" {
		return stroke.Start.Kind == "bottom"
	}
	if stroke.Direction == "down" {
		return stroke.Start.Kind == "top"
	}
	return false
}

func eligibleCenterExtension(previous, next Segment, level int) bool {
	return previous.Chain == next.Chain && previous.Level == level && next.Level == level &&
		next.Confirmed && previous.Direction != next.Direction && previous.End == next.Start
}
