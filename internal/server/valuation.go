package server

import (
	"context"
	"errors"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/valuation"
)

// Go's generic valuation is not yet numerically equivalent to Python v39's
// specialized decision tree. Keep a distinct source/version so Go cannot make
// a Python cache entry look authoritative or silently overwrite it.
const goFairValueSource = "valuation_go_v4"

const goFairValueCacheVersion = "go_v4"

type fundamentalsProvider interface {
	Fundamentals(context.Context, string) (provider.YahooFundamentals, error)
}

// enrichQuoteValuation is optional: the quote path remains usable when Yahoo
// fundamentals are unavailable or incomplete. Existing persisted valuation is
// preserved by SQLite when this function cannot produce a candidate.
func (s *Service) enrichQuoteValuation(ctx context.Context, symbol string, quote *model.Quote) {
	upstream, ok := s.provider.(fundamentalsProvider)
	if !ok || quote == nil {
		return
	}
	fundamentals, err := upstream.Fundamentals(ctx, symbol)
	if err != nil {
		return
	}
	if valuation.IsNonCompanyInstrument(fundamentals.QuoteType) {
		if quote.FairValueSource != "" && quote.FairValue != nil && !isGoFairValueSource(quote.FairValueSource) {
			return
		}
		inputs := valuationInputs(symbol, valuationCurrentPrice(fundamentals, quote.Price), fundamentals)
		result := valuation.Calculate(inputs)
		clearQuoteValuation(quote, result)
		return
	}
	inputs := valuationInputs(symbol, valuationCurrentPrice(fundamentals, quote.Price), fundamentals)
	inputs.Regime, inputs.RegimeSignals = s.detectValuationRegime(ctx, symbol, fundamentals)
	result := valuation.Calculate(inputs)
	applyTimeseriesRetryStatus(fundamentals, &result)
	applyV39ResultDiagnostics(quote, result)
	if result.FairValue == nil {
		applyValuationDiagnostics(quote, result)
		return
	}
	quote.FairValue = *result.FairValue
	quote.FairValueLow = pointerAsAny(result.FairValueLow)
	quote.FairValueHigh = pointerAsAny(result.FairValueHigh)
	quote.FairValueBuyLow = pointerAsAny(result.FairValueBuyLow)
	quote.FairValueBuyHigh = pointerAsAny(result.FairValueBuyHigh)
	quote.FairValueSource = goFairValueSource
	quote.FairValueModel = result.Method
	quote.FairValueMethods = result.Methods
	quote.FairValueDefensive = result.Defensive
	quote.FairValueOptimistic = result.Optimistic
	quote.FairValueRegime = stringPointer(result.Regime)
	quote.FairValueRegimeSignals = result.RegimeSignals
	quote.FairValueModelUnderRegime = stringPointer(result.ModelUnderRegime)
	quote.FairValueQuarterlyMomentum = result.QuarterlyMomentum
	quote.FairValueHistoricalValuationPct = result.HistoricalValuationPercentiles
	quote.FairValueInterestCoverage = copyFloatPointer(result.InterestCoverage)
	quote.FairValueDataQualityScore = float64Pointer(result.DataQualityScore)
	quote.FairValueMarketCapDataQuality = stringPointer(result.MarketCapDataQuality)
	if result.NormalizedEPSSource != "" {
		quote.FairValueNormalizedEPSSource = stringPointer(result.NormalizedEPSSource)
	}
	quote.FairValueShareholderReturnYield = copyFloatPointer(result.ShareholderTotalReturnYield)
	quote.FairValueForwardEPS = result.ForwardEPS
	quote.FairValueSafetyMargin = float64Pointer(result.SafetyMargin)
	confidenceScore, confidenceLabel := apiValuationConfidence(inputs, result)
	quote.FairValueConfidence = stringPointer(confidenceLabel)
	quote.FairValueConfidenceScore = float64Pointer(confidenceScore)
	quote.FairValueOwnerEarningsMaintRatio = copyFloatPointer(result.OwnerEarningsMaintenanceRatio)
	if result.OwnerEarningsRatioSource != "" {
		source := result.OwnerEarningsRatioSource
		quote.FairValueOwnerEarningsRatioSource = &source
	}
	quote.FairValueForwardEPSSource = valuationForwardEPSSource(fundamentals, result.ForwardEPS)
	if result.ForwardEPSSource != "" {
		quote.FairValueForwardEPSSource = stringPointer(result.ForwardEPSSource)
	}
	applyValuationDiagnostics(quote, result)
}

func applyValuationDiagnostics(quote *model.Quote, result valuation.Result) {
	quote.FairValueStatus = apiValuationStatus(result.Status)
	if quote.FairValueStatus == "" {
		quote.FairValueStatus = valuation.StatusReady
	}
	quote.FairValuePending = quote.FairValueStatus == "retry"
	quote.FairValueWarning = result.Warning
	quote.FairValueWarnings = append([]string(nil), result.Warnings...)
	quote.FairValueErrors = append([]string(nil), result.Errors...)
}

// Python exposes terminal valuation failures as "failed". Keep the same API
// status because clients use it to stop polling; "invalid" is an internal
// calculator state and is not part of the quote contract.
func apiValuationStatus(status string) string {
	if status == valuation.StatusInvalid {
		return "failed"
	}
	return status
}

func applyTimeseriesRetryStatus(fundamentals provider.YahooFundamentals, result *valuation.Result) {
	if result == nil || result.FairValue != nil || !(fundamentals.TimeseriesRetryable || fundamentals.ValuationUpstreamRetryable) {
		return
	}
	result.Status = "retry"
	result.Warning = "Yahoo 数据源暂时限流，估值将在稍后自动重试"
}

