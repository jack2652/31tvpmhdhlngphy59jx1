package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestYahooQuotePropagatesDailyFallbackErrorForMissingPrimaryField(t *testing.T) {
	dailyRequested := make(chan struct{}, 1)
	yahoo := NewYahoo(time.Second, 1)
	setYahooSDKTestTicker(yahoo, &yahooSDKTestTicker{
		quote: &ymodels.Quote{Symbol: "TEST", Currency: "USD", RegularMarketPrice: 110, RegularMarketOpen: 108, MarketState: "REGULAR"},
		historyFunc: func(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
			if params.Interval == "1m" {
				return nil, nil
			}
			dailyRequested <- struct{}{}
			return nil, errors.New("Yahoo returned HTTP 503")
		},
	})
	_, err := yahoo.Quote(context.Background(), "TEST")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("Quote() error = %v, want daily fallback HTTP 503", err)
	}
	select {
	case <-dailyRequested:
	default:
		t.Fatal("missing primary previous close did not request daily fallback")
	}

	// Python MarketDataProvider.quote() calls _history_quote() when either
	// fast_info price or previous_close is missing; its outer exception handler
	// returns an error if that required history fallback fails.
}
