package chananalysis

import (
	"fmt"
	"math"
	"sort"
	"time"

	"option-scope-go/internal/marketcalendar"
)

type mergedBar struct {
	high, low              float64
	highIndex, lowIndex    int
	start, end             int
	sourceHigh, sourceLow  int
	sourceStart, sourceEnd int
	chain                  int
}

func directionFromPair(previousHigh, previousLow, nextHigh, nextLow float64) int {
	if nextHigh > previousHigh && nextLow > previousLow {
		return 1
	}
	if nextHigh < previousHigh && nextLow < previousLow {
		return -1
	}
	if nextHigh == previousHigh {
		if nextLow > previousLow {
			return 1
		}
		if nextLow < previousLow {
			return -1
		}
	}
	if nextLow == previousLow {
		if nextHigh > previousHigh {
			return 1
		}
		if nextHigh < previousHigh {
			return -1
		}
	}
	return 0
}

// Analyze uses completed regular-session daily bars only. The last pen remains
// provisional until a valid opposite pen fixes its endpoint. A pen center is
// formed only from confirmed pens; it is not a recursively defined Chan level.
func Analyze(symbol string, input []Bar, spot *float64, cutoff time.Time) Result {
	return analyzeWithComponents(symbol, input, spot, cutoff, nil)
}

func analyzeWithComponents(symbol string, input []Bar, spot *float64, cutoff time.Time, cached *incrementalComponents) Result {
	bars, rejected, openBars, duplicates, reordered, exclusions := completedBarsDetailed(input, cutoff)
	return analyzeValidatedWithComponents(symbol, input, bars, rejected, openBars, duplicates, reordered, exclusions, spot, cutoff, cached)
}

// analyzeValidatedWithComponents runs the shared analysis pipeline when the
// caller already has a validated, chronological bar slice. The input slice is
// retained for quality counters; bars contains only accepted completed bars.
func analyzeValidatedWithComponents(symbol string, input, bars []Bar, rejected, openBars, duplicates, reordered int, exclusions ExclusionCounts, spot *float64, cutoff time.Time, cached *incrementalComponents) Result {
	validatedBars := bars
	result := Result{
		Symbol: symbol, Spot: spot, GeneratedAt: cutoff.UTC(), RuleVersion: RuleVersion,
		Rules: []string{
			"包含关系按已知方向合并；初始方向未知时保留并集；相等极值不构成严格分型",
			"严格旧笔：顶底分型中心至少间隔 4 根合并 K 线，端点必须是区间极值",
			"分型右侧 K 线结束合并后确认；末笔等待有效反向笔固定端点",
			"历史交易日缺口仅分析最近连续行情；无法连续接续的笔链会分段保留历史笔与中枢，当前位置只按最新笔链判断",
			"笔中枢：三根连续已确认笔的价格区间有严格重叠；延伸保留初始边界",
			"本项目的笔级观察口径：围绕同一已形成笔中枢的相邻同向笔创新低或新高且越过中枢外沿，MACD(12,26,9) 同向柱面积低于前一同向笔 80% 时，输出底部或顶部力度衰减观察；80% 为筛选阈值",
			"力度观察至少需要 60 根已收盘 K 线，前一比较笔起点避开前 35 根预热区；仅检查最近四根笔，各侧保留最近一条观察",
			"末笔衰减仅为形成中观察；端点被反向笔固定后列为抄底或逃顶候选；首次回踩不破原低点或反抽不过原高点时，列出回试形态候选",
			"候选失效边界为原始极值，触及后失效；笔力度衰减与首次回试不等同于完整递归级别的一类或二类买卖点，不输出未经回测的胜率",
			"日线与周线表示图表周期；本版不输出递归级别、线段及一二类买卖点",
		}, Warnings: []string{},
	}
	if rejected > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 根日期或 OHLC 无效的 K 线已排除；缺失行情可能影响结构", rejected))
	}
	if openBars > 0 {
		result.Warnings = append(result.Warnings, "未收盘、盘中抓取尚未更新或未来日期的 K 线未用于结构确认")
	}
	bars, chainWarnings := chainDailyBars(bars)
	result.Warnings = append(result.Warnings, chainWarnings...)
	if spot == nil || !finitePositive(*spot) {
		spot = nil
		if len(bars) > 0 {
			spot = ptr(bars[len(bars)-1].Close)
		}
		result.Spot = spot
	}
	if cached != nil && len(cached.merged) > 0 {
		result.Daily = analyzeFrameWithComponents(bars, "1d", "日线", spot, cached.merged, cached.fractals, histogramFromMACD(cached.macd))
	} else {
		result.Daily = analyzeFrame(bars, "1d", "日线", spot)
	}
	result.Daily.Quality = dailyQuality(input, validatedBars, rejected, openBars, duplicates, reordered)
	result.Daily.Quality.Chains = summarizeDataChains(bars, result.Daily.Quality.Gaps)
	result.Daily.Quality.InvalidDateBars = exclusions.InvalidDate
	result.Daily.Quality.NonTradingBars = exclusions.NonTrading
	result.Daily.Quality.InvalidOHLCBars = exclusions.InvalidOHLC
	result.Daily.Quality.InvalidVolumeBars = exclusions.InvalidVolume
	if duplicates > 0 || reordered > 0 {
		result.Daily.Warnings = append(result.Daily.Warnings, fmt.Sprintf("输入中有 %d 根重复日期及 %d 根日期乱序行情，数据质量已降级", duplicates, reordered))
	}
	if len(result.Daily.Quality.Gaps) > 0 {
		result.Daily.Warnings = append(result.Daily.Warnings, fmt.Sprintf("检测到 %d 处预期交易日缺口，结构链不会跨缺口连接", len(result.Daily.Quality.Gaps)))
	}
	if result.Daily.Quality.CorporateActionSuspected {
		result.Daily.Warnings = append(result.Daily.Warnings, "检测到无法由相邻行情与复权因子解释的显著价格/因子跳变；数据已降级并在断点处分链，需核验拆股、分红及源行情")
	}
	weekly, partial, incompleteWeeks, inputWeeks, weeklyExclusions := weeklyBarsQualityDetailed(bars, cutoff)
	result.Weekly = analyzeFrame(weekly, "1wk", "周线", spot)
	result.Weekly.Quality = weeklyDataQuality(result.Daily.Quality, inputWeeks, len(input), len(validatedBars), weekly, incompleteWeeks, weeklyExclusions)
	if partial {
		result.Weekly.Warnings = append(result.Weekly.Warnings, weeklyExclusionWarning(incompleteWeeks, weeklyExclusions))
	}
	result.Summary = frameSummary(result.Daily, result.Weekly)
	return result
}

// AnalyzeAdjusted keeps legacy v3 fields intact and adds the independent v4
// recursive output generated from split/dividend-adjusted OHLC.
func AnalyzeAdjusted(symbol string, input []Bar, adjustedSpot *float64, cutoff time.Time) Result {
	return analyzeAdjustedWithComponents(symbol, input, adjustedSpot, cutoff, recursiveDivergenceRatio, nil)
}

// analyzeAdjustedWithDivergenceRatio is used by historical calibration tools.
// Production APIs always call AnalyzeAdjusted and therefore use the versioned
// default parameter.
func analyzeAdjustedWithDivergenceRatio(symbol string, input []Bar, adjustedSpot *float64, cutoff time.Time, divergenceRatio float64) Result {
	return analyzeAdjustedWithComponents(symbol, input, adjustedSpot, cutoff, divergenceRatio, nil)
}

func analyzeAdjustedWithComponents(symbol string, input []Bar, adjustedSpot *float64, cutoff time.Time, divergenceRatio float64, cached *incrementalComponents) Result {
	validated, adjustedRejected, adjustedIncomplete, adjustedDuplicates, adjustedReordered, adjustedExclusions := validatedAdjustedBars(input, cutoff)
	return analyzeAdjustedValidatedWithComponents(symbol, input, validated, adjustedRejected, adjustedIncomplete, adjustedDuplicates, adjustedReordered, adjustedExclusions, adjustedSpot, cutoff, divergenceRatio, cached)
}

// analyzeAdjustedPrevalidatedWithComponents is used by the append-only
// analyzer after validating each bar at ingestion. It avoids rescanning and
// resorting the full history on every append.
func analyzeAdjustedPrevalidatedWithComponents(symbol string, bars []Bar, adjustedSpot *float64, cutoff time.Time, divergenceRatio float64, cached *incrementalComponents) Result {
	return analyzeAdjustedValidatedWithComponents(symbol, bars, bars, 0, 0, 0, 0, ExclusionCounts{}, adjustedSpot, cutoff, divergenceRatio, cached)
}

func analyzeAdjustedValidatedWithComponents(symbol string, input, validated []Bar, adjustedRejected, adjustedIncomplete, adjustedDuplicates, adjustedReordered int, adjustedExclusions ExclusionCounts, adjustedSpot *float64, cutoff time.Time, divergenceRatio float64, cached *incrementalComponents) Result {
	legacyResult := analyzeValidatedWithComponents(symbol, validated, validated, 0, 0, 0, 0, ExclusionCounts{}, adjustedSpot, cutoff, cached)
	result := legacyResult
	result.LegacyRuleVersion = RuleVersion
	result.RuleVersion = RecursivePreviewVersion
	result.Rules = append(result.Rules, "递归 v4 字段使用独立缓存的拆股/分红自动复权 OHLC；旧 strokes/centers 字段仍采用 strict-old-bi-v3 语义")
	result.Parameters = RecursiveParameters()
	result.Parameters["divergence_ratio"] = divergenceRatio
	dailyBars := result.Daily.Candles
	weeklyBars := result.Weekly.Candles
	// The legacy pass already scanned these accepted bars for gaps and
	// corporate-action events; only the counters and status differ for the
	// adjusted input, so reuse that scan instead of walking the history twice.
	dailyQuality := legacyResult.Daily.Quality
	dailyQuality.InputBars = len(input)
	dailyQuality.AcceptedBars = len(dailyBars)
	dailyQuality.RejectedBars = adjustedRejected
	dailyQuality.IncompleteBars = adjustedIncomplete
	dailyQuality.DuplicateBars = adjustedDuplicates
	dailyQuality.ReorderedBars = adjustedReordered
	dailyQuality.InvalidDateBars = adjustedExclusions.InvalidDate
	dailyQuality.NonTradingBars = adjustedExclusions.NonTrading
	dailyQuality.InvalidOHLCBars = adjustedExclusions.InvalidOHLC
	dailyQuality.InvalidVolumeBars = adjustedExclusions.InvalidVolume
	dailyQuality.Status = qualityStatus(adjustedRejected, adjustedIncomplete, adjustedDuplicates)
	if dailyQuality.CorporateActionSuspected {
		dailyQuality.Status = "degraded"
	}
	if dailyQuality.Status == "ok" && len(dailyQuality.Gaps) > 0 {
		dailyQuality.Status = "partial"
	}
	if adjustedReordered > 0 {
		dailyQuality.Status = "degraded"
	}
	result.Daily = addRecursiveAnalysisWithComponents(result.Daily, dailyBars, divergenceRatio, cached)
	for index := range result.Daily.SignalsV4 {
		result.Daily.SignalsV4[index].Symbol = symbol
	}
	result.Daily.Quality = dailyQuality
	result.Weekly = addRecursiveAnalysisWithDivergenceRatio(result.Weekly, weeklyBars, divergenceRatio)
	for index := range result.Weekly.SignalsV4 {
		result.Weekly.SignalsV4[index].Symbol = symbol
	}
	weeklyQuality := dailyQuality
	_, partialWeeks, incompleteWeeks, inputWeeks, weeklyExclusions := weeklyBarsQualityDetailed(dailyBars, cutoff)
	weeklyQuality = weeklyDataQuality(dailyQuality, inputWeeks, len(input), len(dailyBars), weeklyBars, incompleteWeeks, weeklyExclusions)
	if partialWeeks && weeklyQuality.Status == "ok" {
		weeklyQuality.Status = "partial"
	}
	result.Weekly.Quality = weeklyQuality
	result.Daily.Quality.PriceBasis = "split_dividend_adjusted"
	result.Weekly.Quality.PriceBasis = "split_dividend_adjusted"
	// Daily bars are the direct lower timeframe for the weekly frame. Daily
	// signals need 4h confirmation; this adjusted-history endpoint does not
	// provide a price-coordinate-compatible 4h frame, so keep them explicitly
	// waiting instead of treating the chart periods as interchangeable.
	setTimeframeMapping(&result.Daily, false)
	AttachMappedTimeframe(&result.Weekly, result.Daily)
	if partialWeeks {
		result.Weekly.Warnings = appendWarningOnce(result.Weekly.Warnings, weeklyExclusionWarning(incompleteWeeks, weeklyExclusions))
	}
	if adjustedRejected > 0 {
		warning := fmt.Sprintf("%d 根复权因子或 OHLC 配对无效的 K 线已排除；递归结构已在数据缺口处分链", adjustedRejected)
		result.Daily.Warnings = append(result.Daily.Warnings, warning)
		result.Weekly.Warnings = append(result.Weekly.Warnings, warning)
	}
	if adjustedIncomplete > 0 {
		warning := fmt.Sprintf("%d 根未收盘或未来日期的复权 K 线未用于递归结构", adjustedIncomplete)
		result.Daily.Warnings = append(result.Daily.Warnings, warning)
		result.Weekly.Warnings = append(result.Weekly.Warnings, warning)
	}
	result.AlgorithmComparison = compareAdjustedAlgorithms(legacyResult, result)
	return result
}

