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
- **SDKs** (`packages/sdks/go/`) — thin client stubs. No limiter logic is duplicated per language; every SDK is a wire-protocol client, nothing more. This avoids the drift risk that per-language reimplementations (e.g. independent token-bucket ports across Bucket4j/Guava/resilience4j) each accept.
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

## Priority resolution (scaffolded ahead of Tier 3)

Tier 1 does not use request priority — only Tier 3 (the fleet-usage shedder) does. The resolution mechanism lives in `services/sidecar/proxy/priority.go`, in this fallback order:

1. Per-request `x-ratecap-priority` header (`critical` or `sheddable`)
2. Static route-config match (not yet wired into config — the header path is scaffolded)
3. A safe global default (`sheddable`) — an unset or misconfigured caller cannot accidentally mark every request critical and defeat the shedder

A characterization test (`TestServeHTTP_ParsesPriorityHeaderWithoutError`) pins today's "parsed but discarded" behavior, so a future change to make priority load-bearing can't land without deliberately updating that test.

## Testing strategy

- **Unit tests** for pure decision logic (no Redis, no network) — e.g. `TokenBucketLimiter` tested against a fake `checker`.
- **Integration tests** against a real Redis via [testcontainers-go](https://github.com/testcontainers/testcontainers-go), proving Lua-script atomicity under concurrent load (`services/core/store/redis_test.go`).
- **Race-detector runs** (`go test -race`) on every module — this is how the `Reconfigure` data race was caught.
- **End-to-end verification** via the `deploy/` docker-compose stack: a real SDK call through the real sidecar, real gRPC to a real core, hitting real Redis — proving the full chain works, not just its parts in isolation.
