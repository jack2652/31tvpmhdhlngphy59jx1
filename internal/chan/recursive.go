package chananalysis

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	macdVersion    = "ema-first-close-12-26-9-v1"
	macdWarmupBars = 35
	// recursiveDivergenceRatio is a project screening parameter, not a
	// canonical Chan constant. It is emitted in RecursiveParameters so every
	// result identifies the threshold used to create its divergence signals.
	recursiveDivergenceRatio = 0.8
)

type MACDPoint struct {
	Date      string  `json:"date"`
	DIF       float64 `json:"dif"`
	DEA       float64 `json:"dea"`
	Histogram float64 `json:"histogram"`
	Warmup    bool    `json:"warmup"`
	Version   string  `json:"version"`
}

// macdSeries uses the v4 frozen initialization: first close seeds both price
// EMAs, DEA starts at zero, and warmup bars are exposed but excluded from
// signal strength comparisons.
func macdSeries(bars []Bar) []MACDPoint {
	points := make([]MACDPoint, len(bars))
	if len(bars) == 0 {
		return points
	}
	fast, slow, dea := bars[0].Close, bars[0].Close, 0.0
	for index, bar := range bars {
		fast += 2.0 / 13 * (bar.Close - fast)
		slow += 2.0 / 27 * (bar.Close - slow)
		dif := fast - slow
		dea += 2.0 / 10 * (dif - dea)
		points[index] = MACDPoint{Date: bar.Date, DIF: dif, DEA: dea, Histogram: 2 * (dif - dea), Warmup: index < macdWarmupBars, Version: macdVersion}
	}
	return points
}

func mergedCandleOutput(merged []mergedBar) []MergedCandle {
	result := make([]MergedCandle, 0, len(merged))
	for _, candle := range merged {
		result = append(result, MergedCandle{High: candle.high, Low: candle.low, HighRawBar: candle.sourceHigh, LowRawBar: candle.sourceLow, RawStart: candle.start, RawEnd: candle.end, SourceRawStart: candle.sourceStart, SourceRawEnd: candle.sourceEnd, Chain: candle.chain})
	}
	return result
}

func buildRecursiveLevels(baseStrokes []Stroke, baseSegments []Segment) []LevelAnalysis {
	levels := []LevelAnalysis{}
	baseCenters := buildRecursiveCenters(baseSegments)
	for index := range baseCenters {
		baseCenters[index].OwnerMoveIndex = baseCenters[index].EndSegment
	}
	status := "ready"
	if len(baseSegments) == 0 {
		status = "insufficient"
	}
	forming := hasFormingSegment(baseSegments) || hasFormingStroke(baseStrokes)
	if forming && status == "ready" {
		status = "forming"
	}
	levelZeroWaiting := ""
	if forming {
		levelZeroWaiting = "末端存在形成中笔或线段；高一级只消费已确认对象"
	}
	levels = append(levels, LevelAnalysis{Level: 0, Status: status, InputCount: len(baseStrokes), Strokes: confirmedAndForming(baseStrokes), Segments: baseSegments, Centers: baseCenters, WaitingFor: levelZeroWaiting})
	lower := baseSegments
	for level := 1; level < 8; level++ {
		trends, centers, inputCount := recursiveLevelUp(lower, level)
		if len(lower) > 0 && len(trends) >= len(lower) {
			// 未减少下级单元时没有形成更高层级，避免断链各自退化为同一条走势。
			break
		}
		segments := recursiveTrendSegments(trends)
		levelStatus, waiting := "ready", ""
		closedCenters := 0
		for _, center := range centers {
			if !center.Forming && center.ConfirmedAt != "" {
				closedCenters++
			}
		}
		switch {
		case inputCount == 0:
			levelStatus, waiting = "awaiting_lower_level", "等待下级产生连续的已确认走势单元"
		case len(trends) == 0:
			levelStatus, waiting = "awaiting_lower_level", "等待三个连续下级走势单元形成重叠中枢"
		case closedCenters == 0:
			levelStatus, waiting = "developing", "高一级走势正在形成；中枢离开或后续中枢尚未确认"
		}
		levels = append(levels, LevelAnalysis{Level: level, Status: levelStatus, InputCount: inputCount, Strokes: []Stroke{}, Segments: segments, Trends: trends, Centers: centers, WaitingFor: waiting})
		if len(trends) < 3 {
			break
		}
		lower = segments
	}
	return levels
}

type recursiveCenterSpan struct {
	start, end, through int
	zd, zg              float64
	closedAt            string
}

type recursiveMoveResult struct {
	trend    RecursiveTrend
	centers  []recursiveCenterSpan
	closedAt string
}

