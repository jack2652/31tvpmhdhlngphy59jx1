package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnjoon/go-yfinance/pkg/models"
	"option-scope-go/internal/model"
)

func TestHybridRoutesChainsByNYSESession(t *testing.T) {
	var yahooOptions, cboeOptions atomic.Int64
	yahooServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/options/QQQ" {
			yahooOptions.Add(1)
			if r.URL.Query().Get("date") != "" {
				_, _ = fmt.Fprint(w, `{"optionChain":{"result":[{"expirationDates":[1895011200],"options":[{"expirationDate":1895011200,"calls":[{"contractSymbol":"QQQ300119C00100000","strike":100,"inTheMoney":true}],"puts":[]}]}],"error":null}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"optionChain":{"result":[{"expirationDates":[1895011200],"options":[{"expirationDate":1895011200,"calls":[],"puts":[]}]}],"error":null}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer yahooServer.Close()
	cboeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cboeOptions.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"current_price":100,"options":[{"option":"QQQ300119C00100000"}]}}`)
	}))
	defer cboeServer.Close()

	yahoo := NewYahoo(time.Second, 2)
	yahoo.client = yahooServer.Client()
	yahoo.optionsURL = yahooServer.URL + "/options/%s"
	useLocalYahooOptionsAuth(t, yahoo)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		expirations: []time.Time{time.Unix(1895011200, 0)},
		optionChain: &models.OptionChain{
			Expiration: time.Unix(1895011200, 0),
			Calls:      []models.Option{{ContractSymbol: "QQQ300119C00100000", Strike: 100, InTheMoney: true}},
		},
	})
	delayed := NewCboeOptionsClient(cboeServer.Client(), cboeServer.URL+"/{symbol}")
	hybrid := NewHybrid(yahoo, delayed, nil)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{name: "regular", now: time.Date(2026, time.October, 7, 10, 0, 0, 0, newYork), want: "yahoo"},
		{name: "premarket", now: time.Date(2026, time.October, 7, 8, 0, 0, 0, newYork), want: "cboe"},
		{name: "weekend", now: time.Date(2026, time.October, 10, 11, 0, 0, 0, newYork), want: "cboe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hybrid.now = func() time.Time { return tc.now }
			values, err := hybrid.Expirations(context.Background(), "QQQ")
			if err != nil {
				t.Fatal(err)
			}
			if len(values) == 0 {
				t.Fatalf("expected expiration results: %v", values)
			}
		})
	}
	if got := yahooOptions.Load(); got != 0 {
		t.Errorf("Yahoo option-chain calls=%d, want 0 before a regular-session chain request", got)
	}
	if got := cboeOptions.Load(); got != 1 {
		t.Errorf("Cboe options calls=%d, want 1 because the second market-session read is served from cache", got)
	}

	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 10, 0, 0, 0, newYork) }
	regularRows, _, err := hybrid.Chain(context.Background(), "QQQ", "2030-01-19")
	if err != nil || len(regularRows) != 1 || regularRows[0].Provider != "yahoo-options" {
		t.Fatalf("regular-session Chain() = %#v, %v; want Yahoo options", regularRows, err)
	}
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 8, 0, 0, 0, newYork) }
	delayedRows, _, err := hybrid.Chain(context.Background(), "QQQ", "2030-01-19")
	if err != nil || len(delayedRows) != 1 || delayedRows[0].Provider != "cboe-delayed" {
		t.Fatalf("pre-market Chain() = %#v, %v; want Cboe options", delayedRows, err)
	}
	if got := yahooOptions.Load(); got != 1 {
		t.Errorf("Yahoo option-chain calls=%d, want one regular-session chain request", got)
	}
	if got := cboeOptions.Load(); got != 1 {
		t.Errorf("Cboe options calls=%d, want expiration and delayed chain served by the same cached payload", got)
	}
}

