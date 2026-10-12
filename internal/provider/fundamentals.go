package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ymodels "github.com/wnjoon/go-yfinance/pkg/models"
	"option-scope-go/internal/outboundhttp"
)

const (
	defaultQuoteSummaryURL = "https://query1.finance.yahoo.com/v10/finance/quoteSummary/%s"
	defaultTimeseriesURL   = "https://query1.finance.yahoo.com/ws/fundamentals-timeseries/v1/finance/timeseries/%s"
	defaultHistoricalURL   = "https://query1.finance.yahoo.com/v8/finance/chart/%s"
)

// YahooMetricDTO is the common Yahoo Finance wrapper used by quoteSummary and
// fundamentals-timeseries values. Raw is intentionally kept as JSON because
// Yahoo sometimes returns a number and sometimes a quoted number.
type YahooMetricDTO struct {
	Raw json.RawMessage `json:"raw"`
	Fmt string          `json:"fmt,omitempty"`
}

type YahooAPIErrorDTO struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

func (e *YahooAPIErrorDTO) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Description
	}
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

type YahooQuoteSummaryResponseDTO struct {
	QuoteSummary struct {
		Result []YahooQuoteSummaryResultDTO `json:"result"`
		Error  *YahooAPIErrorDTO            `json:"error"`
	} `json:"quoteSummary"`
}

type YahooQuoteSummaryResultDTO struct {
	QuoteType struct {
		QuoteType string `json:"quoteType"`
	} `json:"quoteType"`
	RiskFreeRate  YahooMetricDTO `json:"riskFreeRate"`
	TreasuryYield YahooMetricDTO `json:"treasuryYield"`
	US10Y         YahooMetricDTO `json:"us10y"`
	Price         struct {
		RegularMarketPrice YahooMetricDTO `json:"regularMarketPrice"`
		PreviousClose      YahooMetricDTO `json:"previousClose"`
		MarketCap          YahooMetricDTO `json:"marketCap"`
	} `json:"price"`
	DefaultKeyStatistics struct {
		TrailingEPS                 YahooMetricDTO `json:"trailingEps"`
		ForwardEPS                  YahooMetricDTO `json:"forwardEps"`
		FreeCashflow                YahooMetricDTO `json:"freeCashflow"`
		SharesOutstanding           YahooMetricDTO `json:"sharesOutstanding"`
		MarketCap                   YahooMetricDTO `json:"marketCap"`
		BookValue                   YahooMetricDTO `json:"bookValue"`
		PriceToSales                YahooMetricDTO `json:"priceToSalesTrailing12Months"`
		EnterpriseToEBITDA          YahooMetricDTO `json:"enterpriseToEbitda"`
		ForwardEBITDA               YahooMetricDTO `json:"forwardEbitda"`
		FFOPerShare                 YahooMetricDTO `json:"ffoPerShare"`
		FundsFromOperationsPerShare YahooMetricDTO `json:"fundsFromOperationsPerShare"`
		NormalizedFFOPerShare       YahooMetricDTO `json:"normalizedFfoPerShare"`
		AFFOPerShare                YahooMetricDTO `json:"affoPerShare"`
		AdjustedAFFOPerShare        YahooMetricDTO `json:"adjustedFundsFromOperationsPerShare"`
		NAVPerShare                 YahooMetricDTO `json:"navPerShare"`
		NetAssetValuePerShare       YahooMetricDTO `json:"netAssetValuePerShare"`
		RABPerShare                 YahooMetricDTO `json:"rabPerShare"`
		RegulatedAssetBasePerShare  YahooMetricDTO `json:"regulatedAssetBasePerShare"`
		EmbeddedValuePerShare       YahooMetricDTO `json:"embeddedValuePerShare"`
		EVPerShare                  YahooMetricDTO `json:"evPerShare"`
		NewBusinessValuePerShare    YahooMetricDTO `json:"newBusinessValuePerShare"`
		NBVPerShare                 YahooMetricDTO `json:"nbvPerShare"`
		PipelineRNPV                YahooMetricDTO `json:"pipelineRnPV"`
		RiskAdjustedNPV             YahooMetricDTO `json:"riskAdjustedNPV"`
		ApprovedDrugValue           YahooMetricDTO `json:"approvedDrugValue"`
		MarketedProductValue        YahooMetricDTO `json:"marketedProductValue"`
		ApprovedProductsValue       YahooMetricDTO `json:"approvedProductsValue"`
		RiskFreeRate                YahooMetricDTO `json:"riskFreeRate"`
		TreasuryYield               YahooMetricDTO `json:"treasuryYield"`
		US10Y                       YahooMetricDTO `json:"us10y"`
	} `json:"defaultKeyStatistics"`
	FinancialData struct {
		CurrentPrice                       YahooMetricDTO `json:"currentPrice"`
		ForwardEPS                         YahooMetricDTO `json:"forwardEps"`
		EPSForward                         YahooMetricDTO `json:"epsForward"`
		FreeCashflow                       YahooMetricDTO `json:"freeCashflow"`
		OperatingCashflow                  YahooMetricDTO `json:"operatingCashflow"`
		Ebitda                             YahooMetricDTO `json:"ebitda"`
		ForwardEBITDA                      YahooMetricDTO `json:"forwardEbitda"`
		EBITDAForward                      YahooMetricDTO `json:"ebitdaForward"`
		TotalDebt                          YahooMetricDTO `json:"totalDebt"`
		TotalCash                          YahooMetricDTO `json:"totalCash"`
		TotalRevenue                       YahooMetricDTO `json:"totalRevenue"`
		TrailingAnnualRevenue              YahooMetricDTO `json:"trailingAnnualRevenue"`
		Revenue                            YahooMetricDTO `json:"revenue"`
		TotalRevenueTTM                    YahooMetricDTO `json:"totalRevenueTTM"`
		EarningsGrowth                     YahooMetricDTO `json:"earningsGrowth"`
		EarningsQuarterlyGrowth            YahooMetricDTO `json:"earningsQuarterlyGrowth"`
		RevenueGrowth                      YahooMetricDTO `json:"revenueGrowth"`
		AICapexGrowth                      YahooMetricDTO `json:"aiCapexGrowth"`
		CapexGrowth                        YahooMetricDTO `json:"capexGrowth"`
		CapitalExpenditureGrowth           YahooMetricDTO `json:"capitalExpenditureGrowth"`
		DaysInventoryOutstanding           YahooMetricDTO `json:"daysInventoryOutstanding"`
		MarketCap                          YahooMetricDTO `json:"marketCap"`
		DividendRate                       YahooMetricDTO `json:"dividendRate"`
		TrailingAnnualDividendRate         YahooMetricDTO `json:"trailingAnnualDividendRate"`
		BookValue                          YahooMetricDTO `json:"bookValue"`
		TargetMeanPrice                    YahooMetricDTO `json:"targetMeanPrice"`
		TargetMedianPrice                  YahooMetricDTO `json:"targetMedianPrice"`
		TargetLowPrice                     YahooMetricDTO `json:"targetLowPrice"`
		TargetHighPrice                    YahooMetricDTO `json:"targetHighPrice"`
		PriceToSales                       YahooMetricDTO `json:"priceToSalesTrailing12Months"`
		PriceToSalesAlias                  YahooMetricDTO `json:"priceToSales"`
		EnterpriseToEBITDA                 YahooMetricDTO `json:"enterpriseToEbitda"`
		EnterpriseToEBITDAForward          YahooMetricDTO `json:"enterpriseToEbitdaForward"`
		InterestExpense                    YahooMetricDTO `json:"interestExpense"`
		InterestExpenseToRevenue           YahooMetricDTO `json:"interestExpenseToRevenue"`
		InterestExpenseNonOperating        YahooMetricDTO `json:"interestExpenseNonOperating"`
		ROE                                YahooMetricDTO `json:"returnOnEquity"`
		ROEAlias                           YahooMetricDTO `json:"roe"`
		ReturnOnAverageEquity              YahooMetricDTO `json:"returnOnAverageEquity"`
		DefenseRevenueGrowth               YahooMetricDTO `json:"defenseRevenueGrowth"`
		AAndDRevenueGrowth                 YahooMetricDTO `json:"aAndDRevenueGrowth"`
		AerospaceDefenseRevenueGrowth      YahooMetricDTO `json:"aerospaceDefenseRevenueGrowth"`
		FFOPerShare                        YahooMetricDTO `json:"ffoPerShare"`
		FundsFromOperationsPerShare        YahooMetricDTO `json:"fundsFromOperationsPerShare"`
		NormalizedFFOPerShare              YahooMetricDTO `json:"normalizedFfoPerShare"`
		AFFOPerShare                       YahooMetricDTO `json:"affoPerShare"`
		AdjustedAFFOPerShare               YahooMetricDTO `json:"adjustedFundsFromOperationsPerShare"`
		NAVPerShare                        YahooMetricDTO `json:"navPerShare"`
		NetAssetValuePerShare              YahooMetricDTO `json:"netAssetValuePerShare"`
		RABPerShare                        YahooMetricDTO `json:"rabPerShare"`
		RegulatedAssetBasePerShare         YahooMetricDTO `json:"regulatedAssetBasePerShare"`
		EmbeddedValuePerShare              YahooMetricDTO `json:"embeddedValuePerShare"`
		EVPerShare                         YahooMetricDTO `json:"evPerShare"`
		EmbeddedValue                      YahooMetricDTO `json:"embeddedValue"`
		NewBusinessValuePerShare           YahooMetricDTO `json:"newBusinessValuePerShare"`
		NBVPerShare                        YahooMetricDTO `json:"nbvPerShare"`
		NewBusinessValue                   YahooMetricDTO `json:"newBusinessValue"`
		PipelineRNPV                       YahooMetricDTO `json:"pipelineRNPV"`
		PipelineRnPV                       YahooMetricDTO `json:"pipelineRnPV"`
		RiskAdjustedNPV                    YahooMetricDTO `json:"riskAdjustedNPV"`
		RNPV                               YahooMetricDTO `json:"rNPV"`
		ApprovedDrugValue                  YahooMetricDTO `json:"approvedDrugValue"`
		MarketedProductValue               YahooMetricDTO `json:"marketedProductValue"`
		ApprovedProductsValue              YahooMetricDTO `json:"approvedProductsValue"`
		MonthlyActiveUsers                 YahooMetricDTO `json:"monthlyActiveUsers"`
		ActiveUsers                        YahooMetricDTO `json:"activeUsers"`
		Users                              YahooMetricDTO `json:"users"`
		UserCount                          YahooMetricDTO `json:"userCount"`
		ARPU                               YahooMetricDTO `json:"arpu"`
		AverageRevenuePerUser              YahooMetricDTO `json:"averageRevenuePerUser"`
		ChurnRate                          YahooMetricDTO `json:"churnRate"`
		UserChurnRate                      YahooMetricDTO `json:"userChurnRate"`
		AIRevenueShare                     YahooMetricDTO `json:"aiRevenueShare"`
		AIRevenuePercentage                YahooMetricDTO `json:"aiRevenuePercentage"`
		ArtificialIntelligenceRevenueShare YahooMetricDTO `json:"artificialIntelligenceRevenueShare"`
		NumberOfAnalystOpinions            YahooMetricDTO `json:"numberOfAnalystOpinions"`
		AnalystCount                       YahooMetricDTO `json:"analystCount"`
		AnalystCoverage                    YahooMetricDTO `json:"analystCoverage"`
		RiskFreeRate                       YahooMetricDTO `json:"riskFreeRate"`
		TreasuryYield                      YahooMetricDTO `json:"treasuryYield"`
		US10Y                              YahooMetricDTO `json:"us10y"`
	} `json:"financialData"`
	EarningsTrend struct {
		Trend []YahooEarningsTrendEntryDTO `json:"trend"`
	} `json:"earningsTrend"`
	SummaryDetail struct {
		Beta                       YahooMetricDTO `json:"beta"`
		Beta3Year                  YahooMetricDTO `json:"beta3Year"`
		SharesOutstanding          YahooMetricDTO `json:"sharesOutstanding"`
		MarketCap                  YahooMetricDTO `json:"marketCap"`
		DividendRate               YahooMetricDTO `json:"dividendRate"`
		TrailingAnnualDividendRate YahooMetricDTO `json:"trailingAnnualDividendRate"`
		BookValue                  YahooMetricDTO `json:"bookValue"`
		PriceToSales               YahooMetricDTO `json:"priceToSalesTrailing12Months"`
		EnterpriseToEBITDA         YahooMetricDTO `json:"enterpriseToEbitda"`
		RiskFreeRate               YahooMetricDTO `json:"riskFreeRate"`
		TreasuryYield              YahooMetricDTO `json:"treasuryYield"`
		US10Y                      YahooMetricDTO `json:"us10y"`
	} `json:"summaryDetail"`
	SummaryProfile struct {
		Sector   string `json:"sector"`
		Industry string `json:"industry"`
	} `json:"summaryProfile"`
	AssetProfile struct {
		Sector              string `json:"sector"`
		Industry            string `json:"industry"`
		IndustryKey         string `json:"industryKey"`
		IndustryKeyAlias    string `json:"industry_key"`
		LongBusinessSummary string `json:"longBusinessSummary"`
	} `json:"assetProfile"`
	BalanceSheetHistory struct {
		Statements []YahooBalanceSheetStatementDTO `json:"balanceSheetStatements"`
	} `json:"balanceSheetHistory"`
	CashflowStatementHistory struct {
		Statements []YahooCashflowStatementDTO `json:"cashflowStatements"`
	} `json:"cashflowStatementHistory"`
}

