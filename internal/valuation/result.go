// Package valuation provides a small, provider-independent fair-value fallback.
//
// It deliberately accepts already-normalized fundamentals. Fetching, caching, and
// industry classification belong to the provider/service layers and are not part
// of this package.
package valuation

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	StatusReady       = "ready"
	StatusUnavailable = "unavailable"
	StatusInvalid     = "invalid"

	MethodOwnerEarnings = "owner_earnings"
	MethodForwardPE     = "forward_eps_pe"
	MethodFCFDCF        = "fcf_dcf"
	MethodREIT          = "reit_ffo_affo_nav"

	MethodREITPriceAFFO = "p_affo"
	MethodREITPriceFFO  = "p_ffo"
	MethodREITNAV       = "reit_nav"

	defaultRequiredReturn        = 0.10
	defaultTerminalGrowth        = 0.015
	defaultPELow                 = 16.0
	defaultPEHigh                = 24.0
	defaultSafetyMargin          = 0.30
	defaultMaintenanceCapexRatio = 0.70
	mobilityStrongSignalCount    = 2
	mobilitySignalMarketCap      = 1e11
	valuationEPSUnitFloor        = 5.0
	valuationEPSUnitCeiling      = 80.0
	valuationPriceFloorRatio     = 0.05
	valuationPriceCeilingRatio   = 20.0

	valueRangeSpread = 0.20
)

const highGrowthInfrastructureWarning = "该标的属于高增长 AI/新能源基础设施赛道，传统 DCF 模型不适用，当前市场定价包含极高的远期预期，模型分歧较大。"

// industryKeyCategoryMap mirrors the stable Yahoo industry_key map used by
// Python v39. The category is deliberately separate from the specialized
// model dispatcher: software, automotive, computer-hardware, and ordinary
// semiconductor keys are useful classification signals but must not by
// themselves force a specialized valuation model.
var industryKeyCategoryMap = map[string]string{
	"semiconductor-memory":         "memory_chip",
	"semiconductors":               "semiconductor",
	"software-infrastructure":      "software",
	"software-application":         "software",
	"auto-manufacturers":           "automotive",
	"computer-hardware":            "computer_hardware",
	"aerospace-defense":            "defense",
	"internet-content-information": "internet_platform",
	"internet-retail":              "internet_platform",
	"reit-residential":             "reit",
	"reit-retail":                  "reit",
	"reit-office":                  "reit",
	"reit-diversified":             "reit",
	"banks-diversified":            "banking",
	"insurance-diversified":        "insurance",
	"biotechnology":                "biotech",
	"utilities-regulated-electric": "utility",
}

func normalizedIndustryKey(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
}

func industryKeyCategory(value string) string {
	return industryKeyCategoryMap[normalizedIndustryKey(value)]
}

// Fundamentals contains the normalized inputs needed by the valuation chain.
// TrailingEPS retains the latest TTM observation; NormalizedEPS is the historical
// earnings anchor used by valuation models. Monetary flow fields are
// total-company values unless their name says per share.
type Fundamentals struct {
	Symbol string
	// InstrumentType is the upstream security classification. Company
	// cash-flow and earnings models do not apply to ETFs and other pooled or
	// non-operating instruments.
	InstrumentType string
	// ProviderBacked marks normalized upstream snapshots even when Yahoo omits
	// industryKey. The provider path still uses sector/industry text for v39
	// candidate filtering and risk classification; direct library callers retain
	// the legacy method-selection behavior unless they opt in.
	ProviderBacked bool
	// TimeseriesRetryable records a transient failure in Yahoo's optional
	// fundamentals-timeseries endpoint. It enables the low-confidence info-page
	// P/S fallback only when the regular valuation models have no candidate.
	TimeseriesRetryable bool
	// UpstreamError records an optional Yahoo source failure that can make the
	// information-page fallback eligible even when the timeseries endpoint worked.
	UpstreamError     bool
	CurrentPrice      *float64
	SharesOutstanding *float64
	TrailingEPS       *float64
	NormalizedEPS     *float64
	Industry          string
	IndustryKey       string
	Sector            string
	Beta              *float64
	RevenueGrowth     *float64
	Regime            string
	RegimeSignals     map[string]any
	HistoricalPrices  []float64

	// Additional normalized inputs used by the industry decision tree. Pointers
	// preserve Yahoo's distinction between an unavailable metric and zero.
	Debt                       *float64
	Cash                       *float64
	Revenue                    *float64
	CurrentYearRevenueEstimate *float64
	NextYearRevenueEstimate    *float64
	CurrentYearRevenueAnalysts int
	NextYearRevenueAnalysts    int
	SharesSource               string
	OperatingCashflow          *float64
	EBITDA                     *float64
	OperatingIncome            *float64
	Equity                     *float64
	QuarterlyRevenue           []float64
	QuarterlyEPS               []float64
	BookValue                  *float64
	TargetLowPrice             *float64
	TargetHighPrice            *float64
	PriceToSales               *float64
	EnterpriseToEBITDA         *float64
	ForwardEBITDA              *float64
	InterestExpense            *float64
	ROE                        *float64
	RABPerShare                *float64
	EmbeddedValuePerShare      *float64
	NewBusinessValuePerShare   *float64
	PipelineRNPV               *float64
	ApprovedDrugValue          *float64
	PlatformUsers              *float64
	ARPU                       *float64
	ChurnRate                  *float64
	AIRevenueShare             *float64

	// Owner earnings can be supplied directly, or derived from the fields below:
	// net income + D&A - maintenance capex - positive change in working capital.
	OwnerEarnings               *float64
	NetIncome                   *float64
	DepreciationAndAmortization *float64
	MaintenanceCapex            *float64
	CapitalExpenditure          *float64
	MaintenanceCapexRatio       *float64
	ChangeInWorkingCapital      *float64

	FreeCashFlow         *float64
	FreeCashFlowPerShare *float64
	ForwardEPS           *float64
	// ForwardEPSObserved distinguishes provider guidance from a historical
	// growth estimate when the service computes API confidence.
	ForwardEPSObserved bool
	GrowthRate         *float64
	// GrowthHintObserved distinguishes Yahoo growth fields from a locally
	// derived historical CAGR for mature-growth classification.
	GrowthHintObserved bool

	// REIT-specific per-share inputs are supplied only when Yahoo publishes
	// usable FFO/AFFO/NAV observations. Missing values leave an identified REIT
	// unavailable rather than using a generic earnings model.
	FFOPerShare                       *float64
	AFFOPerShare                      *float64
	NAVPerShare                       *float64
	IsREIT                            bool
	RiskFreeRate                      *float64
	RequiredReturn                    *float64
	TerminalGrowth                    *float64
	ForwardPELow                      *float64
	ForwardPEHigh                     *float64
	SafetyMargin                      *float64
	NormalizedEPSSource               string
	MaintenanceCapexRatioSource       string
	AnnualEPS                         []float64
	AnnualRevenue                     []float64
	AnnualDebt                        []float64
	AnnualCash                        []float64
	AnnualShares                      []float64
	AnnualOperatingIncome             []float64
	AnnualDepreciationAndAmortization []float64
	AnnualEquity                      []float64
	Buyback                           *float64
	MarketCap                         *float64
	MarketCapDataQuality              string
	// MarketCapWarning is emitted by the provider adapter when Yahoo's market
	// cap and the current price multiplied by shares need reconciliation. Keep
	// the diagnostic separate from the quality enum so the API can preserve the
	// same warning text as Python.
	MarketCapWarning     string
	DividendRate         *float64
	AnalystCount         *float64
	TargetMeanPrice      *float64
	DefenseRevenueGrowth *float64
	LongBusinessSummary  string
}

// Result is the API-neutral fair-value result. Numeric pointers preserve the
// distinction between an unavailable value and a legitimate zero input.
type Result struct {
	FairValue        *float64 `json:"fair_value"`
	FairValueLow     *float64 `json:"fair_value_low"`
	FairValueHigh    *float64 `json:"fair_value_high"`
	FairValueBuyLow  *float64 `json:"fair_value_buy_low"`
	FairValueBuyHigh *float64 `json:"fair_value_buy_high"`
	RiskFreeRate     *float64 `json:"risk_free_rate,omitempty"`
	RequiredReturn   *float64 `json:"required_return,omitempty"`

	// These classification signals are consumed by the API confidence adapter;
	// they are internal metadata and do not change the public valuation payload.
	IndustryModel bool `json:"-"`
	AIExposed     bool `json:"-"`

	Method                         string             `json:"method"`
	Methods                        map[string]float64 `json:"methods,omitempty"`
	Regime                         string             `json:"regime,omitempty"`
	RegimeSignals                  map[string]any     `json:"regime_signals,omitempty"`
	ModelUnderRegime               string             `json:"model_under_regime,omitempty"`
	Defensive                      map[string]any     `json:"defensive,omitempty"`
	Optimistic                     map[string]any     `json:"optimistic,omitempty"`
	QuarterlyMomentum              any                `json:"quarterly_momentum,omitempty"`
	HistoricalValuationPercentiles any                `json:"historical_valuation_percentiles,omitempty"`
	InterestCoverage               *float64           `json:"interest_coverage,omitempty"`
	DataQualityScore               float64            `json:"data_quality_score"`
	NormalizedEPSSource            string             `json:"normalized_eps_source,omitempty"`
	ShareholderTotalReturnYield    *float64           `json:"shareholder_total_return_yield,omitempty"`
	MarketCapDataQuality           string             `json:"market_cap_data_quality,omitempty"`
	OwnerEarningsPerShare          *float64           `json:"owner_earnings_per_share,omitempty"`
	ForwardEPS                     *float64           `json:"forward_eps,omitempty"`
	// The v39 fields below are kept out of the legacy Result JSON. The server
	// adapter maps them into the public valuation payload.
	NormalizedEPS                 *float64        `json:"-"`
	ForwardEPSSource              string          `json:"-"`
	PELow                         *float64        `json:"-"`
	PEHigh                        *float64        `json:"-"`
	DecisionTree                  *DecisionTree   `json:"-"`
	Validation                    CrossValidation `json:"-"`
	SafetyMargin                  float64         `json:"safety_margin"`
	OwnerEarningsMaintenanceRatio *float64        `json:"owner_earnings_maintenance_ratio,omitempty"`
	OwnerEarningsRatioSource      string          `json:"owner_earnings_ratio_source,omitempty"`
	Confidence                    float64         `json:"confidence"`
	ConfidenceLabel               string          `json:"confidence_label"`
	Status                        string          `json:"status"`
	Warning                       string          `json:"warning,omitempty"`
	Warnings                      []string        `json:"warnings,omitempty"`
	Errors                        []string        `json:"errors,omitempty"`
}

// DecisionTree contains the Python v39 industry and lifecycle labels for the
// selected model.
type DecisionTree struct {
	IndustryFilter string `json:"industry_filter"`
	Lifecycle      string `json:"lifecycle"`
	Model          string `json:"model"`
}

// CrossValidation contains independent per-share valuation estimates.
type CrossValidation struct {
	Methods map[string]float64 `json:"methods"`
	Median  *float64           `json:"median"`
}

// ValidationError reports all invalid supplied fields rather than stopping at
// the first malformed value.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "invalid fundamentals"
	}
	return "invalid fundamentals: " + strings.Join(e.Problems, "; ")
}

// Validate checks finite values and basic financial-domain constraints. Missing
// values are allowed because Calculate intentionally supports fallback models.
func (f Fundamentals) Validate() error {
	problems := make([]string, 0)
	finite := func(name string, value *float64) {
		if value != nil && !isFinite(*value) {
			problems = append(problems, name+" must be finite")
		}
	}
	finite("current_price", f.CurrentPrice)
	finite("shares_outstanding", f.SharesOutstanding)
	finite("trailing_eps", f.TrailingEPS)
	finite("normalized_eps", f.NormalizedEPS)
	finite("beta", f.Beta)
	finite("risk_free_rate", f.RiskFreeRate)
	finite("revenue_growth", f.RevenueGrowth)
	finite("owner_earnings", f.OwnerEarnings)
	finite("net_income", f.NetIncome)
	finite("depreciation_and_amortization", f.DepreciationAndAmortization)
	finite("maintenance_capex", f.MaintenanceCapex)
	finite("capital_expenditure", f.CapitalExpenditure)
	finite("maintenance_capex_ratio", f.MaintenanceCapexRatio)
	finite("change_in_working_capital", f.ChangeInWorkingCapital)
	finite("free_cash_flow", f.FreeCashFlow)
	finite("free_cash_flow_per_share", f.FreeCashFlowPerShare)
	finite("forward_eps", f.ForwardEPS)
	finite("growth_rate", f.GrowthRate)
	finite("ffo_per_share", f.FFOPerShare)
	finite("affo_per_share", f.AFFOPerShare)
	finite("nav_per_share", f.NAVPerShare)
	finite("required_return", f.RequiredReturn)
	finite("terminal_growth", f.TerminalGrowth)
	finite("forward_pe_low", f.ForwardPELow)
	finite("forward_pe_high", f.ForwardPEHigh)
	finite("safety_margin", f.SafetyMargin)
	finite("market_cap", f.MarketCap)
	finite("buyback", f.Buyback)
	finite("dividend_rate", f.DividendRate)
	finite("analyst_count", f.AnalystCount)
	finite("target_mean_price", f.TargetMeanPrice)
	finite("target_low_price", f.TargetLowPrice)
	finite("target_high_price", f.TargetHighPrice)
	finite("debt", f.Debt)
	finite("cash", f.Cash)
	finite("revenue", f.Revenue)
	finite("current_year_revenue_estimate", f.CurrentYearRevenueEstimate)
	finite("next_year_revenue_estimate", f.NextYearRevenueEstimate)
	finite("operating_cashflow", f.OperatingCashflow)
	finite("ebitda", f.EBITDA)
	finite("operating_income", f.OperatingIncome)
	finite("equity", f.Equity)
	finite("book_value", f.BookValue)
	finite("price_to_sales", f.PriceToSales)
	finite("enterprise_to_ebitda", f.EnterpriseToEBITDA)
	finite("forward_ebitda", f.ForwardEBITDA)
	finite("interest_expense", f.InterestExpense)
	finite("roe", f.ROE)
	finite("rab_per_share", f.RABPerShare)
	finite("embedded_value_per_share", f.EmbeddedValuePerShare)
	finite("new_business_value_per_share", f.NewBusinessValuePerShare)
	finite("pipeline_rnpv", f.PipelineRNPV)
	finite("approved_drug_value", f.ApprovedDrugValue)
	finite("platform_users", f.PlatformUsers)
	finite("arpu", f.ARPU)
	finite("churn_rate", f.ChurnRate)
	finite("ai_revenue_share", f.AIRevenueShare)
	series := append(append(append(append(append(append([]float64(nil), f.AnnualEPS...), f.AnnualRevenue...), f.AnnualDebt...), f.AnnualCash...), f.AnnualShares...), f.AnnualOperatingIncome...)
	series = append(series, f.AnnualDepreciationAndAmortization...)
	series = append(series, f.AnnualEquity...)
	series = append(series, f.QuarterlyEPS...)
	series = append(series, f.QuarterlyRevenue...)
	for _, value := range series {
		if !isFinite(value) {
			problems = append(problems, "annual series values must be finite")
			break
		}
	}

	positive := func(name string, value *float64) {
		if value != nil && *value <= 0 {
			problems = append(problems, name+" must be greater than zero")
		}
	}
	positive("current_price", f.CurrentPrice)
	positive("shares_outstanding", f.SharesOutstanding)
	positive("forward_pe_low", f.ForwardPELow)
	positive("forward_pe_high", f.ForwardPEHigh)
	if f.MaintenanceCapexRatio != nil && (*f.MaintenanceCapexRatio < 0 || *f.MaintenanceCapexRatio > 1) {
		problems = append(problems, "maintenance_capex_ratio must be between zero and one")
	}
	if f.SafetyMargin != nil && (*f.SafetyMargin < 0 || *f.SafetyMargin >= 1) {
		problems = append(problems, "safety_margin must be between zero inclusive and one exclusive")
	}
	if f.ForwardPELow != nil && f.ForwardPEHigh != nil && *f.ForwardPELow > *f.ForwardPEHigh {
		problems = append(problems, "forward_pe_low must not exceed forward_pe_high")
	}
	if f.RequiredReturn != nil && *f.RequiredReturn <= 0 {
		problems = append(problems, "required_return must be greater than zero")
	}
	if f.Beta != nil && (*f.Beta < 0 || *f.Beta > 10) {
		problems = append(problems, "beta must be between zero and ten")
	}
	return validationResult(problems)
}

func validationResult(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// calculateREIT mirrors the Python REIT branch. FFO is derived only from
// normalized net income, D&A, and shares; no unavailable property-level
// assumptions or generic substitute valuations are invented.
func calculateREIT(f Fundamentals) Result {
	diagnosticInputs := f
	diagnosticInputs.IsREIT = false
	// The generic calculation supplies shared input diagnostics; its candidate
	// values are discarded if the REIT-specific model has no usable inputs.
	diagnosticInputs.IndustryKey = ""
	base := calculateGeneric(diagnosticInputs)

	shares := positivePointer(f.SharesOutstanding)
	ffo := positivePointer(f.FFOPerShare)
	derivedFFO := false
	if ffo == nil && shares != nil && f.NetIncome != nil && isFinite(*f.NetIncome) {
		depreciation := 0.0
		if f.DepreciationAndAmortization != nil && isFinite(*f.DepreciationAndAmortization) {
			depreciation = math.Abs(*f.DepreciationAndAmortization)
		}
		derived := (*f.NetIncome + depreciation) / *shares
		if isPositiveFinite(derived) {
			ffo = &derived
			derivedFFO = true
		}
	}
	affo := positivePointer(f.AFFOPerShare)
	affoDerived := false
	if affo == nil && ffo != nil {
		derived := *ffo * 0.85
		if isPositiveFinite(derived) {
			affo = &derived
			affoDerived = true
		}
	}
	nav := positivePointer(f.NAVPerShare)

	methods := make(map[string]float64, 3)
	if affo != nil {
		methods[MethodREITPriceAFFO] = roundCurrency(*affo * 15.0)
	}
	if ffo != nil {
		methods[MethodREITPriceFFO] = roundCurrency(*ffo * 14.0)
	}
	if nav != nil {
		methods[MethodREITNAV] = roundCurrency(*nav)
	}
	if len(methods) == 0 {
		warning := "REIT-specific FFO/AFFO/NAV data unavailable; valuation was not generated"
		base.Method = ""
		base.Methods = methods
		base.Status = StatusUnavailable
		base.Warning = warning
		base.Warnings = []string{warning}
		base.FairValue = nil
		base.FairValueLow = nil
		base.FairValueHigh = nil
		base.FairValueBuyLow = nil
		base.FairValueBuyHigh = nil
		base.Defensive = nil
		base.Optimistic = nil
		base.Confidence = 0
		base.ConfidenceLabel = "low"
		base.OwnerEarningsPerShare = nil
		base.ModelUnderRegime = ""
		return base
	}

	low, high := 0.0, 0.0
	if affo != nil {
		low, high = *affo*12.0, *affo*18.0
	}
	if nav != nil {
		navLow, navHigh := *nav*0.85, *nav*1.15
		if low <= 0 || high <= 0 {
			// NAV-only REITs still get a usable industry range.
			low, high = navLow, navHigh
		} else {
			// Python combines REIT ranges conservatively by taking the stronger
			// lower bound and stronger upper bound independently. This can narrow
			// an AFFO range when NAV is the tighter cross-check; it is not a union.
			low, high = math.Max(low, navLow), math.Max(high, navHigh)
		}
	}
	if low <= 0 || high <= 0 || high < low {
		return base
	}
	value := roundCurrency((low + high) / 2.0)
	margin := valueOr(f.SafetyMargin, 0.25)
	if margin < 0 || margin >= 1 || !isFinite(margin) {
		margin = 0.25
	}

	result := base
	result.FairValue = floatPointer(value)
	result.FairValueLow = floatPointer(roundCurrency(low))
	result.FairValueHigh = floatPointer(roundCurrency(high))
	result.FairValueBuyLow = floatPointer(roundCurrency(low * (1 - margin)))
	result.FairValueBuyHigh = floatPointer(roundCurrency(high * (1 - margin)))
	result.Method = MethodREIT
	result.Methods = methods
	result.SafetyMargin = margin
	result.Status = StatusReady
	result.Warning = "used REIT FFO/AFFO/NAV valuation; property-level data was not available"
	result.Warnings = []string{result.Warning}
	if derivedFFO {
		result.Warnings = append(result.Warnings, "FFO was derived from net income plus depreciation and amortization")
	}
	if affoDerived {
		result.Warnings = append(result.Warnings, "AFFO was unavailable; estimated at 85% of FFO")
	}
	result.Confidence = 0.75
	result.ConfidenceLabel = confidenceLabel(result.Confidence)
	result.OwnerEarningsPerShare = nil
	result.OwnerEarningsMaintenanceRatio = nil
	result.OwnerEarningsRatioSource = ""
	result.Defensive = scenario(value, .22, "REIT：P/FFO + P/AFFO + NAV 压力测试", margin)
	result.Optimistic = reitScenario(low, high, margin, "REIT：P/FFO + P/AFFO + NAV")
	result.Optimistic["forward_eps"] = nil
	if base.ForwardEPS != nil && isPositiveFinite(*base.ForwardEPS) {
		result.Optimistic["forward_eps"] = roundDecimal(*base.ForwardEPS, 4)
	}
	result.Optimistic["growth_rate"] = roundDecimal(clamp(valueOr(valuationGrowthRate(f), 0), 0, .50), 4)
	return result
}

func reitScenario(low, high, margin float64, model string) map[string]any {
	return map[string]any{
		"value": roundCurrency((low + high) / 2.0),
		"low":   roundCurrency(low), "high": roundCurrency(high),
		"buy_low": roundCurrency(low * (1 - margin)), "buy_high": roundCurrency(high * (1 - margin)),
		"model": model, "safety_margin": roundCurrency(margin),
	}
}

func positivePointer(value *float64) *float64 {
	if value == nil || !isPositiveFinite(*value) {
		return nil
	}
	copy := *value
	return &copy
}

func normalizedEPSValue(f Fundamentals) *float64 {
	if normalized := positivePtr(f.NormalizedEPS); normalized != nil {
		return normalized
	}
	return positivePtr(f.TrailingEPS)
}

func cyclicalEPS(f Fundamentals, normalizedEPS *float64) *float64 {
	cycleEPS := copyPointer(normalizedEPS)
	history := make([]float64, 0, 6)
	if trailing := positivePtr(f.TrailingEPS); trailing != nil {
		history = append(history, *trailing)
	}
	annualCount := 0
	for _, value := range f.AnnualEPS {
		if value := positivePtr(&value); value != nil {
			history = append(history, *value)
			annualCount++
			if annualCount == 5 {
				break
			}
		}
	}
	if len(history) >= 3 {
		sort.Float64s(history)
		history = history[:len(history)-1]
		cycleEPS = floatPointer(history[len(history)/2])
	}
	return cycleEPS
}

// Calculate validates normalized fundamentals and selects the available
// industry-specific or generic model from normalized company metadata.

func Calculate(f Fundamentals) Result {
	if err := f.Validate(); err != nil {
		return invalidResult(err)
	}
	if isNonCompanyInstrument(f.InstrumentType) {
		warning := strings.ToUpper(strings.TrimSpace(f.InstrumentType)) + " is not an operating company; company-based DCF, normalized PE, and PEG valuation is not applicable"
		return Result{
			Status: StatusUnavailable, Warning: warning, Warnings: []string{warning},
			ConfidenceLabel: "low", Methods: map[string]float64{},
		}
	}
	f.Regime = normalizeRegimeName(f.Regime)
	if growth := valuationGrowthRate(f); growth != nil {
		f.GrowthRate = growth
	}
	if isAICloudInfrastructure(f) && !isHighGrowthInfrastructure(f) {
		return calculateAICloudInfrastructure(f)
	}
	var result Result
	if f.IsREIT {
		result = calculateREIT(f)
		result.IndustryModel = true
	} else if specialized, ok := calculateSpecialized(f); ok {
		// Python classification also uses sector, industry, and the business
		// summary when Yahoo omits industryKey. Run the text-backed specialist
		// dispatcher for those records too; unknown descriptions still fall
		// through to the generic calculation.
		result = specialized
	} else {
		result = calculateGeneric(f)
		result.AIExposed = genericAIExposed(f)
	}
	if result.FairValue == nil && (f.TimeseriesRetryable || f.UpstreamError) {
		result = applyInformationPagePSFallback(f, result)
	}
	result = withV39Diagnostics(result, f)
	result = withMarketCapWarning(result, f)
	return withValuationRates(result, f)
}

func isAICloudInfrastructure(f Fundamentals) bool {
	text := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+f.IndustryKey+" "+f.LongBusinessSummary, "_", " "))
	cloudSignal := containsAny(text, "ai cloud", "ai-cloud", "gpu cloud", "gpu clusters", "cloud infrastructure for ai", "cloud platforms for ai")
	if !cloudSignal {
		return false
	}
	if f.FreeCashFlow == nil || *f.FreeCashFlow >= 0 {
		return false
	}
	if f.CapitalExpenditure == nil || f.OperatingCashflow == nil || math.Abs(*f.CapitalExpenditure) < math.Abs(*f.OperatingCashflow)*.70 {
		return false
	}
	return positivePtr(f.SharesOutstanding) != nil && positivePtr(f.Revenue) != nil &&
		positivePtr(f.NextYearRevenueEstimate) != nil && f.NextYearRevenueAnalysts >= 3
}