func pointerAsAny(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func float64Pointer(value float64) *float64 { return &value }
func stringPointer(value string) *string    { return &value }
func copyFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func valuationInputs(symbol string, price *float64, fundamentals provider.YahooFundamentals) valuation.Fundamentals {
	marketCap, marketCapQuality, marketCapWarning := valuationMarketCapInputs(price, fundamentals)
	normalizedEPS, normalizedEPSSource := valuationNormalizedEPS(fundamentals)
	var safetyMargin *float64
	if isREITFundamentals(fundamentals) {
		margin := 0.25
		beta := 1.0
		if fundamentals.Beta != nil && *fundamentals.Beta >= 0.2 && *fundamentals.Beta <= 3.0 {
			beta = *fundamentals.Beta
		}
		if beta > 1.5 {
			margin += 0.05
		}
		safetyMargin = &margin
	}
	return valuation.Fundamentals{
		Symbol: symbol, InstrumentType: fundamentals.QuoteType, ProviderBacked: true, TimeseriesRetryable: fundamentals.TimeseriesRetryable,
		UpstreamError: fundamentals.ValuationUpstreamError,
		CurrentPrice:  price, SharesOutstanding: fundamentals.Shares,
		TrailingEPS: valuationTrailingEPS(fundamentals), NormalizedEPS: normalizedEPS,
		NormalizedEPSSource: normalizedEPSSource,
		Industry:            fundamentals.Industry, IndustryKey: fundamentals.IndustryKey, Sector: fundamentals.Sector,
		Beta: fundamentals.Beta, RevenueGrowth: finiteNumberPointerCopy(fundamentals.RevenueGrowth),
		HistoricalPrices: fundamentals.HistoricalPrices,
		Debt:             fundamentals.Debt, Cash: fundamentals.Cash, Revenue: fundamentals.Revenue,
		CurrentYearRevenueEstimate: fundamentals.CurrentYearRevenueEstimate,
		NextYearRevenueEstimate:    fundamentals.NextYearRevenueEstimate,
		CurrentYearRevenueAnalysts: fundamentals.CurrentYearRevenueAnalysts,
		NextYearRevenueAnalysts:    fundamentals.NextYearRevenueAnalysts,
		SharesSource:               fundamentals.SharesSource,
		OperatingCashflow:          fundamentals.OperatingCashflow, EBITDA: fundamentals.EBITDA,
		OperatingIncome: fundamentals.OperatingIncome, Equity: fundamentals.Equity,
		QuarterlyRevenue: fundamentals.QuarterlyRevenue, QuarterlyEPS: fundamentals.QuarterlyEPS,
		AnnualDebt: fundamentals.AnnualDebt, AnnualCash: fundamentals.AnnualCash, AnnualShares: fundamentals.AnnualShares,
		AnnualOperatingIncome: fundamentals.AnnualOperatingIncome, AnnualDepreciationAndAmortization: fundamentals.AnnualDepreciationAndAmortization,
		AnnualEquity: fundamentals.AnnualEquity,
		BookValue:    fundamentals.BookValue, TargetLowPrice: fundamentals.TargetLowPrice, TargetHighPrice: fundamentals.TargetHighPrice,
		PriceToSales: fundamentals.PriceToSales, EnterpriseToEBITDA: fundamentals.EnterpriseToEBITDA,
		ForwardEBITDA: fundamentals.ForwardEBITDA, InterestExpense: fundamentals.InterestExpense, ROE: fundamentals.ROE,
		RABPerShare: fundamentals.RABPerShare, EmbeddedValuePerShare: fundamentals.EmbeddedValuePerShare,
		NewBusinessValuePerShare: fundamentals.NewBusinessValuePerShare, PipelineRNPV: fundamentals.PipelineRNPV,
		ApprovedDrugValue: fundamentals.ApprovedDrugValue, PlatformUsers: fundamentals.PlatformUsers,
		ARPU: fundamentals.ARPU, ChurnRate: fundamentals.ChurnRate, AIRevenueShare: fundamentals.AIRevenueShare,
		ForwardEPS: valuationForwardEPS(fundamentals), ForwardEPSObserved: fundamentals.ForwardEPS != nil && finitePositive(*fundamentals.ForwardEPS),
		GrowthHintObserved: finiteNumberPointer(fundamentals.EarningsGrowth) || finiteNumberPointer(fundamentals.EarningsQuarterlyGrowth) || finiteNumberPointer(fundamentals.RevenueGrowth),
		FreeCashFlow:       fundamentals.FreeCashflow,
		FFOPerShare:        fundamentals.FFOPerShare, AFFOPerShare: fundamentals.AFFOPerShare,
		NAVPerShare: fundamentals.NAVPerShare, IsREIT: isREITFundamentals(fundamentals),
		NetIncome: fundamentals.NetIncome, DepreciationAndAmortization: fundamentals.DepreciationAndAmortization,
		CapitalExpenditure: fundamentals.CapitalExpenditure, ChangeInWorkingCapital: fundamentals.ChangeInWorkingCapital,
		AnnualEPS: fundamentals.AnnualEPS, AnnualRevenue: fundamentals.AnnualRevenue,
		Buyback: fundamentals.Buyback, DividendRate: fundamentals.DividendRate,
		MarketCap: marketCap, AnalystCount: fundamentals.AnalystCount,
		MarketCapDataQuality:        marketCapQuality,
		MarketCapWarning:            marketCapWarning,
		TargetMeanPrice:             fundamentals.TargetMeanPrice,
		DefenseRevenueGrowth:        fundamentals.DefenseRevenueGrowth,
		LongBusinessSummary:         fundamentals.LongBusinessSummary,
		MaintenanceCapexRatio:       float64Pointer(ownerMaintenanceRatio(fundamentals)),
		MaintenanceCapexRatioSource: ownerMaintenanceRatioSource(fundamentals),
		RiskFreeRate:                float64Pointer(valuationRiskFreeRate(fundamentals)),
		RequiredReturn:              float64Pointer(valuationRequiredReturnWithRate(fundamentals, valuationRiskFreeRate(fundamentals))),
		GrowthRate:                  valuationGrowthRate(fundamentals),
		Regime:                      "normal",
		SafetyMargin:                safetyMargin,
	}
}

func valuationCurrentPrice(fundamentals provider.YahooFundamentals, quotePrice *float64) *float64 {
	if fundamentals.Price != nil && finitePositive(*fundamentals.Price) {
		return copyFloatPointer(fundamentals.Price)
	}
	if quotePrice != nil && finitePositive(*quotePrice) {
		return copyFloatPointer(quotePrice)
	}
	return nil
}

func valuationForwardEPSSource(fundamentals provider.YahooFundamentals, forwardEPS *float64) *string {
	if forwardEPS == nil {
		return nil
	}
	if fundamentals.ForwardEPS != nil && finitePositive(*fundamentals.ForwardEPS) {
		return stringPointer("Yahoo远期EPS")
	}
	return stringPointer("模型估计远期EPS")
}

// valuationMarketCapInputs mirrors Python's reconciliation rule at the adapter
// boundary. Python trusts balance-sheet, info, and fast_info share counts when
// replacing a mismatching Yahoo market cap; annual timeseries and unknown
// sources remain diagnostic-only because they can lag the current share count.
// Quantity errors are kept as unit_mismatch and never converted into a
// plausible-looking cap.
func valuationMarketCapInputs(price *float64, fundamentals provider.YahooFundamentals) (*float64, string, string) {
	marketCap := copyFloatPointer(fundamentals.MarketCap)
	quality := valuationMarketCapQuality(price, fundamentals)
	if price == nil || *price <= 0 || fundamentals.Shares == nil || *fundamentals.Shares <= 0 {
		return marketCap, quality, ""
	}
	implied := *price * *fundamentals.Shares
	if !finitePositive(implied) || marketCap == nil || !finitePositive(*marketCap) {
		if marketCap == nil && finitePositive(implied) {
			return float64Pointer(implied), "derived", ""
		}
		return marketCap, quality, ""
	}
	mismatch := math.Abs(*marketCap-implied) / *marketCap
	if mismatch <= .20 {
		return marketCap, "consistent", ""
	}
	scale := *marketCap / implied
	if scale > 100 || scale < .01 {
		return marketCap, "unit_mismatch", "market_cap 与股本×现价存在数量级差异，已保留市值并降低数据质量评分"
	}
	if trustedMarketCapSharesSource(fundamentals.SharesSource) {
		return float64Pointer(implied), "reconciled", "market_cap 与股本×现价偏差超过 20%，已使用" + fundamentals.SharesSource + "×现价校正市值"
	}
	source := strings.TrimSpace(fundamentals.SharesSource)
	if source == "" {
		source = "unknown"
	}
	return marketCap, "market_cap_preferred", "股本来源 " + source + " 未通过当前口径核验，已保留 Yahoo market_cap"
}

func trustedMarketCapSharesSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "balance_sheet", "info", "fast_info":
		return true
	default:
		return false
	}
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finiteNumberPointer(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func finiteNumberPointerCopy(value *float64) *float64 {
	if !finiteNumberPointer(value) {
		return nil
	}
	return copyFloatPointer(value)
}

func valuationMarketCapQuality(price *float64, fundamentals provider.YahooFundamentals) string {
	if fundamentals.MarketCap != nil && *fundamentals.MarketCap > 0 && price != nil && *price > 0 && fundamentals.Shares != nil && *fundamentals.Shares > 0 {
		implied := *price * *fundamentals.Shares
		mismatch := math.Abs(*fundamentals.MarketCap-implied) / *fundamentals.MarketCap
		if mismatch <= .20 {
			return "consistent"
		}
		if scale := *fundamentals.MarketCap / implied; scale > 100 || scale < .01 {
			return "unit_mismatch"
		}
		return "market_cap_preferred"
	}
	if fundamentals.MarketCap == nil && price != nil && *price > 0 && fundamentals.Shares != nil && *fundamentals.Shares > 0 {
		return "derived"
	}
	return "unknown"
}

func isREITFundamentals(f provider.YahooFundamentals) bool {
	text := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+f.IndustryKey, "_", " "))
	for _, marker := range []string{"reit", "real estate investment trust"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	// Yahoo's stable industry keys are the strongest structured signal, but keep
	// the explicit list narrow so ordinary real-estate operators retain fallback.
	for _, key := range []string{"reit-residential", "reit-retail", "reit-office", "reit-diversified"} {
		if strings.Contains(text, key) {
			return true
		}
	}
	return false
}

