package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"
)

// Hybrid routes option chains to Cboe outside regular NYSE hours and optionally
// overlays Alpaca Basic overnight stock snapshots on Yahoo quotes.
// It does not request or consume Alpaca options feeds.
type HistoryClient interface {
	Fetch(context.Context, string, string) (HistoryResult, error)
	FetchAdjusted(context.Context, string, string) (HistoryResult, error)
}

type Hybrid struct {
	Regular       *Yahoo
	Delayed       *CboeOptionsClient
	Overnight     *Alpaca
	HistoryClient HistoryClient
	now           func() time.Time
}

func NewHybrid(regular *Yahoo, delayed *CboeOptionsClient, overnight *Alpaca) *Hybrid {
	var history HistoryClient
	if regular != nil {
		history = NewYFinanceHistoryClient(regular.semaphore, regular.gateWait, regular.tickerFactory)
	}
	return &Hybrid{Regular: regular, Delayed: delayed, Overnight: overnight, HistoryClient: history, now: time.Now}
}

func (h *Hybrid) Close() error {
	if h != nil && h.Regular != nil {
		if h.Delayed != nil {
			outboundhttp.CloseIdleConnections(h.Delayed.Client)
		}
		if h.Overnight != nil {
			outboundhttp.CloseIdleConnections(h.Overnight.client)
		}
		if h.Delayed == nil {
			outboundhttp.CloseIdleConnections(h.Regular.client)
		}
		return h.Regular.Close()
	}
	return nil
}

func (h *Hybrid) ValidateAlpacaCredentials(ctx context.Context) string {
	if h == nil || h.Overnight == nil {
		return "disabled"
	}
	return h.Overnight.ValidateCredentials(ctx)
}

// NewDefaultHybrid composes deployable clients from runtime configuration.
func NewDefaultHybrid(timeout time.Duration, concurrency int, proxy, alpacaKey, alpacaSecret string, waitValues ...time.Duration) (*Hybrid, error) {
	return newDefaultHybrid(timeout, 15*time.Second, concurrency, proxy, alpacaKey, alpacaSecret, waitValues...)
}

func newDefaultHybrid(timeout, cboeTimeout time.Duration, concurrency int, proxyAddress, alpacaKey, alpacaSecret string, waitValues ...time.Duration) (*Hybrid, error) {
	if err := outboundhttp.ValidateProxy(proxyAddress); err != nil {
		return nil, err
	}
	regular, err := NewYahooWithProxy(timeout, concurrency, proxyAddress)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if cboeTimeout <= 0 {
		cboeTimeout = 15 * time.Second
	}
	httpClient := regular.client
	gate := regular.semaphore
	gateWait := timeout
	if len(waitValues) > 0 {
		gateWait = waitValues[0]
	}
	if gateWait <= 0 {
		gateWait = timeout
	}
	regular.gateWait = gateWait
	regular.fundamentals.GateWait = gateWait
	regular.earnings.GateWait = gateWait
	delayedClient := outboundhttp.UsingTransport(httpClient.Transport, cboeTimeout)
	delayed := NewCboeOptionsClient(delayedClient, "")
	delayed.Gate, delayed.GateWait = gate, gateWait
	overnightProxy, err := outboundhttp.ProxyAddress(proxyAddress)
	if err != nil {
		_ = regular.Close()
		return nil, err
	}
	overnightClient, err := outboundhttp.APIClient(timeout, overnightProxy)
	if err != nil {
		_ = regular.Close()
		return nil, err
	}
	overnightClient.Timeout = timeout
	overnight := NewAlpaca(alpacaKey, alpacaSecret, "", overnightClient)
	overnight.Gate, overnight.GateWait = gate, gateWait
	hybrid := NewHybrid(regular, delayed, overnight)
	return hybrid, nil
}

func (h *Hybrid) usesDelayedOptions() bool { return !isNYSERegularSession(h.marketNow()) }

func (h *Hybrid) marketNow() time.Time {
	now := time.Now()
	if h != nil && h.now != nil {
		now = h.now()
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return now
	}
	return now.In(location)
}

