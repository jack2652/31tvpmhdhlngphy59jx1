package chananalysis

import (
	"fmt"
	"math"
	"sort"
	"time"

	"option-scope-go/internal/marketcalendar"
)

type mergedBar struct {
	high, low           float64
	highIndex, lowIndex int
	start, end          int
}

// Analyze uses completed regular-session daily bars only. The last pen remains
// provisional until a valid opposite pen fixes its endpoint. A pen center is
// formed only from confirmed pens; it is not a recursively defined Chan level.
func Analyze(symbol string, input []Bar, spot *float64, cutoff time.Time) Result {
	bars, rejected, openBars := completedBars(input, cutoff)
	result := Result{
		Symbol: symbol, Spot: spot, GeneratedAt: cutoff.UTC(), RuleVersion: RuleVersion,
		Rules: []string{
			"包含关系按已知方向合并；初始方向未知时保留并集；相等极值不构成严格分型",
			"严格旧笔：顶底分型中心至少间隔 4 根合并 K 线，端点必须是区间极值",
			"分型右侧 K 线结束合并后确认；末笔等待有效反向笔固定端点",
			"历史交易日缺口或严格旧笔无法连续接续时，仅用后续连续行情或笔链识别当前位置",
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
	if tail, gap := contiguousDailyTail(bars); gap != "" {
		bars = tail
		result.Warnings = append(result.Warnings, gap)
	}
	if spot == nil || !finitePositive(*spot) {
		spot = nil
		if len(bars) > 0 {
			spot = ptr(bars[len(bars)-1].Close)
		}
		result.Spot = spot
	}
	result.Daily = analyzeFrame(bars, "1d", "日线", spot)
	weekly, partial := weeklyBars(bars, cutoff)
	result.Weekly = analyzeFrame(weekly, "1wk", "周线", spot)
	if partial {
		result.Weekly.Warnings = append(result.Weekly.Warnings, "未完成或交易日日线缺失的周线未用于结构确认")
	}
	result.Summary = frameSummary(result.Daily, result.Weekly)
	return result
}

// AnalyzeIntraday builds a closed-bar intraday structure from Yahoo's
// 15-minute regular-session bars. The 4-hour view uses two complete session
// blocks (09:30–13:30 and 13:30–16:00 ET); partial blocks are omitted.
func AnalyzeIntraday(symbol string, input []Bar, period string, spot *float64, cutoff time.Time) (Result, error) {
	interval, label := 15*time.Minute, "15分钟"
	bars, rejected, unfinished := completedIntradayBars(input, cutoff, interval)
	if period == "4h" {
		label = "4小时"
		bars, rejected, unfinished = aggregateFourHourBars(input, cutoff)
	} else if period != "15m" {
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
	if rejected > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 根日期、时段或 OHLC 无效的盘中 K 线已排除", rejected))
	}
	if unfinished > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 根尚未收盘或所属时段未完整的 K 线已排除", unfinished))
	}
	if len(bars) == 0 {
		result.Daily = analyzeFrame(nil, period, label, spot)
		result.Daily.Warnings = append(result.Daily.Warnings, "上游尚无完整的已收盘盘中 K 线")
	} else {
		result.Daily = analyzeFrame(bars, period, label, spot)
	}
	result.Daily.Warnings = append(result.Daily.Warnings, result.Warnings...)
	result.Weekly = analyzeFrame(nil, "", "周线", spot)
	result.Summary = Summary{Alignment: "intraday", Label: label + "结构", Detail: "按已收盘盘中 K 线识别；日周结构另行参考"}
	return result, nil
}

func completedIntradayBars(input []Bar, cutoff time.Time, interval time.Duration) ([]Bar, int, int) {
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	byTime := make(map[string]Bar, len(input))
	rejected, unfinished := 0, 0
	for _, bar := range input {
		stamp, err := time.ParseInLocation("2006-01-02 15:04", bar.Date, location)
		if err != nil || !marketcalendar.IsTradingDay(stamp) || stamp.Hour() < 9 || stamp.Hour() > 15 ||
			stamp.Hour() == 9 && stamp.Minute() < 30 || stamp.Hour() == 15 && stamp.Minute() > 45 || stamp.Minute()%15 != 0 ||
			!finitePositive(bar.Open) || !finitePositive(bar.High) || !finitePositive(bar.Low) || !finitePositive(bar.Close) ||
			bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			rejected++
			continue
		}
		if cutoff.Before(stamp.Add(interval)) {
			unfinished++
			continue
		}
		if math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
			bar.Volume = 0
		}
		byTime[bar.Date] = bar
	}
	bars := make([]Bar, 0, len(byTime))
	for _, bar := range byTime {
		bars = append(bars, bar)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, rejected, unfinished
}

