# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/) (v2.3.2 is a documented exception — see its own entry below).

## [Unreleased]

## [2.12.2] — 2026-09-01 — Fix Helm Chart's Unqualified Image References

Patch: Artifact Hub emailed a scan failure for the `ratecap` chart (`error scanning image ratecap-core:latest: image not found`). Root cause was more than cosmetic: `deploy/helm/ratecap/values.yaml`'s `core`/`sidecar`/`sampleapp` image repositories were bare names (`ratecap-core`, etc.) with no registry — a deliberate choice at the chart's original design time (`docs/superpowers/specs/2026-07-19-v2-phase-4c-helm-chart-design.md` §2: "registry-agnostic chart... publishing it as an indexed repo is a future concern, not part of this sub-project"). That future concern shipped in v2.11.0 (real GHCR images, real OCI chart publish, real Artifact Hub listing) but `values.yaml`'s defaults were never updated to match, so an unqualified image name resolves against Docker Hub by default — both for Kubernetes' own image pull and for Artifact Hub's scanner — where these images have never existed. **Any real user running `helm install ratecap oci://ghcr.io/sairam0424/charts/ratecap` with default values would have hit `ImagePullBackOff`**, not just Artifact Hub's scan; no CI job ever caught this because `helm-lint` only lints/templates (never pulls images) and `e2e-smoke` deploys via `docker-compose`, entirely bypassing this chart.

### Fixed