type YahooStatementDateDTO struct {
	Raw json.RawMessage `json:"raw"`
	Fmt string          `json:"fmt"`
}

type YahooBalanceSheetStatementDTO struct {
	EndDate                                    YahooStatementDateDTO `json:"endDate"`
	TotalDebt                                  YahooMetricDTO        `json:"totalDebt"`
	TotalDebtAndCapitalLeaseObligation         YahooMetricDTO        `json:"totalDebtAndCapitalLeaseObligation"`
	CashCashEquivalentsAndShortTermInvestments YahooMetricDTO        `json:"cashCashEquivalentsAndShortTermInvestments"`
	CashAndCashEquivalents                     YahooMetricDTO        `json:"cashAndCashEquivalents"`
	CashFinancial                              YahooMetricDTO        `json:"cashFinancial"`
	OrdinarySharesNumber                       YahooMetricDTO        `json:"ordinarySharesNumber"`
	ShareIssued                                YahooMetricDTO        `json:"shareIssued"`
	StockholdersEquity                         YahooMetricDTO        `json:"stockholdersEquity"`
	TotalStockholderEquity                     YahooMetricDTO        `json:"totalStockholderEquity"`
	CommonStockEquity                          YahooMetricDTO        `json:"commonStockEquity"`
}

type yahooBalanceSheetShareStatementDTO struct {
	EndDate              YahooStatementDateDTO `json:"endDate"`
	OrdinarySharesNumber YahooMetricDTO        `json:"ordinarySharesNumber"`
	ShareIssued          YahooMetricDTO        `json:"shareIssued"`
}

type YahooCashflowStatementDTO struct {
	EndDate                              YahooStatementDateDTO `json:"endDate"`
	FreeCashflow                         YahooMetricDTO        `json:"freeCashFlow"`
	OperatingCashflow                    YahooMetricDTO        `json:"operatingCashFlow"`
	TotalCashFromOperatingActivities     YahooMetricDTO        `json:"totalCashFromOperatingActivities"`
	CapitalExpenditure                   YahooMetricDTO        `json:"capitalExpenditures"`
	CapitalExpenditureAlias              YahooMetricDTO        `json:"capitalExpenditure"`
	InterestExpense                      YahooMetricDTO        `json:"interestExpense"`
	InterestExpenseNonOperating          YahooMetricDTO        `json:"interestExpenseNonOperating"`
	NetNonOperatingInterestIncomeExpense YahooMetricDTO        `json:"netNonOperatingInterestIncomeExpense"`
}

type YahooEarningsTrendEntryDTO struct {
	Period   string `json:"period"`
	EPSTrend struct {
		Current       YahooMetricDTO `json:"current"`
		SevenDaysAgo  YahooMetricDTO `json:"7daysAgo"`
		ThirtyDaysAgo YahooMetricDTO `json:"30daysAgo"`
	} `json:"epsTrend"`
}

type YahooTimeseriesEntryDTO struct {
	AsOfDate      string         `json:"asOfDate"`
	PeriodType    string         `json:"periodType"`
	ReportedValue YahooMetricDTO `json:"reportedValue"`
}

type YahooTimeseriesResultDTO struct {
	Meta struct {
		Type []string `json:"type"`
	} `json:"meta"`
	// Yahoo sometimes returns one timestamp array at the result level and
	// leaves each reportedValue without an asOfDate. Keep it so the adapter can
	// restore the same newest-first ordering used by the Python client.
	Timestamp                      []int64                   `json:"timestamp"`
	AnnualFreeCashflow             []YahooTimeseriesEntryDTO `json:"annualFreeCashFlow"`
	AnnualOperatingCF              []YahooTimeseriesEntryDTO `json:"annualOperatingCashFlow"`
	AnnualCapitalExpend            []YahooTimeseriesEntryDTO `json:"annualCapitalExpenditure"`
	AnnualDilutedEPS               []YahooTimeseriesEntryDTO `json:"annualDilutedEPS"`
	AnnualDilutedShares            []YahooTimeseriesEntryDTO `json:"annualDilutedAverageShares"`
	AnnualDebt                     []YahooTimeseriesEntryDTO `json:"annualTotalDebt"`
	AnnualCash                     []YahooTimeseriesEntryDTO `json:"annualCashCashEquivalentsAndShortTermInvestments"`
	AnnualRevenue                  []YahooTimeseriesEntryDTO `json:"annualTotalRevenue"`
	AnnualOperatingIncome          []YahooTimeseriesEntryDTO `json:"annualOperatingIncome"`
	AnnualNetIncome                []YahooTimeseriesEntryDTO `json:"annualNetIncome"`
	AnnualDepreciationAmortization []YahooTimeseriesEntryDTO `json:"annualDepreciationAndAmortization"`
	AnnualWorkingCapital           []YahooTimeseriesEntryDTO `json:"annualChangeInWorkingCapital"`
	AnnualBuyback                  []YahooTimeseriesEntryDTO `json:"annualRepurchaseOfCapitalStock"`
	AnnualEquity                   []YahooTimeseriesEntryDTO `json:"annualStockholdersEquity"`
	QuarterlyRevenue               []YahooTimeseriesEntryDTO `json:"quarterlyTotalRevenue"`
	QuarterlyDilutedEPS            []YahooTimeseriesEntryDTO `json:"quarterlyDilutedEPS"`
	TrailingDilutedEPS             []YahooTimeseriesEntryDTO `json:"trailingDilutedEPS"`
}

type YahooTimeseriesResponseDTO struct {
	Timeseries struct {
		Result []YahooTimeseriesResultDTO `json:"result"`
		Error  *YahooAPIErrorDTO          `json:"error"`
	} `json:"timeseries"`
}

type yahooCustomQuoteSummaryResponseDTO struct {
	QuoteSummary struct {
		Result []yahooCustomQuoteSummaryResultDTO `json:"result"`
		Error  *YahooAPIErrorDTO                  `json:"error"`
	} `json:"quoteSummary"`
}

// This extension is limited to valuation inputs that go-yfinance's Info model
// does not expose. Standard quoteSummary fields are read through the SDK.
type yahooCustomQuoteSummaryResultDTO struct {
	RiskFreeRate        YahooMetricDTO `json:"riskFreeRate"`
	TreasuryYield       YahooMetricDTO `json:"treasuryYield"`
	US10Y               YahooMetricDTO `json:"us10y"`
	BalanceSheetHistory struct {
		Statements []yahooBalanceSheetShareStatementDTO `json:"balanceSheetStatements"`
	} `json:"balanceSheetHistory"`
	AssetProfile struct {
		IndustryKey      string `json:"industryKey"`
		IndustryKeyAlias string `json:"industry_key"`
	} `json:"assetProfile"`
	DefaultKeyStatistics struct {
		RiskFreeRate                YahooMetricDTO `json:"riskFreeRate"`
		TreasuryYield               YahooMetricDTO `json:"treasuryYield"`
		US10Y                       YahooMetricDTO `json:"us10y"`
		FFOPerShare                 YahooMetricDTO `json:"ffoPerShare"`
		FundsFromOperationsPerShare YahooMetricDTO `json:"fundsFromOperationsPerShare"`
		NormalizedFFOPerShare       YahooMetricDTO `json:"normalizedFfoPerShare"`
		AFFOPerShare                YahooMetricDTO `json:"affoPerShare"`
		AdjustedAFFOPerShare        YahooMetricDTO `json:"adjustedFundsFromOperationsPerShare"`
		NAVPerShare                 YahooMetricDTO `json:"navPerShare"`
		NetAssetValuePerShare       YahooMetricDTO `json:"netAssetValuePerShare"`
		RABPerShare                 YahooMetricDTO `json:"rabPerShare"`
		RegulatedAssetBasePerShare  YahooMetricDTO `json:"regulatedAssetBasePerShare"`
		EmbeddedValuePerShare       YahooMetricDTO `json:"embeddedValuePerShare"`
		EVPerShare                  YahooMetricDTO `json:"evPerShare"`
		NewBusinessValuePerShare    YahooMetricDTO `json:"newBusinessValuePerShare"`
		NBVPerShare                 YahooMetricDTO `json:"nbvPerShare"`
		PipelineRNPV                YahooMetricDTO `json:"pipelineRnPV"`
		RiskAdjustedNPV             YahooMetricDTO `json:"riskAdjustedNPV"`
		ApprovedDrugValue           YahooMetricDTO `json:"approvedDrugValue"`
		MarketedProductValue        YahooMetricDTO `json:"marketedProductValue"`
		ApprovedProductsValue       YahooMetricDTO `json:"approvedProductsValue"`
	} `json:"defaultKeyStatistics"`
	FinancialData struct {
		RiskFreeRate                       YahooMetricDTO `json:"riskFreeRate"`
		TreasuryYield                      YahooMetricDTO `json:"treasuryYield"`
		US10Y                              YahooMetricDTO `json:"us10y"`
		AICapexGrowth                      YahooMetricDTO `json:"aiCapexGrowth"`
		CapexGrowth                        YahooMetricDTO `json:"capexGrowth"`
		CapitalExpenditureGrowth           YahooMetricDTO `json:"capitalExpenditureGrowth"`
		DaysInventoryOutstanding           YahooMetricDTO `json:"daysInventoryOutstanding"`
		ForwardEBITDA                      YahooMetricDTO `json:"forwardEbitda"`
		EBITDAForward                      YahooMetricDTO `json:"ebitdaForward"`
		DefenseRevenueGrowth               YahooMetricDTO `json:"defenseRevenueGrowth"`
		AAndDRevenueGrowth                 YahooMetricDTO `json:"aAndDRevenueGrowth"`
		AerospaceDefenseRevenueGrowth      YahooMetricDTO `json:"aerospaceDefenseRevenueGrowth"`
		FFOPerShare                        YahooMetricDTO `json:"ffoPerShare"`
		FundsFromOperationsPerShare        YahooMetricDTO `json:"fundsFromOperationsPerShare"`
		NormalizedFFOPerShare              YahooMetricDTO `json:"normalizedFfoPerShare"`
		AFFOPerShare                       YahooMetricDTO `json:"affoPerShare"`
		AdjustedAFFOPerShare               YahooMetricDTO `json:"adjustedFundsFromOperationsPerShare"`
		NAVPerShare                        YahooMetricDTO `json:"navPerShare"`
		NetAssetValuePerShare              YahooMetricDTO `json:"netAssetValuePerShare"`
		RABPerShare                        YahooMetricDTO `json:"rabPerShare"`
		RegulatedAssetBasePerShare         YahooMetricDTO `json:"regulatedAssetBasePerShare"`
		EmbeddedValuePerShare              YahooMetricDTO `json:"embeddedValuePerShare"`
		EVPerShare                         YahooMetricDTO `json:"evPerShare"`
		EmbeddedValue                      YahooMetricDTO `json:"embeddedValue"`
		NewBusinessValuePerShare           YahooMetricDTO `json:"newBusinessValuePerShare"`
		NBVPerShare                        YahooMetricDTO `json:"nbvPerShare"`
		NewBusinessValue                   YahooMetricDTO `json:"newBusinessValue"`
		PipelineRNPV                       YahooMetricDTO `json:"pipelineRNPV"`
		PipelineRnPV                       YahooMetricDTO `json:"pipelineRnPV"`
		RiskAdjustedNPV                    YahooMetricDTO `json:"riskAdjustedNPV"`
		RNPV                               YahooMetricDTO `json:"rNPV"`
		ApprovedDrugValue                  YahooMetricDTO `json:"approvedDrugValue"`
		MarketedProductValue               YahooMetricDTO `json:"marketedProductValue"`
		ApprovedProductsValue              YahooMetricDTO `json:"approvedProductsValue"`
		MonthlyActiveUsers                 YahooMetricDTO `json:"monthlyActiveUsers"`
		ActiveUsers                        YahooMetricDTO `json:"activeUsers"`
		Users                              YahooMetricDTO `json:"users"`
		UserCount                          YahooMetricDTO `json:"userCount"`
		ARPU                               YahooMetricDTO `json:"arpu"`
		AverageRevenuePerUser              YahooMetricDTO `json:"averageRevenuePerUser"`
		ChurnRate                          YahooMetricDTO `json:"churnRate"`
		UserChurnRate                      YahooMetricDTO `json:"userChurnRate"`
		AIRevenueShare                     YahooMetricDTO `json:"aiRevenueShare"`
		AIRevenuePercentage                YahooMetricDTO `json:"aiRevenuePercentage"`
		ArtificialIntelligenceRevenueShare YahooMetricDTO `json:"artificialIntelligenceRevenueShare"`
	} `json:"financialData"`
}

