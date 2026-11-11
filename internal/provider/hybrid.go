package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"
)

type HistoryClient interface {
	Fetch(context.Context, string, string) (HistoryResult, error)
	FetchAdjusted(context.Context, string, string) (HistoryResult, error)
}

type IntradayHistoryClient interface {
	FetchIntraday(context.Context, string, string) (HistoryResult, error)
}

type ChanAdjustedHistoryClient interface {
	FetchChanAdjusted(context.Context, string, string) (ChanAdjustedHistoryResult, error)
}

// Hybrid 负责期权链路选择，并合并 Yahoo 近期历史与 Alpaca 15 分钟补数。
type Hybrid struct {
	Regular       *Yahoo
	Delayed       *CboeOptionsClient
	Overnight     *Alpaca
	HistoryClient HistoryClient
	now           func() time.Time

	// previousPost caches the last completed after-hours summary per symbol.
	// During pre-market and overnight the current quote has no after-hours
	// price, so the previous trading day's post-market price has to come from
	// Yahoo's 1-minute prepost chart. It cannot change until the next
	// after-hours window closes, so one lookup is cached for the whole session.
	postMu       sync.Mutex
	previousPost map[string]previousPostEntry
}

type previousPostEntry struct {
	session map[string]any
	day     string
	expires time.Time
}

const (
	previousPostSessionRetry = 2 * time.Minute
	previousPostCacheLimit   = 256
)

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
	return newDefaultHybrid(timeout, 15*time.Second, concurrency, proxy, alpacaKey, alpacaSecret, false, waitValues...)
}

// NewDefaultHybridWithMemoryProfile 按显式资源模式构建默认行情 provider。
func NewDefaultHybridWithMemoryProfile(timeout time.Duration, concurrency int, proxy, alpacaKey, alpacaSecret string, lowMemory bool, waitValues ...time.Duration) (*Hybrid, error) {
	return newDefaultHybrid(timeout, 15*time.Second, concurrency, proxy, alpacaKey, alpacaSecret, lowMemory, waitValues...)
}

// NewDefaultHybridWithCboeTimeout 使用运行配置中的 Cboe 超时。
func NewDefaultHybridWithCboeTimeout(timeout, cboeTimeout time.Duration, concurrency int, proxy, alpacaKey, alpacaSecret string, lowMemory bool, waitValues ...time.Duration) (*Hybrid, error) {
	return newDefaultHybrid(timeout, cboeTimeout, concurrency, proxy, alpacaKey, alpacaSecret, lowMemory, waitValues...)
}

