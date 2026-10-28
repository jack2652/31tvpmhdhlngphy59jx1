package server

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
)

const historyLeaseDuration = 120 * time.Second
const historyHeavyGateWait = 200 * time.Millisecond
const lowMemoryHistoryLockWait = 3 * time.Second

type benchmarkHistoryProvider interface {
	BenchmarkHistory(context.Context, string, string) (provider.HistoryResult, error)
}

func (s *Service) acquireHistoryHeavyGate(ctx context.Context) (func(), error) {
	if !s.cfg.LowMemory {
		return func() {}, nil
	}
	timer := time.NewTimer(historyHeavyGateWait)
	defer timer.Stop()
	select {
	case s.gammaHeavyGate <- struct{}{}:
		return func() { <-s.gammaHeavyGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("内存保护：已有刷新在进行，日线回源稍后重试")
	}
}

func guardedHistoryCall[T any](s *Service, ctx context.Context, callback func() (T, error)) (T, error) {
	release, err := s.acquireHistoryHeavyGate(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return callback()
}

func (s *Service) historyLock(ctx context.Context, key string, wait time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.historyLocks == nil {
		s.historyLocks = make(map[string]*historyLockEntry)
	}
	entry := s.historyLocks[key]
	if entry == nil {
		entry = &historyLockEntry{}
		s.historyLocks[key] = entry
	}
	entry.refs++
	s.mu.Unlock()
	var timeout <-chan time.Time
	var timer *time.Timer
	if wait > 0 {
		timer = time.NewTimer(wait)
		timeout = timer.C
		defer timer.Stop()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !entry.mutex.TryLock() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			entry.refs--
			if entry.refs == 0 && s.historyLocks[key] == entry {
				delete(s.historyLocks, key)
			}
			s.mu.Unlock()
			return nil, ctx.Err()
		case <-timeout:
			s.mu.Lock()
			entry.refs--
			if entry.refs == 0 && s.historyLocks[key] == entry {
				delete(s.historyLocks, key)
			}
			s.mu.Unlock()
			return nil, fmt.Errorf("低内存保护：日线刷新正在进行")
		case <-ticker.C:
		}
	}
	return func() {
		entry.mutex.Unlock()
		s.mu.Lock()
		entry.refs--
		if entry.refs == 0 && s.historyLocks[key] == entry {
			delete(s.historyLocks, key)
		}
		s.mu.Unlock()
	}, nil
}

func (s *Service) loadHistory(ctx context.Context, symbol, period string) map[string]any {
	return s.loadHistoryWithMaxAge(ctx, symbol, period, s.cfg.HistoryMaxAge)
}

func (s *Service) loadHistoryWithMaxAge(ctx context.Context, symbol, period string, maxAge time.Duration) map[string]any {
	cached, err := s.store.LatestHistory(symbol, 3000)
	if err != nil {
		return historyResult(symbol, nil, nil, "none", err.Error())
	}
	bars, _ := cached["bars"].([]map[string]any)
	if len(bars) > 0 {
		if historyFresh(cached["fetched_at"], maxAge) {
			return historyResult(symbol, bars, cached["fetched_at"], "sqlite", nil)
		}
		if s.scheduleHistoryRefresh(symbol, period, maxAge) {
			return historyResult(symbol, bars, cached["fetched_at"], "sqlite", "SQLite 历史行情已先显示，后台正在更新")
		}
		return historyResult(symbol, bars, cached["fetched_at"], "sqlite", nil)
	}
	return s.refreshHistory(ctx, symbol, period, maxAge, cached)
}

func (s *Service) scheduleHistoryRefresh(symbol, period string, maxAge time.Duration) bool {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	s.mu.Lock()
	if s.background.Err() != nil || s.historyRefreshJobs[key] || time.Now().Before(s.historyRefreshAfter[key]) {
		s.mu.Unlock()
		return false
	}
	s.historyRefreshJobs[key] = true
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.historyRefreshJobs, key)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.background, 3*time.Minute)
		defer cancel()
		result := s.refreshHistory(ctx, symbol, period, maxAge, nil)
		if result["source"] == "upstream" {
			s.mu.Lock()
			delete(s.historyRefreshAfter, key)
			s.mu.Unlock()
			s.publishAnalyticsEvent(symbol, result["fetched_at"])
		} else {
			s.mu.Lock()
			s.historyRefreshAfter[key] = time.Now().Add(30 * time.Second)
			trimSymbolStateCache(s.historyRefreshAfter, key, s.cfg.LowMemory, func(at time.Time) time.Time { return at })
			s.mu.Unlock()
		}
		if warning, _ := result["warning"].(string); warning != "" && ctx.Err() == nil {
			log.Printf("background history refresh failed symbol=%s: %s", symbol, warning)
		}
	}()
	return true
}