// YahooFundamentals is the bounded, provider-neutral subset used by valuation.
// Pointers distinguish a missing Yahoo value from a valid zero.
type YahooFundamentals struct {
	Symbol string
	// QuoteType is Yahoo's instrument classification (EQUITY, ETF,
	// MUTUALFUND, INDEX, etc.). Company valuation models must not be applied
	// to pooled investment vehicles.
	QuoteType string
	// TimeseriesRetryable records a transient failure in optional Yahoo
	// fundamentals enrichment so valuation can expose a short retry.
	TimeseriesRetryable bool
	// ValuationUpstreamError includes optional quoteSummary, timeseries, and
	// historical valuation-data failures. Python uses any such failure to allow
	// its low-confidence information-page P/S fallback.
	ValuationUpstreamError            bool
	ValuationUpstreamRetryable        bool
	Price                             *float64
	EPS                               *float64
	ForwardEPS                        *float64
	FreeCashflow                      *float64
	Debt                              *float64
	Cash                              *float64
	Shares                            *float64
	SharesSource                      string
	Revenue                           *float64
	CurrentYearRevenueEstimate        *float64
	NextYearRevenueEstimate           *float64
	CurrentYearRevenueAnalysts        int
	NextYearRevenueAnalysts           int
	Industry                          string
	IndustryKey                       string
	Sector                            string
	OperatingCashflow                 *float64
	EBITDA                            *float64
	OperatingIncome                   *float64
	NetIncome                         *float64
	DepreciationAndAmortization       *float64
	CapitalExpenditure                *float64
	ChangeInWorkingCapital            *float64
	Buyback                           *float64
	Equity                            *float64
	QuarterlyRevenue                  []float64
	QuarterlyEPS                      []float64
	AnnualRevenue                     []float64
	AnnualEPS                         []float64
	AnnualNetIncome                   []float64
	AnnualCapitalExpenditure          []float64
	AnnualDebt                        []float64
	AnnualCash                        []float64
	AnnualShares                      []float64
	AnnualOperatingIncome             []float64
	AnnualDepreciationAndAmortization []float64
	AnnualEquity                      []float64
	Beta                              *float64
	RiskFreeRate                      *float64
	EarningsGrowth                    *float64
	EarningsQuarterlyGrowth           *float64
	RevenueGrowth                     *float64
	CapexGrowth                       *float64
	EPSRevision                       *float64
	InventoryDays                     *float64
	MarketCap                         *float64
	DividendRate                      *float64
	BookValue                         *float64
	TargetMeanPrice                   *float64
	TargetLowPrice                    *float64
	TargetHighPrice                   *float64
	PriceToSales                      *float64
	EnterpriseToEBITDA                *float64
	ForwardEBITDA                     *float64
	InterestExpense                   *float64
	ROE                               *float64
	DefenseRevenueGrowth              *float64
	FFOPerShare                       *float64
	AFFOPerShare                      *float64
	NAVPerShare                       *float64
	RABPerShare                       *float64
	EmbeddedValuePerShare             *float64
	NewBusinessValuePerShare          *float64
	PipelineRNPV                      *float64
	ApprovedDrugValue                 *float64
	PlatformUsers                     *float64
	ARPU                              *float64
	ChurnRate                         *float64
	AIRevenueShare                    *float64
	AnalystCount                      *float64
	LongBusinessSummary               string
	// HistoricalPrices is optional and ordered newest-to-oldest. The SDK history
	// adapter populates it when historical valuation percentiles are requested.
	HistoricalPrices []float64
}

// FundamentalsClient fetches Yahoo public fundamentals. URL templates may
// contain one %s placeholder for the escaped symbol; this makes httptest
// servers and alternative Yahoo-compatible endpoints easy to inject.
type FundamentalsClient struct {
	Client          *http.Client
	crumbAuth       yahooCrumbGetter
	tickerFactory   yahooTickerFactory
	QuoteSummaryURL string
	TimeseriesURL   string
	HistoricalURL   string
	Gate            chan struct{}
	GateWait        time.Duration
}

func NewFundamentalsClient(client *http.Client) *FundamentalsClient {
	result := NewFundamentalsClientWithEndpoints(client, defaultQuoteSummaryURL, defaultTimeseriesURL)
	result.HistoricalURL = defaultHistoricalURL
	return result
}

func NewFundamentalsClientWithEndpoints(client *http.Client, quoteSummaryURL, timeseriesURL string) *FundamentalsClient {
	if client == nil {
		client = outboundhttp.DefaultBrowserClient(30 * time.Second)
	}
	if strings.TrimSpace(quoteSummaryURL) == "" {
		quoteSummaryURL = defaultQuoteSummaryURL
	}
	return &FundamentalsClient{
		Client: client, QuoteSummaryURL: quoteSummaryURL, TimeseriesURL: strings.TrimSpace(timeseriesURL),
	}
}

// Fetch reads SDK-backed Yahoo data and supplements only the valuation fields
// that the SDK models do not expose. Endpoint-injected clients retain the
// legacy fallback path used by compatibility tests.
func (c *FundamentalsClient) Fetch(ctx context.Context, rawSymbol string) (YahooFundamentals, error) {
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return YahooFundamentals{}, err
	}
	if c == nil {
		return YahooFundamentals{}, errors.New("fundamentals client is nil")
	}
	if c.tickerFactory != nil {
		return c.fetchWithSDK(ctx, symbol)
	}
	if c.Client == nil {
		return YahooFundamentals{}, errors.New("fundamentals client is nil")
	}
	return c.fetchWithLegacyEndpoints(ctx, symbol)
}

func (c *FundamentalsClient) fetchWithLegacyEndpoints(ctx context.Context, symbol string) (YahooFundamentals, error) {
	// Production wiring installs go-yfinance's ticker factory in NewYahoo.
	// Keep this raw endpoint path for explicitly endpoint-injected compatibility
	// clients; the SDK-backed path below owns modeled quote, statement, and history data.
	var payload YahooQuoteSummaryResponseDTO
	quoteSummaryErr := c.fetchJSON(ctx, endpointURL(c.QuoteSummaryURL, symbol, map[string]string{
		"modules": "quoteType,price,defaultKeyStatistics,financialData,summaryDetail,summaryProfile,assetProfile,earningsTrend,balanceSheetHistory,cashflowStatementHistory",
	}), &payload)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return YahooFundamentals{}, ctxErr
	}
	if quoteSummaryErr == nil && payload.QuoteSummary.Error != nil {
		quoteSummaryErr = payload.QuoteSummary.Error
	}
	if quoteSummaryErr == nil && len(payload.QuoteSummary.Result) == 0 {
		quoteSummaryErr = errors.New("Yahoo quoteSummary returned no fundamentals")
	}
	if quoteSummaryErr != nil {
		result, fallbackErr := c.fetchTimeseriesAfterQuoteSummaryFailure(ctx, symbol, quoteSummaryErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return YahooFundamentals{}, ctxErr
		}
		return result, fallbackErr
	}
	result := fundamentalsFromQuoteSummary(symbol, payload.QuoteSummary.Result[0])
	quoteSummary := payload.QuoteSummary.Result[0]
	var parallelHistory *historicalFundamentalsResult
	if c.TimeseriesURL != "" && fundamentalsNeedsTimeseries(result) {
		if c.HistoricalURL != "" && c.canOverlapFundamentalsRequests() {
			series, history := c.fetchTimeseriesAndHistory(ctx, symbol)
			parallelHistory = &history
			if series.err == nil {
				mergeMissingFundamentals(&result, series.value)
			} else {
				recordValuationUpstreamError(&result, series.err)
				result.TimeseriesRetryable = transientFundamentalsError(series.err)
			}
		} else if series, seriesErr := c.fetchTimeseries(ctx, symbol); seriesErr == nil {
			mergeMissingFundamentals(&result, series)
		} else {
			recordValuationUpstreamError(&result, seriesErr)
			result.TimeseriesRetryable = transientFundamentalsError(seriesErr)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return YahooFundamentals{}, ctxErr
	}
	mergeStatementFundamentals(&result, quoteSummary)
	if c.HistoricalURL != "" && !result.TimeseriesRetryable {
		// Historical prices only enrich valuation percentile diagnostics. A chart
		// outage must never make otherwise usable quoteSummary fundamentals fail.
		if parallelHistory != nil {
			if parallelHistory.err == nil {
				applyHistoricalFundamentals(&result, parallelHistory.prices, parallelHistory.spot)
			} else {
				recordValuationUpstreamError(&result, parallelHistory.err)
			}
		} else {
			prices, spot, historyErr := c.fetchHistoricalData(ctx, symbol)
			if historyErr == nil {
				applyHistoricalFundamentals(&result, prices, spot)
			} else {
				recordValuationUpstreamError(&result, historyErr)
			}
		}
	}
	if len(result.AnnualShares) == 0 && result.Shares != nil {
		result.AnnualShares = []float64{*result.Shares}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return YahooFundamentals{}, ctxErr
	}
	return result, nil
}

type sdkFundamentalsData struct {
	quote                      *ymodels.Quote
	quoteErr                   error
	info                       *ymodels.Info
	infoErr                    error
	epsTrend                   []ymodels.EPSTrend
	epsTrendErr                error
	currentYearRevenueEstimate *float64
	nextYearRevenueEstimate    *float64
	currentYearRevenueAnalysts int
	nextYearRevenueAnalysts    int
	valuation                  *ymodels.ValuationMeasures
	valuationErr               error
	incomeAnnual               *ymodels.FinancialStatement
	incomeAnnualErr            error
	incomeQuarterly            *ymodels.FinancialStatement
	incomeQuarterlyErr         error
	incomeTrailing             *ymodels.FinancialStatement
	incomeTrailingErr          error
	balanceAnnual              *ymodels.FinancialStatement
	balanceAnnualErr           error
	cashFlowAnnual             *ymodels.FinancialStatement
	cashFlowAnnualErr          error
	history                    []ymodels.Bar
	historyErr                 error
}

func (c *FundamentalsClient) fetchWithSDK(ctx context.Context, symbol string) (YahooFundamentals, error) {
	if c.Gate != nil && cap(c.Gate) > 1 {
		return c.fetchWithSDKConcurrent(ctx, symbol)
	}
	return c.fetchWithSDKSerial(ctx, symbol)
}

func (c *FundamentalsClient) fetchWithSDKSerial(ctx context.Context, symbol string) (YahooFundamentals, error) {
	data, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(ticker yahooTicker) (sdkFundamentalsData, error) {
		var output sdkFundamentalsData
		output.quote, output.quoteErr = ticker.Quote()
		output.info, output.infoErr = ticker.Info()
		output.epsTrend, output.epsTrendErr = ticker.EPSTrend()
		estimates, estimateErr := ticker.RevenueEstimate()
		output.currentYearRevenueEstimate, output.nextYearRevenueEstimate, output.currentYearRevenueAnalysts, output.nextYearRevenueAnalysts = revenueEstimatesFromSDK(estimates)
		_ = estimateErr
		currentOnly := 0
		output.valuation, output.valuationErr = ticker.GetValuationMeasures("quarterly", &currentOnly)
		output.incomeAnnual, output.incomeAnnualErr = ticker.IncomeStatement("annual")
		output.incomeQuarterly, output.incomeQuarterlyErr = ticker.IncomeStatement("quarterly")
		output.incomeTrailing, output.incomeTrailingErr = ticker.IncomeStatement("trailing")
		output.balanceAnnual, output.balanceAnnualErr = ticker.BalanceSheet("annual")
		output.cashFlowAnnual, output.cashFlowAnnualErr = ticker.CashFlow("annual")
		start := time.Now().UTC().AddDate(-6, 0, 0)
		end := time.Now().UTC().AddDate(0, 0, 1)
		output.history, output.historyErr = ticker.History(ymodels.HistoryParams{
			Start: &start, End: &end, Interval: "1mo", AutoAdjust: false,
		})
		return output, nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return YahooFundamentals{}, ctxErr
		}
		if transientFundamentalsError(err) {
			return YahooFundamentals{
				Symbol: symbol, TimeseriesRetryable: true,
				ValuationUpstreamError: true, ValuationUpstreamRetryable: true,
			}, nil
		}
		return YahooFundamentals{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return YahooFundamentals{}, ctxErr
	}
	return c.mapSDKFundamentals(ctx, symbol, data)
}