func valuationTrailingEPS(f provider.YahooFundamentals) *float64 {
	if f.EPS == nil || *f.EPS <= 0 || math.IsNaN(*f.EPS) || math.IsInf(*f.EPS, 0) {
		return nil
	}
	return copyFloatPointer(f.EPS)
}

func valuationNormalizedEPS(f provider.YahooFundamentals) (*float64, string) {
	// Python normalization uses TTM EPS plus up to five annual values, then takes
	// the median of positive finite observations to reduce cycle and data spikes.
	values := make([]float64, 0, 6)
	if f.EPS != nil && *f.EPS > 0 && !math.IsNaN(*f.EPS) && !math.IsInf(*f.EPS, 0) {
		values = append(values, *f.EPS)
	}
	for _, value := range f.AnnualEPS {
		if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			values = append(values, value)
			if len(values) == 6 {
				break
			}
		}
	}
	var historicalEPS *float64
	if len(values) > 0 {
		sort.Float64s(values)
		historicalEPS = float64Pointer(values[len(values)/2])
	}
	historicalSource := ""
	if historicalEPS != nil {
		historicalSource = "历史 EPS 中位数"
	}
	if f.Shares == nil || *f.Shares <= 0 || math.IsNaN(*f.Shares) || math.IsInf(*f.Shares, 0) {
		return historicalEPS, historicalSource
	}
	netIncomeEPS := make([]float64, 0, len(f.AnnualNetIncome))
	for _, netIncome := range f.AnnualNetIncome {
		if netIncome > 0 && !math.IsNaN(netIncome) && !math.IsInf(netIncome, 0) {
			netIncomeEPS = append(netIncomeEPS, netIncome / *f.Shares)
		}
	}
	if len(netIncomeEPS) < 2 {
		return historicalEPS, historicalSource
	}
	sort.Float64s(netIncomeEPS)
	normalizedNetIncomeEPS := netIncomeEPS[len(netIncomeEPS)/2]
	if historicalEPS == nil {
		return float64Pointer(normalizedNetIncomeEPS), "净利润 / 当前股本"
	}
	ratio := normalizedNetIncomeEPS / *historicalEPS
	if ratio < .25 || ratio > 4 {
		return historicalEPS, "历史 EPS 中位数（净利润股本量纲异常回退）"
	}
	return float64Pointer(normalizedNetIncomeEPS), "净利润 / 当前股本"
}

func valuationForwardEPS(f provider.YahooFundamentals) *float64 {
	if f.ForwardEPS != nil && *f.ForwardEPS > 0 && !math.IsNaN(*f.ForwardEPS) && !math.IsInf(*f.ForwardEPS, 0) {
		return f.ForwardEPS
	}
	baseEPS, _ := valuationNormalizedEPS(f)
	growth := valuationGrowthRate(f)
	if baseEPS == nil || growth == nil {
		return nil
	}
	return float64Pointer(*baseEPS * (1 + *growth))
}

func valuationGrowthRate(f provider.YahooFundamentals) *float64 {
	// Match Python's growth_hint precedence: Yahoo earnings growth first,
	// quarterly earnings growth next, then revenue growth before historic CAGR.
	for _, hint := range []*float64{f.EarningsGrowth, f.EarningsQuarterlyGrowth, f.RevenueGrowth} {
		if hint != nil && !math.IsNaN(*hint) && !math.IsInf(*hint, 0) {
			bounded := math.Max(-.15, math.Min(.50, *hint))
			return float64Pointer(bounded)
		}
	}
	if growth := compoundGrowth(f.AnnualEPS); growth != nil {
		return growth
	}
	return compoundGrowth(f.AnnualRevenue)
}

func compoundGrowth(values []float64) *float64 {
	// Values are ordered newest to oldest by the Yahoo adapter. Match the Python
	// fallback by ignoring nonpositive/nonfinite periods before calculating CAGR.
	positive := make([]float64, 0, len(values))
	for _, value := range values {
		if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			positive = append(positive, value)
		}
	}
	if len(positive) < 2 {
		return nil
	}
	growth := math.Pow(positive[0]/positive[len(positive)-1], 1/float64(len(positive)-1)) - 1
	if math.IsNaN(growth) || math.IsInf(growth, 0) {
		return nil
	}
	growth = math.Max(-.15, math.Min(.50, growth))
	return float64Pointer(growth)
}

// Used when none of Yahoo's supported risk-free-rate aliases is available.
const defaultValuationRiskFreeRate = .04

func valuationRiskFreeRate(f provider.YahooFundamentals) float64 {
	if f.RiskFreeRate == nil || *f.RiskFreeRate == 0 {
		return defaultValuationRiskFreeRate
	}
	return normalizeRiskFreeRate(*f.RiskFreeRate)
}

func valuationRequiredReturn(f provider.YahooFundamentals) float64 {
	return valuationRequiredReturnWithRate(f, valuationRiskFreeRate(f))
}

func valuationRequiredReturnWithRate(f provider.YahooFundamentals, riskFreeRate float64) float64 {
	beta := valuationBeta(f.Beta)
	requiredReturn := math.Min(.18, math.Max(.085, normalizeRiskFreeRate(riskFreeRate)+.055*beta))
	if valuationStableCompany(f) {
		requiredReturn = math.Min(.09, math.Max(.08, requiredReturn))
	}
	return requiredReturn
}

func valuationBeta(beta *float64) float64 {
	if beta == nil || math.IsNaN(*beta) || math.IsInf(*beta, 0) || *beta < .2 || *beta > 3 {
		return 1
	}
	return *beta
}

func normalizeRiskFreeRate(rate float64) float64 {
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = defaultValuationRiskFreeRate
	}
	if rate > 1 {
		rate /= 100
	}
	return math.Min(.08, math.Max(.02, rate))
}

func valuationStableCompany(f provider.YahooFundamentals) bool {
	text := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+f.IndustryKey, "_", " "))
	for _, marker := range []string{"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func maintenanceIndustryText(f provider.YahooFundamentals) string {
	return strings.ToLower(strings.ReplaceAll(f.Industry+" "+f.Sector+" "+f.IndustryKey, "_", " "))
}

func maintenanceClassificationText(f provider.YahooFundamentals) string {
	industryKey := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(f.IndustryKey), "_", "-"))
	industryKey = strings.ReplaceAll(industryKey, "-", " ")
	return strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+industryKey+" "+f.LongBusinessSummary, "_", " "))
}