- `deploy/helm/ratecap/values.yaml`: `core`/`sidecar`/`sampleapp` image repositories now fully qualified (`ghcr.io/sairam0424/ratecap-{core,sidecar,sampleapp}`), matching exactly what `publish-release.yml` actually publishes. `redis`'s image was already correctly a real Docker Hub image and is unchanged.
- `deploy/helm/ratecap/Chart.yaml`: bumped `version` (0.1.0 → 0.1.1, never previously bumped since the chart's creation) and `appVersion` (2.11.0 → 2.12.1, was one release stale) — a chart `version` bump is required for the OCI push to actually produce a new, distinguishable artifact Artifact Hub will re-scan; pushing identical chart content under the same version does not trigger a fresh scan.
- `deploy/helm/ratecap/README.md`: updated the `kind load docker-image` local-testing instructions to tag locally-built images under the same fully-qualified names as the new defaults, so local `kind` testing keeps working with zero `--set` overrides.

### Verification

`helm lint`/`helm template` (both the default and `sampleapp.enabled=true` cases) confirm rendered manifests reference the fully-qualified names; `docker manifest inspect` confirms all three images are genuinely pullable at those exact references today, not just correctly-formatted strings.

## [2.12.1] — 2026-09-01 — Fix Signed-Releases Detection

Patch: the live Scorecard re-check for v2.12.0 came back with **Signed-Releases still 0/10**. Root cause, confirmed directly against `ossf/scorecard`'s own source (`probes/releasesAreSigned`, `probes/releasesHaveProvenance`): the check is a plain filename-suffix probe over GitHub Release assets — it only recognizes `.asc`/`.minisig`/`.sig`/`.sign`/`.sigstore`/`.sigstore.json` as signatures and `.intoto.jsonl` as provenance. v2.12.0's `checksums.txt.bundle` (cosign's modern bundle format) and the GitHub Attestations API (not a release asset at all) are both genuinely valid signing/provenance mechanisms, but neither matches a suffix the probe checks for, so the score didn't move despite the artifacts being real and verifiable.

### Fixed

- `.github/workflows/publish-release.yml`: `publish-cli-binaries` now also runs a second, independent `cosign sign-blob --yes checksums.txt` (no `--bundle`) and uploads its plain base64 signature as `checksums.txt.sig` — a real, separately-verifiable keyless signature under a suffix the probe recognizes. The existing `checksums.txt.bundle` step and `README.md`'s documented `--bundle` verification path are unchanged and remain the recommended way to verify a download.

### Note

Scorecard's `Signed-Releases` score is a rolling average over the last 5 releases (`checks/evaluation/signed_releases.go`), including v2.11.0 and earlier, which shipped before any signing existed — so this won't jump straight to 8-10; it climbs as older, unsigned releases age out of that 5-release window. Provenance (the `.intoto.jsonl` half of the check) is intentionally left unfixed here: doing so would mean relabeling `actions/attest-build-provenance`'s Sigstore-bundle output with a `.intoto.jsonl` extension it doesn't actually match the format of, which is a real fix for the *score* but not an honest one for the *artifact* — flagged as a genuine gap in Scorecard's own tooling (it hasn't caught up to GitHub's native attestations API yet), not something to work around by mislabeling a file.

## [2.12.0] — 2026-09-01 — Signed, Multi-Platform CLI Release Binaries

Minor release: closes two gaps found while investigating OpenSSF Scorecard's live **Signed-Releases: 0/10** score (`docs/superpowers/plans/2026-09-01-signed-cli-release-binaries.md`) — SBOMs and provenance were already attached to/attesting the container images and Helm chart, but nothing downloadable was ever attached to a GitHub Release itself, which is specifically what that check looks for; separately, `ratecapctl` had no downloadable binary at all (`go install`/build-from-source only), a real discoverability gap independent of Scorecard. Both are fixed by the same feature: cross-platform, signed `ratecapctl` release binaries.

### Added

- `.github/workflows/publish-release.yml`: new `publish-cli-binaries` job (same `v*` tag trigger) hand-rolls a `GOOS`/`GOARCH` build matrix — `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64` (`windows/arm64` intentionally excluded: no first-party consumer) — using the same `-ldflags "-X .../cmd.Version=..."` pattern already documented in `README.md`. Generates a `checksums.txt` (SHA-256) covering all 5 binaries, signs it keylessly with `cosign sign-blob --bundle`, attests build provenance (`actions/attest-build-provenance`) over the binaries plus `checksums.txt`, and uploads all of it (5 binaries, `checksums.txt`, `checksums.txt.bundle`) as GitHub Release assets via `gh release upload` — least-privilege `permissions:` scoped to just this job (`contents: write`, `id-token: write`, `attestations: write`), matching the existing `publish-images`/`publish-helm-chart` per-job pattern.
- `README.md`: "Downloading a release" (which platform/arch asset to grab, `chmod +x`) and "Verifying a release" (copy-pasteable `cosign verify-blob`/`gh attestation verify` commands, both dry-run against placeholder artifacts to confirm flag syntax before being committed to the doc).

The live Scorecard re-check for the Signed-Releases metric happens after this ships as the real `v2.12.0` tag — not claimed as confirmed here.

## [python-sdk-v0.1.0] — 2026-09-01 — Python SDK: First PyPI Release

`packages/sdks/python` published to PyPI for the first time (`pip install ratecap`), via `.github/workflows/publish-python-sdk.yml`'s PyPI Trusted Publisher (OIDC) flow — no `PYPI_API_TOKEN` secret involved. This SDK is versioned independently of the main `vX.Y.Z` releases (its own `python-sdk-vX.Y.Z` tag series, tracking `packages/sdks/python/pyproject.toml`'s own version), since it ships on its own cadence rather than RateCap's server/CLI release cycle. Verified end-to-end: real release files on PyPI, `pip install ratecap` in a fresh virtualenv, `Client`/`Ticket`/`AllowResult`/`estimate_llm_cost` all importable.

## [2.11.0] — 2026-09-01 — Discoverability & CI/CD Modernization

Minor release: closes every discoverability/CI gap found by directly auditing the live repo/GitHub state and cross-checking against current OSS discoverability and GitHub Actions best-practice research, per `docs/superpowers/plans/2026-09-01-discoverability-ci-modernization.md`.

### Changed

- **Correction, not a feature:** every Go module's declared import path is renamed from the previously-unreachable `github.com/ratecap/*` to the real, resolvable `github.com/sairam0424/RateCap/...` (`core`, `sidecar`, `proto`, `sdk-go`, `cli`, `sampleapp`, and `core/integrationtests`). `github.com/ratecap` has never resolved — confirmed 404 via `curl` and a `go-get=1` probe — so `go get`, `go install`, and pkg.go.dev indexing were non-functional since day one and no real external consumer could ever have depended on the old path; this breaks zero real consumers. Every internal `.go` import, every `go.mod`, all `replace` directives, and every documented code snippet (`README.md`, `CONTRIBUTING.md`, `CLAUDE.md`) are updated to match. Historical records (`CHANGELOG.md`'s past entries, `docs/superpowers/plans/*`) are intentionally left untouched.
- **Helm chart publishing no longer depends on a branch that has never existed.** `publish-release.yml`'s `publish-helm-chart` job previously used `helm/chart-releaser-action`, which requires a `gh-pages` branch — confirmed via `git ls-remote`/`git branch -a` that this branch has never existed on this repo, so every release with chart changes (v2.4.0, v2.6.0, v2.7.0, v2.9.0, v2.10.1) silently failed at that step. It now does `helm package` + `helm push` directly to `oci://ghcr.io/sairam0424/charts`, the same GHCR registry and `GITHUB_TOKEN` already used for Docker images. Helm's OCI support has been the default, non-experimental distribution model since v3.8.0, and `chart-releaser` itself still has no native OCI push support (upstream `helm/chart-releaser#622`, open/unmerged) — so this drops the `gh-pages` dependency entirely rather than patching it in place.

### Added