// recursiveLevelUp ports chan-lun's level_up decomposition: the next level
// consumes confirmed lower-level moves and identifies its center from the
// earliest three-unit overlap. It does not synthesize market fractals at
// arithmetic midpoints or rerun the lower-level feature-sequence segmenter.
func recursiveLevelUp(lower []Segment, level int) ([]RecursiveTrend, []RecursiveCenter, int) {
	trends := []RecursiveTrend{}
	centers := []RecursiveCenter{}
	inputCount := 0
	for chainStart := 0; chainStart < len(lower); {
		chainEnd := chainStart
		for chainEnd+1 < len(lower) && lower[chainEnd+1].Chain == lower[chainStart].Chain && lower[chainEnd].End == lower[chainEnd+1].Start {
			chainEnd++
		}
		// Keep the visible forming tail in the recursive input. The reference
		// level_up decomposes the current prefix, including its unfinished tail;
		// dropping it hides higher-level structures until the next lower segment
		// is already confirmed. Confirmation remains separate, so forming input
		// can only produce forming structures and cannot validate signals.
		units := lower[chainStart : chainEnd+1]
		inputCount += len(units)
		moves := decomposeRecursiveUnits(units, level)
		for _, move := range moves {
			move.trend.Chain = lower[chainStart].Chain
			move.trend.StartInput += chainStart
			move.trend.EndInput += chainStart
			moveIndex := len(trends)
			for _, span := range move.centers {
				if span.closedAt == "" && move.trend.Confirmed {
					span.closedAt = move.closedAt
				}
				centerStart := units[span.start].Start
				centerEnd := units[span.through].End
				closedAt := span.closedAt
				if !recursiveUnitsConfirmed(units[span.start : span.through+1]) {
					closedAt = ""
				}
				center := RecursiveCenter{Level: level, Kind: "initial", InputKind: "segment", Chain: move.trend.Chain,
					Start: centerStart, End: centerEnd, Lower: span.zd, Upper: span.zg,
					FormedAt: confirmedUnitTime(units[span.end]), StartSegment: span.start + chainStart, EndSegment: span.through + chainStart,
					OwnerMoveIndex: moveIndex, ExtensionCount: maxInt(0, span.through-span.end), Forming: closedAt == ""}
				center.CenterID = fmt.Sprintf("L%d-C%d-S%d-E%d-M%d", level, move.trend.Chain, span.start+chainStart, span.through+chainStart, moveIndex)
				if level > 1 {
					center.InputKind = "recursive_trend"
				}
				if closedAt != "" {
					center.Forming = false
					center.ClosedAt = closedAt
					center.ConfirmedAt = closedAt
				}
				for previous := len(centers) - 1; previous >= 0; previous-- {
					if centers[previous].Chain == center.Chain && centers[previous].Level == center.Level {
						previousIndex := previous
						center.RelatedCenter = &previousIndex
						center.PreviousCenterID = centers[previous].CenterID
						switch {
						case center.Lower < centers[previous].Upper && center.Upper > centers[previous].Lower:
							center.RelationToPrevious, center.Relation, center.Kind = "overlap", "expands", "expansion"
						case center.Lower == centers[previous].Upper || center.Upper == centers[previous].Lower:
							center.RelationToPrevious, center.Relation, center.Kind = "touch", "new_after", "newborn"
						case center.Lower > centers[previous].Upper:
							center.RelationToPrevious, center.Relation, center.Kind = "separate_up", "new_after", "newborn"
						case center.Upper < centers[previous].Lower:
							center.RelationToPrevious, center.Relation, center.Kind = "separate_down", "new_after", "newborn"
						default:
							center.Relation, center.Kind = "new_after", "newborn"
						}
						break
					}
				}
				centers = append(centers, center)
				move.trend.CenterIDs = append(move.trend.CenterIDs, center.CenterID)
			}
			trends = append(trends, move.trend)
		}
		chainStart = chainEnd + 1
	}
	return trends, centers, inputCount
}

// decomposeRecursiveUnits follows the reference level-up state machine for one
// contiguous chain. Every center is an intersection of three consecutive
// lower-level units; extensions update its right edge without changing ZD/ZG.
func decomposeRecursiveUnits(units []Segment, level int) []recursiveMoveResult {
	result := []recursiveMoveResult{}
	for position := 0; position < len(units); {
		centerStart := -1
		for index := position; index+2 < len(units); index++ {
			zd := math.Max(units[index].Low, math.Max(units[index+1].Low, units[index+2].Low))
			zg := math.Min(units[index].High, math.Min(units[index+1].High, units[index+2].High))
			if zd <= zg {
				centerStart = index
				break
			}
		}
		if centerStart < 0 {
			result = append(result, makeRecursiveMove(units, position, len(units)-1, nil, level, "incomplete", "", false, ""))
			break
		}
		entryDirection := recursiveUnitDirection(units, position, centerStart)
		zd := math.Max(units[centerStart].Low, math.Max(units[centerStart+1].Low, units[centerStart+2].Low))
		zg := math.Min(units[centerStart].High, math.Min(units[centerStart+1].High, units[centerStart+2].High))
		spans := []recursiveCenterSpan{{start: centerStart, end: centerStart + 2, through: centerStart + 2, zd: zd, zg: zg}}
		kind, k, leaveStart := "consolidation", centerStart+3, -1
		for {
			if k >= len(units) {
				result = append(result, makeRecursiveMove(units, position, len(units)-1, spans, level, kind, entryDirection, false, ""))
				position = len(units)
				break
			}
			unit := units[k]
			center := spans[len(spans)-1]
			if recursiveUnitOverlaps(unit, center.zd, center.zg) {
				if leaveStart < 0 {
					spans[len(spans)-1].through = k
					k++
					continue
				}
				closedAt := confirmedUnitTime(unit)
				spans[len(spans)-1].closedAt = closedAt
				result = append(result, makeRecursiveMove(units, position, k-1, spans, level, kind, entryDirection, closedAt != "", closedAt))
				position = k
				break
			}
			if leaveStart < 0 {
				leaveStart = k
			} else if k-2 >= leaveStart {
				lower := math.Max(units[k-2].Low, math.Max(units[k-1].Low, units[k].Low))
				upper := math.Min(units[k-2].High, math.Min(units[k-1].High, units[k].High))
				if lower <= upper {
					newDirection := ""
					if lower > center.zg {
						newDirection = "up"
					} else if upper < center.zd {
						newDirection = "down"
					}
					if newDirection == "" || entryDirection != "" && entryDirection != newDirection {
						closedAt := confirmedUnitTime(unit)
						spans[len(spans)-1].closedAt = closedAt
						result = append(result, makeRecursiveMove(units, position, leaveStart-1, spans, level, kind, entryDirection, closedAt != "", closedAt))
						position = leaveStart
						break
					}
					if entryDirection == "" {
						entryDirection = newDirection
					}
					kind = newDirection
					spans[len(spans)-1].closedAt = confirmedUnitTime(units[k])
					spans = append(spans, recursiveCenterSpan{start: k - 2, end: k, through: k, zd: lower, zg: upper})
					leaveStart = -1
				}
			}
			k++
		}
	}
	return result
}

