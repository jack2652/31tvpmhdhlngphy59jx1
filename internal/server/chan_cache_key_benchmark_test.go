package server

import (
	"testing"
	"time"

	"option-scope-go/internal/chan"
)

func BenchmarkChanAnalysisCacheKey(b *testing.B) {
	makeBars := func(count int, start time.Time) []chananalysis.Bar {
		bars := make([]chananalysis.Bar, count)
		for index := range bars {
			close := 400 + float64(index%500)/10
			bars[index] = chananalysis.Bar{
				Date: start.AddDate(0, 0, index).Format(time.DateOnly),
				Open: close - 1, High: close + 2, Low: close - 2, Close: close,
				Volume: float64(index * 100), RawOpen: close - 1.1, RawHigh: close + 2.1,
				RawLow: close - 2.1, RawClose: close - .1, AdjustmentFactor: 1.02, Chain: index % 4,
			}
		}
		return bars
	}
	daily := makeBars(3000, time.Date(2015, time.January, 1, 0, 0, 0, 0, time.UTC))
	intraday := makeBars(1000, time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC))
	market := makeBars(3000, time.Date(2015, time.January, 1, 0, 0, 0, 0, time.UTC))
	cutoff := time.Date(2026, time.October, 28, 20, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = chanAnalysisCacheKey("QQQ", daily, intraday, market, cutoff)
	}
}
