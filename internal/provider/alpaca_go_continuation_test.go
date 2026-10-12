package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlpacaValidateCredentialsHonorsAuthenticationCooldown(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, "invalid credentials")
	}))
	defer server.Close()

	client := NewAlpaca("key", "secret", server.URL, server.Client())
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	if _, err := client.Quote(context.Background(), "TEST"); err == nil {
		t.Fatal("Quote() error = nil, want unauthorized response")
	}
	if got := client.ValidateCredentials(context.Background()); got != "invalid_credentials" {
		t.Fatalf("ValidateCredentials() during cooldown = %q, want invalid_credentials", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests during cooldown = %d, want exactly the initial request", got)
	}
}

func TestAlpacaOvernightSessionUsesEasternClockAndTradingDate(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before evening open", now: time.Date(2026, time.October, 7, 23, 59, 0, 0, time.UTC), want: false}, // 19:59 EDT
		{name: "evening open", now: time.Date(2026, time.October, 8, 0, 0, 0, 0, time.UTC), want: true},           // 20:00 EDT
		{name: "early morning before close", now: time.Date(2026, time.October, 8, 7, 59, 0, 0, time.UTC), want: true},
		{name: "early morning close", now: time.Date(2026, time.October, 8, 8, 0, 0, 0, time.UTC), want: false},
		{name: "sunday evening for monday session", now: time.Date(2026, time.October, 12, 0, 30, 0, 0, time.UTC), want: true}, // 20:30 EDT Sunday
		{name: "saturday evening", now: time.Date(2026, time.October, 11, 0, 30, 0, 0, time.UTC), want: false},                 // 20:30 EDT Friday
		{name: "holiday eve overnight closed", now: time.Date(2026, time.July, 3, 0, 0, 0, 0, time.UTC), want: false},          // 20:00 EDT July 2
		{name: "dst evening uses eastern zone", now: time.Date(2026, time.March, 9, 0, 30, 0, 0, time.UTC), want: true},        // 20:30 EDT Sunday
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNYSEOvernightSession(tc.now); got != tc.want {
				t.Errorf("isNYSEOvernightSession(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}
