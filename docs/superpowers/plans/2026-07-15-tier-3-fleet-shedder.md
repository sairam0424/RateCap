# Tier 3 (Fleet Usage Load Shedder) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Tier 3 (Fleet Usage Load Shedder) — a globally-keyed, priority-dependent concurrency shedder reusing Tier 2's exact Redis sorted-set/Lua mechanism, reserving critical traffic room a large volume of sheddable traffic can't consume.

**Architecture:** A new `FleetShedder` `Limiter` implementation (sibling to `ConcurrencyLimiter`) checks a fixed global key against a priority-dependent effective cap, wired as the pipeline's third tier. Priority resolution (`ResolvePriority`, already built in Tier 1's walking skeleton) finally becomes load-bearing, threaded through a new proto field. The existing `skip_concurrency_limit` flag is renamed to `skip_reservations` (v1's final shape — exactly two reservation-issuing tiers, forever) and the sidecar/SDK's single-reservation plumbing is generalized to carry every reservation a request produces, closing a latent multi-tier slot-leak risk.

**Tech Stack:** Go 1.26 modules (`proto`, `services/core`, `services/sidecar`, `packages/sdks/go`, `deploy/sampleapp`), Protocol Buffers/gRPC (`protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`), Redis (existing `IncrConcurrent`/`DecrConcurrent`/`concurrent_limiter.lua` — no changes), Docker Compose for e2e verification.

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module) before every commit that touches that module.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- This is pre-1.0 internal-only proto — rename/add fields outright, no deprecation shims.
- No changes to `services/core/limiter/pipeline.go`, `services/core/grpcserver/server.go`'s `Reservations`-building/`ReleaseConcurrency` handler logic, the Lua scripts, or the `StateStore` interface — confirmed unnecessary by the design spec.
- `critical_routes` static config-based priority resolution is explicitly out of scope this phase.
- A configurable fleet-key name is out of scope — `"fleet"` is a hardcoded constant.
- Exact commands and exact expected output are given in every step; run them verbatim.
- `protoc-gen-go`/`protoc-gen-go-grpc` are installed at `$(go env GOPATH)/bin` but not on `PATH` by default — prefix `protoc` invocations with `PATH="$(go env GOPATH)/bin:$PATH"`.
- Docker has been observed to go unreachable intermittently in this environment (confirmed down as of this worktree's baseline check) — Task 7's live e2e verification requires Docker Desktop to be running; check reachability before starting that task and restart Docker Desktop if needed.

---

### Task 1: Move `Priority` to `limiter`, add `Request.Priority`

**Files:**
- Modify: `services/core/limiter/limiter.go`
- Modify: `services/sidecar/proxy/priority.go`
- Modify: `services/sidecar/proxy/priority_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (first task).
- Produces: `limiter.Priority` (type), `limiter.Sheddable`/`limiter.Critical` (constants), `limiter.Request.Priority Priority` field. Task 2's `FleetShedder` and Task 5's sidecar wiring both depend on these exact names.

- [ ] **Step 1: Write the failing test — `limiter.Request` has a `Priority` field defaulting to `Sheddable`**

Add this test to a new file `services/core/limiter/limiter_test.go`:

```go
package limiter_test

import (
	"testing"

	"github.com/ratecap/core/limiter"
)

func TestRequest_PriorityDefaultsToSheddable(t *testing.T) {
	var req limiter.Request
	if req.Priority != limiter.Sheddable {
		t.Errorf("expected zero-value Priority to be Sheddable, got %v", req.Priority)
	}
}

func TestPriority_CriticalIsDistinctFromSheddable(t *testing.T) {
	if limiter.Critical == limiter.Sheddable {
		t.Error("expected Critical and Sheddable to be distinct values")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/core && go test ./limiter/... -run 'TestRequest_PriorityDefaultsToSheddable|TestPriority_CriticalIsDistinctFromSheddable' 2>&1 | head -20`
Expected: FAIL — compile error, `req.Priority` and `limiter.Critical`/`limiter.Sheddable` don't exist yet

- [ ] **Step 3: Add `Priority` type and `Request.Priority` field to `services/core/limiter/limiter.go`**

Replace the entire file with:

```go
package limiter

import "context"

type Action int

const (
	ALLOW Action = iota
	REJECT_429
	REJECT_503
	SHADOW_LOG
)

type Priority int

const (
	Sheddable Priority = iota
	Critical
)

type TokenReservation struct {
	Key   string
	Token string
}

type Decision struct {
	Action       Action
	RetryAfterMs int64
	Reservations []TokenReservation
}

type Request struct {
	Key              string
	Cost             int
	SkipReservations bool
	Priority         Priority
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
```

Note: `SkipConcurrencyLimit` is renamed to `SkipReservations` here directly — Task 3 (the proto rename) and every other consumer are updated in later steps of this same task and in Task 3, so the codebase does not compile between these edits; that's expected mid-task and resolved by Step 6 below.

- [ ] **Step 4: Update `services/core/limiter/concurrency.go`'s reference to the renamed field**

In `services/core/limiter/concurrency.go`, replace:

```go
	if req.SkipConcurrencyLimit {
		return Decision{Action: ALLOW}, nil
	}
```

with:

```go
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}
```

- [ ] **Step 5: Update `services/core/limiter/concurrency_test.go`'s reference to the renamed field**

In `services/core/limiter/concurrency_test.go`, in `TestConcurrencyLimiter_SkipConcurrencyLimitBypassesTheCapEntirely`, replace:

```go
			d, err := l.Check(ctx, limiter.Request{Key: "user-skip", SkipConcurrencyLimit: true})
```

with:

```go
			d, err := l.Check(ctx, limiter.Request{Key: "user-skip", SkipReservations: true})
```

- [ ] **Step 6: Run limiter tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -v 2>&1 | tail -40`
Expected: PASS — all tests including the two new ones report `--- PASS`, final line `ok      github.com/ratecap/core/limiter`

- [ ] **Step 7: Update `services/sidecar/proxy/priority.go` to import `Priority` from `limiter` instead of defining its own copy**

Replace the entire file with:

```go
package proxy

import "github.com/ratecap/core/limiter"

type Priority = limiter.Priority

const (
	Sheddable = limiter.Sheddable
	Critical  = limiter.Critical
)

func ResolvePriority(headerValue string, defaultPriority Priority) Priority {
	switch headerValue {
	case "critical":
		return Critical
	case "sheddable":
		return Sheddable
	default:
		return defaultPriority
	}
}
```

(`type Priority = limiter.Priority` is a type alias, not a new type — this keeps every existing caller of `proxy.Priority`/`proxy.Sheddable`/`proxy.Critical` compiling unchanged, including `proxy.go`'s `Handler{defaultPriority Priority}` field and `NewHandler`'s parameter, while making the underlying type identical to `limiter.Priority` so it can be assigned directly into a `limiter.Request.Priority` field in Task 5 with no conversion.)

**Note on cross-module import:** `services/sidecar` importing `github.com/ratecap/core/limiter` requires `services/sidecar/go.mod` to depend on the `github.com/ratecap/core` module. Check first:

Run: `grep -n "ratecap/core" services/sidecar/go.mod`

If no match, add the dependency and a local replace directive (matching this project's existing cross-module convention — check `services/sidecar/go.mod` for an existing `replace github.com/ratecap/proto => ../../proto`-style line first, then add the analogous one for core):

```bash
cd services/sidecar
go mod edit -replace github.com/ratecap/core=../core
go mod edit -require github.com/ratecap/core@v0.0.0
go mod tidy
cd ..
```

- [ ] **Step 8: Update `services/sidecar/proxy/priority_test.go`**

No changes needed — the test file references `proxy.ResolvePriority`, `proxy.Sheddable`, `proxy.Critical` exactly as before, and the type-alias in Step 7 keeps all of these names valid with identical behavior. Confirm by running the tests in Step 10.

- [ ] **Step 9: Rebuild the sidecar module to confirm the cross-module import resolves**

Run: `cd services/sidecar && go build ./... 2>&1`
Expected: no output, exit code 0

- [ ] **Step 10: Run the full sidecar test suite**

Run: `cd services/sidecar && go test ./... -race -v 2>&1 | tail -40`
Expected: PASS — every test including all 4 `TestResolvePriority_*` tests reports `--- PASS`, final line `ok` for `proxy`, `auth`, `shadow` packages

- [ ] **Step 11: gofmt check and commit**

Run: `gofmt -l services/core/limiter/limiter.go services/core/limiter/limiter_test.go services/core/limiter/concurrency.go services/core/limiter/concurrency_test.go services/sidecar/proxy/priority.go`
Expected: no output

```bash
git add services/core/limiter/limiter.go services/core/limiter/limiter_test.go services/core/limiter/concurrency.go services/core/limiter/concurrency_test.go services/sidecar/proxy/priority.go services/sidecar/go.mod services/sidecar/go.sum
git commit -m "feat(core,sidecar): move Priority to limiter, rename SkipConcurrencyLimit to SkipReservations

Priority becomes a cross-service concept once tier 3 needs it, so it
moves from sidecar-local to limiter (aliased back into proxy for
compatibility). SkipConcurrencyLimit is renamed now, ahead of tier 3
needing the identical bypass, since v1 has exactly two
reservation-issuing tiers forever — one flag, not one per tier."
```

---

### Task 2: `FleetShedder` — priority-dependent, globally-keyed concurrency limiter

**Files:**
- Create: `services/core/limiter/fleetshedder.go`
- Create: `services/core/limiter/fleetshedder_test.go`

**Interfaces:**
- Consumes: `limiter.Request.Priority`, `limiter.Sheddable`/`limiter.Critical` (Task 1); `concurrencyChecker` interface and `unboundedCap` constant (already defined in `concurrency.go`, reused as-is).
- Produces: `limiter.FleetShedder` implementing `limiter.Limiter`; `limiter.NewFleetShedder(s concurrencyChecker, cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool) *FleetShedder`; `(*FleetShedder) Reconfigure(cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool)`. Task 4's `main.go` wiring depends on these exact names.

- [ ] **Step 1: Write the failing tests**

Create `services/core/limiter/fleetshedder_test.go`:

```go
package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeFleetStore struct {
	mu      sync.Mutex
	tokens  map[string]int
	nextTok int
}

func newFakeFleetStore() *fakeFleetStore {
	return &fakeFleetStore{tokens: make(map[string]int)}
}

func (f *fakeFleetStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := f.tokens[key]
	if count >= cap {
		return false, "", nil
	}
	f.tokens[key] = count + 1
	f.nextTok++
	return true, string(rune('a' + f.nextTok)), nil
}

func (f *fakeFleetStore) DecrConcurrent(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[key]--
	return nil
}

func TestFleetShedder_UsesFixedGlobalKeyRegardlessOfRequestKey(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := l.Check(ctx, limiter.Request{Key: "user-2", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	count := fs.tokens["fleet"]
	perUserCountUser1 := fs.tokens["user-1"]
	fs.mu.Unlock()

	if count != 2 {
		t.Fatalf("expected both requests (different req.Key) to count toward the shared 'fleet' key, got fleet=%d", count)
	}
	if perUserCountUser1 != 0 {
		t.Fatalf("expected req.Key to never be used as the store key, got user-1=%d", perUserCountUser1)
	}
}

func TestFleetShedder_AllowsExactlyCapCriticalRequests(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 5, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
		if len(d.Reservations) != 1 || d.Reservations[0].Key != "fleet" || d.Reservations[0].Token == "" {
			t.Fatalf("request %d: expected reservation {fleet, <non-empty token>}, got %+v", i, d.Reservations)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("6th critical request: expected REJECT_503 (full fleet cap of 5 exceeded), got %v", d.Action)
	}
}

func TestFleetShedder_ShedsSheddableAtReducedCapBeforeFullFleetCap(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("sheddable request %d: expected ALLOW (reduced cap is 10*(100-20)/100=8), got %v", i, d.Action)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("9th sheddable request: expected REJECT_503 (reduced cap of 8 exceeded, even though full fleet cap is 10), got %v", d.Action)
	}
}

func TestFleetShedder_CriticalStillSucceedsAfterSheddableCapExhausted(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable}); err != nil {
			t.Fatalf("unexpected error priming sheddable request %d: %v", i, err)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("critical request after 8 sheddable reservations: expected ALLOW (full fleet cap of 10 not yet exceeded), got %v", d.Action)
	}
}

func TestFleetShedder_ShadowModeReservesAndCoercesToShadowLog(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 1, 20, 30000, true)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Critical}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2", Priority: limiter.Critical})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over cap in shadow mode, got %v", d.Action)
	}
	if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
		t.Fatalf("expected a reserved token even in shadow mode, got %+v", d.Reservations)
	}
}

func TestFleetShedder_SkipReservationsBypassesEntirely(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 1, 20, 30000, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", SkipReservations: true, Priority: limiter.Critical})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW when SkipReservations is set, got %v", i, d.Action)
		}
		if len(d.Reservations) != 0 {
			t.Fatalf("request %d: expected no reservation when SkipReservations is set, got %+v", i, d.Reservations)
		}
	}

	fs.mu.Lock()
	count := fs.tokens["fleet"]
	fs.mu.Unlock()

	if count != 0 {
		t.Fatalf("expected the store to never be touched when SkipReservations is set, got fleet=%d, want 0", count)
	}
}

func TestFleetShedder_ConcurrentCheckAndReconfigureIsRaceFree(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			priority := limiter.Sheddable
			if n%2 == 0 {
				priority = limiter.Critical
			}
			_, _ = l.Check(ctx, limiter.Request{Key: "user-race", Priority: priority})
		}(i)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Reconfigure(10, 20, 30000, n%2 == 0)
		}(i)
	}
	wg.Wait()
}

func TestFleetShedder_ReconfigureChangesReservedPct(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 90, 30000, false)
	ctx := context.Background()

	for i := 0; i < 1; i++ {
		if _, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable}); err != nil {
			t.Fatalf("unexpected error priming request %d: %v", i, err)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("expected REJECT_503 with reservedCriticalPct=90 (sheddable cap=10*(100-90)/100=1), got %v", d.Action)
	}

	l.Reconfigure(10, 0, 30000, false)

	d, err = l.Check(ctx, limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW after reconfiguring reservedCriticalPct=0 (sheddable cap=10*(100-0)/100=10, only 1 reservation exists), got %v", d.Action)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/core && go test ./limiter/... -run FleetShedder 2>&1 | head -20`
Expected: FAIL — compile error, `limiter.NewFleetShedder` / `limiter.FleetShedder` don't exist yet

- [ ] **Step 3: Write `services/core/limiter/fleetshedder.go`**

```go
package limiter

import (
	"context"
	"sync"
)

const fleetKey = "fleet"

type FleetShedder struct {
	store concurrencyChecker

	mu                  sync.RWMutex
	cap                 int
	reservedCriticalPct int
	maxDurationMs       int64
	shadowMode          bool
}

func NewFleetShedder(s concurrencyChecker, cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool) *FleetShedder {
	return &FleetShedder{store: s, cap: cap, reservedCriticalPct: reservedCriticalPct, maxDurationMs: maxDurationMs, shadowMode: shadowMode}
}

func (l *FleetShedder) Reconfigure(cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.reservedCriticalPct = reservedCriticalPct
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *FleetShedder) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, pct, maxDurationMs, shadowMode := l.cap, l.reservedCriticalPct, l.maxDurationMs, l.shadowMode
	l.mu.RUnlock()

	effectiveCap := cap
	if req.Priority != Critical {
		effectiveCap = cap * (100 - pct) / 100
	}

	allowed, token, err := l.store.IncrConcurrent(ctx, fleetKey, effectiveCap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: fleetKey, Token: token}}}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, fleetKey, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: fleetKey, Token: reservedToken}}}, nil
	}

	return Decision{Action: REJECT_503}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -v 2>&1 | tail -80`
Expected: PASS — all `TestFleetShedder_*` tests plus every pre-existing test report `--- PASS`, final line `ok      github.com/ratecap/core/limiter`

- [ ] **Step 5: gofmt check and commit**

Run: `gofmt -l services/core/limiter/fleetshedder.go services/core/limiter/fleetshedder_test.go`
Expected: no output

```bash
git add services/core/limiter/fleetshedder.go services/core/limiter/fleetshedder_test.go
git commit -m "feat(core): add FleetShedder — priority-dependent, globally-keyed concurrency limiter

Reuses ConcurrencyLimiter's exact IncrConcurrent/DecrConcurrent
mechanism against a fixed 'fleet' key instead of req.Key. Critical
requests are checked against the full fleet cap; sheddable requests
against a reduced cap (cap*(1-reservedCriticalPct/100)), so critical
traffic always has reserved room without being literally unbounded."
```

---

### Task 3: Proto — add `priority` field, rename `skip_concurrency_limit` to `skip_reservations`

**Files:**
- Modify: `proto/ratecap/v1/ratecap.proto`
- Regenerate: `proto/ratecap/v1/ratecap.pb.go`, `proto/ratecap/v1/ratecap_grpc.pb.go`
- Modify: `services/core/grpcserver/server.go`
- Modify: `services/core/grpcserver/server_test.go`

**Interfaces:**
- Consumes: `limiter.Request.Priority`, `limiter.Sheddable`/`limiter.Critical`, `limiter.Request.SkipReservations` (Task 1).
- Produces: `ratecapv1.CheckRateLimitRequest.Priority` (new field, proto enum mirroring `limiter.Priority`), `ratecapv1.CheckRateLimitRequest.SkipReservations` (renamed from `SkipConcurrencyLimit`). Task 5's sidecar wiring depends on both.

- [ ] **Step 1: Write the failing test — `grpcserver` maps `limiter.Priority` to/from the proto request**

In `services/core/grpcserver/server_test.go`, replace `TestCheckRateLimit_PropagatesSkipConcurrencyLimitToPipeline` entirely with:

```go
func TestCheckRateLimit_PropagatesSkipReservationsToPipeline(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:              "user-1",
		Cost:             1,
		SkipReservations: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fl.lastReq.SkipReservations {
		t.Error("expected SkipReservations=true to propagate into limiter.Request")
	}
}

func TestCheckRateLimit_PropagatesCriticalPriorityToPipeline(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:      "user-1",
		Cost:     1,
		Priority: ratecapv1.Priority_CRITICAL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Critical {
		t.Errorf("expected Priority to map to limiter.Critical, got %v", fl.lastReq.Priority)
	}
}

func TestCheckRateLimit_DefaultPriorityMapsToSheddable(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fl.lastReq.Priority != limiter.Sheddable {
		t.Errorf("expected default/unset Priority to map to limiter.Sheddable, got %v", fl.lastReq.Priority)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail to compile**

Run: `cd services/core && go test ./grpcserver/... 2>&1 | head -20`
Expected: FAIL — compile error referencing `ratecapv1.Priority_CRITICAL` and `CheckRateLimitRequest.Priority`/`SkipReservations` not existing yet, and `SkipConcurrencyLimit` no longer existing on `limiter.Request` (Task 1 already renamed it, but the proto/grpcserver field copy still uses the old name)

- [ ] **Step 3: Update the proto contract**

Replace the entire contents of `proto/ratecap/v1/ratecap.proto` with:

```protobuf
syntax = "proto3";

package ratecap.v1;

option go_package = "github.com/ratecap/proto/ratecap/v1;ratecapv1";

service RatecapService {
  rpc CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse);
  rpc ReleaseConcurrency(ReleaseConcurrencyRequest) returns (ReleaseConcurrencyResponse);
}

enum Action {
  ALLOW = 0;
  REJECT_429 = 1;
  REJECT_503 = 2;
  SHADOW_LOG = 3;
}

enum Priority {
  SHEDDABLE = 0;
  CRITICAL = 1;
}

message TokenReservation {
  string key = 1;
  string token = 2;
}

message CheckRateLimitRequest {
  string key = 1;
  int32 cost = 2;
  bool skip_reservations = 3;
  Priority priority = 4;
}

message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  repeated TokenReservation reservations = 3;
}

message ReleaseConcurrencyRequest {
  string key = 1;
  string concurrency_token = 2;
}

message ReleaseConcurrencyResponse {}
```

(`skip_concurrency_limit` field 3, `bool` → renamed to `skip_reservations`, same field number 3, same type — a pure rename, wire-compatible with itself since only the Go-generated identifier changes. `priority` is a new field, number 4.)

- [ ] **Step 4: Regenerate the Go proto bindings**

Run (from repo root):

```bash
PATH="$(go env GOPATH)/bin:$PATH" protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
echo "exit: $?"
```

Expected: `exit: 0`

Verify: `grep -n "Priority_CRITICAL\|SkipReservations" proto/ratecap/v1/ratecap.pb.go`
Expected: at least 2 matches

- [ ] **Step 5: Build the proto module**

Run: `cd proto && go build ./... && cd ..`
Expected: no output, exit code 0

- [ ] **Step 6: Update `services/core/grpcserver/server.go`'s field copy**

Replace the `CheckRateLimit` method's `limiter.Request{...}` construction:

```go
	decision, err := s.pipeline.Check(ctx, limiter.Request{
		Key:                  req.Key,
		Cost:                 int(req.Cost),
		SkipConcurrencyLimit: req.SkipConcurrencyLimit,
	})
```

with:

```go
	priority := limiter.Sheddable
	if req.Priority == ratecapv1.Priority_CRITICAL {
		priority = limiter.Critical
	}

	decision, err := s.pipeline.Check(ctx, limiter.Request{
		Key:              req.Key,
		Cost:             int(req.Cost),
		SkipReservations: req.SkipReservations,
		Priority:         priority,
	})
```

- [ ] **Step 7: Run grpcserver tests to verify they pass**

Run: `cd services/core && go test ./grpcserver/... -race -v 2>&1 | tail -60`
Expected: PASS — all tests including the 3 new/replaced ones report `--- PASS`, final line `ok      github.com/ratecap/core/grpcserver`

- [ ] **Step 8: Run the full core module test suite**

Run: `cd services/core && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `config`, `grpcserver`, `limiter` (`store` needs Docker — a Docker-connectivity failure there is unrelated to this task)

- [ ] **Step 9: gofmt check and commit**

Run: `gofmt -l proto/ratecap/v1/ratecap.proto services/core/grpcserver/server.go services/core/grpcserver/server_test.go`
Expected: no output (note: `gofmt` does not check `.proto` files — this command still checks the two `.go` files; the `.proto` file has no linter run against it in this project)

```bash
git add proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go
git commit -m "feat(proto,core): add Priority to the wire contract, rename skip_concurrency_limit

Priority (SHEDDABLE=0, CRITICAL=1) lets the sidecar's already-built
ResolvePriority() finally influence tier 3's decision. Renames
skip_concurrency_limit to skip_reservations at the same time, since
both are v1's final, permanent shape for these two concepts."
```

---

### Task 4: Wire `FleetShedder` into the pipeline + config

**Files:**
- Modify: `services/core/config/config.go`
- Modify: `services/core/config/config_test.go`
- Modify: `services/core/main.go`

**Interfaces:**
- Consumes: `limiter.NewFleetShedder`, `(*FleetShedder).Reconfigure` (Task 2).
- Produces: `config.FleetShedderConfig`; `Config.Tiers.FleetShedder`. Task 7's `deploy/ratecap.yaml` update depends on this exact YAML shape.

- [ ] **Step 1: Write the failing test — config parses a `fleet_shedder` block**

In `services/core/config/config_test.go`, add after `TestLoad_ParsesConcurrencyLimiterTier`:

```go
func TestLoad_ParsesFleetShedderTier(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 100
    reserved_critical_pct: 20
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Tiers.FleetShedder.DefaultMaxConcurrent != 100 {
		t.Errorf("expected DefaultMaxConcurrent=100, got %d", cfg.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if cfg.Tiers.FleetShedder.ReservedCriticalPct != 20 {
		t.Errorf("expected ReservedCriticalPct=20, got %d", cfg.Tiers.FleetShedder.ReservedCriticalPct)
	}
	if cfg.Tiers.FleetShedder.MaxRequestDurationMs != 30000 {
		t.Errorf("expected MaxRequestDurationMs=30000, got %d", cfg.Tiers.FleetShedder.MaxRequestDurationMs)
	}
	if cfg.Tiers.FleetShedder.DefaultPriority != "sheddable" {
		t.Errorf("expected DefaultPriority=%q, got %q", "sheddable", cfg.Tiers.FleetShedder.DefaultPriority)
	}
	if cfg.Tiers.FleetShedder.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.FleetShedder.ShadowMode)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/core && go test ./config/... -run TestLoad_ParsesFleetShedderTier 2>&1 | head -20`
Expected: FAIL — compile error, `cfg.Tiers.FleetShedder` doesn't exist yet

- [ ] **Step 3: Add `FleetShedderConfig` to `services/core/config/config.go`**

Replace the entire file with:

```go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RateLimiterConfig struct {
	DefaultRate  int  `yaml:"default_rate"`
	DefaultBurst int  `yaml:"default_burst"`
	ShadowMode   bool `yaml:"shadow_mode"`
}

type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
}

type FleetShedderConfig struct {
	DefaultMaxConcurrent int    `yaml:"default_max_concurrent"`
	ReservedCriticalPct  int    `yaml:"reserved_critical_pct"`
	MaxRequestDurationMs int64  `yaml:"max_request_duration_ms"`
	DefaultPriority      string `yaml:"default_priority"`
	ShadowMode           bool   `yaml:"shadow_mode"`
}

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter        RateLimiterConfig        `yaml:"rate_limiter"`
		ConcurrencyLimiter ConcurrencyLimiterConfig `yaml:"concurrency_limiter"`
		FleetShedder       FleetShedderConfig       `yaml:"fleet_shedder"`
	} `yaml:"tiers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return &cfg, nil
}
```

- [ ] **Step 4: Run config tests to verify they pass**

Run: `cd services/core && go test ./config/... -race -v 2>&1 | tail -30`
Expected: PASS — all tests including `TestLoad_ParsesFleetShedderTier` report `--- PASS`, final line `ok      github.com/ratecap/core/config`

- [ ] **Step 5: Wire `FleetShedder` into `services/core/main.go`'s pipeline**

Replace the block from `concurrencyLimiter := limiter.NewConcurrencyLimiter(` through `pipeline := limiter.NewPipeline(rateLimiter, concurrencyLimiter)` with:

```go
	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
	)

	fleetShedder := limiter.NewFleetShedder(
		redisStore,
		cfg.Tiers.FleetShedder.DefaultMaxConcurrent,
		cfg.Tiers.FleetShedder.ReservedCriticalPct,
		cfg.Tiers.FleetShedder.MaxRequestDurationMs,
		cfg.Tiers.FleetShedder.ShadowMode,
	)

	pipeline := limiter.NewPipeline(rateLimiter, concurrencyLimiter, fleetShedder)
```

Then replace the `config.Watch` callback:

```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
	})
