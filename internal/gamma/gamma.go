package gamma

import (
	"math"
	"sort"
	"strings"
	"time"

	"option-scope-go/internal/model"
)

const (
	riskFreeRate = 0.005
	ivMin        = 0.03
	ivMax        = 5.0
	ivFloor      = 0.05
	ivCeiling    = 3.0
	ivFallback   = 0.25
	ivMinPrice   = 0.02
	minMinutes   = 30.0
)

var newYork, _ = time.LoadLocation("America/New_York")

// Annotate estimates one implied volatility per expiry from near-ATM option prices,
// then adds model_iv/model_gamma to each row. Provider IV is a bounded fallback only.
func Annotate(rows []model.OptionRow, spot *float64) map[string]any {
	result := map[string]any{}
	if spot == nil || *spot <= 0 {
		return result
	}
	groups := make(map[string][]int)
	for index := range rows {
		groups[rows[index].Expiration] = append(groups[rows[index].Expiration], index)
	}
	for expiration, indexes := range groups {
		now := time.Now()
		years := yearsToExpiry(expiration, now)
		samples := make([]ivSample, 0, len(indexes))
		providerIVs := make([]float64, 0, len(indexes))
		for _, index := range indexes {
			row := rows[index]
			if row.Strike == nil || *row.Strike <= 0 {
				continue
			}
			if row.ImpliedVolatility != nil && *row.ImpliedVolatility >= ivFloor && *row.ImpliedVolatility <= ivCeiling {
				providerIVs = append(providerIVs, *row.ImpliedVolatility)
			}
			price := optionPrice(row)
			if price < ivMinPrice || years <= 0 || math.Abs(math.Log(*row.Strike / *spot)) > 0.12 {
				continue
			}
			isCall := row.ContractType == "call"
			intrinsic := 0.0
			if isCall {
				intrinsic = math.Max(0, *spot-*row.Strike)
			} else {
				intrinsic = math.Max(0, *row.Strike-*spot)
			}
			if price < intrinsic*0.98 {
				continue
			}
			if iv, ok := impliedVolatility(price, *spot, *row.Strike, years, isCall); ok {
				samples = append(samples, ivSample{distance: math.Abs(math.Log(*row.Strike / *spot)), iv: iv})
			}
		}
		volatility, sampleCount, source := chooseIV(samples, providerIVs)
		if len(samples) > 0 {
			if nearCount := nearSampleCount(samples); nearCount >= 3 {
				sampleCount = nearCount
			} else {
				sampleCount = len(samples)
			}
		}
		info := map[string]any{"iv": math.Round(volatility*1e6) / 1e6, "samples": sampleCount, "source": source}
		result[expiration] = info
		for _, index := range indexes {
			row := &rows[index]
			if row.Strike == nil {
				continue
			}
			iv := volatility
			row.ModelIV = &iv
			row.ModelIVSource = source
			if value := blackScholesGamma(*spot, *row.Strike, iv, years); value > 0 && !math.IsInf(value, 0) {
				row.ModelGamma = &value
			}
		}
	}
	return result
}

type ivSample struct {
	distance float64
	iv       float64
}

func nearSampleCount(samples []ivSample) int {
	count := 0
	for _, sample := range samples {
		if sample.distance <= 0.03 {
			count++
		}
	}
	return count
}

func chooseIV(samples []ivSample, provider []float64) (float64, int, string) {
	source := "default"
	near := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.distance <= 0.03 {
			near = append(near, sample.iv)
		}
	}
	values := near
	if len(near) < 3 {
		values = make([]float64, 0, len(samples))
		for _, sample := range samples {
			values = append(values, sample.iv)
		}
	}
	if len(values) > 0 {
		source = "price"
	} else if len(provider) > 0 {
		values = append(values, provider...)
		source = "provider"
	} else {
		return ivFallback, 0, source
	}
	sort.Float64s(values)
	value := values[len(values)/2]
	return math.Max(ivFloor, math.Min(ivCeiling, value)), len(values), source
}

func optionPrice(row model.OptionRow) float64 {
	if row.Bid != nil && row.Ask != nil && *row.Bid > 0 && *row.Ask >= *row.Bid {
		return (*row.Bid + *row.Ask) / 2
	}
	if row.LastPrice != nil {
		return *row.LastPrice
	}
	return 0
}

func yearsToExpiry(expiration string, now time.Time) float64 {
	if newYork == nil {
		return 0
	}
	date, err := time.ParseInLocation("2006-01-02", expiration, newYork)
	if err != nil {
		return 0
	}
	expires := time.Date(date.Year(), date.Month(), date.Day(), 16, 0, 0, 0, newYork)
	seconds := expires.Sub(now)
	return math.Max(seconds.Seconds()/(365*24*3600), minMinutes/(365*24*60))
}

