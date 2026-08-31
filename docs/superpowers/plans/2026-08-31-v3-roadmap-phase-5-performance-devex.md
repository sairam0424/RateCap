# Phase 5 — Performance & DevEx Polish — Implementation Plan

Source: `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 5 (10 items). This is the **final** phase of the v3 roadmap. Version bump: **2.8.0 → 2.9.0** (minor, tooling/CLI/DevEx — no breaking wire or config changes).

## Global Constraints

Apply identically to every implementer, reviewer, and fix-loop prompt for every task below.

1. **Anti-bypass rule.** Never bypass a safety mechanism (permission hooks, sandbox flags, CI gates) to force a denied action through. If a git operation or hook is denied, report `BLOCKED` in your summary and stop — a human will resolve it. Do not retry with `--no-verify`, `dangerouslyDisableSandbox`, or similar.
2. **Empirical verification over assumption.** Before marking any task done, actually run what you changed: `go build ./...` and `go test ./... -race` in every Go module you touched, `gofmt -l .` clean, and — for any task that changes `docker-compose.yml`, a `Dockerfile`, `.github/workflows/*.yml`, or `deploy/helm/**` — bring up the real demo stack (`cd deploy && bash generate-demo-certs.sh && docker compose up --build -d`) and confirm `/healthz` returns 200 before tearing it down. "The code looks right" is not verification.
3. **No fabricated data.** Task 1 requires you to actually run the benchmark CLI against a live stack and report the real numbers it printed. Inventing plausible-looking numbers instead of running the command is a CRITICAL-severity finding, not a shortcut.
4. **No new heavy dependencies without explicit justification in this plan.** Task 2's histogram is hand-rolled (no HDR-histogram library). Task 4's resource capture shells out to `docker`/`redis-cli` rather than adding a Docker SDK or `go-redis` to the `cli` module. `golang.org/x/time` in Task 9 is pre-approved (minimal, ubiquitous, not a new ecosystem). If you believe a task genuinely needs a dependency not already named here, stop and report rather than adding it unilaterally.
5. **Ledger append is idempotent.** Before appending a `Task N: ...` line to the ledger, `grep "^Task N:"` it first; if already present, do not duplicate or overwrite it.
6. **Commit the plan doc.** This file must be committed as part of Setup's initial commit (two prior phases forgot this — check `git status` before Setup hands off).
7. **CI YAML style.** This repo pins every GitHub Action to a full commit SHA with a `# vX.Y.Z` comment (see `.github/workflows/ci.yml`). Any new action reference added in Tasks 5/6 must follow the same convention — no `@v4`-style floating tags.
8. **`VERSION` is the single source of truth** (established Phase 0). Task 9's `--version` flag must read from it (via build-time `-ldflags`, since `cli/` is a separate Go module and cannot `go:embed` a path outside its own module root) — never hardcode a second version string.
9. **Scope discipline carries over from `CLAUDE.md`:** don't add a 5th rate-limiting mechanism, storage backend, or new runtime. This phase is tooling/benchmarking/CI only — it touches zero request-path limiter logic.
10. **Known-already-done items — do not redo them.** GitHub Actions are already SHA-pinned (verified: `.github/workflows/ci.yml` uses `actions/checkout@3d3c42e...`, etc.) — Task 10 only pins **Docker base images**, not Actions. `bench_run.go`'s accept/reject/error counters already exist (Phase 0 item 3 fixed this) — Task 9 only adds `--version` and `--qps`, plus entrypoint tests.

---

## Task 1 — Publish a second benchmark run against un-loosened shipped defaults

**Goal:** the README's existing numbers use `deploy/ratecap-bench.yaml` (limits raised 100-1000x), so nothing is ever rejected — they measure pass-through overhead only, never the load-shedder doing its job. Add a second, real run against the actual shipped defaults (`deploy/ratecap.yaml`), and relabel the existing table honestly.

**Files:** `README.md` (Benchmarks section, currently starting at line 68).

**Steps:**
1. Relabel the existing "Tier 1 — `Allow()`" / "Tier 2 — `Acquire()`/`Release()`" tables' intro sentence to explicitly say these measure **headroom / pass-through overhead only** under limits raised far above shipped defaults — not a capacity or rejection-behavior number.
2. Bring up the demo stack **without** the bench overlay: `cd deploy && bash generate-demo-certs.sh && docker compose -f docker-compose.yml up --build -d` (shipped `ratecap.yaml` defaults: tier 1 rate=2/burst=5, tier 2 max_concurrent=3, tier 3 max_concurrent=5).
3. Build the CLI (`cd cli && go build -o /tmp/ratecapctl .`) and actually run:
   ```
   /tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --concurrency 50 --requests 2000 --key-prefix bench-default-tier1
   /tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --concurrency 50 --requests 2000 --key-prefix bench-default-tier2 --acquire
   ```
   (Lower `--requests` than the loosened run — 2000, not 20000 — since almost every request past the first few will legitimately reject; there's no value in 20k iterations of that.)
4. Add a new README subsection, **"Shipped defaults (load-shedder engaged)"**, directly under the existing tables, with real accepted/rejected/errored counts and the real elapsed/throughput/percentile numbers the command actually printed. State explicitly that high rejection counts here are the *expected, correct* behavior, not a bug.
5. Tear down: `docker compose -f docker-compose.yml down -v && rm -rf certs`.

**Reviewer verification:** independently re-run the same two commands against a fresh stack and confirm the README's numbers are the right order of magnitude and shape (accepted count bounded near `burst` for tier 1; most of the remainder rejected, not errored) — not that they match exactly (timing varies run to run).

---

## Task 2 — `--duration` soak mode with a bounded streaming histogram

**Goal:** replace the unbounded, mutex-guarded `[]benchOutcome` slice in `runBench` with a fixed-memory streaming histogram, and add a `--duration` flag so a run isn't bounded to a fixed request count — a real soak test needs to run for an hour and not retain millions of raw samples.

**Files:** `cli/cmd/histogram.go` (new), `cli/cmd/histogram_test.go` (new), `cli/cmd/bench_run.go` (modify), `cli/cmd/bench_run_test.go` (extend).

**`histogram.go`** — a bounded, log-spaced bucketed histogram (HDR-style approximation, no external dependency):

```go
package cmd

import (
	"math"
	"sync"
	"time"
)

// Histogram is a fixed-memory latency histogram: O(histBuckets) regardless
// of how many samples are recorded, so a multi-hour soak run has the same
// memory footprint as a one-second run.
type Histogram struct {
	mu      sync.Mutex
	buckets [histBuckets]uint64
	count   uint64
}

const (
	histMinMs   = 0.01
	histMaxMs   = 60000.0
	histBuckets = 2048
	logRange    = 0 // computed below; placeholder removed in real impl
)

func newHistogram() *Histogram { return &Histogram{} }

func (h *Histogram) Record(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	idx := bucketIndex(ms)
	h.mu.Lock()
	h.buckets[idx]++
	h.count++
	h.mu.Unlock()
}

func bucketIndex(ms float64) int {
	if ms <= histMinMs {
		return 0
	}
	if ms >= histMaxMs {
		return histBuckets - 1
	}
	ratio := math.Log(ms/histMinMs) / math.Log(histMaxMs/histMinMs)
	idx := int(ratio * float64(histBuckets))
	if idx >= histBuckets {
		idx = histBuckets - 1
	}
	return idx
}

func bucketUpperBoundMs(idx int) float64 {
	return histMinMs * math.Pow(histMaxMs/histMinMs, float64(idx+1)/float64(histBuckets))
}

// Percentile returns an approximate p-th percentile (p in [0,1]) as the
// upper bound of the bucket where the cumulative count first reaches the
// target rank. Error is bounded by the bucket's own width at that point in
// the log scale, not by sample count.
func (h *Histogram) Percentile(p float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return 0
	}
	target := uint64(math.Ceil(p * float64(h.count)))
	var cum uint64
	for i, c := range h.buckets {
		cum += c
		if cum >= target {
			return bucketUpperBoundMs(i)
		}
	}
	return histMaxMs
}

func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Reset zeroes the histogram in place, for periodic windowed reporting
// without allocating a new one every interval.
func (h *Histogram) Reset() {
	h.mu.Lock()
	for i := range h.buckets {
		h.buckets[i] = 0
	}
	h.count = 0
	h.mu.Unlock()
}
```
(Remove the unused `logRange` placeholder constant — it's a planning artifact, not real code; the implementer should delete it.)

**`bench_run.go` changes:**
- Replace `outcomes []benchOutcome` + the final `sort.Slice`/loop with three `*Histogram`s (accepted/rejected/errored aren't separately histogrammed — only accepted latencies were ever percentiled; keep that behavior) plus plain `uint64` counters for accepted/rejected/errored totals (atomics, since workers increment concurrently — no more mutex-guarded slice at all).
- Add flags: `--duration` (Go `time.Duration`, e.g. `"30s"`; zero value = today's fixed-`--requests` behavior, unchanged) and `--report-interval` (`time.Duration`, default `5s`, only meaningful when `--duration` is set).
- When `--duration > 0`: ignore the `jobs` channel entirely; each worker loops on `for { select { case <-ctx.Done(): return; default: issue one request } }` against a `context.WithTimeout(parent, duration)`. A separate goroutine ticks every `--report-interval`, prints a windowed snapshot line (`"[12s] accepted=... rejected=... errored=... p50=...ms p99=...ms"`) using a **second**, per-window `Histogram` that is `Reset()` after each print — the cumulative histogram used for the final summary is untouched by windowing.
- Preserve the exact existing text/JSON output shape for the `--requests`-only (no `--duration`) path — no test currently asserts exact percentile *values*, only presence of the fields, so switching accepted-latency percentiles from exact-sort to histogram-approximation does not break `TestBenchRun_JSONModeEmitsValidJSONWithExpectedFields`.

**New tests:**
- `histogram_test.go`: monotonic percentiles (p50 ≤ p99 ≤ p999) after recording a known distribution; a single recorded value of `X`ms returns `Percentile(0.5)` within one bucket-width of `X`; recording 1,000,000 samples doesn't change `unsafe.Sizeof`-style struct size (assert `len(h.buckets) == histBuckets` unchanged — the point is *no growth*, not a memory profiler).
- `bench_run_test.go`: `TestBenchRun_DurationModeStopsNearDeadline` — run with `--duration 200ms` against a fast `httptest` server, assert wall-clock elapsed is within a generous tolerance (e.g. 150-500ms) rather than tied to any fixed request count.

---

## Task 3 — Wire `net/http/pprof` into core and sidecar, behind a flag, loopback-only

**Goal:** zero profiling exists anywhere today. Add opt-in `net/http/pprof`, off by default, bound to loopback only (never the pod's routable interface) so enabling it in production doesn't accidentally expose profiling data cluster-wide.

**Files:** `services/core/main.go`, `services/sidecar/main.go`, plus a small integration test per service.

**Design:**
- New env var `RATECAP_PPROF_ENABLED` (bool, default `false`), read the same way `RATECAP_TLS_MODE` already is.
- When true, register `net/http/pprof`'s side-effect handlers (`import _ "net/http/pprof"` is insufficient here since it registers on `http.DefaultServeMux`, which this repo doesn't use for its public servers — instead build a **dedicated** `http.NewServeMux()`, manually register `pprof.Index`, `pprof.Cmdline`, `pprof.Profile`, `pprof.Symbol`, `pprof.Trace`, and `pprof.Handler("goroutine")`/`"heap"`/`"allocs"` etc. via `runtime/pprof.Lookup(...)`) and serve it on `127.0.0.1:6060` (core) / `127.0.0.1:6061` (sidecar) — explicitly loopback-bound, never `0.0.0.0`, regardless of what the health/metrics listeners bind to.
- Run this listener in its own goroutine, only when the flag is on; log a warning line at startup when it's enabled ("pprof enabled on 127.0.0.1:6060 — do not expose this port").
- No change to any Dockerfile or Helm chart default — the flag is unset (false) everywhere it's currently deployed.

**Tests:** bind to `127.0.0.1:0` in the test (capture the OS-assigned port via `Listener.Addr()`), enable the flag, `http.Get` `/debug/pprof/` and expect `200`; with the flag off, assert nothing is listening on the configured port (dial fails with connection refused).

---

## Task 4 — Capture resource usage alongside benchmark runs

**Goal:** today a bench run captures only client-observed RTT — nothing about the actual CPU/memory/Redis cost incurred. Add optional, best-effort resource snapshots before and after a run.

**Files:** `cli/cmd/resources.go` (new), `cli/cmd/resources_test.go` (new), `cli/cmd/bench_run.go` (wire in).

**Design:**
```go
package cmd

import (
	"context"
	"os/exec"
)

type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

type ResourceSnapshot struct {
	DockerStats string `json:"docker_stats,omitempty"`
	RedisInfo   string `json:"redis_info,omitempty"`
}

// captureResources is best-effort: a missing docker/redis-cli binary, or a
// non-Docker deployment target, must never fail the benchmark run itself —
// only the snapshot fields are omitted.
func captureResources(ctx context.Context, run commandRunner, containers []string, redisAddr string) ResourceSnapshot {
	var snap ResourceSnapshot
	if len(containers) > 0 {
		args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, containers...)
		if out, err := run(ctx, "docker", args...); err == nil {
			snap.DockerStats = string(out)
		}
	}
	if redisAddr != "" {
		if out, err := run(ctx, "redis-cli", "-u", redisAddr, "INFO"); err == nil {
			snap.RedisInfo = string(out)
		}
	}
	return snap
}
```
- New flags on `bench run`: `--capture-resources` (bool), `--docker-containers` (comma-separated, default empty), `--redis-addr` (default empty, e.g. `redis://localhost:6379`). When `--capture-resources` is unset, skip entirely (zero behavior change for existing callers/tests).
- Capture once immediately before the run starts and once immediately after it ends; attach both `ResourceSnapshot`s to `benchResult` as `ResourceBefore`/`ResourceAfter` (both `json:",omitempty"` so existing JSON-mode assertions checking for specific fields are unaffected); print a short human-readable section only when non-empty.

**Tests:** inject a fake `commandRunner` returning canned output for `"docker"`/`"redis-cli"` and `exec.ErrNotFound` for anything else — assert graceful omission (empty snapshot, no error surfaced) when the binary is "missing," and correct field population when it "succeeds."

---

## Task 5 — Nightly CI benchmark regression job

**Goal:** README already claims "regression-tracking over time" — nothing automates that today. Add a scheduled job comparing a fresh run against a committed baseline, without ever silently rewriting that baseline.

**Files:** `.github/workflows/benchmark.yml` (new), `deploy/bench-baseline.json` (new, committed).

**Design:**
- Trigger: `schedule: cron: '0 3 * * *'` + `workflow_dispatch` (manual trigger for testing/re-verification).
- Steps: checkout → setup-go (pinned SHA, matching `ci.yml`'s convention) → `bash deploy/generate-demo-certs.sh` → bring up the stack with the **loosened** bench overlay (`docker-compose.yml -f docker-compose.bench.yml`, matching the README's existing methodology — a regression signal needs a stable, non-shedding baseline, not one dominated by rejection noise) → build `ratecapctl` → run both tier 1 and tier 2 benchmarks with `--json`, capturing output to files.
- Comparison step (`bash` + `jq`, matching `ci.yml`'s existing `bc`-based coverage-floor pattern — no new language dependency): parse P50/P99 from both the fresh run and `deploy/bench-baseline.json`; compute percentage delta; if P99 regresses by more than a threshold (e.g. 20%), print a clear failure message and `exit 1`. A scheduled workflow that fails surfaces in the Actions tab and (by GitHub's default behavior) emails the repo's watchers — that is the "flagging" this item asks for.
- `deploy/bench-baseline.json` is a **committed, manually-updated** file (`{"tier1": {"p50_ms": ..., "p99_ms": ...}, "tier2": {...}}`) seeded from Task 1/existing README numbers. The job must never write back to it automatically — baseline updates are a deliberate, reviewed change (a maintainer re-runs and commits a new baseline in its own PR), never an automatic side effect of a green nightly run. State this explicitly as a comment in the workflow file.
- Tear down the stack in an `if: always()` step, matching `e2e-smoke`'s existing pattern.

---

## Task 6 — Add `golangci-lint` to CI

**Goal:** `gofmt` is the only lint gate today. Add `golangci-lint`, and actually fix whatever it finds in the existing codebase rather than shipping a gate that's red on day one.

**Files:** `.golangci.yml` (new, repo root), `.github/workflows/ci.yml` (new job), plus fixes to any real findings.

**Design:**
- `.golangci.yml`: enable `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused` (a conservative, high-signal set — not an exhaustive style-linter pile-on). Exclude `proto/` entirely (100% generated code, same reasoning `ci.yml` already documents for its 0% coverage floor) and exclude `*_test.go` from `errcheck` only where a test intentionally ignores an error for brevity (do not blanket-exclude test files from every linter).
- New CI job `golangci-lint`, matrixed over the same 6 Go module directories as the existing `build-and-test` job, using the official `golangci-lint-action` **pinned to a full commit SHA** per this repo's established convention (look up the current release tag's SHA — do not use a floating `@v6` reference).
- **Run it for real against the current tree before wiring it into CI**, and fix genuine findings directly (e.g. an unchecked `defer resp.Body.Close()` return, a genuinely dead branch). Where a finding is a deliberate, safe pattern (e.g. a `defer` Close on a read-only, already-erred-out resource), suppress it narrowly with `//nolint:errcheck // reason` at the exact line — never a blanket per-file or per-package disable.
- Note: `bench_run.go`'s previously-discarded-return-value bug (the exact example this roadmap item names) was already fixed in Phase 0 item 3 — confirm `errcheck` reports zero findings there now rather than re-fixing it.

---

## Task 7 — Isolate `testcontainers-go` into a separate module

**Goal:** `services/core/go.mod` directly requires `testcontainers-go` + its `toxiproxy` module, pulling in a ~40-package transitive tree (`moby/*`, `containerd/*`, `gopsutil`, etc.) into the **production** module purely to support Docker-based Redis integration tests. A build tag alone does not fix this — Go's `go.mod`/`go.sum` track every import reachable under *any* build-tag combination in a module, regardless of default tags. The actual fix is a **separate nested module**.

**Files:** new `services/core/integrationtests/` module; move the testcontainers-based test file(s) there; `services/core/go.mod` (drop the now-unused requires via `go mod tidy`); `go.work`; `.github/workflows/ci.yml`.

**Steps:**
1. `grep -rl testcontainers services/core --include='*_test.go'` to find every file using it (expect the Redis integration test(s) referenced in `CLAUDE.md`'s build&test section — "`services/core`'s Redis integration tests need Docker running locally").
2. Create `services/core/integrationtests/go.mod` (`module github.com/ratecap/core/integrationtests`, same Go version as the rest of the repo) with a `replace github.com/ratecap/core => ../` so it can still import the public packages under test (`store`, `limiter`, etc.) from the working tree rather than a published version.
3. Move the identified test file(s) into `services/core/integrationtests/`, adjusting package name/imports as needed to reference `github.com/ratecap/core/...` instead of relative-package-local names.
4. In `services/core/`, run `go mod tidy` — confirm `testcontainers-go` and `testcontainers-go/modules/toxiproxy` are dropped from `services/core/go.mod`/`go.sum` entirely (verify by `grep testcontainers services/core/go.mod` returning nothing).
5. In `services/core/integrationtests/`, run `go mod tidy` — confirm it now requires `testcontainers-go` there instead.
6. Add `use ./services/core/integrationtests` to the repo-root `go.work`.
7. Update `.github/workflows/ci.yml`: add a dedicated step (or job) running `cd services/core/integrationtests && go test ./... -race` — Docker is already available on `ubuntu-latest` runners (the existing `docker-build` job proves this), so no new CI infrastructure is needed, just the extra step. This step does **not** need a coverage floor (thin integration glue, not unit-level logic) — run it without `-coverprofile`.
8. **Verify empirically, not just by inspection:** run `cd services/core && go build ./... && go test ./... -race -coverprofile=coverage.out` and confirm the coverage percentage still clears the existing 50% floor now that the moved test file no longer contributes to it — if it drops below 50%, that's a real signal the moved test was carrying coverage weight that needs a replacement unit test in the main module, not just a note to ignore.

---

## Task 8 — Generate the Helm chart's inline `config.yaml` from `deploy/ratecap.yaml`; document the missing `concurrencySigningKey` secret

**Goal:** `deploy/helm/ratecap/values.yaml`'s embedded `config.yaml` block is a hand-copied duplicate of `deploy/ratecap.yaml` today — a second, driftable source of truth. And installs fail with an undocumented `CreateContainerConfigError` when `concurrencySigningKey` isn't set, because the Helm README never mentions it (confirmed: zero matches for `concurrencySigningKey` in `deploy/helm/ratecap/README.md` today, even though `values.yaml` itself documents it well via comments).

**Files:** `deploy/helm/ratecap/generate-config.sh` (new), `deploy/helm/ratecap/values.yaml` (add sentinel markers), `deploy/helm/ratecap/README.md` (new sections), `.github/workflows/ci.yml` (`helm-lint` job, add drift check).

**Steps:**
1. Wrap the existing `config:`/`yaml: |` block in `values.yaml` with sentinel comment markers:
   ```yaml
   # BEGIN GENERATED CONFIG (see generate-config.sh) — do not hand-edit between these markers
   config:
     yaml: |
       <existing content, unchanged>
   # END GENERATED CONFIG
   ```
2. Write `generate-config.sh` (matching the existing `bash`, `set -euo pipefail` style of `deploy/generate-demo-certs.sh`): reads `../../ratecap.yaml` (i.e. `deploy/ratecap.yaml`), re-indents it under `  yaml: |\n` with the correct nesting, and replaces everything between the two sentinel markers in `values.yaml` in place using `awk` (no new `python3`/`yq` dependency — portable, matches the repo's existing shell-only tooling convention).
3. Run it for real: confirm the freshly generated block is byte-identical to what's already committed (it should be — the current inline copy already mirrors `deploy/ratecap.yaml` exactly, confirmed by direct comparison during planning), which is itself the correctness proof of the script.
4. Add a **"Regenerating the embedded config"** section to `deploy/helm/ratecap/README.md`: run the script after any edit to `deploy/ratecap.yaml`, commit the resulting `values.yaml` diff.
5. Add a **secrets** section (or extend the existing one, matching however `sharedSecret`/`adminSecret` are already documented there) explicitly covering `concurrencySigningKey`: what it's for (signs/verifies Tier 2 concurrency tokens, core-only), the exact `kubectl create secret generic` command from `values.yaml`'s own comment, and a note that omitting it causes `CreateContainerConfigError` on the core pod.
6. In the `helm-lint` CI job, add a drift-check step: run `generate-config.sh`, then `git diff --exit-code deploy/helm/ratecap/values.yaml` — fails the same way `gofmt -l .` does if `deploy/ratecap.yaml` and the chart's embedded copy ever diverge again.

---

## Task 9 — CLI entrypoint tests, `--version`, `--qps`/pacing

**Goal:** only child commands (`bench run`, `config validate`, `admin`, `tls`) have tests today — `cli/main.go`, `cli/cmd/root.go`, and `cli/cmd/bench.go` (the parent `bench` command itself) have none. Also close the two real remaining gaps this roadmap item names: `--qps`/pacing and `--version` (the accept/reject/error counters it also names are **already done** — Phase 0 item 3 — do not redo them).

**Files:** `cli/cmd/root.go` (add `Version`), `cli/cmd/root_test.go` (new), `cli/main.go` (extract testable `Run`), `cli/cmd/bench_run.go` (add `--qps`), `cli/cmd/bench_run_test.go` (extend).

**Design:**
- `cli/cmd/root.go`: add a package-level `var Version = "dev"` and set `root.Version = Version` in `NewRootCmd()` — Cobra auto-registers `--version`/`-v` once `.Version` is non-empty.
- Inject the real value at build time via `-ldflags "-X github.com/ratecap/cli/cmd.Version=$(cat VERSION)"` (this repo-root `VERSION` file is the Phase-0-established single source of truth) — update the `go build` command in `README.md`'s benchmark-reproduction steps to include this flag, so the published bench numbers and the version string stay linked. `go:embed` cannot be used here since `cli/` is its own Go module (separate `go.mod`) and cannot embed a path outside its own module root.
- `cli/main.go`: extract the two lines of real logic into `func Run(args []string, stdout, stderr io.Writer) int` in `cmd` (or keep `main.go` itself minimal but add a thin, directly-testable wrapper) so `main_test.go`/`root_test.go` can exercise the actual exit-code mapping without needing `os.Exit` in the test process.
- `cli/cmd/bench_run.go`: add `--qps` (float64, default `0` = unlimited, unchanged behavior). When `> 0`, construct one shared `golang.org/x/time/rate.Limiter` (`rate.NewLimiter(rate.Limit(qps), 1)`) and have every worker call `limiter.Wait(ctx)` immediately before issuing each request — pre-approved dependency per Global Constraint 4.

**New tests:**
- `root_test.go`: `TestRootCmd_VersionFlagPrintsVersion` (build with `Version` overridden to a known test value, execute `--version`, assert it's in the output); `TestRootCmd_NoArgsPrintsHelp`; `TestRootCmd_UnknownSubcommandErrors`.
- `bench_run_test.go`: `TestBenchRun_QPSFlagPacesRequests` — run with a small `--qps` and few `--requests` against a fast fake server, assert wall-clock elapsed is at or above the pacing-implied minimum (generous tolerance for CI timing jitter), distinguishing it from the current unpaced behavior which would finish near-instantly.

---

## Task 10 — Pin Docker base images to digests

**Goal:** close the last dependency-drift gap. GitHub Actions are already SHA-pinned (verified — do not touch `.github/workflows/*.yml` action references in this task). Only the three Dockerfiles' `FROM` lines are still tag-only.

**Files:** `services/core/Dockerfile`, `services/sidecar/Dockerfile`, `deploy/sampleapp/Dockerfile`.

**Steps:**
1. For each of `golang:1.27-alpine` and `alpine:3.24`, resolve the current real digest (`docker pull <image> && docker inspect --format='{{index .RepoDigests 0}}' <image>`, or `docker manifest inspect`) — do not fabricate or guess a digest string.
2. Pin each `FROM` line to `image:tag@sha256:<real-digest>`, keeping the human-readable tag in the reference (Docker allows both together) so `docker build` still works and the line stays self-documenting, e.g.:
   ```
   FROM golang:1.27-alpine@sha256:<real digest> AS build
   ```
3. `.github/dependabot.yml` already has `docker` ecosystem entries for these three Dockerfiles (confirmed during planning) — Dependabot supports digest-pinned updates natively, so no dependabot.yml change is needed here.
4. Verify empirically: `docker build -f services/core/Dockerfile -t ratecap-core:pin-test .` (and the same for sidecar/sampleapp) actually succeeds with the pinned digest, matching what the existing `docker-build` CI job already does per-image.
