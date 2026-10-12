package provider

import (
	"context"
	"errors"
	"net/url"
	"time"

	yfinanceclient "github.com/wnjoon/go-yfinance/pkg/client"
	"github.com/wnjoon/go-yfinance/pkg/models"
	"github.com/wnjoon/go-yfinance/pkg/ticker"
)

type yahooQuoteModel = models.Quote
type yahooBarModel = models.Bar

type yahooTicker interface {
	Quote() (*models.Quote, error)
	History(models.HistoryParams) ([]models.Bar, error)
	Options() ([]time.Time, error)
	OptionChain(string) (*models.OptionChain, error)
	Info() (*models.Info, error)
	EPSTrend() ([]models.EPSTrend, error)
	RevenueEstimate() ([]models.RevenueEstimate, error)
	GetValuationMeasures(string, *int) (*models.ValuationMeasures, error)
	IncomeStatement(string) (*models.FinancialStatement, error)
	BalanceSheet(string) (*models.FinancialStatement, error)
	CashFlow(string) (*models.FinancialStatement, error)
	Calendar() (*models.Calendar, error)
	Close()
}

type yahooTickerFactory func(string) (yahooTicker, error)

func newYahooTickerFactory(client *yfinanceclient.Client) yahooTickerFactory {
	return func(symbol string) (yahooTicker, error) {
		return ticker.New(symbol, ticker.WithClient(client))
	}
}

func yahooChartFromSDK(quote *models.Quote, bars []models.Bar) yahooChartResult {
	result := yahooChartResult{}
	if quote != nil {
		result.Meta = yahooChartMeta{
			Symbol: quote.Symbol, Currency: quote.Currency, RegularMarketPrice: quote.RegularMarketPrice,
			RegularMarketOpen: quote.RegularMarketOpen, PreviousClose: quote.RegularMarketPreviousClose,
			MarketState: quote.MarketState, RegularMarketTime: quote.RegularMarketTime.Unix(),
		}
	}
	result.Timestamp = make([]*int64, 0, len(bars))
	values := struct {
		Open   []*float64 `json:"open"`
		High   []*float64 `json:"high"`
		Low    []*float64 `json:"low"`
		Close  []*float64 `json:"close"`
		Volume []*int64   `json:"volume"`
	}{
		Open: make([]*float64, 0, len(bars)), High: make([]*float64, 0, len(bars)),
		Low: make([]*float64, 0, len(bars)), Close: make([]*float64, 0, len(bars)),
		Volume: make([]*int64, 0, len(bars)),
	}
	for _, bar := range bars {
		stamp := bar.Date.Unix()
		result.Timestamp = append(result.Timestamp, &stamp)
		values.Open = append(values.Open, sdkOptionalFloat(bar.Open))
		values.High = append(values.High, sdkOptionalFloat(bar.High))
		values.Low = append(values.Low, sdkOptionalFloat(bar.Low))
		values.Close = append(values.Close, sdkOptionalFloat(bar.Close))
		volume := bar.Volume
		values.Volume = append(values.Volume, &volume)
	}
	result.Indicators.Quote = append(result.Indicators.Quote, values)
	return result
}

func sdkOptionalFloat(value float64) *float64 {
	if !validYahooPrice(value) {
		return nil
	}
	return &value
}

func yahooHistoryParams(interval string, prePost bool) models.HistoryParams {
	return models.HistoryParams{Period: "5d", Interval: interval, PrePost: prePost, AutoAdjust: false, Actions: false, KeepNA: false}
}

func runYahooSDK[T any](ctx context.Context, gate chan struct{}, gateWait time.Duration, factory yahooTickerFactory, symbol string, operation func(yahooTicker) (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if factory == nil {
		return zero, errors.New("go-yfinance ticker factory is unavailable")
	}
	release, err := acquireGateTimeout(ctx, gate, gateWait)
	if err != nil {
		return zero, err
	}
	results := make(chan result, 1)
	go func() {
		released := false
		defer func() {
			if !released {
				release()
			}
		}()
		t, createErr := factory(symbol)
		if createErr != nil {
			release()
			released = true
			results <- result{err: createErr}
			return
		}
		var value T
		var operationErr error
		func() {
			defer t.Close()
			value, operationErr = operation(t)
		}()
		release()
		released = true
		results <- result{value: value, err: operationErr}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case fetched := <-results:
		return fetched.value, fetched.err
	}
}

func runYahooCrumbRequest(ctx context.Context, gate chan struct{}, gateWait time.Duration, auth yahooCrumbGetter, endpoint string, query url.Values) (*yfinanceclient.Response, error) {
	if auth == nil {
		return nil, errors.New("go-yfinance Yahoo authentication client is unavailable")
	}
	release, err := acquireGateTimeout(ctx, gate, gateWait)
	if err != nil {
		return nil, err
	}
	type result struct {
		response *yfinanceclient.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		// Keep the upstream slot until the SDK's non-context request completes.
		defer release()
		response, requestErr := auth.GetWithCrumb(endpoint, query)
		results <- result{response: response, err: requestErr}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case fetched := <-results:
		return fetched.response, fetched.err
	}
}
