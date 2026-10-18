package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/store"
)

type report struct {
	Status      string                              `json:"status,omitempty"`
	Error       string                              `json:"error,omitempty"`
	Symbol      string                              `json:"symbol"`
	Period      string                              `json:"period"`
	Bars        int                                 `json:"bars"`
	Start       string                              `json:"start"`
	End         string                              `json:"end"`
	RuleVersion string                              `json:"rule_version"`
	Benchmark   benchmarkReport                     `json:"benchmark"`
	Costs       costDisclosure                      `json:"costs"`
	Scope       string                              `json:"scope"`
	Method      string                              `json:"method"`
	Conclusion  string                              `json:"threshold_conclusion"`
	Calibration []chananalysis.ThresholdCalibration `json:"calibration"`
	Limitations []string                            `json:"limitations"`
}

type batchReport struct {
	Status  string   `json:"status"`
	Reports []report `json:"reports"`
}

type benchmarkReport struct {
	Symbol             string  `json:"symbol"`
	Bars               int     `json:"bars"`
	Start              string  `json:"start"`
	End                string  `json:"end"`
	Method             string  `json:"method"`
	ReturnPercent      float64 `json:"buy_hold_return_percent"`
	MaxDrawdownPercent float64 `json:"buy_hold_max_drawdown_percent"`
}

type costDisclosure struct {
	Commission string `json:"commission"`
	Slippage   string `json:"slippage"`
	Execution  string `json:"execution"`
	Status     string `json:"status"`
}

func main() {
	database := flag.String("database", "data/options.db", "SQLite database path")
	symbol := flag.String("symbol", "QQQ", "single symbol to evaluate")
	symbolsCSV := flag.String("symbols", "", "comma-separated symbols; emits a batch JSON report")
	barLimit := flag.Int("bars", defaultValidationBarLimit, "maximum daily bars per symbol (about five years)")
	period := flag.String("period", "1d", "bar timeframe: 1d, 15m, 30m, 60m, 1h, 2h, or 4h; intraday bars are aggregated from stored 15m history")
	allowForming := flag.Bool("allow-forming-segments", false, "report forming-segment exploration candidates separately from strict confirmed signals")
	flag.Parse()
	if *period != "1d" && *period != "15m" && *period != "30m" && *period != "60m" && *period != "1h" && *period != "2h" && *period != "4h" {
		fatal(fmt.Errorf("period must be 1d, 15m, 30m, 60m, 1h, 2h, or 4h; got %q", *period))
	}
	if *barLimit < 100 {
		fatal(fmt.Errorf("bars must be at least 100; got %d", *barLimit))
	}
	repository, err := store.Open(*database)
	if err != nil {
		fatal(err)
	}
	defer repository.Close()
	if *symbolsCSV != "" {
		symbols, parseErr := parseSymbols(*symbolsCSV)
		if parseErr != nil {
			fatal(parseErr)
		}
		output := batchReport{Status: "ok", Reports: make([]report, 0, len(symbols))}
		for _, item := range symbols {
			itemReport := buildReport(repository, item, *barLimit, *period, *allowForming)
			if itemReport.Status == "error" {
				output.Status = "partial"
			}
			output.Reports = append(output.Reports, itemReport)
		}
		writeJSON(output)
		return
	}
	writeJSON(buildReport(repository, *symbol, *barLimit, *period, *allowForming))
}

const defaultValidationBarLimit = 1300

