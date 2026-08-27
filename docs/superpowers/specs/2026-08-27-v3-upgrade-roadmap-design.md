# RateCap v3 Upgrade Roadmap — Design Spec

**Date:** 2026-08-27
**Status:** Approved (phasing approach); spec pending user review
**Research basis:** A 13-agent dynamic-workflow research pass — 6 external-research agents (industry patterns: LLM/AI-gateway token-cost limiting, fleet-wide Redis coordination, zero-trust/mTLS defaults, observability best practices, competitor feature evolution, chaos/mutation-testing practices) + 6 internal-audit agents (test/CI maturity, observability current-state, dependency health, release/versioning process, benchmark depth, fleet-coordination feasibility) + 1 synthesis pass, plus a prior 8-agent codebase-indexing pass. ~2.2M combined subagent tokens, ~340 tool calls, all findings grounded in either a primary external source (with URL) or a direct repo file/line reference — no unverified claims.

---

## Problem

RateCap shipped v1 (all four Stripe tiers) and has since accumulated seven more tags (v1.0.1 through v2.3.2) plus seven untagged commits on `main`. A full-power research pass — external industry research plus an internal audit — surfaced ~40 concrete upgrade candidates across eight categories. Several are correctness bugs (a fleet-ceiling gap, a benchmark measurement bug), several are safety-critical blind spots (no fail-open testing, no core-side metrics), and several are process debt (four documents disagreeing about what version RateCap is actually on). This spec sequences the actionable subset into shippable phases.

## Scope & Non-Goals

