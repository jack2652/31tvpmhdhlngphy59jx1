package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	yahooauth "github.com/wnjoon/go-yfinance/pkg/client"
	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestFundamentalsFetchUsesGoYFinanceSDK(t *testing.T) {
	date := func(value string) time.Time {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	statement := func(values ...ymodels.FinancialItem) *ymodels.FinancialStatement {
		return &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
			"TotalRevenue": values,
		}}
	}

	financials := &sdkFundamentalsTickerFixture{
		quote: &ymodels.Quote{QuoteType: "ETF", RegularMarketPrice: 123.5, MarketCap: 1000000, SharesOutstanding: 8000},
		info: &ymodels.Info{
			Symbol: "TEST", CurrentPrice: 123.4, TrailingEps: 5.5, ForwardEps: 6.25,
			TotalRevenue: 990, TotalDebt: 200, TotalCash: 50, SharesOutstanding: 8000,
			Industry: "Technology", Sector: "Technology", Beta: 1.2,
			PriceToSalesTrailing12Mo: 3.5, EnterpriseToEbitda: 12.5,
		},
		valuation: &ymodels.ValuationMeasures{Rows: []ymodels.ValuationMeasureRow{
			{Name: "Price/Sales", RawValues: map[string]*float64{"Current": numberPtr(3.4)}},
		}},
		epsTrend: []ymodels.EPSTrend{{Period: "0q", Current: 2.2, ThirtyDays: 2.0}},
		incomeAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
			"TotalRevenue": {
				{AsOfDate: date("2024-12-31"), Value: 800},
				{AsOfDate: date("2025-12-31"), Value: 1000},
			},
			"DilutedEPS": {
				{AsOfDate: date("2024-12-31"), Value: 4.5},
				{AsOfDate: date("2025-12-31"), Value: 0},
			},
			"NetIncome":                   {{AsOfDate: date("2025-12-31"), Value: 0}},
			"OperatingIncome":             {{AsOfDate: date("2025-12-31"), Value: 120}},
			"EBITDA":                      {{AsOfDate: date("2025-12-31"), Value: 150}},
			"DilutedAverageShares":        {{AsOfDate: date("2025-12-31"), Value: 8000}},
			"InterestExpenseNonOperating": {{AsOfDate: date("2025-12-31"), Value: 7}},
		}},
		incomeQuarterly: statement(
			ymodels.FinancialItem{AsOfDate: date("2025-09-30"), Value: 250},
			ymodels.FinancialItem{AsOfDate: date("2025-12-31"), Value: 300},
		),
		balanceAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
			"TotalDebt":              {{AsOfDate: date("2025-12-31"), Value: 220}},
			"CashAndCashEquivalents": {{AsOfDate: date("2025-12-31"), Value: 60}},
			"StockholdersEquity":     {{AsOfDate: date("2025-12-31"), Value: 400}},
		}},
		cashFlowAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
			"FreeCashFlow":                      {{AsOfDate: date("2025-12-31"), Value: 0}},
			"OperatingCashFlow":                 {{AsOfDate: date("2025-12-31"), Value: 90}},
			"CapitalExpenditure":                {{AsOfDate: date("2025-12-31"), Value: -20}},
			"DepreciationAmortizationDepletion": {{AsOfDate: date("2025-12-31"), Value: 10}},
			"ChangeInWorkingCapital":            {{AsOfDate: date("2025-12-31"), Value: -5}},
			"RepurchaseOfCapitalStock":          {{AsOfDate: date("2025-12-31"), Value: -3}},
		}},
		history: []ymodels.Bar{
			{Date: date("2024-12-01"), Close: 80, AdjClose: 78},
			{Date: date("2025-11-01"), Close: 100, AdjClose: 98},
			{Date: date("2025-12-01"), Close: 110, AdjClose: 108},
		},
	}

	customSummaryRequests := make(chan struct{}, 1)
	customServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customSummaryRequests <- struct{}{}
		if r.URL.Query().Get("modules") != "assetProfile,defaultKeyStatistics,financialData,balanceSheetHistory" {
			t.Errorf("custom modules = %q", r.URL.Query().Get("modules"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{
			"assetProfile":{"industryKey":"semiconductor-memory"},
			"financialData":{"riskFreeRate":{"raw":0.04},"ffoPerShare":{"raw":8.5},"interestExpense":{"raw":999}},
			"balanceSheetHistory":{"balanceSheetStatements":[
				{"endDate":{"fmt":"12/31/2024"},"ordinarySharesNumber":{"raw":6000},"shareIssued":{"raw":5900}},
				{"endDate":{"fmt":"12/31/2025"},"ordinarySharesNumber":{"raw":7000},"shareIssued":{"raw":6900}}
			]}
		}],"error":null}}`))
	}))
	defer customServer.Close()
	client := NewFundamentalsClientWithEndpoints(customServer.Client(), customServer.URL+"/summary/%s", "")
	client.tickerFactory = func(symbol string) (yahooTicker, error) {
		if symbol != "TEST" {
			t.Fatalf("ticker factory symbol = %q, want TEST", symbol)
		}
		return financials, nil
	}
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !financials.closed {
		t.Fatal("SDK ticker was not closed")
	}
	if got.Price == nil || *got.Price != 123.5 || got.EPS == nil || *got.EPS != 5.5 {
		t.Fatalf("SDK quote fields = price %v, EPS %v", got.Price, got.EPS)
	}
	if got.QuoteType != "ETF" {
		t.Fatalf("SDK quote type = %q, want ETF", got.QuoteType)
	}
	if !financials.epsTrendCalled || got.EPSRevision == nil || math.Abs(*got.EPSRevision-0.1) > 1e-12 {
		t.Fatalf("SDK EPS trend = called %v, revision %v", financials.epsTrendCalled, got.EPSRevision)
	}
	if !reflect.DeepEqual(got.AnnualRevenue, []float64{1000, 800}) || !reflect.DeepEqual(got.QuarterlyRevenue, []float64{300, 250}) {
		t.Fatalf("statement revenue ordering = annual %v, quarterly %v", got.AnnualRevenue, got.QuarterlyRevenue)
	}
	if got.Debt == nil || *got.Debt != 220 || got.FreeCashflow == nil || *got.FreeCashflow != 0 {
		t.Fatalf("statement current values = debt %v, FCF %v", got.Debt, got.FreeCashflow)
	}
	if got.InterestExpense == nil || *got.InterestExpense != 7 {
		t.Fatalf("interest expense = %v, want SDK statement value 7", got.InterestExpense)
	}
	if got.Shares == nil || *got.Shares != 7000 || got.SharesSource != "balance_sheet" || !reflect.DeepEqual(got.AnnualShares, []float64{7000, 6000}) {
		t.Fatalf("shares = %v (%q), annual shares %v; want balance-sheet 7000 and newest-first [7000 6000]", got.Shares, got.SharesSource, got.AnnualShares)
	}
	if got.IndustryKey != "semiconductor-memory" || got.RiskFreeRate == nil || *got.RiskFreeRate != 0.04 || got.FFOPerShare == nil || *got.FFOPerShare != 8.5 {
		t.Fatalf("SDK-missing extensions = industry key %q, rate %v, FFO %v", got.IndustryKey, got.RiskFreeRate, got.FFOPerShare)
	}
	select {
	case <-customSummaryRequests:
	default:
		t.Fatal("SDK-missing quoteSummary extensions were not fetched")
	}
	if got.NetIncome == nil || *got.NetIncome != 0 {
		t.Fatalf("explicit zero net income = %v, want non-nil zero", got.NetIncome)
	}
	if !reflect.DeepEqual(got.HistoricalPrices, []float64{108, 78}) {
		t.Fatalf("yearly adjusted history = %v, want [108 78]", got.HistoricalPrices)
	}
}

func TestFundamentalsSDKUsesConfiguredConcurrencyWithoutExceedingGate(t *testing.T) {
	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, "", "")
	client.QuoteSummaryURL = ""
	client.Gate = make(chan struct{}, 2)
	client.GateWait = time.Second
	tracker := &sdkFundamentalsConcurrencyTracker{}
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &trackedFundamentalsTicker{
			sdkFundamentalsTickerFixture: &sdkFundamentalsTickerFixture{},
			tracker:                      tracker,
		}, nil
	}
	if _, err := client.Fetch(context.Background(), "TEST"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got := tracker.maximum.Load(); got < 2 {
		t.Fatalf("maximum concurrent SDK calls = %d, want configured concurrency 2", got)
	}
	if got := tracker.maximum.Load(); got > int32(cap(client.Gate)) {
		t.Fatalf("maximum concurrent SDK calls = %d, gate capacity = %d", got, cap(client.Gate))
	}
}

type sdkFundamentalsConcurrencyTracker struct {
	active  atomic.Int32
	maximum atomic.Int32
}

func (tracker *sdkFundamentalsConcurrencyTracker) call() {
	active := tracker.active.Add(1)
	for maximum := tracker.maximum.Load(); active > maximum && !tracker.maximum.CompareAndSwap(maximum, active); maximum = tracker.maximum.Load() {
	}
	time.Sleep(30 * time.Millisecond)
	tracker.active.Add(-1)
}

type trackedFundamentalsTicker struct {
	*sdkFundamentalsTickerFixture
	tracker *sdkFundamentalsConcurrencyTracker
}

func (ticker *trackedFundamentalsTicker) Quote() (*ymodels.Quote, error) {
	ticker.tracker.call()
	return ticker.sdkFundamentalsTickerFixture.Quote()
}

func (ticker *trackedFundamentalsTicker) Info() (*ymodels.Info, error) {
	ticker.tracker.call()
	return ticker.sdkFundamentalsTickerFixture.Info()
}

func TestFundamentalsCustomExtensionRequestsOnlyUnmodeledFields(t *testing.T) {
	var requestedModules string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedModules = r.URL.Query().Get("modules")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{
			"assetProfile":{"industryKey":"semiconductor-memory"},
			"financialData":{"riskFreeRate":{"raw":0.04},"ffoPerShare":{"raw":8.5},"interestExpense":{"raw":7}}
		}],"error":null}}`))
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/%s", "")
	got, err := client.fetchCustomFundamentals(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("fetchCustomFundamentals() error = %v", err)
	}
	if requestedModules != "assetProfile,defaultKeyStatistics,financialData,balanceSheetHistory" {
		t.Fatalf("requested modules = %q", requestedModules)
	}
	if got.IndustryKey != "semiconductor-memory" || got.RiskFreeRate == nil || *got.RiskFreeRate != 0.04 || got.FFOPerShare == nil || *got.FFOPerShare != 8.5 || got.InterestExpense != nil {
		t.Fatalf("custom fields = industry %q, rate %v, FFO %v, interest expense %v", got.IndustryKey, got.RiskFreeRate, got.FFOPerShare, got.InterestExpense)
	}
}

