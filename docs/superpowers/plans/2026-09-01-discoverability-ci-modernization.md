# RateCap Discoverability & CI/CD Modernization — Implementation Plan

## Context

RateCap (v2.10.1, fully audited and production-dry-run verified) has zero external adoption levers working correctly, discovered by directly auditing the live repo/GitHub state and cross-checking against current (2026) OSS discoverability + GitHub Actions best-practice research (deep-research workflow: 125 claims across 28 sources, 25 adversarially verified, 8 explicitly refuted and excluded below):

- **Go module paths are broken for every external consumer.** All 6 modules (`core`, `sidecar`, `proto`, `sdk-go`, `cli`, `sampleapp`) declare `module github.com/ratecap/*` — but `github.com/ratecap` returns a real 404 (confirmed via curl and a `go-get=1` probe). `go get`, `go install`, and pkg.go.dev indexing are all non-functional today. Since nobody could ever successfully depend on the old path, fixing this breaks zero real consumers. User confirmed: rename to the real path `github.com/sairam0424/RateCap/...`, matching envoyproxy/ratelimit's actual precedent (the closest comparable OSS project) and research's explicit guidance that vanity-import hosting isn't worth the overhead under ~5 packages with no plan to leave GitHub.
- **The Helm chart's auto-publish has been silently broken all session.** `publish-release.yml`'s `publish-helm-chart` job uses `helm/chart-releaser-action`, which requires a `gh-pages` branch — confirmed via `git ls-remote`/`git branch -a` that this branch has **never existed**, locally or on origin. Every release where chart files changed (v2.4.0, v2.6.0, v2.7.0, v2.9.0, v2.10.1) failed at `fatal: invalid reference: origin/gh-pages`; releases where they didn't change (v2.5.0, v2.8.0, v2.10.0) short-circuited to a trivial success ("no chart changes detected"), masking the breakage. Docker images to GHCR, by contrast, genuinely work today.
- **Research confirms the clean fix is also the modern one**: Helm made OCI registries the default, non-experimental distribution model since v3.8.0, and Helm's own docs recommend OCI over classic `gh-pages`/`index.yaml` repos (without declaring the old model deprecated). `chart-releaser` itself still has no native OCI push support (upstream PR #622, open/unmerged) — so the right move is to stop using `chart-releaser-action` and push the chart directly via `helm push` to the *same* `ghcr.io` registry the Docker images already publish to, eliminating the `gh-pages` dependency entirely rather than fixing it in place.
- **No supply-chain security signals exist at all**: no OpenSSF Scorecard, no CodeQL, no SBOM, no build provenance/signing, and `.golangci.yml` has no security-focused linter (`gosec` absent from its deliberately-minimal 5-linter set). Research gives concrete, current, correct patterns to add — notably that `slsa-github-generator` is unmaintained and GitHub's own `actions/attest-build-provenance` is the current recommended path instead.
- **Go Report Card is dead** — officially sunset after 10+ years; do not add that badge. `golangci-lint` (already in CI) is the documented successor.
- **No community-health/discoverability scaffolding**: no issue templates, PR template, CODEOWNERS, FUNDING.yml, or `.github/release.yml`; README has exactly one badge (CI), no table of contents, no `docker pull`/GHCR mention, no Helm/Kubernetes install path at all, despite being a substantial, well-written 2,223-word README with a genuinely good Comparison table.
- Small housekeeping: `deploy/helm/ratecap/Chart.yaml`'s `appVersion` is stuck at `2.2.0` (actual latest is v2.10.1).

Everything below is scoped to what's real, verifiable, and load-bearing for adoption — not generic "add more badges" advice. Version bump: **2.10.1 → 2.11.0** (minor: new distribution channel + tooling, plus a corrective module-path change that breaks zero real consumers since the old path never worked).

## Global Constraints