func aggregateFourHourBars(input []Bar, cutoff time.Time) ([]Bar, int, int) {
	const fifteenMinutes = 15 * time.Minute
	bars, rejected, unfinished := completedIntradayBars(input, cutoff, fifteenMinutes)
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	sessions := make(map[string]map[int]Bar)
	for _, bar := range bars {
		stamp, _ := time.ParseInLocation("2006-01-02 15:04", bar.Date, location)
		open := time.Date(stamp.Year(), stamp.Month(), stamp.Day(), 9, 30, 0, 0, location)
		index := int(stamp.Sub(open) / fifteenMinutes)
		if index < 0 || index >= 26 {
			continue
		}
		day := stamp.Format(time.DateOnly)
		if sessions[day] == nil {
			sessions[day] = make(map[int]Bar)
		}
		sessions[day][index] = bar
	}
	result := make([]Bar, 0, len(sessions)*2)
	for day, session := range sessions {
		dayStart, err := time.ParseInLocation(time.DateOnly, day, location)
		if err != nil {
			continue
		}
		for _, block := range []struct{ start, count int }{{0, 16}, {16, 10}} {
			complete := true
			var combined Bar
			for offset := 0; offset < block.count; offset++ {
				bar, found := session[block.start+offset]
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
				unfinished++
				continue
			}
			start := dayStart.Add(time.Duration(block.start) * fifteenMinutes)
			combined.Date = start.Format("2006-01-02 15:04")
			combined.CapturedAt = start.Add(time.Duration(block.count) * fifteenMinutes)
			result = append(result, combined)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	return result, rejected, unfinished
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func ptr(value float64) *float64 { return &value }

func completedBars(input []Bar, cutoff time.Time) ([]Bar, int, int) {
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	rejected, open := 0, 0
	byDate := make(map[string]Bar, len(input))
	for _, bar := range input {
		day, err := time.ParseInLocation(time.DateOnly, bar.Date, location)
		if err != nil || !marketcalendar.IsTradingDay(day) ||
			!finitePositive(bar.Open) || !finitePositive(bar.High) || !finitePositive(bar.Low) || !finitePositive(bar.Close) ||
			bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) || bar.High < bar.Low {
			rejected++
			continue
		}
		closeTime := day.Add(time.Duration(marketcalendar.RegularCloseMinute(day)) * time.Minute)
		if cutoff.Before(closeTime) || !bar.CapturedAt.IsZero() && bar.CapturedAt.Before(closeTime) {
			open++
			continue
		}
		if math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
			bar.Volume = 0
		}
		byDate[bar.Date] = bar
	}
	bars := make([]Bar, 0, len(byDate))
	for _, bar := range byDate {
		bars = append(bars, bar)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, rejected, open
}

// A missing trading session must not become an artificial neighbouring candle.
// Use the latest continuous run so no pen or weekly candle bridges a data gap.
func contiguousDailyTail(bars []Bar) ([]Bar, string) {
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	start := 0
	missing := ""
	for i := 1; i < len(bars); i++ {
		previous, _ := time.ParseInLocation(time.DateOnly, bars[i-1].Date, location)
		expected := previous.AddDate(0, 0, 1)
		for !marketcalendar.IsTradingDay(expected) {
			expected = expected.AddDate(0, 0, 1)
		}
		if expected.Format(time.DateOnly) != bars[i].Date {
			start, missing = i, expected.Format(time.DateOnly)
		}
	}
	if start == 0 {
		return bars, ""
	}
	return bars[start:], fmt.Sprintf("历史存在交易日缺口（最近缺口始于 %s），仅用 %s 起的连续日线识别当前结构", missing, bars[start].Date)
}

func weeklyBars(daily []Bar, cutoff time.Time) ([]Bar, bool) {
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	result := []Bar{}
	partial := false
	for start := 0; start < len(daily); {
		day, _ := time.ParseInLocation(time.DateOnly, daily[start].Date, location)
		monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		nextWeek := monday.AddDate(0, 0, 7).Format(time.DateOnly)
		end := start + 1
		for end < len(daily) && daily[end].Date < nextWeek {
			end++
		}
		lastTrading := monday.AddDate(0, 0, 4)
		for !marketcalendar.IsTradingDay(lastTrading) && !lastTrading.Before(monday) {
			lastTrading = lastTrading.AddDate(0, 0, -1)
		}
		weekClose := lastTrading.Add(time.Duration(marketcalendar.RegularCloseMinute(lastTrading)) * time.Minute)
		// Skip the truncated first calendar week and any week with missing
		// trading sessions; neither may silently masquerade as a full candle.
		expected := 0
		for date := monday; date.Before(monday.AddDate(0, 0, 5)); date = date.AddDate(0, 0, 1) {
			if marketcalendar.IsTradingDay(date) {
				expected++
			}
		}
		if cutoff.Before(weekClose) || daily[end-1].Date != lastTrading.Format(time.DateOnly) || end-start != expected {
			// The first available candle may start midway through the provider's
			// requested lookback. Skip that boundary without reporting a data gap.
			if cutoff.Before(weekClose) || start > 0 {
				partial = true
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
	return result, partial
}

func mergeBars(bars []Bar) []mergedBar {
	merged := []mergedBar{}
	direction := 0
	for i, bar := range bars {
		next := mergedBar{high: bar.High, low: bar.Low, highIndex: i, lowIndex: i, start: i, end: i}
		if len(merged) == 0 {
			merged = append(merged, next)
			continue
		}
		last := &merged[len(merged)-1]
		contains := (last.high >= next.high && last.low <= next.low) || (next.high >= last.high && next.low <= last.low)
		if !contains {
			if next.high > last.high {
				direction = 1
			} else {
				direction = -1
			}
			merged = append(merged, next)
			continue
		}
		// Before a directional pair exists, retain the union. Guessing an
		// initial direction from future candles would alter past extrema.
		if direction == 0 {
			if next.high > last.high {
				last.high, last.highIndex = next.high, i
			}
			if next.low < last.low {
				last.low, last.lowIndex = next.low, i
			}
			last.end = i
			continue
		}
		if direction >= 0 && next.high >= last.high || direction < 0 && next.high <= last.high {
			last.high, last.highIndex = next.high, i
		}
		if direction > 0 && next.low >= last.low || direction <= 0 && next.low <= last.low {
			last.low, last.lowIndex = next.low, i
		}
		last.end = i
	}
	return merged
}

func findFractals(bars []Bar, merged []mergedBar) []Fractal {
	fractals := []Fractal{}
	// The right neighbour must itself be finalized by a non-containing
	// candle. Otherwise a later inclusion can erase an apparently confirmed
	// fractal. This conservative delay is part of the public rule version.
	for i := 1; i+2 < len(merged); i++ {
		left, middle, right := merged[i-1], merged[i], merged[i+1]
		var kind string
		index, price := middle.highIndex, middle.high
		if middle.high > left.high && middle.high > right.high && middle.low > left.low && middle.low > right.low {
			kind = "top"
		} else if middle.low < left.low && middle.low < right.low && middle.high < left.high && middle.high < right.high {
			kind, index, price = "bottom", middle.lowIndex, middle.low
		} else {
			continue
		}
		fractals = append(fractals, Fractal{Kind: kind, Date: bars[index].Date, Price: price,
			ConfirmedAt: bars[merged[i+2].start].Date, MergedIndex: i, RawIndex: index})
	}
	return fractals
}

func validPen(start, end Fractal, merged []mergedBar) bool {
	if start.Kind == end.Kind || end.MergedIndex-start.MergedIndex < 4 {
		return false
	}
	low, high := math.Min(start.Price, end.Price), math.Max(start.Price, end.Price)
	if start.Kind == "bottom" && end.Price <= start.Price || start.Kind == "top" && end.Price >= start.Price {
		return false
	}
	for _, bar := range merged[start.MergedIndex : end.MergedIndex+1] {
		if bar.low < low || bar.high > high {
			return false
		}
	}
	return true
}

func buildStrokes(fractals []Fractal, merged []mergedBar, bars []Bar) ([]Stroke, string) {
	points := []Fractal{}
	confirmed := []string{}
	restartedAt := ""
	restart := func(fractal Fractal) {
		// A fast reversal can cross the pen's anchor before a valid opposite
		// pen exists. Restart the current chain instead of leaving it frozen
		// at an obsolete endpoint or joining across an invalid interval.
		points, confirmed = []Fractal{fractal}, nil
		restartedAt = fractal.Date
	}
	for _, fractal := range fractals {
		if len(points) == 0 {
			points = append(points, fractal)
			continue
		}
		last := points[len(points)-1]
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
	histogram := macdHistogram(bars)
	strokes := []Stroke{}
	for i := 1; i < len(points); i++ {
		start, end := points[i-1], points[i]
		direction := "up"
		if end.Price < start.Price {
			direction = "down"
		}
		stroke := Stroke{Start: start, End: end, Direction: direction, Low: math.Min(start.Price, end.Price), High: math.Max(start.Price, end.Price)}
		if i-1 < len(confirmed) {
			stroke.Confirmed, stroke.ConfirmedAt = true, confirmed[i-1]
		}
		for k := start.RawIndex; k <= end.RawIndex; k++ {
			if direction == "up" {
				stroke.MACDArea += math.Max(0, histogram[k])
			} else {
				stroke.MACDArea += math.Max(0, -histogram[k])
			}
		}
		strokes = append(strokes, stroke)
	}
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
		if !a.Confirmed || !b.Confirmed || !c.Confirmed {
			break
		}
		lower := math.Max(a.Low, math.Max(b.Low, c.Low))
		upper := math.Min(a.High, math.Min(b.High, c.High))
		if lower >= upper {
			i++
			continue
		}
		center := Center{Start: a.Start.Date, End: c.End.Date, Lower: lower, Upper: upper,
			ConfirmedAt: c.ConfirmedAt, StartStroke: i, EndStroke: i + 2, StrokeCount: 3}
		j := i + 3
		for j < len(strokes) && strokes[j].Confirmed && strokes[j].Low < upper && strokes[j].High > lower {
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
	frame := Frame{Period: period, Label: label, Status: "insufficient", BarCount: len(bars),
		Direction: "unknown", Structure: "等待笔中枢形成", Candles: []Bar{}, Fractals: []Fractal{}, Strokes: []Stroke{},
		Centers: []Center{}, Signals: []Signal{}, Scenarios: []Scenario{}, Warnings: []string{}}
	if len(bars) > 0 {
		frame.AsOf, frame.LastClose = bars[len(bars)-1].Date, ptr(bars[len(bars)-1].Close)
	}
	merged := mergeBars(bars)
	frame.MergedCount = len(merged)
	frame.Fractals = findFractals(bars, merged)
	var restartedAt string
	frame.Strokes, restartedAt = buildStrokes(frame.Fractals, merged, bars)
	if restartedAt != "" {
		frame.Warnings = append(frame.Warnings, "快速反向波动未能连续接续严格旧笔，仅用 "+restartedAt+" 起的连续笔链判断当前结构")
	}
	frame.Centers = buildCenters(frame.Strokes)
	frame.Position = locate(frame.Centers, spot)
	if len(frame.Strokes) > 0 {
		frame.Status = "developing"
		frame.Direction = frame.Strokes[len(frame.Strokes)-1].Direction
	}
	if len(frame.Centers) > 0 {
		frame.Status = "ready"
		frame.Signals = append(structuralSignals(frame.Strokes, frame.Centers), turningSignals(frame.Strokes, frame.Centers, bars)...)
		invalidateSignals(frame.Signals, bars, spot)
		frame.Scenarios = scenarios(frame.Position)
		frame.Structure = centerStructure(frame.Centers)
	} else {
		frame.Warnings = append(frame.Warnings, "尚未形成由三根已确认笔重叠构成的中枢，暂不推演中枢突破与回试")
	}
	if len(bars) < 60 {
		frame.Warnings = append(frame.Warnings, "历史长度不足 60 根，MACD 力度比较暂不输出")
	}
	if len(frame.Strokes) > 0 {
		frame.Warnings = append(frame.Warnings, "末笔端点仍可延伸，图中虚线表示形成中的笔")
	}
	if len(bars) > 240 {
		frame.Candles = bars[len(bars)-240:]
	} else {
		frame.Candles = bars
	}
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
	if !current.Confirmed {
		return signals
	}
	var center *Center
	for i := range centers {
		if centers[i].EndStroke < index {
			center = &centers[i]
		}
	}
	if center != nil && index >= 1 && center.EndStroke == index-1 {
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
		for _, bar := range bars {
			if bar.Date > since && broken(bar.Low, bar.High) {
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