func TestFundamentalsCustomExtensionUsesGoYFinanceAuth(t *testing.T) {
	var gotEndpoint string
	var gotQuery url.Values
	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, "https://query1.finance.yahoo.com/v10/finance/quoteSummary/%s", "")
	client.Gate = make(chan struct{}, 1)
	client.GateWait = time.Second
	client.crumbAuth = yahooCrumbGetterFunc(func(endpoint string, query url.Values) (*yahooauth.Response, error) {
		gotEndpoint = endpoint
		gotQuery = query
		return &yahooauth.Response{
			StatusCode: http.StatusOK,
			Body:       `{"quoteSummary":{"result":[{"assetProfile":{"industryKey":"semiconductor-memory"},"financialData":{"forwardEbitda":{"raw":123}}}],"error":null}}`,
		}, nil
	})

	got, err := client.fetchCustomFundamentals(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("fetchCustomFundamentals() error = %v", err)
	}
	if gotEndpoint != "https://query1.finance.yahoo.com/v10/finance/quoteSummary/TEST" {
		t.Fatalf("crumb endpoint = %q", gotEndpoint)
	}
	if gotQuery.Get("modules") != "assetProfile,defaultKeyStatistics,financialData,balanceSheetHistory" || gotQuery.Has("crumb") {
		t.Fatalf("crumb query = %v", gotQuery)
	}
	if got.IndustryKey != "semiconductor-memory" || got.ForwardEBITDA == nil || *got.ForwardEBITDA != 123 {
		t.Fatalf("custom extension = industry %q, forward EBITDA %v", got.IndustryKey, got.ForwardEBITDA)
	}
	select {
	case client.Gate <- struct{}{}:
		<-client.Gate
	default:
		t.Fatal("upstream concurrency slot was not released")
	}
}

func TestFundamentalsSDKDoesNotRepeatStatementTimeseriesRequest(t *testing.T) {
	var timeseriesRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timeseriesRequests.Add(1)
		t.Errorf("unexpected self-maintained timeseries request: %s?%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), "", server.URL+"/timeseries/%s")
	client.QuoteSummaryURL = ""
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &sdkFundamentalsTickerFixture{
			info: &ymodels.Info{TrailingEps: 2},
			incomeTrailing: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"DilutedEPS": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 2.5}},
			}},
			balanceAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"OrdinarySharesNumber": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 100}},
			}},
			cashFlowAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"FreeCashFlow": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 42}},
			}},
		}, nil
	}
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.EPS == nil || *got.EPS != 2.5 || got.FreeCashflow == nil || *got.FreeCashflow != 42 {
		t.Fatalf("SDK statement values = EPS %v, FCF %v", got.EPS, got.FreeCashflow)
	}
	if timeseriesRequests.Load() != 0 {
		t.Fatalf("self-maintained timeseries requests = %d, want 0", timeseriesRequests.Load())
	}
}

func TestFundamentalsSDKUsesTrailingIncomeStatementWithoutRawFallback(t *testing.T) {
	var rawTimeseriesRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawTimeseriesRequests.Add(1)
		t.Errorf("unexpected self-maintained timeseries request: %s?%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), "", server.URL+"/timeseries/%s")
	client.QuoteSummaryURL = ""
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &sdkFundamentalsTickerFixture{
			info: &ymodels.Info{TrailingEps: 2},
			incomeTrailing: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"DilutedEPS": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 3}},
			}},
			balanceAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"OrdinarySharesNumber": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 100}},
			}},
		}, nil
	}
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if rawTimeseriesRequests.Load() != 0 {
		t.Fatalf("self-maintained timeseries requests = %d, want 0", rawTimeseriesRequests.Load())
	}
	if got.EPS == nil || *got.EPS != 3 {
		t.Fatalf("SDK trailing statement EPS = %v, want 3", got.EPS)
	}
}

func TestFundamentalsSDKUsesAnnualDilutedSharesBeforeInfoFallback(t *testing.T) {
	var rawTimeseriesRequests atomic.Int64
	timeseriesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawTimeseriesRequests.Add(1)
		t.Errorf("unexpected self-maintained timeseries request: %s?%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer timeseriesServer.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, "", timeseriesServer.URL+"/timeseries/%s")
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &sdkFundamentalsTickerFixture{
			info: &ymodels.Info{TrailingEps: 2, SharesOutstanding: 8000},
			incomeAnnual: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"DilutedAverageShares": {
					{AsOfDate: time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC), Value: 6000},
					{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 7000},
				},
			}},
			incomeTrailing: &ymodels.FinancialStatement{Data: map[string][]ymodels.FinancialItem{
				"DilutedEPS": {{AsOfDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 2}},
			}},
		}, nil
	}
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if rawTimeseriesRequests.Load() != 0 {
		t.Fatalf("self-maintained timeseries requests = %d, want 0", rawTimeseriesRequests.Load())
	}
	if got.Shares == nil || *got.Shares != 7000 || got.SharesSource != "fundamentals_timeseries" || !reflect.DeepEqual(got.AnnualShares, []float64{7000, 6000}) {
		t.Fatalf("shares = %v (%q), annual shares %v; want SDK income-statement values 7000 and newest-first [7000 6000]", got.Shares, got.SharesSource, got.AnnualShares)
	}
}