func weeklyExclusionWarning(incompleteWeeks int, exclusions WeeklyExclusionCounts) string {
	return fmt.Sprintf("%d 个不完整交易周未用于结构确认（当前周未收盘 %d，缺少预期交易日日线 %d，周内数据链断开 %d，历史起点位于周中 %d）",
		incompleteWeeks, exclusions.OpenWeek, exclusions.MissingSessions, exclusions.BrokenChain, exclusions.TruncatedStart)
}

func appendWarningOnce(warnings []string, warning string) []string {
	for _, current := range warnings {
		if current == warning {
			return warnings
		}
	}
	return append(warnings, warning)
}

func validatedAdjustedBars(input []Bar, cutoff time.Time) ([]Bar, int, int, int, int, ExclusionCounts) {
	bars, rejected, incomplete, duplicates, reordered, exclusions := completedBarsDetailed(input, cutoff)
	accepted := make([]Bar, 0, len(bars))
	for _, bar := range bars {
		if !validAdjustedPair(bar) {
			rejected++
			continue
		}
		accepted = append(accepted, bar)
	}
	return accepted, rejected, incomplete, duplicates, reordered, exclusions
}

func validAdjustedPair(bar Bar) bool {
	if !finitePositive(bar.AdjustmentFactor) {
		return false
	}
	adjusted := []float64{bar.Open, bar.High, bar.Low, bar.Close}
	raw := []float64{bar.RawOpen, bar.RawHigh, bar.RawLow, bar.RawClose}
	for index, value := range raw {
		if !finitePositive(value) {
			return false
		}
		expected := value * bar.AdjustmentFactor
		if math.Abs(adjusted[index]-expected) > 1e-6*math.Max(1, adjusted[index]) {
			return false
		}
	}
	return true
}

func RecursiveParameters() map[string]any {
	return map[string]any{
		"rule_version": RecursivePreviewVersion, "target_rule_version": RecursiveRuleVersion, "implementation_status": "preview", "timeframes": []string{"15m", "1h", "2h", "4h", "1d", "1wk"},
		"timeframe_mapping": map[string]any{
			"version":   timeframeMappingVersion,
			"ladder":    []string{"15m->1h", "1h->2h", "2h->4h", "4h->1d", "1d->1wk"},
			"semantics": "each chart timeframe computes its own recursive Chan levels; signal confirmation uses only confirmed level-0 structures from the direct lower timeframe",
		},
		"minimum_stroke_merged_bars": 4, "macd": map[string]any{"fast": 12, "slow": 26, "signal": 9, "warmup_bars": macdWarmupBars, "version": macdVersion},
		"divergence_ratio": recursiveDivergenceRatio, "divergence_threshold_source": "project-screening-parameter", "gap_policy": "split-chain", "adjustment": "split_dividend_auto",
	}
}

// AnalyzeIntraday builds a closed-bar intraday structure from Yahoo's
// 15-minute regular-session bars. Larger intervals are session anchored and
// incomplete terminal intervals are omitted from analysis.
func AnalyzeIntraday(symbol string, input []Bar, period string, spot *float64, cutoff time.Time) (Result, error) {
	interval, label := 15*time.Minute, "15分钟"
	source := prepareIntradaySource(input, cutoff)
	bars := source.bars
	var gaps []DataGap
	quality := DataQuality{InputBars: len(input), AcceptedBars: len(bars), RejectedBars: source.rejected, IncompleteBars: source.unfinished, DuplicateBars: source.duplicates, PriceBasis: "raw", Status: qualityStatus(source.rejected, source.unfinished, source.duplicates)}
	switch period {
	case "15m":
		gaps = mergeGaps(source.gaps, expectedIntradayGaps(bars, interval, cutoff))
		quality.AcceptedBars = len(bars)
		quality.Gaps = gaps
		if len(gaps) > 0 && quality.Status == "ok" {
			quality.Status = "partial"
		}
	case "4h":
		interval = intradayPeriodDuration(period)
		label = intradayPeriodLabel(period)
		bars, quality, gaps = aggregateFourHourBarsFromSource(len(input), source.bars, source.rejected, source.unfinished, source.duplicates)
	case "30m", "60m", "1h", "2h":
		interval = intradayPeriodDuration(period)
		label = intradayPeriodLabel(period)
		bars, quality, gaps = aggregateIntradayBarsFromSource(len(input), source, interval)
	default:
		return Result{}, fmt.Errorf("unsupported intraday period %q", period)
	}
	// A missing, zero, or non-finite quote falls back to the last closed bar so
	// the position is never judged against an invalid price.
	if spot == nil || !finitePositive(*spot) {
		spot = nil
		if len(bars) > 0 {
			spot = ptr(bars[len(bars)-1].Close)
		}
	}
	result := Result{Symbol: symbol, Spot: spot, GeneratedAt: cutoff.UTC(), RuleVersion: RuleVersion,
		Rules: []string{
			"只使用已收盘的美东常规交易时段 K 线；盘前、盘后和未完成 K 线不参与结构确认",
			"严格旧笔、分型与笔中枢使用与日线相同的结构规则；盘中历史可能较短",
		}, Warnings: []string{},
	}
	if source.rejected > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 根日期、时段或 OHLC 无效的盘中 K 线已排除", source.rejected))
	}
	if source.unfinished > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 根尚未收盘或所属时段未完整的 K 线已排除", source.unfinished))
	}
	if len(bars) == 0 {
		result.Daily = analyzeFrame(nil, period, label, spot)
		result.Daily.Warnings = append(result.Daily.Warnings, "上游尚无完整的已收盘盘中 K 线")
	} else {
		result.Daily = analyzeFrame(bars, period, label, spot)
	}
	result.Daily.Quality = quality
	result.Daily.Quality.Chains = summarizeDataChains(bars, gaps)
	if len(result.Daily.Quality.Gaps) > 0 {
		result.Daily.Warnings = append(result.Daily.Warnings, fmt.Sprintf("盘中行情存在 %d 处时间缺口", len(result.Daily.Quality.Gaps)))
	}
	result.Daily.Warnings = append(result.Daily.Warnings, result.Warnings...)
	result.LegacyRuleVersion = RuleVersion
	result.RuleVersion = RecursivePreviewVersion
	result.Parameters = RecursiveParameters()
	result.Daily = addRecursiveAnalysis(result.Daily, bars)
	if relation, ok := timeframeRelationFor(period); ok && relation.lower != "" {
		lowerBars, lowerQuality, lowerErr := aggregateIntradayFromSource(len(input), source, relation.lower, cutoff)
		if lowerErr == nil && len(lowerBars) > 0 {
			lowerFrame := analyzeFrame(lowerBars, relation.lower, intradayPeriodLabel(relation.lower), spot)
			lowerFrame.Quality = lowerQuality
			lowerFrame = addRecursiveAnalysis(lowerFrame, lowerBars)
			AttachMappedTimeframe(&result.Daily, lowerFrame)
		} else {
			setTimeframeMapping(&result.Daily, false)
		}
	} else {
		setTimeframeMapping(&result.Daily, false)
	}
	result.Daily.Warnings = append(result.Daily.Warnings, "递归结构使用未复权盘中行情；盘中现价与复权日线结构使用不同价格基准，不直接比较")
	result.Weekly = analyzeFrame(nil, "", "周线", spot)
	result.Weekly.Timeframe = "1wk"
	result.Summary = Summary{Alignment: "intraday", Label: label + "结构", Detail: "按已收盘盘中 K 线识别；日周结构另行参考"}
	return result, nil
}

func intradayPeriodDuration(period string) time.Duration {
	switch period {
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "60m":
		return time.Hour
	case "1h":
		return time.Hour
	case "2h":
		return 2 * time.Hour
	case "4h":
		return 4 * time.Hour
	default:
		return 0
	}
}

func intradayPeriodLabel(period string) string {
	switch period {
	case "15m":
		return "15分钟"
	case "30m":
		return "30分钟"
	case "60m":
		return "60分钟"
	case "1h":
		return "1小时"
	case "2h":
		return "2小时"
	case "4h":
		return "4小时"
	default:
		return "盘中"
	}
}

// AggregateIntradayBars exposes the production intraday quality and bucket
// rules to offline calibration tools. Input must be timestamped 15-minute
// regular-session bars; output bars are closed and chronologically unique.
func AggregateIntradayBars(input []Bar, period string, cutoff time.Time) ([]Bar, DataQuality, error) {
	interval := intradayPeriodDuration(period)
	if interval == 0 {
		return nil, DataQuality{InputBars: len(input), PriceBasis: "raw", Status: "unsupported_period"}, fmt.Errorf("unsupported intraday period %q", period)
	}
	if period == "4h" {
		bars, quality, _ := aggregateFourHourBarsWithQuality(input, cutoff)
		return bars, quality, nil
	}
	if period == "15m" {
		bars, rejected, incomplete, duplicates := completedIntradayBars(input, cutoff, interval)
		bars, gaps := chainIntradayBars(bars, interval)
		gaps = mergeGaps(gaps, expectedIntradayGaps(bars, interval, cutoff))
		quality := DataQuality{InputBars: len(input), AcceptedBars: len(bars), RejectedBars: rejected, IncompleteBars: incomplete, DuplicateBars: duplicates, SourceInputBars: len(input), SourceAcceptedBars: len(bars), SourceRejectedBars: rejected, SourceIncompleteBars: incomplete, SourceDuplicateBars: duplicates, PriceBasis: "raw", Status: qualityStatus(rejected, incomplete, duplicates), Gaps: gaps}
		if len(gaps) > 0 && quality.Status == "ok" {
			quality.Status = "partial"
		}
		return bars, quality, nil
	}
	bars, quality, _ := aggregateIntradayBarsWithQuality(input, cutoff, interval)
	return bars, quality, nil
}

