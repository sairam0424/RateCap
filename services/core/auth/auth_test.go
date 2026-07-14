package auth_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ratecap/core/auth"
)

func TestUnaryServerInterceptor_RejectsMissingSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "response", nil
	}

	_, err := interceptor(context.Background(), "request", &grpc.UnaryServerInfo{}, handler)

	if err == nil {
		t.Fatal("expected error when no metadata is present")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
	if handlerCalled {
		t.Error("expected handler NOT to be called when secret is missing")
	}
}

func TestUnaryServerInterceptor_RejectsWrongSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "response", nil
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "wrong-secret"))
	_, err := interceptor(ctx, "request", &grpc.UnaryServerInfo{}, handler)

	if err == nil {
		t.Fatal("expected error when secret is wrong")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
	if handlerCalled {
		t.Error("expected handler NOT to be called when secret is wrong")
	}
}

func TestUnaryServerInterceptor_AllowsCorrectSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handler := func(ctx context.Context, req any) (any, error) {
		return "response", nil
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "correct-secret"))
	resp, err := interceptor(ctx, "request", &grpc.UnaryServerInfo{}, handler)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "response" {
		t.Errorf("expected handler's response to propagate, got %v", resp)
	}
}