func TestFundamentalsSDKUsesInfoSharesOnlyAfterAnnualSources(t *testing.T) {
	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, "", "")
	client.QuoteSummaryURL = ""
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &sdkFundamentalsTickerFixture{
			info:  &ymodels.Info{SharesOutstanding: 8000},
			quote: &ymodels.Quote{SharesOutstanding: 9000},
		}, nil
	}
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.Shares == nil || *got.Shares != 8000 || got.SharesSource != "info" || !reflect.DeepEqual(got.AnnualShares, []float64{8000}) {
		t.Fatalf("shares = %v (%q), annual shares %v; want Info fallback 8000", got.Shares, got.SharesSource, got.AnnualShares)
	}
}

func TestBalanceSheetShareSeriesFallsBackToShareIssued(t *testing.T) {
	got := balanceSheetShareSeries([]YahooBalanceSheetStatementDTO{
		{EndDate: YahooStatementDateDTO{Fmt: "12/31/2024"}, ShareIssued: YahooMetricDTO{Raw: []byte("210")}},
		{EndDate: YahooStatementDateDTO{Fmt: "12/31/2025"}, ShareIssued: YahooMetricDTO{Raw: []byte("220")}},
	})
	if !reflect.DeepEqual(got, []float64{220, 210}) {
		t.Fatalf("shares = %v, want Share Issued newest-first [220 210]", got)
	}
}

type sdkFundamentalsTickerFixture struct {
	quote            *ymodels.Quote
	info             *ymodels.Info
	valuation        *ymodels.ValuationMeasures
	epsTrend         []ymodels.EPSTrend
	epsTrendCalled   bool
	revenueEstimates []ymodels.RevenueEstimate
	incomeAnnual     *ymodels.FinancialStatement
	incomeQuarterly  *ymodels.FinancialStatement
	incomeTrailing   *ymodels.FinancialStatement
	balanceAnnual    *ymodels.FinancialStatement
	cashFlowAnnual   *ymodels.FinancialStatement
	history          []ymodels.Bar
	closed           bool
}

func (f *sdkFundamentalsTickerFixture) Quote() (*ymodels.Quote, error) { return f.quote, nil }
func (f *sdkFundamentalsTickerFixture) History(params ymodels.HistoryParams) ([]ymodels.Bar, error) {
	if params.Interval != "1mo" || params.AutoAdjust {
		return nil, fmt.Errorf("unexpected history params: %+v", params)
	}
	return f.history, nil
}
func (f *sdkFundamentalsTickerFixture) Options() ([]time.Time, error) { return nil, nil }
func (f *sdkFundamentalsTickerFixture) OptionChain(string) (*ymodels.OptionChain, error) {
	return nil, nil
}
func (f *sdkFundamentalsTickerFixture) Info() (*ymodels.Info, error) { return f.info, nil }
func (f *sdkFundamentalsTickerFixture) EPSTrend() ([]ymodels.EPSTrend, error) {
	f.epsTrendCalled = true
	return f.epsTrend, nil
}
func (f *sdkFundamentalsTickerFixture) RevenueEstimate() ([]ymodels.RevenueEstimate, error) {
	return f.revenueEstimates, nil
}
func (f *sdkFundamentalsTickerFixture) GetValuationMeasures(string, *int) (*ymodels.ValuationMeasures, error) {
	return f.valuation, nil
}
func (f *sdkFundamentalsTickerFixture) IncomeStatement(freq string) (*ymodels.FinancialStatement, error) {
	if freq == "quarterly" {
		return f.incomeQuarterly, nil
	}
	if freq == "trailing" {
		return f.incomeTrailing, nil
	}
	return f.incomeAnnual, nil
}
func (f *sdkFundamentalsTickerFixture) BalanceSheet(string) (*ymodels.FinancialStatement, error) {
	return f.balanceAnnual, nil
}
func (f *sdkFundamentalsTickerFixture) CashFlow(string) (*ymodels.FinancialStatement, error) {
	return f.cashFlowAnnual, nil
}
func (f *sdkFundamentalsTickerFixture) Calendar() (*ymodels.Calendar, error) { return nil, nil }
func (f *sdkFundamentalsTickerFixture) Close()                               { f.closed = true }

func numberPtr(value float64) *float64 { return &value }

func TestFundamentalsFetchParsesQuoteSummaryFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/summary/TEST" {
			t.Fatalf("path = %q, want /summary/TEST", r.URL.Path)
		}
		if r.URL.Query().Get("modules") == "" {
			t.Fatal("quoteSummary modules query is missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"quoteSummary":{"result":[{"quoteType":{"quoteType":"ETF"},"price":{"regularMarketPrice":{"raw":123.45}},"defaultKeyStatistics":{"trailingEps":{"raw":6.25},"forwardEps":{"raw":7.5},"freeCashflow":{"raw":1000000},"sharesOutstanding":{"raw":100000}},"financialData":{"currentPrice":{"raw":123.4},"forwardEps":{"raw":7.4},"freeCashflow":{"raw":900000},"totalDebt":{"raw":4000000},"totalCash":{"raw":5000000},"totalRevenue":{"raw":12000000},"riskFreeRate":{"raw":0.047},"earningsGrowth":{"raw":0.25},"earningsQuarterlyGrowth":{"raw":0.20},"revenueGrowth":{"raw":0.15},"daysInventoryOutstanding":{"raw":130},"defenseRevenueGrowth":{"raw":0.12}},"earningsTrend":{"trend":[{"period":"0q","epsTrend":{"current":{"raw":4.4},"30daysAgo":{"raw":4.0}}}]},"summaryDetail":{"beta":{"raw":1.2},"sharesOutstanding":{"raw":99999}},"summaryProfile":{"industry":"Technology","sector":"Technology"},"assetProfile":{"industryKey":"semiconductor-memory","longBusinessSummary":"memory supplier"}}],"error":null}
		}`))
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "")
	got, err := client.Fetch(context.Background(), " test ")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.Symbol != "TEST" || got.Price == nil || *got.Price != 123.45 {
		t.Fatalf("price/symbol = %#v", got)
	}
	if got.QuoteType != "ETF" {
		t.Fatalf("quote type = %q, want ETF", got.QuoteType)
	}
	if got.SharesSource != "info" {
		t.Errorf("shares source = %q, want Python-compatible info", got.SharesSource)
	}
	if !reflect.DeepEqual(got.AnnualShares, []float64{100000}) {
		t.Errorf("annual shares = %v, want single-value Info fallback [100000]", got.AnnualShares)
	}
	checks := []struct {
		name string
		got  *float64
		want float64
	}{
		{"eps", got.EPS, 6.25}, {"forward eps", got.ForwardEPS, 7.5},
		{"free cash flow", got.FreeCashflow, 1000000}, {"debt", got.Debt, 4000000},
		{"cash", got.Cash, 5000000}, {"shares", got.Shares, 100000},
		{"revenue", got.Revenue, 12000000}, {"beta", got.Beta, 1.2},
		{"risk-free rate", got.RiskFreeRate, .047},
		{"earnings growth", got.EarningsGrowth, .25}, {"quarterly earnings growth", got.EarningsQuarterlyGrowth, .20}, {"revenue growth", got.RevenueGrowth, .15},
		{"defense revenue growth", got.DefenseRevenueGrowth, .12},
		{"inventory days", got.InventoryDays, 130},
	}
	for _, check := range checks {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got.EPSRevision == nil || math.Abs(*got.EPSRevision-.1) > 1e-12 {
		t.Fatalf("EPS revision=%v, want 0.1", got.EPSRevision)
	}
	if got.Industry != "Technology" || got.Sector != "Technology" || got.IndustryKey != "semiconductor-memory" {
		t.Errorf("industry/sector/key=%q/%q/%q, want Technology/Technology/semiconductor-memory", got.Industry, got.Sector, got.IndustryKey)
	}
}

func TestFundamentalsQuoteSummaryRiskFreeRateAliasPriority(t *testing.T) {
	metric := func(value string) YahooMetricDTO { return YahooMetricDTO{Raw: []byte(value)} }
	tests := []struct {
		name  string
		value YahooQuoteSummaryResultDTO
		want  float64
	}{
		{
			name: "riskFreeRate before alternate keys",
			value: YahooQuoteSummaryResultDTO{
				RiskFreeRate: metric("0.052"), TreasuryYield: metric("0.047"), US10Y: metric("0.041"),
			},
			want: .052,
		},
		{
			name:  "treasuryYield fallback",
			value: YahooQuoteSummaryResultDTO{TreasuryYield: metric("0.047"), US10Y: metric("0.041")},
			want:  .047,
		},
		{
			name:  "us10y final alias",
			value: YahooQuoteSummaryResultDTO{US10Y: metric("0.041")},
			want:  .041,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fundamentalsFromQuoteSummary("TEST", test.value).RiskFreeRate
			if got == nil || *got != test.want {
				t.Fatalf("risk-free rate=%v, want %v", got, test.want)
			}
		})
	}
}

func TestFundamentalsFetchReadsRiskRateAliasesFromQuoteSummaryModules(t *testing.T) {
	tests := []struct {
		name   string
		module string
		field  string
		value  string
	}{
		{name: "riskFreeRate", module: "financialData", field: "riskFreeRate", value: "0.043"},
		{name: "treasuryYield", module: "summaryDetail", field: "treasuryYield", value: "0.041"},
		{name: "us10y", module: "defaultKeyStatistics", field: "us10y", value: "0.039"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modules := r.URL.Query().Get("modules")
				if !strings.Contains(modules, test.module) {
					t.Errorf("quoteSummary modules %q omit %q", modules, test.module)
				}
				_, _ = fmt.Fprintf(w, `{"quoteSummary":{"result":[{"%s":{"%s":{"raw":%s}}}],"error":null}}`, test.module, test.field, test.value)
			}))
			defer server.Close()

			client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/%s", "")
			got, err := client.Fetch(context.Background(), "TEST")
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			want, _ := strconv.ParseFloat(test.value, 64)
			if got.RiskFreeRate == nil || *got.RiskFreeRate != want {
				t.Fatalf("RiskFreeRate = %v, want %v from %s.%s", got.RiskFreeRate, want, test.module, test.field)
			}
		})
	}

	t.Run("missing upstream rate stays unavailable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{}],"error":null}}`))
		}))
		defer server.Close()
		client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/%s", "")
		got, err := client.Fetch(context.Background(), "TEST")
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if got.RiskFreeRate != nil {
			t.Fatalf("RiskFreeRate = %v, want nil when Yahoo provides no alias", *got.RiskFreeRate)
		}
	})
}

