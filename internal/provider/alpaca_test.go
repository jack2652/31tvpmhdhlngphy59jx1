package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestNewAlpacaUsesBoundedDefaultClient(t *testing.T) {
	client := NewAlpaca("key", "secret", "", nil)
	if client.client == nil {
		t.Fatal("default HTTP client is nil")
	}
	if client.client.Timeout != defaultAlpacaRequestTimeout {
		t.Fatalf("default HTTP client timeout = %s, want %s", client.client.Timeout, defaultAlpacaRequestTimeout)
	}
}

func TestAlpacaQuoteRequestsOvernightSnapshotAndMatchesSessionContract(t *testing.T) {
	const key, secret = "test-key-secret", "test-api-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v2/stocks/snapshots" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbols"); got != "BRK.B" {
			t.Errorf("symbols query = %q, want BRK.B", got)
		}
		if got := r.URL.Query().Get("feed"); got != "overnight" {
			t.Errorf("feed query = %q, want overnight", got)
		}
		if got := r.Header.Get("APCA-API-KEY-ID"); got != key {
			t.Errorf("key header = %q", got)
		}
		if got := r.Header.Get("APCA-API-SECRET-KEY"); got != secret {
			t.Errorf("secret header = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"BRK.B": {
				"latestTrade": {"p": 412.5, "t": "2025-03-04T02:15:00Z"},
				"latestQuote": {"t": "2025-03-04T02:14:59Z"},
				"prevDailyBar": {"c": 410.0},
				"dailyBar": {"o": 411.0, "v": 900},
				"minuteBar": {"t":"2025-03-04T02:15:00Z", "o":412, "h":413, "l":411, "c":412.5, "v":12},
				"overnightBar": {"t":"2025-03-04T02:15:00Z", "o":412, "h":413, "l":411, "c":412.5, "v":120}
			}
		}`)
	}))
	defer server.Close()

	client := NewAlpaca(key, secret, server.URL, server.Client())
	quote, err := client.Quote(context.Background(), " brk.b ")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if quote.Symbol != "BRK.B" || quote.Price == nil || *quote.Price != 412.5 {
		t.Fatalf("unexpected symbol or price: %#v", quote)
	}
	if quote.PreviousClose == nil || *quote.PreviousClose != 410 {
		t.Errorf("previous close = %v, want 410", quote.PreviousClose)
	}
	if quote.TodayOpen == nil || *quote.TodayOpen != 411 {
		t.Errorf("today open = %v, want 411", quote.TodayOpen)
	}
	if quote.Currency != "USD" || quote.MarketState != "OVERNIGHT" || quote.Provider != "alpaca-overnight" {
		t.Errorf("unexpected quote metadata: %#v", quote)
	}
	if quote.FetchedAt == nil || quote.Sessions == nil {
		t.Errorf("missing fetched time or sessions: %#v", quote)
	}
	session, ok := quote.Sessions["overnight"].(map[string]any)
	if !ok {
		t.Fatalf("overnight session has unexpected type: %#v", quote.Sessions["overnight"])
	}
	if session["price_source"] != "trade" || session["delayed"] != true || session["volume"] != float64(900) {
		t.Errorf("unexpected overnight session: %#v", session)
	}
	if session["as_of"] != "2025-03-04T02:14:59+00:00" {
		t.Errorf("session as_of = %#v", session["as_of"])
	}
	if session["bar_volume"] != float64(12) {
		t.Errorf("session bar_volume = %#v, want minuteBar volume 12", session["bar_volume"])
	}
	if len(session) != 10 {
		t.Fatalf("overnight session has %d fields, want the 10-field Python contract: %#v", len(session), session)
	}
	for _, field := range []string{"minute_bar", "overnight_bar"} {
		if _, ok := session[field]; ok {
			t.Errorf("overnight session unexpectedly contains non-Python field %q", field)
		}
	}
}

func TestAlpacaTimestampMatchesPythonISOFormat(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{input: "2025-03-04T02:14:59Z", want: "2025-03-04T02:14:59+00:00"},
		{input: "2025-03-04T02:14:59.123Z", want: "2025-03-04T02:14:59.123000+00:00"},
		{input: "2025-03-04T09:14:59.123456789-07:00", want: "2025-03-04T16:14:59.123456+00:00"},
		{input: "not-a-timestamp", want: "not-a-timestamp"},
	} {
		if got := firstTimestamp(tc.input); got != tc.want {
			t.Errorf("firstTimestamp(%q) = %#v, want %q", tc.input, got, tc.want)
		}
	}
}

func alpacaQuoteForSnapshot(t *testing.T, snapshot string) (*float64, map[string]any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"TEST":{%s}}`, snapshot)
	}))
	defer server.Close()
	quote, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	session, ok := quote.Sessions["overnight"].(map[string]any)
	if !ok {
		t.Fatalf("overnight session has unexpected type: %#v", quote.Sessions["overnight"])
	}
	return quote.Price, session
}

