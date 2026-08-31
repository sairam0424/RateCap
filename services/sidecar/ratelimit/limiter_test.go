package ratelimit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ratecap/sidecar/ratelimit"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestLimiter_AllowsUpToBurst(t *testing.T) {
	l := ratelimit.NewWithClock(5, 5, fixedClock(time.Unix(0, 0)))

	for i := 0; i < 5; i++ {
		if !l.Allow() {
			t.Fatalf("request %d: expected Allow() to return true within burst of 5", i)
		}
	}
}

func TestLimiter_RejectsBeyondBurst(t *testing.T) {
	l := ratelimit.NewWithClock(5, 5, fixedClock(time.Unix(0, 0)))

	for i := 0; i < 5; i++ {
		if !l.Allow() {
			t.Fatalf("request %d: expected Allow() to return true", i)
		}
	}

	if l.Allow() {
		t.Fatal("6th request: expected Allow() to return false, burst of 5 exhausted")
	}
}

func TestLimiter_RefillsOverElapsedTime(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewWithClock(2, 2, clock)

	// Two separate checks, not `!l.Allow() || !l.Allow()`: that combined form
	// short-circuits the second call whenever the first already returns
	// false, silently consuming only one token instead of the two this test
	// means to exercise.
	if !l.Allow() {
		t.Fatal("expected first initial burst token to be available")
	}
	if !l.Allow() {
		t.Fatal("expected second initial burst token to be available")
	}
	if l.Allow() {
		t.Fatal("expected Allow() to return false, bucket empty")
	}

	now = now.Add(500 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("expected Allow() to return true after 500ms refills 1 token at rate 2/s")
	}
	if l.Allow() {
		t.Fatal("expected Allow() to return false, only one token was refilled")
	}
}

func TestLimiter_RefillNeverExceedsBurstCap(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewWithClock(1, 3, clock)

	now = now.Add(100 * time.Second)

	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("request %d: expected Allow() to return true, refill capped at burst 3", i)
		}
	}
	if l.Allow() {
		t.Fatal("expected Allow() to return false — refill must not exceed the burst cap")
	}
}

func TestLimiter_PartialRefillAccumulatesAcrossMultipleChecks(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewWithClock(10, 1, clock)

	if !l.Allow() {
		t.Fatal("expected initial token to be available")
	}
	if l.Allow() {
		t.Fatal("expected bucket to be empty immediately after consuming the only token")
	}

	now = now.Add(50 * time.Millisecond)
	if l.Allow() {
		t.Fatal("expected Allow() to still return false — only half a token has refilled at rate 10/s")
	}

	now = now.Add(60 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("expected Allow() to return true — enough elapsed time has passed to refill one token")
	}
}

func TestLimiter_ConcurrentAllowNeverExceedsBurstUnderConcurrency(t *testing.T) {
	const burst = 5
	l := ratelimit.NewWithClock(1, burst, fixedClock(time.Unix(0, 0)))

	var wg sync.WaitGroup
	var allowedCount atomic.Int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				allowedCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if allowedCount.Load() != burst {
		t.Fatalf("expected exactly %d concurrent Allow() calls to succeed with a fixed clock, got %d", burst, allowedCount.Load())
	}
}

// TestLimiter_StressConcurrentAllowNeverExceedsBurst hammers a fixed-clock
// (no refill) limiter with 500 concurrent goroutines, mirroring
// worker.Shedder's own concurrency stress-test precedent
// (TestShedder_StressNeverExceedsMaxHeldSimultaneously). With the clock
// frozen, the bucket can never refill mid-test, so the number of successful
// Allow() calls must land exactly on the burst size — any deviation would
// indicate the mutex failed to serialize the read-refill-decrement sequence.
func TestLimiter_StressConcurrentAllowNeverExceedsBurst(t *testing.T) {
	const burst = 10
	const goroutines = 500

	l := ratelimit.NewWithClock(1, burst, fixedClock(time.Unix(0, 0)))

	var wg sync.WaitGroup
	var allowedCount atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				allowedCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if allowedCount.Load() != burst {
		t.Fatalf("expected exactly %d successful Allow() calls out of %d goroutines with a frozen clock, got %d", burst, goroutines, allowedCount.Load())
	}
}

// TestLimiter_StressConcurrentAllowWithAdvancingClockIsRaceFree races many
// goroutines calling Allow() against a handful of goroutines advancing a
// shared fake clock, exercising the mutex under -race with both state
// dimensions (tokens and time) changing concurrently.
func TestLimiter_StressConcurrentAllowWithAdvancingClockIsRaceFree(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(0, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	l := ratelimit.NewWithClock(1000, 10, clock)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Allow()
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			now = now.Add(time.Millisecond)
			mu.Unlock()
		}()
	}
	wg.Wait()
}

func TestLimiter_RetryAfterIsZeroWhenTokenAvailable(t *testing.T) {
	l := ratelimit.NewWithClock(5, 5, fixedClock(time.Unix(0, 0)))

	if got := l.RetryAfter(); got != 0 {
		t.Fatalf("expected RetryAfter() == 0 when a token is available, got %v", got)
	}
}

func TestLimiter_RetryAfterIsPositiveWhenBucketExhausted(t *testing.T) {
	l := ratelimit.NewWithClock(2, 1, fixedClock(time.Unix(0, 0)))

	if !l.Allow() {
		t.Fatal("expected the single burst token to be available")
	}

	got := l.RetryAfter()
	if got <= 0 {
		t.Fatalf("expected RetryAfter() > 0 once the bucket is exhausted, got %v", got)
	}
	if got > time.Second {
		t.Fatalf("expected RetryAfter() <= 1s at rate 2/s, got %v", got)
	}
}