type intradaySource struct {
	bars                 []Bar
	gaps                 []DataGap
	rejected, unfinished int
	duplicates           int
}

func prepareIntradaySource(input []Bar, cutoff time.Time) intradaySource {
	bars, rejected, unfinished, duplicates := completedIntradayBars(input, cutoff, 15*time.Minute)
	bars, gaps := chainIntradayBars(bars, 15*time.Minute)
	return intradaySource{bars: bars, gaps: gaps, rejected: rejected, unfinished: unfinished, duplicates: duplicates}
}

func aggregateIntradayFromSource(inputCount int, source intradaySource, period string, cutoff time.Time) ([]Bar, DataQuality, error) {
	interval := intradayPeriodDuration(period)
	if interval == 0 {
		return nil, DataQuality{InputBars: inputCount, PriceBasis: "raw", Status: "unsupported_period"}, fmt.Errorf("unsupported intraday period %q", period)
	}
	if period == "15m" {
		gaps := mergeGaps(source.gaps, expectedIntradayGaps(source.bars, interval, cutoff))
		quality := DataQuality{InputBars: inputCount, AcceptedBars: len(source.bars), RejectedBars: source.rejected, IncompleteBars: source.unfinished, DuplicateBars: source.duplicates, SourceInputBars: inputCount, SourceAcceptedBars: len(source.bars), SourceRejectedBars: source.rejected, SourceIncompleteBars: source.unfinished, SourceDuplicateBars: source.duplicates, PriceBasis: "raw", Status: qualityStatus(source.rejected, source.unfinished, source.duplicates), Gaps: gaps}
		if len(gaps) > 0 && quality.Status == "ok" {
			quality.Status = "partial"
		}
		return source.bars, quality, nil
	}
	if period == "4h" {
		bars, quality, _ := aggregateFourHourBarsFromSource(inputCount, source.bars, source.rejected, source.unfinished, source.duplicates)
		return bars, quality, nil
	}
	bars, quality, _ := aggregateIntradayBarsFromSource(inputCount, source, interval)
	return bars, quality, nil
}

// aggregateIntradayBarsWithQuality groups complete intervals aligned to the
// regular-session open.
func aggregateIntradayBarsWithQuality(input []Bar, cutoff time.Time, interval time.Duration) ([]Bar, DataQuality, []DataGap) {
	if interval <= 15*time.Minute || interval%(15*time.Minute) != 0 {
		return nil, DataQuality{InputBars: len(input), PriceBasis: "raw", Status: "invalid_interval"}, nil
	}
	source := prepareIntradaySource(input, cutoff)
	return aggregateIntradayBarsFromSource(len(input), source, interval)
}

const (
	intradaySessionOpenMinute = 9*60 + 30
	fourHourBlockBars         = 16
)

