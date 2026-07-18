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
| Token-bucket rate limiting (Tier 1) | ✅ | ✅ [1] | ❌ [2] | ❌ [3] | ❌ [4] | ❌ [5] |
| Concurrency (in-flight request) limiting (Tier 2) | ✅ | ✅ [6] | ❌ [2] | ❌ [3] | ✅ [7] | ✅ [5] |
| Bounded queueing (Tier 2, v2.2.0) | ✅ | ❌ [1] | ✅ [8] | ❌ [3] | ✅ [9] | ✅ [5] |
| Fleet-wide (global) load shedding w/ reserved capacity (Tier 3) | ✅ | Not verified [10] | Not verified [11] | Not verified [3] | Not verified [12] | ❌ [5] |
| Local/worker-utilization load shedding (Tier 4) | ✅ | ✅ [13] | Not verified [2] | Not verified [14] | ✅ [15] | Not verified [5] |

### Sources

1. Envoy's local rate limit filter uses a genuine token-bucket algorithm, entirely local to the Envoy instance/connection — no remote call. [Local rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter). Envoy's separate global rate-limit filter and its work-in-progress rate-limit-quota filter both defer the algorithm to an external service and reject instantly on overflow — neither documents queueing/delay behavior. [Rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter).
2. Kong's official Rate Limiting and Rate Limiting Advanced plugins use fixed-window or sliding-window request counters, never token-bucket, and neither Kong plugin limits concurrent/in-flight requests or sheds load based on local worker utilization — both are time-window-based. [Rate Limiting plugin](https://developer.konghq.com/plugins/rate-limiting/), [Rate Limiting Advanced plugin](https://developer.konghq.com/plugins/rate-limiting-advanced/). (Kong's `nginx_events_worker_connections` setting caps simultaneous nginx worker connections at the infrastructure level — it is not an application-level, per-key concurrency limiter or adaptive utilization-based shedder like RateCap's Tier 2/Tier 4.)
3. Cloudflare's rate limiting rules use a fixed-period request (or complexity-score) counter, not token-bucket, with no concurrency dimension and no documented bounded-queueing/delay mechanism. [Rate limiting rules](https://developers.cloudflare.com/waf/rate-limiting-rules/), [Rate limiting parameters](https://developers.cloudflare.com/waf/rate-limiting-rules/parameters/). (Cloudflare Queues is a separate, general-purpose message-queue product for Workers, unrelated to the WAF rate-limiting rules covered here — not conflated in this table.) No fleet-wide reserved-capacity-for-priority-traffic feature was found in these docs.
4. Sentinel's "Rate limiter" flow-control strategy is explicitly leaky-bucket ("allows requests to pass at a uniform rate"), not token-bucket. [Flow control docs](https://sentinelguard.io/en-us/docs/flow-control.html).
5. Already established in this repo's own v2 Phase 3 design work, extended with a fresh check of the same source for the remaining rows: Netflix's `concurrency-limits` frames its design as explicitly moving away from RPS/time-window thinking toward concurrency-based limits (no token-bucket/fixed-rate component), is explicitly local/per-instance only with no fleet-wide coordination ("each node will adjust and enforce its local view of the limit"), and its Vegas/Gradient2 algorithms adapt to measured request latency rather than an explicit system-load/CPU signal — related to but not the same shape as Sentinel's or RateCap's Tier 4, so left "Not verified" rather than forced into ✅/❌. [Netflix/concurrency-limits](https://github.com/Netflix/concurrency-limits).
6. Envoy's cluster circuit breakers (`max_connections`, `max_pending_requests`, `max_requests`) cap concurrent/parallel requests to an upstream cluster — a genuine concurrency limit, though scoped per-cluster/upstream rather than per-request-key like RateCap's Tier 2. [Circuit breaking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking).
7. Sentinel's "thread count" flow-control mode is documented as semaphore isolation — a direct concurrency limit. [Sentinel wiki home](https://github.com/alibaba/sentinel/wiki), [Migration from Hystrix](https://github.com/alibaba/Sentinel/wiki/Guideline:-Migration-from-Hystrix-to-Sentinel).
8. Kong's Rate Limiting Advanced plugin (3.12+) supports a genuine bounded-queue throttling mode: over-limit requests are held and retried up to `config.throttling.retry_times` times, bounded by `config.throttling.queue_limit`, before falling back to a `429` — structurally similar to RateCap's own bounded-backlog-plus-max-wait design. [Rate Limiting Advanced plugin](https://developer.konghq.com/plugins/rate-limiting-advanced/).
9. Already established in this repo's own v2 Phase 3 design work: Alibaba Sentinel's `ThrottlingController.java` "uniform queueing" behavior parks the calling thread until its turn, rejecting only past a configured max wait — genuine queueing, not instant-reject. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.
10. Envoy's global rate-limit filter supports a per-descriptor "limit override," which overrides a static quota value for matching traffic — not the same as reserving capacity for high-priority traffic ahead of lower-priority traffic, and no such reserved-capacity mechanism is documented. [Rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter). This repo's own prior research (v2 roadmap plan) already established that Envoy Gateway's "global" rate-limiting explicitly avoids cross-DC counter synchronization.
11. Kong's `cluster`/`redis` rate-limiting strategies synchronize a single shared counter across nodes — fleet-wide coordination of one limit — but no priority-tiered reserved-capacity mechanism analogous to RateCap's Tier 3 is documented. Same sources as footnote 2.
12. Sentinel's Cluster Flow Control genuinely coordinates a shared limit across a service cluster (its docs describe limiting "the frequency of one invocation to 10 per second in total" across 50 instances) — a real fleet-wide mechanism, unlike Cloudflare/Envoy's explicitly local-only "global" features. However, no priority-tiered reserved-capacity feature (critical vs. sheddable traffic, as RateCap's Tier 3 implements) is documented alongside it, so this is left "Not verified" for the full Tier-3-equivalent mechanism rather than forced into ✅/❌. [Cluster flow control](https://sentinelguard.io/en-us/docs/cluster-flow-control.html).
13. Envoy's local rate limit filter operates entirely per-Envoy-instance or per-connection with no remote call — architecturally local like RateCap's Tier 4 — though its trigger is a rate/token-bucket signal rather than system/CPU utilization. Same source as footnote 1.
14. Cloudflare operates as an edge/CDN network rather than a per-instance service process, so a "local worker utilization" comparison doesn't map cleanly onto its architecture; no equivalent mechanism was found in the official rate-limiting docs checked (same sources as footnote 3).
15. Sentinel's System Adaptive Protection sheds load based on local system metrics (`load1`, CPU usage, and locally-tracked QPS/response-time), computed entirely within the instance applying the rule — no remote call. [System adaptive protection](https://sentinelguard.io/en-us/docs/system-adaptive-protection.html).

## Design docs

- [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md) — full v1 design
- [`docs/superpowers/plans/2026-07-13-walking-skeleton.md`](docs/superpowers/plans/2026-07-13-walking-skeleton.md) — this implementation's plan
