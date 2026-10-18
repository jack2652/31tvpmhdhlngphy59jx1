package chananalysis

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCompletedBarsRejectsDuplicatesAndReportsOrdering(t *testing.T) {
	cutoff := time.Date(2026, 1, 12, 22, 0, 0, 0, time.UTC)
	input := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10, Volume: 1},
		{Date: "2026-01-05", Open: 10, High: 12, Low: 9, Close: 11, Volume: 1},
		{Date: "2026-01-08", Open: 11, High: 12, Low: 10, Close: 11, Volume: 1},
		{Date: "2026-01-06", Open: 11, High: 12, Low: 10, Close: 11, Volume: 1},
	}
	accepted, rejected, incomplete, duplicates, reordered := completedBars(input, cutoff)
	if len(accepted) != 2 || rejected != 0 || incomplete != 0 || duplicates != 2 || reordered != 1 {
		t.Fatalf("completedBars() accepted=%d rejected=%d incomplete=%d duplicates=%d reordered=%d", len(accepted), rejected, incomplete, duplicates, reordered)
	}
	quality := dailyQuality(input, accepted, rejected, incomplete, duplicates, reordered)
	if quality.Status != "degraded" || len(quality.Gaps) != 1 || quality.Gaps[0].ExpectedDate != "2026-01-07" {
		t.Fatalf("quality = %+v, want degraded with Jan 7 gap", quality)
	}
}

func TestIntradayChainsBreakAtMissingBarsButAllowCompleteOvernightSessions(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	input := []Bar{{Date: "2026-10-15 15:30"}, {Date: "2026-10-15 15:45"}, {Date: "2026-10-16 09:30"}, {Date: "2026-10-16 09:45"}}
	chained, gaps := chainIntradayBars(input, 15*time.Minute)
	if chained[0].Chain != chained[1].Chain || chained[1].Chain == chained[2].Chain || chained[2].Chain != chained[3].Chain || len(gaps) != 1 {
		t.Fatalf("chains=%+v gaps=%+v, want break for omitted session bars", chained, gaps)
	}
	chainSummary := summarizeDataChains(chained, gaps)
	if len(chainSummary) != 2 || chainSummary[0].Start != input[0].Date || chainSummary[1].Start != input[2].Date || chainSummary[1].StartReason != "truncated_session" || chainSummary[1].ExpectedDate != "2026-10-15 09:30" {
		t.Fatalf("chain summary=%+v gaps=%+v, want explicit origin and missing-bar reason", chainSummary, gaps)
	}
	complete := []Bar{}
	start := time.Date(2026, 10, 16, 9, 30, 0, 0, location)
	for index := 0; index < 26; index++ {
		complete = append(complete, Bar{Date: start.Add(time.Duration(index) * 15 * time.Minute).Format("2006-01-02 15:04")})
	}
	start = time.Date(2026, 10, 19, 9, 30, 0, 0, location)
	complete = append(complete, Bar{Date: start.Format("2006-01-02 15:04")}, Bar{Date: start.Add(15 * time.Minute).Format("2006-01-02 15:04")})
	chained, gaps = chainIntradayBars(complete, 15*time.Minute)
	if chained[25].Chain != chained[26].Chain || len(gaps) != 0 {
		t.Fatalf("complete Friday and Monday chains=%+v gaps=%+v, want overnight continuity", chained, gaps)
	}
}

func TestDailyChainSummaryRecordsGapStartAndReason(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-06", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-08", Open: 10, High: 11, Low: 9, Close: 10},
	}
	chained, _ := chainDailyBars(bars)
	quality := dailyQuality(bars, chained, 0, 0, 0, 0)
	quality.Chains = summarizeDataChains(chained, quality.Gaps)
	if len(quality.Chains) != 2 || quality.Chains[0].Start != "2026-01-05" || quality.Chains[0].End != "2026-01-06" || quality.Chains[0].StartReason != "history_start" || quality.Chains[1].Start != "2026-01-08" || quality.Chains[1].StartReason != "missing_session" || quality.Chains[1].ExpectedDate != "2026-01-07" {
		t.Fatalf("daily chains=%+v gaps=%+v, want gap start/reason in second chain", quality.Chains, quality.Gaps)
	}
}