```

with:

```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
```

- [ ] **Step 6: Build core to confirm main.go compiles**

Run: `cd services/core && go build ./... 2>&1`
Expected: no output, exit code 0

- [ ] **Step 7: Run the full core test suite**

Run: `cd services/core && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `config`, `grpcserver`, `limiter` (`store` needs Docker, unrelated to this task)

- [ ] **Step 8: gofmt check and commit**

Run: `gofmt -l services/core/config/config.go services/core/config/config_test.go services/core/main.go`
Expected: no output

```bash
git add services/core/config/config.go services/core/config/config_test.go services/core/main.go
git commit -m "feat(core): wire FleetShedder as the pipeline's third tier

FleetShedderConfig mirrors ConcurrencyLimiterConfig's shape, adding
reserved_critical_pct and default_priority. Hot-reloadable via the
same Reconfigure pattern every tier already uses."
```

---

### Task 5: Sidecar — load-bearing priority, renamed skip param, multi-reservation headers

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify: `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: `ratecapv1.CheckRateLimitRequest.Priority`/`.SkipReservations` (Task 3); `proxy.Priority`/`proxy.Sheddable`/`proxy.Critical` (Task 1, aliased from `limiter`).
- Produces: `/check` sets indexed `Concurrency-Token-N`/`Concurrency-Key-N` headers for every reservation in the response. Task 6's SDK work depends on this exact header naming scheme.

- [ ] **Step 1: Write the failing tests**

In `services/sidecar/proxy/proxy_test.go`, replace `TestServeHTTP_SkipConcurrencyParamSetsSkipConcurrencyLimitOnRequest` and `TestServeHTTP_NoSkipConcurrencyParamLeavesSkipConcurrencyLimitFalse` entirely with:

```go
func TestServeHTTP_SkipReservationsParamSetsSkipReservationsOnRequest(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&skip_reservations=true", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if !client.lastReq.SkipReservations {
		t.Error("expected SkipReservations=true when skip_reservations=true query param is set")
	}
}

