package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCboeOptionsClientExpirationsAndChain(t *testing.T) {
	const expiration = "2030-01-18"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/options/QQQ" {
			t.Errorf("path = %q, want /options/QQQ", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"current_price":450.5,"options":[
			{"option":"QQQ300118C00450000","last_trade_price":12.5,"bid":12,"ask":13,"volume":100,"open_interest":250,"iv":0.32,"gamma":0.04,"percent_change":5.5},
			{"option":"QQQ300118P00450000","last_trade_price":"11.5","bid":"11","ask":"12","volume":"9","open_interest":"50","iv":"0.4","gamma":null,"percent_change":"-3"},
			{"option":"QQQ300215C00450000","last_trade_price":1,"bid":1,"ask":2,"volume":1,"open_interest":1,"iv":0.3},
			{"option":"QQQ  300118C00450000","last_trade_price":1,"bid":1,"ask":2,"volume":1,"open_interest":1,"iv":0.3},
			{"option":"QQQ300118C00400000","last_trade_price":1,"bid":1,"ask":2,"volume":1,"open_interest":1,"iv":0.3},
			{"option":"IWM300118C00200000","last_trade_price":1,"bid":1,"ask":2,"volume":1,"open_interest":1,"iv":0.3}
		]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(http.DefaultClient, server.URL+"/options/{symbol}")
	expirations, err := client.Expirations(context.Background(), " qqq ")
	if err != nil {
		t.Fatalf("Expirations() error = %v", err)
	}
	if want := []string{"2030-01-18", "2030-02-15"}; !reflect.DeepEqual(expirations, want) {
		t.Fatalf("Expirations() = %#v, want %#v", expirations, want)
	}

	rows, fetched, err := client.Chain(context.Background(), "QQQ", expiration)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if fetched == nil || time.Since(*fetched) > time.Minute {
		t.Fatalf("fetched_at = %v, want recent timestamp", fetched)
	}
	if len(rows) != 3 {
		t.Fatalf("Chain() returned %d rows, want 3 (embedded OCC root padding must not be stripped)", len(rows))
	}
	call := rows[0]
	if call.ContractSymbol != "QQQ300118C00450000" || call.Provider != "cboe-delayed" || call.ContractType != "call" || call.Expiration != expiration || call.Strike == nil || *call.Strike != 450 || !call.InTheMoney {
		t.Errorf("call mapping = %#v", call)
	}
	if call.LastPrice == nil || *call.LastPrice != 12.5 || call.Bid == nil || *call.Bid != 12 || call.Ask == nil || *call.Ask != 13 || call.Volume == nil || *call.Volume != 100 || call.OpenInterest == nil || *call.OpenInterest != 250 || call.ImpliedVolatility == nil || *call.ImpliedVolatility != 0.32 || call.Gamma == nil || *call.Gamma != 0.04 || call.ChangePercent == nil || *call.ChangePercent != 5.5 {
		t.Errorf("call numeric fields = %#v", call)
	}
	put := rows[1]
	if put.ContractType != "put" || put.Strike == nil || *put.Strike != 450 || put.InTheMoney || put.LastPrice == nil || *put.LastPrice != 11.5 || put.Volume == nil || *put.Volume != 9 || put.Gamma != nil || put.ChangePercent == nil || *put.ChangePercent != -3 {
		t.Errorf("put mapping = %#v", put)
	}
}

func TestCboeQuotePreservesMissingOrZeroUnderlyingPrice(t *testing.T) {
	for _, test := range []struct {
		name       string
		current    string
		change     string
		wantPrice  *float64
		wantChange float64
	}{
		{name: "missing price with upstream change", current: "null", change: "1.5", wantChange: 1.5},
		{name: "zero price derives change", current: "0", change: "null", wantPrice: floatPointer(0), wantChange: -100},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"data":{"current_price":%s,"prev_day_close":100,"price_change_percent":%s,"options":[]}}`, test.current, test.change)
			}))
			defer server.Close()

			quote, err := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}").Quote(context.Background(), "QQQ")
			if err != nil {
				t.Fatalf("Quote() error = %v", err)
			}
			if test.wantPrice == nil {
				if quote.Price != nil {
					t.Fatalf("Price = %v, want nil", *quote.Price)
				}
			} else if quote.Price == nil || *quote.Price != *test.wantPrice {
				t.Fatalf("Price = %v, want %v", quote.Price, *test.wantPrice)
			}
			if quote.ChangePercent == nil || *quote.ChangePercent != test.wantChange {
				t.Fatalf("ChangePercent = %v, want %v", quote.ChangePercent, test.wantChange)
			}
		})
	}
}

func TestCboeQuoteDefaultsMatchProviderContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	quote, err := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}").Quote(context.Background(), "QQQ")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if quote.Price != nil || quote.ChangePercent != nil || quote.PreviousClose != nil || quote.TodayOpen != nil {
		t.Fatalf("missing upstream quote fields should remain null: %#v", quote)
	}
	if quote.Symbol != "QQQ" || quote.Currency != "USD" || quote.Provider != "cboe-delayed" || quote.Source != "upstream" || quote.Sessions == nil || len(quote.Sessions) != 0 {
		t.Fatalf("unexpected quote defaults: %#v", quote)
	}
}

func TestParseOCCOptionNormalizesAndRejectsMalformed(t *testing.T) {
	got, err := ParseOCCOption(" brk.b  300118p00300000 ")
	if err != nil {
		t.Fatalf("ParseOCCOption() error = %v", err)
	}
	if got.Root != "BRK.B  " || got.Expiration != "2030-01-18" || got.ContractType != "put" || got.Strike != 300 {
		t.Fatalf("ParseOCCOption() = %#v", got)
	}
	zeroStrike, err := ParseOCCOption("QQQ300118C00000000")
	if err != nil || zeroStrike.Strike != 0 {
		t.Fatalf("ParseOCCOption() zero strike = %#v, %v; Python accepts zero strikes", zeroStrike, err)
	}
	for _, value := range []string{"", "QQQ300231C00450000", "QQQ300118X00450000", "300118C00450000"} {
		if _, err := ParseOCCOption(value); err == nil {
			t.Errorf("ParseOCCOption(%q) error = nil, want malformed error", value)
		}
	}
}

func TestCboeExpiredOCCDatesRemainAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"options":[{"option":"QQQ000118C00100000"}]}}`))
	}))
	defer server.Close()
	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")

	parsed, err := ParseOCCOption("QQQ000118C00100000")
	if err != nil || parsed.Expiration != "2000-01-18" {
		t.Fatalf("ParseOCCOption() = %#v, %v; want 2000-01-18", parsed, err)
	}
	expirations, err := client.Expirations(context.Background(), "QQQ")
	if err != nil || !reflect.DeepEqual(expirations, []string{"2000-01-18"}) {
		t.Fatalf("Expirations() = %#v, %v; historical OCC dates should not be filtered", expirations, err)
	}
	rows, _, err := client.Chain(context.Background(), "QQQ", "2000-01-18")
	if err != nil || len(rows) != 1 || rows[0].Expiration != "2000-01-18" {
		t.Fatalf("Chain() = %#v, %v; historical contract should remain parseable", rows, err)
	}
}

