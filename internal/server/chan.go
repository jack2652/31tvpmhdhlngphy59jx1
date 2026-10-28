package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/store"
)

// intradayChanAnalysis serves closed-bar intraday structures. The
// Yahoo intraday window is fetched on demand, kept briefly in memory and
// persisted in SQLite so the default daily/weekly view never pays for it.
func (s *Service) intradayChanAnalysis(c *gin.Context) {
	if !s.validateQuery(c, positiveFloatQueryRule("spot")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	period := strings.ToLower(strings.TrimSpace(c.Query("period")))
	if period == "" {
		period = "15m"
	}
	if period != "15m" && period != "1h" && period != "2h" && period != "4h" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "period 只支持 15m、1h、2h 或 4h"})
		return
	}
	var spot *float64
	if raw, present := queryValue(c, "spot"); present {
		value, _ := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		spot = &value
	} else if quote, found, quoteErr := s.latestQuoteSnapshot(symbol); quoteErr == nil && found && quote.value.Price != nil {
		spot = quote.value.Price
	}
	frame, source, warning, refreshing := s.intradayFrame(symbol)
	result, analyzeErr := chananalysis.AnalyzeIntraday(symbol, intradayBars(frame), period, spot, time.Now())
	if analyzeErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": analyzeErr.Error()})
		return
	}
	if s.store != nil {
		recordChanStructureFrame(s.store, symbol, period, result.RuleVersion, result.Daily.Candles, &result.Daily)
	}
	warnings := append([]string{}, result.Warnings...)
	if warning != "" {
		warnings = append(warnings, warning)
	}
	c.JSON(http.StatusOK, gin.H{
		"symbol": symbol, "spot": result.Spot, "period": period, "frame": result.Daily,
		"rule_version": result.RuleVersion, "target_rule_version": chananalysis.RecursiveRuleVersion,
		"legacy_rule_version": result.LegacyRuleVersion, "parameters": result.Parameters,
		"price_coordinate": gin.H{"basis": "raw", "adjustment_type": "none", "status": "unadjusted"},
		"warnings":         warnings, "source": source, "fetched_at": frame.fetchedAt, "refreshing": refreshing,
	})
}

func (s *Service) intradayFrame(symbol string) (intradayCacheEntry, string, string, bool) {
	now := time.Now()
	// Both periods derive from the same 15-minute series, so one cache entry per
	// symbol covers them and switching periods reuses the fetch.
	s.intradayMu.Lock()
	memoryCached, memoryFound := s.intradayHistoryCache[symbol]
	if memoryFound && now.Sub(memoryCached.fetchedAt) < 3*time.Minute && !intradayPostCloseRefreshNeeded(memoryCached, now) {
		memoryCached.lastAccess = time.Now()
		s.intradayHistoryCache[symbol] = memoryCached
		s.intradayMu.Unlock()
		return memoryCached, "memory", "", false
	}
	s.intradayMu.Unlock()
	persisted := memoryCached
	persistedSource := "memory"
	if s.store != nil {
		if cached, err := s.store.LatestIntradayHistory(symbol, "15m", 10000); err == nil {
			if bars, _ := cached["bars"].([]map[string]any); len(bars) > 0 {
				fetchedAt, _ := historyTimestamp(cached["fetched_at"])
				if len(persisted.frame) == 0 || !fetchedAt.Before(persisted.fetchedAt) {
					persisted = intradayCacheEntry{frame: bars, fetchedAt: fetchedAt, lastAccess: time.Now()}
					persistedSource = "sqlite"
				}
				if !persisted.fetchedAt.IsZero() && now.Sub(persisted.fetchedAt) < 3*time.Minute && !intradayPostCloseRefreshNeeded(persisted, now) {
					s.intradayMu.Lock()
					if s.intradayHistoryCache == nil {
						s.intradayHistoryCache = make(map[string]intradayCacheEntry)
					}
					persisted.lastAccess = time.Now()
					s.intradayHistoryCache[symbol] = persisted
					evictOldestHistoryCacheEntry(s.intradayHistoryCache, symbol, boundedIntradayCacheEntries(s.cfg.LowMemory), func(entry intradayCacheEntry) time.Time { return entry.lastAccess })
					s.intradayMu.Unlock()
					return persisted, persistedSource, "", false
				}
			}
		}
	}
	if len(persisted.frame) > 0 {
		if _, ok := s.provider.(IntradayHistoryProvider); !ok {
			return persisted, "sqlite", "当前行情提供方不支持盘中历史，已显示缓存", false
		}
		refreshing := s.scheduleIntradayRefresh(symbol)
		warning := ""
		if !refreshing {
			warning = "盘中历史仍在刷新冷却期内，暂时显示 SQLite 缓存"
		}
		return persisted, persistedSource, warning, refreshing
	}
	if _, ok := s.provider.(IntradayHistoryProvider); !ok {
		return intradayCacheEntry{}, "none", "当前行情提供方不支持盘中历史，盘中周期结构暂不可用", false
	}
	if !s.scheduleIntradayRefresh(symbol) {
		return intradayCacheEntry{}, "none", "盘中历史刷新暂不可用，请稍候重试", false
	}
	return intradayCacheEntry{}, "none", "盘中历史正在获取，请稍候", true
}

func intradayPostCloseRefreshNeeded(entry intradayCacheEntry, now time.Time) bool {
	if len(entry.frame) == 0 || entry.fetchedAt.IsZero() {
		return false
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	localNow := now.In(location)
	if !marketcalendar.IsTradingDay(localNow) {
		return false
	}
	closeMinute := marketcalendar.RegularCloseMinute(localNow)
	closeAt := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location).
		Add(time.Duration(closeMinute) * time.Minute)
	if localNow.Before(closeAt) || !entry.fetchedAt.Before(closeAt) {
		return false
	}
	today := localNow.Format(time.DateOnly)
	date, _ := entry.frame[len(entry.frame)-1]["date"].(string)
	return strings.HasPrefix(date, today)
}

// scheduleIntradayRefresh fetches stale intraday history without holding the
// request open, so SQLite data can be rendered immediately. Only one fetch
// per symbol may run at a time; failed attempts are briefly cooled down.
func (s *Service) scheduleIntradayRefresh(symbol string) bool {
	if s == nil || s.provider == nil || s.background == nil {
		return false
	}
	s.intradayMu.Lock()
	if s.intradayRefreshJobs == nil {
		s.intradayRefreshJobs = make(map[string]bool)
	}
	if s.intradayRefreshAfter == nil {
		s.intradayRefreshAfter = make(map[string]time.Time)
	}
	if s.intradayRefreshJobs[symbol] {
		s.intradayMu.Unlock()
		return true
	}
	if time.Now().Before(s.intradayRefreshAfter[symbol]) {
		s.intradayMu.Unlock()
		return true
	}
	s.intradayRefreshJobs[symbol] = true
	s.intradayMu.Unlock()
	// Serialize WaitGroup.Add with Service.Close so shutdown cannot begin
	// waiting while a newly scheduled background refresh is being added.
	s.mu.Lock()
	if s.background.Err() != nil {
		s.intradayMu.Lock()
		delete(s.intradayRefreshJobs, symbol)
		s.intradayMu.Unlock()
		s.mu.Unlock()
		return false
	}
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.intradayMu.Lock()
			delete(s.intradayRefreshJobs, symbol)
			s.intradayMu.Unlock()
		}()
		provider, ok := s.provider.(IntradayHistoryProvider)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.background, 3*time.Minute)
		defer cancel()
		result, err := provider.IntradayHistory(ctx, symbol, "15m")
		if err != nil || len(result.Bars) == 0 {
			if err == nil {
				err = errors.New("盘中历史提供方返回空数据")
			}
			log.Printf("background intraday history refresh failed symbol=%s: %v", symbol, err)
			s.intradayMu.Lock()
			s.intradayRefreshAfter[symbol] = time.Now().Add(30 * time.Second)
			trimSymbolStateCache(s.intradayRefreshAfter, symbol, s.cfg.LowMemory, func(at time.Time) time.Time { return at })
			s.intradayMu.Unlock()
			return
		}
		entry := intradayCacheEntry{frame: result.Bars, fetchedAt: result.FetchedAt, lastAccess: time.Now()}
		if entry.fetchedAt.IsZero() {
			entry.fetchedAt = time.Now().UTC()
		}
		if s.store != nil && !entry.fetchedAt.IsZero() && len(entry.frame) > 0 {
			if err := s.store.WriteIntradayHistory(symbol, "15m", entry.frame, entry.fetchedAt); err != nil {
				log.Printf("background intraday history SQLite write failed symbol=%s: %v", symbol, err)
			}
		}
		s.intradayMu.Lock()
		if s.intradayHistoryCache == nil {
			s.intradayHistoryCache = make(map[string]intradayCacheEntry)
		}
		if len(entry.frame) > 0 {
			s.intradayHistoryCache[symbol] = entry
			evictOldestHistoryCacheEntry(s.intradayHistoryCache, symbol, boundedIntradayCacheEntries(s.cfg.LowMemory), func(entry intradayCacheEntry) time.Time { return entry.lastAccess })
		}
		delete(s.intradayRefreshAfter, symbol)
		s.intradayMu.Unlock()
	}()
	return true
}

