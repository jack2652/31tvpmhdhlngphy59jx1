package valuation

import (
	"reflect"
	"testing"
)

func TestHistoricalValuationPercentilesMatchPythonV39(t *testing.T) {
	// Mirrors Python's _historical_valuation_percentiles inputs, newest first.
	// Python result: PE 20.0/40.0 (5 obs), PB 20.0/60.0 (5 obs),
	// EV/EBITDA 17.0/60.0 (5 obs).
	inputs := Fundamentals{
		CurrentPrice: ptr(100), TrailingEPS: ptr(5), BookValue: ptr(5),
		HistoricalPrices:                  []float64{80, 120, 90, 110, 70},
		AnnualEPS:                         []float64{4, 6, 3, 5, 2},
		AnnualShares:                      []float64{10, 11, 9, 10, 8},
		AnnualEquity:                      []float64{50, 55, 45, 50, 40},
		AnnualDebt:                        []float64{30, 50, 20, 60, 40},
		AnnualCash:                        []float64{10, 20, 10, 30, 20},
		AnnualOperatingIncome:             []float64{50, 60, 45, 55, 30},
		AnnualDepreciationAndAmortization: []float64{10, 10, 5, 10, 5},
		// Latest values map Python's shares/debt/cash inputs. Omit scalar operating
		// income so the annual operating-income series supplies the current value.
		SharesOutstanding: ptr(10), Debt: ptr(30), Cash: ptr(10),
	}
	want := map[string]any{
		"pe_current":             20.0,
		"pe_percentile":          40.0,
		"pe_observations":        5,
		"pb_current":             20.0,
		"pb_percentile":          60.0,
		"pb_observations":        5,
		"ev_ebitda_current":      17.0,
		"ev_ebitda_percentile":   60.0,
		"ev_ebitda_observations": 5,
	}
	if got := historicalValuationPercentiles(inputs); !reflect.DeepEqual(got, want) {
		t.Fatalf("historical percentiles = %#v, want Python v39 result %#v", got, want)
	}
}

func TestHistoricalEVEBITDAPercentileUsesAnnualCurrentInputsWhenScalarsAreMissing(t *testing.T) {
	inputs := Fundamentals{
		CurrentPrice: ptr(100), HistoricalPrices: []float64{80, 120, 90, 110, 70},
		SharesOutstanding: nil, AnnualShares: []float64{10, 11, 9, 10, 8},
		Debt: nil, AnnualDebt: []float64{30, 50, 20, 60, 40},
		Cash: nil, AnnualCash: []float64{10, 20, 10, 30, 20},
		OperatingIncome: nil, AnnualOperatingIncome: []float64{50, 60, 45, 55, 30},
		AnnualDepreciationAndAmortization: []float64{10, 10, 5, 10, 5},
	}
	got := historicalValuationPercentiles(inputs)
	for key, want := range map[string]any{
		"ev_ebitda_current":      17.0,
		"ev_ebitda_percentile":   60.0,
		"ev_ebitda_observations": 5,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v; all percentiles=%#v", key, got[key], want, got)
		}
	}
}
