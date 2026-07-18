# RateCap v2 Phase 4b: Published Benchmarks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a benchmark-specific deploy config plus a new `## Benchmarks` section to `README.md`, publishing real P50/P99/P99.9 latency numbers for both RateCap's Tier 1 (`Allow()`) and Tier 2 (`Acquire()`/`Release()`) paths — closing the gap the v1 design spec's own Testing Strategy section promised but never delivered.

**Architecture:** A new `deploy/ratecap-bench.yaml` config (generously-sized limits, no artificial rejections) plus a `deploy/docker-compose.bench.yml` override file (Docker Compose's native multi-file layering — swaps the `core` service's mounted config and raises the `sidecar` service's in-flight cap) let the benchmark run against a live stack without touching the default demo's tiny curl-walkthrough limits. `README.md` gets one new section with the real, already-collected benchmark numbers, framed honestly as a single-machine, same-host, dated snapshot — not a production capacity-planning number.

**Tech Stack:** YAML (deploy config), Docker Compose multi-file override, `ratecapctl bench run` (existing CLI, no code changes), Markdown (`README.md`).

## Global Constraints

- No code changes anywhere — `services/core`, `services/sidecar`, `packages/sdks/go`, `proto/`, and `cli/` are all untouched. Only `deploy/` and `README.md` change.
- No `Co-Authored-By` trailers in any commit.
- `deploy/docker-compose.yml` and `deploy/ratecap.yaml` are left byte-for-byte unchanged — the default demo experience (curl walkthroughs, visible 429s/503s) must be unaffected.
- `deploy/certs/` is already gitignored (confirmed: `.gitignore` contains `/deploy/certs/`) — never instruct committing generated certs.
- The published benchmark numbers are the real, already-collected results from an actual run against a live stack in this exact worktree (see Task 2) — write them into `README.md` verbatim, not as placeholders to fill in later.

---

## Task 1: Benchmark-specific deploy config, verified working end-to-end

**Files:**
- Create: `deploy/ratecap-bench.yaml`
- Create: `deploy/docker-compose.bench.yml`

**Interfaces:**
- Produces: a `docker compose -f docker-compose.yml -f docker-compose.bench.yml up --build -d` invocation (run from `deploy/`) that brings up a stack with generously-sized limits on every tier, suitable for Task 2's benchmark runs to read real latency numbers from without artificial-rejection noise.

- [ ] **Step 1: Create `deploy/ratecap-bench.yaml`**

```yaml
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100000
    default_burst: 100000
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 1000
    max_request_duration_ms: 30000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 1000
    reserved_critical_pct: 40
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
```

- [ ] **Step 2: Create `deploy/docker-compose.bench.yml`**

```yaml
services:
  core:
    volumes:
      - ./ratecap-bench.yaml:/etc/ratecap/ratecap.yaml
      - ./certs:/etc/ratecap/certs:ro

  sidecar:
    environment:
      RATECAP_MAX_INFLIGHT_REQUESTS: "1000"
```

- [ ] **Step 3: Generate demo certs (required — the compose stack has mTLS configured on both `core` and `sidecar`)**

Run: `cd deploy && bash generate-demo-certs.sh`
Expected output (exact):
```
-----
-----
Certificate request self-signature ok
subject=CN=ratecap-core
-----
Certificate request self-signature ok
subject=CN=ratecap-sidecar
Demo certs generated in deploy/certs/ (gitignored, 1-day validity, do not use in production).
```
(If certs already exist from a prior run in this worktree, the script will overwrite them — this is expected and fine, since certs are 1-day-validity and gitignored.)

- [ ] **Step 4: Bring up the benchmark-configured stack**

Run: `cd deploy && docker compose -f docker-compose.yml -f docker-compose.bench.yml up --build -d`
Expected: build succeeds, and the final lines report all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) `Created` then `Started` with no errors.

- [ ] **Step 5: Confirm mTLS is active on both services**

Run: `sleep 3 && docker compose -f docker-compose.yml -f docker-compose.bench.yml logs core sidecar` (from `deploy/`)
Expected output includes these exact lines (container name prefixes may vary slightly, e.g. `deploy-core-1` vs `core-1`, but the log text itself must match):
```
sidecar-1  | .../.../.. ..:..:.. ratecap-sidecar: mTLS enabled
sidecar-1  | .../.../.. ..:..:.. ratecap-sidecar listening on :8080, forwarding to core at core:9090
core-1     | .../.../.. ..:..:.. ratecap-core: mTLS enabled
core-1     | .../.../.. ..:..:.. ratecap-core listening on :9090
```
If "mTLS enabled" is missing from either service's logs, stop and investigate before proceeding — Task 2's benchmark numbers depend on this being the real, intended stack configuration, not an accidentally-plaintext fallback.