func maintenanceFinancialOrRealEstate(text string) bool {
	for _, marker := range []string{"financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func maintenanceMemoryOrCruiseIndustry(text string) bool {
	for _, marker := range []string{"memory", "nand", "dram", "flash", "cruise", "travel services", "resorts"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func maintenanceDefenseTransition(f provider.YahooFundamentals, text string) bool {
	if f.DefenseRevenueGrowth != nil && !math.IsNaN(*f.DefenseRevenueGrowth) && !math.IsInf(*f.DefenseRevenueGrowth, 0) && *f.DefenseRevenueGrowth >= .30 {
		return true
	}
	defense := false
	for _, marker := range []string{"defense", "aerospace", "laser", "military", "directed energy"} {
		if strings.Contains(text, marker) {
			defense = true
			break
		}
	}
	if !defense {
		return false
	}
	if f.FreeCashflow != nil && *f.FreeCashflow < 0 {
		return true
	}
	if growth := valuationGrowthRate(f); growth != nil && *growth >= .10 {
		return true
	}
	return f.RevenueGrowth != nil && !math.IsNaN(*f.RevenueGrowth) && !math.IsInf(*f.RevenueGrowth, 0) && *f.RevenueGrowth >= .10
}

func maintenanceAIStorageCycle(f provider.YahooFundamentals, classificationText, industryContext string) bool {
	if maintenanceFinancialOrRealEstate(industryContext) {
		return false
	}
	if containsAnyText(classificationText, "memory", "dram", "nand", "flash") {
		return true
	}
	storageHardware := containsAnyText(classificationText,
		"hard disk", "disk drive", "hdd", "nand", "flash memory", "solid state drive", "ssd",
		"memory chip", "storage chip", "storage device", "storage hardware", "storage products",
		"storage systems", "enterprise storage", "computer storage",
	) || (strings.Contains(classificationText, "computer hardware") && containsAnyText(classificationText, "storage", "disk"))
	if !storageHardware {
		return false
	}
	if growth := valuationGrowthRate(f); growth != nil && *growth >= .20 {
		return true
	}
	if f.RevenueGrowth != nil && !math.IsNaN(*f.RevenueGrowth) && !math.IsInf(*f.RevenueGrowth, 0) && *f.RevenueGrowth >= .20 {
		return true
	}
	normalizedEPS, _ := valuationNormalizedEPS(f)
	return f.ForwardEPS != nil && finitePositive(*f.ForwardEPS) && normalizedEPS != nil && *f.ForwardEPS >= *normalizedEPS*1.25
}

func ownerMaintenanceRatio(f provider.YahooFundamentals) float64 {
	text := maintenanceIndustryText(f)
	classificationText := maintenanceClassificationText(f)
	financialOrRealEstate := maintenanceFinancialOrRealEstate(text)
	memoryOrCruise := maintenanceMemoryOrCruiseIndustry(classificationText)
	defenseTransition := maintenanceDefenseTransition(f, classificationText)
	aiStorageCycle := maintenanceAIStorageCycle(f, classificationText, text)
	// Python industry filters keep financial/real-estate, memory, cruise, and
	// defense-transition cases out of the generic asset-heavy heuristic.
	if !financialOrRealEstate && !memoryOrCruise && !defenseTransition && !aiStorageCycle {
		capexHeavy := f.CapitalExpenditure != nil && f.OperatingCashflow != nil && math.Abs(*f.CapitalExpenditure) >= math.Abs(*f.OperatingCashflow)*.70
		negativeFCF := f.FreeCashflow != nil && *f.FreeCashflow < 0
		if capexHeavy || negativeFCF {
			return .30
		}
	}
	switch {
	case strings.Contains(text, "utility") || strings.Contains(text, "utilities") || strings.Contains(text, "regulated electric") || strings.Contains(text, "water utility") || strings.Contains(text, "gas utility") || strings.Contains(text, "manufactur") || strings.Contains(text, "industrials") || strings.Contains(text, "machinery") || strings.Contains(text, "factory"):
		return .85
	case strings.Contains(text, "technology") || strings.Contains(text, "software") || strings.Contains(text, "consumer") || strings.Contains(text, "internet") || strings.Contains(text, "semiconductor"):
		return .50
	default:
		return .70
	}
}
func ownerMaintenanceRatioSource(f provider.YahooFundamentals) string {
	ratio := ownerMaintenanceRatio(f)
	text := maintenanceIndustryText(f)
	switch ratio {
	case .30:
		return "重资产转型"
	case .85:
		if strings.Contains(text, "utility") || strings.Contains(text, "utilities") || strings.Contains(text, "regulated electric") || strings.Contains(text, "water utility") || strings.Contains(text, "gas utility") {
			return "公用事业"
		}
		return "成熟制造"
	case .50:
		if strings.Contains(text, "technology") || strings.Contains(text, "software") || strings.Contains(text, "consumer") || strings.Contains(text, "internet") || strings.Contains(text, "semiconductor") {
			return "科技/消费"
		}
		return "通用"
	default:
		return "通用"
	}
}

// scheduleValuation starts one background fundamentals read per symbol. The
// latest quote remains available immediately; a completed valuation is written
// back as a separate SQLite snapshot and shared analysis-cache entry.
func fairValueCacheKey(symbol string) string {
	return "fair-value:" + goFairValueCacheVersion + ":" + strings.ToUpper(strings.TrimSpace(symbol))
}

func valuationCacheFresh(payload map[string]any, createdAt time.Time) bool {
	if payload == nil || createdAt.IsZero() {
		return false
	}
	age := time.Since(createdAt)
	if age < 0 {
		age = 0
	}
	status, _ := payload["status"].(string)
	ttl := 15 * time.Second
	switch {
	case payload["value"] != nil:
		ttl = 6 * time.Hour
	case status == "retry":
		ttl = 2 * time.Second
	}
	return age < ttl
}

func fairValueStatusCacheKey(symbol string) string {
	return fairValueCacheKey(symbol) + ":status"
}

func isGoFairValueSource(source string) bool {
	return strings.HasPrefix(strings.TrimSpace(source), "valuation_go_v")
}

func clearSupersededGoValuation(quote *model.Quote) {
	if quote == nil || !isGoFairValueSource(quote.FairValueSource) || quote.FairValueSource == goFairValueSource {
		return
	}
	quote.FairValue = nil
	quote.FairValueLow, quote.FairValueHigh = nil, nil
	quote.FairValueBuyLow, quote.FairValueBuyHigh = nil, nil
	quote.FairValueSource, quote.FairValueModel = "", ""
	quote.FairValueMethods = nil
	quote.FairValueForwardEPS, quote.FairValueForwardEPSSource = nil, nil
	quote.FairValueNormalizedEPS = nil
	quote.FairValuePELow, quote.FairValuePEHigh = nil, nil
	quote.FairValueDecisionTree, quote.FairValueValidation = nil, nil
	quote.FairValueSafetyMargin = nil
	quote.FairValueConfidence, quote.FairValueConfidenceScore = nil, nil
	quote.FairValueInterestCoverage = nil
	quote.FairValueRegime, quote.FairValueRegimeSignals = nil, nil
	quote.FairValueModelUnderRegime = nil
	quote.FairValueDefensive, quote.FairValueOptimistic = nil, nil
	quote.FairValueNormalizedEPSSource = nil
	quote.FairValueQuarterlyMomentum, quote.FairValueHistoricalValuationPct = nil, nil
	quote.FairValueShareholderReturnYield = nil
	quote.FairValueMarketCapDataQuality, quote.FairValueDataQualityScore = nil, nil
	quote.FairValueOwnerEarningsMaintRatio = nil
	quote.FairValueOwnerEarningsRatioSource = nil
	quote.FairValueStatus, quote.FairValueWarning = "pending", ""
	quote.FairValuePending = true
	quote.FairValueWarnings, quote.FairValueErrors = nil, nil
}

func validValuationPayload(value any) (map[string]any, bool) {
	payload, ok := value.(map[string]any)
	if !ok || payload["source"] != goFairValueSource {
		return nil, false
	}
	status, ok := payload["status"].(string)
	if !ok || status == "" {
		return nil, false
	}
	if value := payload["value"]; value != nil {
		number := numericPointer(value)
		if number == nil || *number <= 0 || status != "ready" {
			return nil, false
		}
	} else {
		switch status {
		case "retry", "failed", valuation.StatusUnavailable, valuation.StatusInvalid:
		default:
			return nil, false
		}
	}
	return payload, true
}

func (s *Service) cachedValuation(symbol string) (map[string]any, bool) {
	if s.store == nil {
		return nil, false
	}
	// The attempt record is newer than any previously successful value. A fresh
	// retry/unavailable record suppresses duplicate work only for its own TTL;
	// once it expires, do not let the older six-hour value hide a retry forever.
	statusValue, statusAt, statusFound, statusErr := s.store.GetAnalysisCacheEntry(fairValueStatusCacheKey(symbol))
	if statusErr == nil && statusFound {
		if statusPayload, ok := validValuationPayload(statusValue); ok {
			if valuationCacheFresh(statusPayload, statusAt) {
				return statusPayload, true
			}
			if statusPayload["value"] == nil {
				return nil, false
			}
		}
	}
	value, createdAt, found, err := s.store.GetAnalysisCacheEntry(fairValueCacheKey(symbol))
	if err != nil || !found {
		return nil, false
	}
	payload, ok := validValuationPayload(value)
	return payload, ok && valuationCacheFresh(payload, createdAt)
}

func (s *Service) valuationCachePayload(symbol, key string) map[string]any {
	if s.store == nil {
		return nil
	}
	value, _, found, err := s.store.GetAnalysisCacheEntry(key)
	if err != nil || !found {
		return nil
	}
	payload, ok := validValuationPayload(value)
	if !ok {
		return nil
	}
	return payload
}

func (s *Service) freshValuationCachePayload(symbol, key string) map[string]any {
	if s.store == nil {
		return nil
	}
	value, createdAt, found, err := s.store.GetAnalysisCacheEntry(key)
	if err != nil || !found {
		return nil
	}
	payload, ok := validValuationPayload(value)
	if !ok || !valuationCacheFresh(payload, createdAt) {
		return nil
	}
	return payload
}

func (s *Service) cachedValuationPayload(symbol string) map[string]any {
	// Keep this helper as the value-only compatibility view. A status attempt is
	// merged separately by mergeCachedValuation so retry/unavailable diagnostics
	// cannot hide an older successful fair value.
	return s.valuationCachePayload(symbol, fairValueCacheKey(symbol))
}

func (s *Service) goValuationCacheExpired(symbol string) bool {
	if s.store == nil {
		return true
	}
	// A recent failed attempt suppresses duplicate work only until its short
	// status TTL expires. Do not let the independent six-hour stale value key
	// mask that expired retry state.
	statusValue, statusAt, statusFound, statusErr := s.store.GetAnalysisCacheEntry(fairValueStatusCacheKey(symbol))
	if statusErr == nil && statusFound {
		if statusPayload, ok := validValuationPayload(statusValue); ok {
			if valuationCacheFresh(statusPayload, statusAt) {
				return false
			}
			if statusPayload["value"] == nil {
				return true
			}
		}
	}
	value, createdAt, found, err := s.store.GetAnalysisCacheEntry(fairValueCacheKey(symbol))
	if err != nil || !found {
		return true
	}
	payload, ok := validValuationPayload(value)
	return !ok || !valuationCacheFresh(payload, createdAt)
}

func (s *Service) persistValuationCache(symbol string, payload map[string]any) {
	if s.store == nil || payload == nil {
		return
	}
	entries := s.cfg.AnalysisCacheEntries
	if entries <= 0 {
		entries = 128
		if s.cfg.LowMemory {
			entries = 8
		}
	}
	if payload["source"] == nil {
		payload["source"] = goFairValueSource
	}
	// Keep terminal/retry results durable even when a prior successful value
	// must remain visible. A ready attempt supersedes the previous status.
	if err := s.store.PutAnalysisCache(fairValueStatusCacheKey(symbol), payload, entries); err != nil {
		return
	}
	if payload["value"] != nil {
		_ = s.store.PutAnalysisCache(fairValueCacheKey(symbol), payload, entries)
	} else {
		s.publishFairValueEvent(symbol)
	}
}

func (s *Service) publishFairValueEvent(symbol string) {
	event := model.Event{Symbol: symbol, Kind: "fair_value", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if id, err := s.store.AddEvent(event); err == nil {
		event.ID = id
	}
	s.mu.Lock()
	s.appendEventLocked(event)
	s.mu.Unlock()
}

func valuationRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"429", "403", "rate limit", "too many requests", "timeout", "timed out", "context deadline exceeded",
		"temporarily unavailable", "service unavailable", "connection reset", "connection refused", "connection aborted",
		"http 408", "http 425", "http 500", "http 502", "http 503", "http 504",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func valuationCachePayload(result valuation.Result, inputs valuation.Fundamentals) map[string]any {
	confidenceScore, confidenceLabel := apiValuationConfidence(inputs, result)
	return map[string]any{
		"value": pointerAsAny(result.FairValue), "low": pointerAsAny(result.FairValueLow), "high": pointerAsAny(result.FairValueHigh),
		"buy_low": pointerAsAny(result.FairValueBuyLow), "buy_high": pointerAsAny(result.FairValueBuyHigh),
		"source": goFairValueSource, "model": result.Method, "methods": result.Methods,
		"forward_eps": pointerAsAny(result.ForwardEPS), "safety_margin": result.SafetyMargin,
		"normalized_eps": pointerAsAny(result.NormalizedEPS), "forward_eps_source": optionalString(result.ForwardEPSSource),
		"pe_low": pointerAsAny(result.PELow), "pe_high": pointerAsAny(result.PEHigh),
		"decision_tree": result.DecisionTree, "validation": result.Validation,
		"risk_free_rate": pointerAsAny(result.RiskFreeRate), "required_return": pointerAsAny(result.RequiredReturn),
		"confidence": confidenceLabel, "confidence_score": confidenceScore,
		"regime": result.Regime, "regime_signals": result.RegimeSignals, "model_under_regime": result.ModelUnderRegime,
		"defensive": result.Defensive, "optimistic": result.Optimistic,
		"quarterly_momentum": result.QuarterlyMomentum, "historical_valuation_percentiles": result.HistoricalValuationPercentiles,
		"interest_coverage":     pointerAsAny(result.InterestCoverage),
		"normalized_eps_source": result.NormalizedEPSSource, "shareholder_total_return_yield": pointerAsAny(result.ShareholderTotalReturnYield),
		"market_cap_data_quality": result.MarketCapDataQuality, "data_quality_score": result.DataQualityScore,
		"owner_earnings_maintenance_ratio": pointerAsAny(result.OwnerEarningsMaintenanceRatio), "owner_earnings_ratio_source": result.OwnerEarningsRatioSource,
		"status": apiValuationStatus(result.Status), "warning": result.Warning, "warnings": result.Warnings, "errors": result.Errors,
	}
}

func (s *Service) mergeCachedValuation(symbol string, quote *model.Quote) {
	if quote == nil {
		return
	}
	// Python valuation rows remain authoritative until Go reaches true v39
	// numeric parity. Go's value and attempt-status caches are independent:
	// a retry/unavailable attempt must not erase a usable prior Go value, while
	// its diagnostics must still be visible to the API.
	pythonAuthoritative := quote.FairValue != nil && quote.FairValueSource != "" && !isGoFairValueSource(quote.FairValueSource)
	if pythonAuthoritative {
		return
	}
	statusPayload := s.freshValuationCachePayload(symbol, fairValueStatusCacheKey(symbol))
	clearSupersededGoValuation(quote)
	if statusPayload != nil && statusPayload["value"] == nil && isInapplicableCompanyValuation(statusPayload) {
		// Terminal no-value outcomes (such as ETF model inapplicability) are
		// durable status entries. Remove an older Go value from the response and
		// do not schedule company valuation again while that status is fresh.
		applyValuationPayloadDiagnostics(quote, statusPayload)
		if quote.FairValueStatus == valuation.StatusUnavailable || quote.FairValueStatus == "failed" {
			quote.FairValue = nil
			quote.FairValueLow, quote.FairValueHigh = nil, nil
			quote.FairValueBuyLow, quote.FairValueBuyHigh = nil, nil
			quote.FairValueSource, quote.FairValueModel = "", ""
			quote.FairValueMethods = nil
			quote.FairValueDefensive, quote.FairValueOptimistic = nil, nil
			return
		}
	}
	if quote.FairValueStatus == "" {
		if quote.FairValue != nil {
			quote.FairValueStatus = valuation.StatusReady
		} else {
			quote.FairValueStatus = "pending"
		}
	}
	quote.FairValuePending = quote.FairValueStatus == "pending" || quote.FairValueStatus == "retry"
	valuePayload := s.cachedValuationPayload(symbol)
	// Successful attempts are written to the status key before the long-lived
	// value key. If the process stops between writes, consume the complete new
	// result from status rather than serving an older value with newer metadata.
	if statusPayload != nil && statusPayload["value"] != nil {
		valuePayload = statusPayload
	}
	if !pythonAuthoritative && valuePayload != nil {
		if value := numericValue(valuePayload["value"]); value != nil && *value > 0 {
			value := *value
			quote.FairValue = value
			quote.FairValueLow = valuePayload["low"]
			quote.FairValueHigh = valuePayload["high"]
			quote.FairValueBuyLow = valuePayload["buy_low"]
			quote.FairValueBuyHigh = valuePayload["buy_high"]
			quote.FairValueSource = goFairValueSource
			if modelName, ok := valuePayload["model"].(string); ok {
				quote.FairValueModel = modelName
			}
			quote.FairValueMethods = valuePayload["methods"]
			quote.FairValueForwardEPS = numericPointer(valuePayload["forward_eps"])
			if source, ok := valuePayload["forward_eps_source"]; ok {
				quote.FairValueForwardEPSSource = stringPointerValue(source)
			}
			quote.FairValueSafetyMargin = numericPointer(valuePayload["safety_margin"])
			quote.FairValueConfidence = stringPointerValue(valuePayload["confidence"])
			quote.FairValueConfidenceScore = cachedConfidenceScore(valuePayload["confidence_score"])
			quote.FairValueRegime = stringPointerValue(valuePayload["regime"])
			quote.FairValueRegimeSignals = valuePayload["regime_signals"]
			quote.FairValueModelUnderRegime = stringPointerValue(valuePayload["model_under_regime"])
			quote.FairValueQuarterlyMomentum = valuePayload["quarterly_momentum"]
			quote.FairValueHistoricalValuationPct = valuePayload["historical_valuation_percentiles"]
			quote.FairValueInterestCoverage = numericPointer(valuePayload["interest_coverage"])
			quote.FairValueDefensive = valuePayload["defensive"]
			quote.FairValueOptimistic = valuePayload["optimistic"]
			quote.FairValueNormalizedEPSSource = stringPointerValue(valuePayload["normalized_eps_source"])
			quote.FairValueShareholderReturnYield = numericPointer(valuePayload["shareholder_total_return_yield"])
			quote.FairValueMarketCapDataQuality = stringPointerValue(valuePayload["market_cap_data_quality"])
			quote.FairValueDataQualityScore = numericPointer(valuePayload["data_quality_score"])
			quote.FairValueOwnerEarningsMaintRatio = numericPointer(valuePayload["owner_earnings_maintenance_ratio"])
			quote.FairValueOwnerEarningsRatioSource = stringPointerValue(valuePayload["owner_earnings_ratio_source"])
		}
		applyV39PayloadDiagnostics(quote, valuePayload)
	}
	// Attempt diagnostics have a short TTL. Once they expire, fall back to the
	// value entry's ready state rather than exposing a permanently stale retry.
	if statusPayload != nil {
		applyValuationPayloadDiagnostics(quote, statusPayload)
	} else if valuePayload != nil {
		applyValuationPayloadDiagnostics(quote, valuePayload)
	}
	// Python starts valuation on the first quote request and returns the market
	// quote immediately. Preserve that behavior for uncached quotes by passing
	// this quote into the background worker instead of requiring an SQLite row.
	if quote.FairValue == nil || (quote.FairValueSource == goFairValueSource && s.goValuationCacheExpired(symbol)) {
		s.scheduleValuationWithQuote(symbol, quote)
	}
}

func isInapplicableCompanyValuation(payload map[string]any) bool {
	if payload == nil || payload["status"] != valuation.StatusUnavailable {
		return false
	}
	warning, _ := payload["warning"].(string)
	return strings.Contains(warning, "not an operating company")
}

func applyValuationPayloadDiagnostics(quote *model.Quote, payload map[string]any) {
	applyV39PayloadDiagnostics(quote, payload)
	if status, ok := payload["status"].(string); ok && status != "" {
		quote.FairValueStatus = apiValuationStatus(status)
	}
	if quote.FairValueStatus == "" {
		quote.FairValueStatus = "pending"
	}
	quote.FairValuePending = quote.FairValueStatus == "pending" || quote.FairValueStatus == "retry"
	if warning, ok := payload["warning"].(string); ok {
		quote.FairValueWarning = warning
	}
	quote.FairValueWarnings = stringSliceValue(payload["warnings"])
	quote.FairValueErrors = stringSliceValue(payload["errors"])
}

func applyV39PayloadDiagnostics(quote *model.Quote, payload map[string]any) {
	if value, ok := payload["normalized_eps"]; ok {
		quote.FairValueNormalizedEPS = numericPointer(value)
	}
	if value, ok := payload["forward_eps_source"]; ok {
		quote.FairValueForwardEPSSource = stringPointerValue(value)
	}
	if value, ok := payload["pe_low"]; ok {
		quote.FairValuePELow = numericPointer(value)
	}
	if value, ok := payload["pe_high"]; ok {
		quote.FairValuePEHigh = numericPointer(value)
	}
	if value, ok := payload["decision_tree"]; ok {
		quote.FairValueDecisionTree = value
	}
	if value, ok := payload["validation"]; ok {
		quote.FairValueValidation = value
	}
}

func applyV39ResultDiagnostics(quote *model.Quote, result valuation.Result) {
	quote.FairValueNormalizedEPS = copyFloatPointer(result.NormalizedEPS)
	quote.FairValuePELow = copyFloatPointer(result.PELow)
	quote.FairValuePEHigh = copyFloatPointer(result.PEHigh)
	quote.FairValueDecisionTree = result.DecisionTree
	quote.FairValueValidation = result.Validation
	if result.ForwardEPSSource != "" {
		quote.FairValueForwardEPSSource = stringPointer(result.ForwardEPSSource)
	}
}

func numericPointer(value any) *float64 {
	return numericValue(value)
}

// Result.Confidence is an internal 0..1 method score. The Python/API contract
// exposes fair_value_confidence_score as a percentage, so conversion belongs at
// this service boundary. Keep cache reads tolerant of old Go entries that used
// to persist the internal ratio directly.
func confidenceScorePercent(score float64) float64 {
	if score >= 0 && score <= 1 {
		score *= 100
	}
	return roundPythonDecimal(score, 1)
}

func roundPythonDecimal(value float64, places int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', places, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

// apiValuationConfidence mirrors the Python provider's v39 confidence policy
// at the service boundary. Result.Confidence remains the provider-independent
// method score used by the valuation package's callers; this score is the
// percentage exposed by quote and persisted in the shared analysis cache.
func apiValuationConfidence(inputs valuation.Fundamentals, result valuation.Result) (float64, string) {
	if result.FairValue == nil || result.Status != valuation.StatusReady {
		return 0, "低"
	}
	// Keep the provider-independent fallback contract for deliberately minimal
	// callers (and old SQLite fixtures) that carry none of the v39 diagnostics.
	// A real Yahoo fundamentals snapshot supplies at least one of these fields.
	if !inputs.ProviderBacked && len(inputs.AnnualRevenue) == 0 && (inputs.MarketCap == nil || inputs.MarketCapDataQuality == "derived") && inputs.Beta == nil &&
		result.InterestCoverage == nil && !hasValuationRegimeSignals(inputs.RegimeSignals) {
		score := confidenceScorePercent(result.Confidence)
		return score, apiConfidenceLabel(score)
	}
	number := func(value any) float64 {
		parsed := numericValue(value)
		if parsed == nil {
			return 0
		}
		return *parsed
	}
	industryModel := result.IndustryModel
	defensiveValue := number(result.Defensive["value"])
	if defensiveValue <= 0 {
		defensiveValue = *result.FairValue
	}
	defensiveLow := number(result.Defensive["low"])
	optimisticHigh := number(result.Optimistic["high"])
	spread := 0.0
	if result.Optimistic != nil && defensiveValue > 0 {
		spread = math.Abs(optimisticHigh-defensiveLow) / math.Max(defensiveValue, 1)
	}
	score := 100.0 - math.Min(35, spread*12)
	aiExposed := !industryModel && (result.AIExposed || (inputs.AIRevenueShare != nil && *inputs.AIRevenueShare > 0))
	forwardMissing := hasValuationWarning(result, "缺乏分析师远期 EPS")
	if forwardMissing {
		score -= 20
	}
	if inputs.FreeCashFlow != nil && *inputs.FreeCashFlow < 0 {
		score -= 18
	}
	if valuationBeta(inputs.Beta) > 1.5 {
		score -= 12
	}
	// Result.InterestCoverage is rounded for the public API (Python also
	// serializes two decimals). Recompute the raw ratio from normalized inputs
	// for threshold decisions so 1.999x still receives Python's <2 penalty.
	interestCoverage, hasInterestCoverage := rawAPIInterestCoverage(inputs)
	if !hasInterestCoverage && result.InterestCoverage != nil {
		interestCoverage, hasInterestCoverage = *result.InterestCoverage, true
	}
	if hasInterestCoverage && interestCoverage < 2 {
		score -= 15
	}
	if aiExposed && result.Regime == valuation.RegimeWinter {
		score -= 20
	} else if aiExposed && result.Regime == valuation.RegimeSlowdown {
		score -= 8
	}
	score -= math.Max(0, 100-result.DataQualityScore) * .35
	if industryModel {
		score += 8
	}
	score = roundPythonDecimal(math.Min(100, math.Max(0, score)), 1)
	return score, apiConfidenceLabel(score)
}

func rawAPIInterestCoverage(inputs valuation.Fundamentals) (float64, bool) {
	if inputs.OperatingIncome == nil || inputs.InterestExpense == nil ||
		!finiteNumberPointer(inputs.OperatingIncome) || !finiteNumberPointer(inputs.InterestExpense) {
		return 0, false
	}
	interest := math.Abs(*inputs.InterestExpense)
	if interest <= 0 {
		return 0, false
	}
	if interest <= 1 && inputs.Revenue != nil && finiteNumberPointer(inputs.Revenue) && *inputs.Revenue > 0 {
		interest *= *inputs.Revenue
	}
	coverage := *inputs.OperatingIncome / interest
	if math.IsNaN(coverage) || math.IsInf(coverage, 0) {
		return 0, false
	}
	return coverage, true
}

func hasValuationWarning(result valuation.Result, fragment string) bool {
	if strings.Contains(result.Warning, fragment) {
		return true
	}
	for _, warning := range result.Warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}

func apiConfidenceLabel(score float64) string {
	if score >= 75 {
		return "高"
	}
	if score < 45 {
		return "低"
	}
	return "中"
}

func hasValuationRegimeSignals(signals map[string]any) bool {
	for _, value := range signals {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if strings.TrimSpace(typed) != "" {
				return true
			}
		case []string:
			if len(typed) > 0 {
				return true
			}
		case []any:
			if len(typed) > 0 {
				return true
			}
		default:
			if numericValue(value) != nil {
				return true
			}
		}
	}
	return false
}

func containsAnyText(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func cachedConfidenceScore(value any) *float64 {
	score := numericValue(value)
	if score == nil {
		return nil
	}
	converted := confidenceScorePercent(*score)
	return &converted
}

func stringPointerValue(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

func stringSliceValue(value any) []string {
	values, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...)
		}
		return nil
	}
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func (s *Service) scheduleValuation(symbol string) {
	s.scheduleValuationWithQuote(symbol, nil)
}

func (s *Service) scheduleValuationWithQuote(symbol string, quote *model.Quote) {
	if _, ok := s.provider.(fundamentalsProvider); !ok {
		return
	}
	// The cache is shared by Go workers and the Python service. Do not launch a
	// duplicate request while a ready or terminal/retry result is still inside
	// its policy TTL; expired entries are deliberately recomputed.
	if _, fresh := s.cachedValuation(symbol); fresh {
		return
	}
	s.mu.Lock()
	if s.valuationJobs[symbol] {
		s.mu.Unlock()
		return
	}
	// Cached quotes may be polled frequently. Bound retries after a failed or
	// incomplete fundamentals response while allowing later recovery attempts.
	if last := s.valuationAttempts[symbol]; !last.IsZero() && time.Since(last) < 2*time.Second {
		s.mu.Unlock()
		return
	}
	s.valuationAttempts[symbol] = time.Now()
	s.valuationJobs[symbol] = true
	s.backgroundWG.Add(1)
	ctx := s.background
	var quoteSnapshot *model.Quote
	if quote != nil {
		copy := *quote
		quoteSnapshot = &copy
	}
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.valuationJobs, symbol)
			s.mu.Unlock()
		}()
		s.runValuationWithQuote(ctx, symbol, quoteSnapshot)
	}()
}

func (s *Service) runValuation(parent context.Context, symbol string) {
	s.runValuationWithQuote(parent, symbol, nil)
}

func (s *Service) runValuationWithQuote(parent context.Context, symbol string, initialQuote *model.Quote) {
	quote, found, err := s.store.LatestQuote(symbol)
	if err != nil {
		return
	}
	// A refresh now schedules valuation as soon as the provider quote returns,
	// before the option chain transaction commits. Use that quote as the working
	// snapshot when SQLite still contains the preceding snapshot; otherwise the
	// timestamp guard below would discard the result as soon as the new quote is
	// written.
	if initialQuote != nil && (!found || !sameQuoteTimestamp(quote.FetchedAt, initialQuote.FetchedAt)) {
		if found && quote.FairValue != nil && quote.FairValueSource != "" && !isGoFairValueSource(quote.FairValueSource) {
			return
		}
		quote = *initialQuote
		found = false
	}
	// Never let the incomplete Go decision tree replace a Python valuation that
	// is already persisted on the quote snapshot.
	if found && quote.FairValue != nil && quote.FairValueSource != "" && !isGoFairValueSource(quote.FairValueSource) {
		return
	}
	if _, fresh := s.cachedValuation(symbol); fresh {
		return
	}
	// UPSTREAM_WAIT_SECONDS only bounds waiting for a shared provider slot.
	// Valuation composes fundamentals plus several optional regime-history
	// requests, so using that slot wait as the whole job deadline cancels the
	// calculation before Yahoo can return a usable result in LOW_MEMORY mode.
	deadline := s.cfg.UpstreamTimeout * 6
	if deadline < 60*time.Second {
		deadline = 60 * time.Second
	}
	if deadline > 120*time.Second {
		deadline = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	upstream := s.provider.(fundamentalsProvider)
	fundamentals, err := upstream.Fundamentals(ctx, symbol)
	if err != nil {
		status := "failed"
		if valuationRetryableError(err) {
			status = "retry"
		}
		warning := "Yahoo 数据源暂时限流，将自动重试"
		if status == "failed" {
			warning = "估值计算暂时失败，将在下一次刷新重试"
		}
		s.persistValuationCache(symbol, map[string]any{
			"value": nil, "source": goFairValueSource, "status": status,
			"warning": warning, "warnings": []string{warning},
			"errors": []string{err.Error()},
		})
		return
	}
	if valuation.IsNonCompanyInstrument(fundamentals.QuoteType) {
		// A detected fund classification invalidates old Go company-model values
		// even if the new provider response contains no usable fund NAV data.
		// Leave Python-owned valuations untouched; it remains the authoritative
		// source until Go reaches full parity.
		latest, found, latestErr := s.store.LatestQuote(symbol)
		if latestErr == nil && found && latest.FairValue != nil && latest.FairValueSource != "" && !isGoFairValueSource(latest.FairValueSource) {
			return
		}
		inputs := valuationInputs(symbol, valuationCurrentPrice(fundamentals, latest.Price), fundamentals)
		result := valuation.Calculate(inputs)
		payload := valuationCachePayload(result, inputs)
		s.persistValuationCache(symbol, payload)
		if found && latest.Price != nil && isGoFairValueSource(latest.FairValueSource) {
			clearQuoteValuation(&latest, result)
			latest.Source = "sqlite"
			latest.Provider = "go-valuation"
			valuationFetchedAt := time.Now().UTC()
			latest.FetchedAt = &valuationFetchedAt
			if saved, saveErr := s.store.SaveQuoteReplacingFairValue(latest); saveErr == nil {
				s.recordValuationEvent(symbol, saved)
			}
		} else if initialQuote != nil {
			clearQuoteValuation(initialQuote, result)
			initialQuote.Source = "sqlite"
			initialQuote.Provider = "go-valuation"
			if saved, saveErr := s.store.SaveQuoteReplacingFairValue(*initialQuote); saveErr == nil {
				s.recordValuationEvent(symbol, saved)
			}
		}
		return
	}
	latest, latestFound, latestErr := s.store.LatestQuote(symbol)
	if latestErr != nil {
		return
	}
	if initialQuote != nil && (!latestFound || !sameQuoteTimestamp(latest.FetchedAt, initialQuote.FetchedAt)) {
		// The chain transaction may still be in flight. Keep the fresh quote that
		// triggered this worker instead of calculating valuation against the prior
		// SQLite snapshot; the later quote save will preserve these fields.
		latest = *initialQuote
		latestFound = true
	}
	if found {
		if !latestFound || !sameQuoteTimestamp(quote.FetchedAt, latest.FetchedAt) {
			return
		}
	} else if latestFound {
		quote = latest
	} else if initialQuote != nil {
		quote = *initialQuote
	}
	if latestFound && latest.FairValue != nil && latest.FairValueSource != "" && !isGoFairValueSource(latest.FairValueSource) {
		return
	}
	price := quote.Price
	if latestFound {
		price = latest.Price
	}
	inputs := valuationInputs(symbol, valuationCurrentPrice(fundamentals, price), fundamentals)
	inputs.Regime, inputs.RegimeSignals = s.detectValuationRegime(ctx, symbol, fundamentals)
	result := valuation.Calculate(inputs)
	applyTimeseriesRetryStatus(fundamentals, &result)
	payload := valuationCachePayload(result, inputs)
	if result.FairValue == nil {
		s.persistValuationCache(symbol, payload)
		return
	}
	if !latestFound || latest.Price == nil {
		s.persistValuationCache(symbol, payload)
		s.publishFairValueEvent(symbol)
		return
	}
	enrichQuoteResult(&latest, fundamentals, inputs, result)
	latest.Source = "sqlite"
	latest.Provider = "go-valuation"
	// Write the valuation as a fresh snapshot. Python's latest_quote uses
	// fetched_at-only ordering, so reusing the quote timestamp would leave the
	// older unvalued row selected on an equal-timestamp tie.
	valuationFetchedAt := time.Now().UTC()
	latest.FetchedAt = &valuationFetchedAt
	// Store the shared result before the snapshot so another worker can serve a
	// completed value even if this process is interrupted during SQLite writing.
	s.persistValuationCache(symbol, payload)
	if _, err := s.store.SaveQuotePreservingFairValue(latest); err != nil {
		return
	}
	s.recordValuationEvent(symbol, latest)
}

func clearQuoteValuation(quote *model.Quote, result valuation.Result) {
	if quote == nil {
		return
	}
	quote.FairValue = nil
	quote.FairValueLow, quote.FairValueHigh = nil, nil
	quote.FairValueBuyLow, quote.FairValueBuyHigh = nil, nil
	quote.FairValueSource, quote.FairValueModel = goFairValueSource, ""
	quote.FairValueMethods = map[string]float64{}
	quote.FairValueForwardEPS, quote.FairValueForwardEPSSource = nil, nil
	quote.FairValueNormalizedEPS = nil
	quote.FairValuePELow, quote.FairValuePEHigh = nil, nil
	quote.FairValueDecisionTree, quote.FairValueValidation = nil, nil
	quote.FairValueSafetyMargin = nil
	quote.FairValueConfidence = stringPointer("低")
	quote.FairValueConfidenceScore = float64Pointer(0)
	quote.FairValueRegime, quote.FairValueRegimeSignals = nil, nil
	quote.FairValueModelUnderRegime = nil
	quote.FairValueDefensive, quote.FairValueOptimistic = nil, nil
	quote.FairValueNormalizedEPSSource = nil
	quote.FairValueQuarterlyMomentum, quote.FairValueHistoricalValuationPct = nil, nil
	quote.FairValueShareholderReturnYield = nil
	quote.FairValueMarketCapDataQuality, quote.FairValueDataQualityScore = nil, nil
	quote.FairValueOwnerEarningsMaintRatio = nil
	quote.FairValueOwnerEarningsRatioSource = nil
	applyValuationDiagnostics(quote, result)
}

func (s *Service) recordValuationEvent(symbol string, quote model.Quote) {
	event := model.Event{Symbol: symbol, Kind: "fair_value", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	event.Payload = map[string]any{"quote": quoteAPIValue(quote)}
	if id, eventErr := s.store.AddEvent(event); eventErr == nil {
		event.ID = id
	}
	s.mu.Lock()
	s.quotes[symbol] = cachedQuote{value: quote, at: time.Now()}
	s.appendEventLocked(event)
	s.mu.Unlock()
}

func enrichQuoteResult(quote *model.Quote, fundamentals provider.YahooFundamentals, inputs valuation.Fundamentals, result valuation.Result) {
	quote.FairValue = *result.FairValue
	quote.FairValueLow = pointerAsAny(result.FairValueLow)
	quote.FairValueHigh = pointerAsAny(result.FairValueHigh)
	quote.FairValueBuyLow = pointerAsAny(result.FairValueBuyLow)
	quote.FairValueBuyHigh = pointerAsAny(result.FairValueBuyHigh)
	quote.FairValueSource = goFairValueSource
	quote.FairValueModel = result.Method
	quote.FairValueMethods = result.Methods
	quote.FairValueDefensive = result.Defensive
	quote.FairValueOptimistic = result.Optimistic
	quote.FairValueRegime = stringPointer(result.Regime)
	quote.FairValueRegimeSignals = result.RegimeSignals
	quote.FairValueModelUnderRegime = stringPointer(result.ModelUnderRegime)
	quote.FairValueQuarterlyMomentum = result.QuarterlyMomentum
	quote.FairValueHistoricalValuationPct = result.HistoricalValuationPercentiles
	quote.FairValueInterestCoverage = copyFloatPointer(result.InterestCoverage)
	quote.FairValueDataQualityScore = float64Pointer(result.DataQualityScore)
	quote.FairValueMarketCapDataQuality = stringPointer(result.MarketCapDataQuality)
	if result.NormalizedEPSSource != "" {
		quote.FairValueNormalizedEPSSource = stringPointer(result.NormalizedEPSSource)
	}
	quote.FairValueShareholderReturnYield = copyFloatPointer(result.ShareholderTotalReturnYield)
	quote.FairValueForwardEPS = result.ForwardEPS
	applyV39ResultDiagnostics(quote, result)
	quote.FairValueSafetyMargin = float64Pointer(result.SafetyMargin)
	confidenceScore, confidenceLabel := apiValuationConfidence(inputs, result)
	quote.FairValueConfidence = stringPointer(confidenceLabel)
	quote.FairValueConfidenceScore = float64Pointer(confidenceScore)
	quote.FairValueOwnerEarningsMaintRatio = copyFloatPointer(result.OwnerEarningsMaintenanceRatio)
	if result.OwnerEarningsRatioSource != "" {
		source := result.OwnerEarningsRatioSource
		quote.FairValueOwnerEarningsRatioSource = &source
	}
	quote.FairValueForwardEPSSource = valuationForwardEPSSource(fundamentals, result.ForwardEPS)
	if result.ForwardEPSSource != "" {
		quote.FairValueForwardEPSSource = stringPointer(result.ForwardEPSSource)
	}
	quote.FairValueStatus = apiValuationStatus(result.Status)
	if quote.FairValueStatus == "" {
		quote.FairValueStatus = "ready"
	}
	quote.FairValuePending = false
	quote.FairValueWarning = result.Warning
	quote.FairValueWarnings = append([]string(nil), result.Warnings...)
	quote.FairValueErrors = append([]string(nil), result.Errors...)
}

func optionalString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func sameQuoteTimestamp(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
