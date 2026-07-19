# Security Policy

RateCap is a rate-limiting and load-shedding system — it sits on your service's request path and is part of your defense surface. We take security issues seriously and appreciate responsible disclosure.

## Supported Versions

RateCap follows semantic versioning. The latest tagged release and the `main` branch receive security fixes.

| Version | Supported |
| ------- | --------- |
| v1.0.x  | ✅ |
| main    | ✅ |
| < v1.0.0 | ❌ |

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please report vulnerabilities privately via [GitHub Security Advisories](https://github.com/sairam0424/RateCap/security/advisories/new) for this repository. This creates a private discussion thread visible only to maintainers until a fix is ready.

When reporting, please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce (a minimal repro is ideal)
- The affected component (`services/core`, `services/sidecar`, `packages/sdks/go`, or `proto`)
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
- mTLS is optional and off by default specifically so upgrading an existing deployment never silently breaks it. It is recommended, not required, for v2. If your deployment cannot guarantee a private network and cannot yet configure certificates, treat this as an open risk and prioritize enabling mTLS.
- Certificate provisioning is the operator's responsibility — RateCap does not issue, rotate, or manage certificates. See `deploy/generate-demo-certs.sh` for how the docker-compose demo generates throwaway, 1-day-validity certs; do not reuse that script's output anywhere but the demo.

### Bounded queueing backlog is per-instance (v2 Phase 3)

`ConcurrencyLimiter`'s optional bounded queueing (`queueing_enabled`, off by default) enforces `max_backlog` independently on each `ratecap-core` instance — it is not coordinated across a fleet. An operator running N core instances with the same `max_backlog` value should expect up to `max_backlog × N` total in-flight queued requests fleet-wide, not a single shared ceiling. This is a known, accepted limitation (matching Tier 4's existing local-only worker shedder), not an oversight — if your deployment needs a fleet-wide coordinated backlog ceiling, do not rely on `max_backlog` alone to provide it.

## Priority Claims (v1)

`ratecap-sidecar` resolves each request's priority (`critical` or `sheddable`) from the caller-supplied `x-ratecap-priority` HTTP header with no authentication, no cost, and no verification (`services/sidecar/proxy/priority.go`). Tier 3 (the Fleet Usage Load Shedder) uses this value to decide whether a request is checked against the full fleet capacity (`critical`) or a reduced, shed-first capacity (`sheddable`). This is v1's explicit, intentional trust boundary:

- Any caller that can reach `ratecap-sidecar`'s HTTP port can unilaterally claim `critical` priority for every request, at zero cost — there is no per-caller identity or authorization tied to a priority claim.
- This is consistent with, not an exception to, the trust boundary already established above: a caller who can reach the sidecar in a correctly-deployed RateCap installation is, by v1's threat model, already inside the trusted network the sidecar itself depends on.
- The `deploy/sampleapp` demo's `/fleet-demo` endpoint exercises this exact header with no additional protection, matching (not exceeding) the same accepted demo risk profile already documented below for `/slow-report`.
- The `deploy/sampleapp` demo's `/worker-demo` endpoint exercises Tier 4 (the Worker Utilization Load Shedder) with the same lack of authentication. Its blast radius is smaller than `/fleet-demo`'s, though: `Shedder` (`services/sidecar/worker/shedder.go`) tracks in-flight requests per-sidecar-instance-locally, not fleet-globally like `FleetShedder`, so an unauthenticated caller hitting `/worker-demo` can only exhaust the local sidecar instance's own in-flight capacity — it cannot consume shared, fleet-wide state the way `/fleet-demo` can.
- A stronger priority-claim authorization mechanism — for example, binding a claim to the existing shared-secret scheme, or a future per-caller identity system — is deferred to v2.

If your deployment cannot guarantee that only trusted callers can reach `ratecap-sidecar`, treat every request as `sheddable` by setting `default_priority: sheddable` and do not rely on the `x-ratecap-priority` header for enforcement until v2.

## Scope

In scope: the core gRPC engine (`services/core`), the sidecar (`services/sidecar`), the Go SDK (`packages/sdks/go`), and the gRPC contract (`proto/`).

Out of scope: the `deploy/sampleapp` demo application (a minimal example, not intended for production use) and third-party dependencies (report those upstream, though we appreciate a heads-up so we can track and update).