func calculateAICloudInfrastructure(f Fundamentals) Result {
	shares := *positivePtr(f.SharesOutstanding)
	if f.CurrentPrice != nil && *f.CurrentPrice > 0 && f.MarketCap != nil && *f.MarketCap > 0 {
		// The annual diluted-share time series can lag current share issuance.
		// A current market cap and quote provide a fresher outstanding-share
		// proxy; use it when it implies more shares than the annual series.
		marketCapShares := *f.MarketCap / *f.CurrentPrice
		if isPositiveFinite(marketCapShares) && marketCapShares > shares {
			shares = marketCapShares
		}
	}
	spotRevenue := *positivePtr(f.Revenue)
	currentYear := valueOr(f.CurrentYearRevenueEstimate, 0)
	nextYear := valueOr(f.NextYearRevenueEstimate, 0)
	if currentYear <= spotRevenue {
		currentYear = spotRevenue
	}
	if nextYear <= 0 {
		nextYear = currentYear
	}
	if nextYear < currentYear {
		nextYear = currentYear
	}
	debt, cash := valueOr(f.Debt, 0), valueOr(f.Cash, 0)
	// The large GPU build-out consumes cash before it can produce mature
	// margins. Use enterprise value/revenue ranges and deduct net debt rather
	// than applying DCF or PEG to negative forward earnings.
	defensiveRevenue := currentYear
	if f.CurrentYearRevenueAnalysts < 3 && nextYear > 0 {
		defensiveRevenue = spotRevenue + (currentYear-spotRevenue)*.75
	}
	defensiveLowEV, defensiveHighEV := defensiveRevenue*8, defensiveRevenue*12
	defensiveLow := math.Max(0, (defensiveLowEV-debt+cash)/shares)
	defensiveHigh := math.Max(defensiveLow, (defensiveHighEV-debt+cash)/shares)
	defensiveMid := (defensiveLow + defensiveHigh) / 2
	optimisticLowEV, optimisticHighEV := nextYear*10, nextYear*15
	optimisticLow := math.Max(defensiveLow, math.Max(0, (optimisticLowEV-debt+cash)/shares))
	optimisticHigh := math.Max(optimisticLow, (optimisticHighEV-debt+cash)/shares)
	optimisticMid := (optimisticLow + optimisticHigh) / 2
	// The market price and analyst targets are not model inputs. The conservative
	// case uses this year's estimate and lower multiples; the optimistic case
	// uses next year's estimate and higher multiples.
	defensiveMargin, optimisticMargin := .40, .45
	defensive := map[string]any{
		"value": roundCurrency(defensiveMid), "low": roundCurrency(defensiveLow), "high": roundCurrency(defensiveHigh),
		"buy_low": roundCurrency(defensiveLow * (1 - defensiveMargin)), "buy_high": roundCurrency(defensiveHigh * (1 - defensiveMargin)),
		"model": "AI 云基础设施：本年收入 EV/Revenue 8–12x（扣除净债务）", "safety_margin": defensiveMargin,
		"revenue": roundCurrency(spotRevenue), "current_year_revenue_estimate": roundCurrency(currentYear),
		"analyst_count": f.CurrentYearRevenueAnalysts,
	}
	optimistic := map[string]any{
		"value": roundCurrency(optimisticMid), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
		"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
		"model": "AI 云基础设施：下一年收入 EV/Revenue 10–15x（扣除净债务）", "safety_margin": optimisticMargin,
		"current_year_revenue_estimate": roundCurrency(currentYear), "next_year_revenue_estimate": roundCurrency(nextYear),
		"analyst_count": f.NextYearRevenueAnalysts,
	}
	methods := map[string]float64{
		"本年收入 EV/Revenue 10x":    roundCurrency((defensiveRevenue*10 - debt + cash) / shares),
		"下一年收入 EV/Revenue 12.5x": roundCurrency((nextYear*12.5 - debt + cash) / shares),
	}
	value := methods["本年收入 EV/Revenue 10x"]
	result := Result{
		Method: "AI 云基础设施：前瞻 EV/Revenue 情景", Methods: methods,
		FairValue: floatPointer(roundCurrency(value)), FairValueLow: floatPointer(roundCurrency(defensiveLow)), FairValueHigh: floatPointer(roundCurrency(defensiveHigh)),
		FairValueBuyLow: floatPointer(roundCurrency(defensiveLow * (1 - defensiveMargin))), FairValueBuyHigh: floatPointer(roundCurrency(defensiveHigh * (1 - defensiveMargin))),
		Defensive: defensive, Optimistic: optimistic, Status: StatusReady, Confidence: .52, ConfidenceLabel: "medium",
		SafetyMargin: defensiveMargin, AIExposed: true, IndustryModel: true,
		Warnings:         []string{"公司处于 GPU 基础设施重投资阶段，负自由现金流使 DCF/PEG 不适用；收入预测受资本开支兑现和融资需求影响较大"},
		Warning:          "公司处于 GPU 基础设施重投资阶段，负自由现金流使 DCF/PEG 不适用；收入预测受资本开支兑现和融资需求影响较大",
		DataQualityScore: dataQualityScore(f), MarketCapDataQuality: marketCapDataQuality(f),
		InterestCoverage: interestCoverage(f), ShareholderTotalReturnYield: shareholderBuybackYield(f),
		Regime: f.Regime, RegimeSignals: f.RegimeSignals, ModelUnderRegime: "AI cloud EV/Revenue (" + f.Regime + ")",
	}
	result = withV39Diagnostics(result, f)
	result.DecisionTree = &DecisionTree{IndustryFilter: "ai_cloud_infrastructure", Lifecycle: "高增长重资本扩张", Model: result.Method}
	result.Validation = crossValidate(f)
	result = withMarketCapWarning(result, f)
	return withValuationRates(result, f)
}

func isNonCompanyInstrument(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ETF", "MUTUALFUND", "CLOSED_END_FUND", "MONEYMARKET", "INDEX", "BOND", "FUTURE", "CURRENCY", "CRYPTOCURRENCY", "OPTION":
		return true
	default:
		return false
	}
}

// IsNonCompanyInstrument reports whether the source identifies a pooled or
// non-operating security for which this package's company valuation models do
// not apply.
func IsNonCompanyInstrument(value string) bool { return isNonCompanyInstrument(value) }

func providerBackedFundamentals(f Fundamentals) bool {
	return f.ProviderBacked || strings.TrimSpace(f.IndustryKey) != ""
}

// withMarketCapWarning keeps the adapter's reconciliation diagnostic in the
// same warnings array that Python exposes in its valuation payload. It is
// applied after every model branch, including specialized and unavailable
// results, so a model choice cannot accidentally hide a data-quality warning.
func withMarketCapWarning(result Result, f Fundamentals) Result {
	warning := strings.TrimSpace(f.MarketCapWarning)
	if warning == "" {
		warning = marketCapWarning(f)
	}
	if warning == "" {
		return result
	}
	for _, existing := range result.Warnings {
		if existing == warning {
			return result
		}
	}
	result.Warnings = append(result.Warnings, warning)
	if strings.TrimSpace(result.Warning) == "" {
		result.Warning = warning
	}
	return result
}

func valuationGrowthRate(f Fundamentals) *float64 {
	if f.GrowthRate != nil {
		growth := clamp(*f.GrowthRate, -.15, .50)
		return &growth
	}
	if f.RevenueGrowth != nil {
		growth := clamp(*f.RevenueGrowth, -.15, .50)
		return &growth
	}
	if growth := historicalGrowthRate(f.AnnualEPS); growth != nil {
		return growth
	}
	return historicalGrowthRate(f.AnnualRevenue)
}

func historicalGrowthRate(values []float64) *float64 {
	positive := make([]float64, 0, len(values))
	for _, value := range values {
		if isPositiveFinite(value) {
			positive = append(positive, value)
		}
	}
	if len(positive) < 2 {
		return nil
	}
	growth := math.Pow(positive[0]/positive[len(positive)-1], 1/float64(len(positive)-1)) - 1
	if !isFinite(growth) {
		return nil
	}
	growth = clamp(growth, -.15, .50)
	return &growth
}

func withV39Diagnostics(result Result, f Fundamentals) Result {
	if normalizedEPS := normalizedEPSValue(f); normalizedEPS != nil {
		rounded := roundDecimal(*normalizedEPS, 4)
		result.NormalizedEPS = &rounded
	}
	if result.ForwardEPS != nil {
		if f.ForwardEPSObserved && positivePtr(f.ForwardEPS) != nil {
			result.ForwardEPSSource = "Yahoo远期EPS"
		} else {
			result.ForwardEPSSource = "模型估计远期EPS"
		}
		if normalizeRegimeName(f.Regime) == RegimeSlowdown && result.AIExposed {
			result.ForwardEPSSource += "（放缓调整70%）"
		}
	}
	result.PELow, result.PEHigh = valuationPEBounds(result, f)
	result.DecisionTree = valuationDecisionTree(result, f)
	result.Validation = crossValidate(f)
	return result
}

func valuationPEBounds(result Result, f Fundamentals) (*float64, *float64) {
	model := result.Method
	low, high := 0.0, 0.0
	switch {
	case strings.Contains(model, "AI 存储/硬件周期"):
		low, high = 20, 30
		if f.Regime == RegimeWinter {
			low, high = 8, 12
		}
	case strings.Contains(model, "AI 存储结构性增长"):
		low, high = 35, 55
	case strings.Contains(model, "周期中值 EPS × PE"):
		low, high = 10, 15
	case strings.Contains(model, "AI寒冬"):
		low, high = 8, 12
	case strings.Contains(model, "Forward EPS × PEG"):
		peMid := clamp(valueOr(f.GrowthRate, 0)*100*1.05, 22, 45)
		low, high = math.Max(18, peMid-5), math.Min(50, peMid+5)
	case model == MethodForwardPE:
		low, high = valueOr(f.ForwardPELow, defaultPELow), valueOr(f.ForwardPEHigh, defaultPEHigh)
	case strings.Contains(model, "远期 EPS × 15–25 倍 PE"):
		low, high = 15, 25
	case strings.Contains(model, "移动科技成长：远期 EPS × PE"):
		low, high = 30, 50
		if f.Regime == RegimeSlowdown {
			low, high = 22, 32
		}
	case strings.Contains(model, "高增长芯片"):
		low, high = 20, 35
	case strings.Contains(model, "成熟增长型巨头"):
		low, high = 20, 35
		maturityGrowth := valueOr(f.GrowthRate, 0)
		if f.RevenueGrowth != nil {
			maturityGrowth = *f.RevenueGrowth
		}
		if marketCap, ok := effectiveMarketCap(f); ok && marketCap >= 1e12 && maturityGrowth >= .08 {
			low, high = 30, 38
		}
	case strings.Contains(model, "稳定现金流"):
		low, high = 20, 35
	case strings.Contains(model, "重资产消费服务"):
		low, high = 10, 15
	case (model == MethodOwnerEarnings || model == MethodFCFDCF) && result.ForwardEPS != nil:
		low, high = valueOr(f.ForwardPELow, defaultPELow), valueOr(f.ForwardPEHigh, defaultPEHigh)
	default:
		return nil, nil
	}
	return floatPointer(roundCurrency(low)), floatPointer(roundCurrency(high))
}

func valuationDecisionTree(result Result, f Fundamentals) *DecisionTree {
	model := result.Method
	filter := "普通行业"
	lifecycle := "普通"
	industryKey := normalizedIndustryKey(f.IndustryKey)
	keyCategory := industryKeyCategory(industryKey)
	industryKeyText := strings.ReplaceAll(industryKey, "-", " ")
	industryContext := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+industryKeyText, "_", " "))
	if result.IndustryModel {
		switch {
		case strings.Contains(model, "高增长 AI/新能源基础设施") || strings.Contains(model, "高增长基础设施"):
			filter, lifecycle = "high_growth_infrastructure", "高增长基础设施"
		case strings.Contains(model, "银行"):
			filter, lifecycle = "banking", "banking"
		case strings.Contains(model, "医疗保险计划"):
			filter, lifecycle = "healthcare_plans", "healthcare_plans"
		case strings.Contains(model, "医疗保险计划"):
			filter, lifecycle = "healthcare_plans", "healthcare_plans"
		case strings.Contains(model, "保险"):
			filter, lifecycle = "insurance", "insurance"
		case f.IsREIT || keyCategory == "reit" || strings.Contains(strings.ToLower(model), "reit"):
			filter, lifecycle = "reit", "reit"
		case strings.Contains(model, "生物科技"):
			filter, lifecycle = "biotech", "biotech"
		case strings.Contains(model, "能源/矿业"):
			filter, lifecycle = "energy_mining", "energy_mining"
		case strings.Contains(model, "公用事业"):
			filter, lifecycle = "utility", "utility"
		case strings.Contains(model, "互联网平台"):
			filter, lifecycle = "internet_platform", "internet_platform"
		}
	}
	financialOrRealEstate := f.IsREIT || containsAny(industryContext,
		"financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
	if lifecycle == "普通" && financialOrRealEstate {
		filter, lifecycle = "金融/地产", "金融/地产"
	}
	if lifecycle == "普通" {
		industryKeyText := strings.ReplaceAll(industryKey, "-", " ")
		classificationText := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+industryKeyText+" "+f.LongBusinessSummary, "_", " "))
		growth := valueOr(f.GrowthRate, 0)
		if f.GrowthRate == nil && f.RevenueGrowth != nil {
			growth = *f.RevenueGrowth
		}
		growth = clamp(growth, -.15, .50)
		revenueGrowth := valueOr(f.RevenueGrowth, growth)
		freeCashFlowMargin := 0.0
		if f.FreeCashFlow != nil && f.Revenue != nil && *f.Revenue > 0 {
			freeCashFlowMargin = *f.FreeCashFlow / *f.Revenue
		}
		forwardEPSObserved := f.ForwardEPSObserved && positivePtr(f.ForwardEPS) != nil
		forwardEPS := positivePtr(f.ForwardEPS)
		normalizedEPS := normalizedEPSValue(f)
		memory := keyCategory == "memory_chip" || containsAny(classificationText, "memory", "dram", "nand", "flash")
		storageHardware := containsAny(classificationText,
			"hard disk", "disk drive", "hdd", "nand", "flash memory", "solid state drive", "ssd", "memory chip", "storage chip", "storage device", "storage hardware", "storage products", "storage systems", "enterprise storage", "computer storage") ||
			(containsAny(classificationText, "computer hardware") && containsAny(classificationText, "storage", "disk"))
		mobilityTechnology := isMobilityTechnology(f, classificationText, industryContext)
		defenseTag := keyCategory == "defense" || containsAny(classificationText, "defense", "aerospace", "laser", "military", "directed energy")
		defenseTransition := (f.DefenseRevenueGrowth != nil && isFinite(*f.DefenseRevenueGrowth) && *f.DefenseRevenueGrowth >= .30) ||
			(defenseTag && (growth >= .10 || revenueGrowth >= .10 || (f.FreeCashFlow != nil && *f.FreeCashFlow < 0)))
		classifiedAIStorageCycle := !financialOrRealEstate && !mobilityTechnology &&
			(memory || (storageHardware && (growth >= .20 || revenueGrowth >= .20 ||
				(forwardEPS != nil && normalizedEPS != nil && *forwardEPS >= *normalizedEPS*1.25))))
		aiStorageCycle := classifiedAIStorageCycle
		if aiStorageCycle && !forwardEPSObserved {
			aiStorageCycle = false
		}
		cyclical := aiStorageCycle || containsAny(industryContext,
			"semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline")
		negativeFCF := f.FreeCashFlow != nil && *f.FreeCashFlow < 0
		capexHeavy := f.CapitalExpenditure != nil && f.OperatingCashflow != nil &&
			math.Abs(*f.CapitalExpenditure) >= math.Abs(*f.OperatingCashflow)*.70
		cruiseOperator := containsAny(classificationText, "cruise", "cruise lines", "hotels resorts cruise", "travel services", "resorts")
		assetHeavyTransition := !financialOrRealEstate && !defenseTransition && !classifiedAIStorageCycle && !memory && !cruiseOperator &&
			(negativeFCF || capexHeavy)
		priceToSales := f.PriceToSales == nil || *f.PriceToSales > 10
		highGrowthChipSignal := !financialOrRealEstate && !mobilityTechnology && !defenseTag && !memory &&
			!classifiedAIStorageCycle && !cruiseOperator && !assetHeavyTransition &&
			containsAny(classificationText, "semiconductor", "ai chip", "graphics processor") && growth >= .25 && priceToSales
		aiExposed := highGrowthChipSignal || classifiedAIStorageCycle || storageHardware ||
			containsAny(classificationText, "semiconductor", "ai chip", "graphics processor") || mobilityTechnology ||
			hasDefaultAIRevenueExposure(f, classificationText)
		aiWinter := normalizeRegimeName(f.Regime) == RegimeWinter && aiExposed
		aiStorageGrowth := aiStorageCycle && storageHardware && !memory &&
			containsAny(classificationText, "hard disk", "disk drive", "enterprise storage") && freeCashFlowMargin >= .15 &&
			(growth >= .30 || revenueGrowth >= .30)
		mobilityGrowth := isMobilityTechnologyGrowth(f, classificationText, mobilityTechnology,
			effectiveMarketCapValue(f), financialOrRealEstate, result.IndustryModel, defenseTransition, aiWinter)
		if aiWinter {
			aiStorageCycle = false
			aiStorageGrowth = false
			assetHeavyTransition = false
			highGrowthChipSignal = false
			mobilityGrowth = false
			cyclical = true
		}
		if defenseTransition {
			lifecycle = "国防订单驱动转型"
		} else if aiStorageGrowth {
			lifecycle = "AI存储结构性增长"
		} else if aiStorageCycle {
			lifecycle = "AI存储/硬件周期"
		} else if mobilityGrowth {
			lifecycle = "移动科技成长"
		} else if assetHeavyTransition {
			lifecycle = "重资产转型"
		} else if highGrowthChipSignal && forwardEPSObserved {
			lifecycle = "高增长芯片"
		} else if cyclical {
			lifecycle = "强周期"
		} else if !financialOrRealEstate &&
			containsAny(classificationText, "consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage") &&
			growth < .15 && freeCashFlowMargin > .15 {
			lifecycle = "稳定现金流"
		}
	}
	return &DecisionTree{IndustryFilter: filter, Lifecycle: lifecycle, Model: model}
}

func crossValidate(f Fundamentals) CrossValidation {
	methods := make(map[string]float64, 4)
	shares := positivePointer(f.SharesOutstanding)
	if shares == nil && len(f.AnnualShares) > 0 {
		shares = positivePointer(&f.AnnualShares[0])
	}
	if shares == nil {
		return CrossValidation{Methods: methods}
	}
	fcf := positivePointer(f.FreeCashFlow)
	debt, cash := valueOr(f.Debt, 0), valueOr(f.Cash, 0)
	if f.Debt == nil && len(f.AnnualDebt) > 0 {
		debt = f.AnnualDebt[0]
	}
	if f.Cash == nil && len(f.AnnualCash) > 0 {
		cash = f.AnnualCash[0]
	}
	priceAnchor := valueOr(f.CurrentPrice, 0)
	if priceAnchor <= 0 && f.MarketCap != nil && *f.MarketCap > 0 {
		priceAnchor = *f.MarketCap / *shares
	}
	addMethod := func(name string, candidate float64) {
		if !isPositiveFinite(candidate) || (priceAnchor > 0 && candidate < priceAnchor*valuationPriceFloorRatio) {
			return
		}
		methods[name] = roundCurrency(candidate)
	}
	if fcf != nil {
		ownerEarnings := *fcf
		if supplied := positivePointer(f.OwnerEarnings); supplied != nil {
			ownerEarnings = *supplied
		}
		addMethod("所有者收益", ownerEarnings*18 / *shares)
		addMethod("FCF收益率", *fcf/.06 / *shares)
	}
	operatingIncome := positivePointer(f.OperatingIncome)
	if operatingIncome == nil && len(f.AnnualOperatingIncome) > 0 {
		operatingIncome = positivePointer(&f.AnnualOperatingIncome[0])
	}
	if operatingIncome != nil {
		addMethod("EV/EBIT", (*operatingIncome*15-debt+cash) / *shares)
	}
	if fcf != nil && debt >= 0 {
		discount := valueOr(f.RequiredReturn, .10)
		growth := .02
		if isHighGrowthInfrastructure(f) {
			growth = valuationDCFRate(f, valueOr(f.RevenueGrowth, valueOr(f.GrowthRate, growth)))
		}
		if discount > .015 {
			fcfPerShare := *fcf / *shares
			candidate := dcfPerShare(fcfPerShare, growth, discount, .015) + (cash-debt) / *shares
			addMethod("DCF", candidate)
		}
	}
	valid := make([]float64, 0, len(methods))
	for _, value := range methods {
		if isPositiveFinite(value) {
			valid = append(valid, value)
		}
	}
	if len(valid) == 0 {
		return CrossValidation{Methods: methods}
	}
	sort.Float64s(valid)
	median := valid[len(valid)/2]
	return CrossValidation{Methods: methods, Median: floatPointer(roundCurrency(median))}
}

