# Signed, Multi-Platform CLI Release Binaries — Implementation Plan

## Context

OpenSSF Scorecard's live score (https://api.scorecard.dev/projects/github.com/sairam0424/RateCap) shows **Signed-Releases: 0/10**. Root cause, confirmed by reading the current `publish-release.yml`: SBOM files are already attached to each GitHub Release as assets (`anchore/sbom-action`), and build provenance is already attested — but only for the OCI image/chart *digests*, never for anything attached to the GitHub Release itself. Scorecard's Signed-Releases check specifically looks for downloadable release assets with a verifiable signature; nothing on this repo's Release pages currently qualifies.

The right fix isn't to attach a token signed file just to move a metric — it's to ship the feature that would naturally produce one: **`ratecapctl` currently has no downloadable binary at all** (only `go install`/build-from-source), which is a real, separate discoverability gap for anyone without a Go toolchain. This plan adds cross-platform signed binaries, closing both gaps at once. Version bump: **2.11.0 → 2.12.0** (minor: new distribution artifact, no behavior change).

## Global Constraints

1. Anti-bypass, empirical verification, no fabricated data — identical to every prior phase.
2. New dependencies pre-approved for this plan only: none beyond what's already in `publish-release.yml` (`cosign`, `actions/attest-build-provenance`) — reuse them, don't add a new tool (no GoReleaser; a hand-rolled `GOOS`/`GOARCH` matrix matches this repo's existing CI style, e.g. `docker-build`'s matrix jobs).
3. Every new GitHub Actions reference pinned to a full commit SHA with a `# vX.Y.Z` comment.
4. The real, definitive proof that this fixes Scorecard's Signed-Releases check only happens after this ships as a real tagged release — Final Review verifies everything it can locally/statically; re-checking the live Scorecard score after the actual release is a follow-up step, not something to fake now.

## Tasks

**Task 1 — Add cross-platform `ratecapctl` binary build to `publish-release.yml`**
New job `publish-cli-binaries` (same `v*` tag trigger). Matrix: `{linux,darwin,windows} x {amd64,arm64}` minus `windows/arm64` (5 real targets: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64). For each: `cd cli && GOOS=$os GOARCH=$arch go build -ldflags "-X github.com/sairam0424/RateCap/cli/cmd.Version=${{ github.ref_name }}" -o ratecapctl_<os>_<arch>[.exe] .` — reuse the exact ldflags pattern already documented in `README.md`/`cli/cmd/root.go`. Collect all 5 binaries in one job (or a matrix + a final gather job — implementer's call, whichever is simpler and correct), generate `checksums.txt` (`sha256sum`) covering all 5, and upload everything (5 binaries + `checksums.txt`) as assets on the GitHub Release via `gh release upload` (needs `contents: write`, scoped narrowly to this job only).

**Task 2 — Sign and attest the release binaries**
Sign `checksums.txt` with `cosign sign-blob --yes` (keyless, matching the exact pattern the Helm chart job already uses for its own digest) and attach the resulting signature/bundle as a Release asset too. Attest build provenance via `actions/attest-build-provenance` with `subject-path` covering the 5 binaries + `checksums.txt` (needs `id-token: write`, `attestations: write`). Least-privilege `permissions:` block on the job, following `publish-images`'/`publish-helm-chart`'s existing per-job pattern.

**Task 3 — Documentation + version bump**
Add a "Downloading a release" section to `README.md` (which platform/arch to grab, `chmod +x`) and a "Verifying a release" section (real, copy-pasteable `cosign verify-blob`/`gh attestation verify` commands matching the actual filenames this job produces — write them, then actually run them against a real prior artifact shape to confirm the command syntax is correct before committing to the doc). `CHANGELOG.md` entry explaining why (Scorecard's Signed-Releases gap + the missing-binary-download gap, fixed together). Bump `VERSION` to `2.12.0`.

## Verification

`gofmt -l .`/`go build ./...` unaffected (no production code changes). Validate the new/modified workflow YAML with a real parser; confirm every new action reference is a real, SHA-pinned commit. If feasible in the sandbox, dry-run at least one platform's build locally (`GOOS=linux GOARCH=arm64 go build ...` from `cli/`) to confirm the ldflags/output-naming convention actually works before trusting the matrix. The live Scorecard re-check happens after this ships as the real `v2.12.0` tag — note this explicitly in Final Review's report rather than claiming the fix is confirmed before that.
