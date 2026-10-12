package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
)

type regimeInputProvider struct {
	*testMarketProvider
	tnxBars []map[string]any
	tnxErr  error
}

func (p *regimeInputProvider) BenchmarkHistory(_ context.Context, symbol, _ string) (provider.HistoryResult, error) {
	if symbol != "^TNX" {
		return provider.HistoryResult{}, nil
	}
	return provider.HistoryResult{Bars: p.tnxBars}, p.tnxErr
}

type regimeConcurrencyProvider struct {
	*testMarketProvider
	active  atomic.Int32
	maximum atomic.Int32
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (p *regimeConcurrencyProvider) BenchmarkHistory(ctx context.Context, _ string, _ string) (provider.HistoryResult, error) {
	active := p.active.Add(1)
	for maximum := p.maximum.Load(); active > maximum; maximum = p.maximum.Load() {
		if p.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	p.calls.Add(1)
	p.started <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		p.active.Add(-1)
		return provider.HistoryResult{}, ctx.Err()
	}
	p.active.Add(-1)
	return provider.HistoryResult{}, nil
}

func TestValuationRegimeBenchmarkConcurrencyFollowsRuntimeBudget(t *testing.T) {
	tests := []struct {
		name        string
		lowMemory   bool
		concurrency int
		wantMaximum int32
	}{
		{name: "parallel within budget", concurrency: 3, wantMaximum: 3},
		{name: "bounded by configured concurrency", concurrency: 2, wantMaximum: 2},
		{name: "low memory is serial", lowMemory: true, concurrency: 3, wantMaximum: 1},
		{name: "single concurrency is serial", concurrency: 1, wantMaximum: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "valuation-regime.db")
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			fake := &regimeConcurrencyProvider{
				testMarketProvider: &testMarketProvider{},
				started:            make(chan struct{}, 3),
				release:            make(chan struct{}),
			}
			service := NewWithProvider(config.Config{
				DatabasePath: path, UpstreamTimeout: time.Second,
				LowMemory: test.lowMemory, UpstreamConcurrency: test.concurrency,
			}, repository, fake)
			defer service.Close()

			done := make(chan struct{})
			go func() {
				service.detectValuationRegime(context.Background(), "QQQ", provider.YahooFundamentals{})
				close(done)
			}()

			for range test.wantMaximum {
				select {
				case <-fake.started:
				case <-time.After(time.Second):
					close(fake.release)
					<-done
					t.Fatal("benchmark requests did not reach the expected concurrency")
				}
			}
			close(fake.release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("valuation regime did not complete after benchmark responses were released")
			}
			if got := fake.calls.Load(); got != 3 {
				t.Fatalf("benchmark calls=%d, want 3", got)
			}
			if got := fake.maximum.Load(); got != test.wantMaximum {
				t.Fatalf("maximum concurrent benchmark calls=%d, want %d", got, test.wantMaximum)
			}
		})
	}
}

func TestValuationRegimeUsesFundamentalRiskFreeRateWhenTNXUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-regime.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &regimeInputProvider{
		testMarketProvider: &testMarketProvider{},
		tnxErr:             errors.New("benchmark unavailable"),
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamConcurrency: 3}, repository, fake)
	defer service.Close()

	regime, signals := service.detectValuationRegime(context.Background(), "AAPL", provider.YahooFundamentals{
		RiskFreeRate: func() *float64 { value := 5.2; return &value }(),
	})
	if got := signals["tnx_yield"]; got != 0.052 {
		t.Fatalf("tnx_yield=%v, want normalized fundamentals fallback 0.052 (regime %s)", got, regime)
	}
}

func TestValuationRegimePrefersAvailableTNXAndDirectCapexGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-regime.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &regimeInputProvider{
		testMarketProvider: &testMarketProvider{},
		tnxBars:            []map[string]any{{"close": 4.1}},
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamConcurrency: 3}, repository, fake)
	defer service.Close()

	directCapexGrowth := -0.03
	fundamentalRate := 0.052
	_, signals := service.detectValuationRegime(context.Background(), "MSFT", provider.YahooFundamentals{
		RiskFreeRate:             &fundamentalRate,
		CapexGrowth:              &directCapexGrowth,
		AnnualCapitalExpenditure: []float64{-10, -5},
	})
	if got := signals["tnx_yield"]; got != 0.041 {
		t.Fatalf("tnx_yield=%v, want benchmark value 0.041", got)
	}
	if got := signals["capex_growth"]; got != -0.03 {
		t.Fatalf("capex_growth=%v, want direct provider value -0.03", got)
	}
}

var _ MarketProvider = (*regimeConcurrencyProvider)(nil)
var _ benchmarkHistoryProvider = (*regimeInputProvider)(nil)
