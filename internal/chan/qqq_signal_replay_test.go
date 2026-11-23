package chananalysis

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// 2026-11-23，Codex：从本项目保存的 QQQ 复权日线提取，复现用户所示顶部及下跌。
// 冻结数据用于算法回归，验证只读取夹具，不访问上游或本地业务数据库。
func TestQQQTopDivergenceAppearsOnlyInAvailablePrefix(t *testing.T) {
	data, err := os.ReadFile("testdata/qqq_daily_2026.json")
	if err != nil {
		t.Fatal(err)
	}
	var bars []Bar
	if err := json.Unmarshal(data, &bars); err != nil {
		t.Fatal(err)
	}
	if len(bars) != 224 {
		t.Fatalf("夹具 bars=%d，预期 224", len(bars))
	}
	cutoff := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	var firstObserved, firstConfirmed string
	var ratio float64
	for end, bar := range bars {
		if bar.Date < "2026-10-01" {
			continue
		}
		// 每次仅给算法截至当天的行情，不能通过完整行情上的 as_of 标签假装回放。
		analysis := AnalyzeAdjusted("QQQ", bars[:end+1], nil, cutoff)
		frame := analysis.Daily
		var top, thirdBuy *V4Signal
		for index := range frame.SignalsV4 {
			signal := &frame.SignalsV4[index]
			for _, at := range []string{signal.ObservedAt, signal.ConfirmedAt, signal.PhaseAt} {
				if at > bar.Date {
					t.Fatalf("%s 使用未来信号时间：%+v", bar.Date, signal)
				}
			}
			for _, phase := range signal.PhaseHistory {
				if phase.At > bar.Date {
					t.Fatalf("%s 使用未来阶段：%+v", bar.Date, signal)
				}
			}
			if signal.SourceKind != "pen" || signal.Level != 0 {
				continue
			}
			if signal.Kind == "class1_sell" && signal.Date == "2026-10-06" {
				top = signal
			}
			if signal.Kind == "class3_buy" && signal.Date == "2026-09-29" {
				thirdBuy = signal
			}
		}
		if bar.Date < "2026-10-08" {
			if top != nil {
				t.Fatalf("%s 过早出现顶部背驰：%+v", bar.Date, top)
			}
			continue
		}
		if top == nil || top.Price == nil || math.Abs(*top.Price-762.8599853515625) > 1e-8 || top.ObservedAt != "2026-10-08" || top.StrengthRatio == nil {
			t.Fatalf("%s 漏掉真实顶部观察信号：%+v", bar.Date, top)
		}
		if firstObserved == "" {
			firstObserved, ratio = bar.Date, *top.StrengthRatio
		}
		if math.Abs(*top.StrengthRatio-ratio) > 1e-12 {
			t.Fatalf("%s 后来行情改写已固定区间力度", bar.Date)
		}
		if bar.Date < "2026-10-22" {
			if top.Status != "observing" || top.ConfirmedAt != "" || top.Phase != "observation" {
				t.Fatalf("%s 顶部背驰过早确认：%+v", bar.Date, top)
			}
		} else {
			if firstConfirmed == "" {
				firstConfirmed = bar.Date
			}
			if top.Status != "divergence_confirmed" || top.ConfirmedAt != "2026-10-22" || top.PhaseAt != "2026-10-22" {
				t.Fatalf("%s 顶部背驰确认错误：%+v", bar.Date, top)
			}
		}
		if thirdBuy == nil || thirdBuy.ConfirmedAt != "2026-10-08" {
			t.Fatalf("%s 应保留 731.63 的三买历史：%+v", bar.Date, thirdBuy)
		}
		if bar.Date >= "2026-10-15" && (thirdBuy.Status != "invalidated" || thirdBuy.PhaseAt != "2026-10-15") {
			t.Fatalf("%s 三买失效未保留：%+v", bar.Date, thirdBuy)
		}
	}
	if firstObserved != "2026-10-08" || firstConfirmed != "2026-10-22" {
		t.Fatalf("首次出现=%s，首次确认=%s", firstObserved, firstConfirmed)
	}
}