func buildReport(repository *store.SQLite, symbol string, barLimit int, period string, allowForming bool) report {
	if period != "1d" {
		result, err := repository.LatestIntradayHistory(symbol, "15m", 10000)
		if err != nil {
			return failedReport(symbol, period, fmt.Errorf("load stored 15m history: %w", err))
		}
		values, ok := result["bars"].([]map[string]any)
		if !ok || len(values) == 0 {
			return failedReport(symbol, period, fmt.Errorf("no stored 15m history for %s; fetch and persist intraday data first", symbol))
		}
		source := make([]chananalysis.Bar, 0, len(values))
		for _, value := range values {
			date, _ := value["date"].(string)
			source = append(source, chananalysis.Bar{Date: date, Open: number(value["open"]), High: number(value["high"]), Low: number(value["low"]), Close: number(value["close"]), Volume: number(value["volume"])})
		}
		bars, quality, err := chananalysis.AggregateIntradayBars(source, period, time.Now())
		if err != nil {
			return failedReport(symbol, period, err)
		}
		calibration, err := chananalysis.CalibrateIntradayDivergenceThresholds(period, bars, []float64{0.6, 0.7, 0.8, 0.9}, chananalysis.CalibrationOptions{AllowFormingSegments: allowForming})
		if err != nil {
			return failedReport(symbol, period, fmt.Errorf("calibrate %s data (%d aggregated bars, %s–%s): %w", period, len(bars), firstBarDate(bars), lastBarDate(bars), err))
		}
		output := report{Status: "ok", Symbol: symbol, Period: period, Bars: len(bars), Start: firstBarDate(bars), End: lastBarDate(bars), RuleVersion: chananalysis.RecursivePreviewVersion,
			Benchmark: benchmarkReport{Method: "盘中样本未单独构造基准；禁止与日线基准统计混用"}, Costs: costDisclosure{Commission: "未建模", Slippage: "未建模", Execution: "未建模；不是可执行交易回测", Status: "directional_diagnostic_only"},
			Scope: "前缀回放的一类信号方向性诊断；forming 样本单列，不进入严格信号", Method: fmt.Sprintf("%s bars 聚合来源为存储的 15m 常规时段数据；信号在当前 bar 收盘后观察，从后续 bar 统计 5/10/20 根表现；70/30 时间切分", period), Calibration: calibration,
			Limitations: []string{fmt.Sprintf("来源为 SQLite 当前保存的 15m 历史，覆盖 %s 至 %s，共 %d 根源 bar；质量状态 %s、完整聚合 bar %d", firstBarDate(source), lastBarDate(source), len(source), quality.Status, len(bars)), "样本只代表当前存储标的与短期覆盖，不能外推为 300+ 标的或长期历史验证", "不是可执行策略净收益；未建模费用、滑点、执行、仓位和重叠资金", "样本不足30不报告胜率结论"}}
		output.Conclusion = thresholdConclusion(calibration)
		return output
	}
	result, err := repository.LatestChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", barLimit)
	if err != nil {
		return failedReport(symbol, period, fmt.Errorf("load adjusted history: %w", err))
	}
	values, ok := result["bars"].([]map[string]any)
	if !ok || len(values) == 0 {
		return failedReport(symbol, period, fmt.Errorf("no adjusted daily history for %s", symbol))
	}
	bars := make([]chananalysis.Bar, 0, len(values))
	for _, value := range values {
		date, _ := value["date"].(string)
		bars = append(bars, chananalysis.Bar{Date: date, Open: number(value["open"]), High: number(value["high"]), Low: number(value["low"]), Close: number(value["close"]), Volume: number(value["volume"]), RawOpen: number(value["raw_open"]), RawHigh: number(value["raw_high"]), RawLow: number(value["raw_low"]), RawClose: number(value["raw_close"]), AdjustmentFactor: number(value["adjustment_factor"])})
	}
	benchmarkHistory, err := repository.LatestChanAdjustedHistory("^GSPC", "1d", "split_dividend_auto", "go-yfinance", barLimit)
	if err != nil {
		return failedReport(symbol, period, fmt.Errorf("load S&P 500 adjusted history: %w", err))
	}
	benchmarkValues, ok := benchmarkHistory["bars"].([]map[string]any)
	if !ok || len(benchmarkValues) == 0 {
		return failedReport(symbol, period, fmt.Errorf("no adjusted daily history for benchmark ^GSPC"))
	}
	benchmarkBars := make([]chananalysis.Bar, 0, len(benchmarkValues))
	for _, value := range benchmarkValues {
		date, _ := value["date"].(string)
		benchmarkBars = append(benchmarkBars, chananalysis.Bar{Date: date, Close: number(value["close"])})
	}
	calibration, err := chananalysis.CalibrateDivergenceThresholdsWithOptions(symbol, bars, []float64{0.6, 0.7, 0.8, 0.9}, chananalysis.CalibrationOptions{AllowFormingSegments: allowForming})
	if err != nil {
		return failedReport(symbol, period, err)
	}
	output := report{Status: "ok", Symbol: symbol, Period: period, Bars: len(bars), Start: bars[0].Date, End: bars[len(bars)-1].Date,
		RuleVersion: chananalysis.RecursivePreviewVersion,
		Benchmark:   benchmarkReport{Symbol: "^GSPC", Method: "同期 S&P 500 买入持有，仅作背景；报告区间收盘回报和路径最大回撤"},
		Costs:       costDisclosure{Commission: "未建模", Slippage: "未建模", Execution: "未建模；不是可执行交易回测", Status: "directional_diagnostic_only"},
		Scope:       "前缀回放的一类信号方向性诊断，不模拟仓位、开平仓或组合曲线",
		Method:      "信号 observed_at 当日收盘后可见；从下一根日线收盘价开始统计 5/10/20 根收盘到收盘方向收益与窗口回撤；70/30 时间切分，样本不少于30才报告 Wilson 95% 区间",
		Calibration: calibration, Limitations: []string{"不是可执行策略净收益；未建模费用、滑点、执行、仓位和重叠资金", "下一根收盘价是收盘到收盘诊断，不包含下一交易日开盘价", "逐信号窗口回撤不是策略组合回撤", "单标的结果不足以得出普遍结论", "样本不足30不报告胜率结论", "阈值比较使用固定信号规则，不用样本外调参"}}
	output.Conclusion = thresholdConclusion(calibration)
	output.Benchmark.Bars, output.Benchmark.Start, output.Benchmark.End, output.Benchmark.ReturnPercent, output.Benchmark.MaxDrawdownPercent, err = benchmarkStats(bars, benchmarkBars)
	if err != nil {
		return failedReport(symbol, period, err)
	}
	return output
}