func (s *Service) refreshHistory(ctx context.Context, symbol, period string, maxAge time.Duration, initial map[string]any) map[string]any {
	cached := initial
	var err error
	if cached == nil {
		cached, err = s.store.LatestHistory(symbol, 3000)
		if err != nil {
			return historyResult(symbol, nil, nil, "none", err.Error())
		}
	}
	if bars, _ := cached["bars"].([]map[string]any); len(bars) > 0 && historyFresh(cached["fetched_at"], maxAge) {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], "sqlite", nil)
	}
	wait := time.Duration(0)
	if s.cfg.LowMemory {
		wait = lowMemoryHistoryLockWait
	}
	unlock, lockErr := s.historyLock(ctx, "bars:"+symbol, wait)
	if lockErr != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), lockErr.Error())
	}
	defer unlock()
	cached, err = s.store.LatestHistory(symbol, 3000)
	if err != nil {
		return historyResult(symbol, nil, nil, "none", err.Error())
	}
	if historyFresh(cached["fetched_at"], maxAge) {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], "sqlite", nil)
	}
	releaseLease, leaseErr := s.acquireHistoryLease(ctx, "history:bars:"+symbol)
	if leaseErr != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), leaseErr.Error())
	}
	defer releaseLease()
	latest, latestErr := s.store.LatestHistory(symbol, 3000)
	if latestErr != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), latestErr.Error())
	}
	if historyFresh(latest["fetched_at"], maxAge) {
		return historyResult(symbol, latest["bars"], latest["fetched_at"], "sqlite", nil)
	}
	loader, ok := s.provider.(HistoryProvider)
	if !ok {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), "market provider does not support daily history")
	}
	result, fetchErr := guardedHistoryCall(s, ctx, func() (provider.HistoryResult, error) {
		return loader.History(ctx, symbol, period)
	})
	if fetchErr != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), fetchErr.Error())
	}
	if len(result.Bars) == 0 || result.FetchedAt.IsZero() {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), "上游返回空历史行情，继续使用 SQLite 缓存")
	}
	if err := s.store.WriteHistory(symbol, result.Bars, result.FetchedAt); err != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), err.Error())
	}
	updated, err := s.store.LatestHistory(symbol, 3000)
	if err != nil {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), err.Error())
	}
	updatedBars, _ := updated["bars"].([]map[string]any)
	if len(updatedBars) == 0 {
		return historyResult(symbol, cached["bars"], cached["fetched_at"], cacheSource(cached["bars"], cached["fetched_at"]), "上游历史行情未包含有效收盘价，继续使用 SQLite 缓存")
	}
	return historyResult(symbol, updatedBars, updated["fetched_at"], "upstream", nil)
}

func (s *Service) acquireHistoryLease(ctx context.Context, name string) (func(), error) {
	owner := fmt.Sprintf("go-history-%d-%d", time.Now().UnixNano(), time.Now().Unix())
	deadline := time.NewTimer(historyLeaseWait(s.cfg.LowMemory))
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		acquired, err := s.store.TryAcquireServiceLeaseContext(ctx, name, owner, historyLeaseDuration)
		if err != nil {
			return nil, err
		}
		if acquired {
			return func() { _ = s.store.ReleaseServiceLease(name, owner) }, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("history lease wait timed out for %s", name)
		case <-ticker.C:
		}
	}
}

