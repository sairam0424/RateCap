# E2E Audit Remediation — Implementation Plan

## Background

`docs/superpowers/plans/2026-09-01-e2e-audit-dryrun-ledger.md`'s corresponding SDD workflow ran a full static audit + live production dry run against `main` @ v2.10.0. 8 medium+ findings were independently adversarially re-verified (8/8 confirmed), and 7 live scenarios surfaced 5 additional real defects (one — the Helm `adminSecret` guard — reproduced via an actual `kind` cluster install failure). This plan fixes every confirmed real defect. Version bump: **2.10.0 → 2.10.1** (patch: bug fixes and doc corrections only, no new features, no behavior change to any request-path decision).

## Global Constraints

1. Anti-bypass, empirical verification, no fabricated data — identical to every prior phase in this repo's history.
2. Every fix must be verified against the SAME failure mode that was originally reproduced — e.g. Task 1's fix must be proven by actually re-running a real `helm install` (kind cluster or `helm template` + schema check) with only the README's documented required secrets set, not just by re-reading the template.
3. No new dependencies.
4. Files 200-400 lines typical/800 max, no comments except non-obvious WHY, gofmt-clean Go.

---

## Task 1 — Fix Helm `adminSecret` install-breaking bug (most severe finding)

**Files:** `deploy/helm/ratecap/templates/sidecar.yaml`.

**Fix:** wrap the `RATECAP_ADMIN_SECRET` env block in `{{- if .Values.adminSecret.existingSecretName }} ... {{- end }}`, matching the existing `tls.enabled` guard pattern already used elsewhere in the same file. When unset, the env var must simply not be present in the container spec (matching how the sidecar's own Go code already treats `RATECAP_ADMIN_SECRET` as optional).

**Verification:** `helm install` (into a real `kind` cluster if available, else `helm template` piped through a schema check) using ONLY `sharedSecret.existingSecretName` and `concurrencySigningKey.existingSecretName` — exactly reproducing the audit's failing command — must now succeed. Also verify the admin-secret-set case still works (`admin.go`'s tests / a live `/admin/set-limit` call) unaffected.

---

## Task 2 — Wire the Helm chart's `tls.mode` through to `RATECAP_TLS_MODE`; gate the NetworkPolicy port

**Files:** `deploy/helm/ratecap/values.yaml`, `deploy/helm/ratecap/templates/core.yaml`, `deploy/helm/ratecap/templates/networkpolicy.yaml`, `deploy/helm/ratecap/README.md`.

