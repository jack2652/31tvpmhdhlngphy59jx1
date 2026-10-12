package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAlpacaQuoteNormalizesNaiveTimestampAsUTC(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("symbols"); got != "TEST" {
			t.Errorf("symbols query = %q, want TEST", got)
		}
		if got := r.URL.Query().Get("feed"); got != "overnight" {
			t.Errorf("feed query = %q, want overnight", got)
		}
		_, _ = fmt.Fprint(w, `{"TEST":{"latestQuote":{"bp":10,"ap":12,"t":"2025-03-04T02:14:59.123456789"}}}`)
	}))
	defer server.Close()

	quote, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	session, ok := quote.Sessions["overnight"].(map[string]any)
	if !ok {
		t.Fatalf("overnight session = %#v", quote.Sessions["overnight"])
	}
	if got, want := session["as_of"], "2025-03-04T02:14:59.123456+00:00"; got != want {
		t.Fatalf("session as_of = %#v, want %q like Python _timestamp()", got, want)
	}
}

func TestAlpacaTimeoutErrorPreservesTimeoutSemantics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewAlpaca("key", "secret", server.URL, &http.Client{Timeout: 25 * time.Millisecond})
	_, err := client.Quote(context.Background(), "TEST")
	if err == nil || err.Error() != "Alpaca overnight request timed out" {
		t.Fatalf("Quote() error = %v, want a sanitized timeout-specific error", err)
	}
}

func TestAlpacaQuoteMidpointDoesNotOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"TEST":{"latestQuote":{"bp":1.6e308,"ap":1.7e308}}}`)
	}))
	defer server.Close()

	quote, err := NewAlpaca("key", "secret", server.URL, server.Client()).Quote(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if quote.Price == nil || math.IsNaN(*quote.Price) || math.IsInf(*quote.Price, 0) {
		t.Fatalf("Quote() price = %v, want a finite midpoint", quote.Price)
	}
	if got := *quote.Price / 1e308; math.Abs(got-1.65) > 1e-14 {
		t.Fatalf("Quote() price = %v, want midpoint 1.65e308", *quote.Price)
	}
}