func TestFundamentalsQuoteSummaryPriceUsesPythonFallbackPriority(t *testing.T) {
	metric := func(value string) YahooMetricDTO { return YahooMetricDTO{Raw: []byte(value)} }
	tests := []struct {
		name     string
		regular  string
		current  string
		previous string
		want     *float64
	}{
		{name: "regular market price wins", regular: "100", current: "99", previous: "98", want: floatPointer(100)},
		{name: "current price precedes previous close", current: "99", previous: "98", want: floatPointer(99)},
		{name: "previous close is final fallback", previous: "98", want: floatPointer(98)},
		{name: "all price fields missing", want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value YahooQuoteSummaryResultDTO
			if test.regular != "" {
				value.Price.RegularMarketPrice = metric(test.regular)
			}
			if test.current != "" {
				value.FinancialData.CurrentPrice = metric(test.current)
			}
			if test.previous != "" {
				value.Price.PreviousClose = metric(test.previous)
			}

			got := fundamentalsFromQuoteSummary("TEST", value).Price
			if test.want == nil {
				if got != nil {
					t.Fatalf("price = %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *test.want {
				t.Fatalf("price = %v, want %v", got, *test.want)
			}
		})
	}
}

func TestFundamentalsQuoteSummaryMapsPythonInfoAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"defaultKeyStatistics":{"marketCap":{"raw":1500000}},"financialData":{"forwardEPS":{"raw":8},"epsForward":{"raw":8.5},"trailingAnnualRevenue":{"raw":1200000}},"summaryDetail":{"beta3Year":{"raw":1.4}},"assetProfile":{"industry_key":"semiconductor-memory"}}],"error":null}}`))
	}))
	defer server.Close()

	got, err := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "").Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]struct {
		got  *float64
		want float64
	}{
		"forward EPS capitalization alias": {got.ForwardEPS, 8}, "trailing annual revenue": {got.Revenue, 1200000},
		"default-statistics market cap": {got.MarketCap, 1500000}, "three-year beta": {got.Beta, 1.4},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want %v", name, check.got, check.want)
		}
	}
	if got.IndustryKey != "semiconductor-memory" {
		t.Errorf("industry key = %q, want snake-case alias", got.IndustryKey)
	}
}

func TestFundamentalsQuoteSummaryMapsPythonCapexGrowthAliasesInPriorityOrder(t *testing.T) {
	tests := []struct {
		name string
		data string
		want float64
	}{
		{
			name: "aiCapexGrowth precedes aliases",
			data: `"aiCapexGrowth":{"raw":-0.21},"capexGrowth":{"raw":0.31},"capitalExpenditureGrowth":{"raw":0.41}`,
			want: -0.21,
		},
		{
			name: "capexGrowth fallback",
			data: `"aiCapexGrowth":null,"capexGrowth":{"raw":0.32},"capitalExpenditureGrowth":{"raw":0.42}`,
			want: 0.32,
		},
		{
			name: "capitalExpenditureGrowth fallback",
			data: `"aiCapexGrowth":{"raw":"NaN"},"capexGrowth":{"raw":null},"capitalExpenditureGrowth":{"raw":0.43}`,
			want: 0.43,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Query().Get("modules"), "financialData") {
					t.Errorf("modules = %q, want financialData", r.URL.Query().Get("modules"))
				}
				_, _ = fmt.Fprintf(w, `{"quoteSummary":{"result":[{"financialData":{%s}}],"error":null}}`, test.data)
			}))
			defer server.Close()

			got, err := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/summary/%s", "").Fetch(context.Background(), "TEST")
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if got.CapexGrowth == nil || *got.CapexGrowth != test.want {
				t.Fatalf("CapexGrowth = %v, want Python alias value %v", got.CapexGrowth, test.want)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestFundamentalsFetchMapsIndustryValuationMetricsAndNulls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"price":{"regularMarketPrice":{"raw":100},"marketCap":{"raw":"123456789"}},"defaultKeyStatistics":{"bookValue":{"raw":12.5},"priceToSalesTrailing12Months":{"raw":2.5},"enterpriseToEbitda":{"raw":9.5},"ffoPerShare":{"raw":4.25},"navPerShare":{"raw":20},"pipelineRnPV":{"raw":0}},"financialData":{"dividendRate":{"raw":1.2},"targetMeanPrice":{"raw":125},"targetLowPrice":{"raw":110},"targetHighPrice":{"raw":140},"forwardEbitda":{"raw":2000000},"interestExpense":{"raw":-300000},"returnOnEquity":{"raw":0.18},"monthlyActiveUsers":{"raw":5000000},"arpu":{"raw":12.5},"churnRate":{"raw":0.03},"aiRevenueShare":{"raw":0.4},"numberOfAnalystOpinions":{"raw":18}},"summaryDetail":{"marketCap":null,"bookValue":null},"summaryProfile":{"industry":"REIT - Retail","sector":"Real Estate"},"assetProfile":{"industryKey":"reit-retail","longBusinessSummary":"property owner"}}],"error":null}}`))
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "")
	got, err := client.Fetch(context.Background(), "REIT")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	checks := []struct {
		name string
		got  *float64
		want float64
	}{
		{"market cap string wrapper", got.MarketCap, 123456789},
		{"dividend rate", got.DividendRate, 1.2},
		{"book value", got.BookValue, 12.5},
		{"target mean", got.TargetMeanPrice, 125},
		{"target low", got.TargetLowPrice, 110},
		{"target high", got.TargetHighPrice, 140},
		{"price to sales", got.PriceToSales, 2.5},
		{"enterprise to EBITDA", got.EnterpriseToEBITDA, 9.5},
		{"forward EBITDA", got.ForwardEBITDA, 2000000},
		{"interest expense", got.InterestExpense, -300000},
		{"ROE", got.ROE, .18},
		{"FFO per share", got.FFOPerShare, 4.25},
		{"NAV per share", got.NAVPerShare, 20},
		{"platform users", got.PlatformUsers, 5000000},
		{"ARPU", got.ARPU, 12.5},
		{"churn", got.ChurnRate, .03},
		{"AI revenue share", got.AIRevenueShare, .4},
		{"analyst count", got.AnalystCount, 18},
	}
	for _, check := range checks {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got.PipelineRNPV == nil || *got.PipelineRNPV != 0 {
		t.Fatalf("explicit zero pipeline rNPV should remain nonnil: %v", got.PipelineRNPV)
	}
	if got.Sector != "Real Estate" || got.IndustryKey != "reit-retail" || got.LongBusinessSummary != "property owner" {
		t.Fatalf("company metadata = %#v", got)
	}

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"price":{"regularMarketPrice":{"raw":100},"marketCap":null},"defaultKeyStatistics":{"bookValue":null},"financialData":{"dividendRate":null,"targetMeanPrice":null,"interestExpense":{"raw":"NaN"},"returnOnEquity":null}}],"error":null}}`))
	}))
	defer server2.Close()
	missing, err := NewFundamentalsClientWithEndpoints(http.DefaultClient, server2.URL+"/summary/%s", "").Fetch(context.Background(), "MISSING")
	if err != nil {
		t.Fatalf("missing metric Fetch() error = %v", err)
	}
	if missing.SharesSource != "" {
		t.Errorf("shares source = %q, want empty when shares are missing", missing.SharesSource)
	}
	for name, value := range map[string]*float64{"market cap": missing.MarketCap, "book value": missing.BookValue, "dividend": missing.DividendRate, "target": missing.TargetMeanPrice, "interest": missing.InterestExpense, "roe": missing.ROE} {
		if value != nil {
			t.Errorf("%s = %v, want nil for omitted/null/non-finite metric", name, value)
		}
	}
}

func TestFundamentalsFetchFillsMissingQuoteSummaryValuesFromTimeseries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"price":{"regularMarketPrice":{"raw":50}},"defaultKeyStatistics":{"trailingEps":{"raw":1},"freeCashflow":{"raw":11}},"financialData":{"forwardEps":{"raw":4}},"summaryProfile":{"industry":"Finance"}}],"error":null}}`))
		case "/timeseries/TEST":
			query := r.URL.Query()
			if query.Get("period1") != "1262304000" {
				t.Errorf("period1 = %q, want Python-compatible 1262304000", query.Get("period1"))
			}
			period2, parseErr := strconv.ParseInt(query.Get("period2"), 10, 64)
			if parseErr != nil || period2 < time.Now().Unix()+86390 || period2 > time.Now().Unix()+86410 {
				t.Errorf("period2 = %q, want current Unix time plus one day", query.Get("period2"))
			}
			_, _ = w.Write([]byte(`{"timeseries":{"result":[
				{"annualFreeCashFlow":[{"asOfDate":"2023-12-31","reportedValue":{"raw":10}},{"asOfDate":"2024-12-31","reportedValue":{"raw":20}}]},
				{"annualTotalDebt":[{"asOfDate":"2024-12-31","reportedValue":{"raw":30}}],"annualCashCashEquivalentsAndShortTermInvestments":[{"asOfDate":"2024-12-31","reportedValue":{"raw":40}}],"annualDilutedAverageShares":[{"asOfDate":"2024-12-31","reportedValue":{"raw":50}}],"annualTotalRevenue":[{"asOfDate":"2023-12-31","reportedValue":{"raw":50}},{"asOfDate":"2024-12-31","reportedValue":{"raw":60}}],"annualDilutedEPS":[{"asOfDate":"2023-12-31","reportedValue":{"raw":2}},{"asOfDate":"2024-12-31","reportedValue":{"raw":3}}],"trailingDilutedEPS":[{"asOfDate":"2024-12-31","reportedValue":{"raw":3.5}}],"annualNetIncome":[{"asOfDate":"2024-12-31","reportedValue":{"raw":70}}],"annualDepreciationAndAmortization":[{"asOfDate":"2024-12-31","reportedValue":{"raw":8}}],"annualCapitalExpenditure":[{"asOfDate":"2024-12-31","reportedValue":{"raw":-9}}],"annualChangeInWorkingCapital":[{"asOfDate":"2024-12-31","reportedValue":{"raw":2}}],"annualOperatingIncome":[{"asOfDate":"2024-12-31","reportedValue":{"raw":80}}],"annualRepurchaseOfCapitalStock":[{"asOfDate":"2024-12-31","reportedValue":{"raw":4}}],"annualStockholdersEquity":[{"asOfDate":"2024-12-31","reportedValue":{"raw":100}}],"quarterlyTotalRevenue":[{"asOfDate":"2024-09-30","reportedValue":{"raw":1}},{"asOfDate":"2024-12-31","reportedValue":{"raw":2}}],"quarterlyDilutedEPS":[{"asOfDate":"2024-12-31","reportedValue":{"raw":3}}]}
			],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	for _, check := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"eps (dated trailing series overrides quoteSummary)", got.EPS, 3.5}, {"free cash flow (annual series overrides quoteSummary)", got.FreeCashflow, 20},
		{"debt", got.Debt, 30}, {"cash", got.Cash, 40},
		{"shares", got.Shares, 50}, {"revenue", got.Revenue, 60},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got.ForwardEPS == nil || *got.ForwardEPS != 4 || got.Price == nil || *got.Price != 50 {
		t.Fatalf("quoteSummary values were lost: %#v", got)
	}
	for name, value := range map[string]*float64{"net_income": got.NetIncome, "da": got.DepreciationAndAmortization, "capex": got.CapitalExpenditure, "working_capital": got.ChangeInWorkingCapital, "operating_income": got.OperatingIncome, "equity": got.Equity, "buyback": got.Buyback} {
		if value == nil {
			t.Errorf("%s missing from timeseries", name)
		}
	}
	if got.NetIncome == nil || *got.NetIncome != 70 || got.CapitalExpenditure == nil || *got.CapitalExpenditure != -9 || len(got.QuarterlyRevenue) != 2 || len(got.AnnualEPS) != 2 || len(got.AnnualRevenue) != 2 {
		t.Fatalf("owner/quarterly values mismatch: %#v", got)
	}
}

func TestFundamentalsFetchPrefersAnnualStatementsOverTimeseriesAndQuoteSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			modules := r.URL.Query().Get("modules")
			if !strings.Contains(modules, "balanceSheetHistory") || !strings.Contains(modules, "cashflowStatementHistory") {
				t.Errorf("modules = %q, missing annual statement modules", modules)
			}
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{
				"price":{"regularMarketPrice":{"raw":100}},
				"defaultKeyStatistics":{"sharesOutstanding":{"raw":900}},
				"financialData":{"totalDebt":{"raw":900},"totalCash":{"raw":900}},
				"balanceSheetHistory":{"balanceSheetStatements":[
					{"endDate":{"fmt":"12/31/2024"},"totalDebt":{"raw":150},"cashAndCashEquivalents":{"raw":40},"ordinarySharesNumber":{"raw":9},"stockholdersEquity":{"raw":70}},
					{"endDate":{"fmt":"12/31/2025"},"totalDebt":{"raw":200},"cashCashEquivalentsAndShortTermInvestments":{"raw":50},"ordinarySharesNumber":{"raw":10},"commonStockEquity":{"raw":80}}
				]},
				"cashflowStatementHistory":{"cashflowStatements":[
					{"endDate":{"fmt":"12/31/2024"},"freeCashFlow":{"raw":11},"capitalExpenditures":{"raw":-10},"interestExpense":{"raw":6}},
					{"endDate":{"fmt":"12/31/2025"},"operatingCashFlow":{"raw":100},"capitalExpenditures":{"raw":-20},"netNonOperatingInterestIncomeExpense":{"raw":7}}
				]}
			}],"error":null}}`))
		case "/timeseries/TEST":
			_, _ = w.Write([]byte(`{"timeseries":{"result":[
				{"annualFreeCashFlow":[{"asOfDate":"2025-12-31","reportedValue":{"raw":999}}]},
				{"annualTotalDebt":[{"asOfDate":"2025-12-31","reportedValue":{"raw":888}}]},
				{"annualCashCashEquivalentsAndShortTermInvestments":[{"asOfDate":"2025-12-31","reportedValue":{"raw":777}}]},
				{"annualDilutedAverageShares":[{"asOfDate":"2025-12-31","reportedValue":{"raw":666}}]},
				{"annualStockholdersEquity":[{"asOfDate":"2025-12-31","reportedValue":{"raw":555}}]}
			],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s").Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]struct {
		got  *float64
		want float64
	}{
		"latest computed FCF": {got.FreeCashflow, 80}, "latest debt": {got.Debt, 200},
		"latest cash": {got.Cash, 50}, "balance sheet shares": {got.Shares, 10},
		"latest equity": {got.Equity, 80}, "latest interest": {got.InterestExpense, 7},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want %v", name, check.got, check.want)
		}
	}
	if got.SharesSource != "balance_sheet" {
		t.Errorf("shares source = %q, want balance_sheet", got.SharesSource)
	}
	if len(got.AnnualCapitalExpenditure) != 2 || got.AnnualCapitalExpenditure[0] != -20 || got.AnnualCapitalExpenditure[1] != -10 {
		t.Errorf("annual capital expenditure = %v, want newest-first [-20 -10]", got.AnnualCapitalExpenditure)
	}
}

func TestFundamentalsFetchUsesChartMetadataAsLastSpotPriceFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"defaultKeyStatistics":{"trailingEps":{"raw":2}}}],"error":null}}`))
		case "/history/TEST":
			_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"regularMarketPrice":123.5},"timestamp":[],"indicators":{"quote":[{"close":[]}]}}],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "")
	client.HistoricalURL = server.URL + "/history/%s"
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	if got.Price == nil || *got.Price != 123.5 {
		t.Fatalf("price = %v, want chart metadata fallback 123.5", got.Price)
	}
}

