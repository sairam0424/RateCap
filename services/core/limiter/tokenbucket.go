package limiter

import (
	"context"
	"sync"

	coremetrics "github.com/ratecap/core/metrics"
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
		// Fail OPEN for Tier 1 only, matching Stripe's documented precedent
		// (fail-open on request-rate, fail-closed on concurrent-requests —
		// see docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md
		// Phase 2 item 3). Tiers 2/3 (ConcurrencyLimiter, FleetShedder) do
		// NOT get this treatment: their whole purpose is bounding concurrent
		// resource usage, so letting them fail open would remove the bound
		// they exist to enforce during exactly the outage when it matters most.
		coremetrics.RecordFailOpen("rate_limiter", "store_error")
		return Decision{Action: ALLOW, Tier: "rate_limiter"}, nil
	}

	if allowed {
		return Decision{Action: ALLOW, Tier: "rate_limiter"}, nil
	}

	if shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs, Tier: "rate_limiter"}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs, Tier: "rate_limiter"}, nil
}
