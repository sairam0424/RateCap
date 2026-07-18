# RateCap v2 Phase 4b: Published Benchmarks — Design Spec

**Date:** 2026-07-18
**Status:** Approved
**Context:** Second of 3 independently-shippable sub-projects under Phase 4 (Production-Readiness & Adoption), the fourth and final phase of the v2 roadmap. Phase 4a (Comparison table) already shipped via PR #35. Phase 1 (observability, mTLS) shipped as v2.0.0, Phase 2 (`ratecapctl` CLI, Python SDK) shipped as v2.1.0, Phase 3 (bounded queueing) shipped as v2.2.0.

Unlike Phase 3, this sub-project adds no new enforcement mechanism and touches no core decision logic — it is a data-collection-and-documentation task. It does not trigger this repo's `CLAUDE.md` sign-off gate; routine spec-review applies.

## Problem

The v1 design spec's own "Testing Strategy" section promised: *"Benchmark suite (`ratecapctl bench`): hammers the sidecar, produces the P99/P999 latency numbers published in the README — this is the concrete 'beast mode = build quality' proof point, not a scope addition."* `ratecapctl bench run` shipped in Phase 2 (v2.1.0), but no numbers were ever actually run and published — `README.md` says nothing about latency or throughput today. This is the second of the two concrete adoption gaps the original deep-research pass found via direct repo inspection (the first, no comparison table, was closed by Phase 4a).

A naive `bench run` against the existing `deploy/docker-compose.yml` stack would not deliver on that promise: the demo's default config is deliberately tiny — `concurrency_limiter.default_max_concurrent: 3`, `fleet_shedder.default_max_concurrent: 5`, and the sidecar's `RATECAP_MAX_INFLIGHT_REQUESTS: "3"` — sized for curl-based walkthroughs where a human triggers a visible 429/503 by hand. Running a real concurrent load generator against those limits would mostly measure how fast RateCap rejects requests once concurrency exceeds single digits, not RateCap's actual per-request processing overhead. Publishing that as "RateCap's benchmark numbers" would be misleading in the opposite direction from what the original promise intended (proving build quality, not proving how quickly it can reject).

## Key Design Decisions

### 1. A separate benchmark-specific config, not a change to the demo's defaults

New file: `deploy/ratecap-bench.yaml`. Same shape as `deploy/ratecap.yaml`, with every tier's cap raised well above the benchmark's own concurrency level, so the run measures genuine per-request latency rather than artificial-rejection latency once a tier's cap is hit. Concretely, planned values (final numbers locked at plan-writing time, chosen to comfortably exceed the benchmark's concurrency setting from decision §2 below):

- `rate_limiter.default_rate` / `default_burst` raised well above the benchmark's total request volume, so Tier 1 never legitimately rejects during the run.
- `concurrency_limiter.default_max_concurrent` and `fleet_shedder.default_max_concurrent` raised well above the benchmark's `--concurrency` worker count, so Tier 2/Tier 3 never legitimately reject during the run.
- `queueing_enabled` left at its default `false` — bounded queueing is Tier 2's own separate, already-benchmarked-by-its-own-tests mechanism; this benchmark measures the unqueued fast path.

This is a new file, not an edit to `deploy/ratecap.yaml` — the default demo experience (visible 429s/503s from `curl`-ing the sample app a few times, the exact walkthrough `README.md`'s Quick start section already documents) is unaffected. `deploy/docker-compose.yml` is left untouched; a new `deploy/docker-compose.bench.yml` override file swaps the `core` service's mounted config to `ratecap-bench.yaml` and raises the `sidecar` service's `RATECAP_MAX_INFLIGHT_REQUESTS` env var (Tier 4 is sidecar-local and doesn't read `ratecap.yaml`, so it needs its own override here). Running the benchmark stack becomes an explicit `docker compose -f docker-compose.yml -f docker-compose.bench.yml up --build` — Docker Compose's native multi-file layering, not a sed script or a manually-edited copy — so the override is copy-pasteable, diffable, and never risks silently drifting from the base compose file's `redis`/`sidecar`/`sampleapp` service definitions.

