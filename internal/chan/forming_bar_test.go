package chananalysis

import (
	"testing"
	"time"
)

func TestFormingIntradayBar(t *testing.T) {
	at := func(hour, minute int) time.Time {
		// 感恩节次日 13:00 提前收盘。
		return time.Date(2026, time.November, 27, hour, minute, 0, 0, newYorkLocation())
	}
	bars := []Bar{
		{Date: "2026-11-27 09:30", Open: 100, High: 103, Low: 98, Close: 102, Volume: 20},
		{Date: "2026-11-27 09:45", Open: 102, High: 106, Low: 101, Close: 104, Volume: 30},
		{Date: "2026-11-27 10:00", Open: 104, High: 105, Low: 99, Close: 101, Volume: 10},
	}
	live := FormingIntradayBar(bars, "1h", at(10, 5), at(10, 7))
	if live == nil || live.Date != bars[0].Date || live.SourceDate != bars[2].Date || live.Open != 100 || live.High != 106 || live.Low != 98 || live.Close != 101 || live.Volume != 60 {
		t.Fatalf("意外形成中K线：%+v", live)
	}
	if got := FormingIntradayBar([]Bar{bars[0], bars[2]}, "1h", at(10, 5), at(10, 7)); got != nil {
		t.Fatalf("缺失槽位产生了形成中K线：%+v", got)
	}
	if FormingIntradayBar(bars, "4h", at(10, 5), at(10, 7)) != nil || FormingIntradayBar(bars, "1h", at(12, 50), at(13, 0)) != nil {
		t.Fatal("提前收盘不能产生不完整的周期")
	}
	// 旧的形成中快照可能因分批合并出现在新收盘数据之前。
	bars[0].CapturedAt = at(10, 15)
	bars[1].CapturedAt = at(9, 50)
	bars[2].CapturedAt = at(10, 15)
	if got, quality, err := AggregateIntradayBars(bars, "15m", at(13, 0)); err != nil ||
		quality.SourceIncompleteBars != 1 || len(got) != 2 || got[1].Date != bars[2].Date {
		t.Fatalf("形成中快照进入历史或误删已收盘数据：%+v，%+v，%v", got, quality, err)
	}
	if got := FormingIntradayBar(bars, "1h", at(10, 15), at(10, 16)); got != nil {
		t.Fatalf("未更新的前序 15 分钟快照不能用于实时聚合：%+v", got)
	}
}

func TestFormingIntradayBarRespectsKnownChainGaps(t *testing.T) {
	const layout = "2006-01-02 15:04"
	start, _ := time.ParseInLocation(layout, "2026-10-16 09:30", newYorkLocation())
	input := make([]Bar, 26)
	for index := range input {
		input[index] = Bar{Date: start.Add(time.Duration(index) * 15 * time.Minute).Format(layout), Open: 100, High: 101, Low: 99, Close: 100}
	}
	now := start.AddDate(0, 0, 3).Add(2 * time.Minute)
	for _, period := range []string{"15m", "4h"} {
		live := FormingBar{Bar: Bar{Date: "2026-10-19 09:30"}}
		closed, _, err := AggregateIntradayBars(input, period, now)
		if err != nil {
			t.Fatal(err)
		}
		lastChain := closed[len(closed)-1].Chain
		if got := live.IntradayChain(input, closed, period, now); got != lastChain {
			t.Errorf("%s 的完整前一交易日应与今早相邻，得到链 %d，预期 %d", period, got, lastChain)
		}
		if got := live.IntradayChain(input[1:], closed, period, now); got != lastChain+1 {
			t.Errorf("%s 的前一交易日存在缺口，应开启新链，得到 %d", period, got)
		}
	}
	live := FormingBar{Bar: Bar{Date: "2026-10-19 10:00"}}
	last := []Bar{{Date: "2026-10-19 09:30", Chain: 2}}
	if got := live.IntradayChain(nil, last, "15m", now); got != 3 {
		t.Errorf("缺失 09:45 周期应开启新链，得到 %d", got)
	}
}
