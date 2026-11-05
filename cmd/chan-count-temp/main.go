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
	db, err := store.Open("data/threshold-study.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	symbol := "QQQ"
	if len(os.Args) > 1 {
		symbol = os.Args[1]
	}
	data, err := db.LatestChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", 1300)
	if err != nil {
		panic(err)
	}
	rows := data["bars"].([]map[string]any)
	bars := make([]chananalysis.Bar, 0, len(rows))
	for _, row := range rows {
		bars = append(bars, chananalysis.Bar{Date: row["date"].(string), Open: n(row["open"]), High: n(row["high"]), Low: n(row["low"]), Close: n(row["close"]), Volume: n(row["volume"]), RawOpen: n(row["raw_open"]), RawHigh: n(row["raw_high"]), RawLow: n(row["raw_low"]), RawClose: n(row["raw_close"]), AdjustmentFactor: n(row["adjustment_factor"])})
	}
	cutoff := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	result := chananalysis.AnalyzeAdjusted(symbol, bars, nil, cutoff)
	levels := []map[string]any{}
	for _, level := range result.Daily.RecursiveLevels {
		confirmed, forming := 0, 0
		for _, seg := range level.Segments {
			if seg.Confirmed {
				confirmed++
			} else {
				forming++
			}
		}
		levels = append(levels, map[string]any{"level": level.Level, "status": level.Status, "strokes": len(level.Strokes), "segments": len(level.Segments), "confirmed_segments": confirmed, "forming_segments": forming, "centers": len(level.Centers)})
		if level.Level > 0 {
			for i, s := range level.Strokes {
				fmt.Printf("level=%d input-stroke=%d dir=%s confirmed=%t from=%s %.2f to=%s %.2f range=%.2f..%.2f\n", level.Level, i, s.Direction, s.Confirmed, s.Start.Date, s.Start.Price, s.End.Date, s.End.Price, s.Low, s.High)
			}
		}
	}
	summary := map[string]any{"input": len(bars), "accepted": result.Daily.Quality.AcceptedBars, "chains": len(result.Daily.Quality.Chains), "candles": len(result.Daily.Candles), "merged": len(result.Daily.MergedCandles), "fractals": len(result.Daily.FractalsV4), "strokes": len(result.Daily.StrokesV4), "segments": len(result.Daily.Segments), "recursive_levels": levels, "recursive_centers": len(result.Daily.RecursiveCenters), "signals": result.Daily.SignalsV4}
	segments := []map[string]any{}
	for _, segment := range result.Daily.Segments {
		segments = append(segments, map[string]any{"chain": segment.Chain, "direction": segment.Direction, "start": segment.Start, "end": segment.End, "confirmed": segment.Confirmed, "confirmed_at": segment.ConfirmedAt, "break_stroke": segment.BreakStroke, "confirm_stroke": segment.ConfirmStroke, "stroke_range": []int{segment.StartStroke, segment.EndStroke}})
	}
	summary["segments_detail"] = segments
	encoded, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(encoded))
}

func n(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case *float64:
		if x != nil {
			return *x
		}
		return 0
	default:
		return 0
	}
}
