// Package chananalysis implements a documented, conservative pen-based interpretation
// of Chan price structure. Timeframes are chart periods, not recursive levels.
package chananalysis

import "time"

const RuleVersion = "strict-old-bi-v3"
const RecursiveRuleVersion = "chan-recursive-v4.0.0"
const RecursivePreviewVersion = "chan-recursive-v4-preview.11"

type Bar struct {
	// CapturedAt keeps a daily candle fetched before its session close from
	// becoming confirmed merely because the clock has advanced.
	CapturedAt       time.Time `json:"-"`
	Date             string    `json:"date"`
	Open             float64   `json:"open"`
	High             float64   `json:"high"`
	Low              float64   `json:"low"`
	Close            float64   `json:"close"`
	Volume           float64   `json:"volume"`
	RawOpen          float64   `json:"raw_open,omitempty"`
	RawHigh          float64   `json:"raw_high,omitempty"`
	RawLow           float64   `json:"raw_low,omitempty"`
	RawClose         float64   `json:"raw_close,omitempty"`
	AdjustmentFactor float64   `json:"adjustment_factor,omitempty"`
	Chain            int       `json:"chain_id,omitempty"`
	SourceIndex      int       `json:"-"`
	HasSourceIndex   bool      `json:"-"`
}

type DataGap struct {
	ExpectedDate string `json:"expected_date"`
	NextDate     string `json:"next_date"`
}

type DataChain struct {
	ID           int    `json:"chain_id"`
	Start        string `json:"start"`
	End          string `json:"end"`
	StartReason  string `json:"start_reason"`
	ExpectedDate string `json:"expected_date,omitempty"`
}

