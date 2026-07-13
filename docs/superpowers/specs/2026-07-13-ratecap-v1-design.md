# RateCap v1 — Design Spec

**Date:** 2026-07-13
**Status:** Approved
**Research basis:** Two deep-research passes (213 subagents, ~15.6M tokens combined), adversarial verification against primary sources; naming/registry availability confirmed via direct API checks (npm, crates.io, PyPI, GitHub, RDAP).

---

## Problem

Stripe's 2017 engineering blog post ["Scaling your API with rate limiters"](https://stripe.com/blog/rate-limiters) describes four layered mechanisms Stripe runs in production to protect its API from overload: a request rate limiter, a concurrent-requests limiter, a fleet usage load shedder, and a worker utilization load shedder. Research confirmed no existing open-source system (Envoy, Kong, Cloudflare, AWS API Gateway, redis-cell, resilience4j, Alibaba Sentinel, Istio, Netflix concurrency-limits, Doorman, Unkey) unifies all four tiers the way Stripe does — each covers at most one or two. This is genuine, confirmed whitespace.

RateCap is a new open-source project that faithfully recreates all four tiers as a reusable, fully configurable, production-grade system — both as a learning exercise in distributed rate-limiting architecture and as something other teams can adopt.

## Research Findings (load-bearing facts)

Verified with high confidence against primary sources (Stripe's blog + companion Redis-Lua gist by the post's author):

1. **Tier 1 — Request Rate Limiter**: token bucket per API key, made atomic via a Redis Lua script (check-and-decrement in one round-trip).
2. **Tier 2 — Concurrent Requests Limiter**: bounds *simultaneous in-flight requests* (not RPS) per user, default cap ~20. Implemented via a Redis sorted set — a random token is `ZADD`ed at request start, `ZREM`ed at completion, `ZCARD` gives the live count. Exists specifically to stop retry storms from overwhelming CPU-intensive endpoints. This is a fundamentally different data structure/problem than tier 1 (bounding concurrency, not flow).
3. **Tier 3 — Fleet Usage Load Shedder**: traffic is split critical/non-critical by API method (e.g. 80/20 reserved-capacity split). Implementation is *literally identical to tier 2*, just swapping the per-user key for a global key — fleet health here is approximated by a concurrency count, not literal CPU/GC-pause telemetry. Sheds non-critical requests with `503` once the reserved-capacity threshold is exceeded.
4. **Tier 4 — Worker Utilization Load Shedder**: the least-documented tier in public sources; the final line of defense at the process/worker-pool level.

Two premises from the original research brief were investigated and refuted, which shaped decisions below:
- YouTube's Doorman is **not** gossip-based — it's centralized-master with a hierarchical leaf/region/root tree; etcd/ZooKeeper are used only for master election, never counter state.
- Unkey's production rate limiter is **Go**, not Rust+Redis — sliding-window + atomic counters, lock-free/wait-free, no mutexes in the hot path, no gRPC exposure. This is real, current (2026) precedent for Go fitting exactly this workload class.

Go-vs-Rust head-to-head P99.9 benchmarks and a WASM-shared-core production precedent were both investigated across two research passes and **did not survive adversarial verification in either direction** — these are open engineering questions, not settled facts, and the architecture below is designed to not require settling them for v1.

## Naming

