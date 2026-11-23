package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
)

type earlyIntradayProvider struct {
	*testMarketProvider
	started, release chan struct{}
	calls            atomic.Int32
}

func (p *earlyIntradayProvider) IntradayHistory(ctx context.Context, symbol, period string) (provider.HistoryResult, error) {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	return provider.HistoryResult{Symbol: symbol, FetchedAt: time.Now(), Source: "test", Bars: []map[string]any{{"date": "2026-10-23 09:45", "open": 100.0, "high": 103.0, "low": 99.0, "close": 102.0, "volume": 100.0}}}, nil
}

// 服务先显示缓存、再异步更新，35秒旧缓存不能阻挡新K线；并发读取只抓取一次。
func TestChanIntradayThirtySecondCacheAndAsyncRefresh(t *testing.T) {
	s, _ := testService(t, "")
	p := &earlyIntradayProvider{testMarketProvider: &testMarketProvider{}, started: make(chan struct{}, 1), release: make(chan struct{})}
	s.provider = p
	s.intradayHistoryCache["QQQ"] = intradayCacheEntry{fetchedAt: time.Now().Add(-20 * time.Second), frame: []map[string]any{{"date": "2026-10-23 09:30", "close": 100.0}}}
	_, source, _, refreshing := s.intradayFrame("QQQ")
	if refreshing || source != "memory" || p.calls.Load() != 0 {
		t.Fatal("新鲜缓存没有复用")
	}
	s.intradayMu.Lock()
	entry := s.intradayHistoryCache["QQQ"]
	entry.fetchedAt = time.Now().Add(-35 * time.Second)
	s.intradayHistoryCache["QQQ"] = entry
	s.intradayMu.Unlock()
	stale, _, _, refreshing := s.intradayFrame("QQQ")
	if !refreshing || stale.frame[0]["close"] != 100.0 {
		close(p.release)
		t.Fatal("旧缓存未立即显示并开始刷新")
	}
	select {
	case <-p.started:
	case <-time.After(time.Second):
		close(p.release)
		t.Fatal("后台刷新未启动")
	}
	s.intradayFrame("QQQ")
	if p.calls.Load() != 1 {
		close(p.release)
		t.Fatal("并发读取重复抓取")
	}
	close(p.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.intradayMu.Lock()
		updated := s.intradayHistoryCache["QQQ"]
		s.intradayMu.Unlock()
		if updated.frame[0]["close"] == 102.0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("后台未写回新的K线")
}

// 2026-11-23，Codex：实际 ORCL 预警必须经过完整/compact API 和缓存命中路径。
func TestChanAdjustedStructureWarningsSurviveCompactAndCache(t *testing.T) {
	service, ts := testService(t, "")
	data, err := os.ReadFile("../chan/testdata/orcl_daily_2026.json")
	if err != nil {
		t.Fatal(err)
	}
	var bars []map[string]any
	if err := json.Unmarshal(data, &bars); err != nil {
		t.Fatal(err)
	}
	if err := service.store.WriteChanAdjustedHistory("ORCL", "1d", "split_dividend_auto", "go-yfinance", bars, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?spot=103.78", "?spot=103.78&compact=true"} {
		response, err := http.Get(ts.URL + "/api/chan/ORCL/adjusted" + query)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			RuleVersion string             `json:"rule_version"`
			Daily       chananalysis.Frame `json:"daily"`
			Weekly      chananalysis.Frame `json:"weekly"`
		}
		err = json.NewDecoder(response.Body).Decode(&payload)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || payload.RuleVersion != chananalysis.RecursivePreviewVersion {
			t.Fatalf("响应状态/版本错误：%d %q %v", response.StatusCode, payload.RuleVersion, err)
		}
		if len(payload.Daily.StructureWarnings) == 0 || payload.Weekly.StructureWarnings == nil {
			t.Fatalf("%s 遗漏独立预警数组", query)
		}
		if len(payload.Daily.EarlyRiskWarnings) == 0 || payload.Weekly.EarlyRiskWarnings == nil {
			t.Fatalf("%s 遗漏早期风险数组", query)
		}
		for _, risk := range payload.Daily.EarlyRiskWarnings {
			if risk.Symbol != "ORCL" || risk.Date != risk.ObservedAt || risk.ProtectionPrice == nil || risk.ConfirmedAt != "" {
				t.Fatalf("API早期风险契约错误：%+v", risk)
			}
		}
		found := false
		for _, warning := range payload.Daily.StructureWarnings {
			if warning.Symbol != "ORCL" || warning.MarketEnvironment == nil || warning.MarketEnvironment.Date > warning.ObservedAt {
				t.Fatalf("预警的标的或观察时环境错误：%+v", warning)
			}
			if warning.Kind == "rebound_failure_sell" && warning.Date == "2026-10-29" && warning.ObservedAt == "2026-11-02" {
				found = true
			}
		}
		if !found {
			t.Fatal("API 遗漏 148.48 反弹承压预警")
		}
	}
}

