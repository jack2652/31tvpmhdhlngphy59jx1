package chananalysis

import (
	"math"
	"time"

	"option-scope-go/internal/marketcalendar"
)

type FormingBar struct {
	Bar
	// SourceDate 是尾价所用的最后一根 15 分钟行情的起始时间。
	SourceDate string
}

// FormingIntradayBar 用当前常规盘内已到达的 15 分钟行情生成尚未闭合的周期 K 线。
// 返回值仅供观察图层使用，不能进入收盘结构分析。
func FormingIntradayBar(input []Bar, period string, capturedAt, now time.Time) *FormingBar {
	duration, err := time.ParseDuration(period)
	if err != nil || capturedAt.IsZero() || capturedAt.After(now) {
		return nil
	}
	days, minute := civilDayMinute(now)
	capturedDay, capturedMinute := civilDayMinute(capturedAt)
	interval := int(duration / time.Minute)
	if interval != 15 && interval != 60 && interval != 120 && interval != 240 {
		return nil
	}
	closeMinute := marketcalendar.RegularCloseMinuteOrdinal(days)
	if capturedDay != days || !marketcalendar.IsTradingDayOrdinal(days) || minute < intradaySessionOpenMinute || minute >= closeMinute {
		return nil
	}
	start := intradaySessionOpenMinute + (minute-intradaySessionOpenMinute)/interval*interval
	if capturedMinute < start || start+interval > closeMinute {
		return nil
	}
	slots := make(map[int]Bar, interval/15)
	for _, bar := range input {
		day, at, valid := marketcalendar.ParseDateTime(bar.Date)
		if !valid || day != days || at < start || at > capturedMinute || at >= start+interval || at%15 != 0 {
			continue
		}
		if !bar.CapturedAt.IsZero() && (bar.CapturedAt.Before(dayTime(day, at)) || bar.CapturedAt.After(now)) {
			continue
		}
		if _, duplicate := slots[at]; duplicate || !finitePositive(bar.Open) || !finitePositive(bar.High) ||
			!finitePositive(bar.Low) || !finitePositive(bar.Close) || bar.High < math.Max(bar.Open, bar.Close) ||
			bar.Low > math.Min(bar.Open, bar.Close) || math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
			return nil
		}
		slots[at] = bar
	}
	combined, found := slots[start]
	if !found {
		return nil
	}
	sourceDate := combined.Date
	last := start + 15*(len(slots)-1)
	for at := start; at <= last; at += 15 {
		bar := slots[at]
		if bar.Date == "" {
			return nil
		}
		stamp := bar.CapturedAt
		if stamp.IsZero() {
			stamp = capturedAt
		}
		if at < last && stamp.Before(dayTime(days, at+15)) {
			return nil
		}
		if at == start {
			continue
		}
		combined.High = math.Max(combined.High, bar.High)
		combined.Low = math.Min(combined.Low, bar.Low)
		combined.Close = bar.Close
		combined.Volume += bar.Volume
		sourceDate = bar.Date
	}
	return &FormingBar{Bar: combined, SourceDate: sourceDate}
}

// IntradayChain 保留相邻周期的指标计算窗口，在已知缺口处重新预热。
func (bar FormingBar) IntradayChain(input, closed []Bar, period string, now time.Time) int {
	if len(closed) == 0 {
		return 0
	}
	last := closed[len(closed)-1]
	lastDay, lastMinute, _ := marketcalendar.ParseDateTime(last.Date)
	day, minute, _ := marketcalendar.ParseDateTime(bar.Date)
	if day == lastDay {
		if minute-lastMinute == int(intradayPeriodDuration(period)/time.Minute) {
			return last.Chain
		}
		return last.Chain + 1
	}
	// 1h 和 2h 的历史按完整交易日确认跨日连续性。
	if period == "1h" || period == "2h" {
		return last.Chain + 1
	}
	if period == "4h" {
		for candidate := marketcalendar.NextTradingDayOrdinal(lastDay); candidate < day; candidate = marketcalendar.NextTradingDayOrdinal(candidate) {
			if marketcalendar.RegularCloseMinuteOrdinal(candidate) >= intradaySessionOpenMinute+fourHourBlockBars*15 {
				return last.Chain + 1
			}
		}
	}
	recent := make([]Bar, 0, 52)
	for _, source := range input {
		if source.Date >= last.Date[:len(time.DateOnly)] && source.Date < bar.Date {
			recent = append(recent, source)
		}
	}
	prior, _, _, _ := completedIntradayBars(recent, now, 15*time.Minute)
	chained, _ := chainIntradayBars(append(prior, bar.Bar), 15*time.Minute)
	for _, source := range chained {
		if source.Date == last.Date && source.Chain == chained[len(chained)-1].Chain {
			return last.Chain
		}
	}
	return last.Chain + 1
}
