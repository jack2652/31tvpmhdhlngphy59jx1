package main

import (
	"encoding/json"
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
	result, err := db.LatestChanAdjustedHistory(os.Args[2], "1d", "split_dividend_auto", "go-yfinance", 1300)
	if err != nil {
		panic(err)
	}
	values, ok := result["bars"].([]map[string]any)
	if !ok || len(values) == 0 {
		panic("empty bars")
	}
	bars := make([]chananalysis.Bar, 0, len(values))
	for _, v := range values {
		bars = append(bars, chananalysis.Bar{Date: str(v["date"]), Open: num(v["open"]), High: num(v["high"]), Low: num(v["low"]), Close: num(v["close"]), Volume: num(v["volume"]), RawOpen: num(v["raw_open"]), RawHigh: num(v["raw_high"]), RawLow: num(v["raw_low"]), RawClose: num(v["raw_close"]), AdjustmentFactor: num(v["adjustment_factor"])})
	}
	full := chananalysis.AnalyzeAdjusted(os.Args[2], bars, nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	fmt.Fprintf(os.Stderr, "bars=%d fullDailySignals=%d levels=%d segments=%d centers=%d\n", len(bars), len(full.Daily.SignalsV4), len(full.Daily.RecursiveLevels), len(full.Daily.Segments), len(full.Daily.RecursiveCenters))
	return
	cal, err := chananalysis.CalibrateDivergenceThresholds(os.Args[2], bars, []float64{0.6, 0.7, 0.8, 0.9})
	if err != nil {
		panic(err)
	}
	out := map[string]any{"symbol": os.Args[2], "bars": len(bars), "start": bars[0].Date, "end": bars[len(bars)-1].Date, "rule_version": chananalysis.RecursivePreviewVersion, "calibration": cal}
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(enc))
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case *float64:
		if x != nil {
			return *x
		}
	}
	return 0
}
