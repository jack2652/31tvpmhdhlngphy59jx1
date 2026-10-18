package chananalysis

import (
	"fmt"
	"math"
)

type featureRange struct {
	FeatureElement
	chain int
}

// buildSegments implements the v4 standard feature-sequence termination
// policy. A segment ends only after a confirmed counter-direction stroke
// breaks its feature sequence and a following confirmed original-direction
// stroke fails to reclaim that broken extreme.
func buildSegments(strokes []Stroke) []Segment {
	segments := []Segment{}
	for index := 0; index < len(strokes); {
		if !strokes[index].Confirmed || !validSegmentStart(strokes[index]) {
			if index+1 == len(strokes) {
				break
			}
			index++
			continue
		}
		chain, direction := strokes[index].Chain, strokes[index].Direction
		chainEnd := index
		for chainEnd+1 < len(strokes) && strokes[chainEnd+1].Chain == chain {
			chainEnd++
		}
		features := []featureRange{}
		featureDirection := 0
		breakAt, confirmAt := -1, -1
		var reclaimThreshold float64
		for cursor := index + 1; cursor <= chainEnd; cursor++ {
			stroke := strokes[cursor]
			if !stroke.Confirmed {
				break
			}
			if stroke.Direction == direction {
				if breakAt >= 0 {
					reclaimed := direction == "up" && stroke.End.Price > reclaimThreshold || direction == "down" && stroke.End.Price < reclaimThreshold
					if reclaimed {
						appendFeature(&features, &featureDirection, strokes[breakAt], breakAt)
						breakAt, confirmAt = -1, -1
						continue
					}
					confirmAt = cursor
					break
				}
				continue
			}
			if _, reclaim, broken := brokenFeatureExtreme(features, stroke, direction); broken {
				breakAt = cursor
				reclaimThreshold = reclaim
				continue
			}
			appendFeature(&features, &featureDirection, stroke, cursor)
		}
		end := chainEnd
		segment := Segment{Direction: direction, Chain: chain, Level: strokes[index].Level, StartStroke: index, EndStroke: end, BreakStroke: -1, ConfirmStroke: -1,
			Start: strokes[index].Start.Date, End: strokes[end].End.Date, StartKind: strokes[index].Start.Kind, EndKind: strokes[end].End.Kind, StartPrice: strokes[index].Start.Price, EndPrice: strokes[end].End.Price,
			StartRawBar: strokes[index].StartRawBar, EndRawBar: strokes[end].EndRawBar, StartInputBar: strokes[index].InputStartBar, EndInputBar: strokes[end].InputEndBar,
			Low: strokes[index].Low, High: strokes[index].High, Feature: []FeatureElement{}}
		if breakAt >= 0 {
			segment.BreakStroke, segment.ConfirmStroke = breakAt, confirmAt
			segment.EndStroke = breakAt - 1
			if segment.EndStroke > segment.StartStroke {
				segment.Low = strokes[segment.StartStroke].Low
				segment.High = strokes[segment.StartStroke].High
				for k := segment.StartStroke + 1; k <= segment.EndStroke; k++ {
					segment.Low = math.Min(segment.Low, strokes[k].Low)
					segment.High = math.Max(segment.High, strokes[k].High)
				}
			}
			segment.Confirmed = confirmAt >= 0
			segment.Forming = !segment.Confirmed
			if segment.Confirmed {
				segment.ConfirmedAt = strokes[confirmAt].ConfirmedAt
			}
		}
		endpointStroke := segmentEndpoint(strokes, segment.StartStroke, segment.EndStroke, direction)
		segment.EndPointStroke = endpointStroke
		segment.End = strokes[endpointStroke].End.Date
		segment.EndKind = strokes[endpointStroke].End.Kind
		segment.EndPrice = strokes[endpointStroke].End.Price
		segment.EndRawBar = strokes[endpointStroke].EndRawBar
		segment.EndInputBar = strokes[endpointStroke].InputEndBar
		featuresEnd := segment.EndStroke
		features = segmentFeatures(strokes, index, featuresEnd, direction, chain)
		for i := range features {
			segment.Feature = append(segment.Feature, features[i].FeatureElement)
		}
		for k := segment.StartStroke + 1; k <= segment.EndStroke; k++ {
			segment.Low = math.Min(segment.Low, strokes[k].Low)
			segment.High = math.Max(segment.High, strokes[k].High)
		}
		if !segment.Confirmed {
			segment.Forming = true
		}
		segments = append(segments, segment)
		next := chainEnd + 1
		if segment.Confirmed {
			// The next segment starts with the counter-direction stroke after the
			// backtracked endpoint. Starting at the later feature break would
			// leave a gap and make recursive continuity impossible.
			next = segment.EndPointStroke + 1
		}
		if next <= index {
			next = index + 1
		}
		index = next
	}
	return segments
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
		if *sequenceDirection == 0 {
			*sequenceDirection = directionFromPair(last.High, last.Low, current.High, current.Low)
		}
		*features = append(*features, current)
		return
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
	featureDirection := 0
	for i := start + 1; i <= end; i++ {
		stroke := strokes[i]
		if stroke.Chain != chain || stroke.Direction == direction {
			continue
		}
		appendFeature(&features, &featureDirection, stroke, i)
	}
	return features
}

func brokenFeatureExtreme(features []featureRange, stroke Stroke, direction string) (float64, float64, bool) {
	for index := 1; index+1 < len(features); index++ {
		left, middle, right := features[index-1], features[index], features[index+1]
		var pivot bool
		if direction == "up" {
			pivot = middle.Low < left.Low && middle.High < left.High && middle.Low < right.Low && middle.High < right.High
			if pivot && stroke.Low < middle.Low {
				return middle.Low, middle.Low, true
			}
		} else {
			pivot = middle.Low > left.Low && middle.High > left.High && middle.Low > right.Low && middle.High > right.High
			if pivot && stroke.High > middle.High {
				return middle.High, middle.High, true
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
