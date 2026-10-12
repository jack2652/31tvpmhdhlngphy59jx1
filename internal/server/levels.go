package server

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"option-scope-go/internal/gamma"
	"option-scope-go/internal/model"
)

const (
	cachedLevelLimit     = 10
	cachedCandidateLimit = 30
)

type cachedLevel struct {
	price   float64
	score   float64
	factor  string
	factors []string
}

type cachedOptionStrike struct {
	strike  float64
	callVol float64
	putVol  float64
	callOI  float64
	putOI   float64
	callGEX float64
	putGEX  float64
}

const (
	cachedATRPeriod            = 14
	cachedFibonacciWindow      = 120
	cachedATRZoneRatio         = 0.30
	cachedMinZoneRatio         = 0.0025
	cachedATRMergeRatio        = 0.35
	cachedMinMergeRatio        = 0.0025
	cachedMaxMergeRatio        = 0.012
	cachedDefaultMergeRatio    = 0.005
	cachedOptionLimit          = 12
	cachedOptionWallProminence = 1.8
	cachedOptionWallShare      = 0.08
	// Exploratory purged walk-forward checks showed that reaction rate alone
	// admitted broken levels. Reinforced therefore
	// also requires a minimum adjusted hold rate and a maximum break rate.
	// The walk-forward sample is small, so these thresholds are a conservative
	// starting gate rather than a universal performance guarantee.
	cachedStrengthCalibratedMinModelScore = 0.55
	cachedStrengthCalibratedMinSamples    = 5
	cachedStrengthCalibratedMinReaction   = 0.80
	cachedStrengthCalibratedMinHold       = 0.40
	cachedStrengthCalibratedMaxBreak      = 0.60
	cachedAbsorptionReboundATR            = 0.5
	cachedAbsorptionBreakATR              = 0.25
	cachedAbsorptionReboundFallback       = 0.01
	cachedAbsorptionBreakFallback         = 0.002
)

type cachedTrendFitResult struct {
	slopePercent float64
	upper        float64
	lower        float64
	bars         int
}

func cachedATRSeries(bars []map[string]any, period int) []float64 {
	values := make([]float64, len(bars))
	if period <= 0 {
		return values
	}
	window := make([]float64, 0, period)
	var previous float64
	hasPrevious := false
	for i, bar := range bars {
		high, low, close := numberValue(bar["high"]), numberValue(bar["low"]), numberValue(bar["close"])
		if high != nil && low != nil && *high >= *low {
			tr := *high - *low
			if hasPrevious && previous > 0 {
				tr = math.Max(tr, math.Max(math.Abs(*high-previous), math.Abs(*low-previous)))
			}
			if tr >= 0 && !math.IsNaN(tr) && !math.IsInf(tr, 0) {
				if len(window) == period {
					window = window[1:]
				}
				window = append(window, tr)
			}
		}
		if close != nil && *close > 0 {
			previous, hasPrevious = *close, true
		}
		if len(window) > 0 {
			// Match Python's sum(window) order exactly for deterministic thresholds.
			total := 0.0
			for _, trueRange := range window {
				total += trueRange
			}
			values[i] = total / float64(len(window))
		}
	}
	return values
}

func cachedATR(bars []map[string]any, period int) float64 {
	values := cachedATRSeries(bars, period)
	if len(values) == 0 {
		return 0
	}
	return values[len(values)-1]
}

func cachedAdaptiveMergeTolerance(spot, atr float64) float64 {
	if spot <= 0 {
		return 0
	}
	return math.Min(spot*cachedMaxMergeRatio, math.Max(spot*cachedMinMergeRatio, atr*cachedATRMergeRatio))
}

func cachedTechnicalLevels(bars []map[string]any, spot float64) []cachedLevel {
	levels := cachedFibonacciLevels(bars)
	levels = append(levels, cachedChipPeaks(bars)...)
	levels = append(levels, cachedAbsorptionLevels(bars, spot)...)
	return levels
}

type cachedPeakItem struct {
	value float64
	index int
}

func sortCachedPeakItems(peaks []cachedPeakItem) {
	sort.Slice(peaks, func(i, j int) bool {
		if peaks[i].value != peaks[j].value {
			return peaks[i].value > peaks[j].value
		}
		// Python sorts (value, index) tuples in reverse order.
		return peaks[i].index > peaks[j].index
	})
}

func cachedChipPeaks(bars []map[string]any) []cachedLevel {
	start := len(bars) - 120
	if start < 0 {
		start = 0
	}
	window := bars[start:]
	if len(window) < 5 {
		return nil
	}
	low, high := math.Inf(1), math.Inf(-1)
	for _, bar := range window {
		l, lok := numericHistoryValue(bar["low"])
		h, hok := numericHistoryValue(bar["high"])
		// Python builds the profile's global range from both endpoints, even
		// when an input bar has inverted OHLC values and is swapped below.
		if lok {
			low, high = math.Min(low, l), math.Max(high, l)
		}
		if hok {
			low, high = math.Min(low, h), math.Max(high, h)
		}
	}
	if math.IsInf(low, 1) || math.IsInf(high, -1) || high <= low {
		return nil
	}
	const bucketCount = 240
	width := (high - low) / bucketCount
	profile := make([]float64, bucketCount)
	for _, bar := range window {
		l, lok := numericHistoryValue(bar["low"])
		h, hok := numericHistoryValue(bar["high"])
		vol, vok := numericHistoryValue(bar["volume"])
		if !lok || !hok || !vok || vol <= 0 {
			continue
		}
		if h < l {
			l, h = h, l
		}
		first := int((l - low) / width)
		last := int((h - low) / width)
		if first < 0 {
			first = 0
		}
		if first >= bucketCount {
			first = bucketCount - 1
		}
		if last < 0 {
			last = 0
		}
		if last >= bucketCount {
			last = bucketCount - 1
		}
		centerPrice, cok := numericHistoryValue(bar["close"])
		if !cok {
			centerPrice = (l + h) / 2
		}
		center := math.Max(0, math.Min(float64(last-first), ((centerPrice-low)/width)-float64(first)))
		weights, total := make([]float64, last-first+1), 0.0
		for i := range weights {
			distance := math.Abs(float64(i) - center)
			denom := math.Max(math.Max(center, float64(last-first)-center), 1)
			weights[i] = 1 - .8*distance/denom
			total += weights[i]
		}
		if total <= 0 {
			continue
		}
		for i, w := range weights {
			profile[first+i] += vol * w / total
		}
	}
	smoothed := make([]float64, bucketCount)
	peak := 0.0
	for i := range profile {
		left := i - 1
		if left < 0 {
			left = 0
		}
		right := i + 1
		if right >= bucketCount {
			right = bucketCount - 1
		}
		smoothed[i] = (profile[left] + profile[i] + profile[right]) / 3
		if smoothed[i] > peak {
			peak = smoothed[i]
		}
	}
	if peak <= 0 {
		return nil
	}
	peaks := make([]cachedPeakItem, 0)
	for i := 1; i < bucketCount-1; i++ {
		if smoothed[i] >= peak*.12 && smoothed[i] >= smoothed[i-1] && smoothed[i] > smoothed[i+1] {
			peaks = append(peaks, cachedPeakItem{smoothed[i], i})
		}
	}
	sortCachedPeakItems(peaks)
	picked := make([]cachedPeakItem, 0, 6)
	for _, p := range peaks {
		near := false
		for _, q := range picked {
			if absInt(p.index-q.index) <= 3 {
				near = true
				break
			}
		}
		if !near {
			picked = append(picked, p)
			if len(picked) == 6 {
				break
			}
		}
	}
	levels := make([]cachedLevel, 0, len(picked))
	for _, p := range picked {
		levels = append(levels, cachedLevel{price: low + (float64(p.index)+.5)*width, score: p.value / peak, factor: "筹码密集", factors: []string{"筹码密集"}})
	}
	return levels
}

func absorptionDistance(price, atr, atrRatio, fallbackRatio float64) float64 {
	if atr > 0 {
		return math.Max(atr*atrRatio, price*cachedMinZoneRatio)
	}
	return price * fallbackRatio
}

func cachedAbsorptionLevels(bars []map[string]any, spot float64) []cachedLevel {
	start := len(bars) - 60
	if start < 0 {
		start = 0
	}
	window := bars[start:]
	if len(window) < 10 || spot <= 0 {
		return nil
	}
	volumes := make([]float64, len(window))
	for index, bar := range window {
		for _, key := range []string{"high", "low", "close"} {
			if _, ok := numericHistoryValue(bar[key]); !ok {
				// Python rejects the entire absorption window when any price bar
				// is missing a required OHLC value.
				return nil
			}
		}
		if volume, ok := numericHistoryValue(bar["volume"]); ok {
			volumes[index] = volume
		}
	}
	atrValues := cachedATRSeries(window, cachedATRPeriod)
	volTotal := 0.0
	for _, volume := range volumes {
		volTotal += volume
	}
	avgVol := volTotal / float64(len(window))
	type candidate struct {
		index        int
		price, score float64
	}
	candidates := make([]candidate, 0)
	for i := 2; i < len(window)-2; i++ {
		low, lok := numericHistoryValue(window[i]["low"])
		high, hok := numericHistoryValue(window[i]["high"])
		close, cok := numericHistoryValue(window[i]["close"])
		if !lok || !hok || !cok || low <= 0 || low >= spot {
			continue
		}
		pivot := true
		for j := i - 2; j <= i+2; j++ {
			if j == i {
				continue
			}
			other, ok := numericHistoryValue(window[j]["low"])
			if ok && low >= other*.999 {
				pivot = false
				break
			}
		}
		if !pivot {
			continue
		}
		atr := atrValues[i]
		breakBuffer := absorptionDistance(low, atr, cachedAbsorptionBreakATR, cachedAbsorptionBreakFallback)
		rebound := close >= low+.5*(high-low)
		for j := i + 1; j < len(window); j++ {
			later, ok := numericHistoryValue(window[j]["low"])
			if ok && later < low-breakBuffer {
				pivot = false
				break
			}
			if j <= i+5 {
				fc, ok := numericHistoryValue(window[j]["close"])
				reboundDistance := absorptionDistance(low, atr, cachedAbsorptionReboundATR, cachedAbsorptionReboundFallback)
				if ok && fc >= low+reboundDistance {
					rebound = true
				}
			}
		}
		if !pivot || !rebound {
			continue
		}
		score := .6
		if avgVol > 0 && volumes[i] >= avgVol*1.2 {
			score += .2
		}
		candidates = append(candidates, candidate{i, low, score})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].index > candidates[j].index })
	if len(candidates) > 4 {
		candidates = candidates[:4]
	}
	levels := make([]cachedLevel, 0, len(candidates))
	for rank, item := range candidates {
		levels = append(levels, cachedLevel{price: item.price, score: item.score * math.Pow(.9, float64(rank)), factor: "承接位", factors: []string{"承接位"}})
	}
	return levels
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func cachedFibonacciLevels(bars []map[string]any) []cachedLevel {
	start := len(bars) - cachedFibonacciWindow
	if start < 0 {
		start = 0
	}
	window := bars[start:]
	if len(window) < 5 {
		return nil
	}
	highs, lows, closes := make([]float64, 0, len(window)), make([]float64, 0, len(window)), make([]float64, 0, len(window))
	for _, bar := range window {
		high, highOK := numericHistoryValue(bar["high"])
		low, lowOK := numericHistoryValue(bar["low"])
		close, closeOK := numericHistoryValue(bar["close"])
		if !highOK || !lowOK || !closeOK {
			return nil
		}
		highs, lows, closes = append(highs, high), append(lows, low), append(closes, close)
	}
	ranges := make([]float64, 0, len(window))
	previous := 0.0
	for i := range window {
		tr := highs[i] - lows[i]
		if i > 0 && previous > 0 {
			tr = math.Max(tr, math.Max(math.Abs(highs[i]-previous), math.Abs(lows[i]-previous)))
		}
		if tr >= 0 && !math.IsNaN(tr) && !math.IsInf(tr, 0) {
			ranges = append(ranges, tr)
		}
		if closes[i] > 0 {
			previous = closes[i]
		}
	}
	if len(ranges) == 0 {
		return nil
	}
	scale := cachedMedianTrueRange(ranges)
	pivots := closingPivots(closes, scale)
	highIndex, lowIndex := -1, -1
	for _, pivot := range pivots {
		if pivot.kind == "high" {
			highIndex = pivot.index
		} else {
			lowIndex = pivot.index
		}
	}
	high, low := 0.0, math.Inf(1)
	if highIndex < 0 || lowIndex < 0 || highIndex == lowIndex {
		high, low = math.Inf(-1), math.Inf(1)
		for i := range highs {
			if highs[i] >= high {
				high, highIndex = highs[i], i
			}
			if lows[i] <= low {
				low, lowIndex = lows[i], i
			}
		}
	} else {
		high, low = highs[highIndex], lows[lowIndex]
	}
	if high <= low || highIndex == lowIndex {
		return nil
	}
	ratioWeights := []struct{ ratio, weight float64 }{{0.236, 0.6}, {0.382, 0.9}, {0.5, 1.0}, {0.618, 1.0}, {0.786, 0.6}}
	levels := make([]cachedLevel, 0, len(ratioWeights))
	for _, item := range ratioWeights {
		price := low + (high-low)*item.ratio
		if lowIndex < highIndex {
			price = high - (high-low)*item.ratio
		}
		factor := "斐波那契 " + strings.TrimRight(strings.TrimRight(fmtFloat(item.ratio*100, 1), "0"), ".") + "%"
		levels = append(levels, cachedLevel{price: price, score: item.weight, factor: factor, factors: []string{factor}})
	}
	return levels
}

type cachedPivot struct {
	index int
	kind  string
}

// cachedMedianTrueRange matches app.level_candidates._median_true_range:
// even-sized samples use the upper middle element, not the arithmetic mean.
func cachedMedianTrueRange(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	return ordered[len(ordered)/2]
}