func TestNormalizeCboeSymbolMatchesPythonProviderBoundaries(t *testing.T) {
	for _, symbol := range []string{"QQQ", "BRK.B", "ABCDEFGHIJ"} {
		got, err := normalizeCboeSymbol(" " + strings.ToLower(symbol) + " ")
		if err != nil || got != symbol {
			t.Errorf("normalizeCboeSymbol(%q) = %q, %v; want %q", symbol, got, err, symbol)
		}
	}
	for _, symbol := range []string{"", "^GSPC", "ABCDEFGHIJK", "-QQQ", ".QQQ"} {
		if _, err := normalizeCboeSymbol(symbol); err == nil {
			t.Errorf("normalizeCboeSymbol(%q) error = nil, want invalid-symbol error", symbol)
		}
	}
}

func TestCboeOptionsClientClampsNegativeVolumeAndOpenInterest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"current_price":100,"options":[{"option":"QQQ300118C00100000","volume":-3.5,"open_interest":"-2"}]}}`))
	}))
	defer server.Close()

	rows, _, err := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}").Chain(context.Background(), "QQQ", "2030-01-18")
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Volume == nil || *rows[0].Volume != 0 || rows[0].OpenInterest == nil || *rows[0].OpenInterest != 0 {
		t.Fatalf("negative counts should clamp to zero like Python: %#v", rows)
	}
}

func TestCboeOptionsClientSaturatesCountsOutsideInt64Range(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"current_price":100,"options":[{"option":"QQQ300118C00100000","volume":1e30,"open_interest":"9e30"}]}}`))
	}))
	defer server.Close()

	rows, _, err := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}").Chain(context.Background(), "QQQ", "2030-01-18")
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Volume == nil || *rows[0].Volume != int64(^uint64(0)>>1) || rows[0].OpenInterest == nil || *rows[0].OpenInterest != int64(^uint64(0)>>1) {
		t.Fatalf("oversized counts should stay non-negative and saturate at int64 max: %#v", rows)
	}
}