func TestAttachStructureWarningMarketEnvironmentUsesObservationDate(t *testing.T) {
	frame := chananalysis.Frame{AsOf: "2026-11-20", StructureWarnings: []chananalysis.V4Signal{{Date: "2026-10-29", ObservedAt: "2026-11-02"}}}
	series := []chananalysis.MarketEnvironment{
		{Date: "2026-10-29", Status: "classified", Regime: "bull"},
		{Date: "2026-11-02", Status: "classified", Regime: "bear"},
		{Date: "2026-11-20", Status: "classified", Regime: "range"},
	}
	attachMarketEnvironment(&frame, series)
	if frame.StructureWarnings[0].MarketEnvironment.Regime != "bear" || frame.MarketEnvironment.Regime != "range" {
		t.Fatal("预警借用了端点日期或未来市场环境")
	}
}

func TestFutureTradingDatesUseMarketCalendar(t *testing.T) {
	frame := chananalysis.Frame{Period: "1d", Candles: []chananalysis.Bar{{Date: "2026-11-25"}}}

	futuresDates := futureTradingDates(frame, true)
	if len(futuresDates) != 1 || futuresDates[0] != "2026-11-26" {
		t.Fatalf("futures future dates = %v, want weekday 2026-11-26 even though XNYS is closed", futuresDates)
	}

	stockDates := futureTradingDates(frame, false)
	if len(stockDates) != 1 || stockDates[0] != "2026-11-27" {
		t.Fatalf("stock future dates = %v, want next XNYS session 2026-11-27", stockDates)
	}
}

func TestFutureTradingDatesSupportWeeklyAndIntradayPeriods(t *testing.T) {
	weekly := chananalysis.Frame{Period: "1wk", Candles: []chananalysis.Bar{
		{Date: "2026-10-30"}, {Date: "2026-11-06"}, {Date: "2026-11-13"}, {Date: "2026-11-20"},
	}}
	weeklyDates := futureTradingDates(weekly, false)
	if len(weeklyDates) != 4 || weeklyDates[0] != "2026-11-27" || weeklyDates[1] != "2026-12-04" {
		t.Fatalf("weekly future dates=%v, want consecutive XNYS week endings", weeklyDates)
	}

	lastBars := map[string]string{"15m": "2026-11-25 15:45", "1h": "2026-11-25 15:00", "2h": "2026-11-25 13:30", "4h": "2026-11-25 09:30"}
	for period, lastDate := range lastBars {
		frame := chananalysis.Frame{Period: period, Candles: []chananalysis.Bar{{Date: "2026-11-24 09:30"}, {Date: "2026-11-25 09:30"}, {Date: lastDate}}}
		stockDates := futureTradingDates(frame, false)
		futureDates := futureTradingDates(frame, true)
		if len(stockDates) != 3 || len(futureDates) != 3 {
			t.Fatalf("%s future dates stock=%v futures=%v, want three slots", period, stockDates, futureDates)
		}
		wantStockDate := "2026-11-27 09:30"
		if period == "4h" {
			wantStockDate = "2026-11-30 09:30"
		}
		if stockDates[0] != wantStockDate {
			t.Fatalf("%s stock dates=%v, want skip Thanksgiving", period, stockDates)
		}
		wantFutureDate := "2026-11-26 09:30"
		if futureDates[0] != wantFutureDate {
			t.Fatalf("%s futures dates=%v, want weekday slot on Thanksgiving", period, futureDates)
		}
	}
}

func TestIntradayDailyBarFallbackKeepsIntradayOpenWhenQuoteOpenConflicts(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(location).Format(time.DateOnly)
	firstBar := map[string]any{
		"date": today + " 09:30", "open": float64(282.48), "high": float64(282.93),
		"low": float64(278.30), "close": float64(280.10), "volume": float64(1000),
	}
	latestBar := map[string]any{
		"date": today + " 09:45", "open": float64(280.10), "high": float64(282.50),
		"low": float64(279.80), "close": float64(281.55), "volume": float64(2000),
	}
	service := &Service{intradayHistoryCache: map[string]intradayCacheEntry{
		"GOOG": {
			frame:     []map[string]any{firstBar, latestBar},
			fetchedAt: time.Now(),
		},
	}}
	quoteOpen, price := 283.805, 281.55
	bar := service.intradayDailyBarFallback("GOOG", model.Quote{
		MarketState: "REGULAR",
		TodayOpen:   &quoteOpen,
		Price:       &price,
	})
	if bar == nil {
		t.Fatal("fallback returned no daily bar when quote open was outside the intraday range")
	}
	if bar.Date != today || bar.Open != 282.48 || bar.High != 282.93 || bar.Low != 278.30 || bar.Close != 281.55 {
		t.Fatalf("fallback daily bar = %#v, want today's intraday OHLC", bar)
	}
	if !bar.IsForming {
		t.Fatal("regular-session daily bar should be marked as forming")
	}
}