func intradayBars(entry intradayCacheEntry) []chananalysis.Bar {
	bars := make([]chananalysis.Bar, 0, len(entry.frame))
	for _, raw := range entry.frame {
		bar := chananalysis.Bar{}
		bar.Date, _ = raw["date"].(string)
		bar.Open, _ = historyMapNumber(raw, "open")
		bar.High, _ = historyMapNumber(raw, "high")
		bar.Low, _ = historyMapNumber(raw, "low")
		bar.Close, _ = historyMapNumber(raw, "close")
		bar.Volume, _ = historyMapNumber(raw, "volume")
		bars = append(bars, bar)
	}
	return bars
}

func (s *Service) intradayDailyBarFallback(symbol string, quote model.Quote) *model.DailyBar {
	state := strings.ToUpper(strings.TrimSpace(quote.MarketState))
	if state != "REGULAR" && state != "POST" {
		return nil
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	date := time.Now().In(location).Format(time.DateOnly)
	entry, _, _, _ := s.intradayFrame(symbol)
	var first, latest *chananalysis.Bar
	var high, low, volume float64
	for index := range entry.frame {
		bar := chananalysis.Bar{}
		bar.Date, _ = entry.frame[index]["date"].(string)
		if len(bar.Date) < len(date) || bar.Date[:len(date)] != date {
			continue
		}
		at, parseErr := time.ParseInLocation("2006-01-02 15:04", bar.Date, location)
		if parseErr != nil || at.Hour()*60+at.Minute() < 9*60+30 || at.Hour()*60+at.Minute() >= 16*60 {
			continue
		}
		bar.Open, _ = historyMapNumber(entry.frame[index], "open")
		bar.High, _ = historyMapNumber(entry.frame[index], "high")
		bar.Low, _ = historyMapNumber(entry.frame[index], "low")
		bar.Close, _ = historyMapNumber(entry.frame[index], "close")
		bar.Volume, _ = historyMapNumber(entry.frame[index], "volume")
		if first == nil {
			first = &bar
			low = bar.Low
		}
		latest = &bar
		high = math.Max(high, bar.High)
		low = math.Min(low, bar.Low)
		volume += bar.Volume
	}
	if first == nil || latest == nil || !finitePositive(first.Open) || !finitePositive(high) || !finitePositive(low) ||
		!finitePositive(latest.Close) || high < low || volume < 0 {
		return nil
	}
	open, close := first.Open, latest.Close
	if finitePositivePointer(quote.TodayOpen) {
		open = *quote.TodayOpen
	}
	forming := state == "REGULAR"
	if forming && validMarketPrice(quote.Price) {
		close = *quote.Price
		high = math.Max(high, close)
		low = math.Min(low, close)
	}
	if high < math.Max(open, close) || low > math.Min(open, close) {
		return nil
	}
	return &model.DailyBar{Date: date, Open: open, High: high, Low: low, Close: close, Volume: volume, IsForming: forming}
}

func finitePositivePointer(value *float64) bool {
	return value != nil && finitePositive(*value)
}

// chanAdjustedAnalysis serves the independent adjusted daily history used by
// recursive Chan analysis. Existing /chan and quote paths remain raw-price.
func (s *Service) chanAdjustedAnalysis(c *gin.Context) {
	if !s.validateQuery(c, positiveFloatQueryRule("spot"), boolQueryRule("compact")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	compact, err := queryBool(c, "compact", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	entry, source, warning, refreshing := s.chanAdjustedHistory(symbol)
	if len(entry.bars) == 0 {
		_, supportsHistory := s.provider.(ChanAdjustedHistoryProvider)
		if refreshing || supportsHistory && strings.Contains(warning, "正在获取") {
			retryAfter := 15
			if !refreshing {
				retryAfter = 30
				cacheKey := symbol + "|1d|split_dividend_auto|go-yfinance"
				s.chanAdjustedMu.Lock()
				retryAt := s.chanAdjustedRetryAfter[cacheKey]
				s.chanAdjustedMu.Unlock()
				if remaining := time.Until(retryAt); remaining > 0 {
					retryAfter = int((remaining + time.Second - 1) / time.Second)
				}
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusAccepted, gin.H{"symbol": symbol, "ready": false, "retry_after_ms": retryAfter * 1000, "rule_version": chananalysis.RecursivePreviewVersion, "target_rule_version": chananalysis.RecursiveRuleVersion, "quality": gin.H{"status": "pending", "price_basis": "unavailable"}, "warnings": []string{warning}})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"symbol": symbol, "ready": false, "rule_version": chananalysis.RecursivePreviewVersion, "target_rule_version": chananalysis.RecursiveRuleVersion, "quality": gin.H{"status": "degraded", "price_basis": "unavailable"}, "warnings": []string{warning}})
		return
	}
	spotRaw, present := queryValue(c, "spot")
	var rawSpot *float64
	if present {
		value, _ := strconv.ParseFloat(strings.TrimSpace(spotRaw), 64)
		rawSpot = &value
	} else if quote, found, quoteErr := s.latestQuoteSnapshot(symbol); quoteErr == nil && found {
		rawSpot = quote.value.Price
	}
	bars := make([]chananalysis.Bar, 0, len(entry.bars))
	for _, raw := range entry.bars {
		bar := chananalysis.Bar{}
		bar.Date, _ = raw["date"].(string)
		bar.Open, _ = historyMapNumber(raw, "open")
		bar.High, _ = historyMapNumber(raw, "high")
		bar.Low, _ = historyMapNumber(raw, "low")
		bar.Close, _ = historyMapNumber(raw, "close")
		bar.Volume, _ = historyMapNumber(raw, "volume")
		bar.AdjustmentFactor, _ = historyMapNumber(raw, "adjustment_factor")
		bar.RawOpen, _ = historyMapNumber(raw, "raw_open")
		bar.RawHigh, _ = historyMapNumber(raw, "raw_high")
		bar.RawLow, _ = historyMapNumber(raw, "raw_low")
		bar.RawClose, _ = historyMapNumber(raw, "raw_close")
		bars = append(bars, bar)
	}
	var adjustedSpot *float64
	factorDate, factorSource := "", ""
	last := entry.bars[len(entry.bars)-1]
	factor := numberValue(last["adjustment_factor"])
	if !validAdjustedHistoryRow(last) {
		factor = nil
	} else {
		factorDate, _ = last["date"].(string)
		factorSource = source
	}
	if rawSpot != nil && finitePositive(*rawSpot) {
		if factor != nil {
			adjusted := *rawSpot * *factor
			adjustedSpot = &adjusted
		}
	}
	cutoff := time.Now()
	// Daily signals use the directly mapped 4-hour structures. Reuse the
	// cached 15-minute history, but move it into the same adjusted price
	// coordinate as the daily frame before aggregating it. Bars without a
	// matching, validated daily adjustment factor are excluded. These inputs
	// determine the structural cache key; the live quote is deliberately not
	// part of it because RebaseSpot handles that cheap, quote-dependent step.
	intradayEntry, _, _, _ := s.intradayFrame(symbol)
	var adjustedIntraday []chananalysis.Bar
	skippedIntraday := 0
	if len(intradayEntry.frame) > 0 {
		adjustedIntraday, skippedIntraday = adjustedIntradayBars(intradayBars(intradayEntry), bars)
	}
	marketHistory, marketSource, marketWarning, marketRefreshing := s.chanMarketHistory()
	marketBars := adjustedAnalysisBars(marketHistory.bars, cutoff)
	marketSeries := chananalysis.ClassifyMarketEnvironment(marketBars)
	keyDailyBars := completedChanDailyBarsForCache(bars, cutoff)
	keyIntradayBars := adjustedIntraday
	if len(adjustedIntraday) > 0 {
		if completed, _, aggregateErr := chananalysis.AggregateIntradayBars(adjustedIntraday, "4h", cutoff); aggregateErr == nil {
			// The lower timeframe attached to the daily result only consumes
			// complete 4-hour intervals. Excluding a still-forming interval keeps
			// quote ticks from invalidating the structural cache every request.
			keyIntradayBars = completed
		}
	}
	cacheKey := chanAnalysisCacheKey(symbol, keyDailyBars, keyIntradayBars, marketBars, cutoff)
	result, analysisCache, analysisErr := s.getOrBuildChanAnalysis(cacheKey, func() (chananalysis.Result, error) {
		result := chananalysis.AnalyzeAdjustedStructure(symbol, bars, cutoff)
		if len(adjustedIntraday) > 0 {
			lowerResult, lowerErr := chananalysis.AnalyzeIntraday(symbol, adjustedIntraday, "4h", nil, cutoff)
			if lowerErr == nil {
				lowerResult.Daily.Quality.PriceBasis = "split_dividend_adjusted"
				chananalysis.AttachMappedTimeframe(&result.Daily, lowerResult.Daily)
				if skippedIntraday > 0 {
					result.Daily.Warnings = append(result.Daily.Warnings, fmt.Sprintf("%d 根 15 分钟 K 线因缺少对应日线复权因子未参与跨周期确认", skippedIntraday))
				}
			}
		}
		attachMarketEnvironment(&result.Daily, marketSeries)
		attachMarketEnvironment(&result.Weekly, marketSeries)
		if s.store != nil {
			recordChanStructureFrame(s.store, symbol, "1d", result.RuleVersion, bars, &result.Daily)
			recordChanStructureFrame(s.store, symbol, "1wk", result.RuleVersion, result.Weekly.Candles, &result.Weekly)
		}
		for index := range result.Daily.SignalsV4 {
			result.Daily.SignalsV4[index].Symbol = symbol
		}
		for index := range result.Weekly.SignalsV4 {
			result.Weekly.SignalsV4[index].Symbol = symbol
		}
		return result, nil
	})
	if analysisErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": analysisErr.Error()})
		return
	}
	// RebaseSpot mutates only the returned value. Clone warning slices first so
	// request-specific quality warnings never change the shared cache entry.
	result.Warnings = append([]string(nil), result.Warnings...)
	result.Daily.Warnings = append([]string(nil), result.Daily.Warnings...)
	result.Weekly.Warnings = append([]string(nil), result.Weekly.Warnings...)
	chananalysis.RebaseSpot(&result, adjustedSpot)
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	coordinateStatus := "available"
	priceCoordinate := gin.H{"basis": "split_dividend_adjusted", "adjustment_type": "split_dividend_auto", "factor_date": factorDate, "factor_source": factorSource, "adjustment_factor": factor, "raw_spot": rawSpot, "adjusted_spot": adjustedSpot}
	if factor == nil {
		coordinateStatus = "degraded"
		result.Warnings = append(result.Warnings, "缺少有效的现价复权因子；当前价未与复权结构比较，数据质量已降级")
		result.Daily.Quality.Status, result.Weekly.Quality.Status = "degraded", "degraded"
	} else if rawSpot == nil || !finitePositive(*rawSpot) {
		coordinateStatus = "spot_unavailable"
	}
	priceCoordinate["status"] = coordinateStatus
	if rawSpot != nil && adjustedSpot == nil && factor != nil {
		result.Warnings = append(result.Warnings, "当前现价无效；未与复权结构比较")
	}
	if marketWarning != "" {
		result.Warnings = append(result.Warnings, marketWarning)
	}
	c.Header("X-Chan-Analysis-Cache", analysisCache)
	response := gin.H{"symbol": symbol, "ready": true, "analysis_cache": analysisCache, "rule_version": result.RuleVersion, "target_rule_version": chananalysis.RecursiveRuleVersion, "legacy_rule_version": result.LegacyRuleVersion, "parameters": result.Parameters, "rules": result.Rules, "algorithm_comparison": result.AlgorithmComparison, "market_environment": latestMarketEnvironment(marketSeries, result.Daily.AsOf), "market_history": gin.H{"symbol": "^GSPC", "source": marketSource, "fetched_at": marketHistory.fetchedAt, "refreshing": marketRefreshing}, "price_coordinate": priceCoordinate, "summary": result.Summary, "warnings": result.Warnings, "history": gin.H{"source": source, "provider": "go-yfinance", "timeframe": "1d", "adjustment_type": "split_dividend_auto", "fetched_at": entry.fetchedAt, "refreshing": refreshing}}
	if compact {
		response["daily"] = compactChanFrame{Frame: result.Daily}
		response["weekly"] = compactChanFrame{Frame: result.Weekly}
	} else {
		response["daily"], response["weekly"] = result.Daily, result.Weekly
	}
	c.JSON(http.StatusOK, response)
}

// compactChanFrame omits heavy fields the browser chart does not read. The
// embedded Frame keeps every field the chart and signal panels consume.
type compactChanFrame struct {
	chananalysis.Frame
	Fractals     []chananalysis.Fractal     `json:"fractals,omitempty"`
	FractalsV4   []chananalysis.Fractal     `json:"fractals_v4,omitempty"`
	StrokesV4    []chananalysis.Stroke      `json:"strokes_v4,omitempty"`
	Strengths    []chananalysis.Strength    `json:"strengths_v4,omitempty"`
	TrendContext *chananalysis.TrendContext `json:"trend_context_v4,omitempty"`
	MACD         []chananalysis.MACDPoint   `json:"macd_v4,omitempty"`
}

// chanAnalysisCacheKey fingerprints every input that can affect the expensive
// structural pass. The live quote is intentionally excluded: quote-dependent
// positions and invalidation are refreshed by chananalysis.RebaseSpot.
func chanAnalysisCacheKey(symbol string, daily, intraday, market []chananalysis.Bar, cutoff time.Time) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	local := cutoff.In(location)
	bucketMinute := (local.Minute() / 15) * 15
	bucket := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), bucketMinute, 0, 0, location).Format(time.RFC3339)
	payload := struct {
		Version  string             `json:"version"`
		Symbol   string             `json:"symbol"`
		Bucket   string             `json:"bucket"`
		Daily    []chananalysis.Bar `json:"daily"`
		Intraday []chananalysis.Bar `json:"intraday"`
		Market   []chananalysis.Bar `json:"market"`
	}{
		Version:  chananalysis.RecursivePreviewVersion + "|" + chananalysis.MarketEnvironmentRuleVersion,
		Symbol:   symbol,
		Bucket:   bucket,
		Daily:    daily,
		Intraday: intraday,
		Market:   market,
	}
	state := chanAnalysisHashPool.Get().(*chanAnalysisHashState)
	state.digest.Reset()
	state.writer.reset()
	_ = state.encoder.Encode(payload)
	var digest [sha256.Size]byte
	digestBytes := state.digest.Sum(digest[:0])
	key := "chan-adjusted-v2:" + hex.EncodeToString(digestBytes)
	chanAnalysisHashPool.Put(state)
	return key
}

