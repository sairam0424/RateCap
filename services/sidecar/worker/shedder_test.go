package worker_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sairam0424/RateCap/services/sidecar/worker"
)

func TestShedder_AllowsExactlyMaxConcurrent(t *testing.T) {
	s := worker.NewShedder(3)

	for i := 0; i < 3; i++ {
		if !s.Allow() {
			t.Fatalf("request %d: expected Allow() to return true within max of 3", i)
		}
	}

	if s.Allow() {
		t.Fatal("4th request: expected Allow() to return false, max of 3 exceeded")
	}
}

func TestShedder_ReleaseFreesASlot(t *testing.T) {
	s := worker.NewShedder(1)

	if !s.Allow() {
		t.Fatal("expected first Allow() to return true")
	}
	if s.Allow() {
		t.Fatal("expected second Allow() to return false, max of 1 exceeded")
	}

	s.Release()

	if !s.Allow() {
		t.Fatal("expected Allow() to return true after Release() frees the slot")
	}
}

func TestShedder_ConcurrentAllowAndReleaseIsRaceFree(t *testing.T) {
	s := worker.NewShedder(10)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				s.Release()
			}
		}()
	}
	wg.Wait()
}

func TestShedder_AllowedCountNeverExceedsMaxUnderConcurrency(t *testing.T) {
	s := worker.NewShedder(5)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != 5 {
		t.Fatalf("expected exactly 5 concurrent Allow() calls to succeed (none released), got %d", allowedCount)
	}
}

// TestShedder_StressNeverExceedsMaxHeldSimultaneously hammers a small max
// (2) with 500 concurrent goroutines that each Allow(), hold the slot for a
// moment, then Release(). It tracks the *live* number of held slots (not
// just a final tally) so it can detect a transient overshoot — e.g. two
// goroutines both getting an Allow()==true at once when max should permit
// only one more slot. It also asserts the live count never goes negative,
// which would indicate a Release() paired with a lost/duplicated Allow().
func TestShedder_StressNeverExceedsMaxHeldSimultaneously(t *testing.T) {
	const max = 2
	const goroutines = 500

	s := worker.NewShedder(max)

	var held atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !s.Allow() {
				return
			}
			defer s.Release()

			current := held.Add(1)
			for {
				p := peak.Load()
				if current <= p || peak.CompareAndSwap(p, current) {
					break
				}
			}
			if current > max {
				t.Errorf("held count %d exceeded max %d — overshoot", current, max)
			}
			if current < 0 {
				t.Errorf("held count went negative: %d", current)
			}
			held.Add(-1)
		}()
	}

	wg.Wait()

	if peak.Load() > max {
		t.Fatalf("peak concurrently-held slots %d exceeded max %d", peak.Load(), max)
	}
	if held.Load() != 0 {
		t.Fatalf("expected held count to return to 0 after all goroutines finished, got %d", held.Load())
	}
}

// TestShedder_StressAllowReleaseInterleaving runs 500 goroutines in a tight
// Allow/Release retry loop against a small max, racing many concurrent
// Release() calls against many concurrent Allow() calls on the same slot
// count. It verifies the internal atomic counter (exposed only indirectly,
// via behavior) never goes negative and Allow() never permits more than max
// callers to hold a slot at once, i.e. it stresses exactly the interleavings
// the CAS loop is meant to make impossible.
func TestShedder_StressAllowReleaseInterleaving(t *testing.T) {
	const max = 3
	const goroutines = 500
	const attemptsPerGoroutine = 20

	s := worker.NewShedder(max)

	var held atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := 0; a < attemptsPerGoroutine; a++ {
				if !s.Allow() {
					continue
				}
				current := held.Add(1)
				if current > max {
					t.Errorf("held count %d exceeded max %d", current, max)
				}
				if current < 1 {
					t.Errorf("held count %d dropped below 1 immediately after Allow()", current)
				}
				held.Add(-1)
				s.Release()
			}
		}()
	}

	wg.Wait()

	if held.Load() != 0 {
		t.Fatalf("expected held count to settle at 0, got %d", held.Load())
	}
}

