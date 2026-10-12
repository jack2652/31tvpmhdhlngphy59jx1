package gamma

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestExpiredValidExpirationRetainsPythonMinimumTime(t *testing.T) {
	now := time.Date(2026, time.October, 9, 20, 0, 0, 0, time.UTC)
	want := 30.0 / (365 * 24 * 60)
	if got := yearsToExpiry("2026-10-08", now); math.Abs(got-want) > 1e-12 {
		t.Fatalf("expired expiration years=%v, want Python minimum %v", got, want)
	}
	if got := yearsToExpiry("invalid-expiration", now); got != 0 {
		t.Fatalf("malformed expiration years=%v, want 0", got)
	}
}

func TestImpliedVolatilityRoundTrip(t *testing.T) {
	spot, strike, volatility, years := 100.0, 100.0, 0.42, 0.25
	price := blackScholesPrice(spot, strike, volatility, years, true)
	actual, ok := impliedVolatility(price, spot, strike, years, true)
	if !ok || math.Abs(actual-volatility) > 1e-6 {
		t.Fatalf("impliedVolatility() = %v, %v, want %.6f", actual, ok, volatility)
	}
}

func TestGammaRejectsNonFiniteInputs(t *testing.T) {
	for _, input := range [][4]float64{
		{math.NaN(), 100, 0.3, 0.25},
		{100, math.Inf(1), 0.3, 0.25},
		{100, 100, math.NaN(), 0.25},
		{100, 100, 0.3, math.Inf(1)},
	} {
		if got := Gamma(input[0], input[1], input[2], input[3]); got != 0 {
			t.Errorf("Gamma(%v)=%v, want 0 for non-finite input", input, got)
		}
	}
}

func TestEstimateOptionGammaMatchesPythonGoldenValues(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name       string
		spot       float64
		strike     float64
		iv         float64
		expiration time.Time
		now        time.Time
		want       float64
	}{
		{
			name: "standard ATM",
			spot: 100, strike: 100, iv: 0.2,
			expiration: time.Date(2030, time.January, 18, 0, 0, 0, 0, utc),
			now:        time.Date(2029, time.January, 18, 0, 0, 0, 0, utc),
			want:       0.019791884347237478,
		},
		{
			name: "volatility floor",
			spot: 100, strike: 100, iv: 0.01,
			expiration: time.Date(2030, time.January, 18, 0, 0, 0, 0, utc),
			now:        time.Date(2029, time.January, 18, 0, 0, 0, 0, utc),
			want:       0.07916753738894991,
		},
		{
			name: "expired minimum term",
			spot: 100, strike: 110, iv: 0.3,
			expiration: time.Date(2028, time.January, 18, 0, 0, 0, 0, utc),
			now:        time.Date(2029, time.January, 18, 0, 0, 0, 0, utc),
			want:       2.6794075891987976e-9,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, ok := EstimateOptionGamma(test.spot, test.strike, test.iv, test.expiration, test.now)
			if !ok || math.Abs(got-test.want) > math.Max(1e-14, math.Abs(test.want)*1e-12) {
				t.Fatalf("EstimateOptionGamma() = %.17g, %v; want %.17g, true", got, ok, test.want)
			}
		})
	}
}

func TestEstimateOptionGammaRejectsInvalidInputs(t *testing.T) {
	validExpiry := time.Date(2030, time.January, 18, 0, 0, 0, 0, time.UTC)
	validNow := time.Date(2029, time.January, 18, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		spot       float64
		strike     float64
		iv         float64
		expiration time.Time
		now        time.Time
	}{
		{name: "zero spot", spot: 0, strike: 100, iv: 0.2, expiration: validExpiry, now: validNow},
		{name: "negative strike", spot: 100, strike: -1, iv: 0.2, expiration: validExpiry, now: validNow},
		{name: "zero volatility", spot: 100, strike: 100, iv: 0, expiration: validExpiry, now: validNow},
		{name: "nan spot", spot: math.NaN(), strike: 100, iv: 0.2, expiration: validExpiry, now: validNow},
		{name: "infinite strike", spot: 100, strike: math.Inf(1), iv: 0.2, expiration: validExpiry, now: validNow},
		{name: "infinite volatility", spot: 100, strike: 100, iv: math.Inf(1), expiration: validExpiry, now: validNow},
		{name: "zero expiration", spot: 100, strike: 100, iv: 0.2, now: validNow},
		{name: "zero now", spot: 100, strike: 100, iv: 0.2, expiration: validExpiry},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got, ok := EstimateOptionGamma(test.spot, test.strike, test.iv, test.expiration, test.now); ok {
				t.Fatalf("EstimateOptionGamma() = %v, true; want false", got)
			}
		})
	}
}

