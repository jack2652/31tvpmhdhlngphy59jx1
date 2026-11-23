package chananalysis

import (
	"fmt"
	"math"
	"sort"
)

// 预警只消费本级可见结构，独立于严格六类点及其下级确认和派生。
func buildStructureWarnings(frame Frame, levels []LevelAnalysis, strengths []Strength, divergenceRatio float64) []V4Signal {
	result := []V4Signal{}
	if math.IsNaN(divergenceRatio) || math.IsInf(divergenceRatio, 0) || divergenceRatio <= 0 || divergenceRatio >= 1 {
		return result
	}
	for _, level := range levels {
		// 高阶中枢的组成索引属于下级输入，不能套用基础层的三段关系。
		if level.Level != 0 {
			continue
		}
		for index := 2; index < len(level.Segments); index++ {
			previous, retest, current := level.Segments[index-2], level.Segments[index-1], level.Segments[index]
			observedAt := entryExitObservationAt(frame, level, current, index)
			if observedAt == "" || observedAt < current.End || frame.AsOf != "" && observedAt > frame.AsOf || current.Direction != previous.Direction || current.Direction == retest.Direction || current.TrendKind != previous.TrendKind || !chanSegmentRangeIsContiguous(level.Segments, index-2, index) {
				continue
			}
			if !warningSegmentVisible(previous, observedAt) || !warningSegmentVisible(retest, observedAt) {
				continue
			}
			var center *RecursiveCenter
			for centerIndex := range level.Centers {
				candidate := &level.Centers[centerIndex]
				if candidate.Chain != current.Chain || candidate.Level != current.Level || candidate.StartSegment > index-2 || candidate.FormedAt == "" || candidate.FormedAt > observedAt || centerSupersededAt(level.Centers, centerIndex, index, observedAt) {
					continue
				}
				if priceLess(previous.Low, candidate.Upper) && priceGreater(previous.High, candidate.Lower) && priceLess(retest.Low, candidate.Upper) && priceGreater(retest.High, candidate.Lower) {
					center = candidate
				}
			}
			if center == nil {
				continue
			}
			kind, detail := "", ""
			conditions := []string{"同级同链连续三段，前推进和回试在观察前已确认", "使用观察前已形成的固定中枢，不要求全部延伸闭合"}
			var ratio *float64
			newExtreme := current.Direction == "up" && priceGreater(current.EndPrice, previous.EndPrice) || current.Direction == "down" && priceLess(current.EndPrice, previous.EndPrice)
			if newExtreme {
				left := findSegmentStrength(strengths, previous, signalSourceKind(level))
				right, ok := measureSegmentStrength(frame.Candles, frame.MACD, level, current)
				if measured := findSegmentStrength(strengths, current, signalSourceKind(level)); measured != nil {
					right, ok = *measured, true
				}
				if left != nil && ok && left.WarmupComplete && right.WarmupComplete && left.MACDArea > 0 && right.MACDArea > 0 {
					value := right.MACDArea / left.MACDArea
					if value < divergenceRatio {
						kind = "center_divergence_sell"
						if current.Direction == "down" {
							kind = "center_divergence_buy"
						}
						ratio = ptr(value)
						conditions = append(conditions, "同向端点严格创新极值", fmt.Sprintf("同向MACD面积比低于 %.2f", divergenceRatio))
						detail = "中枢背景下局部推进价格创新极值而力度衰减；属于独立背驰预警，不等同于严格一类点"
					}
				}
			}
			anchorIndex, hasAnchor := warningBreakoutAnchor(level, *center, index-1, observedAt)
			anchor := 0.0
			if hasAnchor {
				anchor = level.Segments[anchorIndex].StartPrice
			}
			if kind == "" && hasAnchor && current.Direction == "up" && priceLess(retest.EndPrice, center.Lower) && priceLess(retest.EndPrice, previous.StartPrice) && priceLess(current.EndPrice, anchor) && priceLess(current.High, center.Upper) {
				kind = "rebound_failure_sell"
				conditions = append(conditions, "下跌端点跌破中枢下沿及前一推进起点", "反弹端点低于本轮跌破前高点，反弹全段未收复中枢上沿")
				detail = "反弹低于本轮跌破前高点，且未收复中枢上沿；属于反弹承压预警，严格三卖仍要求首次回试全段留在中枢下方并完成确认"
			} else if kind == "" && hasAnchor && current.Direction == "down" && priceGreater(retest.EndPrice, center.Upper) && priceGreater(retest.EndPrice, previous.StartPrice) && priceGreater(current.EndPrice, anchor) && priceGreater(current.Low, center.Lower) {
				kind = "pullback_hold_buy"
				conditions = append(conditions, "上涨端点突破中枢上沿及前一推进起点", "回踩端点高于本轮突破前低点，回踩全段未失守中枢下沿")
				detail = "突破中枢后的回踩形成抬高低点，且未失守中枢下沿；属于回踩支撑预警，不等同于严格三买"
			}
			if kind == "" || warningCoveredByStrictSignal(frame.SignalsV4, level, current, observedAt) {
				continue
			}
			warning := structureWarning(frame, level, *center, index, kind, observedAt, conditions, detail)
			if kind == "rebound_failure_sell" || kind == "pullback_hold_buy" {
				warning.Conditions = append(warning.Conditions, fmt.Sprintf("本轮突破前参考端点 %.4f", anchor))
				if anchorIndex < index-2 {
					warning.StructureRefs = append([]int{anchorIndex}, warning.StructureRefs...)
					warning.StructureIDs = append(warning.StructureIDs, chanSegmentID(level.Level, level.Segments[anchorIndex], signalSourceKind(level)))
				}
			}
			warning.StrengthRatio = ratio
			result = append(result, warning)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Date != result[j].Date {
			return result[i].Date < result[j].Date
		}
		if result[i].SourceKind != result[j].SourceKind {
			return result[i].SourceKind < result[j].SourceKind
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

func warningSegmentVisible(segment Segment, at string) bool {
	return segment.Confirmed && segment.ConfirmedAt != "" && segment.ConfirmedAt >= segment.End && segment.ConfirmedAt <= at
}

// 同轮修复可以多次尝试，比较跌破前高点/突破前低点；收复另一侧即结束这一轮。
func warningBreakoutAnchor(level LevelAnalysis, center RecursiveCenter, retestIndex int, at string) (int, bool) {
	direction := level.Segments[retestIndex].Direction
	for index := retestIndex; index >= center.StartSegment; index-- {
		segment := level.Segments[index]
		if segment.Chain != center.Chain || !warningSegmentVisible(segment, at) || index < retestIndex && !chanSegmentsAreContiguous(segment, level.Segments[index+1]) {
			return 0, false
		}
		if direction == "down" {
			if segment.Direction == "up" && priceGreaterOrEqual(segment.High, center.Upper) {
				return 0, false
			}
			if segment.Direction == "down" && priceGreater(segment.StartPrice, center.Upper) && priceLess(segment.EndPrice, center.Lower) {
				return index, true
			}
		} else if direction == "up" {
			if segment.Direction == "down" && priceLessOrEqual(segment.Low, center.Lower) {
				return 0, false
			}
			if segment.Direction == "up" && priceLess(segment.StartPrice, center.Lower) && priceGreater(segment.EndPrice, center.Upper) {
				return index, true
			}
		}
	}
	return 0, false
}

// 同时已经可见的严格点优先，不能用后来才出现的一类点抹去更早的预警。
func warningCoveredByStrictSignal(signals []V4Signal, level LevelAnalysis, current Segment, observedAt string) bool {
	side := "sell"
	if current.Direction == "down" {
		side = "buy"
	}
	for _, signal := range signals {
		if signal.SourceKind == signalSourceKind(level) && signal.Level == level.Level && signal.Side == side && signal.Date == current.End && signal.Price != nil && priceEqual(*signal.Price, current.EndPrice) && signal.ObservedAt != "" && signal.ObservedAt <= observedAt {
			return true
		}
	}
	return false
}

func structureWarning(frame Frame, level LevelAnalysis, center RecursiveCenter, index int, kind, observedAt string, conditions []string, detail string) V4Signal {
	current := level.Segments[index]
	side := "sell"
	if current.Direction == "down" {
		side = "buy"
	}
	ids := []string{center.CenterID}
	for _, segment := range level.Segments[index-2 : index+1] {
		ids = append(ids, chanSegmentID(level.Level, segment, signalSourceKind(level)))
	}
	warning := V4Signal{
		SignalID: classSignalID(signalSourceKind(level), level.Level, kind, current.End, ""), Level: level.Level, SourceKind: signalSourceKind(level), Kind: kind, Side: side,
		Status: "observing", Phase: "observation", ObservedAt: observedAt, PhaseAt: observedAt, Date: current.End, RuleVersion: RecursivePreviewVersion,
		PhaseHistory: []SignalPhase{{Phase: "observation", At: observedAt}}, StructureRefs: []int{index - 2, index - 1, index}, StructureIDs: ids, RelatedCenterID: center.CenterID,
		Conditions: conditions, Missing: []string{"等待有效反向结构固定当前端点"}, Invalidation: ptr(current.EndPrice), Price: ptr(current.EndPrice),
		MarketFilter: "unfiltered", LowerLevelStatus: "not_required", Detail: detail,
		TimeframeConfirmation: &TimeframeConfirmation{HigherTimeframe: frame.Timeframe, Status: "not_required", StructureIDs: []string{}, Reason: "独立本级结构预警，不用于确认严格六类买卖点"},
	}
	if current.Confirmed && current.ConfirmedAt != "" && current.ConfirmedAt >= observedAt && (frame.AsOf == "" || current.ConfirmedAt <= frame.AsOf) {
		warning.Status, warning.Phase, warning.PhaseAt, warning.ConfirmedAt = "warning_confirmed", "warning_confirmed", current.ConfirmedAt, current.ConfirmedAt
		warning.Missing = []string{}
		warning.PhaseHistory = append(warning.PhaseHistory, SignalPhase{Phase: "warning_confirmed", At: current.ConfirmedAt})
	}
	if at := firstV4InvalidationAt(frame.Candles, observedAt, current.Direction, current.EndPrice); at != "" && (frame.AsOf == "" || at <= frame.AsOf) {
		kept := warning.PhaseHistory[:0]
		for _, phase := range warning.PhaseHistory {
			if phase.At < at {
				kept = append(kept, phase)
			}
		}
		warning.PhaseHistory = append(kept, SignalPhase{Phase: "invalidated", At: at})
		warning.Status, warning.Phase, warning.PhaseAt, warning.Missing = "invalidated", "invalidated", at, []string{}
		if warning.ConfirmedAt >= at {
			warning.ConfirmedAt = ""
		}
	}
	return warning
}

func setFrameSignalSymbols(frame *Frame, symbol string) {
	for _, signals := range [][]V4Signal{frame.SignalsV4, frame.StructureWarnings, frame.EarlyRiskWarnings} {
		for index := range signals {
			signals[index].Symbol = symbol
		}
	}
}