**Fix:**
- Add `tls.mode` to `values.yaml` (default `"strict"`, preserving today's exact behavior when `tls.enabled=true` — do not silently change the current default for existing chart users).
- In `templates/core.yaml`, set `RATECAP_TLS_MODE: {{ .Values.tls.mode }}` when `tls.enabled` is true.
- In `templates/networkpolicy.yaml`, gate the port-9443 ingress rule on `.Values.tls.mode == "permissive"` (not just `.Values.tls.enabled`) — the port core actually opens is conditional on the mode, so the NetworkPolicy must be too.
- Update the Helm README's mTLS section to document `tls.mode` (`strict`|`permissive`) alongside `tls.enabled`.

**Verification:** `helm template --set tls.enabled=true --set tls.mode=permissive` vs `--set tls.mode=strict` must now render DIFFERENT `RATECAP_TLS_MODE` values and different NetworkPolicy port sets (the audit's own reproduction — diffing the two renders — must now show a real diff, not byte-identical output).

---

## Task 3 — Fix CHANGELOG.md's phantom `[2.4.1]` entry

**Files:** `CHANGELOG.md`.

**Fix:** read the `[2.4.1]` section's real content and the `[2.5.0]` section right above it (both added in the same commit per the audit's `git show` evidence). Merge `[2.4.1]`'s real, accurate content into `[2.5.0]`'s entry (it documents real work that genuinely shipped — it was just never given its own tag), and remove the standalone `[2.4.1]` heading entirely, since no `v2.4.1` tag or `VERSION` state ever existed. Add a one-line note at the merge point acknowledging the correction if that's clearer than a silent merge.

**Verification:** `git tag -l` cross-checked against every remaining `## [X.Y.Z]` heading in `CHANGELOG.md` — every heading must now correspond to a real, existing tag.

---

## Task 4 — Fix README.md's broken Quick Start

**Files:** `README.md`.

**Fix:** the Benchmarks section already has the correct sequence (cert-gen step included). Apply the same fix to the top-level Quick Start section: add the `bash deploy/generate-demo-certs.sh` step before `docker compose up --build`, matching `CLAUDE.md`'s own already-correct "run the demo stack" instructions.

**Verification:** actually run the Quick Start's exact commands, in order, on a clean checkout — `docker compose up --build -d` must succeed and `/checkout` must behave as documented (200 then 429 after burst), reproducing the audit's own repro steps but this time getting the documented result instead of a crash.

---

## Task 5 — Fix `deploy/sampleapp/main.go`'s `/fleet-demo` release wire format and reservation leak

**Files:** `deploy/sampleapp/main.go`.

**Fix:**
- Change the release call to send `X-RateCap-Concurrency-Key`/`X-RateCap-Concurrency-Token` as HTTP headers (matching `services/sidecar/proxy/proxy.go`'s `ReleaseHandler`), not query parameters.
- Ensure the Tier-2 per-key concurrency slot is released even when the sidecar's `/check` response is a non-200 (503 shed) — read `Concurrency-Token-N`/`Concurrency-Key-N` response headers before the early-return-on-non-200 branch, not after.

**Verification:** hammer `/fleet-demo` with a burst of concurrent requests (mix of allowed and shed), then check `redis-cli TTL cc:fleet` / the pool's real occupancy in Redis directly — it must return to its true in-use count promptly, not sit exhausted for the full `max_request_duration_ms` reaper window. Reproduce the audit's own check (replay the exact old vs new wire format against a live sidecar) to prove the fix.

---

## Task 6 — Fix `deploy/sampleapp/main.go`'s dropped response headers on relay

**Files:** `deploy/sampleapp/main.go`.

**Fix:** in `/fleet-demo` and `/worker-demo`, copy the relevant response headers (`X-RateCap-Shed-Tier`, `Retry-After-Ms`, `RateLimit-Reset` — whichever are present) from the sidecar's `/check` response onto the outgoing `http.ResponseWriter` before calling `WriteHeader`, for every status code, not just 200.

**Verification:** header-dump comparison (the audit's own method) between a direct sidecar `/check` call and the sampleapp-relayed equivalent for the same decision — headers must now match.

---

## Task 7 — Fix Go SDK's dropped `RateLimit-Reset`

**Files:** `packages/sdks/go/client.go`, its test file.

**Fix:** in both `Allow()` and `Acquire()`, parse the `RateLimit-Reset` response header alongside the existing `Retry-After-Ms` parsing, and expose it on the returned value (add a field, following whatever naming convention the existing `RetryAfterMs`-equivalent field uses).

**Verification:** a test asserting a rejected `Allow()`/`Acquire()` call against a fake sidecar server returning both headers surfaces both values correctly.

---

## Task 8 — Add missing `dependabot.yml` entry

**Files:** `.github/dependabot.yml`.

**Fix:** add a `gomod` entry for `/services/core/integrationtests`, matching the existing 6 entries' exact shape (weekly schedule, grouped).

**Verification:** `.github/dependabot.yml` now has 7 `gomod` entries, one per real Go module directory (`find . -name go.mod` minus `.claude/worktrees`).

---

## Task 9 — Add resource requests/limits to the Helm chart

**Files:** `deploy/helm/ratecap/values.yaml`, `deploy/helm/ratecap/templates/core.yaml`, `templates/sidecar.yaml`, `templates/redis.yaml`, `templates/sampleapp.yaml`.

**Fix:** add a `resources` values block per component (core, sidecar, redis, sampleapp) with sane conservative defaults (e.g. `requests: {cpu: 100m, memory: 128Mi}`, `limits: {cpu: 500m, memory: 256Mi}` — adjust per component's actual footprint), templated the same way `image`/`replicaCount` already are, and wire each Deployment/StatefulSet template to consume it.

**Verification:** `helm template` across the same combinations the audit tested — every container spec now has a `resources:` block with both `requests` and `limits`.

---

## Task 10 — Fix the benchmark overlay's hidden RPS ceiling

**Files:** `deploy/docker-compose.bench.yml`.

**Fix:** add `RATECAP_SIDECAR_MAX_RPS` (and matching burst, if the sidecar's `resolveMaxRPS` takes a separate burst env var — check `services/sidecar/main.go`) to the sidecar service's environment override, raised well above any realistic benchmark throughput this stack can produce (e.g. 50000), alongside the existing `RATECAP_MAX_INFLIGHT_REQUESTS` override.

**Verification:** reproduce the audit's own definitive proof — rerun the loosened-stack benchmark at a throughput that previously showed silent rejections (the audit used `--qps 900` vs uncapped) and confirm zero unexplained rejections now, matching the "almost nothing rejected" methodology the README's Benchmarks section actually relies on.

---

## Task 11 — Documentation fixes: CONTRIBUTING.md, ARCHITECTURE.md, README.md, SECURITY.md

**Files:** `CONTRIBUTING.md`, `ARCHITECTURE.md`, `README.md`, `SECURITY.md`.

**Fix:**
- `CONTRIBUTING.md`: fix the Build loop to include `cli` and `services/core/integrationtests`; fix the Test loop to include `cli`. Mirror `CLAUDE.md`'s already-correct lists exactly.
- `ARCHITECTURE.md`: add `packages/sdks/python/` to the Component overview, and add the pending-PyPI-publish status to its "Known limitations" subsection (currently scoped only to tracing).
- `README.md`: add `cli/` and `packages/sdks/python/` to the "Project layout" section.
- `SECURITY.md`: add `packages/sdks/python` and `cli` to both the vulnerability-report component list and the "In scope"/"Out of scope" sections (decide explicitly which side each belongs on — both handle real security-relevant surface, so "in scope" is the right call for both).

**Verification:** re-run the audit's own cross-checks (grep each doc for the items it previously found missing) and confirm they're now present and accurate.

---

## Task 12 — Defense-in-depth: validate `Cost > 0` server-side in core

**Files:** `services/core/grpcserver/server.go`, its test file.

**Fix:** add an explicit `req.Cost <= 0` rejection (or clamp to 1) in `CheckRateLimit`, so the guarantee doesn't rely solely on the sidecar's own `resolveCost` validation — defense in depth for any other authenticated direct gRPC caller.

**Verification:** a test sending `Cost: -1` (or `0`) directly to `CheckRateLimit` gets a clear, correct rejection/clamp rather than relying on the self-correcting refill-reclamp behavior the audit traced through.

---

## Task 13 — Version bump

**Files:** `VERSION`, `CHANGELOG.md`.

**Fix:** bump `VERSION` to `2.10.1`; add a `## [2.10.1]` entry summarizing this remediation (link back to the audit).