func makeRecursiveMove(units []Segment, start, end int, spans []recursiveCenterSpan, level int, kind, direction string, confirmed bool, confirmedAt string) recursiveMoveResult {
	if start < 0 || end < start || end >= len(units) {
		return recursiveMoveResult{}
	}
	first, last := units[start], units[end]
	confirmed = confirmed && recursiveUnitsConfirmed(units[start:end+1])
	if !confirmed {
		confirmedAt = ""
	}
	trend := RecursiveTrend{Level: level, Kind: kind, Direction: direction, Start: first.Start, End: last.End,
		StartPrice: first.StartPrice, EndPrice: last.EndPrice, Low: first.Low, High: first.High,
		Confirmed: confirmed, ConfirmedAt: confirmedAt, Forming: !confirmed, StartInput: start, EndInput: end,
		StartRawBar: first.StartRawBar, EndRawBar: last.EndRawBar,
		StartInputBar: first.StartInputBar, EndInputBar: last.EndInputBar, CenterIDs: []string{}}
	for index := start + 1; index <= end; index++ {
		trend.Low = math.Min(trend.Low, units[index].Low)
		trend.High = math.Max(trend.High, units[index].High)
	}
	return recursiveMoveResult{trend: trend, centers: append([]recursiveCenterSpan(nil), spans...), closedAt: confirmedAt}
}

func confirmedUnitTime(unit Segment) string {
	if !unit.Confirmed || unit.ConfirmedAt == "" {
		return ""
	}
	return unit.ConfirmedAt
}

func recursiveUnitsConfirmed(units []Segment) bool {
	if len(units) == 0 {
		return false
	}
	for _, unit := range units {
		if !unit.Confirmed || unit.ConfirmedAt == "" {
			return false
		}
	}
	return true
}

func recursiveUnitDirection(units []Segment, start, centerStart int) string {
	if start < centerStart && start >= 0 && start < len(units) && (units[start].Direction == "up" || units[start].Direction == "down") {
		return units[start].Direction
	}
	return ""
}

func recursiveUnitOverlaps(unit Segment, lower, upper float64) bool {
	return !(unit.High < lower || unit.Low > upper)
}

func recursiveTrendSegments(trends []RecursiveTrend) []Segment {
	segments := make([]Segment, 0, len(trends))
	for index, trend := range trends {
		direction := trend.Direction
		if direction == "" && (trend.Kind == "up" || trend.Kind == "down") {
			direction = trend.Kind
		}
		startKind, endKind := "", ""
		if direction == "up" {
			startKind, endKind = "bottom", "top"
		} else if direction == "down" {
			startKind, endKind = "top", "bottom"
		}
		segment := Segment{Direction: direction, TrendKind: trend.Kind, Chain: trend.Chain, Level: trend.Level, StartStroke: trend.StartInput, EndStroke: trend.EndInput, EndPointStroke: trend.EndInput,
			Start: trend.Start, End: trend.End, StartKind: startKind, EndKind: endKind, StartPrice: trend.StartPrice, EndPrice: trend.EndPrice,
			Low: trend.Low, High: trend.High, StartRawBar: trend.StartRawBar, EndRawBar: trend.EndRawBar,
			StartInputBar: trend.StartInputBar, EndInputBar: trend.EndInputBar,
			Confirmed: trend.Confirmed, ConfirmedAt: trend.ConfirmedAt, Forming: trend.Forming, BreakStroke: -1, ConfirmStroke: -1, Feature: []FeatureElement{}}
		if segment.StartRawBar == 0 && segment.EndRawBar == 0 {
			// Synthetic fixtures and old callers may not provide source offsets.
			segment.StartInputBar, segment.EndInputBar = index, index
		}
		segments = append(segments, segment)
	}
	return segments
}

