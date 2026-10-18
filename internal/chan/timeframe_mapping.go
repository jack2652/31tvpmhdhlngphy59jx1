package chananalysis

import "fmt"

const timeframeMappingVersion = "chan-timeframe-map-v2"

type timeframeRelation struct {
	higher string
	lower  string
	role   string
}

// The chart timeframe ladder is explicit and separate from recursive Chan
// levels calculated within each chart: 15m -> 1h -> 2h -> 4h -> 1d -> 1wk.
var timeframeRelations = map[string]timeframeRelation{
	"15m": {higher: "1h", role: "lowest"},
	"1h":  {higher: "2h", lower: "15m", role: "intermediate"},
	"2h":  {higher: "4h", lower: "1h", role: "intermediate"},
	"4h":  {higher: "1d", lower: "2h", role: "intermediate"},
	"1d":  {higher: "1wk", lower: "4h", role: "intermediate"},
	"1wk": {lower: "1d", role: "highest"},
}

func timeframeRelationFor(period string) (timeframeRelation, bool) {
	relation, ok := timeframeRelations[period]
	return relation, ok
}

func setTimeframeMapping(frame *Frame, lowerAvailable bool) {
	if frame == nil {
		return
	}
	relation, ok := timeframeRelationFor(frame.Timeframe)
	if !ok {
		frame.TimeframeMapping = TimeframeMapping{MappingVersion: timeframeMappingVersion, Role: "unmapped", LowerTimeframeStatus: "unavailable", Reason: fmt.Sprintf("周期 %q 未定义跨周期映射", frame.Timeframe)}
		return
	}
	mapping := TimeframeMapping{MappingVersion: timeframeMappingVersion, HigherTimeframe: relation.higher, LowerTimeframe: relation.lower, Role: relation.role}
	switch {
	case relation.lower == "":
		mapping.LowerTimeframeStatus = "not_applicable"
		mapping.Reason = "当前是映射链最低级周期，没有更低周期可供确认"
	case lowerAvailable:
		mapping.LowerTimeframeStatus = "ready"
		mapping.Reason = "已加载映射的直接下级周期"
	default:
		mapping.LowerTimeframeStatus = "awaiting_lower_timeframe"
		mapping.Reason = "尚未加载映射的直接下级周期，不能据此升级信号确认"
	}
	frame.TimeframeMapping = mapping
	if relation.lower == "" {
		setWaitingTimeframeConfirmations(frame, "", "not_applicable", mapping.Reason)
	} else if !lowerAvailable {
		setWaitingTimeframeConfirmations(frame, relation.lower, "awaiting_lower_timeframe", mapping.Reason)
	}
}

// AttachMappedTimeframe links one frame to its explicitly mapped direct lower
// timeframe. Signal confirmation requires a confirmed lower-timeframe
// counter-move followed by a confirmed recovery move after the higher-frame
// signal became observable. The lower data's as-of time bounds the search.
func AttachMappedTimeframe(higher *Frame, lower Frame) {
	if higher == nil {
		return
	}
	relation, ok := timeframeRelationFor(higher.Timeframe)
	if !ok || relation.lower == "" {
		setTimeframeMapping(higher, false)
		return
	}
	if lower.Timeframe != relation.lower || lower.BarCount == 0 {
		setTimeframeMapping(higher, false)
		setWaitingTimeframeConfirmations(higher, relation.lower, "awaiting_lower_timeframe", "映射的直接下级周期数据不可用")
		return
	}
	if higher.Quality.PriceBasis != lower.Quality.PriceBasis {
		setTimeframeMapping(higher, false)
		higher.TimeframeMapping.LowerTimeframeStatus = "incompatible_price_basis"
		higher.TimeframeMapping.Reason = "上下级周期价格基准不同，不能比较结构方向"
		setWaitingTimeframeConfirmations(higher, relation.lower, "incompatible_price_basis", higher.TimeframeMapping.Reason)
		return
	}
	setTimeframeMapping(higher, true)
	for index := range higher.SignalsV4 {
		higher.SignalsV4[index].TimeframeConfirmation = confirmOnMappedTimeframe(higher.SignalsV4[index], *higher, lower)
	}
}