// Gamma exposes the bounded Black-Scholes gamma calculation for consumers that
// need to estimate GEX from a provider implied-volatility fallback.
func Gamma(spot, strike, volatility, years float64) float64 {
	return blackScholesGamma(spot, strike, volatility, years)
}

// EstimateOptionGamma mirrors the Python provider's per-contract estimate.
// Expiration is treated as UTC midnight on its calendar date; a minimum
// one-day term and a 5% volatility floor keep expired/low-IV contracts bounded.
func EstimateOptionGamma(spot, strike, impliedVolatility float64, expiration, now time.Time) (float64, bool) {
	if spot <= 0 || strike <= 0 || impliedVolatility <= 0 ||
		math.IsNaN(spot) || math.IsInf(spot, 0) ||
		math.IsNaN(strike) || math.IsInf(strike, 0) ||
		math.IsNaN(impliedVolatility) || math.IsInf(impliedVolatility, 0) ||
		expiration.IsZero() || now.IsZero() {
		return 0, false
	}
	expiryUTC := expiration.UTC()
	expiryUTC = time.Date(expiryUTC.Year(), expiryUTC.Month(), expiryUTC.Day(), 0, 0, 0, 0, time.UTC)
	years := math.Max(expiryUTC.Sub(now.UTC()).Seconds()/(365*24*60*60), 1.0/365)
	volatility := math.Max(impliedVolatility, 0.05)
	volatilityTime := volatility * math.Sqrt(years)
	d1 := (math.Log(spot/strike) + (riskFreeRate+0.5*volatility*volatility)*years) / volatilityTime
	value := math.Exp(-0.5*d1*d1) / (spot * volatilityTime * math.Sqrt(2*math.Pi))
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}

func blackScholesGamma(spot, strike, volatility, years float64) float64 {
	if spot <= 0 || strike <= 0 || volatility <= 0 || years <= 0 ||
		math.IsNaN(spot) || math.IsNaN(strike) || math.IsNaN(volatility) || math.IsNaN(years) ||
		math.IsInf(spot, 0) || math.IsInf(strike, 0) || math.IsInf(volatility, 0) || math.IsInf(years, 0) {
		return 0
	}
	volatilityTime := volatility * math.Sqrt(years)
	d1 := (math.Log(spot/strike) + (riskFreeRate+0.5*volatility*volatility)*years) / volatilityTime
	return math.Exp(-0.5*d1*d1) / (spot * volatilityTime * math.Sqrt(2*math.Pi))
}

func normCDF(value float64) float64 { return 0.5 * (1 + math.Erf(value/math.Sqrt2)) }

func blackScholesPrice(spot, strike, volatility, years float64, isCall bool) float64 {
	volatilityTime := volatility * math.Sqrt(years)
	if spot <= 0 || strike <= 0 || volatilityTime <= 0 {
		return 0
	}
	d1 := (math.Log(spot/strike) + (riskFreeRate+0.5*volatility*volatility)*years) / volatilityTime
	d2 := d1 - volatilityTime
	discount := math.Exp(-riskFreeRate * years)
	if isCall {
		return spot*normCDF(d1) - strike*discount*normCDF(d2)
	}
	return strike*discount*normCDF(-d2) - spot*normCDF(-d1)
}

func impliedVolatility(price, spot, strike, years float64, isCall bool) (float64, bool) {
	if price <= 0 || spot <= 0 || strike <= 0 || years <= 0 || blackScholesPrice(spot, strike, ivMax, years, isCall) < price || blackScholesPrice(spot, strike, ivMin, years, isCall) > price {
		return 0, false
	}
	low, high := ivMin, ivMax
	for range 60 {
		middle := (low + high) / 2
		if blackScholesPrice(spot, strike, middle, years, isCall) < price {
			low = middle
		} else {
			high = middle
		}
	}
	return (low + high) / 2, true
}

