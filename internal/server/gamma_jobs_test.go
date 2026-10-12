package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/store"
)

type gammaReclaimProvider struct {
	calls        atomic.Int64
	quoteStarted chan int
	quoteRelease chan struct{}
	expiration   string
}

func (p *gammaReclaimProvider) Quote(_ context.Context, symbol string) (model.Quote, error) {
	call := int(p.calls.Add(1))
	p.quoteStarted <- call
	<-p.quoteRelease
	price := 100.0
	return model.Quote{Symbol: symbol, Price: &price, Provider: "test", Currency: "USD", Sessions: map[string]any{}}, nil
}

func (p *gammaReclaimProvider) Expirations(context.Context, string) ([]string, error) {
	return []string{p.expiration}, nil
}

func (p *gammaReclaimProvider) Chain(_ context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	fetched := time.Now().UTC()
	return []model.OptionRow{{ContractSymbol: symbol + "-" + expiration, Expiration: expiration}}, &fetched, nil
}

type gammaJobProvider struct {
	quoteCalls      atomic.Int64
	expirationCalls atomic.Int64
	chainCalls      atomic.Int64
	quoteStarted    chan struct{}
	quoteRelease    chan struct{}
	chainErrors     int32
	panicExpiration bool
	panicChain      bool
	expirations     []string
}

type gammaParallelProvider struct {
	expirations    []string
	started        chan string
	release        chan struct{}
	holdExpiration string
	chainCalls     atomic.Int64
	quoteCalls     atomic.Int64
}

type gammaCancelProvider struct {
	expiration string
	started    chan int
	canceled   chan int
	calls      atomic.Int64
}

func (p *gammaCancelProvider) Quote(_ context.Context, symbol string) (model.Quote, error) {
	price := 100.0
	return model.Quote{Symbol: symbol, Price: &price, Provider: "test", Currency: "USD", Sessions: map[string]any{}}, nil
}

func (p *gammaCancelProvider) Expirations(context.Context, string) ([]string, error) {
	return []string{p.expiration}, nil
}

func (p *gammaCancelProvider) Chain(ctx context.Context, _, _ string) ([]model.OptionRow, *time.Time, error) {
	call := int(p.calls.Add(1))
	p.started <- call
	if call == 1 {
		<-ctx.Done()
		p.canceled <- call
		return nil, nil, ctx.Err()
	}
	fetched := time.Now().UTC()
	return []model.OptionRow{{ContractSymbol: "QQQ-" + p.expiration, Expiration: p.expiration}}, &fetched, nil
}

func (p *gammaParallelProvider) Quote(_ context.Context, symbol string) (model.Quote, error) {
	p.quoteCalls.Add(1)
	price := 100.0
	return model.Quote{Symbol: symbol, Price: &price, Provider: "test", Currency: "USD", Sessions: map[string]any{}}, nil
}

func (p *gammaParallelProvider) Expirations(context.Context, string) ([]string, error) {
	return append([]string(nil), p.expirations...), nil
}

