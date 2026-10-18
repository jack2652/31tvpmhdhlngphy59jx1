package chananalysis

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"option-scope-go/internal/marketcalendar"
)

// ThresholdCalibration is an exploratory, time-forward measurement of the
// directional close-to-close return after first observing a class-one signal.
// It does not model execution, costs, position sizing, or a trading strategy.
type ThresholdCalibration struct {
	Threshold          float64              `json:"threshold"`
	Train              CalibrationPartition `json:"train"`
	OutOfSample        CalibrationPartition `json:"out_of_sample"`
	FormingTrain       CalibrationPartition `json:"forming_train,omitempty"`
	FormingOutOfSample CalibrationPartition `json:"forming_out_of_sample,omitempty"`
	FormingLifecycle   FormingLifecycle     `json:"forming_lifecycle,omitempty"`
}

// CalibrationOptions enables exploratory samples based on segments that were
// still forming at the replay prefix. Such samples are reported separately
// and never enter production analysis or the confirmed partitions.
type CalibrationOptions struct {
	AllowFormingSegments bool
}

type FormingLifecycle struct {
	FirstObserved int `json:"first_observed_candidates"`
	Confirmed     int `json:"later_confirmed"`
	Withdrawn     int `json:"later_withdrawn"`
	StillForming  int `json:"still_forming_at_end"`
}

type formingCandidateState struct {
	Observation calibrationObservation
	LastSeen    int
	Resolved    string
}

func classifyFormingLifecycle(candidates map[string]*formingCandidateState) FormingLifecycle {
	result := FormingLifecycle{FirstObserved: len(candidates)}
	for _, candidate := range candidates {
		switch candidate.Resolved {
		case "confirmed":
			result.Confirmed++
		case "withdrawn":
			result.Withdrawn++
		default:
			result.StillForming++
		}
	}
	return result
}

type CalibrationPartition struct {
	Signals  int                    `json:"signals"`
	Horizons map[int]HorizonMeasure `json:"horizons"`
}

type HorizonMeasure struct {
	Samples     int             `json:"samples"`
	Wins        *int            `json:"wins,omitempty"`
	WinRate     *float64        `json:"directional_win_rate,omitempty"`
	WinRateCI95 *WilsonInterval `json:"directional_win_rate_ci95,omitempty"`
	Mean        *float64        `json:"mean_directional_return_percent,omitempty"`
	Median      *float64        `json:"median_directional_return_percent,omitempty"`
	P10         *float64        `json:"p10_directional_return_percent,omitempty"`
	P90         *float64        `json:"p90_directional_return_percent,omitempty"`
	MaxDrawdown *float64        `json:"max_directional_drawdown_percent,omitempty"`
	Inference   string          `json:"inference"`
}

type WilsonInterval struct {
	Lower      float64 `json:"lower"`
	Upper      float64 `json:"upper"`
	Confidence float64 `json:"confidence"`
}

type calibrationObservation struct {
	date string
	side string
}

// CalibrateDivergenceThresholds replays each candidate threshold one closed
// bar at a time. A signal is captured only on its first visible prefix; its
// subsequent close returns are measured at fixed bar horizons. The split is
// chronological (70% train, 30% holdout), so holdout outcomes do not determine
// whether a signal existed.
func CalibrateDivergenceThresholds(symbol string, bars []Bar, thresholds []float64) ([]ThresholdCalibration, error) {
	return CalibrateDivergenceThresholdsWithOptions(symbol, bars, thresholds, CalibrationOptions{})
}

