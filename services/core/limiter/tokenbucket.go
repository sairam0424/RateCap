package limiter

import (
	"context"
	"sync"
)

type checker interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error)
}

type TokenBucketLimiter struct {
	store checker

	mu         sync.RWMutex
	rate       int
	burst      int
	shadowMode bool
}

func NewTokenBucketLimiter(s checker, rate, burst int, shadowMode bool) *TokenBucketLimiter {
	return &TokenBucketLimiter{store: s, rate: rate, burst: burst, shadowMode: shadowMode}
}

// Reconfigure and Check run concurrently in ratecap-core: Reconfigure is
// invoked from the config watcher's goroutine while Check runs on every
// gRPC handler goroutine. The mutex keeps a reload from tearing rate/burst
// apart mid-read, matching the design spec's atomic-hot-reload requirement.
func (l *TokenBucketLimiter) Reconfigure(rate, burst int, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = rate
	l.burst = burst
	l.shadowMode = shadowMode
}

func (l *TokenBucketLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	l.mu.RLock()
	rate, burst, shadowMode := l.rate, l.burst, l.shadowMode
	l.mu.RUnlock()

	allowed, retryAfterMs, err := l.store.CheckAndDecrement(ctx, req.Key, rate, burst, req.Cost)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW}, nil
	}

	if shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs}, nil
}