func TestAnnotateUsesNearATMPriceSamples(t *testing.T) {
	spot := 100.0
	years := 30.0 / 365.0
	expiration := time.Now().Add(30 * 24 * time.Hour).In(newYork).Format("2006-01-02")
	bid, ask := 5.4, 5.7
	strike := 100.0
	iv := 0.01 // provider placeholder must not override a valid model sample
	rows := []model.OptionRow{{Expiration: expiration, ContractType: "call", Strike: &strike, Bid: &bid, Ask: &ask, ImpliedVolatility: &iv}}
	_ = years
	summary := Annotate(rows, &spot)
	detail, ok := summary[expiration].(map[string]any)
	if !ok || detail["source"] != "price" {
		t.Fatalf("summary = %#v, want price source", summary[expiration])
	}
	if rows[0].ModelIV == nil || rows[0].ModelGamma == nil || *rows[0].ModelGamma <= 0 {
		t.Fatalf("model Greeks missing: %#v", rows[0])
	}
}

func TestZeroGammaScanRangeUsesAllHorizonSelectedStrikes(t *testing.T) {
	// Python bounds its scan with every valid strike selected by date horizon,
	// not only contracts with positive OI that contribute to the GEX curve.
	// Include zero-OI contracts at 86 and 115; the 140 strike is outside the
	// horizon and must not affect bounds. The two active contracts alone would
	// have produced the narrower legacy interval [100,100].
	strikes := []float64{86, 100, 115}
	low, high := scanRange(strikes, 100)
	if low != 86 || math.Abs(high-115) > 1e-12 {
		t.Fatalf("scanRange()=(%v,%v), want (86,115) from Python min/max selected strikes clamped to ±15%%", low, high)
	}
	legacyLow, legacyHigh := scanRange([]float64{100}, 100) // only positive-OI strikes
	if legacyLow != 100 || legacyHigh != 100 {
		t.Fatalf("active-contract-only legacy scan bounds=(%v,%v), want [100,100] to demonstrate discrepancy", legacyLow, legacyHigh)
	}
	low, high = scanRange([]float64{60, 70}, 100)
	if low != 85 || high != 70 {
		t.Fatalf("scanRange()=(%v,%v), want Python's clamped empty interval (85,70)", low, high)
	}
}

func TestZeroGammaUsesAllSelectedRowsForScanBoundsAndExpirationMetadata(t *testing.T) {
	now := time.Now().In(newYork)
	expiration := now.AddDate(0, 0, 3).Format("2006-01-02")
	otherExpiration := now.AddDate(0, 0, 4).Format("2006-01-02")
	strikeLow, strikeATM, strikeHigh := 86.0, 100.0, 115.0
	iv := 0.3
	callOI, putOI := int64(100), int64(100)
	rows := []model.OptionRow{
		// Zero-OI outer strikes must still define Python's scan interval.
		{Expiration: expiration, ContractType: "call", Strike: &strikeLow},
		{Expiration: expiration, ContractType: "put", Strike: &strikeHigh},
		// Equal opposing ATM GEX gives a deterministic zero curve and nonnil result.
		{Expiration: expiration, ContractType: "call", Strike: &strikeATM, OpenInterest: &callOI, ImpliedVolatility: &iv},
		{Expiration: expiration, ContractType: "put", Strike: &strikeATM, OpenInterest: &putOI, ImpliedVolatility: &iv},
		// Python reports all selected expirations, including a selected row that
		// does not contribute because it has no open interest.
		{Expiration: otherExpiration, ContractType: "call", Strike: &strikeATM},
	}
	spot := 100.0
	result := ZeroGamma(rows, &spot, 7)
	if result == nil {
		t.Fatal("expected zero-gamma result with a scan interval supplied by selected outer strikes")
	}
	gotExpirations, ok := result["expirations"].([]string)
	if !ok || len(gotExpirations) != 2 || gotExpirations[0] != expiration || gotExpirations[1] != otherExpiration {
		t.Fatalf("selected expirations=%#v, want both in-horizon expiries including zero-OI rows", result["expirations"])
	}
}

func TestZeroGammaNoPositionInterestReturnsNil(t *testing.T) {
	spot, strike := 100.0, 100.0
	iv := 0.3
	rows := []model.OptionRow{{Expiration: time.Now().Add(7 * 24 * time.Hour).In(newYork).Format("2006-01-02"), ContractType: "call", Strike: &strike, ImpliedVolatility: &iv}}
	if result := ZeroGamma(rows, &spot, 45); result != nil {
		t.Fatalf("ZeroGamma() = %#v, want nil", result)
	}
}