func TestExpectedIntradayGapsIncludesMissingSessionEdges(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	bars := []Bar{
		{Date: "2026-10-16 09:45"},
		{Date: "2026-10-16 15:30"},
	}
	cutoff := time.Date(2026, 10, 16, 17, 0, 0, 0, location)
	gaps := expectedIntradayGaps(bars, 15*time.Minute, cutoff)
	if len(gaps) != 23 || gaps[0].ExpectedDate != "2026-10-16 10:00" || gaps[0].NextDate != "2026-10-16 15:30" || gaps[len(gaps)-1].ExpectedDate != "2026-10-16 15:45" {
		t.Fatalf("gaps=%+v, want first internal gap and trailing 15:45 session gap", gaps)
	}
}

func TestFourHourAggregationRetainsSeparateChainsAroundMissingSourceBars(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	cutoff := time.Date(2026, 10, 16, 17, 0, 0, 0, location)
	input := []Bar{}
	for _, day := range []string{"2026-10-14", "2026-10-15", "2026-10-16"} {
		start, _ := time.ParseInLocation(time.DateOnly, day, location)
		for index := 0; index < 26; index++ {
			stamp := start.Add(9*time.Hour + 30*time.Minute + time.Duration(index)*15*time.Minute)
			input = append(input, Bar{Date: stamp.Format("2006-01-02 15:04"), Open: 10, High: 11, Low: 9, Close: 10, Volume: 1})
		}
	}
	// Omit a source bar from the middle day's only complete 4-hour block.
	input = append(input[:26+8], input[26+9:]...)
	aggregated, _, _, _ := aggregateFourHourBars(input, cutoff)
	if len(aggregated) != 2 {
		t.Fatalf("aggregated=%+v, want the incomplete middle block omitted", aggregated)
	}
	if aggregated[0].Chain == aggregated[1].Chain {
		t.Fatalf("aggregated bars bridged a missing 4-hour block: %+v", aggregated)
	}
}

func TestAnalyzeIntradayExposesTimeframeLevelRawBasisAndFormingState(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 10, 16, 17, 0, 0, 0, location)
	input := []Bar{}
	for dayIndex, day := range []string{"2026-10-15", "2026-10-16"} {
		start, _ := time.ParseInLocation(time.DateOnly, day, location)
		for index := 0; index < 26; index++ {
			stamp := start.Add(9*time.Hour + 30*time.Minute + time.Duration(index)*15*time.Minute)
			price := 100 + float64(dayIndex*26+index)/100
			input = append(input, Bar{Date: stamp.Format("2006-01-02 15:04"), Open: price, High: price + 1, Low: price - 1, Close: price + .1, Volume: 10})
		}
	}
	for _, period := range []string{"15m", "1h", "2h", "4h"} {
		t.Run(period, func(t *testing.T) {
			result, analyzeErr := AnalyzeIntraday("TEST", input, period, nil, cutoff)
			if analyzeErr != nil {
				t.Fatal(analyzeErr)
			}
			frame := result.Daily
			if result.RuleVersion != RecursivePreviewVersion || result.LegacyRuleVersion != RuleVersion || frame.Timeframe != period || frame.Level != 0 || frame.Quality.PriceBasis != "raw" {
				t.Fatalf("intraday response metadata=%+v rule=%s legacy=%s, want preview version, timeframe=%s, level=0 and raw basis", frame, result.RuleVersion, result.LegacyRuleVersion, period)
			}
			if len(frame.RecursiveLevels) == 0 || frame.RecursiveLevels[0].Level != 0 || frame.RecursiveLevels[0].Status == "" {
				t.Fatalf("recursive levels=%+v, want explicit level 0 status", frame.RecursiveLevels)
			}
			if !containsWarning(frame.Warnings, "不同价格基准") {
				t.Fatalf("warnings=%v, raw intraday analysis must explain its price-coordinate limit", frame.Warnings)
			}
			if period == "15m" && (frame.Quality.InputBars != 52 || frame.Quality.AcceptedBars != 52 || frame.Quality.Status != "ok") {
				t.Fatalf("15m quality=%+v, want 52 accepted complete bars", frame.Quality)
			}
			if period == "1h" && (frame.Quality.InputBars != 12 || frame.Quality.AcceptedBars != 12 || frame.Quality.IncompleteBars != 0 || frame.Quality.SourceAcceptedBars != 52 || frame.Quality.Status != "ok") {
				t.Fatalf("1h quality=%+v, want 12 complete 1-hour blocks and 52 accepted source bars", frame.Quality)
			}
			if period == "2h" && (frame.Quality.InputBars != 6 || frame.Quality.AcceptedBars != 6 || frame.Quality.IncompleteBars != 0 || frame.Quality.SourceAcceptedBars != 52 || frame.Quality.Status != "ok") {
				t.Fatalf("2h quality=%+v, want six complete 2-hour blocks and 52 accepted source bars", frame.Quality)
			}
			if period == "4h" && (frame.Quality.InputBars != 2 || frame.Quality.AcceptedBars != 2 || frame.Quality.IncompleteBars != 0 || frame.Quality.SourceAcceptedBars != 52 || frame.Quality.Status != "ok") {
				t.Fatalf("4h quality=%+v, want two accepted blocks and 52 accepted source bars", frame.Quality)
			}
		})
	}
}

