package chananalysis

// 早期风险只在局部假设中使用三根K线端点。严格分型、笔、六类点完全不消费它。
func buildEarlyRiskWarnings(frame Frame, bars []Bar, merged []mergedBar, fractals []Fractal, macd []MACDPoint, threshold float64) []V4Signal {
	result := []V4Signal{}
	histogram := histogramFromMACD(macd)
	for index := 1; index+1 < len(merged); index++ {
		left, middle, right := merged[index-1], merged[index], merged[index+1]
		if left.chain != middle.chain || middle.chain != right.chain {
			continue
		}
		// 右邻的首根非包含K线已经固定左邻和中间K线。后续包含不能提前进入观察条件。
		observedIndex := right.start
		observed := bars[observedIndex]
		if !priceGreater(middle.high, left.high) || !priceGreater(middle.low, left.low) || !priceGreater(middle.high, observed.High) || !priceGreater(middle.low, observed.Low) {
			continue
		}
		prefix := bars[:observedIndex+1]
		visible := make([]Fractal, 0, len(fractals)+1)
		for _, fractal := range fractals {
			if fractal.ConfirmedAt <= observed.Date && fractal.MergedIndex < index {
				visible = append(visible, fractal)
			}
		}
		// 严格前缀用于验证中枢已形成，假设反向笔不能提前形成新中枢。
		strictStrokes, _ := buildStrokesWithHistogram(visible, merged[:index+2], histogram[:observedIndex+1])
		fixedCenters := buildCenters(strictStrokes)
		if len(fixedCenters) == 0 {
			continue
		}
		candidate := Fractal{Kind: "top", Date: bars[middle.end].Date, Price: middle.high, Level: 0,
			FirstObservedAt: observed.Date, ConfirmedAt: observed.Date, MergedIndex: index, RawIndex: middle.highIndex,
			RawStartIndex: middle.start, RawEndIndex: middle.end, SourceRawIndex: middle.sourceHigh,
			SourceRawStart: middle.sourceStart, SourceRawEnd: middle.sourceEnd, Chain: middle.chain}
		projected := Frame{Timeframe: frame.Timeframe, AsOf: observed.Date, Candles: prefix, MACD: macd[:observedIndex+1]}
		projected.StrokesV4, _ = buildStrokesWithHistogram(append(visible, candidate), merged[:index+2], histogram[:observedIndex+1])
		levels := buildSignalScanLevels(projected, nil)
		strengths := buildStrengthsFromMACD(prefix, levels, projected.MACD)
		warnings := buildStructureWarnings(projected, levels, strengths, threshold)
		// 进入/离开中枢的盘整背驰也可提前观察，不能只覆盖局部三段背驰。
		for _, signal := range buildV4SignalsWithDivergenceRatio(projected, levels, strengths, threshold) {
			if signal.Kind == "class1_sell" {
				warnings = append(warnings, signal)
			}
		}
		for _, warning := range warnings {
			if warning.Side != "sell" || warning.Date != candidate.Date || warning.Price == nil || !priceEqual(*warning.Price, candidate.Price) || warning.ObservedAt != observed.Date {
				continue
			}
			var fixed *Center
			for centerIndex := range fixedCenters {
				center := &fixedCenters[centerIndex]
				for _, projectedCenter := range levels[0].Centers {
					if projectedCenter.CenterID == warning.RelatedCenterID && center.Chain == projectedCenter.Chain && center.StartStroke == projectedCenter.StartSegment && center.FormedAt <= observed.Date && priceEqual(center.Lower, projectedCenter.Lower) && priceEqual(center.Upper, projectedCenter.Upper) {
						fixed = center
					}
				}
			}
			if fixed == nil {
				continue
			}
			warning.Kind = "early_top_sell"
			warning.Detail = "三根已收盘标准化K线出现顶部形态，同向推进价格创新高而MACD面积衰减；右邻仍可包含变化，先提示减仓风险，不提前确认分型或一卖"
			if warning.StrengthRatio == nil {
				warning.Kind = "early_rebound_sell"
				warning.Detail = "三根已收盘标准化K线出现顶部形态，反弹仍低于本轮跌破前高点且未收复中枢上沿；先提示反弹承压风险，不提前确认三卖"
			}
			warning.SignalID = classSignalID("pen", 0, warning.Kind, observed.Date, candidate.Date)
			warning.Date, warning.PivotDate = observed.Date, candidate.Date
			warning.Status, warning.Phase, warning.PhaseAt = "observing", "observation", observed.Date
			warning.ConfirmedAt = ""
			warning.ProtectionPrice = ptr(observed.Low)
			warning.PhaseHistory = []SignalPhase{{Phase: "observation", At: observed.Date}}
			warning.Conditions = []string{"三根已收盘标准化K线的顶部形态，右邻尚未固定", "只使用观察时刻已可见分型和已形成中枢", "假设笔仅用于独立风险筛选，不进入严格结构"}
			warning.Missing = []string{"等待右邻固定及有效反向结构确认；跌破观察K线低点时先处理风险"}
			warning.LowerLevelStatus = "not_required"
			warning.TimeframeConfirmation = &TimeframeConfirmation{HigherTimeframe: frame.Timeframe, Status: "not_required", StructureIDs: []string{}, Reason: "提前顶部风险，不用于确认严格六类买卖点"}
			// 收盘失守只能强化风险，不能把假设端点升级为严格确认；断链和创新高终止该风险。
			triggered := false
			for _, later := range bars[observedIndex+1:] {
				if later.Chain != observed.Chain || priceGreaterOrEqual(later.High, candidate.Price) {
					warning.Status, warning.Phase, warning.PhaseAt = "invalidated", "invalidated", later.Date
					warning.PhaseHistory = append(warning.PhaseHistory, SignalPhase{Phase: "invalidated", At: later.Date})
					warning.Missing = []string{}
					break
				}
				if !triggered && priceLess(later.Close, observed.Low) {
					triggered = true
					warning.Status, warning.Phase, warning.PhaseAt = "risk_triggered", "risk_triggered", later.Date
					warning.PhaseHistory = append(warning.PhaseHistory, SignalPhase{Phase: "risk_triggered", At: later.Date})
				}
			}
			result = append(result, warning)
			break // 同一个三根顶部形态只列一条风险，避免局部与中枢进入/离开比较重复。
		}
	}
	return result
}
