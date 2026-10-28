package server

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"option-scope-go/internal/provider"
	"option-scope-go/internal/valuation"
)

// detectValuationRegime keeps the macro walk optional. The normal quote path
// remains usable with providers that only implement quote/fundamentals, while
// the production Hybrid provider supplies SPX, VIX, and Treasury benchmarks.
func (s *Service) detectValuationRegime(ctx context.Context, symbol string, fundamentals provider.YahooFundamentals) (string, map[string]any) {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	s.mu.RLock()
	cached := s.valuationRegimes[key]
	s.mu.RUnlock()
	if !cached.at.IsZero() && time.Since(cached.at) < 15*time.Minute {
		return cached.regime, cloneAnyMap(cached.signals)
	}

	input := valuation.RegimeInput{
		RevenueGrowth: fundamentals.RevenueGrowth,
		EPSRevision:   fundamentals.EPSRevision,
		CapexGrowth:   fundamentals.CapexGrowth,
		InventoryDays: fundamentals.InventoryDays,
	}
	if input.RevenueGrowth == nil {
		input.RevenueGrowth = fundamentals.EarningsGrowth
	}
	if finitePointer(fundamentals.RiskFreeRate) {
		rate := *fundamentals.RiskFreeRate
		if rate > 1 {
			rate /= 100
		}
		input.TNXYield = &rate
	}
	if !finitePointer(input.CapexGrowth) && len(fundamentals.AnnualCapitalExpenditure) >= 2 {
		current := fundamentals.AnnualCapitalExpenditure[0]
		previous := fundamentals.AnnualCapitalExpenditure[1]
		// Yahoo reports capex as a negative cash outflow. Compare magnitudes so
		// growing investment is positive regardless of that sign convention.
		if math.IsNaN(current) || math.IsInf(current, 0) || math.IsNaN(previous) || math.IsInf(previous, 0) || math.Abs(previous) < 1e-9 {
			// Leave the signal unavailable when the series cannot support a
			// meaningful growth rate.
		} else {
			growth := math.Abs(current)/math.Abs(previous) - 1
			if !math.IsNaN(growth) && !math.IsInf(growth, 0) {
				input.CapexGrowth = &growth
			}
		}
	}
	var benchmark benchmarkHistoryProvider
	if candidate, ok := s.provider.(benchmarkHistoryProvider); ok {
		benchmark = candidate
	}
	if benchmark != nil {
		// These requests share the provider's upstream gate. Run them together
		// so one slow benchmark cannot consume the full valuation deadline.
		var benchmarkBars [3]provider.HistoryResult
		var benchmarkErr [3]error
		benchmarkSymbols := []string{"^GSPC", "^VIX", "^TNX"}
		fetch := func(index int) {
			benchmarkBars[index], benchmarkErr[index] = benchmark.BenchmarkHistory(ctx, benchmarkSymbols[index], "6mo")
		}
		if s.cfg.UpstreamConcurrency <= 1 {
			for index := range benchmarkSymbols {
				fetch(index)
			}
		} else {
			limit := s.cfg.UpstreamConcurrency
			if limit > len(benchmarkSymbols) {
				limit = len(benchmarkSymbols)
			}
			gate := make(chan struct{}, limit)
			var wait sync.WaitGroup
			for index := range benchmarkSymbols {
				launched := false
				select {
				case gate <- struct{}{}:
					wait.Add(1)
					go func(index int) {
						defer wait.Done()
						defer func() { <-gate }()
						fetch(index)
					}(index)
					launched = true
				case <-ctx.Done():
					break
				}
				if !launched {
					break
				}
			}
			wait.Wait()
		}
		if benchmarkErr[0] == nil {
			input.BenchmarkCloses = historyCloses(benchmarkBars[0].Bars)
		}
		if benchmarkErr[1] == nil {
			closes := historyCloses(benchmarkBars[1].Bars)
			if len(closes) > 0 {
				value := closes[len(closes)-1]
				input.VIX = &value
			}
		}
		if benchmarkErr[2] == nil {
			closes := historyCloses(benchmarkBars[2].Bars)
			if len(closes) > 0 {
				value := closes[len(closes)-1]
				if value > 1 {
					value /= 100
				}
				input.TNXYield = &value
			}
		}
	}
	regime, signals := valuation.DetectRegime(input)
	s.mu.Lock()
	if s.valuationRegimes == nil {
		s.valuationRegimes = make(map[string]cachedValuationRegime)
	}
	s.valuationRegimes[key] = cachedValuationRegime{regime: regime, signals: cloneAnyMap(signals), at: time.Now()}
	trimSymbolStateCache(s.valuationRegimes, key, s.cfg.LowMemory, func(entry cachedValuationRegime) time.Time { return entry.at })
	s.mu.Unlock()
	return regime, signals
}

func finitePointer(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func historyCloses(bars []map[string]any) []float64 {
	closes := make([]float64, 0, len(bars))
	for _, bar := range bars {
		value := numberValue(bar["close"])
		if value != nil && *value > 0 {
			closes = append(closes, *value)
		}
	}
	return closes
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func normalizeRegime(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case valuation.RegimeBoom:
		return valuation.RegimeBoom
	case valuation.RegimeSlowdown:
		return valuation.RegimeSlowdown
	case valuation.RegimeWinter:
		return valuation.RegimeWinter
	default:
		return valuation.RegimeNormal
	}
}
