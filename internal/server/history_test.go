package server

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
)

type historyTestProvider struct {
	testMarketProvider
	historyCalls atomic.Int64
	history      provider.HistoryResult
	historyErr   error
}

type historyBarrierProvider struct {
	testMarketProvider
	historyCalls    atomic.Int64
	earningsCalls   atomic.Int64
	activeHistory   atomic.Int64
	peakHistory     atomic.Int64
	historyEntered  chan string
	earningsEntered chan string
	releaseHistory  chan struct{}
	releaseEarnings chan struct{}
}

type betaBarrierProvider struct {
	testMarketProvider
	historyEntered   chan string
	benchmarkEntered chan string
	releaseHistory   chan struct{}
	releaseBenchmark chan struct{}
}

func (p *betaBarrierProvider) History(ctx context.Context, symbol, _ string) (provider.HistoryResult, error) {
	select {
	case p.historyEntered <- symbol:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	select {
	case <-p.releaseHistory:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	return betaTestHistory(symbol), nil
}

func (p *betaBarrierProvider) BenchmarkHistory(ctx context.Context, symbol, _ string) (provider.HistoryResult, error) {
	select {
	case p.benchmarkEntered <- symbol:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	select {
	case <-p.releaseBenchmark:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	return betaTestHistory(symbol), nil
}

func betaTestHistory(symbol string) provider.HistoryResult {
	fetchedAt := time.Now().UTC()
	bars := make([]map[string]any, 32)
	firstDay := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for index := range bars {
		day := firstDay.AddDate(0, 0, index).Format("2006-01-02")
		close := 100.0 + float64(index*2)
		if symbol == "^GSPC" {
			close = 100.0 + float64(index)
		}
		bars[index] = map[string]any{"date": day, "open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 100.0}
	}
	return provider.HistoryResult{Symbol: symbol, FetchedAt: fetchedAt, Bars: bars}
}

func (p *historyBarrierProvider) History(ctx context.Context, symbol, _ string) (provider.HistoryResult, error) {
	p.historyCalls.Add(1)
	active := p.activeHistory.Add(1)
	defer p.activeHistory.Add(-1)
	for peak := p.peakHistory.Load(); active > peak; peak = p.peakHistory.Load() {
		if p.peakHistory.CompareAndSwap(peak, active) {
			break
		}
	}
	select {
	case p.historyEntered <- symbol:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	select {
	case <-p.releaseHistory:
	case <-ctx.Done():
		return provider.HistoryResult{}, ctx.Err()
	}
	fetchedAt := time.Now().UTC()
	return provider.HistoryResult{
		Symbol: symbol, FetchedAt: fetchedAt,
		Bars: []map[string]any{{"date": fetchedAt.Format("2006-01-02"), "open": 10.0, "high": 12.0, "low": 9.0, "close": 11.0, "volume": 100.0}},
	}, nil
}

func (p *historyBarrierProvider) EarningsDates(ctx context.Context, symbol string) ([]string, error) {
	p.earningsCalls.Add(1)
	select {
	case p.earningsEntered <- symbol:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.releaseEarnings:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []string{time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")}, nil
}

func waitForHistoryLockRefs(t *testing.T, service *Service, key string, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		service.mu.RLock()
		entry := service.historyLocks[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		service.mu.RUnlock()
		if refs >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("history lock %s refs=%d, want at least %d", key, refs, want)
		case <-ticker.C:
		}
	}
}

func waitForProviderEntry(t *testing.T, entered <-chan string, want string) {
	t.Helper()
	select {
	case got := <-entered:
		if got != want {
			t.Fatalf("provider entered for %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for provider entry %q", want)
	}
}

func (p *historyTestProvider) History(context.Context, string, string) (provider.HistoryResult, error) {
	p.historyCalls.Add(1)
	return p.history, p.historyErr
}

func TestLoadHistoryRefreshesOnceAndUsesPersistedFreshBars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-coordinator.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fetched := time.Now().UTC()
	providerStub := &historyTestProvider{history: provider.HistoryResult{Symbol: "QQQ", FetchedAt: fetched, Bars: []map[string]any{{"date": fetched.Format("2006-01-02"), "open": 10.0, "high": 12.0, "low": 9.0, "close": 11.0, "volume": 100.0}}}}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, providerStub)
	defer service.Close()
	service.gammaHeavyGate <- struct{}{}
	first := service.loadHistory(context.Background(), "QQQ", "2y")
	<-service.gammaHeavyGate
	second := service.loadHistory(context.Background(), "QQQ", "2y")
	if first["source"] != "upstream" || second["source"] != "sqlite" || providerStub.historyCalls.Load() != 1 {
		t.Fatalf("first=%#v second=%#v upstream calls=%d", first, second, providerStub.historyCalls.Load())
	}
}

func TestLoadHistoryDefersWhileLowMemoryHeavyGateIsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-heavy-gate.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().UTC().Add(-2 * time.Hour)
	staleBars := []map[string]any{{"date": staleAt.Format("2006-01-02"), "open": 10.0, "high": 12.0, "low": 9.0, "close": 11.0, "volume": 100.0}}
	if err := repository.WriteHistory("QQQ", staleBars, staleAt); err != nil {
		t.Fatal(err)
	}
	providerStub := &historyTestProvider{history: provider.HistoryResult{Symbol: "QQQ", FetchedAt: time.Now().UTC(), Bars: staleBars}}
	service := NewWithProvider(config.Config{DatabasePath: path, LowMemory: true, HistoryMaxAge: time.Hour}, repository, providerStub)
	defer service.Close()
	service.gammaHeavyGate <- struct{}{}

	got := service.loadHistory(context.Background(), "QQQ", "2y")
	if got["source"] != "sqlite" || got["warning"] == nil {
		t.Fatalf("expected stale SQLite fallback with warning, got %#v", got)
	}
	if providerStub.historyCalls.Load() != 0 {
		t.Fatalf("history provider called while shared low-memory gate was busy: calls=%d", providerStub.historyCalls.Load())
	}
	gotBars, _ := got["bars"].([]map[string]any)
	if len(gotBars) != 1 {
		t.Fatalf("unexpected fallback bars: %#v", gotBars)
	}
	closeValue, closeOK := numericHistoryValue(gotBars[0]["close"])
	if !closeOK || closeValue != 11.0 {
		t.Fatalf("unexpected fallback bars: %#v", gotBars)
	}
}

func TestLoadHistoryCoalescesConcurrentRequestsForSameSymbol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-singleflight.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &historyBarrierProvider{
		historyEntered: make(chan string, 2), releaseHistory: make(chan struct{}, 2),
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		_ = service.Close()
	}()

	results := make(chan map[string]any, 2)
	workers.Add(2)
	go func() { defer workers.Done(); results <- service.loadHistory(ctx, "QQQ", "2y") }()
	waitForProviderEntry(t, providerStub.historyEntered, "QQQ")
	go func() { defer workers.Done(); results <- service.loadHistory(ctx, "QQQ", "2y") }()
	waitForHistoryLockRefs(t, service, "bars:QQQ", 2)
	if got := providerStub.historyCalls.Load(); got != 1 {
		t.Fatalf("duplicate history request reached provider before first refresh completed: calls=%d", got)
	}
	providerStub.releaseHistory <- struct{}{}

	first, second := <-results, <-results
	if providerStub.historyCalls.Load() != 1 {
		t.Fatalf("same-symbol history requests should share one refresh, calls=%d", providerStub.historyCalls.Load())
	}
	upstream, sqlite := 0, 0
	for _, result := range []map[string]any{first, second} {
		switch result["source"] {
		case "upstream":
			upstream++
		case "sqlite":
			sqlite++
		default:
			t.Fatalf("unexpected history result: %#v", result)
		}
	}
	if upstream != 1 || sqlite != 1 {
		t.Fatalf("expected one upstream and one cached result, upstream=%d sqlite=%d; results=%#v %#v", upstream, sqlite, first, second)
	}
}

func TestLoadEarningsCoalescesConcurrentRequestsForSameSymbol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earnings-singleflight.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &historyBarrierProvider{
		earningsEntered: make(chan string, 2), releaseEarnings: make(chan struct{}, 2),
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		_ = service.Close()
	}()

	results := make(chan map[string]any, 2)
	workers.Add(2)
	go func() { defer workers.Done(); results <- service.loadEarnings(ctx, "QQQ") }()
	waitForProviderEntry(t, providerStub.earningsEntered, "QQQ")
	go func() { defer workers.Done(); results <- service.loadEarnings(ctx, "QQQ") }()
	waitForHistoryLockRefs(t, service, "earnings:QQQ", 2)
	if got := providerStub.earningsCalls.Load(); got != 1 {
		t.Fatalf("duplicate earnings request reached provider before first refresh completed: calls=%d", got)
	}
	providerStub.releaseEarnings <- struct{}{}

	first, second := <-results, <-results
	if providerStub.earningsCalls.Load() != 1 {
		t.Fatalf("same-symbol earnings requests should share one refresh, calls=%d", providerStub.earningsCalls.Load())
	}
	upstream, sqlite := 0, 0
	for _, result := range []map[string]any{first, second} {
		switch result["source"] {
		case "upstream":
			upstream++
		case "sqlite":
			sqlite++
		default:
			t.Fatalf("unexpected earnings result: %#v", result)
		}
	}
	if upstream != 1 || sqlite != 1 {
		t.Fatalf("expected one upstream and one cached result, upstream=%d sqlite=%d; results=%#v %#v", upstream, sqlite, first, second)
	}
}

func TestDecorateQuoteLoadsHistoryAndEarningsInParallel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decorate-quote-parallel.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &historyBarrierProvider{
		historyEntered: make(chan string, 1), earningsEntered: make(chan string, 1),
		releaseHistory: make(chan struct{}, 1), releaseEarnings: make(chan struct{}, 1),
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamConcurrency: 2}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var worker sync.WaitGroup
	defer func() {
		cancel()
		worker.Wait()
		_ = service.Close()
	}()

	price := 12.0
	result := make(chan model.Quote, 1)
	worker.Add(1)
	go func() {
		defer worker.Done()
		result <- service.decorateQuote(ctx, model.Quote{Symbol: "QQQ", Price: &price})
	}()
	waitForProviderEntry(t, providerStub.historyEntered, "QQQ")
	waitForProviderEntry(t, providerStub.earningsEntered, "QQQ")
	providerStub.releaseHistory <- struct{}{}
	providerStub.releaseEarnings <- struct{}{}

	var got model.Quote
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatalf("decorateQuote did not complete: %v", ctx.Err())
	}
	if got.PreviousClose == nil || *got.PreviousClose != 11 {
		t.Fatalf("history decoration mismatch: previous_close=%v", got.PreviousClose)
	}
	if got.ChangePercent == nil || math.Abs(*got.ChangePercent-(100.0/11.0)) > 0.0001 {
		t.Fatalf("history change decoration mismatch: change_percent=%v", got.ChangePercent)
	}
	earnings, ok := got.Earnings.(map[string]any)
	if !ok || earnings["source"] != "upstream" || earnings["warning"] != nil {
		t.Fatalf("earnings decoration mismatch: %#v", got.Earnings)
	}
	if earnings["date"] != time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02") {
		t.Fatalf("unexpected earnings date: %#v", earnings["date"])
	}
}

