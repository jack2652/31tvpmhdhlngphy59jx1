package chananalysis

import (
	"fmt"
	"sort"
)

// AlgorithmComparison reports descriptive differences between the legacy
// strict pen analysis and the recursive preview computed from the same bars.
// It is a structural comparison, not a performance or accuracy score.
type AlgorithmComparison struct {
	Method string                   `json:"method"`
	Daily  AlgorithmFrameComparison `json:"daily"`
	Weekly AlgorithmFrameComparison `json:"weekly"`
}

type AlgorithmFrameComparison struct {
	Timeframe            string                         `json:"timeframe"`
	LegacyRuleVersion    string                         `json:"legacy_rule_version"`
	RecursiveRuleVersion string                         `json:"recursive_rule_version"`
	Legacy               AlgorithmStructureCounts       `json:"legacy"`
	Recursive            AlgorithmStructureCounts       `json:"recursive"`
	Differences          []AlgorithmStructureDifference `json:"differences"`
}

type AlgorithmStructureCounts struct {
	Fractals int `json:"fractals"`
	Strokes  int `json:"strokes"`
	Segments int `json:"segments"`
	Centers  int `json:"centers"`
	Signals  int `json:"signals"`
}

type AlgorithmStructureDifference struct {
	Category       string   `json:"category"`
	LegacyCount    int      `json:"legacy_count"`
	RecursiveCount int      `json:"recursive_count"`
	LegacyOnly     []string `json:"legacy_only,omitempty"`
	RecursiveOnly  []string `json:"recursive_only,omitempty"`
	Explanation    string   `json:"explanation"`
}

func compareAlgorithmFrames(legacy, recursive Frame) AlgorithmFrameComparison {
	recursiveSegments := allLevelSegments(recursive)
	recursiveStrokes := allLevelStrokes(recursive)
	recursiveCenters := allLevelCenters(recursive)
	recursiveFractals := allLevelFractals(recursive)
	result := AlgorithmFrameComparison{
		Timeframe: recursive.Timeframe, LegacyRuleVersion: RuleVersion, RecursiveRuleVersion: RecursivePreviewVersion,
		Legacy:      AlgorithmStructureCounts{Fractals: len(legacy.Fractals), Strokes: len(legacy.Strokes), Centers: len(legacy.Centers), Signals: len(legacy.Signals)},
		Recursive:   AlgorithmStructureCounts{Fractals: len(recursiveFractals), Strokes: len(recursiveStrokes), Segments: len(recursiveSegments), Centers: len(recursiveCenters), Signals: len(recursive.SignalsV4)},
		Differences: []AlgorithmStructureDifference{},
	}
	result.Differences = append(result.Differences, compareKeys("fractals", fractalKeys(legacy.Fractals), fractalKeys(recursiveFractals), "两版都识别分型，但合并输入、确认时点或同类端点处理不同会改变端点。"))
	result.Differences = append(result.Differences, compareKeys("strokes", strokeKeys(legacy.Strokes), strokeKeys(recursiveStrokes), "旧笔与递归输入笔使用的连续链及端点确认上下文不同；两版笔字段仍保持各自语义。"))
	result.Differences = append(result.Differences, AlgorithmStructureDifference{Category: "segments", LegacyCount: 0, RecursiveCount: len(recursiveSegments), RecursiveOnly: segmentKeys(recursiveSegments), Explanation: "严格旧算法不计算特征序列线段；递归预览仅由已确认笔构造线段，未确认末段单独标记。"})
	result.Differences = append(result.Differences, compareKeys("centers", centerKeys(legacy.Centers), recursiveCenterKeys(recursiveCenters), "旧 centers 是笔中枢；递归 centers 来自同级走势段，级别与输入对象不同，不能按名称视为同类。"))
	result.Differences = append(result.Differences, AlgorithmStructureDifference{Category: "signals", LegacyCount: len(legacy.Signals), RecursiveCount: len(recursive.SignalsV4), LegacyOnly: signalKeys(legacy.Signals), RecursiveOnly: v4SignalKeys(recursive.SignalsV4), Explanation: "旧信号是笔级观察/第三类笔候选；递归信号按一、二、三类结构条件和确认阶段输出，不表示胜率或收益预测。"})
	return result
}

