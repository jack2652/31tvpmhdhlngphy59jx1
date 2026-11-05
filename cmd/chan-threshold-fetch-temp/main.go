package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
)

func main() {
	cfg := config.Load()
	h, err := provider.NewDefaultHybrid(cfg.UpstreamTimeout, cfg.UpstreamConcurrency, cfg.MarketProxy, cfg.AlpacaAPIKey, cfg.AlpacaAPISecret, cfg.UpstreamWait)
	if err != nil {
		panic(err)
	}
	defer h.Close()
	db, err := store.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	symbols := os.Args[2:]
	if len(symbols) == 0 {
		symbols = []string{"QQQ", "^GSPC"}
	}
	period := strings.TrimSpace(os.Getenv("CHAN_FETCH_PERIOD"))
	if period == "" {
		period = "5y"
	}
	for _, symbol := range symbols {
		r, err := h.ChanAdjustedHistory(ctx, symbol, period)
		if err != nil {
			panic(fmt.Errorf("fetch %s: %w", symbol, err))
		}
		valid := make([]map[string]any, 0, len(r.Bars))
		for i, b := range r.Bars {
			fields := []string{"open", "high", "low", "close", "raw_open", "raw_high", "raw_low", "raw_close", "adjustment_factor"}
			prices := make(map[string]float64, len(fields))
			bad := false
			for _, f := range fields {
				v, ok := b[f].(float64)
				if !ok || v <= 0 {
					bad = true
					break
				}
				prices[f] = v
			}
			for _, f := range []string{"volume", "raw_volume"} {
				if v := number(b[f]); v < 0 {
					bad = true
				}
			}
			if !bad && (prices["high"] < prices["open"] || prices["high"] < prices["close"] || prices["low"] > prices["open"] || prices["low"] > prices["close"] || prices["raw_high"] < prices["raw_open"] || prices["raw_high"] < prices["raw_close"] || prices["raw_low"] > prices["raw_open"] || prices["raw_low"] > prices["raw_close"]) {
				bad = true
			}
			if bad {
				if i < 3 {
					fmt.Printf("invalid %s row %d: %#v\n", symbol, i, b)
				}
				continue
			}
			valid = append(valid, b)
		}
		r.Bars = valid
		if err := db.WriteChanAdjustedHistory(symbol, r.Timeframe, r.AdjustmentType, r.Source, r.Bars, r.FetchedAt); err != nil {
			panic(err)
		}
		fmt.Printf("%s %d %s..%s\n", symbol, len(r.Bars), r.Bars[0]["date"], r.Bars[len(r.Bars)-1]["date"])
	}
}

func number(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return -1
	}
}
