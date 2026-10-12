package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
	"option-scope-go/internal/outboundhttp"
)

type yahooSDKTestTicker struct {
	yahooTicker
	quote       *ymodels.Quote
	quoteErr    error
	quoteFunc   func() (*ymodels.Quote, error)
	bars        map[string][]ymodels.Bar
	historyErr  map[string]error
	expirations []time.Time
	optionsErr  error
	optionChain *ymodels.OptionChain
	chainErr    error
	chainFunc   func(string) (*ymodels.OptionChain, error)
	historyFunc func(ymodels.HistoryParams) ([]ymodels.Bar, error)
	closed      func()
}

func (t *yahooSDKTestTicker) Quote() (*ymodels.Quote, error) {
	if t.quoteFunc != nil {
		return t.quoteFunc()
	}
	return t.quote, t.quoteErr
}

func (t *yahooSDKTestTicker) History(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
	if t.historyFunc != nil {
		return t.historyFunc(params)
	}
	return t.bars[params.Interval], t.historyErr[params.Interval]
}

func (t *yahooSDKTestTicker) Options() ([]time.Time, error) { return t.expirations, t.optionsErr }

func (t *yahooSDKTestTicker) OptionChain(expiration string) (*ymodels.OptionChain, error) {
	if t.chainFunc != nil {
		return t.chainFunc(expiration)
	}
	return t.optionChain, t.chainErr
}

func (t *yahooSDKTestTicker) Close() {
	if t.closed != nil {
		t.closed()
	}
}

func setYahooSDKTestTicker(yahoo *Yahoo, ticker *yahooSDKTestTicker) {
	yahoo.tickerFactory = func(string) (yahooTicker, error) { return ticker, nil }
}

func setYahooSDKTestTickerFromOptionJSON(t *testing.T, yahoo *Yahoo, body string) {
	t.Helper()
	var response ymodels.OptionChainResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode SDK option chain fixture: %v", err)
	}
	chain := &ymodels.OptionChain{}
	if len(response.OptionChain.Result) > 0 && len(response.OptionChain.Result[0].Options) > 0 {
		result := response.OptionChain.Result[0].Options[0]
		chain.Calls = result.Calls
		chain.Puts = result.Puts
		if result.ExpirationDate != 0 {
			chain.Expiration = time.Unix(result.ExpirationDate, 0)
		}
	}
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{optionChain: chain})
}

func TestNewYahooWithProxy(t *testing.T) {
	proxyHit := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"symbol":"TEST","currency":"USD","regularMarketPrice":10,"previousClose":8,"marketState":"REGULAR"}}],"error":null}}`))
	}))
	defer proxy.Close()

	client, err := NewYahooWithProxy(time.Second, 1, proxy.URL)
	if err != nil {
		t.Fatalf("NewYahooWithProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	request, err := http.NewRequest(http.MethodGet, "http://upstream.example/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.client.Transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("request through proxy error = %v", err)
	}
	_ = response.Body.Close()
	select {
	case <-proxyHit:
	default:
		t.Fatal("configured proxy did not receive request")
	}
	if outboundhttp.ProxyAddressFromTransport(client.client.Transport) != proxy.URL {
		t.Fatal("proxy transport was not configured")
	}
	if _, ok := client.client.Transport.(*http.Transport); ok {
		t.Fatal("Yahoo client should use the shared CycleTLS browser transport")
	}
}

func TestOutboundHTTPProxyAddressUsesConfiguredHTTPTransport(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:10808")
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: func(request *http.Request) (*url.URL, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "query1.finance.yahoo.com" {
			t.Fatalf("proxy lookup target = %s, want Yahoo HTTPS", request.URL)
		}
		return proxyURL, nil
	}}

	got := outboundhttp.ProxyAddressFromTransport(transport)
	if got != proxyURL.String() {
		t.Fatalf("ProxyAddressFromTransport() = %q, want %q", got, proxyURL.String())
	}
}

func TestOutboundHTTPProxyAddressAllowsProxyBypass(t *testing.T) {
	transport := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	if got := outboundhttp.ProxyAddressFromTransport(transport); got != "" {
		t.Fatalf("ProxyAddressFromTransport() = %q; want no proxy", got)
	}
}

func TestYahooOptionsAuthenticationUsesGoYFinanceAuthManager(t *testing.T) {
	expirationDate := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	expiration := expirationDate.Unix()
	yahoo := NewYahoo(time.Second, 1)
	t.Cleanup(func() { _ = yahoo.Close() })
	yahoo.optionsURL = "https://query1.finance.yahoo.com/v7/finance/options/%s"
	var sdkCalls int
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{chainFunc: func(date string) (*ymodels.OptionChain, error) {
		sdkCalls++
		if date != yahooSDKExpirationLookupDate(expirationDate, time.Local) {
			t.Errorf("SDK OptionChain date = %q, want requested expiration", date)
		}
		return &ymodels.OptionChain{Calls: []ymodels.Option{{ContractSymbol: "TEST300118C00100000", Strike: 100}}}, nil
	}})
	var calls int
	yahoo.optionsAuth = yahooCrumbGetterFunc(func(endpoint string, query url.Values) (*yahooauth.Response, error) {
		calls++
		if endpoint != "https://query1.finance.yahoo.com/v7/finance/options/TEST" {
			t.Errorf("options endpoint = %q", endpoint)
		}
		if query.Get("date") != strconv.FormatInt(expiration, 10) || query.Has("crumb") {
			t.Errorf("AuthManager params = %v, want SDK-managed crumb and requested expiration", query)
		}
		return &yahooauth.Response{
			StatusCode: http.StatusOK,
			Body:       fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":[{"contractSymbol":"TEST300118C00100000","strike":100}],"puts":[]}]}],"error":null}}`, expiration),
		}, nil
	})

	got, _, err := yahoo.Chain(context.Background(), "TEST", "2030-01-18")
	if err != nil || len(got) != 1 || got[0].ContractSymbol != "TEST300118C00100000" {
		t.Fatalf("Chain() = %#v, %v", got, err)
	}
	if calls != 1 {
		t.Fatalf("go-yfinance authenticated requests = %d, want 1", calls)
	}
	if sdkCalls != 1 {
		t.Fatalf("go-yfinance OptionChain calls = %d, want 1", sdkCalls)
	}
}

