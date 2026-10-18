package chananalysis

import (
	"fmt"
	"time"
)

// IncrementalAnalyzer retains the adjusted daily input and calculation state
// across append-only updates. It incrementally updates merged candles and
// MACD, reevaluates only fractal candidates whose four-candle confirmation
// window can have changed, then rebuilds structure from those cached inputs.
// Structural builders are deliberately rerun because a new confirmed segment
// can change recursive pivots and centers at multiple levels.
type IncrementalAnalyzer struct {
	symbol               string
	bars                 []Bar
	merged               []mergedBar
	fractals             []Fractal
	macd                 []MACDPoint
	lastResult           Result
	updates              uint64
	componentStateReuses uint64
	resultCacheHits      uint64
	mergeDirection       int
	fastEMA              float64
	slowEMA              float64
	signalEMA            float64
	macdSeeded           bool
}

type incrementalComponents struct {
	merged   []mergedBar
	fractals []Fractal
	macd     []MACDPoint
}

// IncrementalMetrics describes the work and retained history at the last
// update. A cache hit means the previous append-only state was reused; a
// non-append update is rejected and requires creating a fresh analyzer.
type IncrementalMetrics struct {
	InputBars            int    `json:"input_bars"`
	MergedBars           int    `json:"merged_bars"`
	Fractals             int    `json:"fractals"`
	MACDPoints           int    `json:"macd_points"`
	IncrementalUpdates   uint64 `json:"incremental_updates"`
	ComponentStateReuses uint64 `json:"component_state_reuses"`
	ResultCacheHits      uint64 `json:"result_cache_hits"`
}

func NewIncrementalAnalyzer(symbol string) *IncrementalAnalyzer {
	return &IncrementalAnalyzer{symbol: symbol, merged: []mergedBar{}, fractals: []Fractal{}, macd: []MACDPoint{}}
}

// Append accepts exactly one new completed adjusted daily bar. Callers must
// reset the analyzer when history is revised, the rule version changes, or a
// bar is inserted before the current end.
func (a *IncrementalAnalyzer) Append(input Bar, cutoff time.Time) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("incremental analyzer is nil")
	}
	if len(a.bars) > 0 && input.Date <= a.bars[len(a.bars)-1].Date {
		return Result{}, fmt.Errorf("incremental append must be strictly newer than %s", a.bars[len(a.bars)-1].Date)
	}
	if !isChronologicalUnique([]Bar{input}) {
		return Result{}, fmt.Errorf("incremental bar date is required")
	}
	validated, rejected, incomplete, duplicates, reordered, _ := validatedAdjustedBars([]Bar{input}, cutoff)
	if rejected > 0 || incomplete > 0 || duplicates > 0 || reordered > 0 || len(validated) != 1 {
		return Result{}, fmt.Errorf("incremental bar %s failed completed adjusted OHLC validation (rejected=%d incomplete=%d duplicates=%d reordered=%d accepted=%d)", input.Date, rejected, incomplete, duplicates, reordered, len(validated))
	}
	bar := validated[0]
	// The one-bar validation pass assigns source index zero. Preserve an index
	// supplied by replay after filtering a larger input; otherwise use the
	// accepted append sequence coordinate used by direct analyses.
	if input.HasSourceIndex {
		bar.SourceIndex = input.SourceIndex
		bar.HasSourceIndex = true
	} else {
		bar.SourceIndex = len(a.bars)
		bar.HasSourceIndex = true
	}
	if len(a.bars) > 0 {
		previous := a.bars[len(a.bars)-1]
		if !adjacentTradingSessions(previous.Date, bar.Date) || previous.Close > 0 && (bar.Close/previous.Close-1 > 0.20 || bar.Close/previous.Close-1 < -0.20) {
			bar.Chain = previous.Chain + 1
		} else {
			bar.Chain = previous.Chain
		}
	}
	a.bars = append(a.bars, bar)
	mergedBarValue := mergedBar{high: bar.High, low: bar.Low, highIndex: len(a.bars) - 1, lowIndex: len(a.bars) - 1, start: len(a.bars) - 1, end: len(a.bars) - 1, sourceHigh: bar.SourceIndex, sourceLow: bar.SourceIndex, sourceStart: bar.SourceIndex, sourceEnd: bar.SourceIndex, chain: bar.Chain}
	if len(a.merged) > 0 && a.merged[len(a.merged)-1].chain == bar.Chain {
		last := &a.merged[len(a.merged)-1]
		contains := (last.high >= mergedBarValue.high && last.low <= mergedBarValue.low) || (mergedBarValue.high >= last.high && mergedBarValue.low <= last.low)
		if contains {
			if a.mergeDirection == 0 {
				if mergedBarValue.high >= last.high {
					last.high, last.highIndex, last.sourceHigh = mergedBarValue.high, mergedBarValue.highIndex, mergedBarValue.sourceHigh
				}
				if mergedBarValue.low <= last.low {
					last.low, last.lowIndex, last.sourceLow = mergedBarValue.low, mergedBarValue.lowIndex, mergedBarValue.sourceLow
				}
			} else {
				if a.mergeDirection >= 0 && mergedBarValue.high >= last.high || a.mergeDirection < 0 && mergedBarValue.high <= last.high {
					last.high, last.highIndex, last.sourceHigh = mergedBarValue.high, mergedBarValue.highIndex, mergedBarValue.sourceHigh
				}
				if a.mergeDirection > 0 && mergedBarValue.low >= last.low || a.mergeDirection <= 0 && mergedBarValue.low <= last.low {
					last.low, last.lowIndex, last.sourceLow = mergedBarValue.low, mergedBarValue.lowIndex, mergedBarValue.sourceLow
				}
			}
			last.end, last.sourceEnd = mergedBarValue.end, mergedBarValue.sourceEnd
		} else {
			if a.mergeDirection == 0 {
				a.mergeDirection = directionFromPair(last.high, last.low, mergedBarValue.high, mergedBarValue.low)
			}
			a.merged = append(a.merged, mergedBarValue)
		}
	} else {
		a.mergeDirection = 0
		a.merged = append(a.merged, mergedBarValue)
	}
	a.appendMACD(bar)
	// A candidate fractal at merged index i can only become final once i+2 is
	// present; remove/recompute the last three candidates to account for
	// containment extending the current merged candle.
	cut := len(a.fractals)
	for cut > 0 && a.fractals[cut-1].MergedIndex >= len(a.merged)-4 {
		cut--
	}
	a.fractals = a.fractals[:cut]
	start := maxInt(1, len(a.merged)-4)
	for i := start; i+2 < len(a.merged); i++ {
		left, middle, right, final := a.merged[i-1], a.merged[i], a.merged[i+1], a.merged[i+2]
		if left.chain != middle.chain || middle.chain != right.chain || right.chain != final.chain {
			continue
		}
		kind, index, price := "", middle.highIndex, middle.high
		if middle.high > left.high && middle.high > right.high && middle.low > left.low && middle.low > right.low {
			kind = "top"
		} else if middle.low < left.low && middle.low < right.low && middle.high < left.high && middle.high < right.high {
			kind, index, price = "bottom", middle.lowIndex, middle.low
		} else {
			continue
		}
		confirmedAt := a.bars[final.start].Date
		a.fractals = append(a.fractals, Fractal{Kind: kind, Date: a.bars[index].Date, Price: price, Level: 0, FirstObservedAt: confirmedAt, ConfirmedAt: confirmedAt, MergedIndex: i, RawIndex: index, RawStartIndex: middle.start, RawEndIndex: middle.end, SourceRawIndex: sourceIndexFor(a.bars, index), SourceRawStart: middle.sourceStart, SourceRawEnd: middle.sourceEnd, Chain: middle.chain})
	}
	a.updates++
	return a.result(cutoff), nil
}

