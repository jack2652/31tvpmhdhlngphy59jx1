package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type hybridIntradayHistoryStub struct {
	result HistoryResult
	err    error
}

func (s hybridIntradayHistoryStub) Fetch(context.Context, string, string) (HistoryResult, error) {
	return HistoryResult{}, nil
}

func (s hybridIntradayHistoryStub) FetchAdjusted(context.Context, string, string) (HistoryResult, error) {
	return HistoryResult{}, nil
}

func (s hybridIntradayHistoryStub) FetchIntraday(context.Context, string, string) (HistoryResult, error) {
	return s.result, s.err
}

func TestHybridIntradayHistoryMergesSourcesWithYahooPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != alpacaHistoricalBarsPath || r.URL.Query().Get("timeframe") != "15Min" {
			t.Errorf("unexpected Alpaca request: %s %s", r.Method, r.URL.String())
		}
		_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[{"t":"2026-04-23T13:30:00Z","o":90,"h":92,"l":89,"c":91,"v":100},{"t":"2026-09-01T13:30:00Z","o":100,"h":102,"l":99,"c":101,"v":200}]},"next_page_token":null}`)
	}))
	defer server.Close()

	alpaca := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
	alpaca.now = func() time.Time { return now }
	yahoo := HistoryResult{
		Symbol: "AAPL", Source: "go-yfinance", FetchedAt: now.Add(-time.Minute),
		Bars: []map[string]any{
			{"date": "2026-09-01 09:30", "open": 200.0, "high": 202.0, "low": 199.0, "close": 201.0, "volume": int64(300)},
			{"date": "2026-10-22 15:45", "open": 210.0, "high": 212.0, "low": 209.0, "close": 211.0, "volume": int64(400)},
		},
	}
	hybrid := &Hybrid{
		HistoryClient: hybridIntradayHistoryStub{result: yahoo},
		Overnight:     alpaca,
	}
	result, err := hybrid.IntradayHistory(context.Background(), "AAPL", "15m")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bars) != 3 || result.Source != "go-yfinance+alpaca-iex" {
		t.Fatalf("unexpected merged result: %+v", result)
	}
	if result.Bars[0]["date"] != "2026-04-23 09:30" || result.Bars[1]["date"] != "2026-09-01 09:30" || result.Bars[2]["date"] != "2026-10-22 15:45" {
		t.Fatalf("merged bars are not sorted: %#v", result.Bars)
	}
	if result.Bars[1]["close"] != float64(201) || result.Bars[1]["volume"] != int64(300) {
		t.Fatalf("Yahoo did not override overlapping Alpaca bar: %#v", result.Bars[1])
	}
}

func TestHybridIntradayHistoryKeepsYahooWhenAlpacaUnavailable(t *testing.T) {
	yahoo := HistoryResult{Symbol: "AAPL", Source: "go-yfinance", Bars: []map[string]any{{"date": "2026-10-22 09:30", "close": 100.0}}}
	t.Run("no credentials", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
		}))
		defer server.Close()
		hybrid := &Hybrid{HistoryClient: hybridIntradayHistoryStub{result: yahoo}, Overnight: NewAlpaca("", "", server.URL, server.Client())}
		result, err := hybrid.IntradayHistory(context.Background(), "AAPL", "15m")
		if err != nil || len(result.Bars) != 1 || result.Source != "go-yfinance" || requests != 0 {
			t.Fatalf("result=%+v err=%v requests=%d", result, err, requests)
		}
	})

	t.Run("upstream error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		hybrid := &Hybrid{
			HistoryClient: hybridIntradayHistoryStub{result: yahoo},
			Overnight:     NewAlpaca("test-key", "test-secret", server.URL, server.Client()),
		}
		result, err := hybrid.IntradayHistory(context.Background(), "AAPL", "15m")
		if err != nil || len(result.Bars) != 1 || result.Source != "go-yfinance" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestHybridIntradayHistoryFallsBackToAlpacaWhenYahooFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"bars":{"AAPL":[{"t":"2026-10-22T13:30:00Z","o":100,"h":102,"l":99,"c":101,"v":200}]},"next_page_token":null}`)
	}))
	defer server.Close()
	alpaca := NewAlpaca("test-key", "test-secret", server.URL, server.Client())
	alpaca.now = func() time.Time { return time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC) }
	hybrid := &Hybrid{
		HistoryClient: hybridIntradayHistoryStub{err: fmt.Errorf("Yahoo unavailable")},
		Overnight:     alpaca,
	}
	result, err := hybrid.IntradayHistory(context.Background(), "AAPL", "15m")
	if err != nil || result.Source != "alpaca-iex" || len(result.Bars) != 1 || result.Bars[0]["date"] != "2026-10-22 09:30" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