func aggregateIntradayBarsFromSource(inputCount int, source intradaySource, interval time.Duration) ([]Bar, DataQuality, []DataGap) {
	if interval <= 15*time.Minute || interval%(15*time.Minute) != 0 {
		return nil, DataQuality{InputBars: inputCount, PriceBasis: "raw", Status: "invalid_interval"}, nil
	}
	groupSize := int(interval / (15 * time.Minute))
	byDay := make(map[int]map[int]Bar)
	for _, bar := range source.bars {
		days, minute, ok := marketcalendar.ParseDateTime(bar.Date)
		if !ok {
			continue
		}
		index := (minute - intradaySessionOpenMinute) / 15
		if index < 0 || index >= 26 {
			continue
		}
		if byDay[days] == nil {
			byDay[days] = map[int]Bar{}
		}
		byDay[days][index] = bar
	}
	result := []Bar{}
	for days, session := range byDay {
		slots := (marketcalendar.RegularCloseMinuteOrdinal(days) - intradaySessionOpenMinute) / 15
		for start := 0; start+groupSize <= slots; start += groupSize {
			complete := true
			var combined Bar
			for offset := 0; offset < groupSize; offset++ {
				bar, found := session[start+offset]
				if !found {
					complete = false
					break
				}
				if offset == 0 {
					combined = bar
					continue
				}
				combined.High = math.Max(combined.High, bar.High)
				combined.Low = math.Min(combined.Low, bar.Low)
				combined.Close, combined.Volume = bar.Close, combined.Volume+bar.Volume
			}
			if !complete {
				continue
			}
			combined.Chain = session[start].Chain
			startMinute := intradaySessionOpenMinute + start*15
			combined.Date = marketcalendar.FormatDateTime(days, startMinute)
			combined.CapturedAt = dayTime(days, startMinute+int(interval/time.Minute))
			result = append(result, combined)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	result = chainAggregatedIntradayBars(result, source.bars, interval, groupSize)
	gaps := append([]DataGap{}, source.gaps...)
	aggregateGaps, expectedCount := expectedAggregatedIntradayGaps(source.bars, result, groupSize, interval)
	gaps = mergeGaps(gaps, aggregateGaps)
	incomplete := maxInt(source.unfinished, expectedCount-len(result))
	quality := DataQuality{InputBars: expectedCount, AcceptedBars: len(result), RejectedBars: source.rejected, IncompleteBars: incomplete, DuplicateBars: source.duplicates, SourceInputBars: inputCount, SourceAcceptedBars: len(source.bars), SourceRejectedBars: source.rejected, SourceIncompleteBars: source.unfinished, SourceDuplicateBars: source.duplicates, PriceBasis: "raw", Status: qualityStatus(source.rejected, incomplete, source.duplicates), Gaps: gaps}
	if len(gaps) > 0 && quality.Status == "ok" {
		quality.Status = "partial"
	}
	return result, quality, gaps
}

func expectedAggregatedIntradayGaps(source, bars []Bar, groupSize int, interval time.Duration) ([]DataGap, int) {
	if len(source) == 0 || groupSize <= 0 || interval <= 0 {
		return []DataGap{}, 0
	}
	firstDays, _, okFirst := marketcalendar.ParseDateTime(source[0].Date)
	lastDays, _, okLast := marketcalendar.ParseDateTime(source[len(source)-1].Date)
	if !okFirst || !okLast {
		return []DataGap{}, 0
	}
	accepted := make(map[string]bool, len(bars))
	for _, bar := range bars {
		accepted[bar.Date] = true
	}
	expected := 0
	gaps := []DataGap{}
	cursor := 0
	for days := firstDays; days <= lastDays; days++ {
		if !marketcalendar.IsTradingDayOrdinal(days) {
			continue
		}
		slots := (marketcalendar.RegularCloseMinuteOrdinal(days) - intradaySessionOpenMinute) / 15
		for sourceIndex := 0; sourceIndex+groupSize <= slots; sourceIndex += groupSize {
			expected++
			key := marketcalendar.FormatDateTime(days, intradaySessionOpenMinute+sourceIndex*15)
			if accepted[key] {
				continue
			}
			// Accepted aggregated bars are sorted, so the next-gap cursor only
			// moves forward across the entire scan.
			for cursor < len(bars) && bars[cursor].Date <= key {
				cursor++
			}
			next := ""
			if cursor < len(bars) {
				next = bars[cursor].Date
			}
			gaps = append(gaps, DataGap{ExpectedDate: key, NextDate: next})
		}
	}
	return gaps, expected
}

// chainAggregatedIntradayBars preserves complete overnight transitions while
// splitting a structure chain around missing buckets or incomplete sessions.
func chainAggregatedIntradayBars(bars, source []Bar, interval time.Duration, groupSize int) []Bar {
	if len(bars) == 0 {
		return bars
	}
	intervalMinutes := int(interval / time.Minute)
	sourceDays := map[int]map[int]bool{}
	for _, bar := range source {
		days, minute, ok := marketcalendar.ParseDateTime(bar.Date)
		if !ok {
			continue
		}
		if sourceDays[days] == nil {
			sourceDays[days] = map[int]bool{}
		}
		sourceDays[days][(minute-intradaySessionOpenMinute)/15] = true
	}
	chain := 0
	previousDays, previousMinute, previousEndMinute, hasPrevious := 0, 0, 0, false
	for index := range bars {
		days, minute, ok := marketcalendar.ParseDateTime(bars[index].Date)
		if !ok {
			chain++
			bars[index].Chain = chain
			continue
		}
		endMinute := minute + intervalMinutes
		if index > 0 {
			split := false
			if hasPrevious && days == previousDays {
				split = minute-previousMinute != intervalMinutes
			} else if hasPrevious {
				nextTrading := marketcalendar.NextTradingDayOrdinal(previousDays)
				previousExpectedEnd := expectedAggregateSessionEndMinute(previousDays, groupSize, intervalMinutes)
				currentExpectedEnd := expectedAggregateSessionEndMinute(days, groupSize, intervalMinutes)
				previousComplete := previousExpectedEnd > previousMinute && previousEndMinute == previousExpectedEnd && completeIntradayGroups(sourceDays[previousDays], previousDays, groupSize)
				currentComplete := days == nextTrading && minute == intradaySessionOpenMinute && endMinute == currentExpectedEnd && completeIntradayGroups(sourceDays[days], days, groupSize)
				split = !previousComplete || !currentComplete
			} else {
				split = true
			}
			if split {
				chain++
			}
		}
		bars[index].Chain = chain
		previousDays, previousMinute, previousEndMinute, hasPrevious = days, minute, endMinute, true
	}
	return bars
}

func expectedAggregateSessionEndMinute(days, groupSize, intervalMinutes int) int {
	slots := (marketcalendar.RegularCloseMinuteOrdinal(days) - intradaySessionOpenMinute) / 15
	groups := slots / groupSize
	return intradaySessionOpenMinute + groups*intervalMinutes
}

func completeIntradayGroups(source map[int]bool, days, groupSize int) bool {
	slots := (marketcalendar.RegularCloseMinuteOrdinal(days) - intradaySessionOpenMinute) / 15
	groupedSlots := (slots / groupSize) * groupSize
	for start := 0; start < groupedSlots; start += groupSize {
		for offset := 0; offset < groupSize; offset++ {
			if !source[start+offset] {
				return false
			}
		}
	}
	return true
}

func completedIntradayBars(input []Bar, cutoff time.Time, interval time.Duration) ([]Bar, int, int, int) {
	sortedInput := true
	for index := 1; index < len(input); index++ {
		if input[index].Date < input[index-1].Date {
			sortedInput = false
			break
		}
	}
	var byTime map[string]Bar
	var duplicateTimes map[string]int
	var orderedBars []Bar
	if sortedInput {
		orderedBars = make([]Bar, 0, len(input))
	} else {
		byTime = make(map[string]Bar, len(input))
		duplicateTimes = make(map[string]int)
	}
	duplicateDate := ""
	type sessionInfo struct {
		valid       bool
		closeMinute int
	}
	sessions := make(map[int]sessionInfo)
	rejected, unfinished, duplicates := 0, 0, 0
	previousDays, previousMinute, hasPrevious := 0, 0, false
	cutoffDays, cutoffMinute := civilDayMinute(cutoff)
	intervalMinutes := int(interval / time.Minute)
	for _, bar := range input {
		days, minute, validStamp := marketcalendar.ParseDateTime(bar.Date)
		if !validStamp {
			rejected++
			continue
		}
		session, checked := sessions[days]
		if !checked {
			if marketcalendar.IsTradingDayOrdinal(days) {
				session = sessionInfo{valid: true, closeMinute: marketcalendar.RegularCloseMinuteOrdinal(days)}
			}
			sessions[days] = session
		}
		hour := minute / 60
		minuteOfHour := minute % 60
		if !session.valid || hour < 9 || hour > 15 ||
			hour == 9 && minuteOfHour < 30 || hour == 15 && minuteOfHour > 45 || minuteOfHour%15 != 0 ||
			!finitePositive(bar.Open) || !finitePositive(bar.High) || !finitePositive(bar.Low) || !finitePositive(bar.Close) ||
			bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			rejected++
			continue
		}
		endMinute := minute + intervalMinutes
		if endMinute > session.closeMinute {
			rejected++
			continue
		}
		if hasPrevious && (days < previousDays || days == previousDays && minute < previousMinute) {
			rejected++
			previousDays, previousMinute = days, minute
			continue
		}
		previousDays, previousMinute, hasPrevious = days, minute, true
		if days > cutoffDays || days == cutoffDays && endMinute > cutoffMinute {
			unfinished++
			continue
		}
		if math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
			rejected++
			continue
		}
		if sortedInput {
			if duplicateDate == bar.Date {
				duplicates++
				continue
			}
			if len(orderedBars) > 0 && orderedBars[len(orderedBars)-1].Date == bar.Date {
				orderedBars = orderedBars[:len(orderedBars)-1]
				duplicateDate = bar.Date
				duplicates += 2
				continue
			}
			orderedBars = append(orderedBars, bar)
			continue
		}
		if _, exists := duplicateTimes[bar.Date]; exists {
			duplicateTimes[bar.Date]++
			duplicates++
			continue
		}
		if _, exists := byTime[bar.Date]; exists {
			delete(byTime, bar.Date)
			duplicateTimes[bar.Date] = 2
			duplicates += 2
			continue
		}
		byTime[bar.Date] = bar
	}
	if sortedInput {
		return orderedBars, rejected, unfinished, duplicates
	}
	bars := make([]Bar, 0, len(byTime))
	for _, bar := range byTime {
		bars = append(bars, bar)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, rejected, unfinished, duplicates
}

func aggregateFourHourBars(input []Bar, cutoff time.Time) ([]Bar, int, int, int) {
	bars, quality, _ := aggregateFourHourBarsWithQuality(input, cutoff)
	return bars, quality.SourceRejectedBars, quality.SourceIncompleteBars, quality.SourceDuplicateBars
}

func aggregateFourHourBarsWithQuality(input []Bar, cutoff time.Time) ([]Bar, DataQuality, []DataGap) {
	const fifteenMinutes = 15 * time.Minute
	source := prepareIntradaySource(input, cutoff)
	return aggregateFourHourBarsFromSource(len(input), source.bars, source.rejected, source.unfinished, source.duplicates)
}

func aggregateFourHourBarsFromSource(inputCount int, sourceBars []Bar, rejected, unfinished, duplicates int) ([]Bar, DataQuality, []DataGap) {
	result := make([]Bar, 0, len(sourceBars)/26)
	for start := 0; start < len(sourceBars); {
		days, _, ok := marketcalendar.ParseDateTime(sourceBars[start].Date)
		if !ok {
			start++
			continue
		}
		dayText := sourceBars[start].Date[:10]
		end := start + 1
		for end < len(sourceBars) && len(sourceBars[end].Date) >= 10 && sourceBars[end].Date[:10] == dayText {
			end++
		}
		slots := (marketcalendar.RegularCloseMinuteOrdinal(days) - intradaySessionOpenMinute) / 15
		if fourHourBlockBars <= slots && end-start >= fourHourBlockBars {
			combined := sourceBars[start]
			complete := true
			for offset := 0; offset < fourHourBlockBars; offset++ {
				bar := sourceBars[start+offset]
				_, minute, valid := marketcalendar.ParseDateTime(bar.Date)
				if !valid || minute != intradaySessionOpenMinute+offset*15 {
					complete = false
					break
				}
				if offset == 0 {
					continue
				}
				combined.High = math.Max(combined.High, bar.High)
				combined.Low = math.Min(combined.Low, bar.Low)
				combined.Close, combined.Volume = bar.Close, combined.Volume+bar.Volume
			}
			if complete {
				combined.Date = dayText + " 09:30"
				combined.CapturedAt = dayTime(days, intradaySessionOpenMinute+fourHourBlockBars*15)
				result = append(result, combined)
			}
		}
		start = end
	}
	blocks := expectedFourHourBlocks(sourceBars)
	result = chainFourHourBars(result, blocks)
	missingBlocks := maxInt(0, len(blocks)-len(result))
	status := qualityStatus(rejected, missingBlocks+boolInt(unfinished > 0), duplicates)
	quality := DataQuality{
		InputBars: len(blocks), AcceptedBars: len(result), IncompleteBars: missingBlocks,
		SourceInputBars: inputCount, SourceAcceptedBars: len(sourceBars), SourceRejectedBars: rejected,
		SourceIncompleteBars: unfinished, SourceDuplicateBars: duplicates,
		PriceBasis: "raw", Status: status,
	}
	gaps := fourHourExpectedGaps(sourceBars, result)
	if len(gaps) > 0 && status == "ok" {
		status = "partial"
	}
	quality.Status = status
	quality.Gaps = gaps
	return result, quality, gaps
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func chainFourHourBars(bars []Bar, expected []int) []Bar {
	if len(bars) == 0 {
		return bars
	}
	indexByDate := make(map[string]int, len(expected))
	for index, days := range expected {
		indexByDate[marketcalendar.FormatDateTime(days, intradaySessionOpenMinute)] = index
	}
	chain := 0
	previousSlot, previousSourceChain := -1, bars[0].Chain
	for index := range bars {
		sourceChain := bars[index].Chain
		slot, found := indexByDate[bars[index].Date]
		if index > 0 && (!found || slot != previousSlot+1 || sourceChain != previousSourceChain) {
			chain++
		}
		bars[index].Chain = chain
		previousSlot, previousSourceChain = slot, sourceChain
	}
	return bars
}

func expectedFourHourBlocks(sourceBars []Bar) []int {
	if len(sourceBars) == 0 {
		return nil
	}
	firstDays, _, okFirst := marketcalendar.ParseDateTime(sourceBars[0].Date)
	lastDays, _, okLast := marketcalendar.ParseDateTime(sourceBars[len(sourceBars)-1].Date)
	if !okFirst || !okLast {
		return nil
	}
	blocks := []int{}
	endMinute := intradaySessionOpenMinute + fourHourBlockBars*15
	for days := firstDays; days <= lastDays; days++ {
		if !marketcalendar.IsTradingDayOrdinal(days) {
			continue
		}
		if endMinute > marketcalendar.RegularCloseMinuteOrdinal(days) {
			continue
		}
		blocks = append(blocks, days)
	}
	return blocks
}

func fourHourExpectedGaps(sourceBars, bars []Bar) []DataGap {
	if len(sourceBars) == 0 {
		return []DataGap{}
	}
	accepted := make(map[string]bool, len(bars))
	for _, bar := range bars {
		accepted[bar.Date] = true
	}
	blocks := expectedFourHourBlocks(sourceBars)
	gaps := []DataGap{}
	for slotIndex, days := range blocks {
		key := marketcalendar.FormatDateTime(days, intradaySessionOpenMinute)
		if accepted[key] {
			continue
		}
		next := ""
		for nextIndex := slotIndex + 1; nextIndex < len(blocks); nextIndex++ {
			candidate := marketcalendar.FormatDateTime(blocks[nextIndex], intradaySessionOpenMinute)
			if accepted[candidate] {
				next = candidate
				break
			}
		}
		gaps = append(gaps, DataGap{ExpectedDate: key, NextDate: next})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].ExpectedDate < gaps[j].ExpectedDate })
	return gaps
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func ptr(value float64) *float64 { return &value }

func sourceIndexFor(bars []Bar, index int) int {
	if index >= 0 && index < len(bars) && bars[index].HasSourceIndex {
		return bars[index].SourceIndex
	}
	return index
}

func completedBars(input []Bar, cutoff time.Time) ([]Bar, int, int, int, int) {
	bars, rejected, open, duplicates, reordered, _ := completedBarsDetailed(input, cutoff)
	return bars, rejected, open, duplicates, reordered
}

func completedBarsDetailed(input []Bar, cutoff time.Time) ([]Bar, int, int, int, int, ExclusionCounts) {
	cutoffDays, cutoffMinute := civilDayMinute(cutoff)
	rejected, open, duplicates, reordered := 0, 0, 0, 0
	exclusions := ExclusionCounts{}
	byDate := make(map[string]Bar, len(input))
	duplicateDates := map[string]bool{}
	previousDate := ""
	var closeUnix map[int]int64
	for inputIndex, bar := range input {
		days, validDate := marketcalendar.ParseDateOnly(bar.Date)
		if !validDate {
			rejected++
			exclusions.InvalidDate++
			continue
		}
		if !marketcalendar.IsTradingDayOrdinal(days) {
			rejected++
			exclusions.NonTrading++
			continue
		}
		if !finitePositive(bar.Open) || !finitePositive(bar.High) || !finitePositive(bar.Low) || !finitePositive(bar.Close) ||
			bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			rejected++
			exclusions.InvalidOHLC++
			continue
		}
		if previousDate != "" && bar.Date < previousDate {
			reordered++
		}
		previousDate = bar.Date
		closeMinute := marketcalendar.RegularCloseMinuteOrdinal(days)
		if cutoffDays < days || cutoffDays == days && cutoffMinute < closeMinute {
			open++
			continue
		}
		if !bar.CapturedAt.IsZero() {
			if closeUnix == nil {
				closeUnix = make(map[int]int64, 32)
			}
			closeAt, cached := closeUnix[days]
			if !cached {
				year, month, dayOfMonth := marketcalendar.CivilFromDays(days)
				closeAt = time.Date(year, time.Month(month), dayOfMonth, closeMinute/60, closeMinute%60, 0, 0, newYorkLocation()).UnixNano()
				closeUnix[days] = closeAt
			}
			if bar.CapturedAt.UnixNano() < closeAt {
				open++
				continue
			}
		}
		if math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
			rejected++
			exclusions.InvalidVolume++
			continue
		}
		if _, duplicate := duplicateDates[bar.Date]; duplicate {
			duplicates++
			continue
		}
		if _, exists := byDate[bar.Date]; exists {
			delete(byDate, bar.Date)
			duplicateDates[bar.Date] = true
			duplicates += 2
			continue
		}
		bar.SourceIndex, bar.HasSourceIndex = inputIndex, true
		byDate[bar.Date] = bar
	}
	bars := make([]Bar, 0, len(byDate))
	for _, bar := range byDate {
		bars = append(bars, bar)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, rejected, open, duplicates, reordered, exclusions
}

