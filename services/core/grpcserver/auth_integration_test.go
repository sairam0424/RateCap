package grpcserver_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/auth"
	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

func startTestServer(t *testing.T, secret string) (ratecapv1.RatecapServiceClient, func()) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(secret)))
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey))

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
	}
	return ratecapv1.NewRatecapServiceClient(conn), cleanup
}

func TestGRPCAuth_RejectsCallWithNoSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	_, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err == nil {
		t.Fatal("expected error when no shared secret is sent")
	}
}

func TestGRPCAuth_RejectsCallWithWrongSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.MetadataKey, "wrong-secret")
	_, err := client.CheckRateLimit(ctx, &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err == nil {
		t.Fatal("expected error when wrong shared secret is sent")
	}
}

func TestGRPCAuth_AllowsCallWithCorrectSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.MetadataKey, "server-secret")
	resp, err := client.CheckRateLimit(ctx, &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}