func applyInformationPagePSFallback(f Fundamentals, result Result) Result {
	if len(result.Methods) != 0 || f.Revenue == nil || !isPositiveFinite(*f.Revenue) ||
		f.SharesOutstanding == nil || !isPositiveFinite(*f.SharesOutstanding) {
		return result
	}
	highGrowthInfrastructure := isHighGrowthInfrastructure(f)
	var value, spread float64
	if highGrowthInfrastructure {
		forwardRevenue := highGrowthInfrastructureForwardRevenue(f)
		low := (forwardRevenue*10 - valueOr(f.Debt, 0) + valueOr(f.Cash, 0)) / *f.SharesOutstanding
		high := (forwardRevenue*15 - valueOr(f.Debt, 0) + valueOr(f.Cash, 0)) / *f.SharesOutstanding
		if !isPositiveFinite(low) || !isPositiveFinite(high) {
			return result
		}
		value = (low + high) / 2
		spread = (high - low) / math.Max(high+low, 1)
	} else {
		priceToSales := valueOr(f.PriceToSales, 2)
		if !isPositiveFinite(priceToSales) {
			priceToSales = 2
		}
		priceToSales = clamp(priceToSales*.65, .75, 4)
		value = (*f.Revenue*priceToSales - valueOr(f.Debt, 0) + valueOr(f.Cash, 0)) / *f.SharesOutstanding
		spread = .22
		text := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+f.IndustryKey, "_", " "))
		if containsAny(text,
			"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores") {
			spread = .15
		}
	}
	if !isPositiveFinite(value) {
		return result
	}
	candidateValue := roundCurrency(value)
	priceAnchor := valueOr(f.CurrentPrice, 0)
	if priceAnchor <= 0 && f.MarketCap != nil && *f.MarketCap > 0 {
		priceAnchor = *f.MarketCap / *f.SharesOutstanding
	}
	warning := "部分 Yahoo 财务接口暂时不可用，已使用可用字段生成估值"
	if priceAnchor > 0 && value > priceAnchor*5 {
		value = priceAnchor * 1.5
		warning = "模型估值远高于现价，Yahoo 财务数据口径异常，已降级到现价锚定"
	}
	value = roundCurrency(value)
	low, high := roundCurrency(value*(1-spread)), roundCurrency(value*(1+spread))
	margin := informationFallbackSafetyMargin(f)
	if result.Methods == nil {
		result.Methods = make(map[string]float64)
	}
	result.Method = "信息页 P/S 低置信度兜底"
	if highGrowthInfrastructure {
		result.Method = "高增长基础设施：前瞻 P/S 10–15 倍低置信度兜底"
	}
	result.Methods[result.Method] = candidateValue
	result.FairValue = floatPointer(value)
	result.FairValueLow, result.FairValueHigh = floatPointer(low), floatPointer(high)
	result.FairValueBuyLow = floatPointer(roundCurrency(low * (1 - margin)))
	result.FairValueBuyHigh = floatPointer(roundCurrency(high * (1 - margin)))
	result.Status = StatusReady
	result.Confidence = .25
	result.ConfidenceLabel = "low"
	result.SafetyMargin = margin
	result.Defensive = scenario(value, spread, result.Method, margin)
	result.Optimistic = scenario(value, spread, result.Method, margin)
	if highGrowthInfrastructure {
		result.Defensive = nil
		result.Optimistic = specializedScenario(low, high, result.Method, margin)
		result.IndustryModel = true
	}
	if result.Warning != "" {
		filtered := result.Warnings[:0]
		for _, existing := range result.Warnings {
			if existing != result.Warning {
				filtered = append(filtered, existing)
			}
		}
		result.Warnings = filtered
	}
	result.Warning = ""
	result.Warnings = append(result.Warnings, warning)
	if highGrowthInfrastructure {
		result.Warnings = append(result.Warnings, highGrowthInfrastructureWarning)
		result.Warning = warning + "; " + highGrowthInfrastructureWarning
	}
	return result
}

// 有限寿命 NAV 仅适用于资源企业，不用泛化的 energy 标签推断。
func isEnergyMiningBusiness(f Fundamentals) bool {
	keyText := strings.ReplaceAll(normalizedIndustryKey(f.IndustryKey), "-", " ")
	industryContext := strings.ToLower(strings.ReplaceAll(
		strings.TrimSpace(f.Sector+" "+f.Industry+" "+keyText), "_", " "))
	classificationText := strings.ToLower(strings.ReplaceAll(
		strings.TrimSpace(industryContext+" "+f.LongBusinessSummary), "_", " "))

	if containsAny(classificationText,
		"energy producer", "energy exploration", "oil producer", "oil exploration", "oil and gas", "oil & gas",
		"natural gas producer", "natural gas exploration", "coal mining", "uranium mining", "uranium producer",
		"gold mining", "gold producer", "silver mining", "silver producer", "copper mining", "copper producer",
		"mining company", "mineral exploration", "metal mining") {
		return true
	}
	if containsAny(industryContext, "utility", "utilities", "regulated electric", "water utilities", "gas utilities") {
		return false
	}
	return containsAny(industryContext, "oil", "gas", "coal", "uranium", "mining", "gold", "silver", "copper")
}

func isRegulatedUtilityBusiness(f Fundamentals) bool {
	key := normalizedIndustryKey(f.IndustryKey)
	switch key {
	case "utilities-regulated-electric", "utilities-regulated-gas", "utilities-regulated-water", "utility":
		return true
	}
	keyText := strings.ReplaceAll(key, "-", " ")
	industryContext := strings.ToLower(strings.ReplaceAll(
		strings.TrimSpace(f.Sector+" "+f.Industry+" "+keyText), "_", " "))
	if containsAny(industryContext,
		"regulated electric", "regulated gas", "regulated water", "electric utility", "water utility", "gas utility") {
		return true
	}
	summary := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(f.LongBusinessSummary), "_", " "))
	return containsAny(summary,
		"regulated utility operator", "regulated electric utility", "regulated gas utility", "regulated water utility",
		"operates regulated utilities", "operates a regulated utility", "owns and operates regulated utilities")
}

func isHighGrowthInfrastructure(f Fundamentals) bool {
	if f.RevenueGrowth == nil || !isFinite(*f.RevenueGrowth) || *f.RevenueGrowth < .20 ||
		isEnergyMiningBusiness(f) || isRegulatedUtilityBusiness(f) {
		return false
	}
	keyText := strings.ReplaceAll(normalizedIndustryKey(f.IndustryKey), "-", " ")
	text := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(
		f.Sector+" "+f.Industry+" "+keyText+" "+f.LongBusinessSummary), "_", " "))
	return containsAny(text,
		"fuel cell", "fuel-cell", "solid oxide", "new energy", "clean energy", "renewable energy",
		"energy storage", "battery storage", "grid storage", "data center", "data-centre", "data centre",
		"data-center", "data-centre infrastructure", "data center infrastructure", "ai infrastructure", "ai computing", "ai compute",
		"ai cloud", "gpu cloud", "gpu clusters", "gpu infrastructure", "compute infrastructure", "power infrastructure", "hydrogen infrastructure",
		"electrolyzer", "新能源", "燃料电池", "数据中心", "算力基础设施", "储能")
}

func highGrowthInfrastructureForwardRevenue(f Fundamentals) float64 {
	revenue := positivePtr(f.Revenue)
	if revenue == nil {
		return 0
	}
	minimum, maximum := *revenue*1.20, *revenue*1.30
	if estimate := positivePtr(f.NextYearRevenueEstimate); estimate != nil && f.NextYearRevenueAnalysts >= 3 {
		return clamp(*estimate, minimum, maximum)
	}
	return *revenue * (1 + clamp(valueOr(f.RevenueGrowth, 0), .20, .30))
}

func informationFallbackSafetyMargin(f Fundamentals) float64 {
	if isHighGrowthInfrastructure(f) {
		return math.Max(valueOr(f.SafetyMargin, defaultSafetyMargin), .50)
	}
	normalize := func(value string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", " "))
	}
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(f.IndustryKey), "_", "-"))
	keyText := strings.ReplaceAll(key, "-", " ")
	classificationText := normalize(f.Sector + " " + f.Industry + " " + keyText + " " + f.LongBusinessSummary)
	industryContext := normalize(f.Sector + " " + f.Industry + " " + keyText)
	betaHigh := normalizedBeta(f.Beta) > 1.5
	margin := valueOr(f.SafetyMargin, defaultSafetyMargin)

	banking := key == "banks-diversified" || key == "banks-regional" || key == "banking" ||
		containsAny(classificationText, "bank", "banking", "regional bank", "diversified banks")
	healthcarePlans := key == "healthcare-plans" || containsAny(industryContext, "healthcare plans", "health care plans", "health plan", "managed care", "managed healthcare")
	insurance := !healthcarePlans && (key == "insurance-diversified" || key == "insurance-life" || key == "insurance" ||
		containsAny(classificationText, "insurance", "life insurance", "property & casualty", "reinsurance", "insurer"))
	reit := f.IsREIT || strings.HasPrefix(key, "reit-") || key == "reit" ||
		containsAny(classificationText, "reit", "real estate investment trust")
	biotech := key == "biotechnology" || key == "drug-manufacturers-general" || key == "pharmaceutical-retailers" ||
		containsAny(classificationText, "biotech", "biotechnology", "drug manufacturer", "pharmaceutical", "drug manufacturers")
	utility := isRegulatedUtilityBusiness(f)
	internet := key == "internet-content-information" || key == "internet-retail" || key == "internet-services" ||
		containsAny(classificationText, "internet content", "internet retail", "interactive media", "social media", "online platform", "internet services", "e commerce", "e-commerce") ||
		(strings.Contains(normalize(f.Sector), "communication services") && containsAny(classificationText, "internet", "media", "interactive", "social"))
	defenseTag := containsAny(classificationText, "defense", "aerospace", "laser", "military", "directed energy")
	mobilityTechnology := isMobilityTechnology(f, classificationText, industryContext)
	financialOrRealEstate := f.IsREIT || containsAny(industryContext,
		"financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
	energyMining := !defenseTag && !mobilityTechnology && isEnergyMiningBusiness(f)
	if mobilityTechnology {
		banking, healthcarePlans, insurance, reit, biotech, utility, internet, energyMining = false, false, false, false, false, false, false, false
	}
	industryModel := ""
	switch {
	case banking:
		industryModel = "banking"
	case healthcarePlans:
		industryModel = "healthcare_plans"
	case insurance:
		industryModel = "insurance"
	case reit:
		industryModel = "reit"
	case biotech:
		industryModel = "biotech"
	case energyMining:
		industryModel = "energy_mining"
	case utility:
		industryModel = "utility"
	case internet:
		industryModel = "internet_platform"
	}
	industryMargins := map[string]float64{
		"banking": .25, "healthcare_plans": .30, "insurance": .35, "reit": .25, "biotech": .45,
		"energy_mining": .35, "utility": .18, "internet_platform": .40,
	}
	if industryModel != "" {
		margin = industryMargins[industryModel]
	} else if financialOrRealEstate {
		margin = .25
	} else {
		growth := valueOr(f.GrowthRate, 0)
		if f.GrowthRate == nil {
			growth = annualGrowthRate(f.AnnualEPS)
			if growth == 0 {
				growth = annualGrowthRate(f.AnnualRevenue)
			}
		}
		growth = clamp(growth, -.15, .50)
		revenueGrowth := valueOr(f.RevenueGrowth, growth)
		freeCashFlow := valueOr(f.FreeCashFlow, 0)
		negativeFCF := f.FreeCashFlow != nil && freeCashFlow < 0
		fcfMargin := 0.0
		if f.Revenue != nil && *f.Revenue > 0 && f.FreeCashFlow != nil {
			fcfMargin = freeCashFlow / *f.Revenue
		}
		normalizedEPS := normalizedEPSValue(f)
		forwardEPS := positivePtr(f.ForwardEPS)
		forwardEPSObserved := f.ForwardEPSObserved && forwardEPS != nil
		memory := containsAny(classificationText, "memory", "dram", "nand", "flash")
		storageHardware := containsAny(classificationText,
			"hard disk", "disk drive", "hdd", "nand", "flash memory", "solid state drive", "ssd", "memory chip", "storage chip", "storage device", "storage hardware", "storage products", "storage systems", "enterprise storage", "computer storage") ||
			(containsAny(classificationText, "computer hardware") && containsAny(classificationText, "storage", "disk"))
		aiStorageCycle := !financialOrRealEstate && (memory || (storageHardware &&
			(growth >= .20 || revenueGrowth >= .20 || (forwardEPSObserved && normalizedEPS != nil && *forwardEPS >= *normalizedEPS*1.25))))
		if mobilityTechnology {
			aiStorageCycle = false
			storageHardware = false
		}
		classifiedAIStorageCycle := aiStorageCycle
		cruiseOperator := containsAny(classificationText, "cruise", "cruise lines", "hotels resorts cruise", "travel services", "resorts")
		defenseTransition := (f.DefenseRevenueGrowth != nil && *f.DefenseRevenueGrowth >= .30) ||
			(defenseTag && (growth >= .10 || revenueGrowth >= .10 || negativeFCF))
		capexHeavy := f.CapitalExpenditure != nil && f.OperatingCashflow != nil &&
			math.Abs(*f.CapitalExpenditure) >= math.Abs(*f.OperatingCashflow)*.70
		assetHeavyTransition := !defenseTransition && !classifiedAIStorageCycle && !memory && !cruiseOperator &&
			(negativeFCF || capexHeavy)
		priceToSales := f.PriceToSales == nil || *f.PriceToSales > 10
		highGrowthChipSignal := !assetHeavyTransition && !aiStorageCycle && !cruiseOperator && !defenseTag && !memory &&
			containsAny(classificationText, "semiconductor", "ai chip", "graphics processor") && growth >= .25 && priceToSales
		marketCap := effectiveMarketCapValue(f)
		if mobilityTechnology && isMobilityTechnologyGrowth(f, classificationText, mobilityTechnology,
			marketCap, financialOrRealEstate, false, defenseTransition,
			normalizeRegimeName(f.Regime) == RegimeWinter) {
			highGrowthChipSignal = false
		}
		aiRevenueShare := valueOr(f.AIRevenueShare, 0)
		aiExposed := highGrowthChipSignal || aiStorageCycle || storageHardware ||
			containsAny(classificationText, "semiconductor", "ai chip", "graphics processor") || mobilityTechnology || aiRevenueShare > 0
		aiWinter := normalizeRegimeName(f.Regime) == RegimeWinter && aiExposed
		if aiStorageCycle && !forwardEPSObserved {
			aiStorageCycle = false
		}
		cyclical := aiStorageCycle || containsAny(industryContext, "semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline")
		if aiWinter {
			cyclical = true
			aiStorageCycle = false
			assetHeavyTransition = false
			highGrowthChipSignal = false
		}
		if industryModel != "" {
			aiStorageCycle, assetHeavyTransition, highGrowthChipSignal = false, false, false
		}
		growthForwardEPS := 0.0
		if forwardEPSObserved {
			growthForwardEPS = *forwardEPS
		} else if normalizedEPS != nil && !(highGrowthChipSignal || classifiedAIStorageCycle) {
			growthForwardEPS = *normalizedEPS * (1 + growth)
		}
		genericHighGrowth := growth >= .18 && growthForwardEPS > 0
		if cyclical && normalizedEPS != nil && growthForwardEPS > *normalizedEPS*2 {
			genericHighGrowth = false
		}
		matureGrowth := marketCap >= 5e11 && f.FreeCashFlow != nil && freeCashFlow > 0 &&
			fcfMargin >= .20 && (revenueGrowth >= .06 || (marketCap >= 1e12 && f.RevenueGrowth == nil && !f.GrowthHintObserved))
		stable := containsAny(industryContext, "consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores")
		digitalRetail := stable && marketCap >= 1e11 &&
			containsAny(industryContext, "retail", "discount stores", "consumer defensive", "consumer staples")
		mobilityGrowth := mobilityTechnology && isMobilityTechnologyGrowth(f, classificationText, mobilityTechnology,
			marketCap, financialOrRealEstate, industryModel != "", defenseTransition, aiWinter)
		if aiWinter || industryModel != "" || defenseTransition || aiStorageCycle || mobilityGrowth || assetHeavyTransition || digitalRetail || cruiseOperator {
			matureGrowth = false
		}
		transition := assetHeavyTransition || (!cyclical && (negativeFCF ||
			(f.OperatingCashflow != nil && f.CapitalExpenditure != nil && math.Abs(*f.CapitalExpenditure) > 0 && math.Abs(*f.CapitalExpenditure) >= *f.OperatingCashflow*.85)))
		if aiWinter || defenseTransition || aiStorageCycle || digitalRetail || cruiseOperator {
			transition = false
		}
		switch {
		case aiStorageCycle:
			margin = .40
		case highGrowthChipSignal && forwardEPSObserved && !aiWinter:
			margin = .30
		case matureGrowth:
			margin = .20
			if betaHigh {
				margin = .25
			}
		case transition:
			margin = .42
			if betaHigh {
				margin = .47
			}
		case cyclical:
			margin = .62
		case genericHighGrowth:
			margin = .45
		case negativeFCF:
			margin = .38
		case stable:
			margin = .18
			if betaHigh {
				margin = .20
			}
		}
	}
	if betaHigh {
		margin += .05
	}
	return clamp(margin, .15, .70)
}

func annualGrowthRate(values []float64) float64 {
	positive := make([]float64, 0, len(values))
	for _, value := range values {
		if isPositiveFinite(value) {
			positive = append(positive, value)
		}
	}
	if len(positive) < 2 {
		return 0
	}
	periods := float64(len(positive) - 1)
	return math.Pow(positive[0]/positive[len(positive)-1], 1/periods) - 1
}

func withValuationRates(result Result, f Fundamentals) Result {
	result.RiskFreeRate = copyPointer(f.RiskFreeRate)
	requiredReturn := valueOr(f.RequiredReturn, defaultRequiredReturn)
	result.RequiredReturn = floatPointer(requiredReturn)
	return result
}

func normalizedBeta(beta *float64) float64 {
	if beta == nil || !isFinite(*beta) || *beta < .2 || *beta > 3 {
		return 1
	}
	return *beta
}

func normalizeRegimeName(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case RegimeBoom:
		return RegimeBoom
	case RegimeSlowdown:
		return RegimeSlowdown
	case RegimeWinter:
		return RegimeWinter
	case "":
		return ""
	default:
		return RegimeNormal
	}
}

func isMobilityTechnology(f Fundamentals, text, industryContext string) bool {
	industrySignal := containsAny(industryContext, "auto", "automotive", "electric vehicle", "mobility", "transportation", "ride-hailing")
	technologySignal := containsAny(text, "autonomous", "self-driving", "self driving", "full self-driving", "full self driving", "robotaxi", "robotics", "optimus", "humanoid", "robot", "autopilot", "fsd")
	if industrySignal && technologySignal {
		return true
	}
	marketCap, ok := effectiveMarketCap(f)
	if !ok || marketCap < mobilitySignalMarketCap {
		return false
	}
	quality := strings.TrimSpace(f.MarketCapDataQuality)
	if quality == "" {
		quality = marketCapDataQuality(f)
	}
	if strings.EqualFold(quality, "unit_mismatch") || strings.EqualFold(quality, "market_cap_preferred") {
		return false
	}
	strongSignals := 0
	for _, signal := range []string{"self-driving", "self driving", "full self-driving", "full self driving", "robotaxi", "autopilot", "fsd", "humanoid", "optimus"} {
		if strings.Contains(text, signal) {
			strongSignals++
		}
	}
	return strongSignals >= mobilityStrongSignalCount
}

func isMobilityTechnologyGrowth(f Fundamentals, text string, mobilityTechnology bool, marketCap float64, financial, specialized, defenseTransition, aiWinter bool) bool {
	if aiWinter || financial || specialized || defenseTransition || marketCap < mobilitySignalMarketCap {
		return false
	}
	electricMobility := containsAny(text, "electric vehicle", "electric vehicles", "ev maker", "battery electric")
	if !mobilityTechnology && (!electricMobility || marketCap < 5e11) {
		return false
	}
	growthEligible := valueOr(f.GrowthRate, 0) >= .05 || valueOr(f.RevenueGrowth, 0) >= .05 ||
		(f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0)
	if f.FreeCashFlow != nil && f.Revenue != nil && *f.Revenue > 0 && *f.FreeCashFlow / *f.Revenue > 0 {
		growthEligible = true
	}
	if growthEligible {
		return true
	}
	return marketCap >= 5e11 && mobilityTechnology && f.FreeCashFlow != nil && f.Revenue != nil && *f.Revenue > 0
}