type yahooCrumbGetterFunc func(string, url.Values) (*yahooauth.Response, error)

func (f yahooCrumbGetterFunc) GetWithCrumb(endpoint string, query url.Values) (*yahooauth.Response, error) {
	return f(endpoint, query)
}

func useLocalYahooOptionsAuth(t *testing.T, yahoo *Yahoo) {
	t.Helper()
	yahoo.optionsAuth = yahooCrumbGetterFunc(func(endpoint string, query url.Values) (*yahooauth.Response, error) {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		params := make(url.Values, len(query)+1)
		for key, values := range query {
			params[key] = append([]string(nil), values...)
		}
		params.Set("crumb", "fixture-crumb")
		request.URL.RawQuery = params.Encode()
		response, err := yahoo.client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		return &yahooauth.Response{StatusCode: response.StatusCode, Body: string(body)}, nil
	})
}

func TestNormalizeSymbolMatchesPythonBoundaries(t *testing.T) {
	for _, raw := range []string{"qqq", "BRK.B", "A-B", "A123456789"} {
		if got, err := NormalizeSymbol(raw); err != nil || got != strings.ToUpper(raw) {
			t.Errorf("NormalizeSymbol(%q) = %q, %v; want normalized valid symbol", raw, got, err)
		}
	}
	for _, raw := range []string{"", "^GSPC", "A=B", "ABCDEFGHIJK", ".QQQ", "-QQQ"} {
		if got, err := NormalizeSymbol(raw); err == nil {
			t.Errorf("NormalizeSymbol(%q) = %q, want validation error", raw, got)
		}
	}
}