// ZeroGamma scans a spot-shift GEX curve and returns the closest regime-consistent root.
func ZeroGamma(rows []model.OptionRow, spot *float64, horizonDays int) map[string]any {
	if spot == nil || *spot <= 0 {
		return nil
	}
	if horizonDays < 1 {
		horizonDays = 45
	}
	now := time.Now()
	expirationSet := map[string]bool{}
	scanStrikes := make([]float64, 0, len(rows))
	type input struct{ strike, iv, oi, sign, years float64 }
	prepared := make([]input, 0, len(rows))
	for _, row := range rows {
		if row.Expiration == "" {
			continue
		}
		expiration, err := time.ParseInLocation("2006-01-02", row.Expiration, newYork)
		if err != nil {
			continue
		}
		marketToday := now.In(newYork)
		endDay := marketToday.AddDate(0, 0, horizonDays)
		expiryTime := time.Date(expiration.Year(), expiration.Month(), expiration.Day(), 16, 0, 0, 0, newYork)
		if expiration.Before(time.Date(marketToday.Year(), marketToday.Month(), marketToday.Day(), 0, 0, 0, 0, newYork)) || expiration.After(endDay) || now.After(expiryTime.Add(30*time.Minute)) {
			continue
		}
		expirationSet[row.Expiration] = true
		if row.Strike == nil || *row.Strike <= 0 {
			continue
		}
		scanStrikes = append(scanStrikes, *row.Strike)
		years := yearsToExpiry(row.Expiration, now)
		if years <= 0 || row.OpenInterest == nil || *row.OpenInterest <= 0 {
			continue
		}
		iv := ivFallback
		if row.ModelIV != nil && *row.ModelIV >= ivFloor && *row.ModelIV <= ivCeiling {
			iv = *row.ModelIV
		} else if row.ImpliedVolatility != nil && *row.ImpliedVolatility >= ivFloor && *row.ImpliedVolatility <= ivMax {
			iv = *row.ImpliedVolatility
		}
		sign := -1.0
		if strings.EqualFold(row.ContractType, "call") {
			sign = 1
		}
		prepared = append(prepared, input{*row.Strike, iv, float64(*row.OpenInterest), sign, years})
	}
	if len(prepared) == 0 || len(scanStrikes) == 0 {
		return nil
	}
	lower, upper := scanRange(scanStrikes, *spot)
	if upper <= lower {
		return nil
	}
	gexAt := func(price float64) float64 {
		total := 0.0
		for _, row := range prepared {
			g := blackScholesGamma(price, row.strike, row.iv, row.years)
			total += row.sign * g * row.oi * 100 * price * price * 0.01
		}
		return total
	}
	roots := []float64{}
	previousSpot := lower
	previousValue := gexAt(lower)
	for index := 1; index <= 161; index++ {
		nextSpot := lower + (upper-lower)*float64(index)/161
		nextValue := gexAt(nextSpot)
		if previousValue == 0 {
			roots = append(roots, previousSpot)
		}
		if previousValue*nextValue < 0 {
			left, right, leftValue := previousSpot, nextSpot, previousValue
			for range 40 {
				middle := (left + right) / 2
				middleValue := gexAt(middle)
				if leftValue*middleValue <= 0 {
					right = middle
				} else {
					left, leftValue = middle, middleValue
				}
			}
			roots = append(roots, (left+right)/2)
		}
		previousSpot, previousValue = nextSpot, nextValue
	}
	if len(roots) == 0 {
		return nil
	}
	netGEX := gexAt(*spot)
	candidates := roots
	if actionable := filterActionable(roots, *spot); len(actionable) > 0 {
		candidates = actionable
	}
	chosen := nearest(candidates, *spot)
	if netGEX >= 0 {
		foundBelow := false
		for _, root := range candidates {
			if root <= *spot && (!foundBelow || root > chosen) {
				chosen, foundBelow = root, true
			}
		}
	} else {
		foundAbove := false
		for _, root := range candidates {
			if root >= *spot && (!foundAbove || root < chosen) {
				chosen, foundAbove = root, true
			}
		}
	}
	expirations := make([]string, 0, len(expirationSet))
	for expiration := range expirationSet {
		expirations = append(expirations, expiration)
	}
	sort.Strings(expirations)
	return map[string]any{"price": chosen, "net_gex": netGEX, "expirations": expirations, "horizon_days": horizonDays, "method": "spot-shift-7d-et-close"}
}

func scanRange(strikes []float64, spot float64) (float64, float64) {
	if len(strikes) == 0 || spot <= 0 {
		return 0, 0
	}
	minimum, maximum := strikes[0], strikes[0]
	for _, strike := range strikes[1:] {
		minimum = math.Min(minimum, strike)
		maximum = math.Max(maximum, strike)
	}
	return math.Max(minimum, spot*0.85), math.Min(maximum, spot*1.15)
}

func filterActionable(roots []float64, spot float64) []float64 {
	filtered := make([]float64, 0, len(roots))
	for _, root := range roots {
		if math.Abs(root-spot)/spot <= 0.08 {
			filtered = append(filtered, root)
		}
	}
	return filtered
}

func nearest(values []float64, spot float64) float64 {
	best := values[0]
	for _, value := range values[1:] {
		if math.Abs(value-spot) < math.Abs(best-spot) {
			best = value
		}
	}
	return best
}