func TestFundamentalsHistoricalPricesUseDividendAdjustedClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"price":{"regularMarketPrice":{"raw":123.5}}}],"error":null}}`))
		case "/history/TEST":
			if r.URL.Query().Get("includeAdjustedClose") != "true" {
				t.Errorf("includeAdjustedClose = %q, want true", r.URL.Query().Get("includeAdjustedClose"))
			}
			_, _ = w.Write([]byte(`{"chart":{"result":[{"timestamp":[1704067200,1735689600],"indicators":{"quote":[{"close":[100,110]}],"adjclose":[{"adjclose":[50,55]}]}}],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "")
	client.HistoricalURL = server.URL + "/history/%s"
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.HistoricalPrices) != 2 || got.HistoricalPrices[0] != 55 || got.HistoricalPrices[1] != 50 {
		t.Fatalf("historical prices = %v, want adjusted annual closes [55 50]", got.HistoricalPrices)
	}
}

func TestFundamentalsFetchMarksTransientTimeseriesFailureAndSkipsHistory(t *testing.T) {
	for _, test := range []struct {
		name               string
		status             int
		wantRetryable      bool
		wantHistoryRequest bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantRetryable: true},
		{name: "permanent client error", status: http.StatusBadRequest, wantHistoryRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			historyRequests := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/summary/TEST":
					_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"price":{"regularMarketPrice":{"raw":50}}} ],"error":null}}`))
				case "/timeseries/TEST":
					http.Error(w, "upstream unavailable", test.status)
				case "/history/TEST":
					select {
					case historyRequests <- struct{}{}:
					default:
					}
					_, _ = w.Write([]byte(`{}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s")
			client.HistoricalURL = server.URL + "/history/%s"
			got, err := client.Fetch(context.Background(), "TEST")
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if got.TimeseriesRetryable != test.wantRetryable {
				t.Errorf("TimeseriesRetryable = %v, want %v", got.TimeseriesRetryable, test.wantRetryable)
			}
			if !got.ValuationUpstreamError {
				t.Error("failed timeseries request was not recorded for P/S fallback eligibility")
			}
			if got.ValuationUpstreamRetryable != test.wantRetryable {
				t.Errorf("ValuationUpstreamRetryable = %v, want %v", got.ValuationUpstreamRetryable, test.wantRetryable)
			}
			gotHistoryRequest := len(historyRequests) > 0
			if gotHistoryRequest != test.wantHistoryRequest {
				t.Errorf("history request = %v, want %v", gotHistoryRequest, test.wantHistoryRequest)
			}
		})
	}
}