func (s *Service) loadExtremes(ctx context.Context, symbol string, history map[string]any) map[string]any {
	cached, err := s.store.LatestPayload("price_extremes", symbol)
	if err != nil {
		return map[string]any{"extremes": nil, "source": "none", "warning": err.Error()}
	}
	if usableFreshExtremes(cached, history, s.cfg.ExtremesMaxAge) {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": "sqlite", "warning": nil}
	}
	if s.cfg.LowMemory {
		bars, _ := history["bars"].([]map[string]any)
		if len(bars) > 0 {
			payload := calculatePriceExtremes(bars)
			fetched, ok := historyTimestamp(history["fetched_at"])
			if !ok {
				return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": "低内存保护：日线抓取时间不可用"}
			}
			if writeErr := s.store.WritePayload("price_extremes", symbol, payload, fetched); writeErr != nil {
				return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": writeErr.Error()}
			}
			return map[string]any{"extremes": payload, "fetched_at": fetched, "source": "sqlite", "warning": "低内存保护：历史极值按两年日线计算"}
		}
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": "低内存保护：等待日线缓存后再计算历史极值"}
	}
	unlock, lockErr := s.historyLock(ctx, "extremes:"+symbol, 0)
	if lockErr != nil {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": lockErr.Error()}
	}
	defer unlock()
	cached, err = s.store.LatestPayload("price_extremes", symbol)
	if err != nil {
		return map[string]any{"extremes": nil, "source": "none", "warning": err.Error()}
	}
	if usableFreshExtremes(cached, history, s.cfg.ExtremesMaxAge) {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": "sqlite", "warning": nil}
	}
	releaseLease, leaseErr := s.acquireHistoryLease(ctx, "history:extremes:"+symbol)
	if leaseErr != nil {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": leaseErr.Error()}
	}
	defer releaseLease()
	latest, latestErr := s.store.LatestPayload("price_extremes", symbol)
	if latestErr != nil {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": latestErr.Error()}
	}
	if usableFreshExtremes(latest, history, s.cfg.ExtremesMaxAge) {
		return map[string]any{"extremes": latest["extremes"], "fetched_at": latest["fetched_at"], "source": "sqlite", "warning": nil}
	}
	var payload map[string]any
	var fetchedAt time.Time
	var compactClient provider.PriceExtremesHistoryClient
	if source, ok := s.provider.(provider.PriceExtremesHistorySource); ok {
		compactClient = source.PriceExtremesHistoryClient()
	}
	if compactClient != nil {
		type compactHistoryFetch struct {
			points    []provider.HistoryExtremePoint
			fetchedAt time.Time
		}
		result, fetchErr := guardedHistoryCall(s, ctx, func() (compactHistoryFetch, error) {
			points, fetchedAt, err := compactClient.FetchPriceExtremes(ctx, symbol)
			return compactHistoryFetch{points: points, fetchedAt: fetchedAt}, err
		})
		if fetchErr != nil {
			return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": fetchErr.Error()}
		}
		payload = calculatePriceExtremesPoints(result.points)
		fetchedAt = result.fetchedAt
	} else {
		loader, ok := s.provider.(HistoryProvider)
		if !ok {
			return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": "market provider does not support daily history"}
		}
		result, fetchErr := guardedHistoryCall(s, ctx, func() (provider.HistoryResult, error) {
			return loader.History(ctx, symbol, "max")
		})
		if fetchErr != nil {
			return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": fetchErr.Error()}
		}
		payload = calculatePriceExtremes(result.Bars)
		fetchedAt = result.FetchedAt
	}
	if payload == nil {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": "upstream history contains no valid high/low bars"}
	}
	if err := s.store.WritePayload("price_extremes", symbol, payload, fetchedAt); err != nil {
		return map[string]any{"extremes": cached["extremes"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["extremes"], cached["fetched_at"]), "warning": err.Error()}
	}
	return map[string]any{"extremes": payload, "fetched_at": fetchedAt, "source": "upstream", "warning": nil}
}

