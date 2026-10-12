package valuation

import (
	"reflect"
	"testing"
)

func TestDetectRegimeUsesProviderFundamentalSignals(t *testing.T) {
	capex := -20.0
	previousCapex := -30.0
	capexGrowth := abs(capex)/abs(previousCapex) - 1
	_ = capexGrowth
	revision := -0.30
	inventory := 135.0
	revenue := -0.12
	regime, signals := DetectRegime(RegimeInput{
		RevenueGrowth: &revenue, EPSRevision: &revision, CapexGrowth: &capexGrowth, InventoryDays: &inventory,
	})
	if regime != RegimeWinter {
		t.Fatalf("regime=%q, want %q with fundamental stress signals", regime, RegimeWinter)
	}
	if signals["eps_revision_1m"] != -0.3 || signals["capex_growth"] != -0.3333 || signals["inventory_days"] != 135.0 {
		t.Fatalf("signals=%#v, want revision/capex/inventory values", signals)
	}
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func regimePtr(value float64) *float64 { return &value }

func pythonV39RegimeSignals(tnx, vix, revenue, revision, capex, inventory, momentum any, extreme, winter, slowdown []string) map[string]any {
	signals := map[string]any{
		"tnx_yield": tnx, "vix": vix, "revenue_growth": revenue,
		"eps_revision_1m": revision, "capex_growth": capex,
		"inventory_days": inventory, "benchmark_momentum_60d": momentum,
		"extreme_hits": append([]string{}, extreme...),
	}
	if winter != nil {
		signals["winter_hits"] = append([]string{}, winter...)
		signals["slowdown_hits"] = append([]string{}, slowdown...)
	}
	return signals
}

func TestDetectRegimeMatchesPythonV39Goldens(t *testing.T) {
	boomCloses := make([]float64, 60)
	extremeCloses := make([]float64, 60)
	moderateDeclineCloses := make([]float64, 60)
	slowdownBoundaryCloses := make([]float64, 60)
	for index := range boomCloses {
		boomCloses[index], extremeCloses[index] = 100, 100
		moderateDeclineCloses[index], slowdownBoundaryCloses[index] = 100, 100
		if index >= 40 {
			boomCloses[index] = 110
			extremeCloses[index] = 79
			moderateDeclineCloses[index] = 84
			slowdownBoundaryCloses[index] = 92
		}
	}
	shortCloses := make([]float64, 59)
	filteredCloses := make([]float64, 80)
	for index := range filteredCloses {
		switch {
		case index < 30:
			filteredCloses[index] = 100
		case index < 60:
			filteredCloses[index] = 0
		default:
			filteredCloses[index] = 110
		}
	}

	cases := []struct {
		name        string
		input       RegimeInput
		wantRegime  string
		wantSignals map[string]any
	}{
		{
			name:       "all signals missing",
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "two slowdown signals",
			input:      RegimeInput{RevenueGrowth: regimePtr(.04), EPSRevision: regimePtr(-.11)},
			wantRegime: RegimeSlowdown,
			wantSignals: pythonV39RegimeSignals(nil, nil, .04, -.11, nil, nil, nil,
				[]string{}, []string{}, []string{"revenue_growth", "eps_revision"}),
		},
		{
			name:       "two winter signals take precedence",
			input:      RegimeInput{RevenueGrowth: regimePtr(-.11), EPSRevision: regimePtr(-.26)},
			wantRegime: RegimeWinter,
			wantSignals: pythonV39RegimeSignals(nil, nil, -.11, -.26, nil, nil, nil,
				[]string{}, []string{"revenue_growth", "eps_revision"}, []string{"revenue_growth", "eps_revision"}),
		},
		{
			name:       "capex and inventory winter signals",
			input:      RegimeInput{CapexGrowth: regimePtr(-.16), InventoryDays: regimePtr(121)},
			wantRegime: RegimeWinter,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, -.16, 121.0, nil,
				[]string{}, []string{"capex_growth", "inventory_days"}, []string{}),
		},
		{
			name:       "treasury yield winter signal",
			input:      RegimeInput{TNXYield: regimePtr(.056)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(.056, nil, nil, nil, nil, nil, nil,
				[]string{}, []string{"tnx_yield"}, []string{"tnx_yield"}),
		},
		{
			name:       "extreme vix override",
			input:      RegimeInput{VIX: regimePtr(46)},
			wantRegime: RegimeWinter,
			wantSignals: pythonV39RegimeSignals(nil, 46.0, nil, nil, nil, nil, nil,
				[]string{"vix_extreme"}, nil, nil),
		},
		{
			name:       "extreme benchmark momentum override",
			input:      RegimeInput{BenchmarkCloses: extremeCloses},
			wantRegime: RegimeWinter,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, -.21,
				[]string{"momentum_extreme"}, nil, nil),
		},
		{
			name:       "extreme earnings revision override",
			input:      RegimeInput{EPSRevision: regimePtr(-.36)},
			wantRegime: RegimeWinter,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, -.36, nil, nil, nil,
				[]string{"eps_revision_extreme"}, nil, nil),
		},
		{
			name:       "boom allows missing revenue growth",
			input:      RegimeInput{BenchmarkCloses: boomCloses},
			wantRegime: RegimeBoom,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, .1,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "boom requires growth strictly above twenty percent",
			input:      RegimeInput{RevenueGrowth: regimePtr(.20), BenchmarkCloses: boomCloses},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, .2, nil, nil, nil, .1,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "winter threshold equality remains slowdown",
			input:      RegimeInput{RevenueGrowth: regimePtr(-.10), EPSRevision: regimePtr(-.25)},
			wantRegime: RegimeSlowdown,
			wantSignals: pythonV39RegimeSignals(nil, nil, -.1, -.25, nil, nil, nil,
				[]string{}, []string{}, []string{"revenue_growth", "eps_revision"}),
		},
		{
			name:       "capex and inventory winter threshold equality",
			input:      RegimeInput{CapexGrowth: regimePtr(-.15), InventoryDays: regimePtr(120)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, -.15, 120.0, nil,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "treasury yield slowdown threshold equality",
			input:      RegimeInput{TNXYield: regimePtr(.045)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(.045, nil, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "vix winter threshold equality remains slowdown",
			input:      RegimeInput{VIX: regimePtr(35)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, 35.0, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{"vix"}),
		},
		{
			name:       "benchmark momentum slowdown signal",
			input:      RegimeInput{BenchmarkCloses: moderateDeclineCloses},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, -.16,
				[]string{}, []string{"benchmark_momentum"}, []string{"benchmark_momentum"}),
		},
		{
			name:       "benchmark momentum slowdown threshold equality",
			input:      RegimeInput{BenchmarkCloses: slowdownBoundaryCloses},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, -.08,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "vix extreme threshold equality is not an override",
			input:      RegimeInput{VIX: regimePtr(45)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, 45.0, nil, nil, nil, nil, nil,
				[]string{}, []string{"vix"}, []string{"vix"}),
		},
		{
			name:       "benchmark requires sixty valid closes",
			input:      RegimeInput{BenchmarkCloses: shortCloses},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "invalid benchmark closes are filtered",
			input:      RegimeInput{BenchmarkCloses: filteredCloses},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, nil, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{}),
		},
		{
			name:       "rounding follows python decimal rounding",
			input:      RegimeInput{VIX: regimePtr(2.675)},
			wantRegime: RegimeNormal,
			wantSignals: pythonV39RegimeSignals(nil, 2.67, nil, nil, nil, nil, nil,
				[]string{}, []string{}, []string{}),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			regime, signals := DetectRegime(test.input)
			if regime != test.wantRegime {
				t.Fatalf("regime=%q, want Python v39 golden %q; signals=%#v", regime, test.wantRegime, signals)
			}
			if !reflect.DeepEqual(signals, test.wantSignals) {
				t.Fatalf("signals=%#v, want Python v39 golden %#v", signals, test.wantSignals)
			}
		})
	}
}