func (a *IncrementalAnalyzer) appendMACD(bar Bar) {
	if !a.macdSeeded {
		a.fastEMA, a.slowEMA, a.signalEMA = bar.Close, bar.Close, 0
		a.macdSeeded = true
	}
	a.fastEMA += 2.0 / 13 * (bar.Close - a.fastEMA)
	a.slowEMA += 2.0 / 27 * (bar.Close - a.slowEMA)
	dif := a.fastEMA - a.slowEMA
	a.signalEMA += 2.0 / 10 * (dif - a.signalEMA)
	a.macd = append(a.macd, MACDPoint{Date: bar.Date, DIF: dif, DEA: a.signalEMA, Histogram: 2 * (dif - a.signalEMA), Warmup: len(a.macd) < macdWarmupBars, Version: macdVersion})
}

func (a *IncrementalAnalyzer) result(cutoff time.Time) Result {
	components := &incrementalComponents{merged: a.merged, fractals: a.fractals, macd: a.macd}
	componentsReusable := len(a.merged) > 0 && len(a.macd) == len(a.bars)
	result := analyzeAdjustedPrevalidatedWithComponents(a.symbol, a.bars, nil, cutoff, recursiveDivergenceRatio, components)
	a.lastResult = result
	if componentsReusable {
		a.componentStateReuses++
	}
	return result
}

func histogramFromMACD(points []MACDPoint) []float64 {
	out := make([]float64, len(points))
	for i, p := range points {
		out[i] = p.Histogram
	}
	return out
}

func (a *IncrementalAnalyzer) Metrics() IncrementalMetrics {
	return IncrementalMetrics{InputBars: len(a.bars), MergedBars: len(a.merged), Fractals: len(a.fractals), MACDPoints: len(a.macd), IncrementalUpdates: a.updates, ComponentStateReuses: a.componentStateReuses, ResultCacheHits: a.resultCacheHits}
}

// ReuseLastResult returns the previous snapshot when an input prefix adds no
// accepted bars. This is a result-cache hit, unlike component state reuse.
func (a *IncrementalAnalyzer) ReuseLastResult() (Result, bool) {
	if a == nil || a.updates == 0 {
		return Result{}, false
	}
	a.resultCacheHits++
	return a.lastResult, true
}