func setWaitingTimeframeConfirmations(frame *Frame, lower, status, reason string) {
	for index := range frame.SignalsV4 {
		frame.SignalsV4[index].TimeframeConfirmation = &TimeframeConfirmation{HigherTimeframe: frame.Timeframe, LowerTimeframe: lower, Status: status, StructureIDs: []string{}, Reason: reason}
	}
}

func confirmOnMappedTimeframe(signal V4Signal, higher, lower Frame) *TimeframeConfirmation {
	relation, ok := timeframeRelationFor(higher.Timeframe)
	confirmation := &TimeframeConfirmation{HigherTimeframe: higher.Timeframe, LowerTimeframe: relation.lower, Status: "awaiting_confirmation", StructureIDs: []string{}}
	if !ok || relation.lower == "" {
		confirmation.Status = "not_applicable"
		confirmation.Reason = "当前周期没有映射的直接下级周期"
		return confirmation
	}
	if signal.ObservedAt == "" || signal.Side != "buy" && signal.Side != "sell" {
		confirmation.Status = "unconfirmed"
		confirmation.Reason = "信号缺少可观察时间或买卖方向，无法进行时间前推确认"
		return confirmation
	}
	segments := levelZeroSegments(lower)
	confirmation.Reason = "等待下级周期先完成反向走势，再完成同信号方向的确认走势"
	direction := "up"
	if signal.Side == "sell" {
		direction = "down"
	}
	counterDirection := "down"
	if direction == "down" {
		counterDirection = "up"
	}
	for index := 0; index+1 < len(segments); index++ {
		counter, recovery := segments[index], segments[index+1]
		if !counter.Confirmed || !recovery.Confirmed || counter.Forming || recovery.Forming || counter.Chain != recovery.Chain || counter.Direction != counterDirection || recovery.Direction != direction || counter.End != recovery.Start {
			continue
		}
		if compareStructureTime(counter.Start, signal.ObservedAt) <= 0 || compareStructureTime(recovery.ConfirmedAt, signal.ObservedAt) <= 0 {
			continue
		}
		confirmedAt := recovery.ConfirmedAt
		if confirmedAt == "" {
			continue
		}
		if higher.AsOf != "" && compareStructureTime(confirmedAt, higher.AsOf) > 0 {
			continue
		}
		if lower.AsOf != "" && compareStructureTime(confirmedAt, lower.AsOf) > 0 {
			continue
		}
		confirmation.Status = "confirmed"
		confirmation.StructureIDs = []string{chanSegmentID(0, counter), chanSegmentID(0, recovery)}
		confirmation.ConfirmedAt = confirmedAt
		confirmation.Reason = fmt.Sprintf("%s 下级周期已先完成反向走势，再完成 %s 方向的已确认走势", relation.lower, signal.Side)
		return confirmation
	}
	return confirmation
}

func levelZeroSegments(frame Frame) []Segment {
	for _, level := range frame.RecursiveLevels {
		if level.Level == 0 {
			return level.Segments
		}
	}
	segments := make([]Segment, 0, len(frame.Segments))
	for _, segment := range frame.Segments {
		if segment.Level == 0 {
			segments = append(segments, segment)
		}
	}
	return segments
}

// compareStructureTime normalizes date-only observations to the end of the
// session. This conservatively prevents a same-day lower bar from being
// treated as known before a daily or weekly signal was observable.
func compareStructureTime(left, right string) int {
	left, right = normalizeStructureTime(left), normalizeStructureTime(right)
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func normalizeStructureTime(value string) string {
	if len(value) == len("2006-01-02") {
		return value + " 23:59"
	}
	return value
}