func closingPivots(closes []float64, scale float64) []cachedPivot {
	if len(closes) < 2 || scale <= 0 {
		return nil
	}
	pivots := make([]cachedPivot, 0)
	direction := ""
	extremeIndex, extremeClose := 0, closes[0]
	lowIndex, lowClose := 0, closes[0]
	highIndex, highClose := 0, closes[0]
	for index, close := range closes {
		if direction == "" {
			if close <= lowClose {
				lowIndex, lowClose = index, close
			}
			if close >= highClose {
				highIndex, highClose = index, close
			}
			up := math.Max(scale*2, math.Abs(lowClose)*0.03)
			down := math.Max(scale*2, math.Abs(highClose)*0.03)
			if close-lowClose >= up && close-lowClose >= highClose-close {
				pivots = append(pivots, cachedPivot{lowIndex, "low"})
				direction = "up"
				extremeIndex, extremeClose = index, close
			} else if highClose-close >= down {
				pivots = append(pivots, cachedPivot{highIndex, "high"})
				direction = "down"
				extremeIndex, extremeClose = index, close
			}
			continue
		}
		threshold := math.Max(scale*2, math.Abs(extremeClose)*0.03)
		if direction == "up" {
			if close >= extremeClose {
				extremeIndex, extremeClose = index, close
			} else if extremeClose-close >= threshold {
				pivots = append(pivots, cachedPivot{extremeIndex, "high"})
				direction = "down"
				extremeIndex, extremeClose = index, close
			}
		} else if close <= extremeClose {
			extremeIndex, extremeClose = index, close
		} else if close-extremeClose >= threshold {
			pivots = append(pivots, cachedPivot{extremeIndex, "low"})
			direction = "up"
			extremeIndex, extremeClose = index, close
		}
	}
	if direction == "up" {
		pivots = append(pivots, cachedPivot{extremeIndex, "high"})
	} else if direction == "down" {
		pivots = append(pivots, cachedPivot{extremeIndex, "low"})
	}
	return pivots
}

func fmtFloat(value float64, digits int) string {
	return strconv.FormatFloat(value, 'f', digits, 64)
}

func cachedOptionLevels(rows []model.OptionRow, spot float64) ([]cachedLevel, string) {
	return cachedOptionLevelsAt(rows, spot, time.Now())
}

func cachedOptionLevelsAt(rows []model.OptionRow, spot float64, now time.Time) ([]cachedLevel, string) {
	grouped := make(map[float64]*cachedOptionStrike)
	for _, row := range rows {
		if row.Strike == nil || *row.Strike <= 0 || math.IsNaN(*row.Strike) || math.IsInf(*row.Strike, 0) {
			continue
		}
		isCall := row.ContractType == "call"
		item := grouped[*row.Strike]
		if item == nil {
			item = &cachedOptionStrike{strike: *row.Strike}
			grouped[*row.Strike] = item
		}
		volume, oi := 0.0, 0.0
		if row.Volume != nil {
			volume = float64(*row.Volume)
		}
		if row.OpenInterest != nil {
			oi = float64(*row.OpenInterest)
		}
		// Python contract_gex derives Gamma from model_iv (then provider IV)
		// and does not trust a serialized model_gamma value.
		volatility := 0.0
		if row.ModelIV != nil && *row.ModelIV != 0 {
			volatility = *row.ModelIV
		} else if row.ImpliedVolatility != nil {
			volatility = *row.ImpliedVolatility
		}
		modelGamma := 0.0
		if volatility > 0 && row.Strike != nil && spot > 0 {
			location, loadErr := time.LoadLocation("America/New_York")
			if loadErr != nil {
				location = time.UTC
			}
			if expiry, parseErr := time.ParseInLocation("2006-01-02", row.Expiration, location); parseErr == nil {
				expires := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 16, 0, 0, 0, location)
				seconds := expires.Sub(now.In(location)).Seconds()
				// Python keeps a 30-minute minimum time value for every parseable
				// expiration, including same-day contracts after the close.
				years := math.Max(seconds/(365*24*3600), 30.0/(365*24*60))
				value := gamma.Gamma(spot, *row.Strike, volatility, years)
				if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
					modelGamma = value
				}
			}
		}
		gex := 0.0
		if modelGamma > 0 && oi > 0 && spot > 0 {
			gex = modelGamma * oi * 100 * spot * spot * 0.01
			if !isCall {
				gex = -gex
			}
		}
		if isCall {
			item.callVol += volume
			item.callOI += oi
			item.callGEX += gex
		} else {
			item.putVol += volume
			item.putOI += oi
			item.putGEX += gex
		}
	}
	// Python aggregates rows by strike, sorts strikes ascending, then sums
	// call/put totals to choose the GEX versus volume metric. Keep the same
	// addition order because values near the 20% threshold can round differently.
	strikes := make([]float64, 0, len(grouped))
	for strike := range grouped {
		strikes = append(strikes, strike)
	}
	sort.Float64s(strikes)
	totalOI, totalVolume := 0.0, 0.0
	for _, strike := range strikes {
		item := grouped[strike]
		totalVolume += item.callVol + item.putVol
		totalOI += item.callOI + item.putOI
	}
	metric := "volume"
	byGEX := totalOI > 0 && totalOI >= totalVolume*0.2
	if byGEX {
		// Preserve Python option_levels' public metric value ("gex").
		metric = "gex"
	}
	type candidate struct {
		value, strike  float64
		side, dominant string
	}
	candidates := make([]candidate, 0, len(grouped))
	for _, item := range grouped {
		if item.strike <= 0 || item.strike == spot {
			continue
		}
		callValue, putValue := item.callVol, item.putVol
		if byGEX {
			callValue, putValue = math.Max(item.callGEX, 0), math.Max(-item.putGEX, 0)
		}
		value := callValue + putValue
		if value <= 0 {
			continue
		}
		dominant := "看跌持仓"
		if callValue >= putValue {
			dominant = "看涨持仓"
		}
		side := "below"
		if item.strike > spot {
			side = "above"
		}
		candidates = append(candidates, candidate{value: value, strike: item.strike, side: side, dominant: dominant})
	}
	levels := make([]cachedLevel, 0, len(candidates))
	for _, side := range []string{"above", "below"} {
		onSide := make([]candidate, 0)
		for _, item := range candidates {
			if item.side == side {
				onSide = append(onSide, item)
			}
		}
		if len(onSide) == 0 {
			continue
		}
		// aggregate_strikes returns ascending strikes, and Python sums
		// side_values before ranking them. Preserve that order for the total.
		sort.Slice(onSide, func(i, j int) bool { return onSide[i].strike < onSide[j].strike })
		values := make([]float64, len(onSide))
		total := 0.0
		for index, item := range onSide {
			values[index], total = item.value, total+item.value
		}
		baseline := medianCached(values)
		upperQuartile := percentileCached(values, .75)
		wallThreshold := math.Max(baseline*cachedOptionWallProminence, upperQuartile*1.2)
		// Python sorts the candidate tuples (value, strike, side, dominant)
		// in reverse order after summing side_values.
		sort.Slice(onSide, func(i, j int) bool {
			if onSide[i].value != onSide[j].value {
				return onSide[i].value > onSide[j].value
			}
			return onSide[i].strike > onSide[j].strike
		})
		peak := onSide[0].value
		peakStrike := onSide[0].strike
		sideTotal := total
		for index, item := range onSide {
			if index >= cachedOptionLimit {
				break
			}
			share := item.value / math.Max(sideTotal, 1e-12)
			prominence := item.value / math.Max(baseline, 1e-12)
			isWall := item.strike == peakStrike && len(onSide) >= 3 && prominence >= cachedOptionWallProminence && item.value >= wallThreshold && share >= cachedOptionWallShare
			factor := item.dominant
			if isWall {
				factor = strings.Replace(factor, "持仓", "墙", 1)
			}
			weight := math.Min(1, 0.7*item.value/math.Max(peak, 1e-12)+0.3*share/math.Max(cachedOptionWallShare, 1e-12))
			levels = append(levels, cachedLevel{price: item.strike, score: weight, factor: factor, factors: []string{factor}})
		}
	}
	return levels, metric
}

func medianCached(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	mid := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[mid]
	}
	return (ordered[mid-1] + ordered[mid]) / 2
}

func percentileCached(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	position := float64(len(ordered)-1) * math.Max(0, math.Min(1, q))
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return ordered[lower]
	}
	fraction := position - float64(lower)
	return ordered[lower] + (ordered[upper]-ordered[lower])*fraction
}

type cachedBuyerQuoteKey struct {
	expiration   string
	contractType string
	strike       float64
}

// dedupeCachedBuyerQuotes follows Python's lowest-spread quote selection for
// repeated expiration/type/strike contracts; equal spreads keep the first row.
func dedupeCachedBuyerQuotes(rows []model.OptionRow) []model.OptionRow {
	result := make([]model.OptionRow, 0, len(rows))
	positions := make(map[cachedBuyerQuoteKey]int, len(rows))
	spread := func(row model.OptionRow) (float64, bool) {
		if row.Strike == nil || *row.Strike <= 0 || math.IsNaN(*row.Strike) || math.IsInf(*row.Strike, 0) ||
			row.Bid == nil || row.Ask == nil || *row.Bid <= 0 || *row.Ask < *row.Bid ||
			math.IsNaN(*row.Bid) || math.IsInf(*row.Bid, 0) || math.IsNaN(*row.Ask) || math.IsInf(*row.Ask, 0) {
			return 0, false
		}
		mid := (*row.Bid + *row.Ask) / 2
		if mid <= 0 || math.IsNaN(mid) || math.IsInf(mid, 0) {
			return 0, false
		}
		spread := (*row.Ask - *row.Bid) / mid
		if math.IsNaN(spread) || math.IsInf(spread, 0) {
			return 0, false
		}
		return spread, true
	}
	for _, row := range rows {
		if !strings.EqualFold(row.ContractType, "call") && !strings.EqualFold(row.ContractType, "put") {
			result = append(result, row)
			continue
		}
		value, valid := spread(row)
		if !valid {
			result = append(result, row)
			continue
		}
		key := cachedBuyerQuoteKey{expiration: row.Expiration, contractType: strings.ToLower(row.ContractType), strike: *row.Strike}
		if index, exists := positions[key]; exists {
			current, currentValid := spread(result[index])
			if !currentValid || value < current {
				result[index] = row
			}
			continue
		}
		positions[key] = len(result)
		result = append(result, row)
	}
	return result
}

func cachedBuyerBelowIntrinsic(mid, spot, strike float64, contractType string) bool {
	intrinsic := 0.0
	if strings.EqualFold(contractType, "call") {
		intrinsic = math.Max(0, spot-strike)
	} else {
		intrinsic = math.Max(0, strike-spot)
	}
	return mid < intrinsic*.98
}

func cachedBuyerHorizonEnd(now time.Time, tradingDays int) time.Time {
	cursor := now
	remaining := tradingDays
	if remaining < 1 {
		remaining = 1
	}
	for remaining > 0 {
		cursor = cursor.AddDate(0, 0, 1)
		if cursor.Weekday() != time.Saturday && cursor.Weekday() != time.Sunday {
			remaining--
		}
	}
	return cursor
}

// cachedBuyerExpirationEligibility mirrors Python's calendar-date DTE and
// excludes contracts expiring before the end of the forward trading-day window.
func cachedBuyerExpirationEligibility(expiration string, now time.Time, tradingDays int) (dte int, eligible, valid bool) {
	location := now.Location()
	day, err := time.ParseInLocation("2006-01-02", expiration, location)
	if err != nil || day.Format("2006-01-02") != expiration {
		return 0, false, false
	}
	localNow := now.In(location)
	dateUTC := func(value time.Time) time.Time {
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	}
	dte = int(dateUTC(day).Sub(dateUTC(localNow)).Hours() / 24)
	if dte < 0 {
		dte = 0
	}
	expiry := time.Date(day.Year(), day.Month(), day.Day(), 16, 0, 0, 0, location)
	return dte, expiry.After(cachedBuyerHorizonEnd(localNow, tradingDays)), true
}

const (
	cachedBuyerRiskFreeRate   = 0.005
	cachedBuyerHorizonDays    = 5
	cachedBuyerMinDTE         = 10
	cachedBuyerMaxDTE         = 45
	cachedBuyerMaxSpread      = 0.10
	cachedBuyerGoodSpread     = 0.05
	cachedBuyerTargetFallback = 0.05
	cachedBuyerMinTargetTouch = 0.15
	cachedBuyerMaxVerticalPct = 0.15
	cachedBuyerDirectionLimit = 5
)

type cachedBuyerContract struct {
	row           model.OptionRow
	mid           float64
	spreadRatio   float64
	dte           int
	years         float64
	activity      float64
	activityScore float64
	iv            float64
	ivSource      string
	delta         float64
	thetaPerDay   float64
	vega          float64
}

type cachedBuyerGroup struct {
	expiration string
	dte        int
	rows       []*cachedBuyerContract
}

func cachedBuyerReferenceIV(groups []*cachedBuyerGroup, spot float64) float64 {
	for _, group := range groups {
		type strikeSamples struct {
			strike float64
			values []float64
		}
		byStrike := make([]strikeSamples, 0)
		positions := make(map[float64]int)
		for _, row := range group.rows {
			strike := row.rowStrike()
			if row.ivSource != "contract" || strike <= 0 || math.Abs(math.Log(strike/spot)) > .03 {
				continue
			}
			position, exists := positions[strike]
			if !exists {
				position = len(byStrike)
				positions[strike] = position
				byStrike = append(byStrike, strikeSamples{strike: strike})
			}
			byStrike[position].values = append(byStrike[position].values, row.iv)
		}
		if len(byStrike) == 0 {
			continue
		}
		paired := make([]strikeSamples, 0, len(byStrike))
		for _, sample := range byStrike {
			if len(sample.values) >= 2 {
				paired = append(paired, sample)
			}
		}
		pool := byStrike
		if len(paired) > 0 {
			pool = paired
		}
		selected := pool[0]
		for _, sample := range pool[1:] {
			if math.Abs(sample.strike-spot) < math.Abs(selected.strike-spot) {
				selected = sample
			}
		}
		total := 0.0
		for _, value := range selected.values {
			total += value
		}
		return total / float64(len(selected.values))
	}

	samples := make([]float64, 0)
	for _, group := range groups {
		for _, row := range group.rows {
			if row.iv > 0 && !math.IsNaN(row.iv) && !math.IsInf(row.iv, 0) {
				samples = append(samples, row.iv)
			}
		}
	}
	if len(samples) == 0 {
		return 0
	}
	sort.Float64s(samples)
	return samples[len(samples)/2]
}

func cachedBuyerNormCDF(value float64) float64 {
	return 0.5 * (1 + math.Erf(value/math.Sqrt2))
}

func cachedBuyerNormPDF(value float64) float64 {
	return math.Exp(-0.5*value*value) / math.Sqrt(2*math.Pi)
}