func TestAnalyzeIntradayMarksMissingFourHourBlockPartial(t *testing.T) {
	location, _ := time.LoadLocation("America/New_York")
	cutoff := time.Date(2026, 10, 16, 17, 0, 0, 0, location)
	input := []Bar{}
	for _, day := range []string{"2026-10-15", "2026-10-16"} {
		start, _ := time.ParseInLocation(time.DateOnly, day, location)
		for index := 0; index < 26; index++ {
			if day == "2026-10-15" && index == 8 {
				continue
			}
			stamp := start.Add(9*time.Hour + 30*time.Minute + time.Duration(index)*15*time.Minute)
			input = append(input, Bar{Date: stamp.Format("2006-01-02 15:04"), Open: 10, High: 11, Low: 9, Close: 10, Volume: 1})
		}
	}
	result, err := AnalyzeIntraday("TEST", input, "4h", nil, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	quality := result.Daily.Quality
	if quality.InputBars != 2 || quality.AcceptedBars != 1 || quality.IncompleteBars != 1 || quality.Status != "partial" || len(quality.Gaps) == 0 {
		t.Fatalf("4h quality=%+v, want missing block reported in 4-hour units", quality)
	}
}

func containsWarning(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

func TestCompletedBarsRejectsBadOHLCVolumeAndNonTradingDay(t *testing.T) {
	cutoff := time.Date(2026, 1, 12, 22, 0, 0, 0, time.UTC)
	input := []Bar{
		{Date: "2026-01-05", Open: 10, High: 9, Low: 8, Close: 10, Volume: 1},
		{Date: "2026-01-06", Open: 10, High: 11, Low: 9, Close: 10, Volume: math.NaN()},
		{Date: "2026-01-10", Open: 10, High: 11, Low: 9, Close: 10, Volume: 1},
		{Date: "2026-01-07", Open: 10, High: 11, Low: 9, Close: 10, Volume: -1},
	}
	accepted, rejected, _, _, _ := completedBars(input, cutoff)
	if len(accepted) != 0 || rejected != 4 {
		t.Fatalf("completedBars() accepted=%d rejected=%d, want 0/4", len(accepted), rejected)
	}
}

func TestCompletedBarsRejectsMalformedDatesAndNonFinitePrices(t *testing.T) {
	cutoff := time.Date(2026, 1, 12, 22, 0, 0, 0, time.UTC)
	input := []Bar{
		{Date: "2026-02-30", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-05T00:00:00Z", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-05", Open: 10, High: math.Inf(1), Low: 9, Close: 10},
	}
	accepted, rejected, _, _, _, exclusions := completedBarsDetailed(input, cutoff)
	if len(accepted) != 0 || rejected != 3 || exclusions.InvalidDate != 2 || exclusions.InvalidOHLC != 1 {
		t.Fatalf("validated bars=%d rejected=%d exclusions=%+v, want 0/3 and date=2 OHLC=1", len(accepted), rejected, exclusions)
	}
}

func TestDailyQualityMarksUnexplainedLargeCloseJump(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-05", Close: 100},
		{Date: "2026-01-06", Close: 150},
	}
	quality := dailyQuality(bars, bars, 0, 0, 0, 0)
	if !quality.CorporateActionSuspected || quality.Status != "degraded" {
		t.Fatalf("quality = %+v, want suspected corporate action and degraded status", quality)
	}
}

func TestAdjustedSeriesDoesNotBreakChainForFactorExplainedSplit(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-05", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 100, RawHigh: 102, RawLow: 98, RawClose: 100, AdjustmentFactor: .5},
		{Date: "2026-01-06", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 50, RawHigh: 51, RawLow: 49, RawClose: 50, AdjustmentFactor: 1},
	}
	for _, bar := range bars {
		if !validAdjustedPair(bar) {
			t.Fatalf("adjusted split fixture is invalid: %+v", bar)
		}
	}
	chained, warnings := chainDailyBars(bars)
	if chained[0].Chain != chained[1].Chain || len(warnings) != 0 {
		t.Fatalf("factor-explained split broke adjusted chain: bars=%+v warnings=%v", chained, warnings)
	}
	quality := dailyQuality(bars, bars, 0, 0, 0, 0)
	if quality.CorporateActionSuspected {
		t.Fatalf("normalized adjusted prices were marked as a corporate-action jump: %+v", quality)
	}
	if len(quality.CorporateActionEvents) != 1 || quality.CorporateActionEvents[0].Kind != "split_candidate" || !quality.CorporateActionEvents[0].FactorVerified || quality.CorporateActionEvents[0].Status != "factor_adjusted" {
		t.Fatalf("split event classification=%+v, want verified split candidate", quality.CorporateActionEvents)
	}
}

