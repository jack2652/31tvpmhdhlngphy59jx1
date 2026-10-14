package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	ym "github.com/wnjoon/go-yfinance/pkg/models"
)

// YFinanceHistoryClient adapts go-yfinance daily bars to the existing API
// representation. It uses the Yahoo ticker factory shared by the other SDK
// adapters so proxy, timeout, and authentication settings stay consistent.
type YFinanceHistoryClient struct {
	Gate          chan struct{}
	GateWait      time.Duration
	TickerFactory yahooTickerFactory
	TimeNow       func() time.Time
	Location      *time.Location
	intradayMu    sync.Mutex
	intradayCache map[string]intradayHistoryCacheEntry
}

type intradayHistoryCacheEntry struct {
	result    HistoryResult
	fetchedAt time.Time
}

func NewYFinanceHistoryClient(gate chan struct{}, gateWait time.Duration, tickerFactory yahooTickerFactory) *YFinanceHistoryClient {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	return &YFinanceHistoryClient{
		Gate: gate, GateWait: gateWait, TickerFactory: tickerFactory,
		TimeNow: time.Now, Location: location,
	}
}

func (c *YFinanceHistoryClient) Fetch(ctx context.Context, symbol, period string) (HistoryResult, error) {
	return c.fetch(ctx, symbol, period, false)
}

func (c *YFinanceHistoryClient) FetchAdjusted(ctx context.Context, symbol, period string) (HistoryResult, error) {
	return c.fetch(ctx, symbol, period, true)
}

// FetchIntraday returns completed-session 15-minute Yahoo bars. Yahoo only
// retains this interval for a limited recent window, so refresh it briefly in
// memory and let callers aggregate larger intraday periods locally.
func (c *YFinanceHistoryClient) FetchIntraday(ctx context.Context, rawSymbol, interval string) (HistoryResult, error) {
	if c == nil {
		return HistoryResult{}, errors.New("go-yfinance history client is nil")
	}
	if interval != "15m" {
		return HistoryResult{}, fmt.Errorf("unsupported intraday history interval %q", interval)
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return HistoryResult{}, err
	}
	now := time.Now()
	if c.TimeNow != nil {
		now = c.TimeNow()
	}
	cacheKey := symbol + ":" + interval
	c.intradayMu.Lock()
	if cached, found := c.intradayCache[cacheKey]; found && now.Sub(cached.fetchedAt) < 3*time.Minute {
		c.intradayMu.Unlock()
		return cached.result, nil
	}
	c.intradayMu.Unlock()
	params := ym.HistoryParams{
		Start: ptrTime(now.AddDate(0, 0, -59)), End: ptrTime(now), Interval: interval,
		AutoAdjust: false, Actions: false, PrePost: false, KeepNA: false,
	}
	location := c.Location
	if location == nil {
		location, _ = time.LoadLocation("America/New_York")
	}
	bars, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.TickerFactory, symbol, func(ticker yahooTicker) ([]ym.Bar, error) {
		return ticker.History(params)
	})
	if err != nil {
		return HistoryResult{}, fmt.Errorf("go-yfinance intraday history: %w", err)
	}
	result, err := normalizeYFinanceIntradayHistory(symbol, bars, location)
	if err != nil {
		return HistoryResult{}, err
	}
	result.FetchedAt, result.Source = now.UTC(), "go-yfinance"
	c.intradayMu.Lock()
	if c.intradayCache == nil {
		c.intradayCache = make(map[string]intradayHistoryCacheEntry)
	}
	c.intradayCache[cacheKey] = intradayHistoryCacheEntry{result: result, fetchedAt: now}
	c.intradayMu.Unlock()
	return result, nil
}

func normalizeYFinanceIntradayHistory(symbol string, bars []ym.Bar, location *time.Location) (HistoryResult, error) {
	if len(bars) == 0 {
		return HistoryResult{}, errors.New("go-yfinance returned empty intraday history")
	}
	if location == nil {
		location, _ = time.LoadLocation("America/New_York")
	}
	if location == nil {
		location = time.UTC
	}
	out := make([]map[string]any, 0, len(bars))
	for index, bar := range bars {
		if bar.Date.IsZero() || bar.Close <= 0 || bar.Volume < 0 || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			continue
		}
		if math.IsNaN(bar.Open) || math.IsInf(bar.Open, 0) || math.IsNaN(bar.High) || math.IsInf(bar.High, 0) || math.IsNaN(bar.Low) || math.IsInf(bar.Low, 0) ||
			bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			return HistoryResult{}, fmt.Errorf("go-yfinance intraday bar %d has invalid OHLC", index)
		}
		out = append(out, map[string]any{
			"date": bar.Date.In(location).Format("2006-01-02 15:04"),
			"open": bar.Open, "high": bar.High, "low": bar.Low, "close": bar.Close, "volume": bar.Volume,
		})
	}
	if len(out) == 0 {
		return HistoryResult{}, errors.New("go-yfinance intraday history has no valid bars")
	}
	return HistoryResult{Symbol: symbol, Bars: out}, nil
}