- Signing of every pushed Helm chart digest with `cosign` (keyless, OIDC), plus an `artifacthub-repo.yml` pushed as a sibling `:artifacthub.io`-tagged OCI artifact via `oras` — the mechanism Artifact Hub documents for discovering and verifying OCI-based Helm repositories.
- `.github/workflows/scorecard.yml` — OpenSSF Scorecard, triggered on push-to-`main`, a weekly cron, and branch-protection-rule changes, publishing results and uploading SARIF to the Security tab; a matching auto-updating badge on the README.
- `.github/workflows/codeql.yml` — CodeQL for Go (`autobuild` mode), triggered on push/PR to `main`/`develop` plus a weekly cron.
- SBOM generation (SPDX, via `anchore/sbom-action`) and build-provenance attestation (`actions/attest-build-provenance`) for every published container image and for the Helm chart in `publish-release.yml`.
- `gosec` in `.golangci.yml` across every Go module. All 98 findings it surfaced against the current tree were triaged before the gate was enabled: genuine issues fixed directly (test-fixture file permissions, `deploy/sampleapp` no longer echoing an unvalidated query param into response bodies, an explicit-timeout `http.Server` replacing a bare `http.ListenAndServe`); deliberate, safe patterns narrowly `//nolint:gosec`'d with a stated reason each.
- Community-health scaffolding: structured issue-form templates (`.github/ISSUE_TEMPLATE/bug_report.yml`, `feature_request.yml`), `.github/PULL_REQUEST_TEMPLATE.md`, `.github/CODEOWNERS`, and `.github/release.yml` (GitHub's native PR-categorization config for auto-generated release notes — a supplement to this hand-written changelog, not a replacement).
- README: License, Latest Release, and OpenSSF Scorecard badges; a table of contents; a `docker pull ghcr.io/sairam0424/ratecap-{core,sidecar,sampleapp}` alternative alongside the existing build-from-source Quick Start; a Helm OCI install mention (`helm install ratecap oci://ghcr.io/sairam0424/charts/ratecap`).
- `deploy/helm/ratecap/.helmignore` — `helm package` had no ignore file and was silently bundling local dev-tooling cruft (e.g. `.claude-flow/`, `.swarm/`) into the shipped chart artifact.

### Fixed

- `deploy/helm/ratecap/Chart.yaml`'s `appVersion` was stuck at `2.2.0` since the chart's introduction (actual latest was v2.10.1) — now tracks the real current release, plus the `artifacthub.io/license` and `artifacthub.io/maintainers` annotations Artifact Hub expects.

## [2.10.1] — 2026-09-01 — E2E Audit Remediation

Patch release: fixes every confirmed real defect from a full static-audit + live-production dry run against `main` @ v2.10.0 (`docs/superpowers/plans/2026-09-01-e2e-audit-dryrun-ledger.md`), remediated per `docs/superpowers/plans/2026-09-01-e2e-audit-remediation.md`. Bug fixes and doc corrections only — no new features, no behavior change to any request-path decision.

### Fixed

- Helm chart: the `RATECAP_ADMIN_SECRET` env block in `sidecar.yaml` is now guarded by `.Values.adminSecret.existingSecretName`, fixing an install-breaking bug when the admin secret is left unset (the audit's most severe finding).
- Helm chart: `tls.mode` (`strict`|`permissive`) is now wired through to `RATECAP_TLS_MODE`, and the NetworkPolicy's port-9443 ingress rule is gated on `tls.mode == "permissive"` instead of just `tls.enabled`.
- `CHANGELOG.md`: merged the phantom `[2.4.1]` entry (no `v2.4.1` tag ever existed) into `[2.5.0]`, the release it actually shipped in.
- `README.md`: added the missing `bash deploy/generate-demo-certs.sh` step to the top-level Quick Start, matching the already-correct Benchmarks section and `CLAUDE.md`.
- `deploy/sampleapp/main.go`: `/fleet-demo`'s release call now sends `X-RateCap-Concurrency-Key`/`-Token` as headers (matching the sidecar's `ReleaseHandler`) instead of query parameters, and releases the Tier-2 per-key concurrency slot on a shed (non-200) response instead of leaking it until the reaper window expires.
- `deploy/sampleapp/main.go`: `/fleet-demo` and `/worker-demo` now relay the sidecar's `X-RateCap-Shed-Tier`/`Retry-After-Ms`/`RateLimit-Reset` response headers for every status code, not just 200.
- Go SDK (`packages/sdks/go/client.go`): `Allow()`/`Acquire()` now parse and surface `RateLimit-Reset` alongside the existing `RetryAfterMs`.
- `.github/dependabot.yml`: added the missing `gomod` entry for `services/core/integrationtests`.
- Helm chart: added `resources` (requests/limits) blocks to every component's container spec (core, sidecar, redis, redis-sentinel, sampleapp).
- `deploy/docker-compose.bench.yml`: raised the sidecar's RPS ceiling in the benchmark overlay, which was silently capping throughput below what the benchmark actually drove.
- `CONTRIBUTING.md`, `ARCHITECTURE.md`, `README.md`, `SECURITY.md`: added the missing `cli` and `packages/sdks/python` references (build/test loops, component overview, project layout, and security scope respectively).
- `services/core/grpcserver/server.go`: `CheckRateLimit` now rejects non-positive `Cost` server-side, defense in depth alongside the sidecar's own `resolveCost` validation.

## [2.10.0] — 2026-09-01 — OpenTelemetry Trace-Context Propagation

Minor release: implements the roadmap's originally-deferred Phase 1 stretch item (`docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 1 item 9) — flagged at the time as the largest item in Phase 1, pre-approved to slip, and never re-picked-up by Phase 5's own item list. No request-path decision logic changes.

### Added

- Distributed tracing across the `ratecap-sidecar` → `ratecap-core` gRPC hop: a real `sdktrace.TracerProvider` and the W3C `traceparent` propagator, bootstrapped by a new `otelinit` package in each service (`services/core/otelinit`, `services/sidecar/otelinit`).
- `otelgrpc` stats handlers on both of core's `RatecapService` gRPC listeners (plaintext and permissive-mode TLS) and on the sidecar's gRPC client to core, so a `traceparent` is actually read from and written to gRPC metadata per RPC.
- `RATECAP_OTEL_EXPORTER_ENDPOINT` (both services) — unset by default, matching this repo's existing opt-in `RATECAP_*` convention. Spans are always created and ended (context propagation is real either way), but exporting them anywhere requires setting this env var; **no OTel Collector, Jaeger, or Tempo is added to `deploy/docker-compose.yml`**, and the demo stack's `/healthz` and sample endpoints are unaffected either way.
- A `ratecap.priority` span attribute (`"sheddable"`/`"critical"`) on both the sidecar's client span and core's server span — the only per-request data attached to a span. The caller-controlled request key, and any header or query-parameter value, is never put in a span attribute or in baggage.

### Verification

- `services/core/grpcserver/otel_integration_test.go` — a real `bufconn`-backed gRPC call, both sides wired with the otelgrpc stats handlers under an in-memory `tracetest` exporter, asserting the recorded client and server spans share one trace ID with a parent-child relationship, and that `ratecap.priority` is correct for both a `SHEDDABLE` and a `CRITICAL` request.

## [2.9.0] — 2026-08-31 — Phase 5 Performance & DevEx Polish

Minor release: Phase 5 (the final phase) of the v3 upgrade roadmap — closes the remaining benchmark, tooling, dependency-hygiene, and CI-coverage gaps. No request-path limiter behavior changes.

### Added

- A second, honest benchmark run in `README.md` against un-loosened shipped defaults (`deploy/ratecap.yaml`, not the loosened `ratecap-bench.yaml`) — the existing numbers are now explicitly labeled as headroom/pass-through overhead only, not a capacity or rejection-behavior measurement.
- `ratecapctl bench run --duration`/`--report-interval` — a soak mode backed by a fixed-memory, log-bucketed streaming histogram (`cli/cmd/histogram.go`), replacing the previously unbounded per-sample slice.
- `RATECAP_PPROF_ENABLED` on `services/core` and `services/sidecar` — opt-in `net/http/pprof`, bound to `127.0.0.1` only, off by default everywhere shipped.
- `ratecapctl bench run --capture-resources`/`--docker-containers`/`--redis-addr` — best-effort `docker stats`/`redis-cli INFO` snapshots alongside a benchmark run.
- `.github/workflows/benchmark.yml` — a nightly (and manually-dispatchable) benchmark regression job comparing against a committed `deploy/bench-baseline.json`, never auto-updating that baseline.
- `golangci-lint` wired into CI (`.golangci.yml`, matrixed across every Go module including `services/core/integrationtests`), alongside the existing `gofmt` gate.
- `ratecapctl --version` (built from the repo's single `VERSION` source of truth via `-ldflags`) and `bench run --qps` (pacing via `golang.org/x/time/rate`), plus entrypoint tests for `cli/main.go`/`cli/cmd/root.go`/`cli/cmd/bench.go` that didn't exist before.
- `deploy/helm/ratecap/generate-config.sh` — generates the chart's embedded `config.yaml` from `deploy/ratecap.yaml` instead of hand-copying it, with a CI drift check; documented the previously-undocumented `concurrencySigningKey` secret in the Helm README.

### Changed

- `services/core/integrationtests` — the `testcontainers-go`-based Redis/Toxiproxy integration tests are now their own Go module (own `go.mod`, added to `go.work`), so the production `services/core` module no longer carries that ~40-package transitive dependency tree. `services/core`'s own coverage remains above its 50% CI floor (verified 60.9% after the split).
- `services/core/Dockerfile`, `services/sidecar/Dockerfile`, `deploy/sampleapp/Dockerfile` — base images pinned to resolved digests (`golang:1.27-alpine@sha256:...`, `alpine:3.24@sha256:...`), closing the last dependency-drift gap (GitHub Actions were already SHA-pinned).

## [2.8.0] — 2026-08-31 — Phase 4 SDK & API: Token-Cost Wiring

Minor release: Phase 4 of the v3 upgrade roadmap — finishes an already-designed feature. RateCap's Tier 1 substrate was already a generic variable-cost token bucket end-to-end; only the sidecar's hardcoded `Cost: 1` stood in the way.

### Added

- `/check?cost=N` — wires the existing cost plumbing through, default `1` (unchanged for existing callers).
- `EstimateLLMCost`/`estimate_llm_cost` helpers on both SDKs, mirroring the AWS Bedrock/LiteLLM token-cost estimate.
- A `RefundCost` gRPC RPC and a new bounded, burst-clamped Lua script, exposed via `/release`'s new `X-RateCap-Refund-Key`/`X-RateCap-Refund-Amount` headers — reserve a cost estimate upfront, refund the unused portion once real usage is known.
- Go SDK: `WithCost`/`WithPriority` options on `Allow`/`Acquire`, `Ticket.Refund`.
- Python SDK: `cost`/`priority` parameters on `allow`/`acquire`, `Ticket.refund`, and — previously entirely missing — timeout, retry/backoff, and TLS support (`Client(..., timeout=, max_retries=, backoff_base=, ca_file=)`), all via the standard library only.
- Shared contract tests (`services/sidecar/contracttest`) driving both SDKs against the real sidecar handler, catching wire-format drift directly instead of via independently-maintained protocol descriptions agreeing by coincidence.
- `RateLimit-Reset` response header (IETF `draft-ietf-httpapi-ratelimit-headers`) on `429` responses — partial compliance; `limit`/`remaining` require a future proto change and are explicitly out of scope here.
- A sidecar-local negative cache for already-denied identifiers, complementary to Tier 4's worker shedder.

## [2.7.0] — 2026-08-29 — Phase 3 Security: mTLS PERMISSIVE Mode

Minor release: Phase 3 of the v3 upgrade roadmap — adds the migration rung between "off" and "all-or-nothing strict" mTLS that every mature service mesh ships. No shipped default changes.

### Added

- `RATECAP_TLS_MODE=off|permissive|strict` on `services/core`. `off` (default, unset) preserves exact pre-v2.7.0 behavior. `permissive` adds a second, additive TLS listener (`RATECAP_GRPC_TLS_ADDR`, default `:9443`) accepting connections with or without a client cert, alongside the unchanged plaintext listener — letting sidecars migrate one at a time. `strict` is the existing all-TLS behavior, now reachable via an explicit mode string.
- `ratecap_core_connection_security_total{transport,client_cert}` — the "is anything still on plaintext" signal a `strict` cutover needs before it's safe.
- `ratecapctl tls check <cert-path> <expected-host>` — a SAN/hostname preflight check catching the exact failure mode the Helm chart's `values.yaml` already documents as producing no server-side log.
- Certificate hot-reload via `fsnotify` on both `services/core` (server cert) and `services/sidecar` (client cert) — an externally-rotated cert now takes effect without a pod restart. The CA pool is not hot-reloaded.
- An opt-in Helm `NetworkPolicy` (`networkPolicy.enabled`, default `false`) restricting core's gRPC/health ports to the sidecar's pod selector and Redis's (and, if enabled, Sentinel's) port to core's selector.

### Security

- `RATECAP_TLS_MODE=permissive` is a deliberate, scoped transitional weakening (`ClientAuth: VerifyClientCertIfGiven` on the new listener only) — see `SECURITY.md`'s new mTLS migration mode section for the intended usage and the signal that says it's safe to move to `strict`.

## [2.6.0] — 2026-08-28 — Phase 2 Reliability & Testing Hardening

Minor release: Phase 2 of the v3 upgrade roadmap — RateCap's core architectural claims (fail-open/fail-closed, atomicity, no-flapping) are now tested invariants instead of assumptions, Redis can run HA via Sentinel, and on-call has a sub-second incident-response lever.

### Added

- Toxiproxy-based real-network-fault regression tests proving Tier 1 fails open and Tiers 2/3 fail closed on a Redis outage (`services/core/reliability`).
- Race regression tests for the priority-partition-at-capacity bug class (Tier 2 and Tier 3) and a concurrent stress test for `services/sidecar/decisionlog`.
- Property-based tests (`pgregory.net/rapid`) for `TokenBucketLimiter` and `ConcurrencyLimiter`.
- Fault-injection tests for the fsnotify config hot-reload path (partial writes, atomic rename-swap, delete+recreate).
- `Config.Hash()` and `ratecap_core_config_version_info{hash}` for detecting cross-replica config divergence during a rollout.
- Coverage measurement + a 50% floor CI gate across all Go modules and the Python SDK; a Gremlins mutation-testing CI gate scoped to `services/core/limiter` and `packages/sdks/go`.
- Opt-in Redis Sentinel support: `RATECAP_REDIS_SENTINEL_ADDRS`/`RATECAP_REDIS_SENTINEL_MASTER_NAME` on `ratecap-core`, and a `redis.sentinel.enabled` Helm values flag (default `false`).
- Gradual Tier 4 shed-curve ramping (`RATECAP_SHED_RAMP_START_PCT`), replacing a hard on/off cutoff.
- A new `SetDynamicLimit` gRPC RPC and a sidecar `/admin/set-limit` HTTP endpoint (gated by a new, separate `RATECAP_ADMIN_SECRET`) for instantly changing Tier 1's rate or Tier 3's `reserved_critical_pct` fleet-wide without a config re-parse — plus `ratecapctl admin set-limit`.

### Security

- The new admin lever requires its own secret (`RATECAP_ADMIN_SECRET`), separate from `RATECAP_SHARED_SECRET`, given its fleet-wide and effectively unbounded blast radius compared to a normal `/check` call.

## [2.5.0] — 2026-08-28 — Phase 0 Housekeeping & Phase 1 Observability Foundation

Minor release combining two v3 upgrade roadmap phases, both cut in the same commit: Phase 0 (housekeeping and quick wins, sequenced before any phase that needed a trustworthy version number to build on) and Phase 1 (`services/core` gains self-instrumentation it previously had none of, the sidecar's `/metrics` no longer shares its self-throttle limiter with real traffic, and both services' health checks reflect real backing-service connectivity instead of static/startup-only state).

**Correction:** Phase 0's work below was originally given its own standalone `## [2.4.1]` heading in this file. No `v2.4.1` tag or `VERSION` state ever existed — the commit that added that heading (`chore: cut v2.4.1 (Phase 0 backfill) and v2.5.0 (Phase 1) in CHANGELOG, bump VERSION to 2.5.0`) bumped `VERSION` directly from `2.4.0` to `2.5.0`. That heading has been merged into this entry, which is the only one that ever actually shipped.

### Added — Phase 0: Housekeeping & Quick Wins

- `.github/dependabot.yml` covering all Go module directories plus `pip`, `github-actions`, and `docker` ecosystems, grouped, on a weekly schedule.
- `VERSION` as the single authoritative version source.
- Merged `fix/v3-config-validation` (Tier 1 `rate_limiter` config validation) and `fix/v3-breaking-wire-changes` (`PRIORITY_UNSPECIFIED` proto enum sentinel — a breaking wire-format renumbering, called out explicitly rather than shipped silently).

### Added — Phase 1: Observability Foundation

- `ratecap-core` `/metrics` endpoint (new `:9092` listener) — gRPC request count/latency by method and status, Redis call latency/error count, config-reload success/failure count.
- `ratecap_fail_open_total{tier,reason}` — Tier 1 (request-rate) now fails OPEN on a Redis/store error instead of surfacing an internal error, matching Stripe's documented precedent; Tiers 2/3 remain fail-closed by design (see `ARCHITECTURE.md`'s new Observability section for the full per-tier contract).
- `ratecap_decision_latency_seconds{tier}`, `ratecap_release_total{result}`, and `ratecap_upstream_errors_total{endpoint}` on the sidecar.
- Starter Grafana dashboard (`deploy/grafana/ratecap-overview.json`) and baseline alert rules (`deploy/grafana/ratecap-alerts.yml`).
- An Observability section in `ARCHITECTURE.md` documenting the full metrics contract, the per-tier Redis-down degradation contract, and current tracing limitations.

