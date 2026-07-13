# Walking Skeleton Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the two Important findings from RateCap's walking-skeleton final review — Dockerfiles that silently serve stale binaries after a dependency bump, and an unguarded priority-header code path with no regression test.

**Architecture:** Each Dockerfile switches from `GOWORK=off` against a copied root `go.work`/`go.work.sum` to generating its own build-context-scoped `go.work` inline (listing only the modules that Dockerfile's `COPY` instructions actually bring in), then building without `GOWORK=off` — so a real dependency change becomes a real cache-invalidating layer instead of a silently-ignored one. Separately, one characterization test pins the sidecar proxy's current "parse the priority header, ignore its value" behavior so a future change to that behavior (Tier 3) can't land without a test update.

**Tech Stack:** Go 1.26 (module workspaces), Docker / Docker Compose, no new dependencies.

## Global Constraints

- No comments in code unless explaining a non-obvious WHY. Never comment on WHAT the code does.
- Files: 200-400 lines typical, 800 max — not relevant here, no file grows.
- Reference spec: `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/docs/superpowers/specs/2026-07-13-walking-skeleton-review-fixes-design.md`
- Reference plan (original walking skeleton, for module/build conventions): `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/docs/superpowers/plans/2026-07-13-walking-skeleton.md`
- Fix 2's new test is a **characterization test**, not a bug-fix test: it is expected to pass immediately against the current, unmodified `proxy.go` — there is no RED phase to drive new production code, because no production code changes for this fix. Do not add or change any code in `services/sidecar/proxy/proxy.go` as part of Fix 2.
- Fix 1 involves no Go code changes at all — only the three Dockerfiles change.
- Both fixes are independent of each other and can be done in either order; this plan does Fix 2 first (faster feedback loop, no Docker required) then Fix 1 (needs Docker to verify).

---

## File Structure

```
services/core/Dockerfile              # Modify: scoped go.work, drop GOWORK=off
services/sidecar/Dockerfile           # Modify: scoped go.work, drop GOWORK=off
deploy/sampleapp/Dockerfile           # Modify: scoped go.work, drop GOWORK=off
services/sidecar/proxy/proxy_test.go  # Modify: add one characterization test
```

No new files are created by this plan.

---

## Task 1: Priority-header characterization test

**Files:**
- Modify: `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: `proxy.NewHandler(client ratecapClient, defaultPriority Priority) *Handler` and `proxy.Sheddable` (existing `Priority` constant), both already defined in `services/sidecar/proxy/proxy.go` and already used by every existing test in this file. `fakeRatecapClient` (existing test double in this same file) satisfies the `ratecapClient` interface.
- Produces: nothing new for later tasks — this is a leaf test with no downstream consumers.

- [ ] **Step 1: Add the characterization test**

Add this function to the end of `services/sidecar/proxy/proxy_test.go` (after `TestServeHTTP_ShadowLogReturns200`, the last existing test in the file):

```go
func TestServeHTTP_ParsesPriorityHeaderWithoutError(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 regardless of priority header (tier 1 ignores it), got %d", rec.Code)
	}
}
```

No new imports are needed — `http`, `httptest`, `ratecapv1`, and `proxy` are already imported by this file for the existing tests.

- [ ] **Step 2: Run the test to confirm it passes immediately (characterization, not TDD)**

Run:
```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/services/sidecar
GOWORK=off go test ./proxy/... -run TestServeHTTP_ParsesPriorityHeaderWithoutError -v
```

Expected:
```
=== RUN   TestServeHTTP_ParsesPriorityHeaderWithoutError
--- PASS: TestServeHTTP_ParsesPriorityHeaderWithoutError (0.00s)
PASS
ok  	github.com/ratecap/sidecar/proxy	...
```

This is expected to PASS on the first run with no production-code change — `proxy.go`'s `ServeHTTP` already calls `ResolvePriority` and discards the result, so behavior is unaffected by the header regardless of its value. This is the point of a characterization test: it locks in existing behavior as an explicit assertion, not a demonstration of newly-added logic.

- [ ] **Step 3: Run the full sidecar module test suite with the race detector**

Run:
```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/services/sidecar
GOWORK=off go test ./... -race -v
```

Expected: all packages (`proxy`, `shadow`) PASS, including the 4 tests now in `proxy_test.go` (3 pre-existing + this new one) and the pre-existing `shadow` package tests. No new failures, no race warnings.

- [ ] **Step 4: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton
git add services/sidecar/proxy/proxy_test.go
git commit -m "test(sidecar): pin priority-header parse-and-discard behavior

Adds a characterization test proving the x-ratecap-priority header is
read but has no effect on tier 1's response, closing the review gap
where nothing exercised the header-parsing call in a live request.
When tier 3 makes priority load-bearing, this test's assertion will
need deliberate updating — that update is the intended tripwire."
```

---

## Task 2: Scoped go.work per Dockerfile

**Files:**
- Modify: `services/core/Dockerfile`
- Modify: `services/sidecar/Dockerfile`
- Modify: `deploy/sampleapp/Dockerfile`

**Interfaces:** None — Dockerfile-only change, no Go code or public interfaces affected.

- [ ] **Step 1: Rewrite services/core/Dockerfile**

Replace the entire contents of `services/core/Dockerfile` with:

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto/ proto/
COPY services/core/ services/core/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/core\n' > go.work
WORKDIR /src/services/core
RUN go build -o /ratecap-core .