func TestFundamentalsFetchOverlapsTimeseriesAndHistoryWithMultiSlotGate(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{}],"error":null}}`))
		case "/timeseries/TEST", "/history/TEST":
			started <- r.URL.Path
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			if r.URL.Path == "/timeseries/TEST" {
				_, _ = w.Write([]byte(`{"timeseries":{"result":[],"error":null}}`))
				return
			}
			_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"regularMarketPrice":77},"timestamp":[],"indicators":{"quote":[{"close":[]}]}}],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	}()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	client.HistoricalURL = server.URL + "/history/%s"
	client.Gate = make(chan struct{}, 2)
	result := make(chan struct {
		value YahooFundamentals
		err   error
	}, 1)
	go func() {
		value, err := client.Fetch(context.Background(), "TEST")
		result <- struct {
			value YahooFundamentals
			err   error
		}{value, err}
	}()

	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(2 * time.Second):
			t.Fatal("timeseries and history requests did not overlap")
		}
	}
	if !seen["/timeseries/TEST"] || !seen["/history/TEST"] {
		t.Fatalf("started requests = %v, want timeseries and history", seen)
	}
	releaseOnce.Do(func() { close(release) })

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Fetch() error = %v", got.err)
		}
		if got.value.Price == nil || *got.value.Price != 77 {
			t.Errorf("chart spot fallback = %v, want 77", got.value.Price)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch() did not finish after both upstream responses were released")
	}
}

func TestFundamentalsFetchSuppressesConcurrentHistoryWhenTimeseriesIsRetryable(t *testing.T) {
	historyRequested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{}],"error":null}}`))
		case "/timeseries/TEST":
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		case "/history/TEST":
			historyRequested <- struct{}{}
			_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"regularMarketPrice":77},"timestamp":[],"indicators":{"quote":[{"close":[]}]}}],"error":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	client.HistoricalURL = server.URL + "/history/%s"
	client.Gate = make(chan struct{}, 2)
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(historyRequested) == 0 {
		t.Fatal("history request did not run alongside the timeseries request")
	}
	if !got.TimeseriesRetryable {
		t.Fatal("transient timeseries failure should retain the retry signal")
	}
	if len(got.HistoricalPrices) != 0 || got.Price != nil {
		t.Fatalf("retryable result must suppress concurrent chart enrichment: %#v", got)
	}
}

