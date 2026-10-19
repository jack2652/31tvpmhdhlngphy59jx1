package chananalysis

import "fmt"

// buildClass23Signals emits explicit waiting candidates when the direct lower
// level is unavailable, and only confirms a signal from structures already
// confirmed in the supplied prefix.
func buildClass23Signals(frame Frame, class1 []V4Signal, levels []LevelAnalysis) []V4Signal {
	result := make([]V4Signal, 0)
	for _, parent := range class1 {
		if parent.Kind != "class1_buy" && parent.Kind != "class1_sell" || parent.ConfirmedAt == "" || parent.Status == "invalidated" {
			continue
		}
		result = append(result, class2Signal(frame, parent, levels))
	}
	result = append(result, class3Signals(frame, levels)...)
	return result
}

func class2Signal(frame Frame, parent V4Signal, levels []LevelAnalysis) V4Signal {
	buy := parent.Kind == "class1_buy"
	kind, side, impulseDir, retestDir, recoveryDir := "class2_sell", "sell", "down", "up", "down"
	if buy {
		kind, side, impulseDir, retestDir, recoveryDir = "class2_buy", "buy", "up", "down", "up"
	}
	base := V4Signal{
		SignalID: classSignalID(parent.Level, kind, parent.Date, parent.SignalID), Symbol: parent.Symbol,
		Level: parent.Level, Kind: kind, Side: side, Status: "awaiting_lower_level",
		Phase: "awaiting_lower_level", PhaseAt: parent.ObservedAt,
		PhaseHistory: []SignalPhase{{Phase: "awaiting_lower_level", At: parent.ObservedAt}},
		RuleVersion:  RecursivePreviewVersion, Date: parent.Date, ObservedAt: parent.ObservedAt,
		StructureRefs: append([]int(nil), parent.StructureRefs...),
		StructureIDs:  []string{parent.SignalID}, RelatedSignalID: parent.SignalID,
		Conditions: []string{"一类点之后次级别先完成反向走势", "次级别第一次回试端点严格不越过一类点失效边界", "本级恢复一类点方向的已确认线段"},
		Missing:    []string{"等待直接下一级别的已确认结构"}, Invalidation: parent.Invalidation,
		MarketFilter: parent.MarketFilter, LowerLevelStatus: "awaiting_lower_level",
		Detail: "等待一类点之后首次次级别回试及本级恢复线段确认",
	}
	if parent.Level <= 0 {
		return base
	}
	lower := findChanLevel(levels, parent.Level-1)
	if lower == nil || lower.Status == "awaiting_lower_level" || len(lower.Segments) == 0 {
		return base
	}
	base.LowerLevelStatus = lower.Status
	chain := signalChain(levels, parent.Level, parent.StructureRefs)
	// The class-one setup is not an actionable reference until its reversal
	// structure is confirmed. Lower-level motion that began before that point
	// cannot be treated as the post-signal impulse/retest sequence.
	parentConfirmedAt := parent.ConfirmedAt
	impulseIndex, retestIndex := -1, -1
	for index, segment := range lower.Segments {
		if segment.Chain != chain || segment.Start < parentConfirmedAt || segment.End <= parent.Date {
			continue
		}
		if impulseIndex < 0 {
			if segment.Confirmed && segment.Direction == impulseDir {
				impulseIndex = index
				continue
			}
			if !segment.Confirmed && segment.Direction == impulseDir {
				base.Missing = []string{"等待次级别反向走势确认"}
				return base
			}
			continue
		}
		if segment.Direction == retestDir {
			impulse := lower.Segments[impulseIndex]
			if segment.Start < impulse.End || segment.Confirmed && segment.ConfirmedAt <= impulse.ConfirmedAt {
				continue
			}
			if !segment.Confirmed {
				base.Missing = []string{"等待次级别第一次回试确认"}
				return base
			}
			retestIndex = index
			break
		}
	}
	if impulseIndex < 0 {
		base.Missing = []string{"等待一类点之后次级别反向走势完成"}
		return base
	}
	impulse := lower.Segments[impulseIndex]
	base.StructureIDs = append(base.StructureIDs, chanSegmentID(lower.Level, impulse))
	base.Missing = []string{"等待该次级别反向走势之后第一次已确认回试"}
	if retestIndex < 0 {
		return base
	}
	retest := lower.Segments[retestIndex]
	base.StructureIDs = append(base.StructureIDs, chanSegmentID(lower.Level, retest))
	base.Date, base.ObservedAt, base.Price = retest.End, retest.ConfirmedAt, ptr(retest.EndPrice)
	base.Phase, base.PhaseAt = "first_retest", retest.ConfirmedAt
	base.PhaseHistory = []SignalPhase{{Phase: "first_retest", At: retest.ConfirmedAt}}
	valid := parent.Invalidation == nil || buy && retest.Low > *parent.Invalidation || !buy && retest.High < *parent.Invalidation
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
	base.Detail = "次级别第一次回试满足一类点失效边界，等待本级恢复线段确认"
	for _, recovery := range levelSegments(levels, parent.Level) {
		if !recovery.Confirmed || recovery.Chain != chain || recovery.Direction != recoveryDir || recovery.Start < retest.End || recovery.ConfirmedAt <= retest.ConfirmedAt || recovery.End <= retest.End {
			continue
		}
		base.Status, base.Phase = "confirmed", "class2_confirmed"
		base.PhaseAt, base.ConfirmedAt = recovery.ConfirmedAt, recovery.ConfirmedAt
		base.PhaseHistory = append(base.PhaseHistory, SignalPhase{Phase: base.Phase, At: recovery.ConfirmedAt})
		base.StructureIDs = append(base.StructureIDs, chanSegmentID(parent.Level, recovery))
		base.Missing = []string{}
		base.Detail = "次级别第一次回试有效，本级恢复线段已确认"
		break
	}
	if parent.Invalidation != nil {
		direction := "up"
		if buy {
			direction = "down"
		}
		if at := firstV4InvalidationAt(frame.Candles, base.ObservedAt, direction, *parent.Invalidation); at != "" {
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

func class3Signals(frame Frame, levels []LevelAnalysis) []V4Signal {
	result := make([]V4Signal, 0)
	for _, level := range levels {
		for _, center := range level.Centers {
			if center.Forming || center.ConfirmedAt == "" {
				continue
			}
			firstAfterCenter := centerOwnerMoveIndex(center) + 1
			if firstAfterCenter < 0 || firstAfterCenter >= len(level.Segments) {
				continue
			}
			departureIndex := -1
			for index := firstAfterCenter; index < len(level.Segments); index++ {
				segment := level.Segments[index]
				if segment.Chain != center.Chain || segment.End <= center.End || segment.ConfirmedAt != "" && segment.ConfirmedAt < center.ConfirmedAt {
					continue
				}
				if recursiveSegmentLeavesCenter(segment, &center) {
					departureIndex = index
					break
				}
			}
			if departureIndex < 0 {
				for index := firstAfterCenter; index < len(level.Segments); index++ {
					segment := level.Segments[index]
					if segment.Chain != center.Chain || segment.End <= center.End {
						continue
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
			if retest.Chain != center.Chain || retest.Direction == departure.Direction || retest.Start != departure.End || retest.StartPrice != departure.EndPrice {
				continue
			}
			buy := departure.Direction == "up" && retest.Direction == "down" && retest.Low > center.Upper
			sell := departure.Direction == "down" && retest.Direction == "up" && retest.High < center.Lower
			if !retest.Confirmed {
				waiting := class3WaitingSignal(level, center, departure, departureIndex, &retest, frame.AsOf)
				if retest.Low <= center.Upper && retest.High >= center.Lower {
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
			if !buy && !sell {
				kind, side, invalidation := "class3_sell", "sell", center.Lower
				if departure.Direction == "up" {
					kind, side, invalidation = "class3_buy", "buy", center.Upper
				}
				at := retest.ConfirmedAt
				result = append(result, V4Signal{
					SignalID: classSignalID(level.Level, kind, departure.End, center.CenterID), Level: level.Level, Kind: kind, Side: side,
					Status: "invalidated", Phase: "invalidated", PhaseAt: at, PhaseHistory: []SignalPhase{{Phase: "invalidated", At: at}}, RuleVersion: RecursivePreviewVersion,
					Date: retest.End, ObservedAt: at, StructureRefs: []int{departureIndex, departureIndex + 1},
					StructureIDs: []string{center.CenterID, chanSegmentID(level.Level, departure), chanSegmentID(level.Level, retest)},
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
				SignalID: classSignalID(level.Level, kind, date, center.CenterID), Level: level.Level, Kind: kind, Side: side,
				Status: status, Phase: phase, PhaseAt: phaseAt, PhaseHistory: phaseHistory, RuleVersion: RecursivePreviewVersion,
				Date: date, ObservedAt: observedAt, ConfirmedAt: observedAt, StructureRefs: []int{departureIndex, departureIndex + 1},
				StructureIDs: []string{center.CenterID, chanSegmentID(level.Level, departure), chanSegmentID(level.Level, retest)},
				Conditions:   conditions, Missing: []string{}, Invalidation: ptr(invalidation), Price: ptr(retest.EndPrice),
				MarketFilter: "unfiltered", LowerLevelStatus: "unconfirmed", RelatedCenterID: center.CenterID,
				Detail: fmt.Sprintf("中枢首次回试严格留在外侧；失效边界 %.4f；市场过滤状态未提供", invalidation),
			})
		}
	}
	return result
}

// recursiveSegmentLeavesCenter requires a candidate departure to begin from
// an overlapping swing and finish beyond the center boundary. A later move
// that starts wholly outside the center is continuation, not a fresh third
// point departure from this center.
func recursiveSegmentLeavesCenter(segment Segment, center *RecursiveCenter) bool {
	if center == nil || !segmentMovesAway(segment, center) {
		return false
	}
	return segment.High >= center.Lower && segment.Low <= center.Upper
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
	ids := []string{center.CenterID, chanSegmentID(level.Level, departure)}
	missing := []string{"等待中枢离开后的首次反方向线段"}
	if retest != nil {
		date, price = retest.End, retest.EndPrice
		refs = append(refs, departureIndex+1)
		ids = append(ids, chanSegmentID(level.Level, *retest))
		missing = []string{"等待首次回试线段确认"}
		if !retest.Confirmed && asOf != "" {
			observedAt = asOf
		}
	} else if !departure.Confirmed {
		// A forming departure has no confirmed exit endpoint yet. Point its
		// provisional marker at the currently visible boundary and keep the
		// reason in the missing-condition text.
		date = departure.End
	}
	return V4Signal{
		SignalID: classSignalID(level.Level, kind, date, center.CenterID), Level: level.Level, Kind: kind, Side: side,
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
		if buy && bar.Low <= boundary || !buy && bar.High >= boundary {
			return bar.Date
		}
	}
	return ""
}

func findChanLevel(levels []LevelAnalysis, level int) *LevelAnalysis {
	for index := range levels {
		if levels[index].Level == level {
			return &levels[index]
		}
	}
	return nil
}

func levelSegments(levels []LevelAnalysis, level int) []Segment {
	if item := findChanLevel(levels, level); item != nil {
		return item.Segments
	}
	return nil
}

func chanSegmentID(level int, segment Segment) string {
	return fmt.Sprintf("%s|segment|L%d|C%d|%s|%s|%s", RecursivePreviewVersion, level, segment.Chain, segment.Direction, segment.Start, segment.End)
}

func classSignalID(level int, kind, date, parent string) string {
	return fmt.Sprintf("%s|L%d|%s|%s|%s", RecursivePreviewVersion, level, kind, date, parent)
}

func signalChain(levels []LevelAnalysis, level int, refs []int) int {
	if item := findChanLevel(levels, level); item != nil && len(refs) > 0 && refs[0] >= 0 && refs[0] < len(item.Segments) {
		return item.Segments[refs[0]].Chain
	}
	return -1
}

func firstClass1Chain(levels []LevelAnalysis, parent V4Signal) int {
	return signalChain(levels, parent.Level, parent.StructureRefs)
}
