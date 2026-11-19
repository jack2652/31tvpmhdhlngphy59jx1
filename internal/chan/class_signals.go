package chananalysis

import (
	"fmt"
	"sort"
)

// buildClass23Signals emits explicit waiting candidates when the direct lower
// level is unavailable, and only confirms a signal from structures already
// confirmed in the supplied prefix.
func buildClass23Signals(frame Frame, class1 []V4Signal, levels []LevelAnalysis) []V4Signal {
	result := make([]V4Signal, 0)
	for _, parent := range class1 {
		if parent.Kind != "class1_buy" && parent.Kind != "class1_sell" {
			continue
		}
		if parent.ConfirmedAt == "" {
			if parent.Status == "invalidated" {
				continue
			}
			result = append(result, class2AwaitingClass1(parent))
			continue
		}
		second := class2Signal(frame, parent, levels)
		if parent.Status == "invalidated" && second.Price == nil {
			continue
		}
		result = append(result, second)
	}
	result = append(result, class3Signals(frame, levels)...)
	return result
}

func class2AwaitingClass1(parent V4Signal) V4Signal {
	kind, side, label := "class2_sell", "sell", "一卖"
	if parent.Kind == "class1_buy" {
		kind, side, label = "class2_buy", "buy", "一买"
	}
	return V4Signal{
		SignalID: classSignalID(parent.SourceKind, parent.Level, kind, parent.Date, parent.SignalID), Symbol: parent.Symbol,
		Level: parent.Level, SourceKind: signalSourceKind(LevelAnalysis{SourceKind: parent.SourceKind}), Kind: kind, Side: side, Status: "awaiting_class1_confirmation",
		Phase: "awaiting_class1_confirmation", PhaseAt: parent.ObservedAt,
		PhaseHistory: []SignalPhase{{Phase: "awaiting_class1_confirmation", At: parent.ObservedAt}},
		RuleVersion:  RecursivePreviewVersion, Date: parent.Date, ObservedAt: parent.ObservedAt,
		StructureRefs: append([]int(nil), parent.StructureRefs...), StructureIDs: []string{parent.SignalID},
		RelatedSignalID: parent.SignalID, Conditions: []string{"一类点背驰候选已出现；一类点确认后才检查次级别回试"},
		Missing: []string{"等待" + label + "反向结构确认"}, Invalidation: parent.Invalidation,
		MarketFilter: parent.MarketFilter, LowerLevelStatus: "awaiting_class1_confirmation",
		Detail: "一类点尚未得到后续反向结构确认，暂不检查二类点",
	}
}

func class2Signal(frame Frame, parent V4Signal, levels []LevelAnalysis) V4Signal {
	base := class2SignalBase(parent)
	if parent.Level <= 0 || parent.ConfirmedAt == "" {
		return base
	}
	parentSource := signalSourceKind(LevelAnalysis{SourceKind: parent.SourceKind})
	lower := findChanLevelWithSource(levels, parent.Level-1, parentSource)
	if lower == nil || lower.Status == "awaiting_lower_level" || len(lower.Segments) == 0 {
		return base
	}
	base.LowerLevelStatus = lower.Status
	chain := signalChain(levels, parent.Level, parent.StructureRefs, parentSource)
	return resolveClass2Signal(frame, parent, levels, base, lower.Segments, lower.Level, signalSourceKind(*lower), chain, true, "", "")
}

func class2SignalFromMappedTimeframe(frame Frame, parent V4Signal, levels []LevelAnalysis, lowerFrame Frame) V4Signal {
	base := class2SignalBase(parent)
	if parent.ConfirmedAt == "" {
		return base
	}
	lowerSegments := levelZeroSegments(lowerFrame)
	if len(lowerSegments) == 0 {
		return base
	}
	base.LowerLevelStatus = "ready"
	parentSource := signalSourceKind(LevelAnalysis{SourceKind: parent.SourceKind})
	chain := signalChain(levels, parent.Level, parent.StructureRefs, parentSource)
	return resolveClass2Signal(frame, parent, levels, base, lowerSegments, 0, "recursive", chain, false, lowerFrame.Timeframe, lowerFrame.AsOf)
}

