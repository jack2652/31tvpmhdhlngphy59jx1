package chananalysis

import "fmt"

func buildSignalScanLevels(frame Frame, recursiveLevels []LevelAnalysis) []LevelAnalysis {
	levels := make([]LevelAnalysis, 0, len(recursiveLevels)+1)
	if len(frame.StrokesV4) > 0 {
		centers := buildCenters(frame.StrokesV4)
		recursiveCenters := make([]RecursiveCenter, 0, len(centers))
		for _, center := range centers {
			closedAt := center.ClosedAt
			recursiveCenters = append(recursiveCenters, RecursiveCenter{
				Level: center.Level, Kind: "initial", InputKind: "stroke", Chain: center.Chain,
				Start: center.Start, End: center.End, Lower: center.Lower, Upper: center.Upper,
				FormedAt: center.FormedAt, ClosedAt: closedAt, ConfirmedAt: closedAt, Forming: closedAt == "",
				StartSegment: center.StartStroke, EndSegment: center.EndStroke, OwnerMoveIndex: center.EndStroke,
				ExtensionCount: maxInt(0, center.StrokeCount-3), CenterID: fmt.Sprintf("P0-C%d-S%d-E%d", center.Chain, center.StartStroke, center.EndStroke),
			})
		}
		status := "ready"
		if hasFormingStroke(frame.StrokesV4) {
			status = "forming"
		}
		moves := make([]Segment, len(frame.StrokesV4))
		for index, stroke := range frame.StrokesV4 {
			moves[index] = segmentFromStroke(stroke, index)
		}
		levels = append(levels, LevelAnalysis{
			Level: 0, SourceKind: "pen", Status: status, InputCount: len(frame.StrokesV4),
			Strokes: frame.StrokesV4, Segments: moves, Centers: recursiveCenters,
		})
	}
	for _, level := range recursiveLevels {
		level.SourceKind = "recursive"
		levels = append(levels, level)
	}
	return levels
}

func segmentFromStroke(stroke Stroke, index int) Segment {
	return Segment{
		Direction: stroke.Direction, Chain: stroke.Chain, Level: 0,
		StartStroke: index, EndStroke: index, EndPointStroke: index,
		Start: stroke.Start.Date, End: stroke.End.Date,
		StartKind: stroke.Start.Kind, EndKind: stroke.End.Kind,
		Low: stroke.Low, High: stroke.High, StartPrice: stroke.Start.Price, EndPrice: stroke.End.Price,
		StartRawBar: stroke.StartRawBar, EndRawBar: stroke.EndRawBar,
		StartInputBar: stroke.InputStartBar, EndInputBar: stroke.InputEndBar,
		Confirmed: stroke.Confirmed, ConfirmedAt: stroke.ConfirmedAt, Forming: !stroke.Confirmed,
	}
}

func chanSegmentsAreContiguous(left, right Segment) bool {
	return left.Chain == right.Chain && left.Level == right.Level && left.End != "" &&
		left.End == right.Start && priceEqual(left.EndPrice, right.StartPrice)
}

func chanSegmentRangeIsContiguous(segments []Segment, start, end int) bool {
	if start < 0 || end < start || end >= len(segments) {
		return false
	}
	for index := start + 1; index <= end; index++ {
		if !chanSegmentsAreContiguous(segments[index-1], segments[index]) {
			return false
		}
	}
	return true
}
