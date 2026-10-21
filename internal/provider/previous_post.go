package provider

import (
	"context"
	"time"

	"option-scope-go/internal/marketcalendar"

	"github.com/wnjoon/go-yfinance/pkg/models"
)

// previousPostSessionWindow returns the regular close and the end of the
// extended-hours session of the most recently completed post-market session.
// A normal session runs 16:00-20:00 ET while an early close ends the
// after-hours window four hours after the 13:00 close.
func previousPostSessionWindow(now time.Time) (start, end time.Time, day time.Time, ok bool) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	now = now.In(location)
	day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	for attempt := 0; attempt < 10; attempt++ {
		if marketcalendar.IsTradingDay(day) {
			closeMinute := marketcalendar.RegularCloseMinute(day)
			start = day.Add(time.Duration(closeMinute) * time.Minute)
			end = start.Add(4 * time.Hour)
			if !now.Before(end) {
				return start, end, day, true
			}
		}
		day = day.AddDate(0, 0, -1)
	}
	return time.Time{}, time.Time{}, time.Time{}, false
}

// nextPostSessionEnd returns when the current post-market basis will be
// superseded by the next completed after-hours session. It keeps the previous
// post lookup cached for the whole session instead of re-requesting the chart
// on every refresh.
func nextPostSessionEnd(now time.Time) time.Time {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	now = now.In(location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	for attempt := 0; attempt < 10; attempt++ {
		if marketcalendar.IsTradingDay(day) {
			closeMinute := marketcalendar.RegularCloseMinute(day)
			end := day.Add(time.Duration(closeMinute)*time.Minute + 4*time.Hour)
			if end.After(now) {
				return end
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return now.Add(24 * time.Hour)
}

func yahooPreMarketHistoryParams(now time.Time) (models.HistoryParams, bool) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	now = now.In(location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	if !marketcalendar.IsTradingDay(day) {
		return models.HistoryParams{}, false
	}
	start := day.Add(4 * time.Hour)
	if now.Before(start) {
		return models.HistoryParams{}, false
	}
	return models.HistoryParams{
		Interval: "1m", Start: &start, End: &now, PrePost: true,
		AutoAdjust: false, Actions: false, KeepNA: false,
	}, true
}

func yahooPreMarketSession(bars []models.Bar, now time.Time, referenceClose float64) (float64, map[string]any, bool) {
	params, ok := yahooPreMarketHistoryParams(now)
	if !ok || len(bars) == 0 {
		return 0, nil, false
	}
	location := params.Start.Location()
	dayKey := params.Start.Format(time.DateOnly)
	regularOpen := time.Date(params.Start.Year(), params.Start.Month(), params.Start.Day(), 9, 30, 0, 0, location)
	latest := time.Time{}
	price := float64(0)
	for _, bar := range bars {
		at := bar.Date.In(location)
		if at.Format(time.DateOnly) != dayKey || at.Before(*params.Start) || at.After(*params.End) || !at.Before(regularOpen) || !validYahooPrice(bar.Close) {
			continue
		}
		if !at.Before(latest) {
			latest, price = at, bar.Close
		}
	}
	if !validYahooPrice(price) {
		return 0, nil, false
	}
	session := map[string]any{
		"price": price,
		"as_of": latest.Format(time.RFC3339),
		"date":  dayKey,
	}
	if validYahooPrice(referenceClose) {
		session["reference_close"] = referenceClose
	}
	return price, session, true
}

// PreviousPostSession returns the last extended-hours price of the most recent
// completed post-market session. Yahoo clears PostMarketPrice from the quote
// payload as soon as pre-market starts, so after 04:00 ET the 1-minute chart
// with includePrePost is the only source for yesterday's after-hours price.
// The last bar of the window is used even when its volume is zero: the chart
// carries quote-only after-hours prices, and the regular close is the natural
// fallback when the source has no extended-hours bars at all.
func (y *Yahoo) PreviousPostSession(ctx context.Context, rawSymbol string) (map[string]any, bool) {
	if y == nil {
		return nil, false
	}
	symbol, err := NormalizeSymbol(rawSymbol)
	if err != nil {
		return nil, false
	}
	start, end, day, ok := previousPostSessionWindow(y.marketNow())
	if !ok {
		return nil, false
	}
	params := models.HistoryParams{
		Interval: "1m", Start: &start, End: &end, PrePost: true,
		AutoAdjust: false, Actions: false, KeepNA: false,
	}
	// Reuse the per-symbol quote ticker so the chart request does not pay for
	// another Yahoo cookie/crumb handshake.
	tickerFactory := func(string) (yahooTicker, error) { return y.quoteTicker(ctx, symbol) }
	bars, err := runYahooSDK(ctx, y.semaphore, y.gateWait, tickerFactory, symbol, func(ticker yahooTicker) ([]yahooBarModel, error) {
		return ticker.History(params)
	})
	if err != nil || len(bars) == 0 {
		return nil, false
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	dayKey := day.Format(time.DateOnly)
	last := time.Time{}
	price := float64(0)
	reference := float64(0)
	for _, bar := range bars {
		at := bar.Date.In(location)
		if at.Format(time.DateOnly) != dayKey || at.Before(start) || !at.Before(end) {
			continue
		}
		if !validYahooPrice(bar.Close) {
			continue
		}
		if at.Equal(start) {
			reference = bar.Close
		}
		if !at.Before(last) {
			last, price = at, bar.Close
		}
	}
	if !validYahooPrice(price) {
		return nil, false
	}
	session := map[string]any{"price": price, "as_of": last.Format(time.RFC3339), "date": dayKey}
	if validYahooPrice(reference) {
		session["reference_close"] = reference
	}
	return session, true
}
