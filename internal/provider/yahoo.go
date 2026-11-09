package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
)

const (
	optionsURL                     = "https://query1.finance.yahoo.com/v7/finance/options/%s"
	futuresChartURL                = "https://query1.finance.yahoo.com/v8/finance/chart/%s"
	lowMemoryQuoteTickerCacheLimit = 128
	defaultQuoteTickerCacheLimit   = 512
)

var symbolPattern = regexp.MustCompile(`^(?:\^[A-Z0-9][A-Z0-9.\-]{0,8}|[A-Z0-9][A-Z0-9.\-]{0,9}|[A-Z0-9]+(?:-[A-Z0-9]+)?=F)$`)

type Yahoo struct {
	client        *http.Client
	authClient    *yahooauth.Client
	optionsAuth   yahooCrumbGetter
	optionsURL    string
	chartURL      string
	fundamentals  *FundamentalsClient
	earnings      *YahooEarningsClient
	tickerFactory yahooTickerFactory
	timeout       time.Duration
	gateWait      time.Duration
	semaphore     chan struct{}
	now           func() time.Time
	// quoteTickers reuses one SDK ticker per symbol for quote refreshes. The SDK
	// caches the Yahoo cookie/crumb inside the ticker, so reuse pays the
	// authentication handshake once per process instead of on every refresh.
	// Only Quote uses these; option-chain and fundamentals keep short-lived
	// tickers so their own caches cannot serve stale data.
	quoteTickersMu      sync.Mutex
	quoteTickers        map[string]yahooTickerCacheEntry
	quoteTickerInit     map[string]*yahooTickerInit
	quoteTickerCacheMax int
	quoteTickerSequence uint64
	// interactiveGate reserves upstream slots for the first-render requests
	// (current price and expiration catalogue). Option chains, Gamma batches,
	// fundamentals, and history share semaphore; without a separate gate a
	// multi-year history fetch can starve the price card for the whole
	// single-upstream-slot low-memory configuration.
	interactiveGate chan struct{}
}

type yahooTickerInit struct {
	done   chan struct{}
	ticker yahooTicker
	err    error
}

type yahooTickerCacheEntry struct {
	ticker   yahooTicker
	lastUsed uint64
}

func boundedQuoteTickerCacheEntries(lowMemory bool) int {
	if lowMemory {
		return lowMemoryQuoteTickerCacheLimit
	}
	return defaultQuoteTickerCacheLimit
}

type yahooCrumbGetter interface {
	GetWithCrumb(string, url.Values) (*yahooauth.Response, error)
}

func NewYahoo(timeout time.Duration, concurrency int) *Yahoo {
	yahoo, err := NewYahooWithProxy(timeout, concurrency, "")
	if err == nil {
		return yahoo
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if concurrency < 1 {
		concurrency = 1
	}
	client := outboundhttp.DefaultBrowserClient(timeout)
	semaphore := make(chan struct{}, concurrency)
	// Quote and the expiration catalogue are tiny responses on the first-render
	// path. Keep two dedicated slots even when the shared gate has one, so a
	// history or chain fetch cannot delay them; both requests together stay far
	// below the memory cost of one option chain response.
	interactiveGate := make(chan struct{}, 2)
	fundamentals := NewFundamentalsClient(client)
	fundamentals.Gate, fundamentals.GateWait = semaphore, timeout
	earnings := NewYahooEarningsClient(client)
	earnings.Gate, earnings.GateWait = semaphore, timeout
	return &Yahoo{client: client, optionsURL: optionsURL, chartURL: futuresChartURL, fundamentals: fundamentals, earnings: earnings, timeout: timeout, gateWait: timeout, semaphore: semaphore, now: time.Now, quoteTickers: make(map[string]yahooTickerCacheEntry), quoteTickerInit: make(map[string]*yahooTickerInit), quoteTickerCacheMax: boundedQuoteTickerCacheEntries(false), interactiveGate: interactiveGate}
}

func NewYahooWithProxy(timeout time.Duration, concurrency int, proxyAddress string) (*Yahoo, error) {
	return newYahooWithProxy(timeout, concurrency, proxyAddress, false)
}

func newYahooWithProxy(timeout time.Duration, concurrency int, proxyAddress string, lowMemory bool) (*Yahoo, error) {
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if concurrency < 1 {
		concurrency = 1
	}
	client, err := outboundhttp.BrowserClient(timeout, proxyAddress)
	if err != nil {
		return nil, err
	}
	sdkProxy := outboundhttp.ProxyAddressFromTransport(client.Transport)
	ja3, userAgent := outboundhttp.BrowserOptions()
	authTimeoutSeconds := int(math.Ceil(timeout.Seconds()))
	authClient, err := yahooauth.New(
		yahooauth.WithTimeout(authTimeoutSeconds),
		yahooauth.WithProxy(sdkProxy),
		yahooauth.WithJA3(ja3),
		yahooauth.WithUserAgent(userAgent),
	)
	if err != nil {
		return nil, fmt.Errorf("create go-yfinance Yahoo client: %w", err)
	}
	semaphore := make(chan struct{}, concurrency)
	// See NewYahoo: the price and expiry requests keep their own slots in every
	// concurrency configuration, including the one-slot low-memory profile.
	interactiveGate := make(chan struct{}, 2)
	fundamentals := NewFundamentalsClient(client)
	fundamentals.Gate = semaphore
	fundamentals.GateWait = timeout
	earnings := NewYahooEarningsClient(client)
	earnings.Gate = semaphore
	earnings.GateWait = timeout
	optionsAuth := yahooauth.NewAuthManager(authClient)
	fundamentals.crumbAuth = optionsAuth
	tickerFactory := newYahooTickerFactory(authClient)
	fundamentals.tickerFactory = tickerFactory
	earnings.tickerFactory = tickerFactory
	return &Yahoo{
		client:              client,
		authClient:          authClient,
		optionsAuth:         optionsAuth,
		optionsURL:          optionsURL,
		chartURL:            futuresChartURL,
		fundamentals:        fundamentals,
		earnings:            earnings,
		tickerFactory:       tickerFactory,
		timeout:             timeout,
		gateWait:            timeout,
		semaphore:           semaphore,
		now:                 time.Now,
		quoteTickers:        make(map[string]yahooTickerCacheEntry),
		quoteTickerInit:     make(map[string]*yahooTickerInit),
		quoteTickerCacheMax: boundedQuoteTickerCacheEntries(lowMemory),
		interactiveGate:     interactiveGate,
	}, nil
}

func (y *Yahoo) Close() error {
	if y != nil {
		if y.authClient != nil {
			y.authClient.Close()
		}
		if y.client != nil {
			outboundhttp.CloseIdleConnections(y.client)
		}
	}
	return nil
}

// interactiveUpstreamGate returns the dedicated first-render lane. It falls
// back to the shared gate for manually constructed clients that predate it.
func (y *Yahoo) interactiveUpstreamGate() chan struct{} {
	if y != nil && y.interactiveGate != nil {
		return y.interactiveGate
	}
	if y == nil {
		return nil
	}
	return y.semaphore
}

// quoteTicker returns a cached SDK ticker for the symbol so successive quote
// refreshes reuse the Yahoo cookie/crumb held by that ticker. The SDK ticker is
// created with a shared client (ownsClient=false), so runYahooSDK's deferred
// Close is a no-op and the instance stays warm between refreshes.
func (y *Yahoo) quoteTicker(ctx context.Context, symbol string) (yahooTicker, error) {
	if y.tickerFactory == nil {
		return nil, errors.New("go-yfinance ticker factory is unavailable")
	}
	y.quoteTickersMu.Lock()
	if entry, ok := y.quoteTickers[symbol]; ok {
		y.quoteTickerSequence++
		entry.lastUsed = y.quoteTickerSequence
		y.quoteTickers[symbol] = entry
		y.quoteTickersMu.Unlock()
		return entry.ticker, nil
	}
	if pending := y.quoteTickerInit[symbol]; pending != nil {
		y.quoteTickersMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			return pending.ticker, pending.err
		}
	}
	pending := &yahooTickerInit{done: make(chan struct{})}
	y.quoteTickerInit[symbol] = pending
	y.quoteTickersMu.Unlock()
	ticker, err := y.tickerFactory(symbol)
	y.quoteTickersMu.Lock()
	if err != nil {
		pending.err = err
	} else {
		y.cacheQuoteTickerLocked(symbol, ticker)
		pending.ticker = ticker
	}
	delete(y.quoteTickerInit, symbol)
	close(pending.done)
	y.quoteTickersMu.Unlock()
	return pending.ticker, pending.err
}

