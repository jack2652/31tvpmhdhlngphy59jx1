package chananalysis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// BenchmarkIncrementalResultCacheHitVsFullRecompute measures the concrete
// no-new-bars cache path used by replay separately from append updates. An
// append is a cache miss for the final Result because it changes the input.
func BenchmarkIncrementalResultCacheHitVsFullRecompute(b *testing.B) {
	for _, count := range []int{252, 1255} {
		b.Run(fmt.Sprintf("bars_%d", count), func(b *testing.B) {
			history := incrementalBenchmarkBars(count)
			last := history[len(history)-1]
			location, _ := time.LoadLocation("America/New_York")
			cutoff := dayForBarClose(last.Date, location)
			state := NewIncrementalAnalyzer("BENCH")
			var result Result
			for _, bar := range history {
				var err error
				result, err = state.Append(bar, cutoff)
				if err != nil {
					b.Fatal(err)
				}
			}
			full := AnalyzeAdjusted("BENCH", history, nil, cutoff)
			cachedJSON, err := json.Marshal(result)
			if err != nil {
				b.Fatal(err)
			}
			fullJSON, err := json.Marshal(full)
			if err != nil {
				b.Fatal(err)
			}
			if !bytes.Equal(cachedJSON, fullJSON) {
				b.Fatal("cached snapshot differs from full recomputation")
			}
			if _, ok := state.ReuseLastResult(); !ok {
				b.Fatal("expected a prior result for cache-hit measurement")
			}
			b.Run("result_cache_hit", func(b *testing.B) {
				b.ReportAllocs()
				b.ReportMetric(float64(count), "input_bars/op")
				b.ReportMetric(float64(mustJSONSize(b, state.lastResult)), "result_json_bytes/op")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, ok := state.ReuseLastResult(); !ok {
						b.Fatal("cached result disappeared")
					}
				}
				b.ReportMetric(1, "result_cache_hits/op")
			})
			b.Run("full_recompute", func(b *testing.B) {
				b.ReportAllocs()
				b.ReportMetric(float64(count), "input_bars/op")
				b.ReportMetric(float64(mustJSONSize(b, full)), "result_json_bytes/op")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = AnalyzeAdjusted("BENCH", history, nil, cutoff)
				}
				b.ReportMetric(0, "result_cache_hits/op")
			})
		})
	}
}