The sidecar's `RATECAP_MAX_INFLIGHT_REQUESTS` raise is the only Tier 4 change needed; every other override in `docker-compose.bench.yml` is the single `core` service's volume-mount swap from `ratecap.yaml` to `ratecap-bench.yaml`.

### 2. Benchmark both the Tier 1 path and the Tier 2 path — two separate result sets

`ratecapctl bench run` already supports both: its default path calls `Allow()` (Tier 1 only, no reservation), and `--acquire` calls `Acquire()` followed by `Ticket.Release()` (Tier 2's full reserve-then-release round trip, which necessarily also passes through Tier 1 and Tier 3 ahead of it in the pipeline). Running both, with the same `--concurrency`/`--requests` settings otherwise held constant, shows the real cost delta between a single-tier check and a multi-tier check-plus-release — a more informative comparison than either number alone, and zero new tool code since both paths already exist in the shipped CLI.

### 3. One-time, dated snapshot — not implied to be continuously accurate

The published numbers are a snapshot from one benchmark run on one date, not a live or periodically-refreshed measurement. `README.md`'s new section states this explicitly: the exact date the benchmark was run, a description of the machine/environment it ran on (single-machine docker-compose, not representative of a distributed production deployment's network/hardware), and the exact reproduction commands (the compose-override invocation from §1, then the two `ratecapctl bench run` invocations from §2) — so a reader can judge the numbers' relevance for their own use case, or re-run them fresh, rather than trusting an unqualified claim. This mirrors the honesty standard Phase 4a's Comparison table already set for itself (citing every claim, marking genuine gaps as "Not verified" rather than guessing).

The write-up explicitly states what these numbers do and do not represent: useful for relative/directional comparison and regression-tracking over time (e.g. "does a future change make Tier 2's overhead meaningfully worse"), not a production capacity-planning number (real deployments have network hops, real hardware variance, and real traffic shapes this single-machine same-host setup doesn't capture).

### 4. Placement: new `## Benchmarks` section, immediately after `## Comparison`

`README.md`'s current section order (`## Status` → `## Architecture` → `## Quick start` → `## Project layout` → `## Comparison` → `## Design docs`) gains one new section between `## Comparison` and `## Design docs`. This keeps the two "why should I trust/adopt this" sections adjacent — a reader evaluating RateCap sees the mechanism comparison and the performance numbers back-to-back, immediately before the deep-dive design-doc links.

### 5. Content shape

- A short intro paragraph: what's measured (Tier 1 `Allow()` path, Tier 2 `Acquire()`/`Release()` path), what environment it ran on, and the explicit "directional/regression-tracking, not capacity-planning" caveat from §3.
- The exact date, machine/environment description, and exact reproduction commands (compose override + both `bench run` invocations), so the whole section is independently reproducible.
- Two result tables (Tier 1, Tier 2), each with: Total requests, Elapsed, Throughput (req/s), P50, P99, P99.9 — the exact fields `ratecapctl bench run` already reports, no new fields invented.

## Out of Scope

- Any change to `services/core`, `services/sidecar`, `packages/sdks/go`, `proto/`, or `cli/` — this is a `README.md` + `deploy/` config addition only, zero code changes.
- Any change to `deploy/ratecap.yaml` or `deploy/docker-compose.yml`'s default behavior — the demo's default curl-walkthrough experience is unaffected; the benchmark config is additive and explicitly opt-in.
- A continuously-refreshed or CI-driven benchmark (e.g. running on every PR, publishing a live dashboard) — this is a one-time, dated, manually-reproducible snapshot per §3.
- Benchmarking Tier 3 or Tier 4 in isolation, or bounded queueing — `bench run`'s existing two modes (`Allow()`, `--acquire`) are the only paths exercised; adding new CLI flags or benchmark modes is out of scope for this sub-project.
- Multi-machine, network-realistic, or production-representative benchmarking infrastructure — explicitly a single-machine, same-host measurement per §3's honesty framing.