func TestDecorateQuoteKeepsHistoryAndEarningsSerialWhenLimited(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{name: "low memory", cfg: config.Config{LowMemory: true, UpstreamConcurrency: 2}},
		{name: "single concurrency", cfg: config.Config{UpstreamConcurrency: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "decorate-quote-serial.db")
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			providerStub := &historyBarrierProvider{
				historyEntered: make(chan string, 1), earningsEntered: make(chan string, 1),
				releaseHistory: make(chan struct{}, 1), releaseEarnings: make(chan struct{}, 1),
			}
			test.cfg.DatabasePath = path
			service := NewWithProvider(test.cfg, repository, providerStub)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var worker sync.WaitGroup
			defer func() {
				cancel()
				worker.Wait()
				_ = service.Close()
			}()

			result := make(chan model.Quote, 1)
			worker.Add(1)
			go func() {
				defer worker.Done()
				result <- service.decorateQuote(ctx, model.Quote{Symbol: "QQQ"})
			}()
			waitForProviderEntry(t, providerStub.historyEntered, "QQQ")
			select {
			case symbol := <-providerStub.earningsEntered:
				t.Fatalf("earnings provider started before history completed: %s", symbol)
			case <-time.After(50 * time.Millisecond):
			}
			providerStub.releaseHistory <- struct{}{}
			waitForProviderEntry(t, providerStub.earningsEntered, "QQQ")
			providerStub.releaseEarnings <- struct{}{}
			select {
			case <-result:
			case <-ctx.Done():
				t.Fatalf("decorateQuote did not complete: %v", ctx.Err())
			}
		})
	}
}