func hasFormingSegment(segments []Segment) bool {
	for _, segment := range segments {
		if segment.Forming || !segment.Confirmed {
			return true
		}
	}
	return false
}

func hasFormingStroke(strokes []Stroke) bool {
	for _, stroke := range strokes {
		if !stroke.Confirmed {
			return true
		}
	}
	return false
}

func confirmedAndForming(strokes []Stroke) []Stroke {
	return append([]Stroke(nil), strokes...)
}

// A recursive pivot is the shared turning endpoint between the first and
// middle segment in a confirmed alternating triple. Consecutive confirmed
// pivots form the input strokes for the next recursive level.
func recursiveInputStrokes(segments []Segment, level int) []Stroke {
	pivots := []Fractal{}
	pivotAt := []int{}
	for index := 0; index+2 < len(segments); index++ {
		a, b, c := segments[index], segments[index+1], segments[index+2]
		if a.Chain != b.Chain || b.Chain != c.Chain || !a.Confirmed || !b.Confirmed || !c.Confirmed || a.Direction != c.Direction || a.Direction == b.Direction || a.End != b.Start || b.End != c.Start {
			continue
		}
		pivotDate := a.End
		pivotPrice := a.EndPrice
		if pivotDate == "" || !finitePositive(pivotPrice) {
			continue
		}
		kind := "bottom"
		if a.Direction == "up" {
			kind = "top"
		}
		confirmedAt := maxTimeString(a.ConfirmedAt, b.ConfirmedAt, c.ConfirmedAt)
		if confirmedAt == "" {
			continue
		}
		pivot := Fractal{Kind: kind, Date: pivotDate, Price: pivotPrice, Level: level, ConfirmedAt: confirmedAt, FirstObservedAt: confirmedAt, Chain: a.Chain}
		if len(pivots) == 0 || pivots[len(pivots)-1].Chain != pivot.Chain {
			pivots = append(pivots, pivot)
			pivotAt = append(pivotAt, index+1)
			continue
		}
		previous := pivots[len(pivots)-1]
		if previous.Kind == pivot.Kind {
			moreExtreme := pivot.Kind == "top" && pivot.Price > previous.Price || pivot.Kind == "bottom" && pivot.Price < previous.Price
			if moreExtreme {
				pivots[len(pivots)-1], pivotAt[len(pivotAt)-1] = pivot, index+1
			}
			continue
		}
		if pivot.Date > previous.Date {
			pivots = append(pivots, pivot)
			pivotAt = append(pivotAt, index+1)
		}
	}
	result := []Stroke{}
	for index := 1; index < len(pivots); index++ {
		start, end := pivots[index-1], pivots[index]
		if start.Chain != end.Chain || start.Kind == end.Kind || end.Price == start.Price {
			continue
		}
		direction := "up"
		if end.Price < start.Price {
			direction = "down"
		}
		first, last := pivotAt[index-1]-1, pivotAt[index]+1
		if first < 0 || last >= len(segments) {
			continue
		}
		low, high := math.MaxFloat64, -math.MaxFloat64
		for segmentIndex := first; segmentIndex <= last; segmentIndex++ {
			low = math.Min(low, segments[segmentIndex].Low)
			high = math.Max(high, segments[segmentIndex].High)
		}
		result = append(result, Stroke{Direction: direction, Chain: start.Chain, Start: start, End: end, Low: low, High: high, Confirmed: true, ConfirmedAt: end.ConfirmedAt, Level: level, StartBar: first, EndBar: last, StartRawBar: segments[first].StartRawBar, EndRawBar: segments[last].EndRawBar, InputStartBar: segments[first].StartInputBar, InputEndBar: segments[last].EndInputBar})
	}
	return result
}