// A previous go-yfinance range=max daily request could be silently coarsened
// by Yahoo into monthly bars. Such a cache still has a recent fetched_at, so
// age-only validation would keep serving incomplete extrema for a full day.
// Compare its 52-week bar count with the regular daily-history cache, which is
// already loaded for this endpoint, and force a max refresh when it is sparse.
func usableFreshExtremes(cached, history map[string]any, maxAge time.Duration) bool {
	if !historyFresh(cached["fetched_at"], maxAge) {
		return false
	}
	payload, ok := cached["extremes"].(map[string]any)
	if !ok {
		return true
	}
	cachedWindow := numberValue(payload["window_bars"])
	if cachedWindow == nil {
		return true
	}
	reference, ok := payload["reference_date"].(string)
	if !ok || len(reference) < len(time.DateOnly) {
		return true
	}
	day, err := time.Parse(time.DateOnly, reference[:len(time.DateOnly)])
	if err != nil {
		return true
	}
	bars, _ := history["bars"].([]map[string]any)
	if len(bars) == 0 {
		return true
	}
	cutoff := day.AddDate(0, 0, -365).Format(time.DateOnly)
	dailyWindow := 0
	for _, bar := range bars {
		date, _ := bar["date"].(string)
		if len(date) >= len(time.DateOnly) && date[:len(time.DateOnly)] >= cutoff {
			dailyWindow++
		}
	}
	return dailyWindow < 180 || *cachedWindow*3 >= float64(dailyWindow)
}

func (s *Service) loadBeta(ctx context.Context, symbol string, stockBars []map[string]any) map[string]any {
	cached, err := s.store.LatestPayload("beta_snapshots", symbol)
	if err != nil {
		return map[string]any{"beta": nil, "source": "none", "warning": err.Error()}
	}
	if historyFresh(cached["fetched_at"], 24*time.Hour) {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": "sqlite", "warning": nil}
	}
	unlock, lockErr := s.historyLock(ctx, "beta:"+symbol, 0)
	if lockErr != nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": lockErr.Error()}
	}
	defer unlock()
	cached, err = s.store.LatestPayload("beta_snapshots", symbol)
	if err != nil {
		return map[string]any{"beta": nil, "source": "none", "warning": err.Error()}
	}
	if historyFresh(cached["fetched_at"], 24*time.Hour) {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": "sqlite", "warning": nil}
	}
	releaseLease, leaseErr := s.acquireHistoryLease(ctx, "history:beta:"+symbol)
	if leaseErr != nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": leaseErr.Error()}
	}
	defer releaseLease()
	latest, latestErr := s.store.LatestPayload("beta_snapshots", symbol)
	if latestErr != nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": latestErr.Error()}
	}
	if historyFresh(latest["fetched_at"], 24*time.Hour) {
		return map[string]any{"beta": latest["beta"], "fetched_at": latest["fetched_at"], "source": "sqlite", "warning": nil}
	}
	cached = latest
	loader, ok := s.provider.(benchmarkHistoryProvider)
	if !ok {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": "market provider does not support benchmark history"}
	}
	fetchBenchmark := func() (provider.HistoryResult, error) {
		return loader.BenchmarkHistory(ctx, "^GSPC", "2y")
	}
	var benchmark provider.HistoryResult
	var fetchErr error
	if len(stockBars) == 0 && !s.cfg.LowMemory && s.cfg.UpstreamConcurrency > 1 {
		stockResult := make(chan []map[string]any, 1)
		benchmarkResult := make(chan struct {
			value provider.HistoryResult
			err   error
		}, 1)
		go func() {
			stockHistory := s.loadHistory(ctx, symbol, "2y")
			bars, _ := stockHistory["bars"].([]map[string]any)
			stockResult <- bars
		}()
		go func() {
			value, err := guardedHistoryCall(s, ctx, fetchBenchmark)
			benchmarkResult <- struct {
				value provider.HistoryResult
				err   error
			}{value: value, err: err}
		}()
		stockBars = <-stockResult
		result := <-benchmarkResult
		benchmark, fetchErr = result.value, result.err
	} else {
		if len(stockBars) == 0 {
			stockHistory := s.loadHistory(ctx, symbol, "2y")
			stockBars, _ = stockHistory["bars"].([]map[string]any)
		}
		if len(stockBars) == 0 {
			return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": "两年标的历史日线不可用"}
		}
		benchmark, fetchErr = guardedHistoryCall(s, ctx, fetchBenchmark)
	}
	if len(stockBars) == 0 {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": "两年标的历史日线不可用"}
	}
	if fetchErr != nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": fetchErr.Error()}
	}
	beta := calculateBeta(stockBars, benchmark.Bars)
	if beta == nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": "两年共同交易日不足，无法计算 Beta"}
	}
	fetchedAt := time.Now().UTC()
	if err := s.store.WritePayload("beta_snapshots", symbol, beta, fetchedAt); err != nil {
		return map[string]any{"beta": cached["beta"], "fetched_at": cached["fetched_at"], "source": cacheSource(cached["beta"], cached["fetched_at"]), "warning": err.Error()}
	}
	return map[string]any{"beta": beta, "fetched_at": fetchedAt, "source": "upstream", "warning": nil}
}