func dailyQuality(input, accepted []Bar, rejected, incomplete, duplicates, reordered int) DataQuality {
	gaps := []DataGap{}
	events := []CorporateActionEvent{}
	suspected := false
	previousDays, hasPrevious := 0, false
	for index := range accepted {
		days, validDate := marketcalendar.ParseDateOnly(accepted[index].Date)
		if !validDate {
			continue
		}
		if hasPrevious {
			expectedGap := marketcalendar.NextTradingDayOrdinal(previousDays)
			if expectedGap != days {
				gaps = append(gaps, DataGap{ExpectedDate: marketcalendar.FormatDateOnly(expectedGap), NextDate: accepted[index].Date})
			}
			// Match adjacentTradingSessions: advance only over non-trading days
			// strictly before the current bar.
			expectedAdjacent := previousDays + 1
			for expectedAdjacent < days && !marketcalendar.IsTradingDayOrdinal(expectedAdjacent) {
				expectedAdjacent++
			}
			if expectedAdjacent == days {
				event, eventSuspected := classifyCorporateAction(accepted[index-1], accepted[index])
				if event != nil {
					events = append(events, *event)
				}
				suspected = suspected || eventSuspected
			}
		}
		previousDays, hasPrevious = days, true
	}
	quality := DataQuality{InputBars: len(input), AcceptedBars: len(accepted), RejectedBars: rejected, IncompleteBars: incomplete, DuplicateBars: duplicates, ReorderedBars: reordered, Gaps: gaps, CorporateActionEvents: events, CorporateActionSuspected: suspected, PriceBasis: "raw", Status: qualityStatus(rejected, incomplete, duplicates)}
	if suspected {
		quality.Status = "degraded"
	}
	if quality.Status == "ok" && len(gaps) > 0 {
		quality.Status = "partial"
	}
	if reordered > 0 {
		quality.Status = "degraded"
	}
	return quality
}

func adjacentTradingSessions(previousDate, currentDate string) bool {
	previous, previousOK := marketcalendar.ParseDateOnly(previousDate)
	current, currentOK := marketcalendar.ParseDateOnly(currentDate)
	if !previousOK || !currentOK {
		return false
	}
	expected := previous + 1
	for expected < current && !marketcalendar.IsTradingDayOrdinal(expected) {
		expected++
	}
	return expected == current
}

func classifyCorporateAction(previous, current Bar) (*CorporateActionEvent, bool) {
	previousAdjusted, currentAdjusted := previous.Close, current.Close
	if !finitePositive(previousAdjusted) || !finitePositive(currentAdjusted) {
		return nil, false
	}
	adjustedReturn := currentAdjusted/previousAdjusted - 1
	previousRaw, currentRaw := previous.RawClose, current.RawClose
	previousFactor, currentFactor := previous.AdjustmentFactor, current.AdjustmentFactor
	hasRawPair := finitePositive(previousRaw) && finitePositive(currentRaw) && finitePositive(previousFactor) && finitePositive(currentFactor)
	if !hasRawPair {
		if math.Abs(adjustedReturn) > .20 {
			return &CorporateActionEvent{Date: current.Date, Kind: "unexplained_price_jump", Status: "degraded", RawCloseChangePercent: adjustedReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100}, true
		}
		return nil, false
	}
	rawReturn := currentRaw/previousRaw - 1
	factorReturn := currentFactor/previousFactor - 1
	if math.Abs(factorReturn) < .001 && math.Abs(rawReturn) < .20 && math.Abs(adjustedReturn) < .20 {
		return nil, false
	}
	verified := math.Abs((1+rawReturn)*(1+factorReturn)-(1+adjustedReturn)) <= 1e-5*math.Max(1, math.Abs(1+adjustedReturn))
	if math.Abs(rawReturn) >= .15 && math.Abs(factorReturn) >= .15 && math.Abs(adjustedReturn) < .20 && verified {
		return &CorporateActionEvent{Date: current.Date, Kind: "split_candidate", Status: "factor_adjusted", RawCloseChangePercent: rawReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100, FactorChangePercent: factorReturn * 100, FactorVerified: true}, false
	}
	if math.Abs(factorReturn) >= .001 && rawReturn < 0 && factorReturn > 0 && math.Abs(adjustedReturn) < .20 && verified {
		return &CorporateActionEvent{Date: current.Date, Kind: "dividend_or_cash_adjustment_candidate", Status: "factor_adjusted", RawCloseChangePercent: rawReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100, FactorChangePercent: factorReturn * 100, FactorVerified: true}, false
	}
	if math.Abs(rawReturn) > .20 || math.Abs(adjustedReturn) > .20 || math.Abs(factorReturn) > .15 {
		return &CorporateActionEvent{Date: current.Date, Kind: "unexplained_price_or_factor_jump", Status: "degraded", RawCloseChangePercent: rawReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100, FactorChangePercent: factorReturn * 100}, true
	}
	if math.Abs(factorReturn) >= .001 {
		return &CorporateActionEvent{Date: current.Date, Kind: "provider_factor_change", Status: "factor_adjusted", RawCloseChangePercent: rawReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100, FactorChangePercent: factorReturn * 100, FactorVerified: verified}, false
	}
	if math.Abs(adjustedReturn) > .20 {
		return &CorporateActionEvent{Date: current.Date, Kind: "unexplained_price_jump", Status: "degraded", RawCloseChangePercent: rawReturn * 100, AdjustedCloseChangePercent: adjustedReturn * 100, FactorChangePercent: factorReturn * 100}, true
	}
	return nil, false
}

func qualityStatus(rejected, incomplete, duplicates int) string {
	if rejected > 0 || duplicates > 0 {
		return "degraded"
	}
	if incomplete > 0 {
		return "partial"
	}
	return "ok"
}

func intradayGaps(bars []Bar, expected time.Duration) []DataGap {
	gaps := []DataGap{}
	location := newYorkLocation()
	for i := 1; i < len(bars); i++ {
		previous, errA := time.ParseInLocation("2006-01-02 15:04", bars[i-1].Date, location)
		current, errB := time.ParseInLocation("2006-01-02 15:04", bars[i].Date, location)
		if errA != nil || errB != nil || previous.Format(time.DateOnly) != current.Format(time.DateOnly) {
			continue
		}
		if current.Sub(previous) > expected {
			gaps = append(gaps, DataGap{ExpectedDate: previous.Add(expected).Format("2006-01-02 15:04"), NextDate: current.Format("2006-01-02 15:04")})
		}
	}
	return gaps
}

// expectedIntradayGaps finds absent closed session bars, including gaps before
// the first or after the last supplied bar. It stops at the requested cutoff
// and respects early closes from the market calendar.
type intradayStamp struct {
	days   int
	minute int
}

func (stamp intradayStamp) key() int64 {
	return int64(stamp.days)*1440 + int64(stamp.minute)
}

func expectedIntradayGaps(bars []Bar, interval time.Duration, cutoff time.Time) []DataGap {
	if len(bars) == 0 || interval <= 0 {
		return []DataGap{}
	}
	firstDays, firstMinute, firstOK := marketcalendar.ParseDateTime(bars[0].Date)
	_, _, lastOK := marketcalendar.ParseDateTime(bars[len(bars)-1].Date)
	if !firstOK || !lastOK {
		return []DataGap{}
	}
	intervalMinutes := int(interval / time.Minute)
	if intervalMinutes <= 0 {
		return []DataGap{}
	}
	known := make(map[int64]bool, len(bars))
	knownStamps := make([]intradayStamp, 0, len(bars))
	for _, bar := range bars {
		days, minute, ok := marketcalendar.ParseDateTime(bar.Date)
		if !ok {
			continue
		}
		stamp := intradayStamp{days: days, minute: minute}
		key := stamp.key()
		if known[key] {
			continue
		}
		known[key] = true
		knownStamps = append(knownStamps, stamp)
	}
	sort.Slice(knownStamps, func(i, j int) bool {
		if knownStamps[i].days != knownStamps[j].days {
			return knownStamps[i].days < knownStamps[j].days
		}
		return knownStamps[i].minute < knownStamps[j].minute
	})
	cutoffDays, cutoffMinute := civilDayMinute(cutoff)
	gaps := []DataGap{}
	cursor := 0
	for days := firstDays; days <= cutoffDays; days++ {
		if !marketcalendar.IsTradingDayOrdinal(days) {
			continue
		}
		closeMinute := marketcalendar.RegularCloseMinuteOrdinal(days)
		startMinute := 9*60 + 30
		if days == firstDays && firstMinute > startMinute {
			startMinute = firstMinute
		}
		beforeCutoffDay := days < cutoffDays
		for minute := startMinute; minute+intervalMinutes <= closeMinute; minute += intervalMinutes {
			if !beforeCutoffDay && minute+intervalMinutes > cutoffMinute {
				break
			}
			key := intradayStamp{days: days, minute: minute}.key()
			if known[key] {
				continue
			}
			for cursor < len(knownStamps) && knownStamps[cursor].key() <= key {
				cursor++
			}
			next := ""
			if cursor < len(knownStamps) {
				next = marketcalendar.FormatDateTime(knownStamps[cursor].days, knownStamps[cursor].minute)
			}
			gaps = append(gaps, DataGap{ExpectedDate: marketcalendar.FormatDateTime(days, minute), NextDate: next})
		}
	}
	return gaps
}

func mergeGaps(groups ...[]DataGap) []DataGap {
	result := []DataGap{}
	byExpected := make(map[string]int)
	for _, group := range groups {
		for _, gap := range group {
			if index, exists := byExpected[gap.ExpectedDate]; exists {
				if result[index].NextDate == "" || gap.NextDate != "" && gap.NextDate < result[index].NextDate {
					result[index].NextDate = gap.NextDate
				}
				continue
			}
			byExpected[gap.ExpectedDate] = len(result)
			result = append(result, gap)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExpectedDate < result[j].ExpectedDate })
	return result
}

func summarizeDataChains(bars []Bar, gaps []DataGap) []DataChain {
	chains := []DataChain{}
	for _, bar := range bars {
		if len(chains) == 0 || chains[len(chains)-1].ID != bar.Chain {
			chain := DataChain{ID: bar.Chain, Start: bar.Date, End: bar.Date, StartReason: "history_start"}
			if len(chains) > 0 {
				chain.StartReason = "unclassified_break"
				previousEnd := chains[len(chains)-1].End
				for _, gap := range gaps {
					if gap.NextDate == bar.Date || gap.ExpectedDate > previousEnd && gap.ExpectedDate <= bar.Date {
						chain.StartReason, chain.ExpectedDate = "missing_bar", gap.ExpectedDate
						if len(gap.ExpectedDate) == 10 && len(bar.Date) == 10 {
							chain.StartReason = "missing_session"
						} else if len(gap.ExpectedDate) >= 10 && len(bar.Date) >= 10 && gap.ExpectedDate[:10] != bar.Date[:10] {
							chain.StartReason = "truncated_session"
						}
						if gap.NextDate != "" && gap.NextDate > bar.Date {
							chain.StartReason = "skipped_session"
						}
						break
					}
				}
			}
			chains = append(chains, chain)
			continue
		}
		chains[len(chains)-1].End = bar.Date
	}
	for index := 1; index < len(bars); index++ {
		if bars[index].Chain == bars[index-1].Chain || bars[index-1].Close <= 0 {
			continue
		}
		if math.Abs(bars[index].Close/bars[index-1].Close-1) > 0.20 {
			for chainIndex := range chains {
				if chains[chainIndex].ID == bars[index].Chain {
					chains[chainIndex].StartReason = "unexplained_price_jump"
					chains[chainIndex].ExpectedDate = bars[index].Date
					break
				}
			}
		}
	}
	return chains
}