func (p *gammaParallelProvider) Chain(ctx context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	p.chainCalls.Add(1)
	select {
	case p.started <- expiration:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if p.release != nil && (p.holdExpiration == "" || expiration == p.holdExpiration) {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	fetched := time.Now().UTC()
	openInterest := int64(10)
	return []model.OptionRow{{
		ContractSymbol: symbol + "-" + expiration, Expiration: expiration, OpenInterest: &openInterest,
	}}, &fetched, nil
}

func (p *gammaJobProvider) Quote(ctx context.Context, symbol string) (model.Quote, error) {
	p.quoteCalls.Add(1)
	select {
	case p.quoteStarted <- struct{}{}:
	default:
	}
	select {
	case <-p.quoteRelease:
	case <-ctx.Done():
		return model.Quote{}, ctx.Err()
	}
	price := 100.0
	return model.Quote{Symbol: symbol, Price: &price, Provider: "test", Currency: "USD", Sessions: map[string]any{}}, nil
}

func (p *gammaJobProvider) Expirations(context.Context, string) ([]string, error) {
	p.expirationCalls.Add(1)
	if p.panicExpiration {
		panic("expiration provider panic")
	}
	return append([]string(nil), p.expirations...), nil
}

func (p *gammaJobProvider) Chain(_ context.Context, symbol, expiration string) ([]model.OptionRow, *time.Time, error) {
	p.chainCalls.Add(1)
	if p.panicChain {
		panic("chain provider panic")
	}
	if atomic.LoadInt32(&p.chainErrors) > 0 && atomic.CompareAndSwapInt32(&p.chainErrors, 1, 0) {
		return nil, nil, fmt.Errorf("temporary chain error")
	}
	fetched := time.Now().UTC()
	return []model.OptionRow{{ContractSymbol: symbol + "-" + expiration, Expiration: expiration}}, &fetched, nil
}

func TestGammaRefreshClaimsOneBatchAndDoesNotDuplicateRunningJob(t *testing.T) {
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "gamma-jobs.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
		expirations: []string{first, second},
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	state, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("first claim = %#v, claimed=%v; want running owner", state, claimed)
	}
	if !hasExactKeys(state, "job_id", "symbol", "horizon_days", "status", "started_at", "claimed", "resume_result") {
		t.Fatalf("initial claim fields differ from Python: %#v", state)
	}
	if _, ok := state["resume_result"]; !ok {
		t.Fatalf("new claim omitted Python resume_result field: %#v", state)
	}
	select {
	case <-provider.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("gamma job did not reach provider")
	}
	duplicate, claimed := service.claimGammaJob("QQQ", 45)
	if claimed || duplicate["status"] != "running" {
		t.Fatalf("duplicate claim = %#v, claimed=%v; want existing running job", duplicate, claimed)
	}
	if !hasExactKeys(duplicate, "job_id", "symbol", "horizon_days", "status", "started_at", "claimed") {
		t.Fatalf("duplicate claim fields differ from Python: %#v", duplicate)
	}

	close(provider.quoteRelease)
	deadline := time.Now().Add(time.Second)
	for {
		status := service.gammaJobStatus("QQQ", 45)
		if status["status"] != "running" {
			result, _ := status["result"].(map[string]any)
			if status["status"] != "completed" || result["has_more"] != true || result["next_cursor"] != first {
				t.Fatalf("first batch status = %#v; want completed with next cursor", status)
			}
			if !hasExactKeys(status, "job_id", "symbol", "horizon_days", "status", "started_at", "finished_at", "result", "error_message") {
				t.Fatalf("persisted status fields differ from Python analysis_job: %#v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gamma job did not finish first batch")
		}
		time.Sleep(time.Millisecond)
	}
	if got := provider.chainCalls.Load(); got != 1 {
		t.Fatalf("chain calls after first batch=%d, want 1", got)
	}

	// A restarted process has no local cursor; the completed durable result must
	// supply it when the next batch is claimed.
	service.mu.Lock()
	delete(service.gammaJobs, service.gammaJobKey("QQQ", 45))
	service.mu.Unlock()
	state, claimed = service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("second batch claim = %#v, claimed=%v; want running owner", state, claimed)
	}
	deadline = time.Now().Add(time.Second)
	for {
		status := service.gammaJobStatus("QQQ", 45)
		if status["status"] != "running" {
			result, _ := status["result"].(map[string]any)
			if status["status"] != "completed" || result["has_more"] != false || result["next_cursor"] != nil {
				t.Fatalf("final batch status = %#v; want completed without cursor", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gamma job did not finish second batch")
		}
		time.Sleep(time.Millisecond)
	}
	if got := provider.chainCalls.Load(); got != 2 {
		t.Fatalf("chain calls after second batch=%d, want 2", got)
	}
	if got := provider.quoteCalls.Load(); got != 1 {
		t.Fatalf("quote calls after both batches=%d, want one cached window quote", got)
	}
	if got := provider.expirationCalls.Load(); got != 1 {
		t.Fatalf("expiration calls after both batches=%d, want one window catalog load", got)
	}
	cooldown, claimed := service.claimGammaJob("QQQ", 45)
	if claimed || cooldown["status"] != "completed" || !hasExactKeys(cooldown, "job_id", "symbol", "horizon_days", "status", "claimed", "finished_at") {
		t.Fatalf("completed cooldown claim differs from Python: state=%#v claimed=%v", cooldown, claimed)
	}
}

func TestGammaRefreshCleansCachedExpirationCatalog(t *testing.T) {
	valid := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	for _, test := range []struct {
		name                string
		catalog             []string
		wantExpirationFetch int64
	}{
		{name: "filters malformed values and deduplicates", catalog: []string{"not-a-date", valid, valid, "2026-02-30"}},
		{name: "refetches a catalog with no valid dates", catalog: []string{"not-a-date", "2026-02-30"}, wantExpirationFetch: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gamma-expiration-cleanup.db")
			cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 4}
			repository, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.SaveExpirations("QQQ", test.catalog); err != nil {
				t.Fatal(err)
			}
			provider := &gammaJobProvider{
				quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
				expirations: []string{valid},
			}
			close(provider.quoteRelease)
			service := NewWithProvider(cfg, repository, provider)
			defer service.Close()

			if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
				t.Fatal("Gamma job was not claimed")
			}
			status := waitForGammaStatus(t, service, "completed", 45)
			result, ok := status["result"].(map[string]any)
			if !ok {
				t.Fatalf("Gamma result=%#v", status["result"])
			}
			for _, field := range []string{"expirations", "batch_expirations"} {
				got, ok := result[field].([]any)
				if !ok || len(got) != 1 || fmt.Sprint(got[0]) != valid {
					t.Fatalf("%s=%#v, want only valid expiration %s", field, result[field], valid)
				}
			}
			if got := provider.expirationCalls.Load(); got != test.wantExpirationFetch {
				t.Fatalf("expiration provider calls=%d, want %d", got, test.wantExpirationFetch)
			}
			if got := provider.chainCalls.Load(); got != 1 {
				t.Fatalf("chain provider calls=%d, want one valid expiration refresh", got)
			}
		})
	}
}

