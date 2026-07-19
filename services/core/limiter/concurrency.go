package limiter

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
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

	backlog atomic.Int64
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

// BacklogDepth reports the current number of goroutines occupying a backlog
// slot. It exists for tests that need to observe live queue depth under
// concurrent load; production code never calls it.
func (l *ConcurrencyLimiter) BacklogDepth() int64 {
	return l.backlog.Load()
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

	if !l.acquireBacklogSlot(maxBacklog) {
		return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
	}
	defer l.backlog.Add(-1)

	return l.pollUntilAllowedOrDeadline(ctx, req, cap, maxDurationMs, maxQueueWaitMs, pollIntervalMs)
}

// acquireBacklogSlot is a counting semaphore via CAS loop, mirroring
// worker.Shedder's exact pattern (services/sidecar/worker/shedder.go),
// rather than a buffered channel — maxBacklog is hot-reloadable via
// Reconfigure, and a channel's capacity cannot be resized after creation.
func (l *ConcurrencyLimiter) acquireBacklogSlot(maxBacklog int) bool {
	for {
		current := l.backlog.Load()
		if current >= int64(maxBacklog) {
			return false
		}
		if l.backlog.CompareAndSwap(current, current+1) {
			return true
		}
	}
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
