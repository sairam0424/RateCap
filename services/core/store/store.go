package store

import "context"

type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (allowed bool, token string, err error)
	DecrConcurrent(ctx context.Context, key, token string) error
}