func TestLoadBetaFetchesMissingHistoriesInParallel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beta-parallel.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &betaBarrierProvider{
		historyEntered: make(chan string, 1), benchmarkEntered: make(chan string, 1),
		releaseHistory: make(chan struct{}, 1), releaseBenchmark: make(chan struct{}, 1),
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamConcurrency: 2}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var worker sync.WaitGroup
	defer func() {
		cancel()
		worker.Wait()
		_ = service.Close()
	}()

	result := make(chan map[string]any, 1)
	worker.Add(1)
	go func() {
		defer worker.Done()
		result <- service.loadBeta(ctx, "QQQ", nil)
	}()
	waitForProviderEntry(t, providerStub.historyEntered, "QQQ")
	waitForProviderEntry(t, providerStub.benchmarkEntered, "^GSPC")
	providerStub.releaseHistory <- struct{}{}
	providerStub.releaseBenchmark <- struct{}{}
	select {
	case got := <-result:
		if got["source"] != "upstream" || got["warning"] != nil {
			t.Fatalf("unexpected beta result: %#v", got)
		}
	case <-ctx.Done():
		t.Fatalf("loadBeta did not complete: %v", ctx.Err())
	}
}

func TestLoadBetaKeepsHistoryAndBenchmarkSerialInLowMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beta-low-memory.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &betaBarrierProvider{
		historyEntered: make(chan string, 1), benchmarkEntered: make(chan string, 1),
		releaseHistory: make(chan struct{}, 1), releaseBenchmark: make(chan struct{}, 1),
	}
	service := NewWithProvider(config.Config{DatabasePath: path, LowMemory: true, UpstreamConcurrency: 2}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var worker sync.WaitGroup
	defer func() {
		cancel()
		worker.Wait()
		_ = service.Close()
	}()

	result := make(chan map[string]any, 1)
	worker.Add(1)
	go func() {
		defer worker.Done()
		result <- service.loadBeta(ctx, "QQQ", nil)
	}()
	waitForProviderEntry(t, providerStub.historyEntered, "QQQ")
	select {
	case symbol := <-providerStub.benchmarkEntered:
		t.Fatalf("benchmark provider started before stock history completed: %s", symbol)
	case <-time.After(50 * time.Millisecond):
	}
	providerStub.releaseHistory <- struct{}{}
	waitForProviderEntry(t, providerStub.benchmarkEntered, "^GSPC")
	providerStub.releaseBenchmark <- struct{}{}
	select {
	case got := <-result:
		if got["source"] != "upstream" || got["warning"] != nil {
			t.Fatalf("unexpected beta result: %#v", got)
		}
	case <-ctx.Done():
		t.Fatalf("loadBeta did not complete: %v", ctx.Err())
	}
}