func newDefaultHybrid(timeout, cboeTimeout time.Duration, concurrency int, proxyAddress, alpacaKey, alpacaSecret string, lowMemory bool, waitValues ...time.Duration) (*Hybrid, error) {
	if err := outboundhttp.ValidateProxy(proxyAddress); err != nil {
		return nil, err
	}
	regular, err := newYahooWithProxy(timeout, concurrency, proxyAddress, lowMemory)
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
	// Yahoo 的夜盘字段优先作为股票夜盘价格；Yahoo 没有有效夜盘字段时，
	// 再使用 Alpaca 作为回退。指数和期货仍始终使用 Yahoo 自身路径。
	// 指数没有可交易的夜盘期权，始终使用 Yahoo 指数行情，避免向 Alpaca 请求无效标的。
	if !IsIndexSymbol(symbol) && !IsFutureSymbol(symbol) && isNYSEOvernightSession(h.marketNow()) {
		type quoteAttempt struct {
			quote model.Quote
			err   error
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		baseCh := make(chan quoteAttempt, 1)
		go func() {
			base, baseErr := h.Regular.Quote(attemptCtx, symbol)
			baseCh <- quoteAttempt{quote: base, err: baseErr}
		}()
		alpacaEnabled := h.Overnight != nil && h.Overnight.CanRequest()
		alpacaCh := make(chan quoteAttempt, 1)
		if alpacaEnabled {
			go func() {
				overnight, overnightErr := h.Overnight.Quote(attemptCtx, symbol)
				alpacaCh <- quoteAttempt{quote: overnight, err: overnightErr}
			}()
		}
		var baseAttempt, alpacaAttempt quoteAttempt
		baseDone, alpacaDone := false, !alpacaEnabled
		for !baseDone || !alpacaDone {
			select {
			case <-ctx.Done():
				return model.Quote{}, ctx.Err()
			case baseAttempt = <-baseCh:
				baseDone = true
				if baseAttempt.err == nil && baseAttempt.quote.Provider == "yahoo-overnight" {
					h.attachPreviousPostSession(ctx, symbol, &baseAttempt.quote)
					return baseAttempt.quote, nil
				}
			case alpacaAttempt = <-alpacaCh:
				alpacaDone = true
			}
		}
		if baseAttempt.err == nil {
			// Yahoo 有夜盘字段时已在上方提前返回（Provider=="yahoo-overnight"）；走到这里
			// 说明 Yahoo 返回了普通收盘口径。若 Alpaca 同时拿到了夜盘价，优先 Alpaca，
			// 否则每次 Yahoo 夜盘字段不稳定时 overnight_available 就会来回翻转，页面标签
			// 会在"夜盘"/"收盘"之间反复抖动。
			if alpacaAttempt.err == nil && alpacaAttempt.quote.Price != nil {
				quote := overnightOnlyQuote(alpacaAttempt.quote)
				h.attachPreviousPostSession(ctx, symbol, &quote)
				return quote, nil
			}
			h.attachPreviousPostSession(ctx, symbol, &baseAttempt.quote)
			return baseAttempt.quote, nil
		}
		if alpacaAttempt.err == nil && alpacaAttempt.quote.Price != nil {
			quote := overnightOnlyQuote(alpacaAttempt.quote)
			h.attachPreviousPostSession(ctx, symbol, &quote)
			return quote, nil
		}
		if alpacaAttempt.err != nil {
			return model.Quote{}, alpacaAttempt.err
		}
		return model.Quote{}, baseAttempt.err
	}
	quote, err := h.Regular.Quote(ctx, symbol)
	if err != nil {
		return quote, err
	}
	// 连续期货的 Chart 已经包含夜盘分钟数据；不要再走股票专用的
	// previous_post 查询，否则会重新初始化 Yahoo SDK 并增加无意义的等待。
	if IsFutureSymbol(symbol) {
		return quote, nil
	}
	// Pre-market and closed quotes carry the prior regular close in
	// sessions.post.reference_close but no after-hours price. Attach the last
	// completed post-market session so the analysis basis never falls back to
	// the pre-market (or overnight) price while it is labelled 盘后价.
	if shouldAttachPreviousPost(&quote) {
		h.attachPreviousPostSession(ctx, symbol, &quote)
	}
	return quote, nil
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

// shouldAttachPreviousPost reports whether the current quote needs the last
// completed post-market session. During POST/REGULAR the active session is
// authoritative; during PRE/OVERNIGHT/CLOSED there is no current post price,
// and sessions.post only carries a reference close.
func shouldAttachPreviousPost(quote *model.Quote) bool {
	if quote == nil {
		return false
	}
	switch quote.MarketState {
	case "PRE", "OVERNIGHT", "CLOSED":
	default:
		return false
	}
	if summary, ok := quote.Sessions["post"].(map[string]any); ok && flexiblePositiveFloat(summary["price"]) != nil {
		return false
	}
	return true
}

// attachPreviousPostSession stores the last completed after-hours summary as
// sessions.previous_post, leaving sessions.post free to describe the current
// trading day. The analysis basis then shows yesterday's after-hours price
// instead of mislabeling the pre-market/overnight price as 盘后价.
// The lookup is best effort: on failure the quote is returned unchanged.
func (h *Hybrid) attachPreviousPostSession(ctx context.Context, symbol string, quote *model.Quote) {
	if h == nil || quote == nil {
		return
	}
	if quote.Sessions == nil {
		quote.Sessions = make(map[string]any)
	}
	if previousPostSummary(quote) != nil {
		return
	}
	if session := h.previousPostSession(ctx, symbol, quote); session != nil {
		quote.Sessions["previous_post"] = session
	}
}

func previousPostSummary(quote *model.Quote) map[string]any {
	if quote == nil {
		return nil
	}
	summary, ok := quote.Sessions["previous_post"].(map[string]any)
	if !ok || flexiblePositiveFloat(summary["price"]) == nil {
		return nil
	}
	return summary
}

func (h *Hybrid) previousPostSession(ctx context.Context, symbol string, source *model.Quote) map[string]any {
	if h == nil || h.Regular == nil {
		return nil
	}
	now := h.marketNow()
	_, _, day, windowOK := previousPostSessionWindow(now)
	dayKey := ""
	if windowOK {
		dayKey = day.Format(time.DateOnly)
	}
	h.postMu.Lock()
	entry, cached := h.previousPost[symbol]
	h.postMu.Unlock()
	if cached && entry.day == dayKey && now.Before(entry.expires) {
		return entry.session
	}

	// A Yahoo quote fetched for this request already carries the previous
	// post summary when the source still exposes it. Alpaca's overnight quote
	// does not, so ask Yahoo once in that path; a Yahoo quote with no
	// previous_post (pre-market) goes straight to the 1-minute chart.
	session := previousPostSummary(source)
	if session == nil && (source == nil || source.Provider != "yahoo-chart") {
		if regular, err := h.Regular.Quote(ctx, symbol); err == nil {
			session = previousPostSummary(&regular)
		}
	}
	if session == nil {
		if summary, ok := h.Regular.PreviousPostSession(ctx, symbol); ok {
			session = summary
		}
	}

	// A hit stays until the next after-hours window closes; a failed or empty
	// lookup is retried soon instead of being cached for the whole session.
	expires := nextPostSessionEnd(now).Add(time.Minute)
	if session == nil {
		expires = now.Add(previousPostSessionRetry)
	}
	h.postMu.Lock()
	if h.previousPost == nil {
		h.previousPost = make(map[string]previousPostEntry)
	}
	for key, value := range h.previousPost {
		if !now.Before(value.expires) {
			delete(h.previousPost, key)
		}
	}
	if len(h.previousPost) >= previousPostCacheLimit {
		for key := range h.previousPost {
			delete(h.previousPost, key)
			break
		}
	}
	h.previousPost[symbol] = previousPostEntry{session: session, day: dayKey, expires: expires}
	h.postMu.Unlock()
	return session
}

// SnapshotQuote matches the Python snapshot fetch path: outside regular
// NYSE hours, Cboe supplies the underlying quote if Yahoo/Alpaca fails or
// returns no usable price. The standalone quote endpoint continues to use
// Quote and does not request this fallback.
func (h *Hybrid) SnapshotQuote(ctx context.Context, rawSymbol string) (model.Quote, error) {
	if h == nil || h.Regular == nil {
		return model.Quote{}, errors.New("hybrid quote provider is unavailable")
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return model.Quote{}, err
	}
	if IsIndexSymbol(symbol) || IsFutureSymbol(symbol) {
		return h.Quote(ctx, symbol)
	}
	if !h.usesDelayedOptions() {
		return h.Quote(ctx, symbol)
	}

	quote, quoteErr := h.Quote(ctx, symbol)
	if err := ctx.Err(); err != nil {
		return model.Quote{}, err
	}
	if quoteErr == nil && quote.Price != nil {
		return quote, nil
	}
	if h.Delayed == nil {
		return quote, quoteErr
	}
	delayed, err := h.Delayed.Quote(ctx, symbol)
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
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, err
	}
	if IsFutureSymbol(normalized) {
		return []string{}, nil
	}
	if IsIndexSymbol(normalized) {
		if h.Delayed == nil {
			return nil, errors.New("Cboe delayed index options provider is unavailable")
		}
		if _, ok := CboeOptionSymbol(normalized); !ok {
			return nil, fmt.Errorf("指数 %s 暂无可用期权根代码", normalized)
		}
		return h.Delayed.Expirations(ctx, normalized)
	}
	if h.usesDelayedOptions() {
		if h.Delayed == nil {
			return nil, errors.New("Cboe delayed options provider is unavailable")
		}
		return h.Delayed.Expirations(ctx, normalized)
	}
	if h.Regular == nil {
		return nil, errors.New("Yahoo options provider is unavailable")
	}
	return h.Regular.Expirations(ctx, normalized)
}

// NearestChain keeps the regular/delayed selection used by Expirations and
// returns the nearest chain from the same upstream payload.
func (h *Hybrid) NearestChain(ctx context.Context, symbol string) ([]string, []model.OptionRow, *time.Time, error) {
	if h == nil {
		return nil, nil, nil, errors.New("hybrid provider is unavailable")
	}
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, nil, nil, err
	}
	if IsFutureSymbol(normalized) {
		return []string{}, []model.OptionRow{}, nil, nil
	}
	if IsIndexSymbol(normalized) {
		if h.Delayed == nil {
			return nil, nil, nil, errors.New("Cboe delayed index options provider is unavailable")
		}
		if _, ok := CboeOptionSymbol(normalized); !ok {
			return nil, nil, nil, fmt.Errorf("指数 %s 暂无可用期权根代码", normalized)
		}
		return h.Delayed.NearestChain(ctx, normalized)
	}
	if h.usesDelayedOptions() {
		if h.Delayed == nil {
			return nil, nil, nil, errors.New("Cboe delayed options provider is unavailable")
		}
		return h.Delayed.NearestChain(ctx, normalized)
	}
	if h.Regular == nil {
		return nil, nil, nil, errors.New("Yahoo options provider is unavailable")
	}
	return h.Regular.NearestChain(ctx, normalized)
}