func TestAdjustedSeriesDoesNotBreakChainForFactorExplainedReverseSplit(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-05", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 50, RawHigh: 51, RawLow: 49, RawClose: 50, AdjustmentFactor: 1},
		{Date: "2026-01-06", Open: 50, High: 51, Low: 49, Close: 50, RawOpen: 100, RawHigh: 102, RawLow: 98, RawClose: 100, AdjustmentFactor: .5},
	}
	for _, bar := range bars {
		if !validAdjustedPair(bar) {
			t.Fatalf("adjusted reverse-split fixture is invalid: %+v", bar)
		}
	}
	chained, warnings := chainDailyBars(bars)
	if chained[0].Chain != chained[1].Chain || len(warnings) != 0 {
		t.Fatalf("factor-explained reverse split broke adjusted chain: bars=%+v warnings=%v", chained, warnings)
	}
	quality := dailyQuality(bars, bars, 0, 0, 0, 0)
	if quality.CorporateActionSuspected || quality.Status != "ok" {
		t.Fatalf("normalized adjusted reverse-split prices were marked degraded: %+v", quality)
	}
	if len(quality.CorporateActionEvents) != 1 || quality.CorporateActionEvents[0].Kind != "split_candidate" || !quality.CorporateActionEvents[0].FactorVerified || quality.CorporateActionEvents[0].Status != "factor_adjusted" {
		t.Fatalf("reverse-split event classification=%+v, want verified split candidate", quality.CorporateActionEvents)
	}
}

