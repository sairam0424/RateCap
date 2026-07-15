package auth_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/ratecap/sidecar/auth"
)

func TestUnaryClientInterceptor_AttachesSecretToOutgoingMetadata(t *testing.T) {
	interceptor := auth.UnaryClientInterceptor("my-secret")
	var capturedCtx context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err := interceptor(context.Background(), "/ratecap.v1.RatecapService/CheckRateLimit", "request", "reply", nil, invoker)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	if !ok {
		t.Fatal("expected outgoing metadata to be set")
	}
	values := md.Get(auth.MetadataKey)
	if len(values) != 1 || values[0] != "my-secret" {
		t.Errorf("expected metadata key %q to carry %q, got %v", auth.MetadataKey, "my-secret", values)
	}
}
