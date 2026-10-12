package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestYahooQuoteDailyFallbackDoesNotReusePriorDayOpen(t *testing.T) {
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", MarketState: "REGULAR"},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval == "1m" {
				return nil, nil
			}
			return []ymodels.Bar{
				{Date: time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC), Open: 99, Close: 100},
				{Date: time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC), Close: 102.5},
			}, nil
		},
	})
	quote, err := yahoo.Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if quote.Price == nil || *quote.Price != 102.5 || quote.PreviousClose == nil || *quote.PreviousClose != 100 {
		t.Fatalf("daily fallback price/previous close = %v/%v, want 102.5/100", quote.Price, quote.PreviousClose)
	}
	if quote.TodayOpen != nil {
		t.Fatalf("today open = %v, want nil when the latest daily row has no open", *quote.TodayOpen)
	}
}

func TestAlpacaAndYahooRejectTrailingJSONData(t *testing.T) {
	t.Run("Alpaca snapshot", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"TEST":{"latestTrade":{"p":10}}} trailing`)
		}))
		defer server.Close()

		_, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
		if err == nil || err.Error() != "Alpaca returned an invalid snapshot response" {
			t.Fatalf("Quote() error = %v, want invalid-snapshot error", err)
		}
	})

	t.Run("Yahoo option response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"optionChain":{"result":[{"expirationDates":[]}],"error":null}} {}`)
		}))
		defer server.Close()

		yahoo := NewYahoo(time.Second, 1)
		yahoo.optionsURL = server.URL + "/v7/finance/options/%s"
		useLocalYahooOptionsAuth(t, yahoo)
		setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{optionChain: &ymodels.OptionChain{
			Calls: []ymodels.Option{{ContractSymbol: "TEST300118C00100000", Strike: 100}},
		}})
		if _, _, err := yahoo.Chain(context.Background(), "TEST", "2030-01-18"); err == nil {
			t.Fatal("Chain() error = nil, want trailing JSON rejection")
		}
	})
}