func TestCompletedBarsExcludesUnclosedDailyBarAtCutoff(t *testing.T) {
	cutoff := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	input := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10, Volume: 1},
		{Date: "2026-01-06", Open: 10, High: 12, Low: 9, Close: 11, Volume: 1},
	}
	accepted, rejected, incomplete, _, _ := completedBars(input, cutoff)
	if len(accepted) != 1 || accepted[0].Date != "2026-01-05" || rejected != 0 || incomplete != 1 {
		t.Fatalf("completedBars() accepted=%+v rejected=%d incomplete=%d, want prior closed bar only and current bar incomplete", accepted, rejected, incomplete)
	}
}

func TestDailyQualityClassifiesDividendFactorAndDegradesUnexplainedJumps(t *testing.T) {
	dividend := []Bar{
		{Date: "2026-01-05", Close: 99, RawClose: 100, AdjustmentFactor: .99},
		{Date: "2026-01-06", Close: 99, RawClose: 99, AdjustmentFactor: 1},
	}
	quality := dailyQuality(dividend, dividend, 0, 0, 0, 0)
	if quality.CorporateActionSuspected || quality.Status != "ok" || len(quality.CorporateActionEvents) != 1 || quality.CorporateActionEvents[0].Kind != "dividend_or_cash_adjustment_candidate" || !quality.CorporateActionEvents[0].FactorVerified {
		t.Fatalf("dividend adjusted event=%+v, want verified dividend/cash adjustment without degrading normalized prices", quality)
	}

	unexplained := []Bar{
		{Date: "2026-01-05", Close: 100, RawClose: 100, AdjustmentFactor: 1},
		{Date: "2026-01-06", Close: 150, RawClose: 150, AdjustmentFactor: 1},
	}
	quality = dailyQuality(unexplained, unexplained, 0, 0, 0, 0)
	if !quality.CorporateActionSuspected || quality.Status != "degraded" || len(quality.CorporateActionEvents) != 1 || quality.CorporateActionEvents[0].Status != "degraded" {
		t.Fatalf("unexplained raw/adjusted jump quality=%+v, want explicit degraded event", quality)
	}
	chained, warnings := chainDailyBars(unexplained)
	if chained[0].Chain == chained[1].Chain || len(warnings) != 1 {
		t.Fatalf("unexplained jump crossed a structure chain: bars=%+v warnings=%v", chained, warnings)
	}
}

func TestCorporateActionClassifierDoesNotTreatNonTradingGapAsOneDayEvent(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-09", Close: 100, RawClose: 100, AdjustmentFactor: 1},
		{Date: "2026-01-13", Close: 150, RawClose: 150, AdjustmentFactor: 1},
	}
	quality := dailyQuality(bars, bars, 0, 0, 0, 0)
	if len(quality.CorporateActionEvents) != 0 || quality.CorporateActionSuspected {
		t.Fatalf("multi-session gap was classified as a one-session corporate action: %+v", quality.CorporateActionEvents)
	}
}

func TestDailyQualityDetectsActionAcrossWeekendSessionBoundary(t *testing.T) {
	bars := []Bar{
		{Date: "2026-01-09", Close: 100, RawClose: 100, AdjustmentFactor: .5},
		{Date: "2026-01-12", Close: 100, RawClose: 50, AdjustmentFactor: 1},
	}
	quality := dailyQuality(bars, bars, 0, 0, 0, 0)
	if len(quality.CorporateActionEvents) != 1 || quality.CorporateActionEvents[0].Kind != "split_candidate" || !quality.CorporateActionEvents[0].FactorVerified {
		t.Fatalf("weekend split candidate was missed or mislabeled: %+v", quality)
	}
}

func TestValidAdjustedPairRequiresPairedOHLCAndFactor(t *testing.T) {
	bar := Bar{Open: 50, High: 55, Low: 45, Close: 52, RawOpen: 100, RawHigh: 110, RawLow: 90, RawClose: 104, AdjustmentFactor: .5}
	if !validAdjustedPair(bar) {
		t.Fatal("valid split adjusted OHLC pair was rejected")
	}
	bar.RawHigh = 120
	if validAdjustedPair(bar) {
		t.Fatal("OHLC pair with inconsistent adjustment factor was accepted")
	}
	bar.RawHigh, bar.AdjustmentFactor = 110, 0
	if validAdjustedPair(bar) {
		t.Fatal("missing adjustment factor was accepted")
	}
}

