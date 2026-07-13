# RateCap

A faithful, open-source recreation of [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters), built as a hybrid core-engine + sidecar system.

## Status

v1 walking skeleton: Tier 1 (Request Rate Limiter) is implemented end-to-end. Tiers 2-4 (Concurrent Requests Limiter, Fleet Usage Load Shedder, Worker Utilization Load Shedder) are planned next — see `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`.

## Architecture

```
App -> SDK -> sidecar (local) -> core (gRPC) -> Redis (Lua token bucket)
```

## Quick start

```bash
cd deploy
docker compose up --build
curl http://localhost:3000/checkout   # repeat 6+ times to see a 429
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
