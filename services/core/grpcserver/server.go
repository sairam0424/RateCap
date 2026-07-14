package grpcserver

import (
	"context"

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
	pipeline checker
	releaser concurrencyReleaser
}

func NewServer(p checker, releaser concurrencyReleaser) *Server {
	return &Server{pipeline: p, releaser: releaser}
}

func (s *Server) CheckRateLimit(ctx context.Context, req *ratecapv1.CheckRateLimitRequest) (*ratecapv1.CheckRateLimitResponse, error) {
	decision, err := s.pipeline.Check(ctx, limiter.Request{
		Key:                  req.Key,
		Cost:                 int(req.Cost),
		SkipConcurrencyLimit: req.SkipConcurrencyLimit,
	})
	if err != nil {
		return nil, err
	}

	return &ratecapv1.CheckRateLimitResponse{
		Action:           toProtoAction(decision.Action),
		RetryAfterMs:     decision.RetryAfterMs,
		ConcurrencyToken: decision.Token,
	}, nil
}

func (s *Server) ReleaseConcurrency(ctx context.Context, req *ratecapv1.ReleaseConcurrencyRequest) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	if err := s.releaser.DecrConcurrent(ctx, req.Key, req.ConcurrencyToken); err != nil {
		return nil, err
	}
	return &ratecapv1.ReleaseConcurrencyResponse{}, nil
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
	default:
		return ratecapv1.Action_REJECT_503
	}
}
