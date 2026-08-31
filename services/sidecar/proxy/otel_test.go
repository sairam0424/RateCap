package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/worker"
)

// withTestTracerProvider installs a real, in-memory-exporting TracerProvider
// as the process-wide global for the duration of the test, restoring
// whatever was previously installed on cleanup. ServeHTTP calls
// otel.Tracer(...) (the global), matching how services/sidecar/main.go
// wires the real one via otelinit.Init — so exercising the global here,
// rather than an explicit provider, is what actually proves production
// behavior, not a substitute for it.
func withTestTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Fatalf("tracer provider shutdown failed: %v", err)
		}
		otel.SetTracerProvider(prev)
	})
	return exporter
}

func TestServeHTTP_SetsRatecapPrioritySpanAttribute(t *testing.T) {
	tests := []struct {
		name         string
		header       string
		defaultPrio  proxy.Priority
		wantPriority string
	}{
		{name: "sheddable", header: "", defaultPrio: proxy.Sheddable, wantPriority: "sheddable"},
		{name: "critical", header: "critical", defaultPrio: proxy.Sheddable, wantPriority: "critical"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exporter := withTestTracerProvider(t)

			client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
			h := proxy.NewHandler(client, tc.defaultPrio, worker.NewShedder(1000))

			req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
			if tc.header != "" {
				req.Header.Set("x-ratecap-priority", tc.header)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if client.lastCtx == nil {
				t.Fatal("expected CheckRateLimit to be called with a non-nil context")
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("expected exactly 1 span, got %d: %+v", len(spans), spans)
			}
			span := spans[0]

			if span.SpanKind != trace.SpanKindClient {
				t.Errorf("expected span kind Client, got %s", span.SpanKind)
			}

			// The span passed into CheckRateLimit's context must be the same
			// span that was exported — proving the attribute really landed on
			// the span that accompanies the outbound RPC, not an unrelated one.
			ctxSpan := trace.SpanFromContext(client.lastCtx)
			if ctxSpan.SpanContext().SpanID() != span.SpanContext.SpanID() {
				t.Fatalf("span passed to CheckRateLimit (%s) is not the exported span (%s)",
					ctxSpan.SpanContext().SpanID(), span.SpanContext.SpanID())
			}

			found := false
			for _, kv := range span.Attributes {
				if string(kv.Key) != "ratecap.priority" {
					continue
				}
				found = true
				if kv.Value.AsString() != tc.wantPriority {
					t.Errorf("ratecap.priority = %q, want %q", kv.Value.AsString(), tc.wantPriority)
				}
			}
			if !found {
				t.Error("span is missing the ratecap.priority attribute")
			}

			// Global Constraint 5: the caller-controlled key must never leave
			// the process via a span attribute.
			for _, kv := range span.Attributes {
				if kv.Value.AsString() == "user-1" {
					t.Errorf("found caller-controlled key value on span attribute %s — this must never happen", kv.Key)
				}
			}
		})
	}
}
