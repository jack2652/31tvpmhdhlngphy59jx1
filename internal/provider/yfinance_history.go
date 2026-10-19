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
	Gate               chan struct{}
	GateWait           time.Duration
	IntradayCacheLimit int
	TickerFactory      yahooTickerFactory
	TimeNow            func() time.Time
	Location           *time.Location
	intradayMu         sync.Mutex
	intradayCache      map[string]intradayHistoryCacheEntry
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
		TimeNow: time.Now, Location: location, IntradayCacheLimit: 4,
	}
}

func (c *YFinanceHistoryClient) Fetch(ctx context.Context, symbol, period string) (HistoryResult, error) {
	return c.fetch(ctx, symbol, period, false)
}

func (c *YFinanceHistoryClient) FetchAdjusted(ctx context.Context, symbol, period string) (HistoryResult, error) {
	return c.fetch(ctx, symbol, period, true)
}

// FetchChanAdjusted derives adjusted OHLC from Yahoo's raw OHLC and adjusted
// close in one history response. Chan analysis must never silently fall back
// to raw prices.
func (c *YFinanceHistoryClient) FetchChanAdjusted(ctx context.Context, symbol, period string) (ChanAdjustedHistoryResult, error) {
	if err := ctx.Err(); err != nil {
		return ChanAdjustedHistoryResult{}, err
	}
	if c == nil {
		return ChanAdjustedHistoryResult{}, errors.New("go-yfinance history client is nil")
	}
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		if strings.EqualFold(strings.TrimSpace(symbol), "^GSPC") {
			normalized = "^GSPC"
		} else {
			return ChanAdjustedHistoryResult{}, err
		}
	}
	period = strings.ToLower(strings.TrimSpace(period))
	if !supportedHistoryPeriod(period) {
		return ChanAdjustedHistoryResult{}, fmt.Errorf("unsupported Yahoo history period %q", period)
	}
	now := time.Now()
	if c.TimeNow != nil {
		now = c.TimeNow()
	}
	params := historyRequestParams(period, false, now)
	bars, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.TickerFactory, normalized, func(ticker yahooTicker) ([]ym.Bar, error) {
		return ticker.History(params)
	})
	if err != nil {
		return ChanAdjustedHistoryResult{}, fmt.Errorf("fetch adjusted Chan history: %w", err)
	}
	result, err := normalizeYFinanceChanAdjustedHistory(normalized, bars, c.Location)
	if err != nil {
		return ChanAdjustedHistoryResult{}, err
	}
	result.FetchedAt, result.Source = now.UTC(), "go-yfinance"
	return result, nil
}

func normalizeYFinanceChanAdjustedHistory(symbol string, bars []ym.Bar, location *time.Location) (ChanAdjustedHistoryResult, error) {
	if len(bars) == 0 {
		return ChanAdjustedHistoryResult{}, errors.New("go-yfinance returned empty adjusted Chan history")
	}
	if location == nil {
		location = time.UTC
	}
	joined := make([]map[string]any, 0, len(bars))
	for index, bar := range bars {
		if bar.Date.IsZero() {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted Chan history bar %d has missing date", index)
		}
		adjustedClose := bar.AdjClose
		if adjustedClose == 0 {
			// Yahoo may omit adjclose for symbols with no corporate-action
			// adjustment. The SDK's auto-adjust path treats that as factor 1.
			adjustedClose = bar.Close
		}
		if bar.Close <= 0 || adjustedClose <= 0 || bar.Volume < 0 ||
			math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) || math.IsNaN(adjustedClose) || math.IsInf(adjustedClose, 0) {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted Chan history bar %d has invalid close or volume", index)
		}
		factor := adjustedClose / bar.Close
		if factor <= 0 || math.IsNaN(factor) || math.IsInf(factor, 0) {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted Chan history bar %d has invalid adjustment factor", index)
		}
		values := []float64{bar.Open, bar.High, bar.Low}
		for _, value := range values {
			if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted Chan history bar %d has invalid raw OHLC", index)
			}
		}
		if bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted Chan history bar %d has inconsistent raw OHLC", index)
		}
		date := bar.Date.In(location).Format(time.DateOnly)
		joined = append(joined, map[string]any{
			"date": date,
			"open": bar.Open * factor, "high": bar.High * factor, "low": bar.Low * factor, "close": adjustedClose,
			"volume": float64(bar.Volume), "raw_open": bar.Open, "raw_high": bar.High, "raw_low": bar.Low,
			"raw_close": bar.Close, "raw_volume": float64(bar.Volume), "adjustment_factor": factor,
		})
	}
	return ChanAdjustedHistoryResult{Symbol: symbol, Bars: joined, Source: "go-yfinance", AdjustmentType: "split_dividend_auto", Timeframe: "1d"}, nil
}

