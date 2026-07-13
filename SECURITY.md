# Security Policy

RateCap is a rate-limiting and load-shedding system — it sits on your service's request path and is part of your defense surface. We take security issues seriously and appreciate responsible disclosure.

## Supported Versions

RateCap is currently in v1 development (Tier 1 walking skeleton). Until a tagged v1.0.0 release exists, only the `main` branch receives security fixes.

| Version | Supported |
| ------- | --------- |
| main (pre-release) | ✅ |

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

## Scope

In scope: the core gRPC engine (`services/core`), the sidecar (`services/sidecar`), the Go SDK (`packages/sdks/go`), and the gRPC contract (`proto/`).

Out of scope: the `deploy/sampleapp` demo application (a minimal example, not intended for production use) and third-party dependencies (report those upstream, though we appreciate a heads-up so we can track and update).