type chanAnalysisHashWriter struct {
	digest  hash.Hash
	last    [1]byte
	pending bool
}

// Write 延迟写入最后一个字节，以去掉 Encoder 追加的换行并保留旧缓存键。
func (writer *chanAnalysisHashWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if writer.pending {
		_, _ = writer.digest.Write(writer.last[:])
	}
	if len(data) > 1 {
		_, _ = writer.digest.Write(data[:len(data)-1])
	}
	writer.last[0] = data[len(data)-1]
	writer.pending = true
	return len(data), nil
}

func (writer *chanAnalysisHashWriter) reset() {
	writer.last[0] = 0
	writer.pending = false
}

type chanAnalysisHashState struct {
	digest  hash.Hash
	writer  chanAnalysisHashWriter
	encoder *json.Encoder
}

var chanAnalysisHashPool = sync.Pool{New: func() any {
	state := &chanAnalysisHashState{digest: sha256.New()}
	state.writer.digest = state.digest
	state.encoder = json.NewEncoder(&state.writer)
	return state
}}

func completedChanDailyBarsForCache(input []chananalysis.Bar, cutoff time.Time) []chananalysis.Bar {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	local := cutoff.In(location)
	cutoffDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	result := make([]chananalysis.Bar, 0, len(input))
	for _, bar := range input {
		day, parseErr := time.ParseInLocation(time.DateOnly, bar.Date, location)
		if parseErr != nil || day.After(cutoffDay) {
			continue
		}
		if day.Equal(cutoffDay) && local.Hour()*60+local.Minute() < marketcalendar.RegularCloseMinute(day) {
			continue
		}
		result = append(result, bar)
	}
	return result
}