// CalibrateDivergenceThresholdsWithOptions replays each candidate threshold
// using strict production signals. When AllowFormingSegments is enabled, an
// additional prefix-only exploration is reported separately. It never changes
// the strict signal path used by production analysis.
func CalibrateDivergenceThresholdsWithOptions(symbol string, bars []Bar, thresholds []float64, options CalibrationOptions) ([]ThresholdCalibration, error) {
	if !isChronologicalUnique(bars) {
		return nil, fmt.Errorf("calibration input must be unique and chronological")
	}
	for _, threshold := range thresholds {
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold >= 1 {
			return nil, fmt.Errorf("invalid divergence threshold %v: expected between 0 and 1", threshold)
		}
	}
	// Calibrate over the same completed, quality-checked adjusted bars that
	// production analysis accepts. In particular, one malformed factor pair
	// must be rejected and start a later data chain rather than aborting the
	// entire historical replay.
	validated, _, _, _, _, _ := validatedAdjustedBars(bars, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC))
	if len(validated) < 100 {
		return nil, fmt.Errorf("calibration requires at least 100 accepted completed adjusted bars; got %d", len(validated))
	}
	bars = validated
	state := NewIncrementalAnalyzer(symbol)
	return calibrateDivergencePrefixes(bars, thresholds, options, func(end int) (Frame, error) {
		location, err := time.LoadLocation("America/New_York")
		if err != nil {
			location = time.UTC
		}
		day, err := time.ParseInLocation(time.DateOnly, bars[end-1].Date, location)
		if err != nil {
			return Frame{}, fmt.Errorf("invalid calibration date %q", bars[end-1].Date)
		}
		result, err := state.Append(bars[end-1], day.Add(time.Duration(marketcalendar.RegularCloseMinute(day))*time.Minute))
		return result.Daily, err
	})
}

// CalibrateIntradayDivergenceThresholds replays aggregated, closed intraday
// bars using production structure builders. The caller must create timeframe
// bars through AggregateIntradayBars so incomplete source buckets are excluded.
func CalibrateIntradayDivergenceThresholds(period string, bars []Bar, thresholds []float64, options CalibrationOptions) ([]ThresholdCalibration, error) {
	if period != "15m" && period != "30m" && period != "60m" && period != "1h" && period != "2h" && period != "4h" {
		return nil, fmt.Errorf("unsupported calibration period %q", period)
	}
	if !isChronologicalUnique(bars) {
		return nil, fmt.Errorf("calibration input must be unique and chronological")
	}
	for _, threshold := range thresholds {
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold >= 1 {
			return nil, fmt.Errorf("invalid divergence threshold %v: expected between 0 and 1", threshold)
		}
	}
	accepted := make([]Bar, 0, len(bars))
	for _, bar := range bars {
		if !finitePositive(bar.Open) || !finitePositive(bar.High) || !finitePositive(bar.Low) || !finitePositive(bar.Close) || bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			continue
		}
		accepted = append(accepted, bar)
	}
	minimum := 100
	if period == "4h" {
		minimum = macdWarmupBars + 2
	}
	if len(accepted) < minimum {
		return nil, fmt.Errorf("%s calibration requires at least %d accepted closed bars; got %d", period, minimum, len(accepted))
	}
	return calibrateDivergencePrefixes(accepted, thresholds, options, func(end int) (Frame, error) {
		prefix := accepted[:end]
		frame := analyzeFrame(prefix, period, intradayPeriodLabel(period), nil)
		return addRecursiveAnalysis(frame, prefix), nil
	})
}

type calibrationPrefix func(end int) (Frame, error)

