// Package otelinit bootstraps the OpenTelemetry SDK for ratecap-core: a
// real (non-no-op) TracerProvider so context propagation is always
// exercised, plus the W3C traceparent propagator. Exporting spans anywhere
// is opt-in via RATECAP_OTEL_EXPORTER_ENDPOINT — unset, spans are created
// and ended but never leave the process.
package otelinit

import (
	"context"
	"crypto/tls"
	"net"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/grpc/credentials"
)

// isLoopbackEndpoint reports whether a "host:port" exporter endpoint's host
// is loopback. Plaintext is only ever safe for a collector on the same host
// (or reachable only via a trusted local network namespace) — anything else
// needs a verified TLS transport, since span data (service name, span
// attributes) would otherwise cross the network unencrypted.
func isLoopbackEndpoint(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Init sets the global TracerProvider and text-map propagator. serviceName
// identifies this process in exported spans (e.g. "ratecap-core"). The
// returned shutdown func must be deferred by the caller; it flushes any
// batched spans and is safe to call even when no exporter is configured.
func Init(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	endpoint := os.Getenv("RATECAP_OTEL_EXPORTER_ENDPOINT")
	if endpoint != "" {
		exporterOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
		if isLoopbackEndpoint(endpoint) {
			// A collector on the same host (or a Unix-domain-socket-style
			// loopback target) never crosses a real network boundary.
			exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
		} else {
			// Any non-loopback endpoint must use a verified TLS transport —
			// span data (service name, ratecap.priority) must never leave
			// this host over plaintext.
			exporterOpts = append(exporterOpts, otlptracegrpc.WithTLSCredentials(
				credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}),
			))
		}
		exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	// With no exporter configured, spans are still created and ended by
	// real code paths (so context propagation is exercised and testable),
	// but nothing is batched or exported anywhere — zero network calls,
	// zero new infra required to run the demo stack.

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}