func TestFundamentalsFetchTracksRetryableHistoryFailureForValuationFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"financialData":{"totalRevenue":{"raw":1000}}}],"error":null}}`))
		case "/timeseries/TEST":
			_, _ = w.Write([]byte(`{"timeseries":{"result":[],"error":null}}`))
		case "/history/TEST":
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	client.HistoricalURL = server.URL + "/history/%s"
	client.Gate = make(chan struct{}, 2)
	got, err := client.Fetch(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.TimeseriesRetryable {
		t.Fatal("history-only rate limit should not be reported as a timeseries failure")
	}
	if !got.ValuationUpstreamError || !got.ValuationUpstreamRetryable {
		t.Fatalf("history failure flags = error %v / retryable %v, want true/true", got.ValuationUpstreamError, got.ValuationUpstreamRetryable)
	}
}

func TestFundamentalsFetchFallsBackToTimeseriesWhenQuoteSummaryFails(t *testing.T) {
	t.Run("partial timeseries result remains usable and retryable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/summary/TEST":
				w.WriteHeader(http.StatusServiceUnavailable)
			case "/timeseries/TEST":
				_, _ = w.Write([]byte(`{"timeseries":{"result":[{"annualFreeCashFlow":[{"asOfDate":"2025-12-31","reportedValue":{"raw":42}}],"annualDilutedEPS":[{"asOfDate":"2025-12-31","reportedValue":{"raw":3}}]}],"error":null}}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		got, err := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s").Fetch(context.Background(), "TEST")
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if got.FreeCashflow == nil || *got.FreeCashflow != 42 || len(got.AnnualEPS) != 1 || got.AnnualEPS[0] != 3 {
			t.Fatalf("timeseries fallback fields = %#v", got)
		}
		if !got.TimeseriesRetryable {
			t.Fatal("transient quoteSummary failure should retain a retry signal")
		}
		if !got.ValuationUpstreamError || !got.ValuationUpstreamRetryable {
			t.Fatalf("quoteSummary failure flags = error %v / retryable %v, want true/true", got.ValuationUpstreamError, got.ValuationUpstreamRetryable)
		}
	})

	t.Run("both transient endpoints produce retryable empty enrichment", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()

		got, err := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", server.URL+"/timeseries/%s").Fetch(context.Background(), "TEST")
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if got.Symbol != "TEST" || !got.TimeseriesRetryable || !got.ValuationUpstreamError ||
			!got.ValuationUpstreamRetryable || got.EPS != nil || got.FreeCashflow != nil {
			t.Fatalf("retryable fallback = %#v", got)
		}
	})
}

func TestTransientFundamentalsErrorMatchesPythonRetryStatuses(t *testing.T) {
	for _, status := range []int{403, 408, 425, 429, 500, 502, 503, 504} {
		err := errors.New("Yahoo fundamentals returned HTTP " + strconv.Itoa(status))
		if !transientFundamentalsError(err) {
			t.Errorf("HTTP %d was not marked retryable", status)
		}
	}
	for _, status := range []int{400, 404} {
		err := errors.New("Yahoo fundamentals returned HTTP " + strconv.Itoa(status))
		if transientFundamentalsError(err) {
			t.Errorf("HTTP %d was unexpectedly marked retryable", status)
		}
	}
	if !transientFundamentalsError(&url.Error{Op: "Get", URL: "https://query1.finance.yahoo.com", Err: errors.New("connection refused")}) {
		t.Fatal("transport error was not marked retryable")
	}
	if transientFundamentalsError(context.Canceled) {
		t.Fatal("explicit cancellation should not be marked retryable")
	}
	if !transientFundamentalsError(context.DeadlineExceeded) {
		t.Fatal("request timeout should be marked retryable")
	}
}