// calculateSpecialized implements the data-backed industry branches that do
// not fit the generic owner-earnings/PE/DCF chain. It uses normalized public
// inputs and leaves an identified industry unavailable when none are usable.
func calculateSpecialized(f Fundamentals) (Result, bool) {
	normalizedEPS := normalizedEPSValue(f)
	key := normalizedIndustryKey(f.IndustryKey)
	keyCategory := industryKeyCategory(key)
	industryKeyText := strings.ReplaceAll(key, "-", " ")
	normalizeClassificationText := func(value string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", " "))
	}
	text := normalizeClassificationText(f.Sector + " " + f.Industry + " " + industryKeyText + " " + f.LongBusinessSummary)
	industryContext := normalizeClassificationText(f.Sector + " " + f.Industry + " " + industryKeyText)
	highGrowthInfrastructure := isHighGrowthInfrastructure(f)
	healthcarePlans := key == "healthcare-plans" || containsAny(industryContext, "healthcare plans", "health care plans", "health plan", "managed care", "managed healthcare")
	lifecycleText := industryContext
	category := map[string]string{
		"banks-diversified": "banking", "banks-regional": "banking", "banking": "banking",
		"insurance-diversified": "insurance", "insurance-life": "insurance", "insurance": "insurance",
		"reit-residential": "reit", "reit-retail": "reit", "reit-office": "reit", "reit-diversified": "reit", "reit": "reit",
		"biotechnology": "biotech", "drug-manufacturers-general": "biotech", "pharmaceutical-retailers": "biotech",
		"utilities-regulated-electric": "utility", "utilities-regulated-gas": "utility", "utilities-regulated-water": "utility", "utility": "utility",
		"internet-content-information": "internet", "internet-retail": "internet", "internet-services": "internet",
		"semiconductor-memory": "memory",
		"aerospace-defense":    "defense", "defense": "defense",
		"energy": "energy", "oil-gas-integrated": "energy", "oil-gas-e-p": "energy",
		"oil-gas-midstream": "energy", "oil-gas-refiners": "energy", "oil-gas-equipment-services": "energy",
		"uranium": "energy", "gold": "energy", "copper": "energy", "other-industrial-metals-mining": "energy",
	}
	keyModelClass := category[key]
	// Keep the structured Python categories authoritative when the textual
	// industry/sector fields are absent or renamed by Yahoo. Only categories
	// that correspond to a specialized model are promoted here; the remaining
	// categories continue through the generic lifecycle heuristics.
	if keyModelClass == "" {
		switch keyCategory {
		case "banking":
			keyModelClass = "banking"
		case "insurance":
			keyModelClass = "insurance"
		case "reit":
			keyModelClass = "reit"
		case "biotech":
			keyModelClass = "biotech"
		case "utility":
			keyModelClass = "utility"
		case "internet_platform":
			keyModelClass = "internet"
		case "defense":
			keyModelClass = "defense"
		case "memory_chip":
			keyModelClass = "memory"
		}
	}
	modelClass := ""
	switch {
	case healthcarePlans:
		// Healthcare plan companies can mention insurance in their summaries,
		// but their earnings are best represented by a healthcare services PE.
		modelClass = "healthcare-plans"
	case keyModelClass == "banking" || containsAny(text, "bank", "banking", "regional bank", "diversified banks"):
		modelClass = "banking"
	case keyModelClass == "insurance" || containsAny(text, "insurance", "insurer", "reinsurance", "life insurance", "property & casualty"):
		modelClass = "insurance"
	case keyModelClass == "reit" || containsAny(text, "reit", "real estate investment trust"):
		modelClass = "reit"
	case keyModelClass == "biotech" || containsAny(text, "biotech", "biotechnology", "pharmaceutical", "drug manufacturer"):
		modelClass = "biotech"
	case keyModelClass == "utility" || isRegulatedUtilityBusiness(f):
		modelClass = "utility"
	case keyModelClass == "energy" || isEnergyMiningBusiness(f):
		if highGrowthInfrastructure {
			modelClass = "high-growth-infrastructure"
		} else {
			modelClass = "energy"
		}
	case highGrowthInfrastructure:
		modelClass = "high-growth-infrastructure"
	case keyModelClass == "internet" || containsAny(text, "internet content", "internet retail", "interactive media", "social media", "online platform", "internet services", "e commerce", "e-commerce") ||
		(strings.Contains(strings.ToLower(f.Sector), "communication services") && containsAny(text, "internet", "media", "interactive", "social")):
		modelClass = "internet"
	case !healthcarePlans && containsAny(industryContext, "financial", "capital markets", "credit", "real estate", "property"):
		modelClass = "financial"
	case containsAny(text, "cruise", "travel services", "resorts"):
		modelClass = "cruise"
	case keyModelClass == "defense" || containsAny(text, "aerospace", "defense", "directed energy", "military", "laser"):
		modelClass = "defense"
	default:
		modelClass = keyModelClass
	}
	financialIndustry := !healthcarePlans && containsAny(industryContext, "financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
	mobilityTechnology := isMobilityTechnology(f, text, industryContext)
	if mobilityTechnology {
		if financialIndustry {
			// Python clears specialist model flags for mobility-tagged companies,
			// but retains its broader financial/real-estate industry filter.
			modelClass = "financial"
		} else {
			modelClass = "mobility"
		}
	}
	if modelClass == "reit" {
		result := calculateREIT(f)
		result.IndustryModel = true
		return result, true
	}
	// The provider normally supplies industryKey, but Yahoo occasionally leaves
	// it empty while still returning enough text and cash-flow data to identify a
	// transition company. Keep these heuristics behind the specialized dispatcher
	// so the provider-independent legacy Calculate contract remains unchanged.
	financial := financialIndustry
	defenseIndustryTag := keyCategory == "defense" || containsAny(text, "defense", "aerospace", "laser", "military", "directed energy")
	defenseTransition := valueOr(f.DefenseRevenueGrowth, 0) >= .30 ||
		(defenseIndustryTag && (valueOr(f.GrowthRate, 0) >= .10 || valueOr(f.RevenueGrowth, 0) >= .10 || (f.FreeCashFlow != nil && *f.FreeCashFlow < 0)))
	if financial || mobilityTechnology {
		defenseTransition = false
	}
	if (modelClass == "defense" && !defenseTransition) || (modelClass == "energy" && defenseIndustryTag) {
		modelClass = ""
	}
	if defenseTransition && modelClass == "" {
		modelClass = "defense"
	}
	memoryText := keyCategory == "memory_chip" || containsAny(text, "memory", "dram", "nand", "flash", "semiconductor memory")
	classGrowth := clamp(valueOr(f.GrowthRate, 0), -.15, .50)
	storageHardware := containsAny(text, "hard disk", "disk drive", "hdd", "nand", "flash memory", "solid state drive", "ssd", "memory chip", "storage chip", "storage device", "storage hardware", "storage products", "storage systems", "enterprise storage", "computer storage") ||
		(containsAny(text, "computer hardware") && containsAny(text, "storage", "disk"))
	aiStorageCycle := !financial && !mobilityTechnology && (memoryText || (storageHardware &&
		(classGrowth >= .20 || valueOr(f.RevenueGrowth, 0) >= .20 ||
			(positivePtr(f.ForwardEPS) != nil && normalizedEPS != nil && *f.ForwardEPS >= *normalizedEPS*1.25))))
	classifiedAIStorageCycle := aiStorageCycle
	industryCyclical := containsAny(industryContext,
		"semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline")
	cyclical := aiStorageCycle || industryCyclical
	cycleEPS := cyclicalEPS(f, normalizedEPS)
	aiRevenueShare := valueOr(f.AIRevenueShare, 0)
	if aiRevenueShare > 1 {
		aiRevenueShare /= 100
	}
	aiRevenueShare = clamp(aiRevenueShare, 0, 1)
	structuralStorage := containsAny(text, "hard disk", "disk drive", "enterprise storage")
	freeCashFlowMargin := 0.0
	if f.FreeCashFlow != nil && f.Revenue != nil && *f.Revenue > 0 {
		freeCashFlowMargin = *f.FreeCashFlow / *f.Revenue
	}
	maturityGrowth := classGrowth
	if f.RevenueGrowth != nil {
		maturityGrowth = *f.RevenueGrowth
	}
	marketCap := effectiveMarketCapValue(f)
	matureGrowth := f.FreeCashFlow != nil && *f.FreeCashFlow > 0 && marketCap >= 5e11 && freeCashFlowMargin >= .20 &&
		(maturityGrowth >= .06 || (marketCap >= 1e12 && f.RevenueGrowth == nil && !f.GrowthHintObserved))
	premiumMatureGrowth := matureGrowth && marketCap >= 1e12 && maturityGrowth >= .08
	stableBusiness := !financial && containsAny(lifecycleText, "consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores")
	stableCashflowBusiness := !financial && containsAny(text,
		"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage")
	stableCashflow := stableCashflowBusiness && classGrowth < .15 && freeCashFlowMargin > .15
	digitalRetail := stableBusiness && marketCap >= 1e11 && containsAny(lifecycleText, "retail", "discount stores", "consumer defensive", "consumer staples")
	aiStorageGrowth := aiStorageCycle && storageHardware && !memoryText && structuralStorage && freeCashFlowMargin >= .15 &&
		(classGrowth >= .30 || valueOr(f.RevenueGrowth, 0) >= .30)
	assetHeavyTransition := !financial && !defenseTransition && !aiStorageCycle && !memoryText && !highGrowthInfrastructure && modelClass != "cruise" &&
		((f.FreeCashFlow != nil && *f.FreeCashFlow < 0) ||
			(f.CapitalExpenditure != nil && f.OperatingCashflow != nil && math.Abs(*f.CapitalExpenditure) >= math.Abs(*f.OperatingCashflow)*.70))
	highGrowthChipSignal := !financial && !mobilityTechnology && !defenseIndustryTag && !memoryText &&
		!aiStorageCycle && modelClass != "cruise" && !assetHeavyTransition &&
		containsAny(text, "semiconductor", "ai chip", "graphics processor") && classGrowth >= .25 &&
		(f.PriceToSales == nil || *f.PriceToSales > 10)
	classifiedHighGrowthChipSignal := highGrowthChipSignal
	forwardEPSObserved := f.ForwardEPSObserved && positivePtr(f.ForwardEPS) != nil
	classifiedHighGrowthChip := classifiedHighGrowthChipSignal && forwardEPSObserved
	forwardEPSMissingWarning := !forwardEPSObserved && (highGrowthChipSignal || aiStorageCycle || defenseTransition)
	if aiRevenueShare <= 0 && !defenseIndustryTag {
		switch {
		case containsAny(text, "ai chip") || highGrowthChipSignal:
			aiRevenueShare = .70
		case containsAny(text, "semiconductor"):
			aiRevenueShare = .60
		case containsAny(text, "storage"):
			aiRevenueShare = .30
		}
	}
	specializedIndustry := !mobilityTechnology && (modelClass == "banking" || modelClass == "healthcare-plans" || modelClass == "insurance" || modelClass == "reit" ||
		modelClass == "biotech" || modelClass == "energy" || modelClass == "utility" || modelClass == "internet" || modelClass == "high-growth-infrastructure")
	internetAIPlatform := modelClass == "internet" && marketCap >= 1e12 && containsAny(text,
		"artificial intelligence", "generative ai", "large language model", "machine learning", "ai infrastructure",
		"ai recommendation", "recommendation engine", "ad ranking", "personalized ads", "ai-powered", "ai powered",
		"ai-driven", "ai driven", "ai models", "llama",
	)
	aiExposed := !specializedIndustry && (highGrowthChipSignal || aiStorageCycle || storageHardware ||
		containsAny(text, "semiconductor", "ai chip", "graphics processor") || mobilityTechnology || aiRevenueShare > 0)
	aiWinter := f.Regime == RegimeWinter && aiExposed
	mobilityTechnologyGrowth := isMobilityTechnologyGrowth(f, text, mobilityTechnology, marketCap,
		financial, specializedIndustry, defenseTransition, aiWinter)
	if mobilityTechnology && !mobilityTechnologyGrowth && modelClass == "mobility" {
		modelClass = ""
	}
	if mobilityTechnologyGrowth {
		modelClass = "mobility"
	}
	highGrowthChip := highGrowthChipSignal && forwardEPSObserved && f.Regime != RegimeWinter
	if (highGrowthChipSignal || aiStorageCycle) && !forwardEPSObserved && f.Regime != RegimeWinter {
		if defenseTransition || specializedIndustry {
			// Specialized and defense models keep priority after Python clears
			// missing-forward-EPS growth signals.
			highGrowthChip = false
			aiStorageCycle = false
			aiStorageGrowth = false
			if defenseTransition && !specializedIndustry {
				modelClass = "defense"
			}
		} else if aiStorageCycle {
			// Python sends memory and storage cycles without analyst EPS to the
			// normalized cyclical EPS model only when the industry itself is cyclical.
			aiStorageCycle = false
			aiStorageGrowth = false
			cyclical = industryCyclical
			if modelClass == "memory" {
				modelClass = ""
			}
			if !cyclical && modelClass == "" {
				fallback := f
				fallback.ForwardEPS = nil
				result := calculateGenericWithoutModeledForwardEPS(fallback)
				result.AIExposed = aiExposed
				warning := "缺乏分析师远期 EPS，已自动降级为保守模型"
				result.Warnings = append(result.Warnings, warning)
				if result.Warning == "" {
					result.Warning = warning
				} else {
					result.Warning += "; " + warning
				}
				return result, true
			}
		} else {
			fallback := f
			fallback.ForwardEPS = nil
			result := calculateGenericWithoutModeledForwardEPS(fallback)
			result.IndustryModel = specializedIndustry
			result.AIExposed = aiExposed
			warning := "缺乏分析师远期 EPS，已自动降级为保守模型"
			result.Warnings = append(result.Warnings, warning)
			if result.Warning == "" {
				result.Warning = warning
			} else {
				result.Warning += "; " + warning
			}
			return result, true
		}
	}
	if highGrowthChip && !defenseTransition && !specializedIndustry {
		modelClass = "high-growth-chip"
	}
	if observedForward := positivePtr(f.ForwardEPS); observedForward != nil {
		if normalizedEPS != nil {
			forwardCap := *normalizedEPS * 3
			if classifiedHighGrowthChip || classifiedAIStorageCycle {
				forwardCap = math.Max(*normalizedEPS*5, 12)
				if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
					forwardCap = math.Min(forwardCap, *f.TargetMeanPrice/35)
				}
			}
			if *observedForward > forwardCap {
				f.ForwardEPS = floatPointer(forwardCap)
			}
		}
	}
	if aiWinter {
		highGrowthChip = false
		aiStorageCycle = false
		aiStorageGrowth = false
		mobilityTechnologyGrowth = false
		matureGrowth = false
		premiumMatureGrowth = false
		assetHeavyTransition = false
	}
	if specializedIndustry {
		defenseTransition = false
		assetHeavyTransition = false
		highGrowthChip = false
		highGrowthChipSignal = false
		aiStorageCycle = false
		aiStorageGrowth = false
		matureGrowth = false
		premiumMatureGrowth = false
		digitalRetail = false
	}
	if aiStorageCycle {
		if aiStorageGrowth && f.Regime != RegimeWinter {
			modelClass = "ai-storage-growth"
		} else {
			modelClass = "ai-storage-cycle"
		}
	}
	if modelClass == "" && !financial {
		if defenseTransition {
			modelClass = "defense"
		} else if forwardEPSObserved && (memoryText || containsAny(text, "hard disk", "disk drive", "solid state drive", "storage hardware", "storage device")) {
			modelClass = "memory"
		}
	}
	if modelClass == "" && !financial {
		capexHeavy := f.CapitalExpenditure != nil && f.OperatingCashflow != nil &&
			math.Abs(*f.CapitalExpenditure) >= math.Abs(*f.OperatingCashflow)*.70
		if digitalRetail {
			modelClass = "digital-retail"
		} else if (f.FreeCashFlow != nil && *f.FreeCashFlow < 0) || capexHeavy {
			modelClass = "asset-heavy"
		} else if matureGrowth {
			modelClass = "mature"
		}
	}
	if modelClass == "" && stableCashflow {
		modelClass = "stable-cashflow"
	}
	if modelClass == "" && !financial && !specializedIndustry && !defenseTransition &&
		!assetHeavyTransition && !matureGrowth && !stableCashflow && cyclical {
		modelClass = "cyclical"
	}
	growthForwardEPS := modeledForwardEPS(f, classGrowth)
	if f.Regime == RegimeSlowdown && aiExposed && growthForwardEPS > 0 {
		growthForwardEPS *= .70
	}
	cyclicalPeak := cyclical && normalizedEPS != nil && growthForwardEPS > *normalizedEPS*2
	genericHighGrowth := classGrowth >= .18 && growthForwardEPS > 0 &&
		(f.ForwardEPSObserved || !highGrowthChipSignal) && !cyclicalPeak
	if modelClass == "" && !financial && !specializedIndustry && !defenseTransition &&
		!assetHeavyTransition && !matureGrowth && !stableCashflow && !cyclical && genericHighGrowth {
		modelClass = "high-growth"
	}
	if aiWinter {
		modelClass = "ai-winter"
	}

	// A small set of shared normalized inputs.
	shares := positivePtr(f.SharesOutstanding)
	book := positivePtr(f.BookValue)
	forward := positivePtr(f.ForwardEPS)
	if forward == nil {
		forward = normalizedEPS
	}
	reportedForward := copyPointer(forward)
	if forwardEPSMissingWarning && (classifiedHighGrowthChipSignal || classifiedAIStorageCycle) {
		reportedForward = nil
	}
	growth := valueOr(f.GrowthRate, 0)
	if growth < -.15 {
		growth = -.15
	}
	if growth > .50 {
		growth = .50
	}
	if !forwardEPSObserved && (modelClass == "insurance" || modelClass == "biotech") {
		if modeled := modeledForwardEPS(f, growth); modeled > 0 {
			forward = floatPointer(modeled)
			reportedForward = copyPointer(forward)
		}
	}
	retailEPS := modeledForwardEPS(f, growth)
	debt, cash := valueOr(f.Debt, 0), valueOr(f.Cash, 0)
	cruiseEBITDA := positivePtr(f.ForwardEBITDA)
	if cruiseEBITDA == nil {
		if currentEBITDA := positivePtr(f.EBITDA); currentEBITDA != nil {
			projected := *currentEBITDA * (1 + clamp(growth, .05, .20))
			cruiseEBITDA = &projected
		}
	}
	methods := map[string]float64{}
	var low, high float64
	var infrastructurePSLow, infrastructurePSHigh float64
	var infrastructurePELow, infrastructurePEHigh float64
	var infrastructureForwardRevenue float64
	var mobilityUpsideLow, mobilityUpsideHigh float64
	var mobilityForwardRevenue, mobilitySharesValue, mobilityOptimisticEPS float64
	var mobilityHasRevenueAnchor bool
	model := ""

	switch modelClass {
	case "banking":
		roe := valueOr(f.ROE, .10)
		if math.Abs(roe) > 1 {
			roe /= 100
		}
		roe = clamp(roe, -.10, .40)
		if book == nil && normalizedEPS != nil && roe > 0 {
			v := *normalizedEPS / roe
			if v > 0 {
				book = &v
			}
		}
		if book != nil {
			pb := clamp(1+(roe-.10)/.10, .65, 2.75)
			methods["银行 P/B-ROE"] = roundCurrency(*book * pb)
			low, high = *book*math.Max(.65, pb-.20), *book*math.Min(3, pb+.20)
			if roe > 0 {
				trig := math.Max(.035, valueOr(f.RequiredReturn, .10)-math.Min(.04, math.Max(0, growth*.25)))
				ri := *book + *book*(roe-valueOr(f.RequiredReturn, .10))/trig
				if ri > 0 {
					methods["银行剩余收益 RI"] = roundCurrency(ri)
				}
			}
		}
		model = "银行：P/B-ROE + 剩余收益 RI"
	case "healthcare-plans":
		earnings := positivePtr(normalizedEPS)
		if earnings == nil {
			earnings = positivePtr(forward)
		}
		if earnings != nil {
			low, high = *earnings*18, *earnings*22
			methods["医疗保险计划正常化 EPS × 20 倍 PE"] = roundCurrency(*earnings * 20)
		}
		model = "医疗保险计划：正常化 EPS × PE"
	case "insurance":
		ev := positivePtr(f.EmbeddedValuePerShare)
		if ev == nil {
			ev = book
		}
		nbv := valueOr(f.NewBusinessValuePerShare, 0)
		multiple := clamp(2+math.Max(0, growth)*3, 1.5, 4)
		if ev != nil {
			low, high = *ev+nbv*math.Max(1.5, multiple-.5), *ev+nbv*math.Min(4.5, multiple+.5)
			methods["保险内含价值 + NBV"] = roundCurrency(*ev + nbv*multiple)
		} else if forward != nil {
			low, high = *forward*12, *forward*18
			methods["保险远期盈利代理"] = roundCurrency((low + high) / 2)
		}
		model = "保险：内含价值 EV + 新业务价值 NBV"
	case "biotech":
		approved := valueOr(f.ApprovedDrugValue, 0)
		if approved <= 0 && forward != nil {
			approved = *forward * 18
		}
		pipeline := math.Max(0, valueOr(f.PipelineRNPV, 0))
		low, high = approved*.75+pipeline*.60, approved*1.10+pipeline*.90
		if high > 0 {
			methods["已上市药品 DCF + 管线 rNPV"] = roundCurrency((low + high) / 2)
		}
		model = "生物科技：已上市药品 DCF + 管线风险调整 NPV"
	case "financial":
		if f.DividendRate != nil && *f.DividendRate > 0 {
			ddmGrowth := clamp(growth*.35, 0, .04)
			ddmRate := math.Max(valueOr(f.RequiredReturn, defaultRequiredReturn), ddmGrowth+.045)
			methods["DDM"] = roundCurrency(*f.DividendRate * (1 + ddmGrowth) / math.Max(ddmRate-ddmGrowth, .025))
		}
		if book != nil {
			pbMultiple := 1.10
			if containsAny(industryContext, "real estate", "property", "reit") {
				pbMultiple = 1.15
			}
			methods["P/B"] = roundCurrency(*book * pbMultiple)
		}
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			methods["分析师共识"] = roundCurrency(*f.TargetMeanPrice)
		}
		if f.TargetLowPrice != nil && *f.TargetLowPrice > 0 {
			methods["分析师目标下沿"] = roundCurrency(*f.TargetLowPrice)
		}
		model = "金融/地产行业过滤：DDM + P/B"
	case "cruise":
		if cruiseEBITDA != nil && shares != nil {
			methods["邮轮 EV/EBITDA 10x"] = roundCurrency((*cruiseEBITDA*10 - debt + cash) / *shares)
			methods["邮轮 EV/EBITDA 12x"] = roundCurrency((*cruiseEBITDA*12 - debt + cash) / *shares)
		}
		if forward != nil {
			methods["邮轮远期 PE 12x"] = roundCurrency(*forward * 12)
			methods["邮轮远期 PE 15x"] = roundCurrency(*forward * 15)
		}
		model = "重资产消费服务：EV/EBITDA + 远期 EPS"
	case "utility":
		r := valueOr(f.RequiredReturn, .10)
		g := clamp(growth*.35, .01, .04)
		r = clamp(r, .07, .10)
		if f.DividendRate != nil && *f.DividendRate > 0 {
			value := *f.DividendRate * (1 + g) / math.Max(r-g, .025)
			low, high = value*.85, value*1.15
			methods["DDM"] = roundCurrency(value)
		}
		if f.RABPerShare != nil && *f.RABPerShare > 0 {
			if low == 0 || high == 0 {
				low, high = *f.RABPerShare*.90, *f.RABPerShare*1.20
			} else {
				low, high = math.Max(low, *f.RABPerShare*.90), math.Max(high, *f.RABPerShare*1.20)
			}
			methods["RAB"] = roundCurrency(*f.RABPerShare)
		}
		model = "公用事业：DDM + RAB"
	case "internet":
		if shares != nil {
			// Yahoo providers are inconsistent here: some return a ratio (0.05),
			// while others return a percentage (5). Normalize before applying the
			// 30% quality cap; clamping first would turn every percentage value into
			// the cap and erase the distinction between 5% and 30% churn.
			churn := valueOr(f.ChurnRate, 0)
			if churn > 1 {
				churn /= 100
			}
			churn = clamp(churn, 0, .30)
			quality := 1 - churn
			psLow, psHigh := 3.0, 8.0
			if f.Revenue != nil && *f.Revenue > 0 {
				perShare := *f.Revenue / *shares
				methods["互联网平台远期 P/S"] = roundCurrency(perShare * (psLow + psHigh) / 2 * quality)
				low, high = perShare*psLow*quality, perShare*psHigh*quality
			}
			if f.PlatformUsers != nil && *f.PlatformUsers > 0 && f.ARPU != nil && *f.ARPU > 0 {
				userRevenue := *f.PlatformUsers * *f.ARPU * 12
				if f.Revenue != nil && *f.Revenue > 0 {
					userRevenue = math.Min(*f.Revenue*2, userRevenue)
				}
				if userRevenue > 0 {
					userRevenuePerShare := userRevenue / *shares
					methods["用户价值校验"] = roundCurrency(userRevenuePerShare * .50 * quality)
					if f.Revenue != nil && *f.Revenue > 0 {
						low = math.Max(low, userRevenuePerShare*2*quality)
						high = math.Max(high, userRevenuePerShare*5*quality)
					}
				}
			}
		}
		model = "互联网平台：用户质量 + 远期 P/S"
		if internetAIPlatform {
			model = "AI 互联网平台：广告/推荐基础设施 + P/S"
		}
	case "high-growth-infrastructure":
		// 前瞻销售额与前瞻 EPS 分别形成乐观区间的低端和高端锚点。
		infrastructureGrowth := clamp(valueOr(f.RevenueGrowth, classGrowth), .20, .30)
		infrastructureForwardRevenue = highGrowthInfrastructureForwardRevenue(f)
		if shares != nil && f.Revenue != nil && isPositiveFinite(*f.Revenue) {
			infrastructurePSLow = (infrastructureForwardRevenue*10 - debt + cash) / *shares
			infrastructurePSHigh = (infrastructureForwardRevenue*15 - debt + cash) / *shares
			if isPositiveFinite(infrastructurePSLow) && isPositiveFinite(infrastructurePSHigh) {
				methods["高增长基础设施远期 P/S"] = roundCurrency((infrastructurePSLow + infrastructurePSHigh) / 2)
				low, high = infrastructurePSLow, infrastructurePSHigh
			}
		}
		if forwardEPS := positivePtr(forward); forwardEPS != nil {
			peLow := clamp(20+infrastructureGrowth*20, 24, 30)
			peHigh := clamp(35+infrastructureGrowth*30, 40, 50)
			infrastructurePELow = *forwardEPS * peLow
			infrastructurePEHigh = *forwardEPS * peHigh
			if isPositiveFinite(infrastructurePELow) && isPositiveFinite(infrastructurePEHigh) {
				methods["高增长基础设施远期 PE"] = roundCurrency((infrastructurePELow + infrastructurePEHigh) / 2)
				if low <= 0 || infrastructurePELow < low {
					low = infrastructurePELow
				}
				high = math.Max(high, infrastructurePEHigh)
			}
		}
		model = "高增长 AI/新能源基础设施：远期 P/S + PE"
	case "digital-retail":
		if retailEPS > 0 && shares != nil {
			methods["传统零售远期 PE"] = roundCurrency(retailEPS * 18)
		}
		model = "防御性数字化零售：远期 EPS + 数字化溢价"
	case "asset-heavy":
		if retailEPS > 0 {
			core := retailEPS * 17.5
			methods["核心业务远期 PE"] = roundCurrency(core)
		}
		if shares != nil && f.Revenue != nil && *f.Revenue > 0 {
			assetMultiple := 2.0
			if containsAny(text, "semiconductor", "foundry", "chip manufacturing", "integrated device") {
				assetMultiple = 1.5
			}
			asset := (*f.Revenue*assetMultiple - debt + cash) / *shares
			methods["制造/云业务 P/S"] = roundCurrency(asset)
		}
		if f.EBITDA != nil && shares != nil && *f.EBITDA > 0 {
			methods["转型 EV/EBITDA"] = roundCurrency((*f.EBITDA*12 - debt + cash) / *shares)
		}
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			methods["分析师共识"] = roundCurrency(*f.TargetMeanPrice)
		}
		model = "重资产转型：核心业务 PE + 制造/云业务 P/S"
	case "mature":
		if forward != nil {
			base := *forward
			if normalizedEPS != nil {
				base = math.Max(*normalizedEPS, math.Min(base, *normalizedEPS*1.35))
			}
			peLow, peHigh := 20.0, 35.0
			if premiumMatureGrowth {
				peLow, peHigh = 30.0, 38.0
			}
			low, high = base*peLow, base*peHigh
			methods["成熟增长 PE"] = roundCurrency(base * (peLow + peHigh) / 2)
		}
		model = "成熟增长型巨头：远期正常化 EPS × PE"
	case "stable-cashflow":
		if forward != nil {
			methods["正常化 EPS × PE"] = roundCurrency(*forward * 27.5)
		}
		model = "稳定现金流：正常化 EPS × 20–35 倍 PE"
	case "energy":
		// Energy and mining companies are valued with finite-life asset/NAV
		// anchors and EV/EBITDA. This keeps volatile commodity cash flow out of
		// the perpetual generic DCF path.
		if f.NAVPerShare != nil && *f.NAVPerShare > 0 {
			low, high = *f.NAVPerShare*.75, *f.NAVPerShare*1.15
			methods["资源 NAV"] = roundCurrency(*f.NAVPerShare)
		} else if shares != nil {
			ebitda := positivePtr(f.ForwardEBITDA)
			if ebitda == nil {
				ebitda = positivePtr(f.EBITDA)
			}
			if ebitda != nil {
				assetLow := math.Max(0, (*ebitda*5-debt+cash) / *shares)
				assetHigh := math.Max(assetLow, (*ebitda*8-debt+cash) / *shares)
				if low <= 0 || high <= 0 {
					low, high = assetLow, assetHigh
				} else {
					low, high = math.Max(low, assetLow), math.Max(high, assetHigh)
				}
				methods["EV/EBITDA"] = roundCurrency((assetLow + assetHigh) / 2)
			}
		}
		model = "能源/矿业：有限寿命 NAV + EV/EBITDA"
	case "mobility":
		// The classification gate requires the same scale and growth evidence as
		// Python; ordinary auto manufacturers never receive a narrative premium.
		if mobilityTechnologyGrowth {
			g := clamp(growth, .05, .35)
			mobilityOptimisticEPS = modeledForwardEPS(f, growth)
			if f.Regime == RegimeSlowdown && aiExposed {
				mobilityOptimisticEPS *= .70
				if forward != nil {
					discounted := *forward * .70
					forward = &discounted
					reportedForward = copyPointer(forward)
				}
			}
			mobilityShares := shares
			if mobilityShares == nil && f.CurrentPrice != nil && *f.CurrentPrice > 0 && marketCap > 0 {
				derivedShares := marketCap / *f.CurrentPrice
				mobilityShares = &derivedShares
			}
			if f.Revenue != nil && *f.Revenue > 0 && mobilityShares != nil {
				mobilityForwardRevenue = *f.Revenue * (1 + g)
				mobilitySharesValue = *mobilityShares
				mobilityUpsideLow, mobilityUpsideHigh = (mobilityForwardRevenue*6-debt+cash)/mobilitySharesValue, (mobilityForwardRevenue*12-debt+cash)/mobilitySharesValue
				mobilityHasRevenueAnchor = true
				methods["移动科技远期 P/S"] = roundCurrency((mobilityUpsideLow + mobilityUpsideHigh) / 2)
			}
			if forward != nil {
				// Keep the primary fair value on the automobile manufacturing
				// floor; the autonomous/robotics P/S range is an upside scenario.
				low, high = *forward*15, *forward*25
				methods["汽车制造远期 EPS × PE"] = roundCurrency((low + high) / 2)
			}
			model = "移动科技成长：自动驾驶/机器人远期 P/S"
		} else if forward != nil {
			low, high = *forward*15, *forward*25
			methods["汽车制造远期 EPS × PE"] = roundCurrency((low + high) / 2)
			model = "汽车制造：远期 EPS × 15–25 倍 PE"
		}
	case "defense":
		defenseGrowth := clamp(growth, .20, .40)
		futureRevenue := valueOr(f.Revenue, 0) * (1 + defenseGrowth)
		if shares != nil && f.Revenue != nil && *f.Revenue > 0 {
			low = (futureRevenue*6 - debt + cash) / *shares
			high = (futureRevenue*8 - debt + cash) / *shares
			methods["国防远期 P/S 6x"] = roundCurrency(low)
			methods["国防远期 P/S 8x"] = roundCurrency(high)
		}
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			methods["分析师共识"] = roundCurrency(*f.TargetMeanPrice)
		}
		if f.TargetLowPrice != nil && *f.TargetLowPrice > 0 {
			methods["分析师目标下沿"] = roundCurrency(*f.TargetLowPrice)
		}
		model = "国防订单驱动转型：远期 P/S + 分析师共识"
	case "ai-winter":
		winterDiscount := 1.0
		switch {
		case aiRevenueShare >= .50:
			winterDiscount = .40
		case aiRevenueShare >= .10:
			winterDiscount = .80
		}
		winterEPS := winterFloorEPS(f, valueOr(normalizedEPS, valueOr(forward, 0)))
		if winterEPS > 0 {
			methods["AI寒冬周期中值 PE"] = roundCurrency(winterEPS * 10 * winterDiscount)
		}
		if book != nil {
			methods["寒冬 P/B 底线"] = roundCurrency(*book * winterDiscount)
		}
		if shares != nil && cash-debt > 0 {
			methods["寒冬净现金保护"] = roundCurrency((cash - debt) / *shares)
		}
		if len(methods) == 0 && shares != nil {
			if ebitda := positivePtr(f.EBITDA); ebitda != nil {
				methods["寒冬 EV/EBITDA 兜底"] = roundCurrency(math.Max(0, (*ebitda*6-debt+cash) / *shares))
			}
		}
		model = "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线"
	case "ai-storage-growth", "ai-storage-cycle":
		storageForward := forward
		if storageForward != nil {
			if normalizedEPS != nil {
				forwardCap := math.Max(*normalizedEPS*5, 12)
				if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
					forwardCap = math.Min(forwardCap, *f.TargetMeanPrice/35)
				}
				if *storageForward > forwardCap {
					capped := forwardCap
					storageForward = &capped
				}
			}
			if f.Regime == RegimeSlowdown {
				discounted := *storageForward * .70
				storageForward = &discounted
			}
			reportedForward = copyPointer(storageForward)
		}
		if f.Regime == RegimeWinter {
			winterEPS := winterFloorEPS(f, valueOr(storageForward, valueOr(forward, 0)))
			if winterEPS > 0 {
				methods["AI寒冬周期中值 PE"] = winterEPS * 10
			}
			if book != nil {
				methods["寒冬 P/B 底线"] = *book
			}
			if shares != nil && cash-debt > 0 {
				methods["寒冬净现金保护"] = (cash - debt) / *shares
			}
			model = "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线"
		} else if modelClass == "ai-storage-growth" {
			if storageForward != nil {
				methods["AI 存储远期 EPS × PE"] = roundCurrency(*storageForward * 45)
			}
			model = "AI 存储结构性增长：远期 EPS × 35–55 倍 PE"
		} else {
			if storageForward != nil {
				adjusted := *storageForward * .70
				peLow, peHigh := 20.0, 30.0
				if f.Regime == RegimeSlowdown {
					peLow, peHigh = 15, 25
				}
				methods["周期调整远期 EPS × PE"] = roundCurrency(adjusted * (peLow + peHigh) / 2)
			}
			model = "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE"
		}
	case "memory":
		if forward != nil {
			// Memory is cyclical and uses a forward-EPS haircut with bounded multiples.
			if modelClass == "memory" {
				baseForward := *forward
				if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
					baseForward = math.Min(baseForward, *f.TargetMeanPrice/35)
				}
				reportedForward = floatPointer(baseForward)
				adjusted := baseForward * .70
				peLow, peHigh := 20.0, 30.0
				if f.Regime == RegimeWinter {
					adjusted = winterFloorEPS(f, *forward)
					peLow, peHigh = 8.0, 12.0
				} else if f.Regime == RegimeSlowdown {
					peLow, peHigh = 15.0, 25.0
				}
				methods["周期调整远期 EPS × PE"] = roundCurrency(adjusted * (peLow + peHigh) / 2)
				low, high = adjusted*peLow, adjusted*peHigh
			} else {
				adjusted := *forward
				peLow, peHigh := 20.0, 35.0
				if f.Regime == RegimeWinter {
					adjusted = winterFloorEPS(f, *forward)
					peLow, peHigh = 8.0, 12.0
				} else if f.Regime == RegimeSlowdown {
					peLow, peHigh = 15.0, 25.0
				}
				methods["远期 EPS × PE"] = roundCurrency(adjusted * (peLow + peHigh) / 2)
				low, high = adjusted*peLow, adjusted*peHigh
			}
		}
		if shares != nil && f.EBITDA != nil && *f.EBITDA > 0 {
			methods["远期 EV/EBITDA"] = roundCurrency((*f.EBITDA*1.15*30 - debt + cash) / *shares)
		}
		model = "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE"
	case "cyclical":
		if cycleEPS != nil {
			low, high = *cycleEPS*10, *cycleEPS*15
			methods["周期中值PE"] = roundCurrency(*cycleEPS * 12.5)
		}
		model = "周期中值 EPS × PE"
	case "high-growth-chip":
		chipForwardEPS := forward
		if chipForwardEPS != nil {
			if normalizedEPS != nil {
				forwardCap := math.Max(*normalizedEPS*5, 12)
				if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
					forwardCap = math.Min(forwardCap, *f.TargetMeanPrice/35)
				}
				if *chipForwardEPS > forwardCap {
					capped := forwardCap
					chipForwardEPS = &capped
				}
			}
			if f.Regime == RegimeSlowdown {
				discounted := *chipForwardEPS * .70
				chipForwardEPS = &discounted
			}
			reportedForward = copyPointer(chipForwardEPS)
			methods["远期 EPS × PE"] = roundCurrency(*chipForwardEPS * 27.5)
		}
		if shares != nil && f.EBITDA != nil && *f.EBITDA > 0 {
			forwardEBITDA := *f.EBITDA * (1 + clamp(growth*.70, .08, .30))
			evMultiple := clamp(valueOr(f.EnterpriseToEBITDA, 30), 25, 35)
			methods["远期 EV/EBITDA"] = roundCurrency((forwardEBITDA*evMultiple - debt + cash) / *shares)
		}
		if shares != nil && f.Revenue != nil && *f.Revenue > 0 {
			forwardRevenue := *f.Revenue * (1 + clamp(growth, .08, .35))
			psMultiple := clamp(valueOr(f.PriceToSales, 12)*.35, 10, 15)
			methods["远期 P/S"] = roundCurrency((forwardRevenue*psMultiple - debt + cash) / *shares)
		}
		model = "高增长芯片：远期盈利 + EV/EBITDA / P/S"
	case "high-growth":
		peMid := clamp(classGrowth*100*1.05, 22, 45)
		peLow, peHigh := math.Max(18, peMid-5), math.Min(50, peMid+5)
		methods["Forward PEG"] = roundCurrency(growthForwardEPS * (peLow + peHigh) / 2)
		reportedForward = floatPointer(growthForwardEPS)
		model = "Forward EPS × PEG"
	}
	specializedLow, specializedHigh := low, high

	unavailable := func() Result {
		warning := "公开财务数据不足，未生成有效估值候选"
		result := Result{
			Method: model, Methods: methods, Status: StatusUnavailable,
			Warning: warning, Warnings: []string{warning}, ConfidenceLabel: "low",
			SafetyMargin: valueOr(f.SafetyMargin, defaultSafetyMargin),
			ForwardEPS:   copyPointer(reportedForward), DataQualityScore: dataQualityScore(f),
			NormalizedEPSSource: normalizedEPSSource(f), MarketCapDataQuality: marketCapDataQuality(f),
			ShareholderTotalReturnYield: shareholderBuybackYield(f), InterestCoverage: interestCoverage(f),
			Regime: f.Regime, RegimeSignals: f.RegimeSignals,
			IndustryModel: specializedIndustry, AIExposed: aiExposed,
		}
		if f.Regime != "" {
			result.ModelUnderRegime = model + " (" + f.Regime + ")"
		}
		if momentum := quarterlyMomentum(f); momentum != nil {
			result.QuarterlyMomentum = momentum
		}
		if percentiles := historicalValuationPercentiles(f); percentiles != nil {
			result.HistoricalValuationPercentiles = percentiles
		}
		return result
	}
	if len(methods) == 0 && (specializedIndustry || modelClass == "financial" || modelClass == "cruise") {
		return unavailable(), true
	}
	if len(methods) == 0 {
		return Result{}, false
	}
	valid := make([]float64, 0, len(methods))
	for _, value := range methods {
		if isPositiveFinite(value) {
			valid = append(valid, value)
		}
	}
	if len(valid) == 0 {
		if specializedIndustry || modelClass == "financial" || modelClass == "cruise" {
			return unavailable(), true
		}
		return Result{}, false
	}
	priceAnchor := 0.0
	if f.CurrentPrice != nil && *f.CurrentPrice > 0 {
		priceAnchor = *f.CurrentPrice
	} else if f.MarketCap != nil && f.SharesOutstanding != nil && *f.MarketCap > 0 && *f.SharesOutstanding > 0 {
		priceAnchor = *f.MarketCap / *f.SharesOutstanding
	}
	epsAnchor := valueOr(reportedForward, valueOr(normalizedEPS, 0))
	valid = filterUnitAnomalyCandidates(valid, epsAnchor, priceAnchor, defenseTransition || highGrowthInfrastructure)
	sort.Float64s(valid)
	value := valid[len(valid)/2]
	anchorWarning := ""
	if priceAnchor > 0 && value > priceAnchor*5 {
		value = roundCurrency(priceAnchor * 1.5)
		anchorWarning = "模型估值远高于现价，Yahoo 财务数据口径异常，已降级到现价锚定"
	}
	switch modelClass {
	case "banking", "healthcare-plans", "insurance", "biotech", "utility", "internet", "energy", "cyclical":
		if low > 0 && high >= low {
			value = roundCurrency((low + high) / 2)
		}
	}
	if modelClass == "cruise" {
		spread := .22
		if len(valid) > 1 {
			spread = .18
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	if modelClass == "high-growth-chip" {
		spread := .22
		if len(valid) > 1 {
			spread = .18
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	if modelClass == "high-growth" {
		low, high = value*.78, value*1.22
	}
	if modelClass == "high-growth-infrastructure" && (low <= 0 || high < low) {
		low, high = value*.80, value*1.20
	}
	if modelClass == "mature" {
		spread := .22
		if len(valid) > 1 {
			spread = .18
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	if modelClass == "ai-storage-growth" || modelClass == "ai-storage-cycle" {
		spread := .20
		if f.Regime == RegimeWinter {
			spread = .22
			if len(valid) > 1 {
				spread = .18
			}
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	if modelClass == "ai-winter" {
		low, high = value*.78, value*1.22
	}
	if modelClass == "digital-retail" {
		// Python's digital-retail candidate is the sole valid stable-company
		// anchor, so the main interval uses its 15% single-candidate spread.
		low, high = value*.85, value*1.15
	}
	if modelClass == "asset-heavy" {
		spread := .22
		if len(valid) > 1 {
			spread = .18
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	if low <= 0 || high <= 0 || high < low {
		spread := valueRangeSpread
		if modelClass == "financial" {
			spread = .22
			if len(valid) > 1 {
				spread = .18
			} else if containsAny(industryContext,
				"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores") {
				spread = .15
			}
		} else if modelClass == "stable-cashflow" {
			spread = .15
		}
		low, high = value*(1-spread), value*(1+spread)
	}
	margin := valueOr(f.SafetyMargin, defaultSafetyMargin)
	switch modelClass {
	case "internet":
		margin = math.Max(margin, .40)
	case "memory":
		margin = math.Max(margin, .40)
	case "ai-storage-growth":
		margin = math.Max(margin, .35)
	case "ai-storage-cycle":
		margin = math.Max(margin, .40)
	case "high-growth-chip":
		margin = math.Max(margin, .30)
	case "high-growth-infrastructure":
		margin = math.Max(margin, .50)
	case "high-growth":
		margin = math.Max(margin, .45)
	case "digital-retail":
		margin = .18
	case "banking":
		margin = .25
	case "healthcare-plans":
		margin = .30
	case "insurance":
		margin = .35
	case "biotech":
		margin = .45
	case "energy":
		margin = .35
	case "utility":
		margin = .18
	case "asset-heavy":
		margin = math.Max(margin, .42)
		if normalizedBeta(f.Beta) > 1.5 {
			margin = math.Max(margin, .47)
		}
	case "mature":
		margin = .20
		if normalizedBeta(f.Beta) > 1.5 {
			margin = .25
		}
	case "stable-cashflow":
		margin = .18
		if normalizedBeta(f.Beta) > 1.5 {
			margin = .20
		}
	case "financial":
		margin = .25
	case "cyclical":
		margin = math.Max(margin, .62)
	}
	if normalizedBeta(f.Beta) > 1.5 {
		margin = math.Min(.70, margin+.05)
	}
	if f.Regime == RegimeWinter && (modelClass == "memory" || modelClass == "ai-storage-cycle" || modelClass == "ai-storage-growth" || modelClass == "internet" || modelClass == "mobility") {
		margin = math.Max(margin, .40)
		if modelClass == "ai-storage-cycle" || modelClass == "ai-storage-growth" {
			margin = math.Max(margin, .60)
		}
	} else if f.Regime == RegimeSlowdown && (modelClass == "memory" || modelClass == "ai-storage-cycle" || modelClass == "ai-storage-growth" || modelClass == "internet" || modelClass == "mobility") {
		margin = math.Max(margin, .35)
	}
	if modelClass == "ai-winter" {
		margin = .62
		if normalizedBeta(f.Beta) > 1.5 {
			margin = .67
		}
	}
	warning := ""
	var warnings []string
	if specializedIndustry {
		warning = "行业专用估值模型已启用，仍需结合行业指标与安全边际判断"
		if forwardEPSMissingWarning {
			warnings = append(warnings, "缺乏分析师远期 EPS，已自动降级为保守模型")
		}
		warnings = append(warnings, warning)
	}
	if modelClass == "high-growth-infrastructure" {
		warnings = append(warnings, highGrowthInfrastructureWarning)
		if warning == "" {
			warning = highGrowthInfrastructureWarning
		} else {
			warning += "; " + highGrowthInfrastructureWarning
		}
	}
	if anchorWarning != "" {
		warnings = append(warnings, anchorWarning)
		if warning == "" {
			warning = anchorWarning
		} else {
			warning += "; " + anchorWarning
		}
	}
	result := Result{
		Methods: methods, Method: model, Status: StatusReady, FairValue: floatPointer(roundCurrency(value)),
		FairValueLow: floatPointer(roundCurrency(low)), FairValueHigh: floatPointer(roundCurrency(high)),
		FairValueBuyLow: floatPointer(roundCurrency(low * (1 - margin))), FairValueBuyHigh: floatPointer(roundCurrency(high * (1 - margin))),
		SafetyMargin: margin, ForwardEPS: reportedForward, Confidence: .70, ConfidenceLabel: "medium", DataQualityScore: dataQualityScore(f),
		NormalizedEPSSource: normalizedEPSSource(f), MarketCapDataQuality: marketCapDataQuality(f),
		ShareholderTotalReturnYield: shareholderBuybackYield(f), Warning: warning, Warnings: warnings,
		Defensive:  specializedScenario(low, high, model+" 防守区间", math.Min(.95, margin+.10)),
		Optimistic: specializedScenario(low, high, model+" 乐观区间", math.Max(.10, margin-.10)),
		Regime:     f.Regime, RegimeSignals: f.RegimeSignals,
		InterestCoverage: interestCoverage(f),
		IndustryModel:    specializedIndustry,
		AIExposed:        aiExposed,
	}
	if specializedIndustry {
		defensiveSpread := .22
		if containsAny(industryContext, "consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores") {
			defensiveSpread = .18
		}
		result.Defensive = scenario(value, defensiveSpread, model+" 压力测试", margin)
		result.Optimistic = specializedScenario(low, high, model, margin)
		result.Optimistic["forward_eps"] = nil
		if reportedForward != nil {
			result.Optimistic["forward_eps"] = roundDecimal(*reportedForward, 4)
		}
		result.Optimistic["growth_rate"] = roundDecimal(clamp(growth, 0, .50), 4)
	}
	if modelClass == "high-growth-infrastructure" {
		defensiveEPS := positivePtr(normalizedEPS)
		if forwardEPS := positivePtr(forward); forwardEPS != nil {
			if defensiveEPS == nil {
				haircutEPS := *forwardEPS * .75
				defensiveEPS = &haircutEPS
			} else {
				cappedForward := math.Min(*forwardEPS, *defensiveEPS*1.25)
				defensiveEPS = floatPointer(math.Max(*defensiveEPS, cappedForward))
			}
		}
		defensiveHigh := 0.0
		if defensiveEPS != nil {
			defensiveLow := *defensiveEPS * 15
			defensiveHigh = *defensiveEPS * 20
			defensiveMargin := math.Max(.45, margin)
			if f.FreeCashFlow != nil && *f.FreeCashFlow < 0 {
				defensiveMargin = math.Max(.50, defensiveMargin)
			}
			result.Defensive = specializedScenario(defensiveLow, defensiveHigh, "高增长基础设施：正常化 EPS × 15–20 倍 PE", defensiveMargin)
		} else {
			if shares != nil && f.Revenue != nil && isPositiveFinite(*f.Revenue) {
				defensiveRevenue := valueOr(f.CurrentYearRevenueEstimate, 0)
				if !isPositiveFinite(defensiveRevenue) {
					defensiveGrowth := clamp(valueOr(f.RevenueGrowth, growth), .20, .30)
					defensiveRevenue = *f.Revenue * (1 + clamp(defensiveGrowth*.5, .10, .15))
				}
				defensiveRevenue = clamp(defensiveRevenue, *f.Revenue, infrastructureForwardRevenue)
				defensiveLow := math.Max(0, (defensiveRevenue*4-debt+cash) / *shares)
				defensiveHigh = math.Max(0, (defensiveRevenue*6-debt+cash) / *shares)
				if defensiveLow > 0 && defensiveHigh >= defensiveLow {
					defensiveMargin := math.Max(.50, margin)
					result.Defensive = specializedScenario(defensiveLow, defensiveHigh, "高增长基础设施：本年收入 EV/Revenue 4–6 倍防守区间", defensiveMargin)
					result.Defensive["revenue_estimate"] = roundCurrency(defensiveRevenue)
					result.Warnings = append(result.Warnings, "缺少可用正向 EPS，保守场景改用本年收入 EV/Revenue 4–6 倍并扣除净债务")
				}
			}
			if result.Defensive == nil {
				defensiveHigh = 0
				result.Warnings = append(result.Warnings, "缺少可用正向 EPS 或收入净债务估值无法得出正值，保守区间暂不可用")
			}
		}

		optimisticLow, optimisticHigh := math.MaxFloat64, 0.0
		for _, candidate := range []float64{infrastructurePSLow, infrastructurePELow} {
			if isPositiveFinite(candidate) {
				optimisticLow = math.Min(optimisticLow, candidate)
			}
		}
		for _, candidate := range []float64{infrastructurePSHigh, infrastructurePEHigh} {
			if isPositiveFinite(candidate) {
				optimisticHigh = math.Max(optimisticHigh, candidate)
			}
		}
		if optimisticLow < math.MaxFloat64 {
			if defensiveHigh > 0 {
				optimisticLow = math.Max(optimisticLow, defensiveHigh)
			}
			optimisticHigh = math.Max(optimisticLow, optimisticHigh)
			result.Optimistic = specializedScenario(optimisticLow, optimisticHigh, "高增长基础设施：前瞻 P/S 10–15 倍 + 远期 PE", .40)
			if reportedForward != nil {
				result.Optimistic["forward_eps"] = roundDecimal(*reportedForward, 4)
			}
			result.Optimistic["growth_rate"] = roundDecimal(clamp(valueOr(f.RevenueGrowth, growth), .20, .30), 4)
			result.Optimistic["price_to_sales_low"] = 10.0
			result.Optimistic["price_to_sales_high"] = 15.0
			if infrastructureForwardRevenue > 0 {
				result.Optimistic["forward_revenue_estimate"] = roundCurrency(infrastructureForwardRevenue)
			}
		}
	}
	if modelClass == "healthcare-plans" {
		earnings := positivePtr(normalizedEPS)
		if earnings == nil {
			earnings = positivePtr(forward)
		}
		if earnings != nil {
			result.Defensive = specializedScenario(*earnings*16, *earnings*19, model+" 压力测试", margin)
			optimisticEPS := *earnings
			if forwardEPS := positivePtr(forward); forwardEPS != nil {
				optimisticEPS = math.Max(optimisticEPS, math.Min(*forwardEPS, optimisticEPS*1.15))
			}
			result.Optimistic = specializedScenario(optimisticEPS*19, optimisticEPS*24, model+"：远期盈利恢复", margin)
			result.Optimistic["forward_eps"] = roundDecimal(optimisticEPS, 4)
			result.Optimistic["growth_rate"] = roundDecimal(clamp(growth, 0, .50), 4)
		}
	}
	if modelClass == "asset-heavy" {
		defensiveMid := value
		defensiveSpread := .22
		if len(valid) > 1 {
			defensiveSpread = .18
		}
		if retailEPS > 0 {
			defensiveMid = retailEPS * 17.5
			defensiveSpread = .25
			if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
				defensiveMid = math.Max(defensiveMid, *f.TargetMeanPrice*.45)
			}
		} else {
			defensiveCandidates := make([]float64, 0, 5)
			for _, name := range []string{"核心业务远期 PE", "制造/云业务 P/S", "转型 EV/EBITDA", "传统业务 PE", "前瞻 P/S", "EV/EBITDA"} {
				if candidate := methods[name]; isPositiveFinite(candidate) {
					defensiveCandidates = append(defensiveCandidates, candidate)
				}
			}
			if f.FreeCashFlow != nil && *f.FreeCashFlow < 0 && f.EBITDA != nil && *f.EBITDA > 0 && shares != nil {
				candidate := (*f.EBITDA*8 - debt + cash) / *shares
				if isPositiveFinite(candidate) {
					defensiveCandidates = append(defensiveCandidates, candidate)
				}
			}
			if len(defensiveCandidates) > 0 {
				sort.Float64s(defensiveCandidates)
				defensiveMid = defensiveCandidates[len(defensiveCandidates)/2]
			}
			if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
				defensiveMid = math.Max(defensiveMid, *f.TargetMeanPrice*.45)
			}
		}
		defensiveMargin := math.Min(.50, math.Max(.45, margin))
		result.Defensive = scenario(defensiveMid, defensiveSpread, model+" 压力测试", defensiveMargin)

		fundamentalHigh := defensiveMid
		for _, name := range []string{"核心业务远期 PE", "制造/云业务 P/S", "转型 EV/EBITDA", "传统业务 PE", "前瞻 P/S", "EV/EBITDA"} {
			if candidate := methods[name]; isPositiveFinite(candidate) {
				fundamentalHigh = math.Max(fundamentalHigh, candidate)
			}
		}
		if f.FreeCashFlow != nil && *f.FreeCashFlow < 0 && f.EBITDA != nil && *f.EBITDA > 0 && shares != nil {
			fundamentalHigh = math.Max(fundamentalHigh, (*f.EBITDA*8-debt+cash) / *shares)
		}
		anchor := value
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			anchor = *f.TargetMeanPrice
		}
		optimisticMid := (anchor*.65 + fundamentalHigh*.35)
		optimisticMid = math.Max(defensiveMid*1.10, optimisticMid)
		optimisticMid = math.Min(defensiveMid*2.50, optimisticMid)
		result.Optimistic = scenario(optimisticMid, .20, "转型期混合估值：共识 + P/S / EV/EBITDA", .40)
		if reportedForward != nil {
			result.Optimistic["forward_eps"] = roundDecimal(*reportedForward, 4)
		} else {
			result.Optimistic["forward_eps"] = nil
		}
		result.Optimistic["growth_rate"] = roundDecimal(clamp(growth, 0, .25), 4)
	}
	if forwardEPSMissingWarning && !specializedIndustry {
		result.Warnings = append(result.Warnings, "缺乏分析师远期 EPS，已自动降级为保守模型")
	}
	if modelClass == "financial" {
		defensiveCandidates := make([]float64, 0, 5)
		for _, name := range []string{"DDM", "P/B"} {
			if candidate := methods[name]; isPositiveFinite(candidate) {
				defensiveCandidates = append(defensiveCandidates, candidate)
			}
		}
		if matureGrowth && normalizedEPS != nil {
			matureForwardEPS := modeledForwardEPS(f, growth)
			defensiveEPS := math.Max(*normalizedEPS, math.Min(matureForwardEPS, *normalizedEPS*1.15))
			defensiveCandidates = append(defensiveCandidates, defensiveEPS*25)
		}
		if cycleEPS != nil {
			stable := containsAny(industryContext,
				"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores")
			peLow, peHigh := 12.0, 20.0
			if stable {
				peLow, peHigh = 15, 21
			} else if cyclical {
				peLow, peHigh = 10, 16
			}
			defensiveCandidates = append(defensiveCandidates, *cycleEPS*(peLow+peHigh)/2)
		}
		if f.FreeCashFlow != nil && *f.FreeCashFlow < 0 && f.EBITDA != nil && *f.EBITDA > 0 && shares != nil {
			candidate := (*f.EBITDA*8 - debt + cash) / *shares
			if isPositiveFinite(candidate) {
				defensiveCandidates = append(defensiveCandidates, candidate)
			}
		}
		defensiveMid, defensiveSpread := value, .22
		if len(defensiveCandidates) > 0 {
			sort.Float64s(defensiveCandidates)
			defensiveMid, defensiveSpread = defensiveCandidates[len(defensiveCandidates)/2], .20
		}
		result.Defensive = scenario(defensiveMid, defensiveSpread, model+" 压力测试", .25)

		industryValues := make([]float64, 0, 2)
		for _, name := range []string{"DDM", "P/B"} {
			if candidate := methods[name]; isPositiveFinite(candidate) {
				industryValues = append(industryValues, candidate)
			}
		}
		if len(industryValues) == 0 {
			result.Optimistic = nil
		} else {
			sort.Float64s(industryValues)
			optimisticLow, optimisticHigh := industryValues[0], industryValues[len(industryValues)-1]
			if f.TargetLowPrice != nil && *f.TargetLowPrice > 0 {
				optimisticLow = math.Min(optimisticLow, *f.TargetLowPrice)
			}
			if f.TargetHighPrice != nil && *f.TargetHighPrice > 0 {
				optimisticHigh = math.Max(optimisticHigh, *f.TargetHighPrice)
			}
			result.Optimistic = map[string]any{
				"value": roundCurrency((optimisticLow + optimisticHigh) / 2),
				"low":   roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
				"buy_low": roundCurrency(optimisticLow * .75), "buy_high": roundCurrency(optimisticHigh * .75),
				"model": "金融/地产：DDM + P/B", "safety_margin": .25,
			}
		}
	}
	if modelClass == "ai-winter" {
		scenarioMargin := .60
		if normalizedBeta(f.Beta) > 1.5 {
			scenarioMargin = .70
		}
		result.Defensive = scenario(value, .22, model+" 压力测试", scenarioMargin)
		result.Optimistic = scenario(value, .20, model, scenarioMargin)
		for _, scenario := range []map[string]any{result.Defensive, result.Optimistic} {
			scenario["forward_eps"] = nil
			scenario["growth_rate"] = roundDecimal(growth, 4)
		}
	}
	if modelClass == "cyclical" {
		defensiveCandidates := make([]float64, 0, 3)
		if cycleEPS != nil {
			defensiveCandidates = append(defensiveCandidates, *cycleEPS*13)
		}
		requiredReturn := valueOr(f.RequiredReturn, defaultRequiredReturn)
		terminalGrowth := valueOr(f.TerminalGrowth, clamp(classGrowth*.25, .015, .035))
		if ownerEarnings, ok := resolveOwnerEarnings(f); ok && shares != nil && requiredReturn > terminalGrowth {
			ownerCandidate := (ownerEarnings / *shares) / math.Max(requiredReturn-terminalGrowth, .045)
			if isPositiveFinite(ownerCandidate) {
				defensiveCandidates = append(defensiveCandidates, ownerCandidate)
			}
		}
		if fcfPerShare, ok := resolveFCFPerShare(f); ok && requiredReturn > terminalGrowth {
			dcfCandidate := dcfPerShare(fcfPerShare, valuationDCFRate(f, classGrowth), requiredReturn, terminalGrowth)
			if isPositiveFinite(dcfCandidate) {
				defensiveCandidates = append(defensiveCandidates, dcfCandidate)
			}
		}
		defensiveMid := value
		if len(defensiveCandidates) > 0 {
			sort.Float64s(defensiveCandidates)
			defensiveMid = defensiveCandidates[len(defensiveCandidates)/2]
		}
		result.Defensive = scenario(defensiveMid, .22, model+" 压力测试", margin)
		result.Optimistic = nil
	}
	if modelClass == "high-growth" {
		defensiveCandidates := make([]float64, 0, 3)
		if normalizedEPS != nil {
			defensiveEPS := *normalizedEPS
			// 收入高速增长且远期 EPS 来自分析师时，用远期盈利建立防守锚点。
			if f.ForwardEPSObserved && forward != nil && valueOr(f.RevenueGrowth, 0) >= .50 {
				defensiveEPS = math.Max(defensiveEPS, *forward)
			}
			defensiveCandidates = append(defensiveCandidates, defensiveEPS*16)
		}
		requiredReturn := valueOr(f.RequiredReturn, defaultRequiredReturn)
		terminalGrowth := valueOr(f.TerminalGrowth, clamp(classGrowth*.25, .015, .035))
		if ownerEarnings, ok := resolveOwnerEarnings(f); ok && shares != nil && requiredReturn > terminalGrowth {
			ownerCandidate := (ownerEarnings / *shares) / math.Max(requiredReturn-terminalGrowth, .045)
			if isPositiveFinite(ownerCandidate) {
				defensiveCandidates = append(defensiveCandidates, ownerCandidate)
			}
		}
		if fcfPerShare, ok := resolveFCFPerShare(f); ok && requiredReturn > terminalGrowth {
			dcfCandidate := dcfPerShare(fcfPerShare, valuationDCFRate(f, classGrowth), requiredReturn, terminalGrowth)
			if isPositiveFinite(dcfCandidate) {
				defensiveCandidates = append(defensiveCandidates, dcfCandidate)
			}
		}
		defensiveMid := value
		if len(defensiveCandidates) > 0 {
			sort.Float64s(defensiveCandidates)
			defensiveMid = defensiveCandidates[len(defensiveCandidates)/2]
		}
		result.Defensive = scenario(defensiveMid, .22, model+" 压力测试", margin)
		optimisticGrowth := clamp(classGrowth, .06, .40)
		optimisticPE := clamp(optimisticGrowth*150, 22, 50)
		optimisticMid := growthForwardEPS * optimisticPE
		optimisticModel, optimisticMargin := "远期 EPS × PEG", .48
		if defensiveMid > 0 && optimisticMid/defensiveMid > 3 {
			consensus := value
			if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
				consensus = *f.TargetMeanPrice
			}
			optimisticMid = math.Min(defensiveMid*3, math.Max(defensiveMid*1.10, (consensus+value)/2))
			optimisticModel, optimisticMargin = "混合估值锚点：共识 + 基本面交叉验证", .35
			warning := fmt.Sprintf("远期模型与防守模型差距 %.1f 倍，已启用混合估值锚点", (growthForwardEPS*clamp(optimisticGrowth*150, 22, 50))/defensiveMid)
			result.Warnings = append(result.Warnings, warning)
			if result.Warning == "" {
				result.Warning = warning
			} else {
				result.Warning += "; " + warning
			}
		}
		result.Optimistic = scenario(optimisticMid, .18, optimisticModel, optimisticMargin)
		result.Optimistic["forward_eps"] = roundDecimal(growthForwardEPS, 4)
		result.Optimistic["growth_rate"] = roundDecimal(optimisticGrowth, 4)
	}
	if modelClass == "stable-cashflow" && normalizedEPS != nil {
		defensivePE := 20.0
		if freeCashFlowMargin >= .15 {
			defensivePE = 24
		}
		defensiveMid := *normalizedEPS * defensivePE
		defensiveMargin := math.Max(.20, margin)
		result.Defensive = scenario(defensiveMid, .18, "稳定消费：正常化 EPS 压力测试", defensiveMargin)
		if forward != nil && *forward > 0 {
			optimisticGrowth := clamp(growth, .06, .40)
			optimisticPE := clamp(optimisticGrowth*100*1.5, 22, 50)
			optimisticMid := *forward * optimisticPE
			result.Optimistic = scenario(optimisticMid, .15, "远期 EPS × PEG", .30)
			result.Optimistic["forward_eps"] = roundDecimal(*forward, 4)
			result.Optimistic["growth_rate"] = roundDecimal(optimisticGrowth, 4)
		}
	}
	if modelClass == "mature" && normalizedEPS != nil {
		defensiveEPS := math.Max(*normalizedEPS, math.Min(valueOr(forward, *normalizedEPS), *normalizedEPS*1.15))
		defensivePE := 25.0
		defensiveSpread := .22
		if premiumMatureGrowth {
			defensivePE = 32
			defensiveSpread = 2.5 / 27.5
		}
		result.Defensive = scenario(defensiveEPS*defensivePE, defensiveSpread, "成熟增长型巨头：正常化 EPS 防守估值", margin)

		optimisticEPS := math.Max(*normalizedEPS, math.Min(valueOr(forward, *normalizedEPS), *normalizedEPS*1.35))
		lowPE, highPE, optimisticPE := 25.0, 35.0, 27.5
		optimisticSpread, optimisticMargin := .18, .18
		if normalizedBeta(f.Beta) > 1.5 {
			optimisticMargin = .20
		}
		if premiumMatureGrowth {
			lowPE, highPE, optimisticPE = 30, 38, 34
			optimisticSpread, optimisticMargin = 0, .20
		}
		optimistic := scenario(optimisticEPS*optimisticPE, optimisticSpread, "成熟增长 PE：远期正常化 EPS", optimisticMargin)
		optimistic["low"] = roundCurrency(optimisticEPS * lowPE)
		optimistic["high"] = roundCurrency(optimisticEPS * highPE)
		optimistic["buy_low"] = roundCurrency(optimisticEPS * lowPE * (1 - optimisticMargin))
		optimistic["buy_high"] = roundCurrency(optimisticEPS * highPE * (1 - optimisticMargin))
		optimistic["forward_eps"] = roundDecimal(optimisticEPS, 4)
		optimistic["growth_rate"] = roundDecimal(clamp(growth, .06, .20), 4)
		if premiumMatureGrowth {
			optimistic["model"] = "成熟增长 PE：远期正常化 EPS × 30–38"
		} else {
			optimistic["model"] = "成熟增长 PE：远期正常化 EPS × 25–35"
		}
		result.Optimistic = optimistic
	}
	if modelClass == "cruise" && shares != nil {
		ev8, ev10, ev12 := 0.0, 0.0, 0.0
		if cruiseEBITDA != nil {
			ev8 = (*cruiseEBITDA*8 - debt + cash) / *shares
			ev10 = (*cruiseEBITDA*10 - debt + cash) / *shares
			ev12 = (*cruiseEBITDA*12 - debt + cash) / *shares
		}
		if ev8 > 0 && ev10 > 0 {
			spread := clamp((ev10-ev8)/math.Max(ev10+ev8, 1), .12, .25)
			result.Defensive = scenario((ev8+ev10)/2, spread, model+" 压力测试", .25)
		}
		pe12, pe15 := 0.0, 0.0
		if forward != nil && *forward > 0 {
			pe12, pe15 = *forward*12, *forward*15
		}
		consensusLow, consensusHigh := 0.0, 0.0
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			consensusLow, consensusHigh = *f.TargetMeanPrice*.70, *f.TargetMeanPrice*.85
		}
		optimisticLow := math.Max(ev10, math.Max(pe12, consensusLow))
		optimisticHigh := math.Max(ev12, math.Max(pe15, consensusHigh))
		if optimisticLow > 0 && optimisticHigh >= optimisticLow {
			optimisticMargin := .25
			result.Optimistic = scenario((optimisticLow+optimisticHigh)/2, (optimisticHigh-optimisticLow)/math.Max(optimisticHigh+optimisticLow, 1), "重资产消费服务：EV/EBITDA 10–12x + 远期 PE 12–15x", optimisticMargin)
			result.Optimistic["low"] = roundCurrency(optimisticLow)
			result.Optimistic["high"] = roundCurrency(optimisticHigh)
			result.Optimistic["buy_low"] = roundCurrency(optimisticLow * (1 - optimisticMargin))
			result.Optimistic["buy_high"] = roundCurrency(optimisticHigh * (1 - optimisticMargin))
			if forward != nil {
				result.Optimistic["forward_eps"] = roundDecimal(*forward, 4)
			}
			result.Optimistic["growth_rate"] = roundDecimal(clamp(growth, .05, .25), 4)
			result.Optimistic["safety_margin"] = optimisticMargin
		}
	}
	if modelClass == "digital-retail" && retailEPS > 0 && shares != nil {
		defensiveMid := retailEPS*18 + 30
		result.Defensive = scenario(defensiveMid, 10/defensiveMid, model+" 压力测试", .20)
		optimisticLow, optimisticHigh := retailEPS*18+30, retailEPS*18+50
		optimisticMargin := .20
		result.Optimistic = map[string]any{
			"value": roundCurrency(retailEPS*18 + 40), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
			"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
			"model": "防御性数字化零售：远期 EPS × 15–20 + 数字化溢价", "safety_margin": optimisticMargin,
			"forward_eps": roundDecimal(retailEPS, 4), "growth_rate": roundDecimal(clamp(growth, .03, .20), 4),
		}
	}
	if modelClass == "high-growth-chip" && forward != nil && *forward > 0 {
		chipForwardEPS := valueOr(reportedForward, *forward)
		defensiveMargin := .30
		defensiveLow, defensiveHigh := chipForwardEPS*20, chipForwardEPS*25
		result.Defensive = map[string]any{
			"value": roundCurrency(chipForwardEPS * 22.5), "low": roundCurrency(defensiveLow), "high": roundCurrency(defensiveHigh),
			"buy_low": roundCurrency(defensiveLow * (1 - defensiveMargin)), "buy_high": roundCurrency(defensiveHigh * (1 - defensiveMargin)),
			"model": "高增长芯片：远期 EPS × 20–25 倍 PE 压力测试", "safety_margin": defensiveMargin,
		}
		optimisticLowPE, optimisticHighPE, optimisticMargin := 30.0, 35.0, .25
		if f.Regime == RegimeSlowdown {
			optimisticLowPE, optimisticHighPE, optimisticMargin = 15, 25, .40
		}
		optimisticLow, optimisticHigh := chipForwardEPS*optimisticLowPE, chipForwardEPS*optimisticHighPE
		optimisticModel := "高增长芯片：远期 EPS × 30–35，并用 EV/EBITDA / P/S 验证"
		if f.Regime == RegimeSlowdown {
			optimisticModel = "高增长芯片：远期 EPS × 15–25（放缓）"
		}
		result.Optimistic = map[string]any{
			"value": roundCurrency((optimisticLow + optimisticHigh) / 2), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
			"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
			"model": optimisticModel, "safety_margin": optimisticMargin,
			"forward_eps": roundDecimal(chipForwardEPS, 4), "growth_rate": roundDecimal(clamp(growth, .08, .35), 4),
		}
	}
	if (modelClass == "ai-storage-growth" || modelClass == "ai-storage-cycle") && reportedForward != nil && *reportedForward > 0 {
		storageForward := *reportedForward
		if f.Regime == RegimeWinter {
			result.Defensive = scenario(value, .20, "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线", .60)
			result.Optimistic = scenario(value, .20, "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线", .60)
		} else if modelClass == "ai-storage-growth" {
			defensiveLow, defensiveHigh := storageForward*35, storageForward*45
			result.Defensive = map[string]any{
				"value": roundCurrency(storageForward * 40), "low": roundCurrency(defensiveLow), "high": roundCurrency(defensiveHigh),
				"buy_low": roundCurrency(defensiveLow * .65), "buy_high": roundCurrency(defensiveHigh * .65),
				"model": "AI 存储结构性增长：远期 EPS × 35–45 倍 PE 压力测试", "safety_margin": .35,
			}
			optimisticLowPE, optimisticHighPE, optimisticMargin := 35.0, 55.0, .35
			if f.Regime == RegimeSlowdown {
				optimisticLowPE, optimisticHighPE, optimisticMargin = 20, 30, .45
			}
			optimisticLow, optimisticHigh := storageForward*optimisticLowPE, storageForward*optimisticHighPE
			optimisticModel := "AI 存储结构性增长：远期 EPS × 35–55 倍 PE"
			if f.Regime == RegimeSlowdown {
				optimisticModel = "AI 存储结构性增长：远期 EPS × 20–30 倍 PE（放缓）"
			}
			result.Optimistic = map[string]any{
				"value": roundCurrency((optimisticLow + optimisticHigh) / 2), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
				"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
				"model": optimisticModel, "safety_margin": optimisticMargin,
				"forward_eps": roundDecimal(storageForward, 4), "growth_rate": roundDecimal(clamp(growth, .10, .50), 4),
			}
			result.Warnings = append(result.Warnings, "AI 存储结构性增长仍有周期回撤风险，采用远期 EPS × 35–55 倍 PE 与 35% 安全边际")
		} else {
			cycleForward := storageForward * .70
			defensiveLow, defensiveHigh := cycleForward*20, cycleForward*25
			result.Defensive = map[string]any{
				"value": roundCurrency(cycleForward * 22.5), "low": roundCurrency(defensiveLow), "high": roundCurrency(defensiveHigh),
				"buy_low": roundCurrency(defensiveLow * .60), "buy_high": roundCurrency(defensiveHigh * .60),
				"model": "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE 压力测试", "safety_margin": .40,
			}
			optimisticLowPE, optimisticHighPE, optimisticMargin := 20.0, 30.0, .40
			if f.Regime == RegimeSlowdown {
				optimisticLowPE, optimisticHighPE, optimisticMargin = 15, 25, .50
			}
			optimisticLow, optimisticHigh := storageForward*optimisticLowPE, storageForward*optimisticHighPE
			optimisticModel := "AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE"
			if f.Regime == RegimeSlowdown {
				optimisticModel = "AI 存储/硬件周期：景气放缓 EPS × 15–25 倍 PE"
			}
			result.Optimistic = map[string]any{
				"value": roundCurrency((optimisticLow + optimisticHigh) / 2), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
				"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
				"model": optimisticModel, "safety_margin": optimisticMargin,
				"forward_eps": roundDecimal(storageForward, 4), "growth_rate": roundDecimal(clamp(growth, .10, .50), 4),
			}
			result.Warnings = append(result.Warnings, "AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际")
		}
	}
	if modelClass == "defense" {
		spread := .22
		if len(valid) > 1 {
			spread = .18
		}
		result.FairValueLow = floatPointer(roundCurrency(value * (1 - spread)))
		result.FairValueHigh = floatPointer(roundCurrency(value * (1 + spread)))
		result.FairValueBuyLow = floatPointer(roundCurrency(value * (1 - spread) * (1 - margin)))
		result.FairValueBuyHigh = floatPointer(roundCurrency(value * (1 + spread) * (1 - margin)))

		defensiveAnchors := make([]float64, 0, 3)
		// Match Python's defense-transition anchor set: scenario A selects the
		// highest of fair value, 6x forward revenue, 80% of analyst target-low,
		// and 70% of analyst consensus.
		defenseGrowth := clamp(growth, .20, .40)
		defenseForwardRevenue := valueOr(f.Revenue, 0) * (1 + defenseGrowth)
		if shares != nil && f.Revenue != nil && *f.Revenue > 0 {
			defensePSValue := (defenseForwardRevenue*6 - debt + cash) / *shares
			if defensePSValue > 0 {
				defensiveAnchors = append(defensiveAnchors, defensePSValue)
			}
		}
		if f.TargetLowPrice != nil && *f.TargetLowPrice > 0 {
			defensiveAnchors = append(defensiveAnchors, *f.TargetLowPrice*.80)
		}
		if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			defensiveAnchors = append(defensiveAnchors, *f.TargetMeanPrice*.70)
		}
		if value > 0 {
			defensiveAnchors = append(defensiveAnchors, value)
		}
		defensiveMid := 0.0
		for _, anchor := range defensiveAnchors {
			if anchor > defensiveMid {
				defensiveMid = anchor
			}
		}
		if len(defensiveAnchors) == 0 {
			defensiveMid = value
		}
		result.Defensive = scenario(defensiveMid, .22, model+" 压力测试", .45)

		consensusLow, consensusHigh := 0.0, 0.0
		if f.TargetLowPrice != nil && *f.TargetLowPrice > 0 {
			consensusLow = *f.TargetLowPrice
		} else if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			consensusLow = *f.TargetMeanPrice * .87
		}
		if f.TargetHighPrice != nil && *f.TargetHighPrice > 0 {
			consensusHigh = *f.TargetHighPrice
		} else if f.TargetMeanPrice != nil && *f.TargetMeanPrice > 0 {
			consensusHigh = *f.TargetMeanPrice * 1.15
		}
		optimisticLow := math.Max(specializedLow, consensusLow)
		optimisticHigh := math.Max(optimisticLow, math.Max(specializedHigh, consensusHigh))
		if optimisticLow > 0 && optimisticHigh >= optimisticLow {
			optimisticMargin := .45
			optimisticMid := (optimisticLow + optimisticHigh) / 2
			result.Optimistic = map[string]any{
				"value": roundCurrency(optimisticMid), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
				"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
				"model": "国防订单驱动转型：远期 P/S 6–8x + 分析师共识", "safety_margin": optimisticMargin,
				"growth_rate": roundDecimal(clamp(valueOr(f.GrowthRate, 0), .20, .40), 4),
			}
		}
	}
	if internetAIPlatform && low > 0 && high > 0 {
		// Match the Python platform scenario: keep the base 3x P/S floor, use a
		// 12x AI ceiling, and prevent the optimistic floor from undercutting the
		// defensive range. Churn quality adjusts the revenue anchors, not EPS.
		if f.Revenue != nil && *f.Revenue > 0 {
			if shares != nil {
				churn := valueOr(f.ChurnRate, 0)
				if churn > 1 {
					churn /= 100
				}
				quality := 1 - clamp(churn, 0, .30)
				revenuePerShare := *f.Revenue / *shares
				defensiveMid := value
				defensiveLow := defensiveMid * .78
				optimisticLow := math.Max(revenuePerShare*3*quality, defensiveLow)
				optimisticHigh := math.Max(revenuePerShare*12*quality, optimisticLow)
				optimisticValue := (optimisticLow + optimisticHigh) / 2
				optimisticMargin := .40
				result.Optimistic = map[string]any{
					"value": roundCurrency(optimisticValue), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
					"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
					"model": "AI 互联网平台：基础 P/S + AI 溢价 P/S 上限", "safety_margin": optimisticMargin,
					"forward_revenue_per_share": roundDecimal(revenuePerShare, 4),
					"growth_rate":               roundDecimal(clamp(valueOr(f.GrowthRate, 0), 0, .50), 4),
				}
			}
		}
	}
	if modelClass == "mobility" {
		defensiveEPS := mobilityOptimisticEPS
		if defensiveEPS <= 0 {
			defensiveEPS = valueOr(forward, 0)
		}
		if defensiveEPS > 0 {
			defensiveMargin := margin
			if f.FreeCashFlow != nil && *f.FreeCashFlow < 0 {
				defensiveMargin = math.Min(.70, math.Max(.20, margin+.10))
			}
			result.Defensive = scenario(defensiveEPS*17.5, .22, "汽车制造：远期 EPS × 17.5 压力测试", defensiveMargin)
		}
	}
	if modelClass == "mobility" && mobilityHasRevenueAnchor {
		optimisticLow, optimisticHigh := mobilityUpsideLow, mobilityUpsideHigh
		optimisticMargin := .35
		optimisticModel := "移动科技成长：远期收入 P/S 6–12x"
		if f.Regime == RegimeSlowdown {
			optimisticLow, optimisticHigh = (mobilityForwardRevenue*4-debt+cash)/mobilitySharesValue, (mobilityForwardRevenue*7-debt+cash)/mobilitySharesValue
			optimisticMargin = .45
			optimisticModel = "移动科技成长：远期收入 P/S 4–7x（放缓）"
		}
		if optimisticHigh > 0 {
			result.Optimistic = map[string]any{
				"value": roundCurrency((optimisticLow + optimisticHigh) / 2), "low": roundCurrency(math.Max(0, optimisticLow)), "high": roundCurrency(math.Max(optimisticLow, optimisticHigh)),
				"buy_low": roundCurrency(math.Max(0, optimisticLow) * (1 - optimisticMargin)), "buy_high": roundCurrency(math.Max(optimisticLow, optimisticHigh) * (1 - optimisticMargin)),
				"model": optimisticModel, "safety_margin": optimisticMargin,
				"forward_revenue": roundCurrency(mobilityForwardRevenue), "growth_rate": roundDecimal(clamp(growth, .05, .35), 4),
			}
		}
	} else if modelClass == "mobility" && mobilityOptimisticEPS > 0 {
		optimisticLowPE, optimisticHighPE, optimisticMargin := 30.0, 50.0, .35
		optimisticModel := "移动科技成长：远期 EPS × 30–50 倍 PE"
		if f.Regime == RegimeSlowdown {
			optimisticLowPE, optimisticHighPE, optimisticMargin = 22, 32, .45
			optimisticModel = "移动科技成长：远期 EPS × 22–32 倍 PE（放缓）"
		}
		optimisticLow, optimisticHigh := mobilityOptimisticEPS*optimisticLowPE, mobilityOptimisticEPS*optimisticHighPE
		result.Optimistic = map[string]any{
			"value": roundCurrency((optimisticLow + optimisticHigh) / 2), "low": roundCurrency(optimisticLow), "high": roundCurrency(optimisticHigh),
			"buy_low": roundCurrency(optimisticLow * (1 - optimisticMargin)), "buy_high": roundCurrency(optimisticHigh * (1 - optimisticMargin)),
			"model": optimisticModel, "safety_margin": optimisticMargin,
			"forward_eps": roundDecimal(mobilityOptimisticEPS, 4), "growth_rate": roundDecimal(clamp(growth, .05, .35), 4),
		}
	}
	if hasLowInterestCoverage(f) && result.Optimistic != nil {
		result.Warnings = append(result.Warnings, "利息覆盖率低于 2 倍，乐观估值已收紧")
		capHigh, ok := result.Optimistic["high"].(float64)
		if ok {
			capHigh *= .85
			result.Optimistic["high"] = roundCurrency(capHigh)
			if value, ok := result.Optimistic["value"].(float64); ok && value > capHigh {
				result.Optimistic["value"] = roundCurrency(capHigh)
			}
			if buyHigh, ok := result.Optimistic["buy_high"].(float64); ok {
				result.Optimistic["buy_high"] = roundCurrency(buyHigh * .85)
			}
		}
	}
	if f.Regime == RegimeWinter && aiWinter {
		result.Warnings = append(result.Warnings, "市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线")
	} else if f.Regime == RegimeSlowdown {
		result.Warnings = append(result.Warnings, "市场增速放缓，估值倍数上限已压缩")
	}
	if defenseTransition {
		warning := "估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动"
		result.Warnings = append(result.Warnings, warning)
		if result.Warning == "" {
			result.Warning = warning
		} else {
			result.Warning += "; " + warning
		}
	}
	if momentum := quarterlyMomentum(f); momentum != nil {
		result.QuarterlyMomentum = momentum
	}
	if percentiles := historicalValuationPercentiles(f); percentiles != nil {
		result.HistoricalValuationPercentiles = percentiles
	}
	if result.Regime == "" {
		result.Regime = RegimeNormal
	}
	result.ModelUnderRegime = model
	if result.Regime != RegimeNormal {
		result.ModelUnderRegime += " (" + result.Regime + ")"
	}
	if result.ForwardEPS != nil {
		rounded := roundDecimal(*result.ForwardEPS, 4)
		result.ForwardEPS = &rounded
	}
	return result, true
}

func specializedScenario(low, high float64, model string, margin float64) map[string]any {
	if low <= 0 || high <= 0 || high < low {
		return scenario(math.Max(low, high), 0, model, margin)
	}
	mid := (low + high) / 2
	spread := (high - low) / math.Max(low+high, 1)
	return scenario(mid, spread, model, margin)
}

func modeledForwardEPS(f Fundamentals, growth float64) float64 {
	if forward := positivePtr(f.ForwardEPS); forward != nil {
		return *forward
	}
	if f.NormalizedEPS != nil {
		if normalized := positivePtr(f.NormalizedEPS); normalized != nil {
			return *normalized * (1 + growth)
		}
	}
	values := make([]float64, 0, 6)
	if f.TrailingEPS != nil && isPositiveFinite(*f.TrailingEPS) {
		values = append(values, *f.TrailingEPS)
	}
	annual := f.AnnualEPS
	if len(annual) > 5 {
		annual = annual[:5]
	}
	for _, value := range annual {
		if isPositiveFinite(value) {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[len(values)/2] * (1 + growth)
}

func quarterlyMomentum(f Fundamentals) map[string]any {
	if len(f.QuarterlyRevenue) < 4 {
		return nil
	}

	latest, prior, yearAgo := f.QuarterlyRevenue[0], f.QuarterlyRevenue[1], f.QuarterlyRevenue[3]
	var revenueYoY, revenueQoQ, epsYoY any
	if yearAgo > 0 {
		revenueYoY = roundDecimal(latest/yearAgo-1, 4)
	}
	if prior > 0 {
		revenueQoQ = roundDecimal(latest/prior-1, 4)
	}
	if len(f.QuarterlyEPS) >= 4 && f.QuarterlyEPS[3] > 0 {
		epsYoY = roundDecimal(f.QuarterlyEPS[0]/f.QuarterlyEPS[3]-1, 4)
	}
	return map[string]any{
		"revenue_yoy": revenueYoY,
		"revenue_qoq": revenueQoQ,
		"eps_yoy":     epsYoY,
	}
}

// historicalValuationPercentiles exposes the transparent PE proxy that can be
// computed from the bounded provider inputs. Go may not have a complete annual
// balance-sheet history, so unavailable PB/EV-EBITDA percentiles remain absent
// instead of being synthesized from current values.
func historicalValuationPercentiles(f Fundamentals) map[string]any {
	if f.CurrentPrice == nil || *f.CurrentPrice <= 0 || len(f.HistoricalPrices) == 0 {
		return nil
	}
	prices := f.HistoricalPrices
	if len(prices) > 5 {
		prices = prices[:5]
	}
	eps := f.AnnualEPS
	if len(eps) > 5 {
		eps = eps[:5]
	}
	peHistory := make([]float64, 0, minInt(len(prices), len(eps)))
	for i := 0; i < len(prices) && i < len(eps); i++ {
		if isPositiveFinite(prices[i]) && isPositiveFinite(eps[i]) {
			peHistory = append(peHistory, prices[i]/eps[i])
		}
	}
	result := make(map[string]any)
	if ttm := positivePtr(f.TrailingEPS); ttm != nil && len(peHistory) > 0 {
		current := *f.CurrentPrice / *ttm
		result["pe_current"] = roundCurrency(current)
		result["pe_percentile"] = percentileRank(peHistory, current)
		result["pe_observations"] = len(peHistory)
	}
	// Historical PB uses the same annual share and equity snapshots as the
	// Python implementation. Missing snapshots simply leave this metric out.
	bookHistory := make([]float64, 0, len(prices))
	for i, price := range prices {
		if i >= len(f.AnnualShares) || i >= len(f.AnnualEquity) || !isPositiveFinite(f.AnnualShares[i]) || !isPositiveFinite(f.AnnualEquity[i]) {
			continue
		}
		bookHistory = append(bookHistory, price*f.AnnualShares[i]/f.AnnualEquity[i])
	}
	if f.BookValue != nil && *f.BookValue > 0 && len(bookHistory) > 0 {
		currentPB := *f.CurrentPrice / *f.BookValue
		result["pb_current"] = roundCurrency(currentPB)
		result["pb_percentile"] = percentileRank(bookHistory, currentPB)
		result["pb_observations"] = len(bookHistory)
	}
	// Historical EV/EBITDA uses operating income plus D&A as the EBITDA proxy,
	// matching Python when a direct EBITDA time series is unavailable.
	currentShares := positivePointer(f.SharesOutstanding)
	if currentShares == nil && len(f.AnnualShares) > 0 {
		currentShares = positivePointer(&f.AnnualShares[0])
	}
	currentOperatingIncome := f.OperatingIncome
	if len(f.AnnualOperatingIncome) > 0 {
		currentOperatingIncome = &f.AnnualOperatingIncome[0]
	}
	if currentShares != nil && currentOperatingIncome != nil {
		currentDA := 0.0
		if len(f.AnnualDepreciationAndAmortization) > 0 {
			currentDA = math.Abs(f.AnnualDepreciationAndAmortization[0])
		} else if f.DepreciationAndAmortization != nil {
			currentDA = math.Abs(*f.DepreciationAndAmortization)
		}
		currentEBITDA := *currentOperatingIncome + currentDA
		debt, cash := valueOr(f.Debt, 0), valueOr(f.Cash, 0)
		if f.Debt == nil && len(f.AnnualDebt) > 0 {
			debt = f.AnnualDebt[0]
		}
		if f.Cash == nil && len(f.AnnualCash) > 0 {
			cash = f.AnnualCash[0]
		}
		enterprise := (*f.CurrentPrice)*(*currentShares) + debt - cash
		history := make([]float64, 0, len(prices))
		for i, price := range prices {
			if i >= len(f.AnnualOperatingIncome) || f.AnnualOperatingIncome[i] <= 0 {
				continue
			}
			shares := *currentShares
			if i < len(f.AnnualShares) && isPositiveFinite(f.AnnualShares[i]) {
				shares = f.AnnualShares[i]
			}
			debtValue, cashValue := debt, cash
			if i < len(f.AnnualDebt) && isFinite(f.AnnualDebt[i]) {
				debtValue = f.AnnualDebt[i]
			}
			if i < len(f.AnnualCash) && isFinite(f.AnnualCash[i]) {
				cashValue = f.AnnualCash[i]
			}
			da := 0.0
			if i < len(f.AnnualDepreciationAndAmortization) {
				da = math.Abs(f.AnnualDepreciationAndAmortization[i])
			}
			ebitda := f.AnnualOperatingIncome[i] + da
			ev := price*shares + debtValue - cashValue
			if ebitda > 0 && ev > 0 {
				history = append(history, ev/ebitda)
			}
		}
		if currentEBITDA > 0 && enterprise > 0 && len(history) > 0 {
			currentEV := enterprise / currentEBITDA
			result["ev_ebitda_current"] = roundCurrency(currentEV)
			result["ev_ebitda_percentile"] = percentileRank(history, currentEV)
			result["ev_ebitda_observations"] = len(history)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func percentileRank(values []float64, current float64) float64 {
	valid := make([]float64, 0, len(values))
	for _, value := range values {
		if isPositiveFinite(value) {
			valid = append(valid, value)
		}
	}
	if len(valid) == 0 || !isPositiveFinite(current) {
		return 0
	}
	count := 0
	for _, value := range valid {
		if value <= current {
			count++
		}
	}
	return roundDecimal(float64(count)/float64(len(valid))*100, 1)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func genericAIExposed(f Fundamentals) bool {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(f.IndustryKey), "_", "-"))
	keyText := strings.ReplaceAll(key, "-", " ")
	text := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+keyText+" "+f.LongBusinessSummary, "_", " "))
	industryContext := strings.ToLower(strings.ReplaceAll(f.Sector+" "+f.Industry+" "+keyText, "_", " "))
	financialOrRealEstate := f.IsREIT || industryKeyCategory(key) == "reit" || containsAny(industryContext,
		"financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
	storageHardware := containsAny(text,
		"hard disk", "disk drive", "hdd", "nand", "flash memory", "solid state drive", "ssd",
		"memory chip", "storage chip", "storage device", "storage hardware", "storage products",
		"storage systems", "enterprise storage", "computer storage",
	) || (containsAny(text, "computer hardware") && containsAny(text, "storage", "disk"))
	return !financialOrRealEstate && (hasDefaultAIRevenueExposure(f, text) || isMobilityTechnology(f, text, industryContext) || storageHardware || containsAny(text,
		"semiconductor", "memory", "dram", "nand", "ai chip", "graphics processor",
	))
}

func hasDefaultAIRevenueExposure(f Fundamentals, text string) bool {
	if valueOr(f.AIRevenueShare, 0) > 0 {
		return true
	}
	defenseTag := containsAny(text, "defense", "aerospace", "laser", "military", "directed energy")
	return !defenseTag && containsAny(text, "storage")
}

func positivePtr(value *float64) *float64 {
	if value == nil || !isPositiveFinite(*value) {
		return nil
	}
	copy := *value
	return &copy
}

func winterFloorEPS(f Fundamentals, fallback float64) float64 {
	annual := f.AnnualEPS
	if len(annual) > 7 {
		annual = annual[:7]
	}
	values := make([]float64, 0, len(annual))
	for _, value := range annual {
		if isPositiveFinite(value) {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return fallback
	}
	sort.Float64s(values)
	if len(values) >= 5 {
		values = values[:len(values)-2]
	}
	limit := 3
	if len(values) < limit {
		limit = len(values)
	}
	total := 0.0
	for _, value := range values[:limit] {
		total += value
	}
	return total / float64(limit)
}

// calculateGeneric selects the strongest generic model. It never panics on
// malformed optional inputs; Calculate performs the public validation step.
func calculateGeneric(f Fundamentals) Result {
	return calculateGenericWithForwardEstimate(f, true)
}

func calculateGenericWithoutModeledForwardEPS(f Fundamentals) Result {
	return calculateGenericWithForwardEstimate(f, false)
}

func calculateGenericWithForwardEstimate(f Fundamentals, allowModeledForwardEPS bool) Result {
	normalizedEPS := normalizedEPSValue(f)
	requiredReturn := valueOr(f.RequiredReturn, defaultRequiredReturn)
	terminalGrowth := defaultTerminalGrowth
	if f.TerminalGrowth != nil {
		terminalGrowth = *f.TerminalGrowth
	} else if f.GrowthRate != nil {
		terminalGrowth = clamp(*f.GrowthRate*0.25, 0.015, 0.035)
	}
	safetyMargin := valueOr(f.SafetyMargin, defaultSafetyMargin)
	providerBacked := providerBackedFundamentals(f)
	if f.SafetyMargin == nil && providerBacked {
		// Provider-backed generic valuations share Python's lifecycle margin rules.
		safetyMargin = informationFallbackSafetyMargin(f)
	}
	peLow := valueOr(f.ForwardPELow, defaultPELow)
	peHigh := valueOr(f.ForwardPEHigh, defaultPEHigh)
	aiExposed := genericAIExposed(f)
	if aiExposed && f.Regime == RegimeWinter {
		floor := winterFloorEPS(f, valueOr(normalizedEPS, 0))
		if floor > 0 {
			f.ForwardEPS = &floor
		}
		peLow, peHigh = 8.0, 12.0
	}
	warnings := make([]string, 0, 4)
	if f.RequiredReturn == nil {
		warnings = append(warnings, fmt.Sprintf("required return defaulted to %.1f%%", defaultRequiredReturn*100))
	}
	if f.TerminalGrowth == nil {
		if f.GrowthRate == nil {
			warnings = append(warnings, fmt.Sprintf("terminal growth defaulted to %.1f%%", defaultTerminalGrowth*100))
		} else {
			warnings = append(warnings, fmt.Sprintf("terminal growth derived from growth rate and bounded to 1.5%%-3.5%% (%.2f%%)", terminalGrowth*100))
		}
	}
	if f.SafetyMargin == nil && !providerBacked {
		warnings = append(warnings, fmt.Sprintf("safety margin defaulted to %.0f%%", defaultSafetyMargin*100))
	}
	if f.ForwardPELow == nil || f.ForwardPEHigh == nil {
		warnings = append(warnings, fmt.Sprintf("forward PE range defaulted to %.0fx-%.0fx", defaultPELow, defaultPEHigh))
	}

	result := Result{
		Methods: make(map[string]float64), SafetyMargin: safetyMargin,
		OwnerEarningsMaintenanceRatio: copyPointer(f.MaintenanceCapexRatio),
		OwnerEarningsRatioSource:      f.MaintenanceCapexRatioSource,
		Status:                        StatusUnavailable, Warnings: warnings, ForwardEPS: copyPointer(f.ForwardEPS),
		DataQualityScore:            dataQualityScore(f),
		NormalizedEPSSource:         normalizedEPSSource(f),
		MarketCapDataQuality:        marketCapDataQuality(f),
		ShareholderTotalReturnYield: shareholderBuybackYield(f),
		InterestCoverage:            interestCoverage(f),
	}
	// Python filters positive raw candidates before rounding the displayed
	// method values. Keep the provider path's selection inputs at full precision
	// so sub-cent values do not become unusable zero candidates prematurely.
	rawMethods := make(map[string]float64, 3)
	if percentiles := historicalValuationPercentiles(f); percentiles != nil {
		result.HistoricalValuationPercentiles = percentiles
	}
	if f.Regime != "" {
		result.Regime = f.Regime
		result.RegimeSignals = f.RegimeSignals
		result.ModelUnderRegime = "generic (" + f.Regime + ")"
	}
	if momentum := quarterlyMomentum(f); momentum != nil {
		result.QuarterlyMomentum = momentum
	}

	// Owner earnings is only a candidate when the result is positive and can be
	// valued with a meaningful discount/spread. Negative owner earnings is not
	// silently turned into a negative fair value; the chain falls through.
	ownerEarnings, ownerAvailable := resolveOwnerEarnings(f)
	if ownerAvailable && ownerEarnings > 0 && f.SharesOutstanding != nil && *f.SharesOutstanding > 0 && requiredReturn > terminalGrowth {
		perShare := ownerEarnings / *f.SharesOutstanding
		candidate := perShare / (requiredReturn - terminalGrowth)
		if isPositiveFinite(candidate) {
			rawMethods[MethodOwnerEarnings] = candidate
			result.Methods[MethodOwnerEarnings] = roundCurrency(candidate)
			result.OwnerEarningsPerShare = floatPointer(roundCurrency(perShare))
		}
	}
	forwardEPS := positivePtr(f.ForwardEPS)
	if allowModeledForwardEPS && forwardEPS == nil && normalizedEPS != nil {
		growth := clamp(valueOr(f.GrowthRate, 0), -.15, .50)
		modeled := *normalizedEPS * (1 + growth)
		if isPositiveFinite(modeled) {
			forwardEPS = floatPointer(modeled)
		}
	}
	if aiExposed && f.Regime == RegimeSlowdown && forwardEPS != nil {
		adjusted := *forwardEPS * .70
		forwardEPS = &adjusted
	}
	forwardEPSWarning := ""
	if forwardEPS != nil && *forwardEPS > 0 && normalizedEPS != nil {
		cap := *normalizedEPS * 3 // Generic-path baseline; special growth-sector exceptions need full classification.
		if *forwardEPS > cap {
			*forwardEPS = cap
			forwardEPSWarning = fmt.Sprintf("forward EPS capped at 3x trailing EPS (%.4f)", cap)
			result.Warnings = append(result.Warnings, forwardEPSWarning)
		}
	}
	result.ForwardEPS = copyPointer(forwardEPS)
	if forwardEPS != nil && *forwardEPS > 0 {
		candidate := *forwardEPS * (peLow + peHigh) / 2
		if isPositiveFinite(candidate) {
			rawMethods[MethodForwardPE] = candidate
			result.Methods[MethodForwardPE] = roundCurrency(candidate)
		}
	}
	if fcfPerShare, ok := resolveFCFPerShare(f); ok && requiredReturn > terminalGrowth {
		growth := valueOr(f.GrowthRate, 0)
		// Keep a fallback DCF bounded and deterministic when a provider sends a
		// short-term growth spike. The owner-earnings and PE paths are unaffected.
		growth = valuationDCFRate(f, growth)
		candidate := dcfPerShare(fcfPerShare, growth, requiredReturn, terminalGrowth)
		if isPositiveFinite(candidate) {
			rawMethods[MethodFCFDCF] = candidate
			result.Methods[MethodFCFDCF] = roundCurrency(candidate)
		}
	}

	method, value := selectMethod(result.Methods)
	if providerBacked {
		method, value = selectMethod(rawMethods)
	}
	mainSpread := valueRangeSpread
	if method == "" {
		result.Warning = unavailableWarning(f, requiredReturn, terminalGrowth)
		result.Warnings = append(result.Warnings, result.Warning)
		if forwardEPSWarning != "" {
			result.Warning += "; " + forwardEPSWarning
		}
		result.ConfidenceLabel = "low"
		return result
	}
	// The Python provider selects the median of usable generic valuation
	// candidates after rejecting unit outliers. Keep the legacy method priority
	// for provider-independent callers, but apply the v39 candidate selection
	// when normalized provider metadata is present. This matters when owner
	// earnings are valid mathematically but inconsistent with the EPS/price
	// anchors while forward PE and FCF DCF remain plausible.
	if providerBacked {
		values := make([]float64, 0, len(rawMethods))
		for _, candidate := range rawMethods {
			if isPositiveFinite(candidate) {
				values = append(values, candidate)
			}
		}
		priceAnchor := 0.0
		if f.CurrentPrice != nil && *f.CurrentPrice > 0 {
			priceAnchor = *f.CurrentPrice
		} else if f.MarketCap != nil && f.SharesOutstanding != nil && *f.MarketCap > 0 && *f.SharesOutstanding > 0 {
			priceAnchor = *f.MarketCap / *f.SharesOutstanding
		}
		epsAnchor := valueOr(positivePtr(forwardEPS), valueOr(normalizedEPS, 0))
		values = filterUnitAnomalyCandidates(values, epsAnchor, priceAnchor, false)
		if len(values) > 0 {
			sort.Float64s(values)
			value = values[len(values)/2]
			if len(values) > 1 {
				mainSpread = .18
			} else if containsAny(strings.ToLower(f.Sector+" "+f.Industry+" "+strings.ReplaceAll(f.IndustryKey, "-", " ")),
				"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores") {
				mainSpread = .15
			} else {
				mainSpread = .22
			}
		}
	}
	// A malformed unit (for example EPS reported in thousands) can produce a
	// mathematically valid value several orders above the traded price. Python's
	// provider applies the same bounded price anchor after collecting candidates;
	// keep the generic Go fallback from publishing an unusable quote.
	priceAnchor := 0.0
	if f.CurrentPrice != nil && *f.CurrentPrice > 0 {
		priceAnchor = *f.CurrentPrice
	} else if f.MarketCap != nil && f.SharesOutstanding != nil && *f.MarketCap > 0 && *f.SharesOutstanding > 0 {
		priceAnchor = *f.MarketCap / *f.SharesOutstanding
	}
	anchorWarning := ""
	// Legacy provider-independent callers intentionally allow a wide PE result;
	// apply the production unit guard only when structured provider metadata is
	// present.
	if providerBacked && priceAnchor > 0 && value > priceAnchor*5 {
		value = roundCurrency(priceAnchor * 1.5)
		anchorWarning = "model value exceeded five times the price anchor; fell back to a bounded price anchor"
		result.Warnings = append(result.Warnings, anchorWarning)
	}
	result.Method = method
	result.FairValue = floatPointer(roundCurrency(value))
	low := roundCurrency(value * (1 - mainSpread))
	high := roundCurrency(value * (1 + mainSpread))
	buyLow := roundCurrency(low * (1 - safetyMargin))
	buyHigh := roundCurrency(high * (1 - safetyMargin))
	result.FairValueLow = floatPointer(low)
	result.FairValueHigh = floatPointer(high)
	result.FairValueBuyLow = floatPointer(buyLow)
	result.FairValueBuyHigh = floatPointer(buyHigh)
	result.Status = StatusReady
	result.Confidence = confidenceFor(method)
	result.ConfidenceLabel = confidenceLabel(result.Confidence)
	result.Defensive = scenario(value*0.80, 0.25, method+" stress test", math.Min(0.95, safetyMargin+0.10))
	result.Optimistic = scenario(value*1.20, 0.25, method+" optimistic", math.Max(0.10, safetyMargin-0.10))
	if providerBacked {
		stable := containsAny(strings.ToLower(f.Sector+" "+f.Industry+" "+strings.ReplaceAll(f.IndustryKey, "-", " ")),
			"consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores")
		shares := positivePtr(f.SharesOutstanding)
		freeCashFlowMargin := 0.0
		if f.FreeCashFlow != nil && f.Revenue != nil && *f.Revenue > 0 {
			freeCashFlowMargin = *f.FreeCashFlow / *f.Revenue
		}
		negativeFCF := f.FreeCashFlow != nil && *f.FreeCashFlow < 0
		defensiveMargin := clamp(safetyMargin, .20, .70)
		if negativeFCF {
			defensiveMargin = clamp(safetyMargin+.10, .20, .70)
		}
		defensiveValues := make([]float64, 0, 4)
		if stable && normalizedEPS != nil {
			pe := 20.0
			if freeCashFlowMargin >= .15 {
				pe = 24
			}
			defensiveValues = append(defensiveValues, *normalizedEPS*pe)
		}
		requiredReturn := valueOr(f.RequiredReturn, defaultRequiredReturn)
		terminalGrowth := valueOr(f.TerminalGrowth, clamp(valueOr(f.GrowthRate, 0)*.25, .015, .035))
		ownerEarnings, ownerAvailable := resolveOwnerEarnings(f)
		if ownerAvailable && shares != nil && requiredReturn > terminalGrowth {
			ownerCandidate := (ownerEarnings / *shares) / math.Max(requiredReturn-terminalGrowth, .045)
			if isPositiveFinite(ownerCandidate) {
				defensiveValues = append(defensiveValues, ownerCandidate)
			}
		}
		if fcfPerShare, ok := resolveFCFPerShare(f); ok && requiredReturn > terminalGrowth {
			dcfCandidate := dcfPerShare(fcfPerShare, valuationDCFRate(f, valueOr(f.GrowthRate, 0)), requiredReturn, terminalGrowth)
			if isPositiveFinite(dcfCandidate) {
				defensiveValues = append(defensiveValues, dcfCandidate)
			}
		}
		cyclical := containsAny(strings.ToLower(f.Sector+" "+f.Industry+" "+strings.ReplaceAll(f.IndustryKey, "-", " ")),
			"semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline")
		if cycleEPS := cyclicalEPS(f, normalizedEPS); cycleEPS != nil {
			peLow, peHigh := 12.0, 20.0
			if stable {
				peLow, peHigh = 15, 21
			} else if cyclical {
				peLow, peHigh = 10, 16
			}
			defensiveValues = append(defensiveValues, *cycleEPS*(peLow+peHigh)/2)
		}
		defensiveMid := value
		if len(defensiveValues) > 0 {
			sort.Float64s(defensiveValues)
			defensiveMid = defensiveValues[len(defensiveValues)/2]
		}
		defensiveSpread := .22
		if stable {
			defensiveSpread = .18
		}
		if stable && normalizedEPS != nil {
			defensiveMid = *normalizedEPS * 24
		}
		result.Defensive = scenario(defensiveMid, defensiveSpread, "保守 DCF / 所有者收益 压力测试", defensiveMargin)
		if forward := positivePtr(forwardEPS); forward != nil && !cyclical {
			optimisticGrowth := clamp(valueOr(f.GrowthRate, 0), .06, .40)
			optimisticPE := clamp(optimisticGrowth*150, 22, 50)
			optimisticMid := *forward * optimisticPE
			optimisticSpread := .15
			optimisticMargin := .30
			if optimisticGrowth >= .18 {
				optimisticSpread = .18
				optimisticMargin = .48
			}
			optimistic := scenario(optimisticMid, optimisticSpread, "远期 EPS × PEG", optimisticMargin)
			optimistic["forward_eps"] = roundDecimal(*forward, 4)
			optimistic["growth_rate"] = roundDecimal(optimisticGrowth, 4)
			result.Optimistic = optimistic
		} else {
			result.Optimistic = nil
		}
	}
	if hasLowInterestCoverage(f) {
		result.Warnings = append(result.Warnings, "利息覆盖率低于 2 倍，乐观估值已收紧")
		if high, ok := result.Optimistic["high"].(float64); ok {
			high *= .85
			result.Optimistic["high"] = roundCurrency(high)
			if optimisticValue, ok := result.Optimistic["value"].(float64); ok && optimisticValue > high {
				result.Optimistic["value"] = roundCurrency(high)
			}
			if buyHigh, ok := result.Optimistic["buy_high"].(float64); ok {
				result.Optimistic["buy_high"] = roundCurrency(buyHigh * .85)
			}
		}
	}

	switch method {
	case MethodForwardPE:
		result.Warning = "owner earnings unavailable; used forward EPS multiplied by PE"
		result.Warnings = append(result.Warnings, result.Warning)
	case MethodFCFDCF:
		result.Warning = "owner earnings and forward EPS unavailable; used FCF DCF"
		result.Warnings = append(result.Warnings, result.Warning)
	}
	if forwardEPSWarning != "" {
		result.Warning = strings.TrimSpace(result.Warning + "; " + forwardEPSWarning)
	}
	if anchorWarning != "" {
		result.Warning = strings.TrimSpace(result.Warning + "; " + anchorWarning)
	}
	return result
}

func invalidResult(err error) Result {
	warning := err.Error()
	problems := []string{warning}
	if validationErr, ok := err.(*ValidationError); ok {
		problems = append([]string(nil), validationErr.Problems...)
	}
	return Result{
		Status:          StatusInvalid,
		ConfidenceLabel: "low",
		Warning:         warning,
		Warnings:        []string{warning},
		Errors:          problems,
		Methods:         map[string]float64{},
	}
}

func resolveOwnerEarnings(f Fundamentals) (float64, bool) {
	if f.OwnerEarnings != nil {
		return *f.OwnerEarnings, true
	}
	if f.NetIncome == nil {
		if f.FreeCashFlow != nil {
			return *f.FreeCashFlow, true
		}
		return 0, false
	}
	maintenanceCapex, ok := resolveMaintenanceCapex(f)
	if !ok {
		return 0, false
	}
	depreciation, workingCapital := 0.0, 0.0
	if f.DepreciationAndAmortization != nil {
		depreciation = math.Abs(*f.DepreciationAndAmortization)
	}
	if f.ChangeInWorkingCapital != nil {
		workingCapital = math.Max(*f.ChangeInWorkingCapital, 0)
	}
	return *f.NetIncome + depreciation - maintenanceCapex - workingCapital, true
}

func resolveMaintenanceCapex(f Fundamentals) (float64, bool) {
	if f.MaintenanceCapex != nil {
		return math.Abs(*f.MaintenanceCapex), true
	}
	if f.CapitalExpenditure == nil {
		return 0, true
	}
	ratio := valueOr(f.MaintenanceCapexRatio, defaultMaintenanceCapexRatio)
	return math.Abs(*f.CapitalExpenditure) * ratio, true
}

func scenario(value, spread float64, model string, margin float64) map[string]any {
	low := roundCurrency(value * (1 - spread))
	high := roundCurrency(value * (1 + spread))
	return map[string]any{
		"value": roundCurrency(value), "low": low, "high": high,
		"buy_low": roundCurrency(low * (1 - margin)), "buy_high": roundCurrency(high * (1 - margin)),
		"model": model, "safety_margin": roundCurrency(margin),
	}
}

// This score is intentionally independent from method confidence. It mirrors
// the Python input-quality deductions for market-cap uncertainty and revenue
// history, while accounting for unavailable analyst coverage in this provider.
func dataQualityScore(f Fundamentals) float64 {
	score := 100.0
	marketCap := f.MarketCap
	if marketCap == nil && f.CurrentPrice != nil && f.SharesOutstanding != nil && *f.CurrentPrice > 0 && *f.SharesOutstanding > 0 {
		derived := *f.CurrentPrice * *f.SharesOutstanding
		marketCap = &derived
	}
	if marketCap != nil && *marketCap != 0 && *marketCap < 5e9 {
		score -= 10
	}
	quality := marketCapDataQuality(f)
	if quality == "reconciled" || quality == "unit_mismatch" || quality == "market_cap_preferred" {
		score -= 15
	}
	if len(f.AnnualRevenue) == 0 {
		// Python uses positive info.totalRevenue as a single-value fallback when
		// Yahoo omits its annual revenue timeseries.
		if f.Revenue == nil || *f.Revenue <= 0 || !isFinite(*f.Revenue) {
			score -= 15
		}
	} else {
		for _, revenue := range f.AnnualRevenue {
			if revenue <= 0 || !isFinite(revenue) {
				score -= 15
				break
			}
		}
	}
	if f.AnalystCount != nil && *f.AnalystCount < 3 {
		score -= 20
	} else if f.AnalystCount == nil && f.TargetMeanPrice == nil {
		score -= 10
	}
	return roundCurrency(clamp(score, 0, 100))
}

// effectiveMarketCap follows Python classification: infer market cap only when
// the provider omitted it and both price and shares are positive. A supplied
// zero remains supplied, matching Python's None-only fallback.
func effectiveMarketCap(f Fundamentals) (float64, bool) {
	if f.MarketCap != nil {
		return *f.MarketCap, true
	}
	if f.CurrentPrice == nil || f.SharesOutstanding == nil || *f.CurrentPrice <= 0 || *f.SharesOutstanding <= 0 {
		return 0, false
	}
	marketCap := *f.CurrentPrice * *f.SharesOutstanding
	if !isFinite(marketCap) || marketCap <= 0 {
		return 0, false
	}
	return marketCap, true
}

func effectiveMarketCapValue(f Fundamentals) float64 {
	marketCap, _ := effectiveMarketCap(f)
	return marketCap
}

func marketCapDataQuality(f Fundamentals) string {
	if strings.TrimSpace(f.MarketCapDataQuality) != "" {
		return f.MarketCapDataQuality
	}
	if f.MarketCap != nil && *f.MarketCap > 0 && f.CurrentPrice != nil && f.SharesOutstanding != nil && *f.CurrentPrice > 0 && *f.SharesOutstanding > 0 {
		implied := *f.CurrentPrice * *f.SharesOutstanding
		if math.Abs(*f.MarketCap-implied) / *f.MarketCap <= .20 {
			return "consistent"
		}
		if strings.EqualFold(strings.TrimSpace(f.SharesSource), "balance_sheet") {
			return "reconciled"
		}
		return "unknown"
	}
	if f.CurrentPrice != nil && f.SharesOutstanding != nil && *f.CurrentPrice > 0 && *f.SharesOutstanding > 0 {
		return "derived"
	}
	return "unknown"
}

// marketCapWarning reconstructs the Python diagnostic for callers that build
// Fundamentals directly. Provider-backed requests normally populate
// Fundamentals.MarketCapWarning before reconciliation, but this fallback keeps
// the provider-independent valuation package equally observable in tests and
// legacy call sites.
func marketCapWarning(f Fundamentals) string {
	if strings.TrimSpace(f.MarketCapWarning) != "" {
		return strings.TrimSpace(f.MarketCapWarning)
	}
	if f.MarketCap == nil || !isPositiveFinite(*f.MarketCap) || f.CurrentPrice == nil ||
		!isPositiveFinite(*f.CurrentPrice) || f.SharesOutstanding == nil || !isPositiveFinite(*f.SharesOutstanding) {
		return ""
	}
	implied := *f.CurrentPrice * *f.SharesOutstanding
	if !isPositiveFinite(implied) {
		return ""
	}
	mismatch := math.Abs(*f.MarketCap-implied) / *f.MarketCap
	if mismatch <= .20 {
		return ""
	}
	scale := *f.MarketCap / implied
	if scale > 100 || scale < .01 {
		return "market_cap 与股本×现价存在数量级差异，已保留市值并降低数据质量评分"
	}
	if trustedMarketCapSharesSource(f.SharesSource) {
		return fmt.Sprintf("market_cap 与股本×现价偏差超过 20%%，已使用%s×现价校正市值", f.SharesSource)
	}
	source := strings.TrimSpace(f.SharesSource)
	if source == "" {
		source = "unknown"
	}
	return fmt.Sprintf("股本来源 %s 未通过当前口径核验，已保留 Yahoo market_cap", source)
}

func trustedMarketCapSharesSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "balance_sheet", "info", "fast_info":
		return true
	default:
		return false
	}
}

func normalizedEPSSource(f Fundamentals) string {
	if source := strings.TrimSpace(f.NormalizedEPSSource); source != "" {
		return source
	}
	values := make([]float64, 0, 6)
	if f.TrailingEPS != nil && isPositiveFinite(*f.TrailingEPS) {
		values = append(values, *f.TrailingEPS)
	}
	for _, value := range f.AnnualEPS {
		if isPositiveFinite(value) {
			values = append(values, value)
			if len(values) == 6 {
				break
			}
		}
	}
	if len(values) == 0 {
		return ""
	}
	return "历史 EPS 中位数"
}

func shareholderBuybackYield(f Fundamentals) *float64 {
	if f.CurrentPrice == nil || f.SharesOutstanding == nil || *f.CurrentPrice <= 0 || *f.SharesOutstanding <= 0 {
		return nil
	}
	dividendPerShare := 0.0
	if f.DividendRate != nil && *f.DividendRate > 0 {
		dividendPerShare = *f.DividendRate
	}
	buybackPerShare := 0.0
	if f.Buyback != nil && isFinite(*f.Buyback) {
		buybackPerShare = math.Abs(*f.Buyback) / *f.SharesOutstanding
	}
	yield := (dividendPerShare + buybackPerShare) / *f.CurrentPrice
	if !isFinite(yield) || yield < 0 || yield > 1 {
		return nil
	}
	return floatPointer(roundCurrency(yield*10000) / 10000)
}

func resolveFCFPerShare(f Fundamentals) (float64, bool) {
	if f.FreeCashFlowPerShare != nil {
		if *f.FreeCashFlowPerShare <= 0 {
			return 0, false
		}
		return *f.FreeCashFlowPerShare, true
	}
	if f.FreeCashFlow == nil || f.SharesOutstanding == nil || *f.SharesOutstanding <= 0 || *f.FreeCashFlow <= 0 {
		return 0, false
	}
	return *f.FreeCashFlow / *f.SharesOutstanding, true
}

func valuationDCFRate(f Fundamentals, growth float64) float64 {
	maxGrowth := .14
	if isHighGrowthInfrastructure(f) {
		maxGrowth = .30
		if f.RevenueGrowth != nil {
			growth = *f.RevenueGrowth
		}
	}
	return clamp(growth, -.02, maxGrowth)
}

func dcfPerShare(base, growth, requiredReturn, terminalGrowth float64) float64 {
	if !isPositiveFinite(base) || requiredReturn <= terminalGrowth || requiredReturn <= 0 {
		return 0
	}
	presentValue := 0.0
	for year := 1; year <= 5; year++ {
		presentValue += base * math.Pow(1+growth, float64(year)) / math.Pow(1+requiredReturn, float64(year))
	}
	terminal := base * math.Pow(1+growth, 5) * (1 + terminalGrowth) / (requiredReturn - terminalGrowth)
	terminal /= math.Pow(1+requiredReturn, 5)
	return presentValue + terminal
}

func selectMethod(methods map[string]float64) (string, float64) {
	for _, method := range []string{MethodOwnerEarnings, MethodForwardPE, MethodFCFDCF} {
		if value, ok := methods[method]; ok && isPositiveFinite(value) {
			return method, value
		}
	}
	return "", 0
}

func filterUnitAnomalyCandidates(candidates []float64, epsAnchor, priceAnchor float64, skipEPSFilter bool) []float64 {
	if len(candidates) <= 1 {
		return candidates
	}
	if !skipEPSFilter && isPositiveFinite(epsAnchor) {
		plausible := make([]float64, 0, len(candidates))
		for _, value := range candidates {
			if value >= epsAnchor*valuationEPSUnitFloor && value <= epsAnchor*valuationEPSUnitCeiling {
				plausible = append(plausible, value)
			}
		}
		if len(plausible) > 0 {
			candidates = plausible
		}
	}
	if len(candidates) > 1 && isPositiveFinite(priceAnchor) {
		plausible := make([]float64, 0, len(candidates))
		for _, value := range candidates {
			if value >= priceAnchor*valuationPriceFloorRatio && value <= priceAnchor*valuationPriceCeilingRatio {
				plausible = append(plausible, value)
			}
		}
		if len(plausible) > 0 {
			candidates = plausible
		}
	}
	return candidates
}

func unavailableWarning(f Fundamentals, requiredReturn, terminalGrowth float64) string {
	if requiredReturn <= terminalGrowth {
		return "no usable valuation candidate: required return must exceed terminal growth"
	}
	if f.SharesOutstanding == nil && f.FreeCashFlow != nil {
		return "no usable valuation candidate: shares outstanding is required for total-company cash flow"
	}
	return "no usable valuation candidate: insufficient positive owner earnings, forward EPS, or free cash flow"
}

func interestCoverage(f Fundamentals) *float64 {
	coverage, ok := rawInterestCoverage(f)
	if !ok {
		return nil
	}
	return floatPointer(roundDecimal(coverage, 2))
}

func hasLowInterestCoverage(f Fundamentals) bool {
	coverage, ok := rawInterestCoverage(f)
	return ok && coverage < 2
}

func rawInterestCoverage(f Fundamentals) (float64, bool) {
	if f.OperatingIncome == nil || f.InterestExpense == nil || !isFinite(*f.OperatingIncome) || !isFinite(*f.InterestExpense) {
		return 0, false
	}
	interest := math.Abs(*f.InterestExpense)
	if interest <= 0 {
		return 0, false
	}
	if interest <= 1 && f.Revenue != nil && *f.Revenue > 0 {
		interest *= *f.Revenue
	}
	coverage := *f.OperatingIncome / interest
	if !isFinite(coverage) {
		return 0, false
	}
	return coverage, true
}

func confidenceFor(method string) float64 {
	switch method {
	case MethodOwnerEarnings:
		return 0.85
	case MethodForwardPE:
		return 0.65
	case MethodFCFDCF:
		return 0.60
	default:
		return 0
	}
}

func confidenceLabel(score float64) string {
	if score >= 0.75 {
		return "high"
	}
	if score >= 0.50 {
		return "medium"
	}
	return "low"
}

func valueOr(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func copyPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func floatPointer(value float64) *float64 { return &value }

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func isPositiveFinite(value float64) bool { return value > 0 && isFinite(value) }

func clamp(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func roundCurrency(value float64) float64 {
	return roundPython(value, 2)
}

func roundDecimal(value float64, places int) float64 {
	return roundPython(value, places)
}

func roundPython(value float64, places int) float64 {
	if !isFinite(value) {
		return value
	}
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', places, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}
