package grpcserver_test

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

// startOTelTestServer wires otelgrpc's stats handlers onto both ends of a
// bufconn-backed gRPC connection using an explicit TracerProvider/propagator
// pair (rather than mutating otel's process-wide globals), so this test
// stays hermetic regardless of what else runs in this package.
func startOTelTestServer(t *testing.T, tp *sdktrace.TracerProvider) (ratecapv1.RatecapServiceClient, func()) {
	propagator := propagation.TraceContext{}
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(tp),
			otelgrpc.WithPropagators(propagator),
		)),
	)
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, &fakeRateLimiter{}, &fakeFleetShedder{}, &fakeRefundStore{}, testSigningKey))

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(tp),
			otelgrpc.WithPropagators(propagator),
		)),
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

func TestOTelPropagation_ClientAndServerSpansShareTraceID(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Fatalf("tracer provider shutdown failed: %v", err)
		}
	}()

	client, cleanup := startOTelTestServer(t, tp)
	defer cleanup()

	// No auth interceptor is wired in this test server (Task 2 is only
	// about the stats handlers, not auth), so an unauthenticated call is
	// fine here — auth propagation is already covered by
	// auth_integration_test.go.
	_, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush failed: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected exactly 2 spans (client + server), got %d: %+v", len(spans), spans)
	}

	traceID := spans[0].SpanContext.TraceID()
	for _, s := range spans {
		if s.SpanContext.TraceID() != traceID {
			t.Errorf("expected all spans to share trace ID %s, got %s for span %q", traceID, s.SpanContext.TraceID(), s.Name)
		}
	}

	var clientSpan, serverSpan *tracetest.SpanStub
	for i := range spans {
		switch spans[i].SpanKind {
		case trace.SpanKindClient:
			clientSpan = &spans[i]
		case trace.SpanKindServer:
			serverSpan = &spans[i]
		}
	}
	if clientSpan == nil || serverSpan == nil {
		t.Fatalf("expected one client span and one server span, got: %+v", spans)
	}

	if serverSpan.Parent.SpanID() != clientSpan.SpanContext.SpanID() {
		t.Errorf("expected server span's parent (%s) to be the client span (%s)", serverSpan.Parent.SpanID(), clientSpan.SpanContext.SpanID())
	}
	if !serverSpan.Parent.IsValid() {
		t.Error("expected server span to have a valid parent span context (propagated over the wire)")
	}
}