func (h *Hybrid) Chain(ctx context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	if h == nil {
		return nil, nil, errors.New("hybrid provider is unavailable")
	}
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return nil, nil, err
	}
	if IsFutureSymbol(normalized) {
		return []model.OptionRow{}, nil, nil
	}
	if IsIndexSymbol(normalized) {
		if h.Delayed == nil {
			return nil, nil, errors.New("Cboe delayed index options provider is unavailable")
		}
		if _, ok := CboeOptionSymbol(normalized); !ok {
			return nil, nil, fmt.Errorf("指数 %s 暂无可用期权根代码", normalized)
		}
		return h.Delayed.Chain(ctx, normalized, expiration)
	}
	if h.usesDelayedOptions() {
		if h.Delayed == nil {
			return nil, nil, errors.New("Cboe delayed options provider is unavailable")
		}
		return h.Delayed.Chain(ctx, normalized, expiration)
	}
	if h.Regular == nil {
		return nil, nil, errors.New("Yahoo options provider is unavailable")
	}
	return h.Regular.Chain(ctx, normalized, expiration)
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

// PriceExtremesHistoryClient 返回底层支持的紧凑价格极值读取能力。
func (h *Hybrid) PriceExtremesHistoryClient() PriceExtremesHistoryClient {
	if h == nil || h.HistoryClient == nil {
		return nil
	}
	client, _ := h.HistoryClient.(PriceExtremesHistoryClient)
	return client
}

