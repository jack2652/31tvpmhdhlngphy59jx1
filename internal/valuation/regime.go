package valuation

import "strconv"

// RegimeInput contains the observable macro inputs used by the Python
// valuation path. Missing fields are represented by nil pointers so an
// unavailable benchmark does not turn into a false signal.
type RegimeInput struct {
	RevenueGrowth   *float64
	EPSRevision     *float64
	CapexGrowth     *float64
	InventoryDays   *float64
	TNXYield        *float64
	VIX             *float64
	BenchmarkCloses []float64
}

const (
	RegimeBoom     = "BOOM"
	RegimeNormal   = "NORMAL"
	RegimeSlowdown = "SLOWDOWN"
	RegimeWinter   = "WINTER"
)

// DetectRegime mirrors the Python two-signal policy, including the single
// extreme-signal override used during volatility shocks. The returned signals
// are JSON-safe and retain nil values for fields that were unavailable.
func DetectRegime(input RegimeInput) (string, map[string]any) {
	round := func(value *float64, places int) any {
		if value == nil || !isFinite(*value) {
			return nil
		}
		rounded, err := strconv.ParseFloat(strconv.FormatFloat(*value, 'f', places, 64), 64)
		if err != nil {
			return nil
		}
		return rounded
	}
	momentum := benchmarkMomentum(input.BenchmarkCloses)
	signals := map[string]any{
		"tnx_yield":              round(input.TNXYield, 4),
		"vix":                    round(input.VIX, 2),
		"revenue_growth":         round(input.RevenueGrowth, 4),
		"eps_revision_1m":        round(input.EPSRevision, 4),
		"capex_growth":           round(input.CapexGrowth, 4),
		"inventory_days":         round(input.InventoryDays, 2),
		"benchmark_momentum_60d": nil,
	}
	if momentum != nil {
		signals["benchmark_momentum_60d"] = round(momentum, 4)
	}

	// Keep these values in one place so the detector is deterministic when the
	// provider omits a signal. They match valuation_config.py.
	const (
		epsRevisionSlowdown   = -0.10
		epsRevisionWinter     = -0.25
		tnxYieldSlowdown      = 0.045
		tnxYieldWinter        = 0.055
		vixSlowdown           = 25.0
		vixWinter             = 35.0
		revenueGrowthSlowdown = 0.05
		revenueGrowthWinter   = -0.10
		capexGrowthWinter     = -0.15
		inventoryDaysWinter   = 120.0
		momentumSlowdown      = -0.08
		momentumWinter        = -0.15
		vixExtreme            = 45.0
		momentumExtreme       = -0.20
		epsRevisionExtreme    = -0.35
	)
	value := func(pointer *float64) (float64, bool) {
		return pointerValue(pointer)
	}
	extremeHits := make([]string, 0, 3)
	if vix, ok := value(input.VIX); ok && vix > vixExtreme {
		extremeHits = append(extremeHits, "vix_extreme")
	}
	if momentum != nil && *momentum < momentumExtreme {
		extremeHits = append(extremeHits, "momentum_extreme")
	}
	if revision, ok := value(input.EPSRevision); ok && revision < epsRevisionExtreme {
		extremeHits = append(extremeHits, "eps_revision_extreme")
	}
	signals["extreme_hits"] = extremeHits
	if len(extremeHits) > 0 {
		return RegimeWinter, signals
	}

	winterHits := make([]string, 0, 7)
	if growth, ok := value(input.RevenueGrowth); ok && growth < revenueGrowthWinter {
		winterHits = append(winterHits, "revenue_growth")
	}
	if revision, ok := value(input.EPSRevision); ok && revision < epsRevisionWinter {
		winterHits = append(winterHits, "eps_revision")
	}
	if growth, ok := value(input.CapexGrowth); ok && growth < capexGrowthWinter {
		winterHits = append(winterHits, "capex_growth")
	}
	if days, ok := value(input.InventoryDays); ok && days > inventoryDaysWinter {
		winterHits = append(winterHits, "inventory_days")
	}
	if yield, ok := value(input.TNXYield); ok && yield > tnxYieldWinter {
		winterHits = append(winterHits, "tnx_yield")
	}
	if vix, ok := value(input.VIX); ok && vix > vixWinter {
		winterHits = append(winterHits, "vix")
	}
	if momentum != nil && *momentum < momentumWinter {
		winterHits = append(winterHits, "benchmark_momentum")
	}

	slowdownHits := make([]string, 0, 5)
	if growth, ok := value(input.RevenueGrowth); ok && growth < revenueGrowthSlowdown {
		slowdownHits = append(slowdownHits, "revenue_growth")
	}
	if revision, ok := value(input.EPSRevision); ok && revision < epsRevisionSlowdown {
		slowdownHits = append(slowdownHits, "eps_revision")
	}
	if yield, ok := value(input.TNXYield); ok && yield > tnxYieldSlowdown {
		slowdownHits = append(slowdownHits, "tnx_yield")
	}
	if vix, ok := value(input.VIX); ok && vix > vixSlowdown {
		slowdownHits = append(slowdownHits, "vix")
	}
	if momentum != nil && *momentum < momentumSlowdown {
		slowdownHits = append(slowdownHits, "benchmark_momentum")
	}
	signals["winter_hits"] = winterHits
	signals["slowdown_hits"] = slowdownHits
	if len(winterHits) >= 2 {
		return RegimeWinter, signals
	}
	if len(slowdownHits) >= 2 {
		return RegimeSlowdown, signals
	}
	if momentum != nil && *momentum > 0.10 {
		if growth, ok := value(input.RevenueGrowth); !ok || growth > 0.20 {
			return RegimeBoom, signals
		}
	}
	return RegimeNormal, signals
}

func benchmarkMomentum(closes []float64) *float64 {
	if len(closes) < 60 {
		return nil
	}
	valid := make([]float64, 0, len(closes))
	for _, close := range closes {
		if isPositiveFinite(close) {
			valid = append(valid, close)
		}
	}
	if len(valid) < 60 {
		return nil
	}
	recent, earlier := valid[len(valid)-20:], valid[len(valid)-60:len(valid)-20]
	average := func(values []float64) float64 {
		total := 0.0
		for _, value := range values {
			total += value
		}
		return total / float64(len(values))
	}
	base := average(earlier)
	if base <= 0 {
		return nil
	}
	result := average(recent)/base - 1
	if !isFinite(result) {
		return nil
	}
	return &result
}

func pointerValue(value *float64) (float64, bool) {
	if value == nil || !isFinite(*value) {
		return 0, false
	}
	return *value, true
}
