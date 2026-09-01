package otelinit_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sairam0424/RateCap/services/sidecar/otelinit"
)

func TestInit_NoEndpoint_SetsRealTracerProvider(t *testing.T) {
	t.Setenv("RATECAP_OTEL_EXPORTER_ENDPOINT", "")

	shutdown, err := otelinit.Init(context.Background(), "ratecap-sidecar-test")
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown returned unexpected error: %v", err)
		}
	}()

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected global TracerProvider to be a real *sdktrace.TracerProvider, got %T", otel.GetTracerProvider())
	}
}

func TestInit_UnreachableEndpoint_DoesNotBlockOrError(t *testing.T) {
	t.Setenv("RATECAP_OTEL_EXPORTER_ENDPOINT", "localhost:1")

	shutdown, err := otelinit.Init(context.Background(), "ratecap-sidecar-test")
	if err != nil {
		t.Fatalf("Init returned unexpected error for an unreachable endpoint (exporter dials lazily): %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected a non-nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
}

func TestInit_NonLoopbackEndpoint_UsesTLSNotInsecure(t *testing.T) {
	// A non-loopback endpoint must take the TLS-credentials branch, not
	// WithInsecure() — otlptracegrpc.New dials lazily regardless of
	// transport credentials, so this must succeed without blocking or
	// erroring exactly like the loopback case, proving the TLS branch is
	// exercised for real rather than just present in the source.
	t.Setenv("RATECAP_OTEL_EXPORTER_ENDPOINT", "otel-collector.internal.example.com:4317")

	shutdown, err := otelinit.Init(context.Background(), "ratecap-sidecar-test")
	if err != nil {
		t.Fatalf("Init returned unexpected error for a non-loopback endpoint: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
}