func TestYahooQuoteExpirationAndChainContract(t *testing.T) {
	expiryTime := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	expiryTimestamp := expiryTime.Unix()
	expectedExpiry := expiryTime.Format("2006-01-02")
	chainRequestDate := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v7/finance/options/TEST":
			date := r.URL.Query().Get("date")
			if date != "" {
				chainRequestDate <- date
			}
			if got := date; got != "" && got != fmt.Sprint(expiryTimestamp) {
				t.Errorf("options date = %q, want %q or empty", got, fmt.Sprint(expiryTimestamp))
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"optionChain":{"result":[{"expirationDates":[%d],"options":[{"expirationDate":%d,"calls":[{"contractSymbol":"TEST300118C00100000","strike":100,"lastPrice":12,"bid":11,"ask":13,"percentChange":5,"volume":7,"openInterest":20,"impliedVolatility":0.25,"gamma":0.015,"inTheMoney":true}],"puts":[{"contractSymbol":"TEST300118P00100000","strike":100,"lastPrice":2,"bid":1,"ask":3,"percentChange":-5,"volume":4,"openInterest":10,"impliedVolatility":0.3,"inTheMoney":false}]}]}],"error":null}}`, expiryTimestamp, expiryTimestamp)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	yahoo := NewYahoo(time.Second, 1)
	yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
	useLocalYahooOptionsAuth(t, yahoo)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote:       &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 110, RegularMarketOpen: 108, RegularMarketPreviousClose: 100, MarketState: "REGULAR"},
		expirations: []time.Time{expiryTime},
		optionChain: &ymodels.OptionChain{
			Expiration: expiryTime,
			Calls:      []ymodels.Option{{ContractSymbol: "TEST300118C00100000", Strike: 100, LastPrice: 12, Bid: 11, Ask: 13, PercentChange: 5, Volume: 7, OpenInterest: 20, ImpliedVolatility: 0.25, InTheMoney: true}},
			Puts:       []ymodels.Option{{ContractSymbol: "TEST300118P00100000", Strike: 100, LastPrice: 2, Bid: 1, Ask: 3, PercentChange: -5, Volume: 4, OpenInterest: 10, ImpliedVolatility: 0.3}},
		},
	})

	quote, err := yahoo.Quote(context.Background(), " test ")
	if err != nil || quote.Symbol != "TEST" || quote.Price == nil || *quote.Price != 110 || quote.ChangePercent == nil || *quote.ChangePercent != 10 || quote.TodayOpen == nil || *quote.TodayOpen != 108 {
		t.Fatalf("unexpected quote=%#v err=%v", quote, err)
	}
	expirations, err := yahoo.Expirations(context.Background(), "TEST")
	if err != nil || len(expirations) != 1 || expirations[0] != expectedExpiry {
		t.Fatalf("unexpected expirations=%#v err=%v", expirations, err)
	}
	rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", expectedExpiry)
	if err != nil || fetchedAt == nil || len(rows) != 2 {
		t.Fatalf("unexpected rows=%#v fetchedAt=%v err=%v", rows, fetchedAt, err)
	}
	if rows[0].Gamma == nil || *rows[0].Gamma != 0.015 {
		t.Errorf("Yahoo upstream gamma=%v, want preserved 0.015", rows[0].Gamma)
	}
	if rows[1].Gamma != nil {
		t.Errorf("missing Yahoo gamma=%v, want nil for the refresh estimator", rows[1].Gamma)
	}
	select {
	case got := <-chainRequestDate:
		if got != fmt.Sprint(expiryTimestamp) {
			t.Fatalf("chain date query = %q, want %q", got, fmt.Sprint(expiryTimestamp))
		}
	default:
		t.Fatal("chain request did not include Unix expiration date")
	}
	if rows[0].ContractType != "call" || rows[0].Provider != "yahoo-options" || rows[0].ContractSymbol != "TEST300118C00100000" || rows[1].ContractType != "put" || rows[1].Provider != "yahoo-options" || rows[1].ContractSymbol != "TEST300118P00100000" {
		t.Fatalf("unexpected contract mapping: %#v", rows)
	}
	if !rows[0].InTheMoneyKnown || !rows[0].InTheMoney || !rows[1].InTheMoneyKnown || rows[1].InTheMoney {
		t.Fatalf("explicit in-the-money values were not preserved: %#v", rows)
	}
}

func TestYahooSDKExpirationLookupDateUsesSDKLocalCacheKey(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		location *time.Location
		want     string
	}{
		{name: "UTC-5", location: time.FixedZone("UTC-5", -5*60*60), want: "2030-01-17"},
		{name: "UTC+8", location: time.FixedZone("UTC+8", 8*60*60), want: "2030-01-18"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := yahooSDKExpirationLookupDate(expiration, test.location); got != test.want {
				t.Fatalf("SDK lookup date = %q, want %q", got, test.want)
			}
		})
	}
}

func TestYahooQuoteFallsBackToDailyBarsWhenIntradayRequestFails(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2026, time.September, 25, 10, 0, 0, 0, location)
	var intradayRequested, dailyRequested atomic.Bool
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quoteErr: errors.New("quote endpoint unavailable"),
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			switch params.Interval {
			case "1m":
				intradayRequested.Store(true)
				if !params.PrePost {
					t.Error("intraday history omitted pre/post sessions")
				}
				return nil, errors.New("intraday unavailable")
			case "1d":
				dailyRequested.Store(true)
				return []ymodels.Bar{
					{Date: time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC), Open: 99, Close: 100},
					{Date: time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC), Open: 101, Close: 102.5},
				}, nil
			default:
				t.Errorf("unexpected history interval: %q", params.Interval)
				return nil, errors.New("unexpected history interval")
			}
		},
	})
	yahoo.now = func() time.Time { return frozen }
	quote, err := yahoo.Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if !intradayRequested.Load() || !dailyRequested.Load() {
		t.Fatalf("requests intraday=%v daily=%v, want both", intradayRequested.Load(), dailyRequested.Load())
	}
	if quote.Price == nil || *quote.Price != 102.5 || quote.PreviousClose == nil || *quote.PreviousClose != 100 || quote.TodayOpen == nil || *quote.TodayOpen != 101 {
		t.Fatalf("daily fallback quote = %#v, want price 102.5 previous 100 open 101", quote)
	}
	if quote.MarketState != "REGULAR" || len(quote.Sessions) != 0 {
		t.Fatalf("quote state/sessions = %q/%#v, want regular quote without optional session data", quote.MarketState, quote.Sessions)
	}
}

func TestYahooQuoteSkipsIntradayWhenExchangeCalendarIsClosed(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Sunday: Yahoo occasionally keeps the previous REGULAR state in its
	// quote payload even though no current intraday bars can exist.
	frozen := time.Date(2026, time.October, 11, 12, 0, 0, 0, location)
	var intradayCalls atomic.Int32
	yahoo := NewYahoo(time.Second, 2)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 100, RegularMarketPreviousClose: 99, MarketState: "REGULAR"},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval == "1m" {
				intradayCalls.Add(1)
			}
			return nil, nil
		},
	})
	yahoo.now = func() time.Time { return frozen }
	if _, err := yahoo.Quote(context.Background(), "TEST"); err != nil {
		t.Fatalf("Quote() error=%v", err)
	}
	if got := intradayCalls.Load(); got != 0 {
		t.Fatalf("closed-market intraday calls=%d, want 0", got)
	}
}

func TestYahooQuoteLoadsQuoteAndIntradayConcurrently(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2026, time.September, 25, 10, 0, 0, 0, location)
	historyStarted := make(chan struct{})
	var overlapped atomic.Bool
	yahoo := NewYahoo(time.Second, 1)
	yahoo.now = func() time.Time { return frozen }
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quoteFunc: func() (*ymodels.Quote, error) {
			select {
			case <-historyStarted:
				overlapped.Store(true)
			case <-time.After(time.Second):
			}
			return &ymodels.Quote{Symbol: "TEST", RegularMarketPrice: 10, RegularMarketPreviousClose: 9, MarketState: "REGULAR"}, nil
		},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval != "1m" {
				t.Errorf("History interval = %q, want 1m", params.Interval)
			}
			close(historyStarted)
			return nil, nil
		},
	})

	if _, err := yahoo.Quote(context.Background(), "TEST"); err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if !overlapped.Load() {
		t.Fatal("quote and intraday history did not overlap")
	}
}

func TestYahooQuoteUsesDailyBarsToFillMissingPrimaryFields(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2026, time.September, 25, 10, 0, 0, 0, location)
	var dailyRequested atomic.Bool
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 110, RegularMarketOpen: 108, MarketState: "REGULAR"},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval == "1m" {
				return nil, nil
			}
			dailyRequested.Store(true)
			return []ymodels.Bar{
				{Date: time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC), Open: 99, Close: 100},
				{Date: time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC), Open: 101, Close: 102.5},
			}, nil
		},
	})
	yahoo.now = func() time.Time { return frozen }
	quote, err := yahoo.Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if !dailyRequested.Load() {
		t.Fatal("missing previous close did not trigger the daily fallback")
	}
	if quote.Price == nil || *quote.Price != 110 || quote.PreviousClose == nil || *quote.PreviousClose != 100 || quote.TodayOpen == nil || *quote.TodayOpen != 108 {
		t.Fatalf("fallback overwrote primary fields or failed to fill previous close: %#v", quote)
	}
}

func TestYahooQuotePreservesPrimaryErrorWhenDailyFallbackAlsoFails(t *testing.T) {
	var intradayRequested, dailyRequested atomic.Bool
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quoteErr: errors.New("original intraday HTTP 502"),
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			switch params.Interval {
			case "1m":
				intradayRequested.Store(true)
				return nil, errors.New("intraday unavailable")
			case "1d":
				dailyRequested.Store(true)
				return nil, errors.New("daily unavailable")
			default:
				t.Errorf("unexpected history interval %q", params.Interval)
				return nil, errors.New("unexpected history interval")
			}
		},
	})
	_, err := yahoo.Quote(context.Background(), "TEST")
	if err == nil || !strings.Contains(err.Error(), "original intraday HTTP 502") {
		t.Fatalf("Quote() error = %v, want primary SDK quote error", err)
	}
	if !intradayRequested.Load() || !dailyRequested.Load() {
		t.Fatalf("requests intraday=%v daily=%v, want both", intradayRequested.Load(), dailyRequested.Load())
	}
}

func TestYahooExpirationsPreservesUpstreamDatesForAPIFiltering(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2026, time.January, 16, 20, 30, 0, 0, newYork)
	past := time.Date(2026, time.January, 9, 0, 0, 0, 0, time.UTC).Unix()
	today := time.Date(2026, time.January, 16, 0, 0, 0, 0, time.UTC).Unix()
	future := time.Date(2026, time.January, 23, 0, 0, 0, 0, time.UTC).Unix()
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{expirations: []time.Time{
		time.Unix(past, 0), time.Unix(today, 0), time.Unix(future, 0),
	}})

	got, err := yahoo.Expirations(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-01-09", "2026-01-16", "2026-01-23"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expirations() = %#v, want upstream dates %#v regardless of New York date %s", got, want, frozen.Format("2006-01-02"))
	}
}

func TestYahooQuoteTreatsEarlyCloseAfternoonAsPostMarket(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2025, time.December, 24, 14, 0, 0, 0, newYork)
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 100, RegularMarketOpen: 99, RegularMarketPreviousClose: 98, MarketState: "REGULAR"},
		bars: map[string][]ymodels.Bar{
			"1m": {{Date: time.Date(2025, time.December, 24, 13, 0, 0, 0, newYork), Open: 99, Close: 100}},
		},
	})
	yahoo.now = func() time.Time { return frozen }

	quote, err := yahoo.Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	if quote.MarketState != "POST" {
		t.Fatalf("MarketState = %q, want POST after the 13:00 early close", quote.MarketState)
	}
}

func TestYahooQuoteMapsPreRegularPostAndOvernightSessions(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	stamps := []time.Time{
		time.Date(2026, 9, 24, 15, 59, 0, 0, location),
		time.Date(2026, 9, 25, 8, 10, 0, 0, location),
		time.Date(2026, 9, 25, 9, 29, 0, 0, location),
		time.Date(2026, 9, 25, 9, 30, 0, 0, location),
		time.Date(2026, 9, 25, 10, 0, 0, 0, location),
		time.Date(2026, 9, 25, 15, 59, 0, 0, location),
		time.Date(2026, 9, 25, 18, 0, 0, 0, location),
		time.Date(2026, 9, 28, 2, 0, 0, 0, location),
	}
	timestamps := make([]string, len(stamps))
	for index, stamp := range stamps {
		timestamps[index] = strconv.FormatInt(stamp.Unix(), 10)
	}
	openValues := []string{"137", "138", "138.2", "138.27", "139", "140", "140.11", "138"}
	closeValues := []string{"139.54", "140", "138.26", "138.4", "140", "137.79", "138.91", "139.1"}
	cases := []struct {
		name, now, state                              string
		wantPrice, wantPrevious, wantOpen, wantChange float64
	}{
		{name: "pre", now: "2026-09-25T09:20:00-04:00", state: "PRE", wantPrice: 140.11, wantPrevious: 139.54, wantOpen: 136, wantChange: (140.11 - 139.54) / 139.54 * 100},
		{name: "regular", now: "2026-09-25T10:01:00-04:00", state: "REGULAR", wantPrice: 140.11, wantPrevious: 139.54, wantOpen: 138.27, wantChange: (140.11 - 139.54) / 139.54 * 100},
		{name: "post", now: "2026-09-25T18:01:00-04:00", state: "POST", wantPrice: 140.11, wantPrevious: 139.54, wantOpen: 138.27, wantChange: (140.11 - 139.54) / 139.54 * 100},
		{name: "overnight", now: "2026-09-28T02:01:00-04:00", state: "OVERNIGHT", wantPrice: 140.11, wantPrevious: 139.54, wantOpen: 138.27, wantChange: (140.11 - 139.54) / 139.54 * 100},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			frozen, err := time.Parse(time.RFC3339, test.now)
			if err != nil {
				t.Fatal(err)
			}
			bars := make([]ymodels.Bar, 0, len(stamps))
			for index, stamp := range stamps {
				if stamp.After(frozen) {
					continue
				}
				open, _ := strconv.ParseFloat(openValues[index], 64)
				close, _ := strconv.ParseFloat(closeValues[index], 64)
				bars = append(bars, ymodels.Bar{Date: stamp, Open: open, Close: close})
			}
			yahoo := NewYahoo(time.Second, 1)
			setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
				quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 140.11, RegularMarketOpen: 136, RegularMarketPreviousClose: 138.91, MarketState: "REGULAR"},
				bars:  map[string][]ymodels.Bar{"1m": bars},
			})
			yahoo.now = func() time.Time { return frozen }
			quote, err := yahoo.Quote(context.Background(), "TEST")
			if err != nil {
				t.Fatalf("Quote() error=%v", err)
			}
			if quote.MarketState != test.state || quote.Price == nil || *quote.Price != test.wantPrice || quote.PreviousClose == nil || *quote.PreviousClose != test.wantPrevious || quote.TodayOpen == nil || *quote.TodayOpen != test.wantOpen || quote.ChangePercent == nil || math.Abs(*quote.ChangePercent-test.wantChange) > 1e-9 {
				t.Fatalf("quote calibration differs: %#v", quote)
			}
			wantPrePrice := float64(140)
			if test.name != "pre" {
				wantPrePrice = 138.26
			}
			pre, preOK := quote.Sessions["pre"].(map[string]any)
			if !preOK || pre["price"] != wantPrePrice {
				t.Errorf("pre summary=%#v, want price=%v", quote.Sessions["pre"], wantPrePrice)
			}
			if test.name == "post" || test.name == "overnight" {
				if value, ok := quote.Sessions["post"].(map[string]any); !ok || value["price"] != float64(138.91) || value["reference_close"] != float64(137.79) {
					t.Errorf("post summary differs: %#v", quote.Sessions["post"])
				}
			}
			if test.name == "overnight" {
				if value, ok := quote.Sessions["overnight"].(map[string]any); !ok || value["price"] != float64(139.1) || value["reference_close"] != float64(137.79) {
					t.Errorf("overnight summary differs: %#v", quote.Sessions["overnight"])
				}
			}
			if preOK && (pre["reference_close"] != float64(139.54) || math.Abs(pre["change_percent"].(float64)-(wantPrePrice-139.54)/139.54*100) > 1e-9) {
				t.Errorf("pre session references wrong regular close: %#v", pre)
			}
			if test.name == "post" || test.name == "overnight" {
				if post, ok := quote.Sessions["post"].(map[string]any); !ok || post["reference_close"] != float64(137.79) {
					t.Errorf("post session reference=%#v, want 137.79", quote.Sessions["post"])
				}
			}
		})
	}
}

func TestYahooQuoteSkipsSparseAndNullIntradayValues(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	latest := time.Date(2026, 9, 25, 8, 10, 0, 0, location)
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 101, RegularMarketPreviousClose: 100, MarketState: "PRE"},
		bars: map[string][]ymodels.Bar{"1m": {
			{Date: latest.Add(-2 * time.Minute), Open: 0, Close: 0},
			{Date: latest.Add(-time.Minute), Open: 0, Close: 103},
			{Date: latest, Open: 100, Close: 102},
		}},
	})
	yahoo.now = func() time.Time { return latest }
	quote, err := yahoo.Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	pre, ok := quote.Sessions["pre"].(map[string]any)
	if !ok || pre["price"] != float64(102) || pre["as_of"] != latest.Format(time.RFC3339) {
		t.Fatalf("sparse/null bars not handled like Python: %#v", quote.Sessions)
	}
	if quote.MarketState != "PRE" || quote.Price == nil || *quote.Price != 101 {
		t.Fatalf("meta price/state fallback changed on sparse bars: %#v", quote)
	}
}

func TestYahooChainRejectsMissingOrEmptyOptionsAndEmptySides(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	cases := []struct {
		name string
		body string
	}{
		{
			name: "missing options",
			body: fmt.Sprintf(`{"optionChain":{"result":[{"expirationDates":[%d]}],"error":null}}`, stamp),
		},
		{
			name: "empty options",
			body: fmt.Sprintf(`{"optionChain":{"result":[{"expirationDates":[%d],"options":[]}],"error":null}}`, stamp),
		},
		{
			name: "empty calls and puts",
			body: fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":[],"puts":[]}]}],"error":null}}`, stamp),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer upstream.Close()

			yahoo := NewYahoo(time.Second, 1)
			yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
			useLocalYahooOptionsAuth(t, yahoo)
			setYahooSDKTestTickerFromOptionJSON(t, yahoo, test.body)
			rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", expiration.Format("2006-01-02"))
			if err == nil || !strings.Contains(err.Error(), "没有期权数据") {
				t.Fatalf("Chain() = (%#v, %v, %v), want empty-chain provider error", rows, fetchedAt, err)
			}
			if fetchedAt != nil {
				t.Fatalf("Chain() fetchedAt = %v on empty chain, want nil", fetchedAt)
			}
		})
	}
}

