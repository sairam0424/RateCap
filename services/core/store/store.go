package store

import "context"

type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
}
