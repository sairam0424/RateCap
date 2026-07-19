# RateCap v2 Phase 4c: Helm Chart — Design Spec

**Date:** 2026-07-19
**Status:** Approved
**Context:** Third and final sub-project under Phase 4 (Production-Readiness & Adoption), the fourth and final phase of the v2 roadmap. Phase 4a (Comparison table) shipped via PR #35, Phase 4b (Published benchmarks) shipped via PR #36. Phase 1 (observability, mTLS) shipped as v2.0.0, Phase 2 (`ratecapctl` CLI, Python SDK) shipped as v2.1.0, Phase 3 (bounded queueing) shipped as v2.2.0.

This is the biggest and most novel sub-project in the v2 roadmap: unlike Phase 4a/4b (pure documentation), this adds real new code (a gRPC health service on `core`, an HTTP health handler on `sidecar`) alongside the chart itself. It adds no new enforcement mechanism and touches no core decision logic, so it does not trigger this repo's `CLAUDE.md` sign-off gate — routine spec-review applies, same as Phase 4a/4b.

Unlike Phase 4a/4b's split into unrelated deliverables, this sub-project's design questions (image source, health checks, secrets, smoke-test scope, chart scope) are all facets of one deliverable — the chart — so this remains a single spec, not decomposed further.

## Problem

The v2 roadmap named a concrete gap: RateCap has no Helm chart, so adopting it in a Kubernetes environment means hand-writing manifests from scratch. `deploy/docker-compose.yml` already defines the real topology (`redis`, `core`, `sidecar`, `sampleapp`) and `deploy/ratecap.yaml` the config surface; templatizing both into a chart is the roadmap's explicit ask, along with two security deltas from compose's demo-only shortcuts: `ClusterIP`-only networking (never `NodePort`/`LoadBalancer`) and Kubernetes `Secret`-based credential handling (never a literal env var in a manifest).

Direct exploration confirmed two things the roadmap only asserted: neither `core` nor `sidecar` has any health-check endpoint today (`core` is pure gRPC with zero HTTP surface; `sidecar` has `/check`, `/release`, `/metrics`, none designed as liveness/readiness probes), and no container-image registry exists anywhere in this project's CI or docs — `docker-compose.yml` builds all three images locally via `build: context: ..`. Both are real gaps a Helm chart has to resolve, not assumptions to design around blindly.

## Key Design Decisions

### 1. Chart layout: `deploy/helm/ratecap/`

A standard Helm chart — `Chart.yaml`, `values.yaml`, `templates/` — sitting alongside the existing `deploy/docker-compose.yml`/`deploy/ratecap.yaml`/`deploy/docker-compose.bench.yml`, not replacing any of them. The docker-compose demo remains the primary local-dev/quick-start path (per `README.md`'s existing Quick start section); the Helm chart is an additional, Kubernetes-specific deployment option.

Templates: one Deployment + Service pair each for `redis`, `core`, `sidecar`, plus a `sampleapp` Deployment + Service gated by a values flag (see §5). A `ConfigMap` holds the templatized `ratecap.yaml` content (mirroring what `deploy/ratecap.yaml` is for compose). No Ingress, no HorizontalPodAutoscaler, no PodDisruptionBudget — none of these have any precedent in the existing compose setup or spec, and adding them would be scope creep beyond what the roadmap asked for.

### 2. Image source: registry-agnostic chart, `kind load docker-image` for local verification

`values.yaml` takes `<component>.image.repository` and `<component>.image.tag` per component (`core`, `sidecar`, `sampleapp`), exactly like any standard Helm chart — the chart has no opinion on how an image got into a registry, and does not build images itself. This is a deliberate divergence from compose's `build: context: ..` convenience, matching how essentially every real-world Helm chart works.

For this sub-project's own local verification (§4's smoke test), there is no registry step at all: build the 3 existing Dockerfiles locally exactly as compose already does, then `kind load docker-image` each into the test cluster. This proves the chart works end-to-end without inventing a registry-publishing story this project doesn't have and isn't in scope to build.

### 3. Networking: every Service is `ClusterIP`

`redis`, `core`, `sidecar`, and `sampleapp`'s Services are all `ClusterIP` — no `type:` override exposed in `values.yaml` for any of them, and no `NodePort`/`LoadBalancer` template path exists at all. This delivers on the private-network principle `SECURITY.md` already establishes (plaintext-fallback communication "must run on a private, trusted network only — e.g. a Docker Compose network, a Kubernetes cluster-internal `ClusterIP`"). If a real deployment needs external access to `sampleapp` (or any component), that's an Ingress/gateway decision layered on top by the operator, explicitly out of this chart's scope.

### 4. Secrets: user-provided Secret name, BYO-only — no chart-generated credentials

`values.yaml` takes a Secret name and key names the user is expected to have already created out-of-band (e.g. `sharedSecret.existingSecretName`/`sharedSecret.existingSecretKey` for `RATECAP_SHARED_SECRET`; equivalent `existingSecretName`/keys for the 3 TLS env vars when mTLS is enabled). Chart templates reference these via `secretKeyRef` in each Deployment's env — the chart never runs `randAlphaNum`, never bakes in a default secret, never ships anything resembling `generate-demo-certs.sh`'s self-signed-cert generation as chart logic.

