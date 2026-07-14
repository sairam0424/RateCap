package limiter

import (
	"context"
	"math"
	"sync"
)

// unboundedCap is passed as the Lua script's cap argument to force its
// `count < cap` check to always pass, so IncrConcurrent still reserves a
// slot even when the real cap is already exceeded. Used only for shadow
// mode's would-be-reject path, where the design spec requires the slot to
// still be reserved so concurrency accounting stays accurate. MaxInt32 is
// chosen to be far larger than any real concurrency count while staying
// well under Lua 5.1's 2^53 integer-precision limit for tonumber().
const unboundedCap = math.MaxInt32

type concurrencyChecker interface {
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error)
	DecrConcurrent(ctx context.Context, key, token string) error
}

type ConcurrencyLimiter struct {
	store concurrencyChecker

	mu            sync.RWMutex
	cap           int
	maxDurationMs int64
	shadowMode    bool
}

func NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{store: s, cap: cap, maxDurationMs: maxDurationMs, shadowMode: shadowMode}
}

// Reconfigure and Check run concurrently in ratecap-core: Reconfigure is
// invoked from the config watcher's goroutine while Check runs on every
// gRPC handler goroutine. The mutex keeps a reload from tearing
// cap/maxDurationMs apart mid-read, matching the design spec's
// atomic-hot-reload requirement (the same pattern TokenBucketLimiter uses).
func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipConcurrencyLimit {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, maxDurationMs, shadowMode := l.cap, l.maxDurationMs, l.shadowMode
	l.mu.RUnlock()

	allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Token: token}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, req.Key, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Token: reservedToken}, nil
	}

	return Decision{Action: REJECT_429}, nil
}