func historyTimestamp(raw any) (time.Time, bool) {
	switch value := raw.(type) {
	case time.Time:
		return value, true
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04:05.999999", time.DateOnly} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func historyFresh(raw any, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	timestamp, ok := historyTimestamp(raw)
	if !ok {
		return false
	}
	age := time.Since(timestamp)
	if age < 0 {
		age = 0
	}
	return age < maxAge
}

// loadEarnings reads a legacy local snapshot only. Runtime quote and levels
// requests do not call this helper or refresh earnings data.
func (s *Service) loadEarnings(_ context.Context, symbol string) map[string]any {
	cached, err := s.store.LatestPayload("earnings_snapshots", symbol)
	if err != nil {
		return map[string]any{"dates": []string{}, "source": "none", "warning": err.Error()}
	}
	cached = normalizeEarningsCache(cached)
	cached["source"] = cacheSource(cached["dates"], cached["fetched_at"])
	return cached
}

func normalizeEarningsCache(cached map[string]any) map[string]any {
	raw, exists := cached["dates"]
	if !exists {
		return map[string]any{}
	}
	var dates []string
	switch values := raw.(type) {
	case []string:
		dates = append([]string(nil), values...)
	case []any:
		dates = make([]string, 0, len(values))
		for _, value := range values {
			dates = append(dates, fmt.Sprint(value))
		}
	default:
		return map[string]any{}
	}
	return map[string]any{"dates": dates, "fetched_at": cached["fetched_at"]}
}

func historyResult(symbol string, rawBars any, fetchedAt any, source string, warning any) map[string]any {
	bars, _ := rawBars.([]map[string]any)
	if bars == nil {
		bars = []map[string]any{}
	}
	if timestamp, ok := historyTimestamp(fetchedAt); ok {
		fetchedAt = timestamp
	}
	return map[string]any{"symbol": symbol, "bars": bars, "fetched_at": fetchedAt, "source": source, "warning": warning}
}

func cacheSource(data any, fetchedAt ...any) string {
	if len(fetchedAt) > 0 && fetchedAt[0] != nil && fetchedAt[0] != "" {
		return "sqlite"
	}
	if data == nil {
		return "none"
	}
	switch list := data.(type) {
	case []string:
		if len(list) == 0 {
			return "none"
		}
	case []any:
		if len(list) == 0 {
			return "none"
		}
	case []map[string]any:
		if len(list) == 0 {
			return "none"
		}
	}
	return "sqlite"
}

func historyLeaseWait(lowMemory bool) time.Duration {
	if lowMemory {
		return 3 * time.Second
	}
	return 60 * time.Second
}

func calculatePriceExtremes(bars []map[string]any) map[string]any {
	points := make([]provider.HistoryExtremePoint, 0, len(bars))
	for _, bar := range bars {
		day, ok := bar["date"].(string)
		if !ok || len(day) < 10 {
			continue
		}
		parsedDay, err := time.Parse(time.DateOnly, day[:10])
		if err != nil {
			continue
		}
		high, highOK := numericHistoryValue(bar["high"])
		low, lowOK := numericHistoryValue(bar["low"])
		points = append(points, provider.HistoryExtremePoint{
			Date: parsedDay, High: high, Low: low, HasHigh: highOK, HasLow: lowOK,
		})
	}
	return calculatePriceExtremesPoints(points)
}

func calculatePriceExtremesPoints(points []provider.HistoryExtremePoint) map[string]any {
	valid := points[:0]
	for _, point := range points {
		if !point.Date.IsZero() {
			valid = append(valid, point)
		}
	}
	points = valid
	if len(points) == 0 {
		return nil
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Date.Before(points[j].Date) })
	reference := points[len(points)-1].Date
	cutoff := reference.AddDate(0, 0, -365)
	extreme := func(windowOnly, wantHigh bool) any {
		bestSet := false
		var best float64
		var bestDate string
		for _, item := range points {
			if windowOnly && item.Date.Before(cutoff) {
				continue
			}
			value, ok := item.Low, item.HasLow
			if wantHigh {
				value, ok = item.High, item.HasHigh
			}
			if !ok {
				continue
			}
			if !bestSet || (wantHigh && value > best) || (!wantHigh && value < best) {
				bestSet, best, bestDate = true, value, item.Date.Format(time.DateOnly)
			}
		}
		if !bestSet {
			return nil
		}
		return map[string]any{"price": roundPythonDecimal(best, 4), "date": bestDate}
	}
	windowBars := 0
	for _, item := range points {
		if !item.Date.Before(cutoff) {
			windowBars++
		}
	}
	return map[string]any{
		"week52":      map[string]any{"high": extreme(true, true), "low": extreme(true, false)},
		"all_time":    map[string]any{"high": extreme(false, true), "low": extreme(false, false)},
		"window_days": 365, "reference_date": reference.Format(time.DateOnly), "bars": len(points), "window_bars": windowBars,
	}
}

