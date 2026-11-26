package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/model"
	"option-scope-go/internal/store"
)

func TestEmptyExpirationProbeSurvivesServiceRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiration-probe.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().UTC().AddDate(0, 0, 14).Format("2006-01-02")
	if err := repository.SaveExpirations("QQQ", []string{expiration}); err != nil {
		t.Fatal(err)
	}
	price := 100.0
	first := NewWithProvider(cfg, repository, &testMarketProvider{quote: model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}}, expirations: []string{}})
	response, err := first.refreshSnapshot(context.Background(), "QQQ", "", 0)
	if err != nil || (!response.QuoteOnly && !response.Skipped) {
		t.Fatalf("empty-catalog refresh response=%#v err=%v", response, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &testMarketProvider{quote: model.Quote{Price: &price, Provider: "test", Sessions: map[string]any{}}, expirations: []string{expiration}}
	second := NewWithProvider(cfg, reopened, fake)
	defer second.Close()
	response, err = second.refreshSnapshot(context.Background(), "QQQ", "", time.Minute)
	if err != nil || fake.expirationCalls.Load() != 0 || fake.chainCalls.Load() != 0 {
		t.Fatalf("restart ignored recent empty probe: response=%#v expiryCalls=%d chainCalls=%d err=%v", response, fake.expirationCalls.Load(), fake.chainCalls.Load(), err)
	}
}

// 持有真实SQLite写锁时，关闭必须取消后台估值缓存写入，不等待锁释放。
func TestServiceCloseCancelsValuationCacheWriterWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valuation-close.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &blockedFirstQuoteValuationProvider{
		testMarketProvider: &testMarketProvider{},
		started:            make(chan struct{}, 1),
		release:            make(chan struct{}),
	}
	service := NewWithProvider(config.Config{DatabasePath: path, UpstreamTimeout: time.Second}, repository, fake)
	t.Cleanup(func() { _ = service.Close() })
	price := 100.0
	service.scheduleValuationWithQuote("QQQ", &model.Quote{Price: &price, Symbol: "QQQ"})
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("后台估值未启动")
	}
	releaseWriter := holdSQLiteWriter(t, path)
	defer releaseWriter()
	close(fake.release)
	// 允许真实工作者进入缓存持久化；关闭时锁仍由另一连接持有。
	time.Sleep(100 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("关闭服务：%v", err)
		}
	case <-time.After(time.Second):
		releaseWriter()
		<-closed
		t.Fatal("关闭服务仍等待后台缓存写锁，未响应取消")
	}
}

// Two independent SQLite handles model separate workers. A worker must defer
// to an existing snapshot owner, and can fetch after that owner releases it.
func TestDurableSnapshotLeaseCoordinatesIndependentServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-lease.db")
	cfg := config.Config{DatabasePath: path, UpstreamTimeout: time.Second}
	leaseOwner, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseOwner.Close()
	workerStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().UTC().AddDate(0, 0, 14).Format("2006-01-02")
	leaseName := "snapshot-refresh:QQQ:" + expiration
	if acquired, err := leaseOwner.TryAcquireServiceLease(leaseName, "other-worker", time.Minute); err != nil || !acquired {
		t.Fatalf("seed lease acquired=%v err=%v", acquired, err)
	}
	price := 100.0
	row := model.OptionRow{ContractSymbol: "QQQ-C-100", Expiration: expiration, ContractType: "call", Bid: floatPointer(.9), Ask: floatPointer(1.1)}
	fake := &testMarketProvider{quote: model.Quote{Price: &price, Provider: "worker", Sessions: map[string]any{}}, expirations: []string{expiration}, chainRows: []model.OptionRow{row}}
	worker := NewWithProvider(cfg, workerStore, fake)
	defer worker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, refreshErr := worker.refreshSnapshot(ctx, "QQQ", expiration, 0)
	cancel()
	if refreshErr == nil || fake.quoteCalls.Load() != 0 {
		t.Fatalf("worker ignored existing durable lease: err=%v quoteCalls=%d", refreshErr, fake.quoteCalls.Load())
	}
	if err := leaseOwner.ReleaseServiceLease(leaseName, "other-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.refreshSnapshot(context.Background(), "QQQ", expiration, 0); err != nil {
		t.Fatalf("refresh after lease release: %v", err)
	}
	if fake.quoteCalls.Load() != 1 || fake.chainCalls.Load() != 1 {
		t.Fatalf("post-lease upstream calls quote=%d chain=%d, want one each", fake.quoteCalls.Load(), fake.chainCalls.Load())
	}
}

func floatPointer(value float64) *float64 { return &value }