### Fixed — Phase 0: Housekeeping & Quick Wins

- Tier 2's bounded-queueing backlog counter is now Redis-backed (`store.IncrConcurrent`/`DecrConcurrent` against a `backlog:` key namespace) instead of a per-instance `atomic.Int64` — with N core replicas, the real ceiling was previously `maxBacklog × N`, not one shared ceiling.
- `bench_run.go`'s `--acquire` path no longer silently drops accepted/rejected/errored request outcomes into the same latency distribution; results are now bucketed separately, and every ticket's `Release()` is called even when the request itself was rejected.
- Dependency skew across `proto`/`services/core`/`services/sidecar` closed by merging the 4 open Dependabot PRs (grpc, go-redis, testcontainers, x/sys, x/text) in lockstep rather than per-module.

### Fixed — Phase 1: Observability Foundation

- Sidecar `/healthz` now reflects real gRPC connectivity to core instead of unconditionally returning 200.
- Core's gRPC health service now reflects real Redis connectivity (re-checked every 5s) instead of being set to `SERVING` once at startup and never updated again.
- `/metrics` and `/healthz` on the sidecar no longer share the process-wide self-throttle rate limiter with `/check`/`/release` — a Prometheus scrape can no longer be 429'd by the same limiter throttling real traffic.

## [2.3.2] — 2026-07-20 — Tier 2 Concurrency-Token Security Hotfix

