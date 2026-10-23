package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"option-scope-go/internal/outboundhttp"
)

const (
	alpacaHistoricalBarsPath  = "/v2/stocks/bars"
	alpacaHistoricalBarLimit  = 10000
	alpacaHistoryCacheTTL     = 24 * time.Hour
	alpacaHistoryCacheEntries = 4
	alpacaHistoryBodyLimit    = 16 << 20
)

type alpacaIntradayCacheEntry struct {
	result    HistoryResult
	fetchedAt time.Time
}

type alpacaHistoricalBarsPage struct {
	Bars          map[string][]alpacaHistoricalBar `json:"bars"`
	NextPageToken *string                          `json:"next_page_token"`
}

type alpacaHistoricalBar struct {
	Timestamp time.Time   `json:"t"`
	Open      float64     `json:"o"`
	High      float64     `json:"h"`
	Low       float64     `json:"l"`
	Close     float64     `json:"c"`
	Volume    json.Number `json:"v"`
}

// FetchIntraday 获取 Alpaca 最近六个月的 15 分钟常规交易时段股票 K 线。
func (a *Alpaca) FetchIntraday(ctx context.Context, rawSymbol, interval string) (HistoryResult, error) {
	if err := ctx.Err(); err != nil {
		return HistoryResult{}, err
	}
	if a == nil {
		return HistoryResult{}, errors.New("Alpaca historical bars client is unavailable")
	}
	if interval != "15m" {
		return HistoryResult{}, fmt.Errorf("unsupported Alpaca intraday history interval %q", interval)
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return HistoryResult{}, err
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	cacheKey := symbol + ":" + interval
	a.historyMu.Lock()
	if cached, found := a.historyCache[cacheKey]; found {
		age := now.Sub(cached.fetchedAt)
		if age >= 0 && age < alpacaHistoryCacheTTL {
			a.historyMu.Unlock()
			return cached.result, nil
		}
	}
	a.historyMu.Unlock()
	if !a.CanRequest() {
		return HistoryResult{}, errors.New("Alpaca historical bars credentials are unavailable")
	}

	start := now.AddDate(0, -6, 0)
	bars, err := a.fetchHistoricalBars(ctx, symbol, start, now)
	if err != nil {
		return HistoryResult{}, err
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	normalized, err := normalizeAlpacaIntradayBars(symbol, bars, location)
	if err != nil {
		return HistoryResult{}, err
	}
	result := HistoryResult{
		Symbol: symbol, Bars: normalized, FetchedAt: now.UTC(), Source: "alpaca-iex",
	}
	a.markAuthSuccess()
	a.cacheIntradayHistory(cacheKey, result, now)
	return result, nil
}

func (a *Alpaca) cacheIntradayHistory(key string, result HistoryResult, fetchedAt time.Time) {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	if a.historyCache == nil {
		a.historyCache = make(map[string]alpacaIntradayCacheEntry)
	}
	if _, exists := a.historyCache[key]; !exists {
		for len(a.historyCache) >= alpacaHistoryCacheEntries {
			oldestKey := ""
			oldestAt := time.Time{}
			for candidate, entry := range a.historyCache {
				if oldestKey == "" || entry.fetchedAt.Before(oldestAt) {
					oldestKey, oldestAt = candidate, entry.fetchedAt
				}
			}
			if oldestKey == "" {
				break
			}
			delete(a.historyCache, oldestKey)
		}
	}
	a.historyCache[key] = alpacaIntradayCacheEntry{result: result, fetchedAt: fetchedAt}
}

func (a *Alpaca) fetchHistoricalBars(ctx context.Context, symbol string, start, end time.Time) ([]alpacaHistoricalBar, error) {
	endpoint, err := url.Parse(a.baseURL + alpacaHistoricalBarsPath)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("invalid Alpaca data base URL")
	}
	if a.client == nil {
		return nil, errors.New("Alpaca historical bars HTTP client is unavailable")
	}
	query := endpoint.Query()
	query.Set("symbols", symbol)
	query.Set("timeframe", "15Min")
	query.Set("start", start.UTC().Format(time.RFC3339Nano))
	query.Set("end", end.UTC().Format(time.RFC3339Nano))
	query.Set("limit", fmt.Sprint(alpacaHistoricalBarLimit))
	query.Set("adjustment", "raw")
	query.Set("feed", "iex")
	query.Set("sort", "asc")

	allBars := make([]alpacaHistoricalBar, 0, 5000)
	seenTokens := make(map[string]struct{})
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pageToken != "" {
			if _, exists := seenTokens[pageToken]; exists {
				return nil, errors.New("Alpaca historical bars returned a repeated page token")
			}
			seenTokens[pageToken] = struct{}{}
			query.Set("page_token", pageToken)
		} else {
			query.Del("page_token")
		}
		endpoint.RawQuery = query.Encode()
		page, err := a.fetchHistoricalBarsPage(ctx, endpoint.String())
		if err != nil {
			return nil, err
		}
		allBars = append(allBars, page.Bars[symbol]...)
		if page.NextPageToken == nil || strings.TrimSpace(*page.NextPageToken) == "" {
			break
		}
		pageToken = *page.NextPageToken
	}
	return allBars, nil
}