func TestServeHTTP_NoSkipReservationsParamLeavesSkipReservationsFalse(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.SkipReservations {
		t.Error("expected SkipReservations=false when skip_reservations param is absent")
	}
}
```

Replace `TestServeHTTP_SetsConcurrencyTokenAndKeyHeadersWhenReservationPresent` and `TestServeHTTP_OmitsConcurrencyHeadersWhenNoReservations` entirely with:

```go
func TestServeHTTP_SetsIndexedConcurrencyHeadersForEachReservation(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{
		Action: ratecapv1.Action_ALLOW,
		Reservations: []*ratecapv1.TokenReservation{
			{Key: "user-1", Token: "tok-abc"},
			{Key: "fleet", Token: "tok-xyz"},
		},
	}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token-0") != "tok-abc" {
		t.Errorf("expected Concurrency-Token-0 %q, got %q", "tok-abc", rec.Header().Get("Concurrency-Token-0"))
	}
	if rec.Header().Get("Concurrency-Key-0") != "user-1" {
		t.Errorf("expected Concurrency-Key-0 %q, got %q", "user-1", rec.Header().Get("Concurrency-Key-0"))
	}
	if rec.Header().Get("Concurrency-Token-1") != "tok-xyz" {
		t.Errorf("expected Concurrency-Token-1 %q, got %q", "tok-xyz", rec.Header().Get("Concurrency-Token-1"))
	}
	if rec.Header().Get("Concurrency-Key-1") != "fleet" {
		t.Errorf("expected Concurrency-Key-1 %q, got %q", "fleet", rec.Header().Get("Concurrency-Key-1"))
	}
	if rec.Header().Get("Concurrency-Token-2") != "" {
		t.Errorf("expected no Concurrency-Token-2 header (only 2 reservations), got %q", rec.Header().Get("Concurrency-Token-2"))
	}
}