func summarizeStructureChains(strokes []Stroke, dataBars []Bar) []StructureChain {
	chains := []StructureChain{}
	dataChainAt := make(map[string]int, len(dataBars))
	for _, bar := range dataBars {
		dataChainAt[bar.Date] = bar.Chain
	}
	for _, stroke := range strokes {
		if len(chains) == 0 || chains[len(chains)-1].ID != stroke.Chain {
			reason, dataChain := "history_start", dataChainAt[stroke.Start.Date]
			if len(chains) > 0 {
				if chains[len(chains)-1].DataChainID != dataChain {
					reason = "data_gap"
				} else {
					reason = "fast_reversal_restart"
				}
			}
			chains = append(chains, StructureChain{ID: stroke.Chain, DataChainID: dataChain, Start: stroke.Start.Date, End: stroke.End.Date, StartReason: reason})
			continue
		}
		chains[len(chains)-1].End = stroke.End.Date
	}
	return chains
}

// chainIntradayBars assigns a new chain after missing in-session bars, skipped
// trading sessions, or a truncated session boundary. Overnight/weekend gaps
// alone do not break continuity when both adjacent sessions are complete.
func chainIntradayBars(bars []Bar, interval time.Duration) ([]Bar, []DataGap) {
	if len(bars) == 0 {
		return bars, nil
	}
	const sessionOpenMinute = 9*60 + 30
	intervalMinutes := int(interval / time.Minute)
	if intervalMinutes <= 0 {
		intervalMinutes = 15
	}
	chain := 0
	gaps := []DataGap{}
	previousDays, previousMinute, previousValid := 0, 0, false
	currentDay, firstMinute, lastMinute, hasCurrent := 0, 0, 0, false
	for index := range bars {
		days, minute, valid := marketcalendar.ParseDateTime(bars[index].Date)
		if !valid {
			chain++
			bars[index].Chain = chain
			previousValid = false
			continue
		}
		if previousValid {
			if days == previousDays {
				if minute-previousMinute != intervalMinutes {
					chain++
					gaps = append(gaps, DataGap{ExpectedDate: marketcalendar.FormatDateTime(previousDays, previousMinute+intervalMinutes), NextDate: bars[index].Date})
				}
			} else {
				expectedNext := marketcalendar.NextTradingDayOrdinal(previousDays)
				currentIsTrading := marketcalendar.IsTradingDayOrdinal(days)
				gap := days != expectedNext
				closeMinute := marketcalendar.RegularCloseMinuteOrdinal(previousDays)
				sessionIntervals := (closeMinute - sessionOpenMinute) / intervalMinutes
				expectedLastMinute := sessionOpenMinute + maxInt(0, sessionIntervals-1)*intervalMinutes
				// hasPreviousLast is always true here: the previous bar was
				// accepted and recorded as the last stamp of its day.
				if !currentIsTrading || gap || lastMinute != expectedLastMinute || firstMinute != sessionOpenMinute || minute != sessionOpenMinute {
					chain++
					expectedDays, expectedMinute := days, sessionOpenMinute
					if !gap && currentIsTrading && minute == sessionOpenMinute {
						if firstMinute != sessionOpenMinute {
							expectedDays, expectedMinute = previousDays, sessionOpenMinute
						} else if lastMinute != expectedLastMinute {
							expectedDays, expectedMinute = previousDays, expectedLastMinute+intervalMinutes
						}
					}
					gaps = append(gaps, DataGap{ExpectedDate: marketcalendar.FormatDateTime(expectedDays, expectedMinute), NextDate: bars[index].Date})
				}
			}
		}
		bars[index].Chain = chain
		if hasCurrent && days == currentDay {
			lastMinute = minute
		} else {
			currentDay, firstMinute, lastMinute, hasCurrent = days, minute, minute, true
		}
		previousDays, previousMinute, previousValid = days, minute, true
	}
	return bars, gaps
}

// chainDailyBars preserves valid historical ranges but starts a new structure
// chain at each missing session or unexplained large price discontinuity.
func chainDailyBars(bars []Bar) ([]Bar, []string) {
	warnings := []string{}
	if len(bars) == 0 {
		return bars, warnings
	}
	chain := 0
	missing := ""
	for i := 1; i < len(bars); i++ {
		previousDays, previousOK := marketcalendar.ParseDateOnly(bars[i-1].Date)
		days, currentOK := marketcalendar.ParseDateOnly(bars[i].Date)
		expected := marketcalendar.NextTradingDayOrdinal(previousDays)
		gap := !previousOK || !currentOK || expected != days
		jump := bars[i-1].Close > 0 && math.Abs(bars[i].Close/bars[i-1].Close-1) > 0.20
		if gap || jump {
			chain++
			missing = marketcalendar.FormatDateOnly(expected)
			if jump && !gap {
				missing = bars[i].Date
			}
		}
		bars[i].Chain = chain
	}
	if chain > 0 {
		warnings = append(warnings, fmt.Sprintf("历史存在 %d 处交易日缺口或超过 20%% 的未解释价格跳变（最近断点为 %s）；保留历史结构并分链，当前位置仅按最新链判断", chain, missing))
	}
	return bars, warnings
}

func weeklyBars(daily []Bar, cutoff time.Time) ([]Bar, bool) {
	bars, partial, _ := weeklyBarsDetailed(daily, cutoff)
	return bars, partial
}

func weeklyBarsDetailed(daily []Bar, cutoff time.Time) ([]Bar, bool, int) {
	bars, partial, incomplete, _, _ := weeklyBarsQualityDetailed(daily, cutoff)
	return bars, partial, incomplete
}

func weeklyBarsQualityDetailed(daily []Bar, cutoff time.Time) ([]Bar, bool, int, int, WeeklyExclusionCounts) {
	cutoffDays, cutoffMinute := civilDayMinute(cutoff)
	result := []Bar{}
	partial, incompleteWeeks, inputWeeks := false, 0, 0
	exclusions := WeeklyExclusionCounts{}
	days := make([]int, len(daily))
	for index := range daily {
		days[index], _ = marketcalendar.ParseDateOnly(daily[index].Date)
	}
	for start := 0; start < len(daily); {
		monday := days[start] - (marketcalendar.WeekdayOrdinal(days[start])+6)%7
		nextWeek := marketcalendar.FormatDateOnly(monday + 7)
		end := start + 1
		for end < len(daily) && daily[end].Date < nextWeek {
			end++
		}
		firstTrading := monday
		for !marketcalendar.IsTradingDayOrdinal(firstTrading) && firstTrading < monday+5 {
			firstTrading++
		}
		lastTrading := monday + 4
		for !marketcalendar.IsTradingDayOrdinal(lastTrading) && lastTrading >= monday {
			lastTrading--
		}
		// Count every excluded partial week, including a truncated first week, in
		// weekly units.
		expected := 0
		for date := monday; date < monday+5; date++ {
			if marketcalendar.IsTradingDayOrdinal(date) {
				expected++
			}
		}
		firstDate := days[start]
		truncatedFirstWeek := start == 0 && firstDate != firstTrading
		inputWeeks++
		chain := daily[start].Chain
		sameChain := true
		for index := start + 1; index < end; index++ {
			if daily[index].Chain != chain {
				sameChain = false
				break
			}
		}
		openWeek := cutoffDays < lastTrading || cutoffDays == lastTrading && cutoffMinute < marketcalendar.RegularCloseMinuteOrdinal(lastTrading)
		missingSessions := !openWeek && !truncatedFirstWeek && (firstDate != firstTrading || days[end-1] != lastTrading || end-start != expected)
		brokenChain := !sameChain
		weekComplete := !openWeek && !brokenChain && !missingSessions
		if truncatedFirstWeek || !weekComplete {
			partial = true
			incompleteWeeks++
			if openWeek {
				exclusions.OpenWeek++
			}
			if missingSessions {
				exclusions.MissingSessions++
			}
			if brokenChain {
				exclusions.BrokenChain++
			}
			if truncatedFirstWeek {
				exclusions.TruncatedStart++
			}
			start = end
			continue
		}
		bar := daily[start]
		for _, next := range daily[start+1 : end] {
			bar.High = math.Max(bar.High, next.High)
			bar.Low = math.Min(bar.Low, next.Low)
			bar.Close, bar.Date = next.Close, next.Date
			bar.Volume += next.Volume
		}
		result = append(result, bar)
		start = end
	}
	return result, partial, incompleteWeeks, inputWeeks, exclusions
}

func weeklyDataQuality(source DataQuality, inputWeeks, sourceInputBars, sourceAcceptedBars int, accepted []Bar, incompleteWeeks int, exclusions WeeklyExclusionCounts) DataQuality {
	quality := source
	quality.InputBars = inputWeeks
	quality.AcceptedBars = len(accepted)
	quality.SourceInputBars = sourceInputBars
	quality.SourceAcceptedBars = sourceAcceptedBars
	quality.SourceRejectedBars = source.RejectedBars
	quality.SourceIncompleteBars = source.IncompleteBars
	quality.SourceDuplicateBars = source.DuplicateBars
	quality.SourceReorderedBars = source.ReorderedBars
	quality.RejectedBars = 0
	quality.IncompleteBars = incompleteWeeks
	quality.DuplicateBars = 0
	quality.ReorderedBars = 0
	quality.InvalidDateBars = 0
	quality.NonTradingBars = 0
	quality.InvalidOHLCBars = 0
	quality.InvalidVolumeBars = 0
	quality.IncompleteWeeks = incompleteWeeks
	quality.WeeklyExclusions = exclusions
	quality.Gaps = weeklyGapsFromBars(accepted)
	quality.Chains = summarizeDataChains(accepted, quality.Gaps)
	if quality.Status != "degraded" && incompleteWeeks > 0 {
		quality.Status = "partial"
	}
	return quality
}

func weeklyGapsFromBars(weekly []Bar) []DataGap {
	gaps := []DataGap{}
	for index := 1; index < len(weekly); index++ {
		previous, previousOK := marketcalendar.ParseDateOnly(weekly[index-1].Date)
		current, currentOK := marketcalendar.ParseDateOnly(weekly[index].Date)
		if !previousOK || !currentOK {
			continue
		}
		monday := previous - (marketcalendar.WeekdayOrdinal(previous)+6)%7 + 7
		lastTrading := monday + 4
		for !marketcalendar.IsTradingDayOrdinal(lastTrading) && lastTrading >= monday {
			lastTrading--
		}
		if current > lastTrading {
			gaps = append(gaps, DataGap{ExpectedDate: marketcalendar.FormatDateOnly(lastTrading), NextDate: weekly[index].Date})
		}
	}
	return gaps
}