func TestCboeChainLeavesInTheMoneyUnknownWithoutSpot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"options":[{"option":"QQQ300118C00100000"}]}}`))
	}))
	defer server.Close()

	rows, _, err := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}").Chain(context.Background(), "QQQ", "2030-01-18")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].InTheMoneyKnown || rows[0].InTheMoney {
		t.Fatalf("missing spot should leave in-the-money unknown: %#v", rows)
	}
}

func TestCboeHTTPErrorIncludesAtMost500ResponseCharacters(t *testing.T) {
	body := strings.Repeat("x", 520)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	_, _, err := client.fetch(context.Background(), "QQQ")
	if err == nil {
		t.Fatal("fetch() error = nil, want HTTP error")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 500)) || strings.Contains(err.Error(), strings.Repeat("x", 501)) {
		t.Fatalf("HTTP error detail should include at most 500 response characters: %q", err)
	}
}

func TestCboeOptionsClientMalformedHTTPAndMissingExpiry(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
		expiration string
	}{
		{name: "http", statusCode: http.StatusBadGateway, body: `{"error":"upstream"}`, wantErr: "HTTP 502"},
		{name: "malformed-json", statusCode: http.StatusOK, body: `{bad`, wantErr: "decode Cboe"},
		{name: "invalid-utf8", statusCode: http.StatusOK, body: "{\"data\":{\"options\":[],\"extra\":\"\xff\"}}", wantErr: "invalid UTF-8"},
		{name: "trailing-garbage", statusCode: http.StatusOK, body: `{"data":{"options":[]}} trailing`, wantErr: "decode Cboe"},
		{name: "second-json-value", statusCode: http.StatusOK, body: `{"data":{"options":[]}} {"data":{"options":[]}}`, wantErr: "multiple JSON values"},
		{name: "missing-data", statusCode: http.StatusOK, body: `{"data":{}}`, wantErr: "missing data.options"},
		{name: "missing-expiry", statusCode: http.StatusOK, body: `{"data":{"options":[{"option":"QQQ300118C00450000"}]}}`, wantErr: "no option data", expiration: "2030-01-19"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := NewCboeOptionsClient(server.Client(), server.URL+"/options/{symbol}")
			expiration := test.expiration
			if expiration == "" {
				expiration = "2030-01-18"
			}
			_, _, err := client.Chain(context.Background(), "QQQ", expiration)
			if err == nil || !contains(err.Error(), test.wantErr) {
				t.Fatalf("Chain() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestCboeOptionsClientExpirationsRejectsOnlyMalformedContracts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"options":[{"option":"not-an-occ-code"}]}}`))
	}))
	defer server.Close()
	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	if _, err := client.Expirations(context.Background(), "QQQ"); err == nil {
		t.Fatal("Expirations() error = nil, want no valid expiration error")
	}
}

