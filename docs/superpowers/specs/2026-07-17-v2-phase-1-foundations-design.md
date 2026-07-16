# RateCap v2 Phase 1: Foundations — Design Spec

**Date:** 2026-07-17
**Status:** Approved
**Context:** First of a 4-phase v2 roadmap, following 3 adversarially-verified deep-research passes (322 subagent calls) and direct codebase exploration against `origin/main`. Covers Prometheus observability, mTLS on the sidecar-to-core gRPC hop, and documentation hygiene — the lowest-risk, fully-additive items in the roadmap (no wire-format changes, no new limiting mechanism, so this phase does not trigger `CLAUDE.md`'s "5th mechanism" sign-off gate the way Phase 3's bounded-queueing work will).

---

## Problem

RateCap v1 (all four of Stripe's tiers) shipped and is tagged through v1.0.1. Two gaps from the original v1 design spec were never closed:

1. **Observability.** The v1 spec sketched Prometheus+OTel metrics (`ratecap_utilization_ratio{tier,key}`, `ratecap_shed_total{tier,reason}`, etc.) and a Grafana dashboard, but zero metrics code exists anywhere in the repo today. Separately, tracked issue **#21** ("Tier 4: zero logging on the real shed path") notes that shadow-mode logging exists but real-path decisions produce no log signal at all.
2. **Transport security.** `SECURITY.md`'s "Network Transport Security (v1)" section explicitly states the sidecar-to-core gRPC hop is plaintext, authenticated only by a shared secret, and that "TLS/mTLS for this hop is deferred to v2." This is a real, documented gap for any team considering RateCap for production use on a network they can't fully isolate.

Research findings that reshape the original spec's approach (see `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the v1 baseline; this document supersedes its Observability and Configuration/network-security sections for v2):

- Both Prometheus's own naming guidance and Envoy Gateway's own docs explicitly warn that unbounded per-key label values (client IPs, user IDs) cause Prometheus cardinality explosion. The v1 spec's `{tier,key}`-labeled metrics would hit this immediately at any real scale — this design corrects that.
- Envoy's own `ratelimit` service is real, current, verified precedent for exactly the TLS/mTLS mechanism this design adopts: env-var-driven cert/CA paths (`GRPC_SERVER_USE_TLS`, `GRPC_SERVER_TLS_CERT`/`_KEY`, `GRPC_CLIENT_TLS_CACERT`), with mTLS layered on top of server-side TLS rather than independent of it.
- SPIFFE/SPIRE has genuine massive-scale production precedent (Uber, Square) but Uber's own retrospective shows library-based adoption stalling at ~10% before they moved to a different model — too heavy for RateCap's current scale. Static certs, matching Envoy's proven low-effort pattern, are the right scope for this phase. SPIFFE/SPIRE remains explicitly out of scope, not just deferred casually.

## Key Design Decisions

### 1. Metrics: `prometheus/client_golang`, sidecar-side only, no per-key labels

**Library:** `prometheus/client_golang` — the de facto standard Go Prometheus client, zero conflicting dependencies, no OpenTelemetry SDK needed for this phase (OTel adds a vendor-neutral abstraction layer RateCap doesn't need yet; the original spec's "Prometheus + OTel" framing is narrowed to Prometheus-only for v2).

**Location:** sidecar only. Every tier's decision already flows through `services/sidecar/proxy/proxy.go`'s existing `Action`-to-HTTP-status switch — this is the single point every decision passes through today, and the sidecar already has an `http.NewServeMux()` to mount `/metrics` on (`services/sidecar/main.go`). `ratecap-core` is gRPC-only with no existing HTTP surface; adding one solely for metrics is new surface area for a benefit not needed while RateCap runs one sidecar per host. Multi-sidecar-per-core aggregate visibility is a real but separate concern, deferred beyond v2.

**Metrics (corrected for cardinality):**

```go
ratecap_decisions_total{tier, action}       // counter
ratecap_shadow_would_reject_total{tier}     // counter
```

- `tier` ∈ `{rate_limiter, concurrency_limiter, fleet_shedder, worker_shedder}`
- `action` ∈ `{allow, reject_429, reject_503, shadow_log}`
- `ratecap_decisions_total` is incremented with the **pre-shadow-coercion** real decision — i.e. if `ConcurrencyLimiter` decides `REJECT_429` but shadow mode coerces the HTTP response to `200 OK`, the counter still increments `action="reject_429"`. This is what makes shadow mode useful: an operator can see exactly what a tier *would* enforce before flipping it on. `ratecap_shadow_would_reject_total{tier}` is incremented separately, once, whenever a coercion happens — mirroring Envoy's own separate `_shadow_mode` metric rather than conflating it into the main counter's action label.
- **No per-key gauge.** The v1 spec's `ratecap_utilization_ratio{tier,key}` is dropped entirely — an unbounded-cardinality label is exactly the anti-pattern Prometheus's and Envoy's own docs warn against. Per-key utilization drill-down lives in structured logs (below), which have no cardinality constraint.
- This replaces the v1 spec's originally separate `ratecap_shed_total{tier,reason}` and `ratecap_limit_hit_total{tier,key}` counters with one `ratecap_decisions_total{tier,action}` — Envoy's own `_total_hits`/`_over_limit`/`_near_limit` pattern is "one family of counters sliced by outcome," and RateCap's 4 discrete `Action` values already give a clean, bounded label set without needing multiple counters.

Instrumentation point: the existing `switch action { ... }` block in `proxy.go`'s `Handler.ServeHTTP`, immediately alongside each `case`'s `w.WriteHeader(...)` call, using the pre-coercion `action` value already available in that scope (shadow coercion via `shadow.CoerceIfShadowOverridden` happens just before this switch — the pre-coercion value must be captured before that call overwrites it, or read from a separate variable).

### 2. Structured logging: `log/slog`, JSON to stdout

Extends `proxy.go`'s existing shadow-mode-only `log.Printf` pattern to the real (non-shadow) path too, using Go's standard-library `log/slog` (built into Go 1.21+; RateCap is on 1.26.2 — zero new dependency).

Fields per decision: `timestamp`, `tier`, `key`, `action`, `priority`, `latency_ms`.

This closes tracked issue **#21** ("Tier 4: zero logging on the real shed path") as a direct side effect — the real-path logging this issue asks for is exactly what this phase's logging work adds, for every tier, not just Tier 4.

### 3. mTLS: additive, optional, off by default

**Mechanism** (mirrors `envoyproxy/ratelimit`'s verified, current pattern exactly):
- `services/core/main.go`: `grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret)))` — additive alongside the existing interceptor option, not a replacement. `grpc.Creds` is transport-layer mutual auth + encryption; the shared-secret interceptor is app-layer auth-in-depth. They compose, they don't conflict.
- `services/sidecar/main.go`: swap `grpc.WithTransportCredentials(insecure.NewCredentials())` for `grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))` in the existing `grpc.NewClient(...)` call.
- New env vars, following the exact naming/fail-handling idiom already established for `RATECAP_SHARED_SECRET`/`RATECAP_REDIS_ADDR`/`RATECAP_CONFIG_PATH`: `RATECAP_TLS_CERT_PATH`, `RATECAP_TLS_KEY_PATH` (core's server cert/key), `RATECAP_TLS_CA_PATH` (the CA both sides use to validate each other's certs, enabling mutual — not just server-side — TLS).

**Optional and off by default.** Unlike `RATECAP_SHARED_SECRET` (mandatory, fail-closed, established in v1 because skipping auth entirely was the dangerous default), TLS env vars are optional: if unset, both services fall back to today's plaintext-gRPC-plus-shared-secret behavior with zero change in behavior. Making TLS mandatory in this phase would silently break every existing v1.0.1 production deployment on upgrade — a materially worse failure mode than "TLS is available but requires opt-in." When `RATECAP_TLS_CERT_PATH`/`RATECAP_TLS_KEY_PATH` are set on core, and `RATECAP_TLS_CA_PATH` is set on the sidecar, TLS activates; if only one side is configured, that's a startup error (fail loud, not fail silent-partial).

**Cert provisioning.** No automated issuance or rotation tooling is built in this phase — that's real infrastructure work (e.g. cert-manager integration) explicitly out of scope. Two concrete deliverables instead:
- `deploy/docker-compose.yml`'s demo stack generates self-signed certs at build time (a small script or `Dockerfile` step using `openssl`), with an explicit "demo only, do not use in production" comment — mirroring the existing demo `RATECAP_SHARED_SECRET` literal's own disclaimer.
- Real deployments bring their own certs via the three env vars above, exactly like they already bring their own shared secret.
- No cert hot-reload in this phase (Envoy's `ratelimit` does this via `goruntime` file-watching, and it's a reasonable v3 stretch item, but out of scope here to keep this phase's surface area tight).

### 4. Documentation hygiene

Bundled into this phase since all three are small, low-risk, and touch security/architecture docs this phase already needs to update for accuracy:

- `ARCHITECTURE.md`: fix the stale line "v1 implements Tier 1 (the Request Rate Limiter) end-to-end; Tiers 2–4 are planned next" — `README.md` already correctly says "v1.0.0 — complete" post-release; `ARCHITECTURE.md` needs the same correction.
- `SECURITY.md`'s "Supported Versions" section: currently says "RateCap is currently in v1 development (Tier 1 walking skeleton). Until a tagged v1.0.0 release exists, only the `main` branch receives security fixes," with a table showing only `main (pre-release)`. Update to reflect that v1.0.0 and v1.0.1 are real tagged releases.
- `SECURITY.md`'s "Network Transport Security (v1)" section: update to describe the new, actual threat model once mTLS ships — the existing "must run on a private, trusted network only" guidance is *relaxed*, not eliminated, since mTLS is optional. The updated text must be explicit that plaintext-plus-shared-secret remains a supported (if less hardened) posture, and that enabling mTLS is recommended but not required.
- Remove the stray committed compiled binary `deploy/sampleapp/sampleapp` and add it to `.gitignore`.

## Testing Strategy

- **Metrics:** unit tests asserting `ratecap_decisions_total` and `ratecap_shadow_would_reject_total` increment with the correct labels for each `Action` value and for the shadow-coercion path specifically (pre- vs. post-coercion value captured correctly).
- **Structured logging:** unit tests asserting the real (non-shadow) path now emits a `slog` JSON line with all 5 required fields, extending the existing shadow-mode-only log test.
- **mTLS:** a real in-process integration test (mirroring the existing shared-secret interceptor's bufconn-based test pattern in `services/core/auth`) proving an unauthenticated/wrong-cert client is rejected over actual TLS transport when TLS is enabled, AND a separate test proving the unset-env-var (TLS-disabled) path behaves identically to today's v1.0.1 plaintext behavior — this second test is the concrete guarantee that existing deployments aren't broken.
- **Live e2e:** docker-compose demo re-verified with TLS enabled (self-signed certs) showing the sidecar-to-core hop still functions correctly end-to-end, plus a regression check that all 4 tiers still behave identically to their pre-this-phase behavior.
- **Doc hygiene:** no automated test — a manual read-through confirming no other stale "planned next"/"pre-release" references remain in `ARCHITECTURE.md`/`SECURITY.md`/`README.md`.

## Out of Scope (this phase)

- OpenTelemetry SDK integration (Prometheus-only for v2; OTel can be layered on later without touching this phase's metric definitions, since `client_golang` counters can be dual-exported later if needed).
- Grafana dashboard JSON (the v1 spec sketched one; building and maintaining a dashboard is a documentation/adoption concern better suited to Phase 4 alongside the comparison table and published benchmarks).
- Core-side `/metrics` endpoint (sidecar-only for this phase; revisit if multi-sidecar-per-core deployments become a real, requested use case).
- SPIFFE/SPIRE workload identity (explicitly rejected for v2 scope per research — too heavy relative to RateCap's current scale; static certs are the correct fit).
- Cert rotation/hot-reload tooling and automated cert issuance (e.g. cert-manager integration) — static, operator-provided certs only.
- Fixing tracked issue #24 (no HTTP server timeouts on the sidecar) — unrelated to this phase's scope, tracked separately.
