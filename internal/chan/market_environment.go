package chananalysis

import (
	"math"
	"sort"
	"time"
)

const MarketEnvironmentRuleVersion = "sp500-sma50-sma200-v1"

// MarketEnvironment is the point-in-time S&P 500 regime available at one
// observation date. It is metadata for filtering, never a reason to discard
// an individual symbol's structural candidates.
type MarketEnvironment struct {
	Date        string   `json:"date"`
	Status      string   `json:"status"`
	Regime      string   `json:"regime"`
	RuleVersion string   `json:"rule_version"`
	Benchmark   string   `json:"benchmark"`
	Close       *float64 `json:"close,omitempty"`
	SMA50       *float64 `json:"sma50,omitempty"`
	SMA200      *float64 `json:"sma200,omitempty"`
	Samples     int      `json:"samples"`
	Reason      string   `json:"reason,omitempty"`
}

// ClassifyMarketEnvironment builds a point-in-time daily regime series. Every
// output uses only the current and earlier valid benchmark closes; SMA values
// are unavailable until their full windows exist. Duplicate dates keep the
// last valid close, matching the latest corrected bar in provider history.
func ClassifyMarketEnvironment(input []Bar) []MarketEnvironment {
	byDate := make(map[string]float64, len(input))
	for _, bar := range input {
		if _, err := time.Parse(time.DateOnly, bar.Date); err != nil || !finitePositive(bar.Close) || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			continue
		}
		byDate[bar.Date] = bar.Close
	}
	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	out := make([]MarketEnvironment, 0, len(dates))
	closes := make([]float64, 0, len(dates))
	for _, date := range dates {
		close := byDate[date]
		closes = append(closes, close)
		entry := MarketEnvironment{Date: date, Status: "unfiltered", Regime: "unfiltered", RuleVersion: MarketEnvironmentRuleVersion, Benchmark: "^GSPC", Close: &close, Samples: len(closes)}
		if len(closes) < 200 {
			entry.Reason = "S&P 500 有效复权收盘样本不足 200 根"
		} else {
			sma50, sma200 := meanTail(closes, 50), meanTail(closes, 200)
			entry.SMA50, entry.SMA200 = &sma50, &sma200
			switch {
			case close > sma50 && sma50 > sma200:
				entry.Status, entry.Regime = "classified", "bull"
			case close < sma50 && sma50 < sma200:
				entry.Status, entry.Regime = "classified", "bear"
			default:
				entry.Status, entry.Regime = "classified", "range"
			}
		}
		out = append(out, entry)
	}
	return out
}

func meanTail(values []float64, count int) float64 {
	start := len(values) - count
	sum := 0.0
	for _, value := range values[start:] {
		sum += value
	}
	return sum / float64(count)
}

// MarketEnvironmentAt returns the most recent benchmark classification at or
// before the signal observation date. It does not look ahead to a later bar.
func MarketEnvironmentAt(series []MarketEnvironment, date string) MarketEnvironment {
	result := MarketEnvironment{Date: date, Status: "unfiltered", Regime: "unfiltered", RuleVersion: MarketEnvironmentRuleVersion, Benchmark: "^GSPC", Reason: "截至观察日期没有可用的 S&P 500 复权数据"}
	for _, entry := range series {
		if entry.Date > date {
			break
		}
		result = entry
	}
	return result
}
