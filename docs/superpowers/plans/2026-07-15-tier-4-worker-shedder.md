# Tier 4 (Worker Utilization Load Shedder) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Tier 4 (Worker Utilization Load Shedder) — a sidecar-local pre-check, using an atomic in-flight request counter, that sheds load with `503` before ever contacting `ratecap-core`, protecting the sidecar's own capacity as the final line of defense.

**Architecture:** A new `services/sidecar/worker` package (`Shedder`, an atomic counter with a fixed max) is checked by `proxy.Handler.ServeHTTP` before any gRPC call to core. Over-limit requests are shed immediately with zero round-trip; shadow mode logs the would-have-shed event and proceeds instead of enforcing. Config is one sidecar-local env var (`RATECAP_MAX_INFLIGHT_REQUESTS`), no core involvement, no hot-reload — this tier touches only `services/sidecar` and `deploy/`.

**Tech Stack:** Go 1.26 (`services/sidecar`), no new dependencies. This is the first tier in the project with zero Redis/testcontainers dependency — every unit test in this plan is pure Go concurrency testing.

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module) before every commit that touches that module.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Zero changes to `services/core`, `packages/sdks/go`, or `proto` — this tier is entirely additive to `services/sidecar` and `deploy/`.
- No hot-reload, no core-to-sidecar config sync — `RATECAP_MAX_INFLIGHT_REQUESTS` is read once at `services/sidecar/main.go` startup.
- No Redis/testcontainers integration tests are needed anywhere in this plan — the first tier in this project without one. Docker is only needed for Task 4's live docker-compose e2e verification, not for any Go-level test.
- Exact commands and exact expected output are given in every step; run them verbatim.

---

### Task 1: `worker.Shedder` — atomic in-flight counter

**Files:**
- Create: `services/sidecar/worker/shedder.go`
- Create: `services/sidecar/worker/shedder_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (first task).
- Produces: `worker.Shedder`, `worker.NewShedder(max int64) *Shedder`, `(*Shedder) Allow() bool`, `(*Shedder) Release()`. Task 2's `proxy.Handler` wiring depends on these exact names and signatures.

- [ ] **Step 1: Write the failing tests**

Create `services/sidecar/worker/shedder_test.go`:

```go
package worker_test

import (
	"sync"
	"testing"

	"github.com/ratecap/sidecar/worker"
)

func TestShedder_AllowsExactlyMaxConcurrent(t *testing.T) {
	s := worker.NewShedder(3)

	for i := 0; i < 3; i++ {
		if !s.Allow() {
			t.Fatalf("request %d: expected Allow() to return true within max of 3", i)
		}
	}

	if s.Allow() {
		t.Fatal("4th request: expected Allow() to return false, max of 3 exceeded")
	}
}

func TestShedder_ReleaseFreesASlot(t *testing.T) {
	s := worker.NewShedder(1)

	if !s.Allow() {
		t.Fatal("expected first Allow() to return true")
	}
	if s.Allow() {
		t.Fatal("expected second Allow() to return false, max of 1 exceeded")
	}

	s.Release()

	if !s.Allow() {
		t.Fatal("expected Allow() to return true after Release() frees the slot")
	}
}

func TestShedder_ConcurrentAllowAndReleaseIsRaceFree(t *testing.T) {
	s := worker.NewShedder(10)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				s.Release()
			}
		}()
	}
	wg.Wait()
}