func TestCboeOptionsClientSkipsNonObjectAndInvalidOptionRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"options":[null,"not-an-object",17,{"option":17},{"option":"QQQ300118C00450000","bid":"1.25"}]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	expirations, err := client.Expirations(context.Background(), "QQQ")
	if err != nil || !reflect.DeepEqual(expirations, []string{"2030-01-18"}) {
		t.Fatalf("Expirations() = %#v, %v; expected the valid contract despite malformed rows", expirations, err)
	}
	rows, _, err := client.Chain(context.Background(), "QQQ", "2030-01-18")
	if err != nil || len(rows) != 1 || rows[0].Bid == nil || *rows[0].Bid != 1.25 {
		t.Fatalf("Chain() = %#v, %v; expected only the valid row", rows, err)
	}
}

func TestCboeMarketStateAccountsForEarlyClose(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		moment string
		want   string
	}{
		{moment: "2024-11-29T12:59:00-05:00", want: "REGULAR"},
		{moment: "2024-11-29T13:00:00-05:00", want: "POST"},
		{moment: "2024-11-29T19:59:00-05:00", want: "POST"},
		{moment: "2024-11-29T20:00:00-05:00", want: "CLOSED"},
	} {
		moment, parseErr := time.Parse(time.RFC3339, test.moment)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if got := cboeMarketState(moment.In(location)); got != test.want {
			t.Errorf("cboeMarketState(%s) = %q, want %q", test.moment, got, test.want)
		}
	}
}

func TestCboeSingleflightCoalescesRequestsWhenCacheDisabled(t *testing.T) {
	var requests atomic.Int32
	requestStarted := make(chan struct{}, 1)
	allowResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		requestStarted <- struct{}{}
		<-allowResponse
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheTTL = 0
	const workers = 8
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(workers)
	var done sync.WaitGroup
	done.Add(workers)
	errs := make(chan error, workers)
	for range workers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, _, err := client.fetch(context.Background(), "QQQ")
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-requestStarted
	// Keep the leader blocked long enough for concurrent callers to join its flight.
	time.Sleep(25 * time.Millisecond)
	close(allowResponse)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("fetch() error = %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want one shared request", got)
	}
}

func TestCboeCanceledWaiterDoesNotCancelLeader(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheTTL = 0
	leaderResult := make(chan error, 1)
	go func() {
		_, _, err := client.fetch(context.Background(), "QQQ")
		leaderResult <- err
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()
	if _, _, err := client.fetch(waiterCtx, "QQQ"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader request was affected by waiter cancellation: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want one leader request", got)
	}
}

func TestCboeCanceledLeaderAllowsActiveWaiterToRetryOnce(t *testing.T) {
	var requests atomic.Int32
	firstStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(firstStarted)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheTTL = 0
	client.Gate = make(chan struct{}, 1)
	client.GateWait = time.Second
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, _, err := client.fetch(leaderCtx, "QQQ")
		leaderResult <- err
	}()
	<-firstStarted

	waiterStarted := make(chan struct{})
	waiterResult := make(chan error, 1)
	go func() {
		close(waiterStarted)
		_, _, err := client.fetch(context.Background(), "QQQ")
		waiterResult <- err
	}()
	<-waiterStarted
	// Let the waiter enter the existing flight before canceling its owner.
	time.Sleep(10 * time.Millisecond)
	cancelLeader()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled leader error = %v, want context.Canceled", err)
	}
	if err := <-waiterResult; err != nil {
		t.Fatalf("active waiter should retry after leader cancellation: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want one canceled flight and one retry", got)
	}
	if got := len(client.Gate); got != 0 {
		t.Fatalf("shared gate occupancy after completion = %d, want 0", got)
	}
}

