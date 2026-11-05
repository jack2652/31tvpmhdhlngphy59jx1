package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/config"
	"option-scope-go/internal/provider"
)

type summary struct {
	Symbol                                                                                                           string `json:"symbol"`
	FetchError                                                                                                       string `json:"fetch_error,omitempty"`
	AnalysisError                                                                                                    string `json:"analysis_error,omitempty"`
	Bars, Accepted, Chains, Strokes, Segments, ConfirmedSegments, Centers, Signals                                   int
	Start, End                                                                                                       string
	Quality                                                                                                          string
	SignalIDs                                                                                                        []string                            `json:"signal_ids,omitempty"`
	Calibration                                                                                                      []chananalysis.ThresholdCalibration `json:"calibration,omitempty"`
	WeeklyBars, WeeklyAccepted, WeeklyStrokes, WeeklySegments, WeeklyConfirmedSegments, WeeklyCenters, WeeklySignals int
}

func main() {
	cfg := config.Load()
	h, err := provider.NewDefaultHybrid(cfg.UpstreamTimeout, cfg.UpstreamConcurrency, cfg.MarketProxy, cfg.AlpacaAPIKey, cfg.AlpacaAPISecret, cfg.UpstreamWait)
	if err != nil {
		panic(err)
	}
	defer h.Close()
	timeout := 12 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	symbols := os.Args[1:]
	period := "5y"
	if len(symbols) == 0 {
		symbols = []string{"SPY", "DIA", "IWM", "AAPL", "MSFT", "NVDA", "XLF", "TLT", "GLD", "^GSPC"}
	}
	cutoff := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for _, symbol := range symbols {
		s := summary{Symbol: symbol}
		history, err := h.ChanAdjustedHistory(ctx, symbol, period)
		if err != nil {
			s.FetchError = err.Error()
			write(s)
			continue
		}
		bars := make([]chananalysis.Bar, 0, len(history.Bars))
		for _, row := range history.Bars {
			bars = append(bars, chananalysis.Bar{Date: str(row["date"]), Open: num(row["open"]), High: num(row["high"]), Low: num(row["low"]), Close: num(row["close"]), Volume: num(row["volume"]), RawOpen: num(row["raw_open"]), RawHigh: num(row["raw_high"]), RawLow: num(row["raw_low"]), RawClose: num(row["raw_close"]), AdjustmentFactor: num(row["adjustment_factor"])})
		}
		if len(bars) == 0 {
			s.FetchError = "provider returned zero paired bars"
			write(s)
			continue
		}
		result := chananalysis.AnalyzeAdjusted(symbol, bars, nil, cutoff)
		s.Bars = len(bars)
		s.Accepted = result.Daily.Quality.AcceptedBars
		s.Chains = len(result.Daily.Quality.Chains)
		s.Strokes = len(result.Daily.StrokesV4)
		s.Segments = len(result.Daily.Segments)
		s.Centers = len(result.Daily.RecursiveCenters)
		s.Signals = len(result.Daily.SignalsV4)
		s.Quality = result.Daily.Quality.Status
		s.WeeklyBars = result.Weekly.Quality.InputBars
		s.WeeklyAccepted = result.Weekly.Quality.AcceptedBars
		s.WeeklyStrokes = len(result.Weekly.StrokesV4)
		s.WeeklySegments = len(result.Weekly.Segments)
		s.WeeklyCenters = len(result.Weekly.RecursiveCenters)
		s.WeeklySignals = len(result.Weekly.SignalsV4)
		s.Start = bars[0].Date
		s.End = bars[len(bars)-1].Date
		for _, seg := range result.Daily.Segments {
			if seg.Confirmed {
				s.ConfirmedSegments++
			}
		}
		for _, seg := range result.Weekly.Segments {
			if seg.Confirmed {
				s.WeeklyConfirmedSegments++
			}
		}
		for _, sig := range result.Daily.SignalsV4 {
			s.SignalIDs = append(s.SignalIDs, sig.SignalID)
		}
		if os.Getenv("CHAN_SCAN_CALIBRATE") != "false" {
			calibration, calibrationErr := chananalysis.CalibrateDivergenceThresholds(symbol, bars, []float64{0.6, 0.7, 0.8, 0.9})
			if calibrationErr == nil {
				s.Calibration = calibration
			} else {
				s.AnalysisError = calibrationErr.Error()
			}
		}
		write(s)
	}
}

func write(v summary)  { b, _ := json.Marshal(v); fmt.Println(string(b)) }
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
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
	}
	return 0
}