func TestFundamentalsFromTimeseriesDerivesFCFFromLatestMatchedAnnualRows(t *testing.T) {
	results := []YahooTimeseriesResultDTO{{
		AnnualOperatingCF: []YahooTimeseriesEntryDTO{
			{AsOfDate: "2025-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("40")}},
			{AsOfDate: "2024-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("30")}},
		},
		AnnualCapitalExpend: []YahooTimeseriesEntryDTO{
			{AsOfDate: "2024-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("-8")}},
			{AsOfDate: "2023-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("-5")}},
		},
	}}
	got := fundamentalsFromTimeseries("TEST", results)
	if got.FreeCashflow == nil || *got.FreeCashflow != 22 {
		t.Fatalf("derived free cash flow=%v want 22 from latest matched year", got.FreeCashflow)
	}
	if got.OperatingCashflow == nil || *got.OperatingCashflow != 40 {
		t.Fatalf("operating cash flow=%v want latest value 40", got.OperatingCashflow)
	}
	if len(got.AnnualCapitalExpenditure) != 2 || got.AnnualCapitalExpenditure[0] != -8 || got.AnnualCapitalExpenditure[1] != -5 {
		t.Fatalf("annual capex series=%v, want newest-to-oldest [-8 -5]", got.AnnualCapitalExpenditure)
	}
}

func TestFundamentalsFromTimeseriesFillsDateForSingleOuterTimestamp(t *testing.T) {
	when := time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC).Unix()
	got := fundamentalsFromTimeseries("TEST", []YahooTimeseriesResultDTO{{
		Timestamp: []int64{when},
		AnnualOperatingCF: []YahooTimeseriesEntryDTO{{
			ReportedValue: YahooMetricDTO{Raw: []byte("40")},
		}},
		AnnualCapitalExpend: []YahooTimeseriesEntryDTO{{
			ReportedValue: YahooMetricDTO{Raw: []byte("-8")},
		}},
	}})
	if got.FreeCashflow == nil || *got.FreeCashflow != 32 {
		t.Fatalf("free cash flow=%v, want 32 from single-dated operating cash flow and capex", got.FreeCashflow)
	}
}

func TestFundamentalsFromTimeseriesUsesReverseIndexForEqualOuterTimestamps(t *testing.T) {
	when := time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC).Unix()
	got := fundamentalsFromTimeseries("TEST", []YahooTimeseriesResultDTO{{
		Timestamp: []int64{when, when},
		AnnualRevenue: []YahooTimeseriesEntryDTO{
			{ReportedValue: YahooMetricDTO{Raw: []byte("10")}},
			{ReportedValue: YahooMetricDTO{Raw: []byte("20")}},
		},
	}})
	if len(got.AnnualRevenue) != 2 || got.AnnualRevenue[0] != 20 || got.AnnualRevenue[1] != 10 {
		t.Fatalf("annual revenue=%v, want reverse-index order [20 10] for equal timestamps", got.AnnualRevenue)
	}
}

func TestFundamentalsFromTimeseriesKeepsAnnualNetIncomeHistory(t *testing.T) {
	got := fundamentalsFromTimeseries("TEST", []YahooTimeseriesResultDTO{{
		AnnualNetIncome: []YahooTimeseriesEntryDTO{
			{AsOfDate: "2023-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("90")}},
			{AsOfDate: "2025-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("120")}},
			{AsOfDate: "2024-12-31", ReportedValue: YahooMetricDTO{Raw: []byte("100")}},
		},
	}})
	if len(got.AnnualNetIncome) != 3 || got.AnnualNetIncome[0] != 120 || got.AnnualNetIncome[1] != 100 || got.AnnualNetIncome[2] != 90 {
		t.Fatalf("annual net income=%v, want newest-first [120 100 90]", got.AnnualNetIncome)
	}
}

func TestFundamentalsFromTimeseriesIgnoresNonpositiveTrailingEPSLikePython(t *testing.T) {
	for _, raw := range []string{"0", "-2"} {
		got := fundamentalsFromTimeseries("TEST", []YahooTimeseriesResultDTO{{
			TrailingDilutedEPS: []YahooTimeseriesEntryDTO{{
				AsOfDate: "2025-12-31", ReportedValue: YahooMetricDTO{Raw: []byte(raw)},
			}},
		}})
		if got.EPS != nil {
			t.Errorf("timeseries EPS %s = %v, want nil (Python accepts only positive TTM EPS)", raw, *got.EPS)
		}
		if got.SharesSource != "" {
			t.Errorf("shares source = %q, want empty without a shares value", got.SharesSource)
		}
	}
}

func TestMergeFundamentalsKeepsPositiveQuoteSummaryEPSWhenTimeseriesEPSIsNonpositive(t *testing.T) {
	quoteEPS, timeseriesEPS := 12.0, -2.0
	destination := YahooFundamentals{EPS: &quoteEPS}
	source := YahooFundamentals{EPS: &timeseriesEPS}
	mergeMissingFundamentals(&destination, source)
	if destination.EPS == nil || *destination.EPS != quoteEPS {
		t.Fatalf("timeseries EPS=%v must not replace positive quoteSummary EPS=%v", source.EPS, destination.EPS)
	}
	destination.EPS = nil
	mergeMissingFundamentals(&destination, source)
	if destination.EPS != nil {
		t.Fatalf("nonpositive timeseries EPS=%v must not fill missing quoteSummary EPS", destination.EPS)
	}
}

func TestMergeFundamentalsPrefersTimeseriesBeforeQuoteSummaryInfoFallbacks(t *testing.T) {
	quoteSummary := YahooFundamentals{
		Debt: floatPointer(100), Cash: floatPointer(20), Shares: floatPointer(10), SharesSource: "info",
		Revenue: floatPointer(500), OperatingCashflow: floatPointer(40),
	}
	timeseries := YahooFundamentals{
		Debt: floatPointer(110), Cash: floatPointer(30), Shares: floatPointer(11), SharesSource: "fundamentals_timeseries",
		Revenue: floatPointer(600), OperatingCashflow: floatPointer(50),
	}
	mergeMissingFundamentals(&quoteSummary, timeseries)
	for name, check := range map[string]struct {
		got  *float64
		want float64
	}{
		"debt": {quoteSummary.Debt, 110}, "cash": {quoteSummary.Cash, 30},
		"shares": {quoteSummary.Shares, 11}, "revenue": {quoteSummary.Revenue, 600},
		"operating cash flow": {quoteSummary.OperatingCashflow, 50},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s = %v, want time-series value %v", name, check.got, check.want)
		}
	}
	if quoteSummary.SharesSource != "fundamentals_timeseries" {
		t.Errorf("shares source = %q, want fundamentals_timeseries", quoteSummary.SharesSource)
	}
}

func TestMetricNumberMatchesPythonFloatForBooleanRaw(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want float64
	}{{"true", 1}, {"false", 0}} {
		got := metricNumber(YahooMetricDTO{Raw: []byte(test.raw)})
		if got == nil || *got != test.want {
			t.Errorf("metricNumber(%s) = %v, want %v", test.raw, got, test.want)
		}
	}
}

func TestFundamentalsNeedsTimeseriesForNormalizedNetIncomeEPS(t *testing.T) {
	ptr := func(value float64) *float64 { return &value }
	value := YahooFundamentals{
		EPS: ptr(10), FreeCashflow: ptr(10), Debt: ptr(10),
		Cash: ptr(10), Shares: ptr(10), Revenue: ptr(10),
		NetIncome: ptr(10), DepreciationAndAmortization: ptr(1),
		CapitalExpenditure: ptr(1), ChangeInWorkingCapital: ptr(0),
		OperatingIncome: ptr(10), InterestExpense: ptr(1),
		Buyback: ptr(0), Equity: ptr(10),
		QuarterlyRevenue: []float64{10}, QuarterlyEPS: []float64{1},
		AnnualRevenue: []float64{10, 9}, AnnualEPS: []float64{1, 1}, AnnualCapitalExpenditure: []float64{-1, -1},
	}
	if !fundamentalsNeedsTimeseries(value) {
		t.Fatal("missing annual net income should trigger a fundamentals timeseries refresh")
	}
	value.AnnualNetIncome = []float64{10, 9}
	if fundamentalsNeedsTimeseries(value) {
		t.Fatal("complete annual net income history should not trigger a refresh")
	}
}

func TestEarningsTrendRevisionUsesOneMonthConsensusChange(t *testing.T) {
	current, prior := 4.4, 4.0
	got := earningsTrendRevision([]YahooEarningsTrendEntryDTO{{
		Period: "0q",
		EPSTrend: struct {
			Current       YahooMetricDTO `json:"current"`
			SevenDaysAgo  YahooMetricDTO `json:"7daysAgo"`
			ThirtyDaysAgo YahooMetricDTO `json:"30daysAgo"`
		}{Current: YahooMetricDTO{Raw: []byte("4.4")}, ThirtyDaysAgo: YahooMetricDTO{Raw: []byte("4.0")}},
	}})
	if got == nil || math.Abs(*got-(current-prior)/prior) > 1e-12 {
		t.Fatalf("EPS revision=%v, want %.2f", got, (current-prior)/prior)
	}
}

func TestFundamentalsFetchReportsHTTPAndYahooErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		code int
	}{
		{"http", `{"error":"nope"}`, http.StatusBadGateway},
		{"yahoo", `{"quoteSummary":{"result":[],"error":{"code":"Not Found","description":"No data"}}}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := NewFundamentalsClientWithEndpoints(http.DefaultClient, server.URL+"/summary/%s", "")
			_, err := client.Fetch(context.Background(), "TEST")
			if err == nil {
				t.Fatal("Fetch() error = nil, want error")
			}
			if test.name == "http" && !containsError(err, "HTTP 502") {
				t.Fatalf("Fetch() error = %v, want HTTP 502", err)
			}
			if test.name == "yahoo" && !containsError(err, "Not Found") {
				t.Fatalf("Fetch() error = %v, want Yahoo API error", err)
			}
		})
	}
}

func TestFundamentalsFetchRejectsTrailingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{}],"error":null}} {}`))
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/summary/%s", "")
	_, err := client.Fetch(context.Background(), "TEST")
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("Fetch() error = %v, want trailing JSON error", err)
	}
}

func TestFundamentalsFetchHonorsContextCancellation(t *testing.T) {
	client := NewFundamentalsClientWithEndpoints(&http.Client{Timeout: time.Second}, "http://127.0.0.1:1/summary/%s", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Fetch(ctx, "TEST")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want context.Canceled", err)
	}
}

func TestFundamentalsFetchPropagatesCancellationDuringConcurrentEnrichment(t *testing.T) {
	started := make(chan string, 2)
	canceled := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			_, _ = fmt.Fprint(w, `{"quoteSummary":{"result":[{}],"error":null}}`)
		case "/timeseries/TEST", "/history/TEST":
			started <- r.URL.Path
			<-r.Context().Done()
			canceled <- r.URL.Path
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	client.HistoricalURL = server.URL + "/history/%s"
	client.Gate = make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Fetch(ctx, "TEST")
		result <- err
	}()

	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatalf("concurrent enrichment requests did not both start: %v", seen)
		}
	}
	if !seen["/timeseries/TEST"] || !seen["/history/TEST"] {
		cancel()
		t.Fatalf("started requests = %v, want timeseries and history", seen)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Fetch() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch() did not finish after caller cancellation")
	}
	for range 2 {
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("caller cancellation did not reach both upstream requests")
		}
	}
}

func TestFundamentalsFetchPropagatesCancellationDuringTimeseriesFallback(t *testing.T) {
	timeseriesStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/summary/TEST":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/timeseries/TEST":
			timeseriesStarted <- struct{}{}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewFundamentalsClientWithEndpoints(server.Client(), server.URL+"/summary/%s", server.URL+"/timeseries/%s")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Fetch(ctx, "TEST")
		result <- err
	}()
	select {
	case <-timeseriesStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeseries fallback request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Fetch() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch() did not finish after caller cancellation")
	}
}

func containsError(err error, fragment string) bool {
	return err != nil && (len(fragment) == 0 || contains(err.Error(), fragment))
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
