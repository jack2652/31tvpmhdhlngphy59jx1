package server

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/config"
	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
	"option-scope-go/internal/valuation"
)

type manualLeaseTicker struct {
	channel chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newManualLeaseTicker() *manualLeaseTicker {
	return &manualLeaseTicker{channel: make(chan time.Time), stopped: make(chan struct{})}
}

func holdSQLiteWriter(t *testing.T, path string) func() {
	t.Helper()
	admin, err := sql.Open("sqlite", path+"?_busy_timeout=30000&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := admin.Conn(context.Background())
	if err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		_ = admin.Close()
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = admin.Close()
		})
	}
}

func (t *manualLeaseTicker) C() <-chan time.Time { return t.channel }
func (t *manualLeaseTicker) Stop()               { t.once.Do(func() { close(t.stopped) }) }

func TestLeaseRenewerJoinsAndCancelsWorkOnOwnershipLoss(t *testing.T) {
	ticker := newManualLeaseTicker()
	var renewCalls atomic.Int32
	ctx, renewer := startLeaseRenewalWithTicker(context.Background(), func() (bool, error) {
		renewCalls.Add(1)
		return false, nil
	}, "test-lease", ticker)
	ticker.channel <- time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel provider context")
	}
	renewer.stop()
	select {
	case <-renewer.done:
	default:
		t.Fatal("renewal goroutine was not joined")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("renewal ticker was not stopped")
	}
	if got := renewCalls.Load(); got != 1 {
		t.Fatalf("renew calls=%d want 1", got)
	}
}

func TestLeaseRenewerStopsWithoutRenewalOnCompletedWork(t *testing.T) {
	ticker := newManualLeaseTicker()
	var renewCalls atomic.Int32
	_, renewer := startLeaseRenewalWithTicker(context.Background(), func() (bool, error) {
		renewCalls.Add(1)
		return true, nil
	}, "test-lease", ticker)
	renewer.stop()
	select {
	case <-renewer.done:
	default:
		t.Fatal("renewal goroutine was not joined")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("renewal ticker was not stopped")
	}
	if got := renewCalls.Load(); got != 0 {
		t.Fatalf("renew calls=%d want 0 after immediate stop", got)
	}
}

func TestSnapshotLeaseWaitHonorsRequestCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Service, context.Context) error
	}{
		{
			name: "quote refresh",
			run: func(service *Service, ctx context.Context) error {
				_, err := service.fetchQuote(ctx, "QQQ")
				return err
			},
		},
		{
			name: "snapshot refresh",
			run: func(service *Service, ctx context.Context) error {
				_, err := service.refreshSnapshot(ctx, "QQQ", "", 0)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lease-cancel.db")
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
			defer service.Close()
			releaseWriter := holdSQLiteWriter(t, path)
			defer releaseWriter()

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- test.run(service, ctx) }()
			time.Sleep(100 * time.Millisecond)
			cancel()

			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("request error=%v, want context cancellation", err)
				}
			case <-time.After(time.Second):
				releaseWriter()
				<-result
				t.Fatal("snapshot lease wait did not honor request cancellation while SQLite was write-locked")
			}
		})
	}
}

func TestQuoteValuationAdapterPassesTrailingEPSForSanityCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-adapter.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	trailing, forward, shares, price := 10.0, 100.0, 1.0, 100.0
	fake := &testMarketProvider{fundamentals: provider.YahooFundamentals{EPS: &trailing, ForwardEPS: &forward, Shares: &shares}}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()
	quote := model.Quote{Symbol: "QQQ", Price: &price}
	service.enrichQuoteValuation(context.Background(), "QQQ", &quote)
	if quote.FairValueForwardEPS == nil || *quote.FairValueForwardEPS != 30 {
		t.Fatalf("valuation adapter forward EPS=%v, want capped 30", quote.FairValueForwardEPS)
	}
	value, ok := quote.FairValue.(float64)
	if !ok || value < 149.99 || value > 150.01 {
		t.Fatalf("valuation adapter fair value=%v, want Python's 1.5x price-anchor fallback 150", quote.FairValue)
	}
	if !strings.Contains(quote.FairValueWarning, "capped at 3x trailing EPS") || !strings.Contains(quote.FairValueWarning, "bounded price anchor") {
		t.Fatalf("missing cap diagnostic: %q", quote.FairValueWarning)
	}
	if quote.FairValueConfidenceScore == nil {
		t.Fatal("valuation confidence score is missing")
	}
	if quote.FairValueConfidence == nil || *quote.FairValueConfidence != apiConfidenceLabel(*quote.FairValueConfidenceScore) {
		t.Fatalf("valuation confidence label=%v does not match score %v", quote.FairValueConfidence, *quote.FairValueConfidenceScore)
	}
	if quote.FairValueForwardEPSSource == nil || *quote.FairValueForwardEPSSource != "Yahoo远期EPS" {
		t.Fatalf("observed forward EPS source=%v, want Yahoo远期EPS", quote.FairValueForwardEPSSource)
	}
}

func TestCachedConfidenceScoreAcceptsLegacyRatioAndPercentage(t *testing.T) {
	if got := cachedConfidenceScore(0.84); got == nil || *got != 84 {
		t.Fatalf("legacy confidence score=%v, want 84", got)
	}
	if got := cachedConfidenceScore(84.0); got == nil || *got != 84 {
		t.Fatalf("percentage confidence score=%v, want 84", got)
	}
}

func TestRefreshInvalidSymbolUsesValidationStatus(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Post(ts.URL+"/api/refresh/INVALID!", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid symbol refresh status=%d, want 422", response.StatusCode)
	}
}

func TestMarketAPIRoutesMatchPythonSymbolValidation(t *testing.T) {
	_, ts := testService(t, "")
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/quote/%s"},
		{http.MethodGet, "/api/expirations/%s"},
		{http.MethodGet, "/api/chain/%s?expiration=2030-01-18"},
		{http.MethodGet, "/api/flow/%s?expiration=2030-01-18"},
		{http.MethodGet, "/api/gamma/%s"},
		{http.MethodPost, "/api/refresh/%s?max_age=0"},
		{http.MethodGet, "/api/events/%s"},
		{http.MethodGet, "/api/status/%s"},
		{http.MethodGet, "/api/levels/%s?expiration=2030-01-18"},
	}
	for _, invalid := range []string{"^GSPC", "ABCDEFGHIJK", "A=B"} {
		for _, test := range paths {
			path := fmt.Sprintf(test.path, invalid)
			t.Run(test.method+" "+path, func(t *testing.T) {
				request, err := http.NewRequest(test.method, ts.URL+path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if response.StatusCode != http.StatusUnprocessableEntity || body["detail"] != "标的代码格式无效" {
					t.Fatalf("response = status %d, body %#v; want Python-compatible 422 detail", response.StatusCode, body)
				}
			})
		}
	}
}

func TestQuoteRefreshWithoutUpstreamPriceUsesPythonFallback(t *testing.T) {
	for _, withCachedQuote := range []bool{false, true} {
		name := "pending without cache"
		if withCachedQuote {
			name = "cached quote fallback"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "quote-no-price.db")
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if withCachedQuote {
				price := 101.0
				fetched := time.Now().UTC()
				if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "cached", FetchedAt: &fetched, Sessions: map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
			defer service.Close()
			ts := httptest.NewServer(service.Router())
			defer ts.Close()

			response, err := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("response status=%d decode=%v body=%#v", response.StatusCode, err, body)
			}
			if withCachedQuote {
				if body["price"] != 101.0 || body["source"] != "sqlite" {
					t.Fatalf("response=%#v; want cached quote fallback", body)
				}
			} else if body["price"] != nil || body["source"] != "pending" || body["provider"] != "upstream" {
				t.Fatalf("response=%#v; want pending quote", body)
			}
		})
	}
}

func TestRouterUnknownAPIPathUsesFastAPIJSONNotFound(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/api/not-a-route")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("unknown route response is not JSON: %v", err)
	}
	if response.StatusCode != http.StatusNotFound || body["detail"] != "Not Found" {
		t.Fatalf("unknown API route response=%#v status=%d, want FastAPI 404", body, response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("unknown API content-type=%q, want application/json like FastAPI", got)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("API GET cache-control=%q, want no-store", response.Header.Get("Cache-Control"))
	}
}

func TestStaticAssetsAreImmutableAndVaryByEncoding(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/static/common/js/request.js?v=test")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("static Cache-Control=%q, want immutable", got)
	}
	if !hasVaryToken(response.Header, "Accept-Encoding") {
		t.Fatalf("static Vary=%v, want Accept-Encoding", response.Header.Values("Vary"))
	}
}

func TestRouterWrongAPIMethodUsesFastAPIJSONMethodNotAllowed(t *testing.T) {
	_, ts := testService(t, "")
	for _, test := range []struct {
		path   string
		method string
		allow  string
	}{
		{path: "/api/quote/QQQ", method: http.MethodPut, allow: http.MethodGet},
		{path: "/api/refresh/QQQ", method: http.MethodGet, allow: http.MethodPost},
	} {
		request, err := http.NewRequest(test.method, ts.URL+test.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil {
			t.Fatalf("%s %s method error response is not JSON: %v", test.method, test.path, decodeErr)
		}
		if response.StatusCode != http.StatusMethodNotAllowed || body["detail"] != "Method Not Allowed" || response.Header.Get("Allow") != test.allow {
			t.Errorf("%s %s response=%#v status=%d allow=%q, want FastAPI 405 Allow %q", test.method, test.path, body, response.StatusCode, response.Header.Get("Allow"), test.allow)
		}
	}
}

func TestHeadMatchesFastAPIGetRouteAvailability(t *testing.T) {
	_, ts := testService(t, "")
	for _, test := range []struct {
		path        string
		status      int
		allow       string
		contentType string
		cache       string
	}{
		{path: "/", status: http.StatusMethodNotAllowed, allow: http.MethodGet, contentType: "application/json"},
		{path: "/health", status: http.StatusMethodNotAllowed, allow: http.MethodGet, contentType: "application/json"},
		{path: "/api/status/QQQ", status: http.StatusMethodNotAllowed, allow: http.MethodGet, contentType: "application/json"},
		{path: "/openapi.json", status: http.StatusOK, contentType: "application/json"},
		{path: "/docs", status: http.StatusOK, contentType: "text/html"},
		{path: "/redoc", status: http.StatusOK, contentType: "text/html"},
		{path: "/static/common/css/styles.css", status: http.StatusOK, contentType: "text/css"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodHead, ts.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if response.StatusCode != test.status || len(body) != 0 {
				t.Fatalf("HEAD %s status=%d body=%q, want %d and empty body", test.path, response.StatusCode, body, test.status)
			}
			if got := response.Header.Get("Allow"); got != test.allow {
				t.Errorf("HEAD %s Allow=%q, want %q", test.path, got, test.allow)
			}
			if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, test.contentType) {
				t.Errorf("HEAD %s Content-Type=%q, want prefix %q", test.path, got, test.contentType)
			}
			if got := response.Header.Get("Cache-Control"); got != test.cache {
				t.Errorf("HEAD %s Cache-Control=%q, want %q", test.path, got, test.cache)
			}
			if test.path == "/docs" || test.path == "/redoc" || test.path == "/openapi.json" {
				if got := response.Header.Get("Referrer-Policy"); got != "" {
					t.Errorf("HEAD %s Referrer-Policy=%q, want absent like FastAPI", test.path, got)
				}
			}
		})
	}
}

func TestQuoteAndStatusHTTPResponsesIncludeNullFairValueWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quote-warning-null.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, fairValue := 100.0, 120.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &price, FairValue: fairValue, FairValueSource: "valuation_v39",
		FairValueStatus: "ready", Provider: "test", FetchedAt: &now, Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	assertNullWarning := func(path string, quoteAtTopLevel bool) {
		t.Helper()
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s response status=%d decode=%v body=%#v", path, response.StatusCode, err, body)
		}
		payload := body
		if !quoteAtTopLevel {
			var ok bool
			payload, ok = body["quote"].(map[string]any)
			if !ok {
				t.Fatalf("%s quote=%#v, want object", path, body["quote"])
			}
		}
		warning, present := payload["fair_value_warning"]
		if !present || warning != nil {
			t.Errorf("%s fair_value_warning=%#v present=%v, want explicit JSON null", path, warning, present)
		}
	}
	assertNullWarning("/api/quote/QQQ", true)
	assertNullWarning("/api/status/QQQ", false)
}

