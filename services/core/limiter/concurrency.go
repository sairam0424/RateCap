package limiter

import (
	"context"
	"sync"
)

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

func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
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
		return Decision{Action: SHADOW_LOG}, nil
	}

	return Decision{Action: REJECT_429}, nil
}
