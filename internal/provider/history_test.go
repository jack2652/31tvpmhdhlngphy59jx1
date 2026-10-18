package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	ym "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestNormalizeYFinanceHistoryPreservesBarsAndExtremes(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	result, err := normalizeYFinanceHistory("QQQ", "max", []ym.Bar{
		{Date: time.Date(2025, 1, 2, 15, 0, 0, 0, time.UTC), Open: 10, High: 12, Low: 9, Close: 11, Volume: 100},
		{Date: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC), Open: 20, High: 24, Low: 18, Close: 22, Volume: 200},
	}, location)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != "" || len(result.Bars) != 2 {
		t.Fatalf("unexpected normalized result: %#v", result)
	}
	if result.Bars[0]["date"] != "2025-01-02" || result.Bars[1]["close"] != float64(22) {
		t.Fatalf("unexpected bars: %#v", result.Bars)
	}
	if result.AllTimeHigh == nil || *result.AllTimeHigh != 24 || result.AllTimeLow == nil || *result.AllTimeLow != 9 {
		t.Fatalf("unexpected extremes: high=%v low=%v", result.AllTimeHigh, result.AllTimeLow)
	}
}

func TestPairChanAdjustedHistoryValidatesConsistentOHLCFactor(t *testing.T) {
	date := "2026-10-16"
	makeResult := func(adjustedClose float64) (HistoryResult, HistoryResult) {
		return HistoryResult{Symbol: "QQQ", FetchedAt: time.Now(), Source: "go-yfinance", Bars: []map[string]any{{
			"date": date, "open": 49.0, "high": 53.0, "low": 48.0, "close": adjustedClose, "volume": 1000.0,
		}}}, HistoryResult{Symbol: "QQQ", FetchedAt: time.Now(), Source: "go-yfinance", Bars: []map[string]any{{
			"date": date, "open": 98.0, "high": 106.0, "low": 96.0, "close": 104.0, "volume": 1000.0,
		}}}
	}
	adjusted, raw := makeResult(52)
	joined, err := pairChanAdjustedBars(adjusted, raw)
	if err != nil || len(joined.Bars) != 1 || joined.Bars[0]["adjustment_factor"] != .5 {
		t.Fatalf("paired history=%+v err=%v", joined, err)
	}
	adjusted, raw = makeResult(52)
	adjusted.Bars[0]["high"] = 55.0
	if _, err := pairChanAdjustedBars(adjusted, raw); err == nil {
		t.Fatal("expected inconsistent adjusted OHLC factor to be rejected")
	}
}

func TestFetchChanAdjustedUsesIndependentAdjustedAndRawHistories(t *testing.T) {
	fixedTime := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	var params []ym.HistoryParams
	client := &YFinanceHistoryClient{
		TimeNow:  func() time.Time { return fixedTime },
		Location: time.UTC,
		TickerFactory: func(symbol string) (yahooTicker, error) {
			if symbol != "QQQ" {
				t.Fatalf("ticker symbol=%q, want QQQ", symbol)
			}
			return &yahooSDKTestTicker{historyFunc: func(request ym.HistoryParams) ([]ym.Bar, error) {
				params = append(params, request)
				bar := ym.Bar{Date: fixedTime, Volume: 100}
				if request.AutoAdjust {
					bar.Open, bar.High, bar.Low, bar.Close = 49, 53, 48, 52
				} else {
					bar.Open, bar.High, bar.Low, bar.Close = 98, 106, 96, 104
				}
				return []ym.Bar{bar}, nil
			}}, nil
		},
	}
	result, err := client.FetchChanAdjusted(context.Background(), "qqq", "5y")
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 2 || !params[0].AutoAdjust || params[1].AutoAdjust || params[0].Interval != "1d" || params[1].Interval != "1d" {
		t.Fatalf("history requests=%+v, want separate adjusted then raw daily queries", params)
	}
	if params[0].Period != "5y" || params[1].Period != "5y" {
		t.Fatalf("history periods=%q/%q, want five-year context for adjusted recursive analysis", params[0].Period, params[1].Period)
	}
	if result.Timeframe != "1d" || result.AdjustmentType != "split_dividend_auto" || result.Source != "go-yfinance" || !result.FetchedAt.Equal(fixedTime) || len(result.Bars) != 1 {
		t.Fatalf("Chan adjusted result=%+v, want one independently paired daily bar", result)
	}
	bar := result.Bars[0]
	if bar["close"] != 52.0 || bar["raw_close"] != 104.0 || bar["adjustment_factor"] != .5 || bar["raw_volume"] != float64(100) {
		t.Fatalf("paired Chan adjusted bar=%#v, want adjusted/raw OHLC and factor", bar)
	}
}

