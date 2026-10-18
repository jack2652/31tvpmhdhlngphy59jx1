package chananalysis

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
	"unsafe"

	"option-scope-go/internal/marketcalendar"
)

// BenchmarkIncrementalAppendVsFullRecompute compares one append against one
// full recomputation at equal history sizes. The append state is warmed once;
// each measured append gets an isolated copy prepared outside the timed
// region, so setup does not repeatedly replay the whole history. Reported
// B/op is transient allocation during the operation. retained_bytes/state is
// an estimate of the analyzer's retained input/component slices; the prior
// public Result snapshot is excluded because each Append replaces it.
func BenchmarkIncrementalAppendVsFullRecompute(b *testing.B) {
	for _, count := range []int{252, 1255} {
		history := incrementalBenchmarkBars(count)
		last := history[len(history)-1]
		location, _ := time.LoadLocation("America/New_York")
		day, _ := time.ParseInLocation(time.DateOnly, last.Date, location)
		next := day.AddDate(0, 0, 1)
		for !marketcalendar.IsTradingDay(next) {
			next = next.AddDate(0, 0, 1)
		}
		price := last.Close * (1 + 0.002*math.Sin(float64(count)))
		appendBar := adjustedReplayBar(next.Format(time.DateOnly), price)
		cutoff := dayForBarClose(appendBar.Date, location)
		b.Run(fmt.Sprintf("bars_%d/full", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = AnalyzeAdjusted("BENCH", append(history, appendBar), nil, cutoff)
			}
			b.ReportMetric(float64(count+1), "input_bars/op")
			b.ReportMetric(0, "state_reuse/op")
		})
		b.Run(fmt.Sprintf("bars_%d/append", count), func(b *testing.B) {
			b.ReportAllocs()
			b.StopTimer()
			state := NewIncrementalAnalyzer("BENCH")
			for _, bar := range history {
				if _, err := state.Append(bar, dayForBarClose(bar.Date, location)); err != nil {
					b.Fatal(err)
				}
			}
			retainedStateBytes := incrementalAnalyzerRetainedBytes(state)
			previousSnapshotBytes := mustJSONSize(b, state.lastResult)
			// The retained state contains the prior bars; each timed Append only
			// accepts one new bar. Allocation metrics describe transient bytes
			// allocated by Append, not the retained analyzer heap.
			b.ResetTimer()
			var lastComponentReuseDelta, lastResultCacheHitDelta uint64
			var lastSnapshotBytes int
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				iterationState := cloneIncrementalAnalyzerForBenchmark(state)
				stateReuseBefore := iterationState.Metrics().ComponentStateReuses
				resultHitsBefore := iterationState.Metrics().ResultCacheHits
				b.StartTimer()
				_, err := iterationState.Append(appendBar, cutoff)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				lastComponentReuseDelta = iterationState.Metrics().ComponentStateReuses - stateReuseBefore
				lastResultCacheHitDelta = iterationState.Metrics().ResultCacheHits - resultHitsBefore
				lastSnapshotBytes = mustJSONSize(b, iterationState.lastResult)
			}
			b.ReportMetric(float64(count+1), "bars/state")
			b.ReportMetric(float64(retainedStateBytes), "retained_bytes/state")
			b.ReportMetric(float64(previousSnapshotBytes), "result_json_bytes_before")
			b.ReportMetric(float64(lastSnapshotBytes), "result_json_bytes_after")
			b.ReportMetric(1, "bars_appended/op")
			b.ReportMetric(float64(lastComponentReuseDelta), "component_state_reuse/op")
			b.ReportMetric(float64(lastResultCacheHitDelta), "result_cache_hits/op")
		})
	}
}

// incrementalAnalyzerRetainedBytes estimates the owned history slices kept by
// the analyzer, excluding lastResult which is measured as serialized output.
func incrementalAnalyzerRetainedBytes(a *IncrementalAnalyzer) int {
	if a == nil {
		return 0
	}
	return cap(a.bars)*int(unsafe.Sizeof(Bar{})) +
		cap(a.merged)*int(unsafe.Sizeof(mergedBar{})) +
		cap(a.fractals)*int(unsafe.Sizeof(Fractal{})) +
		cap(a.macd)*int(unsafe.Sizeof(MACDPoint{}))
}

func cloneIncrementalAnalyzerForBenchmark(source *IncrementalAnalyzer) *IncrementalAnalyzer {
	clone := *source
	clone.bars = append([]Bar(nil), source.bars...)
	clone.merged = append([]mergedBar(nil), source.merged...)
	clone.fractals = append([]Fractal(nil), source.fractals...)
	clone.macd = append([]MACDPoint(nil), source.macd...)
	// Append replaces lastResult. Omit the returned snapshot from each isolated
	// iteration so retained output doesn't inflate analyzer-state memory.
	clone.lastResult = Result{}
	return &clone
}

func mustJSONSize(b *testing.B, value any) int {
	b.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		b.Fatal(err)
	}
	return len(encoded)
}

func incrementalBenchmarkBars(count int) []Bar {
	location, _ := time.LoadLocation("America/New_York")
	day := time.Date(2020, 1, 2, 16, 0, 0, 0, location)
	bars := make([]Bar, 0, count)
	for len(bars) < count {
		if marketcalendar.IsTradingDay(day) {
			index := len(bars)
			price := 100 + 8*math.Sin(float64(index)*.27) + 3*math.Sin(float64(index)*.061)
			bars = append(bars, adjustedReplayBar(day.Format(time.DateOnly), price))
		}
		day = day.AddDate(0, 0, 1)
	}
	return bars
}