1. Anti-bypass, empirical verification, no fabricated data — identical to every prior phase in this repo's history. Never bypass a denied git/CI operation; actually run `gofmt -l .`, `go build ./...`, `go test ./... -race` in every module touched; bring up the real demo stack and confirm `/healthz` before calling anything done.
2. Task 1 (module rename) touches every `.go` file with an internal import — grep the WHOLE tree for `github.com/ratecap/`, not just `go.mod` files, and don't stop until zero matches remain outside historical docs (CHANGELOG.md's past entries should NOT be rewritten — they're a historical record).
3. No new dependencies beyond the specific actions/tools this plan names (`ossf/scorecard-action`, `github/codeql-action`, `anchore/sbom-action`, `actions/attest-build-provenance`, `sigstore/cosign-installer`, `oras`, `gosec`).
4. Every new GitHub Actions reference must be pinned to a full commit SHA with a `# vX.Y.Z` comment, matching this repo's existing convention in `ci.yml`/`benchmark.yml`.
5. Files 200-400 lines typical/800 max, no comments except non-obvious WHY, gofmt-clean Go.

## Tasks

**Task 1 — Rename Go module import paths (highest-risk, do first, in isolation)**
Rename every module's declared path from `github.com/ratecap/*` to `github.com/sairam0424/RateCap/...`:
- `github.com/ratecap/core` → `github.com/sairam0424/RateCap/services/core`
- `github.com/ratecap/sidecar` → `github.com/sairam0424/RateCap/services/sidecar`
- `github.com/ratecap/proto` → `github.com/sairam0424/RateCap/proto`
- `github.com/ratecap/sdk-go` → `github.com/sairam0424/RateCap/packages/sdks/go`
- `github.com/ratecap/cli` → `github.com/sairam0424/RateCap/cli`
- `github.com/ratecap/sampleapp` → `github.com/sairam0424/RateCap/deploy/sampleapp`
- `github.com/ratecap/core/integrationtests` → `github.com/sairam0424/RateCap/services/core/integrationtests`

Update: every `go.mod`'s `module` line; every internal import statement across all `.go` files (this is the real blast radius — grep for `github.com/ratecap/` across the whole tree, not just go.mod files); all 7 existing `replace` directives (`cli/go.mod`, `deploy/sampleapp/go.mod`, `services/core/go.mod`, `services/sidecar/go.mod`, `services/core/integrationtests/go.mod`) to point at the new module names; any code snippets in `README.md`/`CONTRIBUTING.md`/`ARCHITECTURE.md` that reference the old import path. `go.work`'s `use` directives are path-based and need no change.

Verification: `gofmt -l .` clean; `go build ./...` + `go test ./... -race` green in every module; `go list -m` in each module shows the new path; `golangci-lint run` clean in every module; bring up the demo stack and confirm `/healthz` + `/checkout` still work (proves the rename didn't silently break a runtime import).

**Task 2 — Replace broken Helm gh-pages publish with OCI push to GHCR**
In `.github/workflows/publish-release.yml`, replace the `publish-helm-chart` job (currently `helm/chart-releaser-action` targeting a `gh-pages` branch that has never existed) with: `helm package deploy/helm/ratecap` → `helm push` to `oci://ghcr.io/sairam0424/charts` (Helm's OCI support has been default/non-experimental since v3.8.0) → sign the pushed chart digest with `cosign` (keyless, matching how a real comparable project's CI does exactly this) → push an `artifacthub-repo.yml` as a separate `:artifacthub.io`-tagged OCI artifact next to the chart (via `oras`), which is what lets Artifact Hub discover and verify it — this is a different mechanism than the `gh-pages`-based one, not a config tweak to the old path. Fix `deploy/helm/ratecap/Chart.yaml`'s stale `appVersion` (bump to match the current `VERSION`) and add the `artifacthub.io/*` annotations Artifact Hub expects (license, maintainers). Give the job the least-privilege `permissions:` block it needs (`packages: write`, `id-token: write` for cosign) — `ci.yml`'s existing pattern of per-job explicit permissions (top-level default is `contents: read` only) is the template to follow.

Verification: run `helm package`/`helm push` for real against `ghcr.io` (this repo already authenticates there for Docker images, so the same `GITHUB_TOKEN` works for chart push too) and confirm the chart actually lands in the registry; confirm no step references `gh-pages` or `chart-releaser-action` anywhere anymore.

**Task 3 — Add OpenSSF Scorecard**
New `.github/workflows/scorecard.yml`: `ossf/scorecard-action` (pinned to a real commit SHA, matching this repo's existing convention), triggered on push-to-main, a weekly cron, and branch-protection-rule changes; upload results as SARIF to the Security tab (needs `security-events: write`, `id-token: write` per the action's real requirements); `publish_results: true` so the public score/API is live. Add the auto-updating README badge (`https://api.scorecard.dev/projects/github.com/sairam0424/RateCap/badge`).

**Task 4 — Add CodeQL for Go**
New `.github/workflows/codeql.yml` using `github/codeql-action`, Go language, `autobuild` mode, SHA-pinned, triggered on push/PR to `main`/`develop` plus a weekly cron (standard CodeQL default-setup shape). This is genuinely additive — `.golangci.yml`'s 5 linters (errcheck/govet/ineffassign/staticcheck/unused) don't do CodeQL-style dataflow/security analysis.

**Task 5 — SBOM + build provenance on release artifacts**
In `publish-release.yml`'s `publish-images` job: generate an SBOM per image with `anchore/sbom-action` (syft under the hood, SPDX format) and attach it to the GitHub Release; attest build provenance with `actions/attest-build-provenance` (the current GitHub-recommended path — research confirms `slsa-framework/slsa-github-generator` is no longer maintained, so do not use it) for both the pushed container images and the Helm chart from Task 2. Needs `id-token: write`, `attestations: write` on that job.

**Task 6 — Add `gosec` to `.golangci.yml`, fix real findings before enabling**
Add `gosec` to the `enable:` list. Per this repo's own established discipline (Phase 5's golangci-lint rollout): run it for real against the current tree first, fix genuine findings directly, and only narrowly `//nolint:gosec` a specific line with a reason where a finding is a deliberate, safe pattern — never a blanket exclusion.