func TestCachedQuoteAndStatusRetainPythonSelectStarStorageFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "python-quote-fields.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, currency TEXT, market_state TEXT, provider TEXT NOT NULL, raw_json TEXT,
 sessions_json TEXT, today_open REAL, previous_close REAL, fair_value REAL, fair_value_low REAL,
 fair_value_high REAL, fair_value_buy_low REAL, fair_value_buy_high REAL,
 fair_value_source TEXT, fair_value_model TEXT, fair_value_methods_json TEXT,
 fair_value_forward_eps REAL, fair_value_forward_eps_source TEXT, fair_value_safety_margin REAL,
 fair_value_confidence TEXT, fair_value_confidence_score REAL, fair_value_interest_coverage REAL,
 fair_value_regime TEXT, fair_value_regime_signals_json TEXT, fair_value_model_under_regime TEXT,
 fair_value_defensive_json TEXT, fair_value_optimistic_json TEXT, fair_value_normalized_eps_source TEXT,
 fair_value_quarterly_momentum_json TEXT,
 fair_value_historical_valuation_percentiles_json TEXT
);
INSERT INTO quote_snapshots(symbol, fetched_at, price, currency, market_state, provider, raw_json,
 sessions_json, fair_value, fair_value_source, fair_value_methods_json,
 fair_value_historical_valuation_percentiles_json)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 100, 'USD', 'REGULAR', 'python',
 '{"provider":"python","opaque":[1,2]}', '{}', 120, 'valuation_python',
 '{ "owner_earnings" : 120 }', '{"percentiles":[10,50,90]}');`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	for _, path := range []string{"/api/quote/QQQ", "/api/status/QQQ"} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("%s response status=%d decode=%v body=%#v", path, response.StatusCode, err, body)
		}
		response.Body.Close()
		quote := body
		if path == "/api/status/QQQ" {
			var ok bool
			quote, ok = body["quote"].(map[string]any)
			if !ok {
				t.Fatalf("status quote=%#v, want object", body["quote"])
			}
		}
		if quote["id"] != float64(1) || quote["raw_json"] != `{"provider":"python","opaque":[1,2]}` || quote["fair_value_methods_json"] != `{ "owner_earnings" : 120 }` || quote["fair_value_historical_valuation_percentiles_json"] != `{"percentiles":[10,50,90]}` {
			t.Errorf("%s dropped or changed Python SQLite columns: %#v", path, quote)
		}
	}
}

func TestRefreshAPIValueIncludesNullWarningInNestedQuote(t *testing.T) {
	value := 100.0
	body, ok := refreshAPIValue(model.RefreshResponse{Quote: &model.Quote{Symbol: "QQQ", Price: &value}}).(map[string]any)
	if !ok {
		t.Fatalf("refresh API response has type %T, want map", refreshAPIValue(model.RefreshResponse{Quote: &model.Quote{Symbol: "QQQ", Price: &value}}))
	}
	quote, ok := body["quote"].(map[string]any)
	if !ok {
		t.Fatalf("nested quote has type %T, want map", body["quote"])
	}
	if warning, present := quote["fair_value_warning"]; !present || warning != nil {
		t.Fatalf("nested fair_value_warning=%#v present=%v, want explicit JSON null", warning, present)
	}
}

func TestQueryValidationRejectsExplicitEmptyValues(t *testing.T) {
	_, ts := testService(t, "")
	paths := []string{
		"/api/quote/QQQ?refresh=",
		"/api/expirations/QQQ?refresh=",
		"/api/gamma/QQQ?horizon_days=",
		"/api/gamma/QQQ?status_only=",
		"/api/gamma/QQQ?include_rows=",
		"/api/refresh/QQQ?max_age=",
		"/api/levels/QQQ?expiration=2030-01-18&raw_expirations=",
		"/api/levels/QQQ?expiration=2030-01-18&raw=",
		"/api/events/QQQ?last_event_id=",
	}
	for _, path := range paths {
		request, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, "/api/refresh/") {
			request.Method = http.MethodPost
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("request %q: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("request %q status=%d, want 422 for explicitly empty query value", path, response.StatusCode)
		}
	}
}

func TestQueryValidationMatchesFastAPIErrorDetailFixtures(t *testing.T) {
	_, ts := testService(t, "")
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{
			name: "missing required expiration",
			path: "/api/chain/QQQ",
			want: `{"detail":[{"type":"missing","loc":["query","expiration"],"msg":"Field required","input":null}]}`,
		},
		{
			name: "expiration pattern",
			path: "/api/flow/QQQ?expiration=not-date",
			want: `{"detail":[{"type":"string_pattern_mismatch","loc":["query","expiration"],"msg":"String should match pattern '^\\d{4}-\\d{2}-\\d{2}$'","input":"not-date","ctx":{"pattern":"^\\d{4}-\\d{2}-\\d{2}$"}}]}`,
		},
		{
			name: "gamma aggregates parse and boolean failures",
			path: "/api/gamma/QQQ?horizon_days=x&status_only=&include_rows=nope",
			want: `{"detail":[{"type":"int_parsing","loc":["query","horizon_days"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"},{"type":"bool_parsing","loc":["query","status_only"],"msg":"Input should be a valid boolean, unable to interpret input","input":""},{"type":"bool_parsing","loc":["query","include_rows"],"msg":"Input should be a valid boolean, unable to interpret input","input":"nope"}]}`,
		},
		{
			name: "levels aggregates constrained fields in signature order",
			path: "/api/levels/QQQ?spot=0&raw=perhaps&raw_expirations=9",
			want: `{"detail":[{"type":"greater_than","loc":["query","spot"],"msg":"Input should be greater than 0","input":"0","ctx":{"gt":0.0}},{"type":"bool_parsing","loc":["query","raw"],"msg":"Input should be a valid boolean, unable to interpret input","input":"perhaps"},{"type":"less_than_equal","loc":["query","raw_expirations"],"msg":"Input should be less than or equal to 8","input":"9","ctx":{"le":8}}]}`,
		},
		{
			name: "query validation aggregates before endpoint symbol normalization",
			path: "/api/levels/INVALID!?spot=bad&raw=perhaps&raw_expirations=9",
			want: `{"detail":[{"type":"float_parsing","loc":["query","spot"],"msg":"Input should be a valid number, unable to parse string as a number","input":"bad"},{"type":"bool_parsing","loc":["query","raw"],"msg":"Input should be a valid boolean, unable to interpret input","input":"perhaps"},{"type":"less_than_equal","loc":["query","raw_expirations"],"msg":"Input should be less than or equal to 8","input":"9","ctx":{"le":8}}]}`,
		},
		{
			name:   "refresh optional pattern and maximum",
			method: http.MethodPost,
			path:   "/api/refresh/QQQ?expiration=not-date&max_age=4000",
			want:   `{"detail":[{"type":"string_pattern_mismatch","loc":["query","expiration"],"msg":"String should match pattern '^\\d{4}-\\d{2}-\\d{2}$'","input":"not-date","ctx":{"pattern":"^\\d{4}-\\d{2}-\\d{2}$"}},{"type":"less_than_equal","loc":["query","max_age"],"msg":"Input should be less than or equal to 3600","input":"4000","ctx":{"le":3600}}]}`,
		},
		{
			name: "events expiration precedes cursor",
			path: "/api/events/QQQ?expiration=bad&last_event_id=-1",
			want: `{"detail":[{"type":"string_pattern_mismatch","loc":["query","expiration"],"msg":"String should match pattern '^\\d{4}-\\d{2}-\\d{2}$'","input":"bad","ctx":{"pattern":"^\\d{4}-\\d{2}-\\d{2}$"}},{"type":"greater_than_equal","loc":["query","last_event_id"],"msg":"Input should be greater than or equal to 0","input":"-1","ctx":{"ge":0}}]}`,
		},
		{
			name: "integer query rejects decimal-looking string",
			path: "/api/gamma/QQQ?horizon_days=45.0",
			want: `{"detail":[{"type":"int_parsing","loc":["query","horizon_days"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"45.0"}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodGet
			}
			request, err := http.NewRequest(method, ts.URL+test.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var got, want any
			if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
				t.Fatalf("422 body is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatalf("invalid FastAPI fixture: %v", err)
			}
			if response.StatusCode != http.StatusUnprocessableEntity || !reflect.DeepEqual(got, want) {
				t.Errorf("response=%#v status=%d; want FastAPI fixture %#v status=422", got, response.StatusCode, want)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
				t.Errorf("422 content-type=%q, want application/json like FastAPI", contentType)
			}
		})
	}
}

func TestQueryBooleanAcceptsPythonBooleanAliases(t *testing.T) {
	_, ts := testService(t, "")
	for _, value := range []string{"y", "t", "yes", "on", "1", "TRUE"} {
		response, err := http.Get(ts.URL + "/api/gamma/QQQ?status_only=" + value)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusOK || body["status_only"] != true {
			t.Errorf("boolean query %q response=%#v status=%d err=%v", value, body, response.StatusCode, decodeErr)
		}
	}
}

func TestRepeatedQueryParametersUseLastValueLikeFastAPI(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/api/gamma/QQQ?horizon_days=invalid&horizon_days=45&status_only=false&status_only=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.StatusCode != http.StatusOK || body["status_only"] != true {
		t.Fatalf("response=%#v status=%d; want last scalar query values and status_only=true", body, response.StatusCode)
	}
}

func TestRepeatedAccessKeyUsesLastQueryValueLikeFastAPI(t *testing.T) {
	_, ts := testService(t, "secret")
	for _, test := range []struct {
		query string
		want  int
	}{
		{query: "wrong&key=secret", want: http.StatusOK},
		{query: "secret&key=wrong", want: http.StatusForbidden},
	} {
		response, err := http.Get(ts.URL + "/api/quote/QQQ?key=" + test.query)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Errorf("repeated key query %q status=%d, want %d", test.query, response.StatusCode, test.want)
		}
	}
}

func TestNoStoreMatchesPythonEndpointScope(t *testing.T) {
	_, ts := testService(t, "")
	for _, test := range []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/", "no-store"},
		{http.MethodGet, "/health", ""},
		{http.MethodPost, "/api/refresh/INVALID!", ""},
	} {
		request, err := http.NewRequest(test.method, ts.URL+test.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if got := response.Header.Get("Cache-Control"); got != test.want {
			t.Errorf("%s %s Cache-Control=%q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func TestExpirationRoutesValidatePythonPatternWithoutCalendarParsing(t *testing.T) {
	_, ts := testService(t, "")
	for _, path := range []string{
		"/api/chain/QQQ?expiration=2030-02-30",
		"/api/flow/QQQ?expiration=2030-02-30",
		"/api/levels/QQQ?expiration=2030-02-30&raw=true",
	} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("request %q: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("request %q status=%d, want 200 because Python validates date format only", path, response.StatusCode)
		}
	}
}

func TestLaterTimestampMatchesPythonMaxSelection(t *testing.T) {
	older := time.Date(2030, 1, 18, 15, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	if got, ok := laterTimestamp(&older, newer).(*time.Time); !ok || got == nil || !got.Equal(newer) {
		t.Fatalf("later timestamp=%#v, want newer profile time", laterTimestamp(&older, newer))
	}
	if got, ok := laterTimestamp(&newer, older).(*time.Time); !ok || got == nil || !got.Equal(newer) {
		t.Fatalf("later timestamp=%#v, want selected chain time", laterTimestamp(&newer, older))
	}
	if got := laterTimestamp(nil, nil); got != (*time.Time)(nil) {
		t.Fatalf("nil timestamps returned %#v", got)
	}
}

func TestCachedQuoteReadsThrottleFailedValuationRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cached-valuation-retry.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "cache", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{fundamentalsErr: fmt.Errorf("temporary Yahoo error")}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	for index := 0; index < 8; index++ {
		response, err := http.Get(ts.URL + "/api/quote/QQQ")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("cached quote status=%d, want 200", response.StatusCode)
		}
	}
	deadline := time.Now().Add(time.Second)
	for fake.fundamentalsCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := fake.fundamentalsCalls.Load(); got != 1 {
		t.Fatalf("failed fundamentals fetched %d times during retry cooldown, want 1", got)
	}
}

func TestQuoteRefreshFallbackUsesPersistedQuoteAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persisted-refresh-fallback.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 401.0
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "python", Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{quoteErr: fmt.Errorf("upstream unavailable")})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("restart quote fallback status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	if body["price"] != float64(401) || body["warning"] != "upstream unavailable" || body["fair_value_warning"] != nil {
		t.Fatalf("persisted fallback or market warning incorrect: %#v", body)
	}
}

func TestQuoteIgnoresCachedSnapshotWithoutPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quote-null-price.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Provider: "legacy", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{quoteErr: fmt.Errorf("upstream unavailable")})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Get(ts.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	var pending map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&pending)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("quote without refresh status=%d decode=%v body=%#v", response.StatusCode, decodeErr, pending)
	}
	if pending["source"] != "pending" || pending["provider"] != "upstream" || pending["price"] != nil {
		t.Fatalf("NULL-price snapshot should produce Python pending response: %#v", pending)
	}

	response, err = http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var failed map[string]any
	if err := json.NewDecoder(response.Body).Decode(&failed); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("quote refresh with NULL-price fallback status=%d decode=%v body=%#v", response.StatusCode, err, failed)
	}
}

func TestQuoteRefreshDoesNotUseMemoryOnlyQuoteFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quote-refresh-warning.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 400.0
	fake := &testMarketProvider{quoteErr: fmt.Errorf("upstream temporarily unavailable")}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	service.quotes["QQQ"] = cachedQuote{value: model.Quote{Symbol: "QQQ", Price: &price, Provider: "cache", FairValueWarning: "valuation data incomplete", Sessions: map[string]any{}}, at: time.Now()}
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("quote with no persisted fallback status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	service.mu.RLock()
	_, cached := service.quotes["QQQ"]
	service.mu.RUnlock()
	if cached {
		t.Fatal("memory-only quote remained cached after SQLite reported no snapshot")
	}
}

func TestQuoteAndStatusIgnoreMemoryQuoteDeletedByAnotherWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quote-deleted-by-peer.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 401.0
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "python", Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	requestQuote := func() map[string]any {
		t.Helper()
		response, err := http.Get(ts.URL + "/api/quote/QQQ")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("quote response status=%d decode=%v body=%#v", response.StatusCode, err, body)
		}
		return body
	}
	if body := requestQuote(); body["price"] != float64(401) {
		t.Fatalf("initial persisted quote=%#v, want price 401", body)
	}
	service.mu.RLock()
	_, cached := service.quotes["QQQ"]
	service.mu.RUnlock()
	if !cached {
		t.Fatal("initial persisted quote was not loaded into the in-memory cache")
	}

	admin, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("DELETE FROM quote_snapshots WHERE symbol=?", "QQQ"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := admin.Close(); err != nil {
		t.Fatal(err)
	}

	if body := requestQuote(); body["source"] != "pending" || body["price"] != nil {
		t.Fatalf("quote after peer deleted SQLite row=%#v, want pending with no price", body)
	}
	response, err := http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response=%#v code=%d err=%v", status, response.StatusCode, err)
	}
	if status["quote"] != nil {
		t.Fatalf("status returned deleted cached quote: %#v", status["quote"])
	}
	service.mu.RLock()
	_, cached = service.quotes["QQQ"]
	service.mu.RUnlock()
	if cached {
		t.Fatal("deleted SQLite quote remained in the in-memory cache")
	}
}

func TestGammaResponseIncludesOIFallbackMetadata(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/api/gamma/QQQ?include_rows=false")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("gamma status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	fallback, ok := body["oi_fallback"].(map[string]any)
	if !ok || fallback["restored"] != float64(0) || fallback["as_of"] != nil {
		t.Fatalf("gamma response missing OI fallback metadata: %#v", body)
	}
}

func TestGammaLowMemoryDegradedWarningMatchesPython(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-low-memory.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, LowMemory: true}, repository, &testMarketProvider{})
	defer service.Close()
	service.gammaHeavyGate <- struct{}{}
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Get(ts.URL + "/api/gamma/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("degraded Gamma response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	if body["degraded"] != true || body["warning"] != "内存保护：已有刷新在进行，Gamma 窗口稍后计算" {
		t.Fatalf("degraded Gamma response=%#v, want Python warning and degraded=true", body)
	}
}

func TestGammaHeavyGateCapacityMatchesMemoryMode(t *testing.T) {
	for _, test := range []struct {
		name      string
		lowMemory bool
		want      int
	}{
		{name: "normal", want: 4},
		{name: "low memory", lowMemory: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gamma-gate.db")
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			service := NewWithProvider(config.Config{DatabasePath: path, LowMemory: test.lowMemory}, repository, &testMarketProvider{})
			defer service.Close()
			if got := cap(service.gammaHeavyGate); got != test.want {
				t.Fatalf("gamma heavy gate capacity=%d, want %d", got, test.want)
			}
		})
	}
}

func TestValuationAdapterInputsIncludeBetaAndHistoricalGrowth(t *testing.T) {
	beta := 2.0
	fundamentals := provider.YahooFundamentals{
		Beta: &beta, EPS: float64Pointer(10), AnnualEPS: []float64{12, 10},
		AnnualRevenue: []float64{110, 100}, Shares: float64Pointer(10), NetIncome: float64Pointer(100),
	}
	inputs := valuationInputs("TEST", float64Pointer(50), fundamentals)
	if inputs.RequiredReturn == nil || math.Abs(*inputs.RequiredReturn-.15) > 1e-12 {
		t.Fatalf("adapter CAPM input=%v want 15%%", inputs.RequiredReturn)
	}
	if inputs.GrowthRate == nil || math.Abs(*inputs.GrowthRate-.20) > 1e-12 {
		t.Fatalf("adapter growth input=%v want EPS CAGR 20%%", inputs.GrowthRate)
	}
	if inputs.ForwardEPS == nil || math.Abs(*inputs.ForwardEPS-12) > 1e-12 {
		t.Fatalf("adapter estimated forward EPS=%v want 12", inputs.ForwardEPS)
	}
}

func TestValuationAdapterMarksOnlyReportedGrowthAsObserved(t *testing.T) {
	tests := []struct {
		name         string
		fundamentals provider.YahooFundamentals
		wantObserved bool
	}{
		{"earnings growth", provider.YahooFundamentals{EarningsGrowth: float64Pointer(.12)}, true},
		{"quarterly earnings growth", provider.YahooFundamentals{EarningsQuarterlyGrowth: float64Pointer(.12)}, true},
		{"revenue growth", provider.YahooFundamentals{RevenueGrowth: float64Pointer(.12)}, true},
		{"historical EPS CAGR", provider.YahooFundamentals{AnnualEPS: []float64{12, 10}}, false},
		{"no growth data", provider.YahooFundamentals{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs := valuationInputs("TEST", float64Pointer(50), test.fundamentals)
			if inputs.GrowthHintObserved != test.wantObserved {
				t.Fatalf("GrowthHintObserved=%v, want %v", inputs.GrowthHintObserved, test.wantObserved)
			}
		})
	}
}

func TestValuationNormalizedEPSUsesAnnualNetIncomeWithUnitCheck(t *testing.T) {
	consistent := valuationInputs("TEST", float64Pointer(50), provider.YahooFundamentals{
		EPS: float64Pointer(10), AnnualEPS: []float64{12, 10}, AnnualNetIncome: []float64{120, 100}, Shares: float64Pointer(10),
	})
	if consistent.TrailingEPS == nil || *consistent.TrailingEPS != 10 || consistent.NormalizedEPS == nil || *consistent.NormalizedEPS != 12 || consistent.NormalizedEPSSource != "净利润 / 当前股本" {
		t.Fatalf("consistent net-income normalization=%#v", consistent)
	}
	if consistent.ForwardEPS == nil || math.Abs(*consistent.ForwardEPS-14.4) > 1e-12 {
		t.Fatalf("forward EPS from normalized earnings=%v, want 14.4", consistent.ForwardEPS)
	}

	anomalous := valuationInputs("TEST", float64Pointer(50), provider.YahooFundamentals{
		EPS: float64Pointer(10), AnnualEPS: []float64{12, 10}, AnnualNetIncome: []float64{600, 500}, Shares: float64Pointer(10),
	})
	if anomalous.TrailingEPS == nil || *anomalous.TrailingEPS != 10 || anomalous.NormalizedEPS == nil || *anomalous.NormalizedEPS != 10 || anomalous.NormalizedEPSSource != "历史 EPS 中位数（净利润股本量纲异常回退）" {
		t.Fatalf("unit-mismatch fallback=%#v", anomalous)
	}

	withoutEPS := valuationInputs("TEST", float64Pointer(50), provider.YahooFundamentals{
		AnnualNetIncome: []float64{100, 120}, Shares: float64Pointer(10),
	})
	if withoutEPS.TrailingEPS != nil || withoutEPS.NormalizedEPS == nil || *withoutEPS.NormalizedEPS != 12 || withoutEPS.NormalizedEPSSource != "净利润 / 当前股本" {
		t.Fatalf("net-income-only normalization=%#v", withoutEPS)
	}
}

func TestValuationAdapterKeepsTTMAndNormalizedEPSSeparate(t *testing.T) {
	inputs := valuationInputs("TEST", float64Pointer(120), provider.YahooFundamentals{
		EPS: float64Pointer(10), AnnualEPS: []float64{20, 18}, HistoricalPrices: []float64{100, 90},
	})
	if inputs.TrailingEPS == nil || *inputs.TrailingEPS != 10 || inputs.NormalizedEPS == nil || *inputs.NormalizedEPS != 18 {
		t.Fatalf("TTM and normalized EPS should retain separate meanings: %#v", inputs)
	}
	result := valuation.Calculate(inputs)
	percentiles, ok := result.HistoricalValuationPercentiles.(map[string]any)
	if !ok || percentiles["pe_current"] != 12.0 {
		t.Fatalf("current historical PE must use TTM EPS rather than normalized EPS: %#v", result.HistoricalValuationPercentiles)
	}
}

func TestValuationAdapterUsesYahooGrowthHintInFCFDCF(t *testing.T) {
	beta, revenueGrowth := 2.0, .04
	fundamentals := provider.YahooFundamentals{
		Beta: &beta, RevenueGrowth: &revenueGrowth, FreeCashflow: float64Pointer(100), Shares: float64Pointer(10),
		AnnualRevenue: []float64{121, 100},
	}
	inputs := valuationInputs("TEST", float64Pointer(50), fundamentals)
	result := valuation.Calculate(inputs)
	if result.Methods[valuation.MethodFCFDCF] == 0 {
		t.Fatalf("adapter omitted FCF DCF candidate: %#v", result)
	}
	// A live Yahoo revenue-growth hint must outrank historical revenue CAGR
	// for the DCF near-term estimate and its terminal-growth derivation.
	want := valuationDCFExpected(10, .04, .15, .015)
	if math.Abs(result.Methods[valuation.MethodFCFDCF]-want) > .01 {
		t.Fatalf("FCF DCF candidate=%v, want Yahoo-hint growth value %v", result.Methods[valuation.MethodFCFDCF], want)
	}
}

func valuationDCFExpected(base, growth, requiredReturn, terminalGrowth float64) float64 {
	value := 0.0
	for year := 1; year <= 5; year++ {
		value += base * math.Pow(1+growth, float64(year)) / math.Pow(1+requiredReturn, float64(year))
	}
	terminal := base * math.Pow(1+growth, 5) * (1 + terminalGrowth) / (requiredReturn - terminalGrowth)
	return math.Round((value+terminal/math.Pow(1+requiredReturn, 5))*100) / 100
}

func TestValuationForwardEPSUsesObservedOrBoundedCAGREstimate(t *testing.T) {
	observed := 8.0
	if got := valuationForwardEPS(provider.YahooFundamentals{ForwardEPS: &observed, EPS: float64Pointer(5)}); got == nil || *got != observed {
		t.Fatalf("observed forward EPS=%v want %v", got, observed)
	}
	derived := valuationForwardEPS(provider.YahooFundamentals{EPS: float64Pointer(10), AnnualEPS: []float64{12, 10}})
	if derived == nil || math.Abs(*derived-12) > 1e-12 {
		t.Fatalf("CAGR-derived forward EPS=%v want 12", derived)
	}
	if got := valuationForwardEPS(provider.YahooFundamentals{EPS: float64Pointer(10)}); got != nil {
		t.Fatalf("forward EPS invented without growth history: %v", *got)
	}
}

func TestValuationForwardEPSSourceMatchesPythonAPIContract(t *testing.T) {
	observed, modeled, invalid := 8.0, 12.0, math.Inf(1)
	tests := []struct {
		name         string
		fundamentals provider.YahooFundamentals
		forwardEPS   *float64
		want         *string
	}{
		{"Yahoo quote summary", provider.YahooFundamentals{ForwardEPS: &observed}, &observed, stringPointer("Yahoo远期EPS")},
		{"historical estimate", provider.YahooFundamentals{}, &modeled, stringPointer("模型估计远期EPS")},
		{"invalid Yahoo value with model fallback", provider.YahooFundamentals{ForwardEPS: &invalid}, &modeled, stringPointer("模型估计远期EPS")},
		{"missing", provider.YahooFundamentals{}, nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := valuationForwardEPSSource(test.fundamentals, test.forwardEPS)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("forward EPS source=%v, want %v", got, test.want)
			}
		})
	}
}

func TestValuationGrowthRateUsesAnnualEPSThenRevenue(t *testing.T) {
	tests := []struct {
		name string
		data provider.YahooFundamentals
		want float64
	}{
		{"earnings growth hint", provider.YahooFundamentals{EarningsGrowth: float64Pointer(.30), AnnualEPS: []float64{12, 10}}, .30},
		{"quarterly earnings hint", provider.YahooFundamentals{EarningsQuarterlyGrowth: float64Pointer(.25), RevenueGrowth: float64Pointer(.10)}, .25},
		{"revenue growth hint", provider.YahooFundamentals{RevenueGrowth: float64Pointer(.12), AnnualEPS: []float64{12, 10}}, .12},
		{"eps cagr fallback", provider.YahooFundamentals{AnnualEPS: []float64{14.4, 12, 10}, AnnualRevenue: []float64{144, 100}}, .20},
		{"revenue cagr fallback", provider.YahooFundamentals{AnnualRevenue: []float64{110, 100}}, .10},
		{"growth hint bounded", provider.YahooFundamentals{EarningsGrowth: float64Pointer(.80)}, .50},
		{"bounded", provider.YahooFundamentals{AnnualEPS: []float64{1000, 1}}, .50},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := valuationGrowthRate(test.data)
			if got == nil || math.Abs(*got-test.want) > 1e-12 {
				t.Fatalf("growth=%v want %v", got, test.want)
			}
		})
	}
	if got := valuationGrowthRate(provider.YahooFundamentals{AnnualEPS: []float64{0, 10}, AnnualRevenue: []float64{0, 0}}); got != nil {
		t.Fatalf("growth inferred from nonpositive endpoints: %v", *got)
	}
}

func TestValuationRequiredReturnUsesBoundedYahooBeta(t *testing.T) {
	cases := []struct {
		beta *float64
		want float64
	}{
		{nil, .095},
		{float64Pointer(.5), .085},
		{float64Pointer(1), .095},
		{float64Pointer(2), .15},
		{float64Pointer(4), .095},
	}
	for _, test := range cases {
		if got := valuationRequiredReturn(provider.YahooFundamentals{Beta: test.beta}); math.Abs(got-test.want) > 1e-12 {
			t.Errorf("beta=%v required return=%v want %v", test.beta, got, test.want)
		}
	}
}

func TestOwnerMaintenanceRatioUsesAssetHeavyTransitionSignal(t *testing.T) {
	negativeFCF := -1.0
	industry := provider.YahooFundamentals{Industry: "Software—Application", Sector: "Technology", FreeCashflow: &negativeFCF}
	if got := ownerMaintenanceRatio(industry); got != .30 || ownerMaintenanceRatioSource(industry) != "重资产转型" {
		t.Fatalf("negative FCF ratio/source=%v/%q", got, ownerMaintenanceRatioSource(industry))
	}
	capex, operating := 70.0, 100.0
	industry = provider.YahooFundamentals{Industry: "Software", CapitalExpenditure: &capex, OperatingCashflow: &operating}
	if got := ownerMaintenanceRatio(industry); got != .30 {
		t.Fatalf("capex-heavy ratio=%v want .30", got)
	}
	industry = provider.YahooFundamentals{Industry: "Semiconductors", Sector: "Technology"}
	if got := ownerMaintenanceRatio(industry); got != .50 {
		t.Fatalf("ordinary technology ratio=%v want .50", got)
	}
	industry = provider.YahooFundamentals{Industry: "Semiconductors", Sector: "Technology", IndustryKey: "semiconductor_memory", FreeCashflow: float64Pointer(-5)}
	if got := ownerMaintenanceRatio(industry); got != .50 {
		t.Fatalf("memory-cycle industry key should avoid transition ratio, got %v", got)
	}
	industry = provider.YahooFundamentals{Industry: "Technology", IndustryKey: "semiconductor-memory", FreeCashflow: float64Pointer(-5)}
	if got := ownerMaintenanceRatio(industry); got != .50 {
		t.Fatalf("hyphenated memory-cycle key should avoid transition ratio, got %v", got)
	}
}

func TestOwnerMaintenanceRatioRespectsDedicatedIndustryClassification(t *testing.T) {
	negativeFCF, capex, operating := -10.0, 70.0, 100.0
	cases := []struct {
		name string
		data provider.YahooFundamentals
		want float64
	}{
		{"defense negative FCF transition", provider.YahooFundamentals{Industry: "Aerospace & Defense", FreeCashflow: &negativeFCF}, .70},
		{"defense growth transition", provider.YahooFundamentals{Industry: "Aerospace & Defense", AnnualEPS: []float64{12, 10}}, .70},
		{"defense without transition", provider.YahooFundamentals{Industry: "Aerospace & Defense", AnnualEPS: []float64{10, 10}}, .70},
		{"defense capex-heavy without transition", provider.YahooFundamentals{Industry: "Aerospace & Defense", CapitalExpenditure: &capex, OperatingCashflow: &operating}, .30},
		{"industrial without transition", provider.YahooFundamentals{Industry: "Industrials", AnnualEPS: []float64{10, 10}}, .85},
	}
	for _, test := range cases {
		if got := ownerMaintenanceRatio(test.data); got != test.want {
			t.Errorf("%s ratio=%v want %v", test.name, got, test.want)
		}
	}
}

func TestOwnerMaintenanceRatioDoesNotTreatFinancialCashFlowsAsAssetHeavyTransition(t *testing.T) {
	negativeFCF, capex, operating := -25.0, -80.0, 100.0
	cases := []struct {
		fundamentals provider.YahooFundamentals
		want         float64
	}{
		{provider.YahooFundamentals{Sector: "Financial Services", Industry: "Banks—Diversified", FreeCashflow: &negativeFCF, CapitalExpenditure: &capex, OperatingCashflow: &operating}, .70},
		{provider.YahooFundamentals{Sector: "Financial Services", Industry: "Insurance—Diversified", FreeCashflow: &negativeFCF}, .70},
		{provider.YahooFundamentals{Sector: "Real Estate", Industry: "REIT—Retail", FreeCashflow: &negativeFCF}, .70},
		{provider.YahooFundamentals{Industry: "Capital Markets", FreeCashflow: &negativeFCF}, .70},
		// Python's maintenance-ratio rule independently treats consumer-tagged
		// classifications as technology/consumer even though valuation uses a
		// financial-sector-specific model.
		{provider.YahooFundamentals{Industry: "Consumer Credit", FreeCashflow: &negativeFCF}, .50},
	}
	for _, test := range cases {
		fundamentals := test.fundamentals
		if got := ownerMaintenanceRatio(fundamentals); got != test.want {
			t.Errorf("financial/real-estate fields %q / %q produced maintenance ratio %v, want Python %v", fundamentals.Sector, fundamentals.Industry, got, test.want)
		}
	}
}

func TestCachedQuoteReadReschedulesMissingValuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cached-valuation.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, trailing, forward, shares := 100.0, 10.0, 12.0, 1.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "cache", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{fundamentals: provider.YahooFundamentals{EPS: &trailing, ForwardEPS: &forward, Shares: &shares}}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cached quote status=%d, want 200", response.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.fundamentalsCalls.Load() > 0 {
			quote, found, loadErr := repository.LatestQuote("QQQ")
			if loadErr == nil && found && quote.FairValue != nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cached read did not schedule valuation; fundamentals calls=%d", fake.fundamentalsCalls.Load())
}

func testService(t *testing.T, accessKey string) (*Service, *httptest.Server) {
	t.Helper()
	cfg := config.Config{
		Host: "127.0.0.1", Port: "0", DatabasePath: filepath.Join(t.TempDir(), "options.db"),
		AccessKey:     accessKey,
		DefaultSymbol: "QQQ", UpstreamTimeout: 50_000_000, UpstreamConcurrency: 2,
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(service.Router())
	t.Cleanup(func() { ts.Close(); _ = service.Close() })
	return service, ts
}

func TestNewSetsCboeCacheLimitForLowMemoryMode(t *testing.T) {
	for _, test := range []struct {
		name      string
		lowMemory bool
		wantLimit int
	}{
		// The provider bounds its delayed payload cache to 16 by default; low
		// memory keeps only the active symbol.
		{name: "normal", wantLimit: 16},
		{name: "low-memory", lowMemory: true, wantLimit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, err := New(config.Config{
				DatabasePath: filepath.Join(t.TempDir(), "options.db"), LowMemory: test.lowMemory,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			hybrid, ok := service.provider.(*provider.Hybrid)
			if !ok {
				t.Fatalf("provider type=%T, want *provider.Hybrid", service.provider)
			}
			if got := hybrid.Delayed.CacheLimit; got != test.wantLimit {
				t.Errorf("Cboe cache limit=%d, want %d", got, test.wantLimit)
			}
		})
	}
}

type statusTrackingProvider struct {
	*testMarketProvider
	historyCalls  atomic.Int64
	earningsCalls atomic.Int64
	history       provider.HistoryResult
	historyErr    error
	earningsDates []string
	earningsErr   error
}

type chanAdjustedRecordingProvider struct {
	*testMarketProvider
	requests chan string
}

func (p *chanAdjustedRecordingProvider) ChanAdjustedHistory(_ context.Context, symbol, period string) (provider.ChanAdjustedHistoryResult, error) {
	p.requests <- symbol + "|" + period
	return provider.ChanAdjustedHistoryResult{Symbol: symbol, Timeframe: "1d", AdjustmentType: "split_dividend_auto", Source: "go-yfinance", FetchedAt: time.Now().UTC(), Bars: []map[string]any{{
		"date": "2026-10-16", "open": 10.0, "high": 11.0, "low": 9.0, "close": 10.0, "volume": 1.0,
		"raw_open": 10.0, "raw_high": 11.0, "raw_low": 9.0, "raw_close": 10.0, "raw_volume": 1.0, "adjustment_factor": 1.0,
	}}}, nil
}

func (p *statusTrackingProvider) History(_ context.Context, symbol, _ string) (provider.HistoryResult, error) {
	p.historyCalls.Add(1)
	if p.historyErr != nil {
		return provider.HistoryResult{}, p.historyErr
	}
	result := p.history
	if result.Symbol == "" {
		result.Symbol = symbol
	}
	return result, nil
}

func (p *statusTrackingProvider) EarningsDates(context.Context, string) ([]string, error) {
	p.earningsCalls.Add(1)
	if p.earningsErr != nil {
		return nil, p.earningsErr
	}
	return append([]string(nil), p.earningsDates...), nil
}

type nearestChainTestProvider struct {
	*testMarketProvider
	nearestCalls atomic.Int64
}

func (p *nearestChainTestProvider) NearestChain(_ context.Context, _ string) ([]string, []model.OptionRow, *time.Time, error) {
	p.nearestCalls.Add(1)
	strike := 100.0
	fetched := time.Now().UTC()
	return []string{"2030-01-18"}, []model.OptionRow{{
		ContractSymbol: "TEST300118C00100000", Provider: "test", ContractType: "call",
		Strike: &strike, Expiration: "2030-01-18",
	}}, &fetched, nil
}

type testMarketProvider struct {
	quoteCalls        atomic.Int64
	expirationCalls   atomic.Int64
	chainCalls        atomic.Int64
	quoteDelay        time.Duration
	quoteErr          error
	expirationErr     error
	chainErr          error
	quoteStarted      chan struct{}
	quoteRelease      chan struct{}
	expirationStarted chan struct{}
	expirationRelease chan struct{}
	expirations       []string
	chainMu           sync.Mutex
	chainExpirations  []string
	chainRows         []model.OptionRow
	quote             model.Quote
	fundamentals      provider.YahooFundamentals
	fundamentalsErr   error
	fundamentalsCalls atomic.Int64
}

func (p *testMarketProvider) Quote(ctx context.Context, symbol string) (model.Quote, error) {
	p.quoteCalls.Add(1)
	if p.quoteStarted != nil {
		select {
		case p.quoteStarted <- struct{}{}:
		default:
		}
	}
	if p.quoteRelease != nil {
		select {
		case <-p.quoteRelease:
		case <-ctx.Done():
			return model.Quote{}, ctx.Err()
		}
	} else if p.quoteDelay > 0 {
		select {
		case <-time.After(p.quoteDelay):
		case <-ctx.Done():
			return model.Quote{}, ctx.Err()
		}
	}
	if p.quoteErr != nil {
		return model.Quote{}, p.quoteErr
	}
	quote := p.quote
	quote.Symbol = symbol
	return quote, nil
}

func (p *testMarketProvider) Fundamentals(context.Context, string) (provider.YahooFundamentals, error) {
	p.fundamentalsCalls.Add(1)
	if p.fundamentalsErr != nil {
		return provider.YahooFundamentals{}, p.fundamentalsErr
	}
	return p.fundamentals, nil
}

func (p *testMarketProvider) Expirations(ctx context.Context, _ string) ([]string, error) {
	p.expirationCalls.Add(1)
	if p.expirationStarted != nil {
		select {
		case p.expirationStarted <- struct{}{}:
		default:
		}
	}
	if p.expirationRelease != nil {
		select {
		case <-p.expirationRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.expirationErr != nil {
		return nil, p.expirationErr
	}
	return append([]string(nil), p.expirations...), nil
}

func (p *testMarketProvider) Chain(_ context.Context, _ string, expiration string) ([]model.OptionRow, *time.Time, error) {
	p.chainCalls.Add(1)
	if p.chainErr != nil {
		return nil, nil, p.chainErr
	}
	p.chainMu.Lock()
	p.chainExpirations = append(p.chainExpirations, expiration)
	p.chainMu.Unlock()
	fetched := time.Now().UTC()
	return append([]model.OptionRow(nil), p.chainRows...), &fetched, nil
}

func TestRefreshRequestsCoalesceForSymbol(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "refresh.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{quoteDelay: 50 * time.Millisecond, expirations: []string{time.Now().AddDate(0, 0, 8).Format("2006-01-02")}, quote: model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", "", 0)
			if refreshErr != nil {
				t.Errorf("refreshSnapshot: %v", refreshErr)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := fake.quoteCalls.Load(); got != 1 {
		t.Fatalf("quote calls=%d, want 1 coalesced upstream fetch", got)
	}
	if got := fake.expirationCalls.Load(); got != 1 {
		t.Fatalf("expiration calls=%d, want 1", got)
	}
	if got := fake.chainCalls.Load(); got != 1 {
		t.Fatalf("chain calls=%d, want 1", got)
	}
}

func TestRefreshSnapshotFetchesQuoteAndExpirationCatalogConcurrentlyWithinLowMemoryBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parallel-refresh.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{
		quote:             model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		expirations:       []string{time.Now().AddDate(0, 0, 8).Format("2006-01-02")},
		quoteStarted:      make(chan struct{}, 1),
		quoteRelease:      make(chan struct{}),
		expirationStarted: make(chan struct{}, 1),
		expirationRelease: make(chan struct{}),
	}
	service := NewWithProvider(config.Config{
		DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 2, LowMemory: true,
	}, repository, fake)
	defer service.Close()

	result := make(chan error, 1)
	go func() {
		_, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", "", 0)
		result <- refreshErr
	}()

	seenQuote, seenCatalog := false, false
	for !seenQuote || !seenCatalog {
		select {
		case <-fake.quoteStarted:
			seenQuote = true
		case <-fake.expirationStarted:
			seenCatalog = true
		case <-time.After(time.Second):
			close(fake.quoteRelease)
			close(fake.expirationRelease)
			<-result
			t.Fatalf("provider calls had not both started: quote=%v catalog=%v", seenQuote, seenCatalog)
		}
	}
	close(fake.quoteRelease)
	close(fake.expirationRelease)
	if err := <-result; err != nil {
		t.Fatalf("refreshSnapshot() error = %v", err)
	}
}

func TestRefreshSnapshotEstimatesMissingOptionGammaWithoutOverwritingUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-refresh.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := "2099-01-16"
	price, strike, iv, upstreamGamma := 100.0, 100.0, 0.25, 0.123
	fake := &testMarketProvider{
		quote:       model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		expirations: []string{expiration},
		chainRows: []model.OptionRow{
			{ContractSymbol: "QQQ-MISSING-GAMMA", Expiration: expiration, ContractType: "call", Strike: &strike, ImpliedVolatility: &iv},
			{ContractSymbol: "QQQ-UPSTREAM-GAMMA", Expiration: expiration, ContractType: "put", Strike: &strike, ImpliedVolatility: &iv, Gamma: &upstreamGamma},
		},
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()

	if _, err := service.refreshSnapshot(context.Background(), "QQQ", expiration, 0); err != nil {
		t.Fatalf("refreshSnapshot() error = %v", err)
	}
	chain, found, err := repository.LatestChain("QQQ", expiration)
	if err != nil || !found {
		t.Fatalf("LatestChain() found=%v, err=%v", found, err)
	}
	if len(chain.Data) != 2 {
		t.Fatalf("persisted chain rows = %d, want 2", len(chain.Data))
	}
	if chain.Data[0].Gamma == nil || *chain.Data[0].Gamma <= 0 {
		t.Fatalf("missing upstream gamma was not estimated: %#v", chain.Data[0].Gamma)
	}
	if chain.Data[1].Gamma == nil || *chain.Data[1].Gamma != upstreamGamma {
		t.Fatalf("upstream gamma was overwritten: %#v", chain.Data[1].Gamma)
	}
}

func TestRefreshSnapshotQuoteFailureCancelsParallelExpirationFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parallel-refresh-cancel.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	quoteErr := errors.New("quote upstream failed")
	fake := &testMarketProvider{
		quoteErr:          quoteErr,
		expirationStarted: make(chan struct{}, 1),
		expirationRelease: make(chan struct{}),
	}
	service := NewWithProvider(config.Config{
		DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 2,
	}, repository, fake)
	defer service.Close()

	result := make(chan error, 1)
	go func() {
		_, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", "", 0)
		result <- refreshErr
	}()
	select {
	case err := <-result:
		if !errors.Is(err, quoteErr) {
			t.Fatalf("refreshSnapshot() error = %v, want quote error %v", err, quoteErr)
		}
		if got := fake.expirationCalls.Load(); got != 1 {
			t.Fatalf("parallel expiration calls=%d, want one cancellable request", got)
		}
	case <-time.After(time.Second):
		t.Fatal("quote failure did not cancel the parallel expiration request")
	}
}

func TestSubscriptionRefreshLoopUsesDurableClaimAcrossServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscription-refresh.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	firstStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := store.Open(path)
	if err != nil {
		firstStore.Close()
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{
		quoteDelay:  100 * time.Millisecond,
		quote:       model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}},
		expirations: []string{},
	}
	first := NewWithProvider(cfg, firstStore, fake)
	second := NewWithProvider(cfg, secondStore, fake)
	defer first.Close()
	defer second.Close()
	releaseFirst := first.registerSubscription("QQQ|", "QQQ", "")
	releaseSecond := second.registerSubscription("QQQ|", "QQQ", "")
	defer releaseFirst()
	defer releaseSecond()

	deadline := time.Now().Add(2 * time.Second)
	for (fake.quoteCalls.Load() == 0 || fake.expirationCalls.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fake.quoteCalls.Load(); got != 1 {
		t.Fatalf("durable subscription claim upstream quote calls=%d, want one", got)
	}
	// Give the losing worker enough time to observe the shared lease. It must
	// wait rather than start a second provider refresh in the same cadence.
	time.Sleep(250 * time.Millisecond)
	if got := fake.quoteCalls.Load(); got != 1 {
		t.Fatalf("losing subscription started duplicate upstream refresh: quote calls=%d", got)
	}
}

func TestQuoteEndpointFetchCoalescesWithSnapshotRefresh(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "quote-gate.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	expiration := time.Now().AddDate(0, 0, 8).Format("2006-01-02")
	price := 100.0
	fake := &testMarketProvider{
		quoteStarted: make(chan struct{}, 2), quoteRelease: make(chan struct{}), expirations: []string{expiration},
		quote: model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	quoteDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
		if requestErr != nil {
			quoteDone <- requestErr
			return
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			quoteDone <- fmt.Errorf("quote refresh HTTP status %d", response.StatusCode)
			return
		}
		quoteDone <- nil
	}()
	select {
	case <-fake.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("direct quote fetch did not reach upstream")
	}

	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", expiration, 0)
		refreshDone <- refreshErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.RLock()
		gate := service.refreshGates["quote|QQQ"]
		waiting := gate != nil && gate.refs >= 2 && service.refreshGates[snapshotRefreshGateKey("QQQ", expiration)] != nil
		service.mu.RUnlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot refresh did not join the shared quote gate")
		}
		time.Sleep(time.Millisecond)
	}
	close(fake.quoteRelease)
	if err := <-quoteDone; err != nil {
		t.Fatalf("direct quote fetch: %v", err)
	}
	if err := <-refreshDone; err != nil {
		t.Fatalf("snapshot refresh: %v", err)
	}
	if calls := fake.quoteCalls.Load(); calls != 1 {
		t.Fatalf("provider quote calls=%d, want one shared fetch", calls)
	}
	if calls := fake.expirationCalls.Load(); calls != 1 {
		t.Fatalf("expiration calls=%d, want snapshot refresh to continue after quote coalescing", calls)
	}
	if calls := fake.chainCalls.Load(); calls != 1 {
		t.Fatalf("chain calls=%d, want one", calls)
	}
}

func TestConcurrentRefreshesForDifferentExpirationsDoNotCoalesce(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "different-expirations.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	first := time.Now().AddDate(0, 0, 8).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 15).Format("2006-01-02")
	price := 100.0
	fake := &testMarketProvider{
		quoteStarted: make(chan struct{}, 4),
		quoteRelease: make(chan struct{}),
		expirations:  []string{first, second},
		quote:        model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	var wg sync.WaitGroup
	results := make(chan model.RefreshResponse, 2)
	refreshErrors := make(chan error, 2)
	launch := func(expiration string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", expiration, 0)
			results <- response
			refreshErrors <- refreshErr
		}()
	}
	launch(first)
	select {
	case <-fake.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not reach the upstream quote")
	}
	launch(second)
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.RLock()
		firstGate := service.refreshGates[snapshotRefreshGateKey("QQQ", first)]
		secondGate := service.refreshGates[snapshotRefreshGateKey("QQQ", second)]
		waiting := firstGate != nil && secondGate != nil && firstGate != secondGate
		service.mu.RUnlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refreshes for different expirations did not acquire independent gates")
		}
		time.Sleep(time.Millisecond)
	}
	close(fake.quoteRelease)
	wg.Wait()
	close(results)
	close(refreshErrors)
	for err := range refreshErrors {
		if err != nil {
			t.Fatalf("refreshSnapshot: %v", err)
		}
	}
	coalesced := 0
	for result := range results {
		if result.Coalesced {
			coalesced++
		}
	}
	if coalesced != 0 {
		t.Fatalf("different-expiration refreshes coalesced %d times, want 0", coalesced)
	}
	if got := fake.quoteCalls.Load(); got != 2 {
		t.Fatalf("quote calls=%d, want 2 independent refreshes", got)
	}
	fake.chainMu.Lock()
	gotExpirations := append([]string(nil), fake.chainExpirations...)
	fake.chainMu.Unlock()
	if len(gotExpirations) != 2 || gotExpirations[0] == gotExpirations[1] || !(containsExpiration(gotExpirations, first) && containsExpiration(gotExpirations, second)) {
		t.Fatalf("chain expirations=%#v, want both %q and %q", gotExpirations, first, second)
	}
}

type chainBarrierProvider struct {
	*testMarketProvider
	chainStarted chan string
	chainRelease chan struct{}
}

func (p *chainBarrierProvider) Chain(ctx context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	p.chainStarted <- symbol + "|" + expiration
	select {
	case <-p.chainRelease:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	fetched := time.Now().UTC()
	return []model.OptionRow{{ContractSymbol: symbol + "-" + expiration, Expiration: expiration}}, &fetched, nil
}

func TestConcurrentDifferentExpirationRefreshesReachProviderTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parallel-expirations.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().AddDate(0, 0, 8).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 15).Format("2006-01-02")
	price := 100.0
	provider := &chainBarrierProvider{
		testMarketProvider: &testMarketProvider{
			expirations: []string{first, second},
			quote:       model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		},
		chainStarted: make(chan string, 2), chainRelease: make(chan struct{}),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	var releaseOnce sync.Once
	releaseChains := func() { releaseOnce.Do(func() { close(provider.chainRelease) }) }
	defer releaseChains()
	results := make(chan error, 2)
	launch := func(expiration string) {
		go func() {
			_, refreshErr := service.refreshSnapshot(context.Background(), "QQQ", expiration, 0)
			results <- refreshErr
		}()
	}
	launch(first)
	select {
	case got := <-provider.chainStarted:
		if got != "QQQ|"+first {
			t.Fatalf("first chain key=%q, want %q", got, "QQQ|"+first)
		}
	case <-time.After(time.Second):
		t.Fatal("first refresh did not reach the chain provider")
	}
	launch(second)
	select {
	case got := <-provider.chainStarted:
		if got != "QQQ|"+second {
			t.Fatalf("second chain key=%q, want %q", got, "QQQ|"+second)
		}
	case <-time.After(time.Second):
		t.Fatal("different-expiration refresh waited for the first chain to finish")
	}
	releaseChains()
	for range 2 {
		if refreshErr := <-results; refreshErr != nil {
			t.Fatalf("refreshSnapshot: %v", refreshErr)
		}
	}
}

func TestConcurrentRefreshesForDifferentSymbolsReachProviderTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parallel-symbols.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().AddDate(0, 0, 8).Format("2006-01-02")
	price := 100.0
	provider := &chainBarrierProvider{
		testMarketProvider: &testMarketProvider{
			expirations: []string{expiration},
			quote:       model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		},
		chainStarted: make(chan string, 2), chainRelease: make(chan struct{}),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	var releaseOnce sync.Once
	releaseChains := func() { releaseOnce.Do(func() { close(provider.chainRelease) }) }
	defer releaseChains()
	results := make(chan error, 2)
	launch := func(symbol string) {
		go func() {
			_, refreshErr := service.refreshSnapshot(context.Background(), symbol, expiration, 0)
			results <- refreshErr
		}()
	}
	launch("QQQ")
	select {
	case got := <-provider.chainStarted:
		if got != "QQQ|"+expiration {
			t.Fatalf("first chain key=%q, want %q", got, "QQQ|"+expiration)
		}
	case <-time.After(time.Second):
		t.Fatal("first refresh did not reach the chain provider")
	}
	launch("SPY")
	select {
	case got := <-provider.chainStarted:
		if got != "SPY|"+expiration {
			t.Fatalf("second chain key=%q, want %q", got, "SPY|"+expiration)
		}
	case <-time.After(time.Second):
		t.Fatal("different-symbol refresh waited for the first chain to finish")
	}
	releaseChains()
	for range 2 {
		if refreshErr := <-results; refreshErr != nil {
			t.Fatalf("refreshSnapshot: %v", refreshErr)
		}
	}
}

func TestCachedChainReadCompletesWhileGammaJobOwnsHeavyGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain-read-during-gamma.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, LowMemory: true}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	jobExpiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	readExpiration := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	provider := &chainBarrierProvider{
		testMarketProvider: &testMarketProvider{expirations: []string{jobExpiration, readExpiration}},
		chainStarted:       make(chan string, 1),
		chainRelease:       make(chan struct{}),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	var releaseOnce sync.Once
	releaseChains := func() { releaseOnce.Do(func() { close(provider.chainRelease) }) }
	defer releaseChains()
	fetched := time.Now().UTC()
	strike, openInterest := 100.0, int64(25)
	if err := repository.SaveChain(model.ChainResponse{
		Symbol: "QQQ", Expiration: readExpiration, FetchedAt: &fetched,
		Data: []model.OptionRow{{
			ContractSymbol: "QQQ-C100", ContractType: "call", Expiration: readExpiration,
			Strike: &strike, OpenInterest: &openInterest,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveExpirations("QQQ", []string{jobExpiration, readExpiration}); err != nil {
		t.Fatal(err)
	}
	spot := 100.0
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &spot, Provider: "sqlite-test", FetchedAt: &fetched, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
		t.Fatal("Gamma job was not claimed")
	}
	select {
	case got := <-provider.chainStarted:
		if got != "QQQ|"+jobExpiration {
			t.Fatalf("Gamma chain key=%q, want %q", got, "QQQ|"+jobExpiration)
		}
	case <-time.After(time.Second):
		t.Fatal("Gamma job did not reach the blocked chain provider")
	}
	if got := len(service.gammaHeavyGate); got != 1 {
		t.Fatalf("Gamma heavy gate occupancy=%d, want 1 while provider is blocked", got)
	}
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(ts.URL + "/api/chain/QQQ?expiration=" + readExpiration)
	if err != nil {
		t.Fatalf("cached chain read while Gamma job was blocked: %v", err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("cached chain response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	if body["source"] != "sqlite" || body["expiration"] != readExpiration {
		t.Fatalf("cached chain response=%#v, want persisted %s chain", body, readExpiration)
	}
	releaseChains()
	status := waitForGammaStatus(t, service, "completed", 45)
	if status["status"] != "completed" {
		t.Fatalf("Gamma status after releasing provider=%#v", status)
	}
}

func TestSuccessfulEmptyExpirationsPreserveCatalog(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "empty-expirations.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.SaveExpirations("QQQ", []string{time.Now().AddDate(0, 0, 14).Format("2006-01-02")}); err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{expirations: []string{}, quote: model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	response, err := service.refreshSnapshot(context.Background(), "QQQ", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !response.QuoteOnly || len(response.Expirations) != 0 {
		t.Fatalf("expected quote-only with empty catalog, got %#v", response)
	}
	values, found, err := repository.LatestExpirations("QQQ")
	if err != nil || !found || len(values) != 1 || values[0] != time.Now().AddDate(0, 0, 14).Format("2006-01-02") {
		t.Fatalf("previous non-empty catalog should survive empty upstream response: values=%#v found=%v err=%v", values, found, err)
	}
	if got := fake.chainCalls.Load(); got != 0 {
		t.Fatalf("chain calls=%d after no expirations, want 0", got)
	}
}

func TestEmptyExpirationRefreshPreservesCachedCatalogOverHTTP(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "expiration-http.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	oldExpiry := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	if err := repository.SaveExpirations("QQQ", []string{oldExpiry}); err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{expirations: []string{}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	request := func(refresh bool) map[string]any {
		t.Helper()
		path := ts.URL + "/api/expirations/QQQ"
		if refresh {
			path += "?refresh=true"
		}
		response, err := http.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("expiration response status=%d decode=%v body=%#v", response.StatusCode, err, body)
		}
		return body
	}
	upstream := request(true)
	if upstream["source"] != "upstream" || len(upstream["expirations"].([]any)) != 0 {
		t.Fatalf("empty upstream response should remain visible to caller: %#v", upstream)
	}
	cached := request(false)
	values, ok := cached["expirations"].([]any)
	if cached["source"] != "sqlite" || !ok || len(values) != 1 || values[0] != oldExpiry {
		t.Fatalf("empty refresh erased cached expiration catalog: %#v", cached)
	}
}

func TestExpirationsReadsSharedCatalogAndTreatsExpiredOnlyCacheAsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiration-shared-cache.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	if err := repository.SaveExpirations("QQQ", []string{first}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{expirationErr: fmt.Errorf("upstream unavailable")})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	get := func(path string) (int, map[string]any) {
		t.Helper()
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}

	status, body := get("/api/expirations/QQQ")
	if status != http.StatusOK || body["source"] != "sqlite" || !reflect.DeepEqual(body["expirations"], []any{first}) {
		t.Fatalf("initial expiration response status=%d body=%#v", status, body)
	}
	second := time.Now().AddDate(0, 0, 21).Format("2006-01-02")
	if err := repository.SaveExpirations("QQQ", []string{second}); err != nil {
		t.Fatal(err)
	}
	status, body = get("/api/expirations/QQQ")
	if status != http.StatusOK || body["source"] != "sqlite" || !reflect.DeepEqual(body["expirations"], []any{second}) {
		t.Fatalf("shared catalog update was hidden by process cache: status=%d body=%#v", status, body)
	}
	past := "2000-01-01"
	if err := repository.SaveExpirations("QQQ", []string{past}); err != nil {
		t.Fatal(err)
	}
	status, body = get("/api/expirations/QQQ")
	if status != http.StatusOK || body["source"] != "pending" || !reflect.DeepEqual(body["expirations"], []any{}) {
		t.Fatalf("expired-only catalog should match Python pending response: status=%d body=%#v", status, body)
	}
	status, body = get("/api/expirations/QQQ?refresh=true")
	if status != http.StatusBadGateway || body["detail"] != "upstream unavailable" {
		t.Fatalf("expired-only catalog should not mask refresh failure: status=%d body=%#v", status, body)
	}
}

func TestExpirationsDistinguishesCatalogFromReadableSQLiteBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiration-readable-batches.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	if err := repository.SaveExpirations("QQQ", []string{first, second}); err != nil {
		t.Fatal(err)
	}
	strike := 100.0
	now := time.Now().UTC()
	if err := repository.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: second, FetchedAt: &now, Data: []model.OptionRow{{
		ContractSymbol: "QQQ-C-100", ContractType: "call", Strike: &strike,
	}}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{expirationErr: fmt.Errorf("upstream unavailable")})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/expirations/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body model.ExpirationsResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("expiration response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	if !reflect.DeepEqual(body.Expirations, []string{first, second}) {
		t.Errorf("catalog dates=%v, want both provider dates", body.Expirations)
	}
	if !reflect.DeepEqual(body.CachedExpirations, []string{second}) {
		t.Fatalf("readable cached dates=%v, want only %s", body.CachedExpirations, second)
	}
	chainResponse, err := http.Get(ts.URL + "/api/chain/QQQ?expiration=" + second)
	if err != nil {
		t.Fatal(err)
	}
	defer chainResponse.Body.Close()
	var chain map[string]any
	if err := json.NewDecoder(chainResponse.Body).Decode(&chain); err != nil || chainResponse.StatusCode != http.StatusOK {
		t.Fatalf("chain response status=%d decode=%v body=%#v", chainResponse.StatusCode, err, chain)
	}
	rows, _ := chain["data"].([]any)
	if chain["source"] != "sqlite" || len(rows) != 1 {
		t.Fatalf("cached chain not returned on cold service: %#v", chain)
	}
}

func TestCleanupLeaseSerializesSameServiceCallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-local-lock.db")
	firstStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, firstStore, &testMarketProvider{})
	defer service.Close()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	operation := func() error {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return nil
	}
	done := make(chan error, 2)
	go func() { done <- service.withCleanupLease(operation) }()
	<-entered
	go func() { done <- service.withCleanupLease(operation) }()
	select {
	case <-entered:
		t.Fatal("second cleanup callback overlapped first callback")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("cleanup callback %d: %v", i, err)
		}
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent cleanup callbacks=%d, want 1", got)
	}
}

func TestCleanupLeaseRejectsOtherServiceAndReleasesManualLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-durable-lock.db")
	ownerStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workerStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := NewWithProvider(config.Config{DatabasePath: path}, ownerStore, &testMarketProvider{})
	worker := NewWithProvider(config.Config{DatabasePath: path}, workerStore, &testMarketProvider{})
	defer owner.Close()
	defer worker.Close()

	started, release := make(chan struct{}), make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- owner.withCleanupLease(func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	called := false
	err = worker.withCleanupLease(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, errCleanupLeaseBusy) || called {
		t.Fatalf("second service cleanup err=%v called=%v, want busy without mutation", err, called)
	}
	close(release)
	if err := <-ownerDone; err != nil {
		t.Fatalf("owner cleanup: %v", err)
	}
	if err := worker.withCleanupLease(func() error { return nil }); err != nil {
		t.Fatalf("cleanup after lease release: %v", err)
	}

	for _, callbackErr := range []error{nil, errors.New("simulated cleanup failure")} {
		err := worker.withCleanupLease(func() error { return callbackErr })
		if !errors.Is(err, callbackErr) {
			t.Fatalf("callback error=%v, want %v", err, callbackErr)
		}
		acquired, leaseErr := ownerStore.TryAcquireServiceLease(housekeepingLeaseName, "probe-owner", time.Minute)
		if leaseErr != nil || !acquired {
			t.Fatalf("manual lease not released after callback err=%v: acquired=%v leaseErr=%v", callbackErr, acquired, leaseErr)
		}
		if err := ownerStore.ReleaseServiceLease(housekeepingLeaseName, "probe-owner"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHousekeepingLeaseIsBorrowedByManualCleanupAndNotReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-housekeeping-lease.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, &testMarketProvider{})
	defer service.Close()
	if !service.runHousekeepingPass(true) {
		t.Fatal("failed to acquire initial housekeeping lease")
	}
	if err := service.withCleanupLease(func() error { return nil }); err != nil {
		t.Fatalf("manual cleanup did not borrow housekeeping lease: %v", err)
	}
	if acquired, err := repository.TryAcquireServiceLease(housekeepingLeaseName, "other-process", time.Minute); err != nil || acquired {
		t.Fatalf("manual cleanup released long-lived housekeeping lease: acquired=%v err=%v", acquired, err)
	}
	service.releaseHousekeepingLease()
	if acquired, err := repository.TryAcquireServiceLease(housekeepingLeaseName, "other-process", time.Minute); err != nil || !acquired {
		t.Fatalf("housekeeping release did not release lease: acquired=%v err=%v", acquired, err)
	}
}

func TestInitialHousekeepingPrunesLegacyRawJSONWithoutSizeCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "housekeeping-legacy-raw-json.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := sql.Open("sqlite", path)
	if err != nil {
		repository.Close()
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := fixtureDB.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('QQQ', ?, 'python', '{"legacy":true}')`, stamp); err != nil {
		fixtureDB.Close()
		repository.Close()
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	cfg := config.Config{DatabasePath: path, RawRetentionDays: 30, SizeCleanupBudget: time.Second}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	defer service.Close()
	if !service.runHousekeepingPass(true) {
		t.Fatal("failed to acquire initial housekeeping lease")
	}
	fixtureDB, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer fixtureDB.Close()
	var raw sql.NullString
	if err := fixtureDB.QueryRow(`SELECT raw_json FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Fatalf("initial housekeeping left legacy raw_json=%q", raw.String)
	}

	// Subsequent housekeeping passes do not repeat the one-time migration.
	if _, err := fixtureDB.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider, raw_json)
VALUES ('SPY', ?, 'python', '{"legacy":true}')`, stamp); err != nil {
		t.Fatal(err)
	}
	if !service.runHousekeepingPass(false) {
		t.Fatal("subsequent housekeeping pass lost its lease")
	}
	service.releaseHousekeepingLease()
	if err := fixtureDB.QueryRow(`SELECT raw_json FROM quote_snapshots WHERE symbol='SPY'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Valid {
		t.Fatal("subsequent housekeeping unexpectedly reran one-time raw_json pruning")
	}
}

func TestCleanupHTTPReturnsUnavailableWhenOtherProcessOwnsLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-http-lease.db")
	ownerStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workerStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, workerStore, &testMarketProvider{})
	defer service.Close()
	if acquired, err := ownerStore.TryAcquireServiceLease(housekeepingLeaseName, "foreign-housekeeper", 48*time.Hour); err != nil || !acquired {
		t.Fatalf("failed to seed foreign cleanup lease: acquired=%v err=%v", acquired, err)
	}
	defer ownerStore.ReleaseServiceLease(housekeepingLeaseName, "foreign-housekeeper")

	server := httptest.NewServer(service.Router())
	defer server.Close()
	response, err := http.Post(server.URL+"/api/cleanup", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("cleanup status=%d body=%#v, want 503 while another process owns the cleanup lease", response.StatusCode, body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "database cleanup is already running") {
		t.Fatalf("cleanup response detail=%#v is not explanatory", body["detail"])
	}
}

func TestCleanupHTTPDeletesExpiredDataAndReturnsMetrics(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "cleanup-http.db"), RawRetentionDays: 0, UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().AddDate(0, 0, -90)
	fixtureDB, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureDB.Exec(`INSERT INTO refresh_runs(symbol, started_at, status) VALUES ('OLD', ?, 'success')`, oldTime.Format(time.RFC3339Nano)); err != nil {
		fixtureDB.Close()
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveQuote(model.Quote{Symbol: "OLD", Provider: "test", FetchedAt: &oldTime, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	retainedTime := time.Now().UTC().AddDate(0, 0, -15)
	if err := repository.SaveQuote(model.Quote{Symbol: "RETAINED", Provider: "test", Price: new(float64), FetchedAt: &retainedTime, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if quote, found, err := repository.LatestQuote("RETAINED"); err != nil || !found || quote.FetchedAt == nil || !quote.FetchedAt.Equal(retainedTime.Truncate(time.Microsecond)) {
		t.Fatalf("saved retained quote timestamp=%v found=%v err=%v, want %v", quote.FetchedAt, found, err, retainedTime)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	if service.cfg.RawRetentionDays != 30 {
		t.Fatalf("retention days=%d, want default 30", service.cfg.RawRetentionDays)
	}
	service.quotes["OLD"] = cachedQuote{value: model.Quote{Symbol: "OLD", Provider: "test", Price: new(float64), FetchedAt: &oldTime}, at: oldTime}
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Post(ts.URL+"/api/cleanup", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("cleanup status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	deleted, ok := body["deleted"].(map[string]any)
	if _, retained, err := repository.LatestQuote("RETAINED"); err != nil || !retained {
		t.Fatalf("15-day quote should survive the 30-day fallback: retained=%v err=%v deleted=%#v", retained, err, deleted)
	}
	if !ok || deleted["quotes"] != float64(1) || deleted["runs"] != float64(1) {
		t.Fatalf("cleanup deleted metrics=%#v, want one quote and one old run", body["deleted"])
	}
	size, ok := body["size"].(map[string]any)
	if !ok || size["before_bytes"] == nil || size["after_bytes"] == nil || size["live_bytes"] == nil || size["stale_runs"] != float64(0) {
		t.Fatalf("cleanup size metrics missing/incorrect: %#v", body["size"])
	}
	for _, key := range []string{"size_cap_enabled", "before_retention_bytes", "after_retention_bytes", "live_retention_bytes"} {
		if _, exists := size[key]; exists {
			t.Fatalf("cleanup size includes non-Python field %q: %#v", key, size)
		}
	}
	service.mu.RLock()
	_, staleCacheRemains := service.quotes["OLD"]
	service.mu.RUnlock()
	if staleCacheRemains {
		t.Fatal("expired in-memory quote survived cleanup")
	}
}

func TestStatusReportsActiveQuoteRefresh(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "status-refresh.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
		quote: model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	getStatus := func() map[string]any {
		t.Helper()
		response, err := http.Get(ts.URL + "/api/status/QQQ")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("status response=%#v code=%d err=%v", body, response.StatusCode, err)
		}
		return body
	}
	initial := getStatus()
	if initial["quote"] != nil || initial["refresh"] != nil {
		t.Fatalf("empty status should match Python null refresh state: %#v", initial)
	}

	quoteDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Post(ts.URL+"/api/refresh/QQQ?max_age=0", "application/json", strings.NewReader("{}"))
		if requestErr != nil {
			quoteDone <- requestErr
			return
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			quoteDone <- fmt.Errorf("quote refresh status=%d", response.StatusCode)
			return
		}
		quoteDone <- nil
	}()
	select {
	case <-fake.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("quote refresh did not reach provider")
	}
	active := getStatus()
	activeRefresh, _ := active["refresh"].(map[string]any)
	if activeRefresh["status"] != "running" {
		t.Fatalf("status did not report active refresh: %#v", active)
	}
	if activeRefresh["id"] == nil || activeRefresh["started_at"] == nil {
		t.Fatalf("active refresh should expose persisted running metadata: %#v", activeRefresh)
	}
	close(fake.quoteRelease)
	if err := <-quoteDone; err != nil {
		t.Fatal(err)
	}
	completed := getStatus()
	refresh, ok := completed["refresh"].(map[string]any)
	if !ok || refresh["status"] != "success" || refresh["finished_at"] == nil || refresh["rows_written"] != float64(0) {
		t.Fatalf("completed refresh run status mismatch: %#v", completed["refresh"])
	}
	if _, ok := refresh["id"].(float64); !ok {
		t.Fatalf("refresh run missing persisted id: %#v", refresh)
	}
	quote, ok := completed["quote"].(map[string]any)
	if !ok || quote["price"] != float64(100) || quote["source"] != "sqlite" {
		t.Fatalf("status quote payload not compatible: %#v", completed["quote"])
	}
}

func TestStatusDoesNotMixQuoteRefreshWithHistoricalRun(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "status-historical-run.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	oldRunID, err := repository.StartRefreshRun("QQQ")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishRefreshRun(oldRunID, "success", 12, ""); err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
		quote: model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	quoteDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
		if requestErr != nil {
			quoteDone <- requestErr
			return
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			quoteDone <- fmt.Errorf("quote refresh status=%d", response.StatusCode)
			return
		}
		quoteDone <- nil
	}()
	select {
	case <-fake.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("direct quote refresh did not reach provider")
	}
	response, err := http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response=%#v code=%d err=%v", body, response.StatusCode, decodeErr)
	}
	refresh, ok := body["refresh"].(map[string]any)
	if !ok || refresh["status"] != "running" || refresh["id"] != nil || refresh["finished_at"] != nil {
		t.Fatalf("active quote request mixed with historical refresh run: %#v", body["refresh"])
	}
	close(fake.quoteRelease)
	if err := <-quoteDone; err != nil {
		t.Fatal(err)
	}
	latest, found, err := service.store.LatestRefreshRun("QQQ")
	if err != nil || !found || latest["id"] != oldRunID || latest["status"] != "success" {
		t.Fatalf("quote-only refresh unexpectedly rewrote snapshot run: %#v found=%v err=%v", latest, found, err)
	}
}

func TestStatusRestoresLatestRefreshRunAfterServiceRestart(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "status-restart.db")
	cfg := config.Config{DatabasePath: databasePath, UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 123.0
	first := NewWithProvider(cfg, repository, &testMarketProvider{
		quote: model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
	})
	firstServer := httptest.NewServer(first.Router())
	response, err := http.Post(firstServer.URL+"/api/refresh/QQQ?max_age=0", "application/json", strings.NewReader("{}"))
	if err != nil {
		firstServer.Close()
		_ = first.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		firstServer.Close()
		_ = first.Close()
		t.Fatalf("initial refresh status=%d, want 200", response.StatusCode)
	}
	firstServer.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	second := NewWithProvider(cfg, reopened, &testMarketProvider{})
	defer second.Close()
	secondServer := httptest.NewServer(second.Router())
	defer secondServer.Close()
	response, err = http.Get(secondServer.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("restarted status response=%#v code=%d err=%v", body, response.StatusCode, err)
	}
	quote, ok := body["quote"].(map[string]any)
	if !ok || quote["price"] != float64(123) {
		t.Fatalf("restart did not restore cached quote: %#v", body["quote"])
	}
	refresh, ok := body["refresh"].(map[string]any)
	if !ok || refresh["status"] != "success" || refresh["id"] == nil || refresh["finished_at"] == nil {
		t.Fatalf("restart did not restore latest refresh run: %#v", body["refresh"])
	}
}

func TestUnavailableExpirationFinishesRefreshRunAsFailed(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "status-expiration-failed.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	available := time.Now().AddDate(0, 0, 15).Format("2006-01-02")
	requested := time.Now().AddDate(0, 0, 8).Format("2006-01-02")
	fake := &testMarketProvider{
		expirations: []string{available},
		quote:       model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Post(ts.URL+"/api/refresh/QQQ?max_age=0&expiration="+requested, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unavailable expiration status=%d, want 404", response.StatusCode)
	}
	latest, found, err := service.store.LatestRefreshRun("QQQ")
	if err != nil || !found || latest["status"] != "failed" || latest["finished_at"] == nil {
		t.Fatalf("unavailable expiration left unfinished refresh run: latest=%#v found=%v err=%v", latest, found, err)
	}
	message, ok := latest["error_message"].(string)
	if !ok || !strings.Contains(message, requested) {
		t.Fatalf("refresh run missing unavailable-expiration error: %#v", latest)
	}
	if calls := fake.chainCalls.Load(); calls != 0 {
		t.Fatalf("provider chain called for unavailable expiration %q: calls=%d", requested, calls)
	}
}

func TestFailedRefreshRunAppearsInStatus(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "status-failed.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{quoteErr: fmt.Errorf("provider unavailable")}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Post(ts.URL+"/api/refresh/QQQ?max_age=0", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed refresh status=%d, want 502", response.StatusCode)
	}
	response, err = http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response=%#v code=%d err=%v", body, response.StatusCode, err)
	}
	refresh, ok := body["refresh"].(map[string]any)
	if !ok || refresh["status"] != "failed" || refresh["error_message"] != "provider unavailable" || refresh["finished_at"] == nil {
		t.Fatalf("failed refresh not persisted to status: %#v", body["refresh"])
	}
}

func TestStatusReportsRefreshQueuedBeforeSemaphoreAcquire(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "status-queued.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	defer service.Close()
	// Model the deterministic interval after a caller has joined a gate but before
	// it can take the semaphore; status should include queued refresh requests.
	service.mu.Lock()
	service.refreshGates[snapshotRefreshGateKey("QQQ", "")] = &refreshGate{sem: make(chan struct{}, 1), refs: 1}
	service.mu.Unlock()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response=%#v code=%d err=%v", body, response.StatusCode, err)
	}
	refresh, ok := body["refresh"].(map[string]any)
	if !ok || refresh["status"] != "running" {
		t.Fatalf("queued refresh should report running before taking semaphore: %#v", body["refresh"])
	}
}

func TestStatusRefreshesMissingHistoryWithoutFetchingEarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status-no-upstream.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, HistoryMaxAge: time.Hour}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 110.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, FetchedAt: &now, Sessions: nil}); err != nil {
		t.Fatal(err)
	}
	provider := &statusTrackingProvider{
		testMarketProvider: &testMarketProvider{},
		historyErr:         fmt.Errorf("history unavailable"),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	quote, ok := body["quote"].(map[string]any)
	if !ok {
		t.Fatalf("status quote=%#v, want object", body["quote"])
	}
	if sessions, ok := quote["sessions"].(map[string]any); !ok || sessions == nil {
		t.Fatalf("status did not normalize empty sessions: %#v", quote["sessions"])
	}
	if _, exists := quote["earnings"]; exists {
		t.Fatalf("status exposed the removed earnings field: %#v", quote["earnings"])
	}
	if got := provider.historyCalls.Load(); got != 1 {
		t.Fatalf("status history upstream calls=%d, want one attempt", got)
	}
	if got := provider.earningsCalls.Load(); got != 0 {
		t.Fatalf("status made earnings upstream calls=%d, want zero", got)
	}
}

func TestStatusUsesQuoteEnrichmentForFreshHistoryWithoutEarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status-upstream-enrichment.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, HistoryMaxAge: 24 * time.Hour, UpstreamConcurrency: 2}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, stalePrevious := 110.0, 99.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &price, PreviousClose: &stalePrevious, MarketState: "POST",
		Provider: "python", FetchedAt: &now, Sessions: nil,
	}); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(marketLocation())
	latestDay := today.AddDate(0, 0, -1).Format("2006-01-02")
	previousDay := today.AddDate(0, 0, -2).Format("2006-01-02")
	provider := &statusTrackingProvider{
		testMarketProvider: &testMarketProvider{},
		history: provider.HistoryResult{
			FetchedAt: now,
			Bars: []map[string]any{
				{"date": previousDay, "open": 100.0, "high": 104.0, "low": 98.0, "close": 102.0, "volume": 1000},
				{"date": latestDay, "open": 103.0, "high": 108.0, "low": 101.0, "close": 105.0, "volume": 1500},
			},
		},
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	quote, ok := body["quote"].(map[string]any)
	if !ok {
		t.Fatalf("status quote=%#v, want object", body["quote"])
	}
	if quote["previous_close"] != float64(105) || quote["change_percent"] != (float64(110-105)/105*100) {
		t.Fatalf("status did not align quote with refreshed history: %#v", quote)
	}
	if _, exists := quote["earnings"]; exists {
		t.Fatalf("status exposed the removed earnings field: %#v", quote["earnings"])
	}
	if provider.historyCalls.Load() != 1 || provider.earningsCalls.Load() != 0 {
		t.Fatalf("status enrichment calls: history=%d earnings=%d, want history only", provider.historyCalls.Load(), provider.earningsCalls.Load())
	}
}

func TestStatusDecoratesPersistedQuoteWithoutUpstreamAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status-decoration.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, HistoryMaxAge: 24 * time.Hour}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, stalePrevious := 110.0, 99.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &price, PreviousClose: &stalePrevious, MarketState: "POST",
		Provider: "python", FetchedAt: &now, Sessions: nil,
	}); err != nil {
		t.Fatal(err)
	}
	latestDay := time.Now().In(marketLocation()).AddDate(0, 0, -1).Format("2006-01-02")
	previousDay := time.Now().In(marketLocation()).AddDate(0, 0, -2).Format("2006-01-02")
	if err := repository.WriteHistory("QQQ", []map[string]any{
		{"date": previousDay, "open": 100.0, "high": 104.0, "low": 98.0, "close": 102.0, "volume": 1000},
		{"date": latestDay, "open": 103.0, "high": 108.0, "low": 101.0, "close": 105.0, "volume": 1500},
	}, now); err != nil {
		t.Fatal(err)
	}
	earningsFetched := now.Add(-time.Minute)
	if err := repository.WritePayload("earnings_snapshots", "QQQ", map[string]any{"dates": []string{"2099-01-18"}}, earningsFetched); err != nil {
		t.Fatal(err)
	}
	provider := &testMarketProvider{}
	first := NewWithProvider(cfg, repository, provider)
	firstServer := httptest.NewServer(first.Router())
	quoteResponse, err := http.Get(firstServer.URL + "/api/quote/QQQ")
	if err != nil {
		firstServer.Close()
		_ = first.Close()
		t.Fatal(err)
	}
	var quoteBody map[string]any
	quoteDecodeErr := json.NewDecoder(quoteResponse.Body).Decode(&quoteBody)
	quoteResponse.Body.Close()
	firstServer.Close()
	if quoteDecodeErr != nil || quoteResponse.StatusCode != http.StatusOK {
		_ = first.Close()
		t.Fatalf("quote response status=%d decode=%v body=%#v", quoteResponse.StatusCode, quoteDecodeErr, quoteBody)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secondProvider := &testMarketProvider{}
	second := NewWithProvider(cfg, reopened, secondProvider)
	defer second.Close()
	secondServer := httptest.NewServer(second.Router())
	defer secondServer.Close()
	statusResponse, err := http.Get(secondServer.URL + "/api/status/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer statusResponse.Body.Close()
	var statusBody map[string]any
	if err := json.NewDecoder(statusResponse.Body).Decode(&statusBody); err != nil || statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("status response status=%d decode=%v body=%#v", statusResponse.StatusCode, err, statusBody)
	}
	statusQuote, ok := statusBody["quote"].(map[string]any)
	if !ok {
		t.Fatalf("status quote=%#v, want object", statusBody["quote"])
	}
	if statusQuote["price"] != float64(110) || statusQuote["source"] != "sqlite" {
		t.Fatalf("status quote core fields=%#v", statusQuote)
	}
	if statusQuote["previous_close"] != float64(105) || statusQuote["change_percent"] != (float64(110-105)/105*100) {
		t.Fatalf("status did not align persisted quote with local history: %#v", statusQuote)
	}
	if sessions, ok := statusQuote["sessions"].(map[string]any); !ok || sessions == nil {
		t.Fatalf("status did not normalize empty sessions: %#v", statusQuote["sessions"])
	}
	if _, exists := statusQuote["earnings"]; exists {
		t.Fatalf("status exposed the removed earnings field: %#v", statusQuote["earnings"])
	}
	if secondProvider.quoteCalls.Load() != 0 || secondProvider.fundamentalsCalls.Load() != 0 {
		t.Fatalf("status triggered upstream calls: quote=%d fundamentals=%d", secondProvider.quoteCalls.Load(), secondProvider.fundamentalsCalls.Load())
	}

	quoteAgain, err := http.Get(secondServer.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer quoteAgain.Body.Close()
	var quoteAgainBody map[string]any
	if err := json.NewDecoder(quoteAgain.Body).Decode(&quoteAgainBody); err != nil || quoteAgain.StatusCode != http.StatusOK {
		t.Fatalf("quote-after-restart response status=%d decode=%v body=%#v", quoteAgain.StatusCode, err, quoteAgainBody)
	}
	for _, field := range []string{"price", "previous_close", "change_percent", "fair_value_status", "fair_value_pending", "sessions"} {
		if !reflect.DeepEqual(statusQuote[field], quoteAgainBody[field]) {
			t.Fatalf("status and quote field %s differ: status=%#v quote=%#v", field, statusQuote[field], quoteAgainBody[field])
		}
	}
	if _, exists := quoteAgainBody["earnings"]; exists {
		t.Fatalf("quote endpoint exposed the removed earnings field: %#v", quoteAgainBody["earnings"])
	}
}

func TestQuoteHTTPContractIncludesPersistedValuationFields(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "quote-contract.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price, todayOpen, previousClose := 405.0, 399.0, 400.0
	fairValue, forwardEPS, confidenceScore, safetyMargin := 430.0, 10.5, 0.84, 0.12
	epsSource, confidence, regime := "analyst_consensus", "high", "growth"
	fake := &testMarketProvider{quote: model.Quote{
		Price: &price, TodayOpen: &todayOpen, PreviousClose: &previousClose,
		Currency: "USD", MarketState: "REGULAR", Provider: "test", Sessions: map[string]any{},
		FairValue: fairValue, FairValueSource: "valuation_test", FairValueModel: "owner_earnings",
		FairValueForwardEPS: &forwardEPS, FairValueForwardEPSSource: &epsSource,
		FairValueConfidence: &confidence, FairValueConfidenceScore: &confidenceScore,
		FairValueSafetyMargin: &safetyMargin, FairValueRegime: &regime,
		FairValueRegimeSignals:     map[string]any{"trend": "positive"},
		FairValueDefensive:         map[string]any{"value": 390.0},
		FairValueQuarterlyMomentum: []any{map[string]any{"quarter": "Q1", "growth": 0.08}},
	}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	requestQuote := func(refresh bool) map[string]any {
		t.Helper()
		path := "/api/quote/QQQ"
		if refresh {
			path += "?refresh=true"
		}
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("quote response status=%d decode=%v body=%#v", response.StatusCode, err, body)
		}
		return body
	}
	assertFields := func(body map[string]any) {
		t.Helper()
		if body["source"] == "upstream" {
			for _, field := range []string{"id", "raw_json", "fair_value_methods_json", "fair_value_historical_valuation_percentiles_json"} {
				if _, present := body[field]; present {
					t.Errorf("upstream quote unexpectedly includes SQLite column %q: %#v", field, body)
				}
			}
		}
		for field, want := range map[string]any{
			"today_open": float64(399), "fair_value": float64(430),
			"fair_value_forward_eps": float64(10.5), "fair_value_forward_eps_source": "analyst_consensus",
			"fair_value_confidence": "high", "fair_value_safety_margin": float64(0.12),
			"fair_value_regime": "growth", "fair_value_source": "valuation_test",
		} {
			if got := body[field]; got != want {
				t.Errorf("quote field %s=%#v, want %#v", field, got, want)
			}
		}
		for _, field := range []string{"fair_value_regime_signals", "fair_value_defensive", "fair_value_quarterly_momentum"} {
			if body[field] == nil {
				t.Errorf("quote response missing nested valuation field %q", field)
			}
		}
	}
	assertFields(requestQuote(true))
	if _, found, err := repository.LatestQuote("QQQ"); err != nil || found {
		t.Fatalf("direct quote refresh persisted a snapshot: found=%v err=%v", found, err)
	}
	persisted := fake.quote
	persisted.Symbol = "QQQ"
	fetchedAt := time.Now().UTC()
	persisted.FetchedAt = &fetchedAt
	if err := repository.SaveQuote(persisted); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	delete(service.quotes, "QQQ") // Force the next read through SQLite, as after process restart.
	service.mu.Unlock()
	persistedResponse := requestQuote(false)
	assertFields(persistedResponse)
	if persistedResponse["source"] != "sqlite" {
		t.Fatalf("persisted quote source=%v, want sqlite", persistedResponse["source"])
	}
}

func TestQuoteRefreshPreservesCachedValuationOverHTTP(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "quote-refresh.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	oldAt := time.Now().UTC().Add(-time.Hour)
	oldPrice, fairValue, eps, confidenceScore := 400.0, 430.0, 10.5, 0.84
	confidence, source := "high", "valuation_test"
	if err := repository.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &oldPrice, Provider: "cache", FetchedAt: &oldAt,
		FairValue: fairValue, FairValueSource: source, FairValueForwardEPS: &eps,
		FairValueConfidence: &confidence, FairValueConfidenceScore: &confidenceScore,
		FairValueDefensive: map[string]any{"value": 390.0}, Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	newPrice := 405.0
	fake := &testMarketProvider{quote: model.Quote{Price: &newPrice, Provider: "upstream", Sessions: map[string]any{}}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Get(ts.URL + "/api/quote/QQQ?refresh=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("quote refresh status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	for field, want := range map[string]any{
		"price": float64(405), "fair_value": float64(430),
		"fair_value_source": "valuation_test", "fair_value_forward_eps": float64(10.5),
		"fair_value_confidence": "high", "fair_value_confidence_score": float64(0.84),
	} {
		if body[field] != want {
			t.Errorf("refresh field %s=%#v, want %#v", field, body[field], want)
		}
	}
	if _, ok := body["fair_value_defensive"].(map[string]any); !ok {
		t.Errorf("refresh lost cached defensive valuation: %#v", body["fair_value_defensive"])
	}
}

func TestSSEStartsAtLatestUnlessResuming(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "sse-cursor.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{quoteRelease: make(chan struct{})}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	oldID, err := service.store.AddEvent(model.Event{Symbol: "QQQ", Kind: "snapshot", Expiration: "2030-01-18", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := service.store.AddEvent(model.Event{Symbol: "QQQ", Kind: "quote", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}

	openStream := func(lastEventID string) (*http.Response, <-chan string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, ts.URL+"/api/events/QQQ", nil)
		if err != nil {
			t.Fatal(err)
		}
		if lastEventID != "" {
			request.Header.Set("Last-Event-ID", lastEventID)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("SSE status=%d", response.StatusCode)
		}
		frames := make(chan string, 8)
		go func() {
			defer close(frames)
			scanner := bufio.NewScanner(response.Body)
			var frame strings.Builder
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					if frame.Len() > 0 {
						frames <- frame.String()
						frame.Reset()
					}
					continue
				}
				frame.WriteString(line)
				frame.WriteByte('\n')
			}
		}()
		return response, frames
	}
	readFrame := func(response *http.Response, frames <-chan string) string {
		t.Helper()
		select {
		case frame := <-frames:
			return frame
		case <-time.After(2 * time.Second):
			response.Body.Close()
			t.Fatal("timed out waiting for SSE frame")
			return ""
		}
	}

	response, frames := openStream("")
	firstFrame := readFrame(response, frames)
	if !strings.Contains(firstFrame, "event: ready") {
		response.Body.Close()
		t.Fatalf("new connection should start with ready frame, got %q", firstFrame)
	}
	freshID, err := service.store.AddEvent(model.Event{Symbol: "QQQ", Kind: "quote", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	freshFrame := readFrame(response, frames)
	response.Body.Close()
	if !strings.Contains(freshFrame, fmt.Sprintf("id: %d", freshID)) || !strings.Contains(freshFrame, "event: update") {
		t.Fatalf("new connection should ignore old events and receive new event %d; got %q", freshID, freshFrame)
	}

	response, frames = openStream(fmt.Sprint(oldID))
	resumedFrame := readFrame(response, frames)
	response.Body.Close()
	if !strings.Contains(resumedFrame, fmt.Sprintf("id: %d", newID)) || !strings.Contains(resumedFrame, "event: update") {
		t.Fatalf("resume should replay only events after cursor %d; got %q want id %d", oldID, resumedFrame, newID)
	}
	response, frames = openStream("0")
	zeroCursorFrame := readFrame(response, frames)
	response.Body.Close()
	if !strings.Contains(zeroCursorFrame, "event: ready") {
		t.Fatalf("zero cursor should be treated as a new connection; got %q", zeroCursorFrame)
	}
}

func TestCachedTrendMatchesPythonWindowContract(t *testing.T) {
	bars := func(count int, close func(int) float64) []map[string]any {
		result := make([]map[string]any, count)
		for index := range result {
			result[index] = map[string]any{"close": close(index)}
		}
		return result
	}
	if trend := cachedTrend(bars(19, func(index int) float64 { return float64(100 + index) })); trend != nil {
		t.Fatalf("19 valid closes should not produce trend: %#v", trend)
	}
	recent := cachedTrend(bars(20, func(index int) float64 { return float64(100 + index) }))
	if recent == nil || recent["bars"] != 20 || recent["background_direction"] != "up" {
		t.Fatalf("20-bar trend should use the recent window: %#v", recent)
	}
	long := cachedTrend(bars(80, func(index int) float64 { return float64(200 - index) }))
	if long == nil || long["bars"] != 60 || long["background_direction"] != "down" {
		t.Fatalf("long trend should use the 60-bar window: %#v", long)
	}
	if _, ok := recent["rsi"].(map[string]any); !ok {
		t.Fatalf("trend should include RSI metadata: %#v", recent)
	}
}

func TestTrendMarketMatchesPythonSessionAndHistoryFallbacks(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }
	bars := []map[string]any{
		{"date": "2026-09-18", "open": float64(100), "close": float64(105)},
		{"date": "2026-09-21", "open": float64(110), "close": float64(120)},
	}

	regular := trendMarketAt(bars, model.Quote{MarketState: "REGULAR"}, "2026-09-21")
	if regular["today_open"] != float64(110) || regular["previous_close"] != float64(105) || regular["previous_close_date"] != "2026-09-18" || regular["previous_open"] != float64(100) {
		t.Fatalf("regular-session values mismatch: %#v", regular)
	}

	post := trendMarketAt(bars, model.Quote{MarketState: "POST", TodayOpen: ptr(111)}, "2026-09-22")
	if post["today_open"] != float64(111) || post["today_open_date"] != "2026-09-22" || post["previous_close"] != float64(120) || post["previous_open"] != float64(110) {
		t.Fatalf("post-session fallback mismatch: %#v", post)
	}

	staleBars := []map[string]any{{"date": "2026-09-18", "open": float64(100), "close": float64(147.61)}}
	overnight := trendMarketAt(staleBars, model.Quote{
		MarketState: "OVERNIGHT", PreviousClose: ptr(147.61),
		Sessions: map[string]any{"post": map[string]any{
			"reference_close": float64(148.60), "as_of": "2026-09-21T19:59:00-04:00",
		}},
	}, "2026-09-22")
	if overnight["previous_close"] != float64(148.60) || overnight["previous_close_date"] != "2026-09-21" {
		t.Fatalf("extended-session reference close did not replace stale daily close: %#v", overnight)
	}

	premarket := trendMarketAt(staleBars, model.Quote{
		MarketState: "PRE", Sessions: map[string]any{"pre": map[string]any{
			"reference_close": float64(148.60), "as_of": "2026-09-21T08:00:00-04:00",
		}},
	}, "2026-09-22")
	if premarket["previous_close"] != float64(148.60) || premarket["today_open"] != float64(100) || premarket["previous_open"] != float64(100) {
		t.Fatalf("premarket fallback mismatch: %#v", premarket)
	}

	todayOnly := []map[string]any{{"date": "2026-09-21", "open": float64(110), "close": float64(120)}}
	singleRegular := trendMarketAt(todayOnly, model.Quote{MarketState: "REGULAR", TodayOpen: ptr(111), PreviousClose: ptr(105)}, "2026-09-21")
	previousCloseFallback, previousCloseOK := singleRegular["previous_close"].(*float64)
	if singleRegular["today_open"] != float64(110) || singleRegular["previous_open"] != nil || !previousCloseOK || previousCloseFallback == nil || *previousCloseFallback != 105 || singleRegular["previous_close_date"] != nil {
		t.Fatalf("single current-day bar should fall back to quote previous close: %#v", singleRegular)
	}
	singlePre := trendMarketAt(todayOnly, model.Quote{MarketState: "PRE", TodayOpen: ptr(111)}, "2026-09-22")
	if singlePre["today_open"] != float64(110) || singlePre["previous_open"] != float64(110) || singlePre["previous_close"] != float64(120) {
		t.Fatalf("premarket should use last completed daily bar, not quote today_open: %#v", singlePre)
	}
	missingPreviousClose := trendMarketAt(
		[]map[string]any{{"date": "2026-09-18", "open": float64(100), "close": nil}, {"date": "2026-09-21", "open": float64(110), "close": float64(120)}},
		model.Quote{MarketState: "REGULAR", PreviousClose: ptr(105)},
		"2026-09-21",
	)
	closeFallback, closeFallbackOK := missingPreviousClose["previous_close"].(*float64)
	if !closeFallbackOK || closeFallback == nil || *closeFallback != 105 {
		t.Fatalf("missing prior daily close should fall back to quote previous close: %#v", missingPreviousClose)
	}
	malformed := trendMarketAt([]map[string]any{
		{"date": "bad-date", "open": float64(100), "close": float64(105)},
		{"date": "2026-09-21", "open": float64(110), "close": float64(120)},
	}, model.Quote{MarketState: "REGULAR"}, "2026-09-21")
	malformedClose, malformedCloseOK := malformed["previous_close"].(*float64)
	if !malformedCloseOK || malformedClose != nil || malformed["previous_open"] != nil || malformed["previous_close_date"] != nil {
		t.Fatalf("invalid-date historical bar should be excluded: %#v", malformed)
	}
	partial := trendMarketAt([]map[string]any{
		{"date": "2026-09-18", "open": nil, "close": float64(105)},
		{"date": "2026-09-21", "open": float64(110), "close": float64(120)},
	}, model.Quote{MarketState: "REGULAR"}, "2026-09-21")
	if partial["previous_close"] != float64(105) || partial["previous_open"] != nil {
		t.Fatalf("valid partial OHLC bar should preserve its available close: %#v", partial)
	}
}

func TestExpirationRoutesRejectWhitespaceSurroundingISODate(t *testing.T) {
	_, ts := testService(t, "")
	paths := []string{
		"/api/chain/QQQ?expiration=%202030-01-18",
		"/api/flow/QQQ?expiration=2030-01-18%20",
		"/api/events/QQQ?expiration=%202030-01-18",
		"/api/events/QQQ?expiration=",
	}
	for _, path := range paths {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("request %q: %v", path, err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("path %q response=%#v status=%d decodeErr=%v; want 422", path, body, response.StatusCode, decodeErr)
		}
	}
	response, err := http.Post(ts.URL+"/api/refresh/QQQ?expiration=%202030-01-18", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("refresh with whitespace expiration status=%d, want 422", response.StatusCode)
	}
	response.Body.Close()
	response, err = http.Post(ts.URL+"/api/refresh/QQQ?expiration=", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("refresh with explicit empty expiration status=%d, want 422", response.StatusCode)
	}
}

func TestCachedPayloadMetadataTreatsEmptyJSONAsMissing(t *testing.T) {
	if got := cachedSourceValue(map[string]any{}); got != "none" {
		t.Fatalf("empty map source=%q, want none", got)
	}
	if got := cachedWarningValue(map[string]any{}, "missing"); got != "missing" {
		t.Fatalf("empty map warning=%#v, want missing", got)
	}
	if got := cachedSourceValue(map[string]any{"value": 1}); got != "sqlite" {
		t.Fatalf("non-empty map source=%q, want sqlite", got)
	}
	if got := cachedWarningValue(map[string]any{"value": 1}, "missing"); got != nil {
		t.Fatalf("non-empty map warning=%#v, want nil", got)
	}
}

func TestChanAdjustedEndpointUsesCachedAdjustedPriceBasisAndPreviewVersion(t *testing.T) {
	service, ts := testService(t, "")
	fetched := time.Now().UTC()
	bars := []map[string]any{
		{"date": "2026-10-16", "open": 49.0, "high": 53.0, "low": 48.0, "close": 52.0, "volume": 1000.0, "raw_open": 98.0, "raw_high": 106.0, "raw_low": 96.0, "raw_close": 104.0, "raw_volume": 1000.0, "adjustment_factor": 0.5},
	}
	if err := service.store.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", bars, fetched); err != nil {
		t.Fatal(err)
	}
	day, _ := time.ParseInLocation(time.DateOnly, "2026-10-16", marketLocation())
	intraday := make([]map[string]any, 0, 26)
	for index := 0; index < 26; index++ {
		stamp := day.Add(9*time.Hour + 30*time.Minute + time.Duration(index)*15*time.Minute)
		price := 100 + float64(index)/10
		intraday = append(intraday, map[string]any{"date": stamp.Format("2006-01-02 15:04"), "open": price, "high": price + .2, "low": price - .2, "close": price + .1, "volume": 100.0})
	}
	service.intradayHistoryCache["QQQ"] = intradayCacheEntry{frame: intraday, fetchedAt: fetched}
	_, snapshotErr := service.store.RecordChanStructureSnapshot(store.ChanStructureSnapshot{
		Symbol: "QQQ", Timeframe: "1d", RuleVersion: "chan-recursive-v4-preview.2", InputFingerprint: "old-input",
		BarFingerprints:     map[string]string{"2026-10-16": "old-bar"},
		ConfirmedStructures: map[string]json.RawMessage{"segment|level=0|direction=up|start=2026-10-15|end=2026-10-16": json.RawMessage("{\"confirmed\":true}")},
		UpdatedAt:           fetched,
	})
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?spot=110")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	coordinate := payload["price_coordinate"].(map[string]any)
	if response.StatusCode != http.StatusOK || payload["rule_version"] != chananalysis.RecursivePreviewVersion || payload["target_rule_version"] != "chan-recursive-v4.0.0" || coordinate["basis"] != "split_dividend_adjusted" || coordinate["adjusted_spot"] != 55.0 || coordinate["raw_spot"] != 110.0 || coordinate["adjustment_factor"] != 0.5 || coordinate["factor_date"] != "2026-10-16" || coordinate["factor_source"] == "" {
		t.Fatalf("adjusted response status=%d payload=%#v", response.StatusCode, payload)
	}
	daily := payload["daily"].(map[string]any)
	quality := daily["quality"].(map[string]any)
	if quality["price_basis"] != "split_dividend_adjusted" || daily["bar_count"] != float64(1) {
		t.Fatalf("daily adjusted quality/frame = %#v", daily)
	}
	dailyMapping := daily["timeframe_mapping"].(map[string]any)
	weeklyMapping := payload["weekly"].(map[string]any)["timeframe_mapping"].(map[string]any)
	if dailyMapping["lower_timeframe"] != "4h" || dailyMapping["higher_timeframe"] != "1wk" || dailyMapping["lower_timeframe_status"] != "ready" || weeklyMapping["lower_timeframe"] != "1d" || weeklyMapping["lower_timeframe_status"] != "ready" {
		t.Fatalf("adjusted timeframe mapping metadata daily=%#v weekly=%#v", dailyMapping, weeklyMapping)
	}
	revisions, ok := daily["structure_revisions_v4"].([]any)
	if !ok || len(revisions) != 1 {
		t.Fatalf("daily confirmed structure revisions=%#v, want persisted revision", daily["structure_revisions_v4"])
	}
	if revisions[0].(map[string]any)["reason"] != "source_data_revision" {
		t.Fatalf("revision should identify source data change: %#v", revisions[0])
	}
}

func TestChanAdjustedHistoryCoverageDetectsShortLegacyCache(t *testing.T) {
	if adequate, start, end := chanAdjustedHistoryCoverage([]map[string]any{{"date": "2021-10-18"}, {"date": "2026-10-16"}}); !adequate || start != "2021-10-18" || end != "2026-10-16" {
		t.Fatalf("five-year adjusted history coverage=%v %s..%s, want adequate", adequate, start, end)
	}
	if adequate, start, end := chanAdjustedHistoryCoverage([]map[string]any{{"date": "2024-10-16"}, {"date": "2026-10-16"}}); adequate || start != "2024-10-16" || end != "2026-10-16" {
		t.Fatalf("short legacy cache coverage=%v %s..%s, want insufficient", adequate, start, end)
	}
	if adequate, _, _ := chanAdjustedHistoryCoverage([]map[string]any{{"date": "bad-date"}}); adequate {
		t.Fatal("invalid cache dates must not satisfy five-year coverage")
	}
}

func TestChanAdjustedHistoryRefreshRequestsFiveYearContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chan-adjusted-refresh.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &chanAdjustedRecordingProvider{testMarketProvider: &testMarketProvider{}, requests: make(chan string, 1)}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, provider)
	if !service.scheduleChanAdjustedRefresh("QQQ|1d|split_dividend_auto|go-yfinance", "QQQ", true) {
		t.Fatal("five-year adjusted history refresh was not scheduled")
	}
	select {
	case request := <-provider.requests:
		if request != "QQQ|5y" {
			t.Fatalf("Chan adjusted history request=%q, want QQQ|5y", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Chan adjusted history refresh did not reach provider")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdjustedIntradayBarsMapsDateSpecificAdjustedFactor(t *testing.T) {
	daily := []chananalysis.Bar{{Date: "2026-10-16", Open: 50, High: 55, Low: 45, Close: 52, RawOpen: 100, RawHigh: 110, RawLow: 90, RawClose: 104, AdjustmentFactor: .5}}
	input := []chananalysis.Bar{
		{Date: "2026-10-16 09:30", Open: 100, High: 102, Low: 98, Close: 101, Volume: 10},
		{Date: "2026-10-15 09:30", Open: 100, High: 102, Low: 98, Close: 101, Volume: 10},
	}
	got, skipped := adjustedIntradayBars(input, daily)
	if len(got) != 1 || skipped != 1 || got[0].Open != 50 || got[0].High != 51 || got[0].Low != 49 || got[0].Close != 50.5 {
		t.Fatalf("adjusted intraday bars=%+v skipped=%d", got, skipped)
	}
}

func TestSnapshotRefreshDefersAdjustedHistoryPrefetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chan-adjusted-gate.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &chanAdjustedRecordingProvider{testMarketProvider: &testMarketProvider{}, requests: make(chan string, 2)}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, provider)
	defer service.Close()
	finish, _, acquired := service.tryAcquireRefreshGate(snapshotRefreshGateKey("QQQ", ""))
	if !acquired {
		t.Fatal("could not acquire the snapshot refresh gate")
	}
	if service.scheduleChanAdjustedRefresh(chanAdjustedCacheKey("QQQ"), "QQQ", false) {
		t.Fatal("adjusted history started while the snapshot refresh owned the upstream slot")
	}
	service.prefetchSymbolAnalytics("QQQ", false)
	select {
	case request := <-provider.requests:
		t.Fatalf("history request %q started before the snapshot commit", request)
	case <-time.After(100 * time.Millisecond):
	}
	finish("", model.RefreshResponse{}, nil)
	if !service.scheduleChanAdjustedRefresh(chanAdjustedCacheKey("QQQ"), "QQQ", true) {
		t.Fatal("adjusted history did not start after the snapshot commit")
	}
	select {
	case request := <-provider.requests:
		if request != "QQQ|5y" {
			t.Fatalf("Chan adjusted history request=%q, want QQQ|5y", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Chan adjusted history refresh did not reach provider")
	}
}

func TestSnapshotRefreshUsesSingleNearestChainRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nearest-chain.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 123.0
	fetchedAt := time.Now().UTC()
	provider := &nearestChainTestProvider{testMarketProvider: &testMarketProvider{
		quote: model.Quote{Price: &price, FetchedAt: &fetchedAt, Sessions: map[string]any{}},
	}}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, provider)
	defer service.Close()
	response, err := service.refreshSnapshot(context.Background(), "QQQ", "", time.Second)
	if err != nil {
		t.Fatalf("refreshSnapshot() error = %v", err)
	}
	if got := provider.nearestCalls.Load(); got != 1 {
		t.Fatalf("NearestChain calls = %d, want 1", got)
	}
	if got := provider.expirationCalls.Load(); got != 0 {
		t.Fatalf("separate Expirations calls = %d, want 0", got)
	}
	if got := provider.chainCalls.Load(); got != 0 {
		t.Fatalf("separate Chain calls = %d, want 0", got)
	}
	if response.Expiration != "2030-01-18" || response.Rows != 1 || response.Quote == nil || response.Quote.Price == nil || *response.Quote.Price != price {
		t.Fatalf("refresh response = %+v", response)
	}
}

func TestSnapshotRefreshWithQuoteOverrideUsesNearestChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nearest-chain-override.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price := 123.0
	fetchedAt := time.Now().UTC()
	provider := &nearestChainTestProvider{testMarketProvider: &testMarketProvider{}}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, provider)
	defer service.Close()
	quote := model.Quote{Price: &price, FetchedAt: &fetchedAt, Currency: "USD", Provider: "test", Sessions: map[string]any{}}
	response, err := service.refreshSnapshotWithOverrides(context.Background(), "QQQ", "", time.Second, nil, &quote)
	if err != nil {
		t.Fatalf("refreshSnapshotWithOverrides() error = %v", err)
	}
	if got := provider.nearestCalls.Load(); got != 1 {
		t.Fatalf("NearestChain calls = %d, want 1", got)
	}
	if got := provider.expirationCalls.Load(); got != 0 {
		t.Fatalf("separate Expirations calls = %d, want 0", got)
	}
	if got := provider.chainCalls.Load(); got != 0 {
		t.Fatalf("separate Chain calls = %d, want 0", got)
	}
	if got := provider.quoteCalls.Load(); got != 0 {
		t.Fatalf("upstream quote calls = %d, want 0 for the override", got)
	}
	if response.Expiration != "2030-01-18" || response.Rows != 1 || response.Quote == nil || response.Quote.Price == nil || *response.Quote.Price != price {
		t.Fatalf("refresh response = %+v", response)
	}
}

func TestChanAdjustedEndpointSerializesRecursiveStructureAndSignalEvidence(t *testing.T) {
	service, ts := testService(t, "")
	now := time.Now().UTC()
	location := marketLocation()
	day := time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	bars := make([]map[string]any, 0, 180)
	for len(bars) < 180 {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			index := float64(len(bars))
			close := 100 + index*.018 + 6*math.Sin(index*.31) + 1.5*math.Sin(index*.077)
			bar := map[string]any{"date": day.Format(time.DateOnly), "open": close - .15, "high": close + 1, "low": close - 1, "close": close, "volume": 1000.0, "raw_volume": 1000.0, "adjustment_factor": 1.0}
			for _, field := range []string{"open", "high", "low", "close"} {
				bar["raw_"+field] = bar[field]
			}
			bars = append(bars, bar)
		}
		day = day.AddDate(0, 0, 1)
	}
	if err := service.store.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", bars, now); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?spot=110")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || payload["rule_version"] != chananalysis.RecursivePreviewVersion {
		t.Fatalf("adjusted response status=%d version=%v", response.StatusCode, payload["rule_version"])
	}
	comparison, ok := payload["algorithm_comparison"].(map[string]any)
	if !ok || comparison["method"] == "" {
		t.Fatalf("adjusted response missing algorithm comparison summary: %#v", payload["algorithm_comparison"])
	}
	for _, timeframe := range []string{"daily", "weekly"} {
		frameComparison, ok := comparison[timeframe].(map[string]any)
		if !ok {
			t.Fatalf("algorithm comparison missing %s detail: %#v", timeframe, comparison)
		}
		for _, field := range []string{"legacy", "recursive", "differences"} {
			if _, exists := frameComparison[field]; !exists {
				t.Fatalf("algorithm comparison %s missing %s: %#v", timeframe, field, frameComparison)
			}
		}
	}
	for _, timeframe := range []string{"daily", "weekly"} {
		frame := payload[timeframe].(map[string]any)
		for _, field := range []string{"timeframe", "level", "quality", "merged_candles", "segments", "recursive_centers", "recursive_levels", "strengths_v4", "signals_v4", "trend_context_v4"} {
			if _, exists := frame[field]; !exists {
				t.Fatalf("%s frame missing recursive API field %q: %#v", timeframe, field, frame)
			}
		}
	}
	daily := payload["daily"].(map[string]any)
	segments := daily["segments"].([]any)
	if len(segments) == 0 {
		t.Fatalf("test fixture did not produce recursive segments: strokes=%d", len(daily["strokes_v4"].([]any)))
	}
	segment := segments[0].(map[string]any)
	for _, field := range []string{"level", "start_stroke", "end_stroke", "start", "end", "confirmed", "forming", "start_input_bar", "end_input_bar"} {
		if _, exists := segment[field]; !exists {
			t.Fatalf("segment missing API evidence field %q: %#v", field, segment)
		}
	}
	if centers, ok := daily["recursive_centers"].([]any); !ok {
		t.Fatalf("recursive centers type=%T", daily["recursive_centers"])
	} else if len(centers) > 0 {
		center := centers[0].(map[string]any)
		for _, field := range []string{"level", "start_segment", "end_segment", "formed_at", "confirmed_at", "forming", "zd", "zg"} {
			if _, exists := center[field]; !exists {
				t.Fatalf("recursive center missing API evidence field %q: %#v", field, center)
			}
		}
	}
	for _, signalValue := range daily["signals_v4"].([]any) {
		signal := signalValue.(map[string]any)
		for _, field := range []string{"symbol", "level", "side", "rule_version", "date", "observed_at", "structure_refs", "conditions", "missing_conditions", "market_filter", "lower_level_status", "phase_history"} {
			if _, exists := signal[field]; !exists {
				t.Fatalf("signal missing API evidence field %q: %#v", field, signal)
			}
		}
	}
}

func TestChanAdjustedEndpointReturnsPointInTimeSP500Environment(t *testing.T) {
	service, ts := testService(t, "")
	now := time.Now().UTC()
	location := marketLocation()
	benchmark := make([]map[string]any, 0, 220)
	day := time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	for len(benchmark) < 220 {
		if marketcalendar.IsTradingDay(day) {
			close := 100 + float64(len(benchmark))*0.2
			benchmark = append(benchmark, adjustedHistoryRow(day.Format(time.DateOnly), close))
		}
		day = day.AddDate(0, 0, 1)
	}
	if err := service.store.WriteChanAdjustedHistory("^GSPC", "1d", "split_dividend_auto", "go-yfinance", benchmark, now); err != nil {
		t.Fatal(err)
	}
	stockBars := make([]map[string]any, 0, 220)
	day = time.Date(2025, 1, 2, 0, 0, 0, 0, location)
	for len(stockBars) < 220 {
		if marketcalendar.IsTradingDay(day) {
			stockBars = append(stockBars, adjustedHistoryRow(day.Format(time.DateOnly), 100+float64(len(stockBars))*0.05+4*math.Sin(float64(len(stockBars))*0.31)))
		}
		day = day.AddDate(0, 0, 1)
	}
	if err := service.store.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", stockBars, now); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?spot=150")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d payload=%#v", response.StatusCode, payload)
	}
	environment := payload["market_environment"].(map[string]any)
	if environment["status"] != "classified" || environment["regime"] != "bull" || environment["benchmark"] != "^GSPC" {
		t.Fatalf("market environment=%#v", environment)
	}
	if payload["market_history"].(map[string]any)["source"] != "sqlite" {
		t.Fatalf("market history metadata=%#v", payload["market_history"])
	}
	daily := payload["daily"].(map[string]any)
	if daily["market_environment"].(map[string]any)["regime"] != "bull" {
		t.Fatalf("daily environment=%#v", daily["market_environment"])
	}
	for _, raw := range daily["signals_v4"].([]any) {
		signal := raw.(map[string]any)
		env := signal["market_environment"].(map[string]any)
		if env["date"].(string) > signal["observed_at"].(string) {
			t.Fatalf("signal market environment looks ahead: signal=%#v env=%#v", signal, env)
		}
		if signal["market_filter"] == "unfiltered" && env["status"] == "classified" {
			t.Fatalf("classified benchmark should be attached to signal: %#v", signal)
		}
	}
}

func TestAttachMissingSP500EnvironmentKeepsSignalUnfiltered(t *testing.T) {
	frame := chananalysis.Frame{AsOf: "2026-10-16", SignalsV4: []chananalysis.V4Signal{{
		SignalID: "candidate-1", Symbol: "QQQ", Level: 1, Kind: "class1_buy", Side: "buy",
		Status: "observing", Date: "2026-10-15", ObservedAt: "2026-10-16", MarketFilter: "unfiltered",
	}}}
	attachMarketEnvironment(&frame, nil)
	if frame.MarketEnvironment == nil || frame.MarketEnvironment.Status != "unfiltered" || frame.MarketEnvironment.Reason == "" {
		t.Fatalf("missing benchmark environment=%+v, want explicit unfiltered status and reason", frame.MarketEnvironment)
	}
	if len(frame.SignalsV4) != 1 {
		t.Fatalf("market data absence removed individual structural candidate: %+v", frame.SignalsV4)
	}
	signal := frame.SignalsV4[0]
	if signal.MarketFilter != "unfiltered" || signal.MarketEnvironment == nil || signal.MarketEnvironment.Status != "unfiltered" || signal.MarketEnvironment.Date > signal.ObservedAt {
		t.Fatalf("candidate should retain point-in-time unfiltered metadata: %+v", signal)
	}
}

func adjustedHistoryRow(day string, close float64) map[string]any {
	return map[string]any{"date": day, "open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 1000.0, "raw_open": close, "raw_high": close + 1, "raw_low": close - 1, "raw_close": close, "raw_volume": 1000.0, "adjustment_factor": 1.0}
}

func TestChanIntradayEndpointDeclaresPreviewVersionAndRawPriceBasis(t *testing.T) {
	service, ts := testService(t, "")
	service.intradayHistoryCache["QQQ"] = intradayCacheEntry{
		frame: []map[string]any{{
			"date": "2026-10-16 15:45", "open": 100.0, "high": 101.0, "low": 99.0, "close": 100.5, "volume": 1000.0,
		}},
		fetchedAt: time.Now().UTC(),
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ/intraday?period=15m")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	frame, _ := payload["frame"].(map[string]any)
	quality, _ := frame["quality"].(map[string]any)
	coordinate, _ := payload["price_coordinate"].(map[string]any)
	parameters, _ := payload["parameters"].(map[string]any)
	if response.StatusCode != http.StatusOK || payload["rule_version"] != chananalysis.RecursivePreviewVersion || payload["target_rule_version"] != chananalysis.RecursiveRuleVersion || payload["legacy_rule_version"] != chananalysis.RuleVersion {
		t.Fatalf("intraday response status=%d versions=%v/%v/%v", response.StatusCode, payload["rule_version"], payload["target_rule_version"], payload["legacy_rule_version"])
	}
	if parameters["implementation_status"] != "preview" || coordinate["basis"] != "raw" || coordinate["status"] != "unadjusted" || quality["price_basis"] != "raw" || frame["timeframe"] != "15m" {
		t.Fatalf("intraday schema must expose preview state, timeframe and raw price basis: payload=%#v", payload)
	}
	if signals, ok := frame["signals_v4"].([]any); !ok || len(signals) != 0 {
		t.Fatalf("single-bar intraday input must not create a structural signal: %#v", frame["signals_v4"])
	}
}

func TestValidAdjustedHistoryRowChecksEveryOHLCFactor(t *testing.T) {
	bar := map[string]any{"adjustment_factor": .5, "open": 50.0, "high": 55.0, "low": 45.0, "close": 52.0, "raw_open": 100.0, "raw_high": 110.0, "raw_low": 90.0, "raw_close": 104.0}
	if !validAdjustedHistoryRow(bar) {
		t.Fatal("valid adjusted OHLC row was rejected")
	}
	bar["raw_high"] = 120.0
	if validAdjustedHistoryRow(bar) {
		t.Fatal("row with mismatched high adjustment factor was accepted")
	}
}

func TestChanLegacyEndpointKeepsStrictOldStrokeAndCenterSemantics(t *testing.T) {
	service, ts := testService(t, "")
	now := time.Now().UTC()
	base := now.In(marketLocation()).AddDate(0, 0, -60)
	base = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, marketLocation())
	bars := make([]map[string]any, 0, 45)
	for day := 0; len(bars) < 45; day++ {
		date := base.AddDate(0, 0, day)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		price := 100 + 10*math.Sin(float64(len(bars))*0.42)
		bars = append(bars, map[string]any{"date": date.Format(time.DateOnly), "open": price, "high": price + 1, "low": price - 1, "close": price, "volume": 1000.0})
	}
	if err := service.store.WriteHistory("QQQ", bars, now); err != nil {
		t.Fatal(err)
	}
	adjustedBars := make([]map[string]any, 0, len(bars))
	for _, bar := range bars {
		adjusted := map[string]any{}
		for key, value := range bar {
			adjusted[key] = value
		}
		for _, field := range []string{"open", "high", "low", "close", "volume"} {
			adjusted["raw_"+field] = bar[field]
		}
		adjusted["adjustment_factor"] = 1.0
		adjustedBars = append(adjustedBars, adjusted)
	}
	if err := service.store.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", adjustedBars, now); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || payload["rule_version"] != chananalysis.RuleVersion {
		t.Fatalf("legacy response status=%d rule_version=%v payload=%#v", response.StatusCode, payload["rule_version"], payload)
	}
	daily := payload["daily"].(map[string]any)
	if value, exists := daily["strokes_v4"]; exists && value != nil {
		t.Fatalf("legacy daily response unexpectedly populates v4 strokes: %#v", value)
	}
	if value, exists := daily["signals_v4"]; exists && value != nil {
		t.Fatalf("legacy daily response unexpectedly populates v4 signals: %#v", value)
	}
	if _, ok := daily["strokes"].([]any); !ok {
		t.Fatalf("legacy strokes has type %T, want list", daily["strokes"])
	}
	legacyCenters, ok := daily["centers"].([]any)
	if !ok {
		t.Fatalf("legacy centers has type %T, want legacy pen-center list", daily["centers"])
	}
	for _, raw := range legacyCenters {
		center := raw.(map[string]any)
		if center["kind"] != "pen" || center["input_kind"] != "stroke" || center["level"] != float64(0) {
			t.Fatalf("legacy centers semantics changed: %#v", center)
		}
	}
	adjustedResponse, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?spot=100")
	if err != nil {
		t.Fatal(err)
	}
	defer adjustedResponse.Body.Close()
	var adjustedPayload map[string]any
	if err := json.NewDecoder(adjustedResponse.Body).Decode(&adjustedPayload); err != nil {
		t.Fatal(err)
	}
	if adjustedResponse.StatusCode != http.StatusOK || adjustedPayload["rule_version"] != chananalysis.RecursivePreviewVersion || adjustedPayload["legacy_rule_version"] != chananalysis.RuleVersion {
		t.Fatalf("adjusted response status=%d versions=%v/%v", adjustedResponse.StatusCode, adjustedPayload["rule_version"], adjustedPayload["legacy_rule_version"])
	}
	adjustedDaily := adjustedPayload["daily"].(map[string]any)
	legacyStrokeRows, adjustedStrokeRows := daily["strokes"].([]any), adjustedDaily["strokes"].([]any)
	if len(legacyStrokeRows) != len(adjustedStrokeRows) {
		t.Fatalf("legacy stroke count differs between endpoints: %d/%d", len(legacyStrokeRows), len(adjustedStrokeRows))
	}
	for index := range legacyStrokeRows {
		oldStroke, newStroke := legacyStrokeRows[index].(map[string]any), adjustedStrokeRows[index].(map[string]any)
		oldStart, newStart := oldStroke["start"].(map[string]any), newStroke["start"].(map[string]any)
		oldEnd, newEnd := oldStroke["end"].(map[string]any), newStroke["end"].(map[string]any)
		for _, field := range []string{"direction", "confirmed", "confirmed_at", "low", "high"} {
			if !reflect.DeepEqual(oldStroke[field], newStroke[field]) {
				t.Fatalf("legacy stroke %d %s differs: %v/%v", index, field, oldStroke[field], newStroke[field])
			}
		}
		for _, point := range []struct {
			name      string
			old, next map[string]any
		}{{"start", oldStart, newStart}, {"end", oldEnd, newEnd}} {
			for _, field := range []string{"kind", "date", "price"} {
				if !reflect.DeepEqual(point.old[field], point.next[field]) {
					t.Fatalf("legacy stroke %d %s %s differs: %v/%v", index, point.name, field, point.old[field], point.next[field])
				}
			}
		}
	}
	legacyCenterRows, adjustedCenterRows := daily["centers"].([]any), adjustedDaily["centers"].([]any)
	if len(legacyCenterRows) != len(adjustedCenterRows) {
		t.Fatalf("legacy center count differs between endpoints: %d/%d", len(legacyCenterRows), len(adjustedCenterRows))
	}
	for index := range legacyCenterRows {
		oldCenter, newCenter := legacyCenterRows[index].(map[string]any), adjustedCenterRows[index].(map[string]any)
		for _, field := range []string{"kind", "input_kind", "level", "start", "end", "lower", "upper", "confirmed_at"} {
			if !reflect.DeepEqual(oldCenter[field], newCenter[field]) {
				t.Fatalf("legacy center %d %s differs: %v/%v", index, field, oldCenter[field], newCenter[field])
			}
		}
	}
	if _, ok := adjustedDaily["segments"].([]any); !ok {
		t.Fatalf("adjusted v4 segments type=%T", adjustedDaily["segments"])
	}
	compactResponse, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?compact=true&spot=100")
	if err != nil {
		t.Fatal(err)
	}
	defer compactResponse.Body.Close()
	var compactPayload map[string]any
	if err := json.NewDecoder(compactResponse.Body).Decode(&compactPayload); err != nil {
		t.Fatal(err)
	}
	if compactResponse.StatusCode != http.StatusOK {
		t.Fatalf("compact response status=%d", compactResponse.StatusCode)
	}
	compactDaily, ok := compactPayload["daily"].(map[string]any)
	if !ok {
		t.Fatalf("compact daily type=%T", compactPayload["daily"])
	}
	for _, required := range []string{"candles", "strokes", "recursive_levels", "signals_v4", "quality"} {
		if _, present := compactDaily[required]; !present {
			t.Fatalf("compact daily lost %q", required)
		}
	}
	for _, omitted := range []string{"fractals", "fractals_v4", "strokes_v4", "strengths_v4", "macd_v4", "trend_context_v4"} {
		if _, present := compactDaily[omitted]; present {
			t.Fatalf("compact daily unexpectedly carries %q", omitted)
		}
	}
	lightResponse, err := http.Get(ts.URL + "/api/chan/QQQ?light=true&spot=100")
	if err != nil {
		t.Fatal(err)
	}
	defer lightResponse.Body.Close()
	var lightPayload map[string]any
	if err := json.NewDecoder(lightResponse.Body).Decode(&lightPayload); err != nil {
		t.Fatal(err)
	}
	if lightResponse.StatusCode != http.StatusOK {
		t.Fatalf("light response status=%d", lightResponse.StatusCode)
	}
	for _, omitted := range []string{"daily", "weekly", "summary", "rules", "rule_version"} {
		if _, present := lightPayload[omitted]; present {
			t.Fatalf("light response unexpectedly carries %q", omitted)
		}
	}
	if lightPayload["market_analysis"] == nil || lightPayload["trend"] == nil || lightPayload["history"] == nil || lightPayload["spot"] != float64(100) {
		t.Fatalf("light response lost market card inputs: %#v", lightPayload)
	}
}

func TestChanAdjustedEndpointReportsVerifiedActionsAndLargePriceMove(t *testing.T) {
	service, ts := testService(t, "")
	fetched := time.Now().UTC()
	writeBars := func(symbol string, bars []map[string]any) {
		t.Helper()
		if err := service.store.WriteChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", bars, fetched); err != nil {
			t.Fatal(err)
		}
	}
	base := func(date string, close, high, low, rawClose, rawHigh, rawLow, factor float64) map[string]any {
		return map[string]any{"date": date, "open": close, "high": high, "low": low, "close": close, "volume": 100.0, "raw_open": rawClose, "raw_high": rawHigh, "raw_low": rawLow, "raw_close": rawClose, "raw_volume": 100.0, "adjustment_factor": factor}
	}
	writeBars("SPLIT", []map[string]any{
		base("2026-10-15", 50, 51, 49, 100, 102, 98, .5),
		base("2026-10-16", 50, 52, 49, 50, 52, 49, 1),
	})
	writeBars("JUMP", []map[string]any{
		base("2026-10-15", 100, 102, 98, 100, 102, 98, 1),
		base("2026-10-16", 150, 152, 148, 150, 152, 148, 1),
	})
	request := func(symbol string) map[string]any {
		t.Helper()
		response, err := http.Get(ts.URL + "/api/chan/" + symbol + "/adjusted?spot=150")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("adjusted action response status=%d decode=%v payload=%#v", response.StatusCode, err, payload)
		}
		return payload["daily"].(map[string]any)
	}
	splitDaily := request("SPLIT")
	splitQuality := splitDaily["quality"].(map[string]any)
	if splitQuality["corporate_action_suspected"] != false || splitQuality["status"] != "ok" {
		t.Fatalf("factor-explained split degraded adjusted history: %#v", splitQuality)
	}
	events, ok := splitQuality["corporate_action_events"].([]any)
	if !ok || len(events) != 1 || events[0].(map[string]any)["kind"] != "split_candidate" || events[0].(map[string]any)["factor_verified"] != true {
		t.Fatalf("split action event=%#v", splitQuality["corporate_action_events"])
	}
	if chains, ok := splitQuality["chains"].([]any); !ok || len(chains) != 1 {
		t.Fatalf("verified split should remain in one adjusted structure chain: %#v", splitQuality["chains"])
	}

	jumpDaily := request("JUMP")
	jumpQuality := jumpDaily["quality"].(map[string]any)
	if jumpQuality["corporate_action_suspected"] != false || jumpQuality["status"] != "ok" {
		t.Fatalf("valid adjusted price move should not degrade quality: %#v", jumpQuality)
	}
	jumpEvents, ok := jumpQuality["corporate_action_events"].([]any)
	if !ok || len(jumpEvents) != 1 || jumpEvents[0].(map[string]any)["kind"] != "large_price_move" || jumpEvents[0].(map[string]any)["status"] != "observed" {
		t.Fatalf("large adjusted price move event=%#v", jumpQuality["corporate_action_events"])
	}
	if chains, ok := jumpQuality["chains"].([]any); !ok || len(chains) != 2 {
		t.Fatalf("large price move should split structure chains: %#v", jumpQuality["chains"])
	}
}

func TestChanAdjustedEndpointDegradesAndBlocksComparisonWithoutValidSpotFactor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-chan-factor.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	fetched := time.Now().UTC()
	bar := map[string]any{"date": "2026-10-16", "open": 50.0, "high": 55.0, "low": 45.0, "close": 52.0, "volume": 100.0, "raw_open": 100.0, "raw_high": 110.0, "raw_low": 90.0, "raw_close": 104.0, "raw_volume": 100.0, "adjustment_factor": .5}
	if err := repository.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", []map[string]any{bar}, fetched); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`UPDATE chan_adjusted_history SET adjustment_factor=0.25 WHERE symbol='QQQ' AND bar_date='2026-10-16'`); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chan/QQQ/adjusted?spot=110")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("missing-factor response status=%d decode=%v payload=%#v", response.StatusCode, err, payload)
	}
	coordinate := payload["price_coordinate"].(map[string]any)
	daily := payload["daily"].(map[string]any)
	quality := daily["quality"].(map[string]any)
	if coordinate["status"] != "degraded" || coordinate["adjustment_factor"] != nil || coordinate["adjusted_spot"] != nil || coordinate["raw_spot"] != 110.0 || daily["last_close"] != nil || quality["status"] != "degraded" || daily["bar_count"] != float64(0) {
		t.Fatalf("invalid factor must block comparison and degrade the frame: coordinate=%#v daily=%#v", coordinate, daily)
	}
	warnings, ok := payload["warnings"].([]any)
	warningFound := false
	for _, warning := range warnings {
		if warning == "缺少有效的现价复权因子；当前价未与复权结构比较，数据质量已降级" {
			warningFound = true
		}
	}
	if !ok || !warningFound {
		t.Fatalf("missing factor warning not exposed: %#v", payload["warnings"])
	}
}

func TestLevelsDoesNotAppendExpiredSelectedChain(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "levels-expired.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 100.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "test", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	strike := 85.0
	volume := int64(100)
	if err := repository.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expired, FetchedAt: &now, Data: []model.OptionRow{{ContractSymbol: "QQQ-expired", Expiration: expired, ContractType: "call", Strike: &strike, Volume: &volume}}}); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=" + expired)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("expired levels response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	expirations, ok := body["options_expirations"].([]any)
	if !ok || len(expirations) != 0 {
		t.Fatalf("expired selected chain leaked into options_expirations: %#v", body["options_expirations"])
	}
}

func TestLevelsUsesCachedSnapshotsForNonRawResponse(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "levels-cache.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price, previous := 100.0, 99.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, PreviousClose: &previous, Provider: "test", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().AddDate(0, 0, 20).Format("2006-01-02")
	strikeBelow, strikeAbove := 94.0, 106.0
	volume := int64(100)
	if err := repository.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expiration, FetchedAt: &now, Data: []model.OptionRow{
		{ContractSymbol: "QQQ-C94", Expiration: expiration, ContractType: "call", Strike: &strikeBelow, Volume: &volume},
		{ContractSymbol: "QQQ-C106", Expiration: expiration, ContractType: "call", Strike: &strikeAbove, Volume: &volume},
	}}); err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 30; index++ {
		day := time.Now().UTC().AddDate(0, 0, -30+index).Format("2006-01-02")
		closePrice := 86 + float64(index)*0.5
		if _, err := fixtureDB.Exec(`INSERT INTO price_history(symbol, bar_date, open, high, low, close, volume, fetched_at) VALUES ('QQQ', ?, ?, ?, ?, ?, 1000, ?)`, day, closePrice-1, closePrice+2, closePrice-2, closePrice, now.Format(time.RFC3339Nano)); err != nil {
			fixtureDB.Close()
			t.Fatal(err)
		}
	}
	if _, err := fixtureDB.Exec(`INSERT INTO price_extremes(symbol, payload, fetched_at) VALUES ('QQQ', '{"52_week_high": 110, "52_week_low": 80}', ?); INSERT INTO beta_snapshots(symbol, payload, fetched_at) VALUES ('QQQ', '{"value": 1.2}', ?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		fixtureDB.Close()
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=" + expiration)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("cached levels response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	if body["raw"] != nil {
		t.Fatalf("non-raw cached levels unexpectedly marked raw: %#v", body)
	}
	history, historyOK := body["history"].(map[string]any)
	if !historyOK || history["source"] != "sqlite" || history["extremes_source"] != "sqlite" || history["extremes_warning"] != nil {
		t.Fatalf("cached levels lost history metadata wrapper: %#v", body["history"])
	}
	betaMeta, betaMetaOK := body["beta_meta"].(map[string]any)
	if !betaMetaOK || betaMeta["source"] != "sqlite" || betaMeta["warning"] != nil {
		t.Fatalf("cached levels lost beta metadata wrapper: %#v", body["beta_meta"])
	}
	if _, exists := body["earnings"]; exists {
		t.Fatalf("cached levels exposed removed earnings metadata: %#v", body["earnings"])
	}
	resistance, resistanceOK := body["resistance"].([]any)
	support, supportOK := body["support"].([]any)
	if !resistanceOK || !supportOK || len(resistance) == 0 || len(support) == 0 {
		t.Fatalf("cached levels did not return both sides: resistance=%#v support=%#v", body["resistance"], body["support"])
	}
	if body["trend"] == nil || body["trade_points"] == nil || body["plan"] == nil {
		t.Fatalf("cached levels omitted Python response contract fields: %#v", body)
	}
}

func TestLevelsRejectsExpirationOutsideExactISOFormat(t *testing.T) {
	_, ts := testService(t, "")
	for _, expiration := range []string{"%202030-01-18", "2030-1-18", ""} {
		requestURL := ts.URL + "/api/levels/QQQ?expiration=" + expiration + "&raw=true"
		response, err := http.Get(requestURL)
		if err != nil {
			t.Fatalf("expiration %q request: %v", expiration, err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("expiration %q response=%#v status=%d decodeErr=%v; want 422", expiration, body, response.StatusCode, decodeErr)
		}
	}
}

func TestUnavailableLevelsResponseMatchesPythonBuildLevelsShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unavailable-levels.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, &testMarketProvider{})
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2099-01-16")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("unavailable levels response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	for _, field := range []string{
		"symbol", "expiration", "spot", "chain_fetched_at", "options_fetched_at", "options_expirations",
		"options_horizon_days", "history", "trend_market", "beta", "beta_meta", "extremes",
		"history_bars", "options_metric", "resistance", "support", "trend", "recommendation", "trade_points",
		"stop_loss", "trade_points_horizon", "buyer_structures", "plan",
	} {
		if _, exists := body[field]; !exists {
			t.Errorf("unavailable levels omitted Python response field %q: %#v", field, body)
		}
	}
	if body["spot"] != nil || body["options_horizon_days"] != float64(45) {
		t.Errorf("unavailable levels core fields=%#v", body)
	}
	if _, exists := body["candidate_spot"]; exists {
		t.Errorf("Python early build_levels result does not include candidate_spot: %#v", body)
	}
	if _, exists := body["history_validation"]; exists {
		t.Errorf("Python early build_levels result does not include history_validation: %#v", body)
	}
	trend, ok := body["trend_market"].(map[string]any)
	if !ok || trend["previous_close"] != nil || trend["market_state"] != nil {
		t.Errorf("unavailable trend_market shape=%#v", body["trend_market"])
	}
}

func TestLevelsRejectsExplicitEmptySpot(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2030-01-18&spot=")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty spot status=%d, want 422", response.StatusCode)
	}
}

func TestLevelsSpotValidationMatchesFastAPIInfinitySemantics(t *testing.T) {
	_, ts := testService(t, "")
	for _, spot := range []string{"NaN", "-Inf", "0"} {
		requestURL := ts.URL + "/api/levels/QQQ?expiration=2030-01-18&spot=" + spot + "&raw=true"
		response, err := http.Get(requestURL)
		if err != nil {
			t.Fatalf("spot %q request: %v", spot, err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("spot %q response=%#v status=%d decodeErr=%v; want 422", spot, body, response.StatusCode, decodeErr)
		}
	}
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2030-01-18&spot=Inf&raw=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("positive infinite spot response status=%d decode=%v body=%#v; FastAPI accepts it and serializes it as null", response.StatusCode, err, body)
	}
	if body["spot"] != nil {
		t.Fatalf("positive infinite spot serialized as %#v, want null like FastAPI", body["spot"])
	}
}

func TestHealthAndAccessGuard(t *testing.T) {
	_, ts := testService(t, "secret")
	response, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", response.StatusCode)
	}
	response, err = http.Get(ts.URL + "/static")
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusForbidden || !strings.Contains(string(responseBody), "<h1>403 Forbidden</h1>") {
		t.Fatalf("unauthorized /static status=%d err=%v body=%q, want Python access-guard 403", response.StatusCode, readErr, responseBody)
	}
	response, err = http.Get(ts.URL + "/static/common/css/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public static asset status=%d, want 200", response.StatusCode)
	}

	response, err = http.Get(ts.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want 403", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "" {
		t.Fatalf("unauthorized API response Cache-Control=%q, want absent because Python access guard short-circuits before no-store", got)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["detail"] != "403 Forbidden" {
		t.Fatalf("unexpected 403 body: %#v", body)
	}
	response, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	page, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusForbidden || !strings.Contains(string(page), "<h1>403 Forbidden</h1>") {
		t.Fatalf("unauthorized page response status=%d err=%v body=%q", response.StatusCode, err, page)
	}
}

func TestRawExpirationWindowAndLimitValidation(t *testing.T) {
	if got := selectRawExpirations([]string{"2026-01-01", "2026-01-08", "2026-01-15", "2026-01-22", "2026-01-29"}, "2026-01-22", 3); strings.Join(got, ",") != "2026-01-15,2026-01-22,2026-01-29" {
		t.Fatalf("unexpected selected expiration window: %#v", got)
	}
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2030-01-18&raw=true&raw_expirations=9")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid raw_expirations status=%d, want 422", response.StatusCode)
	}
}