type StructureChain struct {
	ID          int    `json:"chain_id"`
	DataChainID int    `json:"data_chain_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	StartReason string `json:"start_reason"`
}

type MergedCandle struct {
	High           float64 `json:"high"`
	Low            float64 `json:"low"`
	HighRawBar     int     `json:"high_raw_bar"`
	LowRawBar      int     `json:"low_raw_bar"`
	RawStart       int     `json:"raw_start"`
	RawEnd         int     `json:"raw_end"`
	SourceRawStart int     `json:"source_raw_start"`
	SourceRawEnd   int     `json:"source_raw_end"`
	Chain          int     `json:"chain_id"`
}

type ExclusionCounts struct {
	InvalidDate   int
	NonTrading    int
	InvalidOHLC   int
	InvalidVolume int
}

type WeeklyExclusionCounts struct {
	OpenWeek        int `json:"open_week"`
	MissingSessions int `json:"missing_sessions"`
	BrokenChain     int `json:"broken_chain"`
	TruncatedStart  int `json:"truncated_start"`
}

// CorporateActionEvent 记录原始价格跳变或供应商复权因子变化。Kind 为行情推断结果，不能替代发行方公司行动记录。
type CorporateActionEvent struct {
	Date                       string  `json:"date"`
	Kind                       string  `json:"kind"`
	Status                     string  `json:"status"`
	RawCloseChangePercent      float64 `json:"raw_close_change_percent"`
	AdjustedCloseChangePercent float64 `json:"adjusted_close_change_percent"`
	FactorChangePercent        float64 `json:"factor_change_percent"`
	FactorVerified             bool    `json:"factor_verified"`
}

// DataQuality describes the validated input used for one timeframe. PriceBasis
// is explicit so callers cannot mistake the legacy raw-price analysis for the
// adjusted-price input required by the recursive algorithm.
type DataQuality struct {
	InputBars                int                    `json:"input_bars"`
	AcceptedBars             int                    `json:"accepted_bars"`
	RejectedBars             int                    `json:"rejected_bars"`
	IncompleteBars           int                    `json:"incomplete_bars"`
	SourceInputBars          int                    `json:"source_input_bars,omitempty"`
	SourceAcceptedBars       int                    `json:"source_accepted_bars,omitempty"`
	SourceRejectedBars       int                    `json:"source_rejected_bars,omitempty"`
	SourceIncompleteBars     int                    `json:"source_incomplete_bars,omitempty"`
	SourceDuplicateBars      int                    `json:"source_duplicate_bars,omitempty"`
	SourceReorderedBars      int                    `json:"source_reordered_bars,omitempty"`
	DuplicateBars            int                    `json:"duplicate_bars"`
	ReorderedBars            int                    `json:"reordered_bars"`
	InvalidDateBars          int                    `json:"invalid_date_bars"`
	NonTradingBars           int                    `json:"non_trading_bars"`
	InvalidOHLCBars          int                    `json:"invalid_ohlc_bars"`
	InvalidVolumeBars        int                    `json:"invalid_volume_bars"`
	IncompleteWeeks          int                    `json:"incomplete_weeks"`
	WeeklyExclusions         WeeklyExclusionCounts  `json:"weekly_exclusions"`
	Gaps                     []DataGap              `json:"gaps"`
	Chains                   []DataChain            `json:"chains"`
	PriceBasis               string                 `json:"price_basis"`
	CorporateActionSuspected bool                   `json:"corporate_action_suspected"`
	CorporateActionEvents    []CorporateActionEvent `json:"corporate_action_events"`
	Status                   string                 `json:"status"`
}

type Fractal struct {
	Kind            string  `json:"kind"`
	Date            string  `json:"date"`
	Price           float64 `json:"price"`
	Level           int     `json:"level"`
	FirstObservedAt string  `json:"first_observed_at"`
	ConfirmedAt     string  `json:"confirmed_at"`
	MergedIndex     int     `json:"merged_index"`
	RawIndex        int     `json:"raw_index"`
	RawStartIndex   int     `json:"raw_start_index"`
	RawEndIndex     int     `json:"raw_end_index"`
	SourceRawIndex  int     `json:"source_raw_index"`
	SourceRawStart  int     `json:"source_raw_start"`
	SourceRawEnd    int     `json:"source_raw_end"`
	Chain           int     `json:"chain_id"`
}

type Stroke struct {
	Direction     string  `json:"direction"`
	Chain         int     `json:"chain,omitempty"`
	Start         Fractal `json:"start"`
	End           Fractal `json:"end"`
	Low           float64 `json:"low"`
	High          float64 `json:"high"`
	Confirmed     bool    `json:"confirmed"`
	ConfirmedAt   string  `json:"confirmed_at,omitempty"`
	MACDArea      float64 `json:"macd_area"`
	Level         int     `json:"level"`
	StartBar      int     `json:"start_bar"`
	EndBar        int     `json:"end_bar"`
	StartRawBar   int     `json:"start_raw_bar"`
	EndRawBar     int     `json:"end_raw_bar"`
	InputStartBar int     `json:"input_start_bar"`
	InputEndBar   int     `json:"input_end_bar"`
}

type Center struct {
	Level       int     `json:"level"`
	Kind        string  `json:"kind"`
	InputKind   string  `json:"input_kind"`
	Start       string  `json:"start"`
	Chain       int     `json:"chain,omitempty"`
	End         string  `json:"end"`
	Lower       float64 `json:"lower"`
	Upper       float64 `json:"upper"`
	ConfirmedAt string  `json:"confirmed_at"`
	StartStroke int     `json:"start_stroke"`
	EndStroke   int     `json:"end_stroke"`
	StrokeCount int     `json:"stroke_count"`
}

type FeatureElement struct {
	StartStroke int     `json:"start_stroke"`
	EndStroke   int     `json:"end_stroke"`
	Low         float64 `json:"low"`
	High        float64 `json:"high"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
}