func cachedBuyerBSPrice(spot, strike, volatility, years float64, isCall bool) float64 {
	if spot <= 0 || strike <= 0 || volatility <= 0 || years <= 0 {
		return 0
	}
	volatilityTime := volatility * math.Sqrt(years)
	if volatilityTime <= 0 {
		return 0
	}
	d1 := (math.Log(spot/strike) + (cachedBuyerRiskFreeRate+0.5*volatility*volatility)*years) / volatilityTime
	d2 := d1 - volatilityTime
	discount := math.Exp(-cachedBuyerRiskFreeRate * years)
	if isCall {
		return spot*cachedBuyerNormCDF(d1) - strike*discount*cachedBuyerNormCDF(d2)
	}
	return strike*discount*cachedBuyerNormCDF(-d2) - spot*cachedBuyerNormCDF(-d1)
}

func cachedBuyerIVFromPrice(price, spot, strike, years float64, isCall bool) (float64, bool) {
	if price <= 0 || spot <= 0 || strike <= 0 || years <= 0 {
		return 0, false
	}
	intrinsic := 0.0
	if isCall {
		intrinsic = math.Max(spot-strike, 0)
	} else {
		intrinsic = math.Max(strike-spot, 0)
	}
	if price < intrinsic*.98 {
		return 0, false
	}
	const lowFloor, highCeiling = 0.03, 5.0
	lowPrice := cachedBuyerBSPrice(spot, strike, lowFloor, years, isCall)
	highPrice := cachedBuyerBSPrice(spot, strike, highCeiling, years, isCall)
	if price < lowPrice || price > highPrice {
		return 0, false
	}
	low, high := lowFloor, highCeiling
	for i := 0; i < 60; i++ {
		middle := (low + high) / 2
		if cachedBuyerBSPrice(spot, strike, middle, years, isCall) < price {
			low = middle
		} else {
			high = middle
		}
	}
	return (low + high) / 2, true
}

func cachedBuyerGreeks(spot, strike, volatility, years float64, isCall bool) (float64, float64, float64, bool) {
	if spot <= 0 || strike <= 0 || volatility <= 0 || years <= 0 {
		return 0, 0, 0, false
	}
	volatilityTime := volatility * math.Sqrt(years)
	if volatilityTime <= 0 {
		return 0, 0, 0, false
	}
	d1 := (math.Log(spot/strike) + (cachedBuyerRiskFreeRate+0.5*volatility*volatility)*years) / volatilityTime
	d2 := d1 - volatilityTime
	var delta, theta float64
	if isCall {
		delta = cachedBuyerNormCDF(d1)
		theta = -spot*cachedBuyerNormPDF(d1)*volatility/(2*math.Sqrt(years)) - cachedBuyerRiskFreeRate*strike*math.Exp(-cachedBuyerRiskFreeRate*years)*cachedBuyerNormCDF(d2)
	} else {
		delta = cachedBuyerNormCDF(d1) - 1
		theta = -spot*cachedBuyerNormPDF(d1)*volatility/(2*math.Sqrt(years)) + cachedBuyerRiskFreeRate*strike*math.Exp(-cachedBuyerRiskFreeRate*years)*cachedBuyerNormCDF(-d2)
	}
	vega := spot * cachedBuyerNormPDF(d1) * math.Sqrt(years)
	if math.IsNaN(delta) || math.IsNaN(theta) || math.IsNaN(vega) || math.IsInf(delta, 0) || math.IsInf(theta, 0) || math.IsInf(vega, 0) {
		return 0, 0, 0, false
	}
	return delta, theta / 365, vega, true
}

func cachedBuyerYears(expiration string, now time.Time) (float64, bool) {
	day, err := time.ParseInLocation("2006-01-02", expiration, now.Location())
	if err != nil || day.Format("2006-01-02") != expiration {
		return 0, false
	}
	expires := time.Date(day.Year(), day.Month(), day.Day(), 16, 0, 0, 0, now.Location())
	years := expires.Sub(now).Hours() / (24 * 365)
	return years, years > 0
}

func cachedBuyerTouchRaw(price, spot, volatility, years float64) (float64, bool) {
	if price <= 0 || spot <= 0 || volatility <= 0 || years <= 0 {
		return 0, false
	}
	sigmaTime := volatility * math.Sqrt(years)
	if sigmaTime <= 0 {
		return 0, false
	}
	distance := math.Abs(math.Log(price / spot))
	value := 2 * cachedBuyerNormCDF(-distance/sigmaTime)
	return math.Min(1, value), true
}

func cachedBuyerModelIV(details map[string]any) (float64, string) {
	if details == nil {
		return 0, ""
	}
	iv := numberValue(details["iv"])
	if iv == nil || *iv <= 0 {
		return 0, ""
	}
	source, _ := details["source"].(string)
	return *iv, source
}

// cachedBuyerTarget keeps the historical helper signature for tests and other
// callers while accepting optional trend metadata for the Python-compatible
// channel fallback.
func cachedBuyerTarget(direction string, spot float64, support, resistance []map[string]any, volatility, horizonYears float64, trends ...map[string]any) (float64, string, float64, bool) {
	levels := resistance
	source := "综合压力位"
	if direction == "put" {
		levels, source = support, "综合支撑位"
	}
	type targetCandidate struct {
		price, utility, touch float64
		hasTouch              bool
	}
	candidates := make([]targetCandidate, 0, len(levels))
	for _, level := range levels {
		price := numberValue(level["price"])
		if price == nil || *price <= 0 || (direction == "call" && *price <= spot) || (direction == "put" && *price >= spot) {
			continue
		}
		strength := numberValue(level["score"])
		if strength == nil {
			strength = numberValue(level["model_score"])
		}
		weight := 0.5
		if strength != nil {
			weight = math.Min(1, math.Max(0, *strength))
		}
		touch, hasTouch := cachedBuyerTouchRaw(*price, spot, volatility, horizonYears)
		probability := 0.5
		if hasTouch {
			probability = touch
		}
		candidates = append(candidates, targetCandidate{price: *price, utility: weight * probability, touch: touch, hasTouch: hasTouch})
	}
	if len(candidates) > 0 {
		reachable := make([]targetCandidate, 0, len(candidates))
		for _, item := range candidates {
			if item.hasTouch && item.touch >= cachedBuyerMinTargetTouch {
				reachable = append(reachable, item)
			}
		}
		pool := candidates
		if len(reachable) > 0 {
			pool = reachable
		}
		best := pool[0]
		for _, item := range pool[1:] {
			if item.utility > best.utility || (item.utility == best.utility && item.touch > best.touch) || (item.utility == best.utility && item.touch == best.touch && math.Abs(item.price-spot) < math.Abs(best.price-spot)) {
				best = item
			}
		}
		return best.price, source, best.touch, best.hasTouch
	}
	if len(trends) > 0 && trends[0] != nil {
		channelKey := "lower"
		if direction == "call" {
			channelKey = "upper"
		}
		if channel := numberValue(trends[0][channelKey]); channel != nil && *channel > 0 && ((direction == "call" && *channel > spot) || (direction == "put" && *channel < spot)) {
			touch, hasTouch := cachedBuyerTouchRaw(*channel, spot, volatility, horizonYears)
			return *channel, "趋势通道", touch, hasTouch
		}
	}
	fallback := spot * (1 + cachedBuyerTargetFallback)
	if direction == "put" {
		fallback = spot * (1 - cachedBuyerTargetFallback)
	}
	touch, hasTouch := cachedBuyerTouchRaw(fallback, spot, volatility, horizonYears)
	return fallback, "情景参考位", touch, hasTouch
}

func cachedBuyerFutureMark(contract *cachedBuyerContract, spot, price, horizonYears float64, isCall bool) float64 {
	remaining := contract.years - horizonYears
	if remaining <= 1/(365*24*60) {
		if isCall {
			return math.Max(price-contract.rowStrike(), 0)
		}
		return math.Max(contract.rowStrike()-price, 0)
	}
	future := cachedBuyerBSPrice(price, contract.rowStrike(), contract.iv, remaining, isCall)
	today := cachedBuyerBSPrice(spot, contract.rowStrike(), contract.iv, contract.years, isCall)
	return math.Max(0, contract.mid+future-today)
}

func (contract *cachedBuyerContract) rowStrike() float64 {
	if contract == nil || contract.row.Strike == nil {
		return 0
	}
	return *contract.row.Strike
}

func cachedBuyerSpreadScore(spread float64) float64 {
	if spread <= 0.02 {
		return 1
	}
	if spread >= cachedBuyerMaxSpread {
		return 0
	}
	return math.Max(0, 1-(spread-0.02)/0.08)
}

func cachedBuyerLiquidity(spread, activity float64) string {
	if spread <= 0.02 && activity >= 0.6 {
		return "良好"
	}
	if spread <= cachedBuyerGoodSpread && activity >= 0.35 {
		return "可接受"
	}
	return "偏弱"
}

func cachedBuyerSingleQuality(contract *cachedBuyerContract, target, spot float64) float64 {
	deltaQuality := math.Max(0, 1-math.Abs(math.Abs(contract.delta)-0.58)/0.58)
	thetaRatio := math.Abs(contract.thetaPerDay) / math.Max(contract.mid, 0.01)
	thetaQuality := math.Max(0, 1-math.Min(thetaRatio/0.08, 1))
	distance := math.Abs(contract.rowStrike()/spot - target/spot)
	targetQuality := math.Max(0, 1-math.Min(distance/0.12, 1))
	return math.Min(1, math.Max(0, cachedBuyerSpreadScore(contract.spreadRatio)*0.30+contract.activityScore*0.20+deltaQuality*0.20+thetaQuality*0.15+targetQuality*0.15))
}

func cachedBuyerSingle(contract *cachedBuyerContract, direction string, target float64, targetSource string, spot, horizonYears, touch float64, hasTouch bool) map[string]any {
	isCall := direction == "call"
	cost := contract.mid * 100
	if contract.mid <= 0 || cost <= 0 {
		return nil
	}
	targetExit := cachedBuyerFutureMark(contract, spot, target, horizonYears, isCall)
	unchangedExit := cachedBuyerFutureMark(contract, spot, spot, horizonYears, isCall)
	halfSpread := contract.mid * contract.spreadRatio / 2
	scenarioReturn := (targetExit - contract.mid) / contract.mid
	expected := (func() float64 {
		probability := 0.5
		if hasTouch {
			probability = touch
		}
		targetReturn := (targetExit - contract.mid - halfSpread) / contract.mid
		unchangedReturn := (unchangedExit - contract.mid - halfSpread) / contract.mid
		return probability*targetReturn + (1-probability)*unchangedReturn
	})()
	if math.IsNaN(scenarioReturn) || math.IsInf(scenarioReturn, 0) || math.IsNaN(expected) || math.IsInf(expected, 0) {
		return nil
	}
	strike := contract.rowStrike()
	breakeven := strike - contract.mid
	if isCall {
		breakeven = strike + contract.mid
	}
	moneyness := "价外"
	if math.Abs(strike/spot-1) <= 0.01 {
		moneyness = "平值"
	} else if (isCall && strike < spot) || (!isCall && strike > spot) {
		moneyness = "价内"
	}
	style := "时间容错" + moneyness + "单腿"
	advantage, risk := "到期更远，时间衰减压力更小", "权利金更高，同样行情下收益率更低"
	if contract.dte <= 21 {
		style = "短线" + moneyness + "单腿"
		advantage, risk = "期限覆盖分析窗口，方向弹性更高", "越接近到期，时间价值损耗越快"
	}
	touchValue := any(nil)
	if hasTouch {
		touchValue = roundCached(touch, 4)
	}
	return map[string]any{
		"kind": "single", "style": style, "direction": direction,
		"label":      map[string]string{"call": "买入看涨", "put": "买入看跌"}[direction],
		"expiration": contract.row.Expiration, "dte": contract.dte,
		"strikes": []float64{roundCached(strike, 4)},
		"legs":    []map[string]any{{"action": "buy", "contract_type": direction, "strike": roundCached(strike, 4)}},
		"iv":      contract.iv, "delta": contract.delta,
		"delta_exposure":     spot * 100 * math.Abs(contract.delta),
		"effective_leverage": spot * 100 * math.Abs(contract.delta) / cost,
		"max_loss":           cost, "cost": cost, "breakeven": breakeven,
		"target_price": target, "target_source": targetSource,
		"scenario_return": scenarioReturn, "scenario_profitable": scenarioReturn >= 0,
		"touch_probability": touchValue, "expected_return": expected,
		"spread_ratio": contract.spreadRatio, "liquidity": cachedBuyerLiquidity(contract.spreadRatio, contract.activityScore),
		"score": cachedBuyerSingleQuality(contract, target, spot), "advantage": advantage, "risk": risk,
		"model_iv_source": map[bool]string{true: "合约中间价反解", false: "到期日模型估算"}[contract.ivSource == "contract"],
		"quote_method":    "买卖价中间价",
	}
}

func cachedBuyerLongRows(group *cachedBuyerGroup, direction string, spot float64) []*cachedBuyerContract {
	low, high := spot*0.90, spot*1.08
	if direction == "put" {
		low, high = spot*0.92, spot*1.10
	}
	rows := make([]*cachedBuyerContract, 0)
	for _, row := range group.rows {
		if strings.EqualFold(row.row.ContractType, direction) && row.rowStrike() >= low && row.rowStrike() <= high {
			rows = append(rows, row)
		}
	}
	if len(rows) > 0 {
		return rows
	}
	var nearest *cachedBuyerContract
	for _, row := range group.rows {
		if !strings.EqualFold(row.row.ContractType, direction) || math.Abs(row.rowStrike()-spot) > spot*.15 {
			continue
		}
		if nearest == nil || math.Abs(row.rowStrike()-spot) < math.Abs(nearest.rowStrike()-spot) {
			nearest = row
		}
	}
	if nearest != nil {
		return []*cachedBuyerContract{nearest}
	}
	return nil
}

