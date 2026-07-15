# Tier 3 — Fleet Usage Load Shedder — Design Spec

**Date:** 2026-07-15
**Status:** Approved
**Context:** The next scoped piece of the v1 design spec after Tier 2 (merged to `develop`, PR #4, commit `28837f9`, including both post-implementation audit-remediation groups). See [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](2026-07-13-ratecap-v1-design.md) for the overall v1 design and [`docs/superpowers/specs/2026-07-14-tier-2-concurrent-limiter-design.md`](2026-07-14-tier-2-concurrent-limiter-design.md) for Tier 2's design, whose plumbing this phase reuses directly.

---

## Problem

Per the v1 design spec's tier table:

> Tier 3. Fleet Usage Load Shedder | Same sorted-set mechanism as tier 2, global key, critical/non-critical split (default 80/20) | Redis sorted set (global) | `503` default action on trip

Stripe's confirmed mechanism: traffic is split critical/non-critical, and non-critical (sheddable) traffic is shed with `503` once a *reserved-capacity* threshold is exceeded — critical traffic keeps room reserved for it. Mechanically this is the same ZADD/ZCARD/ZREM/reap sorted-set lifecycle Tier 2 already implements, with two differences: the key is **global** (one fleet-wide count, not per-user), and the **effective cap depends on request priority** rather than being a single fixed number.

**What Tier 1/2 didn't need that Tier 3 does:** every prior tier keys off `req.Key` and treats every request identically regardless of caller-declared priority. Tier 3 introduces both a fixed key (ignoring `req.Key`) and priority-dependent decision logic for the first time — and it's also the first tier to make `services/sidecar/proxy/priority.go`'s already-built `ResolvePriority()` load-bearing rather than parsed-and-discarded.

## Why This Phase Should Be (Mostly) Additive

Tier 2's post-implementation audit found two guaranteed-trigger architecture bugs specifically because Tier 3 was coming: `Pipeline.Check` dropping an earlier tier's reserved token on a later tier's rejection, and `ReleaseConcurrency` hardcoding `req.Key` as the release key. Both were fixed by replacing the single `Decision.Token string` field with `Decision.Reservations []TokenReservation`, where each reservation **self-describes the key it was reserved against** — precisely so a tier using a different key scheme (Tier 3's global key) wouldn't break release routing. That fix is already merged and already proven correct for exactly this case.

This means Tier 3 does **not** need to touch `services/core/limiter/limiter.go`, `pipeline.go`, or the `Reservations`/release-routing logic in `services/core/grpcserver/server.go` — that seam is done. `ReleaseConcurrency` continues to call `DecrConcurrent(ctx, req.Key, req.ConcurrencyToken)` unchanged; Tier 3's reservations simply carry `Key: "fleet"` instead of `Key: req.Key`, and the existing generic release path handles it correctly with zero core-logic changes.

## Key Design Decisions

### 1. `FleetShedder`: a new `Limiter` implementing priority-dependent, globally-keyed concurrency

`services/core/limiter/fleetshedder.go`, structurally a sibling to `ConcurrencyLimiter` (same `concurrencyChecker` local interface, same `sync.RWMutex`-guarded `Reconfigure`/`Check` pattern — this is the same atomic-hot-reload requirement that caught a real data race earlier in this project, so every new `Limiter` follows the pattern from day one):

```go
const fleetKey = "fleet"

type FleetShedder struct {
    store concurrencyChecker

    mu                sync.RWMutex
    cap               int
    reservedCriticalPct int
    maxDurationMs     int64
    shadowMode        bool
}

func (l *FleetShedder) Check(ctx context.Context, req Request) (Decision, error) {
    if req.SkipReservations {
        return Decision{Action: ALLOW}, nil
    }

    l.mu.RLock()
    cap, pct, maxDurationMs, shadowMode := l.cap, l.reservedCriticalPct, l.maxDurationMs, l.shadowMode
    l.mu.RUnlock()

    effectiveCap := cap
    if req.Priority != Critical {
        effectiveCap = cap * (100 - pct) / 100
    }

    allowed, token, err := l.store.IncrConcurrent(ctx, fleetKey, effectiveCap, maxDurationMs)
    if err != nil {
        return Decision{}, err
    }
    if allowed {
        return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: fleetKey, Token: token}}}, nil
    }
    if shadowMode {
        _, reservedToken, err := l.store.IncrConcurrent(ctx, fleetKey, unboundedCap, maxDurationMs)
        if err != nil {
            return Decision{}, err
        }
        return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: fleetKey, Token: reservedToken}}}, nil
    }
    return Decision{Action: REJECT_503}, nil
}
```