FROM alpine:3.20
COPY --from=build /ratecap-core /usr/local/bin/ratecap-core
ENTRYPOINT ["/usr/local/bin/ratecap-core"]
```

This drops the `COPY go.work go.work.sum ./` line (the root workspace files are no longer used) and the `GOWORK=off` prefix, replacing them with a `go.work` generated fresh inside the image, scoped to exactly the two modules this Dockerfile copies in (`proto`, `services/core`).

- [ ] **Step 2: Rewrite services/sidecar/Dockerfile**

Replace the entire contents of `services/sidecar/Dockerfile` with:

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto/ proto/
COPY services/sidecar/ services/sidecar/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/sidecar\n' > go.work
WORKDIR /src/services/sidecar
RUN go build -o /ratecap-sidecar .

FROM alpine:3.20
COPY --from=build /ratecap-sidecar /usr/local/bin/ratecap-sidecar
ENTRYPOINT ["/usr/local/bin/ratecap-sidecar"]
```

- [ ] **Step 3: Rewrite deploy/sampleapp/Dockerfile**

Replace the entire contents of `deploy/sampleapp/Dockerfile` with:

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY packages/ packages/
COPY deploy/sampleapp/ deploy/sampleapp/
RUN printf 'go 1.26.2\n\nuse ./packages/sdks/go\nuse ./deploy/sampleapp\n' > go.work
WORKDIR /src/deploy/sampleapp
RUN go build -o /sampleapp .

FROM alpine:3.20
COPY --from=build /sampleapp /usr/local/bin/sampleapp
ENTRYPOINT ["/usr/local/bin/sampleapp"]
```

Note `sampleapp`'s workspace lists `./packages/sdks/go` and `./deploy/sampleapp` only — it depends on `github.com/ratecap/sdk-go` (see `deploy/sampleapp/go.mod`'s `replace` directive), not on `proto` directly, so `proto` is correctly absent from this scoped workspace.

- [ ] **Step 4: Ensure Docker is reachable**

```bash
docker info >/dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

If not reachable, run `open -a Docker` and poll (`sleep 10 && docker info`) for up to 60 seconds before proceeding. Do not continue to Step 5 until this prints "docker reachable" — Docker has been observed to go unreachable intermittently in this environment even after starting successfully once.

- [ ] **Step 5: Rebuild all three images from scratch**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/deploy
docker compose build --no-cache
```

Expected: all three custom images (`core`, `sidecar`, `sampleapp`) build successfully with no errors. `--no-cache` is used here specifically to prove the new Dockerfiles work from a cold cache — this is a one-time verification, not a standing requirement (the whole point of this fix is that future builds do NOT need `--no-cache` to stay correct).

- [ ] **Step 6: Bring up the full stack and verify end-to-end rate limiting still works**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/deploy
docker compose up -d
sleep 3
for i in 1 2 3 4 5 6 7; do
  curl -s -o /dev/null -w "request $i: %{http_code}\n" http://localhost:3000/checkout
done
```

Expected: requests 1-5 print `200`, requests 6-7 print `429` — identical behavior to the original walking-skeleton's end-to-end verification (config is `default_rate: 2`, `default_burst: 5`, unchanged by this fix). This proves the new Dockerfiles produce working images, not just images that build.

- [ ] **Step 7: Tear down**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton/deploy
docker compose down
```

- [ ] **Step 8: Confirm no stray files were left in the working tree**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton
git status --short
```

Expected: only the three modified Dockerfiles show as changed — `docker compose build`/`up`/`down` do not write into the repo's working tree, so this should be clean apart from the intended edits.

- [ ] **Step 9: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/walking-skeleton
git add services/core/Dockerfile services/sidecar/Dockerfile deploy/sampleapp/Dockerfile
git commit -m "fix(deploy): scope go.work per Dockerfile instead of GOWORK=off

GOWORK=off made every image build ignore the copied root go.work.sum
entirely, so a real dependency bump produced no new Docker layer and
could leave a stale binary in the image unless a developer remembered
--no-cache. Each Dockerfile now generates its own go.work scoped to
only the modules it actually copies in, then builds normally — a
dependency change now shows up as a genuine cache-invalidating layer
(the COPY of that module's go.mod/go.sum), with no GOWORK=off and no
--no-cache reliance needed for correctness."
```

---

## Self-Review

**Spec coverage:**
- Fix 1 (Dockerfile scoped go.work, all three files, exact contents matching the spec's "Fix 1" section verbatim) — Task 2 ✓
- Fix 2 (characterization test, exact code matching the spec's "Fix 2" section verbatim) — Task 1 ✓
- Verification plan's exact sequence (test first, sidecar suite with -race, then Dockerfiles, then full-stack rebuild+curl, then teardown) — followed across Task 1 then Task 2 ✓
- Spec's "Out of Scope" items (no priority-resolution behavior change, no root go.work change, no CI/lint enforcement) — correctly untouched by both tasks ✓

**Placeholder scan:** No TBD/TODO/FIXME; every step shows exact file contents or exact commands with expected output.

**Type/interface consistency:** Task 1's test uses `proxy.NewHandler`, `proxy.Sheddable`, and `fakeRatecapClient` exactly as they exist in the current `proxy_test.go` (verified by reading the file before writing this plan) — no new types introduced, no signature mismatches possible since no interfaces change.