func TestGammaCatalogPanicFailsJobAndReleasesHeavyGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-expiration-panic.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 4}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
		panicExpiration: true,
	}
	close(provider.quoteRelease)
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
		t.Fatal("Gamma job was not claimed")
	}
	status := waitForGammaStatus(t, service, "failed", 45)
	if status["error_message"] != "expiration provider panic" {
		t.Fatalf("Gamma status=%#v, want the catalog panic recorded", status)
	}
	if got := len(service.gammaHeavyGate); got != 0 {
		t.Fatalf("Gamma heavy gate occupancy=%d after catalog panic, want 0", got)
	}
}

func TestGammaExpirationPanicBecomesAProcessedWindowError(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-chain-panic.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 4}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
		panicChain: true, expirations: []string{expiration},
	}
	close(provider.quoteRelease)
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
		t.Fatal("Gamma job was not claimed")
	}
	status := waitForGammaStatus(t, service, "completed", 45)
	result, ok := status["result"].(map[string]any)
	if !ok {
		t.Fatalf("Gamma result=%#v", status["result"])
	}
	errors, _ := result["errors"].([]any)
	if len(errors) != 1 || errors[0] != expiration+": chain provider panic" {
		t.Fatalf("Gamma errors=%#v, want the failed expiration recorded", errors)
	}
	if result["has_more"] != false || gammaInt(result, "processed") != 1 {
		t.Fatalf("Gamma cursor result=%#v, want failed expiration processed like Python", result)
	}
}

func TestGammaRefreshFetchesTwoExpirationsInParallelAndKeepsOrder(t *testing.T) {
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-parallel.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 4}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaParallelProvider{
		expirations: []string{first, second}, started: make(chan string, 2), release: make(chan struct{}), holdExpiration: first,
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	claimed, owner := service.claimGammaJob("QQQ", 45)
	if !owner || claimed["status"] != "running" {
		t.Fatalf("claim=%#v owner=%v", claimed, owner)
	}
	started := map[string]bool{}
	for range 2 {
		select {
		case expiration := <-provider.started:
			started[expiration] = true
		case <-time.After(time.Second):
			t.Fatal("both expiration chains did not overlap")
		}
	}
	if !started[first] || !started[second] {
		t.Fatalf("started=%#v, want both expiration chains", started)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, found, loadErr := repository.LatestChain("QQQ", second)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second expiration did not persist while the first was blocked")
		}
		time.Sleep(time.Millisecond)
	}
	duplicate, claimedAgain := service.claimGammaJob("QQQ", 45)
	if claimedAgain || duplicate["status"] != "running" {
		t.Fatalf("duplicate claim=%#v claimed=%v, want existing running job", duplicate, claimedAgain)
	}
	close(provider.release)

	status := waitForGammaStatus(t, service, "completed", 45)
	result, ok := status["result"].(map[string]any)
	if !ok {
		t.Fatalf("Gamma result=%#v", status["result"])
	}
	batch, _ := result["batch_expirations"].([]any)
	if gammaInt(result, "batch_size") != 2 || len(batch) != 2 || batch[0] != first || batch[1] != second {
		t.Fatalf("batch=%#v batch_size=%v, want sorted two-expiration batch", batch, result["batch_size"])
	}
	results, _ := result["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results=%#v, want two expiration results", results)
	}
	for index, expected := range []string{first, second} {
		entry, _ := results[index].(map[string]any)
		if entry["expiration"] != expected {
			t.Fatalf("results[%d].expiration=%v, want %s", index, entry["expiration"], expected)
		}
	}
	if result["has_more"] != false || result["next_cursor"] != nil || gammaInt(result, "processed") != 2 {
		t.Fatalf("parallel cursor result=%#v, want completed contiguous prefix", result)
	}
	if provider.chainCalls.Load() != 2 {
		t.Fatalf("parallel chain calls=%d, want 2", provider.chainCalls.Load())
	}
	if provider.quoteCalls.Load() != 1 {
		t.Fatalf("parallel quote calls=%d, want one shared window quote", provider.quoteCalls.Load())
	}
}

func TestGammaLowMemoryKeepsExpirationRefreshSerial(t *testing.T) {
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-low-memory-serial.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 4, LowMemory: true}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaParallelProvider{
		expirations: []string{first, second}, started: make(chan string, 2), release: make(chan struct{}),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
		t.Fatal("Gamma job was not claimed")
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("first chain did not start")
	}
	select {
	case expiration := <-provider.started:
		close(provider.release)
		t.Fatalf("LowMemory started a second expiration concurrently: %s", expiration)
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	status := waitForGammaStatus(t, service, "completed", 45)
	result, _ := status["result"].(map[string]any)
	batch, _ := result["batch_expirations"].([]any)
	if gammaInt(result, "batch_size") != 1 || len(batch) != 1 || batch[0] != first {
		t.Fatalf("result=%#v, want one serial expiration", result)
	}
}