func class2SignalBase(parent V4Signal) V4Signal {
	kind, side := "class2_sell", "sell"
	if parent.Kind == "class1_buy" {
		kind, side = "class2_buy", "buy"
	}
	return V4Signal{
		SignalID: classSignalID(parent.SourceKind, parent.Level, kind, parent.Date, parent.SignalID), Symbol: parent.Symbol,
		Level: parent.Level, SourceKind: signalSourceKind(LevelAnalysis{SourceKind: parent.SourceKind}), Kind: kind, Side: side, Status: "awaiting_lower_level",
		Phase: "awaiting_lower_level", PhaseAt: parent.ObservedAt,
		PhaseHistory: []SignalPhase{{Phase: "awaiting_lower_level", At: parent.ObservedAt}},
		RuleVersion:  RecursivePreviewVersion, Date: parent.Date, ObservedAt: parent.ObservedAt,
		StructureRefs: append([]int(nil), parent.StructureRefs...),
		StructureIDs:  []string{parent.SignalID}, RelatedSignalID: parent.SignalID,
		Conditions: []string{"一类点确认后，次级别先完成同方向推进", "次级别首次反向回试端点严格不越过一类点失效边界", "本级确认恢复一类点方向的已确认线段"},
		Missing:    []string{"等待直接下一级别的已确认结构"}, Invalidation: parent.Invalidation,
		MarketFilter: parent.MarketFilter, LowerLevelStatus: "awaiting_lower_level",
		Detail: "等待一类点确认后的次级别同方向推进、首次回试及本级恢复确认",
	}
}