func TestYahooChainPreservesNullableOptionNumericFields(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	calls := `[
		{"contractSymbol":"MISSING"},
		{"contractSymbol":"NULL","strike":null,"lastPrice":null,"bid":null,"ask":null,"percentChange":null,"impliedVolatility":null,"volume":null,"openInterest":null,"inTheMoney":null},
		{"contractSymbol":"ZERO","strike":0,"lastPrice":0,"bid":0,"ask":0,"percentChange":0,"impliedVolatility":0,"volume":0,"openInterest":0,"inTheMoney":false}
	]`
	body := fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":%s,"puts":[]}]}],"error":null}}`, stamp, calls)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	yahoo := NewYahoo(time.Second, 1)
	yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
	useLocalYahooOptionsAuth(t, yahoo)
	setYahooSDKTestTickerFromOptionJSON(t, yahoo, body)
	rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", expiration.Format("2006-01-02"))
	if err != nil || fetchedAt == nil || len(rows) != 3 {
		t.Fatalf("Chain() = (%#v, %v, %v), want three single-sided contracts", rows, fetchedAt, err)
	}
	for _, index := range []int{0, 1} {
		row := rows[index]
		if row.InTheMoneyKnown || row.InTheMoney {
			t.Errorf("contract %q in-the-money=%v known=%v, want unknown", row.ContractSymbol, row.InTheMoney, row.InTheMoneyKnown)
		}
		fields := map[string]*float64{
			"strike": row.Strike, "last_price": row.LastPrice, "bid": row.Bid, "ask": row.Ask,
			"percent_change": row.ChangePercent, "implied_volatility": row.ImpliedVolatility,
		}
		for name, value := range fields {
			if value != nil {
				t.Errorf("contract %q %s = %v, want nil for omitted/null upstream value", row.ContractSymbol, name, *value)
			}
		}
		if row.Volume != nil || row.OpenInterest != nil {
			t.Errorf("contract %q integer optionals volume=%v openInterest=%v, want nil", row.ContractSymbol, row.Volume, row.OpenInterest)
		}
	}
	zero := rows[2]
	if !zero.InTheMoneyKnown || zero.InTheMoney {
		t.Errorf("explicit false in-the-money value was lost: %#v", zero)
	}
	floatFields := map[string]*float64{
		"strike": zero.Strike, "last_price": zero.LastPrice, "bid": zero.Bid, "ask": zero.Ask,
		"percent_change": zero.ChangePercent, "implied_volatility": zero.ImpliedVolatility,
	}
	for name, value := range floatFields {
		if value == nil || *value != 0 {
			t.Errorf("explicit zero %s = %v, want nonnil pointer to 0", name, value)
		}
	}
	if zero.Volume == nil || *zero.Volume != 0 || zero.OpenInterest == nil || *zero.OpenInterest != 0 {
		t.Errorf("explicit zero integer fields volume=%v openInterest=%v, want nonnil pointers to 0", zero.Volume, zero.OpenInterest)
	}
	if rows[0].ContractType != "call" || rows[1].ContractType != "call" || zero.ContractType != "call" {
		t.Fatalf("single-sided calls were not retained: %#v", rows)
	}
}

