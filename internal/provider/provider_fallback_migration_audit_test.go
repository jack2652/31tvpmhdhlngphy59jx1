package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFallbackProviderRequestUserAgentsMatchPython(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/options/BRK.B":
			if got := r.Header.Get("User-Agent"); got != "option-scope/1.0" {
				t.Errorf("Cboe User-Agent = %q, want Python value option-scope/1.0", got)
			}
			_, _ = fmt.Fprint(w, `{"data":{"options":[]}}`)
		case alpacaSnapshotsPath:
			if got := r.Header.Get("User-Agent"); got != "option-scope/1.0" {
				t.Errorf("Alpaca User-Agent = %q, want Python value option-scope/1.0", got)
			}
			if got := r.URL.Query().Get("symbols"); got != "BRK.B" {
				t.Errorf("Alpaca symbols query = %q, want BRK.B", got)
			}
			if got := r.URL.Query().Get("feed"); got != "overnight" {
				t.Errorf("Alpaca feed query = %q, want overnight", got)
			}
			_, _ = fmt.Fprint(w, `{"BRK.B":{"latestTrade":{"p":400}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cboe := NewCboeOptionsClient(server.Client(), server.URL+"/options/{symbol}")
	if _, err := cboe.Quote(context.Background(), "BRK.B"); err != nil {
		t.Fatalf("Cboe Quote() error = %v", err)
	}

	alpaca := NewAlpaca("key", "secret", server.URL, server.Client())
	if _, err := alpaca.Quote(context.Background(), "BRK.B"); err != nil {
		t.Fatalf("Alpaca Quote() error = %v", err)
	}
}