func (s *Service) getOrBuildChanAnalysis(cacheKey string, build func() (chananalysis.Result, error)) (chananalysis.Result, string, error) {
	if s == nil {
		return chananalysis.Result{}, "miss", errors.New("Chan analysis service unavailable")
	}
	if cached, ok := s.chanAnalysisMemory(cacheKey); ok {
		return cached, "memory", nil
	}
	value, err, _ := s.chanAnalysisFlights.Do(cacheKey, func() (any, error) {
		if cached, ok := s.chanAnalysisMemory(cacheKey); ok {
			return chanAnalysisFlightValue{result: cached, source: "memory"}, nil
		}
		if s.store != nil {
			if encoded, found, readErr := s.store.GetChanAnalysisCache(cacheKey); readErr != nil {
				log.Printf("Chan analysis cache read failed key=%s: %v", cacheKey, readErr)
			} else if found {
				var cached chananalysis.Result
				if json.Unmarshal(encoded, &cached) == nil && cached.RuleVersion != "" {
					s.rememberChanAnalysis(cacheKey, cached)
					return chanAnalysisFlightValue{result: cached, source: "sqlite"}, nil
				}
			}
		}
		built, buildErr := build()
		if buildErr != nil {
			return nil, buildErr
		}
		s.rememberChanAnalysis(cacheKey, built)
		s.persistChanAnalysis(cacheKey, built)
		return chanAnalysisFlightValue{result: built, source: "miss"}, nil
	})
	if err != nil {
		return chananalysis.Result{}, "miss", err
	}
	flight, ok := value.(chanAnalysisFlightValue)
	if !ok {
		return chananalysis.Result{}, "miss", errors.New("invalid Chan analysis cache result")
	}
	return flight.result, flight.source, nil
}

type chanAnalysisFlightValue struct {
	result chananalysis.Result
	source string
}

func (s *Service) chanAnalysisMemory(cacheKey string) (chananalysis.Result, bool) {
	s.chanAnalysisMu.Lock()
	defer s.chanAnalysisMu.Unlock()
	entry, ok := s.chanAnalysisCache[cacheKey]
	if !ok {
		return chananalysis.Result{}, false
	}
	entry.lastAccess = time.Now()
	s.chanAnalysisCache[cacheKey] = entry
	return entry.result, true
}

func (s *Service) rememberChanAnalysis(cacheKey string, result chananalysis.Result) {
	s.chanAnalysisMu.Lock()
	if s.chanAnalysisCache == nil {
		s.chanAnalysisCache = make(map[string]chanAnalysisCacheEntry)
	}
	s.chanAnalysisCache[cacheKey] = chanAnalysisCacheEntry{result: result, lastAccess: time.Now()}
	evictOldestHistoryCacheEntry(s.chanAnalysisCache, cacheKey, boundedChanAnalysisCacheEntries(s.cfg.LowMemory), func(entry chanAnalysisCacheEntry) time.Time { return entry.lastAccess })
	s.chanAnalysisMu.Unlock()
}

func (s *Service) persistChanAnalysis(cacheKey string, result chananalysis.Result) {
	if s.store == nil || s.background == nil {
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		log.Printf("Chan analysis cache encode failed key=%s: %v", cacheKey, err)
		return
	}
	s.mu.Lock()
	if s.background.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		if err := s.store.PutChanAnalysisCache(cacheKey, encoded, boundedChanAnalysisDurableCacheEntries(s.cfg.LowMemory)); err != nil {
			log.Printf("Chan analysis cache write failed key=%s: %v", cacheKey, err)
		}
	}()
}

func adjustedIntradayBars(input, daily []chananalysis.Bar) ([]chananalysis.Bar, int) {
	factors := make(map[string]float64, len(daily))
	for _, bar := range daily {
		if validAdjustedIntradayBasis(bar) {
			factors[bar.Date] = bar.AdjustmentFactor
		}
	}
	adjusted := make([]chananalysis.Bar, 0, len(input))
	skipped := 0
	for _, bar := range input {
		if len(bar.Date) < len("2006-01-02") {
			skipped++
			continue
		}
		factor, ok := factors[bar.Date[:len("2006-01-02")]]
		if !ok || !finitePositive(factor) {
			skipped++
			continue
		}
		bar.Open *= factor
		bar.High *= factor
		bar.Low *= factor
		bar.Close *= factor
		adjusted = append(adjusted, bar)
	}
	return adjusted, skipped
}

func validAdjustedIntradayBasis(bar chananalysis.Bar) bool {
	if !finitePositive(bar.AdjustmentFactor) {
		return false
	}
	factor := bar.AdjustmentFactor
	return adjustedPairMatches(bar.Open, bar.RawOpen, factor) &&
		adjustedPairMatches(bar.High, bar.RawHigh, factor) &&
		adjustedPairMatches(bar.Low, bar.RawLow, factor) &&
		adjustedPairMatches(bar.Close, bar.RawClose, factor)
}

func adjustedPairMatches(adjusted, raw, factor float64) bool {
	return finitePositive(adjusted) && finitePositive(raw) && math.Abs(adjusted-raw*factor) <= 1e-6*math.Max(1, adjusted)
}