func TestServeHTTP_OmitsIndexedConcurrencyHeadersWhenNoReservations(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token-0") != "" {
		t.Errorf("expected no Concurrency-Token-0 header, got %q", rec.Header().Get("Concurrency-Token-0"))
	}
	if rec.Header().Get("Concurrency-Key-0") != "" {
		t.Errorf("expected no Concurrency-Key-0 header, got %q", rec.Header().Get("Concurrency-Key-0"))
	}
}
```

Add a new test proving priority is actually threaded through, after `TestServeHTTP_ParsesPriorityHeaderWithoutError` (which stays as-is — it already pins the tier-1-ignores-it behavior and remains true since tier 1 still ignores `Priority`):

```go
func TestServeHTTP_ThreadsCriticalPriorityHeaderIntoRequest(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	req.Header.Set("x-ratecap-priority", "critical")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_CRITICAL {
		t.Errorf("expected Priority_CRITICAL on the outgoing request, got %v", client.lastReq.Priority)
	}
}

func TestServeHTTP_DefaultsToSheddablePriorityWhenNoHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq == nil {
		t.Fatal("expected CheckRateLimit to be called")
	}
	if client.lastReq.Priority != ratecapv1.Priority_SHEDDABLE {
		t.Errorf("expected Priority_SHEDDABLE by default, got %v", client.lastReq.Priority)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... 2>&1 | head -30`
Expected: FAIL — compile error (`ratecapv1.Priority_CRITICAL` not yet imported/used correctly, `SkipReservations` not yet set) or assertion failures on the old `skip_concurrency`/plain-header behavior

- [ ] **Step 3: Update `services/sidecar/proxy/proxy.go`**

Replace `Handler.ServeHTTP` entirely with:

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
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

	skipReservations := r.URL.Query().Get("skip_reservations") == "true"

	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{
		Key:              key,
		Cost:             1,
		SkipReservations: skipReservations,
		Priority:         protoPriority,
	})
	if err != nil {
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}

	for i, reservation := range resp.Reservations {
		w.Header().Set(fmt.Sprintf("Concurrency-Token-%d", i), reservation.Token)
		w.Header().Set(fmt.Sprintf("Concurrency-Key-%d", i), reservation.Key)
	}

	action := resp.Action
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}
```

Add `"fmt"` to the import block:

```go
import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
)
```

`ReleaseHandler.ServeHTTP` is unchanged — it already takes an arbitrary `(key, token)` pair per call; Task 6's SDK change is what makes it call `/release` once per reservation instead of once per `Ticket`.

- [ ] **Step 4: Run proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -80`
Expected: PASS — every test including all new/replaced ones reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`

- [ ] **Step 5: Run the full sidecar test suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `auth`, `proxy`, `shadow`

- [ ] **Step 6: gofmt check and commit**

Run: `gofmt -l services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go`
Expected: no output

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): thread priority into requests, generalize to N reservation headers