func TestAlpacaQuoteUsesIndicativeQuoteBeforeTrade(t *testing.T) {
	price, session := alpacaQuoteForSnapshot(t, `"latestQuote":{"bp":100,"ap":102},"latestTrade":{"p":104}`)
	if price == nil || *price != 101 || session["price_source"] != "indicative_quote" {
		t.Fatalf("quote midpoint should beat trade: price=%v session=%#v", price, session)
	}
}

func TestAlpacaQuoteUsesSingleSidedQuoteBeforeTrade(t *testing.T) {
	for _, test := range []struct {
		name, quoteJSON string
		want            float64
	}{
		{name: "bid only", quoteJSON: `"bp":100`, want: 100},
		{name: "ask only", quoteJSON: `"ap":102`, want: 102},
	} {
		t.Run(test.name, func(t *testing.T) {
			price, session := alpacaQuoteForSnapshot(t, `"latestQuote":{`+test.quoteJSON+`},"latestTrade":{"p":104}`)
			if price == nil || *price != test.want || session["price_source"] != "indicative_quote" {
				t.Fatalf("single-sided quote did not take priority: price=%v session=%#v", price, session)
			}
		})
	}
}

func TestAlpacaQuoteCrossedQuotePrefersBidBeforeTrade(t *testing.T) {
	price, session := alpacaQuoteForSnapshot(t, `"latestQuote":{"bp":103,"ap":102},"latestTrade":{"p":104}`)
	if price == nil || *price != 103 || session["price_source"] != "indicative_quote" {
		t.Fatalf("crossed quote should fall back to bid before trade: price=%v session=%#v", price, session)
	}
}

func TestAlpacaQuoteFallbackUsesTradeThenMinuteBarNotOvernightBar(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot, source string
		price                  float64
	}{
		{name: "trade before bars", snapshot: `"latestTrade":{"p":20},"minuteBar":{"c":19.25},"overnightBar":{"c":22.5}`, source: "trade", price: 20},
		{name: "minute bar ignores overnight bar", snapshot: `"minuteBar":{"c":19.25},"overnightBar":{"c":22.5}`, source: "minute_bar", price: 19.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"TEST":{%s}}`, tc.snapshot)
			}))
			defer server.Close()
			quote, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
			if err != nil {
				t.Fatalf("Quote() error = %v", err)
			}
			session := quote.Sessions["overnight"].(map[string]any)
			if quote.Price == nil || *quote.Price != tc.price || session["price_source"] != tc.source {
				t.Fatalf("unexpected fallback result: quote=%#v session=%#v", quote, session)
			}
		})
	}
}

func TestAlpacaQuoteRejectsMissingOrInvalidPrice(t *testing.T) {
	for _, value := range []string{"0", "-3", "1e9999", `"invalid"`, "null"} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"TEST":{"latestTrade":{"p":%s}}}`, value)
			}))
			defer server.Close()
			_, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
			if err == nil || !strings.Contains(err.Error(), "finite positive") {
				t.Fatalf("Quote() error = %v, want invalid-price error", err)
			}
		})
	}
}

func TestAlpacaErrorsDoNotExposeCredentialsOrResponseBody(t *testing.T) {
	const key, secret = "very-private-key", "very-private-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "credentials %s %s", key, secret)
	}))
	defer server.Close()
	_, err := NewAlpaca(key, secret, server.URL, server.Client()).Quote(context.Background(), "TEST")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("Quote() error = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed credentials: %v", err)
	}
}