func cachedBuyerShortRows(group *cachedBuyerGroup, long *cachedBuyerContract, direction string, target, spot float64) []*cachedBuyerContract {
	rows := make([]*cachedBuyerContract, 0)
	for _, row := range group.rows {
		if !strings.EqualFold(row.row.ContractType, direction) {
			continue
		}
		if direction == "call" && row.rowStrike() <= long.rowStrike() {
			continue
		}
		if direction == "put" && row.rowStrike() >= long.rowStrike() {
			continue
		}
		if math.Abs(row.rowStrike()-long.rowStrike()) <= spot*cachedBuyerMaxVerticalPct {
			rows = append(rows, row)
		}
	}
	if len(rows) > 0 {
		return rows
	}
	var nearest *cachedBuyerContract
	for _, row := range group.rows {
		if !strings.EqualFold(row.row.ContractType, direction) {
			continue
		}
		if direction == "call" && row.rowStrike() <= long.rowStrike() {
			continue
		}
		if direction == "put" && row.rowStrike() >= long.rowStrike() {
			continue
		}
		if nearest == nil || math.Abs(row.rowStrike()-target) < math.Abs(nearest.rowStrike()-target) {
			nearest = row
		}
	}
	if nearest != nil && math.Abs(nearest.rowStrike()-long.rowStrike()) <= spot*.25 {
		return []*cachedBuyerContract{nearest}
	}
	return nil
}

func cachedBuyerVertical(long, short *cachedBuyerContract, direction string, target float64, targetSource string, spot, horizonYears, touch float64, hasTouch bool) map[string]any {
	isCall := direction == "call"
	debit := long.mid - short.mid
	width := math.Abs(short.rowStrike() - long.rowStrike())
	halfSpread := long.mid*long.spreadRatio/2 + short.mid*short.spreadRatio/2
	if debit <= halfSpread || debit >= width || width <= 0 {
		return nil
	}
	targetExit := math.Min(math.Max(cachedBuyerFutureMark(long, spot, target, horizonYears, isCall)-cachedBuyerFutureMark(short, spot, target, horizonYears, isCall), 0), width)
	unchangedExit := math.Min(math.Max(cachedBuyerFutureMark(long, spot, spot, horizonYears, isCall)-cachedBuyerFutureMark(short, spot, spot, horizonYears, isCall), 0), width)
	scenarioReturn := (targetExit - debit) / debit
	probability := 0.5
	if hasTouch {
		probability = touch
	}
	expected := probability*((targetExit-debit-halfSpread)/debit) + (1-probability)*((unchangedExit-debit-halfSpread)/debit)
	if math.IsNaN(scenarioReturn) || math.IsInf(scenarioReturn, 0) || math.IsNaN(expected) || math.IsInf(expected, 0) {
		return nil
	}
	netDelta := long.delta - short.delta
	maxLoss, maxProfit := debit*100, math.Max(0, width-debit)*100
	activity := math.Min(long.activityScore, short.activityScore)
	spreadQuality := (cachedBuyerSpreadScore(long.spreadRatio) + cachedBuyerSpreadScore(short.spreadRatio)) / 2
	targetQuality := math.Max(0, 1-math.Min(math.Abs(short.rowStrike()-target)/math.Max(spot*.12, .01), 1))
	score := math.Min(1, math.Max(0, spreadQuality*.30+activity*.20+targetQuality*.25+math.Min(math.Abs(netDelta)/.55, 1)*.15+math.Min(maxProfit/math.Max(maxLoss, .01), 1)*.10))
	touchValue := any(nil)
	if hasTouch {
		touchValue = roundCached(touch, 4)
	}
	return map[string]any{
		"kind": "vertical", "style": map[bool]string{true: "近期目标位价差", false: "远期目标位价差"}[long.dte <= 21],
		"direction": direction, "label": map[string]string{"call": "看涨价差", "put": "看跌价差"}[direction],
		"expiration": long.row.Expiration, "dte": long.dte,
		"strikes": []float64{roundCached(long.rowStrike(), 4), roundCached(short.rowStrike(), 4)},
		"legs":    []map[string]any{{"action": "buy", "contract_type": direction, "strike": roundCached(long.rowStrike(), 4)}, {"action": "sell", "contract_type": direction, "strike": roundCached(short.rowStrike(), 4)}},
		"iv":      (long.iv + short.iv) / 2, "delta": netDelta,
		"delta_exposure": spot * 100 * math.Abs(netDelta), "effective_leverage": spot * 100 * math.Abs(netDelta) / maxLoss,
		"max_loss": maxLoss, "max_profit": maxProfit, "cost": maxLoss,
		"breakeven":    map[bool]float64{true: long.rowStrike() + debit, false: long.rowStrike() - debit}[isCall],
		"target_price": target, "target_source": targetSource, "scenario_return": scenarioReturn, "scenario_profitable": scenarioReturn >= 0,
		"touch_probability": touchValue, "expected_return": expected, "spread_ratio": math.Max(long.spreadRatio, short.spreadRatio),
		"liquidity": cachedBuyerLiquidity(math.Max(long.spreadRatio, short.spreadRatio), activity), "score": score,
		"advantage": "卖出腿压低权利金，并降低波动率暴露", "risk": "收益封顶，两条腿都要有可成交报价",
		"model_iv_source": map[bool]string{true: "合约中间价反解", false: "到期日模型估算"}[long.ivSource == "contract" && short.ivSource == "contract"],
		"quote_method":    "双腿买卖价中间价", "net_theta_per_day": long.thetaPerDay - short.thetaPerDay, "net_vega": long.vega - short.vega,
	}
}

func cachedBuyerRankValue(item map[string]any, key string, missing float64) float64 {
	value, ok := item[key].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return missing
	}
	return value
}

func cachedBuyerStrikeList(item map[string]any) []float64 {
	strikes, _ := item["strikes"].([]float64)
	return strikes
}

func cachedBuyerTooSimilar(left, right map[string]any, spot float64) bool {
	leftKind, _ := left["kind"].(string)
	rightKind, _ := right["kind"].(string)
	leftExpiration, _ := left["expiration"].(string)
	rightExpiration, _ := right["expiration"].(string)
	if leftKind != rightKind || leftExpiration != rightExpiration {
		return false
	}
	leftStrikes, rightStrikes := cachedBuyerStrikeList(left), cachedBuyerStrikeList(right)
	if len(leftStrikes) != len(rightStrikes) || len(leftStrikes) == 0 {
		return false
	}
	identical := true
	for index := range leftStrikes {
		if leftStrikes[index] != rightStrikes[index] {
			identical = false
			break
		}
	}
	if identical {
		return true
	}
	tolerance := math.Max(spot*0.015, 0.01)
	if math.Abs(leftStrikes[0]-rightStrikes[0]) > tolerance {
		return false
	}
	if leftKind == "single" {
		return true
	}
	return math.Abs(leftStrikes[len(leftStrikes)-1]-rightStrikes[len(rightStrikes)-1]) <= tolerance
}

func cachedBuyerSelectDiverse(ranked []map[string]any, limit int, spot float64) []map[string]any {
	if limit <= 0 || len(ranked) == 0 {
		return nil
	}
	selected := make([]int, 0, limit)
	seen := make(map[int]bool, limit)
	add := func(index int) bool {
		if seen[index] {
			return false
		}
		seen[index] = true
		selected = append(selected, index)
		return true
	}
	for index, item := range ranked {
		if len(selected) >= limit {
			break
		}
		similar := false
		for _, keptIndex := range selected {
			if cachedBuyerTooSimilar(item, ranked[keptIndex], spot) {
				similar = true
				break
			}
		}
		if !similar {
			add(index)
		}
	}
	ensure := func(kind string) {
		for _, index := range selected {
			if ranked[index]["kind"] == kind {
				return
			}
		}
		candidate := -1
		for index, item := range ranked {
			if item["kind"] == kind {
				candidate = index
				break
			}
		}
		if candidate < 0 {
			return
		}
		if len(selected) < limit {
			add(candidate)
			return
		}
		worstPosition, worstRank := -1, math.Inf(1)
		for position, index := range selected {
			if ranked[index]["kind"] == kind {
				continue
			}
			rank := cachedBuyerRankValue(ranked[index], "expected_return", math.Inf(-1))
			score := cachedBuyerRankValue(ranked[index], "score", 0)
			if worstPosition < 0 || rank < worstRank {
				worstPosition = position
				worstRank = rank
				continue
			}
			if rank == worstRank {
				worstScore := cachedBuyerRankValue(ranked[selected[worstPosition]], "score", 0)
				if score < worstScore {
					worstPosition = position
				}
			}
		}
		if worstPosition < 0 {
			return
		}
		removed := selected[worstPosition]
		selected = append(selected[:worstPosition], selected[worstPosition+1:]...)
		delete(seen, removed)
		add(candidate)
	}
	ensure("single")
	ensure("vertical")
	if len(selected) < limit {
		for index := range ranked {
			if len(selected) >= limit {
				break
			}
			add(index)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		left, right := ranked[selected[i]], ranked[selected[j]]
		leftExpected := cachedBuyerRankValue(left, "expected_return", math.Inf(-1))
		rightExpected := cachedBuyerRankValue(right, "expected_return", math.Inf(-1))
		if leftExpected != rightExpected {
			return leftExpected > rightExpected
		}
		return cachedBuyerRankValue(left, "score", 0) > cachedBuyerRankValue(right, "score", 0)
	})
	result := make([]map[string]any, 0, len(selected))
	for _, index := range selected {
		result = append(result, ranked[index])
	}
	return result
}