func (c *FundamentalsClient) fetchWithSDKConcurrent(ctx context.Context, symbol string) (YahooFundamentals, error) {
	var data sdkFundamentalsData
	type task func()
	tasks := []task{
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.info, data.infoErr = t.Info()
				return struct{}{}, data.infoErr
			})
			data.infoErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.quote, data.quoteErr = t.Quote()
				return struct{}{}, data.quoteErr
			})
			data.quoteErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				currentOnly := 0
				data.valuation, data.valuationErr = t.GetValuationMeasures("quarterly", &currentOnly)
				return struct{}{}, data.valuationErr
			})
			data.valuationErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.epsTrend, data.epsTrendErr = t.EPSTrend()
				estimates, estimateErr := t.RevenueEstimate()
				data.currentYearRevenueEstimate, data.nextYearRevenueEstimate, data.currentYearRevenueAnalysts, data.nextYearRevenueAnalysts = revenueEstimatesFromSDK(estimates)
				_ = estimateErr
				return struct{}{}, data.epsTrendErr
			})
			data.epsTrendErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.incomeAnnual, data.incomeAnnualErr = t.IncomeStatement("annual")
				return struct{}{}, data.incomeAnnualErr
			})
			data.incomeAnnualErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.incomeQuarterly, data.incomeQuarterlyErr = t.IncomeStatement("quarterly")
				return struct{}{}, data.incomeQuarterlyErr
			})
			data.incomeQuarterlyErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.incomeTrailing, data.incomeTrailingErr = t.IncomeStatement("trailing")
				return struct{}{}, data.incomeTrailingErr
			})
			data.incomeTrailingErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.balanceAnnual, data.balanceAnnualErr = t.BalanceSheet("annual")
				return struct{}{}, data.balanceAnnualErr
			})
			data.balanceAnnualErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				data.cashFlowAnnual, data.cashFlowAnnualErr = t.CashFlow("annual")
				return struct{}{}, data.cashFlowAnnualErr
			})
			data.cashFlowAnnualErr = err
		},
		func() {
			_, err := runYahooSDK(ctx, c.Gate, c.GateWait, c.tickerFactory, symbol, func(t yahooTicker) (struct{}, error) {
				start := time.Now().UTC().AddDate(-6, 0, 0)
				end := time.Now().UTC().AddDate(0, 0, 1)
				data.history, data.historyErr = t.History(ymodels.HistoryParams{Start: &start, End: &end, Interval: "1mo", AutoAdjust: false})
				return struct{}{}, data.historyErr
			})
			data.historyErr = err
		},
	}
	workerCount := cap(c.Gate)
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(tasks) {
		workerCount = len(tasks)
	}
	queue := make(chan task, len(tasks))
	for _, run := range tasks {
		queue <- run
	}
	close(queue)
	var requests sync.WaitGroup
	requests.Add(workerCount)
	for range workerCount {
		go func() {
			defer requests.Done()
			for run := range queue {
				if ctx.Err() != nil {
					return
				}
				run()
			}
		}()
	}
	requests.Wait()
	return c.mapSDKFundamentals(ctx, symbol, data)
}

func (c *FundamentalsClient) mapSDKFundamentals(ctx context.Context, symbol string, data sdkFundamentalsData) (YahooFundamentals, error) {
	result := fundamentalsFromSDK(symbol, data)
	for _, requestErr := range []error{
		data.quoteErr, data.infoErr, data.epsTrendErr, data.valuationErr, data.incomeAnnualErr, data.incomeTrailingErr,
		data.incomeQuarterlyErr, data.balanceAnnualErr, data.cashFlowAnnualErr,
	} {
		if requestErr == nil {
			continue
		}
		recordValuationUpstreamError(&result, requestErr)
		if transientFundamentalsError(requestErr) {
			result.TimeseriesRetryable = true
		}
	}
	if data.historyErr != nil {
		recordValuationUpstreamError(&result, data.historyErr)
	}

	if c.QuoteSummaryURL != "" {
		if extension, extensionErr := c.fetchCustomFundamentals(ctx, symbol); extensionErr == nil {
			mergeCustomFundamentals(&result, extension)
		} else {
			recordValuationUpstreamError(&result, extensionErr)
		}
	}
	result.CurrentYearRevenueEstimate = copyFundamentalsFloat(data.currentYearRevenueEstimate)
	result.NextYearRevenueEstimate = copyFundamentalsFloat(data.nextYearRevenueEstimate)
	result.CurrentYearRevenueAnalysts = data.currentYearRevenueAnalysts
	result.NextYearRevenueAnalysts = data.nextYearRevenueAnalysts
	if ttmRevenue, ok := trailingTwelveMonthRevenue(result.QuarterlyRevenue); ok {
		result.Revenue = &ttmRevenue
	}
	if len(result.AnnualShares) == 0 && result.Shares != nil {
		result.AnnualShares = []float64{*result.Shares}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return YahooFundamentals{}, ctxErr
	}
	if data.infoErr != nil && !hasSDKFundamentalData(result) && !transientFundamentalsError(data.infoErr) {
		return YahooFundamentals{}, data.infoErr
	}
	return result, nil
}

func revenueEstimatesFromSDK(estimates []ymodels.RevenueEstimate) (currentYear, nextYear *float64, currentYearAnalysts, nextYearAnalysts int) {
	for _, estimate := range estimates {
		if !finite(estimate.Avg) || estimate.Avg <= 0 {
			continue
		}
		value := estimate.Avg
		switch strings.TrimSpace(estimate.Period) {
		case "0y":
			currentYear = &value
			currentYearAnalysts = estimate.NumberOfAnalysts
		case "+1y":
			nextYear = &value
			nextYearAnalysts = estimate.NumberOfAnalysts
		}
	}
	return currentYear, nextYear, currentYearAnalysts, nextYearAnalysts
}

func trailingTwelveMonthRevenue(quarterly []float64) (float64, bool) {
	if len(quarterly) < 4 {
		return 0, false
	}
	var total float64
	for _, value := range quarterly[:4] {
		if !finite(value) || value <= 0 {
			return 0, false
		}
		total += value
	}
	return total, finite(total) && total > 0
}

func copyFundamentalsFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func fundamentalsFromSDK(symbol string, data sdkFundamentalsData) YahooFundamentals {
	result := YahooFundamentals{Symbol: symbol}
	if info := data.info; info != nil {
		result = fundamentalsFromSDKInfo(symbol, info)
	}
	result.EPSRevision = sdkEPSTrendRevision(data.epsTrend)
	if quote := data.quote; quote != nil {
		if result.QuoteType == "" {
			result.QuoteType = strings.ToUpper(strings.TrimSpace(quote.QuoteType))
		}
		if price := sdkPositiveNumber(quote.RegularMarketPrice); price != nil {
			result.Price = price
		}
		if result.EPS == nil {
			result.EPS = sdkNumber(quote.EpsTrailingTwelveMonths)
		}
		if result.ForwardEPS == nil {
			result.ForwardEPS = sdkNumber(quote.EpsForward)
		}
		if result.Shares == nil {
			result.Shares = sdkInteger(quote.SharesOutstanding)
			result.SharesSource = sharesSourceIfPresent(result.Shares, "quote")
		}
		if result.MarketCap == nil {
			result.MarketCap = sdkInteger(quote.MarketCap)
		}
		if result.DividendRate == nil {
			result.DividendRate = sdkNumber(quote.TrailingAnnualDividendRate)
		}
		if result.BookValue == nil {
			result.BookValue = sdkNumber(quote.BookValue)
		}
	}

	annualIncome := data.incomeAnnual
	quarterlyIncome := data.incomeQuarterly
	trailingIncome := data.incomeTrailing
	annualCashFlow := data.cashFlowAnnual
	annualBalance := data.balanceAnnual
	result.AnnualRevenue = statementSeries(annualIncome, "TotalRevenue", "OperatingRevenue")
	result.AnnualEPS = statementSeries(annualIncome, "DilutedEPS", "BasicEPS")
	result.AnnualNetIncome = statementSeries(annualIncome, "NetIncome", "NetIncomeCommonStockholders")
	result.AnnualOperatingIncome = statementSeries(annualIncome, "OperatingIncome")
	result.AnnualShares = statementSharesSeries(annualBalance)
	sharesSource := "balance_sheet"
	if len(result.AnnualShares) == 0 {
		result.AnnualShares = statementSeries(annualIncome, "DilutedAverageShares", "BasicAverageShares")
		sharesSource = "fundamentals_timeseries"
	}
	result.AnnualDepreciationAndAmortization = statementSeries(annualCashFlow,
		"DepreciationAmortizationDepletion", "Depreciation", "AmortizationOfIntangibles")
	result.AnnualCapitalExpenditure = statementSeries(annualCashFlow, "CapitalExpenditure", "PurchaseOfPPE")
	result.AnnualDebt = statementSeries(annualBalance, "TotalDebt", "NetDebt")
	result.AnnualCash = statementSeries(annualBalance,
		"CashCashEquivalentsAndShortTermInvestments", "CashAndCashEquivalents")
	result.AnnualEquity = statementSeries(annualBalance, "StockholdersEquity", "CommonStockEquity", "TotalEquityGrossMinorityInterest")
	result.QuarterlyRevenue = statementSeries(quarterlyIncome, "TotalRevenue", "OperatingRevenue")
	result.QuarterlyEPS = statementSeries(quarterlyIncome, "DilutedEPS", "BasicEPS")

	setMissing := func(destination **float64, source *float64) {
		if *destination == nil && source != nil {
			copy := *source
			*destination = &copy
		}
	}
	setStatement := func(destination **float64, source *float64) {
		if source != nil {
			copy := *source
			*destination = &copy
		}
	}
	setStatement(&result.Revenue, latestStatementValue(annualIncome, "TotalRevenue", "OperatingRevenue"))
	if trailingEPS := latestStatementValue(trailingIncome, "DilutedEPS", "BasicEPS"); trailingEPS != nil && *trailingEPS > 0 {
		setStatement(&result.EPS, trailingEPS)
	}
	setMissing(&result.OperatingIncome, latestStatementValue(annualIncome, "OperatingIncome"))
	setMissing(&result.NetIncome, latestStatementValue(annualIncome, "NetIncome", "NetIncomeCommonStockholders"))
	setMissing(&result.EBITDA, latestStatementValue(annualIncome, "EBITDA", "NormalizedEBITDA"))
	setMissing(&result.DepreciationAndAmortization, latestStatementValue(annualCashFlow,
		"DepreciationAmortizationDepletion", "Depreciation", "AmortizationOfIntangibles"))
	setMissing(&result.CapitalExpenditure, latestStatementValue(annualCashFlow, "CapitalExpenditure", "PurchaseOfPPE"))
	setMissing(&result.ChangeInWorkingCapital, latestStatementValue(annualCashFlow, "ChangeInWorkingCapital"))
	setMissing(&result.Buyback, latestStatementValue(annualCashFlow, "RepurchaseOfCapitalStock", "CommonStockPayments"))
	setStatement(&result.FreeCashflow, latestStatementValue(annualCashFlow, "FreeCashFlow"))
	setStatement(&result.OperatingCashflow, latestStatementValue(annualCashFlow, "OperatingCashFlow"))
	setStatement(&result.Debt, latestStatementValue(annualBalance, "TotalDebt", "NetDebt"))
	setStatement(&result.Cash, latestStatementValue(annualBalance,
		"CashCashEquivalentsAndShortTermInvestments", "CashAndCashEquivalents"))
	setMissing(&result.Equity, latestStatementValue(annualBalance,
		"StockholdersEquity", "CommonStockEquity", "TotalEquityGrossMinorityInterest"))
	if len(result.AnnualShares) > 0 {
		result.Shares = fundamentalsNumberPointer(result.AnnualShares[0])
		result.SharesSource = sharesSource
	}
	setMissing(&result.InterestExpense, latestStatementValue(annualIncome,
		"InterestExpenseNonOperating", "NetNonOperatingInterestIncomeExpense"))
	if result.FreeCashflow == nil && result.OperatingCashflow != nil && result.CapitalExpenditure != nil {
		derived := *result.OperatingCashflow + *result.CapitalExpenditure
		if finite(derived) {
			result.FreeCashflow = &derived
		}
	}

	if valuation := data.valuation; valuation != nil {
		if value, ok := valuation.FloatValue("Market Cap", "Current"); ok {
			setMissing(&result.MarketCap, &value)
		}
		if value, ok := valuation.FloatValue("Price/Sales", "Current"); ok {
			setMissing(&result.PriceToSales, &value)
		}
		if value, ok := valuation.FloatValue("Enterprise Value/EBITDA", "Current"); ok {
			setMissing(&result.EnterpriseToEBITDA, &value)
		}
	}
	result.HistoricalPrices = historyPricesByYear(data.history)
	return result
}

