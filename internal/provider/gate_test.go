package provider

import (
	"context"
	"testing"
	"time"
)

func TestAcquireGateAllowsConfiguredConcurrencyAndWaitsAtCapacity(t *testing.T) {
	gate := make(chan struct{}, 2)
	firstRelease, err := acquireGate(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease, err := acquireGate(context.Background(), gate)
	if err != nil {
		firstRelease()
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, acquireErr := acquireGateTimeout(context.Background(), gate, time.Second)
		if acquireErr == nil {
			acquired <- release
		}
	}()

	select {
	case release := <-acquired:
		release()
		secondRelease()
		firstRelease()
		t.Fatal("third acquisition passed while both slots were occupied")
	case <-time.After(25 * time.Millisecond):
	}

	firstRelease()
	var thirdRelease func()
	select {
	case thirdRelease = <-acquired:
	case <-time.After(time.Second):
		secondRelease()
		t.Fatal("third acquisition did not proceed after a slot was released")
	}
	thirdRelease()
	secondRelease()
}

func TestAcquireGateTimeoutAndCancellation(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}

	if _, err := acquireGateTimeout(context.Background(), gate, 10*time.Millisecond); err != context.DeadlineExceeded {
		t.Fatalf("timed acquisition error = %v, want context.DeadlineExceeded", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireGate(ctx, gate); err != context.Canceled {
		t.Fatalf("canceled acquisition error = %v, want context.Canceled", err)
	}
}