func TestAnalyzeAdjustedExcludesBrokenFactorAndSplitsChain(t *testing.T) {
	cutoff := time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC)
	input := []Bar{
		{Date: "2026-10-14", Open: 50, High: 51, Low: 49, Close: 50, Volume: 100, RawOpen: 100, RawHigh: 102, RawLow: 98, RawClose: 100, AdjustmentFactor: .5},
		{Date: "2026-10-15", Open: 51, High: 52, Low: 50, Close: 51, Volume: 100, RawOpen: 102, RawHigh: 108, RawLow: 100, RawClose: 102, AdjustmentFactor: .5},
		{Date: "2026-10-16", Open: 52, High: 53, Low: 51, Close: 52, Volume: 100, RawOpen: 104, RawHigh: 106, RawLow: 102, RawClose: 104, AdjustmentFactor: .5},
	}
	result := AnalyzeAdjusted("TEST", input, ptr(52), cutoff)
	if result.RuleVersion != RecursivePreviewVersion || result.Daily.Quality.AcceptedBars != 2 || result.Daily.Quality.RejectedBars != 1 || result.Daily.Quality.Status != "degraded" || len(result.Daily.Quality.Gaps) != 1 || len(result.Daily.Warnings) == 0 {
		t.Fatalf("adjusted quality=%+v warnings=%v version=%s, want invalid bar rejected and following data split", result.Daily.Quality, result.Daily.Warnings, result.RuleVersion)
	}
}

func TestWeeklyQualityUsesWeeklyUnitsAndReportsSourceCounts(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	makeBar := func(day string) Bar {
		return Bar{Date: day, Open: 10, High: 11, Low: 9, Close: 10, Volume: 1}
	}
	daily := []Bar{makeBar("2026-01-05"), makeBar("2026-01-06"), makeBar("2026-01-07"), makeBar("2026-01-08"), makeBar("2026-01-09"), makeBar("2026-01-12"), makeBar("2026-01-13"), makeBar("2026-01-14")}
	cutoff := time.Date(2026, 1, 14, 22, 0, 0, 0, location)
	weekly, partial, incomplete, inputWeeks, exclusions := weeklyBarsQualityDetailed(daily, cutoff)
	if len(weekly) != 1 || !partial || incomplete != 1 || inputWeeks != 2 {
		t.Fatalf("weekly bars=%d partial=%v incomplete=%d input=%d, want 1/true/1/2", len(weekly), partial, incomplete, inputWeeks)
	}
	quality := weeklyDataQuality(DataQuality{Status: "ok", RejectedBars: 2, IncompleteBars: 1, DuplicateBars: 3, ReorderedBars: 4}, inputWeeks, 20, 18, weekly, incomplete, exclusions)
	if quality.InputBars != 2 || quality.AcceptedBars != 1 || quality.IncompleteWeeks != 1 || quality.IncompleteBars != 1 || quality.Status != "partial" || quality.RejectedBars != 0 || quality.DuplicateBars != 0 || quality.ReorderedBars != 0 || quality.SourceInputBars != 20 || quality.SourceAcceptedBars != 18 || quality.SourceRejectedBars != 2 || quality.SourceIncompleteBars != 1 || quality.SourceDuplicateBars != 3 || quality.SourceReorderedBars != 4 {
		t.Fatalf("weekly quality=%+v, want weekly counts plus source daily counts", quality)
	}
}