func fundamentalsFromSDKInfo(symbol string, info *ymodels.Info) YahooFundamentals {
	shares := sdkInteger(info.SharesOutstanding)
	return YahooFundamentals{
		Symbol: symbol, QuoteType: strings.ToUpper(strings.TrimSpace(info.QuoteType)),
		Price: firstSDKNumber(info.CurrentPrice, info.RegularMarketPreviousClose, info.PreviousClose),
		EPS:   sdkNumber(info.TrailingEps), ForwardEPS: sdkNumber(info.ForwardEps),
		FreeCashflow: sdkInteger(info.FreeCashflow), Debt: sdkInteger(info.TotalDebt), Cash: sdkInteger(info.TotalCash),
		Shares: shares, SharesSource: sharesSourceIfPresent(shares, "info"),
		Revenue: sdkInteger(info.TotalRevenue), Industry: strings.TrimSpace(info.Industry),
		Sector: strings.TrimSpace(info.Sector), LongBusinessSummary: strings.TrimSpace(info.LongBusinessSummary),
		OperatingCashflow: sdkInteger(info.OperatingCashflow), EBITDA: sdkInteger(info.Ebitda),
		Beta: sdkNumber(info.Beta), EarningsGrowth: sdkNumber(info.EarningsGrowth),
		EarningsQuarterlyGrowth: sdkNumber(info.EarningsQuarterlyGrowth), RevenueGrowth: sdkNumber(info.RevenueGrowth),
		MarketCap: sdkInteger(info.MarketCap), DividendRate: firstSDKNumber(info.DividendRate, info.TrailingAnnualDividendRate),
		BookValue: sdkNumber(info.BookValue), TargetMeanPrice: firstSDKNumber(info.TargetMeanPrice, info.TargetMedianPrice),
		TargetLowPrice: sdkNumber(info.TargetLowPrice), TargetHighPrice: sdkNumber(info.TargetHighPrice),
		PriceToSales: sdkNumber(info.PriceToSalesTrailing12Mo), EnterpriseToEBITDA: sdkNumber(info.EnterpriseToEbitda),
		ROE: sdkNumber(info.ReturnOnEquity), AnalystCount: sdkInt(info.NumberOfAnalystOpinions),
	}
}

func statementSeries(statement *ymodels.FinancialStatement, fields ...string) []float64 {
	if statement == nil {
		return nil
	}
	for _, field := range fields {
		items := statement.Data[field]
		if len(items) == 0 {
			continue
		}
		ordered := append([]ymodels.FinancialItem(nil), items...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].AsOfDate.After(ordered[j].AsOfDate) })
		values := make([]float64, 0, len(ordered))
		for _, item := range ordered {
			if isFiniteFloat(item.Value) {
				values = append(values, item.Value)
			}
		}
		if len(values) > 0 {
			return values
		}
	}
	return nil
}

func statementSharesSeries(statement *ymodels.FinancialStatement) []float64 {
	if statement == nil {
		return nil
	}
	field := "OrdinarySharesNumber"
	if _, ok := statement.Data[field]; !ok {
		field = "ShareIssued"
	}
	if _, ok := statement.Data[field]; !ok {
		return nil
	}
	return statementSeries(statement, field)
}

func latestStatementValue(statement *ymodels.FinancialStatement, fields ...string) *float64 {
	values := statementSeries(statement, fields...)
	if len(values) == 0 {
		return nil
	}
	value := values[0]
	return &value
}

func historyPricesByYear(bars []ymodels.Bar) []float64 {
	annual := make(map[int]struct {
		date  time.Time
		price float64
	})
	for _, bar := range bars {
		if bar.Date.IsZero() {
			continue
		}
		price := bar.AdjClose
		if !isFiniteFloat(price) || price <= 0 {
			price = bar.Close
		}
		if !isFiniteFloat(price) || price <= 0 {
			continue
		}
		year := bar.Date.UTC().Year()
		if current, ok := annual[year]; !ok || bar.Date.After(current.date) {
			annual[year] = struct {
				date  time.Time
				price float64
			}{date: bar.Date, price: price}
		}
	}
	years := make([]int, 0, len(annual))
	for year := range annual {
		years = append(years, year)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	prices := make([]float64, 0, len(years))
	for _, year := range years {
		prices = append(prices, annual[year].price)
	}
	return prices
}

func sdkNumber(value float64) *float64 {
	// go-yfinance exposes quoteSummary scalars without presence bits, so zero is
	// indistinguishable from an omitted Yahoo field. Preserve null for those
	// ambiguous values; statement items retain explicit zero values separately.
	if !isFiniteFloat(value) || value == 0 {
		return nil
	}
	return &value
}

func sdkPositiveNumber(value float64) *float64 {
	if !isFiniteFloat(value) || value <= 0 {
		return nil
	}
	return &value
}

func sdkInteger(value int64) *float64 {
	if value == 0 {
		return nil
	}
	number := float64(value)
	return &number
}

func sdkInt(value int) *float64 {
	if value == 0 {
		return nil
	}
	number := float64(value)
	return &number
}

func firstSDKNumber(values ...float64) *float64 {
	for _, value := range values {
		if number := sdkNumber(value); number != nil {
			return number
		}
	}
	return nil
}

func sdkEPSTrendRevision(entries []ymodels.EPSTrend) *float64 {
	rank := func(period string) int {
		switch strings.TrimSpace(period) {
		case "0q", "0y":
			return 0
		case "+1q", "+1y":
			return 1
		default:
			return 2
		}
	}
	ordered := append([]ymodels.EPSTrend(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i].Period) < rank(ordered[j].Period) })
	for _, entry := range ordered {
		// The SDK's scalar trend model uses zero for both absent and explicit values.
		if entry.Current == 0 || !isFiniteFloat(entry.Current) {
			continue
		}
		baseline := entry.ThirtyDays
		if baseline == 0 {
			baseline = entry.SevenDays
		}
		if !isFiniteFloat(baseline) || math.Abs(baseline) < 1e-9 {
			continue
		}
		revision := (entry.Current - baseline) / math.Abs(baseline)
		if finite(revision) {
			return &revision
		}
	}
	return nil
}

func hasSDKFundamentalData(value YahooFundamentals) bool {
	return value.Price != nil || value.EPS != nil || value.ForwardEPS != nil || value.FreeCashflow != nil ||
		value.Debt != nil || value.Cash != nil || value.Shares != nil || value.Revenue != nil ||
		value.OperatingCashflow != nil || value.EBITDA != nil || value.MarketCap != nil ||
		len(value.AnnualRevenue) > 0 || len(value.QuarterlyRevenue) > 0
}

func (c *FundamentalsClient) fetchCustomFundamentals(ctx context.Context, symbol string) (YahooFundamentals, error) {
	// go-yfinance v1.7.0 models the standard Info fields, but not IndustryKey,
	// the valuation-specific extension metrics, or quoteSummary share history
	// (OrdinarySharesNumber is absent from its BalanceSheetKeys). Fetch only
	// these extensions through the SDK auth manager.
	var payload yahooCustomQuoteSummaryResponseDTO
	target := endpointURL(c.QuoteSummaryURL, symbol, map[string]string{
		"modules": "assetProfile,defaultKeyStatistics,financialData,balanceSheetHistory",
	})
	if err := c.fetchCustomFundamentalsJSON(ctx, target, &payload); err != nil {
		return YahooFundamentals{}, err
	}
	if payload.QuoteSummary.Error != nil {
		return YahooFundamentals{}, payload.QuoteSummary.Error
	}
	if len(payload.QuoteSummary.Result) == 0 {
		return YahooFundamentals{}, errors.New("Yahoo custom fundamentals returned no result")
	}
	return customFundamentalsFromQuoteSummary(symbol, payload.QuoteSummary.Result[0]), nil
}

func (c *FundamentalsClient) fetchCustomFundamentalsJSON(ctx context.Context, target string, output any) error {
	if c.crumbAuth == nil {
		return c.fetchJSON(ctx, target, output)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("invalid Yahoo custom fundamentals URL")
	}
	query := parsed.Query()
	parsed.RawQuery = ""
	response, err := runYahooCrumbRequest(ctx, c.Gate, c.GateWait, c.crumbAuth, parsed.String(), query)
	if err != nil {
		return err
	}
	if response == nil {
		return errors.New("go-yfinance returned an empty Yahoo fundamentals response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Yahoo fundamentals returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(response.Body), 8<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Yahoo fundamentals response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("decode Yahoo fundamentals response: unexpected trailing JSON")
		}
		return fmt.Errorf("decode Yahoo fundamentals response: %w", err)
	}
	return nil
}

func customFundamentalsFromQuoteSummary(symbol string, value yahooCustomQuoteSummaryResultDTO) YahooFundamentals {
	result := YahooFundamentals{
		Symbol: symbol,
		RiskFreeRate: firstMetric(value.RiskFreeRate, value.FinancialData.RiskFreeRate,
			value.DefaultKeyStatistics.RiskFreeRate, value.TreasuryYield, value.FinancialData.TreasuryYield,
			value.DefaultKeyStatistics.TreasuryYield, value.US10Y, value.FinancialData.US10Y,
			value.DefaultKeyStatistics.US10Y),
		IndustryKey: firstNonBlank(value.AssetProfile.IndustryKey, value.AssetProfile.IndustryKeyAlias),
		CapexGrowth: firstMetric(value.FinancialData.AICapexGrowth, value.FinancialData.CapexGrowth,
			value.FinancialData.CapitalExpenditureGrowth),
		InventoryDays: firstMetric(value.FinancialData.DaysInventoryOutstanding),
		ForwardEBITDA: firstMetric(value.FinancialData.ForwardEBITDA, value.FinancialData.EBITDAForward),
		DefenseRevenueGrowth: firstMetric(value.FinancialData.DefenseRevenueGrowth,
			value.FinancialData.AAndDRevenueGrowth, value.FinancialData.AerospaceDefenseRevenueGrowth),
		FFOPerShare: firstMetric(value.DefaultKeyStatistics.FFOPerShare,
			value.DefaultKeyStatistics.FundsFromOperationsPerShare, value.DefaultKeyStatistics.NormalizedFFOPerShare,
			value.FinancialData.FFOPerShare, value.FinancialData.FundsFromOperationsPerShare,
			value.FinancialData.NormalizedFFOPerShare),
		AFFOPerShare: firstMetric(value.DefaultKeyStatistics.AFFOPerShare, value.DefaultKeyStatistics.AdjustedAFFOPerShare,
			value.FinancialData.AFFOPerShare, value.FinancialData.AdjustedAFFOPerShare),
		NAVPerShare: firstMetric(value.DefaultKeyStatistics.NAVPerShare, value.DefaultKeyStatistics.NetAssetValuePerShare,
			value.FinancialData.NAVPerShare, value.FinancialData.NetAssetValuePerShare),
		RABPerShare: firstMetric(value.DefaultKeyStatistics.RABPerShare,
			value.DefaultKeyStatistics.RegulatedAssetBasePerShare, value.FinancialData.RABPerShare,
			value.FinancialData.RegulatedAssetBasePerShare),
		EmbeddedValuePerShare: firstMetric(value.DefaultKeyStatistics.EmbeddedValuePerShare,
			value.DefaultKeyStatistics.EVPerShare, value.FinancialData.EmbeddedValuePerShare,
			value.FinancialData.EVPerShare, value.FinancialData.EmbeddedValue),
		NewBusinessValuePerShare: firstMetric(value.DefaultKeyStatistics.NewBusinessValuePerShare,
			value.DefaultKeyStatistics.NBVPerShare, value.FinancialData.NewBusinessValuePerShare,
			value.FinancialData.NBVPerShare, value.FinancialData.NewBusinessValue),
		PipelineRNPV: firstMetric(value.DefaultKeyStatistics.PipelineRNPV, value.DefaultKeyStatistics.RiskAdjustedNPV,
			value.FinancialData.PipelineRNPV, value.FinancialData.PipelineRnPV,
			value.FinancialData.RiskAdjustedNPV, value.FinancialData.RNPV),
		ApprovedDrugValue: firstMetric(value.DefaultKeyStatistics.ApprovedDrugValue,
			value.DefaultKeyStatistics.MarketedProductValue, value.DefaultKeyStatistics.ApprovedProductsValue,
			value.FinancialData.ApprovedDrugValue, value.FinancialData.MarketedProductValue,
			value.FinancialData.ApprovedProductsValue),
		PlatformUsers: firstMetric(value.FinancialData.MonthlyActiveUsers, value.FinancialData.ActiveUsers,
			value.FinancialData.Users, value.FinancialData.UserCount),
		ARPU:      firstMetric(value.FinancialData.ARPU, value.FinancialData.AverageRevenuePerUser),
		ChurnRate: firstMetric(value.FinancialData.ChurnRate, value.FinancialData.UserChurnRate),
		AIRevenueShare: firstMetric(value.FinancialData.AIRevenueShare, value.FinancialData.AIRevenuePercentage,
			value.FinancialData.ArtificialIntelligenceRevenueShare),
	}
	result.AnnualShares = yahooBalanceSheetShareSeries(value.BalanceSheetHistory.Statements)
	if len(result.AnnualShares) > 0 {
		result.Shares = fundamentalsNumberPointer(result.AnnualShares[0])
		result.SharesSource = "balance_sheet"
	}
	return result
}