func TestYahooChainMatchesRawExtensionsByContractSymbol(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	body := fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":[{"contractSymbol":"B","bid":22,"gamma":0.02},{"contractSymbol":"A","bid":11,"gamma":0.01}],"puts":[]}]}],"error":null}}`, stamp)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	yahoo := NewYahoo(time.Second, 1)
	yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
	useLocalYahooOptionsAuth(t, yahoo)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{optionChain: &ymodels.OptionChain{
		Expiration: expiration,
		Calls:      []ymodels.Option{{ContractSymbol: "A", Bid: 11}, {ContractSymbol: "B", Bid: 22}},
	}})
	rows, _, err := yahoo.Chain(context.Background(), "TEST", expiration.Format("2006-01-02"))
	if err != nil || len(rows) != 2 {
		t.Fatalf("Chain() = %#v, %v; want two calls", rows, err)
	}
	if rows[0].ContractSymbol != "A" || rows[0].Bid == nil || *rows[0].Bid != 11 || rows[0].Gamma == nil || *rows[0].Gamma != 0.01 {
		t.Errorf("first SDK contract received mismatched raw fields: %#v", rows[0])
	}
	if rows[1].ContractSymbol != "B" || rows[1].Bid == nil || *rows[1].Bid != 22 || rows[1].Gamma == nil || *rows[1].Gamma != 0.02 {
		t.Errorf("second SDK contract received mismatched raw fields: %#v", rows[1])
	}
}