func failedReport(symbol, period string, err error) report {
	return report{Status: "error", Symbol: symbol, Period: period, Error: err.Error()}
}

func firstBarDate(bars []chananalysis.Bar) string {
	if len(bars) == 0 {
		return ""
	}
	return bars[0].Date
}
func lastBarDate(bars []chananalysis.Bar) string {
	if len(bars) == 0 {
		return ""
	}
	return bars[len(bars)-1].Date
}

func parseSymbols(csv string) ([]string, error) {
	seen := map[string]bool{}
	symbols := []string{}
	for _, raw := range strings.Split(csv, ",") {
		symbol := strings.TrimSpace(raw)
		if symbol == "" {
			return nil, fmt.Errorf("symbols contains an empty entry")
		}
		if !seen[symbol] {
			seen[symbol] = true
			symbols = append(symbols, symbol)
		}
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf("at least one symbol is required")
	}
	return symbols, nil
}

func writeJSON(value any) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	_, _ = os.Stdout.Write(append(encoded, '\n'))
}

func thresholdConclusion(calibration []chananalysis.ThresholdCalibration) string {
	maxSignals := 0
	for _, item := range calibration {
		// Signals observed under different thresholds are often the same
		// structures. Do not pool them as independent observations. Use the
		// largest single-threshold sample as a conservative report-level gate.
		signals := item.Train.Signals + item.OutOfSample.Signals
		if signals > maxSignals {
			maxSignals = signals
		}
	}
	if maxSignals == 0 {
		return "no_conclusion_no_observed_signals"
	}
	if maxSignals < 30 {
		return "descriptive_only_insufficient_sample"
	}
	return "descriptive_results_only_not_a_strategy_backtest"
}

func benchmarkStats(target, benchmark []chananalysis.Bar) (sampleBars int, start, end string, returnPercent, maxDrawdownPercent float64, err error) {
	if len(target) == 0 {
		return 0, "", "", 0, 0, fmt.Errorf("target history is empty")
	}
	start, end = target[0].Date, target[len(target)-1].Date
	if !chronologicalUnique(target) || !chronologicalUnique(benchmark) {
		return 0, "", "", 0, 0, fmt.Errorf("target and benchmark histories must be unique and chronological")
	}
	benchmarkByDate := make(map[string]chananalysis.Bar, len(benchmark))
	for _, bar := range benchmark {
		benchmarkByDate[bar.Date] = bar
	}
	period := make([]chananalysis.Bar, 0, len(target))
	for _, targetBar := range target {
		bar, exists := benchmarkByDate[targetBar.Date]
		if !exists || number(bar.Close) <= 0 || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			return 0, "", "", 0, 0, fmt.Errorf("S&P 500 benchmark is missing or invalid for target trading date %s; refusing to report mismatched benchmark statistics", targetBar.Date)
		}
		period = append(period, bar)
	}
	first := period[0].Close
	peak := first
	maxDrawdown := 0.0
	for _, bar := range period {
		if bar.Close <= 0 {
			continue
		}
		if bar.Close > peak {
			peak = bar.Close
		}
		drawdown := (bar.Close/peak - 1) * 100
		if drawdown < maxDrawdown {
			maxDrawdown = drawdown
		}
	}
	last := period[len(period)-1].Close
	if last > 0 {
		returnPercent = (last/first - 1) * 100
	}
	return len(period), period[0].Date, period[len(period)-1].Date, returnPercent, maxDrawdown, nil
}

func chronologicalUnique(bars []chananalysis.Bar) bool {
	previous := ""
	for _, bar := range bars {
		if bar.Date == "" || previous != "" && bar.Date <= previous {
			return false
		}
		previous = bar.Date
	}
	return true
}

func number(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case *float64:
		if number != nil {
			return *number
		}
	}
	return 0
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
