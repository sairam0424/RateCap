# Tier 4 — Worker Utilization Load Shedder — Design Spec

**Date:** 2026-07-15
**Status:** Approved
**Context:** The fourth and final tier of the v1 design spec, after Tier 3 (merged to `develop`, PR #6, commit `4504123`, including audit remediation). See [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](2026-07-13-ratecap-v1-design.md) for the overall v1 design.

---

## Problem

Per the v1 design spec's tier table:

> 4. Worker Utilization Load Shedder | Local process-level check (goroutine pool / queue depth threshold) — no Redis round-trip | In-memory (sidecar-local) | `503` default action on trip

And the spec's own admission: "Tier 4 — Worker Utilization Load Shedder: the least-documented tier in public sources; the final line of defense at the process/worker-pool level." Unlike Tiers 1-3, there is no confirmed Stripe mechanism to faithfully recreate — only the spec's own interpretation. This is genuine, acknowledged creative latitude, not a gap in research.

**What every prior tier assumed that Tier 4 breaks:** Tiers 1-3 all live in `ratecap-core`, share the `Limiter`/`Pipeline` seam, go through a gRPC round-trip from the sidecar, and key state off Redis. Tier 4 is explicitly local, sidecar-side, and has no Redis round-trip — the first tier where "no round-trip" doesn't mean "skip Redis but stay centralized," it means the sidecar decides on its own, before ever contacting core.

## Key Design Decisions

### 1. Tier 4 lives in `ratecap-sidecar`, not `ratecap-core`

Tier 4 does **not** implement `limiter.Limiter` and is **not** a 4th entry in `services/core/limiter/pipeline.go`'s `Pipeline`. That seam exists specifically for core's Redis-backed, reservation-issuing tiers (`Decision.Reservations`, `ReleaseConcurrency`, hot-reload via `config.Watch`) — none of which apply here. Tier 4 protects the sidecar's own capacity to keep functioning, checked as a pre-check in `services/sidecar/proxy/proxy.go`'s `Handler.ServeHTTP`, **before** the sidecar ever calls `h.client.CheckRateLimit`. This is the literal reading of "final line of defense" and "no round-trip": an overwhelmed sidecar sheds load without even burdening core with a call.

This is a deliberate architectural departure from every prior tier, not an oversight — the Tier 3 audit's architecture lens already flagged this exact question ("does Tier 4 genuinely fit additively into the current `Limiter`/`Pipeline`/`grpcserver` seam with zero core changes?") and found no red flags in the *existing* code's assumptions specifically because Tier 4 was never going to be forced into that seam.

### 2. Signal: an atomic in-flight request counter

A new package, `services/sidecar/worker`:

```go
package worker

import "sync/atomic"

type Shedder struct {
    inflight atomic.Int64
    max      int64
}

func NewShedder(max int64) *Shedder {
    return &Shedder{max: max}
}

func (s *Shedder) Allow() bool {
    if s.inflight.Load() >= s.max {
        return false
    }
    s.inflight.Add(1)
    return true
}

func (s *Shedder) Release() {
    s.inflight.Add(-1)
}
```

This measures "is the sidecar itself currently overloaded," not any caller-identified resource — `Shedder.Allow()` takes no arguments at all, since Tier 4 has no relevance to `req.Key`, `Cost`, or `Priority`. Neither `ratecap-core` nor `ratecap-sidecar` has an existing worker-pool abstraction to threshold against; introducing one (fixed goroutines + a channel-based queue) purely to serve this tier would be a significant new architectural primitive with no other consumer in the codebase — out of proportion to what "the least-documented tier" requires for v1. An atomic counter is the simplest signal that directly answers the actual question ("is this process currently handling too many requests at once") without inventing infrastructure nothing else needs.

### 3. Config: one knob, sidecar-local, no hot-reload

```bash
RATECAP_MAX_INFLIGHT_REQUESTS=500  # default if unset
```

Read once at `services/sidecar/main.go` startup, matching the existing `RATECAP_CORE_ADDR`/`RATECAP_SIDECAR_ADDR`/`RATECAP_SHARED_SECRET` pattern exactly — no new config-delivery mechanism. This is v1's concrete interpretation of the spec's `worker_shedder.max_queue_depth` sketch; the spec's second knob, `max_worker_utilization_pct`, is dropped for v1 since there is no real worker pool to compute a percentage against, and a synthetic percentage (e.g. relative to `GOMAXPROCS`) would be arbitrary rather than meaningful.

The original v1 spec called for a core-to-sidecar config-sync mechanism ("each `ratecap-sidecar` pulls its local-enforcement config... from `ratecap-core` at startup and on the `sync_rate` interval") specifically naming "tier 4 thresholds" as an example — that mechanism was never built, and Tier 3 already left an analogous gap (`FleetShedderConfig.DefaultPriority` parsed by core but never consumed by the sidecar) as a tracked, unfixed follow-up rather than building it. Tier 4 follows the same precedent: sidecar-local env var now, real core-to-sidecar config sync explicitly deferred to v2.

### 4. Wiring into `Handler.ServeHTTP`

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    if !h.shedder.Allow() {
        if shadow.GlobalOverrideEnabled() {
            log.Printf("worker shedder: would have shed request, shadow mode active")
        } else {
            w.WriteHeader(http.StatusServiceUnavailable)
            return
        }
    } else {
        defer h.shedder.Release()
    }

    // ... existing key/priority/skip_reservations parsing and CheckRateLimit call, unchanged
}
```

When `Allow()` returns false and shadow mode is off, the handler returns `503` **without ever constructing a `CheckRateLimitRequest` or calling `h.client.CheckRateLimit`** — zero gRPC round-trip, exactly as the spec requires. When shadow mode is on, the would-have-shed event is logged and the request proceeds to Tiers 1-3 as normal; note that in this shadow-mode branch, `Allow()` already returned false without incrementing the counter, so there's no matching `Release()` to call — accounting stays correct without needing the `unboundedCap`-style forced-reservation trick Tiers 2/3 use, since there's no shared state to keep consistent here (each sidecar's counter is process-local and independent).

`Handler` gains a `shedder *worker.Shedder` field, set via `NewHandler`'s existing constructor (adding a parameter) — `services/sidecar/main.go` constructs the `Shedder` from `RATECAP_MAX_INFLIGHT_REQUESTS` and passes it in alongside the existing `client`/`defaultPriority` arguments.

### 5. Shadow mode reuses the existing global override

No new per-tier shadow flag — `shadow.GlobalOverrideEnabled()` (reading `RATECAP_SHADOW_MODE`) is reused as-is, since Tier 4 has no core-side config to carry a dedicated per-tier `shadow_mode` boolean the way Tiers 1-3 do. This is consistent with Tier 4 having no config delivery mechanism at all beyond its one env var.

### 6. `ReleaseHandler` is unaffected

`/release` continues to call `ReleaseConcurrency` on core for Tiers 2/3 reservations exactly as today — Tier 4 has no reservation lifecycle and does not touch `ReleaseHandler.ServeHTTP` at all.

## Build Order

Following the walking-skeleton-style incremental granularity proven in prior phases:

1. `worker.Shedder` — pure unit tests (no HTTP, no gRPC, no Redis): allows exactly `max` concurrent `Allow()` calls, rejects the `max+1`th, `Release()` frees a slot for a subsequent `Allow()`, race-detector-clean under concurrent `Allow()`/`Release()` calls.
2. Wire `Shedder` into `Handler.ServeHTTP` as a pre-check: unit-tested with a fake client proving that an over-limit request returns `503` **and the fake client's `CheckRateLimit` is never called** (the actual round-trip-skipping behavior, not just the status code); shadow-mode test proving an over-limit request still reaches the fake client when shadow mode is active.
3. `services/sidecar/main.go`: read `RATECAP_MAX_INFLIGHT_REQUESTS` (with the documented default), construct the `Shedder`, pass it into `NewHandler`.
4. `deploy/`: demonstrate live — a burst large enough to exceed a deliberately low demo threshold, proving the sidecar sheds locally (503, and confirmable via core's logs showing no corresponding `CheckRateLimit` call for the shed requests); re-verify the full docker-compose stack end-to-end, proving all four tiers now trigger under the right conditions.

## Testing Strategy

- Unit tests for `worker.Shedder` — pure, no dependencies at all (not even HTTP), the simplest test story of any tier in this project.
- Unit tests for `Handler`'s pre-check wiring — a fake client proving the round-trip is genuinely skipped (call-count assertion), not just that the response code changes.
- Race-detector runs (`go test -race`) — the concurrency-safety story here is a single atomic counter, much simpler to reason about than Tiers 2/3's Redis Lua atomicity, but still worth the same rigor this project applies everywhere.
- End-to-end docker-compose verification — a burst exceeding the demo threshold, confirming `503`s that never reached core (distinguishable in logs from Tier 3's `503`s, which do reach core).
- No testcontainers/Redis integration tests needed for this tier — the first tier in this project without one, since Tier 4 has no Redis dependency at all.

## Out of Scope (this phase)

- The core-to-sidecar config-sync mechanism the original v1 spec called for — explicitly deferred to v2, consistent with Tier 3's `DefaultPriority` precedent.
- `max_worker_utilization_pct` as a distinct config knob — dropped; `max_inflight_requests` (this spec's `RATECAP_MAX_INFLIGHT_REQUESTS`) is v1's single, concrete interpretation of "queue depth."
- A real bounded worker pool with an explicit channel-based queue — the atomic counter is the chosen v1 signal; introducing an actual worker-pool primitive that every sidecar request flows through is a materially larger architectural change with no other consumer in this codebase, and is not required to satisfy "the least-documented tier"'s spec language.
- Any change to `services/core` — confirmed unnecessary throughout this design; Tier 4 is entirely additive to `services/sidecar`.
- Any change to `ReleaseHandler` or the `ReleaseConcurrency` RPC — Tier 4 has no reservation lifecycle.
