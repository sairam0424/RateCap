package grpcserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/limiter"
)

type checker interface {
	Check(ctx context.Context, req limiter.Request) (limiter.Decision, error)
}

type concurrencyReleaser interface {
	DecrConcurrent(ctx context.Context, key, token string) error
}

type Server struct {
	ratecapv1.UnimplementedRatecapServiceServer
	pipeline   checker
	releaser   concurrencyReleaser
	signingKey []byte
}

func NewServer(p checker, releaser concurrencyReleaser, signingKey []byte) *Server {
	return &Server{pipeline: p, releaser: releaser, signingKey: signingKey}
}

// verifyToken confirms a Tier 2 concurrency token was actually issued by
// this core instance (the HMAC-SHA256 suffix over the UUID, matching how
// store.IncrConcurrent signs it) before it's allowed to release a slot —
// closing issue #12's forgeable-bearer-token gap. It does not bind the
// token to a specific caller; any authenticated caller can still release
// any other authenticated caller's valid token, matching this repo's
// existing shared-secret trust model (see SECURITY.md).
func verifyToken(token string, signingKey []byte) bool {
	uuidPart, sigPart, found := strings.Cut(token, ".")
	if !found {
		return false
	}
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(uuidPart))
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(sigPart), []byte(expected)) == 1
}

func (s *Server) CheckRateLimit(ctx context.Context, req *ratecapv1.CheckRateLimitRequest) (*ratecapv1.CheckRateLimitResponse, error) {
	// PRIORITY_UNSPECIFIED (a caller that never set the field) and SHEDDABLE
	// both map to limiter.Sheddable — the same safe default either way, now
	// an explicit case rather than an incidental fallthrough of an if-check
	// that couldn't distinguish "never set" from "set to sheddable".
	priority := limiter.Sheddable
	switch req.Priority {
	case ratecapv1.Priority_CRITICAL:
		priority = limiter.Critical
	case ratecapv1.Priority_SHEDDABLE, ratecapv1.Priority_PRIORITY_UNSPECIFIED:
		priority = limiter.Sheddable
	}

	decision, err := s.pipeline.Check(ctx, limiter.Request{
		Key:              req.Key,
		Cost:             int(req.Cost),
		SkipReservations: req.SkipReservations,
		Priority:         priority,
	})
	if err != nil {
		return nil, internalError("CheckRateLimit", err)
	}

	reservations := make([]*ratecapv1.TokenReservation, 0, len(decision.Reservations))
	for _, r := range decision.Reservations {
		reservations = append(reservations, &ratecapv1.TokenReservation{Key: r.Key, Token: r.Token})
	}

	return &ratecapv1.CheckRateLimitResponse{
		Action:       toProtoAction(decision.Action),
		RetryAfterMs: decision.RetryAfterMs,
		Reservations: reservations,
		Tier:         decision.Tier,
	}, nil
}

func (s *Server) ReleaseConcurrency(ctx context.Context, req *ratecapv1.ReleaseConcurrencyRequest) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	if !verifyToken(req.ConcurrencyToken, s.signingKey) {
		log.Printf("grpcserver: ReleaseConcurrency: rejected invalid/forged token for key %q", req.Key)
		return nil, status.Error(codes.PermissionDenied, "invalid concurrency token")
	}
	if err := s.releaser.DecrConcurrent(ctx, req.Key, req.ConcurrencyToken); err != nil {
		return nil, internalError("ReleaseConcurrency", err)
	}
	return &ratecapv1.ReleaseConcurrencyResponse{}, nil
}

func internalError(context string, err error) error {
	log.Printf("grpcserver: %s: %v", context, err)
	return status.Error(codes.Internal, "internal error")
}

func toProtoAction(a limiter.Action) ratecapv1.Action {
	switch a {
	case limiter.ALLOW:
		return ratecapv1.Action_ALLOW
	case limiter.REJECT_429:
		return ratecapv1.Action_REJECT_429
	case limiter.REJECT_503:
		return ratecapv1.Action_REJECT_503
	case limiter.SHADOW_LOG:
		return ratecapv1.Action_SHADOW_LOG
	case limiter.QUEUE:
		return ratecapv1.Action_QUEUE
	default:
		return ratecapv1.Action_REJECT_503
	}
}