func TestShedder_InFlightReflectsCurrentCount(t *testing.T) {
	s := worker.NewShedder(3)

	if s.InFlight() != 0 {
		t.Fatalf("expected InFlight() == 0 initially, got %d", s.InFlight())
	}

	s.Allow()
	if s.InFlight() != 1 {
		t.Fatalf("expected InFlight() == 1 after one Allow(), got %d", s.InFlight())
	}

	s.Allow()
	if s.InFlight() != 2 {
		t.Fatalf("expected InFlight() == 2 after two Allow() calls, got %d", s.InFlight())
	}

	s.Release()
	if s.InFlight() != 1 {
		t.Fatalf("expected InFlight() == 1 after one Release(), got %d", s.InFlight())
	}
}

// TestShedder_BoundaryAtMaxMinusOneAndMax exercises the exact boundary the
// CAS loop's comparison (current >= s.max) must get right: the transition
// from one slot remaining (current == max-1) to zero slots remaining
// (current == max). A single goroutine drives this deterministically to
// pin down off-by-one behavior before adding concurrency stress on top.
func TestShedder_BoundaryAtMaxMinusOneAndMax(t *testing.T) {
	const max = 4
	s := worker.NewShedder(max)

	for i := int64(0); i < max-1; i++ {
		if !s.Allow() {
			t.Fatalf("Allow() %d: expected true while current (%d) < max (%d)", i, i, max)
		}
	}

	// current == max-1 here: exactly one slot must remain.
	if !s.Allow() {
		t.Fatalf("expected Allow() to succeed at current == max-1 (last available slot)")
	}

	// current == max here: no slots must remain.
	if s.Allow() {
		t.Fatalf("expected Allow() to fail at current == max (no slots remaining)")
	}

	s.Release()

	// current == max-1 again after Release(): exactly one slot must be available.
	if !s.Allow() {
		t.Fatalf("expected Allow() to succeed again after Release() brought current back to max-1")
	}
}

func TestShedder_BelowRampStart_AlwaysAllows(t *testing.T) {
	s := worker.NewShedderWithRamp(100, 80) // ramp starts at 80% of max
	for i := 0; i < 79; i++ {
		if !s.Allow() {
			t.Fatalf("expected allow #%d (below ramp-start threshold of 80), but was rejected", i)
		}
	}
}

func TestShedder_AtMax_NeverAllows(t *testing.T) {
	s := worker.NewShedderWithRamp(10, 50)
	// A reject inside the ramp window (current in [5,10)) is a probabilistic
	// admission-control decision, not a slot consumption — retrying until a
	// slot actually fills is safe and deterministic; it's the boundary at
	// max (checked below) that must be unconditional regardless of ramp.
	for i := 0; i < 10; i++ {
		filled := false
		for attempt := 0; attempt < 10000; attempt++ {
			if s.Allow() {
				filled = true
				break
			}
		}
		if !filled {
			t.Fatalf("slot #%d never filled after many retries despite ramp being probabilistic (not a hard reject) below max", i)
		}
	}
	if s.Allow() {
		t.Error("expected reject once inflight has reached max, regardless of ramp")
	}
}

func TestShedder_WithinRampWindow_ProbabilityDecreasesTowardMax(t *testing.T) {
	const max = 1000
	const rampStartPct = 50 // ramp window is [500, 1000)
	const trials = 20000

	rejectedEarlyInWindow := 0
	rejectedLateInWindow := 0
	for i := 0; i < trials; i++ {
		s := worker.NewShedderWithRamp(max, rampStartPct)
		for j := 0; j < 520; j++ {
			s.Allow()
		}
		if !s.Allow() {
			rejectedEarlyInWindow++
		}
	}
	for i := 0; i < trials; i++ {
		s := worker.NewShedderWithRamp(max, rampStartPct)
		for j := 0; j < 980; j++ {
			s.Allow()
		}
		if !s.Allow() {
			rejectedLateInWindow++
		}
	}

	if rejectedLateInWindow <= rejectedEarlyInWindow {
		t.Errorf("expected rejection rate to increase closer to max: early-window rejects=%d, late-window rejects=%d out of %d trials each", rejectedEarlyInWindow, rejectedLateInWindow, trials)
	}
}

func TestShedder_LegacyConstructor_BehavesAsHardCutoff(t *testing.T) {
	s := worker.NewShedder(5)
	for i := 0; i < 5; i++ {
		if !s.Allow() {
			t.Fatalf("expected allow #%d filling to max via legacy NewShedder", i)
		}
	}
	if s.Allow() {
		t.Error("expected NewShedder (no ramp param) to behave as a hard cutoff at max, matching pre-existing behavior")
	}
}