func (h *Hybrid) Quote(ctx context.Context, rawSymbol string) (model.Quote, error) {
	if h == nil || h.Regular == nil {
		return model.Quote{}, errors.New("hybrid quote provider is unavailable")
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return model.Quote{}, err
	}
	now := h.marketNow()
	overnightSession := isNYSEOvernightSession(now)
	canOverlay := overnightSession && h.Overnight != nil && h.Overnight.CanRequest()
	var base, overnight model.Quote
	var baseErr, overnightErr error
	if canOverlay && h.concurrentQuoteFetchAllowed() {
		var requests sync.WaitGroup
		requests.Add(2)
		go func() {
			defer requests.Done()
			base, baseErr = h.Regular.Quote(ctx, symbol)
		}()
		go func() {
			defer requests.Done()
			overnight, overnightErr = h.Overnight.Quote(ctx, symbol)
		}()
		requests.Wait()
	} else {
		base, baseErr = h.Regular.Quote(ctx, symbol)
		if canOverlay {
			overnight, overnightErr = h.Overnight.Quote(ctx, symbol)
		}
	}
	if err := ctx.Err(); err != nil {
		return model.Quote{}, err
	}
	if !canOverlay {
		return base, baseErr
	}
	if baseErr != nil {
		if overnightErr == nil {
			return overnight, nil
		}
		return model.Quote{}, baseErr
	}
	if overnightErr != nil {
		// An unavailable/invalid optional overnight feed must not break the base quote.
		return base, nil
	}
	return mergeOvernightQuote(base, overnight), nil
}

// SnapshotQuote matches the Python snapshot fetch path: outside regular
// NYSE hours, Cboe supplies the underlying quote if Yahoo/Alpaca fails or
// returns no usable price. The standalone quote endpoint continues to use
// Quote and does not request this fallback.
func (h *Hybrid) SnapshotQuote(ctx context.Context, rawSymbol string) (model.Quote, error) {
	if h == nil || h.Regular == nil {
		return model.Quote{}, errors.New("hybrid quote provider is unavailable")
	}
	if !h.usesDelayedOptions() {
		return h.Quote(ctx, rawSymbol)
	}

	quote, quoteErr := h.Quote(ctx, rawSymbol)
	if err := ctx.Err(); err != nil {
		return model.Quote{}, err
	}
	if quoteErr == nil && quote.Price != nil {
		return quote, nil
	}
	if h.Delayed == nil {
		return quote, quoteErr
	}
	delayed, err := h.Delayed.Quote(ctx, rawSymbol)
	if err != nil {
		return model.Quote{}, err
	}
	return withSnapshotFallbackSessions(delayed, quote, quoteErr == nil), nil
}

func withSnapshotFallbackSessions(fallback, original model.Quote, originalSucceeded bool) model.Quote {
	if originalSucceeded && original.Price == nil && len(original.Sessions) > 0 {
		fallback.Sessions = original.Sessions
	}
	return fallback
}

func (h *Hybrid) concurrentQuoteFetchAllowed() bool {
	if h == nil || h.Regular == nil {
		return false
	}
	return h.Regular.semaphore == nil || cap(h.Regular.semaphore) > 1
}

func mergeOvernightQuote(base, overnight model.Quote) model.Quote {
	if base.Sessions == nil {
		base.Sessions = map[string]any{}
	}
	for key, value := range overnight.Sessions {
		base.Sessions[key] = value
	}
	previous := sessionReferenceClose(base.Sessions, "post")
	if previous == nil || *previous <= 0 {
		previous = sessionReferenceClose(base.Sessions, "overnight")
	}
	if previous == nil || *previous <= 0 {
		previous = base.PreviousClose
	}
	if previous == nil || *previous <= 0 {
		previous = overnight.PreviousClose
	}
	price := overnight.Price
	if price == nil || *price <= 0 {
		price = base.Price
	}
	base.Price = price
	base.PreviousClose = previous
	base.TodayOpen = firstValidQuoteValue(overnight.TodayOpen, base.TodayOpen)
	base.MarketState = "OVERNIGHT"
	base.Provider = "hybrid"
	if price != nil && previous != nil && *previous > 0 {
		change := (*price - *previous) / *previous * 100
		if !math.IsNaN(change) && !math.IsInf(change, 0) {
			base.ChangePercent = &change
		}
	}
	return base
}

