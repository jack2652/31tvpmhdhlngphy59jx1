package provider

import (
	"testing"
	"time"

	ym "github.com/wnjoon/go-yfinance/pkg/models"
)

func BenchmarkNormalizeYFinanceHistoryMax(b *testing.B) {
	const count = 15000
	bars := make([]ym.Bar, count)
	start := time.Date(1965, time.January, 1, 16, 0, 0, 0, time.UTC)
	for i := range bars {
		close := 100 + float64(i%1000)/10
		bars[i] = ym.Bar{
			Date: start.AddDate(0, 0, i), Open: close - 1, High: close + 1,
			Low: close - 2, Close: close, Volume: int64(100000 + i),
		}
	}
	b.Run("full_ohlcv_maps", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			result, err := normalizeYFinanceHistory("QQQ", "max", bars, time.UTC)
			if err != nil {
				b.Fatal(err)
			}
			if len(result.Bars) != count {
				b.Fatalf("normalized bars=%d, want %d", len(result.Bars), count)
			}
		}
	})
	b.Run("price_extreme_points", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			points, err := normalizeYFinanceExtremePoints(bars, time.UTC)
			if err != nil {
				b.Fatal(err)
			}
			if len(points) != count {
				b.Fatalf("normalized points=%d, want %d", len(points), count)
			}
		}
	})
}
