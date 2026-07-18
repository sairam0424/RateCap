# RateCap v2 Phase 3: Bounded Queueing — Design Spec

**Date:** 2026-07-17
**Status:** Awaiting explicit sign-off (see Scope Gate below — this is not a normal spec-review checkpoint)
**Context:** Third of a 4-phase v2 roadmap. Phase 1 (observability, mTLS) shipped as v2.0.0, Phase 2 (`ratecapctl` CLI, Python SDK) shipped as v2.1.0. This phase adds a new `QUEUE` response action to `ConcurrencyLimiter` (Tier 2).

---

## Scope Gate — read this before anything else

Per this repo's `CLAUDE.md`: *"v1 is locked to Stripe's exact 4 mechanisms — do not add a 5th limiting mechanism, bounded queueing, additional storage backends, or a Rust/WASM core without updating the design spec first and getting explicit sign-off."* Bounded queueing is named explicitly in that sentence. Unlike Phases 1-2 (additive tooling, observability, security hardening — no new enforcement behavior), this phase adds a genuinely new kind of enforcement mechanism: requests can now be held and retried internally instead of immediately allowed or rejected.

This spec requires the user's **explicit sign-off** — not the routine "please review the spec" used for Phases 1-2 — before `writing-plans` is invoked. That sign-off is requested separately, after this document is committed.

## Problem

RateCap's v1 design spec deferred bounded queueing to v2, noting: *"The `Action` enum is designed so adding `QUEUE` later is additive, not a breaking change."* Research (three adversarially-verified deep-research passes, prior session) found real production precedent for exactly this mechanism:

- **Alibaba Sentinel's "Rate Limiter" control behavior** (verified via direct source inspection) is genuine leaky-bucket-style "uniform queueing": it computes a wait time and parks the calling thread until its turn, rejecting only if the wait exceeds a configured max — real queueing, not instant-reject.
- **Netflix's `concurrency-limits` library** ships a `LifoBlockingLimiter` with a hard-capped backlog (default 100) to prevent unbounded queue growth during sustained overload.
- **Resilience4j**'s `FixedThreadPoolBulkhead` is backed by a genuinely bounded queue (`ArrayBlockingQueue`, default capacity 100).

Common pattern: bounded backlog size + a max-wait cutoff before falling back to reject.

**The architectural tension this design must resolve:** all three precedents queue on *local, in-process, single-instance* state (a local semaphore, a local blocking queue). RateCap's Tier 2 concurrency cap (`IncrConcurrent`/`DecrConcurrent`) is a Redis Lua script — atomic, cross-instance-consistent, with no native "wait for a slot" primitive. This design resolves that tension explicitly (see Key Design Decisions §1) rather than either building unnecessary distributed-queue infrastructure or quietly implementing something that only looks like queueing.

## Key Design Decisions

### 1. Mechanism: bounded local backlog + poll the existing Redis check

A bounded local semaphore, sized `MaxBacklog`, lives inside each `ratecap-core` instance's `ConcurrencyLimiter`. When `IncrConcurrent` first fails (the cap is full) and queueing is enabled, the caller attempts to acquire a backlog slot:

- **Backlog full** → immediate `REJECT_429`, exactly like today's non-queueing behavior. Queueing never makes rejection *more* likely than today; it only adds a bounded chance of eventual success.
- **Backlog slot acquired** → the caller polls `IncrConcurrent` every `PollIntervalMs`, until one of three outcomes:
  - A slot frees and `IncrConcurrent` succeeds → release the backlog slot, return `Decision{Action: ALLOW, ...}` (the caller never knows queueing happened — see §5).
  - `MaxQueueWaitMs` elapses → release the backlog slot, return `Decision{Action: REJECT_429, Tier: "concurrency_limiter"}`, exactly like today's non-queueing rejection.
  - The request's `ctx` is canceled (client disconnected, upstream timeout) → release the backlog slot, propagate the context error.

This reuses the existing, already-atomic `IncrConcurrent` Lua script with **zero new distributed-queue infrastructure** — no Redis `BLPOP`, no pub/sub, no reimplementing a queue inside Redis. The backlog semaphore bounds Redis load by construction: only up to `MaxBacklog` goroutines are ever polling at once, never every rejected request.

**Explicit, documented limitation:** this backlog is per-`ratecap-core`-instance, not fleet-wide. Worst-case total backlog across a fleet of N core instances is `MaxBacklog × N`, not a single coordinated `MaxBacklog`. This mirrors Tier 4's already-accepted local-only worker shedder — RateCap already has this exact category of precedent, and this limitation is stated in `ARCHITECTURE.md`/`SECURITY.md` updates as part of implementation, not hidden.

