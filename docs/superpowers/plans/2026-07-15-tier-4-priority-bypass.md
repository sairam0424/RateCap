# Tier 4 Priority Bypass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the one production-risk finding from Tier 4's pre-PR audit — critical-priority requests now bypass the sidecar's local worker-shedder check entirely, so ordinary sheddable-traffic load can no longer starve critical traffic before Tier 3's reserved-capacity carve-out ever runs.

**Architecture:** Move priority resolution earlier in `Handler.ServeHTTP` (it only needs the request header, no dependency on `key`), then make the shedder pre-check conditional on priority: `Critical` skips `Shedder.Allow()`/`Release()` entirely, `Sheddable` behaves exactly as it does today. This mirrors the existing `SkipReservations` pattern that already lets certain requests bypass Tiers 2/3's Redis-backed checks.

**Tech Stack:** Go 1.26 (`services/sidecar/proxy`), no new dependencies.

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` before every commit that touches `services/sidecar`.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Scope is this ONE finding only. The other 4 Important + 5 Minor findings from Tier 4's audit are tracked as follow-up issues and must NOT be touched by this plan.
- Sheddable-priority behavior must be unchanged — this fix must not weaken Tier 4 for the common case.
- Docker was confirmed reachable as of the just-completed audit — needed for this task's live e2e re-verification step.
- Exact commands and exact expected output are given in every step; run them verbatim.

---

### Task 1: Critical priority bypasses the worker shedder

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify: `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: `worker.Shedder.Allow()`/`Release()` (existing, unchanged signatures), `ResolvePriority`/`Priority`/`Critical`/`Sheddable` (existing, unchanged).
- Produces: no new exported symbols — this is a behavior-only change to `Handler.ServeHTTP`'s internal control flow. No later task depends on anything new here.

- [ ] **Step 1: Write the failing tests**

Add to `services/sidecar/proxy/proxy_test.go`, after `TestServeHTTP_ShadowModeProceedsToClientInsteadOfShedding`:

```go
func TestServeHTTP_CriticalPriorityBypassesShedderWhenOverLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (critical priority bypasses the shedder even at max=0), got %d", rec.Code)
	}
	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called for a critical-priority request, even though the in-flight limit was exceeded")
	}
	if client.lastReq.Priority != ratecapv1.Priority_CRITICAL {
		t.Errorf("expected Priority_CRITICAL on the outgoing request, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_SheddablePriorityStillShedsWhenOverLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "sheddable")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (sheddable priority still sheds at max=0, unchanged behavior), got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected CheckRateLimit to never be called for a sheddable-priority request over the in-flight limit")
	}
}

func TestServeHTTP_CriticalPriorityDoesNotConsumeOrReleaseAShedderSlot(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(1)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if !shedder.Allow() {
		t.Fatal("expected the shedder's single slot to still be free after a critical-priority request, since critical never calls Allow()/Release()")
	}
	shedder.Release()
}
```

Add `"github.com/ratecap/sidecar/worker"` to the test file's import block if it is not already present (it already is, from Tier 4's own implementation — confirm rather than duplicate the import).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_CriticalPriorityBypassesShedderWhenOverLimit|TestServeHTTP_SheddablePriorityStillShedsWhenOverLimit|TestServeHTTP_CriticalPriorityDoesNotConsumeOrReleaseAShedderSlot' -v 2>&1 | tail -30`
Expected: FAIL — `TestServeHTTP_CriticalPriorityBypassesShedderWhenOverLimit` fails with `expected 200 ..., got 503` (today, priority resolution happens after the shed check, so a critical request over the limit is shed exactly like a sheddable one); the other two tests currently pass unchanged, since they describe today's existing behavior for the paths this fix does not touch — confirm they still pass after this run so the RED signal is isolated to the one test that actually needs new behavior.

- [ ] **Step 3: Update `services/sidecar/proxy/proxy.go`**

Replace `ServeHTTP`'s method-check-through-key-validation block:

```go
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.shedder.Allow() {
		if !shadow.GlobalOverrideEnabled() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		log.Printf("worker shedder: would have shed request, shadow mode active")
	} else {
		defer h.shedder.Release()
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	priority := ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)
	protoPriority := ratecapv1.Priority_SHEDDABLE
	if priority == Critical {
		protoPriority = ratecapv1.Priority_CRITICAL
	}
```