func resolveClass2Signal(frame Frame, parent V4Signal, levels []LevelAnalysis, base V4Signal, lowerSegments []Segment, lowerLevel int, lowerSource string, parentChain int, requireParentChain bool, lowerTimeframe, lowerAsOf string) V4Signal {
	buy := parent.Kind == "class1_buy"
	impulseDir, retestDir, recoveryDir := "down", "up", "down"
	if buy {
		impulseDir, retestDir, recoveryDir = "up", "down", "up"
	}
	parentSource := signalSourceKind(LevelAnalysis{SourceKind: parent.SourceKind})
	parentInvalidatedAt := ""
	if parent.Status == "invalidated" && parent.Phase == "invalidated" {
		parentInvalidatedAt = parent.PhaseAt
	}
	// 一类点的反向结构确认后才可作为二类判断依据；此前开始的次级别走势
	// 不能视为一类点确认后的推进与回试序列。
	parentConfirmedAt := parent.ConfirmedAt
	impulseIndex, retestIndex := -1, -1
	lowerChain := -1
	for index, segment := range lowerSegments {
		if segment.Confirmed && !confirmedSegmentWithinSnapshots(segment, frame.AsOf, lowerAsOf) {
			continue
		}
		if requireParentChain && segment.Chain != parentChain {
			continue
		}
		if lowerTimeframe != "" {
			if compareStructureTime(segment.Start, parentConfirmedAt) <= 0 || compareStructureTime(segment.End, parent.Date) <= 0 {
				continue
			}
			if frame.AsOf != "" && (compareStructureTime(segment.End, frame.AsOf) > 0 || segment.ConfirmedAt != "" && compareStructureTime(segment.ConfirmedAt, frame.AsOf) > 0) {
				continue
			}
			if lowerAsOf != "" && (compareStructureTime(segment.End, lowerAsOf) > 0 || segment.ConfirmedAt != "" && compareStructureTime(segment.ConfirmedAt, lowerAsOf) > 0) {
				continue
			}
		} else if segment.Start < parentConfirmedAt || segment.End <= parent.Date {
			continue
		}
		if parentInvalidatedAt != "" && (segment.Start >= parentInvalidatedAt || segment.End >= parentInvalidatedAt || segment.ConfirmedAt == "" || segment.ConfirmedAt >= parentInvalidatedAt) {
			continue
		}
		if !requireParentChain && lowerChain >= 0 && segment.Chain != lowerChain {
			base.Missing = []string{"次级别推进与首次回试之间存在结构链断点"}
			return base
		}
		if impulseIndex < 0 {
			if segment.Confirmed && segment.ConfirmedAt != "" && segment.Direction == impulseDir {
				impulseIndex = index
				lowerChain = segment.Chain
				continue
			}
			if (!segment.Confirmed || segment.ConfirmedAt == "") && segment.Direction == impulseDir {
				base.Missing = []string{"等待次级别同方向推进确认时间"}
				return base
			}
			continue
		}
		impulse := lowerSegments[impulseIndex]
		if segment.Direction == impulseDir {
			if !chanSegmentsAreContiguous(impulse, segment) {
				base.Missing = []string{"等待次级别连续推进结构确认"}
				return base
			}
			if segment.Confirmed && segment.ConfirmedAt != "" {
				impulseIndex = index
				continue
			}
			base.Missing = []string{"等待次级别同方向推进确认时间"}
			return base
		}
		if segment.Direction != retestDir || !chanSegmentsAreContiguous(impulse, segment) {
			base.Missing = []string{"等待次级别推进后的连续首次回试结构"}
			return base
		}
		if !segment.Confirmed {
			base.Missing = []string{"等待次级别第一次回试确认"}
			return base
		}
		if segment.ConfirmedAt <= impulse.ConfirmedAt {
			base.Missing = []string{"等待次级别首次回试的后续确认"}
			return base
		}
		retestIndex = index
		break
	}
	if impulseIndex < 0 {
		base.Missing = []string{"等待一类点确认后次级别同方向推进完成"}
		return base
	}
	impulse := lowerSegments[impulseIndex]
	base.StructureIDs = append(base.StructureIDs, class2LowerStructureID(lowerLevel, impulse, lowerSource, lowerTimeframe))
	base.Missing = []string{"等待该次级别同方向推进后的第一次已确认反向回试"}
	if retestIndex < 0 {
		return base
	}
	retest := lowerSegments[retestIndex]
	base.StructureIDs = append(base.StructureIDs, class2LowerStructureID(lowerLevel, retest, lowerSource, lowerTimeframe))
	base.Date = retest.End
	if lowerTimeframe != "" {
		base.Date = structureDate(retest.End)
	}
	base.ObservedAt, base.Price = retest.ConfirmedAt, ptr(retest.EndPrice)
	base.Phase, base.PhaseAt = "first_retest", retest.ConfirmedAt
	base.PhaseHistory = []SignalPhase{{Phase: "first_retest", At: retest.ConfirmedAt}}
	valid := parent.Invalidation == nil || buy && priceGreater(retest.Low, *parent.Invalidation) || !buy && priceLess(retest.High, *parent.Invalidation)
	if !valid {
		base.Status, base.Phase, base.PhaseAt, base.ConfirmedAt = "invalidated", "invalidated", retest.ConfirmedAt, retest.ConfirmedAt
		base.PhaseHistory = append(base.PhaseHistory, SignalPhase{Phase: "invalidated", At: retest.ConfirmedAt})
		base.Missing = []string{}
		base.Detail = "次级别第一次回试越过一类点失效边界，二类候选失效"
		return base
	}
	if buy {
		base.Conditions = append(base.Conditions, "第一次回试低点严格高于一买低点")
	} else {
		base.Conditions = append(base.Conditions, "第一次反弹高点严格低于一卖高点")
	}
	base.Status, base.Missing = "observing", []string{"等待本级恢复方向的已确认线段"}
	base.Detail = "次级别首次回试满足一类点失效边界，等待本级恢复线段确认"
	for _, recovery := range levelSegments(levels, parent.Level, parentSource) {
		if !recovery.Confirmed || recovery.Chain != parentChain || recovery.Direction != recoveryDir {
			continue
		}
		if lowerTimeframe == "" {
			if recovery.Start < retest.End || recovery.ConfirmedAt <= retest.ConfirmedAt || recovery.End <= retest.End {
				continue
			}
		} else if !higherStructureAfterLower(recovery.Start, retest.End) || !higherStructureAfterLower(recovery.ConfirmedAt, retest.ConfirmedAt) || !higherStructureAfterLower(recovery.End, retest.End) {
			continue
		}
		if parentInvalidatedAt != "" && (recovery.End >= parentInvalidatedAt || recovery.ConfirmedAt >= parentInvalidatedAt) {
			continue
		}
		base.Status, base.Phase = "confirmed", "class2_confirmed"
		base.PhaseAt, base.ConfirmedAt = recovery.ConfirmedAt, recovery.ConfirmedAt
		base.PhaseHistory = append(base.PhaseHistory, SignalPhase{Phase: base.Phase, At: recovery.ConfirmedAt})
		if parentLevel := findChanLevelWithSource(levels, parent.Level, parentSource); parentLevel != nil {
			base.StructureIDs = append(base.StructureIDs, chanSegmentID(parent.Level, recovery, signalSourceKind(*parentLevel)))
		}
		base.Missing = []string{}
		base.Detail = "次级别第一次回试有效，本级恢复线段已确认"
		break
	}
	if parent.Invalidation != nil {
		direction := "up"
		if buy {
			direction = "down"
		}
		at := firstV4InvalidationAt(frame.Candles, base.ObservedAt, direction, *parent.Invalidation)
		if parentInvalidatedAt != "" && (at == "" || parentInvalidatedAt < at) {
			at = parentInvalidatedAt
		}
		if at != "" {
			base.Status, base.Phase, base.PhaseAt, base.ConfirmedAt = "invalidated", "invalidated", at, ""
			kept := base.PhaseHistory[:0]
			for _, event := range base.PhaseHistory {
				if event.At < at {
					kept = append(kept, event)
				}
			}
			base.PhaseHistory = append(kept, SignalPhase{Phase: "invalidated", At: at})
			base.Missing = []string{}
			for _, event := range kept {
				if event.Phase == "class2_confirmed" {
					base.ConfirmedAt = event.At
					base.Detail = "二类点已确认，之后触及一类点失效边界，信号失效"
					break
				}
			}
			if base.ConfirmedAt == "" {
				base.Detail = "二类候选在本级恢复确认前触及一类点失效边界，候选失效"
			}
		}
	}
	return base
}