func (y *Yahoo) cacheQuoteTickerLocked(symbol string, ticker yahooTicker) {
	limit := y.quoteTickerCacheMax
	if limit < 1 {
		limit = lowMemoryQuoteTickerCacheLimit
	}
	if len(y.quoteTickers) >= limit {
		target := limit * 9 / 10
		if target >= limit {
			target = limit - 1
		}
		for len(y.quoteTickers) > target {
			oldestSymbol := ""
			oldestUse := ^uint64(0)
			for cachedSymbol, entry := range y.quoteTickers {
				if entry.lastUsed < oldestUse {
					oldestSymbol, oldestUse = cachedSymbol, entry.lastUsed
				}
			}
			if oldestSymbol == "" {
				break
			}
			delete(y.quoteTickers, oldestSymbol)
		}
	}
	y.quoteTickerSequence++
	y.quoteTickers[symbol] = yahooTickerCacheEntry{ticker: ticker, lastUsed: y.quoteTickerSequence}
}

// Fundamentals fetches optional Yahoo financial inputs used by valuation. It is
// independent of Quote so enrichment failures cannot affect market snapshots.
func (y *Yahoo) Fundamentals(ctx context.Context, symbol string) (YahooFundamentals, error) {
	if y == nil || y.fundamentals == nil {
		return YahooFundamentals{}, errors.New("Yahoo fundamentals client is unavailable")
	}
	return y.fundamentals.Fetch(ctx, symbol)
}

// Earnings remains available for explicit callers; the application no longer invokes it.
func (y *Yahoo) Earnings(ctx context.Context, symbol string) ([]string, error) {
	if y == nil || y.earnings == nil {
		return nil, errors.New("Yahoo earnings client is unavailable")
	}
	return y.earnings.Fetch(ctx, symbol)
}

func NormalizeSymbol(raw string) (string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	switch symbol {
	case ".NDX":
		symbol = "^NDX"
	case ".SPX":
		symbol = "^GSPC"
	case ".DJI":
		symbol = "^DJI"
	case ".IXIC":
		symbol = "^IXIC"
	}
	if !symbolPattern.MatchString(symbol) {
		return "", fmt.Errorf("invalid symbol: %s", raw)
	}
	return symbol, nil
}

// IsIndexSymbol reports whether a symbol is a Yahoo market-index ticker.
func IsIndexSymbol(raw string) bool {
	symbol, err := NormalizeSymbol(raw)
	if err != nil {
		return false
	}
	switch symbol {
	case "^NDX", "^GSPC", "^DJI", "^IXIC":
		return true
	default:
		return false
	}
}

// IsFutureSymbol reports whether a symbol is a Yahoo continuous futures ticker.
func IsFutureSymbol(raw string) bool {
	symbol, err := NormalizeSymbol(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(symbol, "=F")
}

func NormalizeExpiration(raw string) (string, error) {
	value, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("expiration must use YYYY-MM-DD")
	}
	return value.Format("2006-01-02"), nil
}