var _ PriceExtremesHistorySource = (*Hybrid)(nil)

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

func (h *Hybrid) ChanAdjustedHistory(ctx context.Context, symbol, period string) (ChanAdjustedHistoryResult, error) {
	if h == nil || h.HistoryClient == nil {
		return ChanAdjustedHistoryResult{}, errors.New("Yahoo history provider is unavailable")
	}
	client, ok := h.HistoryClient.(ChanAdjustedHistoryClient)
	if !ok {
		return ChanAdjustedHistoryResult{}, errors.New("Yahoo history client does not support paired adjusted Chan history")
	}
	return client.FetchChanAdjusted(ctx, symbol, period)
}

func (h *Hybrid) IntradayHistory(ctx context.Context, symbol, interval string) (HistoryResult, error) {
	if h == nil || h.HistoryClient == nil {
		return HistoryResult{}, errors.New("Yahoo intraday history provider is unavailable")
	}
	client, ok := h.HistoryClient.(IntradayHistoryClient)
	if !ok {
		return HistoryResult{}, errors.New("Yahoo history client does not support intraday history")
	}
	// 指数和期货没有 Alpaca 股票历史 K 线；仅使用 Yahoo，避免夜盘或休市时
	// 对不可用的股票历史接口发起请求。普通股票继续走两路合并与回退逻辑。
	if normalized, err := NormalizeSymbol(symbol); err == nil {
		if IsIndexSymbol(normalized) || IsFutureSymbol(normalized) {
			return client.FetchIntraday(ctx, normalized, interval)
		}
	}
	if h.Overnight == nil || !h.Overnight.CanRequest() {
		return client.FetchIntraday(ctx, symbol, interval)
	}
	type intradayFetch struct {
		result HistoryResult
		err    error
	}
	yahooChannel := make(chan intradayFetch, 1)
	alpacaChannel := make(chan intradayFetch, 1)
	go func() {
		result, err := client.FetchIntraday(ctx, symbol, interval)
		yahooChannel <- intradayFetch{result: result, err: err}
	}()
	go func() {
		result, err := h.Overnight.FetchIntraday(ctx, symbol, interval)
		alpacaChannel <- intradayFetch{result: result, err: err}
	}()
	yahooFetch := <-yahooChannel
	alpacaFetch := <-alpacaChannel
	if err := ctx.Err(); err != nil {
		return HistoryResult{}, err
	}
	yahooResult, yahooErr := yahooFetch.result, yahooFetch.err
	alpacaResult, alpacaErr := alpacaFetch.result, alpacaFetch.err
	if yahooErr != nil {
		if alpacaErr == nil {
			log.Printf("Yahoo intraday history unavailable symbol=%s; using Alpaca history: %v", symbol, yahooErr)
			return alpacaResult, nil
		}
		return HistoryResult{}, fmt.Errorf("intraday history unavailable from Yahoo and Alpaca: %w", errors.Join(yahooErr, alpacaErr))
	}
	if alpacaErr != nil {
		log.Printf("Alpaca intraday history backfill unavailable symbol=%s: %v", symbol, alpacaErr)
		return yahooResult, nil
	}
	return mergeIntradayHistory(alpacaResult, yahooResult), nil
}

