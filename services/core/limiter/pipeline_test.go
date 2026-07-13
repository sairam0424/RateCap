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

func TestPipeline_AllTiersAllowReturnsAllowWithLastToken(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Token: "tok-123"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW, got %v", d.Action)
	}
	if d.Token != "tok-123" {
		t.Fatalf("expected token from tier2 to propagate, got %q", d.Token)
	}
	if !tier1.called || !tier2.called {
		t.Fatal("expected both tiers to be checked")
	}
}

func TestPipeline_FirstTierRejectShortCircuitsSecondTier(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 500}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Token: "notused"}}

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

func TestPipeline_ErrorFromAnyTierShortCircuits(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{err: errors.New("store unavailable")}

	p := limiter.NewPipeline(tier1, tier2)
	_, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err == nil {
		t.Fatal("expected error to propagate from tier2")
	}
}
