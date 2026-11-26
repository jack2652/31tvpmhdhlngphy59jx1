package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"option-scope-go/internal/provider"
)

type chanBenchmarkRefreshProvider struct {
	*testMarketProvider
	started chan string
}

func (p *chanBenchmarkRefreshProvider) History(ctx context.Context, symbol, _ string) (provider.HistoryResult, error) {
	p.started <- symbol + "|raw"
	<-ctx.Done()
	return provider.HistoryResult{}, ctx.Err()
}

func (p *chanBenchmarkRefreshProvider) ChanAdjustedHistory(ctx context.Context, symbol, _ string) (provider.ChanAdjustedHistoryResult, error) {
	p.started <- symbol + "|adjusted"
	<-ctx.Done()
	return provider.ChanAdjustedHistoryResult{}, ctx.Err()
}

func TestChanBenchmarkRefreshesMissingClose(t *testing.T) {
	for _, sample := range []struct{ basis, period, request string }{
		{"raw", "1d", "QQQ|raw"},
		{"raw", "1wk", "QQQ|raw"},
		{"split_dividend_adjusted", "1d", "QQQ|adjusted"},
		{"split_dividend_adjusted", "1wk", "QQQ|adjusted"},
	} {
		t.Run(sample.basis+"/"+sample.period, func(t *testing.T) {
			service, _ := testService(t, "")
			loader := &chanBenchmarkRefreshProvider{testMarketProvider: &testMarketProvider{}, started: make(chan string, 1)}
			service.provider = loader
			service.cfg.HistoryMaxAge = 50 * 365 * 24 * time.Hour
			// 感恩节次日的最后一根日线快照在提前收盘前抓取。
			now := time.Date(2020, time.November, 27, 13, 5, 0, 0, newYorkLocation())
			capturedAt := now.Add(-6 * time.Minute)
			var rows []map[string]any
			for _, date := range []string{"2015-11-30", "2020-11-25", "2020-11-27"} {
				rows = append(rows, map[string]any{"date": date, "open": 100.0, "high": 101.0, "low": 99.0, "close": 100.0, "volume": 25.0,
					"raw_open": 100.0, "raw_high": 101.0, "raw_low": 99.0, "raw_close": 100.0, "raw_volume": 25.0, "adjustment_factor": 1.0})
			}
			if err := service.store.WriteHistory("QQQ", rows, capturedAt); err != nil {
				t.Fatal(err)
			}
			if err := service.store.WriteChanAdjustedHistory("QQQ", "1d", "split_dividend_auto", "go-yfinance", rows, capturedAt); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Params = gin.Params{{Key: "symbol", Value: "QQQ"}}
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/chan-benchmark/QQQ?period="+sample.period+"&basis="+sample.basis, nil)
				service.serveChanBenchmark(ctx, now)
				var response struct {
					Bars       []chanBenchmarkPoint `json:"bars"`
					Refreshing bool                 `json:"refreshing"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != http.StatusOK || !response.Refreshing {
					t.Fatalf("缺少正式收盘时应报告正在刷新，响应 %s", recorder.Body.String())
				}
				for _, bar := range response.Bars {
					if bar.Date == "2020-11-27" {
						t.Fatal("提前收盘前的快照被当作正式收盘")
					}
				}
			}
			select {
			case got := <-loader.started:
				if got != sample.request {
					t.Fatalf("刷新请求为 %s，预期 %s", got, sample.request)
				}
			case <-time.After(time.Second):
				t.Fatal("未发起基准历史刷新")
			}
		})
	}
}

// 基准路由复用各价格口径的真实 SQLite 缓存，并遵守主图的收盘日期。
func TestChanBenchmarkPriceBasisAndAlignment(t *testing.T) {
	service, site := testService(t, "")
	service.provider = &testMarketProvider{}
	fetchedAt := time.Date(2026, 1, 10, 2, 0, 0, 0, time.UTC)
	daily := []map[string]any{}
	adjusted := []map[string]any{}
	for i := 5; i <= 9; i++ {
		date := time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		price := float64(100 + i)
		daily = append(daily, map[string]any{"date": date, "open": price, "high": price, "low": price, "close": price, "volume": 25.0})
		adjusted = append(adjusted, map[string]any{"date": date, "open": price / 2, "high": price / 2, "low": price / 2, "close": price / 2,
			"volume": 25.0, "raw_open": price, "raw_high": price, "raw_low": price, "raw_close": price, "raw_volume": 25.0, "adjustment_factor": 0.5})
	}
	if err := service.store.WriteHistory("QQQ", daily, fetchedAt); err != nil {
		t.Fatal(err)
	}
	if err := service.store.WriteChanAdjustedHistory("SPY", "1d", "split_dividend_auto", "go-yfinance", adjusted, fetchedAt); err != nil {
		t.Fatal(err)
	}
	intraday := []map[string]any{}
	for i, stamp := range []string{"09:30", "09:45", "10:00", "10:15", "10:30"} {
		price := float64(100 + i)
		intraday = append(intraday, map[string]any{"date": "2026-01-09 " + stamp, "open": price, "high": price, "low": price, "close": price, "volume": 100.0})
	}
	// 最后一根在 10:35 采集，即使现在已经收盘也不能进入基准历史。
	if err := service.store.WriteIntradayHistory("QQQ", "15m", intraday, time.Date(2026, 1, 9, 15, 35, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		path, date string
		count      int
		close      float64
	}{
		{"QQQ?period=1d", "2026-01-09", 5, 109},
		{"QQQ?period=1wk", "2026-01-09", 1, 109},
		{"SPY?period=1d&basis=split_dividend_adjusted", "2026-01-09", 5, 54.5},
		{"SPY?period=1wk&basis=split_dividend_adjusted", "2026-01-09", 1, 54.5},
		{"QQQ?period=15m", "2026-01-09 10:15", 4, 103},
		{"QQQ?period=1h", "2026-01-09 09:30", 1, 103},
		{"SPY?period=15m", "", 0, 0},
	} {
		t.Run(sample.path, func(t *testing.T) {
			response, err := http.Get(site.URL + "/api/chan-benchmark/" + sample.path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result struct {
				Bars []chanBenchmarkPoint `json:"bars"`
				Live *chanBenchmarkPoint  `json:"live_bar"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || result.Bars == nil || len(result.Bars) != sample.count || result.Live != nil {
				t.Fatalf("基准响应：状态 %d，数据 %+v", response.StatusCode, result)
			}
			if sample.count > 0 {
				last := result.Bars[len(result.Bars)-1]
				if last.Date != sample.date || last.Close != sample.close || last.IsForming {
					t.Fatalf("最后一根已收盘基准 %+v，预期 %s/%v", last, sample.date, sample.close)
				}
			}
		})
	}
	for _, path := range []string{"ORCL", "SPY?period=5m", "QQQ?basis=unknown", "QQQ?period=1h&basis=split_dividend_adjusted"} {
		response, err := http.Get(site.URL + "/api/chan-benchmark/" + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s 响应码 %d，预期 422", path, response.StatusCode)
		}
	}
}

func TestChanBenchmarkSeparatesLivePricesFromClosedHistory(t *testing.T) {
	service, _ := testService(t, "")
	service.provider = &testMarketProvider{}
	location := newYorkLocation()
	now := time.Date(2026, 10, 23, 9, 37, 0, 0, location)
	daily := []map[string]any{
		{"date": "2026-10-22", "open": 100.0, "high": 101.0, "low": 99.0, "close": 100.0, "volume": 25.0},
		{"date": "2026-10-23", "open": 104.0, "high": 110.0, "low": 100.0, "close": 105.0, "volume": 30.0},
	}
	if err := service.store.WriteHistory("QQQ", daily, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	service.intradayHistoryCache["QQQ"] = intradayCacheEntry{
		fetchedAt: now.Add(-time.Minute),
		frame:     []map[string]any{{"date": "2026-10-23 09:30", "open": 103.0, "high": 108.0, "low": 102.0, "close": 106.0, "volume": 20.0}},
	}
	for _, sample := range []struct {
		period, liveDate string
		closedCount      int
	}{
		{"1d", "2026-10-23", 1},
		{"15m", "2026-10-23 09:30", 0},
		{"1h", "2026-10-23 09:30", 0},
		{"1wk", "", 0},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "symbol", Value: "QQQ"}}
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/chan-benchmark/QQQ?period="+sample.period, nil)
		service.serveChanBenchmark(ctx, now)
		var payload struct {
			Bars []chanBenchmarkPoint `json:"bars"`
			Live *chanBenchmarkPoint  `json:"live_bar"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || len(payload.Bars) != sample.closedCount {
			t.Fatalf("%s 历史状态 %d，响应 %+v", sample.period, recorder.Code, payload)
		}
		if sample.liveDate == "" {
			if payload.Live != nil {
				t.Fatalf("%s 不应提供形成中周线：%+v", sample.period, payload.Live)
			}
		} else if payload.Live == nil || payload.Live.Date != sample.liveDate || payload.Live.Close != 106 ||
			!payload.Live.IsForming || payload.Live.SourceDate != "2026-10-23 09:30" {
			t.Fatalf("%s 形成中行情错误：%+v", sample.period, payload.Live)
		}
	}
}