**Task 7 — Community-health files**
`.github/ISSUE_TEMPLATE/bug_report.yml` and `feature_request.yml` (structured YAML forms, current GitHub convention, not freeform Markdown); `.github/PULL_REQUEST_TEMPLATE.md` (short — link to `CONTRIBUTING.md`'s existing conventions rather than duplicating them); `.github/CODEOWNERS` (`* @sairam0424`); `.github/release.yml` (PR categorization config for GitHub's native auto-generated release notes) — added as a **supplement** attached to each GitHub Release alongside the existing hand-written `CHANGELOG.md` narrative, not a replacement (the manual CHANGELOG entries are materially more detailed than auto-generated notes would be).

**Task 8 — README updates**
Badge row: License, Latest Release, OpenSSF Scorecard (no Go Report Card — confirmed dead; no coverage badge — no badge-hosting service wired up, not worth adding just for a badge). A short table of contents (justified at 2,223 words / 9 sections — none existed before). A `docker pull ghcr.io/sairam0424/ratecap-{core,sidecar,sampleapp}` alternative alongside the existing build-from-source Quick Start. A Helm install mention (`helm install ratecap oci://ghcr.io/sairam0424/charts/ratecap`) once Task 2 ships. Keep everything else (the Comparison table, Benchmarks, Design docs sections) as-is — they're already a genuine strength, not something to change.

**Task 9 — Docs + version bump**
`CHANGELOG.md`: document the module-path fix explicitly as a correction, not a feature ("previously-unreachable `github.com/ratecap/*` paths corrected to the real, resolvable path — no real consumer is broken, since the old path never worked"); document the Helm OCI migration and why (`gh-pages` never existed, OCI is now upstream's own recommended model). `ARCHITECTURE.md`: update any Go import-path examples, add a short note on the Helm OCI distribution model. Bump `VERSION` to `2.11.0`.

## Explicitly out of scope (advisory only, user's call — do not implement)

- awesome-go submission (needs a root go.mod; RateCap's go.work structure has none by design).
- FUNDING.yml (optional, user's call on links).
- Launch sequencing (Show HN, blog post, Terminal Trove/r/commandline, skipping Lobsters) — advisory guidance already given to the user directly, not implementation work.

## Verification (end to end, after all tasks)

Full repo build/test/lint sweep (every module); bring up the real demo stack and confirm `/healthz`/`/checkout`; confirm the new Helm OCI push actually lands a pullable chart in `ghcr.io`; confirm the Scorecard/CodeQL workflows are syntactically valid and reference real, resolvable actions; confirm README badges are well-formed. Ship via the same push → PR → develop → CI → merge → PR → main → CI → merge → tag → release flow used for v2.9.0 through v2.10.1.