func TestPendingReadsAndRefreshValidation(t *testing.T) {
	_, ts := testService(t, "")
	for _, path := range []string{"/api/quote/QQQ", "/api/expirations/QQQ", "/api/chain/QQQ?expiration=2030-01-18"} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil {
			t.Fatalf("%s: %v", path, decodeErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, response.StatusCode)
		}
		if body["source"] != "pending" {
			t.Fatalf("%s source = %v, want pending", path, body["source"])
		}
	}
	response, err := http.Post(ts.URL+"/api/refresh/QQQ?max_age=3601", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid max_age status = %d, want 422", response.StatusCode)
	}
}

func TestDegradedAnalysisContracts(t *testing.T) {
	_, ts := testService(t, "")
	checks := []struct {
		path string
		keys []string
	}{
		{"/api/flow/QQQ?expiration=2030-01-18", []string{"symbol", "expiration", "available", "call", "put", "signal"}},
		{"/api/gamma/QQQ?status_only=true&include_rows=false", []string{"symbol", "refresh", "data", "contract_count", "status_only"}},
		{"/api/levels/QQQ?expiration=2030-01-18&raw=true", []string{"raw", "symbol", "expiration", "options", "history"}},
	}
	for _, check := range checks {
		response, err := http.Get(ts.URL + check.path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d decode=%v body=%#v", check.path, response.StatusCode, decodeErr, body)
		}
		for _, key := range check.keys {
			if _, ok := body[key]; !ok {
				t.Fatalf("%s missing key %q in %#v", check.path, key, body)
			}
		}
	}
}

