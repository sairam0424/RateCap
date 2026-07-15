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

## Design docs

- [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md) — full v1 design
- [`docs/superpowers/plans/2026-07-13-walking-skeleton.md`](docs/superpowers/plans/2026-07-13-walking-skeleton.md) — this implementation's plan