func TestGammaParallelTimeoutCancelsWorkersAndReleasesHeavySlots(t *testing.T) {
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-parallel-timeout.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, UpstreamConcurrency: 2}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaParallelProvider{
		expirations: []string{first, second}, started: make(chan string, 2), release: make(chan struct{}),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	if err := repository.SaveExpirations("QQQ", provider.expirations); err != nil {
		t.Fatal(err)
	}
	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, 30*time.Second, gammaRefreshTimeout)
	if err != nil || !owner {
		t.Fatalf("durable claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	jobID, startedAt := fmt.Sprint(claimed["job_id"]), fmt.Sprint(claimed["started_at"])
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	service.gammaJobs[key] = &gammaJob{jobID: jobID, durableStarted: startedAt, symbol: "QQQ", horizonDays: 45, status: "running", startedAt: time.Now()}
	service.mu.Unlock()
	done := make(chan struct{})
	go func() {
		service.runGammaJobWithTimeout(context.Background(), key, jobID, startedAt, time.Second, func(ctx context.Context) {
			service.runGammaBatch(ctx, key, jobID, startedAt, "QQQ", 45)
		})
		close(done)
	}()
	for range 2 {
		select {
		case <-provider.started:
		case <-time.After(time.Second):
			t.Fatal("both parallel chains did not start before timeout")
		}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout did not cancel and join Gamma workers")
	}
	if got := len(service.gammaHeavyGate); got != 0 {
		t.Fatalf("Gamma heavy gate occupancy=%d after timeout, want 0", got)
	}
	job, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "failed" || job["error_message"] != gammaRefreshTimeoutError {
		t.Fatalf("timed-out parallel job=%#v found=%v err=%v", job, found, err)
	}
}

func TestGammaFailedExpiryAdvancesCursorLikePythonWindowRefresh(t *testing.T) {
	first := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	second := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "gamma-retry.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 2), quoteRelease: make(chan struct{}),
		expirations: []string{first, second}, chainErrors: 1,
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	state, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("initial claim=%#v claimed=%v", state, claimed)
	}
	select {
	case <-provider.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("first batch did not start")
	}
	close(provider.quoteRelease)
	waitForGammaStatus(t, service, "completed", 45)
	partial, found, err := repository.AnalysisJob("QQQ", 45)
	partialResult, resultOK := partial["result"].(map[string]any)
	if err != nil || !found || partial["status"] != "completed" || partial["has_more"] != true || partial["next_cursor"] != first || !resultOK || gammaInt(partialResult, "loaded") != 1 {
		t.Fatalf("per-expiration error checkpoint=%#v resultOK=%v found=%v err=%v", partial, resultOK, found, err)
	}
	errorList, _ := partialResult["errors"].([]any)
	if len(errorList) != 1 || errorList[0] != first+": temporary chain error" {
		t.Fatalf("per-expiration errors=%#v, want the failed expiry recorded and advanced", partialResult["errors"])
	}

	state, claimed = service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("retry claim=%#v claimed=%v", state, claimed)
	}
	waitForGammaStatus(t, service, "completed", 45)
	if got := provider.chainCalls.Load(); got != 2 {
		t.Fatalf("chain calls=%d, want first and second expirations attempted once each", got)
	}
	final, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || final["status"] != "completed" || final["has_more"] != false || final["next_cursor"] != nil || final["processed"] != 2 {
		t.Fatalf("final checkpoint=%#v found=%v err=%v", final, found, err)
	}
}

