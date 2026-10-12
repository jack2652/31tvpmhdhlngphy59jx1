package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
)

func TestQuoteWithoutSnapshotStartsValuationAndPollReadsCachedResult(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "quote-without-snapshot.db")
	repository, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	shares, eps, forwardEPS := 1.0, 10.0, 10.0
	upstream := &testMarketProvider{fundamentals: provider.YahooFundamentals{
		Shares: &shares, EPS: &eps, ForwardEPS: &forwardEPS,
	}}
	service := NewWithProvider(config.Config{
		DatabasePath: databasePath, AnalysisCacheEntries: 16, UpstreamTimeout: time.Second,
	}, repository, upstream)
	defer service.Close()
	router := service.Router()

	request := httptest.NewRequest(http.MethodGet, "/api/quote/TEST", nil)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, request)
	if first.Code != http.StatusOK {
		t.Fatalf("initial quote status=%d, want 200: %s", first.Code, first.Body.String())
	}
	var initial map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial["source"] != "pending" || initial["fair_value_status"] != "pending" {
		t.Fatalf("initial response=%#v, want pending quote", initial)
	}
	if upstream.quoteCalls.Load() != 0 {
		t.Fatalf("no-snapshot quote read made %d quote-provider calls, want none", upstream.quoteCalls.Load())
	}

	deadline := time.Now().Add(2 * time.Second)
	var cached map[string]any
	for time.Now().Before(deadline) {
		if payload, fresh := service.cachedValuation("TEST"); fresh && payload["status"] == "ready" && payload["value"] != nil {
			cached = payload
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cached == nil {
		t.Fatalf("valuation did not reach ready cache state; fundamentals calls=%d", upstream.fundamentalsCalls.Load())
	}
	if upstream.fundamentalsCalls.Load() != 1 {
		t.Fatalf("fundamentals calls=%d, want exactly one background request", upstream.fundamentalsCalls.Load())
	}

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/quote/TEST", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("poll quote status=%d, want 200: %s", second.Code, second.Body.String())
	}
	var polled map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled["source"] != "pending" || polled["fair_value_source"] != goFairValueSource ||
		polled["fair_value_status"] != "ready" || polled["fair_value"] != cached["value"] {
		t.Fatalf("poll response=%#v, want pending quote decorated with ready cached valuation", polled)
	}
	if upstream.fundamentalsCalls.Load() != 1 {
		t.Fatalf("cached quote poll repeated fundamentals request: calls=%d", upstream.fundamentalsCalls.Load())
	}
}