func calibrateDivergencePrefixes(bars []Bar, thresholds []float64, options CalibrationOptions, replay calibrationPrefix) ([]ThresholdCalibration, error) {
	cutoffIndex := len(bars) * 7 / 10
	if cutoffIndex < 1 || cutoffIndex >= len(bars) {
		return nil, fmt.Errorf("invalid chronological calibration split")
	}
	results := make([]ThresholdCalibration, 0, len(thresholds))
	observationsByThreshold := make([][]calibrationObservation, len(thresholds))
	formingObservationsByThreshold := make([][]calibrationObservation, len(thresholds))
	formingSeenByThreshold := make([]map[string]*formingCandidateState, len(thresholds))
	activeFormingByThreshold := make([]map[string]*formingCandidateState, len(thresholds))
	formingEpisodeCounts := make([]map[string]int, len(thresholds))
	seenByThreshold := make([]map[string]bool, len(thresholds))
	for i := range thresholds {
		seenByThreshold[i] = map[string]bool{}
		formingSeenByThreshold[i] = map[string]*formingCandidateState{}
		activeFormingByThreshold[i] = map[string]*formingCandidateState{}
		formingEpisodeCounts[i] = map[string]int{}
	}
	for end := 1; end <= len(bars); end++ {
		prefix := bars[:end]
		frame, replayErr := replay(end)
		if replayErr != nil {
			return nil, fmt.Errorf("calibration replay at %s: %w", prefix[len(prefix)-1].Date, replayErr)
		}
		for thresholdIndex, threshold := range thresholds {
			signals := buildV4SignalsWithDivergenceRatio(frame, frame.RecursiveLevels, frame.Strengths, threshold)
			strictIDs := make(map[string]bool, len(signals))
			for _, signal := range signals {
				if signal.Kind != "class1_buy" && signal.Kind != "class1_sell" || signal.SignalID == "" {
					continue
				}
				identity := calibrationSignalIdentity(signal, frame.RecursiveLevels)
				if identity == "" {
					identity = signal.SignalID
				}
				strictIDs[identity] = true
				if !seenByThreshold[thresholdIndex][identity] {
					seenByThreshold[thresholdIndex][identity] = true
					observationsByThreshold[thresholdIndex] = append(observationsByThreshold[thresholdIndex], calibrationObservation{date: prefix[len(prefix)-1].Date, side: signal.Side})
				}
				if candidate := activeFormingByThreshold[thresholdIndex][identity]; candidate != nil && candidate.Resolved == "" {
					candidate.Resolved = "confirmed"
					delete(activeFormingByThreshold[thresholdIndex], identity)
				}
			}
			if !options.AllowFormingSegments {
				continue
			}
			formingSignals, formingLevels := exploratoryFormingAnalysis(frame, prefix, threshold)
			formingSides := make(map[string]string, len(formingSignals))
			for _, signal := range formingSignals {
				if signal.SignalID == "" {
					continue
				}
				identity := calibrationSignalIdentity(signal, formingLevels)
				if identity == "" {
					identity = signal.SignalID
				}
				formingSides[identity] = signal.Side
			}
			trackFormingCandidateEpisodes(activeFormingByThreshold[thresholdIndex], formingSeenByThreshold[thresholdIndex], formingEpisodeCounts[thresholdIndex], strictIDs, formingSides, prefix[len(prefix)-1].Date, end-1)
		}
	}
	for thresholdIndex, threshold := range thresholds {
		item := ThresholdCalibration{Threshold: threshold, Train: measureCalibration(observationsByThreshold[thresholdIndex], bars, cutoffIndex, true), OutOfSample: measureCalibration(observationsByThreshold[thresholdIndex], bars, cutoffIndex, false)}
		if options.AllowFormingSegments {
			for _, candidate := range formingSeenByThreshold[thresholdIndex] {
				formingObservationsByThreshold[thresholdIndex] = append(formingObservationsByThreshold[thresholdIndex], candidate.Observation)
			}
			item.FormingTrain = measureCalibration(formingObservationsByThreshold[thresholdIndex], bars, cutoffIndex, true)
			item.FormingOutOfSample = measureCalibration(formingObservationsByThreshold[thresholdIndex], bars, cutoffIndex, false)
			item.FormingLifecycle = classifyFormingLifecycle(formingSeenByThreshold[thresholdIndex])
		}
		results = append(results, item)
	}
	return results, nil
}

func trackFormingCandidateEpisodes(active, episodes map[string]*formingCandidateState, episodeCounts map[string]int, strictIDs map[string]bool, formingSides map[string]string, observedAt string, barIndex int) {
	for identity := range strictIDs {
		if candidate := active[identity]; candidate != nil && candidate.Resolved == "" {
			candidate.Resolved = "confirmed"
			delete(active, identity)
		}
	}
	current := make(map[string]bool, len(formingSides))
	for identity, side := range formingSides {
		if strictIDs[identity] {
			continue
		}
		current[identity] = true
		if candidate := active[identity]; candidate == nil {
			episodeCounts[identity]++
			episodeKey := fmt.Sprintf("%s#%d", identity, episodeCounts[identity])
			candidate := &formingCandidateState{Observation: calibrationObservation{date: observedAt, side: side}, LastSeen: barIndex}
			episodes[episodeKey] = candidate
			active[identity] = candidate
		} else if candidate.Resolved == "" {
			candidate.LastSeen = barIndex
		}
	}
	for identity, candidate := range active {
		if candidate.Resolved != "" || current[identity] {
			continue
		}
		candidate.Resolved = "withdrawn"
		delete(active, identity)
	}
}