func cachedBuyerDirectionCandidates(groups []*cachedBuyerGroup, direction string, target float64, targetSource string, spot, horizonYears, touch float64, hasTouch bool) []map[string]any {
	items := make([]map[string]any, 0)
	seenSingles := map[string]bool{}
	seenVerticals := map[string]bool{}
	for _, group := range groups {
		longs := cachedBuyerLongRows(group, direction, spot)
		for _, long := range longs {
			key := group.expiration + ":" + fmtFloat(long.rowStrike(), 6)
			if !seenSingles[key] {
				if item := cachedBuyerSingle(long, direction, target, targetSource, spot, horizonYears, touch, hasTouch); item != nil {
					items = append(items, item)
					seenSingles[key] = true
				}
			}
		}
		for _, long := range longs {
			key := group.expiration + ":" + fmtFloat(long.rowStrike(), 6)
			for _, short := range cachedBuyerShortRows(group, long, direction, target, spot) {
				verticalKey := key + ":" + fmtFloat(short.rowStrike(), 6)
				if seenVerticals[verticalKey] {
					continue
				}
				if item := cachedBuyerVertical(long, short, direction, target, targetSource, spot, horizonYears, touch, hasTouch); item != nil {
					items = append(items, item)
					seenVerticals[verticalKey] = true
				}
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		leftExpected := cachedBuyerRankValue(items[i], "expected_return", math.Inf(-1))
		rightExpected := cachedBuyerRankValue(items[j], "expected_return", math.Inf(-1))
		if leftExpected != rightExpected {
			return leftExpected > rightExpected
		}
		left := cachedBuyerRankValue(items[i], "score", 0)
		right := cachedBuyerRankValue(items[j], "score", 0)
		return left > right
	})
	return cachedBuyerSelectDiverse(items, cachedBuyerDirectionLimit, spot)
}

func cachedBuyerStructures(rows []model.OptionRow, spot float64, recommendation map[string]string, support, resistance []map[string]any, trends ...map[string]any) map[string]any {
	return cachedBuyerStructuresWithIV(rows, spot, recommendation, support, resistance, nil, trends...)
}

func cachedBuyerStructuresWithIV(rows []model.OptionRow, spot float64, recommendation map[string]string, support, resistance []map[string]any, ivModel map[string]any, trends ...map[string]any) map[string]any {
	return cachedBuyerStructuresWithIVAt(rows, spot, recommendation, support, resistance, ivModel, time.Now(), trends...)
}

func cachedBuyerStructuresWithIVAt(rows []model.OptionRow, spot float64, recommendation map[string]string, support, resistance []map[string]any, ivModel map[string]any, now time.Time, trends ...map[string]any) map[string]any {
	horizon := cachedBuyerHorizonDays
	empty := func(reason string) map[string]any {
		return map[string]any{
			"available": false, "status": "unavailable", "direction": nil,
			"horizon_trading_days": horizon, "horizon_label": "未来 5 个交易日",
			"items": []any{}, "reason": reason,
			"method": "布莱克-斯科尔斯模型估算", "quote_source": "server", "iv_source": "server",
			"quote_method": "仅使用有效买卖价中间价",
		}
	}
	if spot <= 0 {
		return empty("缺少有效现价")
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	now = now.In(location)
	horizonEnd := cachedBuyerHorizonEnd(now, horizon)
	workRows := append([]model.OptionRow(nil), rows...)
	if ivModel == nil {
		// Match Python's pipeline: estimate expiration IV from the complete chain
		// before buyer quote deduplication.
		ivModel = gamma.Annotate(workRows, &spot)
	}
	workRows = dedupeCachedBuyerQuotes(workRows)
	for index := range workRows {
		workRows[index].ContractType = strings.ToLower(workRows[index].ContractType)
	}
	type rawCandidate struct {
		row            *model.OptionRow
		mid, spread    float64
		dte            int
		years          float64
		activity       float64
		belowIntrinsic bool
	}
	raw := make([]rawCandidate, 0, len(workRows))
	validQuotedContracts, survivingHorizonContracts := 0, 0
	for index := range workRows {
		row := &workRows[index]
		if row.ContractType != "call" && row.ContractType != "put" || row.Strike == nil || *row.Strike <= 0 || math.IsNaN(*row.Strike) || math.IsInf(*row.Strike, 0) ||
			row.Bid == nil || row.Ask == nil || *row.Bid <= 0 || *row.Ask < *row.Bid ||
			math.IsNaN(*row.Bid) || math.IsInf(*row.Bid, 0) || math.IsNaN(*row.Ask) || math.IsInf(*row.Ask, 0) {
			continue
		}
		dte, eligible, validExpiration := cachedBuyerExpirationEligibility(row.Expiration, now, horizon)
		if !validExpiration {
			continue
		}
		years, validYears := cachedBuyerYears(row.Expiration, now)
		if !validYears {
			continue
		}
		validQuotedContracts++
		if eligible {
			survivingHorizonContracts++
		}
		if !eligible {
			continue
		}
		mid := (*row.Bid + *row.Ask) / 2
		spread := (*row.Ask - *row.Bid) / mid
		if mid <= 0 || math.IsNaN(mid) || math.IsInf(mid, 0) || math.IsNaN(spread) || math.IsInf(spread, 0) || spread >= cachedBuyerMaxSpread {
			continue
		}
		activity := 0.0
		if row.Volume != nil && *row.Volume > 0 {
			activity += float64(*row.Volume)
		}
		if row.OpenInterest != nil && *row.OpenInterest > 0 {
			activity += float64(*row.OpenInterest)
		}
		intrinsic := 0.0
		if row.ContractType == "call" {
			intrinsic = math.Max(spot-*row.Strike, 0)
		} else {
			intrinsic = math.Max(*row.Strike-spot, 0)
		}
		raw = append(raw, rawCandidate{row: row, mid: mid, spread: spread, dte: dte, years: years, activity: activity, belowIntrinsic: mid < intrinsic*.98})
	}
	if len(raw) == 0 {
		if validQuotedContracts > 0 && survivingHorizonContracts == 0 {
			return empty("可用到期日都落在未来 5 个交易日窗口内，暂不生成买方结构")
		}
		if validQuotedContracts > 0 {
			return empty("当前期权链没有满足报价和流动性条件的候选结构")
		}
		return empty("当前缓存缺少有效买卖报价，暂不生成买方结构")
	}
	sweetExpirations := map[string]bool{}
	for _, item := range raw {
		if !item.belowIntrinsic && item.spread < cachedBuyerMaxSpread && item.dte >= cachedBuyerMinDTE && item.dte <= cachedBuyerMaxDTE {
			sweetExpirations[item.row.Expiration] = true
		}
	}
	if len(sweetExpirations) > 0 {
		filtered := raw[:0]
		for _, item := range raw {
			if sweetExpirations[item.row.Expiration] {
				filtered = append(filtered, item)
			}
		}
		raw = filtered
	}
	filtered := raw[:0]
	for _, item := range raw {
		if !item.belowIntrinsic && item.spread < cachedBuyerMaxSpread {
			filtered = append(filtered, item)
		}
	}
	raw = filtered
	if len(raw) == 0 {
		return empty("当前期权链没有满足报价和流动性条件的候选结构")
	}
	groupsByExpiry := map[string]*cachedBuyerGroup{}
	for _, item := range raw {
		iv, source := 0.0, ""
		if solved, ok := cachedBuyerIVFromPrice(item.mid, spot, *item.row.Strike, item.years, item.row.ContractType == "call"); ok && solved > 0 {
			iv, source = solved, "contract"
		}
		if iv <= 0 && item.row.ModelIV != nil && *item.row.ModelIV > 0 && !math.IsNaN(*item.row.ModelIV) && !math.IsInf(*item.row.ModelIV, 0) {
			iv = *item.row.ModelIV
		}
		if iv <= 0 {
			if details, ok := ivModel[item.row.Expiration].(map[string]any); ok {
				iv, _ = cachedBuyerModelIV(details)
			}
		}
		if iv <= 0 || math.IsNaN(iv) || math.IsInf(iv, 0) {
			continue
		}
		delta, theta, vega, ok := cachedBuyerGreeks(spot, *item.row.Strike, iv, item.years, item.row.ContractType == "call")
		if !ok {
			continue
		}
		if source == "" {
			source = "expiration"
		}
		contract := &cachedBuyerContract{row: *item.row, mid: item.mid, spreadRatio: item.spread, dte: item.dte, years: item.years, activity: item.activity, iv: iv, ivSource: source, delta: delta, thetaPerDay: theta, vega: vega}
		group := groupsByExpiry[item.row.Expiration]
		if group == nil {
			group = &cachedBuyerGroup{expiration: item.row.Expiration, dte: item.dte}
			groupsByExpiry[item.row.Expiration] = group
		}
		group.rows = append(group.rows, contract)
	}
	groups := make([]*cachedBuyerGroup, 0, len(groupsByExpiry))
	for _, group := range groupsByExpiry {
		maximum := 0.0
		for _, row := range group.rows {
			maximum = math.Max(maximum, row.activity)
		}
		for _, row := range group.rows {
			if maximum > 0 {
				row.activityScore = math.Min(1, math.Log1p(row.activity)/math.Log1p(maximum))
			}
		}
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return empty("当前期权链没有满足报价和流动性条件的候选结构")
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].dte != groups[j].dte {
			return groups[i].dte < groups[j].dte
		}
		return groups[i].expiration < groups[j].expiration
	})
	horizonYears := horizonEnd.Sub(now).Hours() / (24 * 365)
	if horizonYears <= 0 {
		horizonYears = float64(horizon) / 365
	}
	referenceIV := cachedBuyerReferenceIV(groups, spot)
	action := recommendation["action"]
	primaryDirection := ""
	if action == "buy" {
		primaryDirection = "call"
	} else if action == "sell" {
		primaryDirection = "put"
	}
	directions := []string{"call", "put"}
	callTarget, callSource, callTouch, callHasTouch := cachedBuyerTarget("call", spot, support, resistance, referenceIV, horizonYears, trends...)
	putTarget, putSource, putTouch, putHasTouch := cachedBuyerTarget("put", spot, support, resistance, referenceIV, horizonYears, trends...)
	built := make([]map[string]any, 0)
	if primaryDirection == "call" {
		built = append(built, cachedBuyerDirectionCandidates(groups, "call", callTarget, callSource, spot, horizonYears, callTouch, callHasTouch)...)
		built = append(built, cachedBuyerDirectionCandidates(groups, "put", putTarget, putSource, spot, horizonYears, putTouch, putHasTouch)...)
	} else if primaryDirection == "put" {
		built = append(built, cachedBuyerDirectionCandidates(groups, "put", putTarget, putSource, spot, horizonYears, putTouch, putHasTouch)...)
		built = append(built, cachedBuyerDirectionCandidates(groups, "call", callTarget, callSource, spot, horizonYears, callTouch, callHasTouch)...)
	} else {
		for _, direction := range directions {
			target, source, touch, hasTouch := callTarget, callSource, callTouch, callHasTouch
			if direction == "put" {
				target, source, touch, hasTouch = putTarget, putSource, putTouch, putHasTouch
			}
			built = append(built, cachedBuyerDirectionCandidates(groups, direction, target, source, spot, horizonYears, touch, hasTouch)...)
		}
	}
	if len(built) == 0 {
		return empty("当前期权链没有满足报价和流动性条件的候选结构")
	}
	for _, item := range built {
		direction, _ := item["direction"].(string)
		item["is_primary"] = primaryDirection != "" && direction == primaryDirection
		item["direction_label"] = map[string]string{"call": "看涨方案", "put": "看跌方案"}[direction]
	}
	modelSources := map[string]bool{}
	for _, item := range built {
		if source, ok := item["model_iv_source"].(string); ok {
			modelSources[source] = true
		}
	}
	ivSource := "server_quote"
	if modelSources["到期日模型估算"] {
		ivSource = "server"
		for _, item := range built {
			expiration, _ := item["expiration"].(string)
			details := ivModel[expiration]
			if item, ok := details.(map[string]any); ok {
				if source, _ := item["source"].(string); source == "default" {
					ivSource = "default"
					break
				}
			}
		}
	}
	targetMap := map[string]any{
		"call": map[string]any{"price": callTarget, "source": callSource, "touch_probability": func() any {
			if callHasTouch {
				return roundCached(callTouch, 4)
			}
			return nil
		}()},
		"put": map[string]any{"price": putTarget, "source": putSource, "touch_probability": func() any {
			if putHasTouch {
				return roundCached(putTouch, 4)
			}
			return nil
		}()},
	}
	primary := any(nil)
	if primaryDirection != "" {
		primary = primaryDirection
	}
	direction := "neutral"
	if primaryDirection != "" {
		direction = primaryDirection
	}
	directionLabel := "中性对比：同时观察买入看涨 / 买入看跌"
	if primaryDirection == "call" {
		directionLabel = "主方向：买入看涨 · 同时对比买入看跌"
	} else if primaryDirection == "put" {
		directionLabel = "主方向：买入看跌 · 同时对比买入看涨"
	}
	return map[string]any{
		"available": true, "status": "ok", "direction": direction, "primary_direction": primary,
		"direction_label": directionLabel, "recommendation": recommendation["reason"],
		"horizon_trading_days": horizon, "horizon_label": "未来 5 个交易日", "targets": targetMap,
		"reference_iv": referenceIV, "items": built, "method": "布莱克-斯科尔斯模型估算", "quote_source": "server",
		"iv_source": ivSource, "quote_method": "仅使用有效买卖价中间价，不使用最新成交价代替成本",
		"disclaimer": "综合评分不是历史胜率。排序按触及概率加权，并扣掉半档价差；展示的预计盈亏仍以中间价计算，不扣价差。",
	}
}

// buildCachedLevels computes the Go levels response from normalized history and
// option snapshots. Provider reads and cache coordination stay in the service.
func buildCachedLevels(symbol, expiration string, bars []map[string]any, rows []model.OptionRow, quote model.Quote, spot, candidateSpot *float64, historyFetchedAt any, extremes, beta, earnings map[string]any) map[string]any {
	return buildCachedLevelsAt(symbol, expiration, bars, rows, quote, spot, candidateSpot, historyFetchedAt, extremes, beta, earnings, time.Now(), nil)
}

// fastLevelsPayload keeps the raw response focused on the fields needed to
// render the first screen. The full /api/levels response remains available for
// compatibility, while this subset avoids a second request and a second pass
// through the same cached inputs.
func fastLevelsPayload(levels map[string]any) map[string]any {
	if levels == nil {
		return nil
	}
	result := make(map[string]any, 22)
	for _, key := range []string{
		"symbol", "expiration", "spot", "candidate_spot", "generated_at",
		"chain_fetched_at", "options_fetched_at", "options_expirations", "options_horizon_days", "options_metric",
		"history", "history_bars", "trend_market", "beta", "beta_meta", "earnings", "extremes",
		"resistance", "support", "trend", "recommendation", "trade_points", "stop_loss", "trade_points_horizon",
		"buyer_structures", "plan", "history_validation",
	} {
		if value, ok := levels[key]; ok {
			result[key] = value
		}
	}
	return result
}

func buildCachedLevelsAt(symbol, expiration string, bars []map[string]any, rows []model.OptionRow, quote model.Quote, spot, candidateSpot *float64, historyFetchedAt any, extremes, beta, earnings map[string]any, now time.Time, modelByExpiry map[string]any) map[string]any {
	price := numberPointer(spot)
	if price == nil || *price <= 0 {
		if len(bars) > 0 {
			// Python build_levels falls back only to the latest bar's close.
			price = numberValue(bars[len(bars)-1]["close"])
		}
	}
	if price == nil || *price <= 0 {
		return nil
	}
	anchor := numberPointer(candidateSpot)
	if anchor == nil || *anchor <= 0 {
		anchor = price
	}
	// Estimate contract IV/Gamma before building GEX candidates, matching the
	// Python pipeline's annotate_model_greeks -> aggregate_strikes ordering.
	if modelByExpiry == nil {
		modelByExpiry = gamma.Annotate(rows, anchor)
	}
	// Build technical and option candidates around the stable anchor, then classify
	// them against the displayed spot. This keeps intraday spot changes from
	// rebuilding the candidate source values while preserving side semantics.
	levelsBySide := map[string][]cachedLevel{"above": {}, "below": {}}
	optionLevels, metric := cachedOptionLevelsAt(rows, *anchor, now)
	technical := cachedTechnicalLevels(bars, *anchor)
	technical = append(technical, optionLevels...)
	trend := cachedTrend(bars)
	resistanceWeight, supportWeight := 1.0, 1.0
	if trend != nil {
		if trend["direction"] == "up" {
			resistanceWeight, supportWeight = 0.9, 1.15
		} else if trend["direction"] == "down" {
			resistanceWeight, supportWeight = 1.15, 0.9
		}
	}
	for _, level := range technical {
		if level.price > *anchor {
			levelsBySide["above"] = append(levelsBySide["above"], level)
		} else if level.price < *anchor {
			levelsBySide["below"] = append(levelsBySide["below"], level)
		}
	}
	atr := cachedATR(bars, cachedATRPeriod)
	zoneWidth := math.Max(atr*cachedATRZoneRatio, *anchor*cachedMinZoneRatio)
	mergeTolerance := cachedAdaptiveMergeTolerance(*anchor, atr)
	resistanceAll := mergeCachedLevelsWeighted(levelsBySide["above"], *anchor, true, zoneWidth, mergeTolerance, cachedCandidateLimit, resistanceWeight)
	supportAll := mergeCachedLevelsWeighted(levelsBySide["below"], *anchor, false, zoneWidth, mergeTolerance, cachedCandidateLimit, supportWeight)
	resistanceAll = annotateCachedLevelHistory(resistanceAll, bars, "resistance")
	supportAll = annotateCachedLevelHistory(supportAll, bars, "support")
	resistanceAll = filterCachedSide(resistanceAll, *price, true)
	supportAll = filterCachedSide(supportAll, *price, false)
	ivModel, optionYears := cachedExpiryIVAt(rows, expiration, now)
	if details, ok := modelByExpiry[expiration].(map[string]any); ok {
		if value, ok := details["iv"].(float64); ok && value > 0 {
			ivModel = value
			if optionYears <= 0 {
				optionYears = cachedTouchYears(expiration, now)
			}
		}
	}
	for _, values := range [][]map[string]any{resistanceAll, supportAll} {
		for _, item := range values {
			p, _ := item["price"].(float64)
			item["probability"] = cachedTouchProbability(p, *price, ivModel, optionYears)
		}
	}
	resistance := selectVisibleCachedLevels(resistanceAll, *price, cachedLevelLimit)
	support := selectVisibleCachedLevels(supportAll, *price, cachedLevelLimit)
	planBuy, planAdd := splitCachedSupport(supportAll, *price)
	tradeBuy := cachedBestTradePoint(trend, supportAll, *price, ivModel, optionYears, true)
	tradeSell := cachedBestTradePoint(trend, resistanceAll, *price, ivModel, optionYears, false)
	stopLoss := cachedStopLoss(trend, tradeBuy, supportAll, *price, atr)
	historyFrom, historyTo := cachedBarDates(bars)
	extremesValue := extremes["extremes"]
	betaValue := beta["beta"]
	earningsDates := earnings["dates"]
	earningsAvailable := cachedPayloadAvailable(earningsDates)
	historyMeta := map[string]any{
		"bars": len(bars), "from": historyFrom, "to": historyTo,
		"fetched_at": historyFetchedAt, "source": cachedSource(len(bars)), "warning": cachedWarning(len(bars), historyFetchedAt),
		"extremes_fetched_at": extremes["fetched_at"],
		"extremes_source":     cachedSourceValue(extremesValue),
		"extremes_warning":    cachedWarningValue(extremesValue, "本地没有极值缓存"),
	}
	earningsSummary := summarizeEarnings(earningsDates)
	earningsSummary["fetched_at"] = earnings["fetched_at"]
	earningsSummary["source"] = cachedSourceValue(earningsDates)
	earningsSummary["warning"] = cachedWarningValue(earningsDates, "本地没有财报日期缓存")
	if !earningsAvailable {
		earningsSummary["fetched_at"] = nil
	}
	return map[string]any{
		"symbol": symbol, "expiration": expiration, "spot": *price, "candidate_spot": *anchor,
		"chain_fetched_at": nil, "options_fetched_at": nil, "options_expirations": cachedExpirations(rows),
		"options_horizon_days": 45, "generated_at": now.UTC(), "history": historyMeta,
		"trend_market": trendMarket(bars, quote), "beta": betaValue, "beta_meta": map[string]any{"fetched_at": beta["fetched_at"], "source": cachedSourceValue(betaValue), "warning": cachedWarningValue(betaValue, "本地没有 Beta 缓存")},
		"earnings": earningsSummary, "extremes": extremesValue,
		"extremes_fetched_at": extremes["fetched_at"], "options_metric": metric, "history_bars": len(bars),
		"resistance": resistance, "support": support,
		"trend": trend, "recommendation": cachedRecommendation(trend, support, resistance, *price),
		"trade_points": map[string]any{"buy": tradeBuy, "sell": tradeSell},
		"stop_loss":    stopLoss, "trade_points_horizon": map[string]any{"trading_days": 5, "label": "未来 5 个交易日"},
		"plan":               map[string]any{"buy": planBuy, "add": planAdd, "sell": limitCachedLevels(resistanceAll, cachedLevelLimit)},
		"history_validation": map[string]any{"method": "holdout_price_action", "lookahead_bars": 5, "excluded_recent_bars": 120},
		// Python selects buyer targets from the complete candidate pools. The
		// visible lists are limited to ten rows for the panel, which must not
		// remove a farther but stronger target from option structure selection.
		"buyer_structures": cachedBuyerStructuresWithIVAt(rows, *price, cachedRecommendation(trend, support, resistance, *price), supportAll, resistanceAll, modelByExpiry, now, trend),
	}
}

