# OpenTelemetry Trace-Context Propagation — Implementation Plan

## Background

`docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 1 item 9, explicitly named this the largest item in Phase 1 and pre-approved it slipping to a later phase: *"Zero tracing exists anywhere in the repo today; this is the only layer that would let a Tier 3 shedding incident be debugged with correlated sidecar+core visibility instead of unstructured, uncorrelated log lines."* Phase 5's own item list never re-listed it, so it was never picked up. This plan implements it now as its own addendum — version bump **2.9.0 → 2.10.0** (minor: new instrumentation, zero behavior change to any request-path decision).

Confirmed by grepping the current tree: zero `go.opentelemetry.io/*` packages are a direct dependency of `services/core` or `services/sidecar` today (the ones observed transitively in `services/core/go.mod` before Phase 5 came only from `testcontainers-go`, which Phase 5 Task 7 moved into `services/core/integrationtests` — they're gone from `services/core/go.mod` now). This is a genuine from-scratch addition, not wiring up something half-done.

## Global Constraints

1. **Anti-bypass, empirical verification, no fabricated data** — identical to every prior phase in this repo's history. Never bypass a denied git/CI operation; actually run `gofmt -l .`, `go build ./...`, `go test ./... -race` in every module touched; bring up the real demo stack and confirm `/healthz` before calling anything done.
2. **New dependencies are pre-approved for this plan only, and scoped exactly to these packages** (the roadmap item itself names this instrumentation): `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`. Do not add anything beyond this set without stopping and reporting.
3. **Zero new required infrastructure.** No OTel Collector, Jaeger, or Tempo exists in `deploy/docker-compose.yml` today, and this plan does not add one. Tracing must be safe and correct with **no backend configured** — the SDK's `TracerProvider` is real (not a no-op) so context propagates correctly end-to-end regardless, but spans simply have nowhere to export to until an operator sets the new env var below. The demo stack must come up and pass `/healthz`/the sample endpoints exactly as today with zero env changes.
4. **New env var: `RATECAP_OTEL_EXPORTER_ENDPOINT`** (both `services/core` and `services/sidecar`, consistent with this repo's existing `RATECAP_*` env-var convention for every other opt-in feature — TLS mode, pprof, admin secret). Empty/unset (default): tracing SDK is still initialized and context still propagates via gRPC metadata, but exports go to `sdktrace.NewTracerProvider` with no exporter attached (spans are created and end, but never sent anywhere — no network calls, no behavior difference). Non-empty: configure a real `otlptracegrpc` exporter (gRPC, insecure by default — matching how little else in this repo assumes a fully-hardened observability backend yet) pointed at that endpoint, batched via `sdktrace.WithBatcher`.
5. **Security: never put the caller-controlled `key` value (or any header/query-param value a caller controls) into a span attribute or into baggage.** It can be an arbitrary string a caller chooses (a user ID, an API key, anything) — putting it in telemetry that may leave the process via OTLP export is a real data-exposure surface this repo has correctly avoided everywhere else (see `SECURITY.md`'s existing stance on decision logs). Only put fixed, small-cardinality values on spans: `ratecap.priority` (`"sheddable"`/`"critical"`), `ratecap.tier`, `ratecap.action`. This is a hard constraint, not a style preference — a reviewer must explicitly check for it on every task in this plan.
6. **Health-check gRPC servers are out of scope.** `services/core/main.go`'s `healthGRPCServer := grpc.NewServer()` (line 350) carries no application RPCs worth tracing — do not instrument it. Only the two real `RatecapService` servers (plaintext, and the permissive-mode TLS listener) and the sidecar's single `grpc.NewClient` to core are in scope.
7. **`ARCHITECTURE.md` already has an "Observability" section** (added in Phase 1) — extend it, don't duplicate it.

---

## Task 1 — OTel SDK bootstrap in both services

**Goal:** a shared, minimal `TracerProvider` construction path in each service, gated by `RATECAP_OTEL_EXPORTER_ENDPOINT`, with the W3C `traceparent` propagator actually registered (the SDK's default global propagator is a no-op — without explicitly setting it, nothing would ever actually be written to or read from gRPC metadata regardless of how the interceptors are wired).

**Files:** `services/core/otelinit/otelinit.go` (new package, mirrors the existing per-concern-package layout like `tlsconfig`, `coremetrics`), `services/core/otelinit/otelinit_test.go`, `services/sidecar/otelinit/otelinit.go`, `services/sidecar/otelinit/otelinit_test.go`, `services/core/go.mod`, `services/sidecar/go.mod`, `services/core/main.go`, `services/sidecar/main.go`.

**Design:**
```go
package otelinit

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init sets the global TracerProvider and propagator. serviceName is
// "ratecap-core" or "ratecap-sidecar". The returned shutdown func must be
// deferred by the caller; it flushes any batched spans and is safe to call
// even when no exporter is configured.
func Init(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	endpoint := os.Getenv("RATECAP_OTEL_EXPORTER_ENDPOINT")
	if endpoint != "" {
		exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	// With no exporter configured, spans are created and ended by real code
	// paths (so context propagation is exercised and testable) but never
	// batched/exported anywhere — zero network calls, zero new infra
	// required to run the demo stack.

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}
```
- In each `main.go`, call `shutdown, err := otelinit.Init(ctx, "ratecap-core")` (or `"ratecap-sidecar"`) near the top of `main()`, `log.Fatalf` on error (matching every other startup-failure convention in this repo), and `defer shutdown(context.Background())`.
- `go get` the four packages named in Global Constraint 2 into both `services/core/go.mod` and `services/sidecar/go.mod`; `go mod tidy` each.

**Tests:** `otelinit_test.go` in each package — call `Init` with the env var unset, confirm no error and `otel.GetTracerProvider()` is not the default no-op provider; call it with the env var set to an unreachable address (e.g. `localhost:1`), confirm `Init` itself still succeeds (the exporter is created lazily/async — `otlptracegrpc.New` doesn't dial eagerly, matching the exact "doesn't dial eagerly" pattern this repo's own `healthz_main_test.go` already documents for `grpc.NewClient`) and nothing blocks or panics.

---

## Task 2 — Instrument every real gRPC client/server construction site

**Goal:** wire `otelgrpc`'s stats handlers onto the two real `RatecapService` gRPC servers in `services/core/main.go` and the sidecar's one gRPC client — this is what actually makes spans get created per-RPC and `traceparent` get read/written on the wire.

**Files:** `services/core/main.go`, `services/sidecar/main.go`.

**Design:**
- `otelgrpc.NewServerHandler()` returns a `grpc.StatsHandler` — append `grpc.StatsHandler(otelgrpc.NewServerHandler())` to `serverOpts` (line ~278) and to `tlsServerOpts` (the permissive-mode TLS listener, line ~312-314). Do **not** touch `healthGRPCServer := grpc.NewServer()` (line 350) — out of scope per Global Constraint 6.
- `otelgrpc.NewClientHandler()` returns the client-side equivalent — add `grpc.WithStatsHandler(otelgrpc.NewClientHandler())` to the `grpc.NewClient(...)` call in `services/sidecar/main.go` (~line 207), alongside the existing `grpc.WithTransportCredentials(...)` and `grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret))` — stats handlers and unary interceptors are independent dial options and compose without conflict; verify this by actually building and running the sidecar against core afterward, not by assuming it from the API shape.
- No change to `services/core/grpcserver/server.go`'s handler bodies in this task — that's Task 3.

**Tests:** an integration-style test (can live alongside the existing `services/core/grpcserver/auth_integration_test.go`/`mtls_integration_test.go` pattern, or a new `otel_integration_test.go`) that starts a real core `grpc.NewServer` with the stats handler wired, a real client with its stats handler wired, both under a `sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))` (the OTel SDK's own `go.opentelemetry.io/otel/sdk/trace/tracetest` in-memory exporter — a test-only dependency, does not need go.mod production requires beyond what Task 1 already added since it's part of `otel/sdk`), makes one real `CheckRateLimit` call, and asserts the in-memory exporter recorded **two spans sharing the same trace ID** (client span + server span) with a parent-child relationship — this is the actual, empirical proof that propagation works across the real wire, not an assumption from reading the otelgrpc docs.

---

## Task 3 — Priority attribute on both spans; enforce the no-key-in-telemetry rule

**Goal:** satisfy the roadmap item's specific ask — "baggage carrying the sidecar's resolved priority classification onto both the sidecar's CLIENT span and core's SERVER span" — implemented as a span attribute (simpler and sufficient for the stated debugging goal; real W3C baggage would additionally propagate the value to further downstream hops, which don't exist here since core has no further hop).

**Files:** `services/sidecar/proxy/proxy.go`, `services/core/grpcserver/server.go`, plus tests extending Task 2's integration test.

**Design:**
- In `proxy.go`'s `ServeHTTP`, immediately before the `h.client.CheckRateLimit(...)` call (after `priority`/`protoPriority` are already resolved, ~line 126): `trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("ratecap.priority", priorityLabel(priority)))`. `trace.SpanFromContext` on a context whose span isn't recording (e.g. no exporter path, or tracing genuinely off) is always safe and a documented no-op — confirm this by reading `go.opentelemetry.io/otel/trace`'s own contract, not by assuming.
- In `services/core/grpcserver/server.go`'s `CheckRateLimit` (line 76), immediately after decoding `req.Priority`: same pattern, `trace.SpanFromContext(ctx).SetAttributes(attribute.String("ratecap.priority", ...))`, mapping `ratecapv1.Priority_CRITICAL`/`SHEDDABLE` to the same two label strings the sidecar already uses (`priorityLabel` in `services/sidecar/proxy/proxy.go` — mirror the labels, don't invent new ones).
- **Explicitly do not** add `req.Key`, any header value, or any query-parameter value to a span attribute anywhere in this task or any other. A reviewer must grep the diff for `SetAttributes` calls and manually confirm every value is one of the fixed small-cardinality strings named in Global Constraint 5.

**Tests:** extend Task 2's integration test to also assert the recorded client span and server span each carry `ratecap.priority` with the correct value for both a `SHEDDABLE` and a `CRITICAL` request — two sub-cases, not one.

---

## Task 4 — Documentation

**Goal:** make the new capability discoverable and its zero-infra-required default explicit.

**Files:** `ARCHITECTURE.md` (extend the existing Observability section), `CHANGELOG.md`, `VERSION`.

**Steps:**
1. Add a "Tracing" subsection under `ARCHITECTURE.md`'s existing Observability section: what's instrumented (sidecar→core gRPC hop only), the `RATECAP_OTEL_EXPORTER_ENDPOINT` env var and its off-by-default/no-new-infra-required behavior, the `ratecap.priority` span attribute, and the explicit statement that request keys are never included in span data (cross-reference `SECURITY.md`'s existing decision-log stance if it makes a similar statement).
2. Bump `VERSION` to `2.10.0`.
3. Add a `## [2.10.0]` `CHANGELOG.md` entry above `[2.9.0]`, explicitly noting this is the roadmap's originally-deferred Phase 1 stretch item, finally implemented.
