package chananalysis

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

// 风险必须出现在当时真正可见的日期，不能把事后确认回填为实时信号。
func TestEarlyRiskFirstVisibleOnClosedRightCandle(t *testing.T) {
	for _, tc := range []struct {
		symbol, file, pivot, observed string
		price, protection             float64
	}{
		{"QQQ", "qqq_daily_2026.json", "2026-10-06", "2026-10-07", 762.86, 751.75},
		{"ORCL", "orcl_daily_2026.json", "2026-09-08", "2026-09-09", 170.07122470756355, 159.7686961556582},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			var bars []Bar
			if err := json.Unmarshal(data, &bars); err != nil {
				t.Fatal(err)
			}
			for end, bar := range bars {
				if bar.Date < tc.pivot || bar.Date > tc.observed {
					continue
				}
				result := AnalyzeAdjusted(tc.symbol, bars[:end+1], nil, time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
				encoded, _ := json.Marshal(result)
				var payload struct {
					Daily struct {
						Early []struct {
							V4Signal
							Protection *float64 `json:"protection_price"`
							PivotDate  string   `json:"pivot_date"`
						} `json:"early_risk_warnings_v4"`
					} `json:"daily"`
				}
				if err := json.Unmarshal(encoded, &payload); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, risk := range payload.Daily.Early {
					if risk.PivotDate != tc.pivot {
						continue
					}
					found = true
					if risk.Date != tc.observed || risk.ObservedAt != tc.observed || risk.ConfirmedAt != "" || risk.Status != "observing" || risk.Price == nil || math.Abs(*risk.Price-tc.price) > .001 || risk.Protection == nil || math.Abs(*risk.Protection-tc.protection) > .001 {
						t.Fatalf("提前风险的时间/位置/状态错误：%+v", risk)
					}
				}
				if found != (bar.Date == tc.observed) {
					t.Fatalf("%s 顶部 %s 首次风险预警 found=%v，期待观察日 %s", bar.Date, tc.pivot, found, tc.observed)
				}
			}
		})
	}
}

// 全历史输出须与每个当日真实前缀相符，增量计算也不得借用未来中枢或力度。
func TestEarlyRiskCausalReplayAndIncremental(t *testing.T) {
	cutoff := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for _, symbol := range []string{"QQQ", "ORCL"} {
		data, err := os.ReadFile("testdata/" + map[string]string{"QQQ": "qqq_daily_2026.json", "ORCL": "orcl_daily_2026.json"}[symbol])
		if err != nil {
			t.Fatal(err)
		}
		var bars []Bar
		if err := json.Unmarshal(data, &bars); err != nil {
			t.Fatal(err)
		}
		full := AnalyzeAdjusted(symbol, bars, nil, cutoff).Daily.EarlyRiskWarnings
		incremental := NewIncrementalAnalyzer(symbol)
		for end, bar := range bars {
			added, err := incremental.Append(bar, cutoff)
			if err != nil {
				t.Fatal(err)
			}
			if bar.Date < "2026-09-01" {
				continue
			}
			prefix := AnalyzeAdjusted(symbol, bars[:end+1], nil, cutoff).Daily.EarlyRiskWarnings
			if !reflect.DeepEqual(prefix, added.Daily.EarlyRiskWarnings) {
				t.Fatalf("%s %s 增量风险不同于真实前缀", symbol, bar.Date)
			}
			seen := map[string]bool{}
			for _, risk := range prefix {
				if seen[risk.SignalID] || risk.Date != risk.ObservedAt || risk.ConfirmedAt != "" || risk.ObservedAt > bar.Date || risk.PhaseAt > bar.Date || risk.Symbol != symbol || risk.ProtectionPrice == nil {
					t.Fatalf("%s 风险契约错误：%+v", bar.Date, risk)
				}
				seen[risk.SignalID] = true
				for _, phase := range risk.PhaseHistory {
					if phase.At > bar.Date {
						t.Fatalf("%s 使用未来阶段", bar.Date)
					}
				}
				matched := false
				for _, future := range full {
					if future.SignalID != risk.SignalID {
						continue
					}
					matched = true
					if future.Date != risk.Date || future.PivotDate != risk.PivotDate || *future.ProtectionPrice != *risk.ProtectionPrice || !reflect.DeepEqual(future.Conditions, risk.Conditions) || !reflect.DeepEqual(future.StrengthRatio, risk.StrengthRatio) {
						t.Fatalf("%s 事后改变观察条件：%+v", bar.Date, risk)
					}
				}
				if !matched {
					t.Fatalf("%s 事后抹去风险事件", bar.Date)
				}
			}
		}
	}
}