func TestHybridQuoteDoesNotUseCboeWhenYahooFails(t *testing.T) {
	var cboeRequests atomic.Int64
	cboeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cboeRequests.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"current_price":100,"options":[]}}`)
	}))
	defer cboeServer.Close()

	yahoo := NewYahoo(time.Second, 2)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quoteErr:   errors.New("Yahoo unavailable"),
		historyErr: map[string]error{"1m": errors.New("Yahoo unavailable"), "1d": errors.New("Yahoo unavailable")},
	})
	delayed := NewCboeOptionsClient(cboeServer.Client(), cboeServer.URL+"/{symbol}")
	hybrid := NewHybrid(yahoo, delayed, nil)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
	}{
		{name: "regular", now: time.Date(2026, time.October, 7, 10, 0, 0, 0, newYork)},
		{name: "premarket", now: time.Date(2026, time.October, 7, 8, 0, 0, 0, newYork)},
		{name: "after hours", now: time.Date(2026, time.October, 7, 17, 0, 0, 0, newYork)},
		{name: "weekend", now: time.Date(2026, time.October, 10, 11, 0, 0, 0, newYork)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hybrid.now = func() time.Time { return tc.now }
			if _, err := hybrid.Quote(context.Background(), "QQQ"); err == nil {
				t.Fatal("Quote() error = nil, want Yahoo failure")
			}
			if got := cboeRequests.Load(); got != 0 {
				t.Fatalf("Cboe quote requests = %d, want none", got)
			}
		})
	}
}

func TestHybridOvernightQuoteFallsBackToAlpacaWithoutCboe(t *testing.T) {
	var cboeRequests atomic.Int64
	alpacaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"QQQ":{"latestTrade":{"p":101,"t":"2026-10-08T00:00:00Z"},"prevDailyBar":{"c":100}}}`)
	}))
	defer alpacaServer.Close()
	cboeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cboeRequests.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"current_price":99,"options":[]}}`)
	}))
	defer cboeServer.Close()

	yahoo := NewYahoo(time.Second, 2)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quoteErr:   errors.New("Yahoo unavailable"),
		historyErr: map[string]error{"1m": errors.New("Yahoo unavailable"), "1d": errors.New("Yahoo unavailable")},
	})
	delayed := NewCboeOptionsClient(cboeServer.Client(), cboeServer.URL+"/{symbol}")
	alpaca := NewAlpaca("key", "secret", alpacaServer.URL, alpacaServer.Client())
	hybrid := NewHybrid(yahoo, delayed, alpaca)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 20, 0, 0, 0, newYork) }

	quote, err := hybrid.Quote(context.Background(), "QQQ")
	if err != nil || quote.Price == nil || *quote.Price != 101 || quote.Provider != "alpaca-overnight" {
		t.Fatalf("overnight Quote() = %#v, %v; want Alpaca quote after Yahoo failure", quote, err)
	}
	if got := cboeRequests.Load(); got != 0 {
		t.Fatalf("Cboe quote requests = %d, want none", got)
	}
}

func TestHybridOvernightQuoteMergesAlpacaAndFallsBack(t *testing.T) {
	var price float64 = 100
	base := model.Quote{Symbol: "QQQ", Price: &price, Provider: "yahoo-chart", Currency: "USD", PreviousClose: floatPtr(98.0), Sessions: map[string]any{"post": map[string]any{"reference_close": float64(99)}}}
	overnightPrice, previous, open := 101.0, 99.5, 100.5
	overnight := model.Quote{Symbol: "QQQ", Price: &overnightPrice, PreviousClose: &previous, TodayOpen: &open, Provider: "alpaca-overnight", Sessions: map[string]any{"overnight": map[string]any{"price": overnightPrice}}}
	merged := mergeOvernightQuote(base, overnight)
	if merged.Price == nil || *merged.Price != 101 || merged.PreviousClose == nil || *merged.PreviousClose != 99 || merged.TodayOpen == nil || *merged.TodayOpen != 100.5 || merged.MarketState != "OVERNIGHT" {
		t.Fatalf("unexpected merged quote: %#v", merged)
	}
	if merged.ChangePercent == nil || *merged.ChangePercent < 2.02 || *merged.ChangePercent > 2.03 {
		t.Fatalf("change percent=%v, want near 2.0202", merged.ChangePercent)
	}
}

func TestHybridOvernightQuoteFetchesIndependentProvidersConcurrently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- "alpaca"
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"QQQ":{"latestTrade":{"p":101},"prevDailyBar":{"c":100}}}`)
	}))
	defer server.Close()

	hybrid, err := NewDefaultHybrid(2*time.Second, 2, "", "key", "secret", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hybrid.Regular.tickerFactory = func(string) (yahooTicker, error) {
		return &yahooSDKTestTicker{quoteFunc: func() (*models.Quote, error) {
			entered <- "yahoo"
			<-release
			return &models.Quote{Symbol: "QQQ", Currency: "USD", RegularMarketPrice: 100, RegularMarketPreviousClose: 99, MarketState: "REGULAR"}, nil
		}}, nil
	}
	hybrid.Overnight.baseURL = server.URL
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 20, 0, 0, 0, newYork) }

	result := make(chan error, 1)
	go func() {
		_, quoteErr := hybrid.Quote(context.Background(), "QQQ")
		result <- quoteErr
	}()

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case providerName := <-entered:
			seen[providerName] = true
		case <-time.After(time.Second):
			close(release)
			<-result
			t.Fatalf("only these providers entered before release: %v", seen)
		}
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Hybrid.Quote() error = %v", err)
	}
}