**In scope:** 56 discrete action items across six phases (below) — two of which (merging `fix/v3-config-validation` and `fix/v3-breaking-wire-changes`, both pre-existing branches) are review-and-merge, not new development. Every item is additive/backward-compatible except the wire-format renumbering in item 11 of Phase 0, which is called out explicitly rather than silently shipped as a patch (the exact mistake this roadmap's own Phase 0 item 7 documents fixing for v2.3.2).

**Explicitly deferred — real "ocean," needs its own future spec + sign-off, not bundled here** (per the research's own recommendation and RateCap's existing scope-discipline convention):

- **Per-workload identity (SPIFFE/SPIRE-style), replacing the fleet-wide shared secret.** The shared secret is identical for every sidecar↔core pair today; a single leak compromises the whole fleet. Fixing this properly means a new identity/issuance system, not a config flag.
- **Cross-region/cross-cluster global capacity coordination.** Each deployment's Redis is already correctly fleet-wide *within* one cluster (see Phase 2); coordinating *across* independent deployments needs either a shared cross-region Redis (reintroducing the latency problem Tier 4 was designed to avoid) or a new capacity-broker service — a new component and protocol, not a Lua script change.
- **Tier 3 token/dollar-weighted reservation.** Making the fleet shedder reserve *token throughput* instead of *concurrent-slot count* means swapping its counting primitive entirely (sorted-set concurrency counter → token-bucket-style counter) — a materially bigger, spec-worthy change than the Tier-1 cost-wiring fix in Phase 4.

## Versioning

The audit found four documents (`README.md`, `SECURITY.md`, `CHANGELOG.md`, GitHub Releases) each claiming a different "current version," none matching git's actual state. Phase 0 fixes that, then each subsequent phase ships as its own tagged, released version — partly because that's good practice, partly because it's the roadmap dogfooding the fix to the exact process gap it just closed.

Proposed mapping (adjust freely — the point is *a* deliberate number per phase, not this exact scheme):

| Version | Contents |
| --- | --- |
| v2.4.0 | Tag+release the 7 untagged commits already on `main` (self-throttle limiter — a new feature, so minor bump), plus merge `fix/v3-breaking-wire-changes`' `PRIORITY_UNSPECIFIED` enum renumbering into the same release with an explicit breaking-change note |
| v2.4.1 | Phase 0 (housekeeping — bugfixes + docs, patch bump) |
| v2.5.0 | Phase 1 (Observability foundation — new capability, minor bump) |
| v2.6.0 | Phase 2 (Reliability & Testing — new runtime behavior in Tier 4's shed ramp + HA Redis, minor bump) |
| v2.7.0 | Phase 3 (Security — new PERMISSIVE mode, additive, minor bump) |
| v2.8.0 | Phase 4 (SDK & API — new `cost` param + SDK helpers, additive, minor bump) |
| v2.9.0 | Phase 5 (Performance & DevEx — tooling/CLI, minor bump) |

None of these are breaking changes, so none force a v3.0.0 — despite this document's filename, "v3" here names the *roadmap*, not a promised major version.

---

## Phase 0 — Housekeeping & Quick Wins

**Goal:** ship every cheap, high-confidence fix first, regardless of category, and fix the versioning process before using version numbers for everything after.

1. **Fix Tier 2's bounded-queueing backlog counter to be Redis-backed.** `services/core/limiter/concurrency.go:37` (`backlog atomic.Int64`) is the *only* non-Redis local-state field anywhere in `services/core` — confirmed by repo-wide grep. With N core replicas the real backlog ceiling is `maxBacklog × N`, not one shared ceiling. Fix: replace it with a call to the *existing* `store.IncrConcurrent`/`DecrConcurrent` methods against a new key namespace (`"backlog:" + req.Key`), reusing the already-embedded `concurrent_limiter.lua` — no new Lua, no new `StateStore` method. (Tier 3's fleet cap and Tier 2's own per-key cap are *already* correctly Redis-coordinated across replicas — this is the one narrow gap, not a broader problem.)
2. **Add a cross-instance test proving the backlog fix works.** Two `ConcurrencyLimiter` instances sharing one store, hammered concurrently; assert the *combined* backlog across both never exceeds `maxBacklog`. The existing single-instance stress tests in `concurrency_queue_test.go` can't prove this.
3. **Fix `bench_run.go`'s silent-failure measurement bug.** Today, `Allow()`'s `allowed`/`err` and `Acquire()`'s `err` are discarded entirely in the hot loop — a rejected (429), an errored, and a fully successful request all land in the same P50/P99/P99.9 array with no distinguishing signal. Fix: check the return values, bucket into separate accepted/rejected/errored distributions.
4. **Establish one authoritative version source.** Add a `VERSION` file (or `git describe --tags` in CI) that `README.md`, `SECURITY.md`, and `CHANGELOG.md` generate from or link to. Today: README's Status line says "v1.0.0 — complete" while its own Comparison table marks v2.2.0 features shipped; SECURITY.md's Supported-Versions table lists only v1.0.x while its prose discusses v2.3.2 as already shipped; CHANGELOG.md was cut once for v1.0.0 and never again despite 7 later tags.
5. **Backfill `CHANGELOG.md`.** Cut real `[X.Y.Z]` sections for v1.0.1 through v2.3.2 by mining each tag's already-detailed annotated message (they're good — v2.3.1/v2.3.2's could be pasted in almost verbatim); move today's `[Unreleased]` content into a proper `[2.3.2]` heading since `SECURITY.md`'s own prose confirms that work already shipped.
6. **Publish the missing v2.3.2 GitHub Release** (content already exists in the annotated tag) and **tag `main`'s 7 pending commits** as v2.4.0 per the versioning table above.
7. **Document the v2.3.2 semver exception.** Its tag message self-describes a breaking `/release` header migration but shipped as a patch bump. Record this as an explicit, acknowledged exception so `CHANGELOG.md`'s "intends to follow Semantic Versioning" line stays a trustworthy signal going forward.
8. **Add `.github/dependabot.yml`** with ecosystem entries for all 6 Go module directories, `pip`, `github-actions`, and `docker`, weekly schedule, grouped. Today only the reactive, CVE-triggered "security updates" toggle is on (4 open PRs #61-64 prove it) — nothing catches ordinary staleness, which is exactly how `proto/go.mod`'s `x/sys`/`x/text` silently drifted from its siblings.
9. **Merge the 4 open Dependabot PRs in lockstep**, bumping the same dependency to the same version across core/sidecar/proto in one change (merging sidecar's PRs alone would newly introduce skew against core/proto). Separately run `go get golang.org/x/sys@v0.45.0 golang.org/x/text@v0.37.0` inside `proto/` to close its existing skew.
10. **Merge `fix/v3-config-validation`** (already exists, authored 2026-07-20, single clean commit) — adds Tier 1 `rate_limiter` config validation, matching what Tier 2/3 already have. This is exactly the "`Config.Validate()` never checks Tier 1's `default_rate`/`default_burst`" gap referenced throughout this roadmap's known-gaps context — it was already fixed and just never merged. Near-zero effort: review + merge, not re-implementation.
11. **Merge `fix/v3-breaking-wire-changes`** (already exists, authored 2026-07-20/21, two commits, closes GitHub issues #15 and #16) — adds a `PRIORITY_UNSPECIFIED = 0` sentinel to the proto `Priority` enum (renumbering `SHEDDABLE`/`CRITICAL` to 1/2, following Google's/Buf's protobuf style guide for zero-value enums) and deletes the dead `FleetShedderConfig.DefaultPriority` config field (parsed since v1.0.0, never consumed — the sidecar hardcodes its own default independently). Server-side behavior is unchanged (`PRIORITY_UNSPECIFIED` and `SHEDDABLE` both still map to `limiter.Sheddable`) — but this **is** a breaking wire-format change (enum values renumbered) and must ship with an explicit breaking-change note in the v2.4.0 release notes, not silently. Land this **before** any Phase 4 work that touches the SDK's `Priority` field, so Phase 4 builds on the corrected enum rather than the old one.

## Phase 1 — Observability Foundation

**Goal:** give `services/core` the self-instrumentation it's missing, before Phase 2's reliability tests need something to assert against ("fails open, *with an emitted signal*").

1. **Give `services/core` its own `/metrics` endpoint** (new listener, alongside the existing `:9091` health port) — gRPC request count/latency by method+status, Redis call latency/error count, config-reload success/failure counter. Today core has *zero* self-instrumentation; every visible Tier 1-3 metric is an indirect byproduct of the sidecar observing gRPC responses.
2. **Add `ratecap_fail_open_total{tier,reason}`.** Increment whenever Tier 1-3 fail open due to Redis/core unavailability; alert on any sustained nonzero rate. This is the single most safety-critical missing signal — a fail-open condition is, by design, invisible to normal rejection-rate dashboards.
3. **Export per-decision latency as a histogram.** `ratecap_decision_latency_seconds{tier}` in `services/sidecar/metrics/metrics.go`, recorded at the same `proxy.go` call sites that already compute `time.Since(start)` for `decisionlog`'s `latency_ms` field — the value already exists, it's just never exported as a metric.
4. **Make health checks reflect real state.** Sidecar's `/healthz` unconditionally returns 200 regardless of gRPC connectivity to core; core's `SetServingStatus` is set once at startup and never updated. Both are wired as k8s readiness *and* liveness probes — fix both to actually check Redis/core connectivity.
5. **Move `/metrics` off the self-throttled request path.** It currently shares the sidecar's mux and process-wide rate limiter with `/check`/`/release` — a Prometheus scrape can get 429'd by the same limiter throttling real traffic exactly when visibility matters most.
6. **Instrument `/release` and upstream gRPC failures.** `/release` has zero telemetry today beyond a `log.Printf` on failure; add a success/failure counter. Add `ratecap_upstream_errors_total{endpoint}` for the two `log.Printf`-only gRPC-failure sites in `proxy.go`, so a core outage is an alertable signal instead of a silent drop in decision rate.
7. **Ship a starter Grafana dashboard + basic alert rules.** `deploy/grafana/ratecap-overview.json` (decisions by tier/action, shadow-would-reject rate, worker in-flight) plus alerts for sustained 503 rate and near-cap worker in-flight — zero of this exists anywhere in the repo today, even for the metrics that already exist.
8. **Add an Observability section to `ARCHITECTURE.md`**, documenting the metrics contract (including the `QUEUE` action's metric label, currently only inline prose) and the health-check/tracing state as tracked, known limitations — `ARCHITECTURE.md` has zero mentions of observability anywhere today, despite being the doc most likely read before extending any tier.
9. **(Stretch — largest item in this phase) Add OpenTelemetry trace-context propagation across the sidecar-to-core gRPC hop.** OTel gRPC client/server interceptors injecting/extracting `traceparent` via gRPC metadata, plus baggage carrying the sidecar's resolved priority classification onto both the sidecar's CLIENT span and core's SERVER span. Zero tracing exists anywhere in the repo today; this is the only layer that would let a Tier 3 shedding incident be debugged with correlated sidecar+core visibility instead of unstructured, uncorrelated log lines. Materially larger than the rest of this phase — if Phase 1's timeline is tight, this item alone can slip to Phase 5 without blocking anything else here.

## Phase 2 — Reliability & Testing Hardening

**Goal:** convert RateCap's core architectural claims (fail-open, atomicity, no-flapping) from assumptions into tested invariants.

1. **Add Toxiproxy-based fail-open regression tests per Redis-dependent tier.** `TestTier1/2/3_RedisUnavailable_FailsOpen` — Toxiproxy in front of core's Redis in docker-compose, disable it, assert requests are *allowed* (with the new `ratecap_fail_open_total` signal from Phase 1 firing), not just "no panic." Fail-open on Redis failure is RateCap's core architectural claim, inherited from Stripe, and today it's completely unverified by any test.
2. **Make Redis itself HA.** Recommendation: **Sentinel**, not Cluster — RateCap's current key design (single `cc:fleet` key, plus per-key `rl:`/`cc:` keys with no multi-key Lua ops spanning different logical entities yet) has no sharding need, and Sentinel avoids the CROSSSLOT/hash-tagging constraints Cluster mode would impose for no present benefit. Replace `deploy/helm/ratecap/templates/redis.yaml`'s current single-replica, non-persistent Deployment. Tier 2/3's cross-replica correctness (already confirmed real, see Phase 0 item 1's note) currently rests entirely on one unreplicated pod. Revisit Cluster mode only if/when sharding becomes necessary — at that point, hash-tag `cc:`/`backlog:` keys per caller key (e.g. `{tenant:key}:inflight`, `{tenant:key}:queue`) and `cc:fleet` if it's ever split into separate critical/sheddable counters, so multi-key Lua ops stay single-slot.
3. **Document and test the per-tier Redis-down degradation contract.** State explicitly, per tier, fail-open vs. fail-closed (Stripe's own precedent: fail-open on request-rate, fail-closed on concurrent-requests) — currently undocumented anywhere in `ARCHITECTURE.md`/`SECURITY.md` despite being the easiest invariant to silently invert during a refactor.
4. **Add a race regression test for the priority-partition-at-capacity race.** Replicates Netflix `concurrency-limits`' real, recently-fixed bug class (PR #233/#234): N goroutines racing to acquire the last slots in a priority partition when the global limit is already at capacity, for Tier 2 and Tier 3. RateCap's Lua-script atomicity likely already prevents this — make it an explicit named test, not an assumption.
5. **Add a concurrent stress test for `services/sidecar/decisionlog`.** It has a real mutex-guarded global hit by every concurrent inbound request in production, but `decisionlog_test.go` never races two goroutines against it — the one file in the whole audit where `-race` runs in CI but can never actually trip.
6. **Add coverage measurement and a CI gate.** `-coverprofile` on every `go test ./... -race` in `ci.yml`'s matrix, a coverage job with a minimum floor, `coverage run` wrapping the Python SDK's `unittest` call. Zero coverage tooling exists today — a PR can ship a wholly-untested file and merge cleanly.
7. **Add property-based/model-based tests** (`pgregory.net/rapid`) driving random Acquire/Release/Refill sequences against the real limiter code and a trivial reference model, asserting token/concurrency/reserved-capacity invariants never diverge.
8. **Add mutation testing (Gremlins) as a PR-diff CI gate**, scoped to `services/core`'s Tier 1-4 algorithms and `packages/sdks/go`, tracking `LIVED` mutants on comparison operators in the shed-order ladder and refill math — the off-by-one/boundary bug class high line-coverage-but-shallow-assertion tests miss.
9. **Add fault-injection tests for the fsnotify config hot-reload path** — partial writes, atomic rename-swap, delete+recreate against the watched `ratecap.yaml` — asserting the engine never applies a half-parsed config and always falls back to last-known-good.
10. **Add a config-consistency safeguard across replicas.** Stamp each hot-reloaded config with a version/hash, expose via metrics/logs, so operators can detect replica divergence mid-rollout (config today loads per-replica from that replica's own local file, with no cross-replica check).
11. **Document and, if binary, ramp Tier 4's shedding curve gradually.** Stripe's own documented lesson: binary on/off shedding causes flapping across its 4-tier drop order. Confirm RateCap's actual curve; add gradual ramping if it's currently a hard cutoff.
12. **Add a sub-second incident-response lever for Tier 3's reservation percentage** (and, if useful, Tier 1's per-key limit) — a narrow admin gRPC/API call to instantly bump/lower `reserved_critical_pct` or a rate, modeled on Upstash's `setDynamicLimit` (one round trip, no config re-parse). Today the only lever is the full `ratecap.yaml` + fsnotify hot-reload cycle — on-call has no sub-second override during a live incident.

## Phase 3 — Security: mTLS PERMISSIVE Mode

**Goal:** add the migration rung Istio/Linkerd use to move a fleet without a flag day — RateCap currently jumps straight from "off" to "all-or-nothing strict," with no middle step.

1. **Add `RATECAP_TLS_MODE=off|permissive|strict`** (env var, not `ratecap.yaml`, consistent with how `RATECAP_SHARED_SECRET` is handled). Implement `permissive` on `services/core` as a *second* gRPC listener alongside the existing plaintext `:9090` (e.g. `RATECAP_GRPC_TLS_ADDR`) with `tls.Config{ClientAuth: tls.VerifyClientCertIfGiven}` — reusing the exact dual-listener pattern already established by the plaintext-health-server-on-`:9091` precedent in `main.go`. `off` stays the default; no behavior change for existing deployments. Sidecars migrate one at a time by pointing at the new port once they have certs; the `x-ratecap-shared-secret` interceptor stays active on both listeners throughout.
2. **Add plaintext-vs-TLS connection observability** *before* changing any default — a counter/log line recording, per accepted call, which listener it arrived on and whether a client cert was presented. This is the Istio "still-plaintext" Grafana-dashboard equivalent; without it, flipping a default is a blind cutover.
3. **Add a TLS SAN/hostname preflight check.** A `ratecapctl tls check <cert> <expected-host>` subcommand (or a check inside `tlsconfig.Load`) verifying the cert's SAN list against the address it'll serve/dial. The Helm chart's own `values.yaml` already documents this exact failure mode producing "no server-side log" — `config_validate.go` never touches the TLS env vars at all today.
4. **Wire certificate reload into the existing fsnotify watcher.** Reuse the same library already watching `ratecap.yaml` to watch cert/key paths and feed `tls.Config`'s `GetCertificate`/`GetConfigForClient` hooks — today certs load once in `main()` via `tls.LoadX509KeyPair`, so an externally-rotated cert (e.g. cert-manager) silently doesn't take effect until a pod restart.
5. **Add a NetworkPolicy template to the Helm chart**, restricting core's gRPC/health ports to the sidecar's pod selector and Redis's port to core's selector — operationalizes the network-boundary claim `SECURITY.md` already makes ("must run on private, trusted network only") at the Kubernetes level; only `configmap`/`core`/`redis`/`sampleapp`/`sidecar` templates exist today.
6. **Do not flip any shipped default in this phase.** Sequencing per every mature mesh's own docs: ship the mode with `off` still default → update demo/Helm defaults to `permissive` only *after* item 2's observability is live → flip the shipped default to `strict` only once that metric shows zero plaintext traffic across a full deploy cycle. Skipping straight to a new default repeats the exact flag-day risk mTLS-as-optional was meant to avoid.

## Phase 4 — SDK & API: Token-Cost Wiring

**Goal:** finish an already-designed feature rather than add a new mechanism. RateCap's Tier 1 substrate is *already* a generic variable-cost token bucket end-to-end (proto, Lua script, `RedisStore.CheckAndDecrement` all support arbitrary `cost`) — it's purely request-count-based today only because one line throws the value away.

1. **Wire up the existing `cost` plumbing.** Change `services/sidecar/proxy/proxy.go:84`'s hardcoded `Cost: 1` to read an optional `cost` query parameter (same pattern already used for `key`/`skip_reservations`), defaulting to 1 for backward compatibility. No proto, core, or Lua changes needed.
2. **Add an SDK helper to compute LLM-style token cost.** Go/Python SDK helper mirroring the AWS Bedrock/LiteLLM estimate (`cost = input_tokens + max_tokens`, or a caller-suppliable weighted formula), passed as the new `cost` param — keeps `ratecap-core` transport/schema-agnostic (never parses any LLM provider's response JSON).
3. **Add a bounded refund/reconciliation endpoint for Tier 1.** Extend `/release` (or a sibling endpoint) to accept an optional `actual_cost`, implemented as a *new* bounded Lua script that clamps to `burst` on write — not a reuse of `CheckAndDecrement` with a negative cost, since `token_bucket.lua` only re-clamps to `burst` on the *next* call's refill computation. This ports the Bedrock/LiteLLM "reserve estimate upfront, refund unused at completion" pattern onto the check→release round trip both SDKs already perform.
4. **Add Cost/Priority parameters to the Go SDK's check call.** Today every call implicitly uses `cost=default`, `Priority=SHEDDABLE`; without this, no Go caller can exercise the new `cost` parameter at all.
5. **Add timeout, retry/backoff, and TLS support to the Python SDK.** It currently has zero of the three — a hung sidecar call blocks the Python client indefinitely with no recourse.
6. **Add shared contract tests between the Go SDK, Python SDK, and the sidecar's actual wire protocol.** A golden-fixture suite asserting both SDKs' `/check`/`/release` requests match the sidecar's real expected format — today each SDK independently reimplements the protocol with nothing to catch drift.
7. **Emit IETF `draft-ietf-httpapi-ratelimit-headers`-compliant response headers**, alongside or instead of any legacy `X-RateLimit-*` headers — becoming baseline interoperability in 2026 (Kong, Cloudflare, and even hobby-scale Go rate limiters already emit it).
8. **Add a sidecar-local negative cache for already-denied identifiers**, modeled on Upstash's `ephemeralCache` — a cheap p99 win under sustained abuse floods, complementary to the existing Tier 4 shedder, no core/proto changes required.

## Phase 5 — Performance & DevEx Polish

**Goal:** close the remaining benchmark, tooling, and dependency-hygiene gaps.

1. **Publish a second benchmark run against un-loosened, shipped default limits.** The published README numbers use `deploy/ratecap-bench.yaml`, which raises every tier's limits 100-1000x above shipped defaults — essentially nothing gets rejected, so the numbers measure only the never-limited happy path, never the load-shedder actually doing its job. Add the second run; relabel the existing numbers as "headroom/pass-through overhead only."
2. **Add a duration/soak mode with a streaming histogram.** `--duration` on `bench run`, periodic windowed percentiles, replacing the unbounded mutex-guarded `[]time.Duration` slice with a bounded streaming histogram — today an hour-long run would retain tens of millions of raw durations with no bound and no way to catch GC/connection-leak drift.
3. **Wire `net/http/pprof` into core and sidecar behind a flag**, off by default in production images — zero pprof/expvar exists anywhere today, so no bench run can be paired with a live profile without first patching the binaries.
4. **Capture resource usage alongside benchmark runs** (docker stats + Redis `INFO`/latency stats before/after) — today the benchmark captures only client-observed RTT, nothing about CPU/memory/Redis cost.
5. **Add a nightly CI benchmark regression job**, failing/flagging on a percentage regression vs. the last baseline — the README already claims "regression-tracking over time," but zero automation does that today.
6. **Add `golangci-lint` to CI** alongside Phase 2's coverage gate — `gofmt` is the only lint gate today, leaving bug classes like the discarded-return-value pattern this very audit found in `bench_run.go` unguarded.
7. **Isolate `testcontainers-go` behind a build tag in `services/core`**, so its ~40-package transitive tree (`moby/*`, `containerd/*`, `gopsutil`) stops inflating the production module's tracked dependency graph purely to support Docker-based Redis integration tests.
8. **Generate the Helm chart's inline `config.yaml` from `deploy/ratecap.yaml`** (small script/Makefile target) instead of hand-copying, and document the missing `concurrencySigningKey` secret in the Helm README — today install fails with `CreateContainerConfigError` with no pointer to why.
9. **Add CLI entrypoint tests** for `cli/main.go`/`cli/cmd/root.go`/`cli/cmd/bench.go` (only the child commands have tests today), and complete the already-known bench-tool gaps (`--qps`/pacing, `--version`, accept/reject/error counters).
10. **Pin GitHub Actions to SHAs and Docker base images to digests**, closing the last of the dependency-drift gaps Phase 0's `dependabot.yml` doesn't cover on its own.

---

## Implementation Approach

Implementation will use dynamic multi-agent workflows / specialized agent teams per phase (per user request) rather than one linear pass — the exact agent/workflow assignment per phase is an implementation-plan concern, worked out next via the `writing-plans` skill, not this design spec.

## Dependencies Between Phases

- Phase 2's fail-open tests assert against Phase 1's `ratecap_fail_open_total` signal — Phase 1 must land first.
- Phase 4's SDK contract tests benefit from Phase 2's coverage-gate infrastructure existing, but aren't blocked by it.
- Phase 0's version-source-of-truth fix should land before any phase ships a tagged release, so every subsequent phase's tag is trustworthy from the start.
- All other phases are independent of each other and could be reordered without breaking a dependency, though the impact/effort balance favors the order above (cheapest and most safety-critical first).
