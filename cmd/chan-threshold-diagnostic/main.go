package main

import (
	"encoding/json"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/store"
	"os"
	"time"
)

func main() {
	db, e := store.Open("data/threshold-study.db")
	if e != nil {
		panic(e)
	}
	defer db.Close()
	result, e := db.LatestChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", 1300)
	if e != nil {
		panic(e)
	}
	rows := result["bars"].([]map[string]any)
	bars := make([]chananalysis.Bar, 0, len(rows))
	for _, v := range rows {
		bars = append(bars, chananalysis.Bar{Date: v["date"].(string), Open: n(v["open"]), High: n(v["high"]), Low: n(v["low"]), Close: n(v["close"]), Volume: n(v["volume"])})
	}
	analysis := chananalysis.AnalyzeAdjusted("QQQ", bars, nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	out := struct {
		Fractals int                          `json:"fractals"`
		Strokes  []chananalysis.Stroke        `json:"strokes"`
		Segments []chananalysis.Segment       `json:"segments"`
		Levels   []chananalysis.LevelAnalysis `json:"levels"`
	}{len(analysis.Daily.FractalsV4), analysis.Daily.StrokesV4, analysis.Daily.Segments, analysis.Daily.RecursiveLevels}
	enc, _ := json.Marshal(out)
	_ = os.WriteFile("/tmp/chan-diagnostic.json", enc, 0600)
}
func n(v any) float64 {
	if x, ok := v.(float64); ok {
		return x
	}
	return 0
}
