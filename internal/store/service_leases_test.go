package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceLeaseConcurrentWorkersHaveSingleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease-concurrent.db")
	const workers = 16
	stores := make([]*SQLite, 2)
	for index := range stores {
		repository, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stores[index] = repository
		defer repository.Close()
	}

	start := make(chan struct{})
	var acquired atomic.Int64
	var workersGroup sync.WaitGroup
	errCh := make(chan error, workers)
	for index := 0; index < workers; index++ {
		workersGroup.Add(1)
		go func(index int) {
			defer workersGroup.Done()
			<-start
			ok, err := stores[index%len(stores)].TryAcquireServiceLeaseContext(context.Background(), "gamma:test", fmt.Sprintf("worker-%d", index), time.Minute)
			if err != nil {
				errCh <- err
				return
			}
			if ok {
				acquired.Add(1)
			}
		}(index)
	}
	close(start)
	workersGroup.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("lease acquisition: %v", err)
	}
	if got := acquired.Load(); got != 1 {
		t.Fatalf("concurrent lease winners=%d, want exactly one", got)
	}
}

func TestServiceLeaseContextCancelsWhileWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease-context-cancel.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	claimant, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer claimant.Close()
	writer, err := owner.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(context.Background(), "ROLLBACK")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan struct {
		acquired bool
		err      error
	}, 1)
	go func() {
		acquired, acquireErr := claimant.TryAcquireServiceLeaseContext(ctx, "gamma:cancel", "worker-b", time.Minute)
		result <- struct {
			acquired bool
			err      error
		}{acquired: acquired, err: acquireErr}
	}()
	deadline := time.Now().Add(time.Second)
	for claimant.db.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if claimant.db.Stats().InUse == 0 {
		t.Fatal("lease acquisition did not reach SQLite")
	}
	select {
	case outcome := <-result:
		if outcome.err == nil || outcome.acquired || ctx.Err() == nil {
			t.Fatalf("lease acquisition=(acquired=%v, err=%v, ctx=%v), want cancellation", outcome.acquired, outcome.err, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("lease acquisition did not return after context cancellation")
	}
	var count int
	if err := claimant.db.QueryRow("SELECT COUNT(*) FROM service_leases WHERE name='gamma:cancel'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("canceled lease was persisted: rows=%d", count)
	}
}
