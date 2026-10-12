package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
)

type blockedFirstQuoteValuationProvider struct {
	*testMarketProvider
	started chan struct{}
	release chan struct{}
}

func (p *blockedFirstQuoteValuationProvider) Fundamentals(ctx context.Context, _ string) (provider.YahooFundamentals, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return provider.YahooFundamentals{}, errors.New("429 rate limit")
	case <-ctx.Done():
		return provider.YahooFundamentals{}, ctx.Err()
	}
}

func TestFirstQuoteReturnsPendingWhileValuationRunsInBackground(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "first-quote-valuation.db")
	repository, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	price := 123.45
	base := &testMarketProvider{quote: model.Quote{Price: &price, Currency: "USD", Provider: "yahoo"}}
	fake := &blockedFirstQuoteValuationProvider{
		testMarketProvider: base,
		started:            make(chan struct{}, 1),
		release:            make(chan struct{}),
	}
	service := NewWithProvider(config.Config{DatabasePath: databasePath, UpstreamTimeout: time.Second}, repository, fake)
	server := httptest.NewServer(service.Router())
	defer server.Close()
	defer func() {
		select {
		case <-fake.release:
		default:
			close(fake.release)
		}
		if err := service.Close(); err != nil {
			t.Errorf("close service: %v", err)
		}
	}()

	response, err := http.Get(server.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("first quote status=%d body=%#v", response.StatusCode, body)
	}
	if body["price"] != nil || body["source"] != "pending" || body["provider"] != "upstream" || body["fair_value_status"] != "pending" || body["fair_value_pending"] != true {
		t.Fatalf("first quote response=%#v; want Python-compatible pending placeholder", body)
	}
	if warning, present := body["fair_value_warning"]; !present || warning != nil {
		t.Fatalf("first fair_value_warning=%#v present=%v; want explicit null", warning, present)
	}
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("first quote did not start background fundamentals fetch")
	}

	startedAt := time.Now()
	refreshResponse, err := http.Get(server.URL + "/api/quote/QQQ?refresh=true")
	if err != nil {
		t.Fatal(err)
	}
	defer refreshResponse.Body.Close()
	var refreshed map[string]any
	if err := json.NewDecoder(refreshResponse.Body).Decode(&refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshResponse.StatusCode != http.StatusOK || refreshed["price"] != price || refreshed["fair_value_status"] != "pending" || refreshed["fair_value_pending"] != true {
		t.Fatalf("refresh quote response=%#v status=%d; want immediate market quote with pending valuation", refreshed, refreshResponse.StatusCode)
	}
	if elapsed := time.Since(startedAt); elapsed >= 900*time.Millisecond {
		t.Fatalf("refresh quote waited for blocked valuation: %s", elapsed)
	}
	if warning, present := refreshed["fair_value_warning"]; !present || warning != nil {
		t.Fatalf("refresh fair_value_warning=%#v present=%v; want explicit null", warning, present)
	}

	close(fake.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		payload, _, found, cacheErr := repository.GetAnalysisCacheEntry(fairValueStatusCacheKey("QQQ"))
		if cacheErr != nil {
			t.Fatal(cacheErr)
		}
		if cachePayload, ok := payload.(map[string]any); found && ok {
			status, _ := cachePayload["status"].(string)
			if status == "retry" {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	payload, _, found, cacheErr := repository.GetAnalysisCacheEntry(fairValueStatusCacheKey("QQQ"))
	cachePayload, payloadOK := payload.(map[string]any)
	if cacheErr != nil || !found || !payloadOK || cachePayload["status"] != "retry" || cachePayload["warning"] != "Yahoo 数据源暂时限流，将自动重试" {
		t.Fatalf("published valuation status=%#v found=%v err=%v; want retry", payload, found, cacheErr)
	}

	secondResponse, err := http.Get(server.URL + "/api/quote/QQQ")
	if err != nil {
		t.Fatal(err)
	}
	defer secondResponse.Body.Close()
	var secondBody map[string]any
	if err := json.NewDecoder(secondResponse.Body).Decode(&secondBody); err != nil {
		t.Fatal(err)
	}
	if secondBody["fair_value_status"] != "retry" || secondBody["fair_value_pending"] != true || secondBody["fair_value_warning"] == nil {
		t.Fatalf("second quote did not expose the published retry result: %#v", secondBody)
	}
}