func TestLoadHistoryAllowsDifferentSymbolsToFetchInParallel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-parallel-symbols.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &historyBarrierProvider{
		historyEntered: make(chan string, 2), releaseHistory: make(chan struct{}, 2),
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, providerStub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		_ = service.Close()
	}()

	results := make(chan map[string]any, 2)
	workers.Add(2)
	go func() { defer workers.Done(); results <- service.loadHistory(ctx, "AAPL", "2y") }()
	go func() { defer workers.Done(); results <- service.loadHistory(ctx, "MSFT", "2y") }()
	seen := map[string]bool{}
	for range 2 {
		select {
		case symbol := <-providerStub.historyEntered:
			seen[symbol] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("different-symbol provider calls did not overlap; entered=%v", seen)
		}
	}
	if len(seen) != 2 || providerStub.peakHistory.Load() < 2 {
		t.Fatalf("provider calls were serialized across symbols: entered=%v peak=%d", seen, providerStub.peakHistory.Load())
	}
	providerStub.releaseHistory <- struct{}{}
	providerStub.releaseHistory <- struct{}{}
	for range 2 {
		if result := <-results; result["source"] != "upstream" {
			t.Fatalf("different-symbol fetch failed: %#v", result)
		}
	}
}

func TestLoadEarningsWithoutProviderUsesStaleCacheWithoutWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-earnings-cache.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().UTC().Add(-7 * time.Hour)
	if err := repository.WritePayload("earnings_snapshots", "QQQ", map[string]any{"dates": []string{"2026-10-10"}}, staleAt); err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, &historyTestProvider{})
	defer service.Close()

	got := service.loadEarnings(context.Background(), "QQQ")
	if got["source"] != "sqlite" || got["warning"] != nil {
		t.Fatalf("stale cached earnings without provider should match Python fallback, got %#v", got)
	}
	dates, _ := got["dates"].([]string)
	if len(dates) != 1 || dates[0] != "2026-10-10" {
		t.Fatalf("unexpected cached earnings dates: %#v", got["dates"])
	}
}

