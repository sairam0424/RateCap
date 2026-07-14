package grpcserver_test

import (
	"context"
	"errors"
	"testing"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

type fakeLimiter struct {
	decision limiter.Decision
	err      error
	lastReq  limiter.Request
}

func (f *fakeLimiter) Check(_ context.Context, req limiter.Request) (limiter.Decision, error) {
	f.lastReq = req
	return f.decision, f.err
}

type fakeReleaser struct {
	lastKey   string
	lastToken string
	err       error
}

func (f *fakeReleaser) DecrConcurrent(_ context.Context, key, token string) error {
	f.lastKey = key
	f.lastToken = token
	return f.err
}

func TestCheckRateLimit_ReturnsAllowDecision(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}

func TestCheckRateLimit_ReturnsReject429WithRetryAfter(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 250}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_REJECT_429 {
		t.Errorf("expected REJECT_429, got %v", resp.Action)
	}
	if resp.RetryAfterMs != 250 {
		t.Errorf("expected RetryAfterMs=250, got %d", resp.RetryAfterMs)
	}
}

func TestCheckRateLimit_ReturnsReservationsWhenPresent(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "tok-abc"}}}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Reservations) != 1 {
		t.Fatalf("expected 1 reservation, got %d", len(resp.Reservations))
	}
	if resp.Reservations[0].Key != "user-1" || resp.Reservations[0].Token != "tok-abc" {
		t.Fatalf("expected reservation {user-1 tok-abc}, got {%s %s}", resp.Reservations[0].Key, resp.Reservations[0].Token)
	}
}

func TestCheckRateLimit_ReturnsNoReservationsWhenNonePresent(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Reservations) != 0 {
		t.Fatalf("expected 0 reservations, got %d", len(resp.Reservations))
	}
}

func TestCheckRateLimit_PropagatesSkipConcurrencyLimitToPipeline(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:                  "user-1",
		Cost:                 1,
		SkipConcurrencyLimit: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fl.lastReq.SkipConcurrencyLimit {
		t.Error("expected SkipConcurrencyLimit=true to propagate into limiter.Request")
	}
}

func TestReleaseConcurrency_CallsDecrConcurrentWithKeyAndToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: "tok-abc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if releaser.lastKey != "user-1" {
		t.Errorf("expected DecrConcurrent called with key=%q, got %q", "user-1", releaser.lastKey)
	}
	if releaser.lastToken != "tok-abc" {
		t.Errorf("expected DecrConcurrent called with token=%q, got %q", "tok-abc", releaser.lastToken)
	}
}

func TestReleaseConcurrency_PropagatesStoreError(t *testing.T) {
	releaser := &fakeReleaser{err: errors.New("redis unavailable")}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: "tok-abc",
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}