// exploratoryFormingSignals creates a private, prefix-bounded copy of the
// structure graph and provisionally closes its currently forming segments at
// this replay timestamp. This is only a candidate generator: returned values
// are never attached to Result or used by production handlers.
func exploratoryFormingAnalysis(frame Frame, bars []Bar, threshold float64) ([]V4Signal, []LevelAnalysis) {
	if len(bars) == 0 {
		return nil, nil
	}
	levels := exploratoryLevels(frame, bars[len(bars)-1].Date)
	if len(levels) == 0 {
		return nil, nil
	}
	signals := buildV4SignalsWithDivergenceRatio(frame, levels, buildStrengthsFromMACD(bars, levels, frame.MACD), threshold)
	out := make([]V4Signal, 0, len(signals))
	for _, signal := range signals {
		if signal.Kind != "class1_buy" && signal.Kind != "class1_sell" {
			continue
		}
		signal.Status = "forming_candidate"
		signal.Phase = "forming_exploration"
		signal.ObservedAt = bars[len(bars)-1].Date
		signal.ConfirmedAt = ""
		signal.PhaseAt = bars[len(bars)-1].Date
		out = append(out, signal)
	}
	return out, levels
}

func exploratoryLevels(frame Frame, asOf string) []LevelAnalysis {
	if len(frame.Segments) == 0 {
		return nil
	}
	segments := append([]Segment(nil), frame.Segments...)
	hadForming := false
	for index := range segments {
		if segments[index].Confirmed && !segments[index].Forming {
			continue
		}
		segments[index].Confirmed = true
		segments[index].Forming = false
		segments[index].ConfirmedAt = asOf
		hadForming = true
	}
	if !hadForming {
		return nil
	}
	levels := buildRecursiveLevels(frame.StrokesV4, segments)
	for levelIndex := range levels {
		for segmentIndex := range levels[levelIndex].Segments {
			segment := &levels[levelIndex].Segments[segmentIndex]
			if segment.ConfirmedAt == asOf && segment.Forming {
				segment.ConfirmedAt = asOf
			}
		}
	}
	return levels
}

func calibrationSignalIdentity(signal V4Signal, levels []LevelAnalysis) string {
	for _, level := range levels {
		if level.Level != signal.Level || len(signal.StructureRefs) < 3 {
			continue
		}
		indices := signal.StructureRefs[len(signal.StructureRefs)-3:]
		parts := make([]string, 0, len(indices))
		valid := true
		for _, index := range indices {
			if index < 0 || index >= len(level.Segments) {
				valid = false
				break
			}
			segment := level.Segments[index]
			parts = append(parts, fmt.Sprintf("%d:%s:%s", segment.Chain, segment.Direction, segment.Start))
		}
		if valid {
			return fmt.Sprintf("%s|L%d|%s|%s", RecursivePreviewVersion, signal.Level, signal.Kind, strings.Join(parts, "|"))
		}
	}
	return ""
}

