# Walking Skeleton Review Fixes — Design Spec

**Date:** 2026-07-13
**Status:** Approved
**Context:** Fixes for the two Important findings from the walking-skeleton's final whole-branch review (all 11 tasks + final review completed; see `2026-07-13-ratecap-v1-design.md` and `2026-07-13-walking-skeleton.md` for the original plan/spec).

---

## Problem

The final whole-branch review found zero Critical issues but two Important findings, both non-correctness robustness gaps:

1. **Dockerfile stale-cache risk.** `services/core/Dockerfile`, `services/sidecar/Dockerfile`, and `deploy/sampleapp/Dockerfile` all build with `GOWORK=off go build ./...` against a copied root `go.work`/`go.work.sum`. Because `GOWORK=off` tells Go to ignore the workspace file entirely, a change to `go.work.sum` (e.g. bumping a shared dependency) produces no new Docker layer to invalidate the cache — a rebuild can silently serve a stale binary unless the developer remembers `docker compose build --no-cache`.

2. **Missing priority-header regression test.** `services/sidecar/proxy/proxy.go`'s `ServeHTTP` calls `ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)` but discards the result (`_ = ...`), since Tier 1 doesn't use priority — only Tier 3 (out of this plan's scope) will. `priority_test.go` covers `ResolvePriority` as a pure function; `proxy_test.go` covers the handler's status-code behavior; nothing proves the header-parsing call is actually wired into the live HTTP path. When Tier 3 replaces the discard with real branching logic, there's no existing test that would need updating to reflect that change — no tripwire.

## Investigation

**Dockerfile fix — empirically verified.** Removing `GOWORK=off` outright (the reviewer's stated preference) breaks every build: each Dockerfile only `COPY`s its own module's source (e.g. `services/core/Dockerfile` never copies `services/sidecar/` or `packages/`), while the root `go.work` lists all 5 repo modules. Go refuses to build when `go.work` references a `use` directory absent from the build context (confirmed via a local reproduction: `go: cannot load module ../sidecar listed in go.work file: open ../sidecar/go.mod: no such file or directory`).

The fix that actually works: generate a **build-context-scoped `go.work`** inline in each Dockerfile, listing only the modules that Dockerfile's `COPY` instructions actually bring in, then build normally (no `GOWORK=off`, no need to copy the root `go.work`/`go.work.sum` at all — each module already carries its own `go.sum`, and Go resolves a fresh workspace from that). This was verified end-to-end against the real `services/core` module: the image built successfully and the resulting binary ran correctly (failing only on the expected missing-mounted-config error, proving it's a real, working binary).

This fixes the actual defect — a real dependency bump now shows up as a change to the `COPY services/core/` layer (via `go.mod`/`go.sum`), which already invalidates Docker's cache correctly, no `--no-cache` reliance needed — without the cost of copying the entire repo into every image's build context.

## Fix 1: Scoped go.work per Dockerfile

For each of the three Dockerfiles, replace the `COPY go.work go.work.sum ./` + `GOWORK=off go build` pattern with an inline-generated workspace file scoped to that Dockerfile's actual dependencies, then a plain `go build`.

**`services/core/Dockerfile`** (needs `proto`, `services/core`):
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

**`services/sidecar/Dockerfile`** (needs `proto`, `services/sidecar`):
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

**`deploy/sampleapp/Dockerfile`** (needs `packages/sdks/go`, `deploy/sampleapp` — note this one has no dependency on `proto` directly):
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

## Fix 2: Priority-header characterization test

Add one test to `services/sidecar/proxy/proxy_test.go` pinning today's behavior — the header is read and has no effect on the response, because Tier 1 ignores priority entirely:

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

This is a **characterization test**, not a bug-fix test — it pins existing behavior rather than driving new behavior, so it is expected to pass immediately with no production-code change. When Tier 3 later makes the priority value load-bearing, this test's assertion will need deliberate revisiting, which is the regression tripwire the reviewer asked for.

## Verification Plan

1. Add the test first, confirm it passes against current `proxy.go` (no code change expected or needed).
2. Run `services/sidecar`'s full test suite (`GOWORK=off go test ./... -race`, standalone module test — unaffected by the Dockerfile changes).
3. Apply the three Dockerfile changes.
4. Rebuild the full docker-compose stack (`docker compose up --build`) and re-run the same end-to-end curl verification used after the original walking-skeleton completion (5 requests → `200`, 6th+ → `429`), proving the Dockerfile change doesn't regress the working demo.
5. Tear down cleanly (`docker compose down`).

## Out of Scope

- No behavior change to priority resolution itself — it remains parsed-and-discarded at Tier 1, exactly as designed. Making priority load-bearing is Tier 3 work, not this fix.
- No change to `go.work` at the repo root — it continues to list all 5 modules for local development; only the Docker build path changes.
- No CI/lint enforcement added to catch a *future* Dockerfile drifting back to `GOWORK=off` — out of scope for this small fix; could be a follow-up if it recurs.
