# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project intends to follow [Semantic Versioning](https://semver.org/) once a first tagged release is cut.

## [Unreleased]

### Added

- `.github/workflows/ci.yml` — GitHub Actions CI building and testing all five Go modules on every push/PR to `develop`/`main`.
- One-time PyPI Trusted Publisher setup instructions for `.github/workflows/publish-python-sdk.yml`, documented in [CONTRIBUTING.md](CONTRIBUTING.md#releasing-the-python-sdk-to-pypi-one-time-setup) — without this manual PyPI + GitHub Environments setup, the first `python-sdk-v*` tag push fails with an OIDC authentication error.

## [2.6.0] — 2026-08-28 — Phase 2 Reliability & Testing Hardening

Minor release: Phase 2 of the v3 upgrade roadmap — RateCap's core architectural claims (fail-open/fail-closed, atomicity, no-flapping) are now tested invariants instead of assumptions, Redis can run HA via Sentinel, and on-call has a sub-second incident-response lever.

### Added

- Toxiproxy-based real-network-fault regression tests proving Tier 1 fails open and Tiers 2/3 fail closed on a Redis outage (`services/core/reliability`).
- Race regression tests for the priority-partition-at-capacity bug class (Tier 2 and Tier 3) and a concurrent stress test for `services/sidecar/decisionlog`.
- Property-based tests (`pgregory.net/rapid`) for `TokenBucketLimiter` and `ConcurrencyLimiter`.
- Fault-injection tests for the fsnotify config hot-reload path (partial writes, atomic rename-swap, delete+recreate).
- `Config.Hash()` and `ratecap_core_config_version_info{hash}` for detecting cross-replica config divergence during a rollout.
- Coverage measurement + a 50% floor CI gate across all Go modules and the Python SDK; a Gremlins mutation-testing CI gate scoped to `services/core/limiter` and `packages/sdks/go`.
- Opt-in Redis Sentinel support: `RATECAP_REDIS_SENTINEL_ADDRS`/`RATECAP_REDIS_SENTINEL_MASTER_NAME` on `ratecap-core`, and a `redis.sentinel.enabled` Helm values flag (default `false`).
- Gradual Tier 4 shed-curve ramping (`RATECAP_SHED_RAMP_START_PCT`), replacing a hard on/off cutoff.
- A new `SetDynamicLimit` gRPC RPC and a sidecar `/admin/set-limit` HTTP endpoint (gated by a new, separate `RATECAP_ADMIN_SECRET`) for instantly changing Tier 1's rate or Tier 3's `reserved_critical_pct` fleet-wide without a config re-parse — plus `ratecapctl admin set-limit`.

### Security

- The new admin lever requires its own secret (`RATECAP_ADMIN_SECRET`), separate from `RATECAP_SHARED_SECRET`, given its fleet-wide and effectively unbounded blast radius compared to a normal `/check` call.

## [2.5.0] — 2026-08-28 — Phase 1 Observability Foundation

Minor release: Phase 1 of the v3 upgrade roadmap — `services/core` gains self-instrumentation it previously had none of, the sidecar's `/metrics` no longer shares its self-throttle limiter with real traffic, and both services' health checks reflect real backing-service connectivity instead of static/startup-only state.

### Added

- `ratecap-core` `/metrics` endpoint (new `:9092` listener) — gRPC request count/latency by method and status, Redis call latency/error count, config-reload success/failure count.
- `ratecap_fail_open_total{tier,reason}` — Tier 1 (request-rate) now fails OPEN on a Redis/store error instead of surfacing an internal error, matching Stripe's documented precedent; Tiers 2/3 remain fail-closed by design (see `ARCHITECTURE.md`'s new Observability section for the full per-tier contract).
- `ratecap_decision_latency_seconds{tier}`, `ratecap_release_total{result}`, and `ratecap_upstream_errors_total{endpoint}` on the sidecar.
- Starter Grafana dashboard (`deploy/grafana/ratecap-overview.json`) and baseline alert rules (`deploy/grafana/ratecap-alerts.yml`).
- An Observability section in `ARCHITECTURE.md` documenting the full metrics contract, the per-tier Redis-down degradation contract, and current tracing limitations.

### Fixed

- Sidecar `/healthz` now reflects real gRPC connectivity to core instead of unconditionally returning 200.
- Core's gRPC health service now reflects real Redis connectivity (re-checked every 5s) instead of being set to `SERVING` once at startup and never updated again.
- `/metrics` and `/healthz` on the sidecar no longer share the process-wide self-throttle rate limiter with `/check`/`/release` — a Prometheus scrape can no longer be 429'd by the same limiter throttling real traffic.

## [2.4.1] — 2026-08-27 — Phase 0 Housekeeping & Quick Wins

Patch release: Phase 0 of the v3 upgrade roadmap (see `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`) — housekeeping and quick wins, sequenced before any phase that needed a trustworthy version number to build on.

### Fixed