func numericHistoryValue(raw any) (float64, bool) {
	value := numberValue(raw)
	if value == nil {
		return 0, false
	}
	return *value, true
}

func calculateBeta(stock, benchmark []map[string]any) map[string]any {
	toMap := func(bars []map[string]any) map[string]float64 {
		result := map[string]float64{}
		for _, bar := range bars {
			day, _ := bar["date"].(string)
			close, ok := numericHistoryValue(bar["close"])
			if len(day) >= 10 && ok && close > 0 {
				result[day[:10]] = close
			}
		}
		return result
	}
	stockMap, benchmarkMap := toMap(stock), toMap(benchmark)
	days := make([]string, 0)
	for day := range stockMap {
		if _, ok := benchmarkMap[day]; ok {
			days = append(days, day)
		}
	}
	sort.Strings(days)
	if len(days) < 31 {
		return nil
	}
	stockReturns, benchmarkReturns := make([]float64, 0, len(days)-1), make([]float64, 0, len(days)-1)
	for i := 1; i < len(days); i++ {
		previous, current := days[i-1], days[i]
		stockReturns = append(stockReturns, stockMap[current]/stockMap[previous]-1)
		benchmarkReturns = append(benchmarkReturns, benchmarkMap[current]/benchmarkMap[previous]-1)
	}
	if len(stockReturns) < 30 {
		return nil
	}
	stockMean, benchmarkMean := 0.0, 0.0
	for i := range stockReturns {
		stockMean += stockReturns[i]
		benchmarkMean += benchmarkReturns[i]
	}
	stockMean /= float64(len(stockReturns))
	benchmarkMean /= float64(len(benchmarkReturns))
	covariance, variance := 0.0, 0.0
	for i := range stockReturns {
		covariance += (stockReturns[i] - stockMean) * (benchmarkReturns[i] - benchmarkMean)
		variance += (benchmarkReturns[i] - benchmarkMean) * (benchmarkReturns[i] - benchmarkMean)
	}
	if variance <= 0 {
		return nil
	}
	value := covariance / variance
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return map[string]any{"value": roundPythonDecimal(value, 4), "benchmark": "标普500", "benchmark_symbol": "^GSPC", "period": "2y", "period_label": "2年", "observations": len(stockReturns)}
}