func TestHistoryLockCancellationDoesNotLeakLockEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-lock.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, &historyTestProvider{})
	defer service.Close()
	unlock, err := service.historyLock(context.Background(), "bars:QQQ", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.historyLock(ctx, "bars:QQQ", 0); err == nil {
		t.Fatal("canceled lock acquisition succeeded")
	}
	unlock()
	if len(service.historyLocks) != 0 {
		t.Fatalf("history lock entry leaked: %#v", service.historyLocks)
	}
}

func TestHistoryLockHonorsBoundedWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-lock-timeout.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, &historyTestProvider{})
	defer service.Close()
	unlock, err := service.historyLock(context.Background(), "bars:QQQ", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	started := time.Now()
	if _, err := service.historyLock(context.Background(), "bars:QQQ", 25*time.Millisecond); err == nil {
		t.Fatal("bounded history lock unexpectedly acquired")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded wait took %v, want less than 1s", elapsed)
	}
}

func TestAcquireHistoryLeaseContextCancellationWhileWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-lease-context-cancel.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path}, repository, &historyTestProvider{})
	defer service.Close()

	releaseWriter := holdSQLiteWriter(t, path)
	writerReleased := false
	defer func() {
		if !writerReleased {
			releaseWriter()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		release func()
		err     error
	}
	acquired := make(chan result, 1)
	go func() {
		release, acquireErr := service.acquireHistoryLease(ctx, "history:cancel-test")
		acquired <- result{release: release, err: acquireErr}
	}()
	select {
	case got := <-acquired:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("lease acquisition returned before the SQLite writer lock was released: %#v", got)
	case <-time.After(100 * time.Millisecond):
	}

	canceledAt := time.Now()
	cancel()
	var got result
	select {
	case got = <-acquired:
	case <-time.After(time.Second):
		releaseWriter()
		writerReleased = true
		select {
		case got = <-acquired:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("lease acquisition ignored cancellation for %v and returned after writer release: err=%v", time.Since(canceledAt), got.err)
		case <-time.After(time.Second):
			t.Fatal("lease acquisition did not return after cancellation and writer release")
		}
	}
	if got.err != context.Canceled || got.release != nil {
		t.Fatalf("lease acquisition=(release=%v, err=%v), want context cancellation without acquiring", got.release != nil, got.err)
	}
	if elapsed := time.Since(canceledAt); elapsed > time.Second {
		t.Fatalf("lease cancellation took %v, want less than 1s", elapsed)
	}
	releaseWriter()
	writerReleased = true

	db, err := sql.Open("sqlite", path+"?_busy_timeout=1000&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM service_leases WHERE name=?", "history:cancel-test").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("canceled lease was persisted: rows=%d", count)
	}
}

func TestHistoryFreshTreatsFutureTimestampAsCurrent(t *testing.T) {
	if !historyFresh(time.Now().UTC().Add(time.Minute), time.Second) {
		t.Fatal("future timestamp should be treated as age zero, matching Python")
	}
}

func TestHistoryFreshParsesDateOnlyTimestamp(t *testing.T) {
	if !historyFresh(time.Now().UTC().Format(time.DateOnly), 24*time.Hour) {
		t.Fatal("date-only timestamp should parse as midnight UTC, matching Python")
	}
}

func TestHistoryQuoteAlignmentUsesCalendarDayWindow(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	latestDay := time.Date(2025, time.January, 1, 16, 0, 0, 0, location)
	sevenDaysLaterAfterClose := time.Date(2025, time.January, 8, 17, 0, 0, 0, location)
	if !historyWithinQuoteAlignmentWindow(latestDay, sevenDaysLaterAfterClose) {
		t.Fatal("latest bar exactly 7 calendar days old should remain eligible after 168 hours, matching Python")
	}
	eightCalendarDaysLater := time.Date(2025, time.January, 9, 0, 0, 0, 0, location)
	if historyWithinQuoteAlignmentWindow(latestDay, eightCalendarDaysLater) {
		t.Fatal("latest bar more than 7 calendar days old must not align the quote")
	}
}