**RateCap** — verified clean via direct registry queries (not research-agent claims, which failed twice on this specific check): npm (`registry.npmjs.org/ratecap` → 404, available), crates.io (available), PyPI (available), GitHub org/user `ratecap` (available), `.dev` domain (confirmed via Google's RDAP endpoint, `pubapi.registry.google/rdap/domain/ratecap.dev` → 404 not-found, i.e. available). Only `ratecap.com` is parked/unavailable, which is not a blocker for OSS infra projects (Envoy, Consul, etcd, Vector all shipped without owning their exact `.com`).

## Architecture

### Component overview

```
                     ┌─────────────────────────┐
   App (any lang) ── │  RateCap SDK (thin       │
                     │  gRPC/HTTP client stub)  │
                     └───────────┬─────────────┘
                                 │ localhost, ~0.1-0.5ms
                     ┌───────────▼─────────────┐
                     │  ratecap-sidecar (Go)    │  <- co-located per host
                     │  - local enforcement     │
                     │  - priority resolution   │
                     │  - config cache          │
                     └───────────┬─────────────┘
                                 │ gRPC (only on cache-miss/sync)
                     ┌───────────▼─────────────┐
                     │  ratecap-core (Go)       │  <- central, holds shared state
                     │  - all 4 limiter tiers   │
                     │  - hot-reload config     │
                     └───────────┬─────────────┘
                                 │ Lua scripts (atomic)
                     ┌───────────▼─────────────┐
                     │  Redis                   │
                     │  - tier1: token bucket    │
                     │  - tier2/3: sorted sets   │
                     └─────────────────────────┘
```

### Key architectural decisions

**Hybrid core-engine + sidecar model, all Go for v1.** SDKs are thin gRPC/HTTP client stubs only — no per-language reimplementation of limiter logic (avoiding the drift risk that Bucket4j/Guava/resilience4j each live with independently, each maintaining its own token-bucket implementation). All decision logic lives once, in `ratecap-core`, behind a clean `Limiter` interface. This resolves the SDK-distribution question (WASM-core vs sidecar vs per-language reimplementation) that research left unresolved — sidecar/RPC-only sidesteps the unresolved WASM question entirely rather than betting v1 on an unproven pattern.

**Rust is deferred to v2, not forced into v1.** With SDKs reduced to thin stubs, there's no natural v1 slot for Rust (the WASM-shared-core use case that would justify it is exactly the risky, unproven pattern being avoided). The `Limiter` interface in `ratecap-core` is designed to be swappable so a Rust/WASM decision-logic engine can be substituted for v2's true in-process enforcement mode without a rewrite of the surrounding gRPC/config/observability scaffolding.

**Redis + Lua, faithful to Stripe.** Tier 1 uses a Lua token-bucket script; tiers 2-3 use sorted sets (ZADD/ZCARD/ZREM), tier 3 with a global key instead of per-user. This matches the "faithful recreation" v1 scope exactly. The storage layer sits behind a `StateStore` Go interface so etcd, in-memory (dev/test), or other backends can be added later without touching limiter logic:

```go
type StateStore interface {
    CheckAndDecrement(ctx context.Context, key string, tokens int) (bool, error)
    IncrConcurrent(ctx context.Context, key string) (count int, err error)
    DecrConcurrent(ctx context.Context, key string) error
}
```

## The Four Tiers (v1 scope)

Each tier implements a shared interface so `ratecap-core` composes them into a pipeline without tier-specific glue code in the sidecar:

```go
type Request struct {
    Key  string
    Cost int
}

type Limiter interface {
    Check(ctx context.Context, req Request) (Decision, error)
}
```

`Request` carries `key` as a field rather than a separate parameter, and is passed by value — this matches the flat `CheckRateLimitRequest{key, cost}` shape already fixed by the Tier-1 gRPC contract (`proto/ratecap/v1/ratecap.proto`), so the gRPC-to-`Limiter` mapping in `ratecap-core` stays a straight field copy with no restructuring at the seam.

| Tier | Mechanism | Storage | Default action on trip |
|---|---|---|---|
| 1. Request Rate Limiter | Token bucket per API key, atomic Lua script | Redis key | `429` |
| 2. Concurrent Requests Limiter | Sorted set (ZADD/ZCARD/ZREM), per-user in-flight cap (default 20) | Redis sorted set | `429` |
| 3. Fleet Usage Load Shedder | Same sorted-set mechanism as tier 2, global key, critical/non-critical split (default 80/20) | Redis sorted set (global) | `503` |
| 4. Worker Utilization Load Shedder | Local process-level check (goroutine pool / queue depth threshold) — no Redis round-trip | In-memory (sidecar-local) | `503` |

**Pipeline order:** tier 1 → tier 2 → tier 3 → tier 4 (cheapest/most specific check first; a request short-circuits at the first tier that rejects it).

**Response actions:**

```go
type Action int
const (
    ALLOW       Action = iota
    REJECT_429         // tier 1, tier 2
    REJECT_503         // tier 3, tier 4
    SHADOW_LOG         // any tier in shadow_mode
    // QUEUE is deliberately NOT in v1 — see Explicitly Deferred to v2
)
```

**Shadow mode:** every tier independently supports a per-tier `shadow_mode` config key (full evaluation — real cache lookups, real stats — but the result is always coerced to `ALLOW`, with the would-have-rejected outcome logged/metriced), plus a global `RATECAP_SHADOW_MODE` env var override, matching Envoy's confirmed production pattern. This is the safe-rollout mechanism: operators can turn RateCap on in production and observe what it *would* do before it enforces anything.

## Priority / Criticality Tagging

Stripe's blog references a critical/non-critical traffic split for tier 3 but never shows the actual mechanism. RateCap resolves priority in this order:

1. Per-request override: `x-ratecap-priority: critical|sheddable` header (HTTP) or gRPC metadata field, checked by the sidecar first.
2. Static route-config match: `critical_routes` list in `ratecap.yaml` (route/method pattern).
3. Global `default_priority` (safe default: `sheddable` — an unset/misconfigured caller cannot accidentally mark everything critical and defeat the shedder).

## Configuration

`ratecap.yaml` is the source of truth and is loaded by **`ratecap-core`** (the central engine owns config, since tiers 2-4's global/fleet-wide state depends on every sidecar sharing one consistent view of limits). `ratecap-core` watches the file for changes and hot-reloads without restart — swapping the in-memory config atomically with no dropped requests mid-reload. Each `ratecap-sidecar` pulls its local-enforcement config (tier 4 thresholds, priority defaults) from `ratecap-core` at startup and on the `sync_rate` interval, caching it locally so a transient core outage doesn't stall local checks. (A future v2 could move config storage to etcd for multi-core-instance consistency; out of scope for v1's single-core-instance deployment model.)

```yaml
sync_rate: 5              # Kong-style: 0=sync every check, -1=local-only, N=seconds (20ms floor)

tiers:
  rate_limiter:
    default_rate: 100      # tokens/sec
    default_burst: 500
    shadow_mode: false
    per_key_overrides:
      "acct_premium_123": { rate: 1000, burst: 5000 }

  concurrency_limiter:
    default_max_concurrent: 20
    shadow_mode: false

  fleet_shedder:
    reserved_critical_pct: 20
    default_priority: sheddable
    critical_routes:
      - "POST /v1/charges"
      - "POST /v1/payment_intents"

  worker_shedder:
    max_queue_depth: 1000
    max_worker_utilization_pct: 90
```

## Observability

Prometheus + OpenTelemetry metrics, matching what research found operators actually dashboard on:

- `ratecap_utilization_ratio{tier, key}` — current usage vs. limit
- `ratecap_shed_total{tier, reason}` — counter of shed requests
- `ratecap_limit_hit_total{tier, key}` — counter of 429/503 responses
- `ratecap_shadow_would_reject_total{tier, key}` — shadow-mode signal

A Grafana dashboard JSON pre-wired to these metric names ships in `deploy/grafana/`.

## Directory Structure

Following this workspace's established polyglot-project convention (Tombstone, Graph-Forge: `go.work` + `services/` + `packages/sdks/`):

```
RateCap/
├── CLAUDE.md
├── go.work                      # use ./services/core, ./services/sidecar, ./cli
├── services/
│   ├── core/                    # ratecap-core: gRPC engine, all 4 tiers, StateStore interface
│   │   ├── limiter/             #   tier1_rate/, tier2_concurrency/, tier3_fleet/, tier4_worker/
│   │   ├── store/                #   redis.go (StateStore impl), lua/ (*.lua scripts)
│   │   └── config/               #   hot-reload watcher
│   └── sidecar/                 # ratecap-sidecar: local proxy, priority resolution, shadow-mode
├── packages/
│   └── sdks/
│       └── go/                  # first SDK: generated gRPC client stub + ergonomic wrapper
├── cli/                          # ratecapctl: config validate, live-tail decisions, benchmark runner
├── proto/                        # ratecap.proto — gRPC contract, source of truth for all SDKs
├── deploy/
│   ├── docker-compose.yml       # one-command demo: core+sidecar+Redis+sample app
│   └── grafana/                 # dashboard JSON
└── docs/
    └── superpowers/specs/       # this spec, future specs
```

## Walking Skeleton (build order)

Proves every architectural seam (gRPC contract, sidecar-to-core, core-to-Redis, config hot-reload, shadow-mode) using only tier 1, before tiers 2-4 reuse the proven plumbing:

1. `proto/ratecap.proto` — gRPC contract for tier 1 only
2. `services/core` — tier 1 (token bucket + Lua/Redis) + gRPC server + hot-reload config
3. `services/sidecar` — local proxy, calls core, shadow-mode toggle, priority header parsing
4. `packages/sdks/go` — thin client wrapping the generated stub
5. `deploy/docker-compose.yml` — wire core+sidecar+Redis+a trivial sample Go app; prove a real request gets rate-limited end-to-end
6. Only after step 5 works end-to-end: add tiers 2, 3, 4 to `services/core`, each reusing the same gRPC/sidecar/config/observability plumbing

## Testing Strategy

- **Unit tests per tier's decision logic**, pure (no Redis): assert Stripe-equivalent semantics — e.g. tier 1 allows exactly N requests in a burst window, tier 2 rejects the 21st concurrent request.
- **Integration tests against real Redis** (testcontainers): prove Lua-script atomicity holds under concurrent load.
- **Benchmark suite** (`ratecapctl bench`): hammers the sidecar, produces the P99/P999 latency numbers published in the README — this is the concrete "beast mode = build quality" proof point, not a scope addition.

## Explicitly Deferred to v2

To keep v1 a faithful, shippable recreation of Stripe's exact four mechanisms:

- **Bounded queueing** (hold requests briefly instead of instant reject, à la Alibaba Sentinel's Rate Limiter mode). The `Action` enum is designed so adding `QUEUE` later is additive, not a breaking change.
- **Rust/WASM decision-logic core** for true zero-network-hop in-process enforcement. The `Limiter` interface is designed to be swappable so this can be substituted later without touching the gRPC/config/observability scaffolding.
- **Additional storage backends** (etcd, in-memory) behind the existing `StateStore` interface.

## Out of Scope (not planned for any version)

- Any 5th limiting mechanism beyond Stripe's four tiers.
- Multi-region/cross-datacenter state reconciliation (Stripe's public blog doesn't describe this, and research found no verified pattern worth copying — Unkey's approach here was investigated and several specific claims about it were refuted on verification).
