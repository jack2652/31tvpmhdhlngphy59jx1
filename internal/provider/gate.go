package provider

import (
	"context"
	"time"
)

// acquireGate shares an upstream concurrency budget across provider adapters.
// A nil gate means the caller is deliberately unbounded (primarily tests).
func acquireGate(ctx context.Context, gate chan struct{}) (func(), error) {
	return acquireGateTimeout(ctx, gate, 0)
}

func acquireGateTimeout(ctx context.Context, gate chan struct{}, wait time.Duration) (func(), error) {
	if gate == nil {
		return func() {}, nil
	}
	if wait <= 0 {
		select {
		case gate <- struct{}{}:
			return func() { <-gate }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	}
}
