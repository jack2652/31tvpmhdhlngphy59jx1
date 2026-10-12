package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnjoon/go-yfinance/pkg/models"
	"option-scope-go/internal/model"
)

func TestHybridSnapshotQuoteFallsBackToCboeOutsideRegularSession(t *testing.T) {
	var cboeRequests atomic.Int64
	cboeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cboeRequests.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"current_price":100.5,"prev_day_close":99,"options":[]}}`)
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
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 8, 0, 0, 0, newYork) }

	quote, err := hybrid.SnapshotQuote(context.Background(), "QQQ")
	if err != nil || quote.Provider != "cboe-delayed" || quote.Price == nil || *quote.Price != 100.5 {
		t.Fatalf("SnapshotQuote() = %#v, %v; want Cboe fallback", quote, err)
	}
	if got := cboeRequests.Load(); got != 1 {
		t.Fatalf("Cboe requests = %d, want 1", got)
	}
}

func TestHybridSnapshotQuoteUsesRegularQuoteDuringRegularSession(t *testing.T) {
	yahoo := NewYahoo(time.Second, 2)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{quote: &models.Quote{
		Symbol: "QQQ", Currency: "USD", RegularMarketPrice: 101, RegularMarketPreviousClose: 100, MarketState: "REGULAR",
	}})
	hybrid := NewHybrid(yahoo, nil, nil)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	hybrid.now = func() time.Time { return time.Date(2026, time.October, 7, 10, 0, 0, 0, newYork) }

	quote, err := hybrid.SnapshotQuote(context.Background(), "QQQ")
	if err != nil || quote.Provider != "yahoo-chart" || quote.Price == nil || *quote.Price != 101 {
		t.Fatalf("SnapshotQuote() = %#v, %v; want regular Yahoo quote", quote, err)
	}
}

func TestSnapshotFallbackPreservesSessionsWhenOriginalQuoteHasNoPrice(t *testing.T) {
	sessions := map[string]any{"post": map[string]any{"reference_close": float64(100)}}
	fallback := model.Quote{Provider: "cboe-delayed", Sessions: map[string]any{}}
	original := model.Quote{Sessions: sessions}

	got := withSnapshotFallbackSessions(fallback, original, true)
	if got.Sessions["post"] == nil {
		t.Fatalf("fallback sessions = %#v, want original sessions", got.Sessions)
	}
	if got := withSnapshotFallbackSessions(fallback, original, false); len(got.Sessions) != 0 {
		t.Fatalf("failed original quote sessions = %#v, want no copied sessions", got.Sessions)
	}
}