func TestGammaSkipsMultipleFreshChainsBeforeRefreshingNextStaleExpiry(t *testing.T) {
	expirations := []string{
		time.Now().AddDate(0, 0, 7).Format("2006-01-02"),
		time.Now().AddDate(0, 0, 14).Format("2006-01-02"),
		time.Now().AddDate(0, 0, 21).Format("2006-01-02"),
	}
	path := filepath.Join(t.TempDir(), "gamma-fresh-skip.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveExpirations("QQQ", expirations); err != nil {
		t.Fatal(err)
	}
	openInterest, strike := int64(25), 100.0
	fetched := time.Now().UTC()
	for _, expiration := range expirations[:2] {
		if err := repository.SaveChain(model.ChainResponse{
			Symbol: "QQQ", Expiration: expiration, FetchedAt: &fetched, Data: []model.OptionRow{{
				ContractSymbol: "QQQ-" + expiration, ContractType: "call", Expiration: expiration,
				Strike: &strike, OpenInterest: &openInterest,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}), expirations: expirations,
	}
	close(provider.quoteRelease)
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	state, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("claim=%#v claimed=%v", state, claimed)
	}
	status := waitForGammaStatus(t, service, "completed", 45)
	result, ok := status["result"].(map[string]any)
	if !ok {
		t.Fatalf("Gamma result=%#v, want refresh_window result", status["result"])
	}
	batch, _ := result["batch_expirations"].([]any)
	if len(batch) != 1 || batch[0] != expirations[2] || gammaInt(result, "loaded") != 3 || result["has_more"] != false {
		t.Fatalf("fresh-chain scan result=%#v, want first two skipped and third refreshed", result)
	}
	if got := provider.chainCalls.Load(); got != 1 {
		t.Fatalf("provider chain calls=%d, want only the stale third expiry", got)
	}
}

func TestGammaBusyInitialHeavyGateReportsDeferredWindowResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-busy-gate.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second, LowMemory: true}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	nextExpiration := time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}), expirations: []string{expiration, nextExpiration},
	}
	close(provider.quoteRelease)
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	service.gammaHeavyGate <- struct{}{}

	state, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || state["status"] != "running" {
		t.Fatalf("claim=%#v claimed=%v", state, claimed)
	}
	status := waitForGammaStatus(t, service, "failed", 45)
	result, ok := status["result"].(map[string]any)
	if !ok || status["error_message"] != "内存保护：已有重任务在进行，Gamma 窗口已让路" || result["deferred"] != true {
		t.Fatalf("busy gate state=%#v result=%#v", status, status["result"])
	}
	if result["has_more"] != true || result["next_cursor"] != nil {
		t.Fatalf("deferred checkpoint=%#v, want retryable initial cursor", result)
	}
	<-service.gammaHeavyGate

	// A deferred batch must bypass the normal failed-job cooldown so the UI's
	// has_more continuation can claim it as soon as the competing work releases.
	retry, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || retry["status"] != "running" {
		t.Fatalf("claim after gate release=%#v claimed=%v, want immediate retry", retry, claimed)
	}
	completed := waitForGammaStatus(t, service, "completed", 45)
	completedResult, ok := completed["result"].(map[string]any)
	if !ok || completedResult["has_more"] != true || completedResult["next_cursor"] != expiration || provider.chainCalls.Load() != 1 {
		t.Fatalf("retry completion=%#v result=%#v chain calls=%d", completed, completed["result"], provider.chainCalls.Load())
	}

	// A later batch can also be deferred. Its cursor must survive persistence so
	// that the next claim resumes after the already refreshed expiration.
	service.gammaHeavyGate <- struct{}{}
	deferred, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || deferred["status"] != "running" {
		t.Fatalf("next batch claim=%#v claimed=%v", deferred, claimed)
	}
	status = waitForGammaStatus(t, service, "failed", 45)
	result, ok = status["result"].(map[string]any)
	if !ok || result["has_more"] != true || result["next_cursor"] != expiration || gammaInt(result, "processed") != 1 || gammaInt(result, "total") != 2 {
		t.Fatalf("deferred continuation checkpoint=%#v, want preserved cursor/progress", status["result"])
	}
	<-service.gammaHeavyGate
	deferred, claimed = service.claimGammaJob("QQQ", 45)
	if !claimed || deferred["status"] != "running" {
		t.Fatalf("continuation retry=%#v claimed=%v, want immediate resumed batch", deferred, claimed)
	}
	completed = waitForGammaStatus(t, service, "completed", 45)
	completedResult, ok = completed["result"].(map[string]any)
	if !ok || completedResult["has_more"] != false || provider.chainCalls.Load() != 2 {
		t.Fatalf("resumed completion=%#v result=%#v chain calls=%d", completed, completed["result"], provider.chainCalls.Load())
	}
}

func TestGammaCloseCancelsProviderWorkAndJoinsBackgroundJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-close-cancel.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	provider := &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}), expirations: []string{expiration},
	}
	service := NewWithProvider(cfg, repository, provider)
	if _, claimed := service.claimGammaJob("QQQ", 45); !claimed {
		t.Fatal("Gamma job was not claimed")
	}
	select {
	case <-provider.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("Gamma refresh did not reach provider")
	}
	closed := make(chan struct{})
	go func() {
		_ = service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel and join Gamma provider work")
	}
}