func numberValue(value any) *float64 {
	switch number := value.(type) {
	case json.Number:
		converted, err := number.Float64()
		if err != nil {
			return nil
		}
		return numberValue(converted)
	case bool:
		converted := 0.0
		if number {
			converted = 1
		}
		return &converted
	case string:
		converted, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		if err != nil {
			return nil
		}
		return numberValue(converted)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil
		}
		return &number
	case float32:
		converted := float64(number)
		return numberValue(converted)
	case int:
		converted := float64(number)
		return &converted
	case int8:
		converted := float64(number)
		return &converted
	case int16:
		converted := float64(number)
		return &converted
	case int32:
		converted := float64(number)
		return &converted
	case int64:
		converted := float64(number)
		return &converted
	case uint:
		converted := float64(number)
		return &converted
	case uint8:
		converted := float64(number)
		return &converted
	case uint16:
		converted := float64(number)
		return &converted
	case uint32:
		converted := float64(number)
		return &converted
	case uint64:
		converted := float64(number)
		return &converted
	case *float64:
		if number == nil || math.IsNaN(*number) || math.IsInf(*number, 0) {
			return nil
		}
		return number
	case *int64:
		if number == nil {
			return nil
		}
		converted := float64(*number)
		return &converted
	}
	return nil
}

func numberPointer(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

func mergeCachedLevels(values []cachedLevel, spot float64, above bool, zoneWidth, tolerance float64, limits ...int) []map[string]any {
	limit := cachedLevelLimit
	if len(limits) > 0 {
		limit = limits[0]
		if limit < 0 {
			limit = 0
		}
	}
	return mergeCachedLevelsWeighted(values, spot, above, zoneWidth, tolerance, limit, 1.0)
}

func mergeCachedLevelsWeighted(values []cachedLevel, spot float64, above bool, zoneWidth, tolerance float64, limit int, trendWeight float64) []map[string]any {
	if spot <= 0 || math.IsNaN(spot) || math.IsInf(spot, 0) {
		return []map[string]any{}
	}
	filtered := make([]cachedLevel, 0, len(values))
	for _, value := range values {
		if value.price <= 0 || (above && value.price <= spot) || (!above && value.price >= spot) {
			continue
		}
		if len(value.factors) == 0 && value.factor != "" {
			value.factors = []string{value.factor}
		}
		filtered = append(filtered, value)
	}
	// Python's sorted(..., key=price) is stable; preserve source order for equal prices.
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].price < filtered[j].price })
	if tolerance == 0 || math.IsNaN(tolerance) || math.IsInf(tolerance, 0) {
		// Python merge_candidates treats an explicit zero as the default
		// MERGE_TOLERANCE (0.5%); non-finite values fail _number and do too.
		tolerance = spot * cachedDefaultMergeRatio
	} else if tolerance < 0 {
		tolerance = 0
	}
	if math.IsNaN(zoneWidth) || math.IsInf(zoneWidth, 0) || zoneWidth < 0 {
		zoneWidth = 0
	}
	zoneWidth = math.Max(zoneWidth, spot*cachedMinZoneRatio)
	type merged struct {
		price, representativeScore, rawLow, rawHigh, score float64
		factors                                            []string
		factorWeights                                      map[string]float64
	}
	groups := make([]merged, 0, len(filtered))
	for _, value := range filtered {
		if len(groups) == 0 || value.price-groups[len(groups)-1].rawLow > tolerance {
			weights := make(map[string]float64, len(value.factors))
			for _, factor := range value.factors {
				weights[factor] = value.score
			}
			groups = append(groups, merged{price: value.price, representativeScore: value.score, rawLow: value.price, rawHigh: value.price, score: math.Max(0, value.score), factors: append([]string(nil), value.factors...), factorWeights: weights})
			continue
		}
		group := &groups[len(groups)-1]
		group.rawHigh = math.Max(group.rawHigh, value.price)
		if value.score > group.representativeScore {
			group.price = value.price
			group.representativeScore = value.score
		}
		group.score += math.Max(0, value.score)
		for _, factor := range value.factors {
			found := false
			for _, existing := range group.factors {
				if existing == factor {
					found = true
					break
				}
			}
			if !found {
				group.factors = append(group.factors, factor)
				group.factorWeights[factor] = value.score
			} else if value.score > group.factorWeights[factor] {
				group.factorWeights[factor] = value.score
			}
		}
	}
	if limit < 0 {
		limit = 0
	}
	result := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		// Match Python merge_candidates: normalize raw evidence first, then apply
		// a small directional bias after composite scoring.
		rawScore := group.score * math.Max(trendWeight, 0)
		if math.IsNaN(rawScore) {
			// Python's max(0.0, NaN) keeps the first argument and treats the
			// resulting composite as zero; do not leak NaN into JSON scores.
			rawScore = 0
		}
		score := math.Min(1, 0.25+0.31*math.Min(1, math.Max(0, rawScore)))
		factorGroups := make(map[string]bool, len(group.factors))
		hasWall, hasAbsorption, hasKeyFib := false, false, false
		for _, factor := range group.factors {
			if strings.Contains(factor, "看涨墙") || strings.Contains(factor, "看跌墙") {
				hasWall = true
			}
			if factor == "承接位" {
				hasAbsorption = true
			}
			if factor == "斐波那契 50%" || factor == "斐波那契 61.8%" {
				hasKeyFib = true
			}
			factorGroups[cachedFactorGroup(factor)] = true
		}
		sort.SliceStable(group.factors, func(i, j int) bool {
			return group.factorWeights[group.factors[i]] > group.factorWeights[group.factors[j]]
		})
		if hasWall {
			score = math.Max(score, .72)
		} else if hasKeyFib {
			score = math.Max(score, .62)
		} else if hasAbsorption {
			score = math.Max(score, .58)
		}
		if len(factorGroups) > 1 {
			score += math.Min(.28, float64(len(factorGroups)-1)*.14)
		}
		if hasAbsorption && len(factorGroups) > 1 {
			score += .04
		}
		score = math.Min(1, math.Max(0, score+(trendWeight-1)*.15))
		tier := "normal"
		if score >= .70 && (hasWall || hasAbsorption || len(factorGroups) >= 2) {
			tier = "strong"
		} else if score >= .55 && (hasWall || hasAbsorption || len(factorGroups) >= 2) {
			tier = "reinforced"
		}
		low := math.Max(0, group.rawLow-zoneWidth)
		high := group.rawHigh + zoneWidth
		result = append(result, map[string]any{"price": roundCached(group.price, 4), "score": score, "model_score": roundCached(score, 2), "strength_tier": tier, "factors": group.factors, "zone_low": roundCached(low, 4), "zone_high": roundCached(high, 4), "probability": nil, "history_samples": 0, "history_hold_rate": nil, "history_break_rate": nil, "history_confidence": 0.0})
	}
	// Python propagates part of a strong level's score to nearby clusters before
	// priority ranking; keep model_score unmodified for downstream confidence.
	baseScores := make([]float64, len(result))
	for i, item := range result {
		baseScores[i] = item["score"].(float64)
	}
	for i, item := range result {
		price, score := item["price"].(float64), baseScores[i]
		for j, other := range result {
			if i == j {
				continue
			}
			otherPrice, otherScore := other["price"].(float64), baseScores[j]
			if price > 0 && math.Abs(otherPrice/price-1) <= .018 && otherScore >= .70 {
				score = math.Max(score, otherScore*.8)
			}
		}
		item["score"] = score
	}
	sort.SliceStable(result, func(i, j int) bool {
		leftPrice, rightPrice := result[i]["price"].(float64), result[j]["price"].(float64)
		leftDistance, rightDistance := math.Abs(leftPrice/spot-1), math.Abs(rightPrice/spot-1)
		leftScore, rightScore := result[i]["score"].(float64), result[j]["score"].(float64)
		leftPriority, rightPriority := leftScore/(1+100*leftDistance), rightScore/(1+100*rightDistance)
		return leftPriority > rightPriority
	})
	if len(result) > limit {
		result = result[:limit]
	}
	for _, item := range result {
		item["score"] = roundCached(item["score"].(float64), 2)
	}
	priceOrder := append([]map[string]any(nil), result...)
	sort.Slice(priceOrder, func(i, j int) bool { return priceOrder[i]["price"].(float64) < priceOrder[j]["price"].(float64) })
	for i := 0; i+1 < len(priceOrder); i++ {
		leftHigh, _ := priceOrder[i]["zone_high"].(float64)
		rightLow, _ := priceOrder[i+1]["zone_low"].(float64)
		if leftHigh > rightLow {
			boundary := roundCached((priceOrder[i]["price"].(float64)+priceOrder[i+1]["price"].(float64))/2, 4)
			priceOrder[i]["zone_high"] = math.Min(leftHigh, boundary)
			priceOrder[i+1]["zone_low"] = math.Max(rightLow, boundary)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return math.Abs(result[i]["price"].(float64)-spot) < math.Abs(result[j]["price"].(float64)-spot)
	})
	return result
}