type yahooHTTPStatusError struct {
	status int
}

func (e *yahooHTTPStatusError) Error() string {
	return fmt.Sprintf("Yahoo returned HTTP %d", e.status)
}

type yahooAPIError struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

type yahooChartResult struct {
	Meta       yahooChartMeta `json:"meta"`
	Timestamp  []*int64       `json:"timestamp"`
	Indicators struct {
		Quote []struct {
			Open   []*float64 `json:"open"`
			High   []*float64 `json:"high"`
			Low    []*float64 `json:"low"`
			Close  []*float64 `json:"close"`
			Volume []*int64   `json:"volume"`
		} `json:"quote"`
		Adjusted []struct {
			Close []*float64 `json:"adjclose"`
		} `json:"adjclose"`
	} `json:"indicators"`
}

type chartEnvelope struct {
	Chart struct {
		Result []yahooChartResult `json:"result"`
		Error  *yahooAPIError     `json:"error"`
	} `json:"chart"`
}

type yahooChartMeta struct {
	Symbol             string  `json:"symbol"`
	Currency           string  `json:"currency"`
	RegularMarketPrice float64 `json:"regularMarketPrice"`
	RegularMarketOpen  float64 `json:"regularMarketOpen"`
	PreviousClose      float64 `json:"previousClose"`
	ChartPreviousClose float64 `json:"chartPreviousClose"`
	MarketState        string  `json:"marketState"`
	RegularMarketTime  int64   `json:"regularMarketTime"`
}

func (e *yahooAPIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Description == "" {
		return "Yahoo API error: " + e.Code
	}
	if e.Code == "" {
		return "Yahoo API error: " + e.Description
	}
	return fmt.Sprintf("Yahoo API error (%s): %s", e.Code, e.Description)
}

func yahooSessionOf(at time.Time) string {
	minute := at.Hour()*60 + at.Minute()
	switch {
	case minute >= 4*60 && minute < 9*60+30:
		return "pre"
	case minute >= 9*60+30 && minute < 16*60:
		return "regular"
	case minute >= 16*60 && minute < 20*60:
		return "post"
	default:
		return "overnight"
	}
}

func yahooTradingDay(at time.Time) time.Time {
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	if at.Hour()*60+at.Minute() >= 20*60 {
		return day.AddDate(0, 0, 1)
	}
	return day
}

func yahooTradingSession(at time.Time) bool { return isNYSETradingDay(yahooTradingDay(at)) }

func (y *Yahoo) marketNow() time.Time {
	if y != nil && y.now != nil {
		return y.now()
	}
	return time.Now()
}

