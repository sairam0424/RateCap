# Architecture

RateCap faithfully recreates [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters) as a hybrid core-engine + sidecar system. v1.0.0 implements all four tiers end-to-end; this document is updated as v2 work lands.

For the full design rationale, decision history, and research basis, see [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md). For a diagram-first view of the same material, see [`THINKING_DIAGRAM.md`](THINKING_DIAGRAM.md).

## Component overview

```text
App (any language) -> RateCap SDK (thin client) -> ratecap-sidecar (local, per-host)
                                                          |
                                                          | gRPC (only on cache-miss/sync)
                                                          v
                                                    ratecap-core (central)
                                                          |
                                                          | Lua scripts (atomic)
                                                          v
                                                        Redis
```

- **`ratecap-core`** (`services/core/`) — the central gRPC engine. Owns the limiter decision logic, the Redis-backed shared state, and hot-reloadable configuration. It is the single source of truth for what "the current rate limit" is at any moment.
- **`ratecap-sidecar`** (`services/sidecar/`) — a thin, co-located proxy. Apps talk to the sidecar over plain HTTP; the sidecar forwards checks to `ratecap-core` over gRPC, authenticated with a shared secret (`RATECAP_SHARED_SECRET`) but not encrypted — this hop must stay on a private network (see [`SECURITY.md`](SECURITY.md#network-transport-security-v1); TLS/mTLS is deferred to v2). This is where safe-rollout (shadow mode) and priority resolution live.
- **SDKs** (`packages/sdks/go/`, `packages/sdks/python/`) — thin client stubs. No limiter logic is duplicated per language; every SDK is a wire-protocol client, nothing more. This avoids the drift risk that per-language reimplementations (e.g. independent token-bucket ports across Bucket4j/Guava/resilience4j) each accept.
- **`proto/`** — the gRPC contract (`ratecap.proto`), the single source of truth every service and SDK is generated against.

## Why a hybrid core + sidecar model

Two options were considered for distributing limiter logic across languages: a WASM-compiled shared core (single source of truth, in-process, no network hop) versus a sidecar/RPC model (like Envoy's). Research into this question found no production-proven pattern for the WASM approach at the time of designing v1, so RateCap took the sidecar/RPC path — proven by Envoy's global rate-limiting model — and deferred a possible WASM core to v2, behind a swappable interface (see below).

## Tier 1: Request Rate Limiter

Matches Stripe's reference implementation exactly:

- **Algorithm:** token bucket, keyed per API key/client identity.
- **Atomicity:** a single Redis Lua script (`services/core/store/lua/token_bucket.lua`) performs the check-and-decrement in one round-trip, avoiding the read-then-write race a naive client-side implementation would have.
- **Decision logic:** pure, in `services/core/limiter/tokenbucket.go` — no Redis import in this package. It depends on a narrow local `checker` interface rather than the concrete `store.StateStore`, so it's unit-testable with a fake and has zero network dependency in its test suite.
- **Response actions:** `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`, `QUEUE` — a 5-value enum (v1 shipped the first 4; `QUEUE` was added in v2 Phase 3, see below). `REJECT_503` remains reserved for `FleetShedder`'s shed path; Tier 1 itself only ever emits `ALLOW`, `REJECT_429`, and `SHADOW_LOG`.

## Swappable interfaces (why v2 doesn't require a rewrite)

Two interfaces are deliberately abstracted so later work can extend the system without touching what's already built:

```go
// services/core/store/store.go
type StateStore interface {
    CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
}

// services/core/limiter/limiter.go
type Limiter interface {
    Check(ctx context.Context, req Request) (Decision, error)
}
```

- `StateStore` is implemented today only by `RedisStore` (Lua/Redis). A future etcd- or in-memory-backed store can implement the same interface without changing limiter logic.
- `Limiter` is implemented by `TokenBucketLimiter` (Tier 1), `ConcurrencyLimiter` (Tier 2), and `FleetShedder` (Tier 3), each composed into a pipeline in `ratecap-core`. Tier 4 (the worker-utilization shedder) is deliberately sidecar-local, not a `Limiter` — see `services/sidecar/worker/shedder.go`.

## Tier 2 bounded queueing (v2 Phase 3)

`ConcurrencyLimiter` optionally queues a request that finds the concurrency cap full, instead of instantly rejecting it. This is off by default (`queueing_enabled: false`) — enabling it is an explicit per-deployment opt-in with no change to existing behavior otherwise.

When enabled, a request that finds the cap full first tries to acquire a slot in a bounded local semaphore (`max_backlog`). If the semaphore is full, the request is rejected immediately, exactly like today's non-queueing behavior — queueing never makes rejection *more* likely, only adds a bounded chance of eventual success. If a slot is acquired, the request polls the existing, unmodified `IncrConcurrent` Redis Lua script every `poll_interval_ms` until it succeeds, `max_queue_wait_ms` elapses, or the request's context is canceled.

**This backlog is per-`ratecap-core`-instance, not fleet-wide.** Each core instance enforces its own `max_backlog` independently; there is no cross-instance coordination of queue depth. Worst-case total backlog across a fleet of N core instances is `max_backlog × N`, not a single coordinated ceiling. This mirrors Tier 4's already-accepted local-only worker shedder (`services/sidecar/worker/shedder.go`) — RateCap already has this exact category of precedent, and it is stated here deliberately rather than left implicit.

No ordering (LIFO/FIFO) is imposed on waiters — with independent polling goroutines, "who gets served first" is naturally whichever waiter's poll happens to succeed first. A queued-then-served request is fully transparent to the client: it returns a plain `200`, with the `QUEUE` action existing only for server-side attribution (feeding `ratecap_decisions_total{tier="concurrency_limiter",action="queue"}` and structured decision logs, where the elevated `latency_ms` already makes queueing visible without a dedicated wire field).

## Configuration and hot-reload

`ratecap-core` owns `ratecap.yaml` — the central engine, not the sidecar, is the source of truth, since fleet-wide state (planned for Tier 3) requires every sidecar to observe one consistent view of limits. `ratecap-core` watches the config file (`services/core/config/watcher.go`, via [fsnotify](https://github.com/fsnotify/fsnotify)) and hot-reloads without restart. `TokenBucketLimiter.Reconfigure` applies the new rate/burst/shadow-mode atomically under a `sync.RWMutex` — an earlier implementation mutated these fields without synchronization, which the race detector caught as a real concurrency bug before it shipped; see the design spec's fix-round history for the full story.

## Safe rollout: shadow mode

Every tier supports `shadow_mode`: the limiter runs its full decision logic (real cache lookups, real stats) but the result is always coerced to `ALLOW` rather than actually rejecting the request, with the would-have-rejected outcome logged. This lets an operator turn RateCap on in production and observe what it *would* do before it enforces anything — matching Envoy's confirmed production pattern for the same problem. Shadow mode is controlled per-tier via config and globally via the `RATECAP_SHADOW_MODE` environment variable, resolved in `services/sidecar/shadow/shadow.go`.

## Priority resolution

Tier 1 does not use request priority — only Tier 3 (the fleet-usage shedder) does, splitting its effective cap between `critical` and `sheddable` traffic (`services/core/limiter/fleetshedder.go`). The resolution mechanism lives in `services/sidecar/proxy/priority.go`, in this fallback order:

1. Per-request `x-ratecap-priority` header (`critical` or `sheddable`)
2. Static route-config match — not implemented; deferred as a fast-follow (see the Tier 3 design spec)
3. A safe global default (`sheddable`), currently hardcoded to `proxy.Sheddable` in `services/sidecar/main.go` with no operator-configurable override — an unset or misconfigured caller cannot accidentally mark every request critical and defeat the shedder

On the wire, `Priority`'s proto zero-value is `PRIORITY_UNSPECIFIED` (0), distinct from `SHEDDABLE` (1) — a caller that never sets the field is now distinguishable, at the wire level, from one that explicitly requests `sheddable`. Both map to the same safe `limiter.Sheddable` default server-side (`services/core/grpcserver/server.go`'s priority-conversion switch), so this is purely a correctness/observability improvement, not a behavior change: a caller-side bug (forgetting to set priority) no longer looks byte-for-byte identical to intentional sheddable traffic.

## Testing strategy

- **Unit tests** for pure decision logic (no Redis, no network) — e.g. `TokenBucketLimiter` tested against a fake `checker`.
- **Integration tests** against a real Redis via [testcontainers-go](https://github.com/testcontainers/testcontainers-go), proving Lua-script atomicity under concurrent load (`services/core/integrationtests/store/redis_test.go`, in the separate `services/core/integrationtests` module).
- **Race-detector runs** (`go test -race`) on every module — this is how the `Reconfigure` data race was caught.
- **End-to-end verification** via the `deploy/` docker-compose stack: a real SDK call through the real sidecar, real gRPC to a real core, hitting real Redis — proving the full chain works, not just its parts in isolation.

## Observability

### Metrics

`ratecap-sidecar` exposes `/metrics` (Prometheus format) on its main listen address, on a path that deliberately bypasses the sidecar's own self-throttle limiter (`newTopMux` in `services/sidecar/main.go`) — a Prometheus scrape must never compete with real `/check`/`/release` traffic for the same rate-limit budget, especially during the overload event an operator most needs visibility into. `ratecap-core` exposes `/metrics` on a separate listener (`RATECAP_METRICS_ADDR`, default `:9092`), distinct from both its main gRPC port (`:9090`) and its plaintext health port (`:9091`).

| Metric | Emitted by | Labels | Meaning |
| --- | --- | --- | --- |
| `ratecap_decisions_total` | sidecar | `tier`, `action` | Every `/check` decision, by the tier that produced it and the resulting action (`allow`, `reject_429`, `reject_503`, `shadow_log`, `queue`). `queue` is emitted only when Tier 2's bounded queueing (`queueing_enabled`) is on and a request waits for a slot rather than being rejected immediately. |
| `ratecap_shadow_would_reject_total` | sidecar | `tier` | Decisions that would have rejected/shed but were coerced to `allow` by shadow mode. |
| `ratecap_worker_inflight_requests` | sidecar | — | Current Tier 4 (worker shedder) in-flight count on this sidecar instance. |
| `ratecap_decision_latency_seconds` | sidecar | `tier` | End-to-end `/check` latency histogram, labeled by the tier that produced the final decision. |
| `ratecap_release_total` | sidecar | `result` | `/release` call outcomes (`success`/`failure`). |
| `ratecap_upstream_errors_total` | sidecar | `endpoint` | Failed gRPC calls from sidecar to core (`check_rate_limit`, `release_concurrency`). |
| `ratecap_core_grpc_requests_total` | core | `method`, `code` | Every *authenticated* gRPC request core serves, by RPC name and resulting `google.golang.org/grpc/codes` status string. The metrics interceptor is chained after the auth interceptor (see `main.go`), so a rejected shared-secret attempt is never counted here. |
| `ratecap_core_grpc_request_duration_seconds` | core | `method` | gRPC request latency histogram. |
| `ratecap_core_redis_call_duration_seconds` | core | `operation` | Redis call latency histogram (`check_and_decrement`, `incr_concurrent`, `decr_concurrent`). |
| `ratecap_core_redis_errors_total` | core | `operation` | Failed Redis calls. |
| `ratecap_core_config_reload_total` | core | `result` | Config hot-reload attempts (`success`/`failure`) — a `failure` covers both a malformed YAML file and a well-formed-but-invalid config caught by `Config.Validate()`. |
| `ratecap_fail_open_total` | core | `tier`, `reason` | Requests allowed through via fail-open after a tier's store call errored. **Only Tier 1 (`rate_limiter`) currently fails open** — Tiers 2/3 fail closed by design; see "Redis-down degradation contract" below. |

A starter Grafana dashboard is at `deploy/grafana/ratecap-overview.json`; baseline alert rules are at `deploy/grafana/ratecap-alerts.yml`.

### Redis-down degradation contract

When a tier's backing Redis call errors (timeout, connection refused, or any other `StateStore` error):

- **Tier 1 (Request Rate Limiter) fails OPEN.** The request is allowed and `ratecap_fail_open_total{tier="rate_limiter",reason="store_error"}` is incremented. This matches Stripe's own documented precedent of failing open on request-rate limiting specifically.
- **Tier 2 (Concurrent Requests Limiter) and Tier 3 (Fleet Usage Load Shedder) fail CLOSED.** The gRPC call returns `codes.Internal`, which the sidecar surfaces as an HTTP 500 to the caller. Both tiers exist specifically to bound concurrent resource usage; failing open would remove that bound during exactly the outage when it matters most.
- This is a **per-tier** contract, not a whole-pipeline guarantee: `Pipeline.Check` still runs every tier in order, so a total Redis outage still surfaces as a 500 overall once the request reaches Tier 2 or Tier 3, even though Tier 1 itself failed open along the way.

Enforced by real network-fault-injection tests (Toxiproxy, not mocks) in `services/core/integrationtests/reliability/redis_failure_test.go` — `TestTier1_RedisUnavailable_FailsOpen`, `TestTier2_RedisUnavailable_FailsClosed`, `TestTier3_RedisUnavailable_FailsClosed`.

### Health checks

- `ratecap-sidecar`'s `/healthz` reflects the real gRPC connectivity state to `ratecap-core` (healthy unless the connection is in `TRANSIENT_FAILURE` or `SHUTDOWN`), and — like `/metrics` — bypasses the sidecar's self-throttle limiter.
- `ratecap-core`'s gRPC health service (`:9091`, plaintext, unauthenticated — deliberately outside mTLS, since Kubernetes' native gRPC probe action has no TLS/client-cert support, so a probe against the mTLS-enforcing main port would always fail once mTLS is enabled; see [SECURITY.md](SECURITY.md#network-transport-security-v1) for the broader transport-security model) reflects real Redis connectivity, re-checked every 5 seconds via a background ping loop, rather than being set once at startup and never updated.

### Tracing

Distributed tracing covers exactly one hop: `ratecap-sidecar`'s gRPC call to `ratecap-core`'s `RatecapService` (both the plaintext listener and the permissive-mode TLS listener — not the health-check gRPC server on `:9091`, which carries no application RPCs worth tracing). Both services bootstrap a real (non-no-op) `sdktrace.TracerProvider` and register the W3C `traceparent` propagator at startup (`services/core/otelinit`, `services/sidecar/otelinit`), so context always propagates correctly on the wire — independent of whether anything is actually exported.

- **Off by default, zero new infra required.** `RATECAP_OTEL_EXPORTER_ENDPOINT` (both services) is unset by default: spans are still created and ended by real code on every `CheckRateLimit` call, but the `TracerProvider` has no exporter attached, so nothing is batched or sent anywhere — no network calls, no Collector/Jaeger/Tempo dependency, and the `deploy/docker-compose.yml` demo stack's `/healthz` and sample endpoints behave identically to before this feature existed. Setting the env var to a `host:port` configures a real `otlptracegrpc` exporter (gRPC, insecure by default) batched via `sdktrace.WithBatcher`.
- **Wiring.** `otelgrpc.NewServerHandler()` is attached as a `grpc.StatsHandler` on both of core's `RatecapService` listeners; `otelgrpc.NewClientHandler()` is attached the same way on the sidecar's single `grpc.NewClient` to core (`services/sidecar/main.go`). Because otelgrpc's client stats handler creates its own per-RPC span internally and never hands the span-bearing context back to caller code, `services/sidecar/proxy/proxy.go`'s `ServeHTTP` starts its own explicit `SpanKindClient` span (`ratecap.sidecar.check_rate_limit`) immediately before calling `CheckRateLimit`; that span becomes otelgrpc's own span's parent, so a single trace ID still links the sidecar's hop to core's server span end-to-end.
- **`ratecap.priority` attribute.** Both the sidecar's client span and core's server span (`services/core/grpcserver/server.go`'s `CheckRateLimit`) carry a `ratecap.priority` attribute set to `"sheddable"` or `"critical"` — the same two labels `priorityLabel` already uses for the `ratecap_decisions_total` metric above, mirrored on core's side by `priorityAttrLabel` so both hops read identically.
- **Never the request key.** `req.Key`, any HTTP header, and any query parameter are never set as a span attribute or as baggage anywhere in this instrumentation — only the two fixed, small-cardinality priority strings above. This matches the same key-exclusion pattern the Metrics table already follows (`ratecap_decisions_total` etc. are labeled by `tier`/`action`, never by the raw key) — telemetry that can leave the process via OTLP export gets the same treatment as the metrics already do, for the same reason: the key is an arbitrary caller-controlled string (a user ID, an API key, anything), and exporting it would be a real data-exposure surface. Verified by a real in-memory-exporter integration test (`services/core/grpcserver/otel_integration_test.go`) asserting the recorded client and server spans share one trace ID with a parent-child relationship, and that `ratecap.priority` is present and correct for both a `SHEDDABLE` and a `CRITICAL` request.

### Known limitations

- Tracing does not extend past the sidecar→core hop: no span wraps the Redis/Lua-script calls core makes underneath `CheckRateLimit`, and there is no further hop to propagate to beyond core. The health-check gRPC server on `:9091` and the caller→sidecar HTTP leg are also not instrumented (out of scope — see the Tracing section above).
- The Python SDK's PyPI publish is pending: `.github/workflows/publish-python-sdk.yml` and its PyPI Trusted Publisher (OIDC) setup are ready, but no `python-sdk-v*` tag has been pushed yet — see [CONTRIBUTING.md](CONTRIBUTING.md#releasing-the-python-sdk-to-pypi-one-time-setup) for the one-time setup a repo admin must complete first.

### Tier 4 shed-curve ramping

`worker.Shedder` ramps gradually rather than cutting off hard at its cap: below `RATECAP_SHED_RAMP_START_PCT` (default 100 — i.e. no ramp, matching pre-v2.6.0 behavior) of `RATECAP_MAX_INFLIGHT_REQUESTS`, every request is admitted; within the ramp window, rejection probability increases linearly to 100% exactly at the cap. This avoids the binary-on/off flapping failure mode Stripe's own load shedders are documented to have hit.

## mTLS migration path (v2.7.0)

Flipping mTLS from off to fully enforced in one step is a flag day — any sidecar without a cert yet goes down the instant the switch flips. `RATECAP_TLS_MODE` adds the middle rung every mature service mesh (Istio, Linkerd) ships for exactly this reason:

| `RATECAP_TLS_MODE` | `services/core` behavior |
| --- | --- |
| unset / `off` (default) | Unchanged from pre-v2.7.0: plaintext-only on `:9090`, unless `RATECAP_TLS_CERT_PATH`/`KEY_PATH`/`CA_PATH` happen to be set, in which case the single listener becomes TLS-only (`RequireAndVerifyClientCert`) — this is the same implicit behavior that existed before this env var did. |
| `permissive` | The plaintext listener on `:9090` keeps running unchanged. A **second** listener (`RATECAP_GRPC_TLS_ADDR`, default `:9443`) is added, with `ClientAuth: VerifyClientCertIfGiven` — a sidecar without a cert can still connect (server-authenticated only); a sidecar with a cert gets it verified. Sidecars migrate one at a time by pointing `RATECAP_CORE_ADDR` at the TLS port once they have certs. |
| `strict` | Same as the implicit "certs set" behavior above, now reachable via an explicit, self-documenting mode string: single listener, TLS-only, `RequireAndVerifyClientCert`. |

Both `permissive` and `strict` require the TLS cert env vars to be set — core fails closed at startup otherwise.

**Recommended rollout sequence** (mirrors Istio's/Linkerd's own documented migration path — do not skip a step):
1. Ship with `off` as the default (this release does — no shipped default changes).
2. Once every sidecar in a fleet is capable of connecting with a cert, an operator sets `RATECAP_TLS_MODE=permissive` on core and migrates sidecars one at a time.
3. Watch `ratecap_core_connection_security_total{transport="plaintext"}` (see the Observability section) drop to zero across a full deploy cycle — this is the "is anything still on plaintext" signal a strict cutover needs before it's safe.
4. Only once that metric is confirmed zero does an operator flip to `RATECAP_TLS_MODE=strict`.

RateCap does not flip any of this automatically or by default — every transition above is an explicit operator action.

### Certificate SAN/hostname preflight

`ratecapctl tls check <cert-path> <expected-host>` verifies a certificate's SAN list covers the hostname it will actually serve/dial *before* deploying it — catching the exact "demo certs' SAN (core/sidecar) don't match a real Helm release name" failure mode `deploy/helm/ratecap/values.yaml` already documents as producing no server-side log.

### Certificate hot-reload

Both services now watch their cert/key files via the same `fsnotify` library already used for `ratecap.yaml`'s config hot-reload, and swap in the reloaded certificate without a restart — an externally-rotated cert (e.g. cert-manager) takes effect automatically. The CA pool is loaded once at startup and is NOT hot-reloaded; CA rotation still requires a restart.

## Token-cost wiring (v2.8.0)

Tier 1 has always been a generic variable-cost token bucket internally (`token_bucket.lua`, `RedisStore.CheckAndDecrement`, and `CheckRateLimitRequest.cost` all supported arbitrary cost since v1) — the sidecar simply hardcoded `Cost: 1` on every call. `/check?cost=N` now wires this through (default `1`, unchanged for any caller that doesn't pass it).

### Reserve-upfront, refund-unused

Mirroring the AWS Bedrock/LiteLLM pattern for LLM token accounting: a caller can pass a conservative cost estimate (e.g. `EstimateLLMCost(input_tokens, max_tokens)`) to `/check`, then once the real usage is known, refund the unused portion via `/release` with `X-RateCap-Refund-Key`/`X-RateCap-Refund-Amount`. This is a *new*, separate bounded Lua script (`refund_tokens.lua`) that clamps to `burst` on write — it is NOT implemented as `CheckAndDecrement` with a negative cost, since `token_bucket.lua`'s refill arithmetic only re-clamps to `burst` on the *next* call, which would let a large refund transiently exceed `burst` and stay there until some unrelated future call happened to correct it.

A refund and a Tier 2 concurrency release are independent and can be combined in one `/release` call — see `services/sidecar/proxy.ReleaseHandler`.

### IETF rate-limit headers (partial)

`RateLimit-Reset` (seconds, IETF `draft-ietf-httpapi-ratelimit-headers`) is emitted on `429` responses, computed from the existing `RetryAfterMs`. `limit` and `remaining` are **not** emitted — `ratecap-core` does not track or return a per-key remaining-token count today; adding that is a proto change (new Lua script return value, new `CheckRateLimitResponse` field) that belongs in its own future spec, not folded silently into this partial step.

### Negative cache

The sidecar keeps a local, in-memory cache of recently-denied identifiers (`services/sidecar/negativecache`), keyed by the same `RetryAfterMs` value the real decision already returned — a repeat request for a key still within its own denial window short-circuits before ever calling core, a cheap p99 win under a sustained abuse flood. This is complementary to, not a replacement for, Tier 4's worker shedder: the cache only ever short-circuits a key core has *already* rejected once; it never makes an independent shedding decision of its own.
