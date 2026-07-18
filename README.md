# RateCap

[![CI](https://github.com/sairam0424/RateCap/actions/workflows/ci.yml/badge.svg)](https://github.com/sairam0424/RateCap/actions/workflows/ci.yml)

A faithful, open-source recreation of [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters), built as a hybrid core-engine + sidecar system.

## Status

**v1.0.0 — complete.** All four of Stripe's tiers are implemented, live-e2e-verified, and audited end-to-end: Tier 1 (Request Rate Limiter), Tier 2 (Concurrent Requests Limiter), Tier 3 (Fleet Usage Load Shedder), Tier 4 (Worker Utilization Load Shedder). See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the full design and `CHANGELOG.md` for what shipped in each tier.

## Architecture

```
App -> SDK -> sidecar (local) -> core (gRPC) -> Redis (tiers 1-3)
                 |
                 +-> tier 4: local in-flight cap, no round-trip
```

## Quick start

```bash
cd deploy
docker compose up --build
curl http://localhost:3000/checkout             # repeat 6+ times to see a 429  (tier 1)
curl http://localhost:3000/slow-report           # repeat concurrently to see a 429 (tier 2)
curl "http://localhost:3000/fleet-demo?priority=sheddable"   # repeat concurrently to see a 503 (tier 3)
curl http://localhost:3000/worker-demo           # repeat concurrently to see a 503 (tier 4)
```

## Project layout

- `proto/` — gRPC contract (source of truth for all SDKs)
- `services/core/` — central engine: limiter logic, Redis state, config hot-reload
- `services/sidecar/` — local proxy: priority resolution, shadow-mode override
- `packages/sdks/go/` — thin Go client SDK
- `deploy/` — docker-compose demo and sample app

## Comparison

RateCap is the only project among these six that implements all four of Stripe's original rate-limiter/load-shedder tiers plus bounded queueing as one open-source system. The table below compares RateCap's five mechanisms against five widely-used systems — Envoy, Kong, Cloudflare, Alibaba Sentinel, and Netflix's `concurrency-limits` library — based on each system's own official documentation or source, not secondary claims. "Not verified" means a confident answer wasn't found in a targeted official-source check, not that the mechanism is confirmed absent.

| Mechanism | RateCap | Envoy | Kong | Cloudflare | Alibaba Sentinel | Netflix `concurrency-limits` |
| --- | --- | --- | --- | --- | --- | --- |
| Token-bucket rate limiting (Tier 1) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |
| Concurrency (in-flight request) limiting (Tier 2) | ✅ | Not verified | Not verified | Not verified | Not verified | ✅ [1] |
| Bounded queueing (Tier 2, v2.2.0) | ✅ | Not verified | Not verified | Not verified | ✅ [2] | ✅ [1] |
| Fleet-wide (global) load shedding w/ reserved capacity (Tier 3) | ✅ | Not verified [3] | Not verified | Not verified [3] | Not verified | ❌ [1] |
| Local/worker-utilization load shedding (Tier 4) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |

### Sources

1. Already established in this repo's own v2 Phase 3 design work: Netflix's `concurrency-limits` is fundamentally a concurrency-limiting library with a `LifoBlockingLimiter` (hard-capped backlog, genuine bounded queueing) — but is explicitly local/per-instance only ("each node will adjust and enforce its local view of the limit"), with no fleet-wide coordination. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md` and [Netflix/concurrency-limits](https://github.com/Netflix/concurrency-limits).
2. Already established in this repo's own v2 Phase 3 design work: Alibaba Sentinel's `ThrottlingController.java` "uniform queueing" behavior parks the calling thread until its turn, rejecting only past a configured max wait — genuine queueing, not instant-reject. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.
3. This repo's own prior research (see the v2 roadmap plan) already established that Cloudflare and Envoy Gateway both explicitly avoid cross-DC/cross-instance counter synchronization in their "global" rate-limiting features — evidence against a fleet-wide-coordinated-state mechanism like RateCap's Tier 3, though not yet a confirmed final verdict for either system's specific reserved-capacity-for-priority-traffic question.

## Design docs

- [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md) — full v1 design
- [`docs/superpowers/plans/2026-07-13-walking-skeleton.md`](docs/superpowers/plans/2026-07-13-walking-skeleton.md) — this implementation's plan