func maxTimeString(values ...string) string {
	maximum := ""
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func recursiveCenterList(levels []LevelAnalysis) []RecursiveCenter {
	result := []RecursiveCenter{}
	for _, level := range levels {
		offset := len(result)
		for _, center := range level.Centers {
			copy := center
			if copy.RelatedCenter != nil {
				index := *copy.RelatedCenter + offset
				copy.RelatedCenter = &index
			}
			result = append(result, copy)
		}
	}
	return result
}

func buildStrengths(bars []Bar, levels []LevelAnalysis) []Strength {
	macd := macdSeries(bars)
	return buildStrengthsFromMACD(bars, levels, macd)
}

func buildStrengthsFromMACD(bars []Bar, levels []LevelAnalysis, macd []MACDPoint) []Strength {
	result := []Strength{}
	for _, level := range levels {
		for _, segment := range level.Segments {
			if !segment.Confirmed {
				continue
			}
			start, end := barIndexAtOrAfter(bars, segment.Start), barIndexAtOrBefore(bars, segment.End)
			if start < 0 || end < start || end >= len(macd) {
				continue
			}
			area := 0.0
			for index := maxInt(start, macdWarmupBars); index <= end; index++ {
				if segment.Direction == "up" {
					area += math.Max(0, macd[index].Histogram)
				} else {
					area += math.Max(0, -macd[index].Histogram)
				}
			}
			duration := maxInt(1, end-start)
			base := segment.StartPrice
			slope, amplitude := 0.0, 0.0
			if base > 0 {
				slope = 100 * (segment.EndPrice/base - 1) / float64(duration)
				amplitude = 100 * (segment.High - segment.Low) / base
			}
			result = append(result, Strength{Direction: segment.Direction, Level: level.Level, Start: segment.Start, End: segment.End, ConfirmedAt: segment.ConfirmedAt, MACDArea: area, MACDVersion: macdVersion, WarmupBars: macdWarmupBars, WarmupComplete: start >= macdWarmupBars, SlopePercent: slope, DurationBars: duration, Amplitude: amplitude, StartIndex: start, EndIndex: end})
		}
	}
	return result
}

func barIndexAtOrAfter(bars []Bar, date string) int {
	index := sort.Search(len(bars), func(index int) bool { return bars[index].Date >= date })
	if index >= len(bars) || bars[index].Date != date {
		return -1
	}
	return index
}

func barIndexAtOrBefore(bars []Bar, date string) int {
	index := sort.Search(len(bars), func(index int) bool { return bars[index].Date > date }) - 1
	if index < 0 || bars[index].Date != date {
		return -1
	}
	return index
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func addRecursiveAnalysis(frame Frame, bars []Bar) Frame {
	return addRecursiveAnalysisWithDivergenceRatio(frame, bars, recursiveDivergenceRatio)
}

func addRecursiveAnalysisWithDivergenceRatio(frame Frame, bars []Bar, divergenceRatio float64) Frame {
	return addRecursiveAnalysisWithComponents(frame, bars, divergenceRatio, nil)
}

func addRecursiveAnalysisWithComponents(frame Frame, bars []Bar, divergenceRatio float64, cached *incrementalComponents) Frame {
	frame.MergedCandles = []MergedCandle{}
	frame.FractalsV4 = []Fractal{}
	frame.StrokesV4 = []Stroke{}
	frame.Segments = []Segment{}
	frame.RecursiveLevels = []LevelAnalysis{}
	frame.RecursiveCenters = []RecursiveCenter{}
	frame.Strengths = []Strength{}
	frame.SignalsV4 = []V4Signal{}
	frame.MACD = []MACDPoint{}
	frame.TrendState = "unknown"
	frame.TrendContext = TrendContext{Status: "unknown", Direction: "unknown", CenterPosition: "unknown", DepartureStatus: "unknown", Reason: "尚无已确认递归走势和中枢可供判断"}
	frame.StructureChains = []StructureChain{}
	if len(bars) == 0 {
		return frame
	}
	var merged []mergedBar
	var fractals []Fractal
	var macd []MACDPoint
	if cached != nil && len(cached.macd) == len(bars) && len(cached.merged) > 0 {
		// incrementalComponents is private and constructed only by
		// IncrementalAnalyzer from the exact append-only input prefix.
		merged, fractals, macd = cached.merged, cached.fractals, cached.macd
	} else {
		merged = mergeBars(bars)
		fractals = findFractals(bars, merged)
		macd = macdSeries(bars)
	}
	frame.MergedCandles = mergedCandleOutput(merged)
	frame.FractalsV4 = fractals
	frame.StrokesV4, _ = buildStrokesWithHistogram(frame.FractalsV4, merged, histogramFromMACD(macd))
	frame.StructureChains = summarizeStructureChains(frame.StrokesV4, bars)
	frame.Segments = buildSegments(frame.StrokesV4)
	frame.RecursiveLevels = buildRecursiveLevels(frame.StrokesV4, frame.Segments)
	frame.RecursiveCenters = recursiveCenterList(frame.RecursiveLevels)
	frame.Strengths = buildStrengthsFromMACD(bars, frame.RecursiveLevels, macd)
	frame.MACD = macd
	frame.SignalsV4 = buildV4SignalsWithDivergenceRatio(frame, frame.RecursiveLevels, frame.Strengths, divergenceRatio)
	if len(frame.RecursiveLevels) > 0 {
		frame.TrendState = frameTrendState(frame.RecursiveLevels[0])
		frame.TrendContext = buildTrendContext(frame.RecursiveLevels[0], frame.TrendState)
	}
	return frame
}

func buildV4Signals(frame Frame, levels []LevelAnalysis, strengths []Strength) []V4Signal {
	return buildV4SignalsWithDivergenceRatio(frame, levels, strengths, recursiveDivergenceRatio)
}

func buildV4SignalsWithDivergenceRatio(frame Frame, levels []LevelAnalysis, strengths []Strength, divergenceRatio float64) []V4Signal {
	result := []V4Signal{}
	if len(levels) == 0 || math.IsNaN(divergenceRatio) || math.IsInf(divergenceRatio, 0) || divergenceRatio <= 0 || divergenceRatio >= 1 {
		return result
	}
	byLevel := make(map[int][]Strength)
	for _, strength := range strengths {
		byLevel[strength.Level] = append(byLevel[strength.Level], strength)
	}
	for _, level := range levels {
		if len(level.Centers) == 0 {
			continue
		}
		centers := []RecursiveCenter{}
		for _, center := range level.Centers {
			if !center.Forming {
				centers = append(centers, center)
			}
		}
		if len(centers) == 0 {
			continue
		}
		// Compare two pushes in the same direction with the intervening
		// counter-swing as a retest. Adjacent segments normally alternate, so
		// comparing index-1 with index would make divergence unreachable.
		for index := 2; index < len(level.Segments); index++ {
			current, retest, previous := level.Segments[index], level.Segments[index-1], level.Segments[index-2]
			if !current.Confirmed || !retest.Confirmed || !previous.Confirmed || current.Direction != previous.Direction || retest.Direction == current.Direction || current.Chain != previous.Chain || retest.Chain != current.Chain {
				continue
			}
			var center *RecursiveCenter
			for i := range centers {
				// The center must have been closed no later than the first push,
				// and both compared pushes must occur after its final overlapping
				// segment. The center-closing segment may itself be that first push.
				if centers[i].Chain == current.Chain && centers[i].ConfirmedAt != "" && centers[i].ConfirmedAt <= previous.ConfirmedAt && centerOwnerMoveIndex(centers[i]) < index-2 {
					center = &centers[i]
				}
			}
			if center == nil {
				continue
			}
			previousMove := segmentMovesAway(previous, center)
			currentMove := segmentMovesAway(current, center)
			// Require the intervening counter-swing to remain fully outside the
			// center. A retest that enters the zone is not the frozen departure /
			// non-return background for this comparison.
			retestStayedOutside := current.Direction == "up" && retest.Low > center.Upper || current.Direction == "down" && retest.High < center.Lower
			if !previousMove || !currentMove || !retestStayedOutside {
				continue
			}
			leftStrength, rightStrength := findSegmentStrength(byLevel[level.Level], previous), findSegmentStrength(byLevel[level.Level], current)
			if leftStrength == nil || rightStrength == nil || !leftStrength.WarmupComplete || !rightStrength.WarmupComplete || leftStrength.MACDArea <= 0 || rightStrength.MACDArea <= 0 {
				continue
			}
			newExtreme := current.Direction == "up" && current.EndPrice > previous.EndPrice || current.Direction == "down" && current.EndPrice < previous.EndPrice
			ratio := rightStrength.MACDArea / leftStrength.MACDArea
			if !newExtreme || ratio >= divergenceRatio {
				continue
			}
			side, kind := "sell", "class1_sell"
			if current.Direction == "down" {
				side, kind = "buy", "class1_buy"
			}
			status := "observing"
			phase := "observation"
			observedAt := current.ConfirmedAt
			confirmedAt := ""
			phaseAt := current.ConfirmedAt
			phaseHistory := []SignalPhase{{Phase: "observation", At: current.ConfirmedAt}}
			conditions := []string{"同级、同链、同方向的两段已确认推进，中间有已确认反向回试", "回试全段严格停留在中枢外，未回到中枢", "价格端点严格创新高/低", fmt.Sprintf("MACD 同向力度比低于版本参数 %.2f", divergenceRatio), "前一推进段已离开同级已确认中枢"}
			missing := []string{"后续反向线段确认走势端点", "反向结构后续延续得到一条已确认同向线段"}
			if index+1 < len(level.Segments) {
				reversal := level.Segments[index+1]
				if reversal.Confirmed && reversal.Direction != current.Direction && reversal.ConfirmedAt >= current.ConfirmedAt {
					status, phase, confirmedAt, phaseAt = "divergence_confirmed", "divergence_confirmed", reversal.ConfirmedAt, reversal.ConfirmedAt
					phaseHistory = append(phaseHistory, SignalPhase{Phase: phase, At: reversal.ConfirmedAt})
					missing = []string{"后续反向结构继续形成已确认走势段"}
					if index+3 < len(level.Segments) {
						followThrough := level.Segments[index+3]
						if followThrough.Confirmed && followThrough.Direction == reversal.Direction && followThrough.Chain == current.Chain && followThrough.ConfirmedAt >= reversal.ConfirmedAt {
							status, phase, phaseAt = "reversal_confirmed", "reversal_confirmed", followThrough.ConfirmedAt
							phaseHistory = append(phaseHistory, SignalPhase{Phase: phase, At: followThrough.ConfirmedAt})
							missing = []string{}
						}
					}
				}
			}
			// Scan only bars visible after the observation time. A touch of the
			// structural endpoint invalidates the divergence; no pre-observation
			// price may be used to rewrite its lifecycle.
			invalidatedAt := firstV4InvalidationAt(frame.Candles, observedAt, current.Direction, current.EndPrice)
			if invalidatedAt != "" {
				// Keep the phase history chronological if invalidation happened
				// before a later structural confirmation was available.
				kept := phaseHistory[:0]
				for _, event := range phaseHistory {
					// Daily bars cannot order an intraday structural confirmation
					// and price touch occurring on the same date; treat the touch as
					// first and do not claim that later confirmation was observable.
					if event.At < invalidatedAt {
						kept = append(kept, event)
					}
				}
				phaseHistory = append(kept, SignalPhase{Phase: "invalidated", At: invalidatedAt})
				status, phase, phaseAt = "invalidated", "invalidated", invalidatedAt
				confirmedAt = ""
				if len(phaseHistory) > 1 && phaseHistory[len(phaseHistory)-2].Phase == "divergence_confirmed" {
					confirmedAt = phaseHistory[len(phaseHistory)-2].At
				}
				missing = []string{}
			}
			ratioCopy, price := ratio, current.EndPrice
			levelStatus := "unconfirmed"
			for _, lower := range levels {
				if lower.Level == level.Level-1 {
					levelStatus = lower.Status
				}
			}
			result = append(result, V4Signal{Symbol: "", Level: level.Level, Kind: kind, Side: side, Status: status, Phase: phase, PhaseAt: phaseAt, PhaseHistory: phaseHistory, RuleVersion: RecursivePreviewVersion, Date: current.End, ObservedAt: observedAt, ConfirmedAt: confirmedAt, StructureRefs: []int{index - 2, index - 1, index}, Conditions: conditions, Missing: missing, Invalidation: ptr(current.EndPrice), StrengthRatio: &ratioCopy, Price: &price, MarketFilter: "unfiltered", LowerLevelStatus: levelStatus, Detail: fmt.Sprintf("同级 %s 走势段价格创新极值而 MACD 力度减弱；失效边界 %.4f；大盘过滤状态未提供", current.Direction, current.EndPrice)})
		}
	}
	// De-duplicate candidates by kind, level, and structural endpoint.
	seen := make(map[string]bool)
	filtered := result[:0]
	for _, signal := range result {
		key := fmt.Sprintf("%d|%s|%s", signal.Level, signal.Kind, signal.Date)
		if seen[key] {
			continue
		}
		seen[key] = true
		signal.SignalID = fmt.Sprintf("%s|L%d|%s|%s", RecursivePreviewVersion, signal.Level, signal.Kind, signal.Date)
		filtered = append(filtered, signal)
	}
	combined := append(filtered, buildClass23Signals(frame, filtered, levels)...)
	sort.SliceStable(combined, func(i, j int) bool {
		if combined[i].Date != combined[j].Date {
			return combined[i].Date < combined[j].Date
		}
		if combined[i].Level != combined[j].Level {
			return combined[i].Level < combined[j].Level
		}
		return combined[i].Kind < combined[j].Kind
	})
	return combined
}

func firstV4InvalidationAt(bars []Bar, observedAt, direction string, invalidation float64) string {
	if observedAt == "" || !finitePositive(invalidation) {
		return ""
	}
	for _, bar := range bars {
		if bar.Date <= observedAt {
			continue
		}
		if direction == "up" && bar.High >= invalidation || direction == "down" && bar.Low <= invalidation {
			return bar.Date
		}
	}
	return ""
}

func segmentMovesAway(segment Segment, center *RecursiveCenter) bool {
	if segment.Direction == "up" {
		return segment.EndPrice > center.Upper
	}
	if segment.Direction == "down" {
		return segment.EndPrice < center.Lower
	}
	return false
}

func findSegmentStrength(strengths []Strength, segment Segment) *Strength {
	for index := range strengths {
		if strengths[index].Start == segment.Start && strengths[index].End == segment.End && strengths[index].Level == segment.Level {
			return &strengths[index]
		}
	}
	return nil
}

func centerOwnerMoveIndex(center RecursiveCenter) int {
	if center.Level == 0 {
		return center.EndSegment
	}
	return center.OwnerMoveIndex
}

func frameTrendState(level LevelAnalysis) string {
	center, _ := lastClosedRecursiveCenter(level.Centers)
	segment, segmentIndex := lastConfirmedSegment(level.Segments)
	if center == nil || segment == nil {
		return "unknown"
	}
	ownerIndex := centerOwnerMoveIndex(*center)
	if segment.Chain != center.Chain || segment.Level != center.Level || segmentIndex <= ownerIndex {
		return "unknown"
	}
	departureIndex := ownerIndex + 1
	if departureIndex >= len(level.Segments) {
		return "unknown"
	}
	departure := level.Segments[departureIndex]
	if !departure.Confirmed || departure.Chain != center.Chain {
		return "unknown"
	}
	departureDirection := ""
	if departure.EndPrice > center.Upper && departure.Direction == "up" {
		departureDirection = "up"
	} else if departure.EndPrice < center.Lower && departure.Direction == "down" {
		departureDirection = "down"
	}
	if departureDirection == "" {
		return "range"
	}
	if segment.Direction != departureDirection {
		if segment.Low < center.Upper && segment.High > center.Lower {
			return "returning"
		}
		return "returning"
	}
	if segmentIndex == departureIndex {
		return "departing"
	}
	// A resumed move after a completed counter-swing outside the center is a
	// directional trend state; a single departure alone remains departing.
	for index := departureIndex + 1; index < segmentIndex; index++ {
		counter := level.Segments[index]
		if counter.Confirmed && counter.Chain == center.Chain && counter.Direction != departureDirection {
			if departureDirection == "up" && counter.Low > center.Upper || departureDirection == "down" && counter.High < center.Lower {
				if departureDirection == "up" {
					return "uptrend"
				}
				return "downtrend"
			}
		}
	}
	return "departing"
}

func buildTrendContext(level LevelAnalysis, status string) TrendContext {
	context := TrendContext{Status: status, Direction: "unknown", Level: level.Level, CenterPosition: "unknown", DepartureStatus: "unknown", Reason: "等待已确认的同级走势段和可用中枢"}
	segment, segmentIndex := lastConfirmedSegment(level.Segments)
	if segment == nil {
		return context
	}
	context.AsOf = segment.ConfirmedAt
	context.Direction = segment.Direction
	center, _ := lastClosedRecursiveCenter(level.Centers)
	if center == nil {
		context.Status = "unknown"
		context.Reason = "没有已确认的同级中枢，不能判定离开或回试关系"
		return context
	}
	context.CenterID = center.CenterID
	context.CenterRelation = center.RelationToPrevious
	switch {
	case segment.EndPrice > center.Upper:
		context.CenterPosition = "above"
	case segment.EndPrice < center.Lower:
		context.CenterPosition = "below"
	default:
		context.CenterPosition = "inside"
	}
	ownerIndex := centerOwnerMoveIndex(*center)
	if segment.Chain != center.Chain || segment.Level != center.Level || segmentIndex <= ownerIndex {
		context.Status = "unknown"
		context.DepartureStatus = "unknown"
		context.Reason = "末端走势段不属于该中枢之后的同链同级结构"
		return context
	}
	departureIndex := ownerIndex + 1
	if departureIndex >= len(level.Segments) {
		context.Status, context.DepartureStatus = "range", "no_departure"
		context.Reason = "中枢形成后尚无已确认走势段离开"
		return context
	}
	departure := level.Segments[departureIndex]
	if !departure.Confirmed || departure.Chain != center.Chain || departure.Level != center.Level {
		context.Status, context.DepartureStatus = "unknown", "unknown"
		context.Reason = "中枢后的首个走势段尚未确认"
		return context
	}
	departureDirection := ""
	if departure.Direction == "up" && departure.EndPrice > center.Upper {
		departureDirection = "up"
	} else if departure.Direction == "down" && departure.EndPrice < center.Lower {
		departureDirection = "down"
	}
	if departureDirection == "" {
		context.Status, context.DepartureStatus = "range", "inside_center"
		context.Reason = "中枢后的首个走势段未能收在中枢外"
		return context
	}
	if segmentIndex > departureIndex && segment.Direction != departureDirection {
		if context.CenterPosition == "inside" || segment.Low < center.Upper && segment.High > center.Lower {
			context.Status, context.DepartureStatus = "returning", "returned_to_center"
			context.Reason = "离开后反向回试重新进入中枢范围"
		} else {
			context.Status, context.DepartureStatus = "returning", "retest_outside"
			context.Reason = "离开后正在反向回试，尚未确认进入中枢"
		}
		return context
	}
	if segmentIndex > departureIndex && segment.Direction == departureDirection {
		for index := departureIndex + 1; index < segmentIndex; index++ {
			counter := level.Segments[index]
			if counter.Confirmed && counter.Direction != departureDirection {
				if departureDirection == "up" && counter.Low > center.Upper || departureDirection == "down" && counter.High < center.Lower {
					context.Status = departureDirection + "trend"
					context.DepartureStatus = "trend_resumed_after_outside_retest"
					context.Reason = "离开后反向回试保持在中枢外，随后同向走势再次确认"
					return context
				}
			}
		}
	}
	if context.CenterPosition == "inside" {
		context.Status, context.DepartureStatus = "range", "inside_center"
		context.Reason = "末端已确认走势段收于中枢范围内"
		return context
	}
	context.Status, context.DepartureStatus = "departing", "departed"
	context.Reason = "末端已确认走势段仍沿离开方向运行并位于中枢外侧"
	return context
}

func lastConfirmedSegment(segments []Segment) (*Segment, int) {
	for index := len(segments) - 1; index >= 0; index-- {
		if segments[index].Confirmed {
			return &segments[index], index
		}
	}
	return nil, -1
}

func lastClosedRecursiveCenter(centers []RecursiveCenter) (*RecursiveCenter, int) {
	for index := len(centers) - 1; index >= 0; index-- {
		if !centers[index].Forming && centers[index].ConfirmedAt != "" {
			return &centers[index], index
		}
	}
	return nil, -1
}

func parseTradingDate(value string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04", time.DateOnly} {
		if date, err := time.Parse(layout, value); err == nil {
			return date
		}
	}
	return time.Time{}
}

func midpointDate(start, end string) (string, bool) {
	startAt, endAt := parseTradingDate(start), parseTradingDate(end)
	if startAt.IsZero() || endAt.IsZero() || endAt.Before(startAt) {
		return "", false
	}
	midpoint := startAt.Add(endAt.Sub(startAt) / 2)
	if len(start) > len(time.DateOnly) {
		return midpoint.Format("2006-01-02 15:04"), true
	}
	return midpoint.Format(time.DateOnly), true
}