func firstValidQuoteValue(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil && *value > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0) {
			return value
		}
	}
	return nil
}

func (h *Hybrid) Expirations(ctx context.Context, symbol string) ([]string, error) {
	if h == nil {
		return nil, errors.New("hybrid provider is unavailable")
	}
	if h.usesDelayedOptions() {
		if h.Delayed == nil {
			return nil, errors.New("Cboe delayed options provider is unavailable")
		}
		return h.Delayed.Expirations(ctx, symbol)
	}
	if h.Regular == nil {
		return nil, errors.New("Yahoo options provider is unavailable")
	}
	return h.Regular.Expirations(ctx, symbol)
}

func (h *Hybrid) Chain(ctx context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	if h == nil {
		return nil, nil, errors.New("hybrid provider is unavailable")
	}
	if h.usesDelayedOptions() {
		if h.Delayed == nil {
			return nil, nil, errors.New("Cboe delayed options provider is unavailable")
		}
		return h.Delayed.Chain(ctx, symbol, expiration)
	}
	if h.Regular == nil {
		return nil, nil, errors.New("Yahoo options provider is unavailable")
	}
	return h.Regular.Chain(ctx, symbol, expiration)
}

func isNYSERegularSession(moment time.Time) bool {
	return marketcalendar.IsRegularSession(moment)
}

func isNYSEOvernightSession(moment time.Time) bool {
	return marketcalendar.IsOvernightSession(moment)
}

func isNYSETradingDay(day time.Time) bool     { return marketcalendar.IsTradingDay(day) }
func isNYSEEarlyCloseDate(day time.Time) bool { return marketcalendar.IsEarlyClose(day) }

func sessionReferenceClose(sessions map[string]any, name string) *float64 {
	if sessions == nil {
		return nil
	}
	value, ok := sessions[name]
	if !ok {
		return nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return flexiblePositiveFloat(entry["reference_close"])
}

func flexiblePositiveFloat(value any) *float64 {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if number <= 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

func (h *Hybrid) History(ctx context.Context, symbol, period string) (HistoryResult, error) {
	if h == nil || h.HistoryClient == nil {
		return HistoryResult{}, errors.New("Yahoo history provider is unavailable")
	}
	return h.HistoryClient.Fetch(ctx, symbol, period)
}

// Fundamentals keeps the hybrid provider's optional valuation surface aligned
// with the regular Yahoo client.  Quote and option routing may switch by market
// session, but company fundamentals remain a Yahoo quoteSummary/timeseries
// concern and must still be visible to the service's valuation enrichment.
func (h *Hybrid) Fundamentals(ctx context.Context, symbol string) (YahooFundamentals, error) {
	if h == nil || h.Regular == nil {
		return YahooFundamentals{}, errors.New("Yahoo fundamentals provider is unavailable")
	}
	return h.Regular.Fundamentals(ctx, symbol)
}

func (h *Hybrid) BenchmarkHistory(ctx context.Context, symbol, period string) (HistoryResult, error) {
	if h == nil || h.HistoryClient == nil {
		return HistoryResult{}, errors.New("Yahoo benchmark history provider is unavailable")
	}
	return h.HistoryClient.FetchAdjusted(ctx, symbol, period)
}

func (h *Hybrid) EarningsDates(ctx context.Context, symbol string) ([]string, error) {
	if h == nil || h.Regular == nil || h.Regular.earnings == nil {
		return nil, errors.New("Yahoo earnings provider is unavailable")
	}
	return h.Regular.earnings.Fetch(ctx, symbol)
}
