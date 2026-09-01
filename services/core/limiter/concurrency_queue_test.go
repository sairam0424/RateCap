package limiter_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sairam0424/RateCap/services/core/limiter"
)

// TestConcurrencyLimiter_BacklogSharedAcrossInstances proves the backlog
// ceiling is enforced fleet-wide (shared across every ConcurrencyLimiter
// instance backed by the same store — e.g. every ratecap-core replica),
// not per-instance. Before the fix, each instance tracked its own local
// atomic.Int64 counter, so a second instance had no visibility into the
// first instance's admissions and would wrongly believe it had room.
func TestConcurrencyLimiter_BacklogSharedAcrossInstances(t *testing.T) {
	const maxBacklog = 3
	const maxQueueWaitMs = 2000
	fs := newFakeConcurrencyStore()
	l1 := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, maxQueueWaitMs, 10)
	l2 := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, maxQueueWaitMs, 10)
	ctx := context.Background()

	// Fill the real concurrency slot so every Check() call falls through to
	// backlog admission instead of an immediate ALLOW, and nothing ever
	// frees it (so a backlog-admitted request just waits until timeout).
	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	// Fill l1's backlog to maxBacklog. Give the goroutines time to acquire
	// their slots before l2 tries — this ordering is what makes the
	// assertion below deterministic.
	var wg sync.WaitGroup
	for i := 0; i < maxBacklog; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l1.Check(ctx, limiter.Request{Key: "k"})
		}()
	}
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	d, err := l2.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected l2 to be rejected once l1 filled the shared backlog, got %v", d.Action)
	}
	// If backlog accounting is per-instance (the bug), l2 wrongly believes
	// it has room and polls for the full maxQueueWaitMs before timing out.
	// If it's shared (the fix), l2's own admission attempt fails
	// immediately against the already-full shared counter.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected l2 to reject immediately (shared backlog full) rather than poll for maxQueueWaitMs (%dms); took %v — backlog accounting is not fleet-wide", maxQueueWaitMs, elapsed)
	}

	wg.Wait()
}

func TestConcurrencyLimiter_BacklogFullReturnsImmediate429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 1, 300, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		l.Check(ctx, limiter.Request{Key: "k"})
	}()
	<-started
	time.Sleep(20 * time.Millisecond)

	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 when backlog is full, got %v", d.Action)
	}
	wg.Wait()
}

func TestConcurrencyLimiter_SuccessfulPollReturnsQueueAction(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 5000, 10)
	ctx := context.Background()

	token1 := ""
	if _, tok, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err == nil {
		token1 = tok
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		fs.DecrConcurrent(ctx, "k", token1)
	}()

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.QUEUE {
		t.Fatalf("expected QUEUE once a slot frees after waiting (server-side attribution; wire-transparent to the client), got %v", d.Action)
	}
	if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
		t.Fatalf("expected a reservation from the successful poll, got %+v", d.Reservations)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("expected to wait for the slot to free (~30ms), got %v", elapsed)
	}
}

func TestConcurrencyLimiter_DeadlineExceededReturns429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 50, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 after MaxQueueWaitMs elapses, got %v", d.Action)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected to wait roughly MaxQueueWaitMs (50ms) before timing out, got %v", elapsed)
	}
}

func TestConcurrencyLimiter_ContextCancellationPropagates(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 5000, 10)
	if _, _, err := fs.IncrConcurrent(context.Background(), "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := l.Check(ctx, limiter.Request{Key: "k"})
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

func TestConcurrencyLimiter_ShadowModeSkipsQueueingEvenWithQueueingEnabled(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, true, 5, 5000, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG (shadow mode takes precedence over queueing), got %v", d.Action)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("expected shadow mode to return immediately without queueing, took %v", elapsed)
	}
}

func TestConcurrencyLimiter_QueueingDisabledStillReturnsImmediate429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 5, 5000, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected immediate REJECT_429 when queueing_enabled is false regardless of MaxBacklog, got %v", d.Action)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("expected immediate rejection with no polling when queueing is disabled, took %v", elapsed)
	}
}

// TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog hammers a small
// MaxBacklog (3) with 50 concurrent waiters against a permanently-full cap,
// sampling the shared store's backlog key directly (BacklogDepth() was
// removed along with the per-instance local counter it reported on).
func TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog(t *testing.T) {
	const maxBacklog = 3
	const goroutines = 50
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, 200, 5)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Check(ctx, limiter.Request{Key: "k"})
		}()
	}

	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		for i := 0; i < 100; i++ {
			fs.mu.Lock()
			depth := int64(fs.tokens["backlog:k"])
			fs.mu.Unlock()
			if depth > peak.Load() {
				peak.Store(depth)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	<-sampleDone

	if peak.Load() > maxBacklog {
		t.Fatalf("backlog peaked at %d, exceeding maxBacklog %d — overshoot", peak.Load(), maxBacklog)
	}

	fs.mu.Lock()
	finalDepth := fs.tokens["backlog:k"]
	fs.mu.Unlock()
	if finalDepth != 0 {
		t.Fatalf("expected backlog to return to 0 after all goroutines finished, got %d", finalDepth)
	}
}

// TestConcurrencyLimiter_StressManyWaitersOneSlotFreeingRepeatedly stresses
// the interleaving of many waiters racing to grab a single slot as it frees
// and refills repeatedly, mirroring
// TestShedder_StressAllowReleaseInterleaving's shape. It asserts every
// completed Check() returned either QUEUE (won a freed slot) or REJECT_429
// (timed out) — never ALLOW (which would mean queueing was bypassed) and
// never an unexpected action.
func TestConcurrencyLimiter_StressManyWaitersOneSlotFreeingRepeatedly(t *testing.T) {
	const goroutines = 40
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, goroutines, 500, 5)
	ctx := context.Background()

	token := ""
	if _, tok, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err == nil {
		token = tok
	}

	stopFreeing := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFreeing:
				return
			default:
				time.Sleep(15 * time.Millisecond)
				fs.DecrConcurrent(ctx, "k", token)
				time.Sleep(5 * time.Millisecond)
				_, newTok, _ := fs.IncrConcurrent(ctx, "k", 1, 30000)
				token = newTok
			}
		}
	}()

	var wg sync.WaitGroup
	results := make(chan limiter.Action, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Check(ctx, limiter.Request{Key: "k"})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			results <- d.Action
		}()
	}
	wg.Wait()
	close(stopFreeing)
	close(results)

	for a := range results {
		if a != limiter.QUEUE && a != limiter.REJECT_429 {
			t.Errorf("expected every queued waiter to resolve to QUEUE or REJECT_429, got %v", a)
		}
	}
}
