package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	chananalysis "option-scope-go/internal/chan"
)

func TestChanAnalysisCacheKeyPreservesJSONFingerprint(t *testing.T) {
	cutoff := time.Date(2026, time.October, 28, 20, 7, 0, 0, time.UTC)
	daily := []chananalysis.Bar{{Date: "2026-10-27", Open: 100, High: 105, Low: 99, Close: 103, Volume: 1500, RawOpen: 99.5, AdjustmentFactor: 1.02, Chain: 3}}
	intraday := []chananalysis.Bar{{Date: "2026-10-28", Open: 103, High: 104, Low: 102, Close: 103.5, Volume: 500}}
	market := []chananalysis.Bar{{Date: "2026-10-27", Open: 600, High: 605, Low: 599, Close: 603, Volume: 2000}}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	local := cutoff.In(location)
	bucketMinute := (local.Minute() / 15) * 15
	bucket := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), bucketMinute, 0, 0, location).Format(time.RFC3339)
	payload := struct {
		Version  string             `json:"version"`
		Symbol   string             `json:"symbol"`
		Bucket   string             `json:"bucket"`
		Daily    []chananalysis.Bar `json:"daily"`
		Intraday []chananalysis.Bar `json:"intraday"`
		Market   []chananalysis.Bar `json:"market"`
	}{
		Version:  chananalysis.RecursivePreviewVersion + "|" + chananalysis.MarketEnvironmentRuleVersion,
		Symbol:   "QQQ",
		Bucket:   bucket,
		Daily:    daily,
		Intraday: intraday,
		Market:   market,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	want := "chan-adjusted-v2:" + hex.EncodeToString(digest[:])
	if got := chanAnalysisCacheKey("QQQ", daily, intraday, market, cutoff); got != want {
		t.Fatalf("cache key = %q, want existing JSON fingerprint %q", got, want)
	}
}

func TestChanAnalysisCacheKeyConcurrentCallsRemainStable(t *testing.T) {
	cutoff := time.Date(2026, time.October, 28, 20, 7, 0, 0, time.UTC)
	daily := []chananalysis.Bar{{Date: "2026-10-27", Open: 100, High: 105, Low: 99, Close: 103, Volume: 1500}}
	intraday := []chananalysis.Bar{{Date: "2026-10-28", Open: 103, High: 104, Low: 102, Close: 103.5, Volume: 500}}
	market := []chananalysis.Bar{{Date: "2026-10-27", Open: 600, High: 605, Low: 599, Close: 603, Volume: 2000}}
	want := chanAnalysisCacheKey("QQQ", daily, intraday, market, cutoff)
	results := make(chan string, 32)
	var workers sync.WaitGroup
	workers.Add(cap(results))
	for range cap(results) {
		go func() {
			defer workers.Done()
			results <- chanAnalysisCacheKey("QQQ", daily, intraday, market, cutoff)
		}()
	}
	workers.Wait()
	close(results)
	for got := range results {
		if got != want {
			t.Fatalf("concurrent cache key = %q, want %q", got, want)
		}
	}
}
