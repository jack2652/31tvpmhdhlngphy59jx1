package model

import (
	"encoding/json"
	"time"
)

type Quote struct {
	SnapshotID                        int64          `json:"-"`
	RawJSON                           *string        `json:"-"`
	FairValueMethodsJSON              *string        `json:"-"`
	FairValueHistoricalValuationJSON  *string        `json:"-"`
	Symbol                            string         `json:"symbol"`
	Price                             *float64       `json:"price"`
	ChangePercent                     *float64       `json:"change_percent"`
	TodayOpen                         *float64       `json:"today_open"`
	PreviousClose                     *float64       `json:"previous_close"`
	DailyBar                          *DailyBar      `json:"daily_bar,omitempty"`
	Currency                          string         `json:"currency"`
	MarketState                       string         `json:"market_state"`
	Provider                          string         `json:"provider"`
	Source                            string         `json:"source"`
	FetchedAt                         *time.Time     `json:"fetched_at"`
	Sessions                          map[string]any `json:"sessions"`
	FairValue                         any            `json:"fair_value"`
	FairValueLow                      any            `json:"fair_value_low"`
	FairValueHigh                     any            `json:"fair_value_high"`
	FairValueBuyLow                   any            `json:"fair_value_buy_low"`
	FairValueBuyHigh                  any            `json:"fair_value_buy_high"`
	FairValueSource                   string         `json:"fair_value_source"`
	FairValueModel                    string         `json:"fair_value_model"`
	FairValueMethods                  any            `json:"fair_value_methods,omitempty"`
	FairValueForwardEPS               *float64       `json:"fair_value_forward_eps"`
	FairValueForwardEPSSource         *string        `json:"fair_value_forward_eps_source"`
	FairValueNormalizedEPS            *float64       `json:"fair_value_normalized_eps"`
	FairValuePELow                    *float64       `json:"fair_value_pe_low"`
	FairValuePEHigh                   *float64       `json:"fair_value_pe_high"`
	FairValueDecisionTree             any            `json:"fair_value_decision_tree"`
	FairValueValidation               any            `json:"fair_value_validation"`
	FairValueSafetyMargin             *float64       `json:"fair_value_safety_margin"`
	FairValueConfidence               *string        `json:"fair_value_confidence"`
	FairValueConfidenceScore          *float64       `json:"fair_value_confidence_score"`
	FairValueInterestCoverage         *float64       `json:"fair_value_interest_coverage"`
	FairValueRegime                   *string        `json:"fair_value_regime"`
	FairValueRegimeSignals            any            `json:"fair_value_regime_signals"`
	FairValueModelUnderRegime         *string        `json:"fair_value_model_under_regime"`
	FairValueDefensive                any            `json:"fair_value_defensive"`
	FairValueOptimistic               any            `json:"fair_value_optimistic"`
	FairValueNormalizedEPSSource      *string        `json:"fair_value_normalized_eps_source"`
	FairValueQuarterlyMomentum        any            `json:"fair_value_quarterly_momentum"`
	FairValueHistoricalValuationPct   any            `json:"fair_value_historical_valuation_percentiles,omitempty"`
	FairValueShareholderReturnYield   *float64       `json:"fair_value_shareholder_total_return_yield"`
	FairValueMarketCapDataQuality     *string        `json:"fair_value_market_cap_data_quality"`
	FairValueDataQualityScore         *float64       `json:"fair_value_data_quality_score"`
	FairValueOwnerEarningsMaintRatio  *float64       `json:"fair_value_owner_earnings_maintenance_ratio"`
	FairValueOwnerEarningsRatioSource *string        `json:"fair_value_owner_earnings_ratio_source"`
	FairValueStatus                   string         `json:"fair_value_status"`
	FairValuePending                  bool           `json:"fair_value_pending"`
	FairValueWarning                  string         `json:"fair_value_warning,omitempty"`
	FairValueWarnings                 []string       `json:"fair_value_warnings,omitempty"`
	FairValueErrors                   []string       `json:"fair_value_errors,omitempty"`
	Warning                           string         `json:"warning,omitempty"`
	Earnings                          any            `json:"-"`
}

type DailyBar struct {
	Date      string  `json:"date"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
	IsForming bool    `json:"is_forming"`
}

type ExpirationsResponse struct {
	Symbol            string   `json:"symbol"`
	Expirations       []string `json:"expirations"`
	CachedExpirations []string `json:"cached_expirations"`
	Source            string   `json:"source"`
	Warning           string   `json:"warning,omitempty"`
}

type OptionRow struct {
	ContractSymbol    string   `json:"contract_symbol"`
	Provider          string   `json:"provider,omitempty"`
	ContractType      string   `json:"contract_type"`
	Strike            *float64 `json:"strike"`
	LastPrice         *float64 `json:"last_price"`
	Bid               *float64 `json:"bid"`
	Ask               *float64 `json:"ask"`
	Volume            *int64   `json:"volume"`
	OpenInterest      *int64   `json:"open_interest"`
	ImpliedVolatility *float64 `json:"implied_volatility"`
	Gamma             *float64 `json:"gamma"`
	ModelIV           *float64 `json:"model_iv"`
	ModelGamma        *float64 `json:"model_gamma"`
	ModelIVSource     string   `json:"model_iv_source,omitempty"`
	InTheMoney        bool     `json:"-"`
	// InTheMoneyKnown distinguishes explicit false from an upstream null.
	InTheMoneyKnown bool     `json:"-"`
	ChangePercent   *float64 `json:"change_percent"`
	Expiration      string   `json:"expiration"`
}

func (row OptionRow) MarshalJSON() ([]byte, error) {
	type optionRowAlias OptionRow
	var inTheMoney *bool
	if row.InTheMoneyKnown {
		inTheMoney = &row.InTheMoney
	}
	return json.Marshal(struct {
		*optionRowAlias
		InTheMoney *bool `json:"in_the_money"`
	}{optionRowAlias: (*optionRowAlias)(&row), InTheMoney: inTheMoney})
}

type ChainResponse struct {
	Symbol     string         `json:"symbol"`
	Expiration string         `json:"expiration"`
	FetchedAt  *time.Time     `json:"fetched_at"`
	Data       []OptionRow    `json:"data"`
	Source     string         `json:"source"`
	IVModel    map[string]any `json:"iv_model,omitempty"`
	OIFallback map[string]any `json:"oi_fallback,omitempty"`
}

type RefreshResponse struct {
	Skipped     bool           `json:"skipped"`
	Coalesced   bool           `json:"coalesced,omitempty"`
	Deferred    bool           `json:"deferred,omitempty"`
	Stale       bool           `json:"stale,omitempty"`
	Warning     string         `json:"warning,omitempty"`
	FetchedAt   *time.Time     `json:"fetched_at,omitempty"`
	AgeSeconds  float64        `json:"age_seconds,omitempty"`
	Expiration  string         `json:"expiration,omitempty"`
	Rows        int            `json:"rows,omitempty"`
	QuoteOnly   bool           `json:"quote_only,omitempty"`
	Quote       *Quote         `json:"quote,omitempty"`
	Chain       *ChainResponse `json:"chain,omitempty"`
	Expirations []string       `json:"expirations"`
}

type Event struct {
	ID         int64  `json:"-"`
	Symbol     string `json:"symbol"`
	Kind       string `json:"kind"`
	CreatedAt  string `json:"created_at"`
	Expiration string `json:"expiration,omitempty"`
	// Payload is transient data attached to live SSE frames. It is deliberately
	// excluded from JSON/SQLite persistence so the existing push_events schema
	// remains compatible with the Python service.
	Payload map[string]any `json:"-"`
}