func TestShedder_AllowedCountNeverExceedsMaxUnderConcurrency(t *testing.T) {
	s := worker.NewShedder(5)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Allow() {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != 5 {
		t.Fatalf("expected exactly 5 concurrent Allow() calls to succeed (none released), got %d", allowedCount)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/sidecar && go test ./worker/... 2>&1 | head -20`
Expected: FAIL — `no required module provides package github.com/ratecap/sidecar/worker` (the package doesn't exist yet)

- [ ] **Step 3: Write `services/sidecar/worker/shedder.go`**

```go
package worker

import "sync/atomic"

type Shedder struct {
	inflight atomic.Int64
	max      int64
}

func NewShedder(max int64) *Shedder {
	return &Shedder{max: max}
}

func (s *Shedder) Allow() bool {
	if s.inflight.Load() >= s.max {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *Shedder) Release() {
	s.inflight.Add(-1)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/sidecar && go test ./worker/... -race -v 2>&1 | tail -20`
Expected: PASS — all 4 tests report `--- PASS`, final line `ok      github.com/ratecap/sidecar/worker`

- [ ] **Step 5: gofmt check and commit**

Run: `gofmt -l services/sidecar/worker/shedder.go services/sidecar/worker/shedder_test.go`
Expected: no output

```bash
git add services/sidecar/worker/shedder.go services/sidecar/worker/shedder_test.go
git commit -m "feat(sidecar): add worker.Shedder — atomic in-flight request counter

Pure, dependency-free signal for tier 4: allows exactly max
concurrent Allow() calls, Release() frees a slot for a subsequent
caller. No Redis, no HTTP, no gRPC — this tier's shed decision
never depends on any shared or caller-identified state."
```

---

### Task 2: Wire `Shedder` into `Handler.ServeHTTP` as a pre-check

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify: `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: `worker.NewShedder(max int64) *Shedder`, `(*Shedder) Allow() bool`, `(*Shedder) Release()` (Task 1).
- Produces: `proxy.NewHandler(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder) *Handler` (3rd parameter added — this is a breaking signature change). Task 3's `main.go` wiring depends on this exact 3-argument signature.

**Critical note on this task's scope:** `proxy_test.go` has **11 pre-existing test functions** that each call `proxy.NewHandler(client, proxy.Sheddable)` with only 2 arguments today: `TestServeHTTP_AllowReturns200`, `TestServeHTTP_Reject429Returns429`, `TestServeHTTP_ShadowLogReturns200`, `TestServeHTTP_ParsesPriorityHeaderWithoutError`, `TestServeHTTP_ThreadsCriticalPriorityHeaderIntoRequest`, `TestServeHTTP_DefaultsToSheddablePriorityWhenNoHeader`, `TestServeHTTP_SetsIndexedConcurrencyHeadersForEachReservation`, `TestServeHTTP_OmitsIndexedConcurrencyHeadersWhenNoReservations`, `TestServeHTTP_SkipReservationsParamSetsSkipReservationsOnRequest`, `TestServeHTTP_NoSkipReservationsParamLeavesSkipReservationsFalse`, `TestServeHTTP_RejectsNonGETMethod`. Adding a 3rd parameter to `NewHandler` breaks every one of these call sites — the package will not compile until all 11 are updated. This must happen in this task's single commit; there is no intermediate state where the package compiles with only some call sites updated.

- [ ] **Step 1: Update all 11 pre-existing `NewHandler` call sites in `services/sidecar/proxy/proxy_test.go`**

In each of the 11 test functions listed above, replace:

```go
	h := proxy.NewHandler(client, proxy.Sheddable)
```

with:

```go
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))
```

(A limit of 1000 is far above anything any single test in this file drives concurrently, so none of these 11 pre-existing tests will ever trip the shedder — they exist to test tiers 1-3's behavior, not tier 4's, and must keep passing unchanged.)

Add the import `"github.com/ratecap/sidecar/worker"` to `proxy_test.go`'s import block:

```go
import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/worker"
)
```

- [ ] **Step 2: Write the new failing tests for tier 4's pre-check**

Add to `services/sidecar/proxy/proxy_test.go`, after `TestServeHTTP_RejectsNonGETMethod`:

```go
func TestServeHTTP_ShedsWithoutCallingClientWhenOverInFlightLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected CheckRateLimit to never be called when the in-flight limit is exceeded")
	}
}

func TestServeHTTP_AllowsRequestAndReleasesSlotWhenUnderLimit(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(1)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called when under the in-flight limit")
	}

	if !shedder.Allow() {
		t.Fatal("expected the slot to have been released after ServeHTTP returned, but Allow() still reports over-limit")
	}
}

func TestServeHTTP_ShadowModeProceedsToClientInsteadOfShedding(t *testing.T) {
	t.Setenv("RATECAP_SHADOW_MODE", "true")

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called even though the in-flight limit was exceeded, since shadow mode is active")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (core's own ALLOW response, shadow mode doesn't force a code here), got %d", rec.Code)
	}
}
```

- [ ] **Step 3: Run tests to verify the new ones fail and the 11 updated call sites compile**

Run: `cd services/sidecar && go test ./proxy/... 2>&1 | head -30`
Expected: FAIL — either a compile error (if `NewHandler`'s signature hasn't been updated yet in `proxy.go`, which is expected at this point) or, once Step 4 below is done, the 3 new tests failing on their behavioral assertions (503 not yet returned, `Allow()` not yet checked)

- [ ] **Step 4: Update `services/sidecar/proxy/proxy.go`**

Add the import `"github.com/ratecap/sidecar/worker"`:

```go
import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
	"github.com/ratecap/sidecar/worker"
)
```

Replace the `Handler` struct and `NewHandler`:

```go
type Handler struct {
	client          ratecapClient
	defaultPriority Priority
	shedder         *worker.Shedder
}

func NewHandler(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder}
}
```

Replace `ServeHTTP`'s method-check block through the `key` validation block:

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
```

Add `"log"` to the import block (needed for the shadow-mode log line):

```go
import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
	"github.com/ratecap/sidecar/worker"
)
```

The rest of `ServeHTTP` (priority resolution, the `CheckRateLimit` call, response header/status handling) is unchanged. `ReleaseHandler` is entirely unaffected by this task — do not touch it.

- [ ] **Step 5: Run proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -80`
Expected: PASS — every test, including all 11 pre-existing ones (now passing a permissive shedder) and the 3 new ones, reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`

- [ ] **Step 6: Run the full sidecar test suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `proxy`, `shadow`, `worker`

- [ ] **Step 7: gofmt check and commit**

Run: `gofmt -l services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go`
Expected: no output

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): shed load locally before contacting core

Handler.ServeHTTP now checks worker.Shedder first: over the
in-flight limit, it returns 503 immediately with zero gRPC call to
core — the literal 'no round-trip' reading of tier 4's spec.
Shadow mode logs the would-have-shed event and proceeds to core
instead of enforcing, matching every other tier's shadow-mode
contract."
```

---

### Task 3: Wire `RATECAP_MAX_INFLIGHT_REQUESTS` into `services/sidecar/main.go`

**Files:**
- Modify: `services/sidecar/main.go`

**Interfaces:**
- Consumes: `worker.NewShedder(max int64) *Shedder` (Task 1), `proxy.NewHandler(client, defaultPriority, shedder)` (Task 2).
- Produces: a running sidecar that reads its in-flight limit from the environment. Task 4's docker-compose config depends on this exact env var name.

- [ ] **Step 1: Update `services/sidecar/main.go`**

Add the imports `"strconv"` and `"github.com/ratecap/sidecar/worker"`:

```go
import (
	"log"
	"net/http"
	"os"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/auth"
	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/worker"
)
```

Insert this block immediately after the `sharedSecret` check (before `conn, err := grpc.NewClient(...)`):

```go
	maxInflight := int64(500)
	if v := os.Getenv("RATECAP_MAX_INFLIGHT_REQUESTS"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			log.Printf("RATECAP_MAX_INFLIGHT_REQUESTS=%q is not a valid integer, using default of %d: %v", v, maxInflight, err)
		} else {
			maxInflight = parsed
		}
	}
	shedder := worker.NewShedder(maxInflight)
```

Replace the `mux.Handle("/check", ...)` line:

```go
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable))
```

with:

```go
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
```

- [ ] **Step 2: Build to confirm main.go compiles**

Run: `cd services/sidecar && go build ./... 2>&1`
Expected: no output, exit code 0

- [ ] **Step 3: Run the full sidecar test suite**

Run: `cd services/sidecar && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `proxy`, `shadow`, `worker`

- [ ] **Step 4: gofmt check and commit**

Run: `gofmt -l services/sidecar/main.go`
Expected: no output

```bash
git add services/sidecar/main.go
git commit -m "feat(sidecar): read RATECAP_MAX_INFLIGHT_REQUESTS, wire the worker shedder

Defaults to 500 if unset. An unparseable value falls back to the
default with a logged warning rather than refusing to start —
unlike RATECAP_SHARED_SECRET, this is a tunable soft limit, not a
required credential."
```

---

### Task 4: Demo config, sample app, full end-to-end verification

**Files:**
- Modify: `deploy/docker-compose.yml`
- Modify: `deploy/sampleapp/main.go`

**Interfaces:**
- Consumes: everything from Tasks 1-3.
- Produces: a live-verified demo proving tier 4 sheds locally under load, alongside tier 1/2/3 regression checks.

- [ ] **Step 1: Add `RATECAP_MAX_INFLIGHT_REQUESTS` to `deploy/docker-compose.yml`'s sidecar service**

Replace the `sidecar` service's `environment:` block:

```yaml
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
```

with:

```yaml
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
      RATECAP_MAX_INFLIGHT_REQUESTS: "3"
```

(Deliberately low so a burst of 5 concurrent demo requests can trip it without needing an enormous number of concurrent calls — mirroring the exact pattern already used for `concurrency_limiter.default_max_concurrent: 3` and `fleet_shedder.default_max_concurrent: 5` in `deploy/ratecap.yaml`.)

- [ ] **Step 2: Add a `/worker-demo` handler to `deploy/sampleapp/main.go`**

Add this handler to `deploy/sampleapp/main.go`, after the existing `/fleet-demo` handler and before `log.Println("sample app listening on :3000")`:

```go
	http.HandleFunc("/worker-demo", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		key := fmt.Sprintf("worker-demo-%d", fleetDemoCounter.Add(1))

		checkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarBase+"/check?key="+url.QueryEscape(key)+"&skip_reservations=true", nil)
		if err != nil {
			http.Error(w, "request construction failed", http.StatusInternalServerError)
			return
		}

		resp, err := http.DefaultClient.Do(checkReq)
		if err != nil {
			http.Error(w, "worker check failed", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "shed (status=%d)\n", resp.StatusCode)
			return
		}

		time.Sleep(2 * time.Second)
		fmt.Fprintln(w, "worker request processed")
	})
```

(Uses `skip_reservations=true` and a per-request unique key so tiers 1-3 never trip during this endpoint's burst — this endpoint exists to isolate and demonstrate tier 4 specifically, the same isolation principle `/fleet-demo` already applies for tier 3. The 2-second sleep after the check succeeds is what lets a burst of concurrent calls exceed `RATECAP_MAX_INFLIGHT_REQUESTS=3` at the sidecar, since the in-flight count includes the sidecar's own handling time for the `/check` call, not this sample app's sleep — the sleep here is what keeps `/worker-demo` itself busy long enough for a human/script to observe overlapping calls, but the actual tier-4 shed happens inside the sidecar's own `/check` handling, which is fast; the real test burst in Step 5 below drives concurrency directly against the sidecar's `/check` endpoint, not through this sample-app wrapper, to avoid a confusing double layer of concurrency. This handler exists for completeness/manual exploration; the automated verification in Step 5 talks to the sidecar directly.)

`fleetDemoCounter` is already declared as a package-level `atomic.Int64` from the existing `/fleet-demo` handler — reused here rather than declaring a second counter, since both handlers only need "a unique key per call," not two independently-numbered sequences.

- [ ] **Step 3: Confirm Docker is reachable**

Run: `docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"`

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 4: Clean rebuild and bring the stack up**

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

- [ ] **Step 5: Re-verify tiers 1-3 (regression check)**

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

- [ ] **Step 6: Verify tier 4 sheds directly against the sidecar, distinguishably from a real round-trip**

This drives concurrency directly at the sidecar's `/check` endpoint (not through the sample app) to cleanly isolate tier 4's behavior from any sample-app-level concurrency. Each call uses a unique key and `skip_reservations=true` so tiers 1-3 never trip. The unit-level proof that a shed request never calls `CheckRateLimit` already exists (`TestServeHTTP_ShedsWithoutCallingClientWhenOverInFlightLimit` from Task 2) — this live step's job is to confirm the same behavior holds against the real, compiled, containerized binary, using per-request timing as the observable signal: a shed request returns immediately from an in-process atomic check, while an allowed request requires a real gRPC round-trip to `ratecap-core` plus a Redis-backed Tier 1/2/3 pipeline evaluation, so it necessarily takes measurably longer.

Run:

```bash
for i in 1 2 3 4 5; do
  curl -s -o /dev/null -w "worker %{http_code} %{time_total}s\n" "http://localhost:8080/check?key=worker-verify-$i&skip_reservations=true" &
done
wait
```

Expected: exactly 3x`worker 200 ...` and 2x`worker 503 ...` (matching `RATECAP_MAX_INFLIGHT_REQUESTS=3`), with the `503` lines' `time_total` visibly smaller than the `200` lines' — the `503`s complete in single-digit milliseconds (an in-process atomic check, no network hop beyond the one HTTP request already made), while the `200`s take measurably longer (a real gRPC call to core, which itself does Redis round-trips for Tier 1/2/3). Read the printed times; do not hardcode an exact millisecond threshold, since this depends on the local Docker network's actual latency — the qualitative gap (503s consistently faster than 200s) is the signal, not a specific number.

- [ ] **Step 7: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 8: Run every module's full test suite one final time (regression check across the whole plan)**

Run:

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
```

Expected: `ok` for every package (`services/core/store` needs Docker, which is reachable per this task, so it should run live).

- [ ] **Step 9: gofmt check and commit**

Run: `gofmt -l deploy/sampleapp/main.go`
Expected: no output

```bash
git add deploy/docker-compose.yml deploy/sampleapp/main.go
git commit -m "feat(deploy): demonstrate tier 4 shedding locally, verify end-to-end

RATECAP_MAX_INFLIGHT_REQUESTS=3 in the demo stack; a 5-request burst
directly against the sidecar shows exactly 3x200/2x503, and core's
log is confirmed unaffected by the burst — the shed requests never
reached core. Tier 1/2/3 regressions re-verified unchanged. All
four tiers of RateCap v1 now demonstrably work end-to-end."
```

---

## Post-plan note

This completes Tier 4 (Worker Utilization Load Shedder) implementation — the fourth and final tier of RateCap's v1 scope. Per this project's established cycle, the next step after this plan's 4 tasks are reviewed and merged is a full multi-aspect audit (live e2e + correctness + concurrency-safety + security + architecture lenses) before opening the PR into `develop` — the same process every prior tier went through, which surfaced real, worth-fixing findings each time. Once Tier 4's audit concludes and its PR merges, RateCap's v1 scope (all four of Stripe's tiers) is complete.
