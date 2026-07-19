package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeConcurrencyStore struct {
	mu      sync.Mutex
	tokens  map[string]int
	nextTok int
}

func newFakeConcurrencyStore() *fakeConcurrencyStore {
	return &fakeConcurrencyStore{tokens: make(map[string]int)}
}

func (f *fakeConcurrencyStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := f.tokens[key]
	if count >= cap {
		return false, "", nil
	}
	f.tokens[key] = count + 1
	f.nextTok++
	return true, string(rune('a' + f.nextTok)), nil
}

func (f *fakeConcurrencyStore) DecrConcurrent(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[key]--
	return nil
}

func TestConcurrencyLimiter_AllowsExactlyCapRequests(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 3, 30000, false, false, 0, 0, 0)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1"})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
		if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
			t.Fatalf("request %d: expected exactly one reservation with a non-empty token, got %+v", i, d.Reservations)
		}
		if d.Reservations[0].Key != "user-1" {
			t.Fatalf("request %d: expected reservation key %q, got %q", i, "user-1", d.Reservations[0].Key)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("4th request: expected REJECT_429, got %v", d.Action)
	}
}

func TestConcurrencyLimiter_DecisionCarriesConcurrencyLimiterTier(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false, false, 0, 0, 0)

	d, err := l.Check(context.Background(), limiter.Request{Key: "user-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "concurrency_limiter" {
		t.Errorf(`expected Tier="concurrency_limiter", got %q`, d.Tier)
	}
}

func TestConcurrencyLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, false, 0, 0, 0)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over cap in shadow mode, got %v", d.Action)
	}
}

func TestConcurrencyLimiter_ShadowModeStillReservesSlot(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, false, 0, 0, 0)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-4"})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if i > 0 {
			if d.Action != limiter.SHADOW_LOG {
				t.Fatalf("request %d: expected SHADOW_LOG, got %v", i, d.Action)
			}
			if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
				t.Fatalf("request %d: expected a reserved token even in shadow mode, got %+v", i, d.Reservations)
			}
		}
	}

	fs.mu.Lock()
	count := fs.tokens["user-4"]
	fs.mu.Unlock()

	if count != 3 {
		t.Fatalf("expected shadow mode to reserve a slot for every over-cap request (accurate accounting), got tracked count %d, want 3", count)
	}
}

func TestConcurrencyLimiter_SkipConcurrencyLimitBypassesTheCapEntirely(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 0, 0, 0)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-skip", SkipReservations: true})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW when SkipConcurrencyLimit is set, got %v", i, d.Action)
		}
		if len(d.Reservations) != 0 {
			t.Fatalf("request %d: expected no reservation when SkipConcurrencyLimit is set, got %+v", i, d.Reservations)
		}
	}

	fs.mu.Lock()
	count := fs.tokens["user-skip"]
	fs.mu.Unlock()

	if count != 0 {
		t.Fatalf("expected the store to never be touched when SkipConcurrencyLimit is set, got tracked count %d, want 0", count)
	}
}

func TestConcurrencyLimiter_RejectionSetsRetryAfterMsToMaxDuration(t *testing.T) {
	fs := newFakeConcurrencyStore()
	const maxDurationMs = 30000
	l := limiter.NewConcurrencyLimiter(fs, 1, maxDurationMs, false, false, 0, 0, 0)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-retry"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-retry"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != maxDurationMs {
		t.Fatalf("expected RetryAfterMs to equal maxDurationMs (%d), got %d", maxDurationMs, d.RetryAfterMs)
	}
}

func TestConcurrencyLimiter_ConcurrentCheckAndReconfigureIsRaceFree(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false, false, 0, 0, 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Check(ctx, limiter.Request{Key: "user-race"})
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Reconfigure(10, 30000, n%2 == 0, false, 0, 0, 0)
		}(i)
	}
	wg.Wait()
}

func TestConcurrencyLimiter_ReconfigureChangesCap(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 0, 0, 0)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-3"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 before reconfigure, got %v", d.Action)
	}

	l.Reconfigure(1, 30000, true, false, 0, 0, 0)

	d, err = l.Check(ctx, limiter.Request{Key: "user-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG after enabling shadow mode via reconfigure, got %v", d.Action)
	}
}