func yahooSessionState(now time.Time, latest *time.Time) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	now = now.In(location)
	if !yahooTradingSession(now) {
		return "CLOSED"
	}
	if latest != nil {
		barTime := latest.In(location)
		if math.Abs(now.Sub(barTime).Seconds()) <= (30 * time.Minute).Seconds() {
			if yahooTradingSession(barTime) {
				return map[string]string{"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}[yahooSessionOf(barTime)]
			}
			return "CLOSED"
		}
	}
	minute := now.Hour()*60 + now.Minute()
	closeMinute := marketcalendar.RegularCloseMinute(now)
	if !isNYSERegularSession(now) && minute >= closeMinute && minute < 20*60 {
		return "POST"
	}
	return map[string]string{"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}[yahooSessionOf(now)]
}

// yahooFutureSessionState 不使用 NYSE 交易日历判断期货是否活跃。
// CME 连续期货在美股收盘后仍会交易，只有日间维护窗口和无新成交时才应标记为 CLOSED。
func yahooFutureSessionState(now, latest time.Time, marketState string) string {
	if !latest.IsZero() && math.Abs(now.Sub(latest).Seconds()) > (30*time.Minute).Seconds() {
		return "CLOSED"
	}
	switch strings.ToUpper(strings.TrimSpace(marketState)) {
	case "REGULAR":
		return "REGULAR"
	case "PRE", "PREPRE":
		return "PRE"
	case "POST", "POSTPOST":
		return "POST"
	case "OVERNIGHT":
		return "OVERNIGHT"
	case "CLOSED":
		return "CLOSED"
	}
	if latest.IsZero() {
		return "CLOSED"
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	local := now.In(location)
	minute := local.Hour()*60 + local.Minute()
	if minute >= 17*60 && minute < 18*60 {
		return "CLOSED"
	}
	if minute >= 9*60+30 && minute < 16*60 {
		return "REGULAR"
	}
	return "OVERNIGHT"
}

func yahooFutureTradingDate(at time.Time, location *time.Location) string {
	local := at.In(location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	if local.Hour()*60+local.Minute() >= 18*60 {
		date = date.AddDate(0, 0, 1)
	}
	for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		date = date.AddDate(0, 0, 1)
	}
	return date.Format(time.DateOnly)
}

func yahooCurrentSessionTime(at, now time.Time) bool {
	if at.IsZero() || at.Unix() <= 0 {
		return false
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	at, now = at.In(location), now.In(location)
	return !at.After(now) && yahooTradingDay(at).Format("2006-01-02") == yahooTradingDay(now).Format("2006-01-02")
}

func yahooQuoteSessions(quote *yahooQuoteModel, now time.Time) map[string]any {
	sessions := make(map[string]any)
	if quote == nil {
		return sessions
	}
	state := yahooSessionState(now, nil)
	add := func(name string, price, reference float64, priceAt, referenceAt time.Time) {
		if !validYahooPrice(price) && !validYahooPrice(reference) {
			return
		}
		summary := make(map[string]any, 4)
		if validYahooPrice(price) {
			summary["price"] = price
		}
		if validYahooPrice(reference) {
			summary["reference_close"] = reference
			if validYahooPrice(price) {
				summary["change_percent"] = (price - reference) / reference * 100
			}
		}
		at := priceAt
		if at.IsZero() || at.Unix() <= 0 {
			at = referenceAt
		}
		if !at.IsZero() && at.Unix() > 0 {
			summary["as_of"] = at.Format(time.RFC3339)
			// Quote timestamps can arrive in the host's timezone. Keep a
			// trading-day label in ET so clients do not render +08:00 times as
			// the next calendar day.
			if location, err := time.LoadLocation("America/New_York"); err == nil {
				summary["date"] = at.In(location).Format(time.DateOnly)
			}
		}
		sessions[name] = summary
	}
	if state == "PRE" && yahooHasCurrentPreMarketPrice(quote, now) {
		// Before the open, RegularMarketPrice is the last completed session close;
		// RegularMarketPreviousClose is one session older.
		add("pre", quote.PreMarketPrice, quote.RegularMarketPrice, quote.PreMarketTime, quote.RegularMarketTime)
	}
	// Keep the formal close baseline even when Yahoo omits the post-market price.
	postPrice := float64(0)
	postTime := time.Time{}
	if (state == "POST" || state == "OVERNIGHT") && yahooCurrentSessionTime(quote.PostMarketTime, now) {
		postPrice, postTime = quote.PostMarketPrice, quote.PostMarketTime
	}
	add("post", postPrice, quote.RegularMarketPrice, postTime, quote.RegularMarketTime)
	// During the overnight session Yahoo still carries the after-hours price of
	// the trading day that just finished. It is deliberately kept out of
	// sessions.post (which means "the current trading day's after-hours"), but
	// the analysis panel needs it: with no overnight quote for the post-market
	// basis it would otherwise fall back to the overnight price and label that
	// as 盘后价. Cap the age so a symbol without any after-hours trade for days
	// does not surface an ancient print as yesterday's close.
	if state == "OVERNIGHT" && !yahooCurrentSessionTime(quote.PostMarketTime, now) &&
		validYahooPrice(quote.PostMarketPrice) && !quote.PostMarketTime.IsZero() &&
		!quote.PostMarketTime.After(now) && now.Sub(quote.PostMarketTime) <= 7*24*time.Hour {
		add("previous_post", quote.PostMarketPrice, quote.RegularMarketPrice, quote.PostMarketTime, quote.RegularMarketTime)
	}
	return sessions
}

func validYahooPrice(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func yahooQuoteHasCurrentPrice(quote *yahooQuoteModel, now time.Time) bool {
	if quote == nil {
		return false
	}
	switch yahooSessionState(now, nil) {
	case "PRE":
		return (validYahooPrice(quote.PreMarketPrice) && yahooCurrentSessionTime(quote.PreMarketTime, now)) || validYahooPrice(quote.RegularMarketPrice)
	case "POST":
		return (validYahooPrice(quote.PostMarketPrice) && yahooCurrentSessionTime(quote.PostMarketTime, now)) || validYahooPrice(quote.RegularMarketPrice)
	default:
		return validYahooPrice(quote.RegularMarketPrice)
	}
}

func yahooHasCurrentPreMarketPrice(quote *yahooQuoteModel, now time.Time) bool {
	if quote == nil || !validYahooPrice(quote.PreMarketPrice) {
		return false
	}
	params, ok := yahooPreMarketHistoryParams(now)
	return ok && !quote.PreMarketTime.Before(*params.Start) && !quote.PreMarketTime.After(*params.End)
}

// quoteFutureFromChart 使用 Yahoo chart 接口读取连续期货的最新分钟 K 线。
// 期货报价不需要 quoteSummary 的完整字段，也不需要先获取 cookie/crumb；
// 直接走 chart 可以避免 go-yfinance 首次认证和失败重试造成的长时间等待。
func (y *Yahoo) quoteFutureFromChart(ctx context.Context, symbol string) (model.Quote, error) {
	if y == nil || y.client == nil {
		return model.Quote{}, errors.New("Yahoo chart client is unavailable")
	}
	template := y.chartURL
	if strings.TrimSpace(template) == "" {
		template = futuresChartURL
	}
	endpoint := fmt.Sprintf(template, url.PathEscape(symbol))
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return model.Quote{}, errors.New("invalid Yahoo futures chart URL")
	}
	query := parsed.Query()
	query.Set("range", "1d")
	query.Set("interval", "1m")
	query.Set("includePrePost", "true")
	query.Set("events", "div,splits")
	parsed.RawQuery = query.Encode()

	release, err := acquireGateTimeout(ctx, y.interactiveUpstreamGate(), y.gateWait)
	if err != nil {
		return model.Quote{}, err
	}
	defer release()
	startedAt := time.Now()
	request, err := outboundhttp.NewRequest(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return model.Quote{}, fmt.Errorf("build Yahoo futures chart request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "option-scope-go/1.0")
	response, err := outboundhttp.Do(y.client, request)
	if err != nil {
		if ctx.Err() != nil {
			return model.Quote{}, ctx.Err()
		}
		return model.Quote{}, errors.New("Yahoo futures chart request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return model.Quote{}, fmt.Errorf("Yahoo futures chart returned HTTP %d", response.StatusCode)
	}
	var payload chartEnvelope
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return model.Quote{}, fmt.Errorf("decode Yahoo futures chart response: %w", err)
	}
	if payload.Chart.Error != nil {
		return model.Quote{}, payload.Chart.Error
	}
	if len(payload.Chart.Result) == 0 {
		return model.Quote{}, errors.New("Yahoo futures chart returned no data")
	}
	quote, quoteErr := futureQuoteFromChartResult(symbol, payload.Chart.Result[0], y.marketNow())
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		log.Printf("slow Yahoo futures chart symbol=%s total=%s", symbol, elapsed.Round(time.Millisecond))
	}
	return quote, quoteErr
}

func futureQuoteFromChartResult(symbol string, result yahooChartResult, now time.Time) (model.Quote, error) {
	if len(result.Indicators.Quote) == 0 {
		return model.Quote{}, errors.New("Yahoo futures chart returned no quote bars")
	}
	bars := result.Indicators.Quote[0]
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	latestAt := time.Time{}
	latestPrice := 0.0
	for index, stamp := range result.Timestamp {
		if stamp == nil || index >= len(bars.Close) || bars.Close[index] == nil || !validYahooPrice(*bars.Close[index]) {
			continue
		}
		at := time.Unix(*stamp, 0)
		if at.After(now.Add(5 * time.Minute)) {
			continue
		}
		if latestAt.IsZero() || at.After(latestAt) {
			latestAt, latestPrice = at, *bars.Close[index]
		}
	}
	if latestAt.IsZero() && validYahooPrice(result.Meta.RegularMarketPrice) {
		latestPrice = result.Meta.RegularMarketPrice
		if result.Meta.RegularMarketTime > 0 {
			latestAt = time.Unix(result.Meta.RegularMarketTime, 0)
		}
	}
	if !validYahooPrice(latestPrice) {
		return model.Quote{}, errors.New("Yahoo futures chart returned no valid price")
	}
	previous := result.Meta.PreviousClose
	if !validYahooPrice(previous) {
		previous = result.Meta.ChartPreviousClose
	}
	priceAt := latestAt
	if priceAt.IsZero() {
		priceAt = now
	}
	state := yahooFutureSessionState(now, latestAt, result.Meta.MarketState)
	date := yahooFutureTradingDate(priceAt, location)
	open, high, low, volume := 0.0, 0.0, 0.0, int64(0)
	haveOHLC := false
	for index, stamp := range result.Timestamp {
		if stamp == nil || yahooFutureTradingDate(time.Unix(*stamp, 0), location) != date {
			continue
		}
		if index < len(bars.Open) && bars.Open[index] != nil && validYahooPrice(*bars.Open[index]) && !validYahooPrice(open) {
			open = *bars.Open[index]
		}
		if index < len(bars.High) && bars.High[index] != nil && validYahooPrice(*bars.High[index]) && (!haveOHLC || *bars.High[index] > high) {
			high = *bars.High[index]
		}
		if index < len(bars.Low) && bars.Low[index] != nil && validYahooPrice(*bars.Low[index]) && (!haveOHLC || low == 0 || *bars.Low[index] < low) {
			low = *bars.Low[index]
		}
		if index < len(bars.Volume) && bars.Volume[index] != nil && *bars.Volume[index] > 0 {
			volume += *bars.Volume[index]
		}
		haveOHLC = haveOHLC || validYahooPrice(open) || validYahooPrice(high) || validYahooPrice(low)
	}
	var dailyBar *model.DailyBar
	if haveOHLC {
		if !validYahooPrice(open) {
			open = latestPrice
		}
		if !validYahooPrice(high) {
			high = latestPrice
		}
		if !validYahooPrice(low) {
			low = latestPrice
		}
		if high >= math.Max(open, latestPrice) && low <= math.Min(open, latestPrice) && low <= high {
			dailyBar = &model.DailyBar{Date: date, Open: open, High: high, Low: low, Close: latestPrice, Volume: float64(volume), IsForming: state != "CLOSED"}
		}
	}
	var change *float64
	if validYahooPrice(previous) {
		value := (latestPrice - previous) / previous * 100
		change = &value
	}
	sessions := map[string]any{}
	sessionName := strings.ToLower(state)
	if sessionName == "closed" {
		sessionName = "regular"
	}
	summary := map[string]any{"price": latestPrice, "as_of": priceAt.Format(time.RFC3339), "date": date}
	if validYahooPrice(previous) {
		summary["reference_close"] = previous
		if change != nil {
			summary["change_percent"] = *change
		}
	}
	sessions[sessionName] = summary
	fetched := now.UTC()
	return model.Quote{Symbol: symbol, Price: &latestPrice, ChangePercent: change, TodayOpen: optionalFloat(open), PreviousClose: optionalFloat(previous), DailyBar: dailyBar, Currency: stringOr(result.Meta.Currency, "USD"), MarketState: strings.ToUpper(state), Provider: "yahoo-chart", Source: "upstream", FetchedAt: &fetched, Sessions: sessions, FairValuePending: true}, nil
}

func (y *Yahoo) Quote(ctx context.Context, symbol string) (model.Quote, error) {
	symbol, err := NormalizeSymbol(symbol)
	if err != nil {
		return model.Quote{}, err
	}
	if IsFutureSymbol(symbol) {
		return y.quoteFutureFromChart(ctx, symbol)
	}
	now := y.marketNow()
	state := yahooSessionState(now, nil)
	type quoteSnapshot struct {
		quote     *yahooQuoteModel
		quoteErr  error
		daily     []yahooBarModel
		dailyErr  error
		preMarket []yahooBarModel
	}
	type quotePhaseTimes struct {
		quote, daily, preMarket time.Duration
	}
	var phases quotePhaseTimes
	startedAt := time.Now()
	quoteTickerFactory := func(string) (yahooTicker, error) { return y.quoteTicker(ctx, symbol) }
	quoteGate := y.interactiveUpstreamGate()
	snapshot, fetchErr := runYahooSDK(ctx, quoteGate, y.gateWait, quoteTickerFactory, symbol, func(ticker yahooTicker) (quoteSnapshot, error) {
		started := time.Now()
		quote, quoteErr := ticker.Quote()
		phases.quote = time.Since(started)
		snapshot := quoteSnapshot{quote: quote, quoteErr: quoteErr}
		if state == "PRE" && !yahooHasCurrentPreMarketPrice(quote, now) {
			if params, ok := yahooPreMarketHistoryParams(now); ok {
				started = time.Now()
				snapshot.preMarket, _ = ticker.History(params)
				phases.preMarket = time.Since(started)
			}
		}
		needsDaily := quoteErr != nil || !yahooQuoteHasCurrentPrice(quote, now) || quote == nil || !validYahooPrice(quote.RegularMarketPreviousClose)
		if needsDaily {
			started = time.Now()
			snapshot.daily, snapshot.dailyErr = ticker.History(yahooHistoryParams("1d", false))
			phases.daily = time.Since(started)
		}
		return snapshot, nil
	})
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		log.Printf("slow Yahoo quote symbol=%s total=%s quote=%s premarket_fallback=%s daily_fallback=%s", symbol, elapsed.Round(time.Millisecond), phases.quote.Round(time.Millisecond), phases.preMarket.Round(time.Millisecond), phases.daily.Round(time.Millisecond))
	}
	if fetchErr != nil {
		return model.Quote{}, fetchErr
	}
	metaQuote := snapshot.quote
	sessions := yahooQuoteSessions(metaQuote, now)
	// The exchange calendar determines the active session, avoiding stale
	// marketState values returned by Yahoo after regular or early closes.
	price := float64(0)
	previous := float64(0)
	regularOpen := (*float64)(nil)
	currency := "USD"
	if metaQuote != nil {
		price = metaQuote.RegularMarketPrice
		previous = metaQuote.RegularMarketPreviousClose
		if state == "PRE" && validYahooPrice(metaQuote.RegularMarketPrice) {
			previous = metaQuote.RegularMarketPrice
		}
		regularOpen = optionalFloat(metaQuote.RegularMarketOpen)
		currency = metaQuote.Currency
		switch state {
		case "PRE":
			if yahooHasCurrentPreMarketPrice(metaQuote, now) {
				price = metaQuote.PreMarketPrice
			}
		case "POST":
			if validYahooPrice(metaQuote.PostMarketPrice) && yahooCurrentSessionTime(metaQuote.PostMarketTime, now) {
				price = metaQuote.PostMarketPrice
			}
		}
	}
	dailyOpen := float64(0)
	if len(snapshot.daily) > 0 {
		last := snapshot.daily[len(snapshot.daily)-1]
		if !validYahooPrice(price) {
			price = last.Close
		}
		if state == "PRE" && (metaQuote == nil || !validYahooPrice(metaQuote.RegularMarketPrice)) {
			previous = last.Close
		} else if len(snapshot.daily) > 1 && !validYahooPrice(previous) {
			previous = snapshot.daily[len(snapshot.daily)-2].Close
		}
		dailyOpen = last.Open
	}
	preMarketPrice := float64(0)
	if state == "PRE" && !yahooHasCurrentPreMarketPrice(metaQuote, now) {
		referenceClose := float64(0)
		if metaQuote != nil {
			referenceClose = metaQuote.RegularMarketPrice
		}
		if !validYahooPrice(referenceClose) && len(snapshot.daily) > 0 {
			referenceClose = snapshot.daily[len(snapshot.daily)-1].Close
		}
		if fallbackPrice, session, ok := yahooPreMarketSession(snapshot.preMarket, now, referenceClose); ok {
			preMarketPrice = fallbackPrice
			price = fallbackPrice
			if validYahooPrice(referenceClose) {
				previous = referenceClose
			}
			sessions["pre"] = session
		}
	}
	if snapshot.quoteErr != nil && len(snapshot.daily) == 0 && !validYahooPrice(preMarketPrice) {
		return model.Quote{}, snapshot.quoteErr
	}
	if snapshot.dailyErr != nil && (!validYahooPrice(price) || !validYahooPrice(previous)) && !validYahooPrice(preMarketPrice) {
		if snapshot.quoteErr != nil {
			return model.Quote{}, snapshot.quoteErr
		}
		return model.Quote{}, snapshot.dailyErr
	}
	if regularOpen == nil {
		regularOpen = optionalFloat(dailyOpen)
	}
	var change *float64
	if validYahooPrice(price) && validYahooPrice(previous) {
		value := (price - previous) / previous * 100
		change = &value
	}
	fetched := now.UTC()
	return model.Quote{
		Symbol: symbol, Price: optionalFloat(price), ChangePercent: change, TodayOpen: regularOpen, PreviousClose: optionalFloat(previous),
		DailyBar: yahooRegularDailyBar(metaQuote, now, state),
		Currency: stringOr(currency, "USD"), MarketState: strings.ToUpper(state), Provider: "yahoo-chart", Source: "upstream", FetchedAt: &fetched,
		Sessions: sessions, FairValuePending: true,
	}, nil
}

func yahooRegularDailyBar(quote *yahooQuoteModel, now time.Time, state string) *model.DailyBar {
	if quote == nil {
		return nil
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	dateTime := quote.RegularMarketTime
	if dateTime.IsZero() || dateTime.After(now) {
		return nil
	}
	forming := state == "REGULAR" &&
		dateTime.In(location).Format(time.DateOnly) == now.In(location).Format(time.DateOnly)
	bar := &model.DailyBar{
		Date: dateTime.In(location).Format(time.DateOnly), Open: quote.RegularMarketOpen,
		High: quote.RegularMarketDayHigh, Low: quote.RegularMarketDayLow,
		Close: quote.RegularMarketPrice, Volume: float64(quote.RegularMarketVolume), IsForming: forming,
	}
	if !validYahooPrice(bar.Open) || !validYahooPrice(bar.High) || !validYahooPrice(bar.Low) || !validYahooPrice(bar.Close) ||
		bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.Low > bar.High || bar.Volume < 0 {
		return nil
	}
	return bar
}

type yahooOptionChainResult struct {
	ExpirationDates []int64                 `json:"expirationDates"`
	Options         []yahooOptionExpiration `json:"options"`
}

type yahooOptionExpiration struct {
	ExpirationDate int64                          `json:"expirationDate"`
	Calls          []yahooOptionContractExtension `json:"calls"`
	Puts           []yahooOptionContractExtension `json:"puts"`
}

type yahooOptionExtensions struct {
	OptionChain struct {
		Result []yahooOptionChainResult `json:"result"`
		Error  *yahooAPIError           `json:"error"`
	} `json:"optionChain"`
}

type yahooOptionContractExtension struct {
	ContractSymbol    string               `json:"contractSymbol"`
	Strike            yahooJSONFloat       `json:"strike"`
	LastPrice         yahooJSONFloat       `json:"lastPrice"`
	Bid               yahooJSONFloat       `json:"bid"`
	Ask               yahooJSONFloat       `json:"ask"`
	PercentChange     yahooJSONFloat       `json:"percentChange"`
	Volume            yahooJSONInt         `json:"volume"`
	OpenInterest      yahooJSONInt         `json:"openInterest"`
	ImpliedVolatility yahooJSONFloat       `json:"impliedVolatility"`
	InTheMoney        yahooJSONBool        `json:"inTheMoney"`
	Gamma             yahooJSONStrictFloat `json:"gamma"`
}

type yahooJSONFloat struct {
	value   float64
	present bool
	valid   bool
}

func (value *yahooJSONFloat) UnmarshalJSON(data []byte) error {
	*value = yahooJSONFloat{present: true}
	if isJSONNull(data) {
		return nil
	}
	if err := json.Unmarshal(data, &value.value); err == nil {
		value.valid = true
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err == nil {
			value.value = parsed
			value.valid = true
		}
	}
	return nil
}

func yahooFloatPointer(value yahooJSONFloat) *float64 {
	if !value.present || !value.valid || math.IsNaN(value.value) || math.IsInf(value.value, 0) {
		return nil
	}
	parsed := value.value
	return &parsed
}

type yahooJSONStrictFloat struct {
	value   float64
	present bool
	null    bool
}

func (value *yahooJSONStrictFloat) UnmarshalJSON(data []byte) error {
	*value = yahooJSONStrictFloat{present: true}
	if isJSONNull(data) {
		value.null = true
		return nil
	}
	return json.Unmarshal(data, &value.value)
}

func yahooStrictFloatPointer(value yahooJSONStrictFloat) *float64 {
	if !value.present || value.null || math.IsNaN(value.value) || math.IsInf(value.value, 0) {
		return nil
	}
	parsed := value.value
	return &parsed
}

type yahooJSONInt struct {
	value   int64
	present bool
	valid   bool
}

func (value *yahooJSONInt) UnmarshalJSON(data []byte) error {
	*value = yahooJSONInt{present: true}
	if isJSONNull(data) {
		return nil
	}
	if err := json.Unmarshal(data, &value.value); err == nil {
		value.valid = true
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err == nil {
			value.value = parsed
			value.valid = true
		}
	}
	return nil
}

func yahooIntPointer(value yahooJSONInt) *int64 {
	if !value.present || !value.valid {
		return nil
	}
	parsed := value.value
	return &parsed
}

type yahooJSONBool struct {
	value   bool
	present bool
	null    bool
	valid   bool
}

func (value *yahooJSONBool) UnmarshalJSON(data []byte) error {
	*value = yahooJSONBool{present: true}
	if isJSONNull(data) {
		value.null = true
		return nil
	}
	if err := json.Unmarshal(data, &value.value); err == nil {
		value.valid = true
	}
	return nil
}

func (value yahooJSONBool) known() bool {
	return value.present && !value.null
}

func (value yahooJSONBool) boolOr(fallback bool) bool {
	if !value.present || value.null || !value.valid {
		return fallback
	}
	return value.value
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (y *Yahoo) Expirations(ctx context.Context, symbol string) ([]string, error) {
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, err
	}
	expirations, err := runYahooSDK(ctx, y.interactiveUpstreamGate(), y.gateWait, y.tickerFactory, normalized, func(ticker yahooTicker) ([]time.Time, error) {
		return ticker.Options()
	})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(expirations))
	for _, expiration := range expirations {
		value := expiration.UTC().Format("2006-01-02")
		result = append(result, value)
	}
	return result, nil
}

func (y *Yahoo) Chain(ctx context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	normalizedSymbol, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := NormalizeExpiration(expiration)
	if err != nil {
		return nil, nil, err
	}
	when, _ := time.Parse("2006-01-02", normalized)
	// One dated Yahoo options response contains the SDK chain fields plus Gamma
	// and nullable-field presence. Fetching the SDK chain and then requesting
	// this same endpoint again serialized two network round trips per expiry.
	rawExtensions, err := y.options(ctx, normalizedSymbol, fmt.Sprint(when.Unix()))
	if err != nil {
		return nil, nil, err
	}
	if rawExtensions.OptionChain.Error != nil {
		return nil, nil, rawExtensions.OptionChain.Error
	}
	if len(rawExtensions.OptionChain.Result) == 0 || len(rawExtensions.OptionChain.Result[0].Options) == 0 {
		return nil, nil, fmt.Errorf("%s %s 没有期权数据", normalizedSymbol, normalized)
	}
	selected := rawExtensions.OptionChain.Result[0].Options[0]
	if selected.ExpirationDate != 0 {
		returnedExpiration := time.Unix(selected.ExpirationDate, 0).UTC().Format("2006-01-02")
		if returnedExpiration != normalized {
			return nil, nil, fmt.Errorf("Yahoo returned expiration %s for requested expiration %s", returnedExpiration, normalized)
		}
	}
	if len(selected.Calls) == 0 && len(selected.Puts) == 0 {
		return nil, nil, fmt.Errorf("%s %s 没有期权数据", normalizedSymbol, normalized)
	}
	rows := yahooOptionRows(selected, normalized)
	fetched := time.Now().UTC()
	return rows, &fetched, nil
}

// NearestChain returns the expiration catalogue and the nearest active chain
// from a single no-date options response. Cold callers otherwise pay two
// round trips (catalogue then chain) for the same endpoint.
func (y *Yahoo) NearestChain(ctx context.Context, symbol string) ([]string, []model.OptionRow, *time.Time, error) {
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, nil, nil, err
	}
	rawExtensions, err := y.options(ctx, normalized, "")
	if err != nil {
		return nil, nil, nil, err
	}
	if rawExtensions.OptionChain.Error != nil {
		return nil, nil, nil, rawExtensions.OptionChain.Error
	}
	if len(rawExtensions.OptionChain.Result) == 0 {
		return nil, nil, nil, fmt.Errorf("%s 没有期权数据", normalized)
	}
	result := rawExtensions.OptionChain.Result[0]
	dates := result.ExpirationDates
	if len(dates) == 0 {
		for _, option := range result.Options {
			dates = append(dates, option.ExpirationDate)
		}
	}
	expirations := yahooActiveExpirationDates(dates)
	if len(expirations) == 0 {
		return nil, nil, nil, fmt.Errorf("%s 没有有效期权到期日", normalized)
	}
	selected, found := yahooNearestOption(result.Options, expirations[0])
	if !found || len(selected.Calls) == 0 && len(selected.Puts) == 0 {
		return expirations, nil, nil, nil
	}
	rows := yahooOptionRows(selected, expirations[0])
	fetched := time.Now().UTC()
	return expirations, rows, &fetched, nil
}

func yahooActiveExpirationDates(values []int64) []string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format(time.DateOnly)
	seen := make(map[string]bool, len(values))
	expirations := make([]string, 0, len(values))
	for _, value := range values {
		date := yahooExpirationDate(value)
		if date == "" || date < today || seen[date] {
			continue
		}
		seen[date] = true
		expirations = append(expirations, date)
	}
	sort.Strings(expirations)
	return expirations
}

func yahooNearestOption(options []yahooOptionExpiration, expiration string) (yahooOptionExpiration, bool) {
	for _, option := range options {
		if yahooExpirationDate(option.ExpirationDate) == expiration {
			return option, true
		}
	}
	return yahooOptionExpiration{}, false
}

func yahooExpirationDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.DateOnly)
}

