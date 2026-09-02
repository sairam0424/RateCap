# Security Policy

RateCap is a rate-limiting and load-shedding system — it sits on your service's request path and is part of your defense surface. We take security issues seriously and appreciate responsible disclosure.

## Supported Versions

RateCap follows semantic versioning. The latest tagged release and the `main` branch receive security fixes.

| Version | Supported |
| ------- | --------- |
| Latest tagged release (see [`VERSION`](VERSION)) | ✅ |
| `main` | ✅ |
| All earlier tagged releases | ❌ |

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please report vulnerabilities privately via [GitHub Security Advisories](https://github.com/sairam0424/RateCap/security/advisories/new) for this repository. This creates a private discussion thread visible only to maintainers until a fix is ready.

When reporting, please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce (a minimal repro is ideal)
- The affected component (`services/core`, `services/sidecar`, `packages/sdks/go`, `packages/sdks/python`, `cli`, or `proto`)
- Any suggested remediation, if you have one

## What to Expect

- We will acknowledge receipt of your report as soon as possible.
- We will investigate and aim to provide an initial assessment promptly.
- We will keep you informed as a fix is developed, and credit you in the fix's release notes unless you prefer to remain anonymous.
- Once a fix is released, we will publish a GitHub Security Advisory with details, coordinated with your disclosure timeline where reasonable.

## Network Transport Security

`ratecap-core` and `ratecap-sidecar` are always authenticated by a shared secret (`RATECAP_SHARED_SECRET`); both services fail closed if it is unset. Transport encryption is separate and optional:

- **Without TLS configured** (the default): communication is plaintext, authenticated only by the shared secret. This does **not** encrypt traffic or protect against a network-level eavesdropper or man-in-the-middle. **`ratecap-core` and `ratecap-sidecar` must run on a private, trusted network only** — e.g. a Docker Compose network, a Kubernetes cluster-internal `ClusterIP`, or an equivalent isolated segment. Never expose `ratecap-core`'s gRPC port to an untrusted network.
- **With TLS configured** (`RATECAP_TLS_CERT_PATH`, `RATECAP_TLS_KEY_PATH`, `RATECAP_TLS_CA_PATH` set on both services): the hop is encrypted, and both sides present and verify certificates via mutual TLS — the sidecar cannot connect to an impostor core, and core rejects any client that doesn't present a certificate signed by the configured CA. This is layered on top of, not a replacement for, the shared-secret check.
- mTLS is optional and off by default specifically so upgrading an existing deployment never silently breaks it. It is recommended, but not required. If your deployment cannot guarantee a private network and cannot yet configure certificates, treat this as an open risk and prioritize enabling mTLS.
- Certificate provisioning is the operator's responsibility — RateCap does not issue, rotate, or manage certificates. See `deploy/generate-demo-certs.sh` for how the docker-compose demo generates throwaway, 1-day-validity certs; do not reuse that script's output anywhere but the demo.

### Bounded queueing backlog is fleet-wide (fixed in v2.5.0)

`ConcurrencyLimiter`'s optional bounded queueing (`queueing_enabled`, off by default) enforces `max_backlog` as a single shared ceiling across all `ratecap-core` instances, backed by the same Redis-side counter (`store.IncrConcurrent`/`DecrConcurrent` against a `backlog:` key namespace) the concurrency limiter itself uses — not a per-instance count. An earlier version of this mechanism used a per-instance `atomic.Int64`, meaning N core replicas with the same `max_backlog` value produced a real ceiling of `max_backlog × N`, not one shared ceiling; that was fixed in v2.5.0 (see `CHANGELOG.md`). Unlike Tier 4's worker shedder, which is intentionally local-per-sidecar-instance by design (see below), a fleet-wide coordinated backlog ceiling is exactly what this mechanism provides today.

### Global shadow-mode override undercounts in-flight concurrency on a coerced Tier 2 rejection

`RATECAP_SHADOW_MODE=true` is a global, response-coercion-only override applied in `ratecap-sidecar` (`shadow.CoerceIfShadowOverridden`): it turns a real `REJECT_429`/`REJECT_503` into a `200 OK` on the wire without touching how the decision itself was computed. For Tier 1 and Tier 3, and for `ConcurrencyLimiter`'s own per-tier `shadow_mode`, a would-be reject still reserves a slot/token first (the `unboundedCap` trick), so accounting stays accurate even though the caller sees `200`. The global override has no such hook: when `ConcurrencyLimiter.Check` genuinely returns `REJECT_429` (per-tier `shadow_mode` off, cap exceeded), no reservation is made in Redis — there is nothing to coerce a slot for, because none was requested. If the global override then coerces that response to `200 OK`, the caller proceeds with real work while Redis holds zero record of the request occupying a concurrency slot.

This is architecturally correct — the global override is intentionally a pure response layer, matching the documented Envoy pattern, and adding a reservation here would mean the override doing tier-specific limiter work it was never designed to do. But it means concurrency accounting and metrics become inaccurate for the duration the global override is active: operators should treat a globally-shadow-coerced `200` as **not actually holding a concurrency slot**, and should not use Redis-side concurrency counts as a source of truth for real in-flight load while `RATECAP_SHADOW_MODE=true` is set.

### Tier 2 concurrency tokens are signed and header-transported (fixed in v2.3.2)

Prior to v2.3.2, `ratecap-sidecar`'s `/release` endpoint accepted the Tier 2 concurrency token and key as plaintext URL query parameters, and `ReleaseConcurrency` performed no verification that a token was ever actually issued — a real vulnerability class with documented precedent in production software (e.g. Portainer CVE-2026-44883, nhost CVE-2026-34969: both accepted a bearer-style token via `?token=...`, which is recorded in reverse-proxy access logs, browser history, and HTTP `Referer` headers before the application itself can react to it).

v2.3.2 fixes both halves:

- The token and key now travel as request headers (`X-RateCap-Concurrency-Key`, `X-RateCap-Concurrency-Token`) on `/release`, never in the query string. This is a breaking wire-contract change for any direct HTTP caller bypassing the Go/Python SDKs — see `CHANGELOG.md`.
- Every Tier 2 concurrency token is now an HMAC-SHA256-signed value (`<uuid>.<hex-signature>`, signed with `RATECAP_CONCURRENCY_SIGNING_KEY`, a new required env var on `ratecap-core`). `ReleaseConcurrency` verifies the signature before releasing a slot, rejecting a forged or tampered token with `codes.PermissionDenied`.

**Residual limitation, by design:** this signature proves a token was genuinely issued by this `ratecap-core` instance — it does not bind the token to the specific caller who acquired it. Any authenticated sidecar client (auth is service-to-service via the shared secret, not per-end-user) that somehow obtains a valid signed token can still release it, the same way any authenticated caller could before this fix. This matches RateCap's existing shared-secret trust model (see Network Transport Security above) rather than introducing a new one; true per-caller ownership binding would require a caller-identity concept RateCap does not have, and is out of scope for this fix.

## mTLS migration mode

`RATECAP_TLS_MODE=permissive` on `services/core` intentionally accepts connections from clients *without* a certificate (`ClientAuth: VerifyClientCertIfGiven`) on its TLS listener, alongside the still-running plaintext listener — this is a deliberate transitional weakening of the all-or-nothing mTLS posture, scoped to migration only. Do not leave a fleet running in `permissive` mode indefinitely; the whole point is to reach `strict` once `ratecap_core_connection_security_total{transport="plaintext"}` confirms zero remaining plaintext traffic. The shared-secret (`RATECAP_SHARED_SECRET`) gRPC interceptor remains active and enforced on both listeners throughout every mode — mTLS is a second, independent layer, not a replacement for it.

## Admin lever (`/admin/set-limit`)

A sub-second incident-response endpoint for changing Tier 1's rate or Tier 3's `reserved_critical_pct` fleet-wide without a config re-parse. Gated by its own secret, `RATECAP_ADMIN_SECRET`, checked at the sidecar's HTTP layer via the `X-RateCap-Admin-Secret` header — deliberately separate from `RATECAP_SHARED_SECRET`, since this capability has fleet-wide, effectively unbounded blast radius (unlike `/check`, which is self-bounding). A leaked admin secret lets an attacker disable rate limiting or fleet load-shedding fleet-wide in one call; rotate it independently of the general shared secret if either is suspected of leaking.

This endpoint is bound by the same network-level trust boundary as the rest of the sidecar's HTTP surface (see "Network Transport Security" above) — the admin secret is defense-in-depth on top of that, not a replacement for running RateCap on a private, trusted network.

## Priority Claims (v1)

`ratecap-sidecar` resolves each request's priority (`critical` or `sheddable`) from the caller-supplied `x-ratecap-priority` HTTP header with no authentication, no cost, and no verification (`services/sidecar/proxy/priority.go`). Tier 3 (the Fleet Usage Load Shedder) uses this value to decide whether a request is checked against the full fleet capacity (`critical`) or a reduced, shed-first capacity (`sheddable`). This is v1's explicit, intentional trust boundary:

- Any caller that can reach `ratecap-sidecar`'s HTTP port can unilaterally claim `critical` priority for every request, at zero cost — there is no per-caller identity or authorization tied to a priority claim.
- This is consistent with, not an exception to, the trust boundary already established above: a caller who can reach the sidecar in a correctly-deployed RateCap installation is, by v1's threat model, already inside the trusted network the sidecar itself depends on.
- The `deploy/sampleapp` demo's `/fleet-demo` endpoint exercises this exact header with no additional protection, matching (not exceeding) the same accepted demo risk profile already documented below for `/slow-report`.
- The `deploy/sampleapp` demo's `/worker-demo` endpoint exercises Tier 4 (the Worker Utilization Load Shedder) with the same lack of authentication. Its blast radius is smaller than `/fleet-demo`'s, though: `Shedder` (`services/sidecar/worker/shedder.go`) tracks in-flight requests per-sidecar-instance-locally, not fleet-globally like `FleetShedder`, so an unauthenticated caller hitting `/worker-demo` can only exhaust the local sidecar instance's own in-flight capacity — it cannot consume shared, fleet-wide state the way `/fleet-demo` can.
- A stronger priority-claim authorization mechanism — for example, binding a claim to the existing shared-secret scheme, or a future per-caller identity system — is deferred to v2.
- The optional `x-ratecap-route` header (`services/sidecar/criticalroutes`, matched against an operator-configured `critical_routes` allowlist) is governed by this identical trust boundary — a caller can claim membership in a configured critical route with the same lack of authentication, cost, or verification as `x-ratecap-priority` claims critical directly, so it is not a new vulnerability class, just the same accepted claim through a second door.

If your deployment cannot guarantee that only trusted callers can reach `ratecap-sidecar`, do not rely on the `x-ratecap-priority` header for enforcement until v2. There is currently no operator-configurable default: `ratecap-sidecar` always falls back to `sheddable` (`proxy.Sheddable`, hardcoded in `services/sidecar/main.go`) for any request that omits the header — the safe choice, since an unset/misconfigured caller cannot accidentally mark everything critical and defeat the shedder.

## Scope

In scope: the core gRPC engine (`services/core`), the sidecar (`services/sidecar`), the Go SDK (`packages/sdks/go`), the Python SDK (`packages/sdks/python`), the operator CLI (`cli`), and the gRPC contract (`proto/`).

Out of scope: the `deploy/sampleapp` demo application (a minimal example, not intended for production use) and third-party dependencies (report those upstream, though we appreciate a heads-up so we can track and update).