func annotateCachedLevelHistory(levels []map[string]any, bars []map[string]any, side string) []map[string]any {
	const lookahead, formation, recentWindow = 5, 120, 45
	validationEnd := len(bars) - formation
	if validationEnd < 0 {
		validationEnd = 0
	}
	atr := cachedATR(bars, cachedATRPeriod)
	if atr <= 0 || validationEnd <= lookahead {
		for _, item := range levels {
			item["history_samples"] = 0
			item["history_hold_rate"] = nil
			item["history_break_rate"] = nil
			item["history_reaction_rate"] = nil
			item["history_adjusted_hold_rate"] = nil
			item["history_adjusted_break_rate"] = nil
			item["history_confidence"] = 0.0
			item["validation_method"] = "insufficient_history"
			item["recent_samples"] = 0
			item["recent_reactions"] = 0
			item["recent_reaction_rate"] = nil
			item["strength_tier"] = cachedLevelStrengthTier(item)
		}
		return levels
	}
	atrByIndex := cachedATRSeries(bars, cachedATRPeriod)
	for _, item := range levels {
		centerValue := numberValue(item["price"])
		zoneLowValue := numberValue(item["zone_low"])
		zoneHighValue := numberValue(item["zone_high"])
		if centerValue == nil || *centerValue <= 0 || zoneLowValue == nil || zoneHighValue == nil {
			item["history_samples"] = 0
			item["history_hold_rate"] = nil
			item["history_break_rate"] = nil
			item["history_reaction_rate"] = nil
			item["history_adjusted_hold_rate"] = nil
			item["history_adjusted_break_rate"] = nil
			item["history_confidence"] = 0.0
			item["validation_method"] = "invalid_level"
			item["recent_samples"] = 0
			item["recent_reactions"] = 0
			item["recent_reaction_rate"] = nil
			item["strength_tier"] = cachedLevelStrengthTier(item)
			continue
		}
		center, zoneLow, zoneHigh := *centerValue, *zoneLowValue, *zoneHighValue
		samples, holds, breaks, reactions := 0, 0, 0, 0
		lastTouch := -lookahead
		indices := make([]int, 0)
		for i := 0; i < validationEnd-lookahead; i++ {
			if i-lastTouch < lookahead {
				continue
			}
			low, lok := numericHistoryValue(bars[i]["low"])
			high, hok := numericHistoryValue(bars[i]["high"])
			if !lok || !hok || high < zoneLow || low > zoneHigh {
				continue
			}
			futureLow, futureHigh, futureClose := math.Inf(1), math.Inf(-1), math.Inf(1)
			for j := i + 1; j <= i+lookahead; j++ {
				l, lok := numericHistoryValue(bars[j]["low"])
				h, hok := numericHistoryValue(bars[j]["high"])
				c, cok := numericHistoryValue(bars[j]["close"])
				if lok && l < futureLow {
					futureLow = l
				}
				if hok && h > futureHigh {
					futureHigh = h
				}
				if cok && c < futureClose {
					futureClose = c
				}
			}
			if math.IsInf(futureLow, 1) || math.IsInf(futureHigh, -1) || math.IsInf(futureClose, 1) {
				continue
			}
			lastTouch = i
			samples++
			indices = append(indices, i)
			sampleATR := atr
			if i < len(atrByIndex) && atrByIndex[i] > 0 {
				sampleATR = atrByIndex[i]
			}
			buffer := math.Max(sampleATR*.25, center*cachedMinZoneRatio)
			broken, reacted := false, false
			if side == "support" {
				broken = futureLow < zoneLow-buffer
				maxClose := math.Inf(-1)
				for j := i + 1; j <= i+lookahead; j++ {
					if c, ok := numericHistoryValue(bars[j]["close"]); ok && c > maxClose {
						maxClose = c
					}
				}
				reacted = maxClose >= center+buffer
			} else {
				broken = futureHigh > zoneHigh+buffer
				minClose := math.Inf(1)
				for j := i + 1; j <= i+lookahead; j++ {
					if c, ok := numericHistoryValue(bars[j]["close"]); ok && c < minClose {
						minClose = c
					}
				}
				reacted = minClose <= center-buffer
			}
			if broken {
				breaks++
			} else {
				holds++
			}
			if reacted {
				reactions++
			}
		}
		holdRate, breakRate, reactionRate, adjustedHold, adjustedBreak, confidence := any(nil), any(nil), any(nil), any(nil), any(nil), 0.0
		if samples > 0 {
			holdRate = roundCached(float64(holds)/float64(samples), 4)
			breakRate = roundCached(float64(breaks)/float64(samples), 4)
			reactionRate = roundCached(float64(reactions)/float64(samples), 4)
			adjustedHold = roundCached(float64(holds+2)/float64(samples+3), 4)
			adjustedBreak = roundCached(float64(breaks+1)/float64(samples+3), 4)
			confidence = roundCached(float64(samples)/float64(samples+3), 4)
		}
		item["history_samples"] = samples
		item["history_hold_rate"] = holdRate
		item["history_break_rate"] = breakRate
		item["history_reaction_rate"] = reactionRate
		item["history_adjusted_hold_rate"] = adjustedHold
		item["history_adjusted_break_rate"] = adjustedBreak
		item["history_confidence"] = confidence
		item["validation_method"] = "holdout_price_action"
		recentCutoff := validationEnd - recentWindow
		if recentCutoff < 0 {
			recentCutoff = 0
		}
		recentSamples, recentReactions := 0, 0
		for _, i := range indices {
			if i >= recentCutoff {
				recentSamples++
				if side == "support" {
					maxClose := math.Inf(-1)
					for j := i + 1; j <= i+lookahead; j++ {
						if c, ok := numericHistoryValue(bars[j]["close"]); ok && c > maxClose {
							maxClose = c
						}
					}
					sampleATR := atr
					if i < len(atrByIndex) && atrByIndex[i] > 0 {
						sampleATR = atrByIndex[i]
					}
					if maxClose >= center+math.Max(sampleATR*.25, center*cachedMinZoneRatio) {
						recentReactions++
					}
				} else {
					minClose := math.Inf(1)
					for j := i + 1; j <= i+lookahead; j++ {
						if c, ok := numericHistoryValue(bars[j]["close"]); ok && c < minClose {
							minClose = c
						}
					}
					sampleATR := atr
					if i < len(atrByIndex) && atrByIndex[i] > 0 {
						sampleATR = atrByIndex[i]
					}
					if minClose <= center-math.Max(sampleATR*.25, center*cachedMinZoneRatio) {
						recentReactions++
					}
				}
			}
		}
		item["recent_samples"] = recentSamples
		item["recent_reactions"] = recentReactions
		item["recent_reaction_rate"] = nil
		if recentSamples > 0 {
			item["recent_reaction_rate"] = roundCached(float64(recentReactions)/float64(recentSamples), 4)
		}
		if samples > 0 {
			weight := math.Min(.25, float64(samples)/20)
			base, _ := item["score"].(float64)
			empirical := math.Max(0, math.Min(1, (float64(holds+2)/float64(samples+3))*(1-float64(breaks+1)/float64(samples+3))))
			item["score"] = roundCached(base*(1-weight)+empirical*weight, 2)
		}
		item["strength_tier"] = cachedLevelStrengthTier(item)
	}
	return levels
}

func roundCached(value float64, places int) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', places, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

func cachedLevelStrengthTier(item map[string]any) string {
	scoreValue := numberValue(item["score"])
	score := 0.0
	if scoreValue != nil {
		score = *scoreValue
	}
	modelScoreValue := numberValue(item["model_score"])
	modelScore := score
	if modelScoreValue != nil && *modelScoreValue != 0 {
		modelScore = *modelScoreValue
	}
	factors := cachedFactorStrings(item["factors"])
	groups := make(map[string]bool)
	wall, absorption := false, false
	for _, factor := range factors {
		groups[cachedFactorGroup(factor)] = true
		if strings.Contains(factor, "看涨墙") || strings.Contains(factor, "看跌墙") {
			wall = true
		}
		if factor == "承接位" {
			absorption = true
		}
	}
	if modelScore < .55 || (!wall && !absorption && len(groups) < 2) {
		return "normal"
	}
	samples := 0
	if value := numberValue(item["history_samples"]); value != nil {
		samples = int(*value)
	}
	confidenceValue := numberValue(item["history_confidence"])
	confidence := 0.0
	if confidenceValue != nil {
		confidence = *confidenceValue
	} else if samples > 0 {
		confidence = float64(samples) / float64(samples+3)
	}
	holdValue := numberValue(item["history_adjusted_hold_rate"])
	if holdValue == nil {
		holdValue = numberValue(item["history_hold_rate"])
	}
	breakValue := numberValue(item["history_adjusted_break_rate"])
	if breakValue == nil {
		breakValue = numberValue(item["history_break_rate"])
	}
	hasHistoryRates := holdValue != nil && breakValue != nil
	hold, broken := 0.0, 0.0
	if holdValue != nil {
		hold = *holdValue
	}
	if breakValue != nil {
		broken = *breakValue
	}
	if score >= .7 && samples >= 6 && confidence >= .65 && hasHistoryRates && hold >= .65 && broken <= .25 {
		return "strong"
	}

	// A detected option wall already passed strict prominence and share checks
	// in cachedOptionLevelsAt. With fewer than three historical touches, retain
	// it as a focused level only when its model score is near the strong cutoff.
	// Once enough history exists, the empirical checks below take precedence.
	if wall && samples < 3 && modelScore >= .68 && (!hasHistoryRates || (hold >= .65 && broken <= .35)) {
		return "reinforced"
	}

	// Historical reactions can identify a relevant zone even when it does not
	// qualify as strong. Require a minimum adjusted hold rate as well, so a
	// short bounce cannot reinforce a level that is usually broken.
	reactionValue := numberValue(item["history_reaction_rate"])
	if modelScore >= cachedStrengthCalibratedMinModelScore &&
		samples >= cachedStrengthCalibratedMinSamples && reactionValue != nil &&
		*reactionValue >= cachedStrengthCalibratedMinReaction && hasHistoryRates &&
		hold >= cachedStrengthCalibratedMinHold && broken <= cachedStrengthCalibratedMaxBreak {
		return "reinforced"
	}

	return "normal"
}

func cachedFactorStrings(value any) []string {
	switch factors := value.(type) {
	case []string:
		return factors
	case []any:
		result := make([]string, 0, len(factors))
		for _, factor := range factors {
			if text, ok := factor.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func cachedFactorGroup(factor string) string {
	if strings.Contains(factor, "看涨") || strings.Contains(factor, "看跌") {
		return "options"
	}
	if strings.HasPrefix(factor, "斐波那契") {
		return "fibonacci"
	}
	if factor == "筹码密集" {
		return "chips"
	}
	if factor == "承接位" {
		return "absorption"
	}
	return "other:" + factor
}

func filterCachedSide(values []map[string]any, spot float64, above bool) []map[string]any {
	filtered := make([]map[string]any, 0, len(values))
	for _, item := range values {
		price := 0.0
		if parsed := numberValue(item["price"]); parsed != nil {
			price = *parsed
		}
		if (above && price > spot) || (!above && price < spot) {
			filtered = append(filtered, item)
		}
	}
	distance := func(item map[string]any) float64 {
		price := numberValue(item["price"])
		if price == nil || *price == 0 {
			return math.Abs(spot - spot)
		}
		return math.Abs(*price - spot)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return distance(filtered[i]) < distance(filtered[j]) })
	return filtered
}

func selectVisibleCachedLevels(values []map[string]any, spot float64, limit int) []map[string]any {
	if limit <= 0 {
		return []map[string]any{}
	}
	if len(values) <= limit {
		selected := make([]map[string]any, len(values))
		copy(selected, values)
		return selected
	}
	nearest := append([]map[string]any(nil), values...)
	sort.SliceStable(nearest, func(i, j int) bool {
		return math.Abs(nearest[i]["price"].(float64)/spot-1) < math.Abs(nearest[j]["price"].(float64)/spot-1)
	})
	nearCount := limit
	if nearCount > 5 {
		nearCount = 5
	}
	selected := append([]map[string]any(nil), nearest[:nearCount]...)
	seen := make(map[uintptr]bool, limit)
	for _, item := range selected {
		seen[reflect.ValueOf(item).Pointer()] = true
	}
	strong := append([]map[string]any(nil), values...)
	sort.SliceStable(strong, func(i, j int) bool {
		leftScore, rightScore := strong[i]["score"].(float64), strong[j]["score"].(float64)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return math.Abs(strong[i]["price"].(float64)/spot-1) < math.Abs(strong[j]["price"].(float64)/spot-1)
	})
	for _, item := range strong {
		if len(selected) >= limit {
			break
		}
		strengthTier, hasStrengthTier := item["strength_tier"]
		isStrong := strengthTier == "strong"
		if !hasStrengthTier {
			if score := numberValue(item["score"]); score != nil {
				isStrong = *score >= .7
			}
		}
		identity := reflect.ValueOf(item).Pointer()
		if !isStrong || seen[identity] {
			continue
		}
		selected = append(selected, item)
		seen[identity] = true
	}
	for _, item := range nearest {
		if len(selected) >= limit {
			break
		}
		identity := reflect.ValueOf(item).Pointer()
		if !seen[identity] {
			selected = append(selected, item)
			seen[identity] = true
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return math.Abs(selected[i]["price"].(float64)-spot) < math.Abs(selected[j]["price"].(float64)-spot)
	})
	return selected
}

func splitCachedSupport(values []map[string]any, spots ...float64) ([]map[string]any, []map[string]any) {
	if len(values) == 0 {
		return []map[string]any{}, []map[string]any{}
	}
	spot := 0.0
	if len(spots) > 0 {
		spot = spots[0]
	}
	ordered := append([]map[string]any(nil), values...)
	distance := func(item map[string]any) float64 {
		price := numberValue(item["price"])
		if price == nil || *price == 0 {
			return math.Abs(spot - spot)
		}
		return math.Abs(*price - spot)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return distance(ordered[i]) < distance(ordered[j]) })
	split := (len(ordered) + 1) / 2
	if split > cachedLevelLimit {
		split = cachedLevelLimit
	}
	buy := append([]map[string]any{}, ordered[:split]...)
	add := append([]map[string]any{}, ordered[split:]...)
	sort.SliceStable(add, func(i, j int) bool {
		left, right := numberValue(add[i]["score"]), numberValue(add[j]["score"])
		leftScore, rightScore := 0.0, 0.0
		if left != nil {
			leftScore = *left
		}
		if right != nil {
			rightScore = *right
		}
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return distance(add[i]) < distance(add[j])
	})
	if len(add) > cachedLevelLimit {
		add = add[:cachedLevelLimit]
	}
	sort.SliceStable(add, func(i, j int) bool { return distance(add[i]) < distance(add[j]) })
	return buy, add
}

func limitCachedLevels(values []map[string]any, limit int) []map[string]any {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func cachedExpiryIV(rows []model.OptionRow, expiration string) (float64, float64) {
	return cachedExpiryIVAt(rows, expiration, time.Now())
}

func cachedExpiryIVAt(rows []model.OptionRow, expiration string, now time.Time) (float64, float64) {
	if expiration == "" {
		return 0, 0
	}
	var spot *float64
	for _, row := range rows {
		if row.Expiration == expiration && row.ModelIV != nil && *row.ModelIV > 0 {
			if spot == nil {
				value := *row.ModelIV
				spot = &value
			}
		}
	}
	if spot == nil {
		return 0, 0
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return *spot, 0
	}
	expiry, err := time.ParseInLocation("2006-01-02", expiration, location)
	if err != nil {
		return *spot, 0
	}
	now = now.In(location)
	years := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 16, 0, 0, 0, location).Sub(now).Hours() / (24 * 365)
	return *spot, math.Max(years, 30.0/(365*24*60))
}

// cachedTouchYears keeps probability calculations usable when the selected
// expiration has a modeled IV but the rows were loaded from a raw snapshot
// before their ModelIV fields were persisted. The IV model still defines the
// volatility; expiry time is independent of where that IV came from.
func cachedTouchYears(expiration string, now time.Time) float64 {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	expiry, err := time.ParseInLocation("2006-01-02", expiration, location)
	if err != nil {
		return 0
	}
	years := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 16, 0, 0, 0, location).Sub(now.In(location)).Hours() / (24 * 365)
	return math.Max(years, 30.0/(365*24*60))
}

func cachedTouchProbability(level, spot, volatility, years float64) any {
	if level <= 0 || spot <= 0 || volatility <= 0 || years <= 0 {
		return nil
	}
	st := volatility * math.Sqrt(years)
	if st <= 0 {
		return nil
	}
	distance := math.Abs(math.Log(level / spot))
	cdf := .5 * (1 + math.Erf((-distance/st)/math.Sqrt2))
	return roundCached(math.Min(1, 2*cdf), 4)
}

