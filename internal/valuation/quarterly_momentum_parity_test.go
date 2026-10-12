package valuation

import (
	"reflect"
	"testing"
)

func TestQuarterlyMomentumMatchesPythonV39(t *testing.T) {
	tests := []struct {
		name string
		f    Fundamentals
		want map[string]any
	}{
		{
			name: "EPS alone does not create momentum",
			f:    Fundamentals{QuarterlyEPS: []float64{4, 3, 2, 1}},
		},
		{
			name: "fewer than four revenue quarters does not create momentum",
			f: Fundamentals{
				QuarterlyRevenue: []float64{40, 30, 20},
				QuarterlyEPS:     []float64{4, 3, 2, 1},
			},
		},
		{
			name: "revenue momentum retains null EPS when EPS is unavailable",
			f:    Fundamentals{QuarterlyRevenue: []float64{40, 20, 10, 20}},
			want: map[string]any{"revenue_yoy": 1.0, "revenue_qoq": 1.0, "eps_yoy": nil},
		},
		{
			name: "nonpositive revenue denominators retain null fields",
			f: Fundamentals{
				QuarterlyRevenue: []float64{5, 0, -2, 0},
				QuarterlyEPS:     []float64{4, 3, 2, 0},
			},
			want: map[string]any{"revenue_yoy": nil, "revenue_qoq": nil, "eps_yoy": nil},
		},
		{
			name: "valid revenue and EPS denominators produce all metrics",
			f: Fundamentals{
				QuarterlyRevenue: []float64{40, 20, 10, 20},
				QuarterlyEPS:     []float64{3, 2, 1, 2},
			},
			want: map[string]any{"revenue_yoy": 1.0, "revenue_qoq": 1.0, "eps_yoy": 0.5},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := quarterlyMomentum(test.f); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("quarterlyMomentum() = %#v, want %#v", got, test.want)
			}
		})
	}
}