func TestWeeklyQualityCountsMissingSessionAndChainBreakAsIncompleteWeek(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	daily := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-06", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-08", Open: 10, High: 11, Low: 9, Close: 10},
		{Date: "2026-01-09", Open: 10, High: 11, Low: 9, Close: 10},
	}
	cutoff := time.Date(2026, 1, 12, 22, 0, 0, 0, location)
	weekly, partial, incomplete, inputWeeks, exclusions := weeklyBarsQualityDetailed(daily, cutoff)
	if len(weekly) != 0 || !partial || incomplete != 1 || inputWeeks != 1 {
		t.Fatalf("weekly bars=%d partial=%v incomplete=%d input=%d, want 0/true/1/1", len(weekly), partial, incomplete, inputWeeks)
	}
	if exclusions.MissingSessions != 1 {
		t.Fatalf("weekly exclusions=%+v, want one missing-session week", exclusions)
	}
}

func TestWeeklyQualityReportsBrokenChainReason(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	daily := []Bar{
		{Date: "2026-01-05", Open: 10, High: 11, Low: 9, Close: 10, Chain: 0},
		{Date: "2026-01-06", Open: 10, High: 11, Low: 9, Close: 10, Chain: 0},
		{Date: "2026-01-07", Open: 10, High: 11, Low: 9, Close: 10, Chain: 1},
		{Date: "2026-01-08", Open: 10, High: 11, Low: 9, Close: 10, Chain: 1},
		{Date: "2026-01-09", Open: 10, High: 11, Low: 9, Close: 10, Chain: 1},
	}
	cutoff := time.Date(2026, 1, 12, 22, 0, 0, 0, location)
	weekly, partial, incomplete, inputWeeks, exclusions := weeklyBarsQualityDetailed(daily, cutoff)
	if len(weekly) != 0 || !partial || incomplete != 1 || inputWeeks != 1 || exclusions.BrokenChain != 1 || exclusions.MissingSessions != 0 {
		t.Fatalf("weekly bars=%d partial=%v incomplete=%d input=%d exclusions=%+v, want one broken-chain week", len(weekly), partial, incomplete, inputWeeks, exclusions)
	}
}

func TestAnalyzeAdjustedReportsWeeklyQualitySeparateFromDailyCounts(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	dates := []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09", "2026-01-12", "2026-01-13", "2026-01-14"}
	bars := make([]Bar, 0, len(dates))
	for _, date := range dates {
		bars = append(bars, Bar{Date: date, Open: 10, High: 11, Low: 9, Close: 10, Volume: 100, RawOpen: 10, RawHigh: 11, RawLow: 9, RawClose: 10, AdjustmentFactor: 1})
	}
	cutoff := time.Date(2026, 1, 14, 22, 0, 0, 0, location)
	result := AnalyzeAdjusted("TEST", bars, ptr(10), cutoff)
	weekly, daily := result.Weekly.Quality, result.Daily.Quality
	if weekly.InputBars != 2 || weekly.AcceptedBars != 1 || weekly.IncompleteWeeks != 1 || weekly.IncompleteBars != 1 || weekly.Status != "partial" || weekly.WeeklyExclusions.OpenWeek != 1 || weekly.WeeklyExclusions.MissingSessions != 0 {
		t.Fatalf("weekly quality=%+v, want two candidate weeks, one accepted, one incomplete", weekly)
	}
	if weekly.SourceInputBars != len(bars) || weekly.SourceAcceptedBars != daily.AcceptedBars || weekly.SourceIncompleteBars != daily.IncompleteBars {
		t.Fatalf("weekly source counts=%+v daily=%+v, want explicit daily counts", weekly, daily)
	}
	if weekly.RejectedBars != 0 || weekly.DuplicateBars != 0 || weekly.ReorderedBars != 0 {
		t.Fatalf("weekly bar counts leaked daily errors: %+v", weekly)
	}
	wantWarning := "1 个不完整交易周未用于结构确认（当前周未收盘 1，缺少预期交易日日线 0，周内数据链断开 0，历史起点位于周中 0）"
	warningCount := 0
	for _, warning := range result.Weekly.Warnings {
		if warning == wantWarning {
			warningCount++
		}
	}
	if warningCount != 1 {
		t.Fatalf("weekly warnings=%v, want exactly one explicit excluded-week reason", result.Weekly.Warnings)
	}
}