- Tier 2's bounded-queueing backlog counter is now Redis-backed (`store.IncrConcurrent`/`DecrConcurrent` against a `backlog:` key namespace) instead of a per-instance `atomic.Int64` — with N core replicas, the real ceiling was previously `maxBacklog × N`, not one shared ceiling.
- `bench_run.go`'s `--acquire` path no longer silently drops accepted/rejected/errored request outcomes into the same latency distribution; results are now bucketed separately, and every ticket's `Release()` is called even when the request itself was rejected.
- Dependency skew across `proto`/`services/core`/`services/sidecar` closed by merging the 4 open Dependabot PRs (grpc, go-redis, testcontainers, x/sys, x/text) in lockstep rather than per-module.

### Added

- `.github/dependabot.yml` covering all Go module directories plus `pip`, `github-actions`, and `docker` ecosystems, grouped, on a weekly schedule.
- `VERSION` as the single authoritative version source.
- Merged `fix/v3-config-validation` (Tier 1 `rate_limiter` config validation) and `fix/v3-breaking-wire-changes` (`PRIORITY_UNSPECIFIED` proto enum sentinel — a breaking wire-format renumbering, called out explicitly rather than shipped silently).

## [2.3.2] — 2026-07-20 — Tier 2 Concurrency-Token Security Hotfix

**Semver note:** this release contains a breaking wire-format change (see below) but was tagged as a patch bump (2.3.1 → 2.3.2), inconsistent with this file's stated intent to follow Semantic Versioning. Documented here as an acknowledged exception rather than corrected retroactively (re-tagging a already-published release would be worse) — going forward, a breaking change gets at minimum a minor bump, called out explicitly in its own CHANGELOG entry, the way this one now is.

Patch release closing issues #12 and #13, both confirmed exploitable via real-world CVE precedent (Portainer CVE-2026-44883, nhost CVE-2026-34969).

### Security

- `ReleaseConcurrency` now verifies an HMAC-SHA256 signature over the concurrency token before releasing a slot, rejecting a forged/tampered/wrong-key token with `codes.PermissionDenied` (#12).
- **Breaking change:** `/release`'s `key` and `token` now travel as request headers (`X-RateCap-Concurrency-Key`/`X-RateCap-Concurrency-Token`) instead of URL query parameters, closing a real leakage vector via proxy/access logs and `Referer` headers (#13). Any direct HTTP caller of `/release` bypassing the Go/Python SDKs must migrate to the new headers.
- New required env var `RATECAP_CONCURRENCY_SIGNING_KEY` on `ratecap-core` (fail-closed at startup).

## [2.3.1] — 2026-07-20 — Tier 2/3/4 Hardening Batch

Patch release: 5 merged PRs (#42-#46) closing 14 tracked issues.

### Added

- Tier 2 hardening: `RetryAfterMs` on rejection, `DecrConcurrent` regression tests, shadow-mode docs (#8, #9, #11).
- Tier 3 hardening: sheddable-cap rounding tests, shadow-mode reservation test, mixed-priority atomicity test, blast-radius docs (#14, #17, #18, #19).
- Tier 4 hardening: `X-RateCap-Shed-Tier` header, in-flight metrics gauge, HTTP server timeouts, `/worker-demo` docs (#20, #22, #24, #25).
- Sidecar error logging: real upstream errors now logged server-side on `/check` and `/release` failures (#41).

### Fixed

- Config watcher debounce: fixes a flaky fsnotify-related test via event coalescing (#37).

## [2.3.0] — 2026-07-19 — Phase 4 Production-Readiness & Adoption

### Added

- Production-readiness and adoption work per Phase 4 of the v2 roadmap (PR #40) — see `docs/superpowers/specs/2026-07-18-v2-phase-4a-comparison-table-design.md`, `2026-07-18-v2-phase-4b-benchmarks-design.md`, and `2026-07-19-v2-phase-4c-helm-chart-design.md` for the full design rationale behind this release's comparison table, benchmarks, and Helm chart.

## [2.2.0] — 2026-07-17 — Phase 3 Bounded Queueing (ConcurrencyLimiter)

### Added

- Bounded queueing for Tier 2's `ConcurrencyLimiter` (PR #34) — see `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.

## [2.1.0] — 2026-07-17 — Phase 2 Developer Tooling

### Added

- `ratecapctl` CLI and the Python SDK (PR #33) — see `docs/superpowers/specs/2026-07-17-v2-phase-2-tooling-design.md`.

## [2.0.0] — 2026-07-16 — Phase 1 Foundations

### Added

- Observability, structured logging, and optional mTLS (PR #31) — see `docs/superpowers/specs/2026-07-17-v2-phase-1-foundations-design.md`.

## [1.0.1] — 2026-07-16 — Config Validation Gaps

### Fixed

- Config validation gaps found in a post-v1.0.0 audit (PR #29) — see `docs/superpowers/specs/2026-07-16-v1.0.1-config-validation-gaps.md`.

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
