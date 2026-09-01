package grpcserver_test

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/sairam0424/RateCap/proto/ratecap/v1"

	"github.com/sairam0424/RateCap/services/core/grpcserver"
	"github.com/sairam0424/RateCap/services/core/limiter"
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
		conn.Close() //nolint:gosec,errcheck // test-only bufconn client conn; process/test is tearing down either way
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

// spanAttr reads a single attribute value off a recorded span, returning
// ("", false) if it's absent.
func spanAttr(s tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestOTelPropagation_PriorityAttributeOnBothSpans covers Task 3: the
// sidecar's resolved priority must land on both the outbound client-side
// span and core's inbound server span, for both priority values. otelgrpc's
// client stats handler creates its own per-RPC span deep inside
// grpc.ClientConn.Invoke and never returns that context to caller code
// (confirmed by reading otelgrpc's clientHandler.TagRPC), so the caller
// (services/sidecar/proxy/proxy.go, mirrored here) must start its own
// client-kind span around the call to have anywhere real to put the
// attribute — this test proves that span, otelgrpc's own nested client
// span, and core's server span all still share one trace ID end-to-end.
func TestOTelPropagation_PriorityAttributeOnBothSpans(t *testing.T) {
	tests := []struct {
		name     string
		priority ratecapv1.Priority
		want     string
	}{
		{name: "sheddable", priority: ratecapv1.Priority_SHEDDABLE, want: "sheddable"},
		{name: "critical", priority: ratecapv1.Priority_CRITICAL, want: "critical"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			defer func() {
				if err := tp.Shutdown(context.Background()); err != nil {
					t.Fatalf("tracer provider shutdown failed: %v", err)
				}
			}()

			client, cleanup := startOTelTestServer(t, tp)
			defer cleanup()

			callCtx, span := tp.Tracer("test").Start(
				context.Background(), "ratecap.sidecar.check_rate_limit", trace.WithSpanKind(trace.SpanKindClient),
			)
			span.SetAttributes(attribute.String("ratecap.priority", tc.want))

			_, err := client.CheckRateLimit(callCtx, &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1, Priority: tc.priority})
			span.End()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if err := tp.ForceFlush(context.Background()); err != nil {
				t.Fatalf("force flush failed: %v", err)
			}

			spans := exporter.GetSpans()
			// our wrapping client span + otelgrpc's own client span + the server span
			if len(spans) != 3 {
				t.Fatalf("expected exactly 3 spans, got %d: %+v", len(spans), spans)
			}

			traceID := spans[0].SpanContext.TraceID()
			var wrapperSpan, serverSpan *tracetest.SpanStub
			for i := range spans {
				if spans[i].SpanContext.TraceID() != traceID {
					t.Errorf("expected all spans to share trace ID %s, got %s for span %q", traceID, spans[i].SpanContext.TraceID(), spans[i].Name)
				}
				switch {
				case spans[i].Name == "ratecap.sidecar.check_rate_limit":
					wrapperSpan = &spans[i]
				case spans[i].SpanKind == trace.SpanKindServer:
					serverSpan = &spans[i]
				}
			}
			if wrapperSpan == nil {
				t.Fatalf("expected to find the wrapping client span, got: %+v", spans)
			}
			if serverSpan == nil {
				t.Fatalf("expected to find the server span, got: %+v", spans)
			}

			if got, ok := spanAttr(*wrapperSpan, "ratecap.priority"); !ok || got != tc.want {
				t.Errorf("client span ratecap.priority = %q (present=%v), want %q", got, ok, tc.want)
			}
			if got, ok := spanAttr(*serverSpan, "ratecap.priority"); !ok || got != tc.want {
				t.Errorf("server span ratecap.priority = %q (present=%v), want %q", got, ok, tc.want)
			}

			// Global Constraint 5: the caller-controlled key must never leave
			// the process via a span attribute, on either span.
			for _, s := range []*tracetest.SpanStub{wrapperSpan, serverSpan} {
				for _, kv := range s.Attributes {
					if kv.Value.AsString() == "user-1" {
						t.Errorf("span %q: found caller-controlled key value on attribute %s", s.Name, kv.Key)
					}
				}
			}
		})
	}
}
