package chananalysis

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// 2026-11-23，Codex：冻结本项目 ORCL 缓存，测试独立预警的实际出现时间。
// 通过 JSON 字段读取，既验证 API 输出，也能在尚无预警实现时明确复现遗漏。
func TestORCLStructuralWarningsUseOnlyAvailableHistory(t *testing.T) {
	data, err := os.ReadFile("testdata/orcl_daily_2026.json")
	if err != nil {
		t.Fatal(err)
	}
	var bars []Bar
	if err := json.Unmarshal(data, &bars); err != nil {
		t.Fatal(err)
	}
	wants := []struct {
		kind, date, observed, confirmed string
		price                           float64
	}{
		{"center_divergence_sell", "2026-09-08", "2026-09-10", "2026-09-18", 170.0709932151129},
		{"rebound_failure_sell", "2026-10-06", "2026-10-08", "2026-10-22", 146.3985436976994},
		{"rebound_failure_sell", "2026-10-29", "2026-11-02", "2026-11-11", 148.47999572753906},
	}
	firstObserved, firstConfirmed := map[string]string{}, map[string]string{}
	cutoff := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	incremental := NewIncrementalAnalyzer("ORCL")
	for end, bar := range bars {
		appended, err := incremental.Append(bar, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if bar.Date < "2026-09-01" {
			continue
		}
		result := AnalyzeAdjusted("ORCL", bars[:end+1], nil, cutoff)
		actual, _ := json.Marshal(appended.Daily.StructureWarnings)
		expected, _ := json.Marshal(result.Daily.StructureWarnings)
		if string(actual) != string(expected) {
			t.Fatalf("%s 增量预警与全量前缀不同", bar.Date)
		}
		for _, signal := range result.Daily.SignalsV4 {
			if !strings.HasPrefix(signal.Kind, "class") {
				t.Fatalf("预警混入严格六类点：%+v", signal)
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Daily struct {
				Warnings []V4Signal `json:"structure_warnings_v4"`
			} `json:"daily"`
		}
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		for _, warning := range payload.Daily.Warnings {
			for _, at := range []string{warning.ObservedAt, warning.ConfirmedAt, warning.PhaseAt} {
				if at > bar.Date {
					t.Fatalf("%s 使用未来预警时间：%+v", bar.Date, warning)
				}
			}
			for _, phase := range warning.PhaseHistory {
				if phase.At > bar.Date {
					t.Fatalf("%s 使用未来阶段：%+v", bar.Date, warning)
				}
			}
		}
		for _, want := range wants {
			key := want.kind + "|" + want.date
			var got *V4Signal
			for index := range payload.Daily.Warnings {
				warning := &payload.Daily.Warnings[index]
				if warning.Kind == want.kind && warning.Date == want.date && warning.SourceKind == "pen" {
					if got != nil {
						t.Fatalf("同一端点重复预警：%s", key)
					}
					got = warning
				}
			}
			if bar.Date < want.observed {
				if got != nil {
					t.Fatalf("%s 过早出现 %s：%+v", bar.Date, key, got)
				}
				continue
			}
			if got == nil {
				t.Fatalf("%s 遗漏 ORCL %.2f 的 %s 预警", bar.Date, want.price, want.kind)
			}
			if firstObserved[key] == "" {
				firstObserved[key] = bar.Date
			}
			if got.Symbol != "ORCL" || got.ObservedAt != want.observed || got.Price == nil || math.Abs(*got.Price-want.price) > 1e-8 {
				t.Fatalf("%s 预警位置/观察日期错误：%+v", bar.Date, got)
			}
			if bar.Date < want.confirmed {
				if got.ConfirmedAt != "" || got.Status != "observing" {
					t.Fatalf("%s 预警过早确认：%+v", bar.Date, got)
				}
			} else {
				if firstConfirmed[key] == "" {
					firstConfirmed[key] = bar.Date
				}
				if got.ConfirmedAt != want.confirmed {
					t.Fatalf("%s 预警确认时间错误：%+v", bar.Date, got)
				}
			}
		}
	}
	for _, want := range wants {
		key := want.kind + "|" + want.date
		if firstObserved[key] != want.observed || firstConfirmed[key] != want.confirmed {
			t.Fatalf("%s 首次出现=%s，确认=%s", key, firstObserved[key], firstConfirmed[key])
		}
	}
}

// 原始与复权入口均经过统一预警引擎，标的装饰不能遗漏独立数组。
func TestStructureWarningsAvailableFromRawAndAdjustedEntryPoints(t *testing.T) {
	data, err := os.ReadFile("testdata/orcl_daily_2026.json")
	if err != nil {
		t.Fatal(err)
	}
	var bars []Bar
	if err := json.Unmarshal(data, &bars); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for _, result := range []Result{Analyze("ORCL", bars, nil, cutoff), AnalyzeAdjusted("ORCL", bars, nil, cutoff)} {
		if len(result.Daily.StructureWarnings) == 0 {
			t.Fatal("统一入口没有输出独立预警")
		}
		for _, frame := range []Frame{result.Daily, result.Weekly} {
			if frame.StructureWarnings == nil {
				t.Fatal("预警数组不能为 null")
			}
			for _, warning := range frame.StructureWarnings {
				if warning.Symbol != "ORCL" || warning.RuleVersion != RecursivePreviewVersion {
					t.Fatalf("预警标的/版本遗漏：%+v", warning)
				}
			}
		}
	}
}
