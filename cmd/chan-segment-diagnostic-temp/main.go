package main

import (
	"fmt"
	"os"
	"strconv"
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
	args := os.Args[2:]
	limit := 1300
	if len(args) > 0 {
		if parsed, parseErr := strconv.Atoi(args[len(args)-1]); parseErr == nil && parsed > 0 {
			limit = parsed
			args = args[:len(args)-1]
		}
	}
	for _, symbol := range args {
		result, err := db.LatestChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", limit)
		if err != nil {
			panic(err)
		}
		rows, ok := result["bars"].([]map[string]any)
		if !ok || len(rows) == 0 {
			panic("no bars")
		}
		bars := make([]chananalysis.Bar, 0, len(rows))
		for _, row := range rows {
			bars = append(bars, chananalysis.Bar{Date: str(row["date"]), Open: num(row["open"]), High: num(row["high"]), Low: num(row["low"]), Close: num(row["close"]), Volume: num(row["volume"]), RawOpen: num(row["raw_open"]), RawHigh: num(row["raw_high"]), RawLow: num(row["raw_low"]), RawClose: num(row["raw_close"]), AdjustmentFactor: num(row["adjustment_factor"])})
		}
		out := chananalysis.AnalyzeAdjusted(symbol, bars, nil, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
		f := out.Daily
		fmt.Printf("%s bars=%d chains=%d strokes=%d segments=%d centers=%d signals=%d\n", symbol, f.Quality.AcceptedBars, len(f.Quality.Chains), len(f.StrokesV4), len(f.Segments), len(f.RecursiveCenters), len(f.SignalsV4))
		for _, fractal := range f.FractalsV4 {
			if fractal.Date >= "2025-03-01" && fractal.Date <= "2025-09-01" {
				fmt.Printf("fractal chain=%d kind=%s date=%s price=%.4f confirmed=%s merged=%d\n", fractal.Chain, fractal.Kind, fractal.Date, fractal.Price, fractal.ConfirmedAt, fractal.MergedIndex)
			}
		}
		if symbol == "QQQ" {
			type point struct {
				kind, date string
				price      float64
				merged     int
			}
			points := []point{}
			for _, fractal := range f.FractalsV4 {
				if fractal.Date < "2025-03-01" || fractal.Date > "2025-09-01" {
					continue
				}
				current := point{fractal.Kind, fractal.Date, fractal.Price, fractal.MergedIndex}
				if len(points) == 0 {
					points = append(points, current)
					fmt.Printf("pivot-trace start %s %s %.4f merged=%d\n", current.kind, current.date, current.price, current.merged)
					continue
				}
				last := points[len(points)-1]
				if last.kind == current.kind {
					more := current.kind == "top" && current.price > last.price || current.kind == "bottom" && current.price < last.price
					if more {
						anchor := last
						valid := len(points) == 1 || diagValidPen(points[len(points)-2], current, f.MergedCandles)
						if valid {
							points[len(points)-1] = current
							fmt.Printf("pivot-trace replace %s %s %.4f anchor=%s gap=%d valid=true\n", current.kind, current.date, current.price, anchor.date, current.merged-anchor.merged)
						} else {
							fmt.Printf("pivot-trace restart %s %s %.4f anchor=%s\n", current.kind, current.date, current.price, anchor.date)
							points = []point{current}
						}
					} else {
						fmt.Printf("pivot-trace ignore same-kind %s %s %.4f vs %.4f\n", current.kind, current.date, current.price, last.price)
					}
					continue
				}
				valid := diagValidPen(last, current, f.MergedCandles)
				if !valid {
					breached := false
					for _, bar := range f.MergedCandles[last.merged : current.merged+1] {
						if last.kind == "top" && bar.High > last.price || last.kind == "bottom" && bar.Low < last.price {
							breached = true
						}
					}
					if breached {
						fmt.Printf("pivot-trace restart crossed anchor %s %s %.4f from %s %.4f\n", current.kind, current.date, current.price, last.kind, last.price)
						points = []point{current}
					} else {
						fmt.Printf("pivot-trace reject gap %s %.4f -> %s %.4f merged-gap=%d\n", last.date, last.price, current.date, current.price, current.merged-last.merged)
					}
					continue
				}
				points = append(points, current)
				fmt.Printf("pivot-trace append %s %s %.4f from %s %.4f gap=%d\n", current.kind, current.date, current.price, last.date, last.price, current.merged-last.merged)
			}
		}
		for i, segment := range f.Segments {
			fmt.Printf("segment=%d chain=%d dir=%s confirmed=%t forming=%t strokes=%d..%d break=%d confirm=%d start=%s %.4f end=%s %.4f at=%s low=%.4f high=%.4f features=%d\n", i, segment.Chain, segment.Direction, segment.Confirmed, segment.Forming, segment.StartStroke, segment.EndStroke, segment.BreakStroke, segment.ConfirmStroke, segment.Start, segment.StartPrice, segment.End, segment.EndPrice, segment.ConfirmedAt, segment.Low, segment.High, len(segment.Feature))
			for _, feature := range segment.Feature {
				fmt.Printf("  feature strokes=%d..%d start=%s end=%s low=%.4f high=%.4f\n", feature.StartStroke, feature.EndStroke, feature.Start, feature.End, feature.Low, feature.High)
			}
		}
		strokeLimit := len(f.StrokesV4)
		for i := 0; i < strokeLimit; i++ {
			stroke := f.StrokesV4[i]
			fmt.Printf("stroke=%d chain=%d dir=%s confirmed=%t %s %.4f -> %s %.4f range=%.4f..%.4f\n", i, stroke.Chain, stroke.Direction, stroke.Confirmed, stroke.Start.Date, stroke.Start.Price, stroke.End.Date, stroke.End.Price, stroke.Low, stroke.High)
		}
	}
}

func str(value any) string { s, _ := value.(string); return s }
func diagValidPen(start, end struct {
	kind, date string
	price      float64
	merged     int
}, merged []chananalysis.MergedCandle) bool {
	if start.kind == end.kind || end.merged-start.merged < 4 {
		return false
	}
	low, high := start.price, end.price
	if low > high {
		low, high = high, low
	}
	if start.kind == "bottom" && end.price <= start.price || start.kind == "top" && end.price >= start.price {
		return false
	}
	for _, bar := range merged[start.merged : end.merged+1] {
		if bar.Low < low || bar.High > high {
			return false
		}
	}
	return true
}
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