func measureCalibration(observations []calibrationObservation, bars []Bar, cutoff int, train bool) CalibrationPartition {
	result := CalibrationPartition{Horizons: map[int]HorizonMeasure{5: {Inference: "no_outcomes"}, 10: {Inference: "no_outcomes"}, 20: {Inference: "no_outcomes"}}}
	returns := map[int][]float64{5: {}, 10: {}, 20: {}}
	paths := map[int][][]float64{5: {}, 10: {}, 20: {}}
	for _, observation := range observations {
		observedIndex := barIndexAtOrAfter(bars, observation.date)
		// The signal becomes visible only after the observation bar closes.
		// Measure outcomes from the next bar's close so the baseline is the
		// earliest full close-to-close period after observation.
		index := observedIndex + 1
		if observedIndex < 0 || index >= len(bars) || train && observedIndex >= cutoff || !train && observedIndex < cutoff {
			continue
		}
		result.Signals++
		for _, horizon := range []int{5, 10, 20} {
			if index+horizon >= len(bars) || train && index+horizon >= cutoff || !train && index < cutoff {
				continue
			}
			start, end := bars[index].Close, bars[index+horizon].Close
			if !finitePositive(start) || !finitePositive(end) {
				continue
			}
			direction := 1.0
			if observation.side == "sell" {
				direction = -1
			}
			value := direction * (end/start - 1) * 100
			measure := result.Horizons[horizon]
			// Wins is a descriptive count, so preserve an explicit zero when all
			// observed returns are non-positive. The rate and interval remain
			// withheld below when the sample is too small.
			if measure.Wins == nil {
				wins := 0
				measure.Wins = &wins
			}
			measure.Samples++
			if value > 0 {
				*measure.Wins++
			}
			mean := 0.0
			if measure.Mean != nil {
				mean = *measure.Mean
			}
			mean += value
			measure.Mean = &mean
			result.Horizons[horizon] = measure
			returns[horizon] = append(returns[horizon], value)
			path := directionalPath(bars, index, horizon, direction)
			if len(path) > 0 {
				paths[horizon] = append(paths[horizon], path)
			}
		}
	}
	for horizon, measure := range result.Horizons {
		if measure.Samples > 0 {
			mean := *measure.Mean / float64(measure.Samples)
			measure.Mean = &mean
			sort.Float64s(returns[horizon])
			middle := len(returns[horizon]) / 2
			median := 0.0
			if len(returns[horizon])%2 == 0 {
				median = (returns[horizon][middle-1] + returns[horizon][middle]) / 2
			} else {
				median = returns[horizon][middle]
			}
			measure.Median = &median
			p10, p90 := percentile(returns[horizon], .10), percentile(returns[horizon], .90)
			measure.P10, measure.P90 = &p10, &p90
			maxDrawdown := maxDirectionalDrawdown(paths[horizon])
			measure.MaxDrawdown = &maxDrawdown
			if measure.Samples < 30 {
				measure.Inference = "insufficient_sample_no_win_rate_claim"
			} else {
				wins := 0
				if measure.Wins != nil {
					wins = *measure.Wins
				}
				rate := float64(wins) / float64(measure.Samples)
				measure.WinRate = &rate
				measure.WinRateCI95 = wilsonInterval(wins, measure.Samples)
				measure.Inference = "descriptive_wilson_95"
			}
		}
		result.Horizons[horizon] = measure
	}
	return result
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if quantile < 0 {
		quantile = 0
	} else if quantile > 1 {
		quantile = 1
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func directionalPath(bars []Bar, start, horizon int, direction float64) []float64 {
	if start < 0 || horizon < 1 || start+horizon >= len(bars) || !finitePositive(bars[start].Close) {
		return nil
	}
	base := bars[start].Close
	path := make([]float64, 0, horizon+1)
	path = append(path, 0)
	for index := start + 1; index <= start+horizon; index++ {
		if !finitePositive(bars[index].Close) {
			return nil
		}
		path = append(path, direction*(bars[index].Close/base-1)*100)
	}
	return path
}

func maxDirectionalDrawdown(paths [][]float64) float64 {
	maximum := 0.0
	for _, path := range paths {
		peak := 0.0
		for _, value := range path {
			if value > peak {
				peak = value
			}
			drawdown := value - peak
			if drawdown < maximum {
				maximum = drawdown
			}
		}
	}
	return maximum
}

func wilsonInterval(wins, samples int) *WilsonInterval {
	if samples <= 0 {
		return nil
	}
	z := 1.959963984540054
	n := float64(samples)
	p := float64(wins) / n
	z2 := z * z
	denominator := 1 + z2/n
	center := (p + z2/(2*n)) / denominator
	half := z * math.Sqrt((p*(1-p)+z2/(4*n))/n) / denominator
	return &WilsonInterval{Lower: math.Max(0, center-half), Upper: math.Min(1, center+half), Confidence: 0.95}
}
