package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/store"
)

func newAPIRemainingParityService(t *testing.T, market *testMarketProvider) (*Service, *httptest.Server, *store.SQLite) {
	t.Helper()
	cfg := config.Config{
		DatabasePath:        filepath.Join(t.TempDir(), "api-parity.db"),
		UpstreamTimeout:     time.Second,
		UpstreamConcurrency: 1,
	}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, market)
	ts := httptest.NewServer(service.Router())
	t.Cleanup(func() {
		ts.Close()
		_ = service.Close()
	})
	return service, ts, repository
}

func TestSSEEventPayloadMatchesPythonNullableExpiration(t *testing.T) {
	for _, event := range []model.Event{
		{Symbol: "QQQ", Kind: "quote", CreatedAt: "2030-01-18T15:00:00Z"},
		{Symbol: "QQQ", Kind: "snapshot", CreatedAt: "2030-01-18T15:00:00Z", Expiration: "2030-01-18"},
	} {
		encoded, err := json.Marshal(eventAPIValue(event))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		var expiration any
		if event.Expiration != "" {
			expiration = event.Expiration
		}
		want := map[string]any{
			"symbol": event.Symbol, "kind": event.Kind,
			"created_at": event.CreatedAt, "expiration": expiration,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SSE payload=%#v, want Python format_event payload %#v", got, want)
		}
	}
}

func TestRefreshRejectsFractionalIntegerQuery(t *testing.T) {
	market := &testMarketProvider{}
	_, ts, _ := newAPIRemainingParityService(t, market)
	response, err := http.Post(ts.URL+"/api/refresh/QQQ?max_age=60.5", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("fractional max_age status=%d, want 422", response.StatusCode)
	}
	if got := market.quoteCalls.Load() + market.expirationCalls.Load(); got != 0 {
		t.Fatalf("invalid max_age made %d upstream calls", got)
	}
}

func TestRefreshFailureUsesPythonStaleFallbackRules(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	oldFetchedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	oldPrice, newPrice, strike := 100.0, 101.0, 100.0
	cases := []struct {
		name             string
		requested        bool
		saveQuote        bool
		saveChain        bool
		failure          string
		wantStatus       int
		wantStale        bool
		wantQuoteOnly    bool
		wantOldQuote     bool
		wantQuotePersist bool
	}{
		{name: "explicit chain failure without cache", requested: true, failure: "chain", wantStatus: http.StatusBadGateway},
		{name: "explicit chain failure with old chain", requested: true, saveQuote: true, saveChain: true, failure: "chain", wantStatus: http.StatusOK, wantStale: true, wantOldQuote: true, wantQuotePersist: true},
		{name: "explicit chain failure with quote only", requested: true, saveQuote: true, failure: "chain", wantStatus: http.StatusBadGateway, wantQuotePersist: true},
		{name: "nearest chain failure falls back to quote", saveQuote: true, failure: "chain", wantStatus: http.StatusOK, wantStale: true, wantQuoteOnly: true, wantOldQuote: true, wantQuotePersist: true},
		{name: "quote failure with old chain", requested: true, saveQuote: true, saveChain: true, failure: "quote", wantStatus: http.StatusOK, wantStale: true, wantOldQuote: true, wantQuotePersist: true},
		{name: "expiration failure with old chain", requested: true, saveQuote: true, saveChain: true, failure: "expiration", wantStatus: http.StatusOK, wantStale: true, wantOldQuote: true, wantQuotePersist: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			market := &testMarketProvider{
				expirations: []string{expiration},
				quote:       model.Quote{Price: &newPrice, Provider: "test", Sessions: map[string]any{}},
			}
			switch tc.failure {
			case "quote":
				market.quoteErr = fmt.Errorf("quote unavailable")
			case "expiration":
				market.expirationErr = fmt.Errorf("expiration unavailable")
			case "chain":
				market.chainErr = fmt.Errorf("chain unavailable")
			}
			_, ts, repository := newAPIRemainingParityService(t, market)
			if tc.saveQuote {
				if err := repository.SaveQuote(model.Quote{
					Symbol: "QQQ", Price: &oldPrice, Provider: "python", FetchedAt: &oldFetchedAt, Sessions: map[string]any{},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.saveChain {
				if err := repository.SaveChain(model.ChainResponse{
					Symbol: "QQQ", Expiration: expiration, FetchedAt: &oldFetchedAt,
					Data: []model.OptionRow{{ContractSymbol: "QQQ-old-call", ContractType: "call", Strike: &strike, Expiration: expiration}},
				}); err != nil {
					t.Fatal(err)
				}
			}

			path := "/api/refresh/QQQ?max_age=0"
			if tc.requested {
				path += "&expiration=" + expiration
			}
			response, err := http.Post(ts.URL+path, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d body=%#v, want %d", response.StatusCode, body, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				quote, found, err := repository.LatestQuote("QQQ")
				if err != nil || found != tc.wantQuotePersist {
					t.Fatalf("persisted quote found=%v, want %v; quote=%#v err=%v", found, tc.wantQuotePersist, quote, err)
				}
				return
			}
			stale, _ := body["stale"].(bool)
			quoteOnly, _ := body["quote_only"].(bool)
			if stale != tc.wantStale || quoteOnly != tc.wantQuoteOnly {
				t.Fatalf("stale/quote_only=%v/%v, want %v/%v: %#v", stale, quoteOnly, tc.wantStale, tc.wantQuoteOnly, body)
			}
			quote, ok := body["quote"].(map[string]any)
			if !ok {
				t.Fatalf("missing quote fallback: %#v", body)
			}
			wantPrice := newPrice
			if tc.wantOldQuote {
				wantPrice = oldPrice
			}
			if quote["price"] != wantPrice {
				t.Fatalf("fallback quote price=%#v, want %v", quote["price"], wantPrice)
			}
			stored, found, err := repository.LatestQuote("QQQ")
			if err != nil || found != tc.wantQuotePersist {
				t.Fatalf("persisted quote found=%v, want %v; quote=%#v err=%v", found, tc.wantQuotePersist, stored, err)
			}
			if found && stored.Price != nil && tc.wantOldQuote && *stored.Price != oldPrice {
				t.Fatalf("stale fallback overwrote old quote: %#v", stored)
			}
		})
	}
}

func TestRemainingAPIsUseLastRepeatedScalarQueryValue(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	price := 100.0
	market := &testMarketProvider{
		expirations: []string{expiration},
		quote:       model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}},
	}
	_, ts, _ := newAPIRemainingParityService(t, market)
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
	for _, path := range []string{
		"/api/quote/QQQ?refresh=invalid&refresh=false",
		"/api/expirations/QQQ?refresh=invalid&refresh=false",
		"/api/chain/QQQ?expiration=invalid&expiration=" + expiration,
		"/api/flow/QQQ?expiration=invalid&expiration=" + expiration,
		"/api/status/QQQ?ignored=invalid&ignored=still-ignored",
	} {
		status, body := get(path)
		if status != http.StatusOK {
			t.Errorf("%s status=%d body=%#v, want 200", path, status, body)
		}
	}
	response, err := http.Post(
		ts.URL+"/api/refresh/QQQ?expiration=invalid&expiration="+expiration+"&max_age=invalid&max_age=0",
		"application/json", strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		t.Fatalf("refresh repeated query values status=%d body=%#v, want 200", response.StatusCode, body)
	}
}