func TestNormalizeYFinanceHistoryPreservesMissingPriceSemantics(t *testing.T) {
	result, err := normalizeYFinanceHistory("QQQ", "6mo", []ym.Bar{
		{Date: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC), Open: 10, High: 11, Low: 9, Close: 0, Volume: 100},
		{Date: time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC), Close: 12, Volume: 0},
	}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bars) != 1 {
		t.Fatalf("got %d bars, want row with missing close omitted: %#v", len(result.Bars), result.Bars)
	}
	bar := result.Bars[0]
	if bar["date"] != "2026-01-05" || bar["close"] != float64(12) || bar["volume"] != int64(0) {
		t.Fatalf("unexpected retained bar: %#v", bar)
	}
	for _, field := range []string{"open", "high", "low"} {
		if value, ok := bar[field]; !ok || value != nil {
			t.Errorf("%s = %#v, want nil for a missing SDK numeric field", field, value)
		}
	}
}

func TestNormalizeYFinanceHistoryRejectsInvalidRows(t *testing.T) {
	for _, bar := range []ym.Bar{
		{Open: 1, High: 1, Low: 1, Close: 1, Volume: 1},
		{Date: time.Now(), Open: 1, High: 1, Low: 1, Close: 1, Volume: -1},
	} {
		if _, err := normalizeYFinanceHistory("QQQ", "1mo", []ym.Bar{bar}, time.UTC); err == nil {
			t.Fatalf("expected invalid bar error for %#v", bar)
		}
	}
	_, err := normalizeYFinanceHistory("QQQ", "6mo", []ym.Bar{
		{Date: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC), Open: 10, High: 11, Low: 9},
	}, time.UTC)
	if err == nil || err.Error() != "go-yfinance history has no valid closing prices" {
		t.Fatalf("normalizeYFinanceHistory() error = %v, want no valid closing prices", err)
	}
}

func TestYFinanceHistoryUsesSDKForRawAndAdjustedBars(t *testing.T) {
	fixedTime := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	for _, adjusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "adjusted"}[adjusted], func(t *testing.T) {
			var gotSymbol string
			var gotParams ym.HistoryParams
			ticker := &yahooSDKTestTicker{historyFunc: func(params ym.HistoryParams) ([]ym.Bar, error) {
				gotParams = params
				return []ym.Bar{{
					Date: time.Date(2026, 2, 2, 15, 0, 0, 0, time.UTC),
					Open: 10, High: 12, Low: 9, Close: 11, Volume: 100,
				}}, nil
			}}
			client := &YFinanceHistoryClient{
				Gate: make(chan struct{}, 1), GateWait: time.Second, TimeNow: func() time.Time { return fixedTime },
				TickerFactory: func(symbol string) (yahooTicker, error) { gotSymbol = symbol; return ticker, nil },
				Location:      time.UTC,
			}
			result, err := client.fetch(context.Background(), " aapl ", "1y", adjusted)
			if err != nil {
				t.Fatal(err)
			}
			if gotSymbol != "AAPL" || gotParams.Period != "1y" || gotParams.Interval != "1d" || gotParams.AutoAdjust != adjusted || gotParams.Actions || gotParams.PrePost || gotParams.KeepNA {
				t.Fatalf("SDK request symbol/params = %q/%+v", gotSymbol, gotParams)
			}
			if result.Source != "go-yfinance" || !result.FetchedAt.Equal(fixedTime) || len(result.Bars) != 1 {
				t.Fatalf("unexpected history result: %#v", result)
			}
		})
	}
}

