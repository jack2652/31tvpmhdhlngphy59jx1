package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
	models "github.com/wnjoon/go-yfinance/pkg/models"
)

type optionsParityTicker struct {
	yahooTicker
	expirations []time.Time
	chain       *models.OptionChain
	optionCalls int
	chainDates  []string
}

func (t *optionsParityTicker) Options() ([]time.Time, error) {
	t.optionCalls++
	return append([]time.Time(nil), t.expirations...), nil
}

func (t *optionsParityTicker) OptionChain(date string) (*models.OptionChain, error) {
	t.chainDates = append(t.chainDates, date)
	return t.chain, nil
}

func (t *optionsParityTicker) Close() {}

func TestYahooOptionsUseSDKAndOnlySupplementMissingGammaPresence(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	raw := fmt.Sprintf("{\"optionChain\":{\"result\":[{\"options\":[{\"expirationDate\":%d,\"calls\":[{\"contractSymbol\":\"SDK-RAW\",\"gamma\":0.015},{\"contractSymbol\":\"SDK-NULL\",\"strike\":null,\"lastPrice\":null,\"bid\":null,\"ask\":null,\"percentChange\":null,\"volume\":null,\"openInterest\":null,\"impliedVolatility\":null,\"gamma\":null,\"inTheMoney\":null}],\"puts\":[]}]}],\"error\":null}}", stamp)
	var rawRequestDate string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRequestDate = r.URL.Query().Get("date")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, raw)
	}))
	defer upstream.Close()

	ticker := &optionsParityTicker{
		expirations: []time.Time{expiration},
		chain: &models.OptionChain{Expiration: expiration, Calls: []models.Option{
			{ContractSymbol: "SDK-RAW", Strike: 100, LastPrice: 12.5, Bid: 12, Ask: 13, PercentChange: 2.5, Volume: 20, OpenInterest: 30, ImpliedVolatility: 0.25, InTheMoney: true},
			{ContractSymbol: "SDK-NULL", Strike: 90, LastPrice: 4, Bid: 3, Ask: 5, PercentChange: -1, Volume: 8, OpenInterest: 9, ImpliedVolatility: 0.4, InTheMoney: true},
			{ContractSymbol: "SDK-ONLY", Strike: 80, LastPrice: 2, Bid: 1, Ask: 3, PercentChange: 1, Volume: 5, OpenInterest: 6, ImpliedVolatility: 0.3, InTheMoney: false},
		}},
	}
	yahoo := NewYahoo(time.Second, 1)
	t.Cleanup(func() { _ = yahoo.Close() })
	yahoo.optionsURL = upstream.URL + "/v7/finance/options/%s"
	yahoo.optionsAuth = yahooCrumbGetterFunc(func(endpoint string, query url.Values) (*yahooauth.Response, error) {
		if endpoint != upstream.URL+"/v7/finance/options/TEST" {
			t.Errorf("supplement endpoint=%q", endpoint)
		}
		rawRequestDate = query.Get("date")
		return &yahooauth.Response{StatusCode: http.StatusOK, Body: raw}, nil
	})
	yahoo.tickerFactory = func(symbol string) (yahooTicker, error) {
		if symbol != "TEST" {
			t.Errorf("SDK ticker symbol=%q, want TEST", symbol)
		}
		return ticker, nil
	}

	expirations, err := yahoo.Expirations(context.Background(), "TEST")
	if err != nil || len(expirations) != 1 || expirations[0] != "2030-01-18" {
		t.Fatalf("Expirations()=(%#v,%v)", expirations, err)
	}
	rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", "2030-01-18")
	if err != nil || fetchedAt == nil || len(rows) != 3 {
		t.Fatalf("Chain()=(%#v,%v,%v)", rows, fetchedAt, err)
	}
	if ticker.optionCalls != 1 {
		t.Errorf("SDK Options calls=%d, want one", ticker.optionCalls)
	}
	if len(ticker.chainDates) != 1 || ticker.chainDates[0] != yahooSDKExpirationLookupDate(expiration, time.Local) {
		t.Errorf("SDK OptionChain dates=%v, want local cache key %q", ticker.chainDates, yahooSDKExpirationLookupDate(expiration, time.Local))
	}
	if rawRequestDate != strconv.FormatInt(stamp, 10) {
		t.Errorf("supplement date=%q, want %d", rawRequestDate, stamp)
	}

	first := rows[0]
	if first.Strike == nil || *first.Strike != 100 || first.LastPrice == nil || *first.LastPrice != 12.5 || first.Bid == nil || *first.Bid != 12 || first.Ask == nil || *first.Ask != 13 || first.Volume == nil || *first.Volume != 20 || first.OpenInterest == nil || *first.OpenInterest != 30 || first.ImpliedVolatility == nil || *first.ImpliedVolatility != 0.25 {
		t.Errorf("SDK numeric fields were not retained when absent from raw extension: %#v", first)
	}
	if first.Gamma == nil || *first.Gamma != 0.015 || !first.InTheMoneyKnown || !first.InTheMoney {
		t.Errorf("raw Gamma/presence extension=%#v", first)
	}
	second := rows[1]
	if second.Gamma != nil || second.Strike != nil || second.LastPrice != nil || second.Bid != nil || second.Ask != nil || second.Volume != nil || second.OpenInterest != nil || second.ImpliedVolatility != nil || second.InTheMoneyKnown {
		t.Errorf("explicit raw nulls were not retained: %#v", second)
	}
	third := rows[2]
	if third.Strike == nil || *third.Strike != 80 || third.Gamma != nil || third.InTheMoneyKnown || third.InTheMoney {
		t.Errorf("SDK-only contract was not retained without a raw extension: %#v", third)
	}
}
