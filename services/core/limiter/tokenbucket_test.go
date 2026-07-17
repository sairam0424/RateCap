package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeStore struct {
	mu     sync.Mutex
	tokens map[string]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tokens: make(map[string]int)}
}

func (f *fakeStore) CheckAndDecrement(_ context.Context, key string, _, burst, cost int) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

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
	fs := newFakeStore()
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

func TestTokenBucketLimiter_DecisionCarriesRateLimiterTier(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 10, 5, false)

	d, err := l.Check(context.Background(), limiter.Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, d.Tier)
	}
}

func TestTokenBucketLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeStore()
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

func TestTokenBucketLimiter_ReconfigureChangesLimits(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 10, 1, false)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 before reconfigure, got %v", d.Action)
	}

	l.Reconfigure(10, 1, true)

	d, err = l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG after enabling shadow mode via reconfigure, got %v", d.Action)
	}
}

func TestTokenBucketLimiter_ConcurrentCheckAndReconfigureIsRaceFree(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 10, 100, false)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Check(ctx, limiter.Request{Key: "user-race", Cost: 1})
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Reconfigure(10, 100, n%2 == 0)
		}(i)
	}
	wg.Wait()
}
