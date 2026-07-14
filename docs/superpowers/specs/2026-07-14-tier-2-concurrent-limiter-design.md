# Tier 2 — Concurrent Requests Limiter — Design Spec

**Date:** 2026-07-14
**Status:** Approved
**Context:** The next scoped piece of RateCap's v1 design after the Tier 1 walking skeleton (merged to `main`, live at github.com/sairam0424/RateCap). See [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](2026-07-13-ratecap-v1-design.md) for the overall v1 design and [`ARCHITECTURE.md`](../../../ARCHITECTURE.md) for the current system overview.

---

## Problem

Per the v1 design spec's tier table:

> Tier 2. Concurrent Requests Limiter | Sorted set (ZADD/ZCARD/ZREM), per-user in-flight cap (default 20) | Redis sorted set | `429` default action on trip

Stripe's confirmed mechanism (from the original research): bounds *simultaneous in-flight requests* (not RPS) per user. A random token is `ZADD`ed at request start, `ZREM`ed at completion, `ZCARD` gives the live concurrency count. This exists specifically to stop retry storms from overwhelming CPU-intensive endpoints — a fundamentally different problem than Tier 1's flow-rate bounding.

**What Tier 1 didn't need that Tier 2 does:** Tier 1 is a single stateless check-and-decrement — a token bucket doesn't need to know when a request finishes. Tier 2 requires tracking request *lifecycle*: a slot is acquired at request start and must be released at request end, or reaped if the caller never releases (crash, timeout, network partition). This is the central new problem this phase solves, and every downstream decision in this spec follows from it.

## Key Design Decisions

### 1. SDK: new `Acquire()`/`Ticket` API, `Allow()` untouched

The existing SDK method `Allow(ctx, key) (allowed bool, retryAfterMs int64, err error)` is a single fire-and-forget call, already used by the shipped `deploy/sampleapp` demo. It cannot express acquire+release, and must not change — no breaking change to already-shipped code.

A new method is added alongside it:

```go
ticket, err := client.Acquire(ctx, "user-1")
if err != nil { /* handle */ }
defer ticket.Release(ctx)

if !ticket.Allowed {
    // rejected by tier 1 (rate) or tier 2 (concurrency) — Ticket.RetryAfterMs is set
    return
}
// do the work; slot releases via defer
```

`Ticket` carries `Allowed bool`, `RetryAfterMs int64`, and a `Release(ctx context.Context) error` method. Callers who only need Tier 1 keep using `Allow()`; callers who want Tier 2's protection use `Acquire()`/`Ticket`.

**Release is best-effort, no retry.** If `Release()` fails (network blip between app and sidecar, or sidecar and core), the caller gets the error back to log or ignore. The slot self-heals via the reaper (see below) within `max_request_duration_ms` regardless of whether `Release()` ran — this matches Stripe's own design, where reaping IS the resilience mechanism for lost releases, not a backstop for a separate retry system. Adding SDK-side retry logic would be scope creep beyond what Stripe's reference design does.

### 2. Wire contract: token-correlated acquire/release

`proto/ratecap/v1/ratecap.proto` changes:

```protobuf
service RatecapService {
  rpc CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse);
  rpc ReleaseConcurrency(ReleaseConcurrencyRequest) returns (ReleaseConcurrencyResponse);
}

message CheckRateLimitRequest {
  string key = 1;
  int32 cost = 2;
}

message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  string concurrency_token = 3;
}

message ReleaseConcurrencyRequest {
  string key = 1;
  string concurrency_token = 2;
}

message ReleaseConcurrencyResponse {}
```

`concurrency_token` is empty when no tier reserved a releasable slot (e.g. Tier 1 already rejected before Tier 2 ran — nothing to release). The sidecar's HTTP layer mirrors this: `GET /check?key=...` returns the token in a response header (alongside the existing `Retry-After-Ms` header pattern); a new `POST /release?key=...&token=...` endpoint calls `ReleaseConcurrency`.

### 3. StateStore extension

```go
type StateStore interface {
    CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
    IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (allowed bool, token string, err error)
    DecrConcurrent(ctx context.Context, key, token string) error
}
```

`IncrConcurrent`'s Lua script (new file, `services/core/store/lua/concurrent_limiter.lua`), atomic like Tier 1's script:

1. `ZREMRANGEBYSCORE key -inf (now - maxDurationMs)` — reap stale entries (self-healing; no separate background reaper process).
2. `ZCARD key` — count live entries after reaping.
3. If count < cap: generate a random token, `ZADD key now token`, return `allowed=true, token`.
4. Else: return `allowed=false, token=""`.

The sorted set's **score** is the acquire timestamp (enables step 1's reaping); the **member** is a random token (enables exact-match `ZREM` on release — this is why the token must be returned to the caller and threaded through `Release()`). `DecrConcurrent` is a plain `ZREM key token` — no Lua/atomicity needed since it removes one already-known member.

### 4. Limiter pipeline

Tier 1's `Limiter.Check(ctx, Request) (Decision, error)` interface is extended, not replaced:

```go
type Decision struct {
    Action       Action
    RetryAfterMs int64
    Token        string // non-empty if this tier reserved a slot the caller must Release
}

type Limiter interface {
    Check(ctx context.Context, req Request) (Decision, error)
}
```

A new `Pipeline` type composes an ordered list of `Limiter`s and short-circuits on the first non-`ALLOW`:

