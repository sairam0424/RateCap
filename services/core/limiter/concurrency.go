package limiter

import (
	"context"
	"math"
	"sync"
	"time"
)

// unboundedCap is passed as the Lua script's cap argument to force its
// `count < cap` check to always pass, so IncrConcurrent still reserves a
// slot even when the real cap is already exceeded. Used only for shadow
// mode's would-be-reject path, where the design spec requires the slot to
// still be reserved so concurrency accounting stays accurate. MaxInt32 is
// chosen to be far larger than any real concurrency count while staying
// well under Lua 5.1's 2^53 integer-precision limit for tonumber().
const unboundedCap = math.MaxInt32

// backlogKeyPrefix namespaces Tier 2's bounded-queueing backlog counter in
// the shared store, distinct from the real per-key concurrency-slot key
// (bare req.Key), so the two counters never collide on the same key and so
// the backlog ceiling is enforced fleet-wide across every
// ConcurrencyLimiter instance sharing this store — not per-instance.
const backlogKeyPrefix = "backlog:"

type concurrencyChecker interface {
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error)
	DecrConcurrent(ctx context.Context, key, token string) error
}

type ConcurrencyLimiter struct {
	store concurrencyChecker

	mu              sync.RWMutex
	cap             int
	maxDurationMs   int64
	shadowMode      bool
	queueingEnabled bool
	maxBacklog      int
	maxQueueWaitMs  int64
	pollIntervalMs  int64
}

func NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		store:           s,
		cap:             cap,
		maxDurationMs:   maxDurationMs,
		shadowMode:      shadowMode,
		queueingEnabled: queueingEnabled,
		maxBacklog:      maxBacklog,
		maxQueueWaitMs:  maxQueueWaitMs,
		pollIntervalMs:  pollIntervalMs,
	}
}

// Reconfigure and Check run concurrently in ratecap-core: Reconfigure is
// invoked from the config watcher's goroutine while Check runs on every
// gRPC handler goroutine. The mutex keeps a reload from tearing
// cap/maxDurationMs apart mid-read, matching the design spec's
// atomic-hot-reload requirement (the same pattern TokenBucketLimiter uses).
func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
	l.queueingEnabled = queueingEnabled
	l.maxBacklog = maxBacklog
	l.maxQueueWaitMs = maxQueueWaitMs
	l.pollIntervalMs = pollIntervalMs
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, maxDurationMs, shadowMode := l.cap, l.maxDurationMs, l.shadowMode
	queueingEnabled, maxBacklog, maxQueueWaitMs, pollIntervalMs := l.queueingEnabled, l.maxBacklog, l.maxQueueWaitMs, l.pollIntervalMs
	l.mu.RUnlock()

	allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: req.Key, Token: token}}, Tier: "concurrency_limiter"}, nil
	}

	// Shadow mode's entire purpose is to observe without ever blocking a real
	// caller, so it takes precedence over queueing and skips it entirely.
	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, req.Key, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: req.Key, Token: reservedToken}}, Tier: "concurrency_limiter"}, nil
	}

	if !queueingEnabled {
		return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
	}

	backlogAllowed, backlogToken, err := l.acquireBacklogSlot(ctx, req.Key, maxBacklog, maxQueueWaitMs)
	if err != nil {
		return Decision{}, err
	}
	if !backlogAllowed {
		return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
	}
	defer func() {
		// Best-effort release: a lost DecrConcurrent (e.g. context canceled
		// concurrently with this defer) self-heals via the backlog key's own
		// reap deadline (maxQueueWaitMs, passed as IncrConcurrent's
		// maxDurationMs above) — the same safety net the real concurrency
		// slot already relies on, so the error is deliberately not retried.
		_ = l.store.DecrConcurrent(ctx, backlogKeyPrefix+req.Key, backlogToken)
	}()

	return l.pollUntilAllowedOrDeadline(ctx, req, cap, maxDurationMs, maxQueueWaitMs, pollIntervalMs)
}

// acquireBacklogSlot reserves a backlog slot in the shared store under a
// namespaced key, so the backlog ceiling is fleet-wide across every
// ConcurrencyLimiter instance sharing this store (e.g. every ratecap-core
// replica) — not per-instance, which was the bug this replaces.
func (l *ConcurrencyLimiter) acquireBacklogSlot(ctx context.Context, key string, maxBacklog int, maxQueueWaitMs int64) (bool, string, error) {
	return l.store.IncrConcurrent(ctx, backlogKeyPrefix+key, maxBacklog, maxQueueWaitMs)
}

func (l *ConcurrencyLimiter) pollUntilAllowedOrDeadline(ctx context.Context, req Request, cap int, maxDurationMs, maxQueueWaitMs, pollIntervalMs int64) (Decision, error) {
	deadline := time.NewTimer(time.Duration(maxQueueWaitMs) * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Duration(pollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-deadline.C:
			return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
		case <-ticker.C:
			allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
			if err != nil {
				return Decision{}, err
			}
			if allowed {
				return Decision{Action: QUEUE, Reservations: []TokenReservation{{Key: req.Key, Token: token}}, Tier: "concurrency_limiter"}, nil
			}
		}
	}
}
