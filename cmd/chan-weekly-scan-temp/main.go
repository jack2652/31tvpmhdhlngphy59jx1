package main

import (
	"fmt"
	"os"
	"time"

	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/store"
)

func main() {
	db, err := store.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	for _, symbol := range os.Args[2:] {
		result, err := db.LatestChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", 1300)
		if err != nil {
			fmt.Println(symbol, err)
			continue
		}
		rows, ok := result["bars"].([]map[string]any)
		if !ok || len(rows) == 0 {
			fmt.Println(symbol, "no bars")
			continue
		}
		bars := make([]chananalysis.Bar, 0, len(rows))
		for _, row := range rows {
			bars = append(bars, chananalysis.Bar{Date: str(row["date"]), Open: num(row["open"]), High: num(row["high"]), Low: num(row["low"]), Close: num(row["close"]), Volume: num(row["volume"]), RawOpen: num(row["raw_open"]), RawHigh: num(row["raw_high"]), RawLow: num(row["raw_low"]), RawClose: num(row["raw_close"]), AdjustmentFactor: num(row["adjustment_factor"])})
		}
		cutoff := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
		out := chananalysis.AnalyzeAdjusted(symbol, bars, nil, cutoff)
		daily, weekly := out.Daily, out.Weekly
		dailyConfirmed, weeklyConfirmed := 0, 0
		for _, seg := range daily.Segments {
			if seg.Confirmed {
				dailyConfirmed++
			}
		}
		for _, seg := range weekly.Segments {
			if seg.Confirmed {
				weeklyConfirmed++
			}
		}
		fmt.Printf("%s daily=%d strokes=%d segments=%d/%d centers=%d signals=%d weekly=%d strokes=%d segments=%d/%d centers=%d signals=%d\n", symbol, daily.Quality.AcceptedBars, len(daily.StrokesV4), len(daily.Segments), dailyConfirmed, len(daily.RecursiveCenters), len(daily.SignalsV4), weekly.Quality.AcceptedBars, len(weekly.StrokesV4), len(weekly.Segments), weeklyConfirmed, len(weekly.RecursiveCenters), len(weekly.SignalsV4))
	}
}

func str(value any) string { s, _ := value.(string); return s }
func num(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case *float64:
		if v != nil {
			return *v
		}
	}
	return 0
}