```go
type Pipeline struct {
    tiers []Limiter
}

func NewPipeline(tiers ...Limiter) *Pipeline

func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
    final := Decision{Action: ALLOW}
    for _, tier := range p.tiers {
        d, err := tier.Check(ctx, req)
        if err != nil || d.Action != ALLOW {
            return d, err
        }
        if d.Token != "" {
            final.Token = d.Token // last non-empty token wins; only one tier (2) issues one in this phase
        }
    }
    return final, nil
}
```

Only Tier 2 issues a token in this phase, so `Decision.Token` being a single field (not a slice) is sufficient scope for now. If a future tier (3 or 4) also needs a releasable reservation, `Decision`/`Ticket` will need to carry multiple tokens — deliberately not designed here since neither tier's actual mechanism is confirmed yet (see Out of Scope).

`ratecap-core`'s `grpcserver.Server` now holds a `*limiter.Pipeline`, not a single `limiter.Limiter` — this is the seam Tiers 3 and 4 extend later by appending to the `tiers` slice, with zero changes to `grpcserver` itself. `TokenBucketLimiter` (Tier 1) is unchanged; a new `ConcurrencyLimiter` (Tier 2) implements the same `Limiter` interface, wrapping `StateStore.IncrConcurrent`/`DecrConcurrent`.

**Release routing:** `grpcserver.Server.ReleaseConcurrency` calls `DecrConcurrent` directly (not through the pipeline — release is a targeted cleanup of one tier's reservation, not a re-run of the whole check sequence).

### 5. Config

```yaml
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 20
    max_request_duration_ms: 30000
    shadow_mode: false
```

Mirrors Tier 1's existing shape exactly (`default_max_concurrent` in place of `default_rate`/`default_burst`, plus `max_request_duration_ms` for the reap cutoff). Hot-reloadable via the same `Reconfigure` pattern Tier 1 already uses — `ConcurrencyLimiter` gets its own `Reconfigure(maxConcurrent int, maxDurationMs int64, shadowMode bool)` method, guarded by the same `sync.RWMutex` pattern `TokenBucketLimiter.Reconfigure` established (see the v1 design spec's fix-round history for why this matters — an earlier unsynchronized version of this exact pattern was a real, race-detector-caught data race).

### 6. Shadow mode

`ConcurrencyLimiter` supports `shadow_mode` exactly as `TokenBucketLimiter` does: on a would-be reject, still reserve the slot (so concurrency accounting stays accurate) but return `SHADOW_LOG` instead of `REJECT_429`, coerced the same way the sidecar's existing `shadow.CoerceIfShadowOverridden` already handles for Tier 1.

## Build Order

Following the walking-skeleton's proven per-layer task granularity:

1. Extend `proto/ratecap/v1/ratecap.proto` — `concurrency_token` field, `ReleaseConcurrency` RPC.
2. `StateStore.IncrConcurrent`/`DecrConcurrent` + `concurrent_limiter.lua`, with a testcontainers integration test proving reap-then-count-then-add atomicity under concurrent load (mirroring Tier 1's `TestCheckAndDecrement_ConcurrentAtomicity`).
3. `ConcurrencyLimiter` implementing `Limiter`, pure unit-tested against a fake store (mirroring `TokenBucketLimiter`'s test pattern).
4. `Pipeline` composing `[]Limiter`, unit-tested with fake tiers proving short-circuit-on-first-reject and token pass-through.
5. Wire `Pipeline` into `grpcserver.Server` (replacing the single-`Limiter` field), add the `ReleaseConcurrency` RPC handler.
6. Extend `config.Config` with `ConcurrencyLimiterConfig`, wire hot-reload for both tiers' `Reconfigure` calls in `services/core/main.go`.
7. Sidecar: `/check` response includes the token (header), new `/release` endpoint calling `ReleaseConcurrency`.
8. SDK: `Acquire()`/`Ticket.Release()`, unit-tested against a fake sidecar HTTP server (mirroring `Allow()`'s existing test pattern).
9. Update `deploy/ratecap.yaml` with a `concurrency_limiter` block, update the sample app to demonstrate Tier 2 (e.g. a slow endpoint holding its ticket for a few seconds under concurrent load), re-verify the full docker-compose stack end-to-end — proving BOTH tiers now trigger under the right conditions, not just Tier 1.

## Testing Strategy

- Unit tests for `ConcurrencyLimiter` (pure, fake store) and `Pipeline` (pure, fake tiers) — no Redis, no network.
- Integration test against real Redis (testcontainers) proving the reap-count-add Lua script is atomic under concurrent load, and proving reaping actually evicts stale entries after `maxDurationMs`.
- End-to-end docker-compose verification, extended beyond Tier 1's existing 5×200-then-429 check to also demonstrate Tier 2 tripping (e.g. N+1 concurrent slow requests where N = `default_max_concurrent`).

## Out of Scope (this phase)

- Tiers 3 and 4 (Fleet Usage Load Shedder, Worker Utilization Load Shedder) — separate future phases.
- Priority/criticality tagging — Tier 3's concern, not Tier 2's. `services/sidecar/proxy/priority.go` remains scaffolded-but-unused.
- Release retry/resilience machinery beyond the reaper — an explicit trade-off accepted above, not a gap to close later without a documented reason.
- Any change to `Allow()`'s existing signature or behavior.
