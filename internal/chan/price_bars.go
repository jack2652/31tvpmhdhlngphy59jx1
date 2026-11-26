package chananalysis

import "time"

// CompletedPriceBars 返回与缠论图表对齐的行情及最近的已收盘日线日期。
func CompletedPriceBars(input []Bar, period string, cutoff time.Time) (bars []Bar, lastClosedDay string) {
	daily, _, _, _, _, _ := completedBarsDetailed(input, cutoff)
	if len(daily) > 0 {
		lastClosedDay = daily[len(daily)-1].Date
	}
	if period != "1wk" {
		return daily, lastClosedDay
	}
	daily, _ = chainDailyBars(daily)
	weekly, _ := weeklyBars(daily, cutoff)
	return weekly, lastClosedDay
}