func mergeBars(bars []Bar) []mergedBar {
	merged := make([]mergedBar, 0, len(bars))
	for i, bar := range bars {
		sourceIndex := sourceIndexFor(bars, i)
		next := mergedBar{high: bar.High, low: bar.Low, highIndex: i, lowIndex: i, start: i, end: i, sourceHigh: sourceIndex, sourceLow: sourceIndex, sourceStart: sourceIndex, sourceEnd: sourceIndex, chain: bar.Chain}
		if len(merged) == 0 {
			merged = append(merged, next)
			continue
		}
		last := &merged[len(merged)-1]
		if last.chain != next.chain {
			merged = append(merged, next)
			continue
		}
		contains := (last.high >= next.high && last.low <= next.low) || (next.high >= last.high && next.low <= last.low)
		if !contains {
			merged = append(merged, next)
			continue
		}
		// The reference processes inclusion sequentially and decides its
		// direction from the immediately preceding merged pair at the moment
		// the inclusion occurs. A persistent direction becomes stale after a
		// reversal; the first pair uses the incoming candle as its reference.
		directionUp := next.high >= last.high
		if len(merged) >= 2 {
			directionUp = last.high >= merged[len(merged)-2].high
		}
		if directionUp && next.high >= last.high || !directionUp && next.high <= last.high {
			last.high, last.highIndex = next.high, i
			last.sourceHigh = sourceIndex
		}
		if directionUp && next.low >= last.low || !directionUp && next.low <= last.low {
			last.low, last.lowIndex = next.low, i
			last.sourceLow = sourceIndex
		}
		last.end = i
		last.sourceEnd = sourceIndex
	}
	return merged
}

func findFractals(bars []Bar, merged []mergedBar) []Fractal {
	fractals := make([]Fractal, 0, len(merged)/2+1)
	// The right neighbour must itself be finalized by a non-containing
	// candle. Otherwise a later inclusion can erase an apparently confirmed
	// fractal. This conservative delay is part of the public rule version.
	for i := 1; i+2 < len(merged); i++ {
		left, middle, right := merged[i-1], merged[i], merged[i+1]
		if left.chain != middle.chain || middle.chain != right.chain || right.chain != merged[i+2].chain {
			continue
		}
		var kind string
		index, price := middle.highIndex, middle.high
		if middle.high > left.high && middle.high > right.high && middle.low > left.low && middle.low > right.low {
			kind = "top"
		} else if middle.low < left.low && middle.low < right.low && middle.high < left.high && middle.high < right.high {
			kind, index, price = "bottom", middle.lowIndex, middle.low
		} else {
			continue
		}
		confirmedAt := bars[merged[i+2].start].Date
		// Chan's inclusion pipeline timestamps the standardized candle at the
		// last source bar absorbed into that candle. Keep the pivot price's raw
		// source index separately, but anchor the fractal and every downstream
		// pen/segment to the merged candle's date. Using the raw extreme's date
		// here shifts structural endpoints inside the merged candle and causes
		// recursive spans to diverge from the reference implementation.
		fractals = append(fractals, Fractal{Kind: kind, Date: bars[middle.end].Date, Price: price, Level: 0,
			FirstObservedAt: confirmedAt, ConfirmedAt: confirmedAt, MergedIndex: i, RawIndex: index,
			RawStartIndex: middle.start, RawEndIndex: middle.end, SourceRawIndex: sourceIndexFor(bars, index),
			SourceRawStart: middle.sourceStart, SourceRawEnd: middle.sourceEnd, Chain: middle.chain})
	}
	return fractals
}

func validPen(start, end Fractal, merged []mergedBar) bool {
	if start.Kind == end.Kind || end.MergedIndex-start.MergedIndex < 4 {
		return false
	}
	// A pen is determined by alternating confirmed fractals, the minimum
	// independent-bar distance, and the top/bottom price relation. Requiring
	// every intervening merged candle to stay inside the endpoint price range
	// rejects valid pens whenever an interior wick crosses an endpoint. That
	// extra envelope condition is not part of the Python reference's 81-rule
	// implementation and makes the recursive structure collapse on real data.
	if start.Kind == "bottom" {
		return end.Price > start.Price
	}
	if start.Kind == "top" {
		return end.Price < start.Price
	}
	return false
}

func buildStrokes(fractals []Fractal, merged []mergedBar, bars []Bar) ([]Stroke, string) {
	return buildStrokesWithHistogram(fractals, merged, macdHistogram(bars))
}

func buildStrokesWithHistogram(fractals []Fractal, merged []mergedBar, histogram []float64) ([]Stroke, string) {
	points := make([]Fractal, 0, len(fractals)+1)
	confirmed := make([]string, 0, len(fractals))
	strokes := make([]Stroke, 0, len(fractals))
	chain, sourceChain, restartCount := 0, -1, 0
	restartedAt := ""
	flush := func(includeForming bool) {
		for i := 1; i < len(points); i++ {
			isConfirmed := i-1 < len(confirmed)
			if !isConfirmed && !(includeForming && i == len(points)-1) {
				break
			}
			start, end := points[i-1], points[i]
			direction := "up"
			if end.Price < start.Price {
				direction = "down"
			}
			stroke := Stroke{Chain: chain, Start: start, End: end, Direction: direction, Level: 0,
				Low: math.Min(start.Price, end.Price), High: math.Max(start.Price, end.Price),
				StartBar: start.MergedIndex, EndBar: end.MergedIndex, StartRawBar: start.SourceRawIndex, EndRawBar: end.SourceRawIndex,
				InputStartBar: merged[start.MergedIndex].start, InputEndBar: merged[end.MergedIndex].end}
			if isConfirmed {
				stroke.Confirmed, stroke.ConfirmedAt = true, confirmed[i-1]
			}
			for k := start.RawIndex; k <= end.RawIndex && k < len(histogram); k++ {
				if direction == "up" {
					stroke.MACDArea += math.Max(0, histogram[k])
				} else {
					stroke.MACDArea += math.Max(0, -histogram[k])
				}
			}
			strokes = append(strokes, stroke)
		}
	}
	restart := func(fractal Fractal) {
		// A fast reversal can cross the pen's anchor before a valid opposite
		// pen exists. Keep already confirmed pens, then start a new chain so
		// historical structures remain visible without connecting across gaps.
		flush(false)
		restartCount++
		chain = -restartCount
		points, confirmed = append(points[:0], fractal), confirmed[:0]
		restartedAt = fractal.Date
	}
	for _, fractal := range fractals {
		if len(points) == 0 {
			sourceChain, chain = fractal.Chain, fractal.Chain
			points = append(points, fractal)
			continue
		}
		last := points[len(points)-1]
		if fractal.Chain != sourceChain {
			flush(false)
			sourceChain, chain = fractal.Chain, fractal.Chain
			points, confirmed = append(points[:0], fractal), confirmed[:0]
			restartedAt = fractal.Date
			continue
		}
		if last.Kind == fractal.Kind {
			moreExtreme := fractal.Kind == "top" && fractal.Price > last.Price || fractal.Kind == "bottom" && fractal.Price < last.Price
			if moreExtreme {
				if len(points) == 1 || validPen(points[len(points)-2], fractal, merged) {
					points[len(points)-1] = fractal
				} else {
					restart(fractal)
				}
			}
			continue
		}
		if !validPen(last, fractal, merged) {
			for _, bar := range merged[last.MergedIndex : fractal.MergedIndex+1] {
				if last.Kind == "top" && bar.high > last.Price || last.Kind == "bottom" && bar.low < last.Price {
					restart(fractal)
					break
				}
			}
			continue
		}
		if len(points) >= 2 {
			confirmed = append(confirmed, fractal.ConfirmedAt)
		}
		points = append(points, fractal)
	}
	flush(true)
	return strokes, restartedAt
}

func macdHistogram(bars []Bar) []float64 {
	result := make([]float64, len(bars))
	if len(bars) == 0 {
		return result
	}
	fast, slow, signal := bars[0].Close, bars[0].Close, 0.0
	for i, bar := range bars {
		fast += 2.0 / 13 * (bar.Close - fast)
		slow += 2.0 / 27 * (bar.Close - slow)
		diff := fast - slow
		signal += 2.0 / 10 * (diff - signal)
		result[i] = 2 * (diff - signal)
	}
	return result
}

func buildCenters(strokes []Stroke) []Center {
	centers := []Center{}
	for i := 0; i+2 < len(strokes); {
		a, b, c := strokes[i], strokes[i+1], strokes[i+2]
		if a.Chain != b.Chain || b.Chain != c.Chain || !a.Confirmed || !b.Confirmed || !c.Confirmed {
			i++
			continue
		}
		lower := math.Max(a.Low, math.Max(b.Low, c.Low))
		upper := math.Min(a.High, math.Min(b.High, c.High))
		if lower >= upper {
			i++
			continue
		}
		center := Center{Level: 0, Kind: "pen", InputKind: "stroke", Start: a.Start.Date, End: c.End.Date, Chain: a.Chain, Lower: lower, Upper: upper,
			ConfirmedAt: c.ConfirmedAt, StartStroke: i, EndStroke: i + 2, StrokeCount: 3}
		j := i + 3
		for j < len(strokes) && strokes[j].Chain == a.Chain && strokes[j].Confirmed && strokes[j].Low < upper && strokes[j].High > lower {
			center.End, center.EndStroke, center.StrokeCount = strokes[j].End.Date, j, center.StrokeCount+1
			j++
		}
		centers = append(centers, center)
		// First pen wholly outside the center begins the search for the next
		// non-overlapping center; extensions never narrow the original zone.
		i = j
	}
	return centers
}

func analyzeFrame(bars []Bar, period, label string, spot *float64) Frame {
	merged := mergeBars(bars)
	fractals := findFractals(bars, merged)
	return analyzeFrameWithComponents(bars, period, label, spot, merged, fractals, macdHistogram(bars))
}

func analyzeFrameWithComponents(bars []Bar, period, label string, spot *float64, merged []mergedBar, fractals []Fractal, histogram []float64) Frame {
	frame := Frame{Period: period, Label: label, Status: "insufficient", BarCount: len(bars), Level: 0,
		Direction: "unknown", Structure: "等待笔中枢形成", Candles: []Bar{}, Fractals: []Fractal{}, Strokes: []Stroke{},
		Centers: []Center{}, Signals: []Signal{}, Scenarios: []Scenario{}, Warnings: []string{}}
	frame.Timeframe = period
	if len(bars) > 0 {
		frame.AsOf, frame.LastClose = bars[len(bars)-1].Date, ptr(bars[len(bars)-1].Close)
	}
	frame.MergedCount = len(merged)
	frame.Fractals = fractals
	var restartedAt string
	frame.Strokes, restartedAt = buildStrokesWithHistogram(frame.Fractals, merged, histogram)
	if restartedAt != "" {
		frame.Warnings = append(frame.Warnings, "历史中存在无法连续接续的快速反向波动；已保留各段有效笔，并仅以 "+restartedAt+" 起的最新连续笔链判断当前位置")
	}
	frame.Centers = buildCenters(frame.Strokes)
	frame.Segments = buildSegments(frame.Strokes)
	frame.RecursiveCenters = buildRecursiveCenters(frame.Segments)
	latestChain := 0
	if len(bars) > 0 {
		latestChain = bars[len(bars)-1].Chain
	}
	// A fast reversal may restart stroke continuity inside the latest market
	// data chain. In that case use the newest stroke chain, even when it has
	// not formed a center yet; do not keep presenting a stale center as current.
	if len(frame.Strokes) > 0 {
		latestChain = frame.Strokes[len(frame.Strokes)-1].Chain
	}
	currentCenters := make([]Center, 0, len(frame.Centers))
	for _, center := range frame.Centers {
		if center.Chain == latestChain {
			currentCenters = append(currentCenters, center)
		}
	}
	frame.Position = locate(currentCenters, spot)
	if len(frame.Strokes) > 0 {
		frame.Status = "developing"
		frame.Direction = frame.Strokes[len(frame.Strokes)-1].Direction
	}
	if len(currentCenters) > 0 {
		frame.Status = "ready"
		frame.Signals = append(structuralSignals(frame.Strokes, currentCenters), turningSignals(frame.Strokes, currentCenters, bars)...)
		frame.Scenarios = scenarios(frame.Position)
		frame.Structure = centerStructure(currentCenters)
	} else {
		frame.Warnings = append(frame.Warnings, "尚未形成由三根已确认笔重叠构成的中枢，暂不推演中枢突破与回试")
	}
	// Keep historical pen-level third-point observations in their own field;
	// they are not recursive v4 buy/sell signals and must not be mixed into the
	// structural signal contract.
	frame.PenSignals = penThirdPointSignals(frame.Strokes, frame.Centers)
	invalidateSignals(frame.Signals, bars, spot)
	invalidateSignals(frame.PenSignals, bars, spot)
	if len(bars) < 60 {
		frame.Warnings = append(frame.Warnings, "历史长度不足 60 根，MACD 力度比较暂不输出")
	}
	if len(frame.Strokes) > 0 {
		frame.Warnings = append(frame.Warnings, "末笔端点仍可延伸，图中虚线表示形成中的笔")
	}
	// Keep the full analyzed series in the response so the chart can expose
	// every historical candle through its zoom and pan controls. Structure
	// calculations above already use this complete, validated history.
	frame.Candles = bars
	return frame
}