**Semver note:** this release contains a breaking wire-format change (see below) but was tagged as a patch bump (2.3.1 → 2.3.2), inconsistent with this file's stated intent to follow Semantic Versioning. Documented here as an acknowledged exception rather than corrected retroactively (re-tagging a already-published release would be worse) — going forward, a breaking change gets at minimum a minor bump, called out explicitly in its own CHANGELOG entry, the way this one now is.

Patch release closing issues #12 and #13, both confirmed exploitable via real-world CVE precedent (Portainer CVE-2026-44883, nhost CVE-2026-34969).

### Security

- `ReleaseConcurrency` now verifies an HMAC-SHA256 signature over the concurrency token before releasing a slot, rejecting a forged/tampered/wrong-key token with `codes.PermissionDenied` (#12).
- **Breaking change:** `/release`'s `key` and `token` now travel as request headers (`X-RateCap-Concurrency-Key`/`X-RateCap-Concurrency-Token`) instead of URL query parameters, closing a real leakage vector via proxy/access logs and `Referer` headers (#13). Any direct HTTP caller of `/release` bypassing the Go/Python SDKs must migrate to the new headers.
- New required env var `RATECAP_CONCURRENCY_SIGNING_KEY` on `ratecap-core` (fail-closed at startup).

## [2.3.1] — 2026-07-20 — Tier 2/3/4 Hardening Batch

Patch release: 5 merged PRs (#42-#46) closing 14 tracked issues.

### Added

- Tier 2 hardening: `RetryAfterMs` on rejection, `DecrConcurrent` regression tests, shadow-mode docs (#8, #9, #11).
- Tier 3 hardening: sheddable-cap rounding tests, shadow-mode reservation test, mixed-priority atomicity test, blast-radius docs (#14, #17, #18, #19).
- Tier 4 hardening: `X-RateCap-Shed-Tier` header, in-flight metrics gauge, HTTP server timeouts, `/worker-demo` docs (#20, #22, #24, #25).
- Sidecar error logging: real upstream errors now logged server-side on `/check` and `/release` failures (#41).

### Fixed

- Config watcher debounce: fixes a flaky fsnotify-related test via event coalescing (#37).

## [2.3.0] — 2026-07-19 — Phase 4 Production-Readiness & Adoption

### Added

- Production-readiness and adoption work per Phase 4 of the v2 roadmap (PR #40) — see `docs/superpowers/specs/2026-07-18-v2-phase-4a-comparison-table-design.md`, `2026-07-18-v2-phase-4b-benchmarks-design.md`, and `2026-07-19-v2-phase-4c-helm-chart-design.md` for the full design rationale behind this release's comparison table, benchmarks, and Helm chart.

## [2.2.0] — 2026-07-17 — Phase 3 Bounded Queueing (ConcurrencyLimiter)

### Added

- Bounded queueing for Tier 2's `ConcurrencyLimiter` (PR #34) — see `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.

## [2.1.0] — 2026-07-17 — Phase 2 Developer Tooling

### Added

- `ratecapctl` CLI and the Python SDK (PR #33) — see `docs/superpowers/specs/2026-07-17-v2-phase-2-tooling-design.md`.

## [2.0.0] — 2026-07-16 — Phase 1 Foundations

### Added

- Observability, structured logging, and optional mTLS (PR #31) — see `docs/superpowers/specs/2026-07-17-v2-phase-1-foundations-design.md`.

## [1.0.1] — 2026-07-16 — Config Validation Gaps

### Fixed

- Config validation gaps found in a post-v1.0.0 audit (PR #29) — see `docs/superpowers/specs/2026-07-16-v1.0.1-config-validation-gaps.md`.

## [1.0.0] — All Four Tiers — 2026-07-16

RateCap's v1 scope is complete: all four of Stripe's rate-limiter/load-shedder mechanisms are implemented, live-e2e-verified, and audited (correctness, concurrency-safety, security, architecture lenses via multi-agent adversarial review) end to end — SDK → sidecar → core → Redis, plus a sidecar-local fourth tier with no Redis round-trip at all.

### Added — Tier 2: Concurrent Requests Limiter

- `ConcurrencyLimiter` (a `Limiter` sibling to `TokenBucketLimiter`) bounding simultaneous in-flight requests per key via a Redis sorted set (`ZADD`/`ZCARD`/`ZREM`, atomic reap-count-add Lua script for stale-entry cleanup).
- `Pipeline` composing ordered `Limiter` tiers with short-circuit on the first non-`ALLOW` decision, accumulating every tier's reservations (not just the last one) so a request that trips multiple tiers correctly carries and releases all of them.
- SDK `Acquire()`/`Ticket.Release()` API; sidecar `/check` (returns `Concurrency-Token`/`Concurrency-Key` headers) and new `/release` endpoint.
- Shared-secret gRPC authentication between sidecar and core (fail-closed on missing `RATECAP_SHARED_SECRET`), sanitized gRPC error responses, HTTP method enforcement (`/check` GET-only, `/release` POST-only), and explicit `SECURITY.md`/`ARCHITECTURE.md` documentation of the plaintext-internal-network v1 trust boundary.

### Added — Tier 3: Fleet Usage Load Shedder

- `FleetShedder` — mechanically identical to Tier 2 (same Redis sorted-set lifecycle) but keyed globally (`"fleet"`, never `req.Key`) with a priority-dependent effective cap: critical traffic checked against the full fleet cap, sheddable traffic against a reduced cap (`cap*(1-reservedCriticalPct/100)`), so critical traffic always has reserved headroom.
- `Priority` (`SHEDDABLE`/`CRITICAL`) made load-bearing on the wire contract; `skip_concurrency_limit` renamed to the tier-agnostic `skip_reservations` — v1's final shape, exactly two reservation-issuing tiers.
- `Config.Validate()` enforcing `fleet_shedder.default_max_concurrent > 0` and `reserved_critical_pct` in `[0,100]`, failing closed on startup and skipping (not crashing on) a bad hot-reload — closes a silent 100%-outage config gap.

### Added — Tier 4: Worker Utilization Load Shedder

- `worker.Shedder` — a dependency-free, sidecar-local atomic in-flight request counter (`Allow()`/`Release()`, `CompareAndSwap` retry loop) checked before any gRPC call to core — genuinely zero round-trip on the shed path, unlike every other tier.
- Wired into `Handler.ServeHTTP` as a pre-check, configured via `RATECAP_MAX_INFLIGHT_REQUESTS` (soft-fail to a default of 500 on an unparseable value).
- Critical-priority requests bypass the shedder check entirely (`Allow()`/`Release()` never called), closing a priority-blind-starvation gap found by this tier's own pre-PR audit where ordinary sheddable load could 503 a critical request before Tier 3's reserved-capacity carve-out ever ran.

### Fixed

- `Pipeline.Check` silently dropping an earlier tier's reserved token when a later tier rejected the request (dormant in Tier 2 alone, guaranteed to leak a slot once Tier 3 shipped downstream) — fixed via `Decision.Reservations []TokenReservation`, where each reservation self-describes its own key.
- `ReleaseConcurrency` hardcoding `req.Key` as the release key instead of the server-supplied reservation key.
- A genuine check-then-act concurrency race in `worker.Shedder.Allow()`'s original `Load()`-then-`Add()` implementation, caught by a reviewer's stress test (~2-5% overshoot at `-count=200`) and fixed with a `CompareAndSwap` retry loop before it ever shipped.

### Follow-up work (tracked as issues, not blocking this release)

18 Minor/Important findings from the Tier 2/3/4 audits — spec-fidelity gaps, missing edge-case tests, observability gaps, and documentation nits — are filed as individual GitHub issues (#8–#25) rather than fixed inline, per this project's established audit-then-triage workflow.

## [0.1.0] — Walking Skeleton — 2026-07-13

The first working slice of RateCap: Tier 1 (Request Rate Limiter) proven end-to-end across every architectural seam — SDK → sidecar → core → Redis — before Tiers 2–4 are built on the same plumbing.

### Added

- `proto/` — the `RatecapService` gRPC contract (`CheckRateLimit` RPC, 4-value `Action` enum: `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`).
- `services/core` — the central engine:
  - `store` — `StateStore` interface with a Redis-backed implementation using an atomic Lua token-bucket script.
  - `limiter` — `Limiter` interface with `TokenBucketLimiter`, pure decision logic with no Redis dependency, unit-tested via a fake store.
  - `config` — YAML config loading and `fsnotify`-based hot-reload, with error-logging on reload failure and hardening for atomic file replacement.
  - `grpcserver` + `main.go` — wires everything into a running gRPC service.
- `services/sidecar` — the local proxy:
  - `proxy` — priority-header resolution (`x-ratecap-priority` → route config → safe default) and the HTTP handler forwarding checks to core.
  - `shadow` — per-tier and global (`RATECAP_SHADOW_MODE`) shadow-mode override for safe production rollout.
- `packages/sdks/go` — a thin Go client SDK wrapping the sidecar's HTTP endpoint.
- `deploy/` — a Docker Compose demo (core + sidecar + Redis + sample app) proving real rate-limiting end-to-end.
- `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` — the full v1 design spec.

### Fixed

- A data race in `TokenBucketLimiter.Reconfigure`, which mutated shared config fields with no synchronization while `Check` read them concurrently from gRPC handler goroutines — caught by the race detector before it shipped, fixed with a `sync.RWMutex`.
- A protobuf descriptor-path leak (`proto/ratecap/v1/...` instead of the idiomatic `ratecap/v1/...`) from an initial `protoc` invocation missing `-I proto`.

### Post-review fixes

- Added a characterization test pinning the sidecar's current "parse the priority header, don't act on it" behavior — a regression tripwire for when Tier 3 makes priority load-bearing.
- Replaced `GOWORK=off` (which caused Docker's build cache to ignore `go.work.sum` entirely) with a build-context-scoped `go.work` generated inline in each Dockerfile, so a real dependency bump now correctly invalidates the Docker layer cache.
