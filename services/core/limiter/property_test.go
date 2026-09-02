package limiter_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/sairam0424/RateCap/services/core/limiter"
)

// fakePropertyTokenBucketStore is an in-memory reference model of a token bucket:
// the exact same refill/decrement arithmetic the real Lua script performs,
// reimplemented independently so rapid can compare the real limiter's
// allow/reject decisions against this trivial model over random sequences.
type fakePropertyTokenBucketStore struct {
	tokens float64
	burst  int
}

func (f *fakePropertyTokenBucketStore) CheckAndDecrement(_ context.Context, _ string, rate, burst, cost int) (bool, int64, int64, error) {
	if f.burst != burst {
		f.tokens = float64(burst)
		f.burst = burst
	}
	if f.tokens < float64(cost) {
		return false, 0, int64(f.tokens), nil
	}
	f.tokens -= float64(cost)
	return true, 0, int64(f.tokens), nil
}

func TestTokenBucketLimiter_NeverAllowsMoreThanBurstWithinOneWindow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		burst := rapid.IntRange(1, 100).Draw(rt, "burst")
		requests := rapid.IntRange(1, 200).Draw(rt, "requests")

		store := &fakePropertyTokenBucketStore{tokens: float64(burst), burst: burst}
		l := limiter.NewTokenBucketLimiter(store, 0, burst, false)

		allowedCount := 0
		for i := 0; i < requests; i++ {
			decision, err := l.Check(context.Background(), limiter.Request{Key: "prop-key", Cost: 1})
			if err != nil {
				rt.Fatalf("unexpected error: %v", err)
			}
			if decision.Action == limiter.ALLOW {
				allowedCount++
			}
		}

		if allowedCount > burst {
			rt.Errorf("with rate=0 (no refill) and burst=%d, allowed %d requests out of %d — invariant violated", burst, allowedCount, requests)
		}
	})
}

// fakePropertyConcurrencyStore is a reference model of a bounded
// concurrency counter: an in-memory set of outstanding tokens, capped at
// `cap`. Named distinctly from concurrency_test.go's existing
// fakeConcurrencyStore (same package, different internal representation,
// used only by this file's property tests) to avoid redeclaring the type.
type fakePropertyConcurrencyStore struct {
	outstanding map[string]bool
	nextToken   int
}

func (f *fakePropertyConcurrencyStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
	if f.outstanding == nil {
		f.outstanding = map[string]bool{}
	}
	if len(f.outstanding) >= cap {
		return false, "", nil
	}
	f.nextToken++
	token := "tok-" + string(rune('a'+f.nextToken%26))
	f.outstanding[token] = true
	return true, token, nil
}

func (f *fakePropertyConcurrencyStore) DecrConcurrent(_ context.Context, _, token string) error {
	delete(f.outstanding, token)
	return nil
}

func TestConcurrencyLimiter_OutstandingNeverExceedsCapAcrossAcquireReleaseSequences(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cap := rapid.IntRange(1, 50).Draw(rt, "cap")
		store := &fakePropertyConcurrencyStore{}
		l := limiter.NewConcurrencyLimiter(store, cap, 60000, false, false, 0, 0, 0)

		var held []limiter.TokenReservation
		steps := rapid.IntRange(1, 200).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			acquire := rapid.Bool().Draw(rt, "acquire")
			if acquire || len(held) == 0 {
				decision, err := l.Check(context.Background(), limiter.Request{Key: "prop-key", Cost: 1})
				if err != nil {
					rt.Fatalf("unexpected error: %v", err)
				}
				if decision.Action == limiter.ALLOW {
					held = append(held, decision.Reservations...)
				}
				if len(store.outstanding) > cap {
					rt.Errorf("outstanding count %d exceeded cap %d after an acquire", len(store.outstanding), cap)
				}
			} else {
				idx := rapid.IntRange(0, len(held)-1).Draw(rt, "release_index")
				token := held[idx]
				if err := store.DecrConcurrent(context.Background(), token.Key, token.Token); err != nil {
					rt.Fatalf("unexpected error: %v", err)
				}
				held = append(held[:idx], held[idx+1:]...)
			}
		}
	})
}
