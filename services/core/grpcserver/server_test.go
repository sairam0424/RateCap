package grpcserver_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

var testSigningKey = []byte("test-signing-key-do-not-use-in-production")

func signTestToken(uuid string, signingKey []byte) string {
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(uuid))
	return uuid + "." + hex.EncodeToString(mac.Sum(nil))
}

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
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

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

func TestCheckRateLimit_ConvertsQueueActionToProtoQueue(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.QUEUE, Tier: "concurrency_limiter"}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_QUEUE {
		t.Errorf("expected Action_QUEUE, got %v", resp.Action)
	}
}

func TestCheckRateLimit_ReturnsTierFromDecision(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW, Tier: "rate_limiter"}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, resp.Tier)
	}
}

func TestCheckRateLimit_ReturnsReject429WithRetryAfter(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 250}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

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
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

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
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

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

func TestCheckRateLimit_PropagatesSkipReservationsToPipeline(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:              "user-1",
		Cost:             1,
		SkipReservations: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fl.lastReq.SkipReservations {
		t.Error("expected SkipReservations=true to propagate into limiter.Request")
	}
}

func TestCheckRateLimit_PropagatesCriticalPriorityToPipeline(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:      "user-1",
		Cost:     1,
		Priority: ratecapv1.Priority_CRITICAL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Critical {
		t.Errorf("expected Priority to map to limiter.Critical, got %v", fl.lastReq.Priority)
	}
}

func TestCheckRateLimit_DefaultPriorityMapsToSheddable(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Sheddable {
		t.Errorf("expected default/unset Priority to map to limiter.Sheddable, got %v", fl.lastReq.Priority)
	}
}

func TestCheckRateLimit_ExplicitSheddablePriorityMapsToSheddable(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:      "user-1",
		Cost:     1,
		Priority: ratecapv1.Priority_SHEDDABLE,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Sheddable {
		t.Errorf("expected explicit Priority_SHEDDABLE to map to limiter.Sheddable, got %v", fl.lastReq.Priority)
	}
}

func TestCheckRateLimit_UnspecifiedPriorityMapsToSheddable(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:      "user-1",
		Cost:     1,
		Priority: ratecapv1.Priority_PRIORITY_UNSPECIFIED,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Sheddable {
		t.Errorf("expected explicit Priority_PRIORITY_UNSPECIFIED to map to limiter.Sheddable (the same safe default as never setting the field), got %v", fl.lastReq.Priority)
	}
}

func TestCheckRateLimit_SanitizesStoreError(t *testing.T) {
	fl := &fakeLimiter{err: errors.New("redis: unexpected type *redis.StatusCmd for result")}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
	if strings.Contains(err.Error(), "StatusCmd") {
		t.Errorf("expected sanitized error, but original error text leaked: %v", err)
	}
}

func TestReleaseConcurrency_CallsDecrConcurrentWithKeyAndToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	token := signTestToken("tok-abc", testSigningKey)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: token,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if releaser.lastKey != "user-1" {
		t.Errorf("expected DecrConcurrent called with key=%q, got %q", "user-1", releaser.lastKey)
	}
	if releaser.lastToken != token {
		t.Errorf("expected DecrConcurrent called with token=%q, got %q", token, releaser.lastToken)
	}
}

func TestReleaseConcurrency_SanitizesStoreErrorButPropagatesFailure(t *testing.T) {
	releaser := &fakeReleaser{err: errors.New("dial tcp 10.0.0.5:6379: connect: connection refused")}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	token := signTestToken("tok-abc", testSigningKey)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: token,
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
	if strings.Contains(err.Error(), "10.0.0.5") || strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected sanitized error, but original error text leaked: %v", err)
	}
}

func TestReleaseConcurrency_AcceptsValidSignedToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	tok := signTestToken("real-uuid", testSigningKey)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: tok,
	})
	if err != nil {
		t.Fatalf("unexpected error for a validly signed token: %v", err)
	}
	if releaser.lastToken != tok {
		t.Errorf("expected DecrConcurrent to be called with the validated token %q, got %q", tok, releaser.lastToken)
	}
}

func TestReleaseConcurrency_RejectsTamperedToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	tok := signTestToken("real-uuid", testSigningKey)
	tampered := tok[:len(tok)-1] + "0"

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: tampered,
	})
	if err == nil {
		t.Fatal("expected an error for a tampered token")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if releaser.lastToken != "" {
		t.Errorf("expected DecrConcurrent to never be called for a tampered token, but it was called with %q", releaser.lastToken)
	}
}

func TestReleaseConcurrency_RejectsMalformedToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	malformed := "no-dot-separator-here"

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: malformed,
	})
	if err == nil {
		t.Fatal("expected an error for a malformed token with no signature separator")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if releaser.lastToken != "" {
		t.Errorf("expected DecrConcurrent to never be called for a malformed token, but it was called with %q", releaser.lastToken)
	}
}

func TestReleaseConcurrency_RejectsTokenSignedWithDifferentKey(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser, testSigningKey)
	otherKeyForTest := []byte("a-completely-different-hmac-key")
	tok := signTestToken("real-uuid", otherKeyForTest)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: tok,
	})
	if err == nil {
		t.Fatal("expected an error for a token signed with a different key")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if releaser.lastToken != "" {
		t.Errorf("expected DecrConcurrent to never be called for a token signed with a different key, but it was called with %q", releaser.lastToken)
	}
}