func TestGammaStaleInMemoryAttemptCanBeReclaimedAndLateFinishIsFenced(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-stale-reclaim.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaReclaimProvider{
		quoteStarted: make(chan int, 2), quoteRelease: make(chan struct{}), expiration: expiration,
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()
	var releaseOnce sync.Once
	releaseQuotes := func() { releaseOnce.Do(func() { close(provider.quoteRelease) }) }
	defer releaseQuotes()

	first, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || first["status"] != "running" {
		t.Fatalf("first claim=%#v claimed=%v", first, claimed)
	}
	select {
	case <-provider.quoteStarted:
	case <-time.After(time.Second):
		t.Fatal("first attempt did not reach provider")
	}

	stale := time.Now().Add(-gammaRefreshTimeout - time.Second)
	staleStamp := stale.UTC().Format(time.RFC3339Nano)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id='QQQ:45'`, staleStamp); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	firstAttempt := *service.gammaJobs[key]
	service.gammaJobs[key].startedAt = stale
	service.mu.Unlock()

	second, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || second["status"] != "running" || second["started_at"] == first["started_at"] {
		t.Fatalf("stale attempt was not reclaimed: state=%#v claimed=%v", second, claimed)
	}

	service.finishGammaJob(key, firstAttempt.jobID, firstAttempt.durableStarted, "completed", "late result", "", false, 1, 1)
	service.mu.RLock()
	current := *service.gammaJobs[key]
	service.mu.RUnlock()
	if current.status != "running" || current.durableStarted == firstAttempt.durableStarted {
		t.Fatalf("late first attempt changed current in-memory job: %#v", current)
	}
	job, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "running" || job["started_at"] != current.durableStarted {
		t.Fatalf("late first attempt changed durable job: %#v found=%v err=%v", job, found, err)
	}

	releaseQuotes()
	waitForGammaStatus(t, service, "completed", 45)
}

func TestGammaReclaimCancelsStaleProviderAttempt(t *testing.T) {
	expiration := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "gamma-reclaim-cancel.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := &gammaCancelProvider{
		expiration: expiration, started: make(chan int, 4), canceled: make(chan int, 4),
	}
	service := NewWithProvider(cfg, repository, provider)
	defer service.Close()

	first, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || first["status"] != "running" {
		t.Fatalf("first claim=%#v claimed=%v", first, claimed)
	}
	select {
	case call := <-provider.started:
		if call != 1 {
			t.Fatalf("first provider call=%d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first Gamma attempt did not reach the provider")
	}

	staleStamp := time.Now().Add(-gammaRefreshTimeout - time.Second).UTC().Format(time.RFC3339Nano)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id='QQQ:45'`, staleStamp); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	second, claimed := service.claimGammaJob("QQQ", 45)
	if !claimed || second["status"] != "running" || second["started_at"] == first["started_at"] {
		t.Fatalf("stale attempt was not reclaimed: state=%#v claimed=%v", second, claimed)
	}
	select {
	case call := <-provider.canceled:
		if call != 1 {
			t.Fatalf("reclaim canceled provider call %d, want stale call 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("reclaim did not cancel the stale provider call")
	}
	waitForGammaStatus(t, service, "completed", 45)
	job, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "completed" || job["started_at"] != second["started_at"] {
		t.Fatalf("stale cancellation changed replacement state: %#v found=%v err=%v", job, found, err)
	}
}

func TestGammaTimeoutMarksAttemptFailedAndFencesLateCompletion(t *testing.T) {
	cfg := config.Config{DatabasePath: filepath.Join(t.TempDir(), "gamma-timeout.db"), UpstreamTimeout: time.Second}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
	})
	defer service.Close()

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, 30*time.Second, gammaRefreshTimeout)
	if err != nil || !owner {
		t.Fatalf("claim timed-out attempt=%#v owner=%v err=%v", claimed, owner, err)
	}
	jobID, startedAt := fmt.Sprint(claimed["job_id"]), fmt.Sprint(claimed["started_at"])
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	service.gammaJobs[key] = &gammaJob{
		jobID: jobID, durableStarted: startedAt, symbol: "QQQ", horizonDays: 45,
		status: "running", startedAt: time.Now(),
	}
	service.mu.Unlock()

	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		service.runGammaJobWithTimeout(context.Background(), key, jobID, startedAt, 20*time.Millisecond, func(ctx context.Context) {
			close(entered)
			<-ctx.Done()
			service.finishGammaJob(key, jobID, startedAt, "completed", "late completion", "", false, 1, 1)
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timeout test worker did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout test worker did not observe cancellation")
	}
	job, found, err := repository.AnalysisJob("QQQ", 45)
	result, resultIsMap := job["result"].(map[string]any)
	if err != nil || !found || job["status"] != "failed" || job["error_message"] != gammaRefreshTimeoutError || (resultIsMap && result != nil) {
		t.Fatalf("timed-out job=%#v found=%v err=%v", job, found, err)
	}
	service.mu.RLock()
	current := *service.gammaJobs[key]
	service.mu.RUnlock()
	if current.status != "failed" || current.warning != gammaRefreshTimeoutError {
		t.Fatalf("late completion changed timeout state: %#v", current)
	}
}