func yahooOptionRows(selected yahooOptionExpiration, expiration string) []model.OptionRow {
	rows := make([]model.OptionRow, 0, len(selected.Calls)+len(selected.Puts))
	appendRows := func(contracts []yahooOptionContractExtension, kind string) {
		for _, extension := range contracts {
			rows = append(rows, model.OptionRow{
				ContractSymbol: extension.ContractSymbol, Provider: "yahoo-options", ContractType: kind,
				Strike: yahooFloatPointer(extension.Strike), LastPrice: yahooFloatPointer(extension.LastPrice),
				Bid: yahooFloatPointer(extension.Bid), Ask: yahooFloatPointer(extension.Ask),
				Volume: yahooIntPointer(extension.Volume), OpenInterest: yahooIntPointer(extension.OpenInterest),
				ImpliedVolatility: yahooFloatPointer(extension.ImpliedVolatility),
				Gamma:             yahooStrictFloatPointer(extension.Gamma),
				InTheMoney:        extension.InTheMoney.boolOr(false),
				InTheMoneyKnown:   extension.InTheMoney.known(),
				ChangePercent:     yahooFloatPointer(extension.PercentChange), Expiration: expiration,
			})
		}
	}
	appendRows(selected.Calls, "call")
	appendRows(selected.Puts, "put")
	return rows
}