func TestYahooChainAcceptsEitherNonEmptySide(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	cases := []struct {
		name        string
		calls, puts string
		wantType    string
	}{
		{
			name:     "calls only",
			calls:    `[{"contractSymbol":"TEST300118C00100000","strike":100,"lastPrice":12,"bid":11,"ask":13}]`,
			puts:     `[]`,
			wantType: "call",
		},
		{
			name:     "puts only",
			calls:    `[]`,
			puts:     `[{"contractSymbol":"TEST300118P00100000","strike":100,"lastPrice":2,"bid":1,"ask":3}]`,
			wantType: "put",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":%s,"puts":%s}]}],"error":null}}`, stamp, test.calls, test.puts)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer upstream.Close()

			yahoo := NewYahoo(time.Second, 1)
			yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
			useLocalYahooOptionsAuth(t, yahoo)
			setYahooSDKTestTickerFromOptionJSON(t, yahoo, body)
			rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", expiration.Format("2006-01-02"))
			if err != nil || fetchedAt == nil || len(rows) != 1 || rows[0].ContractType != test.wantType {
				t.Fatalf("Chain() = (%#v, %v, %v), want one %s contract and success", rows, fetchedAt, err, test.wantType)
			}
		})
	}
}

func TestYahooChainRejectsMismatchedReturnedExpiration(t *testing.T) {
	requested := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	returned := time.Date(2030, time.January, 25, 0, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":[],"puts":[]}]}],"error":null}}`, returned.Unix())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	yahoo := NewYahoo(time.Second, 1)
	yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
	useLocalYahooOptionsAuth(t, yahoo)
	setYahooSDKTestTickerFromOptionJSON(t, yahoo, body)
	_, _, err := yahoo.Chain(context.Background(), "TEST", requested.Format("2006-01-02"))
	if err == nil || !strings.Contains(err.Error(), "returned expiration 2030-01-25") {
		t.Fatalf("Chain() error = %v, want mismatched-expiration error", err)
	}
}

func TestYahooSDKCancellationRetainsGateUntilRequestFinishes(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseSDK := make(chan struct{})
	closed := make(chan struct{})
	yahoo := NewYahoo(time.Minute, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", RegularMarketPrice: 10, RegularMarketPreviousClose: 9, MarketState: "REGULAR"},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval == "1m" {
				started <- struct{}{}
				<-releaseSDK
			}
			return nil, nil
		},
		closed: func() { close(closed) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := yahoo.Quote(ctx, "TEST")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Quote() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Quote() did not return after cancellation")
	}
	if len(yahoo.semaphore) != 1 {
		t.Fatal("SDK operation released the shared gate after caller cancellation")
	}
	close(releaseSDK)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SDK ticker was not closed after the request finished")
	}
	deadline := time.Now().Add(time.Second)
	for len(yahoo.semaphore) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(yahoo.semaphore) != 0 {
		t.Fatal("SDK operation did not release its gate after completion")
	}
}

