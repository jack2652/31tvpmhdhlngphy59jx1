package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlpacaFetchIntradayUsesHistoricalBarsPaginationAndCache(t *testing.T) {
	const key, secret = "test-key", "test-secret"
	now := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != alpacaHistoricalBarsPath {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, alpacaHistoricalBarsPath)
		}
		if got := r.Header.Get("APCA-API-KEY-ID"); got != key {
			t.Errorf("key header = %q", got)
		}
		if got := r.Header.Get("APCA-API-SECRET-KEY"); got != secret {
			t.Errorf("secret header = %q", got)
		}
		query := r.URL.Query()
		for name, want := range map[string]string{
			"symbols": "AAPL", "timeframe": "15Min", "feed": "iex", "adjustment": "raw",
			"limit": "10000", "sort": "asc", "start": now.AddDate(0, -6, 0).UTC().Format(time.RFC3339Nano),
			"end": now.UTC().Format(time.RFC3339Nano),
		} {
			if got := query.Get(name); got != want {
				t.Errorf("query %s = %q, want %q", name, got, want)
			}
		}
		requests.Add(1)
		switch query.Get("page_token") {
		case "":
			_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[{"t":"2026-10-22T13:30:00Z","o":100,"h":102,"l":99,"c":101,"v":123},{"t":"2026-10-22T13:15:00Z","o":98,"h":100,"l":97,"c":99,"v":20},{"t":"2026-10-22T20:00:00Z","o":101,"h":102,"l":100,"c":101,"v":10}]},"next_page_token":"next"}`)
		case "next":
			_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[{"t":"2026-10-22T13:45:00Z","o":101,"h":103,"l":100,"c":102,"v":456}]},"next_page_token":null}`)
		default:
			t.Errorf("unexpected page token %q", query.Get("page_token"))
			_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[]},"next_page_token":null}`)
		}
	}))
	defer server.Close()

	client := NewAlpaca(key, secret, server.URL, server.Client())
	client.now = func() time.Time { return now }
	result, err := client.FetchIntraday(context.Background(), "aapl", "15m")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bars) != 2 || result.Source != "alpaca-iex" || result.Bars[0]["date"] != "2026-10-22 09:30" || result.Bars[1]["date"] != "2026-10-22 09:45" {
		t.Fatalf("unexpected intraday history: %+v", result)
	}
	if result.Bars[0]["volume"] != int64(123) || result.Bars[1]["close"] != float64(102) {
		t.Fatalf("unexpected normalized OHLCV: %#v", result.Bars)
	}
	if _, err := client.FetchIntraday(context.Background(), "AAPL", "15m"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests after cached read = %d, want 2", requests.Load())
	}
	now = now.Add(24*time.Hour + time.Minute)
	if _, err := client.FetchIntraday(context.Background(), "AAPL", "15m"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 {
		t.Fatalf("requests after cache expiration = %d, want 4", requests.Load())
	}
}

func TestNormalizeAlpacaIntradayBarsUsesEasternSessionAndTimezone(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	bars := []alpacaHistoricalBar{
		{Timestamp: time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC), Open: 10, High: 12, Low: 9, Close: 11, Volume: "100"},
		{Timestamp: time.Date(2026, 1, 5, 14, 15, 0, 0, time.UTC), Open: 10, High: 11, Low: 9, Close: 10, Volume: "10"},
		{Timestamp: time.Date(2026, 1, 5, 21, 0, 0, 0, time.UTC), Open: 11, High: 12, Low: 10, Close: 11, Volume: "10"},
		{Timestamp: time.Date(2026, 6, 5, 13, 30, 0, 0, time.UTC), Open: 20, High: 22, Low: 19, Close: 21, Volume: "200"},
		{Timestamp: time.Date(2026, 6, 6, 13, 30, 0, 0, time.UTC), Open: 20, High: 22, Low: 19, Close: 21, Volume: "200"},
	}
	result, err := normalizeAlpacaIntradayBars("AAPL", bars, location)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0]["date"] != "2026-01-05 09:30" || result[1]["date"] != "2026-06-05 09:30" {
		t.Fatalf("normalized bars = %#v", result)
	}
}

func TestAlpacaFetchIntradayMarksAuthFailureAndRejectsTrailingJSON(t *testing.T) {
	t.Run("auth failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, "do not expose test-secret")
		}))
		defer server.Close()
		client := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
		if _, err := client.FetchIntraday(context.Background(), "AAPL", "15m"); err == nil || strings.Contains(err.Error(), "test-secret") {
			t.Fatalf("FetchIntraday() error = %v, want sanitized auth failure", err)
		}
		if client.CanRequest() {
			t.Fatal("credentials should be in cooldown after HTTP 401")
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[]},"next_page_token":null} {}`)
		}))
		defer server.Close()
		client := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
		if _, err := client.FetchIntraday(context.Background(), "AAPL", "15m"); err == nil {
			t.Fatal("expected trailing JSON to be rejected")
		}
	})
}

func TestAlpacaIntradayCacheHasBoundedEntries(t *testing.T) {
	now := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		symbol := r.URL.Query().Get("symbols")
		_, _ = fmt.Fprintf(w, `{"bars":{"%s":[{"t":"2026-10-22T13:30:00Z","o":100,"h":102,"l":99,"c":101,"v":10}]},"next_page_token":null}`, symbol)
	}))
	defer server.Close()
	client := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
	client.now = func() time.Time { return now }
	for index, symbol := range []string{"AAPL", "MSFT", "GOOG", "META", "SPY", "AAPL"} {
		if index > 0 {
			now = now.Add(time.Minute)
		}
		if _, err := client.FetchIntraday(context.Background(), symbol, "15m"); err != nil {
			t.Fatalf("FetchIntraday(%s): %v", symbol, err)
		}
	}
	if requests.Load() != 6 {
		t.Fatalf("historical requests=%d, want six after the oldest cache entry is evicted", requests.Load())
	}
}
