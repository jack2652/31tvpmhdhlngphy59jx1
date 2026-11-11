package server

import (
	"testing"
	"time"

	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/model"
)

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