type Segment struct {
	Direction      string           `json:"direction"`
	TrendKind      string           `json:"trend_kind,omitempty"`
	Chain          int              `json:"chain_id"`
	Level          int              `json:"level"`
	StartStroke    int              `json:"start_stroke"`
	EndStroke      int              `json:"end_stroke"`
	EndPointStroke int              `json:"endpoint_stroke"`
	Start          string           `json:"start"`
	End            string           `json:"end"`
	StartKind      string           `json:"start_kind"`
	EndKind        string           `json:"end_kind"`
	Low            float64          `json:"low"`
	High           float64          `json:"high"`
	StartPrice     float64          `json:"start_price"`
	EndPrice       float64          `json:"end_price"`
	StartRawBar    int              `json:"start_raw_bar"`
	EndRawBar      int              `json:"end_raw_bar"`
	StartInputBar  int              `json:"start_input_bar"`
	EndInputBar    int              `json:"end_input_bar"`
	Confirmed      bool             `json:"confirmed"`
	ConfirmedAt    string           `json:"confirmed_at,omitempty"`
	Forming        bool             `json:"forming"`
	BreakStroke    int              `json:"break_stroke,omitempty"`
	ConfirmStroke  int              `json:"confirm_stroke,omitempty"`
	Feature        []FeatureElement `json:"feature_sequence"`
}