func TestYFinanceMaxHistoryUsesExplicitRangeLikePython(t *testing.T) {
	fixedTime := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	var got ym.HistoryParams
	client := &YFinanceHistoryClient{
		TimeNow: func() time.Time { return fixedTime }, Location: time.UTC,
		TickerFactory: func(string) (yahooTicker, error) {
			return &yahooSDKTestTicker{historyFunc: func(params ym.HistoryParams) ([]ym.Bar, error) {
				got = params
				return []ym.Bar{{Date: fixedTime, Open: 10, High: 12, Low: 9, Close: 11}}, nil
			}}, nil
		},
	}
	if _, err := client.Fetch(context.Background(), "LITE", "max"); err != nil {
		t.Fatal(err)
	}
	wantEnd := time.Unix(fixedTime.Unix(), 0)
	wantStart := time.Unix(wantEnd.Unix()-3122064000+5, 0)
	if got.Period != "" || got.Start == nil || !got.Start.Equal(wantStart) || got.End == nil || !got.End.Equal(wantEnd) || got.Interval != "1d" || got.AutoAdjust {
		t.Fatalf("max history params = %+v, want explicit raw daily range [%s, %s]", got, wantStart, wantEnd)
	}
}

func TestYFinanceHistoryRejectsUnsupportedPeriodBeforeTickerCreation(t *testing.T) {
	called := false
	client := &YFinanceHistoryClient{TickerFactory: func(string) (yahooTicker, error) {
		called = true
		return nil, errors.New("unexpected ticker creation")
	}}
	if _, err := client.Fetch(context.Background(), "QQQ", "unsupported"); err == nil {
		t.Fatal("expected unsupported period error")
	}
	if called {
		t.Fatal("unsupported period reached the SDK ticker factory")
	}
}

func TestYFinanceHistorySupportsLongDailyPeriods(t *testing.T) {
	fixedTime := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, period := range []string{"10y", "20y"} {
		t.Run(period, func(t *testing.T) {
			var got ym.HistoryParams
			client := &YFinanceHistoryClient{
				TimeNow: func() time.Time { return fixedTime }, Location: time.UTC,
				TickerFactory: func(string) (yahooTicker, error) {
					return &yahooSDKTestTicker{historyFunc: func(params ym.HistoryParams) ([]ym.Bar, error) {
						got = params
						return []ym.Bar{{Date: fixedTime, Open: 10, High: 12, Low: 9, Close: 11}}, nil
					}}, nil
				},
			}
			if _, err := client.Fetch(context.Background(), "SPY", period); err != nil {
				t.Fatal(err)
			}
			if got.Period != period || got.Interval != "1d" {
				t.Fatalf("SDK request = %+v, want %s daily bars", got, period)
			}
		})
	}
}

func TestYFinanceHistorySDKCallKeepsGateUntilCanceledRequestFinishes(t *testing.T) {
	gate := make(chan struct{}, 1)
	started := make(chan struct{})
	finish := make(chan struct{})
	closed := make(chan struct{})
	client := &YFinanceHistoryClient{Gate: gate, GateWait: time.Second, TickerFactory: func(string) (yahooTicker, error) {
		return &yahooSDKTestTicker{
			historyFunc: func(ym.HistoryParams) ([]ym.Bar, error) { close(started); <-finish; return nil, errors.New("finished") },
			closed:      func() { close(closed) },
		}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Fetch(ctx, "QQQ", "1y")
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want context.Canceled", err)
	}
	select {
	case gate <- struct{}{}:
		t.Fatal("SDK gate was released before its non-context-aware call finished")
	default:
	}
	close(finish)
	<-closed
	select {
	case gate <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("SDK gate was not released after the call finished")
	}
}