func TestRawLevelsMatchesPythonOptionAndEarningsFields(t *testing.T) {
	service, ts := testService(t, "")
	price, strike, farStrike := 100.0, 100.0, 1e9
	last, bid, ask, iv, gammaValue := 3.0, 2.9, 3.1, 0.3, 0.02
	volume, openInterest := int64(50), int64(200)
	now := time.Now().UTC()
	expiration := now.AddDate(0, 0, 30).Format("2006-01-02")
	if err := service.store.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "test", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := service.store.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expiration, FetchedAt: &now, Data: []model.OptionRow{
		{
			ContractSymbol: "QQQ-test", Expiration: expiration, ContractType: "call", Strike: &strike,
			LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
			ImpliedVolatility: &iv, Gamma: &gammaValue, Provider: "fixture", InTheMoney: true,
		},
		{
			ContractSymbol: "QQQ-far", Expiration: expiration, ContractType: "call", Strike: &farStrike,
			LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
			ImpliedVolatility: &iv, Gamma: &gammaValue, Provider: "fixture",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=" + expiration + "&raw=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("raw levels response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	options, ok := body["options"].([]any)
	if !ok || len(options) != 2 {
		t.Fatalf("raw options=%#v, want two selected contracts", body["options"])
	}
	option, ok := options[0].(map[string]any)
	if !ok {
		t.Fatalf("raw option has type %T, want object", options[0])
	}
	wantOptionKeys := []string{"ask", "bid", "contract_symbol", "contract_type", "expiration", "gamma", "implied_volatility", "last_price", "model_gamma", "model_iv", "model_iv_source", "open_interest", "strike", "volume"}
	if len(option) != len(wantOptionKeys) {
		t.Fatalf("raw option keys=%v, want exactly %v", mapKeys(option), wantOptionKeys)
	}
	for _, key := range wantOptionKeys {
		if _, exists := option[key]; !exists {
			t.Errorf("raw option is missing Python-compatible field %q", key)
		}
	}
	for _, key := range []string{"provider", "in_the_money", "change_percent"} {
		if _, exists := option[key]; exists {
			t.Errorf("raw option unexpectedly exposes %q", key)
		}
	}
	if option["model_iv"] == nil || option["model_gamma"] == nil {
		t.Errorf("raw option model annotations=%#v, want both populated with spot available", option)
	}
	farOption, ok := options[1].(map[string]any)
	if !ok {
		t.Fatalf("far raw option has type %T, want object", options[1])
	}
	if _, exists := farOption["model_iv"]; !exists || farOption["model_iv"] == nil {
		t.Errorf("far raw option model_iv=%#v, want populated value", farOption["model_iv"])
	}
	if value, exists := farOption["model_gamma"]; !exists || value != nil {
		t.Errorf("far raw option model_gamma=%#v (exists=%v), want explicit null", value, exists)
	}
	if _, exists := body["earnings"]; exists {
		t.Errorf("raw levels exposed removed earnings field: %#v", body["earnings"])
	}
	history, ok := body["history"].(map[string]any)
	if !ok {
		t.Fatalf("raw history has type %T, want object", body["history"])
	}
	for _, key := range []string{"extremes_fetched_at", "extremes_source", "extremes_warning"} {
		if _, exists := history[key]; !exists {
			t.Errorf("raw history is missing Python-compatible field %q", key)
		}
	}
}

func TestRawLevelsOmitsModelGreeksWithoutSpot(t *testing.T) {
	service, ts := testService(t, "")
	strike, last, bid, ask, iv, gammaValue := 100.0, 3.0, 2.9, 3.1, 0.3, 0.02
	volume, openInterest := int64(50), int64(200)
	now := time.Now().UTC()
	expiration := now.AddDate(0, 0, 30).Format("2006-01-02")
	if err := service.store.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expiration, FetchedAt: &now, Data: []model.OptionRow{{
		ContractSymbol: "QQQ-test", Expiration: expiration, ContractType: "call", Strike: &strike,
		LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
		ImpliedVolatility: &iv, Gamma: &gammaValue, Provider: "fixture",
	}}}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=" + expiration + "&raw=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("raw levels response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	options, ok := body["options"].([]any)
	if !ok || len(options) != 1 {
		t.Fatalf("raw options=%#v, want one selected contract", body["options"])
	}
	option, ok := options[0].(map[string]any)
	if !ok {
		t.Fatalf("raw option has type %T, want object", options[0])
	}
	for _, key := range []string{"model_iv", "model_gamma"} {
		if _, exists := option[key]; exists {
			t.Errorf("raw option unexpectedly exposes %q without spot: %#v", key, option)
		}
	}
}

func TestChainRowsMatchPythonCachedRowFields(t *testing.T) {
	service, ts := testService(t, "")
	price, strike, farStrike, last, bid, ask, iv, gammaValue := 100.0, 100.0, 1e9, 3.0, 2.9, 3.1, 0.3, 0.02
	change := 1.5
	volume, openInterest := int64(50), int64(200)
	now := time.Now().UTC()
	expiration := now.AddDate(0, 0, 30).Format("2006-01-02")
	if err := service.store.SaveQuote(model.Quote{Symbol: "QQQ", Price: &price, Provider: "test", FetchedAt: &now, Sessions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := service.store.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expiration, FetchedAt: &now, Data: []model.OptionRow{
		{
			ContractSymbol: "QQQ-test", Expiration: expiration, ContractType: "call", Strike: &strike,
			LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
			ImpliedVolatility: &iv, Gamma: &gammaValue, ChangePercent: &change,
			Provider: "fixture-provider", InTheMoney: true, InTheMoneyKnown: true,
		},
		{
			ContractSymbol: "QQQ-test-far", Expiration: expiration, ContractType: "call", Strike: &farStrike,
			LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
			ImpliedVolatility: &iv, Gamma: &gammaValue, Provider: "fixture-provider",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chain/QQQ?expiration=" + expiration)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("chain response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	rows, ok := body["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("chain data=%#v, want two rows", body["data"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("chain row has type %T, want object", rows[0])
	}
	wantKeys := []string{"ask", "bid", "change_percent", "contract_symbol", "contract_type", "expiration", "gamma", "implied_volatility", "in_the_money", "last_price", "model_gamma", "model_iv", "open_interest", "strike", "volume"}
	if got := mapKeys(row); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("chain row keys=%v, want exactly Python DB fields %v", got, wantKeys)
	}
	for _, key := range []string{"provider", "model_iv_source"} {
		if _, exists := row[key]; exists {
			t.Errorf("chain row unexpectedly exposes Go-only field %q", key)
		}
	}
	if row["model_iv"] == nil || row["model_gamma"] == nil {
		t.Fatalf("chain model annotations=%#v, want populated IV and gamma with spot available", row)
	}
	farRow, ok := rows[1].(map[string]any)
	if !ok {
		t.Fatalf("far chain row has type %T, want object", rows[1])
	}
	if _, exists := farRow["model_iv"]; !exists || farRow["model_iv"] == nil {
		t.Errorf("far chain row model_iv=%#v, want populated value", farRow["model_iv"])
	}
	if value, exists := farRow["model_gamma"]; !exists || value != nil {
		t.Errorf("far chain row model_gamma=%#v (exists=%v), want explicit null", value, exists)
	}
}

func TestChainRowsOmitModelGreeksWithoutSpot(t *testing.T) {
	service, ts := testService(t, "")
	strike, last, bid, ask, iv, gammaValue := 100.0, 3.0, 2.9, 3.1, 0.3, 0.02
	volume, openInterest := int64(50), int64(200)
	now := time.Now().UTC()
	expiration := now.AddDate(0, 0, 30).Format("2006-01-02")
	if err := service.store.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: expiration, FetchedAt: &now, Data: []model.OptionRow{{
		ContractSymbol: "QQQ-test", Expiration: expiration, ContractType: "call", Strike: &strike,
		LastPrice: &last, Bid: &bid, Ask: &ask, Volume: &volume, OpenInterest: &openInterest,
		ImpliedVolatility: &iv, Gamma: &gammaValue, Provider: "fixture-provider",
	}}}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/chain/QQQ?expiration=" + expiration)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("chain response status=%d decode=%v body=%#v", response.StatusCode, err, body)
	}
	rows, ok := body["data"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("chain data=%#v, want one row", body["data"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("chain row has type %T, want object", rows[0])
	}
	wantKeys := []string{"ask", "bid", "change_percent", "contract_symbol", "contract_type", "expiration", "gamma", "implied_volatility", "in_the_money", "last_price", "open_interest", "strike", "volume"}
	if got := mapKeys(row); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("chain row keys without spot=%v, want exactly %v", got, wantKeys)
	}
	if summary, ok := body["iv_model"].(map[string]any); !ok || len(summary) != 0 {
		t.Fatalf("iv_model=%#v, want the empty summary returned by Python without spot", body["iv_model"])
	}
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestMarketRefreshInterval(t *testing.T) {
	cases := []struct {
		instant string
		want    time.Duration
	}{
		{"2026-10-05T10:00:00-04:00", 20 * time.Second}, // regular session
		{"2026-10-05T06:00:00-04:00", 60 * time.Second}, // weekday extended hours
		{"2026-10-04T06:00:00-04:00", 5 * time.Minute},  // Sunday closure
		{"2026-10-05T21:00:00-04:00", 60 * time.Second}, // overnight into a trading day
		{"2026-11-26T10:00:00-05:00", 5 * time.Minute},  // Thanksgiving
		{"2026-11-27T12:59:00-05:00", 20 * time.Second}, // day after Thanksgiving, before early close
		{"2026-11-27T13:30:00-05:00", 60 * time.Second}, // after early close
		{"2026-12-24T14:00:00-05:00", 60 * time.Second}, // Christmas Eve early close
		{"2026-04-03T10:00:00-04:00", 5 * time.Minute},  // Good Friday
		{"2026-07-03T10:00:00-04:00", 5 * time.Minute},  // Independence Day observed
		{"2026-06-19T10:00:00-04:00", 5 * time.Minute},  // Juneteenth
		{"2026-01-19T10:00:00-05:00", 5 * time.Minute},  // Martin Luther King Jr. Day
		{"2026-12-25T10:00:00-05:00", 5 * time.Minute},  // Christmas
		{"2021-12-31T10:00:00-05:00", 20 * time.Second}, // New Year's Day Saturday, market remains open
		{"2021-12-24T10:00:00-05:00", 5 * time.Minute},  // Christmas Day Saturday observed closure
		{"2020-07-03T10:00:00-04:00", 5 * time.Minute},  // Independence Day Saturday observed closure
	}
	for _, test := range cases {
		instant, err := time.Parse(time.RFC3339, test.instant)
		if err != nil {
			t.Fatal(err)
		}
		if got := marketRefreshInterval(instant); got != test.want {
			t.Errorf("marketRefreshInterval(%s)=%s want %s", test.instant, got, test.want)
		}
	}
}

func TestSSERejectsInvalidCursors(t *testing.T) {
	_, ts := testService(t, "")
	for _, test := range []struct {
		name   string
		path   string
		header string
	}{
		{name: "query", path: "/api/events/QQQ?last_event_id=abc"},
		{name: "negative query", path: "/api/events/QQQ?last_event_id=-1"},
		{name: "header", path: "/api/events/QQQ", header: "abc"},
		{name: "negative header", path: "/api/events/QQQ", header: "-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, ts.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.header != "" {
				request.Header.Set("Last-Event-ID", test.header)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			wantStatus := http.StatusUnprocessableEntity
			if test.header != "" {
				wantStatus = http.StatusOK
			}
			if response.StatusCode != wantStatus {
				t.Fatalf("cursor status=%d, want %d", response.StatusCode, wantStatus)
			}
		})
	}
}

func TestSSEHeaderCursorAcceptsArbitraryPrecisionDigitValues(t *testing.T) {
	oversized := "9223372036854775808"
	parsed := parseSSEHeaderCursor(oversized)
	if parsed == nil || parsed.String() != oversized {
		t.Fatalf("oversized digit-only header cursor parsed as %v", parsed)
	}
	if got := eventCursorInt64(parsed); got != math.MaxInt64 {
		t.Fatalf("event-store cursor=%d, want saturation at MaxInt64", got)
	}

	if parsed := parseSSEHeaderCursor("00123"); parsed == nil || parsed.String() != "123" {
		t.Fatalf("leading-zero header cursor parsed as %v, want 123", parsed)
	}
	for _, malformed := range []string{"", "+1", "-1", " 1", "1 ", "1.0", "abc"} {
		if parsed := parseSSEHeaderCursor(malformed); parsed != nil {
			t.Errorf("malformed header cursor %q parsed as %s", malformed, parsed)
		}
	}
}

func TestSSERefreshLoopRegistersPerSubscription(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "sse.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 200.0
	fake := &testMarketProvider{quote: model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}}, expirations: []string{}}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	release := service.registerSubscription("QQQ|", "QQQ", "")
	defer release()
	deadline := time.Now().Add(time.Second)
	for (fake.quoteCalls.Load() == 0 || fake.expirationCalls.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.quoteCalls.Load() == 0 || fake.expirationCalls.Load() == 0 {
		t.Fatalf("subscription did not trigger snapshot refresh: quote=%d expiration=%d", fake.quoteCalls.Load(), fake.expirationCalls.Load())
	}
}

func TestSubscriptionQuoteRefreshRunsDuringSnapshotBackoff(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "priority-quote.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a failed chain cycle: its durable retry window must not block
	// the live quote.
	if _, err := repository.DeferPushRefresh("QQQ", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	price := 100.0
	fake := &testMarketProvider{
		quote:       model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		expirations: []string{time.Now().AddDate(0, 0, 8).Format("2006-01-02")},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()

	release := service.registerSubscription("QQQ|", "QQQ", "")
	defer release()
	deadline := time.Now().Add(time.Second)
	var stored model.Quote
	var found bool
	for time.Now().Before(deadline) {
		var err error
		stored, found, err = repository.LatestQuote("QQQ")
		if err == nil && found && stored.Price != nil && *stored.Price == price {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := fake.quoteCalls.Load(); got != 1 {
		t.Fatalf("priority quote calls = %d, want one", got)
	}
	if got := fake.expirationCalls.Load(); got != 0 {
		t.Fatalf("catalog calls = %d, want none while the snapshot retry window is active", got)
	}
	if got := fake.chainCalls.Load(); got != 0 {
		t.Fatalf("chain calls = %d, want none while the snapshot retry window is active", got)
	}
	if !found || stored.Price == nil || *stored.Price != price {
		t.Fatalf("latest quote = %#v, found=%t; want persisted price %v", stored, found, price)
	}
}

func TestBackgroundRefreshDoesNotTreatIdleSSESubscriptionAsActiveWork(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "background-idle-sse.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 200.0
	fake := &testMarketProvider{
		quoteStarted: make(chan struct{}, 1),
		quote:        model.Quote{Price: &price, Currency: "USD", Provider: "test", Sessions: map[string]any{}},
		expirations:  []string{},
	}
	service := NewWithProvider(cfg, repository, fake)
	defer service.Close()
	// An SSE subscription is long-lived even when its worker is sleeping until
	// the next interval. Model that idle state while the page requests a refresh.
	service.mu.Lock()
	service.subscriptions["QQQ|"] = eventSubscription{cancel: func() {}, refs: 1}
	service.mu.Unlock()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()

	response, err := http.Post(ts.URL+"/api/refresh/QQQ?max_age=0&background=true", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || body["deferred"] != true {
		t.Fatalf("background refresh status=%d body=%#v, want deferred 200", response.StatusCode, body)
	}
	select {
	case <-fake.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("idle SSE subscription suppressed starting the background provider refresh")
	}
}

func TestRawLevelsAlignsStaleQuoteAnchorWithFreshHistory(t *testing.T) {
	service, ts := testService(t, "")
	staleClose := 99.0
	price := 110.0
	now := time.Now().UTC()
	quoteTime := now.Add(-24 * time.Hour)
	if err := service.store.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &price, PreviousClose: &staleClose,
		FetchedAt: &quoteTime, MarketState: "POST", Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	latestDay := now.In(marketLocation()).AddDate(0, 0, -1).Format("2006-01-02")
	previousDay := now.In(marketLocation()).AddDate(0, 0, -2).Format("2006-01-02")
	if err := service.store.WriteHistory("QQQ", []map[string]any{
		{"date": previousDay, "open": 100.0, "high": 103.0, "low": 99.0, "close": 102.0, "volume": 1000},
		{"date": latestDay, "open": 103.0, "high": 107.0, "low": 102.0, "close": 105.0, "volume": 1500},
	}, now); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2099-01-16&raw=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%#v", response.StatusCode, body)
	}
	if got := body["candidate_spot"]; got != float64(105) {
		t.Fatalf("raw candidate_spot=%#v, want fresh history close 105 instead of stale quote close 99", got)
	}
	if got := body["spot"]; got != float64(110) {
		t.Fatalf("display spot=%#v, want current quote 110", got)
	}
}

func marketLocation() *time.Location {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return location
}

func TestRawLevelsDoesNotExposeEmptySelectedChainTimestamp(t *testing.T) {
	service, ts := testService(t, "")
	fetched := time.Now().UTC().Truncate(time.Second)
	if err := service.store.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: "2099-01-16", FetchedAt: &fetched, Data: []model.OptionRow{}}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2099-01-16&raw=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%#v", response.StatusCode, body)
	}
	for _, key := range []string{"chain_fetched_at", "options_fetched_at"} {
		if body[key] != nil {
			t.Fatalf("%s = %#v, want null because empty chain does not create a latest batch marker", key, body[key])
		}
	}
}

func TestSSEReadyFrame(t *testing.T) {
	_, ts := testService(t, "")
	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events/QQQ", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", response.StatusCode)
	}
	if value := response.Header.Get("Content-Type"); !strings.HasPrefix(value, "text/event-stream") {
		t.Fatalf("Content-Type = %q", value)
	}
	buffer := make([]byte, 128)
	count, err := response.Body.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer[:count]), "event: ready") {
		t.Fatalf("missing ready frame: %q", string(buffer[:count]))
	}
}

func TestCachedQuoteReadRefreshesExpiredGoValuationWithoutDroppingStaleValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expired-go-valuation.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, staleValue, trailing, forward, shares := 100.0, 80.0, 10.0, 12.0, 1.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "QQQ", Price: &price, FairValue: staleValue, FairValueSource: goFairValueSource,
		FairValueStatus: "ready", Provider: "cache", FetchedAt: &now, Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{fundamentals: provider.YahooFundamentals{EPS: &trailing, ForwardEPS: &forward, Shares: &shares}}
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()
	ready := map[string]any{"value": 80.0, "source": goFairValueSource, "status": valuation.StatusReady}
	if err := repository.PutAnalysisCache(fairValueCacheKey("QQQ"), ready, 32); err != nil {
		t.Fatal(err)
	}
	if err := repository.PutAnalysisCache(fairValueStatusCacheKey("QQQ"), ready, 32); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339Nano)
	if _, err := admin.Exec(`UPDATE api_analysis_cache SET created_at=? WHERE cache_key IN (?, ?)`, ancient, fairValueCacheKey("QQQ"), fairValueStatusCacheKey("QQQ")); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := admin.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("stale cached quote status=%d decode=%v body=%#v", response.StatusCode, decodeErr, body)
	}
	if body["fair_value"] != staleValue {
		t.Fatalf("stale value should stay visible while refreshing: %#v", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.fundamentalsCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := fake.fundamentalsCalls.Load(); calls != 1 {
		t.Fatalf("expired Go value did not schedule refresh: fundamentals calls=%d", calls)
	}
}

func TestCachedQuoteReadDoesNotServeSupersededGoValuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "superseded-go-valuation.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	price, staleValue := 431.87, 160.0
	now := time.Now().UTC()
	if err := repository.SaveQuote(model.Quote{
		Symbol: "HUM", Price: &price, FairValue: staleValue, FairValueSource: "valuation_go_v1",
		FairValueStatus: "ready", FairValueDefensive: map[string]any{"value": 160.0},
		Provider: "cache", FetchedAt: &now, Sessions: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{fundamentals: provider.YahooFundamentals{
		Industry: "Health Care Plans", Sector: "Healthcare",
		EPS: floatPointer(20.6), ForwardEPS: floatPointer(17), Shares: floatPointer(10),
	}}
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 32, UpstreamTimeout: time.Second}, repository, fake)
	defer service.Close()
	ts := httptest.NewServer(service.Router())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/quote/HUM")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("quote response status=%d decode=%v body=%#v", response.StatusCode, decodeErr, body)
	}
	if body["fair_value"] != nil || body["fair_value_defensive"] != nil || body["fair_value_optimistic"] != nil {
		t.Fatalf("superseded estimate remained visible while recalculating: %#v", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.fundamentalsCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := fake.fundamentalsCalls.Load(); calls != 1 {
		t.Fatalf("superseded Go value did not schedule refresh: fundamentals calls=%d", calls)
	}
}

func TestFastAPIDocumentRoutesExposeOpenAPISchemaAndUIs(t *testing.T) {
	_, ts := testService(t, "")
	response, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&document)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("OpenAPI response status=%d decode=%v", response.StatusCode, decodeErr)
	}
	if got := response.Header.Get("Cache-Control"); got != "" {
		t.Fatalf("OpenAPI Cache-Control=%q, want absent like FastAPI", got)
	}
	if got := response.Header.Get("Referrer-Policy"); got != "" {
		t.Fatalf("OpenAPI Referrer-Policy=%q, want absent like FastAPI", got)
	}
	info, ok := document["info"].(map[string]any)
	if !ok || info["title"] != "Option Scope" || info["version"] != "0.1.0" || document["openapi"] != "3.1.0" {
		t.Fatalf("OpenAPI metadata = %#v, want FastAPI title/version and 3.1.0", document)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok || len(paths) != 11 {
		t.Fatalf("OpenAPI paths = %#v, want all 10 original API routes plus Chan structure", document["paths"])
	}
	for _, path := range []string{
		"/api/events/{stock_symbol}", "/api/quote/{stock_symbol}", "/api/expirations/{stock_symbol}",
		"/api/chain/{stock_symbol}", "/api/flow/{stock_symbol}", "/api/gamma/{stock_symbol}",
		"/api/refresh/{stock_symbol}", "/api/levels/{stock_symbol}", "/api/status/{stock_symbol}", "/api/cleanup",
		"/api/chan/{stock_symbol}",
	} {
		if _, exists := paths[path]; !exists {
			t.Errorf("OpenAPI path %q missing", path)
		}
	}
	chain := paths["/api/chain/{stock_symbol}"].(map[string]any)["get"].(map[string]any)
	parameters := chain["parameters"].([]any)
	expirationSchema := parameters[1].(map[string]any)["schema"].(map[string]any)
	if got := expirationSchema["pattern"]; got != `^\d{4}-\d{2}-\d{2}$` {
		t.Fatalf("expiration pattern=%v, want date regex", got)
	}
	if _, exists := paths["/health"]; exists {
		t.Fatal("health should remain excluded from the FastAPI OpenAPI schema")
	}

	for _, path := range []string{"/docs", "/redoc"} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "/openapi.json") {
			t.Errorf("%s status=%d read=%v body=%q", path, response.StatusCode, readErr, body)
		}
		if got := response.Header.Get("Cache-Control"); got != "" {
			t.Errorf("%s Cache-Control=%q, want absent like FastAPI", path, got)
		}
		if got := response.Header.Get("Referrer-Policy"); got != "" {
			t.Errorf("%s Referrer-Policy=%q, want absent like FastAPI", path, got)
		}
		if strings.Contains(string(body), `\n`) {
			t.Errorf("%s contains literal \\n instead of HTML newlines", path)
		}
	}
}