### 2. Scope: `ConcurrencyLimiter` (Tier 2) only

`FleetShedder` (Tier 3) queueing is **explicitly deferred**, not part of this phase. Tier 3's priority split (critical vs. sheddable effective caps) raises its own design question — would a queued sheddable request wait behind critical traffic indefinitely under sustained load? — that deserves dedicated brainstorming once Tier 2's queueing mechanism has proven itself in production use. `FleetShedder`'s `Check()` is untouched by this phase.

### 3. Configuration: off by default, extends the existing hot-reload pattern

New fields on `ConcurrencyLimiterConfig`:

```go
type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
	QueueingEnabled       bool  `yaml:"queueing_enabled"`
	MaxBacklog            int   `yaml:"max_backlog"`
	MaxQueueWaitMs        int64 `yaml:"max_queue_wait_ms"`
	PollIntervalMs        int64 `yaml:"poll_interval_ms"`
}
```

`QueueingEnabled` defaults to `false` (Go's zero value) — an existing `ratecap.yaml` with no `queueing_enabled` key gets zero behavior change on upgrade, matching Phase 1's mTLS precedent exactly (additive, opt-in, no surprise for deployments that don't ask for it). The three new numeric fields extend `ConcurrencyLimiter.Reconfigure(...)`'s existing signature, so this hot-reloads through the same config-watcher mechanism already proven across all four tiers — no new plumbing.

### 4. Ordering: none — first-successful-poll-wins, by design

No explicit LIFO or FIFO ordering is implemented. With independent polling (each waiter has its own goroutine calling `IncrConcurrent` on its own timer, not parked in a real queue data structure), "who gets served first" is naturally whichever waiter's poll happens to succeed first — imposing an artificial ordering on top of this would add real complexity without a real behavioral guarantee to back it up (a "LIFO queue" over independent pollers is theater, not a genuine ordering primitive). This is a deliberate MVP simplification, not an oversight — worth revisiting only if real usage shows a concrete fairness problem.

### 5. Wire shape: fully transparent to the client

A queued-then-served request returns `200`, indistinguishable on the wire from an immediate `ALLOW`. No new response header. The `Action`/`ratecapv1.Action` enums gain a `QUEUE` value purely for **server-side attribution** — it flows into `Decision.Tier`/`CheckRateLimitResponse.tier` and, via Phase 1's existing instrumentation, into `ratecap_decisions_total{tier="concurrency_limiter",action="queue"}` and structured decision logs (where the elevated `latency_ms` already makes queueing visible without a dedicated field). This mirrors exactly how `SHADOW_LOG` already works today: server-observable, invisible to the caller. `retry_after_ms` is not repurposed for queueing — a queued-and-eventually-allowed request has no retry to hint at, and a queued-then-rejected request already gets `REJECT_429`'s existing hint semantics unchanged.

**Wire-format note:** adding `QUEUE = 4;` to `proto/ratecap/v1/ratecap.proto`'s `Action` enum is additive/non-breaking — `services/core/grpcserver/server.go`'s `toProtoAction` already has a `default: return ratecapv1.Action_REJECT_503` fallback for any `limiter.Action` it doesn't recognize, so this is a pure enum addition, not a wire-shape change.

### 6. Testing: unit tests + real concurrency stress tests

- **Unit tests** (fake `concurrencyChecker`): backlog-full-returns-immediate-429, successful-poll-returns-allow, deadline-exceeded-returns-429, context-cancellation-propagates, config validation for the new fields (e.g. `MaxBacklog <= 0` should be rejected the same way `DefaultMaxConcurrent <= 0` already is).
- **Real concurrency stress tests**, mirroring Tier 4's `worker.Shedder` precedent (real goroutines, real `time.Sleep`-based timing with generous margins, no simulation framework): spin up more concurrent callers than `MaxBacklog` against a real or realistic fake store and confirm the semaphore never over-admits waiters; confirm a request that exceeds `MaxQueueWaitMs` genuinely times out to `REJECT_429`, not early and not late; confirm a slot that frees mid-wait is genuinely picked up by a polling waiter within one `PollIntervalMs` cycle.

## Out of Scope (this phase)

- `FleetShedder` (Tier 3) queueing — explicitly deferred, own future design.
- Any explicit LIFO/FIFO ordering primitive — deliberately not built (see §4).
- Any client-visible signal that queueing occurred (headers, wire fields beyond the server-side `QUEUE` action) — fully transparent by design (see §5).
- Fleet-wide/cross-instance-coordinated backlog — this remains an honestly local-per-instance mechanism (see §1's documented limitation).
- Distributed-queue infrastructure in Redis (`BLPOP`, pub/sub, or similar) — the polling-based approach was chosen specifically to avoid this.