func allLevelSegments(frame Frame) []Segment {
	result := append([]Segment(nil), frame.Segments...)
	for _, level := range frame.RecursiveLevels {
		if level.Level > 0 {
			result = append(result, level.Segments...)
		}
	}
	return result
}
func allLevelStrokes(frame Frame) []Stroke {
	result := append([]Stroke(nil), frame.StrokesV4...)
	for _, level := range frame.RecursiveLevels {
		if level.Level > 0 {
			result = append(result, level.Strokes...)
		}
	}
	return result
}
func allLevelCenters(frame Frame) []RecursiveCenter {
	result := append([]RecursiveCenter(nil), frame.RecursiveCenters...)
	if len(frame.RecursiveLevels) == 0 {
		return result
	}
	// RecursiveCenters is the canonical flattened output; recursive level copies
	// are only a fallback for legacy fixtures that populate no flat collection.
	if len(result) == 0 {
		for _, level := range frame.RecursiveLevels {
			result = append(result, level.Centers...)
		}
	}
	return result
}
func allLevelFractals(frame Frame) []Fractal {
	result := append([]Fractal(nil), frame.FractalsV4...)
	for _, level := range frame.RecursiveLevels {
		if level.Level > 0 {
			for _, stroke := range level.Strokes {
				result = append(result, stroke.Start, stroke.End)
			}
		}
	}
	return result
}

func compareAdjustedAlgorithms(legacy, recursive Result) AlgorithmComparison {
	return AlgorithmComparison{Method: "同一复权行情分别计算 strict-old-bi-v3 与 " + RecursivePreviewVersion + "；列出结构数量和按日期/类型/价格匹配后的代表性差异。差异解释描述规则与结构层级，不评价准确率、收益或胜率。", Daily: compareAlgorithmFrames(legacy.Daily, recursive.Daily), Weekly: compareAlgorithmFrames(legacy.Weekly, recursive.Weekly)}
}

func compareKeys(category string, legacy, recursive []string, explanation string) AlgorithmStructureDifference {
	legacySet, recursiveSet := keySet(legacy), keySet(recursive)
	legacyOnly, recursiveOnly := []string{}, []string{}
	for _, key := range legacy {
		if !recursiveSet[key] {
			legacyOnly = append(legacyOnly, key)
		}
	}
	for _, key := range recursive {
		if !legacySet[key] {
			recursiveOnly = append(recursiveOnly, key)
		}
	}
	sort.Strings(legacyOnly)
	sort.Strings(recursiveOnly)
	return AlgorithmStructureDifference{Category: category, LegacyCount: len(legacy), RecursiveCount: len(recursive), LegacyOnly: representativeKeys(legacyOnly), RecursiveOnly: representativeKeys(recursiveOnly), Explanation: explanation}
}

func keySet(keys []string) map[string]bool {
	result := make(map[string]bool, len(keys))
	for _, key := range keys {
		result[key] = true
	}
	return result
}
func representativeKeys(keys []string) []string {
	if len(keys) > 8 {
		return append([]string(nil), keys[:8]...)
	}
	return append([]string(nil), keys...)
}
func fractalKeys(items []Fractal) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("%s|%s|%.8g", item.Date, item.Kind, item.Price))
	}
	return keys
}
func strokeKeys(items []Stroke) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("L%d|%s|%s|%s|%.8g|%.8g", item.Level, item.Direction, item.Start.Date, item.End.Date, item.Start.Price, item.End.Price))
	}
	return keys
}
func segmentKeys(items []Segment) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		state := "confirmed"
		if !item.Confirmed {
			state = "forming"
		}
		keys = append(keys, fmt.Sprintf("L%d|%s|%s..%s|%s", item.Level, item.Direction, item.Start, item.End, state))
	}
	return keys
}
func centerKeys(items []Center) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("L%d|%s|%s..%s|%.8g..%.8g", item.Level, item.InputKind, item.Start, item.End, item.Lower, item.Upper))
	}
	return keys
}
func recursiveCenterKeys(items []RecursiveCenter) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		state := "confirmed"
		if item.Forming {
			state = "forming"
		}
		keys = append(keys, fmt.Sprintf("L%d|%s|%s..%s|%.8g..%.8g|%s", item.Level, item.InputKind, item.Start, item.End, item.Lower, item.Upper, state))
	}
	return keys
}
func signalKeys(items []Signal) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("%s|%s|%s", item.Kind, item.Date, item.Status))
	}
	return keys
}
func v4SignalKeys(items []V4Signal) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("L%d|%s|%s|%s", item.Level, item.Kind, item.Date, item.Status))
	}
	return keys
}