func (s *Service) decorateQuote(ctx context.Context, quote model.Quote) model.Quote {
	if quote.Symbol == "" {
		return quote
	}
	// The fair-value worker writes its result independently of the market quote
	// snapshot. Reattach that durable result on every lightweight quote read.
	s.mergeCachedValuation(quote.Symbol, &quote)
	var history map[string]any
	history = s.loadHistory(ctx, quote.Symbol, "2y")
	bars, _ := history["bars"].([]map[string]any)
	if latestDay := historyLatestDate(bars); len(bars) > 0 && historyFresh(history["fetched_at"], s.cfg.HistoryMaxAge) && historyWithinQuoteAlignmentWindow(latestDay, time.Now()) {
		market := trendMarket(bars, quote)
		if previous, ok := positiveHistoryNumber(market["previous_close"]); ok {
			quote.PreviousClose = &previous
			if price := positiveHistoryNumberPointer(quote.Price); price != nil {
				change := (*price - previous) / previous * 100
				if !math.IsNaN(change) && !math.IsInf(change, 0) {
					quote.ChangePercent = &change
				}
			}
		}
	}
	return quote
}

// decorateCachedQuote enriches a SQLite quote without touching an upstream
// provider. It is used by the lightweight quote endpoint so a symbol with a
// persisted snapshot renders immediately; stale history is refreshed in the
// background.
func (s *Service) decorateCachedQuote(quote model.Quote) model.Quote {
	if quote.Symbol == "" {
		return quote
	}
	s.mergeCachedValuation(quote.Symbol, &quote)
	if history, err := s.store.LatestHistory(quote.Symbol, 3000); err == nil {
		bars, _ := history["bars"].([]map[string]any)
		if latestDay := historyLatestDate(bars); len(bars) > 0 && historyWithinQuoteAlignmentWindow(latestDay, time.Now()) {
			market := trendMarket(bars, quote)
			if previous, ok := positiveHistoryNumber(market["previous_close"]); ok {
				quote.PreviousClose = &previous
				if price := positiveHistoryNumberPointer(quote.Price); price != nil {
					change := (*price - previous) / previous * 100
					if !math.IsNaN(change) && !math.IsInf(change, 0) {
						quote.ChangePercent = &change
					}
				}
			}
		}
	}
	return quote
}

func historyWithinQuoteAlignmentWindow(latestDay, now time.Time) bool {
	if latestDay.IsZero() {
		return false
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	latest := latestDay.In(location)
	today := now.In(location)
	latestDate := time.Date(latest.Year(), latest.Month(), latest.Day(), 0, 0, 0, 0, time.UTC)
	todayDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	return todayDate.Sub(latestDate) <= 7*24*time.Hour
}

func historyLatestDate(bars []map[string]any) time.Time {
	latestDay := ""
	for _, bar := range bars {
		day, ok := bar["date"].(string)
		if !ok || day == "" {
			continue
		}
		if len(day) > 10 {
			day = day[:10]
		}
		if day > latestDay {
			latestDay = day
		}
	}
	if latestDay == "" {
		return time.Time{}
	}
	latest, err := time.Parse(time.DateOnly, latestDay)
	if err != nil {
		return time.Time{}
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return latest.Add(16 * time.Hour)
	}
	return time.Date(latest.Year(), latest.Month(), latest.Day(), 16, 0, 0, 0, location)
}

func positiveHistoryNumber(raw any) (float64, bool) {
	value, ok := numericHistoryValue(raw)
	return value, ok && value > 0
}

func positiveHistoryNumberPointer(raw *float64) *float64 {
	if raw == nil || *raw <= 0 || math.IsNaN(*raw) || math.IsInf(*raw, 0) {
		return nil
	}
	return raw
}