func TestYahooPropagatesProviderErrors(t *testing.T) {
	for _, name := range []string{"quote error", "expiration error is not empty catalog", "chain error"} {
		t.Run(name, func(t *testing.T) {
			yahoo := NewYahoo(time.Second, 1)
			var err error
			switch name {
			case "quote error":
				setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
					quoteErr:   errors.New("Quote not found"),
					historyErr: map[string]error{"1m": errors.New("history unavailable"), "1d": errors.New("history unavailable")},
				})
				_, err = yahoo.Quote(context.Background(), "TEST")
			case "expiration error is not empty catalog":
				setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{optionsErr: errors.New("Invalid cookie")})
				_, err = yahoo.Expirations(context.Background(), "TEST")
			case "chain error":
				setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{chainErr: errors.New("API error: Bad Request - Invalid expiration")})
				_, _, err = yahoo.Chain(context.Background(), "TEST", "2030-01-18")
			}
			wantMessage := map[string]string{
				"quote error": "Quote not found", "expiration error is not empty catalog": "Invalid cookie", "chain error": "Invalid expiration",
			}[name]
			if err == nil || !strings.Contains(err.Error(), wantMessage) {
				t.Fatalf("provider error = %v, want containing %q", err, wantMessage)
			}
		})
	}
}

func TestYahooRequestHonorsContextCancellationWhileWaitingForSlot(t *testing.T) {
	yahoo := NewYahoo(time.Second, 1)
	yahoo.semaphore <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := yahoo.Quote(ctx, "TEST")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Quote() error = %v, want context.Canceled", err)
	}
}

func TestNewYahooWithProxyAcceptsSupportedSchemes(t *testing.T) {
	for _, value := range []string{
		"http://proxy.example:8080",
		"https://proxy.example:8443",
		"socks5://proxy.example:1080",
		"socks5h://proxy.example:1080",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := NewYahooWithProxy(time.Second, 1, value); err != nil {
				t.Fatalf("NewYahooWithProxy(%q) error = %v", value, err)
			}
		})
	}
}

func TestNewYahooWithProxyRejectsInvalidURL(t *testing.T) {
	for _, value := range []string{"not-a-url", "ftp://proxy.example:21", "http:///missing-host"} {
		if _, err := NewYahooWithProxy(time.Second, 1, value); err == nil {
			t.Errorf("NewYahooWithProxy(%q) expected an error", value)
		}
	}
}