func (a *Alpaca) fetchHistoricalBarsPage(ctx context.Context, address string) (alpacaHistoricalBarsPage, error) {
	request, err := outboundhttp.NewRequest(ctx, http.MethodGet, address, nil)
	if err != nil {
		return alpacaHistoricalBarsPage{}, errors.New("could not create Alpaca historical bars request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "option-scope/1.0")
	request.Header.Set("APCA-API-KEY-ID", a.apiKey)
	request.Header.Set("APCA-API-SECRET-KEY", a.secret)
	release, err := acquireGateTimeout(ctx, a.Gate, a.GateWait)
	if err != nil {
		return alpacaHistoricalBarsPage{}, err
	}
	defer release()

	response, err := outboundhttp.Do(a.client, request)
	if err != nil {
		if ctx.Err() != nil {
			return alpacaHistoricalBarsPage{}, ctx.Err()
		}
		var networkErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkErr) && networkErr.Timeout()) {
			return alpacaHistoricalBarsPage{}, errors.New("Alpaca historical bars request timed out")
		}
		return alpacaHistoricalBarsPage{}, errors.New("Alpaca historical bars request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			a.markAuthFailure()
		}
		return alpacaHistoricalBarsPage{}, fmt.Errorf("Alpaca historical bars request failed (HTTP %d)", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, alpacaHistoryBodyLimit))
	decoder.UseNumber()
	var page alpacaHistoricalBarsPage
	if err := decoder.Decode(&page); err != nil {
		if ctx.Err() != nil {
			return alpacaHistoricalBarsPage{}, ctx.Err()
		}
		return alpacaHistoricalBarsPage{}, errors.New("Alpaca returned invalid historical bars")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return alpacaHistoricalBarsPage{}, errors.New("Alpaca returned invalid historical bars")
	}
	return page, nil
}

func normalizeAlpacaIntradayBars(symbol string, bars []alpacaHistoricalBar, location *time.Location) ([]map[string]any, error) {
	if location == nil {
		location, _ = time.LoadLocation("America/New_York")
	}
	if location == nil {
		location = time.UTC
	}
	byTime := make(map[string]map[string]any, len(bars))
	for index, bar := range bars {
		if bar.Timestamp.IsZero() {
			continue
		}
		localTime := bar.Timestamp.In(location)
		minuteOfDay := localTime.Hour()*60 + localTime.Minute()
		if localTime.Weekday() == time.Saturday || localTime.Weekday() == time.Sunday || minuteOfDay < 9*60+30 || minuteOfDay >= 16*60 {
			continue
		}
		if bar.Close <= 0 || bar.Volume == "" || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			continue
		}
		volume, err := bar.Volume.Int64()
		if err != nil || volume < 0 {
			continue
		}
		if math.IsNaN(bar.Open) || math.IsInf(bar.Open, 0) || math.IsNaN(bar.High) || math.IsInf(bar.High, 0) || math.IsNaN(bar.Low) || math.IsInf(bar.Low, 0) ||
			bar.Open <= 0 || bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			return nil, fmt.Errorf("Alpaca intraday bar %d has invalid OHLC", index)
		}
		key := localTime.Format("2006-01-02 15:04")
		byTime[key] = map[string]any{
			"date": key, "open": bar.Open, "high": bar.High, "low": bar.Low, "close": bar.Close, "volume": volume,
		}
	}
	if len(byTime) == 0 {
		return nil, errors.New("Alpaca returned no valid regular-session intraday bars for " + symbol)
	}

	times := make([]string, 0, len(byTime))
	for key := range byTime {
		times = append(times, key)
	}
	sort.Strings(times)
	result := make([]map[string]any, 0, len(times))
	for _, key := range times {
		result = append(result, byTime[key])
	}
	return result, nil
}