func mergeCustomFundamentals(destination *YahooFundamentals, source YahooFundamentals) {
	merge := func(target **float64, value *float64) {
		if *target == nil && value != nil {
			copy := *value
			*target = &copy
		}
	}
	if destination.IndustryKey == "" {
		destination.IndustryKey = source.IndustryKey
	}
	if destination.QuoteType == "" {
		destination.QuoteType = source.QuoteType
	}
	if source.SharesSource == "balance_sheet" && source.Shares != nil && destination.SharesSource != "balance_sheet" {
		destination.Shares = fundamentalsNumberPointer(*source.Shares)
		destination.SharesSource = source.SharesSource
		destination.AnnualShares = append([]float64(nil), source.AnnualShares...)
	}
	merge(&destination.RiskFreeRate, source.RiskFreeRate)
	merge(&destination.CapexGrowth, source.CapexGrowth)
	merge(&destination.InventoryDays, source.InventoryDays)
	merge(&destination.ForwardEBITDA, source.ForwardEBITDA)
	merge(&destination.DefenseRevenueGrowth, source.DefenseRevenueGrowth)
	merge(&destination.FFOPerShare, source.FFOPerShare)
	merge(&destination.AFFOPerShare, source.AFFOPerShare)
	merge(&destination.NAVPerShare, source.NAVPerShare)
	merge(&destination.RABPerShare, source.RABPerShare)
	merge(&destination.EmbeddedValuePerShare, source.EmbeddedValuePerShare)
	merge(&destination.NewBusinessValuePerShare, source.NewBusinessValuePerShare)
	merge(&destination.PipelineRNPV, source.PipelineRNPV)
	merge(&destination.ApprovedDrugValue, source.ApprovedDrugValue)
	merge(&destination.PlatformUsers, source.PlatformUsers)
	merge(&destination.ARPU, source.ARPU)
	merge(&destination.ChurnRate, source.ChurnRate)
	merge(&destination.AIRevenueShare, source.AIRevenueShare)
}

func applyHistoricalFundamentals(result *YahooFundamentals, prices []float64, spot *float64) {
	result.HistoricalPrices = prices
	if result.Price == nil {
		result.Price = spot
	}
}

func (c *FundamentalsClient) canOverlapFundamentalsRequests() bool {
	// A missing gate is used by direct tests and deliberately stays serial. In
	// production the shared gate carries the configured provider concurrency.
	return c != nil && c.Gate != nil && cap(c.Gate) > 1
}

type fundamentalsFetchResult struct {
	value YahooFundamentals
	err   error
}

type historicalFundamentalsResult struct {
	prices []float64
	spot   *float64
	err    error
}

func (c *FundamentalsClient) fetchTimeseriesAndHistory(ctx context.Context, symbol string) (fundamentalsFetchResult, historicalFundamentalsResult) {
	var requests sync.WaitGroup
	requests.Add(2)
	var series fundamentalsFetchResult
	var history historicalFundamentalsResult
	go func() {
		defer requests.Done()
		series.value, series.err = c.fetchTimeseries(ctx, symbol)
	}()
	go func() {
		defer requests.Done()
		history.prices, history.spot, history.err = c.fetchHistoricalData(ctx, symbol)
	}()
	requests.Wait()
	return series, history
}

func (c *FundamentalsClient) fetchTimeseriesAfterQuoteSummaryFailure(ctx context.Context, symbol string, cause error) (YahooFundamentals, error) {
	if c.TimeseriesURL == "" {
		return YahooFundamentals{}, cause
	}
	result, err := c.fetchTimeseries(ctx, symbol)
	if err != nil {
		if transientFundamentalsError(cause) || transientFundamentalsError(err) {
			return YahooFundamentals{
				Symbol: symbol, TimeseriesRetryable: true,
				ValuationUpstreamError: true, ValuationUpstreamRetryable: true,
			}, nil
		}
		return YahooFundamentals{}, cause
	}
	recordValuationUpstreamError(&result, cause)
	if c.HistoricalURL != "" {
		if prices, spot, historyErr := c.fetchHistoricalData(ctx, symbol); historyErr == nil {
			result.HistoricalPrices = prices
			if result.Price == nil {
				result.Price = spot
			}
		} else {
			recordValuationUpstreamError(&result, historyErr)
		}
	}
	result.TimeseriesRetryable = transientFundamentalsError(cause)
	return result, nil
}

func recordValuationUpstreamError(result *YahooFundamentals, err error) {
	if result == nil || err == nil {
		return
	}
	result.ValuationUpstreamError = true
	if transientFundamentalsError(err) {
		result.ValuationUpstreamRetryable = true
	}
}

func transientFundamentalsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, status := range []string{"408", "425", "500", "502", "503", "504"} {
		if strings.Contains(text, "http "+status) || strings.Contains(text, "status "+status) {
			return true
		}
	}
	for _, marker := range []string{
		"429", "403", "too many requests", "rate limit", "timed out", "timeout",
		"temporarily unavailable", "service unavailable", "connection reset", "connection aborted",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (c *FundamentalsClient) fetchHistoricalData(ctx context.Context, symbol string) ([]float64, *float64, error) {
	var payload chartEnvelope
	if err := c.fetchJSON(ctx, endpointURL(c.HistoricalURL, symbol, map[string]string{
		"range": "6y", "interval": "1mo", "events": "div,splits", "includeAdjustedClose": "true",
	}), &payload); err != nil {
		return nil, nil, err
	}
	if payload.Chart.Error != nil {
		return nil, nil, payload.Chart.Error
	}
	if len(payload.Chart.Result) == 0 {
		return nil, nil, errors.New("Yahoo chart returned no historical prices")
	}
	result := payload.Chart.Result[0]
	var spot *float64
	if isFiniteFloat(result.Meta.RegularMarketPrice) && result.Meta.RegularMarketPrice > 0 {
		spot = &result.Meta.RegularMarketPrice
	}
	if len(result.Indicators.Quote) == 0 {
		return nil, spot, nil
	}
	closes := result.Indicators.Quote[0].Close
	var adjustedCloses []*float64
	if len(result.Indicators.Adjusted) > 0 {
		adjustedCloses = result.Indicators.Adjusted[0].Close
	}
	annual := make(map[int]float64)
	for index, timestamp := range result.Timestamp {
		if timestamp == nil || index >= len(closes) || closes[index] == nil || !isFiniteFloat(*closes[index]) || *closes[index] <= 0 {
			continue
		}
		year := time.Unix(*timestamp, 0).UTC().Year()
		price := *closes[index]
		if index < len(adjustedCloses) && adjustedCloses[index] != nil && isFiniteFloat(*adjustedCloses[index]) && *adjustedCloses[index] > 0 {
			price = *adjustedCloses[index]
		}
		annual[year] = price
	}
	years := make([]int, 0, len(annual))
	for year := range annual {
		years = append(years, year)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	prices := make([]float64, 0, len(years))
	for _, year := range years {
		prices = append(prices, annual[year])
	}
	return prices, spot, nil
}

func isFiniteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (c *FundamentalsClient) fetchTimeseries(ctx context.Context, symbol string) (YahooFundamentals, error) {
	metrics := "annualFreeCashFlow,annualOperatingCashFlow,annualCapitalExpenditure,annualDilutedAverageShares,annualDilutedEPS,annualTotalDebt,annualCashCashEquivalentsAndShortTermInvestments,annualTotalRevenue,annualOperatingIncome,annualNetIncome,annualDepreciationAndAmortization,annualChangeInWorkingCapital,annualRepurchaseOfCapitalStock,annualStockholdersEquity,quarterlyTotalRevenue,quarterlyDilutedEPS,trailingDilutedEPS"
	return c.fetchTimeseriesWithMetrics(ctx, symbol, metrics)
}

func (c *FundamentalsClient) fetchTimeseriesWithMetrics(ctx context.Context, symbol, metrics string) (YahooFundamentals, error) {
	var payload YahooTimeseriesResponseDTO
	query := map[string]string{
		"symbol": symbol, "type": metrics,
		"period1": "1262304000", "period2": strconv.FormatInt(time.Now().Unix()+86400, 10),
	}
	target := endpointURL(c.TimeseriesURL, symbol, query)
	var err error
	if c.crumbAuth != nil {
		err = c.fetchCustomFundamentalsJSON(ctx, target, &payload)
	} else {
		err = c.fetchJSON(ctx, target, &payload)
	}
	if err != nil {
		return YahooFundamentals{}, err
	}
	if payload.Timeseries.Error != nil {
		return YahooFundamentals{}, payload.Timeseries.Error
	}
	return fundamentalsFromTimeseries(symbol, payload.Timeseries.Result), nil
}

func (c *FundamentalsClient) fetchJSON(ctx context.Context, target string, output any) error {
	release, err := acquireGateTimeout(ctx, c.Gate, c.GateWait)
	if err != nil {
		return err
	}
	defer release()
	request, err := outboundhttp.NewRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build Yahoo fundamentals request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "option-scope-go/0.1")
	response, err := outboundhttp.Do(c.Client, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Yahoo fundamentals returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Yahoo fundamentals response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("decode Yahoo fundamentals response: unexpected trailing JSON")
		}
		return fmt.Errorf("decode Yahoo fundamentals response: %w", err)
	}
	return nil
}

func endpointURL(template, symbol string, values map[string]string) string {
	if strings.TrimSpace(template) == "" {
		return ""
	}
	encoded := url.PathEscape(symbol)
	if strings.Contains(template, "%s") {
		template = fmt.Sprintf(template, encoded)
	}
	parsed, err := url.Parse(template)
	if err != nil {
		return template
	}
	query := parsed.Query()
	for key, value := range values {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if normalized := strings.TrimSpace(value); normalized != "" {
			return normalized
		}
	}
	return ""
}

func earningsTrendRevision(entries []YahooEarningsTrendEntryDTO) *float64 {
	// Yahoo exposes analyst EPS snapshots for several horizons. Prefer the
	// current quarter, then the next quarter, and compare the current estimate
	// with the one-month-old estimate. A relative change based on the absolute
	// prior estimate also behaves sensibly when consensus EPS is negative.
	rank := func(period string) int {
		switch strings.TrimSpace(period) {
		case "0q", "0y":
			return 0
		case "+1q", "+1y":
			return 1
		default:
			return 2
		}
	}
	ordered := append([]YahooEarningsTrendEntryDTO(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i].Period) < rank(ordered[j].Period) })
	for _, entry := range ordered {
		current := metricNumber(entry.EPSTrend.Current)
		if current == nil {
			continue
		}
		baseline := metricNumber(entry.EPSTrend.ThirtyDaysAgo)
		if baseline == nil {
			baseline = metricNumber(entry.EPSTrend.SevenDaysAgo)
		}
		if baseline == nil || math.Abs(*baseline) < 1e-9 {
			continue
		}
		revision := (*current - *baseline) / math.Abs(*baseline)
		if finite(revision) {
			return &revision
		}
	}
	return nil
}

