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

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"
)

const (
	optionsURL = "https://query1.finance.yahoo.com/v7/finance/options/%s"
)

var symbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,9}$`)

type Yahoo struct {
	client        *http.Client
	authClient    *yahooauth.Client
	optionsAuth   yahooCrumbGetter
	optionsURL    string
	fundamentals  *FundamentalsClient
	earnings      *YahooEarningsClient
	tickerFactory yahooTickerFactory
	timeout       time.Duration
	gateWait      time.Duration
	semaphore     chan struct{}
	now           func() time.Time
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
	fundamentals := NewFundamentalsClient(client)
	fundamentals.Gate, fundamentals.GateWait = semaphore, timeout
	earnings := NewYahooEarningsClient(client)
	earnings.Gate, earnings.GateWait = semaphore, timeout
	return &Yahoo{client: client, optionsURL: optionsURL, fundamentals: fundamentals, earnings: earnings, timeout: timeout, gateWait: timeout, semaphore: semaphore, now: time.Now}
}

func NewYahooWithProxy(timeout time.Duration, concurrency int, proxyAddress string) (*Yahoo, error) {
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
	fundamentals := NewFundamentalsClient(client)
	fundamentals.Gate = semaphore
	fundamentals.GateWait = timeout
	optionsAuth := yahooauth.NewAuthManager(authClient)
	fundamentals.crumbAuth = optionsAuth
	earnings := NewYahooEarningsClient(client)
	earnings.Gate = semaphore
	earnings.GateWait = timeout
	tickerFactory := newYahooTickerFactory(authClient)
	fundamentals.tickerFactory = tickerFactory
	earnings.tickerFactory = tickerFactory
	return &Yahoo{
		client:        client,
		authClient:    authClient,
		optionsAuth:   optionsAuth,
		optionsURL:    optionsURL,
		fundamentals:  fundamentals,
		earnings:      earnings,
		tickerFactory: tickerFactory,
		timeout:       timeout,
		gateWait:      timeout,
		semaphore:     semaphore,
		now:           time.Now,
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

// Fundamentals fetches optional Yahoo financial inputs used by valuation. It is
// independent of Quote so enrichment failures cannot affect market snapshots.
func (y *Yahoo) Fundamentals(ctx context.Context, symbol string) (YahooFundamentals, error) {
	if y == nil || y.fundamentals == nil {
		return YahooFundamentals{}, errors.New("Yahoo fundamentals client is unavailable")
	}
	return y.fundamentals.Fetch(ctx, symbol)
}

// Earnings fetches the dates Yahoo currently exposes for the symbol.
func (y *Yahoo) Earnings(ctx context.Context, symbol string) ([]string, error) {
	if y == nil || y.earnings == nil {
		return nil, errors.New("Yahoo earnings client is unavailable")
	}
	return y.earnings.Fetch(ctx, symbol)
}

func NormalizeSymbol(raw string) (string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	if !symbolPattern.MatchString(symbol) {
		return "", fmt.Errorf("invalid symbol: %s", raw)
	}
	return symbol, nil
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

type yahooAPIError struct {
	Code        string `json:"code"`
	Description string `json:"description"`
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

type yahooIntradayBar struct {
	at    time.Time
	open  *float64
	close float64
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

func summarizeYahooChart(result yahooChartResult, now time.Time) (map[string]any, string, *float64, *float64) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	now = now.In(location)
	bars := make([]yahooIntradayBar, 0, len(result.Timestamp))
	var opens, closes []*float64
	if len(result.Indicators.Quote) > 0 {
		opens, closes = result.Indicators.Quote[0].Open, result.Indicators.Quote[0].Close
	}
	for index, rawTimestamp := range result.Timestamp {
		if rawTimestamp == nil || index >= len(closes) || closes[index] == nil {
			continue
		}
		close := *closes[index]
		if close <= 0 || math.IsNaN(close) || math.IsInf(close, 0) {
			continue
		}
		bar := yahooIntradayBar{at: time.Unix(*rawTimestamp, 0).In(location), close: close}
		if bar.at.After(now) {
			continue
		}
		if index < len(opens) && opens[index] != nil && *opens[index] > 0 && !math.IsNaN(*opens[index]) && !math.IsInf(*opens[index], 0) {
			value := *opens[index]
			bar.open = &value
		}
		bars = append(bars, bar)
	}
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].at.Before(bars[j].at) })
	sessions := make(map[string]any)
	regularCloses := make(map[string]float64)
	regularOpens := make(map[string]float64)
	var latest *time.Time
	for _, bar := range bars {
		at := bar.at
		latestAt := at
		latest = &latestAt
		kind, day := yahooSessionOf(at), at.Format("2006-01-02")
		if kind == "regular" {
			regularCloses[day] = bar.close
			if bar.open != nil {
				if _, seen := regularOpens[day]; !seen {
					regularOpens[day] = *bar.open
				}
			}
			continue
		}
		var reference *float64
		for _, regularDay := range sortedKeys(regularCloses) {
			if regularDay <= day {
				value := regularCloses[regularDay]
				reference = &value
			}
		}
		summary := map[string]any{"price": bar.close, "change_percent": nil, "as_of": at.Format(time.RFC3339)}
		if reference != nil && *reference > 0 {
			summary["reference_close"] = *reference
			summary["change_percent"] = (bar.close - *reference) / *reference * 100
		}
		sessions[kind] = summary
	}
	state := yahooSessionState(now, latest)
	day := yahooTradingDay(now).Format("2006-01-02")
	regularDays := sortedKeys(regularCloses)
	if len(regularDays) > 0 && (state == "OVERNIGHT" || state == "POST" || (state == "REGULAR" && regularDays[len(regularDays)-1] == day)) {
		regularDays = regularDays[:len(regularDays)-1]
	}
	var previousClose *float64
	if len(regularDays) > 0 {
		value := regularCloses[regularDays[len(regularDays)-1]]
		previousClose = &value
	}
	var regularOpen *float64
	openDays := sortedKeys(regularOpens)
	for _, openDay := range openDays {
		if openDay <= now.Format("2006-01-02") {
			value := regularOpens[openDay]
			regularOpen = &value
		}
	}
	if state == "PRE" && previousClose != nil {
		for _, session := range []string{"pre", "post"} {
			if summary, ok := sessions[session].(map[string]any); ok {
				summary["reference_close"] = *previousClose
				if value, ok := summary["price"].(float64); ok {
					summary["change_percent"] = (value - *previousClose) / *previousClose * 100
				}
			}
		}
	}
	return sessions, state, regularOpen, previousClose
}

func sortedKeys(values map[string]float64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validYahooPrice(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (y *Yahoo) Quote(ctx context.Context, symbol string) (model.Quote, error) {
	symbol, err := NormalizeSymbol(symbol)
	if err != nil {
		return model.Quote{}, err
	}
	type quoteSnapshot struct {
		quote       *yahooQuoteModel
		quoteErr    error
		intraday    []yahooBarModel
		intradayErr error
		daily       []yahooBarModel
		dailyErr    error
	}
	type quotePhaseTimes struct {
		quote, intraday, daily time.Duration
	}
	var phases quotePhaseTimes
	startedAt := time.Now()
	snapshot, fetchErr := runYahooSDK(ctx, y.semaphore, y.gateWait, y.tickerFactory, symbol, func(ticker yahooTicker) (quoteSnapshot, error) {
		type quoteResult struct {
			quote *yahooQuoteModel
			err   error
		}
		type historyResult struct {
			bars []yahooBarModel
			err  error
		}
		quoteResults := make(chan quoteResult, 1)
		quoteForHistory := make(chan quoteResult, 1)
		historyResults := make(chan historyResult, 1)
		dailyResults := make(chan historyResult, 1)
		var requests sync.WaitGroup
		// Closed-market quote snapshots have no current pre/post/overnight bars
		// to add. Yahoo's quote endpoint can report a stale REGULAR marketState
		// after the close, so the exchange calendar is authoritative here.
		marketClosed := yahooSessionState(y.marketNow(), nil) == "CLOSED"
		requests.Add(2)
		go func() {
			defer requests.Done()
			started := time.Now()
			quote, quoteErr := ticker.Quote()
			phases.quote = time.Since(started)
			result := quoteResult{quote: quote, err: quoteErr}
			quoteResults <- result
			quoteForHistory <- result
		}()
		go func() {
			defer requests.Done()
			var quote quoteResult
			needsIntraday := !marketClosed
			if needsIntraday {
				started := time.Now()
				bars, historyErr := ticker.History(yahooHistoryParams("1m", true))
				phases.intraday = time.Since(started)
				historyResults <- historyResult{bars: bars, err: historyErr}
			} else {
				historyResults <- historyResult{}
			}
			if quote.quote == nil && quote.err == nil {
				// The quote goroutine may still be in flight. Daily history is only
				// a fallback for missing regular fields, so decide after receiving it.
				quote = <-quoteForHistory
			}
			needsDaily := quote.err != nil || quote.quote == nil || !validYahooPrice(quote.quote.RegularMarketPrice) || !validYahooPrice(quote.quote.RegularMarketPreviousClose)
			if needsDaily {
				started := time.Now()
				bars, historyErr := ticker.History(yahooHistoryParams("1d", false))
				phases.daily = time.Since(started)
				dailyResults <- historyResult{bars: bars, err: historyErr}
			}
		}()
		requests.Wait()
		quoteCall := <-quoteResults
		intradayCall := <-historyResults
		quote, quoteErr := quoteCall.quote, quoteCall.err
		intraday, historyErr := intradayCall.bars, intradayCall.err
		var daily []yahooBarModel
		var dailyErr error
		if quoteErr != nil || quote == nil || !validYahooPrice(quote.RegularMarketPrice) || !validYahooPrice(quote.RegularMarketPreviousClose) {
			dailyCall := <-dailyResults
			daily, dailyErr = dailyCall.bars, dailyCall.err
		}
		if quoteErr != nil && len(intraday) == 0 && len(daily) == 0 {
			return quoteSnapshot{}, quoteErr
		}
		if historyErr != nil && quote == nil && len(daily) == 0 {
			return quoteSnapshot{}, historyErr
		}
		return quoteSnapshot{quote: quote, quoteErr: quoteErr, intraday: intraday, intradayErr: historyErr, daily: daily, dailyErr: dailyErr}, nil
	})
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		log.Printf("slow Yahoo quote symbol=%s total=%s quote=%s intraday=%s daily=%s", symbol, elapsed.Round(time.Millisecond), phases.quote.Round(time.Millisecond), phases.intraday.Round(time.Millisecond), phases.daily.Round(time.Millisecond))
	}
	if fetchErr != nil {
		return model.Quote{}, fetchErr
	}
	now := y.marketNow()
	metaQuote := snapshot.quote
	chart := yahooChartFromSDK(metaQuote, snapshot.intraday)
	sessions, state, regularOpen, mappedPrevious := summarizeYahooChart(chart, now)
	meta := chart.Meta
	if metaQuote != nil {
		meta.MarketState = strings.ToUpper(metaQuote.MarketState)
	}
	if state == "" || (len(snapshot.intraday) == 0 && meta.MarketState != "") {
		state = strings.ToUpper(meta.MarketState)
	}
	price := meta.RegularMarketPrice
	if metaQuote != nil {
		switch strings.ToUpper(metaQuote.MarketState) {
		case "PRE":
			if validYahooPrice(metaQuote.PreMarketPrice) {
				price = metaQuote.PreMarketPrice
			}
		case "POST":
			if validYahooPrice(metaQuote.PostMarketPrice) {
				price = metaQuote.PostMarketPrice
			}
		}
	}
	if !validYahooPrice(price) && len(snapshot.intraday) > 0 {
		price = snapshot.intraday[len(snapshot.intraday)-1].Close
	}
	previous := meta.PreviousClose
	if mappedPrevious != nil && validYahooPrice(*mappedPrevious) {
		previous = *mappedPrevious
	}
	dailyOpen := float64(0)
	if len(snapshot.daily) > 0 {
		last := snapshot.daily[len(snapshot.daily)-1]
		if !validYahooPrice(price) {
			price = last.Close
		}
		if len(snapshot.daily) > 1 && !validYahooPrice(previous) {
			previous = snapshot.daily[len(snapshot.daily)-2].Close
		}
		dailyOpen = last.Open
	}
	if snapshot.dailyErr != nil && (!validYahooPrice(price) || !validYahooPrice(previous)) {
		if snapshot.intradayErr != nil {
			return model.Quote{}, snapshot.intradayErr
		}
		if snapshot.quoteErr != nil {
			return model.Quote{}, snapshot.quoteErr
		}
		return model.Quote{}, snapshot.dailyErr
	}
	if state == "PRE" && mappedPrevious != nil && validYahooPrice(*mappedPrevious) && validYahooPrice(price) && math.Abs(price-*mappedPrevious) / *mappedPrevious <= 0.001 {
		previous = price
	}
	if state == "" {
		state = yahooSessionState(now, nil)
	}
	if regularOpen == nil || (state != "REGULAR" && state != "POST" && state != "OVERNIGHT") {
		regularOpen = optionalFloat(meta.RegularMarketOpen)
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
	if sessions == nil {
		sessions = map[string]any{}
	}
	return model.Quote{
		Symbol: symbol, Price: optionalFloat(price), ChangePercent: change, TodayOpen: regularOpen, PreviousClose: optionalFloat(previous),
		Currency: stringOr(meta.Currency, "USD"), MarketState: strings.ToUpper(state), Provider: "yahoo-chart", Source: "upstream", FetchedAt: &fetched,
		Sessions: sessions, FairValuePending: true,
	}, nil
}

type yahooOptionExtensions struct {
	OptionChain struct {
		Result []struct {
			Options []struct {
				Calls []yahooOptionContractExtension `json:"calls"`
				Puts  []yahooOptionContractExtension `json:"puts"`
			} `json:"options"`
		} `json:"result"`
	} `json:"optionChain"`
}

// The SDK's option model omits Gamma and field-presence information.
type yahooOptionContractExtension struct {
	fields         map[string]json.RawMessage
	contractSymbol string
	gamma          float64
}

func (contract *yahooOptionContractExtension) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	contract.fields = make(map[string]json.RawMessage, 11)
	for _, name := range []string{
		"contractSymbol", "strike", "lastPrice", "bid", "ask", "percentChange",
		"volume", "openInterest", "impliedVolatility", "inTheMoney", "gamma",
	} {
		if raw, ok := fields[name]; ok {
			contract.fields[name] = raw
		}
	}
	if raw := contract.fields["contractSymbol"]; len(raw) > 0 && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &contract.contractSymbol); err != nil {
			return err
		}
	}
	if raw := contract.fields["gamma"]; len(raw) > 0 && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &contract.gamma); err != nil {
			return err
		}
	}
	return nil
}

func (contract yahooOptionContractExtension) float(name string, value float64) *float64 {
	raw, ok := contract.fields[name]
	if !ok {
		// The SDK model remains authoritative when the dated raw extension is
		// missing a contract or field. Its non-zero values are still useful; a
		// zero without raw presence cannot be distinguished from an omitted field.
		if value != 0 {
			return &value
		}
		return nil
	}
	if isJSONNull(raw) {
		return nil
	}
	return &value
}

func (contract yahooOptionContractExtension) integer(name string, value int64) *int64 {
	raw, ok := contract.fields[name]
	if !ok {
		if value != 0 {
			return &value
		}
		return nil
	}
	if isJSONNull(raw) {
		return nil
	}
	return &value
}

func (contract yahooOptionContractExtension) boolKnown(name string, sdkValue bool) bool {
	raw, ok := contract.fields[name]
	if !ok {
		return sdkValue
	}
	return !isJSONNull(raw)
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (y *Yahoo) Expirations(ctx context.Context, symbol string) ([]string, error) {
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, err
	}
	expirations, err := runYahooSDK(ctx, y.semaphore, y.gateWait, y.tickerFactory, normalized, func(ticker yahooTicker) ([]time.Time, error) {
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
	sdkExpiration := yahooSDKExpirationLookupDate(when, time.Local)
	chain, err := runYahooSDK(ctx, y.semaphore, y.gateWait, y.tickerFactory, normalizedSymbol, func(ticker yahooTicker) (*ymodels.OptionChain, error) {
		return ticker.OptionChain(sdkExpiration)
	})
	if err != nil {
		return nil, nil, err
	}
	if chain == nil {
		return nil, nil, fmt.Errorf("%s %s 没有期权数据", strings.ToUpper(strings.TrimSpace(symbol)), normalized)
	}
	if !chain.Expiration.IsZero() {
		returnedExpiration := chain.Expiration.UTC().Format("2006-01-02")
		if returnedExpiration != normalized {
			return nil, nil, fmt.Errorf("Yahoo returned expiration %s for requested expiration %s", returnedExpiration, normalized)
		}
	}
	if len(chain.Calls) == 0 && len(chain.Puts) == 0 {
		return nil, nil, fmt.Errorf("%s %s 没有期权数据", strings.ToUpper(strings.TrimSpace(symbol)), normalized)
	}

	// go-yfinance v1.7.0 models OptionChain but drops Gamma and null-vs-zero
	// presence bits; its OptionsJSON only fetches the nearest expiry. Keep a
	// dated raw response for these fields through the SDK auth manager.
	rawExtensions, err := y.options(ctx, normalizedSymbol, strconv.FormatInt(when.Unix(), 10))
	if err != nil {
		return nil, nil, err
	}
	var chainExtensions struct {
		Calls []yahooOptionContractExtension
		Puts  []yahooOptionContractExtension
	}
	if len(rawExtensions.OptionChain.Result) > 0 && len(rawExtensions.OptionChain.Result[0].Options) > 0 {
		chainExtensions.Calls = rawExtensions.OptionChain.Result[0].Options[0].Calls
		chainExtensions.Puts = rawExtensions.OptionChain.Result[0].Options[0].Puts
	}
	rows := make([]model.OptionRow, 0, len(chain.Calls)+len(chain.Puts))
	appendRows := func(contracts []ymodels.Option, contractExtensions []yahooOptionContractExtension, kind string) {
		byContract := make(map[string]yahooOptionContractExtension, len(contractExtensions))
		for _, extension := range contractExtensions {
			if extension.contractSymbol != "" {
				byContract[extension.contractSymbol] = extension
			}
		}
		for index, contract := range contracts {
			var extension yahooOptionContractExtension
			if matched, ok := byContract[contract.ContractSymbol]; contract.ContractSymbol != "" && ok {
				extension = matched
			} else if contract.ContractSymbol == "" && index < len(contractExtensions) && contractExtensions[index].contractSymbol == "" {
				extension = contractExtensions[index]
			}
			rows = append(rows, model.OptionRow{
				ContractSymbol: contract.ContractSymbol, Provider: "yahoo-options", ContractType: kind,
				Strike: extension.float("strike", contract.Strike), LastPrice: extension.float("lastPrice", contract.LastPrice),
				Bid: extension.float("bid", contract.Bid), Ask: extension.float("ask", contract.Ask),
				Volume: extension.integer("volume", contract.Volume), OpenInterest: extension.integer("openInterest", contract.OpenInterest),
				ImpliedVolatility: extension.float("impliedVolatility", contract.ImpliedVolatility),
				Gamma:             extension.float("gamma", extension.gamma),
				InTheMoney:        contract.InTheMoney,
				InTheMoneyKnown:   extension.boolKnown("inTheMoney", contract.InTheMoney),
				ChangePercent:     extension.float("percentChange", contract.PercentChange), Expiration: normalized,
			})
		}
	}
	appendRows(chain.Calls, chainExtensions.Calls, "call")
	appendRows(chain.Puts, chainExtensions.Puts, "put")
	fetched := time.Now().UTC()
	return rows, &fetched, nil
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
		decoder := json.NewDecoder(strings.NewReader(fetched.response.Body))
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("decode Yahoo options response: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return errors.New("decode Yahoo options response: unexpected trailing JSON")
			}
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