with:

```go
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	priority := ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)
	protoPriority := ratecapv1.Priority_SHEDDABLE
	if priority == Critical {
		protoPriority = ratecapv1.Priority_CRITICAL
	}

	if priority != Critical {
		if !h.shedder.Allow() {
			if !shadow.GlobalOverrideEnabled() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			defer h.shedder.Release()
		}
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
```

The rest of `ServeHTTP` (the `CheckRateLimitRequest` construction using `protoPriority`, response header/status handling) is unchanged — it already only reads `protoPriority`, which is now computed earlier but with an identical value in every case. `ReleaseHandler` is entirely unaffected — do not touch it.

- [ ] **Step 4: Run proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -100`
Expected: PASS — every test, including all pre-existing ones and the 3 new ones, reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`

- [ ] **Step 5: Run the full sidecar test suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `proxy`, `shadow`, `worker`

- [ ] **Step 6: Confirm Docker is reachable**

Run: `docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"`

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 7: Live re-verification — critical priority survives sheddable-priority contention**

Run from `deploy/`:

```bash
cd deploy
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`.

Then drive a burst that would have starved a critical request under the OLD behavior — 5 sheddable-priority requests directly against the sidecar (saturating the in-flight cap of 3), plus 1 critical-priority request fired into the same burst:

```bash
for i in 1 2 3 4 5; do
  curl -s -o /dev/null -w "sheddable %{http_code}\n" "http://localhost:8080/check?key=priority-fix-sheddable-$i&skip_reservations=true" -H "x-ratecap-priority: sheddable" &
done
curl -s -o /dev/null -w "critical %{http_code}\n" "http://localhost:8080/check?key=priority-fix-critical-1&skip_reservations=true" -H "x-ratecap-priority: critical" &
wait
```

(Priority is controlled exclusively via the `x-ratecap-priority` header, per `proxy.go`'s actual implementation — there is no `priority` query parameter.)

Expected: among the 5 `sheddable` lines, exactly 3x`200` and 2x`503` (unchanged Tier 4 behavior for sheddable priority, matching `RATECAP_MAX_INFLIGHT_REQUESTS=3`); the single `critical` line reports `200` — proving the fix: a critical-priority request now succeeds even while the in-flight cap is fully saturated by sheddable-priority traffic, which would have been shed under the pre-fix behavior.

- [ ] **Step 8: Re-verify tiers 1-3 (regression check)**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```
Expected: exactly 5x`checkout 200` then 2x`checkout 429`.

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
wait
```
Expected: exactly 3x`fleet-demo 200` and 2x`fleet-demo 503`.

- [ ] **Step 9: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 10: Run every module's full test suite one final time**

Run:

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
```

Expected: `ok` for every package (`services/core/store` needs Docker, which is reachable per this task, so it should run live).

- [ ] **Step 11: gofmt check and commit**

Run: `gofmt -l services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go`
Expected: no output

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "fix(sidecar): let critical priority bypass the worker shedder

Tier 4's shed check was priority-blind: ordinary sheddable-priority
load could saturate the in-flight cap and 503 critical-priority
requests before tier 3's reserved-capacity carve-out ever ran,
silently defeating the entire point of priority tagging. Critical
requests now skip Shedder.Allow()/Release() entirely, mirroring how
SkipReservations already bypasses tiers 2/3's Redis-backed checks.
Sheddable-priority behavior is unchanged."
```

---

## Post-plan note

This closes the one production-risk finding from Tier 4's pre-PR audit that the user chose to fix now. The remaining 4 Important + 5 Minor findings (wire-identical 503s between Tier 3 and Tier 4, zero logging on the real shed path, no observability/metrics for Tier 4, no `RATECAP_MAX_INFLIGHT_REQUESTS` range validation, no HTTP server timeouts, `/worker-demo` missing from `SECURITY.md`'s per-endpoint list) are tracked as GitHub follow-up issues, not implemented here. Once this plan's single task is reviewed and merged, Tier 4 — and with it, all four of Stripe's tiers per RateCap's v1 scope — is ready for its PR into `develop`.
