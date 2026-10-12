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