func class2LowerStructureID(level int, segment Segment, sourceKind, timeframe string) string {
	id := chanSegmentID(level, segment, sourceKind)
	if timeframe != "" {
		return timeframe + "|" + id
	}
	return id
}

func attachMappedClass2Signals(higher *Frame, lower Frame) {
	if higher == nil {
		return
	}
	levels := buildSignalScanLevels(*higher, higher.RecursiveLevels)
	for _, parent := range higher.SignalsV4 {
		if parent.Level != 0 || parent.ConfirmedAt == "" || parent.Kind != "class1_buy" && parent.Kind != "class1_sell" {
			continue
		}
		second := class2SignalFromMappedTimeframe(*higher, parent, levels, lower)
		if parent.Status == "invalidated" && second.Price == nil {
			continue
		}
		found := false
		for index := range higher.SignalsV4 {
			if higher.SignalsV4[index].RelatedSignalID == parent.SignalID && (higher.SignalsV4[index].Kind == "class2_buy" || higher.SignalsV4[index].Kind == "class2_sell") {
				higher.SignalsV4[index] = second
				found = true
				break
			}
		}
		if !found {
			higher.SignalsV4 = append(higher.SignalsV4, second)
		}
	}
	sort.SliceStable(higher.SignalsV4, func(i, j int) bool {
		if higher.SignalsV4[i].Date != higher.SignalsV4[j].Date {
			return higher.SignalsV4[i].Date < higher.SignalsV4[j].Date
		}
		if higher.SignalsV4[i].Level != higher.SignalsV4[j].Level {
			return higher.SignalsV4[i].Level < higher.SignalsV4[j].Level
		}
		if higher.SignalsV4[i].Kind != higher.SignalsV4[j].Kind {
			return higher.SignalsV4[i].Kind < higher.SignalsV4[j].Kind
		}
		return higher.SignalsV4[i].SourceKind < higher.SignalsV4[j].SourceKind
	})
}