func TestAlpacaAuthenticationCooldownAndTransientServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway} {
		name := fmt.Sprintf("HTTP %d", status)
		t.Run(name, func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "sensitive response body")
			}))
			defer server.Close()

			client := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
			now := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
			client.now = func() time.Time { return now }

			_, firstErr := client.Quote(context.Background(), "TEST")
			if firstErr == nil || strings.Contains(firstErr.Error(), "sensitive response body") || strings.Contains(firstErr.Error(), "test-key") || strings.Contains(firstErr.Error(), "test-secret") {
				t.Fatalf("first Quote() error must be sanitized: %v", firstErr)
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				if client.CanRequest() {
					t.Fatal("credentials remain requestable during auth cooldown")
				}
				if _, err := client.Quote(context.Background(), "TEST"); err == nil || !strings.Contains(err.Error(), "cooldown") {
					t.Fatalf("Quote() during cooldown error=%v, want cooldown", err)
				}
				if requests != 1 {
					t.Fatalf("requests during cooldown=%d, want exactly 1", requests)
				}
				now = now.Add(AlpacaAuthCooldownDuration - time.Second)
				if client.CanRequest() {
					t.Fatal("credentials became requestable before cooldown elapsed")
				}
				now = now.Add(time.Second)
				if !client.CanRequest() {
					t.Fatal("credentials did not become requestable after cooldown elapsed")
				}
				_, _ = client.Quote(context.Background(), "TEST")
				if requests != 2 || client.CanRequest() {
					t.Fatalf("post-cooldown auth retry requests=%d requestable=%v, want 2 and cooldown reset", requests, client.CanRequest())
				}
			} else {
				if !client.CanRequest() {
					t.Fatal("HTTP 5xx must not invalidate credentials")
				}
				_, _ = client.Quote(context.Background(), "TEST")
				if requests != 2 {
					t.Fatalf("HTTP 5xx should remain retryable; requests=%d, want 2", requests)
				}
			}
		})
	}
}

func TestAlpacaValidateCredentialsReportsOptionalStartupStatus(t *testing.T) {
	for _, test := range []struct {
		name, key, secret string
		status            int
		body              string
		want              string
		wantRequests      int
	}{
		{name: "disabled", want: "disabled"},
		{name: "incomplete", key: "test-key", want: "invalid_config"},
		{name: "valid", key: "test-key", secret: "test-secret", status: http.StatusOK, body: `{"SPY":{"latestTrade":{"p":500}}}`, want: "valid", wantRequests: 1},
		{name: "valid without current SPY snapshot", key: "test-key", secret: "test-secret", status: http.StatusOK, body: `{}`, want: "valid", wantRequests: 1},
		{name: "invalid credentials", key: "test-key", secret: "test-secret", status: http.StatusUnauthorized, want: "invalid_credentials", wantRequests: 1},
		{name: "provider unavailable", key: "test-key", secret: "test-secret", status: http.StatusBadGateway, want: "unavailable", wantRequests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Query().Get("symbols") != "SPY" || r.URL.Query().Get("feed") != "overnight" {
					t.Errorf("preflight query=%q, want SPY overnight snapshot", r.URL.RawQuery)
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				if test.body != "" {
					_, _ = fmt.Fprint(w, test.body)
				}
			}))
			defer server.Close()

			client := NewAlpaca(test.key, test.secret, server.URL, server.Client())
			if got := client.ValidateCredentials(context.Background()); got != test.want {
				t.Fatalf("ValidateCredentials()=%q, want %q", got, test.want)
			}
			if requests != test.wantRequests {
				t.Fatalf("preflight requests=%d, want %d", requests, test.wantRequests)
			}
		})
	}
}

func TestHybridSkipsAlpacaDuringAuthenticationCooldown(t *testing.T) {
	var alpacaRequests int
	alpacaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alpacaRequests++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, "invalid credentials")
	}))
	defer alpacaServer.Close()

	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 100, RegularMarketPreviousClose: 99, MarketState: "REGULAR"},
	})
	alpaca := NewAlpaca("key", "secret", alpacaServer.URL, alpacaServer.Client())
	fixed := time.Date(2026, time.October, 7, 20, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	alpaca.now = func() time.Time { return fixed }
	hybrid := NewHybrid(yahoo, nil, alpaca)
	hybrid.now = func() time.Time { return fixed }

	for i := 0; i < 2; i++ {
		quote, err := hybrid.Quote(context.Background(), "TEST")
		if err != nil || quote.Price == nil || *quote.Price != 100 {
			t.Fatalf("Hybrid.Quote() iteration %d quote=%#v err=%v; base quote should remain available", i+1, quote, err)
		}
	}
	if alpacaRequests != 1 {
		t.Fatalf("Alpaca requests=%d, want one request before hybrid honors cooldown", alpacaRequests)
	}
}

func TestAlpacaQuoteHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewAlpaca("key", "secret", server.URL, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Quote(ctx, "TEST")
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
}

func TestAlpacaQuoteNeedsCredentials(t *testing.T) {
	client := NewAlpaca("key-only", "", "http://127.0.0.1:1", nil)
	_, err := client.Quote(context.Background(), "TEST")
	if err == nil || strings.Contains(err.Error(), "key-only") {
		t.Fatalf("Quote() error = %v, want sanitized missing-credential error", err)
	}
}
