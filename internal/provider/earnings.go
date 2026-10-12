package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	yfinanceclient "github.com/wnjoon/go-yfinance/pkg/client"
	"github.com/wnjoon/go-yfinance/pkg/models"
	"github.com/wnjoon/go-yfinance/pkg/ticker"
	"option-scope-go/internal/outboundhttp"
)

// YahooEarningsClient fetches earnings dates through go-yfinance's Calendar API.
// The SDK ticker has no context-aware methods, so runYahooSDK holds the shared
// concurrency slot until its request finishes or the SDK timeout expires.
type YahooEarningsClient struct {
	Client        *http.Client
	Gate          chan struct{}
	GateWait      time.Duration
	tickerFactory yahooTickerFactory
}

func NewYahooEarningsClient(client *http.Client) *YahooEarningsClient {
	if client == nil {
		client = outboundhttp.DefaultBrowserClient(30 * time.Second)
	}
	return &YahooEarningsClient{Client: client}
}

// Fetch returns unique earnings dates as ascending YYYY-MM-DD strings in the
// New York timezone. Yahoo's calendar can omit earnings dates; that is an
// empty result, while SDK and transport failures remain errors.
func (c *YahooEarningsClient) Fetch(ctx context.Context, rawSymbol string) ([]string, error) {
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return nil, errors.New("invalid symbol")
	}
	if c == nil || c.Client == nil {
		return nil, errors.New("Yahoo earnings client is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, errors.New("America/New_York timezone is unavailable")
	}
	factory := c.tickerFactory
	if factory == nil {
		factory = c.standaloneTickerFactory()
	}
	calendar, err := runYahooSDK(ctx, c.Gate, c.GateWait, factory, symbol, func(t yahooTicker) (*models.Calendar, error) {
		return t.Calendar()
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Yahoo earnings request failed")
	}
	if calendar == nil {
		return []string{}, nil
	}
	return normalizeYahooEarningsDates(calendar.EarningsDate, location), nil
}

func (c *YahooEarningsClient) standaloneTickerFactory() yahooTickerFactory {
	timeout := 30
	if c.Client.Timeout > 0 {
		timeout = int(math.Ceil(c.Client.Timeout.Seconds()))
		if timeout < 1 {
			timeout = 1
		}
	}
	proxy := outboundhttp.ProxyAddressFromTransport(c.Client.Transport)
	ja3, userAgent := outboundhttp.BrowserOptions()
	return func(symbol string) (yahooTicker, error) {
		options := []yfinanceclient.ClientOption{
			yfinanceclient.WithTimeout(timeout),
			yfinanceclient.WithJA3(ja3),
			yfinanceclient.WithUserAgent(userAgent),
			// Pass an empty value too, so go-yfinance cannot silently pick up a
			// proxy from its separate global config when MARKET_PROXY is unset.
			yfinanceclient.WithProxy(proxy),
		}
		sdkClient, err := yfinanceclient.New(options...)
		if err != nil {
			return nil, err
		}
		sdkTicker, err := ticker.New(symbol, ticker.WithClient(sdkClient))
		if err != nil {
			sdkClient.Close()
			return nil, err
		}
		return &ownedYahooEarningsTicker{yahooTicker: sdkTicker, client: sdkClient}, nil
	}
}

type ownedYahooEarningsTicker struct {
	yahooTicker
	client *yfinanceclient.Client
}

func (t *ownedYahooEarningsTicker) Close() {
	t.yahooTicker.Close()
	t.client.Close()
}

func normalizeYahooEarningsDates(input []time.Time, location *time.Location) []string {
	unique := make(map[string]struct{}, len(input))
	for _, date := range input {
		if !date.IsZero() {
			unique[date.In(location).Format("2006-01-02")] = struct{}{}
		}
	}
	dates := make([]string, 0, len(unique))
	for date := range unique {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	return dates
}
