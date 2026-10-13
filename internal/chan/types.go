// Package chananalysis implements a documented, conservative pen-based interpretation
// of Chan price structure. Timeframes are chart periods, not recursive levels.
package chananalysis

import "time"

const RuleVersion = "strict-old-bi-v2"

type Bar struct {
	// CapturedAt keeps a daily candle fetched before its session close from
	// becoming confirmed merely because the clock has advanced.
	CapturedAt time.Time `json:"-"`
	Date       string    `json:"date"`
	Open       float64   `json:"open"`
	High       float64   `json:"high"`
	Low        float64   `json:"low"`
	Close      float64   `json:"close"`
	Volume     float64   `json:"volume"`
}

type Fractal struct {
	Kind        string  `json:"kind"`
	Date        string  `json:"date"`
	Price       float64 `json:"price"`
	ConfirmedAt string  `json:"confirmed_at"`
	MergedIndex int     `json:"merged_index"`
	RawIndex    int     `json:"raw_index"`
}

type Stroke struct {
	Direction   string  `json:"direction"`
	Start       Fractal `json:"start"`
	End         Fractal `json:"end"`
	Low         float64 `json:"low"`
	High        float64 `json:"high"`
	Confirmed   bool    `json:"confirmed"`
	ConfirmedAt string  `json:"confirmed_at,omitempty"`
	MACDArea    float64 `json:"macd_area"`
}

type Center struct {
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Lower       float64 `json:"lower"`
	Upper       float64 `json:"upper"`
	ConfirmedAt string  `json:"confirmed_at"`
	StartStroke int     `json:"start_stroke"`
	EndStroke   int     `json:"end_stroke"`
	StrokeCount int     `json:"stroke_count"`
}

type Position struct {
	Code       string   `json:"code"`
	Label      string   `json:"label"`
	Detail     string   `json:"detail"`
	Center     *Center  `json:"center"`
	Support    *float64 `json:"support"`
	Resistance *float64 `json:"resistance"`
}

type Signal struct {
	Kind             string   `json:"kind"`
	Label            string   `json:"label"`
	Status           string   `json:"status"`
	Date             string   `json:"date"`
	ConfirmedAt      string   `json:"confirmed_at"`
	Detail           string   `json:"detail"`
	Invalidation     *float64 `json:"invalidation"`
	StrengthRatio    *float64 `json:"strength_ratio,omitempty"`
	Side             string   `json:"side,omitempty"`
	Price            *float64 `json:"price,omitempty"`
	TriggerPrice     *float64 `json:"trigger_price,omitempty"`
	TriggerConfirmed bool     `json:"trigger_confirmed,omitempty"`
	RetestPrice      *float64 `json:"retest_price,omitempty"`
	Confirmation     string   `json:"confirmation,omitempty"`
	NextStep         string   `json:"next_step,omitempty"`
	OriginDate       string   `json:"origin_date,omitempty"`
	Phase            string   `json:"phase,omitempty"`
	ObservedAt       string   `json:"observed_at,omitempty"`
	PhaseAt          string   `json:"phase_at,omitempty"`
}

type Scenario struct {
	Direction    string `json:"direction"`
	Label        string `json:"label"`
	Condition    string `json:"condition"`
	Expectation  string `json:"expectation"`
	Invalidation string `json:"invalidation"`
}

type Frame struct {
	Period      string     `json:"period"`
	Label       string     `json:"label"`
	Status      string     `json:"status"`
	BarCount    int        `json:"bar_count"`
	MergedCount int        `json:"merged_count"`
	AsOf        string     `json:"as_of"`
	LastClose   *float64   `json:"last_close"`
	Position    Position   `json:"position"`
	Direction   string     `json:"direction"`
	Structure   string     `json:"structure"`
	Candles     []Bar      `json:"candles"`
	Fractals    []Fractal  `json:"fractals"`
	Strokes     []Stroke   `json:"strokes"`
	Centers     []Center   `json:"centers"`
	Signals     []Signal   `json:"signals"`
	Scenarios   []Scenario `json:"scenarios"`
	Warnings    []string   `json:"warnings"`
}

type Summary struct {
	Alignment string `json:"alignment"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
}

type Result struct {
	Symbol      string    `json:"symbol"`
	Spot        *float64  `json:"spot"`
	GeneratedAt time.Time `json:"generated_at"`
	RuleVersion string    `json:"rule_version"`
	Rules       []string  `json:"rules"`
	Summary     Summary   `json:"summary"`
	Daily       Frame     `json:"daily"`
	Weekly      Frame     `json:"weekly"`
	Warnings    []string  `json:"warnings"`
}