func cachedBestTradePoint(trend map[string]any, levels []map[string]any, spot, volatility, years float64, buy bool) map[string]any {
	if spot <= 0 {
		return nil
	}
	direction := ""
	if trend != nil {
		direction, _ = trend["direction"].(string)
	}
	best, bestScore := map[string]any(nil), math.Inf(-1)
	for _, row := range levels {
		priceValue := numberValue(row["price"])
		if priceValue == nil || *priceValue <= 0 || (buy && *priceValue >= spot) || (!buy && *priceValue <= spot) {
			continue
		}
		price := *priceValue
		strengthValue := numberValue(row["model_score"])
		strength := 0.0
		if strengthValue != nil {
			strength = *strengthValue
		}
		if strength == 0 {
			if score := numberValue(row["score"]); score != nil {
				strength = *score
			}
		}
		strength = math.Min(1, math.Max(0, strength))
		factors := cachedFactorStrings(row["factors"])
		groups := make(map[string]bool)
		for _, factor := range factors {
			groups[cachedFactorGroup(factor)] = true
		}
		factorAgreement := math.Min(1, float64(len(groups))/3)
		touch := .5
		// Python best_trade_points ranks candidates using a fixed five-trading-day
		// touch horizon, not the selected option expiration's potentially longer term.
		if p, ok := cachedTouchProbability(price, spot, volatility, 5.0/252.0).(float64); ok {
			touch = p
		} else if p := numberValue(row["probability"]); p != nil {
			touch = *p
		}
		distance := math.Abs(price/spot - 1)
		proximity := math.Max(0, 1-math.Min(distance/.2, 1))
		alignment := .62
		if direction == "range" || direction == "" {
			alignment = .78
		}
		if (buy && direction == "up") || (!buy && direction == "down") {
			alignment = 1
		}
		modelConfidence := strength*.4 + factorAgreement*.2 + touch*.15 + proximity*.1 + alignment*.15
		historyHold := numberValue(row["history_adjusted_hold_rate"])
		if historyHold == nil {
			historyHold = numberValue(row["history_hold_rate"])
		}
		historyBreak := numberValue(row["history_adjusted_break_rate"])
		if historyBreak == nil {
			historyBreak = numberValue(row["history_break_rate"])
		}
		sampleConfidence := numberValue(row["history_confidence"])
		if sampleConfidence == nil {
			// Python best_trade_points does not infer confidence from the sample
			// count when the explicit history_confidence field is absent.
			zero := 0.0
			sampleConfidence = &zero
		}
		historyWeight := 0.0
		if historyHold != nil {
			historyWeight = math.Min(.25, math.Max(0, *sampleConfidence)*.25)
		}
		hold := .5
		if historyHold != nil {
			hold = *historyHold
		}
		confidence := modelConfidence*(1-historyWeight) + hold*historyWeight
		confidence = math.Min(1, math.Max(0, confidence))
		if confidence > bestScore {
			bestScore = confidence
			var holdValue, breakValue any
			if historyHold != nil {
				holdValue = *historyHold
			}
			if historyBreak != nil {
				breakValue = *historyBreak
			}
			low, high := numberValue(row["zone_low"]), numberValue(row["zone_high"])
			if low == nil || *low == 0 {
				low = &price
			}
			if high == nil || *high == 0 {
				high = &price
			}
			if *low > *high {
				low, high = high, low
			}
			samples := 0
			if value := numberValue(row["history_samples"]); value != nil {
				samples = int(*value)
			}
			confidenceSample := math.Min(1, math.Max(0, *sampleConfidence))
			if factors == nil {
				factors = []string{}
			}
			best = map[string]any{"price": price, "zone_low": roundCached(*low, 4), "zone_high": roundCached(*high, 4), "confidence": roundCached(confidence, 4), "model_confidence": roundCached(modelConfidence, 4), "history_weight": roundCached(historyWeight, 4), "history_samples": samples, "history_hold_rate": holdValue, "history_break_rate": breakValue, "history_sample_confidence": roundCached(confidenceSample, 4), "factors": factors, "reason": map[bool]string{true: "多因子共振", false: "单一主因子，需结合行情确认"}[len(factors) >= 2]}
		}
	}
	return best
}

func cachedTradePoint(values []map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	value := values[0]
	return map[string]any{"price": value["price"], "zone_low": value["zone_low"], "zone_high": value["zone_high"], "confidence": value["score"], "model_confidence": value["score"], "history_samples": 0, "factors": value["factors"], "reason": "Go 缓存价位估算"}
}

func cachedStopLoss(trend, buy map[string]any, support []map[string]any, spot, atr float64) map[string]any {
	anchor, source := 0.0, ""
	consider := func(value any, label string) {
		price := numberValue(value)
		if price != nil && *price > 0 && *price < spot && *price > anchor {
			anchor, source = *price, label
		}
	}
	if buy != nil {
		value := buy["zone_low"]
		if number := numberValue(value); number == nil || *number <= 0 {
			value = buy["price"]
		}
		consider(value, "近期最佳买入点下沿")
	}
	if trend != nil {
		consider(trend["lower"], "趋势通道下轨")
	}
	for _, level := range support {
		value := level["zone_low"]
		if number := numberValue(value); number == nil || *number <= 0 {
			value = level["price"]
		}
		consider(value, "最近支撑位")
	}
	if anchor <= 0 || anchor >= spot {
		return nil
	}
	buffer := math.Max(math.Max(atr, 0)*.25, spot*.005)
	price := roundCached(anchor-buffer, 4)
	if price <= 0 || price >= spot {
		return nil
	}
	return map[string]any{"price": price, "anchor_price": roundCached(anchor, 4), "source": source, "buffer": roundCached(buffer, 4), "reason": "低于" + source + "并预留波动缓冲"}
}

func cachedRecommendation(trend map[string]any, support, resistance []map[string]any, spot float64) map[string]string {
	hold := func() map[string]string {
		return map[string]string{"action": "hold", "label": "继续持有", "reason": "趋势方向与支撑/压力信号未形成明确共振"}
	}
	if trend == nil || spot <= 0 {
		return map[string]string{"action": "hold", "label": "继续持有", "reason": "历史趋势或价位数据不足，暂不形成明确买卖信号"}
	}
	direction, _ := trend["direction"].(string)
	gap := func(rows []map[string]any, supportSide bool) (float64, bool) {
		if len(rows) == 0 {
			return 0, false
		}
		level, ok := numericHistoryValue(rows[0]["price"])
		if !ok || level <= 0 {
			return 0, false
		}
		value := (level - spot) / spot
		if supportSide {
			value = (spot - level) / spot
		}
		return value, value >= 0
	}
	supportGap, hasSupport := gap(support, true)
	resistanceGap, hasResistance := gap(resistance, false)
	channelPosition, hasChannel := 0.0, false
	lower, lowerOK := numericHistoryValue(trend["lower"])
	upper, upperOK := numericHistoryValue(trend["upper"])
	if lowerOK && upperOK && upper > lower {
		channelPosition = math.Max(0, math.Min(1, (spot-lower)/(upper-lower)))
		hasChannel = true
	}
	act := func(action, reason string) map[string]string {
		label := "推荐买入"
		if action == "sell" {
			label = "推荐卖出"
		}
		return map[string]string{"action": action, "label": label, "reason": reason}
	}
	switch direction {
	case "up":
		if hasSupport && supportGap <= .025 && (!hasChannel || channelPosition <= .6) {
			return act("buy", "上行趋势，现价接近支撑位")
		}
		if hasResistance && resistanceGap <= .015 && hasChannel && channelPosition >= .75 {
			return act("sell", "上行通道上沿附近，现价接近压力位")
		}
	case "down":
		if hasResistance && resistanceGap <= .025 && (!hasChannel || channelPosition >= .4) {
			return act("sell", "下行趋势，现价接近压力位")
		}
	default:
		if hasSupport && supportGap <= .015 && (!hasResistance || supportGap <= resistanceGap) {
			return act("buy", "震荡区间下沿，现价接近支撑位")
		}
		if hasResistance && resistanceGap <= .015 && (!hasSupport || resistanceGap < supportGap) {
			return act("sell", "震荡区间上沿，现价接近压力位")
		}
	}
	return hold()
}

func cachedTrend(bars []map[string]any) map[string]any {
	values := make([]float64, 0, len(bars))
	for _, bar := range bars {
		if value := numberValue(bar["close"]); value != nil && *value > 0 {
			values = append(values, *value)
		}
	}
	const longWindow, recentWindow = 60, 20
	if len(values) < recentWindow {
		return nil
	}
	longValues := values
	if len(longValues) > longWindow {
		longValues = longValues[len(longValues)-longWindow:]
	}
	recentValues := values
	if len(recentValues) > recentWindow {
		recentValues = recentValues[len(recentValues)-recentWindow:]
	}
	longFit := cachedTrendFit(longValues)
	recentFit := cachedTrendFit(recentValues)
	if longFit == nil || recentFit == nil {
		return nil
	}
	const slopeThreshold, minChange, minRebound = 0.05, 0.05, 0.08
	longDirection := "range"
	if longFit.slopePercent > slopeThreshold {
		longDirection = "up"
	} else if longFit.slopePercent < -slopeThreshold {
		longDirection = "down"
	}
	recentUp := recentFit.slopePercent > slopeThreshold
	recentDown := recentFit.slopePercent < -slopeThreshold
	recentChange := recentValues[len(recentValues)-1]/recentValues[0] - 1
	minimum, maximum := recentValues[0], recentValues[0]
	for _, value := range recentValues[1:] {
		minimum = math.Min(minimum, value)
		maximum = math.Max(maximum, value)
	}
	rebound := recentValues[len(recentValues)-1]/minimum - 1
	pullback := maximum/recentValues[len(recentValues)-1] - 1
	reversalUp := recentUp && recentChange >= minChange && rebound >= minRebound
	reversalDown := recentDown && recentChange <= -minChange && pullback >= minRebound
	selected := longFit
	direction, label := longDirection, "区间震荡 · 方向待定"
	if longDirection == "up" {
		label = "上行通道 · 上涨趋势"
	} else if longDirection == "down" {
		label = "下行通道 · 下跌趋势"
	}
	reversal := false
	if reversalUp && longDirection != "up" {
		selected, direction, label, reversal = recentFit, "up", "反弹上行 · 上涨趋势", true
	} else if reversalDown && longDirection != "down" {
		selected, direction, label, reversal = recentFit, "down", "短线转弱 · 下跌趋势", true
	}
	return map[string]any{
		"direction": direction, "label": label,
		"slope_percent": math.RoundToEven(selected.slopePercent*1000) / 1000,
		"upper":         selected.upper, "lower": selected.lower, "bars": selected.bars,
		"background_direction": longDirection, "reversal_confirmed": reversal,
		"rsi": cachedRSI(values, 14),
	}
}

func cachedTrendFit(values []float64) *cachedTrendFitResult {
	if len(values) < 20 {
		return nil
	}
	count := float64(len(values))
	meanX := (count - 1) / 2
	meanY := 0.0
	for _, value := range values {
		meanY += value
	}
	meanY /= count
	if meanY <= 0 {
		return nil
	}
	variance, covariance := 0.0, 0.0
	for index, value := range values {
		x := float64(index)
		variance += (x - meanX) * (x - meanX)
		covariance += (x - meanX) * (value - meanY)
	}
	if variance <= 0 {
		return nil
	}
	slope := covariance / variance
	intercept := meanY - slope*meanX
	deviation := 0.0
	for index, value := range values {
		residual := value - (intercept + slope*float64(index))
		deviation += residual * residual
	}
	deviation = math.Sqrt(deviation / count)
	fitted := intercept + slope*(count-1)
	return &cachedTrendFitResult{
		slopePercent: slope / meanY * 100,
		upper:        math.RoundToEven((fitted+1.5*deviation)*10000) / 10000,
		lower:        math.RoundToEven((fitted-1.5*deviation)*10000) / 10000,
		bars:         len(values),
	}
}

func cachedRSI(values []float64, period int) map[string]any {
	if period < 1 || len(values) < period+1 {
		return nil
	}
	gain, loss := 0.0, 0.0
	for index := 1; index <= period; index++ {
		delta := values[index] - values[index-1]
		gain += math.Max(delta, 0)
		loss += math.Max(-delta, 0)
	}
	averageGain, averageLoss := gain/float64(period), loss/float64(period)
	for index := period + 1; index < len(values); index++ {
		delta := values[index] - values[index-1]
		currentGain, currentLoss := math.Max(delta, 0), math.Max(-delta, 0)
		averageGain = (averageGain*float64(period-1) + currentGain) / float64(period)
		averageLoss = (averageLoss*float64(period-1) + currentLoss) / float64(period)
	}
	reading := 50.0
	if averageLoss == 0 && averageGain > 0 {
		reading = 100
	} else if averageLoss > 0 {
		reading = 100 - 100/(1+averageGain/averageLoss)
	}
	state, label := "neutral", "中性"
	if reading >= 70 {
		state, label = "overbought", "超买"
	} else if reading <= 30 {
		state, label = "oversold", "超卖"
	}
	return map[string]any{"value": math.RoundToEven(reading*10) / 10, "period": period, "state": state, "label": label}
}

func cachedExpirations(rows []model.OptionRow) []string {
	seen := map[string]bool{}
	values := []string{}
	for _, row := range rows {
		if row.Expiration != "" && !seen[row.Expiration] {
			seen[row.Expiration] = true
			values = append(values, row.Expiration)
		}
	}
	sort.Strings(values)
	return values
}
func cachedBarDates(bars []map[string]any) (any, any) {
	if len(bars) == 0 {
		return nil, nil
	}
	return bars[0]["date"], bars[len(bars)-1]["date"]
}
func cachedSource(count int) string {
	if count > 0 {
		return "sqlite"
	}
	return "none"
}
func cachedWarning(count int, fetchedAt any) any {
	if count > 0 && fetchedAt != nil {
		return nil
	}
	return "本地没有日线缓存"
}
func cachedPayloadAvailable(value any) bool {
	switch item := value.(type) {
	case nil:
		return false
	case []any:
		return len(item) > 0 && cachedPayloadShapeValid(item)
	case []string:
		return len(item) > 0
	case map[string]any:
		return len(item) > 0 && cachedPayloadShapeValid(item)
	case float64:
		return !math.IsNaN(item) && !math.IsInf(item, 0)
	case float32:
		return !math.IsNaN(float64(item)) && !math.IsInf(float64(item), 0)
	case int, int64, int32, uint, uint64, uint32:
		return true
	default:
		// Unknown scalar/object shapes are not a valid cached payload. This
		// prevents malformed JSON values from being reported as sqlite data.
		return false
	}
}

func cachedPayloadShapeValid(value any) bool {
	switch item := value.(type) {
	case nil, string, bool:
		return true
	case float64:
		return !math.IsNaN(item) && !math.IsInf(item, 0)
	case float32:
		return !math.IsNaN(float64(item)) && !math.IsInf(float64(item), 0)
	case int, int64, int32, uint, uint64, uint32:
		return true
	case []string:
		return true
	case []any:
		for _, child := range item {
			if !cachedPayloadShapeValid(child) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, child := range item {
			if !cachedPayloadShapeValid(child) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func cachedSourceValue(value any) string {
	if cachedPayloadAvailable(value) {
		return "sqlite"
	}
	return "none"
}
func cachedWarningValue(value any, warning string) any {
	if cachedPayloadAvailable(value) {
		return nil
	}
	return warning
}