func class3Signals(frame Frame, levels []LevelAnalysis) []V4Signal {
	result := make([]V4Signal, 0)
	for _, level := range levels {
		for _, center := range level.Centers {
			if center.Forming || center.ConfirmedAt == "" {
				continue
			}
			ownerIndex := centerOwnerMoveIndex(center)
			firstDeparture := ownerIndex
			if firstDeparture < 0 || firstDeparture >= len(level.Segments) {
				continue
			}
			departureIndex := -1
			for index := firstDeparture; index < len(level.Segments); index++ {
				segment := level.Segments[index]
				if segment.Chain != center.Chain || segment.Level != center.Level || index != ownerIndex && (segment.End <= center.End || segment.ConfirmedAt != "" && segment.ConfirmedAt < center.ConfirmedAt) {
					continue
				}
				if index > ownerIndex && !chanSegmentsAreContiguous(level.Segments[index-1], segment) {
					break
				}
				if recursiveSegmentLeavesCenter(segment, &center) {
					departureIndex = index
					break
				}
			}
			if departureIndex < 0 {
				for index := firstDeparture; index < len(level.Segments); index++ {
					segment := level.Segments[index]
					if segment.Chain != center.Chain || segment.Level != center.Level || index != ownerIndex && segment.End <= center.End {
						continue
					}
					if index > ownerIndex && !chanSegmentsAreContiguous(level.Segments[index-1], segment) {
						break
					}
					if !segment.Confirmed && recursiveSegmentLeavesCenter(segment, &center) {
						result = append(result, class3WaitingSignal(level, center, segment, index, nil, frame.AsOf))
						break
					}
				}
				continue
			}
			departure := level.Segments[departureIndex]
			if !departure.Confirmed || departure.ConfirmedAt == "" {
				result = append(result, class3WaitingSignal(level, center, departure, departureIndex, nil, frame.AsOf))
				continue
			}
			if departureIndex+1 >= len(level.Segments) {
				result = append(result, class3WaitingSignal(level, center, departure, departureIndex, nil, frame.AsOf))
				continue
			}
			retest := level.Segments[departureIndex+1]
			if retest.Chain != center.Chain || retest.Level != center.Level || retest.Direction == departure.Direction || !chanSegmentRangeIsContiguous(level.Segments, ownerIndex, departureIndex+1) {
				continue
			}
			buy := departure.Direction == "up" && retest.Direction == "down" && priceGreater(retest.Low, center.Upper)
			sell := departure.Direction == "down" && retest.Direction == "up" && priceLess(retest.High, center.Lower)
			if !retest.Confirmed {
				waiting := class3WaitingSignal(level, center, departure, departureIndex, &retest, frame.AsOf)
				if priceLessOrEqual(retest.Low, center.Upper) && priceGreaterOrEqual(retest.High, center.Lower) {
					waiting.Status, waiting.Phase, waiting.PhaseAt = "invalidated", "invalidated", frame.AsOf
					waiting.PhaseHistory = append(waiting.PhaseHistory, SignalPhase{Phase: "invalidated", At: frame.AsOf})
					waiting.Missing = []string{}
					waiting.Detail = "首次回试已进入中枢范围，三类候选失效"
				} else if retest.Direction == departure.Direction {
					waiting.Status, waiting.Phase, waiting.PhaseAt = "invalidated", "invalidated", frame.AsOf
					waiting.PhaseHistory = append(waiting.PhaseHistory, SignalPhase{Phase: "invalidated", At: frame.AsOf})
					waiting.Missing = []string{}
					waiting.Detail = "首次反方向回试结束并继续离开，三类回试条件已失效"
				}
				result = append(result, waiting)
				continue
			}
			if retest.ConfirmedAt == "" || retest.ConfirmedAt <= departure.ConfirmedAt {
				continue
			}
			if center.ConfirmedAt > retest.ConfirmedAt {
				continue
			}
			if !buy && !sell {
				kind, side, invalidation := "class3_sell", "sell", center.Lower
				if departure.Direction == "up" {
					kind, side, invalidation = "class3_buy", "buy", center.Upper
				}
				at := retest.ConfirmedAt
				result = append(result, V4Signal{
					SignalID: classSignalID(signalSourceKind(level), level.Level, kind, departure.End, center.CenterID), Level: level.Level, SourceKind: signalSourceKind(level), Kind: kind, Side: side,
					Status: "invalidated", Phase: "invalidated", PhaseAt: at, PhaseHistory: []SignalPhase{{Phase: "invalidated", At: at}}, RuleVersion: RecursivePreviewVersion,
					Date: retest.End, ObservedAt: at, StructureRefs: []int{departureIndex, departureIndex + 1},
					StructureIDs: []string{center.CenterID, chanSegmentID(level.Level, departure, signalSourceKind(level)), chanSegmentID(level.Level, retest, signalSourceKind(level))},
					Conditions:   []string{"已确认中枢闭合", "已确认走势段离开中枢", "首次反方向回试已确认"}, Missing: []string{}, Invalidation: ptr(invalidation), Price: ptr(retest.EndPrice),
					MarketFilter: "unfiltered", LowerLevelStatus: "unconfirmed", RelatedCenterID: center.CenterID,
					Detail: fmt.Sprintf("首次反方向回试触及或进入中枢范围，三类候选失效；边界 %.4f", invalidation),
				})
				continue
			}
			kind, side, invalidation := "class3_sell", "sell", center.Lower
			if buy {
				kind, side, invalidation = "class3_buy", "buy", center.Upper
			}
			observedAt := retest.ConfirmedAt
			status, phase, phaseAt := "confirmed", "class3_confirmed", observedAt
			phaseHistory := []SignalPhase{{Phase: phase, At: phaseAt}}
			if at := firstClass3InvalidationAt(frame.Candles, observedAt, buy, invalidation); at != "" {
				status, phase, phaseAt = "invalidated", "invalidated", at
				phaseHistory = append(phaseHistory, SignalPhase{Phase: phase, At: phaseAt})
			}
			conditions := []string{"已确认中枢闭合", "已确认走势段离开中枢", "首次反方向回试全段严格留在中枢外"}
			if buy {
				conditions = append(conditions, "回试低点严格高于 ZG")
			} else {
				conditions = append(conditions, "回试高点严格低于 ZD")
			}
			date := retest.End
			result = append(result, V4Signal{
				SignalID: classSignalID(signalSourceKind(level), level.Level, kind, date, center.CenterID), Level: level.Level, SourceKind: signalSourceKind(level), Kind: kind, Side: side,
				Status: status, Phase: phase, PhaseAt: phaseAt, PhaseHistory: phaseHistory, RuleVersion: RecursivePreviewVersion,
				Date: date, ObservedAt: observedAt, ConfirmedAt: observedAt, StructureRefs: []int{departureIndex, departureIndex + 1},
				StructureIDs: []string{center.CenterID, chanSegmentID(level.Level, departure, signalSourceKind(level)), chanSegmentID(level.Level, retest, signalSourceKind(level))},
				Conditions:   conditions, Missing: []string{}, Invalidation: ptr(invalidation), Price: ptr(retest.EndPrice),
				MarketFilter: "unfiltered", LowerLevelStatus: "unconfirmed", RelatedCenterID: center.CenterID,
				Detail: fmt.Sprintf("中枢首次回试严格留在外侧；失效边界 %.4f；市场过滤状态未提供", invalidation),
			})
		}
	}
	return result
}

