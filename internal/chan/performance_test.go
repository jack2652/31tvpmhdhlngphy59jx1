package chananalysis

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/marketcalendar"
)

// These benchmarks use one year of synthetic market data at realistic bar
// counts. They are intended to report cost, not to assert a machine-specific
// latency threshold. Run with: go test ./internal/chan -run '^$' -bench '^BenchmarkAnalysisByTimeframe$' -benchmem
func BenchmarkAnalysisByTimeframe(b *testing.B) {
	daily, intraday, cutoff := benchmarkBars()
	for _, benchmark := range []struct {
		name string
		run  func()
	}{
		{name: "daily_and_weekly_252d", run: func() {
			_ = AnalyzeAdjusted("BENCH", adjustedBenchmarkBars(daily), nil, cutoff)
		}},
		{name: "intraday_15m_6552bars", run: func() {
			_, _ = AnalyzeIntraday("BENCH", intraday, "15m", nil, cutoff)
		}},
		{name: "intraday_4h_504bars", run: func() {
			_, _ = AnalyzeIntraday("BENCH", intraday, "4h", nil, cutoff)
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmark.run()
			}
		})
	}
}

func benchmarkBars() ([]Bar, []Bar, time.Time) {
	location, _ := time.LoadLocation("America/New_York")
	day := time.Date(2024, 1, 2, 0, 0, 0, 0, location)
	daily := make([]Bar, 0, 252)
	intraday := make([]Bar, 0, 252*26)
	for len(daily) < 252 {
		if marketcalendar.IsTradingDay(day) {
			index := len(daily)
			price := 100 + float64(index)*0.02 + 4*math.Sin(float64(index)*0.19)
			daily = append(daily, benchmarkBar(day.Format(time.DateOnly), price))
			for barIndex := 0; barIndex < 26; barIndex++ {
				stamp := day.Add(9*time.Hour + 30*time.Minute + time.Duration(barIndex)*15*time.Minute)
				barPrice := price + 0.03*math.Sin(float64(index*26+barIndex)*0.37)
				intraday = append(intraday, benchmarkBar(stamp.Format("2006-01-02 15:04"), barPrice))
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	lastDay, _ := time.ParseInLocation(time.DateOnly, daily[len(daily)-1].Date, location)
	cutoff := time.Date(lastDay.Year(), lastDay.Month(), lastDay.Day(), 17, 0, 0, 0, location)
	return daily, intraday, cutoff
}

func adjustedBenchmarkBars(input []Bar) []Bar {
	result := make([]Bar, len(input))
	for i, bar := range input {
		bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose = bar.Open, bar.High, bar.Low, bar.Close
		bar.AdjustmentFactor = 1
		result[i] = bar
	}
	return result
}

func benchmarkBar(date string, close float64) Bar {
	return Bar{Date: date, Open: close, High: close + 0.6, Low: close - 0.6, Close: close + 0.1, Volume: 1000}
}