func locate(centers []Center, spot *float64) Position {
	position := Position{Code: "unknown", Label: "等待中枢形成", Detail: "数据或已确认笔不足，暂不能判断中枢位置"}
	if len(centers) == 0 || spot == nil {
		return position
	}
	center := centers[len(centers)-1]
	position.Center = &center
	position.Support, position.Resistance = ptr(center.Lower), ptr(center.Upper)
	switch {
	case *spot > center.Upper:
		position.Code, position.Label = "above", "位于最近中枢上方"
		position.Detail = "现价向上离开中枢；需观察回试是否守住中枢上沿"
		position.Support = ptr(center.Upper)
		position.Resistance = nil
	case *spot < center.Lower:
		position.Code, position.Label = "below", "位于最近中枢下方"
		position.Detail = "现价向下离开中枢；需观察反抽能否重新进入中枢"
		position.Support = nil
		position.Resistance = ptr(center.Lower)
	default:
		position.Code, position.Label = "inside", "位于最近中枢内部"
		position.Detail = "现价处于中枢重叠区，优先观察边界突破及随后的回试"
	}
	return position
}

func structuralSignals(strokes []Stroke, centers []Center) []Signal {
	signals := []Signal{}
	if len(strokes) < 2 {
		return signals
	}
	// Compare the newest fixed pen, never the mutable last pen. Signals are
	// observations at their confirmation dates; invalidation is explicit.
	index := len(strokes) - 2
	current := strokes[index]
	if index < 1 || !current.Confirmed || strokes[index-1].Chain != current.Chain {
		return signals
	}
	var center *Center
	for i := range centers {
		if centers[i].EndStroke < index {
			center = &centers[i]
		}
	}
	if center != nil && center.Chain == current.Chain && index >= 1 && center.EndStroke == index-1 {
		departure := strokes[index-1]
		if current.Direction == "down" && departure.Direction == "up" && departure.End.Price > center.Upper && current.Low > center.Upper {
			signals = append(signals, Signal{Kind: "third_buy_candidate", Label: "第三类买点候选（笔结构）", Status: "candidate",
				Date: current.End.Date, ConfirmedAt: current.ConfirmedAt, Invalidation: ptr(center.Upper),
				Detail: "向上离开中枢后的回试笔已固定，低点未进入中枢；仍需次级别走势确认"})
		}
		if current.Direction == "up" && departure.Direction == "down" && departure.End.Price < center.Lower && current.High < center.Lower {
			signals = append(signals, Signal{Kind: "third_sell_candidate", Label: "第三类卖点候选（笔结构）", Status: "candidate",
				Date: current.End.Date, ConfirmedAt: current.ConfirmedAt, Invalidation: ptr(center.Lower),
				Detail: "向下离开中枢后的反抽笔已固定，高点未进入中枢；仍需次级别走势确认"})
		}
	}
	return signals
}

// penThirdPointSignals keeps the reference implementation's historical
// pen-level third-buy/third-sell observations visible. They are not recursive
// Chan-level signals: the UI labels them as pen observations, and v4 signal
// generation continues to use only RecursiveLevels.
func penThirdPointSignals(strokes []Stroke, centers []Center) []Signal {
	result := []Signal{}
	seen := map[string]bool{}
	for _, center := range centers {
		if center.StartStroke < 0 || center.StartStroke >= len(strokes) {
			continue
		}
		for _, buy := range []bool{true, false} {
			leftCenter, departureIndex := false, -1
			for index := center.StartStroke; index < len(strokes); index++ {
				stroke := strokes[index]
				if stroke.Chain != center.Chain {
					break
				}
				if !stroke.Confirmed || stroke.Start.Date < center.Start {
					continue
				}
				leaves := buy && stroke.Direction == "up" && stroke.End.Price > center.Upper ||
					!buy && stroke.Direction == "down" && stroke.End.Price < center.Lower
				if !leftCenter && leaves {
					leftCenter, departureIndex = true, index
					continue
				}
				if !leftCenter || buy && stroke.Direction != "down" || !buy && stroke.Direction != "up" {
					continue
				}
				departure := strokes[departureIndex]
				boundary, returned := center.Upper, stroke.End.Price >= center.Upper
				kind, label, side := "third_buy_candidate", "第三类买点观察（笔级）", "buy"
				leavePower := departure.End.Price/departure.Start.Price - 1
				pullPower := math.Abs(stroke.End.Price/stroke.Start.Price - 1)
				if !buy {
					boundary, returned = center.Lower, stroke.End.Price <= center.Lower
					kind, label, side = "third_sell_candidate", "第三类卖点观察（笔级）", "sell"
					leavePower = math.Abs(departure.End.Price/departure.Start.Price - 1)
					pullPower = stroke.End.Price/stroke.Start.Price - 1
				}
				if !returned {
					leftCenter = false
					departureIndex = -1
					continue
				}
				if pullPower <= 0 {
					break
				}
				ratio := leavePower / pullPower
				identity := kind + "|" + stroke.End.Date
				if ratio > 1 && !seen[identity] {
					seen[identity] = true
					result = append(result, Signal{Kind: kind, Label: label, Status: "candidate", Side: side,
						Date: stroke.End.Date, OriginDate: stroke.End.Date, ObservedAt: stroke.ConfirmedAt,
						ConfirmedAt: stroke.ConfirmedAt, PhaseAt: stroke.ConfirmedAt, Phase: "pen_observation",
						Price: ptr(stroke.End.Price), Invalidation: ptr(boundary), StrengthRatio: ptr(ratio),
						Detail: "同一已形成笔中枢外侧的离开与首次回试满足笔级三类位置条件；这是参考实现的笔级观察，不等同于递归级别买卖点"})
				}
				// The reference scans only the first opposite pen. A failed
				// strength check is not retried with a later, weaker retest.
				break
			}
		}
	}
	return result
}

func invalidateSignals(signals []Signal, bars []Bar, spot *float64) {
	for i := range signals {
		signal := &signals[i]
		if signal.Invalidation == nil || !invalidatableSignal(signal.Kind) {
			continue
		}
		broken := func(low, high float64) bool {
			return (signal.Kind == "third_buy_candidate" || signal.Kind == "bottom_reversal_candidate") && low <= *signal.Invalidation ||
				(signal.Kind == "third_sell_candidate" || signal.Kind == "top_exit_candidate") && high >= *signal.Invalidation
		}
		invalid := spot != nil && broken(*spot, *spot)
		since := signal.ConfirmedAt
		if signal.ObservedAt != "" {
			since = signal.ObservedAt
		}
		// bars are chronological, so binary search the first stamp after the
		// signal became visible instead of rescanning the full history per signal.
		start := sort.Search(len(bars), func(index int) bool {
			return bars[index].Date > since
		})
		for _, bar := range bars[start:] {
			if broken(bar.Low, bar.High) {
				invalid = true
				break
			}
		}
		if invalid {
			signal.Status = "invalidated"
			signal.Detail += "；识别后已触及失效边界"
		}
	}
}

func invalidatableSignal(kind string) bool {
	switch kind {
	case "third_buy_candidate", "third_sell_candidate", "bottom_reversal_candidate", "top_exit_candidate":
		return true
	default:
		return false
	}
}

func centerStructure(centers []Center) string {
	if len(centers) < 2 {
		return "单个笔中枢，观察延伸与离开"
	}
	previous, latest := centers[len(centers)-2], centers[len(centers)-1]
	if latest.Lower > previous.Upper {
		return "相邻笔中枢向上抬升且不重叠"
	}
	if latest.Upper < previous.Lower {
		return "相邻笔中枢向下移动且不重叠"
	}
	return "相邻笔中枢区间仍有重叠或接触"
}

func scenarios(position Position) []Scenario {
	if position.Center == nil {
		return []Scenario{}
	}
	low, high := position.Center.Lower, position.Center.Upper
	return []Scenario{
		{Direction: "up", Label: "向上延续条件", Condition: fmt.Sprintf("收盘价突破 %.2f，后续回试笔低点仍高于 %.2f", high, high),
			Expectation:  "观察第三类买点候选；后续若形成更高且不重叠的中枢，再判断上涨结构延续",
			Invalidation: fmt.Sprintf("回试重新进入 %.2f–%.2f，中枢上方延续条件失效", low, high)},
		{Direction: "range", Label: "中枢震荡条件", Condition: fmt.Sprintf("价格在 %.2f–%.2f 内反复运行，离开后又回到区间", low, high),
			Expectation:  "继续观察中枢延伸及上下边界的回试，方向尚待选择",
			Invalidation: "出现离开后不重新进入中枢的已确认回试结构"},
		{Direction: "down", Label: "向下延续条件", Condition: fmt.Sprintf("收盘价跌破 %.2f，后续反抽笔高点仍低于 %.2f", low, low),
			Expectation:  "观察第三类卖点候选；后续若形成更低且不重叠的中枢，再判断下跌结构延续",
			Invalidation: fmt.Sprintf("反抽重新进入 %.2f–%.2f，中枢下方延续条件失效", low, high)},
	}
}

func frameSummary(daily, weekly Frame) Summary {
	if daily.Position.Center == nil || weekly.Position.Center == nil {
		return Summary{Alignment: "insufficient", Label: "等待日周结构完整", Detail: "至少一个周期尚未形成已确认笔中枢，先观察已有周期的结构与边界"}
	}
	d, w := daily.Position.Code, weekly.Position.Code
	if d == "above" && w == "above" {
		return Summary{Alignment: "up", Label: "日周均处于中枢上方", Detail: "两个周期的位置偏强，继续观察日线回试能否守住中枢上沿"}
	}
	if d == "below" && w == "below" {
		return Summary{Alignment: "down", Label: "日周均处于中枢下方", Detail: "两个周期的位置偏弱，关注日线反抽能否重新进入中枢"}
	}
	if d == "inside" && w == "inside" {
		return Summary{Alignment: "range", Label: "日周均处于中枢内部", Detail: "两个周期都在重叠区内，等待突破和回试结构"}
	}
	return Summary{Alignment: "mixed", Label: "日周位置尚未同向", Detail: "日线与周线处于不同结构位置，结合周线边界观察日线离开与回试"}
}