ResolvePriority()'s result was parsed and discarded since the
walking skeleton — now load-bearing via CheckRateLimitRequest.Priority.
/check now emits indexed Concurrency-Token-N/Concurrency-Key-N
headers for every reservation in the response (was: only the
first), since tier 3 means a single request can now produce two
reservations (tier 2 per-user + tier 3 global)."
```

---

### Task 6: SDK — multi-reservation `Ticket`, renamed skip param

**Files:**
- Modify: `packages/sdks/go/client.go`
- Modify: `packages/sdks/go/client_test.go`

**Interfaces:**
- Consumes: indexed `Concurrency-Token-N`/`Concurrency-Key-N` response headers (Task 5).
- Produces: `Ticket.Release(ctx) error` releases every reservation the `Ticket` holds, not just one. No new exported symbols — `Client`, `Ticket`, `Allow`, `Acquire` signatures are unchanged.

- [ ] **Step 1: Write the failing tests**

In `packages/sdks/go/client_test.go`, replace `TestAllow_RequestsSkipConcurrencyLimit` and `TestAcquire_DoesNotRequestSkipConcurrencyLimit` entirely with:

```go
func TestAllow_RequestsSkipReservations(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, err := client.Allow(context.Background(), "user-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("skip_reservations"); got != "true" {
		t.Errorf("expected skip_reservations=true on Allow()'s /check request, got %q", got)
	}
}