func fundamentalsFromQuoteSummary(symbol string, value YahooQuoteSummaryResultDTO) YahooFundamentals {
	shares := firstMetric(value.DefaultKeyStatistics.SharesOutstanding, value.SummaryDetail.SharesOutstanding)
	return YahooFundamentals{
		Symbol: symbol, QuoteType: strings.ToUpper(strings.TrimSpace(value.QuoteType.QuoteType)),
		RiskFreeRate: firstMetric(
			value.RiskFreeRate, value.FinancialData.RiskFreeRate, value.DefaultKeyStatistics.RiskFreeRate, value.SummaryDetail.RiskFreeRate,
			value.TreasuryYield, value.FinancialData.TreasuryYield, value.DefaultKeyStatistics.TreasuryYield, value.SummaryDetail.TreasuryYield,
			value.US10Y, value.FinancialData.US10Y, value.DefaultKeyStatistics.US10Y, value.SummaryDetail.US10Y,
		),
		Price:                   firstMetric(value.Price.RegularMarketPrice, value.FinancialData.CurrentPrice, value.Price.PreviousClose),
		EPS:                     firstMetric(value.DefaultKeyStatistics.TrailingEPS),
		ForwardEPS:              firstMetric(value.DefaultKeyStatistics.ForwardEPS, value.FinancialData.ForwardEPS, value.FinancialData.EPSForward),
		FreeCashflow:            firstMetric(value.DefaultKeyStatistics.FreeCashflow, value.FinancialData.FreeCashflow),
		Debt:                    firstMetric(value.FinancialData.TotalDebt),
		Cash:                    firstMetric(value.FinancialData.TotalCash),
		Shares:                  shares,
		SharesSource:            sharesSourceIfPresent(shares, "info"),
		Revenue:                 firstMetric(value.FinancialData.TotalRevenue, value.FinancialData.TrailingAnnualRevenue, value.FinancialData.Revenue, value.FinancialData.TotalRevenueTTM),
		Industry:                firstNonBlank(value.SummaryProfile.Industry, value.AssetProfile.Industry),
		IndustryKey:             firstNonBlank(value.AssetProfile.IndustryKey, value.AssetProfile.IndustryKeyAlias),
		Sector:                  firstNonBlank(value.SummaryProfile.Sector, value.AssetProfile.Sector),
		LongBusinessSummary:     strings.TrimSpace(value.AssetProfile.LongBusinessSummary),
		OperatingCashflow:       firstMetric(value.FinancialData.OperatingCashflow),
		EBITDA:                  firstMetric(value.FinancialData.Ebitda),
		Beta:                    firstMetric(value.SummaryDetail.Beta, value.SummaryDetail.Beta3Year),
		EarningsGrowth:          firstMetric(value.FinancialData.EarningsGrowth),
		EarningsQuarterlyGrowth: firstMetric(value.FinancialData.EarningsQuarterlyGrowth),
		RevenueGrowth:           firstMetric(value.FinancialData.RevenueGrowth),
		CapexGrowth: firstMetric(value.FinancialData.AICapexGrowth, value.FinancialData.CapexGrowth,
			value.FinancialData.CapitalExpenditureGrowth),
		EPSRevision:   earningsTrendRevision(value.EarningsTrend.Trend),
		InventoryDays: firstMetric(value.FinancialData.DaysInventoryOutstanding),
		MarketCap: firstMetric(value.Price.MarketCap, value.DefaultKeyStatistics.MarketCap, value.FinancialData.MarketCap,
			value.SummaryDetail.MarketCap),
		DividendRate: firstMetric(value.FinancialData.DividendRate, value.FinancialData.TrailingAnnualDividendRate,
			value.SummaryDetail.DividendRate, value.SummaryDetail.TrailingAnnualDividendRate),
		BookValue: firstMetric(value.DefaultKeyStatistics.BookValue, value.FinancialData.BookValue,
			value.SummaryDetail.BookValue),
		TargetMeanPrice: firstMetric(value.FinancialData.TargetMeanPrice, value.FinancialData.TargetMedianPrice),
		TargetLowPrice:  firstMetric(value.FinancialData.TargetLowPrice),
		TargetHighPrice: firstMetric(value.FinancialData.TargetHighPrice),
		PriceToSales: firstMetric(value.DefaultKeyStatistics.PriceToSales, value.FinancialData.PriceToSales,
			value.FinancialData.PriceToSalesAlias, value.SummaryDetail.PriceToSales),
		EnterpriseToEBITDA: firstMetric(value.DefaultKeyStatistics.EnterpriseToEBITDA,
			value.FinancialData.EnterpriseToEBITDA, value.FinancialData.EnterpriseToEBITDAForward,
			value.SummaryDetail.EnterpriseToEBITDA),
		ForwardEBITDA: firstMetric(value.DefaultKeyStatistics.ForwardEBITDA,
			value.FinancialData.ForwardEBITDA, value.FinancialData.EBITDAForward),
		InterestExpense: firstMetric(value.FinancialData.InterestExpense, value.FinancialData.InterestExpenseToRevenue,
			value.FinancialData.InterestExpenseNonOperating),
		ROE: firstMetric(value.FinancialData.ROE, value.FinancialData.ROEAlias,
			value.FinancialData.ReturnOnAverageEquity),
		DefenseRevenueGrowth: firstMetric(value.FinancialData.DefenseRevenueGrowth,
			value.FinancialData.AAndDRevenueGrowth, value.FinancialData.AerospaceDefenseRevenueGrowth),
		FFOPerShare: firstMetric(value.DefaultKeyStatistics.FFOPerShare,
			value.DefaultKeyStatistics.FundsFromOperationsPerShare, value.DefaultKeyStatistics.NormalizedFFOPerShare,
			value.FinancialData.FFOPerShare, value.FinancialData.FundsFromOperationsPerShare, value.FinancialData.NormalizedFFOPerShare),
		AFFOPerShare: firstMetric(value.DefaultKeyStatistics.AFFOPerShare, value.DefaultKeyStatistics.AdjustedAFFOPerShare,
			value.FinancialData.AFFOPerShare, value.FinancialData.AdjustedAFFOPerShare),
		NAVPerShare: firstMetric(value.DefaultKeyStatistics.NAVPerShare, value.DefaultKeyStatistics.NetAssetValuePerShare,
			value.FinancialData.NAVPerShare, value.FinancialData.NetAssetValuePerShare),
		RABPerShare: firstMetric(value.DefaultKeyStatistics.RABPerShare, value.DefaultKeyStatistics.RegulatedAssetBasePerShare,
			value.FinancialData.RABPerShare, value.FinancialData.RegulatedAssetBasePerShare),
		EmbeddedValuePerShare: firstMetric(value.DefaultKeyStatistics.EmbeddedValuePerShare,
			value.DefaultKeyStatistics.EVPerShare, value.FinancialData.EmbeddedValuePerShare,
			value.FinancialData.EVPerShare, value.FinancialData.EmbeddedValue),
		NewBusinessValuePerShare: firstMetric(value.DefaultKeyStatistics.NewBusinessValuePerShare,
			value.DefaultKeyStatistics.NBVPerShare, value.FinancialData.NewBusinessValuePerShare,
			value.FinancialData.NBVPerShare, value.FinancialData.NewBusinessValue),
		PipelineRNPV: firstMetric(value.DefaultKeyStatistics.PipelineRNPV, value.DefaultKeyStatistics.RiskAdjustedNPV,
			value.FinancialData.PipelineRNPV, value.FinancialData.PipelineRnPV, value.FinancialData.RiskAdjustedNPV, value.FinancialData.RNPV),
		ApprovedDrugValue: firstMetric(value.DefaultKeyStatistics.ApprovedDrugValue,
			value.DefaultKeyStatistics.MarketedProductValue, value.DefaultKeyStatistics.ApprovedProductsValue,
			value.FinancialData.ApprovedDrugValue, value.FinancialData.MarketedProductValue, value.FinancialData.ApprovedProductsValue),
		PlatformUsers: firstMetric(value.FinancialData.MonthlyActiveUsers, value.FinancialData.ActiveUsers,
			value.FinancialData.Users, value.FinancialData.UserCount),
		ARPU:      firstMetric(value.FinancialData.ARPU, value.FinancialData.AverageRevenuePerUser),
		ChurnRate: firstMetric(value.FinancialData.ChurnRate, value.FinancialData.UserChurnRate),
		AIRevenueShare: firstMetric(value.FinancialData.AIRevenueShare, value.FinancialData.AIRevenuePercentage,
			value.FinancialData.ArtificialIntelligenceRevenueShare),
		AnalystCount: firstMetric(value.FinancialData.NumberOfAnalystOpinions,
			value.FinancialData.AnalystCount, value.FinancialData.AnalystCoverage),
	}
}

func fundamentalsFromTimeseries(symbol string, results []YahooTimeseriesResultDTO) YahooFundamentals {
	results = normalizeTimeseriesResults(results)
	freeCashflow := latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualFreeCashflow })
	operatingCashflow := latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualOperatingCF })
	eps := latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.TrailingDilutedEPS })
	shares := latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualDilutedShares })
	if eps != nil && *eps <= 0 {
		eps = nil
	}
	if freeCashflow == nil {
		freeCashflow = latestAnnualFCFFromComponents(results)
	}
	return YahooFundamentals{
		Symbol:            symbol,
		EPS:               eps,
		FreeCashflow:      freeCashflow,
		OperatingCashflow: operatingCashflow,
		Debt:              latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualDebt }),
		Cash:              latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualCash }),
		Shares:            shares,
		SharesSource:      sharesSourceIfPresent(shares, "fundamentals_timeseries"),
		Revenue:           latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualRevenue }),
		OperatingIncome:   latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualOperatingIncome }),
		NetIncome:         latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualNetIncome }),
		DepreciationAndAmortization: latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO {
			return result.AnnualDepreciationAmortization
		}),
		CapitalExpenditure:       latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualCapitalExpend }),
		ChangeInWorkingCapital:   latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualWorkingCapital }),
		Buyback:                  latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualBuyback }),
		Equity:                   latestSeriesValue(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualEquity }),
		QuarterlyRevenue:         seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.QuarterlyRevenue }),
		QuarterlyEPS:             seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.QuarterlyDilutedEPS }),
		AnnualRevenue:            seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualRevenue }),
		AnnualEPS:                seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualDilutedEPS }),
		AnnualNetIncome:          seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualNetIncome }),
		AnnualCapitalExpenditure: seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualCapitalExpend }),
		AnnualDebt:               seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualDebt }),
		AnnualCash:               seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualCash }),
		AnnualShares:             seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualDilutedShares }),
		AnnualOperatingIncome:    seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualOperatingIncome }),
		AnnualDepreciationAndAmortization: seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO {
			return result.AnnualDepreciationAmortization
		}),
		AnnualEquity: seriesValues(results, func(result YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO { return result.AnnualEquity }),
	}
}

func sharesSourceIfPresent(shares *float64, source string) string {
	if shares == nil {
		return ""
	}
	return source
}

func mergeStatementFundamentals(destination *YahooFundamentals, source YahooQuoteSummaryResultDTO) {
	cashflowRows := append([]YahooCashflowStatementDTO(nil), source.CashflowStatementHistory.Statements...)
	sort.SliceStable(cashflowRows, func(i, j int) bool {
		left, right := statementDateKey(cashflowRows[i].EndDate), statementDateKey(cashflowRows[j].EndDate)
		return left != "" && (right == "" || left > right)
	})
	var freeCashflow, operatingCashflow, capitalExpenditure, interestExpense []float64
	for _, row := range cashflowRows {
		fcf := firstMetric(row.FreeCashflow)
		operating := firstMetric(row.OperatingCashflow, row.TotalCashFromOperatingActivities)
		capex := firstMetric(row.CapitalExpenditure, row.CapitalExpenditureAlias)
		if fcf == nil && operating != nil && capex != nil {
			value := *operating + *capex
			if isFiniteFloat(value) {
				fcf = &value
			}
		}
		if fcf != nil {
			freeCashflow = append(freeCashflow, *fcf)
		}
		if operating != nil {
			operatingCashflow = append(operatingCashflow, *operating)
		}
		if capex != nil {
			capitalExpenditure = append(capitalExpenditure, *capex)
		}
		if value := firstMetric(row.InterestExpenseNonOperating, row.InterestExpense, row.NetNonOperatingInterestIncomeExpense); value != nil {
			interestExpense = append(interestExpense, *value)
		}
	}
	if len(freeCashflow) > 0 {
		destination.FreeCashflow = fundamentalsNumberPointer(freeCashflow[0])
	}
	if len(operatingCashflow) > 0 {
		destination.OperatingCashflow = fundamentalsNumberPointer(operatingCashflow[0])
	}
	if len(capitalExpenditure) > 0 {
		destination.CapitalExpenditure = fundamentalsNumberPointer(capitalExpenditure[0])
		destination.AnnualCapitalExpenditure = capitalExpenditure
	}
	if len(interestExpense) > 0 {
		destination.InterestExpense = fundamentalsNumberPointer(interestExpense[0])
	}

	balanceRows := append([]YahooBalanceSheetStatementDTO(nil), source.BalanceSheetHistory.Statements...)
	sort.SliceStable(balanceRows, func(i, j int) bool {
		left, right := statementDateKey(balanceRows[i].EndDate), statementDateKey(balanceRows[j].EndDate)
		return left != "" && (right == "" || left > right)
	})
	var debt, cash, equity []float64
	for _, row := range balanceRows {
		if value := firstMetric(row.TotalDebt, row.TotalDebtAndCapitalLeaseObligation); value != nil {
			debt = append(debt, *value)
		}
		if value := firstMetric(row.CashCashEquivalentsAndShortTermInvestments, row.CashAndCashEquivalents, row.CashFinancial); value != nil {
			cash = append(cash, *value)
		}
		if value := firstMetric(row.StockholdersEquity, row.TotalStockholderEquity, row.CommonStockEquity); value != nil {
			equity = append(equity, *value)
		}
	}
	if len(debt) > 0 {
		destination.Debt = fundamentalsNumberPointer(debt[0])
		destination.AnnualDebt = debt
	}
	if len(cash) > 0 {
		destination.Cash = fundamentalsNumberPointer(cash[0])
		destination.AnnualCash = cash
	}
	shares := balanceSheetShareSeries(balanceRows)
	if len(shares) > 0 {
		destination.Shares = fundamentalsNumberPointer(shares[0])
		destination.SharesSource = "balance_sheet"
		destination.AnnualShares = shares
	}
	if len(equity) > 0 {
		destination.Equity = fundamentalsNumberPointer(equity[0])
		destination.AnnualEquity = equity
	}
}

func balanceSheetShareSeries(rows []YahooBalanceSheetStatementDTO) []float64 {
	shares := make([]yahooBalanceSheetShareStatementDTO, 0, len(rows))
	for _, row := range rows {
		shares = append(shares, yahooBalanceSheetShareStatementDTO{
			EndDate: row.EndDate, OrdinarySharesNumber: row.OrdinarySharesNumber, ShareIssued: row.ShareIssued,
		})
	}
	return yahooBalanceSheetShareSeries(shares)
}