func TestHybridOvernightQuoteDoesNotReturnFallbackAfterCancellation(t *testing.T) {
	responseGate := make(chan struct{})
	var responseGateOnce sync.Once
	releaseResponse := func() { responseGateOnce.Do(func() { close(responseGate) }) }
	requestStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestStarted <- struct{}{}
		<-responseGate
		_, _ = fmt.Fprint(w, `{"QQQ":{"latestTrade":{"p":101},"prevDailyBar":{"c":100}}}`)
	}))
	defer server.Close()
	defer releaseResponse()

	historyStarted := make(chan struct{})
	historyRelease := make(chan struct{})
	var historyReleaseOnce sync.Once
	releaseHistory := func() { historyReleaseOnce.Do(func() { close(historyRelease) }) }
	defer releaseHistory()
	historyFinished := make(chan struct{})
	yahoo := NewYahoo(time.Second, 2)
	yahoo.tickerFactory = func(string) (yahooTicker, error) {
		return &yahooSDKTestTicker{
			quoteFunc: func() (*models.Quote, error) {
				return &models.Quote{Symbol: "QQQ", RegularMarketPrice: 100, RegularMarketPreviousClose: 99, MarketState: "REGULAR"}, nil
			},
			historyFunc: func(params models.HistoryParams) ([]models.Bar, error) {
				if params.Interval == "1m" {
					close(historyStarted)
					<-historyRelease
					close(historyFinished)
				}
				return nil, nil
			},
		}, nil
	}
	alpaca := NewAlpaca("key", "secret", server.URL, server.Client())
	hybrid := NewHybrid(yahoo, nil, alpaca)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 20, 0, 0, 0, newYork) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type quoteResult struct {
		quote model.Quote
		err   error
	}
	result := make(chan quoteResult, 1)
	go func() {
		quote, quoteErr := hybrid.Quote(ctx, "QQQ")
		result <- quoteResult{quote: quote, err: quoteErr}
	}()

	for name, signal := range map[string]<-chan struct{}{
		"Alpaca request": requestStarted,
		"Yahoo history":  historyStarted,
	} {
		select {
		case <-signal:
		case <-time.After(time.Second):
			releaseResponse()
			releaseHistory()
			t.Fatalf("%s did not start", name)
		}
	}

	// The request has passed CanRequest. A non-zero cooldown marker lets
	// the test observe Alpaca's successful response parsing via markAuthSuccess.
	alpaca.authMu.Lock()
	alpaca.authRetryAt = time.Now().Add(time.Hour)
	alpaca.authMu.Unlock()
	releaseResponse()
	deadline := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		alpaca.authMu.Lock()
		responseParsed := alpaca.authRetryAt.IsZero()
		alpaca.authMu.Unlock()
		if responseParsed {
			break
		}
		select {
		case <-deadline.C:
			releaseHistory()
			t.Fatal("Alpaca did not finish parsing the successful response")
		case <-ticker.C:
		}
	}
	cancel()

	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Hybrid.Quote() = %#v, error %v; want context.Canceled after Alpaca succeeds", got.quote, got.err)
		}
	case <-time.After(time.Second):
		releaseHistory()
		t.Fatal("Hybrid.Quote() did not return after cancellation")
	}
	releaseHistory()
	select {
	case <-historyFinished:
	case <-time.After(time.Second):
		t.Fatal("Yahoo SDK operation did not finish after releasing its history call")
	}
}