// 三类点离开段须从与中枢重叠的摆动开始，并在结束时越过中枢边界。
// 后续走势若从中枢外开始，只算延续，不算从该中枢重新离开。
func recursiveSegmentLeavesCenter(segment Segment, center *RecursiveCenter) bool {
	if center == nil || !segmentMovesAway(segment, center) {
		return false
	}
	return priceGreaterOrEqual(segment.High, center.Lower) && priceLessOrEqual(segment.Low, center.Upper)
}

func class3WaitingSignal(level LevelAnalysis, center RecursiveCenter, departure Segment, departureIndex int, retest *Segment, asOf string) V4Signal {
	buy := departure.Direction == "up"
	kind, side, boundary := "class3_sell", "sell", center.Lower
	if buy {
		kind, side, boundary = "class3_buy", "buy", center.Upper
	}
	observedAt := departure.ConfirmedAt
	date := departure.End
	price := departure.EndPrice
	refs := []int{departureIndex}
	ids := []string{center.CenterID, chanSegmentID(level.Level, departure, signalSourceKind(level))}
	missing := []string{"等待中枢离开后的首次反方向线段"}
	if retest != nil {
		date, price = retest.End, retest.EndPrice
		refs = append(refs, departureIndex+1)
		ids = append(ids, chanSegmentID(level.Level, *retest, signalSourceKind(level)))
		missing = []string{"等待首次回试线段确认"}
		if !retest.Confirmed && asOf != "" {
			observedAt = asOf
		}
	} else if !departure.Confirmed {
		// 形成中的离开段尚无已确认的离开端点；临时标记锚定当前可见边界，
		// 并在缺失条件中说明仍待确认。
		date = departure.End
	}
	return V4Signal{
		SignalID: classSignalID(signalSourceKind(level), level.Level, kind, date, center.CenterID), Level: level.Level, SourceKind: signalSourceKind(level), Kind: kind, Side: side,
		Status: "observing", Phase: "awaiting_first_retest", PhaseAt: observedAt,
		PhaseHistory: []SignalPhase{{Phase: "awaiting_first_retest", At: observedAt}}, RuleVersion: RecursivePreviewVersion,
		Date: date, ObservedAt: observedAt, StructureRefs: refs, StructureIDs: ids, Conditions: []string{"已确认中枢闭合", "已确认走势段离开中枢", "只评估离开后的第一次反方向回试"},
		Missing: missing, Invalidation: ptr(boundary), Price: ptr(price), MarketFilter: "unfiltered", LowerLevelStatus: "unconfirmed", RelatedCenterID: center.CenterID,
		Detail: "中枢离开已确认；等待首次反方向回试及其严格留在中枢外的确认条件",
	}
}