func adjustedAnalysisBars(rows []map[string]any, cutoff time.Time) []chananalysis.Bar {
	bars := make([]chananalysis.Bar, 0, len(rows))
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	localCutoff := cutoff.In(location)
	for _, row := range rows {
		date, _ := row["date"].(string)
		day, dateErr := time.ParseInLocation(time.DateOnly, date, location)
		if dateErr != nil || day.After(time.Date(localCutoff.Year(), localCutoff.Month(), localCutoff.Day(), 0, 0, 0, 0, location)) {
			continue
		}
		if day.Year() == localCutoff.Year() && day.YearDay() == localCutoff.YearDay() && localCutoff.Hour()*60+localCutoff.Minute() < marketcalendar.RegularCloseMinute(day) {
			continue
		}
		close, ok := historyMapNumber(row, "close")
		if !ok {
			continue
		}
		bars = append(bars, chananalysis.Bar{Date: date, Close: close})
	}
	return bars
}

// historyMapNumber reads the scalar values used by history adapters without
// allocating a temporary *float64 for every OHLC field.
func historyMapNumber(row map[string]any, field string) (float64, bool) {
	value, found := row[field]
	if !found || value == nil {
		return 0, false
	}
	var number float64
	switch value := value.(type) {
	case bool:
		if value {
			number = 1
		}
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	case *float64:
		if value == nil {
			return 0, false
		}
		number = *value
	case *int64:
		if value == nil {
			return 0, false
		}
		number = float64(*value)
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func latestMarketEnvironment(series []chananalysis.MarketEnvironment, asOf string) chananalysis.MarketEnvironment {
	return chananalysis.MarketEnvironmentAt(series, asOf)
}

func attachMarketEnvironment(frame *chananalysis.Frame, series []chananalysis.MarketEnvironment) {
	if frame == nil {
		return
	}
	environment := chananalysis.MarketEnvironmentAt(series, frame.AsOf)
	frame.MarketEnvironment = &environment
	for index := range frame.SignalsV4 {
		observedAt := frame.SignalsV4[index].ObservedAt
		if observedAt == "" {
			observedAt = frame.SignalsV4[index].Date
		}
		signalEnvironment := chananalysis.MarketEnvironmentAt(series, observedAt)
		frame.SignalsV4[index].MarketEnvironment = &signalEnvironment
		frame.SignalsV4[index].MarketFilter = signalEnvironment.Status
		if signalEnvironment.Status == "unfiltered" {
			frame.SignalsV4[index].MarketFilter = "unfiltered"
			frame.SignalsV4[index].Detail += "；S&P 500 环境未过滤：" + signalEnvironment.Reason
		}
	}
}

// chanMarketHistory reuses the isolated adjusted-history cache and refresh path
// with the S&P 500 ticker. It never falls back to raw OHLC.
func (s *Service) chanMarketHistory() (chanAdjustedHistoryCacheEntry, string, string, bool) {
	return s.chanAdjustedHistory("^GSPC")
}

func recordChanStructureFrame(repository *store.SQLite, symbol, timeframe, ruleVersion string, bars []chananalysis.Bar, frame *chananalysis.Frame) {
	snapshot := chanStructureSnapshot(symbol, timeframe, ruleVersion, bars, *frame)
	revision, err := repository.RecordChanStructureSnapshot(snapshot)
	if err != nil {
		log.Printf("Chan structure snapshot failed symbol=%s timeframe=%s: %v", symbol, timeframe, err)
		frame.Warnings = append(frame.Warnings, "已确认缠论结构修订记录暂不可用")
		return
	}
	if revision != nil {
		log.Printf("Chan confirmed structure revision symbol=%s timeframe=%s reason=%s id=%d", symbol, timeframe, revision.Reason, revision.ID)
	}
	revisions, err := repository.LatestChanStructureRevisions(symbol, timeframe, 20)
	if err != nil {
		log.Printf("Chan structure revision history failed symbol=%s timeframe=%s: %v", symbol, timeframe, err)
		frame.Warnings = append(frame.Warnings, "已确认缠论结构修订记录暂不可用")
		return
	}
	frame.StructureRevisions = chanStructureRevisionOutput(revisions)
}

type chanSnapshotBar struct {
	Date                               string `json:"date"`
	Open, High, Low, Close, Volume     float64
	RawOpen, RawHigh, RawLow, RawClose float64
	AdjustmentFactor                   float64 `json:"adjustment_factor"`
}

func chanStructureSnapshot(symbol, timeframe, ruleVersion string, bars []chananalysis.Bar, frame chananalysis.Frame) store.ChanStructureSnapshot {
	barFingerprints := make(map[string]string, len(bars))
	inputHasher := sha256.New()
	_, _ = inputHasher.Write([]byte{'['})
	var encodedBar bytes.Buffer
	encoder := json.NewEncoder(&encodedBar)
	for index, bar := range bars {
		value := chanSnapshotBar{Date: bar.Date, Open: bar.Open, High: bar.High, Low: bar.Low, Close: bar.Close, Volume: bar.Volume, RawOpen: bar.RawOpen, RawHigh: bar.RawHigh, RawLow: bar.RawLow, RawClose: bar.RawClose, AdjustmentFactor: bar.AdjustmentFactor}
		encodedBar.Reset()
		_ = encoder.Encode(value)
		encoded := encodedBar.Bytes()
		if len(encoded) > 0 && encoded[len(encoded)-1] == 10 {
			encoded = encoded[:len(encoded)-1]
		}
		sum := sha256.Sum256(encoded)
		barFingerprints[bar.Date] = hex.EncodeToString(sum[:])
		if index > 0 {
			_, _ = inputHasher.Write([]byte{','})
		}
		_, _ = inputHasher.Write(encoded)
	}
	_, _ = inputHasher.Write([]byte{']'})
	inputSum := inputHasher.Sum(nil)
	confirmed := map[string]json.RawMessage{}
	add := func(kind, key string, value any) {
		encoded, err := json.Marshal(value)
		if err == nil {
			confirmed[fmt.Sprintf("%s|%s", kind, key)] = encoded
		}
	}
	for _, fractal := range frame.FractalsV4 {
		if fractal.ConfirmedAt != "" {
			add("fractal", fmt.Sprintf("fractal|level=%d|kind=%s|end=%s", fractal.Level, fractal.Kind, fractal.Date), map[string]any{"kind": fractal.Kind, "date": fractal.Date, "price": fractal.Price, "level": fractal.Level, "confirmed_at": fractal.ConfirmedAt})
		}
	}
	levels := frame.RecursiveLevels
	if len(levels) == 0 {
		levels = []chananalysis.LevelAnalysis{{Level: 0, Strokes: frame.StrokesV4, Segments: frame.Segments, Centers: frame.RecursiveCenters}}
	}
	for _, level := range levels {
		for _, stroke := range level.Strokes {
			if stroke.Confirmed {
				add("stroke", fmt.Sprintf("stroke|level=%d|direction=%s|start=%s|end=%s", stroke.Level, stroke.Direction, stroke.Start.Date, stroke.End.Date), map[string]any{"direction": stroke.Direction, "level": stroke.Level, "start": stableFractal(stroke.Start), "end": stableFractal(stroke.End), "low": stroke.Low, "high": stroke.High, "confirmed_at": stroke.ConfirmedAt})
			}
		}
		for _, segment := range level.Segments {
			if segment.Confirmed {
				add("segment", fmt.Sprintf("segment|level=%d|direction=%s|start=%s|end=%s", segment.Level, segment.Direction, segment.Start, segment.End), map[string]any{"direction": segment.Direction, "level": segment.Level, "start": segment.Start, "end": segment.End, "start_kind": segment.StartKind, "end_kind": segment.EndKind, "start_price": segment.StartPrice, "end_price": segment.EndPrice, "low": segment.Low, "high": segment.High, "confirmed_at": segment.ConfirmedAt})
			}
		}
		for _, center := range level.Centers {
			if !center.Forming && center.ConfirmedAt != "" {
				id := fmt.Sprintf("center|level=%d|start=%s|end=%s|zd=%.8g|zg=%.8g", center.Level, center.Start, center.End, center.Lower, center.Upper)
				add("center", id, map[string]any{"level": center.Level, "kind": center.Kind, "input_kind": center.InputKind, "start": center.Start, "end": center.End, "zd": center.Lower, "zg": center.Upper, "formed_at": center.FormedAt, "closed_at": center.ClosedAt, "extension_count": center.ExtensionCount})
			}
		}
	}
	return store.ChanStructureSnapshot{Symbol: symbol, Timeframe: timeframe, RuleVersion: ruleVersion, InputFingerprint: hex.EncodeToString(inputSum), BarFingerprints: barFingerprints, ConfirmedStructures: confirmed, UpdatedAt: time.Now().UTC()}
}

func stableFractal(fractal chananalysis.Fractal) map[string]any {
	return map[string]any{"kind": fractal.Kind, "date": fractal.Date, "price": fractal.Price, "level": fractal.Level, "confirmed_at": fractal.ConfirmedAt}
}

func chanStructureRevisionOutput(revisions []store.ChanStructureRevision) []chananalysis.StructureRevision {
	output := make([]chananalysis.StructureRevision, 0, len(revisions))
	for _, revision := range revisions {
		item := chananalysis.StructureRevision{ID: revision.ID, RuleVersion: revision.RuleVersion, PreviousRuleVersion: revision.PreviousRuleVersion, Reason: revision.Reason, InputFingerprint: revision.InputFingerprint, CreatedAt: revision.CreatedAt}
		for _, bar := range revision.ChangedBars {
			item.ChangedBars = append(item.ChangedBars, chananalysis.StructureBarChange{Date: bar.Date, OldFingerprint: bar.Old, NewFingerprint: bar.New})
		}
		for _, change := range revision.ChangedConfirmedItems {
			var before, after any
			if len(change.Before) > 0 {
				_ = json.Unmarshal(change.Before, &before)
			}
			if len(change.After) > 0 {
				_ = json.Unmarshal(change.After, &after)
			}
			item.ChangedConfirmedItems = append(item.ChangedConfirmedItems, chananalysis.StructureChange{ID: change.ID, Before: before, After: after})
		}
		output = append(output, item)
	}
	return output
}

func validAdjustedHistoryRow(bar map[string]any) bool {
	factor, ok := historyMapNumber(bar, "adjustment_factor")
	if !ok || factor <= 0 {
		return false
	}
	for _, field := range []string{"open", "high", "low", "close"} {
		adjusted, adjustedOK := historyMapNumber(bar, field)
		raw, rawOK := historyMapNumber(bar, "raw_"+field)
		if !adjustedOK || !rawOK || !adjustedPairMatches(adjusted, raw, factor) {
			return false
		}
	}
	return true
}

const chanAdjustedHistoryBarLimit = 1300 // approximately five years of daily sessions
const chanAdjustedHistoryRetryDelay = 24 * time.Hour

// Daily adjusted bars change at most once per session; refetching the five-year
// window every quarter hour made revisiting a symbol needlessly slow.
const chanAdjustedHistoryFreshness = 15 * time.Minute

func chanAdjustedCacheKey(symbol string) string {
	return symbol + "|1d|split_dividend_auto|go-yfinance"
}

func (s *Service) chanAdjustedJobActive(symbol string) bool {
	key := chanAdjustedCacheKey(symbol)
	s.chanAdjustedMu.Lock()
	active := s.chanAdjustedJobs[key]
	s.chanAdjustedMu.Unlock()
	return active
}

// ensureChanAdjustedHistory starts the adjusted history job after a snapshot is
// committed. On a cold symbol this is the only history request: it writes the
// raw OHLC cache as well, so a separate two-year fetch would just queue a
// duplicate behind the same single upstream slot.
func (s *Service) ensureChanAdjustedHistory(symbol string) {
	if s == nil || s.store == nil || s.provider == nil {
		return
	}
	if _, ok := s.provider.(ChanAdjustedHistoryProvider); !ok {
		return
	}
	cacheKey := chanAdjustedCacheKey(symbol)
	s.chanAdjustedMu.Lock()
	entry, found := s.chanAdjustedCache[cacheKey]
	s.chanAdjustedMu.Unlock()
	if found && time.Since(entry.fetchedAt) < chanAdjustedHistoryFreshness {
		if coverageOK, _, _ := chanAdjustedHistoryCoverage(entry.bars); coverageOK {
			return
		}
	}
	if cached, err := s.store.LatestChanAdjustedHistory(symbol, "1d", "split_dividend_auto", "go-yfinance", chanAdjustedHistoryBarLimit); err == nil {
		if bars, _ := cached["bars"].([]map[string]any); len(bars) > 0 {
			fetchedAt, _ := historyTimestamp(cached["fetched_at"])
			if time.Since(fetchedAt) < chanAdjustedHistoryFreshness {
				if coverageOK, _, _ := chanAdjustedHistoryCoverage(bars); coverageOK {
					return
				}
			}
		}
	}
	s.scheduleChanAdjustedRefresh(cacheKey, symbol, true)
}

func (s *Service) chanAdjustedHistory(symbol string) (chanAdjustedHistoryCacheEntry, string, string, bool) {
	const timeframe, adjustmentType = "1d", "split_dividend_auto"
	if s == nil || s.store == nil {
		return chanAdjustedHistoryCacheEntry{}, "none", "复权缠论历史存储不可用", false
	}
	cacheKey := chanAdjustedCacheKey(symbol)
	s.chanAdjustedMu.Lock()
	entry, found := s.chanAdjustedCache[cacheKey]
	if found {
		entry.lastAccess = time.Now()
		s.chanAdjustedCache[cacheKey] = entry
	}
	s.chanAdjustedMu.Unlock()
	coverageOK, _, _ := chanAdjustedHistoryCoverage(entry.bars)
	if !found || !coverageOK || time.Since(entry.fetchedAt) >= chanAdjustedHistoryFreshness {
		if cached, err := s.store.LatestChanAdjustedHistory(symbol, timeframe, adjustmentType, "go-yfinance", chanAdjustedHistoryBarLimit); err == nil {
			if bars, _ := cached["bars"].([]map[string]any); len(bars) > 0 {
				fetchedAt, _ := historyTimestamp(cached["fetched_at"])
				if !found || fetchedAt.After(entry.fetchedAt) {
					entry = chanAdjustedHistoryCacheEntry{bars: bars, fetchedAt: fetchedAt, source: "sqlite", lastAccess: time.Now()}
					found = true
					s.chanAdjustedMu.Lock()
					s.chanAdjustedCache[cacheKey] = entry
					evictOldestHistoryCacheEntry(s.chanAdjustedCache, cacheKey, boundedAdjustedHistoryCacheEntries(s.cfg.LowMemory), func(entry chanAdjustedHistoryCacheEntry) time.Time { return entry.lastAccess })
					s.chanAdjustedMu.Unlock()
				}
			}
		}
	}
	coverageOK, coverageStart, coverageEnd := chanAdjustedHistoryCoverage(entry.bars)
	if found && time.Since(entry.fetchedAt) < chanAdjustedHistoryFreshness && coverageOK {
		return entry, entry.source, "", false
	}
	_, ok := s.provider.(ChanAdjustedHistoryProvider)
	if !ok {
		if found {
			return entry, entry.source, "当前行情提供方不支持复权缠论历史，继续显示缓存", false
		}
		return chanAdjustedHistoryCacheEntry{}, "none", "当前行情提供方不支持复权缠论历史；新算法暂停，未回退到未复权行情", false
	}
	refreshing := s.scheduleChanAdjustedRefresh(cacheKey, symbol, false)
	if found {
		if !coverageOK {
			warning := fmt.Sprintf("复权缠论缓存仅覆盖 %s 至 %s（%d 根）；正在后台补齐约五年历史，暂显示已有数据", coverageStart, coverageEnd, len(entry.bars))
			return entry, entry.source, warning, refreshing
		}
		return entry, entry.source, "复权缠论历史正在后台更新；暂显示缓存", refreshing
	}
	return chanAdjustedHistoryCacheEntry{}, "none", "复权缠论历史正在获取；新算法暂停，未回退到未复权行情", refreshing
}

func chanAdjustedHistoryCoverage(bars []map[string]any) (bool, string, string) {
	if len(bars) == 0 {
		return false, "未知", "未知"
	}
	first, _ := bars[0]["date"].(string)
	last, _ := bars[len(bars)-1]["date"].(string)
	firstDate, firstErr := time.Parse(time.DateOnly, first)
	lastDate, lastErr := time.Parse(time.DateOnly, last)
	if firstErr != nil || lastErr != nil || !lastDate.After(firstDate) {
		return false, first, last
	}
	// Allow normal exchange-calendar variation at the five-year boundary.
	// This detects legacy two-year caches without demanding a fixed bar count,
	// which would incorrectly reject newly listed symbols.
	minimumStart := lastDate.AddDate(-5, 0, 45)
	return !firstDate.After(minimumStart), first, last
}

func (s *Service) scheduleChanAdjustedRefresh(cacheKey, symbol string, force bool) bool {
	if s == nil || s.provider == nil || s.background == nil {
		return false
	}
	if !force && (s.snapshotRefreshActive(symbol) || !s.hasDurableQuote(symbol)) {
		// The snapshot refresh will call ensureChanAdjustedHistory once the
		// quote and chain are committed; starting the five-year request now
		// would occupy the only upstream slot ahead of the option table. A cold
		// browser can also lose the EventSource handshake race, so require the
		// durable quote rather than only an in-flight refresh.
		return false
	}
	s.chanAdjustedMu.Lock()
	if s.chanAdjustedJobs[cacheKey] {
		s.chanAdjustedMu.Unlock()
		return true
	}
	if time.Now().Before(s.chanAdjustedRetryAfter[cacheKey]) {
		s.chanAdjustedMu.Unlock()
		return false
	}
	s.chanAdjustedJobs[cacheKey] = true
	s.chanAdjustedMu.Unlock()
	s.mu.Lock()
	if s.background.Err() != nil {
		s.chanAdjustedMu.Lock()
		delete(s.chanAdjustedJobs, cacheKey)
		s.chanAdjustedMu.Unlock()
		s.mu.Unlock()
		return false
	}
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.chanAdjustedMu.Lock()
			delete(s.chanAdjustedJobs, cacheKey)
			s.chanAdjustedMu.Unlock()
			// The raw daily cache was either just written by this job or the job
			// failed and a two-year fallback is needed. Run the levels prefetch
			// only after the job leaves the active map so it can proceed.
			s.prefetchSymbolAnalytics(symbol, true)
		}()
		loader, ok := s.provider.(ChanAdjustedHistoryProvider)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.background, 3*time.Minute)
		defer cancel()
		// Recursive segments and centers need context that predates the displayed
		// five-year validation window. Keep the data window aligned with the
		// documented near-term study instead of truncating it to two years.
		result, err := loader.ChanAdjustedHistory(ctx, symbol, "5y")
		if err != nil || result.FetchedAt.IsZero() || len(result.Bars) == 0 {
			if err == nil {
				err = errors.New("adjusted Chan history provider returned no bars")
			}
			log.Printf("background adjusted Chan history refresh failed symbol=%s: %v", symbol, err)
			s.chanAdjustedMu.Lock()
			s.chanAdjustedRetryAfter[cacheKey] = time.Now().Add(30 * time.Second)
			trimSymbolStateCache(s.chanAdjustedRetryAfter, cacheKey, s.cfg.LowMemory, func(at time.Time) time.Time { return at })
			s.chanAdjustedMu.Unlock()
			return
		}
		if err := s.store.WriteChanAdjustedHistory(symbol, result.Timeframe, result.AdjustmentType, result.Source, result.Bars, result.FetchedAt); err != nil {
			log.Printf("background adjusted Chan history SQLite write failed symbol=%s: %v", symbol, err)
			s.chanAdjustedMu.Lock()
			s.chanAdjustedRetryAfter[cacheKey] = time.Now().Add(30 * time.Second)
			trimSymbolStateCache(s.chanAdjustedRetryAfter, cacheKey, s.cfg.LowMemory, func(at time.Time) time.Time { return at })
			s.chanAdjustedMu.Unlock()
			return
		}
		if rawBars := rawChanHistoryBars(result.Bars); len(rawBars) > 0 {
			if err := s.store.WriteHistory(symbol, rawBars, result.FetchedAt); err != nil {
				log.Printf("background adjusted Chan raw history cache write failed symbol=%s: %v", symbol, err)
			}
		}
		s.chanAdjustedMu.Lock()
		if s.chanAdjustedCache == nil {
			s.chanAdjustedCache = make(map[string]chanAdjustedHistoryCacheEntry)
		}
		s.chanAdjustedCache[cacheKey] = chanAdjustedHistoryCacheEntry{bars: result.Bars, fetchedAt: result.FetchedAt, source: "upstream", lastAccess: time.Now()}
		evictOldestHistoryCacheEntry(s.chanAdjustedCache, cacheKey, boundedAdjustedHistoryCacheEntries(s.cfg.LowMemory), func(entry chanAdjustedHistoryCacheEntry) time.Time { return entry.lastAccess })
		delete(s.chanAdjustedRetryAfter, cacheKey)
		if adequate, _, _ := chanAdjustedHistoryCoverage(result.Bars); !adequate {
			// Shorter histories are valid for newer listings. Avoid retrying the
			// same five-year request on every page view when the symbol has not
			// traded for five years yet.
			s.chanAdjustedRetryAfter[cacheKey] = time.Now().Add(chanAdjustedHistoryRetryDelay)
			trimSymbolStateCache(s.chanAdjustedRetryAfter, cacheKey, s.cfg.LowMemory, func(at time.Time) time.Time { return at })
		}
		s.chanAdjustedMu.Unlock()
		s.publishAnalyticsEvent(symbol, result.FetchedAt)
	}()
	return true
}

