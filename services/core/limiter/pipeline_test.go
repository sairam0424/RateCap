package limiter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeTier struct {
	decision limiter.Decision
	err      error
	called   bool
}

func (f *fakeTier) Check(_ context.Context, _ limiter.Request) (limiter.Decision, error) {
	f.called = true
	return f.decision, f.err
}

func TestPipeline_AllTiersAllowAccumulatesAllReservations(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "tok-123"}}}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW, got %v", d.Action)
	}
	if len(d.Reservations) != 1 {
		t.Fatalf("expected tier2's reservation to propagate, got %d reservations", len(d.Reservations))
	}
	if d.Reservations[0].Key != "user-1" || d.Reservations[0].Token != "tok-123" {
		t.Fatalf("expected reservation {user-1 tok-123} to propagate, got %+v", d.Reservations[0])
	}
	if !tier1.called || !tier2.called {
		t.Fatal("expected both tiers to be checked")
	}
}

func TestPipeline_AllTiersAllowPropagatesLastTierTier(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Tier: "rate_limiter"}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Tier: "concurrency_limiter"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "concurrency_limiter" {
		t.Errorf(`expected the final ALLOW decision to carry the last tier's Tier ("concurrency_limiter"), got %q`, d.Tier)
	}
}

func TestPipeline_FirstTierRejectShortCircuitsSecondTier(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 500}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "notused"}}}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 500 {
		t.Fatalf("expected RetryAfterMs=500, got %d", d.RetryAfterMs)
	}
	if tier2.called {
		t.Fatal("expected tier2 to be short-circuited, but it was called")
	}
}

func TestPipeline_SecondTierRejectPropagatesDecision(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 250}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 250 {
		t.Fatalf("expected RetryAfterMs=250, got %d", d.RetryAfterMs)
	}
}

func TestPipeline_EarlierTierReservationSurvivesLaterTierRejection(t *testing.T) {
	tier1Token := "tok-tier1"
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: tier1Token}}}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 500}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if len(d.Reservations) != 1 {
		t.Fatalf("expected tier1's reservation to survive tier2's rejection, got %d reservations", len(d.Reservations))
	}
	if d.Reservations[0].Key != "user-1" || d.Reservations[0].Token != tier1Token {
		t.Fatalf("expected tier1's reservation {user-1 %s} to survive, got %+v", tier1Token, d.Reservations[0])
	}
}

func TestPipeline_ErrorFromAnyTierShortCircuits(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{err: errors.New("store unavailable")}

	p := limiter.NewPipeline(tier1, tier2)
	_, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err == nil {
		t.Fatal("expected error to propagate from tier2")
	}
}
