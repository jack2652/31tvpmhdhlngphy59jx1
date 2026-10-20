package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
)

// The dated authenticated response now supplies the whole chain in one request
// (the SDK chain call was removed to halve upstream round trips). This test
// pins the extension fields that response must retain: Gamma and explicit
// null-vs-zero presence.
func TestYahooOptionsRawExtensionsRetainGammaAndNullPresence(t *testing.T) {
	expiration := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	stamp := expiration.Unix()
	raw := fmt.Sprintf(`{"optionChain":{"result":[{"options":[{"expirationDate":%d,"calls":[`+
		`{"contractSymbol":"RAW-FULL","strike":100,"lastPrice":12.5,"bid":12,"ask":13,"percentChange":2.5,"volume":20,"openInterest":30,"impliedVolatility":0.25,"inTheMoney":true,"gamma":0.015},`+
		`{"contractSymbol":"RAW-NULL","strike":null,"lastPrice":null,"bid":null,"ask":null,"percentChange":null,"volume":null,"openInterest":null,"impliedVolatility":null,"inTheMoney":null,"gamma":null}`+
		`],"puts":[]}]}],"error":null}}`, stamp)
	var rawRequestDate string
	yahoo := NewYahoo(time.Second, 1)
	t.Cleanup(func() { _ = yahoo.Close() })
	yahoo.optionsURL = "https://query1.finance.yahoo.com/v7/finance/options/%s"
	yahoo.optionsAuth = yahooCrumbGetterFunc(func(endpoint string, query url.Values) (*yahooauth.Response, error) {
		if endpoint != "https://query1.finance.yahoo.com/v7/finance/options/TEST" {
			t.Errorf("chain endpoint=%q", endpoint)
		}
		rawRequestDate = query.Get("date")
		return &yahooauth.Response{StatusCode: http.StatusOK, Body: raw}, nil
	})

	rows, fetchedAt, err := yahoo.Chain(context.Background(), "TEST", "2030-01-18")
	if err != nil || fetchedAt == nil || len(rows) != 2 {
		t.Fatalf("Chain()=(%#v,%v,%v)", rows, fetchedAt, err)
	}
	if rawRequestDate != strconv.FormatInt(stamp, 10) {
		t.Errorf("chain date=%q, want %d", rawRequestDate, stamp)
	}

	first := rows[0]
	if first.Strike == nil || *first.Strike != 100 || first.LastPrice == nil || *first.LastPrice != 12.5 || first.Bid == nil || *first.Bid != 12 || first.Ask == nil || *first.Ask != 13 || first.Volume == nil || *first.Volume != 20 || first.OpenInterest == nil || *first.OpenInterest != 30 || first.ImpliedVolatility == nil || *first.ImpliedVolatility != 0.25 {
		t.Errorf("raw numeric fields not retained: %#v", first)
	}
	if first.Gamma == nil || *first.Gamma != 0.015 || !first.InTheMoneyKnown || !first.InTheMoney {
		t.Errorf("raw Gamma/presence extension=%#v", first)
	}
	second := rows[1]
	if second.Gamma != nil || second.Strike != nil || second.LastPrice != nil || second.Bid != nil || second.Ask != nil || second.Volume != nil || second.OpenInterest != nil || second.ImpliedVolatility != nil || second.InTheMoneyKnown {
		t.Errorf("explicit raw nulls were not retained: %#v", second)
	}
}