func TestDefaultHybridSharesGateWithoutSerializingProviders(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- "alpaca"
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"BBB":{"latestTrade":{"p":101},"prevDailyBar":{"c":100},"dailyBar":{"o":100},"minuteBar":{"c":101}}}`)
	}))
	defer server.Close()

	hybrid, err := NewDefaultHybrid(2*time.Second, 2, "", "key", "secret", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hybrid.Regular.tickerFactory = func(string) (yahooTicker, error) {
		return &yahooSDKTestTicker{quoteFunc: func() (*models.Quote, error) {
			entered <- "yahoo"
			<-release
			return &models.Quote{Symbol: "AAA", Currency: "USD", RegularMarketPrice: 100, RegularMarketPreviousClose: 99, MarketState: "REGULAR"}, nil
		}}, nil
	}
	hybrid.Overnight.baseURL = server.URL

	if cap(hybrid.Regular.semaphore) != 2 || hybrid.Overnight.Gate != hybrid.Regular.semaphore ||
		hybrid.Delayed.Gate != hybrid.Regular.semaphore || hybrid.Regular.fundamentals.Gate != hybrid.Regular.semaphore ||
		hybrid.Regular.earnings.Gate != hybrid.Regular.semaphore {
		t.Fatal("default hybrid clients do not share the configured two-request gate")
	}

	results := make(chan error, 2)
	go func() {
		_, quoteErr := hybrid.Regular.Quote(context.Background(), "AAA")
		results <- quoteErr
	}()
	go func() {
		_, quoteErr := hybrid.Overnight.Quote(context.Background(), "BBB")
		results <- quoteErr
	}()

	paths := make([]string, 0, 2)
	for len(paths) < 2 {
		select {
		case path := <-entered:
			paths = append(paths, path)
		case <-time.After(time.Second):
			close(release)
			for range 2 {
				<-results
			}
			t.Fatalf("only %d provider request(s) entered the server concurrently", len(paths))
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("provider request failed: %v", err)
		}
	}
}

func TestDefaultHybridUsesPythonProviderTimeouts(t *testing.T) {
	hybrid, err := NewDefaultHybrid(20*time.Second, 2, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := hybrid.Regular.client.Timeout; got != 20*time.Second {
		t.Errorf("Yahoo timeout=%s, want configured 20s", got)
	}
	if got := hybrid.Delayed.Client.Timeout; got != 15*time.Second {
		t.Errorf("Cboe timeout=%s, want Python's 15s", got)
	}

	defaults, err := NewDefaultHybrid(0, 2, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := defaults.Regular.client.Timeout; got != 12*time.Second {
		t.Errorf("default Yahoo timeout=%s, want Python's 12s", got)
	}
	if got := defaults.Delayed.Client.Timeout; got != 15*time.Second {
		t.Errorf("default Cboe timeout=%s, want Python's 15s", got)
	}
}

type hybridHistorySpy struct {
	calls  []string
	result HistoryResult
}

func (s *hybridHistorySpy) Fetch(_ context.Context, symbol, period string) (HistoryResult, error) {
	s.calls = append(s.calls, "history:"+symbol+":"+period)
	return s.result, nil
}

func (s *hybridHistorySpy) FetchAdjusted(_ context.Context, symbol, period string) (HistoryResult, error) {
	s.calls = append(s.calls, "adjusted:"+symbol+":"+period)
	return s.result, nil
}

func TestNewHybridConfiguresDefaultYFinanceHistoryClient(t *testing.T) {
	regular := NewYahoo(time.Second, 3)
	hybrid := NewHybrid(regular, nil, nil)
	history, ok := hybrid.HistoryClient.(*YFinanceHistoryClient)
	if !ok {
		t.Fatalf("HistoryClient type = %T, want *YFinanceHistoryClient", hybrid.HistoryClient)
	}
	if history.TickerFactory == nil || history.Gate != regular.semaphore || history.GateWait != regular.gateWait {
		t.Fatal("default history client must share Yahoo's SDK ticker factory and request gate")
	}
}

func TestHybridForwardsHistoryBenchmarkAndEarnings(t *testing.T) {
	regular := NewYahoo(time.Second, 2)
	hybrid := NewHybrid(regular, nil, nil)
	spy := &hybridHistorySpy{result: HistoryResult{Symbol: "TEST"}}
	hybrid.HistoryClient = spy

	if _, err := hybrid.History(context.Background(), "test", "6mo"); err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if _, err := hybrid.BenchmarkHistory(context.Background(), "^GSPC", "2y"); err != nil {
		t.Fatalf("BenchmarkHistory() error = %v", err)
	}
	if got, want := strings.Join(spy.calls, ","), "history:test:6mo,adjusted:^GSPC:2y"; got != want {
		t.Fatalf("history forwarding calls = %q, want %q", got, want)
	}

	var gotSymbol string
	regular.earnings = &YahooEarningsClient{Client: http.DefaultClient}
	regular.earnings.tickerFactory = func(symbol string) (yahooTicker, error) {
		gotSymbol = symbol
		return &calendarTestTicker{calendar: &models.Calendar{EarningsDate: []time.Time{
			time.Date(2032, 1, 9, 0, 0, 0, 0, time.UTC),
		}}}, nil
	}
	dates, err := hybrid.EarningsDates(context.Background(), "test")
	if err != nil || fmt.Sprint(dates) != "[2032-01-08]" {
		t.Fatalf("EarningsDates() = %#v, %v; want forwarded Yahoo result", dates, err)
	}
	if gotSymbol != "TEST" {
		t.Fatalf("earnings ticker symbol = %q, want TEST", gotSymbol)
	}
}

func TestCboeConcurrentSameSymbolRequestsShareOneUpstreamFetch(t *testing.T) {
	var requests atomic.Int64
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		_, _ = fmt.Fprint(w, `{"data":{"current_price":100,"options":[{"option":"QQQ300118C00100000"}]}}`)
	}))
	defer server.Close()
	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheTTL = time.Minute
	client.Gate = make(chan struct{}, 2)
	client.GateWait = time.Second

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, _, err := client.fetch(context.Background(), "QQQ")
			results <- err
		}()
	}
	close(start)
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Cboe upstream request did not start")
	}
	// Let all callers observe the in-flight fetch before it completes.
	time.Sleep(25 * time.Millisecond)
	close(release)
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Cboe fetch failed: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent upstream requests = %d, want 1", got)
	}
}

func TestHybridSessionBoundariesAndHolidays(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		now       time.Time
		regular   bool
		overnight bool
	}{
		{"regular open", time.Date(2026, time.October, 7, 9, 30, 0, 0, location), true, false},
		{"regular close", time.Date(2026, time.October, 7, 16, 0, 0, 0, location), false, false},
		{"early close before 13:00", time.Date(2025, time.December, 24, 12, 59, 0, 0, location), true, false},
		{"early close after 13:00", time.Date(2025, time.December, 24, 13, 0, 0, 0, location), false, false},
		{"independence day", time.Date(2026, time.July, 3, 10, 0, 0, 0, location), false, false},
		{"overnight opens for next session", time.Date(2026, time.October, 7, 20, 0, 0, 0, location), false, true},
		{"holiday overnight", time.Date(2026, time.July, 2, 20, 0, 0, 0, location), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNYSERegularSession(tc.now); got != tc.regular {
				t.Errorf("regular=%v, want %v", got, tc.regular)
			}
			if got := isNYSEOvernightSession(tc.now); got != tc.overnight {
				t.Errorf("overnight=%v, want %v", got, tc.overnight)
			}
		})
	}
}