func yahooBalanceSheetShareSeries(rows []yahooBalanceSheetShareStatementDTO) []float64 {
	ordered := append([]yahooBalanceSheetShareStatementDTO(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := statementDateKey(ordered[i].EndDate), statementDateKey(ordered[j].EndDate)
		return left != "" && (right == "" || left > right)
	})

	// Python selects one row label for the full statement before reading dates.
	field := "ordinary"
	for _, row := range ordered {
		if len(row.OrdinarySharesNumber.Raw) > 0 {
			field = "ordinary"
			break
		}
		if len(row.ShareIssued.Raw) > 0 {
			field = "issued"
		}
	}
	values := make([]float64, 0, len(ordered))
	for _, row := range ordered {
		metric := row.OrdinarySharesNumber
		if field == "issued" {
			metric = row.ShareIssued
		}
		if value := metricNumber(metric); value != nil {
			values = append(values, *value)
		}
	}
	return values
}

func fundamentalsNumberPointer(value float64) *float64 { return &value }

func statementDateKey(value YahooStatementDateDTO) string {
	parse := func(text string) string {
		text = strings.TrimSpace(text)
		for _, layout := range []string{"2006-01-02", "1/2/2006", "01/02/2006", "Jan 2, 2006"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed.UTC().Format("2006-01-02")
			}
		}
		return ""
	}
	if date := parse(value.Fmt); date != "" {
		return date
	}
	if raw := rawFiniteNumber(value.Raw); raw != nil && *raw > 0 {
		return time.Unix(int64(*raw), 0).UTC().Format("2006-01-02")
	}
	var text string
	if json.Unmarshal(value.Raw, &text) == nil {
		return parse(text)
	}
	return ""
}

// normalizeTimeseriesResults handles the two Yahoo response shapes: dated
// entries (asOfDate on each row) and result-level timestamps paired by index.
// All metric arrays are copied and sorted newest first when timestamps are
// available. Missing dates are filled from the timestamp for consumers that
// match annual components by date.
func normalizeTimeseriesResults(results []YahooTimeseriesResultDTO) []YahooTimeseriesResultDTO {
	if len(results) == 0 {
		return results
	}
	copyResults := append([]YahooTimeseriesResultDTO(nil), results...)
	for resultIndex := range copyResults {
		result := &copyResults[resultIndex]
		if len(result.Timestamp) == 0 {
			continue
		}
		order := make([]int, len(result.Timestamp))
		for index := range order {
			order[index] = index
		}
		sort.SliceStable(order, func(i, j int) bool {
			left, right := order[i], order[j]
			if result.Timestamp[left] == result.Timestamp[right] {
				// Python sorts (timestamp, original index) tuples in reverse order.
				return left > right
			}
			return result.Timestamp[left] > result.Timestamp[right]
		})
		dateFor := func(index int) string {
			if index < 0 || index >= len(result.Timestamp) || result.Timestamp[index] <= 0 {
				return ""
			}
			return time.Unix(result.Timestamp[index], 0).UTC().Format("2006-01-02")
		}
		reorder := func(entries []YahooTimeseriesEntryDTO) []YahooTimeseriesEntryDTO {
			if len(entries) != len(result.Timestamp) {
				return entries
			}
			ordered := make([]YahooTimeseriesEntryDTO, len(entries))
			for outputIndex, inputIndex := range order {
				ordered[outputIndex] = entries[inputIndex]
				if strings.TrimSpace(ordered[outputIndex].AsOfDate) == "" {
					ordered[outputIndex].AsOfDate = dateFor(inputIndex)
				}
			}
			return ordered
		}
		result.AnnualFreeCashflow = reorder(result.AnnualFreeCashflow)
		result.AnnualOperatingCF = reorder(result.AnnualOperatingCF)
		result.AnnualCapitalExpend = reorder(result.AnnualCapitalExpend)
		result.AnnualDilutedEPS = reorder(result.AnnualDilutedEPS)
		result.AnnualDilutedShares = reorder(result.AnnualDilutedShares)
		result.AnnualDebt = reorder(result.AnnualDebt)
		result.AnnualCash = reorder(result.AnnualCash)
		result.AnnualRevenue = reorder(result.AnnualRevenue)
		result.AnnualOperatingIncome = reorder(result.AnnualOperatingIncome)
		result.AnnualNetIncome = reorder(result.AnnualNetIncome)
		result.AnnualDepreciationAmortization = reorder(result.AnnualDepreciationAmortization)
		result.AnnualWorkingCapital = reorder(result.AnnualWorkingCapital)
		result.AnnualBuyback = reorder(result.AnnualBuyback)
		result.AnnualEquity = reorder(result.AnnualEquity)
		result.QuarterlyRevenue = reorder(result.QuarterlyRevenue)
		result.QuarterlyDilutedEPS = reorder(result.QuarterlyDilutedEPS)
		result.TrailingDilutedEPS = reorder(result.TrailingDilutedEPS)
	}
	return copyResults
}

func latestAnnualFCFFromComponents(results []YahooTimeseriesResultDTO) *float64 {
	operating := make(map[string]float64)
	capital := make(map[string]float64)
	for _, result := range results {
		for _, entry := range result.AnnualOperatingCF {
			if value := metricNumber(entry.ReportedValue); value != nil {
				operating[strings.TrimSpace(entry.AsOfDate)] = *value
			}
		}
		for _, entry := range result.AnnualCapitalExpend {
			if value := metricNumber(entry.ReportedValue); value != nil {
				capital[strings.TrimSpace(entry.AsOfDate)] = *value
			}
		}
	}
	latestDate := ""
	var freeCashflow *float64
	for date, cashflow := range operating {
		capex, ok := capital[date]
		if !ok || date == "" || date < latestDate {
			continue
		}
		derived := cashflow + capex
		if !finite(derived) {
			continue
		}
		latestDate, freeCashflow = date, &derived
	}
	return freeCashflow
}

func seriesValues(results []YahooTimeseriesResultDTO, selector func(YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO) []float64 {
	entries := make([]YahooTimeseriesEntryDTO, 0)
	for _, result := range results {
		entries = append(entries, selector(result)...)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].AsOfDate > entries[j].AsOfDate })
	values := make([]float64, 0, len(entries))
	for _, entry := range entries {
		if value := metricNumber(entry.ReportedValue); value != nil {
			values = append(values, *value)
		}
	}
	return values
}

func latestSeriesValue(results []YahooTimeseriesResultDTO, selectors ...func(YahooTimeseriesResultDTO) []YahooTimeseriesEntryDTO) *float64 {
	var selected *float64
	latestDate := ""
	for _, result := range results {
		for _, selector := range selectors {
			for _, entry := range selector(result) {
				value := metricNumber(entry.ReportedValue)
				if value == nil {
					continue
				}
				date := strings.TrimSpace(entry.AsOfDate)
				if selected == nil || (date != "" && (latestDate == "" || date > latestDate)) {
					copy := *value
					selected, latestDate = &copy, date
				}
			}
		}
	}
	return selected
}

func mergeMissingFundamentals(destination *YahooFundamentals, source YahooFundamentals) {
	// Python valuation reads dated annual series before quoteSummary/info
	// fallbacks for these inputs; retain the snapshot only when Yahoo omits a
	// corresponding timeseries value.
	// Python uses the timeseries EPS only when it is positive; otherwise it falls
	// back to quoteSummary trailingEps instead of replacing a usable TTM value.
	if source.EPS != nil && *source.EPS > 0 {
		destination.EPS = source.EPS
	}
	if source.QuoteType != "" {
		destination.QuoteType = source.QuoteType
	}
	if destination.QuoteType == "" {
		destination.QuoteType = source.QuoteType
	}
	if source.FreeCashflow != nil {
		destination.FreeCashflow = source.FreeCashflow
	}
	if source.Debt != nil {
		destination.Debt = source.Debt
	}
	if source.Cash != nil {
		destination.Cash = source.Cash
	}
	if source.Shares != nil {
		destination.Shares = source.Shares
		destination.SharesSource = source.SharesSource
	}
	if destination.InterestExpense == nil {
		destination.InterestExpense = source.InterestExpense
	}
	if source.Revenue != nil {
		destination.Revenue = source.Revenue
	}
	if destination.OperatingIncome == nil {
		destination.OperatingIncome = source.OperatingIncome
	}
	if destination.NetIncome == nil {
		destination.NetIncome = source.NetIncome
	}
	if destination.DepreciationAndAmortization == nil {
		destination.DepreciationAndAmortization = source.DepreciationAndAmortization
	}
	if destination.CapitalExpenditure == nil {
		destination.CapitalExpenditure = source.CapitalExpenditure
	}
	if destination.ChangeInWorkingCapital == nil {
		destination.ChangeInWorkingCapital = source.ChangeInWorkingCapital
	}
	if destination.Buyback == nil {
		destination.Buyback = source.Buyback
	}
	if destination.Equity == nil {
		destination.Equity = source.Equity
	}
	if len(destination.QuarterlyRevenue) == 0 {
		destination.QuarterlyRevenue = source.QuarterlyRevenue
	}
	if len(destination.QuarterlyEPS) == 0 {
		destination.QuarterlyEPS = source.QuarterlyEPS
	}
	if len(destination.AnnualRevenue) == 0 {
		destination.AnnualRevenue = source.AnnualRevenue
	}
	if len(destination.AnnualEPS) == 0 {
		destination.AnnualEPS = source.AnnualEPS
	}
	if len(destination.AnnualNetIncome) == 0 {
		destination.AnnualNetIncome = source.AnnualNetIncome
	}
	if len(destination.AnnualCapitalExpenditure) == 0 {
		destination.AnnualCapitalExpenditure = source.AnnualCapitalExpenditure
	}
	if len(destination.AnnualDebt) == 0 {
		destination.AnnualDebt = source.AnnualDebt
	}
	if len(destination.AnnualCash) == 0 {
		destination.AnnualCash = source.AnnualCash
	}
	if len(destination.AnnualShares) == 0 {
		destination.AnnualShares = source.AnnualShares
	}
	if len(destination.AnnualOperatingIncome) == 0 {
		destination.AnnualOperatingIncome = source.AnnualOperatingIncome
	}
	if len(destination.AnnualDepreciationAndAmortization) == 0 {
		destination.AnnualDepreciationAndAmortization = source.AnnualDepreciationAndAmortization
	}
	if len(destination.AnnualEquity) == 0 {
		destination.AnnualEquity = source.AnnualEquity
	}
	if destination.Industry == "" {
		destination.Industry = source.Industry
	}
	if destination.IndustryKey == "" {
		destination.IndustryKey = source.IndustryKey
	}
	if destination.Sector == "" {
		destination.Sector = source.Sector
	}
	if source.OperatingCashflow != nil {
		destination.OperatingCashflow = source.OperatingCashflow
	}
	if destination.EBITDA == nil {
		destination.EBITDA = source.EBITDA
	}
	if destination.EPSRevision == nil {
		destination.EPSRevision = source.EPSRevision
	}
	if destination.InventoryDays == nil {
		destination.InventoryDays = source.InventoryDays
	}
}

func fundamentalsNeedsTimeseries(value YahooFundamentals) bool {
	return value.EPS == nil || value.FreeCashflow == nil || value.Debt == nil || value.Cash == nil || value.Shares == nil || value.Revenue == nil ||
		value.NetIncome == nil || value.DepreciationAndAmortization == nil || value.CapitalExpenditure == nil || value.ChangeInWorkingCapital == nil ||
		value.OperatingIncome == nil || value.InterestExpense == nil || value.Buyback == nil || value.Equity == nil || len(value.QuarterlyRevenue) == 0 || len(value.QuarterlyEPS) == 0 || len(value.AnnualRevenue) < 2 || len(value.AnnualEPS) < 2 || len(value.AnnualNetIncome) < 2 || len(value.AnnualCapitalExpenditure) < 2
}

func firstMetric(values ...YahooMetricDTO) *float64 {
	for _, value := range values {
		if parsed := metricNumber(value); parsed != nil {
			return parsed
		}
	}
	return nil
}

func metricNumber(value YahooMetricDTO) *float64 {
	return rawFiniteNumber(value.Raw)
}

func rawFiniteNumber(raw json.RawMessage) *float64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	if strings.HasPrefix(text, "{") {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil {
			for _, key := range []string{"raw", "value"} {
				if nested, ok := object[key]; ok {
					return rawFiniteNumber(nested)
				}
			}
		}
		return nil
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && finite(number) {
		return &number
	}
	var boolean bool
	if json.Unmarshal(raw, &boolean) == nil {
		if boolean {
			number = 1
		}
		return &number
	}
	var stringValue string
	if json.Unmarshal(raw, &stringValue) == nil {
		number, err := strconv.ParseFloat(strings.TrimSpace(stringValue), 64)
		if err == nil && finite(number) {
			return &number
		}
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