func firstClass3InvalidationAt(bars []Bar, observedAt string, buy bool, boundary float64) string {
	for _, bar := range bars {
		if bar.Date <= observedAt {
			continue
		}
		if buy && priceLessOrEqual(bar.Low, boundary) || !buy && priceGreaterOrEqual(bar.High, boundary) {
			return bar.Date
		}
	}
	return ""
}

func findChanLevelWithSource(levels []LevelAnalysis, level int, sourceKind string) *LevelAnalysis {
	if sourceKind == "" {
		sourceKind = "recursive"
	}
	for index := range levels {
		if levels[index].Level == level && signalSourceKind(levels[index]) == sourceKind {
			return &levels[index]
		}
	}
	return nil
}

func levelSegments(levels []LevelAnalysis, level int, sourceKind string) []Segment {
	if item := findChanLevelWithSource(levels, level, sourceKind); item != nil {
		return item.Segments
	}
	return nil
}

func chanSegmentID(level int, segment Segment, sourceKind string) string {
	if sourceKind == "" {
		sourceKind = "recursive"
	}
	return fmt.Sprintf("%s|%s|L%d|C%d|%s|%s|%s", RecursivePreviewVersion, sourceKind, level, segment.Chain, segment.Direction, segment.Start, segment.End)
}

func classSignalID(sourceKind string, level int, kind, date, parent string) string {
	if sourceKind == "" {
		sourceKind = "recursive"
	}
	return fmt.Sprintf("%s|%s|L%d|%s|%s|%s", RecursivePreviewVersion, sourceKind, level, kind, date, parent)
}

func signalSourceKind(level LevelAnalysis) string {
	if level.SourceKind != "" {
		return level.SourceKind
	}
	return "recursive"
}

func signalChain(levels []LevelAnalysis, level int, refs []int, sourceKind string) int {
	if item := findChanLevelWithSource(levels, level, sourceKind); item != nil && len(refs) > 0 && refs[0] >= 0 && refs[0] < len(item.Segments) {
		return item.Segments[refs[0]].Chain
	}
	return -1
}

func firstClass1Chain(levels []LevelAnalysis, parent V4Signal) int {
	return signalChain(levels, parent.Level, parent.StructureRefs, parent.SourceKind)
}
