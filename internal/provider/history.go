package provider

import (
	"context"
	"time"
)

// HistoryResult 保存日线或盘中历史行情的标准化 OHLC 结果。
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

// ChanAdjustedHistoryResult keeps the adjusted structure series apart from the
// raw OHLC used by quotes and other analysis modules. Each bar includes both
// coordinate systems plus the adjusted-close/raw-close conversion factor.
type ChanAdjustedHistoryResult struct {
	Symbol         string           `json:"symbol"`
	Bars           []map[string]any `json:"bars"`
	FetchedAt      time.Time        `json:"fetched_at"`
	Source         string           `json:"source"`
	AdjustmentType string           `json:"adjustment_type"`
	Timeframe      string           `json:"timeframe"`
}

// HistoryExtremePoint 保存计算价格极值所需的单根日线数据；日期使用 UTC 零点。
type HistoryExtremePoint struct {
	Date    time.Time
	High    float64
	Low     float64
	HasHigh bool
	HasLow  bool
}

// PriceExtremesHistoryClient 只返回高低价计算所需的数据，不构造完整 OHLCV 记录。
type PriceExtremesHistoryClient interface {
	FetchPriceExtremes(context.Context, string) ([]HistoryExtremePoint, time.Time, error)
}

// PriceExtremesHistorySource 暴露可选的紧凑价格极值读取能力。
type PriceExtremesHistorySource interface {
	PriceExtremesHistoryClient() PriceExtremesHistoryClient
}