func mergeIntradayHistory(alpaca, yahoo HistoryResult) HistoryResult {
	barsByTime := make(map[string]map[string]any, len(alpaca.Bars)+len(yahoo.Bars))
	addBars := func(bars []map[string]any) {
		for _, bar := range bars {
			at, ok := bar["date"].(string)
			if !ok || strings.TrimSpace(at) == "" {
				continue
			}
			copyBar := make(map[string]any, len(bar))
			for key, value := range bar {
				copyBar[key] = value
			}
			barsByTime[at] = copyBar
		}
	}
	addBars(alpaca.Bars)
	addBars(yahoo.Bars)

	times := make([]string, 0, len(barsByTime))
	for at := range barsByTime {
		times = append(times, at)
	}
	sort.Strings(times)
	bars := make([]map[string]any, 0, len(times))
	for _, at := range times {
		bars = append(bars, barsByTime[at])
	}
	fetchedAt := yahoo.FetchedAt
	if alpaca.FetchedAt.After(fetchedAt) {
		fetchedAt = alpaca.FetchedAt
	}
	symbol := yahoo.Symbol
	if symbol == "" {
		symbol = alpaca.Symbol
	}
	source := yahoo.Source
	if source == "" {
		source = "go-yfinance"
	}
	return HistoryResult{
		Symbol: symbol, Bars: bars, FetchedAt: fetchedAt, Source: source + "+alpaca-iex",
	}
}

// EarningsDates remains available for explicit callers; the service no longer invokes it.
func (h *Hybrid) EarningsDates(ctx context.Context, symbol string) ([]string, error) {
	if h == nil || h.Regular == nil || h.Regular.earnings == nil {
		return nil, errors.New("Yahoo earnings provider is unavailable")
	}
	return h.Regular.earnings.Fetch(ctx, symbol)
}