func failGammaEventInsert(t *testing.T, path string) func() {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TRIGGER fail_gamma_event
BEFORE INSERT ON push_events WHEN NEW.kind='gamma'
BEGIN SELECT RAISE(ABORT, 'gamma event rejected'); END`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			database, openErr := sql.Open("sqlite", path)
			if openErr != nil {
				t.Errorf("open database to remove Gamma event trigger: %v", openErr)
				return
			}
			if _, dropErr := database.Exec(`DROP TRIGGER fail_gamma_event`); dropErr != nil {
				t.Errorf("remove Gamma event failure trigger: %v", dropErr)
			}
			if closeErr := database.Close(); closeErr != nil {
				t.Errorf("close database after removing Gamma event trigger: %v", closeErr)
			}
		})
	}
}

func TestGammaFinishPersistenceFailureIsVisibleAndRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-finish-persist-failure.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
	})
	defer service.Close()

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, 30*time.Second, gammaRefreshTimeout)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	jobID, startedAt := fmt.Sprint(claimed["job_id"]), fmt.Sprint(claimed["started_at"])
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	service.gammaJobs[key] = &gammaJob{
		jobID: jobID, durableStarted: startedAt, symbol: "QQQ", horizonDays: 45,
		status: "running", startedAt: time.Now(),
	}
	service.mu.Unlock()

	removeTrigger := failGammaEventInsert(t, path)
	defer removeTrigger()
	service.finishGammaJobAttempt(key, jobID, startedAt, "completed", "", map[string]any{"has_more": false})

	durable, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || durable["status"] != "running" {
		t.Fatalf("durable row after forced write failure=%#v found=%v err=%v", durable, found, err)
	}
	visible := service.gammaJobStatus("QQQ", 45)
	if visible["status"] != "completed" || !strings.Contains(fmt.Sprint(visible["error_message"]), "Gamma 状态写入 SQLite 失败") {
		t.Fatalf("in-memory terminal state did not expose persistence failure: %#v", visible)
	}

	removeTrigger()
	retried, claimedAgain := service.claimGammaJob("QQQ", 45)
	if claimedAgain || retried["status"] != "completed" {
		t.Fatalf("claim after persistence retry=%#v claimed=%v, want completed cooldown", retried, claimedAgain)
	}
	durable, found, err = repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || durable["status"] != "completed" || durable["error_message"] != nil {
		t.Fatalf("durable row after retry=%#v found=%v err=%v", durable, found, err)
	}
	visible = service.gammaJobStatus("QQQ", 45)
	if visible["status"] != "completed" || visible["error_message"] != nil {
		t.Fatalf("successful persistence retry left stale warning: %#v", visible)
	}
}

func TestGammaTimeoutPersistenceFailureIsVisibleAndRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-timeout-persist-failure.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
	})
	defer service.Close()

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, 30*time.Second, gammaRefreshTimeout)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	jobID, startedAt := fmt.Sprint(claimed["job_id"]), fmt.Sprint(claimed["started_at"])
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	service.gammaJobs[key] = &gammaJob{
		jobID: jobID, durableStarted: startedAt, symbol: "QQQ", horizonDays: 45,
		status: "running", startedAt: time.Now(),
	}
	service.mu.Unlock()

	removeTrigger := failGammaEventInsert(t, path)
	defer removeTrigger()
	done := make(chan struct{})
	go func() {
		service.runGammaJobWithTimeout(context.Background(), key, jobID, startedAt, 20*time.Millisecond, func(ctx context.Context) {
			<-ctx.Done()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout job did not finish persistence retries")
	}

	durable, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || durable["status"] != "running" {
		t.Fatalf("durable row after forced timeout write failure=%#v found=%v err=%v", durable, found, err)
	}
	visible := service.gammaJobStatus("QQQ", 45)
	if visible["status"] != "failed" || !strings.Contains(fmt.Sprint(visible["error_message"]), "Gamma 状态写入 SQLite 失败") {
		t.Fatalf("timeout persistence failure was not visible: %#v", visible)
	}

	removeTrigger()
	retried, claimedAgain := service.claimGammaJob("QQQ", 45)
	if claimedAgain || retried["status"] != "failed" {
		t.Fatalf("claim after timeout persistence retry=%#v claimed=%v, want failed cooldown", retried, claimedAgain)
	}
	durable, found, err = repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || durable["status"] != "failed" || durable["error_message"] != gammaRefreshTimeoutError {
		t.Fatalf("durable timeout state after retry=%#v found=%v err=%v", durable, found, err)
	}
	visible = service.gammaJobStatus("QQQ", 45)
	if visible["status"] != "failed" || visible["error_message"] != gammaRefreshTimeoutError {
		t.Fatalf("successful timeout persistence retry left stale warning: %#v", visible)
	}
}

func TestGammaTimeoutCancelsProviderBeforeWaitingForSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-timeout-sqlite-lock.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(cfg, repository, &gammaJobProvider{
		quoteStarted: make(chan struct{}, 1), quoteRelease: make(chan struct{}),
	})
	defer service.Close()

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, 30*time.Second, gammaRefreshTimeout)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	jobID, startedAt := fmt.Sprint(claimed["job_id"]), fmt.Sprint(claimed["started_at"])
	key := service.gammaJobKey("QQQ", 45)
	service.mu.Lock()
	service.gammaJobs[key] = &gammaJob{
		jobID: jobID, durableStarted: startedAt, symbol: "QQQ", horizonDays: 45,
		status: "running", startedAt: time.Now(),
	}
	service.mu.Unlock()

	lockDB, err := sql.Open("sqlite", path+"?_busy_timeout=30000&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		_ = lockDB.Close()
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = lockConn.Close()
		_ = lockDB.Close()
		t.Fatal(err)
	}

	started, canceled, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		service.runGammaJobWithTimeout(context.Background(), key, jobID, startedAt, 20*time.Millisecond, func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			close(canceled)
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Gamma job did not start")
	}
	select {
	case <-canceled:
	case <-time.After(300 * time.Millisecond):
		_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		_ = lockConn.Close()
		_ = lockDB.Close()
		t.Fatal("timeout waited for SQLite before canceling provider work")
	}

	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		_ = lockConn.Close()
		_ = lockDB.Close()
		t.Fatal(err)
	}
	if err := lockConn.Close(); err != nil {
		_ = lockDB.Close()
		t.Fatal(err)
	}
	if err := lockDB.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout callback did not finish after SQLite lock release")
	}
	job, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "failed" || job["error_message"] != gammaRefreshTimeoutError {
		t.Fatalf("timed-out job=%#v found=%v err=%v", job, found, err)
	}
}

func TestGammaTerminalJobCacheIsBoundedWithoutDroppingActiveOrUnpersistedJobs(t *testing.T) {
	service := &Service{gammaJobs: make(map[string]*gammaJob)}
	for index := 0; index < gammaTerminalJobCacheLimit+2; index++ {
		key := fmt.Sprintf("SYM%d|24h0m0s", index)
		service.gammaJobs[key] = &gammaJob{
			jobID: fmt.Sprintf("SYM%d:45", index), durableStarted: fmt.Sprintf("started-%d", index),
			status: "running", startedAt: time.Now(), symbol: fmt.Sprintf("SYM%d", index), horizonDays: 45,
		}
		if !service.markGammaJobFinished(key, service.gammaJobs[key].jobID, service.gammaJobs[key].durableStarted, "completed", "", map[string]any{}) {
			t.Fatalf("marking terminal Gamma job %d failed", index)
		}
		service.completeGammaPersistence(key, service.gammaJobs[key].jobID, service.gammaJobs[key].durableStarted)
	}

	service.gammaJobs["ACTIVE|45ns"] = &gammaJob{
		jobID: "ACTIVE:45", durableStarted: "active-start", status: "running", startedAt: time.Now(),
		symbol: "ACTIVE", horizonDays: 45,
	}
	service.gammaJobs["RETRY|45ns"] = &gammaJob{
		jobID: "RETRY:45", durableStarted: "retry-start", status: "failed", persistError: "database busy",
		finishedAt: time.Now(), symbol: "RETRY", horizonDays: 45,
	}
	service.gammaJobs["PENDING|45ns"] = &gammaJob{
		jobID: "PENDING:45", durableStarted: "pending-start", status: "completed", persistPending: true,
		finishedAt: time.Now(), symbol: "PENDING", horizonDays: 45,
	}
	service.gammaJobs["TRIGGER|45ns"] = &gammaJob{
		jobID: "TRIGGER:45", durableStarted: "trigger-start", status: "running", startedAt: time.Now(),
		symbol: "TRIGGER", horizonDays: 45,
	}
	if !service.markGammaJobFinished("TRIGGER|45ns", "TRIGGER:45", "trigger-start", "completed", "", map[string]any{}) {
		t.Fatal("marking trigger Gamma job failed")
	}
	service.completeGammaPersistence("TRIGGER|45ns", "TRIGGER:45", "trigger-start")

	if got := len(service.gammaJobs); got != gammaTerminalJobCacheLimit+3 {
		t.Fatalf("Gamma cache size=%d, want %d cached terminal plus active/retry/pending jobs", got, gammaTerminalJobCacheLimit+3)
	}
	for _, key := range []string{"SYM0|24h0m0s", "SYM1|24h0m0s", "SYM2|24h0m0s"} {
		if _, ok := service.gammaJobs[key]; ok {
			t.Fatalf("old terminal Gamma job %q was retained", key)
		}
	}
	for _, key := range []string{"SYM65|24h0m0s", "TRIGGER|45ns"} {
		if _, ok := service.gammaJobs[key]; !ok {
			t.Fatalf("recent terminal Gamma job %q was evicted", key)
		}
	}
	if _, ok := service.gammaJobs["ACTIVE|45ns"]; !ok {
		t.Fatal("active Gamma job was evicted")
	}
	if _, ok := service.gammaJobs["RETRY|45ns"]; !ok {
		t.Fatal("Gamma job with persistence failure was evicted")
	}
	if _, ok := service.gammaJobs["PENDING|45ns"]; !ok {
		t.Fatal("Gamma job with pending persistence was evicted")
	}
}

func waitForGammaStatus(t *testing.T, service *Service, want string, horizon int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := service.gammaJobStatus("QQQ", horizon)
		if status["status"] == want {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Gamma status did not reach %q: %#v", want, service.gammaJobStatus("QQQ", horizon))
	return nil
}

func hasExactKeys(values map[string]any, expected ...string) bool {
	if len(values) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}