func yahooSDKExpirationLookupDate(expiration time.Time, location *time.Location) string {
	if location == nil {
		location = time.UTC
	}
	return expiration.In(location).Format("2006-01-02")
}

func (y *Yahoo) options(ctx context.Context, symbol, timestamp string) (yahooOptionExtensions, error) {
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return yahooOptionExtensions{}, err
	}
	query := url.Values{}
	if timestamp != "" {
		query.Set("date", timestamp)
	}
	var payload yahooOptionExtensions
	endpoint := fmt.Sprintf(y.optionsURL, url.PathEscape(normalized))
	if err := y.getOptionsWithCrumb(ctx, endpoint, query, &payload); err != nil {
		return yahooOptionExtensions{}, err
	}
	return payload, nil
}

func (y *Yahoo) getOptionsWithCrumb(ctx context.Context, endpoint string, query url.Values, target any) error {
	if y.optionsAuth == nil {
		return errors.New("go-yfinance Yahoo authentication client is unavailable")
	}
	release, err := acquireGateTimeout(ctx, y.semaphore, y.gateWait)
	if err != nil {
		return err
	}
	type result struct {
		response *yahooauth.Response
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		defer release()
		response, requestErr := y.optionsAuth.GetWithCrumb(endpoint, query)
		resultCh <- result{response: response, err: requestErr}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case fetched := <-resultCh:
		if fetched.err != nil {
			return fetched.err
		}
		if fetched.response == nil {
			return errors.New("go-yfinance returned an empty Yahoo options response")
		}
		if fetched.response.StatusCode < http.StatusOK || fetched.response.StatusCode >= http.StatusMultipleChoices {
			return &yahooHTTPStatusError{status: fetched.response.StatusCode}
		}
		if err := json.Unmarshal([]byte(fetched.response.Body), target); err != nil {
			return fmt.Errorf("decode Yahoo options response: %w", err)
		}
		return nil
	}
}

func floatPtr(value float64) *float64 { return &value }

func optionalFloat(value float64) *float64 {
	if value == 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func stringOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