func TestUpcomingNYSETradingSessions(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		day  time.Time
		want []string
	}{
		{time.Date(2026, time.November, 25, 12, 0, 0, 0, location), []string{"2026-11-25", "2026-11-27", "2026-11-30", "2026-12-01", "2026-12-02"}},
		{time.Date(2026, time.November, 28, 12, 0, 0, 0, location), []string{"2026-11-30", "2026-12-01", "2026-12-02", "2026-12-03", "2026-12-04"}},
	}
	for _, tc := range cases {
		got := upcomingNYSETradingSessions(tc.day, 5)
		if len(got) != len(tc.want) {
			t.Fatalf("sessions=%v want=%v", got, tc.want)
		}
		for index, value := range got {
			if value.Format("2006-01-02") != tc.want[index] {
				t.Fatalf("sessions=%v want=%v", got, tc.want)
			}
		}
	}
}

func TestCalculateBetaRoundsToPythonPrecision(t *testing.T) {
	stock, benchmark := make([]map[string]any, 32), make([]map[string]any, 32)
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range stock {
		stockClose, benchmarkClose := 100.0, 100.0
		if i > 0 {
			stockClose = 102.67515
			benchmarkClose = 101.0
		}
		date := day.AddDate(0, 0, i).Format("2006-01-02")
		stock[i] = map[string]any{"date": date, "close": stockClose}
		benchmark[i] = map[string]any{"date": date, "close": benchmarkClose}
	}
	result := calculateBeta(stock, benchmark)
	if result == nil {
		t.Fatal("expected beta")
	}
	value, ok := result["value"].(float64)
	if !ok || value != 2.6752 {
		t.Fatalf("beta value=%v, want Python round result 2.6752", result["value"])
	}
}

func TestCalculatePriceExtremesRoundsLikePython(t *testing.T) {
	extremes := calculatePriceExtremes([]map[string]any{{
		"date": "2025-01-01", "high": 9.87655, "low": 1.23445,
	}})
	week52 := extremes["week52"].(map[string]any)
	high := week52["high"].(map[string]any)["price"]
	low := week52["low"].(map[string]any)["price"]
	if high != 9.8765 || low != 1.2345 {
		t.Fatalf("extremes high=%v low=%v, want Python rounded values 9.8765 and 1.2345", high, low)
	}
}

func TestUsableFreshExtremesRejectsSparseMonthlyYahooCache(t *testing.T) {
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	cache := map[string]any{
		"extremes":   map[string]any{"reference_date": "2026-10-09", "window_bars": 13.0},
		"fetched_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	bars := make([]map[string]any, 0, 252)
	for i := 0; i < 252; i++ {
		bars = append(bars, map[string]any{"date": day.AddDate(0, 0, -i).Format(time.DateOnly)})
	}
	if usableFreshExtremes(cache, map[string]any{"bars": bars}, 24*time.Hour) {
		t.Fatal("sparse monthly extrema cache was treated as fresh despite daily history")
	}
	cache["extremes"].(map[string]any)["window_bars"] = 252.0
	if !usableFreshExtremes(cache, map[string]any{"bars": bars}, 24*time.Hour) {
		t.Fatal("daily extrema cache should be treated as fresh")
	}
}

func TestLoadExtremesRefreshesFreshSparseCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-extremes-sparse.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	providerStub := &historyTestProvider{history: provider.HistoryResult{
		Symbol: "LITE", FetchedAt: time.Now().UTC(),
		Bars: []map[string]any{{"date": "2026-10-09", "high": 1143.0, "low": 1077.375, "close": 1103.35}},
	}}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamConcurrency: 2}, repository, providerStub)
	defer service.Close()
	if err := repository.WritePayload("price_extremes", "LITE", map[string]any{
		"reference_date": "2026-10-09", "window_bars": 13.0,
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	bars := make([]map[string]any, 0, 252)
	for i := 0; i < 252; i++ {
		bars = append(bars, map[string]any{"date": time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -i).Format(time.DateOnly)})
	}
	got := service.loadExtremes(context.Background(), "LITE", map[string]any{"bars": bars})
	if got["source"] != "upstream" || providerStub.historyCalls.Load() != 1 {
		t.Fatalf("sparse cached extrema not refreshed: result=%#v calls=%d", got, providerStub.historyCalls.Load())
	}
}

var _ MarketProvider = (*historyTestProvider)(nil)
