package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/marketcalendar"
)

type chanBenchmarkPoint struct {
	Date       string  `json:"date"`
	Close      float64 `json:"close"`
	IsForming  bool    `json:"is_forming,omitempty"`
	SourceDate string  `json:"source_date,omitempty"`
}

func (s *Service) chanBenchmark(c *gin.Context) {
	s.serveChanBenchmark(c, time.Now())
}

func (s *Service) serveChanBenchmark(c *gin.Context, now time.Time) {
	symbol := strings.ToUpper(c.Param("symbol"))
	if symbol != "QQQ" && symbol != "SPY" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "基准只支持 QQQ 或 SPY"})
		return
	}
	period := c.DefaultQuery("period", "1d")
	basis := c.DefaultQuery("basis", "raw")
	var bars []chananalysis.Bar
	var live *chanBenchmarkPoint
	var refreshing bool
	switch period {
	case "15m", "1h", "2h", "4h":
		if basis != "raw" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "盘中基准只支持未复权行情"})
			return
		}
		entry, _, _, pending := s.intradayFrame(symbol)
		input := intradayBars(entry)
		bars, _, _ = chananalysis.AggregateIntradayBars(input, period, now)
		if bar := chananalysis.FormingIntradayBar(input, period, entry.fetchedAt, now); bar != nil {
			live = &chanBenchmarkPoint{Date: bar.Date, Close: bar.Close, IsForming: true, SourceDate: bar.SourceDate}
		}
		refreshing = pending
	case "1d", "1wk":
		var rows []map[string]any
		var fetchedAt time.Time
		const adjustedBasis = "split_dividend_adjusted"
		captureField := "captured_at"
		switch basis {
		case adjustedBasis:
			entry, _, _, pending := s.chanAdjustedHistory(symbol)
			rows, fetchedAt, refreshing = entry.bars, entry.fetchedAt, pending || s.chanAdjustedJobActive(symbol)
			captureField = "fetched_at"
		case "raw":
			history, err := s.store.LatestHistoryWithTimestamps(symbol, 3000)
			if err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "基准日线缓存暂不可用"})
				return
			}
			rows, _ = history["bars"].([]map[string]any)
			fetchedAt, _ = historyTimestamp(history["fetched_at"])
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "基准价格口径无效"})
			return
		}
		input := make([]chananalysis.Bar, 0, len(rows))
		for _, row := range rows {
			date, _ := row["date"].(string)
			open, _ := historyMapNumber(row, "open")
			high, _ := historyMapNumber(row, "high")
			low, _ := historyMapNumber(row, "low")
			close, _ := historyMapNumber(row, "close")
			volume, _ := historyMapNumber(row, "volume")
			capturedAt, _ := historyTimestamp(row[captureField])
			if capturedAt.IsZero() {
				capturedAt = fetchedAt
			}
			bar := chananalysis.Bar{Date: date, Open: open, High: high, Low: low, Close: close, Volume: volume, CapturedAt: capturedAt}
			if basis == adjustedBasis {
				bar.AdjustmentFactor, _ = historyMapNumber(row, "adjustment_factor")
				bar.RawOpen, _ = historyMapNumber(row, "raw_open")
				bar.RawHigh, _ = historyMapNumber(row, "raw_high")
				bar.RawLow, _ = historyMapNumber(row, "raw_low")
				bar.RawClose, _ = historyMapNumber(row, "raw_close")
			}
			input = append(input, bar)
		}
		var lastClosedDay string
		bars, lastClosedDay = chananalysis.CompletedPriceBars(input, period, now)
		missingClose := lastClosedDay < chanExpectedHistoryDay(now)
		if basis == adjustedBasis {
			if _, supported := s.provider.(ChanAdjustedHistoryProvider); supported && !refreshing && missingClose && now.Sub(fetchedAt) >= time.Minute {
				refreshing = s.scheduleChanAdjustedRefresh(chanAdjustedCacheKey(symbol), symbol, true)
			}
		} else if _, supported := s.provider.(HistoryProvider); supported {
			maxAge := s.cfg.HistoryMaxAge
			if missingClose {
				maxAge = min(maxAge, time.Minute)
			}
			if fetchedAt.IsZero() || now.Sub(fetchedAt) >= maxAge {
				refreshing = s.scheduleHistoryRefresh(symbol, "5y", maxAge)
				if !refreshing {
					s.mu.Lock()
					refreshing = s.historyRefreshJobs[symbol]
					s.mu.Unlock()
				}
			}
		}
		if period == "1d" && marketcalendar.IsRegularSession(now) {
			location := newYorkLocation()
			date := now.In(location).Format(time.DateOnly)
			factor := 1.0
			if basis == adjustedBasis {
				factor = 0
				if len(rows) > 0 {
					row := rows[len(rows)-1]
					if validAdjustedHistoryRow(row) {
						factor, _ = historyMapNumber(row, "adjustment_factor")
						factorDate, _ := row["date"].(string)
						if factorDate != date && (factorDate >= date || factor < 1-1e-8 || factor > 1+1e-8) {
							factor = 0
						}
					}
				}
			}
			if factor > 0 {
				entry, _, _, pending := s.intradayFrame(symbol)
				refreshing = refreshing || pending
				if len(entry.frame) > 0 {
					row := entry.frame[len(entry.frame)-1]
					stamp, _ := row["date"].(string)
					capturedAt, _ := historyTimestamp(row["fetched_at"])
					if capturedAt.IsZero() {
						capturedAt = entry.fetchedAt
					}
					at, stampErr := time.ParseInLocation("2006-01-02 15:04", stamp, location)
					if close, ok := historyMapNumber(row, "close"); ok && close > 0 && stampErr == nil &&
						at.Format(time.DateOnly) == date && marketcalendar.IsRegularSession(at) &&
						!at.After(capturedAt) && !capturedAt.After(now) {
						live = &chanBenchmarkPoint{Date: date, Close: close * factor, IsForming: true, SourceDate: stamp}
					}
				}
			}
		}
	default:
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "基准周期无效"})
		return
	}
	points := make([]chanBenchmarkPoint, 0, len(bars))
	for _, bar := range bars {
		points = append(points, chanBenchmarkPoint{Date: bar.Date, Close: bar.Close})
	}
	c.JSON(http.StatusOK, gin.H{"symbol": symbol, "period": period, "basis": basis, "bars": points, "live_bar": live, "refreshing": refreshing})
}
