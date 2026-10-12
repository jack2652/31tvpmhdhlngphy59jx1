package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnjoon/go-yfinance/pkg/models"
)

type calendarTestTicker struct {
	yahooTicker
	calendar *models.Calendar
	err      error
	started  chan struct{}
	release  <-chan struct{}
	closed   chan struct{}
}

func (t *calendarTestTicker) Calendar() (*models.Calendar, error) {
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.release != nil {
		<-t.release
	}
	return t.calendar, t.err
}

func (t *calendarTestTicker) Close() {
	if t.closed != nil {
		close(t.closed)
	}
}

func TestYahooEarningsFetchUsesSDKCalendarAndNormalizesDates(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	var gotSymbol string
	closed := make(chan struct{})
	client := &YahooEarningsClient{Client: http.DefaultClient}
	client.tickerFactory = func(symbol string) (yahooTicker, error) {
		gotSymbol = symbol
		return &calendarTestTicker{
			calendar: &models.Calendar{EarningsDate: []time.Time{
				time.Date(2032, 1, 9, 2, 0, 0, 0, time.UTC),
				time.Date(2032, 1, 9, 12, 0, 0, 0, newYork),
				time.Date(2032, 1, 9, 2, 0, 0, 0, time.UTC),
				{},
			}},
			closed: closed,
		}, nil
	}

	got, err := client.Fetch(context.Background(), " brk-b ")
	want := []string{"2032-01-08", "2032-01-09"}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Fetch() = %#v, %v; want %#v", got, err, want)
	}
	if gotSymbol != "BRK-B" {
		t.Fatalf("ticker symbol = %q, want BRK-B", gotSymbol)
	}
	select {
	case <-closed:
	default:
		t.Fatal("SDK ticker was not closed")
	}
}

func TestYahooEarningsFetchReturnsNonNilEmptyForMissingDates(t *testing.T) {
	for name, calendar := range map[string]*models.Calendar{
		"nil calendar":      nil,
		"no earnings dates": {},
	} {
		t.Run(name, func(t *testing.T) {
			client := &YahooEarningsClient{Client: http.DefaultClient}
			client.tickerFactory = func(string) (yahooTicker, error) {
				return &calendarTestTicker{calendar: calendar}, nil
			}
			got, err := client.Fetch(context.Background(), "test")
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("Fetch() = %#v, %v; want non-nil empty slice", got, err)
			}
		})
	}
}

func TestYahooEarningsFetchSanitizesSDKErrors(t *testing.T) {
	client := &YahooEarningsClient{Client: http.DefaultClient}
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &calendarTestTicker{err: errors.New("upstream included private response data")}, nil
	}
	_, err := client.Fetch(context.Background(), "test")
	if err == nil || strings.Contains(err.Error(), "private response data") {
		t.Fatalf("Fetch() error = %v, want sanitized error", err)
	}
}

func TestYahooEarningsFetchCancellationRetainsGateUntilSDKReturns(t *testing.T) {
	gate := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	releaseSDK := make(chan struct{})
	closed := make(chan struct{})
	client := &YahooEarningsClient{Client: http.DefaultClient, Gate: gate}
	client.tickerFactory = func(string) (yahooTicker, error) {
		return &calendarTestTicker{started: started, release: releaseSDK, closed: closed}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Fetch(ctx, "test")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("SDK Calendar request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Fetch() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fetch() did not return after cancellation")
	}
	if len(gate) != 1 {
		t.Fatal("shared gate released before SDK operation finished")
	}
	close(releaseSDK)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SDK ticker did not close after operation finished")
	}
	deadline := time.Now().Add(time.Second)
	for len(gate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(gate) != 0 {
		t.Fatal("shared gate was not released after SDK operation finished")
	}
}

func TestYahooEarningsFetchRejectsInvalidSymbolAndUnavailableClient(t *testing.T) {
	if _, err := (&YahooEarningsClient{Client: http.DefaultClient}).Fetch(context.Background(), "bad ticker!"); err == nil {
		t.Fatal("invalid symbol accepted")
	}
	if _, err := (&YahooEarningsClient{}).Fetch(context.Background(), "TEST"); err == nil || err.Error() != "Yahoo earnings client is unavailable" {
		t.Fatalf("Fetch() error = %v, want unavailable client", err)
	}
}