func TestAcquire_DoesNotRequestSkipReservations(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, err := client.Acquire(context.Background(), "user-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("skip_reservations"); got != "" {
		t.Errorf("expected no skip_reservations param on Acquire()'s /check request, got %q", got)
	}
}
```

Replace `TestAcquire_ReturnsAllowedTicketOn200` entirely with:

```go
func TestAcquire_ReturnsAllowedTicketOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Concurrency-Token-0", "tok-abc")
		w.Header().Set("Concurrency-Key-0", "user-1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ticket.Allowed {
		t.Error("expected Allowed=true on 200 response")
	}
}
```

Replace `TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey` entirely with:

```go
func TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "server-assigned-key")
			w.WriteHeader(http.StatusOK)
		case "/release":
			capturedQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "caller-supplied-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error releasing: %v", err)
	}

	if capturedQuery == nil {
		t.Fatal("expected /release to be called")
	}
	if got := capturedQuery.Get("key"); got != "server-assigned-key" {
		t.Errorf("expected key=server-assigned-key (from Concurrency-Key-0 header, not the caller's Acquire key), got %q", got)
	}
	if got := capturedQuery.Get("token"); got != "tok-abc" {
		t.Errorf("expected token=tok-abc, got %q", got)
	}
}

func TestTicket_Release_ReleasesEveryReservation(t *testing.T) {
	var releaseCalls []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "user-1")
			w.Header().Set("Concurrency-Token-1", "tok-xyz")
			w.Header().Set("Concurrency-Key-1", "fleet")
			w.WriteHeader(http.StatusOK)
		case "/release":
			releaseCalls = append(releaseCalls, r.URL.Query())
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error releasing: %v", err)
	}

	if len(releaseCalls) != 2 {
		t.Fatalf("expected 2 /release calls (one per reservation), got %d", len(releaseCalls))
	}

	byKey := map[string]string{}
	for _, q := range releaseCalls {
		byKey[q.Get("key")] = q.Get("token")
	}
	if byKey["user-1"] != "tok-abc" {
		t.Errorf("expected a release call for key=user-1 token=tok-abc, got %+v", byKey)
	}
	if byKey["fleet"] != "tok-xyz" {
		t.Errorf("expected a release call for key=fleet token=tok-xyz, got %+v", byKey)
	}
}
```

Replace `TestTicket_Release_ReturnsErrorOnNon200FromSidecar` entirely with:

```go
func TestTicket_Release_ReturnsErrorOnNon200FromSidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token-0", "tok-abc")
			w.Header().Set("Concurrency-Key-0", "user-1")
			w.WriteHeader(http.StatusOK)
		case "/release":
			http.Error(w, "upstream release failed", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err == nil {
		t.Fatal("expected error when sidecar returns non-200 from /release")
	}
}
```

`TestTicket_Release_NoOpWhenNoTokenWasIssued` is unchanged (no headers means zero reservations parsed, `Release()` still must be a no-op) — confirm it still passes in Step 4 below.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd packages/sdks/go && go test ./... 2>&1 | head -30`
Expected: FAIL — compile error or assertion failures against the old single-`Concurrency-Token`-header behavior

- [ ] **Step 3: Update `packages/sdks/go/client.go`**

Replace `Allow`'s URL construction line:

```go
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key) + "&skip_concurrency=true"
```

