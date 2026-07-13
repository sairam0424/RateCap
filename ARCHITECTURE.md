# Architecture

RateCap faithfully recreates [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters) as a hybrid core-engine + sidecar system. v1 implements Tier 1 (the Request Rate Limiter) end-to-end; Tiers 2–4 are planned next.

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
- **`ratecap-sidecar`** (`services/sidecar/`) — a thin, co-located proxy. Apps talk to the sidecar over plain HTTP; the sidecar forwards checks to `ratecap-core` over gRPC. This is where safe-rollout (shadow mode) and priority resolution live.
- **SDKs** (`packages/sdks/go/`) — thin client stubs. No limiter logic is duplicated per language; every SDK is a wire-protocol client, nothing more. This avoids the drift risk that per-language reimplementations (e.g. independent token-bucket ports across Bucket4j/Guava/resilience4j) each accept.
- **`proto/`** — the gRPC contract (`ratecap.proto`), the single source of truth every service and SDK is generated against.

## Why a hybrid core + sidecar model

Two options were considered for distributing limiter logic across languages: a WASM-compiled shared core (single source of truth, in-process, no network hop) versus a sidecar/RPC model (like Envoy's). Research into this question found no production-proven pattern for the WASM approach at the time of designing v1, so RateCap took the sidecar/RPC path — proven by Envoy's global rate-limiting model — and deferred a possible WASM core to v2, behind a swappable interface (see below).

## Tier 1: Request Rate Limiter

Matches Stripe's reference implementation exactly:

- **Algorithm:** token bucket, keyed per API key/client identity.
- **Atomicity:** a single Redis Lua script (`services/core/store/lua/token_bucket.lua`) performs the check-and-decrement in one round-trip, avoiding the read-then-write race a naive client-side implementation would have.
- **Decision logic:** pure, in `services/core/limiter/tokenbucket.go` — no Redis import in this package. It depends on a narrow local `checker` interface rather than the concrete `store.StateStore`, so it's unit-testable with a fake and has zero network dependency in its test suite.
- **Response actions:** `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG` — a fixed 4-value enum. `REJECT_503` and a future `QUEUE` action are reserved for tiers/behaviors not yet built; v1 only emits `ALLOW`, `REJECT_429`, and `SHADOW_LOG`.

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
- `Limiter` is implemented today only by `TokenBucketLimiter`. Tiers 2–4 (concurrent-requests limiter, fleet-usage shedder, worker-utilization shedder) will each be a new `Limiter` implementation composed into a pipeline in `ratecap-core`, reusing the same gRPC/config/observability scaffolding already proven by Tier 1.

## Configuration and hot-reload

`ratecap-core` owns `ratecap.yaml` — the central engine, not the sidecar, is the source of truth, since fleet-wide state (planned for Tier 3) requires every sidecar to observe one consistent view of limits. `ratecap-core` watches the config file (`services/core/config/watcher.go`, via [fsnotify](https://github.com/fsnotify/fsnotify)) and hot-reloads without restart. `TokenBucketLimiter.Reconfigure` applies the new rate/burst/shadow-mode atomically under a `sync.RWMutex` — an earlier implementation mutated these fields without synchronization, which the race detector caught as a real concurrency bug before it shipped; see the design spec's fix-round history for the full story.

## Safe rollout: shadow mode

Every tier supports `shadow_mode`: the limiter runs its full decision logic (real cache lookups, real stats) but the result is always coerced to `ALLOW` rather than actually rejecting the request, with the would-have-rejected outcome logged. This lets an operator turn RateCap on in production and observe what it *would* do before it enforces anything — matching Envoy's confirmed production pattern for the same problem. Shadow mode is controlled per-tier via config and globally via the `RATECAP_SHADOW_MODE` environment variable, resolved in `services/sidecar/shadow/shadow.go`.

## Priority resolution (scaffolded ahead of Tier 3)

Tier 1 does not use request priority — only Tier 3 (the fleet-usage shedder, not yet built) will. The resolution mechanism is nonetheless built and tested now (`services/sidecar/proxy/priority.go`), in this fallback order:

1. Per-request `x-ratecap-priority` header (`critical` or `sheddable`)
2. Static route-config match (not yet wired into config — the header path is scaffolded)
3. A safe global default (`sheddable`) — an unset or misconfigured caller cannot accidentally mark every request critical and defeat the shedder

A characterization test (`TestServeHTTP_ParsesPriorityHeaderWithoutError`) pins today's "parsed but discarded" behavior, so a future change to make priority load-bearing can't land without deliberately updating that test.

## Testing strategy

- **Unit tests** for pure decision logic (no Redis, no network) — e.g. `TokenBucketLimiter` tested against a fake `checker`.
- **Integration tests** against a real Redis via [testcontainers-go](https://github.com/testcontainers/testcontainers-go), proving Lua-script atomicity under concurrent load (`services/core/store/redis_test.go`).
- **Race-detector runs** (`go test -race`) on every module — this is how the `Reconfigure` data race was caught.
- **End-to-end verification** via the `deploy/` docker-compose stack: a real SDK call through the real sidecar, real gRPC to a real core, hitting real Redis — proving the full chain works, not just its parts in isolation.