func (c *YFinanceHistoryClient) fetch(ctx context.Context, rawSymbol, period string, adjusted bool) (HistoryResult, error) {
	if err := ctx.Err(); err != nil {
		return HistoryResult{}, err
	}
	if c == nil {
		return HistoryResult{}, errors.New("go-yfinance history client is nil")
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		if strings.EqualFold(strings.TrimSpace(rawSymbol), "^GSPC") {
			symbol = "^GSPC"
		} else {
			return HistoryResult{}, err
		}
	}
	period = strings.ToLower(strings.TrimSpace(period))
	if !supportedHistoryPeriod(period) {
		return HistoryResult{}, fmt.Errorf("unsupported Yahoo history period %q", period)
	}
	// yfinance expands period="max" to an explicit period1/period2 range.
	// Yahoo's range=max form with daily bars can silently return one bar per
	// month, which drops valid daily extrema. Keep the Go request on the same
	// wire shape as the Python provider.
	historyParams := ym.HistoryParams{
		Period: period, Interval: "1d", AutoAdjust: adjusted,
		Actions: false, PrePost: false, KeepNA: false,
	}
	if period == "max" {
		now := time.Now()
		if c.TimeNow != nil {
			now = c.TimeNow()
		}
		end := time.Unix(now.Unix(), 0)
		historyParams.Period = ""
		historyParams.Start = ptrTime(time.Unix(end.Unix()-3122064000+5, 0))
		historyParams.End = ptrTime(end)
	}
	bars, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.TickerFactory, symbol, func(ticker yahooTicker) ([]ym.Bar, error) {
		return ticker.History(historyParams)
	})
	if err != nil {
		return HistoryResult{}, fmt.Errorf("go-yfinance history: %w", err)
	}
	result, err := normalizeYFinanceHistory(symbol, period, bars, c.Location)
	if err != nil {
		return HistoryResult{}, err
	}
	if c.TimeNow != nil {
		result.FetchedAt = c.TimeNow().UTC()
	} else {
		result.FetchedAt = time.Now().UTC()
	}
	result.Source = "go-yfinance"
	return result, nil
}

func ptrTime(value time.Time) *time.Time { return &value }

func supportedHistoryPeriod(period string) bool {
	switch period {
	case "1mo", "6mo", "1y", "2y", "5y", "max":
		return true
	default:
		return false
	}
}

func normalizeYFinanceHistory(symbol, period string, bars []ym.Bar, location *time.Location) (HistoryResult, error) {
	if len(bars) == 0 {
		return HistoryResult{}, errors.New("go-yfinance returned empty history")
	}
	if location == nil {
		location = time.UTC
	}
	out := make([]map[string]any, 0, len(bars))
	for index, bar := range bars {
		if bar.Date.IsZero() {
			return HistoryResult{}, fmt.Errorf("go-yfinance history bar %d has missing date", index)
		}
		values := []struct {
			name  string
			value float64
		}{
			{"open", bar.Open}, {"high", bar.High}, {"low", bar.Low}, {"close", bar.Close},
		}
		for _, item := range values {
			if math.IsNaN(item.value) || math.IsInf(item.value, 0) {
				return HistoryResult{}, fmt.Errorf("go-yfinance history bar %d has non-finite %s", index, item.name)
			}
		}
		if bar.Close == 0 {
			// The SDK decodes Yahoo null prices as zero; omit rows with no close.
			continue
		}
		if bar.Volume < 0 {
			return HistoryResult{}, fmt.Errorf("go-yfinance history bar %d has negative volume", index)
		}
		var open, high, low any = bar.Open, bar.High, bar.Low
		if bar.Open == 0 {
			open = nil
		}
		if bar.High == 0 {
			high = nil
		}
		if bar.Low == 0 {
			low = nil
		}
		out = append(out, map[string]any{
			"date": bar.Date.In(location).Format("2006-01-02"),
			"open": open, "high": high, "low": low, "close": bar.Close,
			"volume": bar.Volume,
		})
	}
	if len(out) == 0 {
		return HistoryResult{}, errors.New("go-yfinance history has no valid closing prices")
	}
	result := HistoryResult{Symbol: symbol, Bars: out}
	if period == "max" {
		result.AllTimeHigh, result.AllTimeLow = historyExtremes(out)
		latest, err := time.ParseInLocation("2006-01-02", out[len(out)-1]["date"].(string), location)
		if err == nil {
			cutoff := latest.AddDate(-1, 0, 0)
			window := make([]map[string]any, 0, len(out))
			for _, item := range out {
				day, parseErr := time.ParseInLocation("2006-01-02", item["date"].(string), location)
				if parseErr == nil && !day.Before(cutoff) {
					window = append(window, item)
				}
			}
			result.FiftyTwoWeekHigh, result.FiftyTwoWeekLow = historyExtremes(window)
		}
	}
	return result, nil
}

var _ interface {
	Fetch(context.Context, string, string) (HistoryResult, error)
	FetchAdjusted(context.Context, string, string) (HistoryResult, error)
} = (*YFinanceHistoryClient)(nil)