with:

```go
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key) + "&skip_reservations=true"
```

Replace the `Ticket` type and `Release` method:

```go
type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client *Client
	key    string
	tok    string
}

// Release is best-effort with no retry: a non-nil error is a signal for the
// caller to log, not something to retry or otherwise act on — the design
// spec's Lua reaper (max_request_duration_ms) is the actual mechanism that
// frees a slot after a lost or failed Release, not a fallback for one.
func (t *Ticket) Release(ctx context.Context) error {
	if t.tok == "" {
		return nil
	}

	params := url.Values{}
	params.Set("key", t.key)
	params.Set("token", t.tok)
	reqURL := t.client.sidecarAddr + "/release?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: release failed with status %d", resp.StatusCode)
	}

	return nil
}
```

with:

```go
type reservation struct {
	key string
	tok string
}

type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client       *Client
	reservations []reservation
}

// Release is best-effort with no retry, releasing every reservation the
// Ticket holds (a single Acquire can produce more than one — e.g. tier 2's
// per-user slot and tier 3's global slot): a non-nil error is a signal for
// the caller to log, not something to retry or otherwise act on — the
// design spec's Lua reaper (max_request_duration_ms) is the actual
// mechanism that frees a slot after a lost or failed Release, not a
// fallback for one, for every reservation individually.
func (t *Ticket) Release(ctx context.Context) error {
	var errs []error
	for _, r := range t.reservations {
		if err := t.releaseOne(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *Ticket) releaseOne(ctx context.Context, r reservation) error {
	params := url.Values{}
	params.Set("key", r.key)
	params.Set("token", r.tok)
	reqURL := t.client.sidecarAddr + "/release?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: release failed with status %d", resp.StatusCode)
	}

	return nil
}
```

Replace `Acquire`'s body from `concurrencyTok := resp.Header.Get("Concurrency-Token")` through its final `return &Ticket{...}`:

```go
	concurrencyTok := resp.Header.Get("Concurrency-Token")
	concurrencyKey := resp.Header.Get("Concurrency-Key")

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, key: concurrencyKey, tok: concurrencyTok}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, key: concurrencyKey, tok: concurrencyTok}, nil
```

with:

```go
	var reservations []reservation
	for i := 0; ; i++ {
		tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
		if tok == "" {
			break
		}
		key := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
		reservations = append(reservations, reservation{key: key, tok: tok})
	}

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, reservations: reservations}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, reservations: reservations}, nil
```

Add `"errors"` to the import block:

```go
import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)
```

- [ ] **Step 4: Run SDK tests to verify they pass**

Run: `cd packages/sdks/go && go test ./... -race -v 2>&1 | tail -80`
Expected: PASS — every test including all new/replaced ones (`TestTicket_Release_ReleasesEveryReservation`, `TestTicket_Release_NoOpWhenNoTokenWasIssued`, etc.) reports `--- PASS`, final line `ok      github.com/ratecap/sdk-go`

- [ ] **Step 5: Rebuild every module together**

Run: `(cd services/core && go build ./...) && (cd services/sidecar && go build ./...) && (cd packages/sdks/go && go build ./...) && (cd deploy/sampleapp && go build ./...)`
Expected: no output, exit code 0 for each

- [ ] **Step 6: gofmt check and commit**

Run: `gofmt -l packages/sdks/go/client.go packages/sdks/go/client_test.go`
Expected: no output

```bash
git add packages/sdks/go/client.go packages/sdks/go/client_test.go
git commit -m "feat(sdk): release every reservation a Ticket holds, rename skip param

Acquire() can now return a Ticket carrying 2 reservations (tier 2's
per-user slot + tier 3's global slot). Release() releases each one
independently, joining any errors — a failure releasing one
reservation no longer silently drops the others; the reaper remains
the resilience backstop for any that fail."
```

---

### Task 7: Demo config, sample app, full end-to-end verification

**Files:**
- Modify: `deploy/ratecap.yaml`
- Modify: `deploy/sampleapp/main.go`

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces: a live-verified demo proving Tier 3 trips correctly under a mixed critical/sheddable load, alongside Tier 1/2 regression checks.

- [ ] **Step 1: Add a `fleet_shedder` block to `deploy/ratecap.yaml`**

Replace the entire file with:

```yaml
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 2
    default_burst: 5
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 3
    max_request_duration_ms: 30000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 5
    reserved_critical_pct: 40
    max_request_duration_ms: 30000
    default_priority: sheddable
    shadow_mode: false
```

(`default_max_concurrent: 5`, `reserved_critical_pct: 40` means: full fleet cap is 5, sheddable's effective cap is `5*(100-40)/100 = 3`. A burst of 5 sheddable-priority requests should show exactly 3×200 then 2×503; a burst of 5 critical-priority requests should show all 5×200.)

- [ ] **Step 2: Add a `/fleet-demo` handler to `deploy/sampleapp/main.go` demonstrating the critical/sheddable split**

Replace the entire file with:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	ratecap "github.com/ratecap/sdk-go"
)

var fleetDemoCounter atomic.Int64

