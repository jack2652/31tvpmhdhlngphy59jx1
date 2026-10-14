package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

type IntradayHistoryClient interface {
	FetchIntraday(context.Context, string, string) (HistoryResult, error)
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
		// Give each alternate price source its own reserved upstream slot so a
		// saturated shared gate cannot starve the overnight/delayed quote, while
		// still letting the sources be fetched in parallel.
		if regular.interactiveGate != nil {
			if delayed != nil && delayed.InteractiveGate == nil {
				delayed.InteractiveGate = make(chan struct{}, 1)
			}
			if overnight != nil && overnight.InteractiveGate == nil {
				overnight.InteractiveGate = make(chan struct{}, 1)
			}
		}
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
	// During the overnight session Alpaca's overnight snapshot is the current
	// price, so the Yahoo regular quote is not fetched. When Alpaca is not
	// configured (or is in an authentication cooldown) Yahoo takes over.
	if isNYSEOvernightSession(h.marketNow()) && h.Overnight != nil && h.Overnight.CanRequest() {
		overnight, overnightErr := h.Overnight.Quote(ctx, symbol)
		if err := ctx.Err(); err != nil {
			return model.Quote{}, err
		}
		if overnightErr == nil && overnight.Price != nil {
			return overnightOnlyQuote(overnight), nil
		}
		base, baseErr := h.Regular.Quote(ctx, symbol)
		if baseErr != nil {
			if overnightErr != nil {
				return model.Quote{}, overnightErr
			}
			return model.Quote{}, baseErr
		}
		return base, nil
	}
	return h.Regular.Quote(ctx, symbol)
}

// overnightOnlyQuote presents the Alpaca overnight snapshot as the current
// quote. The Yahoo regular quote is not fetched during the overnight session, so
// there is no separate base to merge; the change percent is derived against the
// previous close and the Alpaca provider label is kept.
func overnightOnlyQuote(overnight model.Quote) model.Quote {
	if overnight.Sessions == nil {
		overnight.Sessions = map[string]any{}
	}
	previous := overnight.PreviousClose
	if previous == nil || *previous <= 0 {
		previous = sessionReferenceClose(overnight.Sessions, "overnight")
	}
	if price := overnight.Price; price != nil && previous != nil && *previous > 0 {
		change := (*price - *previous) / *previous * 100
		if !math.IsNaN(change) && !math.IsInf(change, 0) {
			overnight.ChangePercent = &change
		}
	}
	overnight.MarketState = "OVERNIGHT"
	return overnight
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

func (h *Hybrid) IntradayHistory(ctx context.Context, symbol, interval string) (HistoryResult, error) {
	if h == nil || h.HistoryClient == nil {
		return HistoryResult{}, errors.New("Yahoo intraday history provider is unavailable")
	}
	client, ok := h.HistoryClient.(IntradayHistoryClient)
	if !ok {
		return HistoryResult{}, errors.New("Yahoo history client does not support intraday history")
	}
	return client.FetchIntraday(ctx, symbol, interval)
}

func (h *Hybrid) EarningsDates(ctx context.Context, symbol string) ([]string, error) {
	if h == nil || h.Regular == nil || h.Regular.earnings == nil {
		return nil, errors.New("Yahoo earnings provider is unavailable")
	}
	return h.Regular.earnings.Fetch(ctx, symbol)
}
