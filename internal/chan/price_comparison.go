package chananalysis

import "math"

const relativePriceTolerance = 1e-8

func priceTolerance(a, b float64) float64 {
	return relativePriceTolerance * math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func priceEqual(a, b float64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	return math.Abs(a-b) <= priceTolerance(a, b)
}

func priceLess(a, b float64) bool {
	return a < b && !priceEqual(a, b)
}

func priceGreater(a, b float64) bool {
	return a > b && !priceEqual(a, b)
}

func priceLessOrEqual(a, b float64) bool {
	return a < b || priceEqual(a, b)
}

func priceGreaterOrEqual(a, b float64) bool {
	return a > b || priceEqual(a, b)
}