Two deliberate departures from `ConcurrencyLimiter`:
- **Fixed key, not `req.Key`.** This is the literal "swap the per-user key for a global key" from the spec — `fleetKey` is an unexported constant, not configurable in v1 (no known use case for a configurable fleet-key name; YAGNI).
- **Priority-dependent effective cap.** Both priorities increment the *same* Redis sorted set (`cc:fleet` — one global count), but a `Sheddable` request is checked against a smaller effective cap than a `Critical` one. This is the actual reserved-capacity split: critical traffic is never literally unbounded (it's still capped at the full fleet cap, so a critical-traffic storm still gets a ceiling), but it always has room a large volume of sheddable traffic can't consume.
- **Rejection action is `REJECT_503`, not `REJECT_429`.** This is the first tier to ever emit `REJECT_503` — the proto enum value and the sidecar's response-switch (`case ratecapv1.Action_REJECT_503: w.WriteHeader(http.StatusServiceUnavailable)`) both already exist and require no changes.

`unboundedCap` (already defined in `concurrency.go` for `ConcurrencyLimiter`'s shadow-mode reservation trick) is reused as-is — no duplication, same constant, same rationale.

### 2. `Request` gains a `Priority` field; the `Priority` enum moves to a shared location

`services/sidecar/proxy/priority.go` currently defines `Priority`/`Sheddable`/`Critical`/`ResolvePriority` as sidecar-local — but priority is now a cross-service concept (sidecar resolves it, core's `FleetShedder` decides on it). The enum moves to `services/core/limiter/limiter.go` (alongside `Action`, `Decision`, `Request`):

```go
type Priority int

const (
    Sheddable Priority = iota
    Critical
)

type Request struct {
    Key              string
    Cost             int
    SkipReservations bool
    Priority         Priority
}
```

`services/sidecar/proxy/priority.go` keeps `ResolvePriority(headerValue string, defaultPriority Priority) Priority` (pure header-parsing logic belongs in the sidecar, which is where the header lives), but now imports `limiter.Priority`/`limiter.Sheddable`/`limiter.Critical` from core instead of defining its own copy. `CheckRateLimitRequest` in the proto gains a matching `Priority priority = 4` field (a proto enum mirroring `Sheddable`/`Critical`, following the same pattern `Action` already uses).

Every tier before `FleetShedder` in the pipeline (`TokenBucketLimiter`, `ConcurrencyLimiter`) ignores `Request.Priority` entirely — no change to either file's decision logic.

### 3. `skip_concurrency_limit` → `skip_reservations` (rename, not addition)

`Allow()`'s existing tier-2 bypass (`SkipConcurrencyLimit`) has the exact same problem re-emerging for Tier 3: `Allow()` is fire-and-forget with no `Release()` call, so if `FleetShedder` ever reserved a global slot on an `Allow()` call, that slot would leak until the reaper's `max_request_duration_ms` window expired — the identical regression Tier 2 hit during its own end-to-end verification.

Rather than add a second flag (`skip_fleet_limit` alongside `skip_concurrency_limit`), this phase renames the existing field to `skip_reservations` and has *both* `ConcurrencyLimiter` and `FleetShedder` check it identically. This is a one-time, complete fix: v1 has exactly two reservation-issuing tiers, forever — Tier 4 (Worker Utilization Load Shedder) is an in-process, no-Redis-round-trip check per the v1 spec, with no reservation lifecycle at all. There is no future tier that will need a third flag. This isn't speculative generalization; it's recognizing v1's actual, already-fixed final shape — and it closes the audit's own "SkipConcurrencyLimit flag will proliferate" Minor finding as a direct side effect, not a separately-scheduled fix.

The rename touches: `proto/ratecap/v1/ratecap.proto` (`skip_concurrency_limit` → `skip_reservations`, regenerated), `limiter.Request.SkipConcurrencyLimit` → `SkipReservations`, `grpcserver/server.go`'s field copy, `services/sidecar/proxy/proxy.go`'s `skip_concurrency` query param → `skip_reservations`, and `packages/sdks/go/client.go`'s `Allow()` (`&skip_concurrency=true` → `&skip_reservations=true`). This is pre-1.0 internal-only proto — renamed outright, no deprecation shim.

### 4. Wiring `ResolvePriority()` — finally load-bearing

`services/sidecar/proxy/proxy.go`'s `Handler.ServeHTTP` replaces today's `_ = ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)` with an actual assignment, threading the result into `CheckRateLimitRequest.Priority`. The existing characterization test (`TestServeHTTP_ParsesPriorityHeaderWithoutError`) pinned exactly this moment as the deliberate point where "parsed but discarded" becomes "parsed and load-bearing" — this phase is that deliberate, reviewed change the test's own name anticipated.

**Deferred, not implemented this phase:** the v1 spec's priority-resolution fallback order has three steps (1: per-request header, 2: static `critical_routes` config match, 3: global default). This phase implements steps 1 and 3 only (both already built); step 2 (route-pattern matching against a config list) is a separable, independently-useful feature that doesn't block Tier 3's core mechanism — tracked as a fast-follow, not implemented here.

### 5. Multi-reservation generalization (sidecar `+` SDK)

Once `FleetShedder` ships, a single `/check` call that clears both Tier 2 and Tier 3 produces **two** reservations in `CheckRateLimitResponse.Reservations` — one per-user (`Key: req.Key`), one global (`Key: "fleet"`). Today's sidecar only forwards `resp.Reservations[0]` via `Concurrency-Token`/`Concurrency-Key` headers, and the SDK's `Ticket` only stores one `(key, token)` pair — both would silently drop the second reservation, leaking a fleet slot on every `Acquire()` call that passes both tiers. This is exactly the class of bug Group A's audit fixed at the core-logic layer; this phase completes the fix at the outermost layers, closing it as a real, tested capability rather than leaving it as latent debt.

**Sidecar (`services/sidecar/proxy/proxy.go`):** `Handler.ServeHTTP` iterates `resp.Reservations` and emits one indexed header pair per reservation — `Concurrency-Token-0`/`Concurrency-Key-0`, `Concurrency-Token-1`/`Concurrency-Key-1`, etc. (indexed, not repeated same-name headers, since Go's `http.Header.Get` only returns the first value for a repeated header name — indexing avoids that footgun entirely and keeps the SDK's parsing loop trivial: `for i := 0; ; i++ { tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i)); if tok == "" { break } ... }`).

**SDK (`packages/sdks/go/client.go`):** `Ticket` becomes:

```go
type reservation struct {
    key string
    tok string
}

type Ticket struct {
    Allowed      bool
    RetryAfterMs int64

    client       *Client
    reservations []reservation
}
```

`Acquire()` parses the indexed headers into `[]reservation`. `Release(ctx)` iterates every reservation, calling `/release` once per entry, collecting any errors with `errors.Join` (best-effort per the existing no-retry contract — a failure releasing one reservation doesn't block attempting the others; the reaper remains the resilience backstop for any that fail, exactly as today's single-reservation design already documents).

### 6. No changes to core Reservation/release-routing logic

Confirmed by the design above: `services/core/limiter/limiter.go`, `pipeline.go`, and `services/core/grpcserver/server.go`'s `Reservations`-building and `ReleaseConcurrency` handler are untouched. `Pipeline.Check` already accumulates every tier's reservations regardless of key; `ReleaseConcurrency` already takes an arbitrary `(key, token)` pair and calls `DecrConcurrent` generically. Tier 3 is additive at exactly the seam Group A's fix was designed to make additive.

### 7. Config

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
  fleet_shedder:
    default_max_concurrent: 100
    reserved_critical_pct: 20
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
```

`fleet_shedder` mirrors `concurrency_limiter`'s shape (the pattern Tier 2 actually shipped, not the v1 spec's older sketch), adding `reserved_critical_pct` (int, 0-100) and `default_priority` (string, `"sheddable"` or `"critical"` — parsed the same way `ResolvePriority`'s header value already is). `critical_routes` is omitted per the deferred-scope decision above. Hot-reloadable via the same `Reconfigure` pattern every tier already uses.

### 8. Pipeline wiring

`services/core/main.go` adds `FleetShedder` as the pipeline's third tier: `limiter.NewPipeline(rateLimiter, concurrencyLimiter, fleetShedder)`, matching the spec's documented order (tier 1 → tier 2 → tier 3 → tier 4, cheapest/most specific check first). `config.Watch`'s reconfigure callback gains a third `Reconfigure` call alongside the existing two.

## Build Order

Following the walking-skeleton-style incremental granularity proven in both prior phases:

1. Move `Priority`/`Sheddable`/`Critical` from `services/sidecar/proxy/priority.go` into `services/core/limiter/limiter.go`; add `Request.Priority`; update `services/sidecar/proxy/priority.go` to import from core instead of defining its own copy. Update existing tests that reference the old location.
2. `FleetShedder` implementing `Limiter`, pure unit-tested against a fake store (mirroring `ConcurrencyLimiter`'s test pattern): allows exactly `cap` critical requests, allows exactly `cap*(1-pct/100)` sheddable requests before the smaller cap trips, both types count toward one shared global count, shadow mode reserves-but-coerces, `SkipReservations` bypasses entirely, `Reconfigure` race-tested.
3. Proto: add `priority` field to `CheckRateLimitRequest`, rename `skip_concurrency_limit` → `skip_reservations`; regenerate. Update `grpcserver/server.go`'s field copy (mechanical rename + new field, no logic change).
4. Wire `FleetShedder` into `services/core/main.go`'s pipeline (3rd tier) and config extension (`FleetShedderConfig` + hot-reload wiring).
5. Sidecar: wire `ResolvePriority()`'s result into `CheckRateLimitRequest.Priority` (no longer discarded); rename the `skip_concurrency` query param to `skip_reservations`; generalize `/check`'s response-header emission to indexed `Concurrency-Token-N`/`Concurrency-Key-N` pairs for all of `resp.Reservations`.
6. SDK: generalize `Ticket` to `[]reservation`; `Release()` releases every reservation with `errors.Join`; rename `Allow()`'s query param to match Task 5's rename.
7. Update `deploy/ratecap.yaml` with a `fleet_shedder` block; update the sample app to demonstrate the critical/sheddable split live (e.g. a route where most calls are sheddable-priority and a smaller volume of calls set `x-ratecap-priority: critical`, showing sheddable requests getting `503`'d while critical ones keep succeeding once fleet-wide concurrency is pushed past the reduced sheddable cap); re-verify the full docker-compose stack end-to-end proving all three tiers now trigger under the right conditions.

## Testing Strategy

- Unit tests for `FleetShedder` (pure, fake store, no Redis) — priority-dependent cap arithmetic, shared global count across priorities, shadow mode, `SkipReservations` bypass, `Reconfigure` concurrency safety.
- Unit tests for the sidecar's multi-reservation header emission and the SDK's multi-reservation parsing/release (fake sidecar HTTP server, mirroring existing patterns).
- Integration test against real Redis (testcontainers) is **not** newly needed for `FleetShedder` — it reuses `IncrConcurrent`/`DecrConcurrent` and the existing `concurrent_limiter.lua` script byte-for-byte, both already covered by Tier 2's integration tests (`TestIncrConcurrent_ConcurrentAtomicity`, `TestIncrConcurrent_ReapsStaleEntriesPastMaxDuration`, etc.) against arbitrary keys including `"fleet"`. No new Lua script, no new `StateStore` method.
- End-to-end docker-compose verification, extended beyond Tier 1/2's existing checks to demonstrate Tier 3 tripping: a burst of sheddable-priority requests gets `503`'d once fleet concurrency exceeds the reduced sheddable cap, while critical-priority requests in the same burst continue succeeding up to the full fleet cap.

## Out of Scope (this phase)

- Tier 4 (Worker Utilization Load Shedder) — separate future phase.
- `critical_routes` static config-based priority resolution (v1 spec's fallback-order step 2) — deferred as a fast-follow; header + global default (steps 1 and 3) are sufficient to demonstrate and test Tier 3's core mechanism.
- A configurable fleet-key name — `"fleet"` is a hardcoded constant; no known v1 use case for changing it.
- Any change to `TokenBucketLimiter`, `Pipeline`, or `grpcserver`'s `Reservations`-building/`ReleaseConcurrency` logic — confirmed unnecessary by this spec's "Why This Phase Should Be (Mostly) Additive" section.
- Any change to the Lua scripts or `StateStore` interface — `FleetShedder` reuses `IncrConcurrent`/`DecrConcurrent` and `concurrent_limiter.lua` exactly as Tier 2 built them, with a different key and cap argument, nothing else.
