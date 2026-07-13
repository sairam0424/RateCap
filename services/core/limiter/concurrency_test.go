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
	l := limiter.NewConcurrencyLimiter(fs, 3, 30000, false)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1"})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
		if d.Token == "" {
			t.Fatalf("request %d: expected non-empty token", i)
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

func TestConcurrencyLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true)
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
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true)
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
			if d.Token == "" {
				t.Fatalf("request %d: expected a reserved token even in shadow mode, got empty string", i)
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

func TestConcurrencyLimiter_ConcurrentCheckAndReconfigureIsRaceFree(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false)
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
			l.Reconfigure(10, 30000, n%2 == 0)
		}(i)
	}
	wg.Wait()
}

func TestConcurrencyLimiter_ReconfigureChangesCap(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false)
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

	l.Reconfigure(1, 30000, true)

	d, err = l.Check(ctx, limiter.Request{Key: "user-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG after enabling shadow mode via reconfigure, got %v", d.Action)
	}
}