func rawChanHistoryBars(adjusted []map[string]any) []map[string]any {
	if len(adjusted) == 0 {
		return nil
	}
	bars := make([]map[string]any, 0, len(adjusted))
	for _, row := range adjusted {
		date, ok := row["date"].(string)
		if !ok || date == "" {
			continue
		}
		open, openOK := historyMapNumber(row, "raw_open")
		high, highOK := historyMapNumber(row, "raw_high")
		low, lowOK := historyMapNumber(row, "raw_low")
		close, closeOK := historyMapNumber(row, "raw_close")
		volume, volumeOK := historyMapNumber(row, "raw_volume")
		if !openOK || !highOK || !lowOK || !closeOK || !volumeOK {
			continue
		}
		bars = append(bars, map[string]any{"date": date, "open": open, "high": high, "low": low, "close": close, "volume": volume})
	}
	return bars
}

// chanAnalysis shares the existing daily history cache and asynchronous
// prefetch. It never fetches upstream history on the foreground request.
func (s *Service) chanAnalysis(c *gin.Context) {
	if !s.validateQuery(c, positiveFloatQueryRule("spot"), boolQueryRule("light")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	light, err := queryBool(c, "light", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	history, err := s.store.LatestHistoryWithTimestamps(symbol, 3000)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	var spot *float64
	spotSource := "daily_close"
	if raw, present := queryValue(c, "spot"); present {
		value, _ := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		spot, spotSource = &value, "displayed_quote"
	} else if quote, found, quoteErr := s.latestQuoteSnapshot(symbol); quoteErr == nil && found && quote.value.Price != nil {
		spot, spotSource = quote.value.Price, "regular_quote"
	}
	rawBars, _ := history["bars"].([]map[string]any)
	bars := make([]chananalysis.Bar, 0, len(rawBars))
	for _, raw := range rawBars {
		bar := chananalysis.Bar{}
		bar.Date, _ = raw["date"].(string)
		bar.CapturedAt, _ = historyTimestamp(raw["captured_at"])
		bar.Open, _ = historyMapNumber(raw, "open")
		bar.High, _ = historyMapNumber(raw, "high")
		bar.Low, _ = historyMapNumber(raw, "low")
		bar.Close, _ = historyMapNumber(raw, "close")
		bar.Volume, _ = historyMapNumber(raw, "volume")
		bars = append(bars, bar)
	}
	now := time.Now()
	var result chananalysis.Result
	if light {
		// Light mode feeds the trend/support cards only. The adjusted endpoint
		// owns the chart structures, so skip the daily/weekly analysis and the
		// ~470 KiB of frames it would serialize on every analytics refresh.
		result = chananalysis.Result{Symbol: symbol, Spot: spot, GeneratedAt: now}
	} else {
		result = chananalysis.Analyze(symbol, bars, spot, now)
	}
	quote := model.Quote{Symbol: symbol, Sessions: map[string]any{}}
	if cachedQuote, found, quoteErr := s.latestQuoteSnapshot(symbol); quoteErr == nil && found {
		quote = cachedQuote.value
	}
	if spot != nil {
		quote.Price = spot
	}
	trend := cachedTrend(rawBars)
	trendMarketData := trendMarket(rawBars, quote)
	extremes, extremesErr := s.store.LatestPayload("price_extremes", symbol)
	if extremesErr != nil {
		extremes = nil
	}
	if !cachedPayloadAvailable(extremes["extremes"]) {
		if calculated := calculatePriceExtremes(rawBars); calculated != nil {
			extremes = map[string]any{"extremes": calculated, "fetched_at": history["fetched_at"], "source": "sqlite"}
		}
	}
	beta, betaErr := s.store.LatestPayload("beta_snapshots", symbol)
	if betaErr != nil {
		beta = nil
	}
	analysisSpot := spot
	if analysisSpot == nil || *analysisSpot <= 0 {
		if light {
			if last := len(rawBars) - 1; last >= 0 {
				lastClose, _ := historyMapNumber(rawBars[last], "close")
				analysisSpot = &lastClose
			}
		} else {
			analysisSpot = result.Spot
		}
	}
	marketAnalysis := buildCachedLevelsAt(symbol, "", rawBars, nil, quote, analysisSpot, analysisSpot, history["fetched_at"], extremes, beta, nil, now, map[string]any{})
	var stopLoss any
	if spot != nil && *spot > 0 && trend != nil {
		stopLoss = cachedStopLoss(trend, nil, nil, *spot, cachedATR(rawBars, cachedATRPeriod))
	}
	warnings := append([]string(nil), result.Warnings...)
	warnings = append(warnings, "历史沿用现有未复权日线；拆股和除息跳空可能改变笔与中枢，请结合公司行动核对")
	expected := chanExpectedHistoryDay(now)
	asOf := result.Daily.AsOf
	if light {
		asOf = ""
		if last := len(rawBars) - 1; last >= 0 {
			asOf, _ = rawBars[last]["date"].(string)
		}
	}
	missingClose := asOf < expected
	needsRefresh := !historyFresh(history["fetched_at"], s.cfg.HistoryMaxAge) || missingClose
	_, supportsHistory := s.provider.(HistoryProvider)
	refreshing := needsRefresh && supportsHistory
	if refreshing {
		s.prefetchChanHistory(symbol, missingClose)
		warnings = append(warnings, "历史行情正在后台更新；当前展示已有缓存，更新后重新计算")
	}
	if missingClose {
		warnings = append(warnings, "日线缓存尚未覆盖最近已收盘交易日 "+expected+"，结构判断以图中截止日为准")
	}
	if needsRefresh && !supportsHistory {
		warnings = append(warnings, "当前行情提供方不支持历史日线更新，结构分析暂以已有缓存为准")
	}
	response := gin.H{
		"symbol": result.Symbol, "spot": result.Spot, "spot_source": spotSource,
		"generated_at": result.GeneratedAt, "warnings": warnings,
		"trend": trend, "trend_market": trendMarketData, "extremes": extremes["extremes"],
		"beta": beta["beta"], "trend_stop_loss": stopLoss, "market_analysis": marketAnalysis,
		"history": gin.H{"source": cacheSource(history["bars"], history["fetched_at"]), "fetched_at": history["fetched_at"], "refreshing": refreshing, "bars": len(rawBars)},
	}
	if light {
		response["symbol"], response["spot"], response["generated_at"] = symbol, analysisSpot, now
	} else {
		response["rule_version"], response["rules"], response["summary"] = result.RuleVersion, result.Rules, result.Summary
		response["daily"], response["weekly"] = result.Daily, result.Weekly
	}
	c.JSON(http.StatusOK, response)
}

func chanExpectedHistoryDay(now time.Time) string {
	location, _ := time.LoadLocation("America/New_York")
	if location == nil {
		location = time.UTC
	}
	local := now.In(location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	closeTime := day.Add(time.Duration(marketcalendar.RegularCloseMinute(day)) * time.Minute)
	if now.Before(closeTime) {
		day = day.AddDate(0, 0, -1)
	}
	for !marketcalendar.IsTradingDay(day) {
		day = day.AddDate(0, 0, -1)
	}
	return day.Format(time.DateOnly)
}

// Missing final closes may need an earlier refresh than the normal history
// TTL. A short freshness window coalesces simultaneous instances through the
// established history lock, SQLite lease and shared upstream budget.
func (s *Service) prefetchChanHistory(symbol string, missingClose bool) {
	// Leave the first upstream slot to the quote and chain while a snapshot
	// refresh is running, and do not start a cold symbol's history before any
	// durable quote exists: the refresh completion starts it once the page has
	// data to show.
	if s.snapshotRefreshActive(symbol) || !s.hasDurableQuote(symbol) {
		return
	}
	// A cold adjusted Chan request fetches the same raw OHLC together with
	// Yahoo's adjusted close. Let that job seed the shared raw-history cache
	// instead of launching a competing two-year request.
	if s.chanAdjustedJobActive(symbol) {
		return
	}
	s.mu.Lock()
	if s.background.Err() != nil || s.chanHistoryJobs[symbol] {
		s.mu.Unlock()
		return
	}
	if s.chanHistoryJobs == nil {
		s.chanHistoryJobs = make(map[string]bool)
	}
	s.chanHistoryJobs[symbol] = true
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() { s.mu.Lock(); delete(s.chanHistoryJobs, symbol); s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(s.background, 90*time.Second)
		defer cancel()
		maxAge := s.cfg.HistoryMaxAge
		if missingClose && maxAge > 15*time.Second {
			maxAge = 15 * time.Second
		}
		history := s.loadHistoryWithMaxAge(ctx, symbol, "2y", maxAge)
		if ctx.Err() == nil && history["source"] == "upstream" {
			s.publishAnalyticsEvent(symbol, history["fetched_at"])
		}
	}()
}