func pairChanAdjustedBars(adjusted, raw HistoryResult) (ChanAdjustedHistoryResult, error) {
	rawByDate := make(map[string]map[string]any, len(raw.Bars))
	for _, bar := range raw.Bars {
		if date, ok := bar["date"].(string); ok {
			rawByDate[date] = bar
		}
	}
	joined := make([]map[string]any, 0, len(adjusted.Bars))
	for _, bar := range adjusted.Bars {
		date, ok := bar["date"].(string)
		if !ok {
			continue
		}
		if parsed, err := time.Parse(time.DateOnly, date); err != nil || parsed.Format(time.DateOnly) != date {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("invalid adjusted Chan bar date %q", date)
		}
		rawBar, found := rawByDate[date]
		if !found {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("raw OHLC missing for adjusted Chan bar %s", date)
		}
		adjustedClose, adjOK := historyNumber(bar["close"])
		rawClose, rawOK := historyNumber(rawBar["close"])
		if !adjOK || !rawOK || adjustedClose <= 0 || rawClose <= 0 {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("invalid raw or adjusted close for Chan bar %s", date)
		}
		item := make(map[string]any, len(bar)+6)
		for key, value := range bar {
			item[key] = value
		}
		factor := adjustedClose / rawClose
		for _, field := range []string{"open", "high", "low", "close", "volume"} {
			value, valid := historyNumber(rawBar[field])
			if !valid || field != "volume" && value <= 0 || field == "volume" && value < 0 {
				return ChanAdjustedHistoryResult{}, fmt.Errorf("raw %s is invalid for Chan bar %s", field, date)
			}
			item["raw_"+field] = value
			if field == "volume" {
				continue
			}
			adjustedValue, valid := historyNumber(bar[field])
			if !valid || adjustedValue <= 0 || math.Abs(adjustedValue-value*factor) > 1e-6*math.Max(1, adjustedValue) {
				return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted %s is invalid for Chan bar %s", field, date)
			}
		}
		if rawBarHigh, ok := historyNumber(rawBar["high"]); !ok {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("raw high is invalid for Chan bar %s", date)
		} else if rawBarLow, ok := historyNumber(rawBar["low"]); !ok || rawBarLow > rawBarHigh {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("raw OHLC is inconsistent for Chan bar %s", date)
		}
		if adjustedHigh, ok := historyNumber(bar["high"]); !ok {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted high is invalid for Chan bar %s", date)
		} else if adjustedLow, ok := historyNumber(bar["low"]); !ok || adjustedLow > adjustedHigh {
			return ChanAdjustedHistoryResult{}, fmt.Errorf("adjusted OHLC is inconsistent for Chan bar %s", date)
		}
		item["adjustment_factor"] = factor
		joined = append(joined, item)
	}
	if len(joined) == 0 {
		return ChanAdjustedHistoryResult{}, errors.New("no paired raw and adjusted Chan bars")
	}
	fetchedAt := adjusted.FetchedAt
	if raw.FetchedAt.After(fetchedAt) {
		fetchedAt = raw.FetchedAt
	}
	return ChanAdjustedHistoryResult{Symbol: adjusted.Symbol, Bars: joined, FetchedAt: fetchedAt, Source: adjusted.Source,
		AdjustmentType: "split_dividend_auto", Timeframe: "1d"}, nil
}

func historyNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	case int64:
		return float64(number), true
	case int:
		return float64(number), true
	default:
		return 0, false
	}
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
	limit := c.IntradayCacheLimit
	if limit < 1 {
		limit = 4
	}
	if _, exists := c.intradayCache[cacheKey]; !exists {
		for len(c.intradayCache) >= limit {
			oldestKey := ""
			oldestAt := time.Time{}
			for key, entry := range c.intradayCache {
				if oldestKey == "" || entry.fetchedAt.Before(oldestAt) {
					oldestKey, oldestAt = key, entry.fetchedAt
				}
			}
			if oldestKey == "" {
				break
			}
			delete(c.intradayCache, oldestKey)
		}
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
	historyNow := time.Now()
	if c.TimeNow != nil {
		historyNow = c.TimeNow()
	}
	historyParams := historyRequestParams(period, adjusted, historyNow)
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

func historyRequestParams(period string, adjusted bool, now time.Time) ym.HistoryParams {
	params := ym.HistoryParams{Period: period, Interval: "1d", AutoAdjust: adjusted, Actions: false, PrePost: false, KeepNA: false}
	if period == "max" {
		end := time.Unix(now.Unix(), 0)
		params.Period = ""
		params.Start = ptrTime(time.Unix(end.Unix()-3122064000+5, 0))
		params.End = ptrTime(end)
	}
	return params
}

func ptrTime(value time.Time) *time.Time { return &value }

func supportedHistoryPeriod(period string) bool {
	switch period {
	case "1mo", "6mo", "1y", "2y", "5y", "10y", "20y", "max":
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