This mirrors `SECURITY.md`'s existing stance that "certificate provisioning is the operator's responsibility — RateCap does not issue, rotate, or manage certificates," extended to the shared secret as well. It is a real, honest gap for a first-time user (`helm install` alone does not "just work" without first creating a Secret) — §6's chart README must state this plainly as a prerequisite, with the exact `kubectl create secret generic` command needed, rather than leaving a silent gap.

mTLS itself stays optional per existing precedent — `values.yaml`'s TLS section defaults to disabled, and enabling it means setting `tls.existingSecretName` and the 3 corresponding key names.

### 5. Chart scope: `sampleapp` included, gated off by default

The chart's `values.yaml` includes a `sampleapp.enabled` flag defaulting to `false`. When `false`, no `sampleapp` Deployment/Service is rendered at all — a production `helm install` gets only `redis`/`core`/`sidecar`. When `true` (used by §4's smoke test), `sampleapp` deploys exactly as it does in compose, giving the tier-regression traffic (`checkout`/`slow-report`/`fleet-demo`/`worker-demo`) somewhere real to originate from, without inventing a separate curl-pod mechanism just for the smoke test. This matches `SECURITY.md`'s existing "not intended for production use" framing for `sampleapp` — the chart includes it as an optional demo/verification aid, not as a production component a real deployment would normally enable.

### 6. Health checks: new code on both services, using each service's own idiomatic mechanism

- **`sidecar`** gains a new `/healthz` HTTP handler in `services/sidecar/main.go`'s existing `http.NewServeMux()` — a trivial always-`200 OK` response, no dependency checks, registered alongside the existing `/check`/`/release`/`/metrics` handlers. Used for both the chart's liveness and readiness probes (`httpGet` on `/healthz`).
- **`core`** gains the standard `grpc.health.v1.Health` service, registered on the same `grpc.NewServer(...)` core already constructs in `services/core/main.go`. This requires zero new external dependencies — `google.golang.org/grpc/health` and its generated `grpc_health_v1` package are subpackages of the `google.golang.org/grpc` module already at v1.82.0 in `services/core/go.mod`. The chart's `core` Deployment probes it via a `grpc` probe action (`readinessProbe.grpc.port`/`livenessProbe.grpc.port`, the native Kubernetes gRPC probe mechanism, stable since Kubernetes 1.27 — no `grpc_health_probe` sidecar binary or exec wrapper needed). `kind` v0.31.0's default node image and `kubectl` v1.36.0 (both confirmed present in this environment) are well past that floor, so this sub-project's own local verification can rely on the native mechanism directly.

This is the one piece of this sub-project that is genuine new product code, not purely deployment tooling — both additions are additive and off the request-serving hot path (the health service/handler don't touch `Pipeline`, `Limiter`, or any tier's decision logic).

### 7. Smoke test: pods Ready + full tier-regression traffic through a real cluster

The verification for this sub-project (run once during implementation, mirroring how Phase 1's docker-compose live-e2e-verification proved the original 4 tiers and how Phase 4b's benchmark numbers were collected from a real run before being written into a plan) is:

1. Build the 3 images locally (existing Dockerfiles, unchanged build process).
2. Create (or reuse) a local `kind` cluster; `kind load docker-image` all 3 images into it.
3. `kubectl create secret generic` the shared secret (and, if testing mTLS, generate throwaway certs via the existing `deploy/generate-demo-certs.sh` logic and create a second Secret from them) — proving the BYO-secret flow from §4 genuinely works, not just that the chart's templates parse.
4. `helm install` with `sampleapp.enabled=true` and the created Secret names wired into `--set`/a values override file.
5. `kubectl wait --for=condition=Ready` on every pod; if any pod fails to reach Ready, this is a failure — investigate before proceeding, do not report partial success.
6. Port-forward to `sampleapp`'s Service and re-run the exact same `checkout`/`slow-report`/`fleet-demo`/`worker-demo` curl checks used throughout this project's prior tiers' live verifications, confirming the expected 429/503/200 behavior for each tier through the real cluster's networking, config, secrets, and health-check plumbing — not just that pods started.
7. `helm uninstall` and tear down the test cluster cleanly.

A chart that only passes `helm template`/lint checks but was never actually installed against a real cluster does not meet this sub-project's bar, per the roadmap's own explicit requirement ("a real `helm install` + smoke test against a local cluster (kind/minikube), not just template linting").

## Out of Scope

- Any change to `services/core`, `services/sidecar` beyond the two additive health-check mechanisms in §6 — no changes to `Pipeline`, any `Limiter`, `config`, `store`, or any tier's decision logic.
- Publishing container images to any registry, or building any CI pipeline for doing so — the chart is registry-agnostic per §2; actually publishing images is a separate, future concern.
- Chart-generated or auto-provisioned secrets/certs of any kind — strictly BYO per §4.
- Ingress, HorizontalPodAutoscaler, PodDisruptionBudget, NetworkPolicy, or any other Kubernetes resource type beyond Deployment/Service/ConfigMap — none have existing precedent in this project and none were asked for by the roadmap.
- `NodePort`/`LoadBalancer` Service types, or any values.yaml knob to select them — every Service is `ClusterIP` only, per §3.
- A Helm chart repository / `helm repo add`-able index — this chart is consumed via local path (`helm install ./deploy/helm/ratecap`) for now; publishing it as an indexed repo is a future concern, not part of this sub-project.
- Any change to `deploy/docker-compose.yml`, `deploy/ratecap.yaml`, or `deploy/docker-compose.bench.yml` — the compose-based demo path stays exactly as-is; the Helm chart is purely additive.
