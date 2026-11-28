package chananalysis

import (
	"fmt"
	"slices"
)

func buildSignalScanLevels(frame Frame, recursiveLevels []LevelAnalysis) []LevelAnalysis {
	levels := make([]LevelAnalysis, 0, len(recursiveLevels)+1)
	if len(frame.StrokesV4) > 0 {
		var workspace penSignalWorkspace
		levels = append(levels, workspace.build(frame.StrokesV4))
	}
	for _, level := range recursiveLevels {
		level.SourceKind = "recursive"
		levels = append(levels, level)
	}
	return levels
}

type penSignalWorkspace struct {
	centers          []Center
	recursiveCenters []RecursiveCenter
	moves            []Segment
}

// 提前预警会连续检查多个历史前缀，每次检查结束后可覆盖临时走势和中枢切片。
func (workspace *penSignalWorkspace) build(strokes []Stroke) LevelAnalysis {
	workspace.centers = appendCenters(workspace.centers[:0], strokes)
	workspace.recursiveCenters = slices.Grow(workspace.recursiveCenters[:0], len(workspace.centers))
	for _, center := range workspace.centers {
		closedAt := center.ClosedAt
		workspace.recursiveCenters = append(workspace.recursiveCenters, RecursiveCenter{
			Level: center.Level, Kind: "initial", InputKind: "stroke", Chain: center.Chain,
			Start: center.Start, End: center.End, Lower: center.Lower, Upper: center.Upper,
			FormedAt: center.FormedAt, ClosedAt: closedAt, ConfirmedAt: closedAt, Forming: closedAt == "",
			StartSegment: center.StartStroke, EndSegment: center.EndStroke, OwnerMoveIndex: center.EndStroke,
			ExtensionCount: maxInt(0, center.StrokeCount-3), CenterID: fmt.Sprintf("P0-C%d-S%d-E%d", center.Chain, center.StartStroke, center.EndStroke),
		})
	}
	status := "ready"
	if hasFormingStroke(strokes) {
		status = "forming"
	}
	workspace.moves = slices.Grow(workspace.moves[:0], len(strokes))[:len(strokes)]
	for index, stroke := range strokes {
		workspace.moves[index] = segmentFromStroke(stroke, index)
	}
	return LevelAnalysis{Level: 0, SourceKind: "pen", Status: status, InputCount: len(strokes),
		Strokes: strokes, Segments: workspace.moves, Centers: workspace.recursiveCenters}
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