// 未收盘右邻不能触发；包含变化不移动保护价，后续突破顶部必须终止风险。
func TestEarlyRiskIncompleteInclusionAndInvalidation(t *testing.T) {
	data, err := os.ReadFile("testdata/qqq_daily_2026.json")
	if err != nil {
		t.Fatal(err)
	}
	var bars []Bar
	if err := json.Unmarshal(data, &bars); err != nil {
		t.Fatal(err)
	}
	end := 0
	for end < len(bars) && bars[end].Date <= "2026-10-07" {
		end++
	}
	bars = bars[:end]
	lookup := func(result Result) *V4Signal {
		for index := range result.Daily.EarlyRiskWarnings {
			risk := &result.Daily.EarlyRiskWarnings[index]
			if risk.PivotDate == "2026-10-06" {
				return risk
			}
		}
		return nil
	}
	cutoff := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	open := append([]Bar(nil), bars...)
	open[len(open)-1].CapturedAt = time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	if lookup(AnalyzeAdjusted("QQQ", open, nil, cutoff)) != nil {
		t.Fatal("未收盘右邻触发了风险")
	}
	makeBar := func(date string, open, high, low, close float64) Bar {
		return Bar{Date: date, Open: open, High: high, Low: low, Close: close, Volume: 100, RawOpen: open, RawHigh: high, RawLow: low, RawClose: close, AdjustmentFactor: 1}
	}
	included := append(append([]Bar(nil), bars...), makeBar("2026-10-08", 754, 757, 752, 755))
	risk := lookup(AnalyzeAdjusted("QQQ", included, nil, cutoff))
	if risk == nil || risk.ObservedAt != "2026-10-07" || *risk.ProtectionPrice != 751.75 || risk.Status != "observing" {
		t.Fatalf("包含改变风险观察时间或保护价：%+v", risk)
	}
	broken := append(included, makeBar("2026-10-09", 755, 764, 750, 752))
	risk = lookup(AnalyzeAdjusted("QQQ", broken, nil, cutoff))
	if risk == nil || risk.Status != "invalidated" || risk.PhaseAt != "2026-10-09" || risk.ConfirmedAt != "" {
		t.Fatalf("创新高后风险未终止：%+v", risk)
	}
	gap := append(append([]Bar(nil), bars...), makeBar("2026-10-13", 754, 757, 749, 750))
	risk = lookup(AnalyzeAdjusted("QQQ", gap, nil, cutoff))
	if risk == nil || risk.Status != "invalidated" || risk.PhaseAt != "2026-10-13" {
		t.Fatalf("断链风险仍沿用：%+v", risk)
	}
	// 完整历史中10-08首次收盘失守；只强化风险，后续不得冒充严格确认。
	data, _ = os.ReadFile("testdata/qqq_daily_2026.json")
	json.Unmarshal(data, &bars)
	risk = lookup(AnalyzeAdjusted("QQQ", bars, nil, cutoff))
	if risk == nil || len(risk.PhaseHistory) < 2 || risk.PhaseHistory[1].Phase != "risk_triggered" || risk.PhaseHistory[1].At != "2026-10-08" || risk.ConfirmedAt != "" {
		t.Fatalf("收盘失守阶段错误：%+v", risk)
	}
}
