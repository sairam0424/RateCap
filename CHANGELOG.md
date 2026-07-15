# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project intends to follow [Semantic Versioning](https://semver.org/) once a first tagged release is cut.

## [Unreleased]

### Added

- `.github/workflows/ci.yml` — GitHub Actions CI building and testing all five Go modules on every push/PR to `develop`/`main`.

## [1.0.0] — All Four Tiers — 2026-07-16

RateCap's v1 scope is complete: all four of Stripe's rate-limiter/load-shedder mechanisms are implemented, live-e2e-verified, and audited (correctness, concurrency-safety, security, architecture lenses via multi-agent adversarial review) end to end — SDK → sidecar → core → Redis, plus a sidecar-local fourth tier with no Redis round-trip at all.

### Added — Tier 2: Concurrent Requests Limiter

- `ConcurrencyLimiter` (a `Limiter` sibling to `TokenBucketLimiter`) bounding simultaneous in-flight requests per key via a Redis sorted set (`ZADD`/`ZCARD`/`ZREM`, atomic reap-count-add Lua script for stale-entry cleanup).
- `Pipeline` composing ordered `Limiter` tiers with short-circuit on the first non-`ALLOW` decision, accumulating every tier's reservations (not just the last one) so a request that trips multiple tiers correctly carries and releases all of them.
- SDK `Acquire()`/`Ticket.Release()` API; sidecar `/check` (returns `Concurrency-Token`/`Concurrency-Key` headers) and new `/release` endpoint.
- Shared-secret gRPC authentication between sidecar and core (fail-closed on missing `RATECAP_SHARED_SECRET`), sanitized gRPC error responses, HTTP method enforcement (`/check` GET-only, `/release` POST-only), and explicit `SECURITY.md`/`ARCHITECTURE.md` documentation of the plaintext-internal-network v1 trust boundary.

### Added — Tier 3: Fleet Usage Load Shedder

- `FleetShedder` — mechanically identical to Tier 2 (same Redis sorted-set lifecycle) but keyed globally (`"fleet"`, never `req.Key`) with a priority-dependent effective cap: critical traffic checked against the full fleet cap, sheddable traffic against a reduced cap (`cap*(1-reservedCriticalPct/100)`), so critical traffic always has reserved headroom.
- `Priority` (`SHEDDABLE`/`CRITICAL`) made load-bearing on the wire contract; `skip_concurrency_limit` renamed to the tier-agnostic `skip_reservations` — v1's final shape, exactly two reservation-issuing tiers.
- `Config.Validate()` enforcing `fleet_shedder.default_max_concurrent > 0` and `reserved_critical_pct` in `[0,100]`, failing closed on startup and skipping (not crashing on) a bad hot-reload — closes a silent 100%-outage config gap.

### Added — Tier 4: Worker Utilization Load Shedder

- `worker.Shedder` — a dependency-free, sidecar-local atomic in-flight request counter (`Allow()`/`Release()`, `CompareAndSwap` retry loop) checked before any gRPC call to core — genuinely zero round-trip on the shed path, unlike every other tier.
- Wired into `Handler.ServeHTTP` as a pre-check, configured via `RATECAP_MAX_INFLIGHT_REQUESTS` (soft-fail to a default of 500 on an unparseable value).
- Critical-priority requests bypass the shedder check entirely (`Allow()`/`Release()` never called), closing a priority-blind-starvation gap found by this tier's own pre-PR audit where ordinary sheddable load could 503 a critical request before Tier 3's reserved-capacity carve-out ever ran.

### Fixed

- `Pipeline.Check` silently dropping an earlier tier's reserved token when a later tier rejected the request (dormant in Tier 2 alone, guaranteed to leak a slot once Tier 3 shipped downstream) — fixed via `Decision.Reservations []TokenReservation`, where each reservation self-describes its own key.
- `ReleaseConcurrency` hardcoding `req.Key` as the release key instead of the server-supplied reservation key.
- A genuine check-then-act concurrency race in `worker.Shedder.Allow()`'s original `Load()`-then-`Add()` implementation, caught by a reviewer's stress test (~2-5% overshoot at `-count=200`) and fixed with a `CompareAndSwap` retry loop before it ever shipped.

### Follow-up work (tracked as issues, not blocking this release)

18 Minor/Important findings from the Tier 2/3/4 audits — spec-fidelity gaps, missing edge-case tests, observability gaps, and documentation nits — are filed as individual GitHub issues (#8–#25) rather than fixed inline, per this project's established audit-then-triage workflow.

## [0.1.0] — Walking Skeleton — 2026-07-13

The first working slice of RateCap: Tier 1 (Request Rate Limiter) proven end-to-end across every architectural seam — SDK → sidecar → core → Redis — before Tiers 2–4 are built on the same plumbing.

### Added

- `proto/` — the `RatecapService` gRPC contract (`CheckRateLimit` RPC, 4-value `Action` enum: `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`).
- `services/core` — the central engine:
  - `store` — `StateStore` interface with a Redis-backed implementation using an atomic Lua token-bucket script.
  - `limiter` — `Limiter` interface with `TokenBucketLimiter`, pure decision logic with no Redis dependency, unit-tested via a fake store.
  - `config` — YAML config loading and `fsnotify`-based hot-reload, with error-logging on reload failure and hardening for atomic file replacement.
  - `grpcserver` + `main.go` — wires everything into a running gRPC service.
- `services/sidecar` — the local proxy:
  - `proxy` — priority-header resolution (`x-ratecap-priority` → route config → safe default) and the HTTP handler forwarding checks to core.
  - `shadow` — per-tier and global (`RATECAP_SHADOW_MODE`) shadow-mode override for safe production rollout.
- `packages/sdks/go` — a thin Go client SDK wrapping the sidecar's HTTP endpoint.
- `deploy/` — a Docker Compose demo (core + sidecar + Redis + sample app) proving real rate-limiting end-to-end.
- `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` — the full v1 design spec.

### Fixed

- A data race in `TokenBucketLimiter.Reconfigure`, which mutated shared config fields with no synchronization while `Check` read them concurrently from gRPC handler goroutines — caught by the race detector before it shipped, fixed with a `sync.RWMutex`.
- A protobuf descriptor-path leak (`proto/ratecap/v1/...` instead of the idiomatic `ratecap/v1/...`) from an initial `protoc` invocation missing `-I proto`.

### Post-review fixes

- Added a characterization test pinning the sidecar's current "parse the priority header, don't act on it" behavior — a regression tripwire for when Tier 3 makes priority load-bearing.
- Replaced `GOWORK=off` (which caused Docker's build cache to ignore `go.work.sum` entirely) with a build-context-scoped `go.work` generated inline in each Dockerfile, so a real dependency bump now correctly invalidates the Docker layer cache.
