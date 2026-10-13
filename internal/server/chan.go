package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
)

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
		for field, target := range map[string]*float64{"open": &bar.Open, "high": &bar.High, "low": &bar.Low, "close": &bar.Close, "volume": &bar.Volume} {
			if value := numberValue(raw[field]); value != nil {
				*target = *value
			}
		}
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
	marketAnalysis := buildCachedLevelsAt(symbol, "", rawBars, nil, quote, analysisSpot, analysisSpot, history["fetched_at"], extremes, beta, map[string]any{}, now, map[string]any{})
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