type RecursiveCenter struct {
	Level       int     `json:"level"`
	Kind        string  `json:"kind"`
	InputKind   string  `json:"input_kind"`
	Chain       int     `json:"chain_id"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Lower       float64 `json:"zd"`
	Upper       float64 `json:"zg"`
	FormedAt    string  `json:"formed_at"`
	ConfirmedAt string  `json:"confirmed_at"`
	Forming     bool    `json:"forming"`
	ClosedAt    string  `json:"closed_at,omitempty"`
	// StartSegment and EndSegment index the segments consumed by the level that
	// form or extend this center. OwnerMoveIndex identifies the higher-level
	// move that contains it; that is the index to use when searching for later
	// moves at the center's own recursive level.
	StartSegment       int    `json:"start_segment"`
	EndSegment         int    `json:"end_segment"`
	OwnerMoveIndex     int    `json:"owner_move_index"`
	ExtensionCount     int    `json:"extension_count"`
	RelationToPrevious string `json:"relation_to_previous,omitempty"`
	RelatedCenter      *int   `json:"related_center,omitempty"`
	Relation           string `json:"relation,omitempty"`
	CenterID           string `json:"center_id"`
	PreviousCenterID   string `json:"previous_center_id,omitempty"`
}

type LevelAnalysis struct {
	Level      int               `json:"level"`
	Status     string            `json:"status"`
	InputCount int               `json:"input_count"`
	Strokes    []Stroke          `json:"strokes"`
	Segments   []Segment         `json:"segments"`
	Trends     []RecursiveTrend  `json:"trends,omitempty"`
	Centers    []RecursiveCenter `json:"centers"`
	WaitingFor string            `json:"waiting_for,omitempty"`
}

// RecursiveTrend is a higher-level move produced from a contiguous sequence
// of lower-level segments. Its center ranges refer to the input level, while
// the trend's price and time bounds cover all input segments in its span.
type RecursiveTrend struct {
	Level         int      `json:"level"`
	Chain         int      `json:"chain_id"`
	Kind          string   `json:"kind"`
	Direction     string   `json:"direction"`
	Start         string   `json:"start"`
	End           string   `json:"end"`
	StartPrice    float64  `json:"start_price"`
	EndPrice      float64  `json:"end_price"`
	Low           float64  `json:"low"`
	High          float64  `json:"high"`
	Confirmed     bool     `json:"confirmed"`
	ConfirmedAt   string   `json:"confirmed_at,omitempty"`
	Forming       bool     `json:"forming"`
	StartInput    int      `json:"start_input"`
	EndInput      int      `json:"end_input"`
	StartRawBar   int      `json:"start_raw_bar"`
	EndRawBar     int      `json:"end_raw_bar"`
	StartInputBar int      `json:"start_input_bar"`
	EndInputBar   int      `json:"end_input_bar"`
	CenterIDs     []string `json:"center_ids,omitempty"`
}

type Strength struct {
	Direction      string  `json:"direction"`
	Level          int     `json:"level"`
	Start          string  `json:"start"`
	End            string  `json:"end"`
	ConfirmedAt    string  `json:"confirmed_at"`
	MACDArea       float64 `json:"macd_area"`
	MACDVersion    string  `json:"macd_version"`
	WarmupBars     int     `json:"warmup_bars"`
	WarmupComplete bool    `json:"warmup_complete"`
	SlopePercent   float64 `json:"slope_percent_per_bar"`
	DurationBars   int     `json:"duration_bars"`
	Amplitude      float64 `json:"amplitude_percent"`
	StartIndex     int     `json:"start_index"`
	EndIndex       int     `json:"end_index"`
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

type V4Signal struct {
	SignalID              string                 `json:"signal_id,omitempty"`
	Symbol                string                 `json:"symbol"`
	Level                 int                    `json:"level"`
	Kind                  string                 `json:"kind"`
	Side                  string                 `json:"side"`
	Status                string                 `json:"status"`
	Phase                 string                 `json:"phase"`
	PhaseAt               string                 `json:"phase_at"`
	PhaseHistory          []SignalPhase          `json:"phase_history"`
	RuleVersion           string                 `json:"rule_version"`
	Date                  string                 `json:"date"`
	ObservedAt            string                 `json:"observed_at"`
	ConfirmedAt           string                 `json:"confirmed_at,omitempty"`
	StructureRefs         []int                  `json:"structure_refs"`
	StructureIDs          []string               `json:"structure_ids,omitempty"`
	RelatedSignalID       string                 `json:"related_signal_id,omitempty"`
	RelatedCenterID       string                 `json:"related_center_id,omitempty"`
	Conditions            []string               `json:"conditions"`
	Missing               []string               `json:"missing_conditions"`
	Invalidation          *float64               `json:"invalidation_price,omitempty"`
	StrengthRatio         *float64               `json:"strength_ratio,omitempty"`
	Price                 *float64               `json:"price,omitempty"`
	MarketFilter          string                 `json:"market_filter"`
	MarketEnvironment     *MarketEnvironment     `json:"market_environment,omitempty"`
	LowerLevelStatus      string                 `json:"lower_level_status"`
	TimeframeConfirmation *TimeframeConfirmation `json:"timeframe_confirmation,omitempty"`
	Detail                string                 `json:"detail"`
}

// TimeframeConfirmation records whether a higher-timeframe signal has a
// directionally aligned, confirmed structure in its mapped lower timeframe.
// It is independent of LowerLevelStatus, which describes recursive levels
// calculated inside the same chart timeframe.
type TimeframeConfirmation struct {
	HigherTimeframe string   `json:"higher_timeframe"`
	LowerTimeframe  string   `json:"lower_timeframe"`
	Status          string   `json:"status"`
	StructureIDs    []string `json:"structure_ids"`
	ConfirmedAt     string   `json:"confirmed_at,omitempty"`
	Reason          string   `json:"reason"`
}

// TimeframeMapping documents the direct timeframe relationship for this
// frame. A frame can exist without the mapped neighbor being available.
type TimeframeMapping struct {
	MappingVersion       string `json:"mapping_version"`
	HigherTimeframe      string `json:"higher_timeframe,omitempty"`
	LowerTimeframe       string `json:"lower_timeframe,omitempty"`
	Role                 string `json:"role"`
	LowerTimeframeStatus string `json:"lower_timeframe_status"`
	Reason               string `json:"reason"`
}

type SignalPhase struct {
	Phase string `json:"phase"`
	At    string `json:"at"`
}

type TrendContext struct {
	Status          string `json:"status"`
	Direction       string `json:"direction"`
	Level           int    `json:"level"`
	AsOf            string `json:"as_of"`
	CenterID        string `json:"center_id,omitempty"`
	CenterRelation  string `json:"center_relation,omitempty"`
	CenterPosition  string `json:"center_position"`
	DepartureStatus string `json:"departure_status"`
	Reason          string `json:"reason"`
}

type Scenario struct {
	Direction    string `json:"direction"`
	Label        string `json:"label"`
	Condition    string `json:"condition"`
	Expectation  string `json:"expectation"`
	Invalidation string `json:"invalidation"`
}

type Frame struct {
	Period             string              `json:"period"`
	Timeframe          string              `json:"timeframe"`
	TimeframeMapping   TimeframeMapping    `json:"timeframe_mapping"`
	Label              string              `json:"label"`
	Status             string              `json:"status"`
	BarCount           int                 `json:"bar_count"`
	MergedCount        int                 `json:"merged_count"`
	MergedCandles      []MergedCandle      `json:"merged_candles"`
	AsOf               string              `json:"as_of"`
	LastClose          *float64            `json:"last_close"`
	Position           Position            `json:"position"`
	Direction          string              `json:"direction"`
	Structure          string              `json:"structure"`
	Candles            []Bar               `json:"candles"`
	Fractals           []Fractal           `json:"fractals"`
	Strokes            []Stroke            `json:"strokes"`
	Centers            []Center            `json:"centers"`
	Signals            []Signal            `json:"signals"`
	PenSignals         []Signal            `json:"pen_signals"`
	Scenarios          []Scenario          `json:"scenarios"`
	Warnings           []string            `json:"warnings"`
	Quality            DataQuality         `json:"quality"`
	MarketEnvironment  *MarketEnvironment  `json:"market_environment,omitempty"`
	Level              int                 `json:"level"`
	Segments           []Segment           `json:"segments"`
	RecursiveCenters   []RecursiveCenter   `json:"recursive_centers"`
	RecursiveLevels    []LevelAnalysis     `json:"recursive_levels"`
	FractalsV4         []Fractal           `json:"fractals_v4"`
	StrokesV4          []Stroke            `json:"strokes_v4"`
	Strengths          []Strength          `json:"strengths_v4"`
	SignalsV4          []V4Signal          `json:"signals_v4"`
	TrendState         string              `json:"trend_state_v4"`
	TrendContext       TrendContext        `json:"trend_context_v4"`
	MACD               []MACDPoint         `json:"macd_v4"`
	StructureChains    []StructureChain    `json:"structure_chains_v4"`
	StructureRevisions []StructureRevision `json:"structure_revisions_v4,omitempty"`
}

type StructureRevision struct {
	ID                    int64                `json:"id"`
	RuleVersion           string               `json:"rule_version"`
	Reason                string               `json:"reason"`
	PreviousRuleVersion   string               `json:"previous_rule_version"`
	InputFingerprint      string               `json:"input_fingerprint"`
	ChangedBars           []StructureBarChange `json:"changed_bars"`
	ChangedConfirmedItems []StructureChange    `json:"changed_confirmed_structures"`
	CreatedAt             time.Time            `json:"created_at"`
}

type StructureBarChange struct {
	Date           string `json:"date"`
	OldFingerprint string `json:"old_fingerprint,omitempty"`
	NewFingerprint string `json:"new_fingerprint,omitempty"`
}

type StructureChange struct {
	ID     string `json:"id"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

type Summary struct {
	Alignment string `json:"alignment"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
}

type Result struct {
	Symbol              string              `json:"symbol"`
	Spot                *float64            `json:"spot"`
	GeneratedAt         time.Time           `json:"generated_at"`
	RuleVersion         string              `json:"rule_version"`
	LegacyRuleVersion   string              `json:"legacy_rule_version,omitempty"`
	Parameters          map[string]any      `json:"parameters,omitempty"`
	Rules               []string            `json:"rules"`
	Summary             Summary             `json:"summary"`
	AlgorithmComparison AlgorithmComparison `json:"algorithm_comparison,omitempty"`
	Daily               Frame               `json:"daily"`
	Weekly              Frame               `json:"weekly"`
	Warnings            []string            `json:"warnings"`
}