func TestFastAPIDocumentRoutesHonorAccessKeyAndPassItToUISchemaRequest(t *testing.T) {
	_, ts := testService(t, "secret")
	for _, path := range []string{"/openapi.json", "/docs", "/redoc"} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusForbidden || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") || string(body) != forbiddenPage {
			t.Errorf("unauthorized %s status=%d type=%q read=%v body=%q", path, response.StatusCode, response.Header.Get("Content-Type"), readErr, body)
		}
	}

	for _, test := range []struct {
		path   string
		header bool
	}{
		{path: "/openapi.json?key=secret"},
		{path: "/docs?key=secret"},
		{path: "/redoc?key=secret"},
		{path: "/openapi.json", header: true},
		{path: "/docs", header: true},
		{path: "/redoc", header: true},
	} {
		request, err := http.NewRequest(http.MethodGet, ts.URL+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if test.header {
			request.Header.Set("X-Access-Key", "secret")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			t.Errorf("authorized %s status=%d read=%v body=%q", test.path, response.StatusCode, readErr, body)
			continue
		}
		if strings.HasPrefix(test.path, "/openapi.json") {
			var document map[string]any
			if err := json.Unmarshal(body, &document); err != nil || document["info"] == nil {
				t.Errorf("authorized OpenAPI body=%q decode=%v", body, err)
			}
			continue
		}
		if !strings.Contains(string(body), "/openapi.json?key=secret") {
			t.Errorf("authorized docs page %s did not propagate its key to OpenAPI URL", test.path)
		}
	}

	request, err := http.NewRequest(http.MethodGet, ts.URL+"/docs?key=wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Access-Key", "secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong query key overrode by header, status=%d want 403", response.StatusCode)
	}
}
