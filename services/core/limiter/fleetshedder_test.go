package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeFleetStore struct {
	mu      sync.Mutex
	tokens  map[string]int
	nextTok int
}

func newFakeFleetStore() *fakeFleetStore {
	return &fakeFleetStore{tokens: make(map[string]int)}
}

func (f *fakeFleetStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
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

func (f *fakeFleetStore) DecrConcurrent(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[key]--
	return nil
}

func TestFleetShedder_UsesFixedGlobalKeyRegardlessOfRequestKey(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := l.Check(ctx, limiter.Request{Key: "user-2", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	count := fs.tokens["fleet"]
	perUserCountUser1 := fs.tokens["user-1"]
	fs.mu.Unlock()

	if count != 2 {
		t.Fatalf("expected both requests (different req.Key) to count toward the shared 'fleet' key, got fleet=%d", count)
	}
	if perUserCountUser1 != 0 {
		t.Fatalf("expected req.Key to never be used as the store key, got user-1=%d", perUserCountUser1)
	}
}

func TestFleetShedder_AllowsExactlyCapCriticalRequests(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 5, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
		if len(d.Reservations) != 1 || d.Reservations[0].Key != "fleet" || d.Reservations[0].Token == "" {
			t.Fatalf("request %d: expected reservation {fleet, <non-empty token>}, got %+v", i, d.Reservations)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("6th critical request: expected REJECT_503 (full fleet cap of 5 exceeded), got %v", d.Action)
	}
}

func TestFleetShedder_ShedsSheddableAtReducedCapBeforeFullFleetCap(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("sheddable request %d: expected ALLOW (reduced cap is 10*(100-20)/100=8), got %v", i, d.Action)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("9th sheddable request: expected REJECT_503 (reduced cap of 8 exceeded, even though full fleet cap is 10), got %v", d.Action)
	}
}

func TestFleetShedder_CriticalStillSucceedsAfterSheddableCapExhausted(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable}); err != nil {
			t.Fatalf("unexpected error priming sheddable request %d: %v", i, err)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("critical request after 8 sheddable reservations: expected ALLOW (full fleet cap of 10 not yet exceeded), got %v", d.Action)
	}
}

func TestFleetShedder_ShadowModeReservesAndCoercesToShadowLog(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 1, 20, 30000, true)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over cap in shadow mode, got %v", d.Action)
	}
	if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
		t.Fatalf("expected a reserved token even in shadow mode, got %+v", d.Reservations)
	}
}

func TestFleetShedder_SkipReservationsBypassesEntirely(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 1, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", SkipReservations: true, Priority: limiter.Critical})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW when SkipReservations is set, got %v", i, d.Action)
		}
		if len(d.Reservations) != 0 {
			t.Fatalf("request %d: expected no reservation when SkipReservations is set, got %+v", i, d.Reservations)
		}
	}

	fs.mu.Lock()
	count := fs.tokens["fleet"]
	fs.mu.Unlock()

	if count != 0 {
		t.Fatalf("expected the store to never be touched when SkipReservations is set, got fleet=%d, want 0", count)
	}
}

func TestFleetShedder_ConcurrentCheckAndReconfigureIsRaceFree(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			priority := limiter.Sheddable
			if n%2 == 0 {
				priority = limiter.Critical
			}
			_, _ = l.Check(ctx, limiter.Request{Key: "user-race", Priority: priority})
		}(i)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Reconfigure(10, 20, 30000, n%2 == 0)
		}(i)
	}
	wg.Wait()
}

func TestFleetShedder_ReconfigureChangesReservedPct(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 90, 30000, false)
	ctx := context.Background()

	for i := 0; i < 1; i++ {
		if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable}); err != nil {
			t.Fatalf("unexpected error priming request %d: %v", i, err)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("expected REJECT_503 with reservedCriticalPct=90 (sheddable cap=10*(100-90)/100=1), got %v", d.Action)
	}

	l.Reconfigure(10, 0, 30000, false)

	d, err = l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW after reconfiguring reservedCriticalPct=0 (sheddable cap=10*(100-0)/100=10, only 1 reservation exists), got %v", d.Action)
	}
}