- [ ] **Step 6: Confirm the raised limits are genuinely in effect (no artificial rejections under light load)**

Build `ratecapctl` and run a small sanity check:
```bash
cd cli && go build -o /tmp/ratecapctl .
/tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --concurrency 5 --requests 50 --key-prefix sanity-check
```
Expected: `Total requests: 50` with no errors printed, and a P50/P99/P99.9 that are all small (low tens of milliseconds or less) — if the raised config in Step 1 didn't actually take effect (e.g. the volume mount is wrong), this would instead show a P99.9 that's dramatically inflated or a total-requests count under 50 (dropped/failed requests), which would mean investigate before proceeding to Task 2's real benchmark runs.

- [ ] **Step 7: Confirm zero rejections via the metrics endpoint**

Run: `curl -s http://localhost:8080/metrics | grep -E "ratecap_decisions_total|ratecap_shadow"`
Expected: every line contains `action="allow"` — e.g. `ratecap_decisions_total{action="allow",tier="rate_limiter"} 50` (the exact tier label and count reflect this step's 50-request sanity check only; there should be no `reject_429` or `reject_503` line present at all).

- [ ] **Step 8: Tear down**

Run: `cd deploy && docker compose -f docker-compose.yml -f docker-compose.bench.yml down`
Expected: all 4 containers and the network report `Stopping`/`Stopped`/`Removing`/`Removed`, no errors.

- [ ] **Step 9: Confirm the default (non-benchmark) demo stack is unaffected**

Run: `cd deploy && git diff --stat -- docker-compose.yml ratecap.yaml`
Expected: no output (zero changes to either file — this task only adds 2 new files, `docker-compose.bench.yml` and `ratecap-bench.yaml`).

- [ ] **Step 10: Commit**

```bash
git add deploy/ratecap-bench.yaml deploy/docker-compose.bench.yml
git commit -m "feat(deploy): add benchmark-specific config and compose override"
```

---

## Task 2: Publish real benchmark numbers in a new `## Benchmarks` section

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: `deploy/ratecap-bench.yaml` and `deploy/docker-compose.bench.yml` (Task 1) — the reproduction commands in this section's write-up reference these exact file paths and the exact `docker compose -f ... -f ...` invocation Task 1 already verified working.

This task's benchmark numbers are the real, already-collected results of an actual run performed against a live stack in this exact worktree during planning — insert them verbatim; do not re-run the benchmark as part of this task unless Task 1's Step 6 sanity check in this task's own execution reveals a genuine discrepancy (e.g. the stack no longer builds). If everything in Task 1 passed as specified, these numbers are final.

- [ ] **Step 1: Insert the new `## Benchmarks` section into `README.md`**

Find this exact existing text in `README.md` (the end of the Comparison section's Sources list, immediately before Design docs):

```markdown
15. Sentinel's System Adaptive Protection sheds load based on local system metrics (`load1`, CPU usage, and locally-tracked QPS/response-time), computed entirely within the instance applying the rule — no remote call. [System adaptive protection](https://sentinelguard.io/en-us/docs/system-adaptive-protection.html).

## Design docs
```

Replace it with (inserting the new section in between, leaving the existing Sources list entry and the Design docs heading unchanged):

```markdown
15. Sentinel's System Adaptive Protection sheds load based on local system metrics (`load1`, CPU usage, and locally-tracked QPS/response-time), computed entirely within the instance applying the rule — no remote call. [System adaptive protection](https://sentinelguard.io/en-us/docs/system-adaptive-protection.html).

## Benchmarks

The numbers below measure two request paths through the demo stack: RateCap's Tier 1 (`Allow()`, a single token-bucket check) and Tier 2 (`Acquire()` followed by `Ticket.Release()`, a concurrency-limit reservation plus its later release — this necessarily also passes through Tier 1 and Tier 3 ahead of it in the pipeline). These are a one-time, dated snapshot from a single machine running the full `sidecar` + `core` + Redis stack over Docker Compose on one host — useful for relative/directional comparison and regression-tracking over time (e.g. "did a later change make Tier 2's overhead meaningfully worse"), not a production capacity-planning number. A real deployment has network hops between services, different hardware, and different traffic shapes that this same-host setup doesn't capture.

**Environment:** 2026-07-18, Apple M4 (Darwin arm64), Docker Engine 29.5.3, Go 1.26.2.

**Reproduce it yourself:**

```bash
cd deploy
bash generate-demo-certs.sh
docker compose -f docker-compose.yml -f docker-compose.bench.yml up --build -d
cd ../cli && go build -o /tmp/ratecapctl .

# Tier 1 — Allow()
/tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --concurrency 50 --requests 20000 --key-prefix bench-tier1

# Tier 2 — Acquire()/Release()
/tmp/ratecapctl bench run --sidecar-addr http://localhost:8080 --concurrency 50 --requests 20000 --key-prefix bench-tier2 --acquire

cd ../deploy && docker compose -f docker-compose.yml -f docker-compose.bench.yml down
```

**Tier 1 — `Allow()`** (concurrency 50, 20,000 requests):

| Total requests | Elapsed | Throughput | P50 | P99 | P99.9 |
| --- | --- | --- | --- | --- | --- |
| 20,000 | 1725ms | 11,591.4 req/s | 3.88ms | 11.65ms | 28.02ms |

**Tier 2 — `Acquire()`/`Release()`** (concurrency 50, 20,000 requests):

| Total requests | Elapsed | Throughput | P50 | P99 | P99.9 |
| --- | --- | --- | --- | --- | --- |
| 20,000 | 5395ms | 3,706.7 req/s | 12.96ms | 25.67ms | 34.25ms |

Tier 2's higher latency and lower throughput reflect its extra round trip: `Acquire()` reserves a slot (a Tier 2 check plus a Tier 3 check, both real Redis-backed operations) and the benchmark client then calls `Release()` to free it — genuinely more work per request than Tier 1's single token-bucket check.

## Design docs
```

- [ ] **Step 2: Verify the section landed in the right place**

Run: `grep -n "^## " README.md`
Expected output (exact):
```
7:## Status
11:## Architecture
19:## Quick start
30:## Project layout
38:## Comparison
68:## Benchmarks
NN:## Design docs
```
(the exact line number `NN` for `## Design docs` will be higher than its previous value of `68`, since the new section adds lines above it — confirm `## Benchmarks` appears immediately after the last Sources entry and immediately before `## Design docs`, with no other section in between.)

- [ ] **Step 3: Confirm both markdown tables render as valid tables**

Run: `python3 -c "
with open('README.md') as f:
    content = f.read()
start = content.index('## Benchmarks')
end = content.index('## Design docs')
section = content[start:end]
tables = [line for line in section.split(chr(10)) if line.strip().startswith('|')]
counts = set(len(line.split('|')) for line in tables)
print('distinct column-split counts:', counts)
print('total table rows found:', len(tables))
"`
Expected output:
```
distinct column-split counts: {8}
total table rows found: 6
```
(2 tables × 3 rows each [header + separator + 1 data row] = 6 rows; each row has 6 data columns + 2 empty leading/trailing fields from the split = 8.)

- [ ] **Step 4: Confirm no code files changed**

Run: `git diff --stat HEAD`
Expected output: exactly one file, `README.md`, with insertions only (no deletions — this step only adds a new section, it doesn't edit existing Comparison-section content).

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: publish Tier 1 and Tier 2 benchmark numbers"
```

---

## Self-Review Notes (completed during plan authoring)

**Spec coverage:** §1 (benchmark-specific config, not a demo-defaults change) → Task 1 Steps 1-2, 9. §2 (both Tier 1 and Tier 2 paths measured) → Task 2's two result tables. §3 (one-time dated snapshot, explicit environment/reproduction, directional-not-capacity-planning framing) → Task 2 Step 1's intro paragraph and Environment/Reproduce blocks. §4 (placement immediately after Comparison, before Design docs) → Task 2 Step 1's exact insertion point, confirmed against README.md's actual current content (post-Phase-4a). §5 (content shape: intro, date/environment/repro commands, two result tables with the tool's exact reported fields) → Task 2 Step 1's full section text.

**Placeholder scan:** No "TBD"/"TODO". Every benchmark number in Task 2 is the real, already-collected result from an actual run against a live stack in this worktree (Tier 1: 20000/1725ms/11591.4 req/s/3.88/11.65/28.02ms; Tier 2: 20000/5395ms/3706.7 req/s/12.96/25.67/34.25ms) — not placeholders. Task 1's verification steps show the real log/output text actually observed during that run (the "mTLS enabled" lines, the sanity-check behavior), not generic expected-output templates.

**Type/reference consistency:** Task 2's reproduction commands reference exactly the file paths and flags Task 1 creates and verifies (`docker-compose.bench.yml`, `ratecap-bench.yaml`, `--acquire`, `--concurrency 50 --requests 20000`) — no drift between what Task 1 builds and what Task 2's write-up tells a reader to run.