func TestCboeGateWaitHonorsTimeoutAndCancellation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheTTL = 0
	client.Gate = make(chan struct{}, 1)
	client.Gate <- struct{}{}
	client.GateWait = 10 * time.Millisecond
	if _, _, err := client.fetch(context.Background(), "QQQ"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch() while shared gate is full error = %v, want deadline exceeded", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests before gate release = %d, want 0", got)
	}

	client.GateWait = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.fetch(ctx, "QQQ"); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch() with canceled context while gate is full error = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests after canceled gate wait = %d, want 0", got)
	}

	<-client.Gate
	if _, _, err := client.fetch(context.Background(), "QQQ"); err != nil {
		t.Fatalf("fetch() after shared gate release: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests after gate release = %d, want 1", got)
	}
}

func TestCboeOptionsClientRejectsInvalidExpiration(t *testing.T) {
	client := NewCboeOptionsClient(http.DefaultClient, "http://127.0.0.1:1/{symbol}")
	if _, _, err := client.Chain(context.Background(), "QQQ", "2030-1-18"); err == nil {
		t.Fatal("Chain() error = nil, want strict expiration error")
	}
}

func TestCboeOptionsClientDefaultCacheTTLAndExpiry(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"current_price":100,"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	if client.CacheTTL != 15*time.Second {
		t.Fatalf("default CacheTTL=%s, want 15s to match Python Cboe cache", client.CacheTTL)
	}
	clock := time.Date(2030, time.January, 2, 15, 0, 0, 0, time.UTC)
	client.clockNow = func() time.Time { return clock }

	if _, _, err := client.fetch(context.Background(), "QQQ"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	clock = clock.Add(14*time.Second + 999*time.Millisecond)
	if _, _, err := client.fetch(context.Background(), "QQQ"); err != nil {
		t.Fatalf("fetch within default TTL: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests within 15s=%d, want cached single request", requests)
	}

	clock = clock.Add(time.Millisecond + time.Nanosecond)
	if _, _, err := client.fetch(context.Background(), "QQQ"); err != nil {
		t.Fatalf("fetch after default TTL: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests after exceeding 15s=%d, want second upstream request", requests)
	}
}

func TestCboeOptionsClientDefaultCacheHasNoSymbolLimit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	for _, symbol := range []string{"QQQ", "SPY", "AAPL"} {
		if _, _, err := client.fetch(context.Background(), symbol); err != nil {
			t.Fatalf("fetch(%s): %v", symbol, err)
		}
	}
	for _, symbol := range []string{"QQQ", "SPY", "AAPL"} {
		if _, _, err := client.fetch(context.Background(), symbol); err != nil {
			t.Fatalf("cached fetch(%s): %v", symbol, err)
		}
	}
	if requests != 3 {
		t.Fatalf("upstream requests=%d, want one request per symbol with default cache", requests)
	}
	if len(client.cache) != 3 {
		t.Fatalf("cached symbol count=%d, want 3", len(client.cache))
	}
}

func TestCboeOptionsClientRespectsLowMemoryCacheLimit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":{"options":[]}}`))
	}))
	defer server.Close()

	client := NewCboeOptionsClient(server.Client(), server.URL+"/{symbol}")
	client.CacheLimit = 1
	for _, symbol := range []string{"QQQ", "SPY"} {
		if _, _, err := client.fetch(context.Background(), symbol); err != nil {
			t.Fatalf("fetch(%s): %v", symbol, err)
		}
	}
	if len(client.cache) != 1 {
		t.Fatalf("cached symbol count=%d, want one in low-memory mode", len(client.cache))
	}
	if _, ok := client.cache["SPY"]; !ok {
		t.Fatalf("cache=%v, want only most recently fetched SPY payload", client.cache)
	}
	if _, ok := client.cache["QQQ"]; ok {
		t.Fatalf("cache retained previous QQQ payload: %v", client.cache)
	}
	if _, _, err := client.fetch(context.Background(), "QQQ"); err != nil {
		t.Fatalf("fetch(QQQ) after eviction: %v", err)
	}
	if requests != 3 {
		t.Fatalf("upstream requests=%d, want 3 after evicted symbol refetch", requests)
	}
}