func main() {
	sidecarAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if sidecarAddr == "" {
		sidecarAddr = "http://localhost:8080"
	}

	client := ratecap.NewClient(sidecarAddr)
	sidecarBase := sidecarAddr

	http.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		allowed, retryAfterMs, err := client.Allow(ctx, "demo-user")
		if err != nil {
			http.Error(w, "rate limit check failed", http.StatusInternalServerError)
			return
		}

		if !allowed {
			w.Header().Set("Retry-After-Ms", fmt.Sprintf("%d", retryAfterMs))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}

		fmt.Fprintln(w, "checkout processed")
	})

	http.HandleFunc("/slow-report", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		ticket, err := client.Acquire(ctx, "demo-user-reports")
		if err != nil {
			http.Error(w, "concurrency check failed", http.StatusInternalServerError)
			return
		}
		defer ticket.Release(ctx)

		if !ticket.Allowed {
			w.Header().Set("Retry-After-Ms", fmt.Sprintf("%d", ticket.RetryAfterMs))
			http.Error(w, "too many concurrent reports in flight", http.StatusTooManyRequests)
			return
		}

		time.Sleep(2 * time.Second)
		fmt.Fprintln(w, "report generated")
	})

	http.HandleFunc("/fleet-demo", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		priority := r.URL.Query().Get("priority")

		// A fresh key per request keeps tier 1 (per-key token bucket) and
		// tier 2 (per-key concurrency cap) from ever tripping here — this
		// endpoint exists to demonstrate tier 3 specifically, which ignores
		// req.Key entirely and checks a single shared "fleet" key instead,
		// so every request's tier-3 reservation still accumulates into one
		// shared count regardless of each request using a distinct key.
		key := fmt.Sprintf("fleet-demo-%d", fleetDemoCounter.Add(1))

		checkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarBase+"/check?key="+url.QueryEscape(key), nil)
		if err != nil {
			http.Error(w, "request construction failed", http.StatusInternalServerError)
			return
		}
		if priority == "critical" {
			checkReq.Header.Set("x-ratecap-priority", "critical")
		} else {
			checkReq.Header.Set("x-ratecap-priority", "sheddable")
		}

		resp, err := http.DefaultClient.Do(checkReq)
		if err != nil {
			http.Error(w, "fleet check failed", http.StatusInternalServerError)
			return
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "shed (priority=%s)\n", priority)
			return
		}

		var releaseParams []url.Values
		for i := 0; ; i++ {
			tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
			if tok == "" {
				break
			}
			resKey := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
			params := url.Values{}
			params.Set("key", resKey)
			params.Set("token", tok)
			releaseParams = append(releaseParams, params)
		}
		resp.Body.Close()

		time.Sleep(2 * time.Second)

		for _, params := range releaseParams {
			releaseReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, sidecarBase+"/release?"+params.Encode(), nil)
			if err != nil {
				continue
			}
			if relResp, err := http.DefaultClient.Do(releaseReq); err == nil {
				relResp.Body.Close()
			}
		}

		fmt.Fprintf(w, "fleet request processed (priority=%s)\n", priority)
	})

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}
```

`/fleet-demo` deliberately calls the sidecar's `/check`/`/release` directly via `http.DefaultClient` rather than through the Go SDK, since demonstrating the raw `x-ratecap-priority` header mechanism — which any language's client would use, not just Go's SDK — is more illustrative for a demo than adding SDK-level priority support that no other part of this plan requires. Every successful check releases **all** of its reservations before responding (typically two: tier 2's per-key slot on this request's unique key, and tier 3's shared "fleet" slot) — without this, tier 3's global count would only clear via the 30-second reaper, and the very next verification step below would run against a corrupted, non-zero starting count.

- [ ] **Step 3: Confirm Docker is reachable**

Docker has been observed to go unreachable intermittently in this environment (confirmed down as of this worktree's baseline check). Run:

```bash
docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

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

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`. If `core` or `sidecar` exits immediately, check `docker compose logs core sidecar` — most likely cause is a config-parsing error from the new `fleet_shedder` YAML block; re-check Step 1's indentation against the exact YAML above.

- [ ] **Step 5: Re-verify Tier 1 (regression check)**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```

Expected: exactly 5 lines `checkout 200` followed by 2 lines `checkout 429`.

- [ ] **Step 6: Re-verify Tier 2 (regression check)**

Run:

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "slow-report %{http_code}\n" http://localhost:3000/slow-report & done
wait
```

Expected: exactly 3 lines `slow-report 200` and 2 lines `slow-report 429`.

- [ ] **Step 7: Verify Tier 3 sheds sheddable-priority traffic at the reduced cap**

Run:

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo(sheddable) %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
wait
```

Expected: exactly 3 lines `fleet-demo(sheddable) 200` and 2 lines `fleet-demo(sheddable) 503` — matching `default_max_concurrent: 5` and `reserved_critical_pct: 40` (`5*(100-40)/100 = 3`).

- [ ] **Step 8: Verify Tier 3 lets critical-priority traffic through up to the full fleet cap**

Step 7's `wait` already blocked until every background call's HTTP response was sent, and `/fleet-demo` only responds to a successful check after its release call completes — so the fleet count is already back to 0 by the time Step 7 finished; no extra wait is needed here. Run:

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo(critical) %{http_code}\n" "http://localhost:3000/fleet-demo?priority=critical" & done
wait
```

Expected: all 5 lines `fleet-demo(critical) 200` — critical traffic is checked against the full fleet cap of 5, not the reduced sheddable cap of 3.

- [ ] **Step 9: Verify a mixed burst — critical traffic keeps succeeding while sheddable reservations are still held**

The critical request must fire *while* the sheddable requests are still holding their reservations (during their 2-second hold, before they release) — not after they've already completed — to actually prove reserved capacity under contention. Run:

```bash
for i in 1 2 3; do curl -s -o /dev/null -w "fleet-demo(sheddable) %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
sleep 0.3
curl -s -o /dev/null -w "fleet-demo(critical) %{http_code}\n" "http://localhost:3000/fleet-demo?priority=critical"
wait
```

Expected: all 3 sheddable calls succeed (3×200, filling the reduced cap of 3) and the critical call — fired 0.3s later, while all 3 sheddable reservations are still held during their 2-second hold — also succeeds (200), because critical is checked against the full fleet cap of 5 (count of 3 held + 1 new critical = 4, still under 5). This proves critical traffic has reserved room the 3 sheddable requests couldn't consume, under actual contention rather than after the fact.

- [ ] **Step 10: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 11: Run every module's full test suite one final time (regression check across the whole plan)**

Run:

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
```

Expected: `ok` for every package except `services/core/store` (Docker-dependent, unrelated to this task's changes).

- [ ] **Step 12: gofmt check and commit**

Run: `gofmt -l deploy/sampleapp/main.go`
Expected: no output

```bash
git add deploy/ratecap.yaml deploy/sampleapp/main.go
git commit -m "feat(deploy): demonstrate tier 3 via a priority-aware endpoint, verify end-to-end

/fleet-demo exercises the x-ratecap-priority header directly against
the sidecar, showing sheddable traffic shed at the reduced cap while
critical traffic keeps succeeding up to the full fleet cap. Live
e2e re-verified: tier 1 and tier 2 regressions pass unchanged, tier
3's critical/sheddable split behaves exactly as designed."
```

---

## Post-plan note

This completes Tier 3 (Fleet Usage Load Shedder) implementation. Per this project's established cycle (walking skeleton, Tier 2, Tier 2's two audit-remediation groups), the next step after this plan's tasks are all reviewed and merged is a full multi-aspect audit (live e2e + correctness + concurrency-safety + security + architecture lenses) before opening the PR into `develop` — the same process Tier 2 went through, which surfaced real, worth-fixing findings both times. Tier 4 (Worker Utilization Load Shedder) remains a separate future phase, out of scope here.
