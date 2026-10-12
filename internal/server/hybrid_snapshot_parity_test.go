package server

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/store"
)

type snapshotQuoteParityProvider struct {
	*testMarketProvider
	snapshotQuote model.Quote
	snapshotCalls atomic.Int64
}

func (p *snapshotQuoteParityProvider) SnapshotQuote(_ context.Context, symbol string) (model.Quote, error) {
	p.snapshotCalls.Add(1)
	quote := p.snapshotQuote
	quote.Symbol = symbol
	return quote, nil
}

func TestSnapshotRefreshUsesOptionalSnapshotQuoteOnlyForRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hybrid-snapshot.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPrice, snapshotPrice := 99.0, 100.0
	provider := &snapshotQuoteParityProvider{
		testMarketProvider: &testMarketProvider{
			quote: model.Quote{Price: &ordinaryPrice, Provider: "ordinary"},
		},
		snapshotQuote: model.Quote{Price: &snapshotPrice, Provider: "snapshot"},
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, provider)
	defer service.Close()

	ordinary, err := service.fetchQuote(context.Background(), "QQQ")
	if err != nil || ordinary.Provider != "ordinary" || provider.snapshotCalls.Load() != 0 {
		t.Fatalf("fetchQuote() = %#v, %v; snapshot calls=%d", ordinary, err, provider.snapshotCalls.Load())
	}

	response, err := service.refreshSnapshotWithOverrides(context.Background(), "QQQ", "", 0, []string{}, nil)
	if err != nil || response.Quote == nil || response.Quote.Provider != "snapshot" || provider.snapshotCalls.Load() != 1 {
		t.Fatalf("snapshot refresh = %#v, %v; snapshot calls=%d", response, err, provider.snapshotCalls.Load())
	}
	if provider.quoteCalls.Load() != 1 {
		t.Fatalf("ordinary provider quote calls = %d, want 1", provider.quoteCalls.Load())
	}
}
