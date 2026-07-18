# RateCap v2 Phase 4a: Comparison Table — Design Spec

**Date:** 2026-07-18
**Status:** Approved
**Context:** First of 3 independently-shippable sub-projects under Phase 4 (Production-Readiness & Adoption), the fourth and final phase of the v2 roadmap. Phase 1 (observability, mTLS) shipped as v2.0.0, Phase 2 (`ratecapctl` CLI, Python SDK) shipped as v2.1.0, Phase 3 (bounded queueing) shipped as v2.2.0. Phase 4 was decomposed into 3 sub-projects (comparison table, published benchmarks, Helm chart) rather than one combined spec, since each has an independent risk profile and none blocks the others. This spec covers the comparison table only.

Unlike Phase 3, this sub-project adds no new enforcement mechanism and touches no core decision logic — it is pure documentation. It does not trigger this repo's `CLAUDE.md` sign-off gate; routine spec-review applies.

## Problem

The original deep-research pass (prior session) confirmed a concrete, self-verified adoption gap via direct repo inspection: RateCap has zero stars/forks, no Helm chart, no benchmarks, and no comparison table anywhere in the repo. An adopter evaluating RateCap against Envoy, Kong, Cloudflare, Alibaba Sentinel, or Netflix's `concurrency-limits` library has no way to see, at a glance, what RateCap actually offers that those don't (or vice versa). `README.md` today says nothing about how RateCap relates to any of them.

## Key Design Decisions

### 1. Placement: new `## Comparison` section in `README.md`

Inserted after the existing `## Project layout` section and before `## Design docs` — the point in the README where an adopter has already seen what RateCap does (Status, Architecture, Quick start, Project layout) and is deciding whether to keep reading the design docs or look elsewhere. This is a new top-level `##` section; no existing section is restructured.

### 2. Shape: one table, rows = RateCap's 5 mechanisms, columns = the 5 comparison targets + RateCap itself

Rows:
1. Tier 1 — token-bucket rate limiting
2. Tier 2 — concurrency (in-flight request) limiting
3. Tier 2 — bounded queueing (v2 Phase 3)
4. Tier 3 — fleet-wide (global) load shedding
5. Tier 4 — local/worker-utilization load shedding

Columns: RateCap, Envoy, Kong, Cloudflare, Alibaba Sentinel, Netflix `concurrency-limits`.

This mechanism-focused shape (rather than a broader feature checklist — language, license, star count, etc.) was chosen because it demonstrates RateCap's actual differentiator directly: no single one of the other five systems bundles all 5 of these mechanisms in one project. A feature checklist would bury that signal under adoption-metric rows RateCap can't yet compete on (stars, production users) and that are not RateCap's design story.

### 3. Cell values: verified facts only, three possible values

- **✅** — the system has a verifiable equivalent mechanism. Every ✅ cell must cite the specific verified fact backing it (a short inline note or footnote referencing the source), not just an unqualified checkmark.
- **❌** — the system explicitly does not have this mechanism, per a verified source (e.g. Cloudflare/Envoy Gateway explicitly avoiding cross-DC counter sync is evidence for a `❌`-with-caveat on global/fleet shedding, not silence).
- **Not verified** — no confident answer was found. This is an honest, explicit table value — never silently omitted, never guessed. A "not verified" cell is not a failure of the table; it is the table being honest about the limits of what a lightweight verification pass can confirm about a competitor's live production internals.

No other cell values (no partial credit, no "~", no prose paragraphs inside cells). Keeping the value set to exactly these three keeps every cell independently checkable by a reader without needing to parse a bespoke scale.

### 4. Sourcing: reuse already-verified facts first, light targeted lookups second

This repo already carries adversarially-verified facts about several of these cells, surfaced across two prior design specs and the v2 roadmap doc itself:

- **Sentinel** — `ThrottlingController.java`'s "uniform queueing" behavior is genuine leaky-bucket-style queueing (parks the calling thread, rejects only past a max wait) — verified via direct source inspection (`docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`). This directly answers the bounded-queueing row for Sentinel: **✅**.
- **Netflix `concurrency-limits`** — ships a `LifoBlockingLimiter` with a hard-capped backlog (default 100) — same source. Answers the bounded-queueing row for Netflix: **✅**. Its LIFO/backlog design is fundamentally a concurrency-limiting library, so the Tier 2 concurrency-limiting row is also **✅**; it does not implement fleet-wide or per-request-cost rate limiting, so Tier 1 and Tier 3 rows are **not verified** unless a quick check finds otherwise (this library is narrowly scoped, so "not verified" rather than a guessed ❌ is the honest default absent a direct check).
- **Cloudflare / Envoy Gateway** — both explicitly avoid cross-DC/cross-instance counter sync in their "global" rate-limiting features (v2 roadmap doc, `gentle-mixing-kettle.md`). This is direct evidence for how their fleet-wide-shedding equivalent (if any) actually behaves — cite this explicitly wherever the fleet-wide-shed row touches either system, rather than treating "has a global feature" and "coordinates fleet-wide state the way RateCap's Tier 3 does" as the same claim.
- **Kong** — fixed/sliding-window algorithms only, no GCRA production use (roadmap doc) — this is evidence about *algorithm choice*, not directly about which of the 5 mechanism rows Kong has equivalents for. Relevant as supporting color, not a row-filling fact on its own.
- **Envoy's `ratelimit` service** — proven mTLS/env-var-driven cert pattern (roadmap doc, feeds Phase 1) — this is about transport security, not a comparison-table row; not used here.

Every other cell (there are 30 total: 5 mechanisms × 6 columns, minus the RateCap column which is self-evident from this repo's own docs, so 25 cells actually need sourcing) starts as **not verified** by default. During implementation, the person/agent writing the table does one light, targeted lookup per still-unverified cell — checking that system's official docs or source directly, not a secondary blog post — and either fills in a sourced ✅/❌ or leaves it "not verified" if the official docs don't give a confident answer within a single focused check. This is explicitly NOT a second deep-research pass (no adversarial multi-agent verification, no fan-out) — it is a single-source confirmation check, scoped small on purpose because this is a documentation task, not a research project. If official docs are ambiguous or silent, the cell stays "not verified" — never resolved by inference or by what "seems likely."

### 5. Framing text above the table

One short paragraph (2-4 sentences) immediately before the table, stating RateCap's differentiator plainly and factually: it is the only one of these six that implements all 4 of Stripe's original rate-limiter/load-shedder tiers plus bounded queueing as one open-source project — a claim the table itself substantiates, not marketing copy asserting it. No adjectives like "powerful," "enterprise-grade," "best-in-class." The claim is exactly as strong as the table's own verified cells support and no stronger.

## Out of Scope

- Any change to `services/core`, `services/sidecar`, `packages/sdks/go`, `proto/`, or `cli/` — this is a `README.md`-only change.
- Adoption-metric rows (GitHub stars, production-user counts, license comparison) — deliberately excluded per the mechanism-focused shape decided in §2.
- A second deep-research pass or multi-agent adversarial verification — per §4, this uses light single-source lookups only.
- Any comparison target beyond the 5 named in the v2 roadmap (Envoy, Kong, Cloudflare, Alibaba Sentinel, Netflix `concurrency-limits`).
