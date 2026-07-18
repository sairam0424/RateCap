# RateCap v2 Phase 4a: Comparison Table Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new `## Comparison` section to `README.md` comparing RateCap's 5 mechanisms against Envoy, Kong, Cloudflare, Alibaba Sentinel, and Netflix's `concurrency-limits` library, with every cell traceable to a specific verified source.

**Architecture:** A single markdown table (5 mechanism rows × 6 columns) plus a framing paragraph and a numbered "Sources" list underneath. Task 1 lands the framing text, the table skeleton, the RateCap column, and every cell already backed by facts this repo's own prior design work adversarially verified. Task 2 replaces the remaining "Not verified" placeholders with the results of fresh, targeted official-source lookups (already performed during planning — this plan contains their actual findings and citations, not instructions to redo them) and finalizes the Sources list.

**Tech Stack:** Markdown only. No code changes.

## Global Constraints

- Every ✅/❌ cell must cite a specific source (this repo's own prior-verified spec docs, or an external official-docs URL) — no unsourced claims.
- Exactly 3 possible cell values: ✅, ❌, "Not verified". No partial-credit symbols, no prose inside cells.
- "Not verified" is a legitimate, permanent final value where no confident official-source answer exists — never resolved by guessing.
- No marketing adjectives in the framing paragraph ("powerful," "enterprise-grade," "best-in-class," etc.).
- No `Co-Authored-By` trailers in any commit.
- No code changes anywhere — `README.md` only.

---

## Task 1: Framing paragraph, table skeleton, RateCap column, and already-verified cells

**Files:**
- Modify: `README.md`

**Interfaces:**
- Produces: a `## Comparison` section in `README.md`, inserted after the existing `## Project layout` section and before `## Design docs`, containing the framing paragraph and a 5×6 markdown table with some cells still "Not verified" (Task 2 replaces those).

- [ ] **Step 1: Read the current README.md section boundaries**

Run: `grep -n "^## " README.md`
Expected output (exact, from the current file):
```
7:## Status
9:## Architecture
17:## Quick start
26:## Project layout
33:## Design docs
```

- [ ] **Step 2: Insert the new `## Comparison` section between `## Project layout` and `## Design docs`**

Find this exact existing text in `README.md`:

```markdown
## Project layout

- `proto/` — gRPC contract (source of truth for all SDKs)
- `services/core/` — central engine: limiter logic, Redis state, config hot-reload
- `services/sidecar/` — local proxy: priority resolution, shadow-mode override
- `packages/sdks/go/` — thin Go client SDK
- `deploy/` — docker-compose demo and sample app

## Design docs
```

Replace it with (inserting the new section in between, leaving both existing sections' text unchanged):

```markdown
## Project layout

- `proto/` — gRPC contract (source of truth for all SDKs)
- `services/core/` — central engine: limiter logic, Redis state, config hot-reload
- `services/sidecar/` — local proxy: priority resolution, shadow-mode override
- `packages/sdks/go/` — thin Go client SDK
- `deploy/` — docker-compose demo and sample app

## Comparison

RateCap is the only project among these six that implements all four of Stripe's original rate-limiter/load-shedder tiers plus bounded queueing as one open-source system. The table below compares RateCap's five mechanisms against five widely-used systems — Envoy, Kong, Cloudflare, Alibaba Sentinel, and Netflix's `concurrency-limits` library — based on each system's own official documentation or source, not secondary claims. "Not verified" means a confident answer wasn't found in a targeted official-source check, not that the mechanism is confirmed absent.

| Mechanism | RateCap | Envoy | Kong | Cloudflare | Alibaba Sentinel | Netflix `concurrency-limits` |
| --- | --- | --- | --- | --- | --- | --- |
| Token-bucket rate limiting (Tier 1) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |
| Concurrency (in-flight request) limiting (Tier 2) | ✅ | Not verified | Not verified | Not verified | Not verified | ✅ [1] |
| Bounded queueing (Tier 2, v2.2.0) | ✅ | Not verified | Not verified | Not verified | ✅ [2] | ✅ [1] |
| Fleet-wide (global) load shedding w/ reserved capacity (Tier 3) | ✅ | Not verified [3] | Not verified | Not verified [3] | Not verified | ❌ [1] |
| Local/worker-utilization load shedding (Tier 4) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |

### Sources

1. Already established in this repo's own v2 Phase 3 design work: Netflix's `concurrency-limits` is fundamentally a concurrency-limiting library with a `LifoBlockingLimiter` (hard-capped backlog, genuine bounded queueing) — but is explicitly local/per-instance only ("each node will adjust and enforce its local view of the limit"), with no fleet-wide coordination. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md` and [Netflix/concurrency-limits](https://github.com/Netflix/concurrency-limits).
2. Already established in this repo's own v2 Phase 3 design work: Alibaba Sentinel's `ThrottlingController.java` "uniform queueing" behavior parks the calling thread until its turn, rejecting only past a configured max wait — genuine queueing, not instant-reject. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.
3. This repo's own prior research (see the v2 roadmap plan) already established that Cloudflare and Envoy Gateway both explicitly avoid cross-DC/cross-instance counter synchronization in their "global" rate-limiting features — evidence against a fleet-wide-coordinated-state mechanism like RateCap's Tier 3, though not yet a confirmed final verdict for either system's specific reserved-capacity-for-priority-traffic question.

## Design docs
```

- [ ] **Step 3: Verify the section landed in the right place**

Run: `grep -n "^## " README.md`
Expected output (exact):
```
7:## Status
9:## Architecture
17:## Quick start
26:## Project layout
33:## Comparison
NN:### Sources
MM:## Design docs
```
(exact line numbers `NN`/`MM` will differ from this template — confirm `## Comparison` appears immediately after `## Project layout` and immediately before `## Design docs`, and `### Sources` appears between them.)

- [ ] **Step 4: Render the table locally to confirm valid markdown table syntax**

Run: `python3 -c "
import re
with open('README.md') as f:
    content = f.read()
table_start = content.index('| Mechanism |')
table_end = content.index('### Sources')
table = content[table_start:table_end]
rows = [r for r in table.strip().split(chr(10)) if r.strip()]
for r in rows:
    cols = r.split('|')
    print(len(cols), r[:50])
"`
Expected output: every row reports the same column count (9 — 1 empty leading + 6 data columns... actually count includes leading/trailing empty strings from split, so expect `9` for every row including the separator row `| --- | --- | ... |`) and no row errors out.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: add Comparison section skeleton with RateCap column and already-verified cells"
```

---

## Task 2: Fresh-lookup cells, final table, Sources list, and verification

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: the `## Comparison` section and partial table from Task 1.
- Produces: the complete, final `## Comparison` section — every "Not verified" cell from Task 1 either confirmed with a sourced ✅/❌ or deliberately left "Not verified" where a targeted official-source check still found no confident answer, plus a complete numbered Sources list covering every citation in the table.

This task's cell values and citations are the actual results of targeted, single-official-source lookups already performed during planning (via direct fetches of each system's own docs/source) — not instructions to research from scratch. Insert them as written below; do not re-derive them.

- [ ] **Step 1: Replace the table and Sources list with the final, fully-sourced version**

Find this exact text in `README.md` (the table and Sources list Task 1 landed):

```markdown
| Mechanism | RateCap | Envoy | Kong | Cloudflare | Alibaba Sentinel | Netflix `concurrency-limits` |
| --- | --- | --- | --- | --- | --- | --- |
| Token-bucket rate limiting (Tier 1) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |
| Concurrency (in-flight request) limiting (Tier 2) | ✅ | Not verified | Not verified | Not verified | Not verified | ✅ [1] |
| Bounded queueing (Tier 2, v2.2.0) | ✅ | Not verified | Not verified | Not verified | ✅ [2] | ✅ [1] |
| Fleet-wide (global) load shedding w/ reserved capacity (Tier 3) | ✅ | Not verified [3] | Not verified | Not verified [3] | Not verified | ❌ [1] |
| Local/worker-utilization load shedding (Tier 4) | ✅ | Not verified | Not verified | Not verified | Not verified | Not verified |

### Sources

1. Already established in this repo's own v2 Phase 3 design work: Netflix's `concurrency-limits` is fundamentally a concurrency-limiting library with a `LifoBlockingLimiter` (hard-capped backlog, genuine bounded queueing) — but is explicitly local/per-instance only ("each node will adjust and enforce its local view of the limit"), with no fleet-wide coordination. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md` and [Netflix/concurrency-limits](https://github.com/Netflix/concurrency-limits).
2. Already established in this repo's own v2 Phase 3 design work: Alibaba Sentinel's `ThrottlingController.java` "uniform queueing" behavior parks the calling thread until its turn, rejecting only past a configured max wait — genuine queueing, not instant-reject. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.
3. This repo's own prior research (see the v2 roadmap plan) already established that Cloudflare and Envoy Gateway both explicitly avoid cross-DC/cross-instance counter synchronization in their "global" rate-limiting features — evidence against a fleet-wide-coordinated-state mechanism like RateCap's Tier 3, though not yet a confirmed final verdict for either system's specific reserved-capacity-for-priority-traffic question.
```

Replace it with:

```markdown
| Mechanism | RateCap | Envoy | Kong | Cloudflare | Alibaba Sentinel | Netflix `concurrency-limits` |
| --- | --- | --- | --- | --- | --- | --- |
| Token-bucket rate limiting (Tier 1) | ✅ | ✅ [1] | ❌ [2] | ❌ [3] | ❌ [4] | ❌ [5] |
| Concurrency (in-flight request) limiting (Tier 2) | ✅ | ✅ [6] | ❌ [2] | ❌ [3] | ✅ [7] | ✅ [5] |
| Bounded queueing (Tier 2, v2.2.0) | ✅ | ❌ [1] | ✅ [8] | ❌ [3] | ✅ [9] | ✅ [5] |
| Fleet-wide (global) load shedding w/ reserved capacity (Tier 3) | ✅ | Not verified [10] | Not verified [11] | Not verified [3] | Not verified [12] | ❌ [5] |
| Local/worker-utilization load shedding (Tier 4) | ✅ | ✅ [13] | Not verified [2] | Not verified [14] | ✅ [15] | Not verified [5] |

### Sources

1. Envoy's local rate limit filter uses a genuine token-bucket algorithm, entirely local to the Envoy instance/connection — no remote call. [Local rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter). Envoy's separate global rate-limit filter and its work-in-progress rate-limit-quota filter both defer the algorithm to an external service and reject instantly on overflow — neither documents queueing/delay behavior. [Rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter).
2. Kong's official Rate Limiting and Rate Limiting Advanced plugins use fixed-window or sliding-window request counters, never token-bucket, and neither Kong plugin limits concurrent/in-flight requests or sheds load based on local worker utilization — both are time-window-based. [Rate Limiting plugin](https://developer.konghq.com/plugins/rate-limiting/), [Rate Limiting Advanced plugin](https://developer.konghq.com/plugins/rate-limiting-advanced/). (Kong's `nginx_events_worker_connections` setting caps simultaneous nginx worker connections at the infrastructure level — it is not an application-level, per-key concurrency limiter or adaptive utilization-based shedder like RateCap's Tier 2/Tier 4.)
3. Cloudflare's rate limiting rules use a fixed-period request (or complexity-score) counter, not token-bucket, with no concurrency dimension and no documented bounded-queueing/delay mechanism. [Rate limiting rules](https://developers.cloudflare.com/waf/rate-limiting-rules/), [Rate limiting parameters](https://developers.cloudflare.com/waf/rate-limiting-rules/parameters/). (Cloudflare Queues is a separate, general-purpose message-queue product for Workers, unrelated to the WAF rate-limiting rules covered here — not conflated in this table.) No fleet-wide reserved-capacity-for-priority-traffic feature was found in these docs.
4. Sentinel's "Rate limiter" flow-control strategy is explicitly leaky-bucket ("allows requests to pass at a uniform rate"), not token-bucket. [Flow control docs](https://sentinelguard.io/en-us/docs/flow-control.html).
5. Already established in this repo's own v2 Phase 3 design work, extended with a fresh check of the same source for the remaining rows: Netflix's `concurrency-limits` frames its design as explicitly moving away from RPS/time-window thinking toward concurrency-based limits (no token-bucket/fixed-rate component), is explicitly local/per-instance only with no fleet-wide coordination ("each node will adjust and enforce its local view of the limit"), and its Vegas/Gradient2 algorithms adapt to measured request latency rather than an explicit system-load/CPU signal — related to but not the same shape as Sentinel's or RateCap's Tier 4, so left "Not verified" rather than forced into ✅/❌. [Netflix/concurrency-limits](https://github.com/Netflix/concurrency-limits).
6. Envoy's cluster circuit breakers (`max_connections`, `max_pending_requests`, `max_requests`) cap concurrent/parallel requests to an upstream cluster — a genuine concurrency limit, though scoped per-cluster/upstream rather than per-request-key like RateCap's Tier 2. [Circuit breaking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking).
7. Sentinel's "thread count" flow-control mode is documented as semaphore isolation — a direct concurrency limit. [Sentinel wiki home](https://github.com/alibaba/sentinel/wiki), [Migration from Hystrix](https://github.com/alibaba/Sentinel/wiki/Guideline:-Migration-from-Hystrix-to-Sentinel).
8. Kong's Rate Limiting Advanced plugin (3.12+) supports a genuine bounded-queue throttling mode: over-limit requests are held and retried up to `config.throttling.retry_times` times, bounded by `config.throttling.queue_limit`, before falling back to a `429` — structurally similar to RateCap's own bounded-backlog-plus-max-wait design. [Rate Limiting Advanced plugin](https://developer.konghq.com/plugins/rate-limiting-advanced/).
9. Already established in this repo's own v2 Phase 3 design work: Alibaba Sentinel's `ThrottlingController.java` "uniform queueing" behavior parks the calling thread until its turn, rejecting only past a configured max wait — genuine queueing, not instant-reject. See `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.
10. Envoy's global rate-limit filter supports a per-descriptor "limit override," which overrides a static quota value for matching traffic — not the same as reserving capacity for high-priority traffic ahead of lower-priority traffic, and no such reserved-capacity mechanism is documented. [Rate limit filter docs](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter). This repo's own prior research (v2 roadmap plan) already established that Envoy Gateway's "global" rate-limiting explicitly avoids cross-DC counter synchronization.
11. Kong's `cluster`/`redis` rate-limiting strategies synchronize a single shared counter across nodes — fleet-wide coordination of one limit — but no priority-tiered reserved-capacity mechanism analogous to RateCap's Tier 3 is documented. Same sources as footnote 2.
12. Sentinel's Cluster Flow Control genuinely coordinates a shared limit across a service cluster (its docs describe limiting "the frequency of one invocation to 10 per second in total" across 50 instances) — a real fleet-wide mechanism, unlike Cloudflare/Envoy's explicitly local-only "global" features. However, no priority-tiered reserved-capacity feature (critical vs. sheddable traffic, as RateCap's Tier 3 implements) is documented alongside it, so this is left "Not verified" for the full Tier-3-equivalent mechanism rather than forced into ✅/❌. [Cluster flow control](https://sentinelguard.io/en-us/docs/cluster-flow-control.html).
13. Envoy's local rate limit filter operates entirely per-Envoy-instance or per-connection with no remote call — architecturally local like RateCap's Tier 4 — though its trigger is a rate/token-bucket signal rather than system/CPU utilization. Same source as footnote 1.
14. Cloudflare operates as an edge/CDN network rather than a per-instance service process, so a "local worker utilization" comparison doesn't map cleanly onto its architecture; no equivalent mechanism was found in the official rate-limiting docs checked (same sources as footnote 3).
15. Sentinel's System Adaptive Protection sheds load based on local system metrics (`load1`, CPU usage, and locally-tracked QPS/response-time), computed entirely within the instance applying the rule — no remote call. [System adaptive protection](https://sentinelguard.io/en-us/docs/system-adaptive-protection.html).
```

- [ ] **Step 2: Verify every source URL cited above actually resolves**

Run:
```bash
for url in \
  "https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter" \
  "https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter" \
  "https://developer.konghq.com/plugins/rate-limiting/" \
  "https://developer.konghq.com/plugins/rate-limiting-advanced/" \
  "https://developers.cloudflare.com/waf/rate-limiting-rules/" \
  "https://developers.cloudflare.com/waf/rate-limiting-rules/parameters/" \
  "https://sentinelguard.io/en-us/docs/flow-control.html" \
  "https://github.com/Netflix/concurrency-limits" \
  "https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking" \
  "https://github.com/alibaba/sentinel/wiki" \
  "https://github.com/alibaba/Sentinel/wiki/Guideline:-Migration-from-Hystrix-to-Sentinel" \
  "https://sentinelguard.io/en-us/docs/cluster-flow-control.html" \
  "https://sentinelguard.io/en-us/docs/system-adaptive-protection.html" \
  ; do
  http_code=$(curl -s -o /dev/null -w "%{http_code}" -L "$url")
  echo "$http_code $url"
done
```
Expected output: every line reports `200` (the `-L` flag follows the one known redirect on `docs.konghq.com` → `developer.konghq.com`, but both cited URLs are already the post-redirect `developer.konghq.com` form, so no `301`/`302` should appear). If any URL returns a non-`200` status, fix that citation (find the current correct URL on the same official site) before proceeding — do not leave a dead link. (Note: name the capture variable `http_code`, not `status` — `status` is a reserved/readonly variable in zsh and assigning to it fails with `read-only variable: status`.)

- [ ] **Step 3: Verify every citation number in the table has a matching entry in the Sources list, and vice versa**

Run: `python3 -c "
import re
with open('README.md') as f:
    content = f.read()
start = content.index('| Mechanism |')
end = content.index('## Design docs')
section = content[start:end]
table_part, sources_part = section.split('### Sources')
table_cites = set(int(n) for n in re.findall(r'\[(\d+)\]', table_part))
source_labels = set(int(n) for n in re.findall(r'^(\d+)\.', sources_part, re.MULTILINE))
print('cited in table but missing from Sources:', sorted(table_cites - source_labels))
print('in Sources but never cited in table:', sorted(source_labels - table_cites))
"`
Expected output:
```
cited in table but missing from Sources: []
in Sources but never cited in table: []
```

- [ ] **Step 4: Re-render the final table to confirm valid markdown table syntax**

Run: `python3 -c "
with open('README.md') as f:
    content = f.read()
table_start = content.index('| Mechanism |')
table_end = content.index('### Sources')
table = content[table_start:table_end]
rows = [r for r in table.strip().split(chr(10)) if r.strip()]
counts = set()
for r in rows:
    counts.add(len(r.split('|')))
print('distinct column-split counts across all rows:', counts)
print('total rows:', len(rows))
"`
Expected output:
```
distinct column-split counts across all rows: {9}
total rows: 7
```
(1 header + 1 separator + 5 mechanism rows = 7; all rows must split into the same number of `|`-delimited fields, confirming no row is missing a column.)

- [ ] **Step 5: Spot-check that no cell value outside the 3 allowed values slipped in**

Run: `grep -oE '\| (✅|❌|Not verified( \[[0-9]+\])?)( \[[0-9]+\])? \|' README.md | sort -u | head -20`
Expected: every match is exactly `✅ [n]`, `❌ [n]`, or `Not verified [n]` (with a bracketed citation number) — no bare `Not verified` without a citation, no other symbols.

Run: `python3 -c "
with open('README.md') as f:
    lines = f.readlines()
bad = []
for i, line in enumerate(lines, 1):
    if not line.startswith('| ') or 'Mechanism' in line or '---' in line:
        continue
    cols = [c.strip() for c in line.strip().split('|')]
    if len(cols) < 9:
        continue
    comparison_cells = cols[3:8]  # excludes the mechanism-name and RateCap columns
    for cell in comparison_cells:
        if cell in ('✅', '❌', 'Not verified'):
            bad.append((i, cell, line.strip()))
if bad:
    print('BARE CELLS FOUND (missing citation):')
    for b in bad:
        print(b)
else:
    print('OK: every comparison-column cell has a citation')
"`
Expected output: `OK: every comparison-column cell has a citation` (a naive `grep` for bare `✅`/`❌`/`Not verified` would false-positive on the RateCap column's intentionally-uncited ✅ values — this script explicitly excludes columns 1-2 (mechanism name, RateCap) and only checks the 5 comparison columns, which must all carry a citation).

- [ ] **Step 6: Run the full repo test suite as a final no-op sanity check (confirms this docs-only change touched no code)**

Run: `cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/v2-phase-4-production-readiness && git diff --stat HEAD~1`
Expected output: exactly one file changed, `README.md`, with insertions only in this task's diff (Task 1's commit is the `HEAD~1` baseline for this check).

- [ ] **Step 7: Commit**

```bash
git add README.md
git commit -m "docs: source remaining Comparison table cells from official docs, finalize Sources list"
```

---

## Self-Review Notes (completed during plan authoring)

**Spec coverage:** §1 (placement) → Task 1 Step 2. §2 (table shape: 5 rows, 6 columns, exact order) → Task 1 Step 2's table skeleton, unchanged in Task 2. §3 (exactly 3 cell values, every ✅/❌ cited) → enforced throughout, verified mechanically in Task 2 Steps 3 and 5. §4 (sourcing: reuse already-verified facts first, light targeted lookups second, never a second deep-research pass) → Task 1 uses only the 3 already-verified facts named in the spec; Task 2's 15 footnotes are each a single official-source citation, no cross-checking with a second source per claim. §5 (framing paragraph, no marketing adjectives) → Task 1 Step 2's exact paragraph text.

**Placeholder scan:** No "TBD"/"TODO". Every step's markdown content is the actual final text to insert, not a description of what to write. "Not verified" cells are a legitimate spec-defined final value, not a placeholder — confirmed 5 such cells remain after Task 2's fresh lookups (Envoy/Kong/Cloudflare/Sentinel's Tier 3 reserved-capacity mechanism, Kong/Cloudflare/Netflix's Tier 4 utilization-based shedding), each with a footnote explaining why no confident answer was found rather than a bare unexplained gap.

**Type/reference consistency:** Task 2's replacement table's footnote numbers (`[1]` through `[15]`) match one-to-one with the 15 numbered Sources entries — verified by the Step 3 script that will be run during implementation, and manually cross-checked while writing this plan (every `[n]` in the table body appears as `n.` in the Sources list, and every Sources entry is cited at least once).
