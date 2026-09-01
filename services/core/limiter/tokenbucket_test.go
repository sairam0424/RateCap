package limiter_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sairam0424/RateCap/services/core/limiter"
	coremetrics "github.com/sairam0424/RateCap/services/core/metrics"
)

type fakeStore struct {
	mu     sync.Mutex
	tokens map[string]int
	err    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{tokens: make(map[string]int)}
}

func (f *fakeStore) CheckAndDecrement(_ context.Context, key string, _, burst, cost int) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return false, 0, f.err
	}

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

func TestTokenBucketLimiter_Check_FailsOpenOnStoreError(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("dial tcp: connection refused")
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	decision, err := l.Check(context.Background(), limiter.Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("expected fail-open (no error), got: %v", err)
	}
	if decision.Action != limiter.ALLOW {
		t.Errorf("expected Action=ALLOW on a store error (fail-open), got %v", decision.Action)
	}
	if decision.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, decision.Tier)
	}
}

func TestTokenBucketLimiter_Check_RecordsFailOpenMetricOnStoreError(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("dial tcp: connection refused")
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	_, _ = l.Check(context.Background(), limiter.Request{Key: "user-1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before+1 {
		t.Errorf("expected FailOpenTotal{tier=rate_limiter,reason=store_error} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestTokenBucketLimiter_Check_NoStoreErrorDoesNotRecordFailOpen(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	_, _ = l.Check(context.Background(), limiter.Request{Key: "user-1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before {
		t.Errorf("expected FailOpenTotal unchanged when the store call succeeds, before=%v after=%v", before, after)
	}
}

func TestTokenBucketLimiter_SetRate_ChangesEffectiveRate(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	previous := l.SetRate(999)

	if previous != 100 {
		t.Errorf("expected previous rate of 100, got %d", previous)
	}
}

func TestTokenBucketLimiter_Burst_ReturnsCurrentBurst(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 42, false)
	if got := l.Burst(); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestTokenBucketLimiter_SetRate_ConcurrentWithCheckIsRaceFree(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.SetRate(n)
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Check(context.Background(), limiter.Request{Key: "k", Cost: 1})
		}()
	}
	wg.Wait()
}
