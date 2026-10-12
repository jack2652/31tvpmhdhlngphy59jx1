package provider

import "time"

// HistoryResult contains daily Yahoo bars normalized to the API's raw or
// adjusted OHLC representation. Extremes are populated only for max-period
// requests.
type HistoryResult struct {
	Symbol           string           `json:"symbol"`
	Bars             []map[string]any `json:"bars"`
	FetchedAt        time.Time        `json:"fetched_at"`
	Source           string           `json:"source"`
	FiftyTwoWeekHigh *float64         `json:"fifty_two_week_high,omitempty"`
	FiftyTwoWeekLow  *float64         `json:"fifty_two_week_low,omitempty"`
	AllTimeHigh      *float64         `json:"all_time_high,omitempty"`
	AllTimeLow       *float64         `json:"all_time_low,omitempty"`
}

func historyExtremes(bars []map[string]any) (*float64, *float64) {
	var high, low *float64
	for _, bar := range bars {
		if value, ok := bar["high"].(float64); ok {
			if high == nil || value > *high {
				v := value
				high = &v
			}
		}
		if value, ok := bar["low"].(float64); ok {
			if low == nil || value < *low {
				v := value
				low = &v
			}
		}
	}
	return high, low
}
