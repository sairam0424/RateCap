package limiter

import "context"

type checker interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error)
}

type TokenBucketLimiter struct {
	store      checker
	rate       int
	burst      int
	shadowMode bool
}

func NewTokenBucketLimiter(s checker, rate, burst int, shadowMode bool) *TokenBucketLimiter {
	return &TokenBucketLimiter{store: s, rate: rate, burst: burst, shadowMode: shadowMode}
}

func (l *TokenBucketLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	allowed, retryAfterMs, err := l.store.CheckAndDecrement(ctx, req.Key, l.rate, l.burst, req.Cost)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW}, nil
	}

	if l.shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs}, nil
}
