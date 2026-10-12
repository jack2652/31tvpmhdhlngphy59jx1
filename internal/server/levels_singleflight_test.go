package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/store"
)

type levelsCacheResult struct {
	value map[string]any
	err   error
}

func TestLevelsCacheMissBuildsOnceAndWaitersReadIndependentCacheValues(t *testing.T) {
	service := newLevelsCacheTestService(t)
	key := "levels-flight-shared"
	started, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	build := func() map[string]any {
		builds.Add(1)
		close(started)
		<-release
		return map[string]any{"nested": map[string]any{"value": float64(7)}}
	}
	results := make(chan levelsCacheResult, 2)
	go func() {
		value, err := service.getOrBuildLevels(context.Background(), key, 16, build)
		results <- levelsCacheResult{value: value, err: err}
	}()
	<-started
	go func() {
		value, err := service.getOrBuildLevels(context.Background(), key, 16, build)
		results <- levelsCacheResult{value: value, err: err}
	}()
	waitForLevelsCacheWaiters(t, service, key, 1)
	close(release)

	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent results errors=%v/%v", first.err, second.err)
	}
	if builds.Load() != 1 {
		t.Fatalf("build count=%d, want one", builds.Load())
	}
	firstNested := first.value["nested"].(map[string]any)
	secondNested := second.value["nested"].(map[string]any)
	firstNested["value"] = float64(99)
	if got := secondNested["value"]; got != float64(7) {
		t.Fatalf("waiter shares leader's mutable map: value=%v, want 7", got)
	}

	cached, err := service.getOrBuildLevels(context.Background(), key, 16, build)
	if err != nil || cached["nested"].(map[string]any)["value"] != float64(7) || builds.Load() != 1 {
		t.Fatalf("subsequent cache read=%#v err=%v builds=%d", cached, err, builds.Load())
	}
}

func TestLevelsCacheFlightReturnsSharedEmptyOutcomeWithoutRebuilding(t *testing.T) {
	service := newLevelsCacheTestService(t)
	key := "levels-flight-empty"
	started, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	build := func() map[string]any {
		builds.Add(1)
		close(started)
		<-release
		return nil
	}
	results := make(chan levelsCacheResult, 2)
	for range 2 {
		go func() {
			value, err := service.getOrBuildLevels(context.Background(), key, 16, build)
			results <- levelsCacheResult{value: value, err: err}
		}()
		if builds.Load() == 0 {
			<-started
		}
	}
	waitForLevelsCacheWaiters(t, service, key, 1)
	close(release)
	for range 2 {
		if result := <-results; result.value != nil || result.err != nil {
			t.Fatalf("empty levels result=%#v, want nil result without error", result)
		}
	}
	if builds.Load() != 1 {
		t.Fatalf("empty build count=%d, want one", builds.Load())
	}
}

func TestLevelsCacheFlightHonorsCallerCancellationWithoutCancelingSharedBuild(t *testing.T) {
	service := newLevelsCacheTestService(t)
	key := "levels-flight-cancel"
	started, release := make(chan struct{}), make(chan struct{})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	var builds atomic.Int64
	build := func() map[string]any {
		builds.Add(1)
		close(started)
		<-release
		return map[string]any{"ready": true}
	}
	leaderResult := make(chan levelsCacheResult, 1)
	go func() {
		value, err := service.getOrBuildLevels(leaderCtx, key, 16, build)
		leaderResult <- levelsCacheResult{value: value, err: err}
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan levelsCacheResult, 1)
	go func() {
		value, err := service.getOrBuildLevels(waiterCtx, key, 16, build)
		waiterResult <- levelsCacheResult{value: value, err: err}
	}()
	waitForLevelsCacheWaiters(t, service, key, 1)
	cancelWaiter()
	if result := <-waiterResult; !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled waiter error=%v, want context.Canceled", result.err)
	}

	activeWaiterResult := make(chan levelsCacheResult, 1)
	go func() {
		value, err := service.getOrBuildLevels(context.Background(), key, 16, build)
		activeWaiterResult <- levelsCacheResult{value: value, err: err}
	}()
	waitForLevelsCacheWaiters(t, service, key, 1)
	cancelLeader()
	close(release)
	if result := <-leaderResult; !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled leader error=%v, want context.Canceled", result.err)
	}
	if result := <-activeWaiterResult; result.err != nil || result.value["ready"] != true {
		t.Fatalf("active waiter result=%#v, want cached shared build", result)
	}
	if builds.Load() != 1 {
		t.Fatalf("build count=%d, want one", builds.Load())
	}
}

func newLevelsCacheTestService(t *testing.T) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "levels-singleflight.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithProvider(config.Config{DatabasePath: path, AnalysisCacheEntries: 16}, repository, nil)
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close levels cache test service: %v", err)
		}
	})
	return service
}

func waitForLevelsCacheWaiters(t *testing.T, service *Service, key string, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		service.mu.RLock()
		flight := service.levelsFlights[key]
		waiters := 0
		if flight != nil {
			waiters = flight.waiters
		}
		service.mu.RUnlock()
		if waiters >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d levels cache waiters on %q", count, key)
}
