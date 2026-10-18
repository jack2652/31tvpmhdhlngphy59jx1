package chananalysis

import "fmt"

// turningSignals applies a pen-level reversal heuristic, not the recursive
// trend-divergence theorem. A mutable last pen can only produce a watch; its
// MACD area and endpoint may still change. Every date describes when that
// stage became observable, rather than backdating it to the price extreme.
func turningSignals(strokes []Stroke, centers []Center, bars []Bar) []Signal {
	signals := []Signal{}
	if len(bars) < 60 || len(strokes) < 3 {
		return signals
	}
	seen := map[string]bool{}
	// Keep only recent setups: the divergent pen, its reversal and first
	// retest. An old divergence must not remain a perpetual trade candidate.
	for index := len(strokes) - 1; index >= max(2, len(strokes)-4); index-- {
		current, previous := strokes[index], strokes[index-2]
		side := "sell"
		if current.Direction == "down" {
			side = "buy"
		}
		if seen[side] || !previous.Confirmed || previous.Direction != current.Direction ||
			previous.Chain != current.Chain ||
			previous.Start.RawIndex < 35 || previous.MACDArea <= 1e-9 || current.MACDArea <= 1e-9 {
			continue
		}
		observedAt := current.End.ConfirmedAt
		// The 60-bar history requirement must also have been satisfied on
		// the observation date, not only by the end of today's input.
		if observedAt < bars[59].Date {
			continue
		}
		var shared *Center
		for i := range centers {
			center := &centers[i]
			// Both pens must touch the same center, which must already have
			// been known when this observation could first be made.
			if center.StartStroke <= index-2 && center.ConfirmedAt <= observedAt &&
				previous.High >= center.Lower && previous.Low <= center.Upper &&
				current.High >= center.Lower && current.Low <= center.Upper {
				shared = center
			}
		}
		if shared == nil {
			continue
		}
		extended := side == "buy" && current.End.Price < previous.End.Price && current.End.Price < shared.Lower ||
			side == "sell" && current.End.Price > previous.End.Price && current.End.Price > shared.Upper
		ratio := current.MACDArea / previous.MACDArea
		if !extended || ratio >= 0.8 {
			continue
		}
		extreme := "创新高"
		if side == "buy" {
			extreme = "创新低"
		}
		signal := Signal{Side: side, Status: "watch", Phase: "weakening",
			Date: current.End.Date, OriginDate: current.End.Date, ObservedAt: observedAt, ConfirmedAt: observedAt, PhaseAt: observedAt,
			Price: ptr(current.End.Price), Invalidation: ptr(current.End.Price), StrengthRatio: ptr(ratio),
			Detail: fmt.Sprintf("与 %s 结束的同方向波段相比，价格%s，但 MACD 同方向柱面积只有上次的 %.0f%%，提示价格推动减弱；两段走势都经过同一个已形成的整理重叠区（笔中枢）",
				previous.End.Date, extreme, ratio*100)}
		if side == "buy" {
			signal.Kind, signal.Label = "bottom_reversal_candidate", "底部力度衰减观察"
			signal.Confirmation = "这段下跌还在形成，等待随后出现符合识别规则的一段反弹来确认低点；在此之前，低点和 MACD 柱面积还会更新"
			signal.NextStep = fmt.Sprintf("低点确认后，观察反弹后的第一次回落能否停在 %.2f 上方，再看收盘能否涨过反弹高点", current.End.Price)
		} else {
			signal.Kind, signal.Label = "top_exit_candidate", "顶部力度衰减观察"
			signal.Confirmation = "这段上涨还在形成，等待随后出现符合识别规则的一段回落来确认高点；在此之前，高点和 MACD 柱面积还会更新"
			signal.NextStep = fmt.Sprintf("高点确认后，观察下跌后的第一次反弹能否涨回 %.2f，再看收盘是否跌破回落低点", current.End.Price)
		}
		if current.Confirmed && index+1 < len(strokes) && strokes[index+1].Chain == current.Chain {
			reversal := strokes[index+1]
			signal.Status, signal.Phase = "candidate", "reversal"
			signal.ConfirmedAt, signal.PhaseAt = current.ConfirmedAt, current.ConfirmedAt
			signal.TriggerPrice = ptr(reversal.End.Price)
			signal.TriggerConfirmed = reversal.Confirmed
			if side == "buy" {
				signal.Label = "抄底候选 · 低点已固定"
				signal.Confirmation = fmt.Sprintf("下跌后已出现一段有效反弹，反弹高点在 %.2f；继续观察反弹后的第一次回落，最低价需高于原低点 %.2f", reversal.End.Price, current.End.Price)
				signal.NextStep = fmt.Sprintf("等待第一次回落守住原低点，并观察收盘能否涨过 %.2f；跌至 %.2f 时，本次抄底候选失效", reversal.End.Price, current.End.Price)
			} else {
				signal.Label = "逃顶候选 · 高点已固定"
				signal.Confirmation = fmt.Sprintf("上涨后已出现一段有效回落，回落低点在 %.2f；继续观察下跌后的第一次反弹，最高价需低于原高点 %.2f", reversal.End.Price, current.End.Price)
				signal.NextStep = fmt.Sprintf("等待第一次反弹结束并形成比原高点更低的高点，再看收盘是否跌破 %.2f；涨至 %.2f 时，本次逃顶候选失效", reversal.End.Price, current.End.Price)
			}
			if !reversal.Confirmed {
				signal.Confirmation += "；这段反向走势还在形成，所列观察价仍会更新"
			} else {
				signal.PhaseAt = reversal.ConfirmedAt
			}
			if reversal.Confirmed && index+2 < len(strokes) && strokes[index+2].Chain == current.Chain {
				retest := strokes[index+2]
				held := side == "buy" && retest.Low > current.End.Price || side == "sell" && retest.High < current.End.Price
				if retest.Confirmed && retest.Direction == current.Direction && held {
					signal.Phase, signal.PhaseAt = "retest", retest.ConfirmedAt
					signal.RetestPrice = ptr(retest.End.Price)
					if side == "buy" {
						signal.Label = "抄底候选 · 首次回踩守住低点"
						signal.Confirmation = fmt.Sprintf("反弹后的第一次回落已由后续走势确认，最低价 %.2f 高于原低点 %.2f；继续观察收盘能否涨过反弹高点 %.2f", retest.End.Price, current.End.Price, reversal.End.Price)
						signal.NextStep = fmt.Sprintf("收盘涨过 %.2f 后观察上涨能否持续；再次跌至本次回落低点 %.2f 时，低点抬高的改善受到影响；原候选的失效价仍为 %.2f", reversal.End.Price, retest.End.Price, current.End.Price)
					} else {
						signal.Label = "逃顶候选 · 首次反抽未过高点"
						signal.Confirmation = fmt.Sprintf("下跌后的第一次反弹已由后续走势确认，最高价 %.2f 低于原高点 %.2f；继续观察收盘是否跌破回落低点 %.2f", retest.End.Price, current.End.Price, reversal.End.Price)
						signal.NextStep = fmt.Sprintf("收盘跌破 %.2f 后观察下跌是否持续；再次涨至本次反弹高点 %.2f 时，高点降低的判断受到影响；原候选的失效价仍为 %.2f", reversal.End.Price, retest.End.Price, current.End.Price)
					}
				}
			}
		}
		seen[side] = true
		signals = append(signals, signal)
	}
	return signals
}
