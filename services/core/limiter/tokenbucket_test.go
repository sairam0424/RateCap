package limiter_test

import (
	"context"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeStore struct {
	tokens map[string]int
	burst  int
}

func newFakeStore(burst int) *fakeStore {
	return &fakeStore{tokens: make(map[string]int), burst: burst}
}

func (f *fakeStore) CheckAndDecrement(_ context.Context, key string, _, burst, cost int) (bool, int64, error) {
	remaining, ok := f.tokens[key]
	if !ok {
		remaining = burst
	}
	if remaining >= cost {
		f.tokens[key] = remaining - cost
		return true, 0, nil
	}
	return false, 100, nil
}

func TestTokenBucketLimiter_AllowsExactlyBurstRequests(t *testing.T) {
	fs := newFakeStore(5)
	l := limiter.NewTokenBucketLimiter(fs, 10, 5, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Cost: 1})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("6th request: expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 100 {
		t.Fatalf("expected RetryAfterMs=100, got %d", d.RetryAfterMs)
	}
}

func TestTokenBucketLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeStore(1)
	l := limiter.NewTokenBucketLimiter(fs, 10, 1, true)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-2", Cost: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over limit in shadow mode, got %v", d.Action)
	}
}
