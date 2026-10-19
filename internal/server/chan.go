package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
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
	// Both periods derive from the same 15-minute series, so one cache entry per
	// symbol covers them and switching periods reuses the fetch.
	s.intradayMu.Lock()
	memoryCached, memoryFound := s.intradayHistoryCache[symbol]
	if memoryFound && time.Since(memoryCached.fetchedAt) < 3*time.Minute {
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
				if !persisted.fetchedAt.IsZero() && time.Since(persisted.fetchedAt) < 3*time.Minute {
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

// chanAdjustedAnalysis serves the independent adjusted daily history used by
// recursive Chan analysis. Existing /chan and quote paths remain raw-price.
func (s *Service) chanAdjustedAnalysis(c *gin.Context) {
	if !s.validateQuery(c, positiveFloatQueryRule("spot")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	entry, source, warning, refreshing := s.chanAdjustedHistory(symbol)
	if len(entry.bars) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"symbol": symbol, "rule_version": chananalysis.RecursivePreviewVersion, "target_rule_version": chananalysis.RecursiveRuleVersion, "quality": gin.H{"status": "degraded", "price_basis": "unavailable"}, "warnings": []string{warning}})
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
	result := chananalysis.AnalyzeAdjusted(symbol, bars, adjustedSpot, time.Now())
	// Daily signals use the directly mapped 4-hour structures. Reuse the
	// cached 15-minute history, but move it into the same adjusted price
	// coordinate as the daily frame before aggregating it. Bars without a
	// matching, validated daily adjustment factor are excluded.
	intradayEntry, _, _, _ := s.intradayFrame(symbol)
	if len(intradayEntry.frame) > 0 {
		adjustedIntraday, skipped := adjustedIntradayBars(intradayBars(intradayEntry), bars)
		if len(adjustedIntraday) > 0 {
			lowerResult, lowerErr := chananalysis.AnalyzeIntraday(symbol, adjustedIntraday, "4h", adjustedSpot, time.Now())
			if lowerErr == nil {
				lowerResult.Daily.Quality.PriceBasis = "split_dividend_adjusted"
				chananalysis.AttachMappedTimeframe(&result.Daily, lowerResult.Daily)
				if skipped > 0 {
					result.Daily.Warnings = append(result.Daily.Warnings, fmt.Sprintf("%d 根 15 分钟 K 线因缺少对应日线复权因子未参与跨周期确认", skipped))
				}
			}
		}
	}
	marketHistory, marketSource, marketWarning, marketRefreshing := s.chanMarketHistory()
	marketBars := adjustedAnalysisBars(marketHistory.bars, time.Now())
	marketSeries := chananalysis.ClassifyMarketEnvironment(marketBars)
	attachMarketEnvironment(&result.Daily, marketSeries)
	attachMarketEnvironment(&result.Weekly, marketSeries)
	if s.store != nil {
		recordChanStructureFrame(s.store, symbol, "1d", result.RuleVersion, bars, &result.Daily)
		recordChanStructureFrame(s.store, symbol, "1wk", result.RuleVersion, result.Weekly.Candles, &result.Weekly)
	}
	if adjustedSpot == nil {
		result.Spot = nil
	}
	for index := range result.Daily.SignalsV4 {
		result.Daily.SignalsV4[index].Symbol = symbol
	}
	for index := range result.Weekly.SignalsV4 {
		result.Weekly.SignalsV4[index].Symbol = symbol
	}
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
	c.JSON(http.StatusOK, gin.H{"symbol": symbol, "rule_version": result.RuleVersion, "target_rule_version": chananalysis.RecursiveRuleVersion, "legacy_rule_version": result.LegacyRuleVersion, "parameters": result.Parameters, "rules": result.Rules, "algorithm_comparison": result.AlgorithmComparison, "market_environment": latestMarketEnvironment(marketSeries, result.Daily.AsOf), "market_history": gin.H{"symbol": "^GSPC", "source": marketSource, "fetched_at": marketHistory.fetchedAt, "refreshing": marketRefreshing}, "price_coordinate": priceCoordinate, "daily": result.Daily, "weekly": result.Weekly, "summary": result.Summary, "warnings": result.Warnings, "history": gin.H{"source": source, "provider": "go-yfinance", "timeframe": "1d", "adjustment_type": "split_dividend_auto", "fetched_at": entry.fetchedAt, "refreshing": refreshing}})
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

func (s *Service) chanAdjustedHistory(symbol string) (chanAdjustedHistoryCacheEntry, string, string, bool) {
	const timeframe, adjustmentType = "1d", "split_dividend_auto"
	if s == nil || s.store == nil {
		return chanAdjustedHistoryCacheEntry{}, "none", "复权缠论历史存储不可用", false
	}
	cacheKey := symbol + "|" + timeframe + "|" + adjustmentType + "|go-yfinance"
	s.chanAdjustedMu.Lock()
	entry, found := s.chanAdjustedCache[cacheKey]
	if found {
		entry.lastAccess = time.Now()
		s.chanAdjustedCache[cacheKey] = entry
	}
	s.chanAdjustedMu.Unlock()
	coverageOK, _, _ := chanAdjustedHistoryCoverage(entry.bars)
	if !found || !coverageOK || time.Since(entry.fetchedAt) >= 15*time.Minute {
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
	if found && time.Since(entry.fetchedAt) < 15*time.Minute && coverageOK {
		return entry, entry.source, "", false
	}
	_, ok := s.provider.(ChanAdjustedHistoryProvider)
	if !ok {
		if found {
			return entry, entry.source, "当前行情提供方不支持复权缠论历史，继续显示缓存", false
		}
		return chanAdjustedHistoryCacheEntry{}, "none", "当前行情提供方不支持复权缠论历史；新算法暂停，未回退到未复权行情", false
	}
	refreshing := s.scheduleChanAdjustedRefresh(cacheKey, symbol)
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

func (s *Service) scheduleChanAdjustedRefresh(cacheKey, symbol string) bool {
	if s == nil || s.provider == nil || s.background == nil {
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
			s.chanAdjustedMu.Lock()
			s.chanAdjustedRetryAfter[cacheKey] = time.Now().Add(30 * time.Second)
			s.chanAdjustedMu.Unlock()
			return
		}
		if err := s.store.WriteChanAdjustedHistory(symbol, result.Timeframe, result.AdjustmentType, result.Source, result.Bars, result.FetchedAt); err != nil {
			s.chanAdjustedMu.Lock()
			s.chanAdjustedRetryAfter[cacheKey] = time.Now().Add(30 * time.Second)
			s.chanAdjustedMu.Unlock()
			return
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
		}
		s.chanAdjustedMu.Unlock()
	}()
	return true
}

// chanAnalysis shares the existing daily history cache and asynchronous
// prefetch. It never fetches upstream history on the foreground request.
func (s *Service) chanAnalysis(c *gin.Context) {
	if !s.validateQuery(c, positiveFloatQueryRule("spot")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
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
	result := chananalysis.Analyze(symbol, bars, spot, now)
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
		analysisSpot = result.Spot
	}
	marketAnalysis := buildCachedLevelsAt(symbol, "", rawBars, nil, quote, analysisSpot, analysisSpot, history["fetched_at"], extremes, beta, nil, now, map[string]any{})
	var stopLoss any
	if spot != nil && *spot > 0 && trend != nil {
		stopLoss = cachedStopLoss(trend, nil, nil, *spot, cachedATR(rawBars, cachedATRPeriod))
	}
	result.Warnings = append(result.Warnings, "历史沿用现有未复权日线；拆股和除息跳空可能改变笔与中枢，请结合公司行动核对")
	expected := chanExpectedHistoryDay(now)
	missingClose := result.Daily.AsOf < expected
	needsRefresh := !historyFresh(history["fetched_at"], s.cfg.HistoryMaxAge) || missingClose
	_, supportsHistory := s.provider.(HistoryProvider)
	refreshing := needsRefresh && supportsHistory
	if refreshing {
		s.prefetchChanHistory(symbol, missingClose)
		result.Warnings = append(result.Warnings, "历史行情正在后台更新；当前展示已有缓存，更新后重新计算")
	}
	if missingClose {
		result.Warnings = append(result.Warnings, "日线缓存尚未覆盖最近已收盘交易日 "+expected+"，结构判断以图中截止日为准")
	}
	if needsRefresh && !supportsHistory {
		result.Warnings = append(result.Warnings, "当前行情提供方不支持历史日线更新，结构分析暂以已有缓存为准")
	}
	c.JSON(http.StatusOK, gin.H{
		"symbol": result.Symbol, "spot": result.Spot, "spot_source": spotSource,
		"generated_at": result.GeneratedAt, "rule_version": result.RuleVersion, "rules": result.Rules,
		"summary": result.Summary, "daily": result.Daily, "weekly": result.Weekly, "warnings": result.Warnings,
		"trend": trend, "trend_market": trendMarketData, "extremes": extremes["extremes"],
		"beta": beta["beta"], "trend_stop_loss": stopLoss, "market_analysis": marketAnalysis,
		"history": gin.H{"source": cacheSource(history["bars"], history["fetched_at"]), "fetched_at": history["fetched_at"], "refreshing": refreshing, "bars": len(rawBars)},
	})
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
			s.publishAnalyticsEvent(symbol)
		}
	}()
}
