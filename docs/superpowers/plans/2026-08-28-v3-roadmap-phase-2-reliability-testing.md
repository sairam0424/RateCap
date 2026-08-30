# RateCap v3 Roadmap — Phase 2: Reliability & Testing Hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** convert RateCap's core architectural claims (fail-open/fail-closed, atomicity, no-flapping) from assumptions into tested invariants, add coverage/mutation-testing CI gates, make Redis itself HA, and add a sub-second incident-response lever for on-call.

**Architecture:** Sixteen tasks spanning `services/core` (new tests + Sentinel-aware Redis client + a new admin RPC), `services/sidecar` (a new authenticated admin HTTP endpoint + gradual Tier 4 shed ramp), `proto` (one new RPC), `cli` (one new subcommand), `deploy/helm` (Redis→Sentinel), and `.github/workflows/ci.yml` (coverage + mutation-testing gates).

**Tech Stack:** Go 1.26; new test-only dependencies `github.com/testcontainers/testcontainers-go/modules/toxiproxy@v0.44.0` (matches the already-pinned `testcontainers-go v0.44.0`) and `pgregory.net/rapid` (property-based testing) in `services/core`; `github.com/go-gremlins/gremlins` (mutation testing, installed via `go install` in CI, not a go.mod dependency); `protoc-gen-go`/`protoc-gen-go-grpc` (already verified installable via `go install ...@latest` in this environment) for the one proto change.

**Spec:** `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 2 section (items 1–12).

## Global Constraints

- **Fail-open/closed scope (carried forward from Phase 1, 2026-08-28 decision):** Tier 1 (`TokenBucketLimiter`) fails OPEN on a store error; Tiers 2/3 (`ConcurrencyLimiter`, `FleetShedder`) fail CLOSED. This corrects the roadmap spec's Phase 2 item 1 wording, which assumed (incorrectly) that all three tiers fail open — they don't; Task 1 below tests the real, corrected behavior (`TestTier1_RedisUnavailable_FailsOpen`, `TestTier2_RedisUnavailable_FailsClosed`, `TestTier3_RedisUnavailable_FailsClosed`), not the spec's original three-fail-open framing.
- **Admin RPC auth (user decision, 2026-08-28):** the new incident-response admin lever (Task 12/13) requires a NEW, separate secret (`RATECAP_ADMIN_SECRET`) checked at the sidecar's HTTP layer, in addition to (not instead of) the existing sidecar→core shared-secret gRPC auth. This is deliberate defense-in-depth for a capability with fleet-wide, unbounded blast radius — do not reuse `RATECAP_SHARED_SECRET` for this, and do not skip the check pending Phase 3's mTLS work.
- **`-race` is mandatory** on every `go test` invocation touching `services/core` or `services/sidecar`.
- **Docker-dependent tests** (anything using `testcontainers-go`, including the new Toxiproxy-based tests) require real Docker in the execution environment. If Docker is unavailable, run `go build ./...` to prove compilation, note in the task report which tests could not execute and why — this is an environment limitation, not an implementation defect, and must not be treated as one by a reviewer.
- **Helm/Sentinel verification ceiling:** `helm lint` and `helm template` (already a CI job) catch template syntax errors and missing-value bugs, and must pass. Full Redis Sentinel failover behavior cannot be verified without a real multi-node cluster — Task 10 must say so explicitly in `ARCHITECTURE.md` rather than imply full verification occurred.
- **Mutation-testing scope is intentionally narrow** (Task 8): `services/core/limiter` and `packages/sdks/go` only — not the whole codebase. Mutation testing is slow; scoping it to the algorithmically dense tiers is a deliberate cost/value tradeoff, not an oversight.
- **No comments except non-obvious WHY**, matching the existing codebase's terse style.
- Files: 200-400 lines typical, 800 max.
- Never run `git push`, `git branch -D`, or any destructive git command — commit locally only.

---

### Task 1: Toxiproxy-based per-tier Redis-failure regression tests

**Files:**
- Create: `services/core/reliability/redis_failure_test.go` (new package, integration-only — separate from `store`/`limiter` unit tests since this drives the full `Pipeline` + real Redis + a real network fault, not an isolated unit)
- Modify: `services/core/go.mod`, `services/core/go.sum` (add `github.com/testcontainers/testcontainers-go/modules/toxiproxy@v0.44.0`)

**Interfaces:**
- Consumes: `store.NewRedisStore`, `limiter.NewTokenBucketLimiter`/`NewConcurrencyLimiter`/`NewFleetShedder`, `metrics.FailOpenTotal` (all from Phase 1).
- Produces: nothing new — this task only adds tests proving Phase 1's fail-open/closed behavior survives a *real* network partition to Redis, not just a mocked store error.

- [ ] **Step 1: Add the dependency**

Run from `services/core`:
```bash
go get github.com/testcontainers/testcontainers-go/modules/toxiproxy@v0.44.0
```

- [ ] **Step 2: Write the failing tests**

```go
// services/core/reliability/redis_failure_test.go
package reliability_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/toxiproxy"
	tcwait "github.com/testcontainers/testcontainers-go/wait"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/core/limiter"
	coremetrics "github.com/ratecap/core/metrics"
	"github.com/ratecap/core/store"
)

var testSigningKey = []byte("test-signing-key-do-not-use-in-production")

// redisBehindToxiproxy starts a real Redis container plus a Toxiproxy
// container proxying to it, and returns a client pointed at the proxy so
// the test can sever the connection on demand via toxiproxy's API without
// tearing down Redis itself — a closer match to a real network partition
// than closing the client connection directly.
func redisBehindToxiproxy(t *testing.T) (*redis.Client, func(cutConnection bool)) {
	ctx := context.Background()

	redisReq := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   tcwait.ForListeningPort("6379/tcp"),
		Networks:     []string{"toxiproxy-test-net"},
		NetworkAliases: map[string][]string{
			"toxiproxy-test-net": {"redis-target"},
		},
	}
	_, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: redisReq,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}

	toxiproxyContainer, err := toxiproxy.Run(ctx, "ghcr.io/shopify/toxiproxy:2.9.0", testcontainers.WithNetwork("toxiproxy-test-net", nil))
	if err != nil {
		t.Fatalf("failed to start toxiproxy container: %v", err)
	}
	t.Cleanup(func() { _ = toxiproxyContainer.Terminate(ctx) })

	proxy, err := toxiproxyContainer.NewProxy(ctx, "redis", "0.0.0.0:6379", "redis-target:6379")
	if err != nil {
		t.Fatalf("failed to create toxiproxy proxy: %v", err)
	}

	host, err := toxiproxyContainer.ProxiedAddr(ctx, proxy)
	if err != nil {
		t.Fatalf("failed to get proxied address: %v", err)
	}

	client := redis.NewClient(&redis.Options{Addr: host, DialTimeout: 500 * time.Millisecond, ReadTimeout: 500 * time.Millisecond})

	cut := func(cutConnection bool) {
		if cutConnection {
			if err := proxy.Disable(ctx); err != nil {
				t.Fatalf("failed to disable proxy: %v", err)
			}
		} else {
			if err := proxy.Enable(ctx); err != nil {
				t.Fatalf("failed to enable proxy: %v", err)
			}
		}
	}
	return client, cut
}

func TestTier1_RedisUnavailable_FailsOpen(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	tokenBucket := limiter.NewTokenBucketLimiter(s, 100, 500, false)

	cut(true)
	defer cut(false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	decision, err := tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if err != nil {
		t.Fatalf("expected Tier 1 to fail OPEN (no error) on a real network partition, got: %v", err)
	}
	if decision.Action != limiter.ALLOW {
		t.Errorf("expected Action=ALLOW on a real Redis outage, got %v", decision.Action)
	}
	if after != before+1 {
		t.Errorf("expected ratecap_fail_open_total{tier=rate_limiter} to increment, before=%v after=%v", before, after)
	}
}

func TestTier2_RedisUnavailable_FailsClosed(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	concurrencyLimiter := limiter.NewConcurrencyLimiter(s, 100, 60000, false, false, 0, 0, 0)

	cut(true)
	defer cut(false)

	_, err := concurrencyLimiter.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier2", Cost: 1})
	if err == nil {
		t.Fatal("expected Tier 2 to fail CLOSED (return an error) on a real network partition, got no error")
	}
}

func TestTier3_RedisUnavailable_FailsClosed(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	fleetShedder := limiter.NewFleetShedder(s, 100, 20, 60000, false)

	cut(true)
	defer cut(false)

	_, err := fleetShedder.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier3", Cost: 1, Priority: limiter.Sheddable})
	if err == nil {
		t.Fatal("expected Tier 3 to fail CLOSED (return an error) on a real network partition, got no error")
	}
}

func TestTier1_RedisRecovers_ResumesNormalOperation(t *testing.T) {
	client, cut := redisBehindToxiproxy(t)
	s := store.NewRedisStore(client, testSigningKey)
	tokenBucket := limiter.NewTokenBucketLimiter(s, 1, 1, false)

	cut(true)
	decision, err := tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1-recovery", Cost: 1})
	if err != nil || decision.Action != limiter.ALLOW {
		t.Fatalf("expected fail-open ALLOW while cut, got action=%v err=%v", decision.Action, err)
	}

	cut(false)
	time.Sleep(200 * time.Millisecond)

	decision, err = tokenBucket.Check(context.Background(), limiter.Request{Key: "toxiproxy-tier1-recovery-2", Cost: 1})
	if err != nil {
		t.Fatalf("expected a normal decision once Redis recovers, got error: %v", err)
	}
	if decision.Action != limiter.ALLOW {
		t.Errorf("expected a fresh key's first request to be ALLOW once Redis recovers, got %v", decision.Action)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd services/core && go test ./reliability/... -race -v`
Expected: FAIL — package doesn't exist yet, or (once created) the tests would fail against pre-Task-1 code only if Phase 1's fail-open logic weren't already in place. Since Phase 1 already shipped fail-open for Tier 1, `TestTier1_RedisUnavailable_FailsOpen` may pass immediately once the package compiles — that is expected; this task is about proving the behavior with a *real* fault injector, not introducing new behavior.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/core && go test ./reliability/... -race -v`
Expected: PASS (4 tests). Requires Docker (two containers per test: Redis + Toxiproxy, on a shared Docker network) — if unavailable, note in the report and rely on `go build ./...` to prove compilation.

- [ ] **Step 5: Commit**

```bash
git add services/core/reliability/redis_failure_test.go services/core/go.mod services/core/go.sum
git commit -m "test(core): add Toxiproxy-based per-tier Redis-failure regression tests"
```

---

### Task 2: Race regression test — priority-partition-at-capacity for Tier 2 and Tier 3

**Files:**
- Create: `services/core/limiter/priority_race_test.go`

**Interfaces:**
- Consumes: `limiter.NewConcurrencyLimiter`, `limiter.NewFleetShedder`, `store.NewRedisStore` (real Redis via testcontainers, matching `store/redis_test.go`'s existing `startRedis(t)` pattern — duplicate that helper locally since this is a different package).

- [ ] **Step 1: Write the failing tests**

```go
// services/core/limiter/priority_race_test.go
package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ratecap/core/limiter"
	"github.com/ratecap/core/store"
)

var raceTestSigningKey = []byte("test-signing-key-do-not-use-in-production")

func startRaceTestRedis(t *testing.T) *redis.Client {
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("failed to get mapped port: %v", err)
	}
	return redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
}

// TestConcurrencyLimiter_PriorityPartitionAtCapacityRace replicates the
// Netflix concurrency-limits bug class (PR #233/#234): many goroutines
// racing to acquire the last slots when the limit is already at (or just
// under) capacity. Tier 2 has no priority partitioning of its own (that's
// Tier 3's job) — this proves the plain at-capacity race is clean here too.
func TestConcurrencyLimiter_PriorityPartitionAtCapacityRace(t *testing.T) {
	client := startRaceTestRedis(t)
	s := store.NewRedisStore(client, raceTestSigningKey)
	const cap = 20
	l := limiter.NewConcurrencyLimiter(s, cap, 60000, false, false, 0, 0, 0)

	const goroutines = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := l.Check(context.Background(), limiter.Request{Key: "race-key", Cost: 1})
			if err != nil {
				return
			}
			if decision.Action == limiter.ALLOW {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed > cap {
		t.Errorf("expected at most %d allowed under concurrent load at capacity, got %d — a slot was double-issued", cap, allowed)
	}
}

// TestFleetShedder_MixedPriorityAtCapacityRace drives BOTH critical and
// sheddable traffic concurrently at exactly the effective-cap boundary for
// each partition, asserting neither partition's own effective cap is ever
// exceeded even under race — the property store/redis_test.go's
// TestIncrConcurrent_MixedPriorityConcurrentAtomicity already proves at the
// raw-store level; this proves it survives composition with FleetShedder's
// own effective-cap arithmetic.
func TestFleetShedder_MixedPriorityAtCapacityRace(t *testing.T) {
	client := startRaceTestRedis(t)
	s := store.NewRedisStore(client, raceTestSigningKey)
	const cap = 20
	const reservedCriticalPct = 50
	l := limiter.NewFleetShedder(s, cap, reservedCriticalPct, 60000, false)
	sheddableEffectiveCap := cap * (100 - reservedCriticalPct) / 100

	const goroutinesPerPriority = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCritical, allowedSheddable := 0, 0

	launch := func(priority limiter.Priority, count *int) {
		for i := 0; i < goroutinesPerPriority; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				decision, err := l.Check(context.Background(), limiter.Request{Key: "race-key", Cost: 1, Priority: priority})
				if err != nil {
					return
				}
				if decision.Action == limiter.ALLOW {
					mu.Lock()
					*count++
					mu.Unlock()
				}
			}()
		}
	}
	launch(limiter.Critical, &allowedCritical)
	launch(limiter.Sheddable, &allowedSheddable)
	wg.Wait()

	if allowedCritical > cap {
		t.Errorf("expected at most %d critical allows (full cap), got %d", cap, allowedCritical)
	}
	if allowedSheddable > sheddableEffectiveCap {
		t.Errorf("expected at most %d sheddable allows (effective cap after reservation), got %d", sheddableEffectiveCap, allowedSheddable)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (or reveal the property already holds)**

Run: `cd services/core && go test ./limiter/... -race -run "AtCapacityRace" -v -count=5`
Expected: PASS on all 5 runs (the underlying Lua-script atomicity already makes this correct — this task's job is proving it with an explicit, repeatable test, not fixing a bug). If any run fails, that is a genuine finding requiring investigation before proceeding, not something to paper over.

- [ ] **Step 3: Commit**

```bash
git add services/core/limiter/priority_race_test.go
git commit -m "test(core): add priority-partition-at-capacity race regression tests"
```

---

### Task 3: Concurrent stress test for `services/sidecar/decisionlog`

**Files:**
- Modify: `services/sidecar/decisionlog/decisionlog_test.go`

**Interfaces:**
- Consumes: `decisionlog.Log`, `decisionlog.SetOutput` (existing).

- [ ] **Step 1: Write the failing test**

Append to `services/sidecar/decisionlog/decisionlog_test.go`:
```go
func TestLog_ConcurrentCallsAreRaceFree(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	const goroutines = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			decisionlog.Log("rate_limiter", "concurrent-key", "allow", "sheddable", time.Duration(n)*time.Microsecond)
		}(i)
	}
	wg.Wait()

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != goroutines {
		t.Fatalf("expected %d log lines, got %d — some writes may have been lost or interleaved", goroutines, len(lines))
	}
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("line %d is not valid JSON (likely torn/interleaved write under concurrency): %v — line was %q", i, err, line)
		}
	}
}

func TestSetOutput_ConcurrentWithLogIsRaceFree(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	defer decisionlog.SetOutput(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.SetOutput(&buf1)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.Log("rate_limiter", "k", "allow", "sheddable", time.Millisecond)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisionlog.SetOutput(&buf2)
		}()
	}
	wg.Wait()
}
```

Add `"sync"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails without -race, then confirm -race catches any real issue**

Run: `cd services/sidecar && go test ./decisionlog/... -race -v -count=10`
Expected: PASS across all 10 runs — `decisionlog.Log`/`SetOutput` are already mutex-guarded (`mu sync.Mutex`), so this is expected to already be race-free; this task makes that an explicit, repeatable, CI-enforced property instead of an untested assumption (per the roadmap's own framing: "the one file in the whole audit where `-race` runs in CI but can never actually trip").

- [ ] **Step 3: Commit**

```bash
git add services/sidecar/decisionlog/decisionlog_test.go
git commit -m "test(sidecar): add concurrent stress test for decisionlog"
```

---

### Task 4: Property-based tests (rapid) for TokenBucketLimiter and ConcurrencyLimiter

**Files:**
- Create: `services/core/limiter/property_test.go`
- Modify: `services/core/go.mod`, `services/core/go.sum` (add `pgregory.net/rapid`)

**Interfaces:**
- Consumes: `limiter.NewTokenBucketLimiter`, `limiter.NewConcurrencyLimiter`, and a fake in-memory `checker`/`concurrencyChecker` (not real Redis — property tests need to run many thousands of times fast; the atomicity/wire-correctness properties are already covered by Tasks 1/2's real-Redis tests). **Naming note:** `services/core/limiter`'s existing test files (`package limiter_test`) already define `fakeStore` (tokenbucket_test.go), `fakeConcurrencyStore` (concurrency_test.go), `fakeFleetStore` (fleetshedder_test.go), and `fakeTier` (pipeline_test.go) — this task's new fakes below use different names (`fakePropertyTokenBucketStore`, `fakePropertyConcurrencyStore`) specifically to avoid redeclaring a type already in the same package.

- [ ] **Step 1: Add the dependency**

```bash
cd services/core && go get pgregory.net/rapid@latest
```

- [ ] **Step 2: Write the property tests**

```go
// services/core/limiter/property_test.go
package limiter_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ratecap/core/limiter"
)

// fakePropertyTokenBucketStore is an in-memory reference model of a token bucket:
// the exact same refill/decrement arithmetic the real Lua script performs,
// reimplemented independently so rapid can compare the real limiter's
// allow/reject decisions against this trivial model over random sequences.
type fakePropertyTokenBucketStore struct {
	tokens float64
	burst  int
}

func (f *fakePropertyTokenBucketStore) CheckAndDecrement(_ context.Context, _ string, rate, burst, cost int) (bool, int64, error) {
	if f.burst != burst {
		f.tokens = float64(burst)
		f.burst = burst
	}
	if f.tokens < float64(cost) {
		return false, 0, nil
	}
	f.tokens -= float64(cost)
	return true, 0, nil
}

func TestTokenBucketLimiter_NeverAllowsMoreThanBurstWithinOneWindow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		burst := rapid.IntRange(1, 100).Draw(rt, "burst")
		requests := rapid.IntRange(1, 200).Draw(rt, "requests")

		store := &fakePropertyTokenBucketStore{tokens: float64(burst), burst: burst}
		l := limiter.NewTokenBucketLimiter(store, 0, burst, false)

		allowedCount := 0
		for i := 0; i < requests; i++ {
			decision, err := l.Check(context.Background(), limiter.Request{Key: "prop-key", Cost: 1})
			if err != nil {
				rt.Fatalf("unexpected error: %v", err)
			}
			if decision.Action == limiter.ALLOW {
				allowedCount++
			}
		}

		if allowedCount > burst {
			rt.Errorf("with rate=0 (no refill) and burst=%d, allowed %d requests out of %d — invariant violated", burst, allowedCount, requests)
		}
	})
}

// fakePropertyConcurrencyStore is a reference model of a bounded
// concurrency counter: an in-memory set of outstanding tokens, capped at
// `cap`. Named distinctly from concurrency_test.go's existing
// fakeConcurrencyStore (same package, different internal representation,
// used only by this file's property tests) to avoid redeclaring the type.
type fakePropertyConcurrencyStore struct {
	outstanding map[string]bool
	nextToken   int
}

func (f *fakePropertyConcurrencyStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
	if f.outstanding == nil {
		f.outstanding = map[string]bool{}
	}
	if len(f.outstanding) >= cap {
		return false, "", nil
	}
	f.nextToken++
	token := "tok-" + string(rune('a'+f.nextToken%26))
	f.outstanding[token] = true
	return true, token, nil
}

func (f *fakePropertyConcurrencyStore) DecrConcurrent(_ context.Context, _, token string) error {
	delete(f.outstanding, token)
	return nil
}

func TestConcurrencyLimiter_OutstandingNeverExceedsCapAcrossAcquireReleaseSequences(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cap := rapid.IntRange(1, 50).Draw(rt, "cap")
		store := &fakePropertyConcurrencyStore{}
		l := limiter.NewConcurrencyLimiter(store, cap, 60000, false, false, 0, 0, 0)

		var held []limiter.TokenReservation
		steps := rapid.IntRange(1, 200).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			acquire := rapid.Bool().Draw(rt, "acquire")
			if acquire || len(held) == 0 {
				decision, err := l.Check(context.Background(), limiter.Request{Key: "prop-key", Cost: 1})
				if err != nil {
					rt.Fatalf("unexpected error: %v", err)
				}
				if decision.Action == limiter.ALLOW {
					held = append(held, decision.Reservations...)
				}
				if len(store.outstanding) > cap {
					rt.Errorf("outstanding count %d exceeded cap %d after an acquire", len(store.outstanding), cap)
				}
			} else {
				idx := rapid.IntRange(0, len(held)-1).Draw(rt, "release_index")
				token := held[idx]
				if err := store.DecrConcurrent(context.Background(), token.Key, token.Token); err != nil {
					rt.Fatalf("unexpected error: %v", err)
				}
				held = append(held[:idx], held[idx+1:]...)
			}
		}
	})
}
```

- [ ] **Step 3: Run tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -run "TestTokenBucketLimiter_NeverAllows|TestConcurrencyLimiter_Outstanding" -v`
Expected: PASS. `rapid.Check` runs hundreds of randomized cases per test by default; a genuine invariant violation would print the minimal failing case rapid shrinks to.

- [ ] **Step 4: Commit**

```bash
git add services/core/limiter/property_test.go services/core/go.mod services/core/go.sum
git commit -m "test(core): add property-based tests for TokenBucketLimiter and ConcurrencyLimiter"
```

---

### Task 5: Fault-injection tests for the fsnotify config hot-reload path

**Files:**
- Modify: `services/core/config/watcher_test.go`

**Interfaces:**
- Consumes: `config.Watch` (Phase 1's `func(*Config, error)` signature).

- [ ] **Step 1: Write the failing tests**

Append to `services/core/config/watcher_test.go`:
```go
func TestWatch_SurvivesPartialWrite(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	// Simulate a partial write: truncate then write only the first half of
	// valid YAML, without the closing content — a real interrupted-write
	// scenario, distinct from TestWatch_SkipsInvalidConfigWithoutCrashing's
	// well-formed-but-semantically-invalid case.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("failed to open for partial write: %v", err)
	}
	if _, err := f.WriteString("sync_rate: 10\ntiers:\n  rate_lim"); err != nil {
		t.Fatalf("failed to write partial content: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close after partial write: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	select {
	case cfg := <-changes:
		t.Errorf("expected no onChange for a partial/truncated write, got a config with SyncRate=%d", cfg.SyncRate)
	default:
	}

	validContents := `
sync_rate: 20
tiers:
  rate_limiter:
    default_rate: 300
    default_burst: 1500
    shadow_mode: false
`
	if err := os.WriteFile(path, []byte(validContents), 0644); err != nil {
		t.Fatalf("failed to write recovery content: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 20 {
			t.Errorf("expected recovery reload with SyncRate=20, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery reload after a partial write")
	}
}

func TestWatch_SurvivesAtomicRenameSwap(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	// Atomic rename-swap: write to a temp file in the same directory, then
	// os.Rename over the watched path — the pattern tools like Kubernetes
	// ConfigMap volume mounts and `mv` use, distinct from an in-place write.
	tmpPath := dir + "/ratecap.yaml.tmp"
	if err := os.WriteFile(tmpPath, []byte(`
sync_rate: 30
tiers:
  rate_limiter:
    default_rate: 400
    default_burst: 2000
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("failed to atomically rename over watched path: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 30 {
			t.Errorf("expected reload via rename-swap with SyncRate=30, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after an atomic rename-swap")
	}
}

func TestWatch_SurvivesDeleteAndRecreate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ratecap.yaml"
	if err := os.WriteFile(path, []byte(`
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	changes := make(chan *config.Config, 5)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		if loadErr == nil {
			changes <- cfg
		}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to delete watched file: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(path, []byte(`
sync_rate: 40
tiers:
  rate_limiter:
    default_rate: 500
    default_burst: 2500
    shadow_mode: false
`), 0644); err != nil {
		t.Fatalf("failed to recreate watched file: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.SyncRate != 40 {
			t.Errorf("expected reload after delete+recreate with SyncRate=40, got %d", cfg.SyncRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after delete+recreate — fsnotify.Add watches a directory here, so a recreated file under the same dir should still be seen")
	}
}
```

- [ ] **Step 2: Run tests**

Run: `cd services/core && go test ./config/... -race -run "TestWatch_Survives" -v`
Expected: PASS. If `TestWatch_SurvivesDeleteAndRecreate` fails because `fsnotify`'s directory watch drops the specific filename after a delete event on some platforms, that is a genuine finding — investigate `watcher.go`'s `event.Name == path` filter (it watches the directory, so a recreate under the same name should still match) before assuming the test itself is wrong.

- [ ] **Step 3: Commit**

```bash
git add services/core/config/watcher_test.go
git commit -m "test(core): add fault-injection tests for the fsnotify config hot-reload path"
```

---

### Task 6: Config-consistency version/hash stamp across replicas

**Files:**
- Modify: `services/core/config/config.go`, `services/core/config/config_test.go`, `services/core/metrics/metrics.go`, `services/core/metrics/metrics_test.go`, `services/core/main.go`

**Interfaces:**
- Produces: `config.Config.Hash() string` (a stable content hash), `metrics.ConfigVersionInfo` (a `GaugeVec` with a `hash` label set to 1, existing labels cleared on each reload — the standard Prometheus "info metric" pattern for exposing a string value operators can query/diff across replicas).

- [ ] **Step 1: Write the failing tests**

```go
// services/core/config/config_test.go — append
func TestConfig_Hash_IsStableForIdenticalContent(t *testing.T) {
	yaml := `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`
	cfg1, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg1.Hash() != cfg2.Hash() {
		t.Errorf("expected identical config content to hash identically, got %q and %q", cfg1.Hash(), cfg2.Hash())
	}
}

func TestConfig_Hash_DiffersForDifferentContent(t *testing.T) {
	cfg1, err := Load(writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2, err := Load(writeTempConfig(t, `
sync_rate: 10
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg1.Hash() == cfg2.Hash() {
		t.Error("expected different config content to hash differently")
	}
}
```

This reuses `config_test.go`'s existing `writeTempConfig(t, contents string) string` helper (already defined in that file for the watcher tests) and `Load`, so it's guaranteed to compile against `Config`'s real (unexported-shape) type — no need to construct a `Config` literal by hand.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/core && go test ./config/... -race -run TestConfig_Hash -v`
Expected: FAIL — `Hash()` undefined.

- [ ] **Step 3: Implement `Config.Hash()`**

```go
// services/core/config/config.go — add near the bottom, after Validate()
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	// ...(keep existing imports: "fmt", "os", "gopkg.in/yaml.v3")
)

// Hash returns a short, stable content hash of the config, independent of
// key ordering or whitespace in the source YAML — two replicas that loaded
// byte-different but semantically identical files must still agree.
// json.Marshal (not the raw YAML bytes) is the content this hashes, since
// struct field order is fixed by the Go type, unlike YAML key order.
func (c *Config) Hash() string {
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/core && go test ./config/... -race -v`

- [ ] **Step 5: Add the metric**

```go
// services/core/metrics/metrics.go — add
var ConfigVersionInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "ratecap_core_config_version_info",
	Help: "Info metric (always 1) whose hash label is the currently-active config's content hash — compare this label across replicas to detect hot-reload divergence.",
}, []string{"hash"})

var currentConfigHash string

// RecordConfigVersion clears the previous hash's series (if any) and sets
// the new one to 1, so ratecap_core_config_version_info always has exactly
// one active series per replica rather than accumulating a stale series per
// historical hash forever.
func RecordConfigVersion(hash string) {
	if currentConfigHash != "" && currentConfigHash != hash {
		ConfigVersionInfo.DeleteLabelValues(currentConfigHash)
	}
	currentConfigHash = hash
	ConfigVersionInfo.WithLabelValues(hash).Set(1)
}
```

Add a test to `services/core/metrics/metrics_test.go`:
```go
func TestRecordConfigVersion_ClearsPreviousHashSeries(t *testing.T) {
	metrics.RecordConfigVersion("hash-aaa")
	if got := testutil.ToFloat64(metrics.ConfigVersionInfo.WithLabelValues("hash-aaa")); got != 1 {
		t.Errorf("expected hash-aaa series set to 1, got %v", got)
	}

	metrics.RecordConfigVersion("hash-bbb")
	if got := testutil.ToFloat64(metrics.ConfigVersionInfo.WithLabelValues("hash-bbb")); got != 1 {
		t.Errorf("expected hash-bbb series set to 1, got %v", got)
	}
	count := testutil.CollectAndCount(metrics.ConfigVersionInfo)
	if count != 1 {
		t.Errorf("expected exactly one active series after switching hashes, got %d", count)
	}
}
```

- [ ] **Step 6: Wire into `main.go` and log it**

In `services/core/main.go`'s `config.Watch` callback, after the existing `coremetrics.RecordConfigReload("success")` line:
```go
		coremetrics.RecordConfigReload("success")
		coremetrics.RecordConfigVersion(newCfg.Hash())
		log.Printf("ratecap-core: config reloaded, hash=%s", newCfg.Hash())
```
Also call `coremetrics.RecordConfigVersion(cfg.Hash())` once right after the initial `cfg, err := config.Load(configPath)` / `cfg.Validate()` succeeds at startup, so the metric is populated before the first hot-reload ever fires — not just on reload.

- [ ] **Step 7: Run full core test suite and build**

Run: `cd services/core && go build ./... && go test ./... -race`

- [ ] **Step 8: Commit**

```bash
git add services/core/config/config.go services/core/config/config_test.go services/core/metrics/metrics.go services/core/metrics/metrics_test.go services/core/main.go
git commit -m "feat(core): add config content-hash and cross-replica version metric"
```

---

### Task 7: Coverage measurement and CI gate (Go modules + Python SDK)

**Files:**
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Add coverage to the Go build-and-test matrix job**

Change the `Test` step in the `build-and-test` job:
```yaml
      - name: Test
        working-directory: ${{ matrix.module }}
        run: go test ./... -race -coverprofile=coverage.out -covermode=atomic

      - name: Enforce coverage floor
        working-directory: ${{ matrix.module }}
        run: |
          pct=$(go tool cover -func=coverage.out | tail -1 | awk '{print $NF}' | tr -d '%')
          echo "Total coverage for ${{ matrix.module }}: ${pct}%"
          floor=50
          if (( $(echo "$pct < $floor" | bc -l) )); then
            echo "Coverage ${pct}% is below the ${floor}% floor for ${{ matrix.module }}" >&2
            exit 1
          fi
```
Note: `deploy/sampleapp` has no tests (per `CLAUDE.md`'s existing "excludes deploy/sampleapp (demo binary, no tests)" note) — `go test ./... -coverprofile=coverage.out` with zero test files still writes an (empty) coverage file and `go tool cover -func` on it exits 0 with no function lines, making `tail -1 | awk '{print $NF}'` produce an empty string that would break the `bc` comparison. Guard this explicitly:
```yaml
      - name: Enforce coverage floor
        working-directory: ${{ matrix.module }}
        run: |
          if [ ! -s coverage.out ]; then
            echo "No coverage data for ${{ matrix.module }} (no test files) — skipping floor check"
            exit 0
          fi
          pct=$(go tool cover -func=coverage.out | tail -1 | awk '{print $NF}' | tr -d '%')
          echo "Total coverage for ${{ matrix.module }}: ${pct}%"
          floor=50
          if (( $(echo "$pct < $floor" | bc -l) )); then
            echo "Coverage ${pct}% is below the ${floor}% floor for ${{ matrix.module }}" >&2
            exit 1
          fi
```

- [ ] **Step 2: Add coverage to the Python SDK job**

Change the `python-sdk` job's steps:
```yaml
      - name: Install package
        working-directory: packages/sdks/python
        run: pip install -e . && pip install coverage

      - name: Test
        working-directory: packages/sdks/python
        run: coverage run -m unittest discover -s tests -v && coverage report --fail-under=50
```

- [ ] **Step 3: Verify locally before committing**

Run against each real module to confirm the exact floor (50%) is realistic and not immediately breaking CI — this repo's existing test suites are extensive (per `CLAUDE.md`'s description of thorough tier-by-tier audits), so 50% is a conservative starting floor, not a stretch target:
```bash
cd services/core && go test ./... -race -coverprofile=/tmp/core-cov.out -covermode=atomic 2>&1 | tail -5 && go tool cover -func=/tmp/core-cov.out | tail -1
cd ../sidecar && go test ./... -race -coverprofile=/tmp/sidecar-cov.out -covermode=atomic 2>&1 | tail -5 && go tool cover -func=/tmp/sidecar-cov.out | tail -1
cd ../../proto && go test ./... -race -coverprofile=/tmp/proto-cov.out -covermode=atomic 2>&1 | tail -5 && go tool cover -func=/tmp/proto-cov.out | tail -1
cd ../packages/sdks/go && go test ./... -race -coverprofile=/tmp/sdkgo-cov.out -covermode=atomic 2>&1 | tail -5 && go tool cover -func=/tmp/sdkgo-cov.out | tail -1
cd ../../../cli && go test ./... -race -coverprofile=/tmp/cli-cov.out -covermode=atomic 2>&1 | tail -5 && go tool cover -func=/tmp/cli-cov.out | tail -1
```
If any module's real coverage is below 50%, either lower that module's floor to a value just below its current real number (rounded down to the nearest 5%) with a comment explaining why, or flag it as a follow-up issue rather than silently weakening the gate for every module to match the weakest one.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add coverage measurement and a 50% floor gate for all Go modules and the Python SDK"
```

---

### Task 8: Mutation testing (Gremlins) CI gate

**Files:**
- Create: `.gremlins.yaml` (repo root — Gremlins' config is project-wide, but its `--paths`/target flags scope which packages it mutates per invocation)
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Write the Gremlins config**

```yaml
# .gremlins.yaml
unleash:
  dry-run: false
  test-cpu: 1
  timeout-coefficient: 3
  threshold-efficacy: 60
  threshold-mcover: 30
integration-mode: false
```

- [ ] **Step 2: Add a CI job scoped to `services/core/limiter` and `packages/sdks/go`**

```yaml
  mutation-testing:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1

      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: "1.26.2"

      - name: Install gremlins
        run: go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.5.4

      - name: Mutation test services/core/limiter
        working-directory: services/core
        run: $(go env GOPATH)/bin/gremlins unleash ./limiter/...

      - name: Mutation test packages/sdks/go
        working-directory: packages/sdks/go
        run: $(go env GOPATH)/bin/gremlins unleash ./...
```

- [ ] **Step 3: Verify locally**

Run: `cd services/core && ~/go/bin/gremlins unleash ./limiter/...` (using the already-installed binary from this session's earlier feasibility check) and confirm it runs to completion and reports a mutation-testing efficacy score. If the `threshold-efficacy: 60` / `threshold-mcover: 30` values in Step 1 cause Gremlins to exit non-zero against the current `limiter` package's real test suite, either the tests have a genuine gap worth investigating, or the thresholds need adjusting to match this package's real, current baseline (document which, in the commit message) — do not silently set thresholds to whatever number happens to pass without checking which case applies.

- [ ] **Step 4: Commit**

```bash
git add .gremlins.yaml .github/workflows/ci.yml
git commit -m "ci: add Gremlins mutation-testing gate scoped to services/core/limiter and packages/sdks/go"
```

---

### Task 9: Core Redis client becomes Sentinel-aware

**Files:**
- Modify: `services/core/main.go`

**Interfaces:**
- Produces: a `RATECAP_REDIS_SENTINEL_ADDRS` env var (comma-separated `host:port` list) that, when set, switches `main.go` from `redis.NewClient` to `redis.NewFailoverClient` with a `MasterName` (`RATECAP_REDIS_SENTINEL_MASTER_NAME`, default `"mymaster"`) — when unset, behavior is byte-for-byte identical to today (single-instance `redis.NewClient`), so this is purely additive.

- [ ] **Step 1: Write the failing test**

Create `services/core/redisclient_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestParseSentinelAddrs_EmptyStringReturnsNil(t *testing.T) {
	got := parseSentinelAddrs("")
	if got != nil {
		t.Errorf("expected nil for empty string, got %v", got)
	}
}

func TestParseSentinelAddrs_SplitsCommaSeparatedList(t *testing.T) {
	got := parseSentinelAddrs("sentinel-0:26379,sentinel-1:26379,sentinel-2:26379")
	want := []string{"sentinel-0:26379", "sentinel-1:26379", "sentinel-2:26379"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestParseSentinelAddrs_TrimsWhitespaceAroundEntries(t *testing.T) {
	got := parseSentinelAddrs(" sentinel-0:26379 , sentinel-1:26379 ")
	want := []string{"sentinel-0:26379", "sentinel-1:26379"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/core && go test . -race -run TestParseSentinelAddrs -v`
Expected: FAIL — `parseSentinelAddrs` undefined.

- [ ] **Step 3: Implement**

Add to `services/core/main.go` (near the other `resolveX` helpers, or near the top-level functions if this file has none yet — check `grep -n "^func " services/core/main.go` first since Phase 1 added `runRedisHealthLoop` as this file's first extracted helper):

```go
func parseSentinelAddrs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	addrs := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	return addrs
}

func newRedisClient(redisAddr, sentinelAddrsRaw, sentinelMasterName string) *redis.Client {
	sentinelAddrs := parseSentinelAddrs(sentinelAddrsRaw)
	if len(sentinelAddrs) == 0 {
		return redis.NewClient(&redis.Options{Addr: redisAddr})
	}
	log.Printf("ratecap-core: using Redis Sentinel (master=%s, sentinels=%v)", sentinelMasterName, sentinelAddrs)
	return redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:    sentinelMasterName,
		SentinelAddrs: sentinelAddrs,
	})
}
```

Replace the existing:
```go
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
```
with:
```go
	sentinelMasterName := os.Getenv("RATECAP_REDIS_SENTINEL_MASTER_NAME")
	if sentinelMasterName == "" {
		sentinelMasterName = "mymaster"
	}
	redisClient := newRedisClient(redisAddr, os.Getenv("RATECAP_REDIS_SENTINEL_ADDRS"), sentinelMasterName)
```

Add `"strings"` to the import block if not already present.

- [ ] **Step 4: Run tests to verify they pass, and confirm the build**

Run: `cd services/core && go test . -race -v && go build ./...`

- [ ] **Step 5: Commit**

```bash
git add services/core/main.go services/core/redisclient_test.go
git commit -m "feat(core): make the Redis client Sentinel-aware via RATECAP_REDIS_SENTINEL_ADDRS"
```

---

### Task 10: Helm chart — Redis Sentinel templates

**Files:**
- Modify: `deploy/helm/ratecap/templates/redis.yaml`, `deploy/helm/ratecap/values.yaml`
- Create: `deploy/helm/ratecap/templates/redis-sentinel-configmap.yaml`

**Interfaces:**
- Produces: a `redis.sentinel.enabled` values flag (default `false`, so existing deployments are unaffected until explicitly opted in — matching this repo's established "off by default, explicit opt-in" convention from `tls.enabled`).

- [ ] **Step 1: Add Sentinel values**

In `deploy/helm/ratecap/values.yaml`, extend the `redis:` block:
```yaml
redis:
  image:
    repository: redis
    tag: 7-alpine
    pullPolicy: IfNotPresent
  port: 6379
  sentinel:
    enabled: false
    replicas: 3
    port: 26379
    masterName: mymaster
    quorum: 2
```

- [ ] **Step 2: Rewrite `redis.yaml` to conditionally render single-instance vs. Sentinel-backed**

```yaml
{{- if not .Values.redis.sentinel.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Release.Name }}-redis
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-redis
    spec:
      containers:
        - name: redis
          image: "{{ .Values.redis.image.repository }}:{{ .Values.redis.image.tag }}"
          imagePullPolicy: {{ .Values.redis.image.pullPolicy }}
          ports:
            - containerPort: {{ .Values.redis.port }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-redis
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-redis
  ports:
    - port: {{ .Values.redis.port }}
      targetPort: {{ .Values.redis.port }}
{{- else }}
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Release.Name }}-redis
spec:
  serviceName: {{ .Release.Name }}-redis-headless
  replicas: {{ .Values.redis.sentinel.replicas }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}-redis
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-redis
    spec:
      containers:
        - name: redis
          image: "{{ .Values.redis.image.repository }}:{{ .Values.redis.image.tag }}"
          imagePullPolicy: {{ .Values.redis.image.pullPolicy }}
          # Pod 0 boots as master; every other pod replicates from pod 0 via
          # the StatefulSet's own stable DNS name (<name>-N via the headless
          # service) — no external coordination needed for initial startup.
          # Sentinel (below) handles promotion if pod 0 later fails.
          command:
            - sh
            - -c
            - |
              if [ "$(hostname)" = "{{ .Release.Name }}-redis-0" ]; then
                exec redis-server --port {{ .Values.redis.port }}
              else
                exec redis-server --port {{ .Values.redis.port }} --replicaof {{ .Release.Name }}-redis-0.{{ .Release.Name }}-redis-headless {{ .Values.redis.port }}
              fi
          ports:
            - containerPort: {{ .Values.redis.port }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-redis-headless
spec:
  clusterIP: None
  selector:
    app: {{ .Release.Name }}-redis
  ports:
    - port: {{ .Values.redis.port }}
      targetPort: {{ .Values.redis.port }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-redis
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-redis
    statefulset.kubernetes.io/pod-name: {{ .Release.Name }}-redis-0
  ports:
    - port: {{ .Values.redis.port }}
      targetPort: {{ .Values.redis.port }}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Release.Name }}-redis-sentinel
spec:
  serviceName: {{ .Release.Name }}-redis-sentinel-headless
  replicas: {{ .Values.redis.sentinel.replicas }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}-redis-sentinel
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-redis-sentinel
    spec:
      containers:
        - name: sentinel
          image: "{{ .Values.redis.image.repository }}:{{ .Values.redis.image.tag }}"
          imagePullPolicy: {{ .Values.redis.image.pullPolicy }}
          command: ["redis-sentinel", "/etc/sentinel/sentinel.conf"]
          ports:
            - containerPort: {{ .Values.redis.sentinel.port }}
          volumeMounts:
            - name: sentinel-config
              mountPath: /etc/sentinel
      volumes:
        - name: sentinel-config
          configMap:
            name: {{ .Release.Name }}-redis-sentinel-config
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-redis-sentinel-headless
spec:
  clusterIP: None
  selector:
    app: {{ .Release.Name }}-redis-sentinel
  ports:
    - port: {{ .Values.redis.sentinel.port }}
      targetPort: {{ .Values.redis.sentinel.port }}
{{- end }}
```

- [ ] **Step 3: Add the Sentinel ConfigMap**

```yaml
# deploy/helm/ratecap/templates/redis-sentinel-configmap.yaml
{{- if .Values.redis.sentinel.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-redis-sentinel-config
data:
  sentinel.conf: |
    port {{ .Values.redis.sentinel.port }}
    sentinel monitor {{ .Values.redis.sentinel.masterName }} {{ .Release.Name }}-redis-0.{{ .Release.Name }}-redis-headless {{ .Values.redis.port }} {{ .Values.redis.sentinel.quorum }}
    sentinel down-after-milliseconds {{ .Values.redis.sentinel.masterName }} 5000
    sentinel failover-timeout {{ .Values.redis.sentinel.masterName }} 60000
    sentinel parallel-syncs {{ .Values.redis.sentinel.masterName }} 1
{{- end }}
```

- [ ] **Step 4: Verify with the existing CI-equivalent commands, both flag states**

```bash
helm lint deploy/helm/ratecap
helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set concurrencySigningKey.existingSecretName=test-signing-key
helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set concurrencySigningKey.existingSecretName=test-signing-key --set redis.sentinel.enabled=true
```
Expected: both render without error, and the second invocation's output includes the StatefulSet/ConfigMap/Sentinel resources while the first's does not.

**This verification proves the templates are syntactically correct and render the intended resource set — it does NOT prove Sentinel failover actually works, since that requires a real multi-node cluster.** Say this explicitly in the commit message and note it as a manual pre-production verification step, not something this task claims to have proven.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/ratecap/templates/redis.yaml deploy/helm/ratecap/templates/redis-sentinel-configmap.yaml deploy/helm/ratecap/values.yaml
git commit -m "feat(helm): add opt-in Redis Sentinel StatefulSet, off by default

helm lint/template verified for both redis.sentinel.enabled=false (default,
unchanged single-instance behavior) and =true. Real failover behavior is
NOT verified here — that requires a live multi-node cluster and should be
validated in a staging environment before enabling in production."
```

---

### Task 11: Tier 4 gradual shed-curve ramping

**Files:**
- Modify: `services/sidecar/worker/shedder.go`, `services/sidecar/worker/shedder_test.go`, `ARCHITECTURE.md`

**Context:** `services/sidecar/worker/shedder.go`'s current `Allow()` is a hard binary cutoff (`current >= max` → reject, otherwise allow) — confirmed by reading the file: no ramping exists today. Stripe's documented lesson (cited in the roadmap spec) is that binary on/off shedding causes flapping. This task replaces the hard cutoff with a probabilistic ramp starting at a configurable threshold below `max`.

**Interfaces:**
- Produces: `NewShedderWithRamp(max int64, rampStartPct int) *Shedder` — `NewShedder(max)` keeps its exact current signature and behavior for backward compatibility (defaults `rampStartPct` to 100, meaning "ramp starts exactly at max," i.e. the original hard-cutoff behavior), so `services/sidecar/main.go`'s existing call site is unaffected unless explicitly opted into ramping via a new env var.

- [ ] **Step 1: Write the failing tests**

```go
// services/sidecar/worker/shedder_test.go — append
func TestShedder_BelowRampStart_AlwaysAllows(t *testing.T) {
	s := NewShedderWithRamp(100, 80) // ramp starts at 80% of max
	for i := 0; i < 79; i++ {
		if !s.Allow() {
			t.Fatalf("expected allow #%d (below ramp-start threshold of 80), but was rejected", i)
		}
	}
}

func TestShedder_AtMax_NeverAllows(t *testing.T) {
	s := NewShedderWithRamp(10, 50)
	for i := 0; i < 10; i++ {
		if !s.Allow() {
			t.Fatalf("expected allow #%d filling to max", i)
		}
	}
	if s.Allow() {
		t.Error("expected reject once inflight has reached max, regardless of ramp")
	}
}

func TestShedder_WithinRampWindow_ProbabilityDecreasesTowardMax(t *testing.T) {
	const max = 1000
	const rampStartPct = 50 // ramp window is [500, 1000)
	const trials = 20000

	rejectedEarlyInWindow := 0
	rejectedLateInWindow := 0
	for i := 0; i < trials; i++ {
		s := NewShedderWithRamp(max, rampStartPct)
		for j := 0; j < 520; j++ {
			s.Allow()
		}
		if !s.Allow() {
			rejectedEarlyInWindow++
		}
	}
	for i := 0; i < trials; i++ {
		s := NewShedderWithRamp(max, rampStartPct)
		for j := 0; j < 980; j++ {
			s.Allow()
		}
		if !s.Allow() {
			rejectedLateInWindow++
		}
	}

	if rejectedLateInWindow <= rejectedEarlyInWindow {
		t.Errorf("expected rejection rate to increase closer to max: early-window rejects=%d, late-window rejects=%d out of %d trials each", rejectedEarlyInWindow, rejectedLateInWindow, trials)
	}
}

func TestShedder_LegacyConstructor_BehavesAsHardCutoff(t *testing.T) {
	s := NewShedder(5)
	for i := 0; i < 5; i++ {
		if !s.Allow() {
			t.Fatalf("expected allow #%d filling to max via legacy NewShedder", i)
		}
	}
	if s.Allow() {
		t.Error("expected NewShedder (no ramp param) to behave as a hard cutoff at max, matching pre-existing behavior")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./worker/... -race -v`
Expected: FAIL — `NewShedderWithRamp` undefined.

- [ ] **Step 3: Implement the ramp**

```go
// services/sidecar/worker/shedder.go
package worker

import (
	"math/rand"
	"sync/atomic"
)

type Shedder struct {
	inflight     atomic.Int64
	max          int64
	rampStartPct int64
}

func NewShedder(max int64) *Shedder {
	return NewShedderWithRamp(max, 100)
}

// NewShedderWithRamp starts probabilistically rejecting once inflight
// crosses rampStartPct% of max, linearly increasing the reject probability
// to 100% exactly at max — replacing a hard on/off cutoff (Stripe's
// documented flapping failure mode for binary shed curves) with a gradual
// one. rampStartPct=100 recovers the exact original hard-cutoff behavior.
func NewShedderWithRamp(max int64, rampStartPct int) *Shedder {
	return &Shedder{max: max, rampStartPct: int64(rampStartPct)}
}

func (s *Shedder) Allow() bool {
	for {
		current := s.inflight.Load()
		if current >= s.max {
			return false
		}
		if !s.shouldAdmitAtLoad(current) {
			return false
		}
		if s.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// shouldAdmitAtLoad returns false with a probability that ramps linearly
// from 0 at rampStartPct of max to (just under) 1 at max itself — current
// is always < s.max here (checked by the caller), so rejectProbability
// never reaches exactly 1 via this path alone.
func (s *Shedder) shouldAdmitAtLoad(current int64) bool {
	rampStart := s.max * s.rampStartPct / 100
	if current < rampStart {
		return true
	}
	rampWindow := s.max - rampStart
	if rampWindow <= 0 {
		return false
	}
	intoRamp := current - rampStart
	rejectProbability := float64(intoRamp) / float64(rampWindow)
	return rand.Float64() >= rejectProbability
}

func (s *Shedder) Release() {
	s.inflight.Add(-1)
}

func (s *Shedder) InFlight() int64 {
	return s.inflight.Load()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./worker/... -race -v -count=3`
Expected: PASS across 3 runs (the probabilistic test uses a large trial count specifically to keep it deterministically non-flaky; if it flakes, increase `trials` rather than loosening the assertion).

- [ ] **Step 5: Wire an opt-in env var into `main.go`**

In `services/sidecar/main.go`, change:
```go
	maxInflight := resolveMaxInflight(os.Getenv("RATECAP_MAX_INFLIGHT_REQUESTS"), 500)
	shedder := worker.NewShedder(maxInflight)
```
to:
```go
	maxInflight := resolveMaxInflight(os.Getenv("RATECAP_MAX_INFLIGHT_REQUESTS"), 500)
	rampStartPct := resolveRampStartPct(os.Getenv("RATECAP_SHED_RAMP_START_PCT"), 100)
	shedder := worker.NewShedderWithRamp(maxInflight, rampStartPct)
```
Add the helper (same file, near `resolveMaxRPS`):
```go
func resolveRampStartPct(envVal string, defaultVal int) int {
	if envVal == "" {
		return defaultVal
	}
	parsed, err := strconv.Atoi(envVal)
	if err != nil {
		log.Printf("RATECAP_SHED_RAMP_START_PCT=%q is not a valid integer, using default of %d: %v", envVal, defaultVal, err)
		return defaultVal
	}
	if parsed <= 0 || parsed > 100 {
		log.Printf("RATECAP_SHED_RAMP_START_PCT=%q must be in (0, 100], using default of %d", envVal, defaultVal)
		return defaultVal
	}
	return parsed
}
```
Add matching tests to `services/sidecar/main_test.go` (`TestResolveRampStartPct_EmptyStringReturnsDefault`, `_ValidValueIsUsed`, `_UnparseableStringReturnsDefault`, `_ZeroReturnsDefault`, `_AboveOneHundredReturnsDefault`), following the exact pattern of the file's existing `TestResolveMaxRPS_*` tests.

- [ ] **Step 6: Document the ramp in `ARCHITECTURE.md`**

Add to the Observability section's "Known limitations" area, or as a new small subsection right after it:
```markdown
### Tier 4 shed-curve ramping

`worker.Shedder` ramps gradually rather than cutting off hard at its cap: below `RATECAP_SHED_RAMP_START_PCT` (default 100 — i.e. no ramp, matching pre-v2.6.0 behavior) of `RATECAP_MAX_INFLIGHT_REQUESTS`, every request is admitted; within the ramp window, rejection probability increases linearly to 100% exactly at the cap. This avoids the binary-on/off flapping failure mode Stripe's own load shedders are documented to have hit.
```

- [ ] **Step 7: Run the full sidecar suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race`

- [ ] **Step 8: Commit**

```bash
git add services/sidecar/worker/shedder.go services/sidecar/worker/shedder_test.go services/sidecar/main.go services/sidecar/main_test.go ARCHITECTURE.md
git commit -m "feat(sidecar): ramp Tier 4's shed curve gradually instead of a hard cutoff"
```

---

### Task 12: Admin RPC — proto definition, core-side setters, and gRPC handler

**Files:**
- Modify: `proto/ratecap/v1/ratecap.proto`, `services/core/grpcserver/server.go`, `services/core/grpcserver/server_test.go`, `services/core/limiter/tokenbucket.go`, `services/core/limiter/tokenbucket_test.go`, `services/core/limiter/fleetshedder.go`, `services/core/limiter/fleetshedder_test.go` (check if this test file exists first — Phase 1 didn't touch it, verify with `ls`), `services/core/main.go`

**Interfaces:**
- Produces: `TokenBucketLimiter.SetRate(rate int) (previous int)`, `FleetShedder.SetReservedCriticalPct(pct int) (previous int, err error)`, proto RPC `SetDynamicLimit(SetDynamicLimitRequest) returns (SetDynamicLimitResponse)` on the existing `RatecapService`.

- [ ] **Step 1: Add the proto RPC**

In `proto/ratecap/v1/ratecap.proto`, add the RPC to the existing service and the two new messages:
```protobuf
service RatecapService {
  rpc CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse);
  rpc ReleaseConcurrency(ReleaseConcurrencyRequest) returns (ReleaseConcurrencyResponse);
  rpc SetDynamicLimit(SetDynamicLimitRequest) returns (SetDynamicLimitResponse);
}
```
```protobuf
message SetDynamicLimitRequest {
  string tier = 1; // "rate_limiter" or "fleet_shedder"
  int32 value = 2; // rate_limiter: new rate (requests/sec); fleet_shedder: new reserved_critical_pct (0-100)
}

message SetDynamicLimitResponse {
  string tier = 1;
  int32 previous_value = 2;
  int32 new_value = 3;
}
```

- [ ] **Step 2: Regenerate**

Run from the repo root (installing the plugins first if not already on `PATH` — already verified installable in this environment via `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` and `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`):
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
cd proto && go build ./... && go test ./... -race
```
Expected: regenerates `proto/ratecap/v1/ratecap.pb.go` and `ratecap_grpc.pb.go` with the new message types and an `UnimplementedRatecapServiceServer.SetDynamicLimit` stub; both existing proto tests and the build stay green.

- [ ] **Step 3: Write the failing limiter tests**

**Naming note:** `tokenbucket_test.go` already defines `fakeStore`/`newFakeStore()` (fields: `tokens map[string]int`, `err error`) and `fleetshedder_test.go` already defines `fakeFleetStore`/`newFakeFleetStore()` (both files exist, both in `package limiter_test`) — reuse them exactly as-is below rather than inventing new fake types.

```go
// services/core/limiter/tokenbucket_test.go — append
func TestTokenBucketLimiter_SetRate_ChangesEffectiveRate(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	previous := l.SetRate(999)

	if previous != 100 {
		t.Errorf("expected previous rate of 100, got %d", previous)
	}
}

func TestTokenBucketLimiter_SetRate_ConcurrentWithCheckIsRaceFree(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 500, false)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.SetRate(n)
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Check(context.Background(), limiter.Request{Key: "k", Cost: 1})
		}()
	}
	wg.Wait()
}
```
`"sync"` is already imported in this file (used by the existing `TestTokenBucketLimiter_ConcurrentCheckAndReconfigureIsRaceFree`).

```go
// services/core/limiter/fleetshedder_test.go — append
func TestFleetShedder_SetReservedCriticalPct_ChangesEffectivePct(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 100, 20, 60000, false)

	previous, err := l.SetReservedCriticalPct(50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if previous != 20 {
		t.Errorf("expected previous pct of 20, got %d", previous)
	}
}

func TestFleetShedder_SetReservedCriticalPct_RejectsOutOfRangeValue(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 100, 20, 60000, false)

	if _, err := l.SetReservedCriticalPct(101); err == nil {
		t.Error("expected an error for a value above 100")
	}
	if _, err := l.SetReservedCriticalPct(-1); err == nil {
		t.Error("expected an error for a negative value")
	}
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `cd services/core && go test ./limiter/... -race -run "SetRate|SetReservedCriticalPct" -v`

- [ ] **Step 5: Implement the setters**

```go
// services/core/limiter/tokenbucket.go — add method
// SetRate is a narrow, single-field alternative to Reconfigure for the
// sub-second incident-response admin lever (no config re-parse, one field).
func (l *TokenBucketLimiter) SetRate(rate int) (previous int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous = l.rate
	l.rate = rate
	return previous
}
```
```go
// services/core/limiter/fleetshedder.go — add method
func (l *FleetShedder) SetReservedCriticalPct(pct int) (previous int, err error) {
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("reserved_critical_pct must be between 0 and 100 inclusive, got %d", pct)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	previous = l.reservedCriticalPct
	l.reservedCriticalPct = pct
	return previous, nil
}
```
Add `"fmt"` to `fleetshedder.go`'s imports.

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -v`

- [ ] **Step 7: Wire the gRPC handler**

`grpcserver.Server` currently only holds the abstract `checker` (the `Pipeline`), not the two concrete limiters this RPC needs to reach. Extend it:

```go
// services/core/grpcserver/server.go
type dynamicLimitSetter interface {
	SetRate(rate int) (previous int)
}

type reservedPctSetter interface {
	SetReservedCriticalPct(pct int) (previous int, err error)
}

type Server struct {
	ratecapv1.UnimplementedRatecapServiceServer
	pipeline    checker
	releaser    concurrencyReleaser
	rateLimiter dynamicLimitSetter
	fleetShedder reservedPctSetter
	signingKey  []byte
}

func NewServer(p checker, releaser concurrencyReleaser, rateLimiter dynamicLimitSetter, fleetShedder reservedPctSetter, signingKey []byte) *Server {
	return &Server{pipeline: p, releaser: releaser, rateLimiter: rateLimiter, fleetShedder: fleetShedder, signingKey: signingKey}
}
```

Add the RPC method:
```go
func (s *Server) SetDynamicLimit(ctx context.Context, req *ratecapv1.SetDynamicLimitRequest) (*ratecapv1.SetDynamicLimitResponse, error) {
	switch req.Tier {
	case "rate_limiter":
		previous := s.rateLimiter.SetRate(int(req.Value))
		log.Printf("grpcserver: SetDynamicLimit: rate_limiter rate changed %d -> %d", previous, req.Value)
		return &ratecapv1.SetDynamicLimitResponse{Tier: req.Tier, PreviousValue: int32(previous), NewValue: req.Value}, nil
	case "fleet_shedder":
		previous, err := s.fleetShedder.SetReservedCriticalPct(int(req.Value))
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		log.Printf("grpcserver: SetDynamicLimit: fleet_shedder reserved_critical_pct changed %d -> %d", previous, req.Value)
		return &ratecapv1.SetDynamicLimitResponse{Tier: req.Tier, PreviousValue: int32(previous), NewValue: req.Value}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, `tier must be "rate_limiter" or "fleet_shedder"`)
	}
}
```

**`NewServer`'s signature change breaks every existing call site — update all of them:**
- `services/core/main.go`: `grpcserver.NewServer(pipeline, redisStore, []byte(concurrencySigningKey))` → `grpcserver.NewServer(pipeline, redisStore, rateLimiter, fleetShedder, []byte(concurrencySigningKey))` (both `rateLimiter` and `fleetShedder` are already local variables in `main()` from the existing `limiter.NewTokenBucketLimiter(...)`/`limiter.NewFleetShedder(...)` calls).
- `services/core/grpcserver/server_test.go`: every `grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey)` call site needs two more args. Add minimal fakes:
  ```go
  type fakeRateLimiter struct{ setCalls []int }
  func (f *fakeRateLimiter) SetRate(rate int) int { f.setCalls = append(f.setCalls, rate); return 100 }

  type fakeFleetShedder struct{ setCalls []int; err error }
  func (f *fakeFleetShedder) SetReservedCriticalPct(pct int) (int, error) {
  	if f.err != nil { return 0, f.err }
  	f.setCalls = append(f.setCalls, pct)
  	return 20, nil
  }
  ```
  and pass `&fakeRateLimiter{}, &fakeFleetShedder{}` at every existing `grpcserver.NewServer(...)` call site in this file (there are many — Phase 0/1's test file has ~15 test functions all constructing a `Server`; update every one mechanically, do not skip any).
- `services/core/grpcserver/auth_integration_test.go` and `mtls_integration_test.go`: check both for their own `NewServer(...)` call sites and update identically.

- [ ] **Step 8: Write new tests for the RPC itself**

Add to `services/core/grpcserver/server_test.go`:
```go
func TestSetDynamicLimit_RateLimiterTier_CallsSetRate(t *testing.T) {
	rl := &fakeRateLimiter{}
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, rl, &fakeFleetShedder{}, testSigningKey)

	resp, err := s.SetDynamicLimit(context.Background(), &ratecapv1.SetDynamicLimitRequest{Tier: "rate_limiter", Value: 500})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.NewValue != 500 {
		t.Errorf("expected NewValue=500, got %d", resp.NewValue)
	}
	if len(rl.setCalls) != 1 || rl.setCalls[0] != 500 {
		t.Errorf("expected SetRate(500) called once, got %v", rl.setCalls)
	}
}

func TestSetDynamicLimit_FleetShedderTier_CallsSetReservedCriticalPct(t *testing.T) {
	fs := &fakeFleetShedder{}
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, &fakeRateLimiter{}, fs, testSigningKey)

	resp, err := s.SetDynamicLimit(context.Background(), &ratecapv1.SetDynamicLimitRequest{Tier: "fleet_shedder", Value: 60})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.NewValue != 60 {
		t.Errorf("expected NewValue=60, got %d", resp.NewValue)
	}
	if len(fs.setCalls) != 1 || fs.setCalls[0] != 60 {
		t.Errorf("expected SetReservedCriticalPct(60) called once, got %v", fs.setCalls)
	}
}

func TestSetDynamicLimit_UnknownTier_ReturnsInvalidArgument(t *testing.T) {
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, &fakeRateLimiter{}, &fakeFleetShedder{}, testSigningKey)

	_, err := s.SetDynamicLimit(context.Background(), &ratecapv1.SetDynamicLimitRequest{Tier: "not_a_real_tier", Value: 1})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %v", status.Code(err))
	}
}

func TestSetDynamicLimit_FleetShedderOutOfRange_ReturnsInvalidArgument(t *testing.T) {
	fs := &fakeFleetShedder{err: fmt.Errorf("reserved_critical_pct must be between 0 and 100 inclusive, got 200")}
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, &fakeRateLimiter{}, fs, testSigningKey)

	_, err := s.SetDynamicLimit(context.Background(), &ratecapv1.SetDynamicLimitRequest{Tier: "fleet_shedder", Value: 200})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %v", status.Code(err))
	}
}
```
Add `"fmt"` to this test file's imports if not already present.

- [ ] **Step 9: Update `main.go`'s call site and run everything**

Run: `cd services/core && go build ./... && go test ./... -race`

- [ ] **Step 10: Commit**

```bash
git add proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go services/core/grpcserver/auth_integration_test.go services/core/grpcserver/mtls_integration_test.go services/core/limiter/tokenbucket.go services/core/limiter/tokenbucket_test.go services/core/limiter/fleetshedder.go services/core/limiter/fleetshedder_test.go services/core/main.go
git commit -m "feat(core,proto): add SetDynamicLimit RPC for sub-second Tier 1/3 limit changes"
```

---

### Task 13: Sidecar admin secret + `/admin/set-limit` endpoint

**Files:**
- Create: `services/sidecar/admin/admin.go`, `services/sidecar/admin/admin_test.go`
- Modify: `services/sidecar/main.go`, `SECURITY.md`

**Interfaces:**
- Produces: `admin.NewHandler(client adminClient, secret string) *Handler` where `adminClient` is a narrow interface wrapping `SetDynamicLimit`.

- [ ] **Step 1: Write the failing tests**

```go
// services/sidecar/admin/admin_test.go
package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/admin"
)

type fakeAdminClient struct {
	lastReq *ratecapv1.SetDynamicLimitRequest
	resp    *ratecapv1.SetDynamicLimitResponse
	err     error
}

func (f *fakeAdminClient) SetDynamicLimit(ctx context.Context, in *ratecapv1.SetDynamicLimitRequest, opts ...grpc.CallOption) (*ratecapv1.SetDynamicLimitResponse, error) {
	f.lastReq = in
	return f.resp, f.err
}

func TestServeHTTP_RejectsMissingAdminSecret(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a missing admin secret, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected the request to never reach core when the admin secret is missing")
	}
}

func TestServeHTTP_RejectsWrongAdminSecret(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "wrong-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a wrong admin secret, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected the request to never reach core when the admin secret is wrong")
	}
}

func TestServeHTTP_ForwardsValidRequestToCore(t *testing.T) {
	client := &fakeAdminClient{resp: &ratecapv1.SetDynamicLimitResponse{Tier: "rate_limiter", PreviousValue: 100, NewValue: 500}}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastReq == nil || client.lastReq.Tier != "rate_limiter" || client.lastReq.Value != 500 {
		t.Errorf("expected forwarded request {rate_limiter, 500}, got %+v", client.lastReq)
	}
}

func TestServeHTTP_RejectsNonPostMethod(t *testing.T) {
	client := &fakeAdminClient{}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodGet, "/admin/set-limit", nil)
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestServeHTTP_PropagatesCoreErrorAsBadRequest(t *testing.T) {
	client := &fakeAdminClient{err: context.DeadlineExceeded}
	h := admin.NewHandler(client, "correct-secret")

	req := httptest.NewRequest(http.MethodPost, "/admin/set-limit", strings.NewReader(`{"tier":"rate_limiter","value":500}`))
	req.Header.Set("X-RateCap-Admin-Secret", "correct-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when the upstream call fails, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./admin/... -race -v`
Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Implement**

```go
// services/sidecar/admin/admin.go
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"
)

type adminClient interface {
	SetDynamicLimit(ctx context.Context, in *ratecapv1.SetDynamicLimitRequest, opts ...grpc.CallOption) (*ratecapv1.SetDynamicLimitResponse, error)
}

type Handler struct {
	client adminClient
	secret string
}

func NewHandler(client adminClient, secret string) *Handler {
	return &Handler{client: client, secret: secret}
}

type setLimitBody struct {
	Tier  string `json:"tier"`
	Value int32  `json:"value"`
}

// ServeHTTP checks X-RateCap-Admin-Secret with constant-time comparison
// (matching this repo's existing pattern in core/auth and grpcserver's
// token verification) BEFORE ever forwarding to core — a capability with
// fleet-wide, unbounded blast radius gets its own gate, not a reuse of the
// general sidecar<->core shared secret, per the explicit design decision
// this task implements.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provided := r.Header.Get("X-RateCap-Admin-Secret")
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(h.secret)) != 1 {
		log.Printf("sidecar: /admin/set-limit: rejected request with missing/invalid admin secret")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body setLimitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := h.client.SetDynamicLimit(r.Context(), &ratecapv1.SetDynamicLimitRequest{Tier: body.Tier, Value: body.Value})
	if err != nil {
		log.Printf("sidecar: /admin/set-limit: upstream call failed: %v", err)
		http.Error(w, "upstream call failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./admin/... -race -v`

- [ ] **Step 5: Wire into `main.go`**

Add the required env var check (fail-closed at startup, matching `RATECAP_SHARED_SECRET`'s existing pattern) right after the existing `sharedSecret` check:
```go
	adminSecret := os.Getenv("RATECAP_ADMIN_SECRET")
	if adminSecret == "" {
		log.Fatalf("RATECAP_ADMIN_SECRET must be set — ratecap-sidecar refuses to start without the admin-lever endpoint's own authentication configured")
	}
```
Extend `newTopMux` (from Phase 1 Task 9) with one more untouched-by-throttle route — an incident-response lever must not be blockable by the very limiter it might need to adjust:
```go
func newTopMux(protected http.Handler, limiter *ratelimit.Limiter, metricsHandler http.Handler, healthz http.HandlerFunc, adminHandler http.Handler) *http.ServeMux {
	throttled := ratelimit.Middleware(limiter, protected)

	mux := http.NewServeMux()
	mux.Handle("/check", throttled)
	mux.Handle("/release", throttled)
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/admin/set-limit", adminHandler)
	return mux
}
```
Update the call site:
```go
	handler := newTopMux(protectedMux, limiter, metrics.Handler(), newHealthzHandler(conn), admin.NewHandler(client, adminSecret))
```
Add the import `"github.com/ratecap/sidecar/admin"`.

Update `services/sidecar/main_test.go`'s `TestNewTopMux_*` tests (from Phase 1 Task 9) to pass a fifth argument — a trivial `http.HandlerFunc` stub is sufficient since those tests don't exercise the admin path.

- [ ] **Step 6: Run the full sidecar suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race`

- [ ] **Step 7: Document in `SECURITY.md`**

Add a subsection (find the right insertion point via `grep -n "^## " SECURITY.md` first):
```markdown
## Admin lever (`/admin/set-limit`)

A sub-second incident-response endpoint for changing Tier 1's rate or Tier 3's `reserved_critical_pct` fleet-wide without a config re-parse. Gated by its own secret, `RATECAP_ADMIN_SECRET`, checked at the sidecar's HTTP layer via the `X-RateCap-Admin-Secret` header — deliberately separate from `RATECAP_SHARED_SECRET`, since this capability has fleet-wide, effectively unbounded blast radius (unlike `/check`, which is self-bounding). A leaked admin secret lets an attacker disable rate limiting or fleet load-shedding fleet-wide in one call; rotate it independently of the general shared secret if either is suspected of leaking.

This endpoint is bound by the same network-level trust boundary as the rest of the sidecar's HTTP surface (see "Trust Boundary" above) — the admin secret is defense-in-depth on top of that, not a replacement for running RateCap on a private, trusted network.
```

- [ ] **Step 8: Commit**

```bash
git add services/sidecar/admin/admin.go services/sidecar/admin/admin_test.go services/sidecar/main.go services/sidecar/main_test.go SECURITY.md
git commit -m "feat(sidecar): add authenticated /admin/set-limit endpoint for the incident-response lever"
```

---

### Task 14: CLI — `ratecapctl admin set-limit`

**Files:**
- Create: `cli/cmd/admin.go`, `cli/cmd/admin_test.go`
- Modify: `cli/cmd/root.go`

**Interfaces:**
- Produces: `ratecapctl admin set-limit --sidecar-addr <addr> --admin-secret <secret> --tier <tier> --value <n>`.

- [ ] **Step 1: Write the failing test**

```go
// cli/cmd/admin_test.go
package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminSetLimit_SendsCorrectRequestAndPrintsResult(t *testing.T) {
	var receivedBody map[string]any
	var receivedSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSecret = r.Header.Get("X-RateCap-Admin-Secret")
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tier": "rate_limiter", "previous_value": 100, "new_value": 500})
	}))
	defer server.Close()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"admin", "set-limit", "--sidecar-addr", server.URL, "--admin-secret", "test-secret", "--tier", "rate_limiter", "--value", "500"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedSecret != "test-secret" {
		t.Errorf("expected admin secret header to be forwarded, got %q", receivedSecret)
	}
	if receivedBody["tier"] != "rate_limiter" || receivedBody["value"].(float64) != 500 {
		t.Errorf("expected request body {tier: rate_limiter, value: 500}, got %v", receivedBody)
	}
	if out.String() == "" {
		t.Error("expected some output confirming the result")
	}
}

func TestAdminSetLimit_RequiresTierFlag(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"admin", "set-limit", "--value", "500"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	if err := root.Execute(); err == nil {
		t.Error("expected an error when --tier is not provided")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cli && go test ./cmd/... -race -run TestAdminSetLimit -v`

- [ ] **Step 3: Implement**

```go
// cli/cmd/admin.go
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	adminCmd := &cobra.Command{
		Use:   "admin",
		Short: "Incident-response admin commands",
	}
	adminCmd.AddCommand(newAdminSetLimitCmd())
	return adminCmd
}

func newAdminSetLimitCmd() *cobra.Command {
	var sidecarAddr string
	var adminSecret string
	var tier string
	var value int

	cmd := &cobra.Command{
		Use:   "set-limit",
		Short: "Instantly change Tier 1's rate or Tier 3's reserved_critical_pct fleet-wide",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := json.Marshal(map[string]any{"tier": tier, "value": value})
			if err != nil {
				return err
			}

			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, sidecarAddr+"/admin/set-limit", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("X-RateCap-Admin-Secret", adminSecret)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("calling sidecar: %w", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, respBody)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", respBody)
			return nil
		},
	}

	cmd.Flags().StringVar(&sidecarAddr, "sidecar-addr", "http://localhost:8080", "target sidecar address")
	cmd.Flags().StringVar(&adminSecret, "admin-secret", "", "admin secret (or set RATECAP_ADMIN_SECRET)")
	cmd.Flags().StringVar(&tier, "tier", "", "tier to change: rate_limiter or fleet_shedder")
	cmd.Flags().IntVar(&value, "value", 0, "new value: rate (rate_limiter) or reserved_critical_pct (fleet_shedder)")
	_ = cmd.MarkFlagRequired("tier")
	_ = cmd.MarkFlagRequired("value")

	return cmd
}
```

In `cli/cmd/root.go`, add:
```go
	root.AddCommand(newAdminCmd())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cli && go build ./... && go test ./... -race -v`

- [ ] **Step 5: Commit**

```bash
git add cli/cmd/admin.go cli/cmd/admin_test.go cli/cmd/root.go
git commit -m "feat(cli): add ratecapctl admin set-limit for the incident-response lever"
```

---

### Task 15: Redis-down degradation contract cross-check (closes item 3)

**Files:**
- Modify: `ARCHITECTURE.md`

**Context:** item 3 asked to "document and test" the per-tier Redis-down contract. Phase 1 already documented it (ARCHITECTURE.md's "Redis-down degradation contract" subsection); Task 1 of this phase adds the real Toxiproxy-based tests proving it. This task is the small remaining piece: cross-link the two so a reader of the doc knows where the enforcing tests live.

- [ ] **Step 1: Update the doc**

In `ARCHITECTURE.md`'s "Redis-down degradation contract" subsection (added in Phase 1), append one line:
```markdown
Enforced by real network-fault-injection tests (Toxiproxy, not mocks) in `services/core/reliability/redis_failure_test.go` — `TestTier1_RedisUnavailable_FailsOpen`, `TestTier2_RedisUnavailable_FailsClosed`, `TestTier3_RedisUnavailable_FailsClosed`.
```

- [ ] **Step 2: Commit**

```bash
git add ARCHITECTURE.md
git commit -m "docs: cross-link the Redis-down degradation contract to its enforcing tests"
```

---

### Task 16: Version bump to v2.6.0

**Files:**
- Modify: `VERSION`, `CHANGELOG.md`

- [ ] **Step 1: Add the CHANGELOG entry**

Insert above the current top heading:
```markdown
## [2.6.0] — 2026-08-28 — Phase 2 Reliability & Testing Hardening

Minor release: Phase 2 of the v3 upgrade roadmap — RateCap's core architectural claims (fail-open/fail-closed, atomicity, no-flapping) are now tested invariants instead of assumptions, Redis can run HA via Sentinel, and on-call has a sub-second incident-response lever.

### Added

- Toxiproxy-based real-network-fault regression tests proving Tier 1 fails open and Tiers 2/3 fail closed on a Redis outage (`services/core/reliability`).
- Race regression tests for the priority-partition-at-capacity bug class (Tier 2 and Tier 3) and a concurrent stress test for `services/sidecar/decisionlog`.
- Property-based tests (`pgregory.net/rapid`) for `TokenBucketLimiter` and `ConcurrencyLimiter`.
- Fault-injection tests for the fsnotify config hot-reload path (partial writes, atomic rename-swap, delete+recreate).
- `Config.Hash()` and `ratecap_core_config_version_info{hash}` for detecting cross-replica config divergence during a rollout.
- Coverage measurement + a 50% floor CI gate across all Go modules and the Python SDK; a Gremlins mutation-testing CI gate scoped to `services/core/limiter` and `packages/sdks/go`.
- Opt-in Redis Sentinel support: `RATECAP_REDIS_SENTINEL_ADDRS`/`RATECAP_REDIS_SENTINEL_MASTER_NAME` on `ratecap-core`, and a `redis.sentinel.enabled` Helm values flag (default `false`).
- Gradual Tier 4 shed-curve ramping (`RATECAP_SHED_RAMP_START_PCT`), replacing a hard on/off cutoff.
- A new `SetDynamicLimit` gRPC RPC and a sidecar `/admin/set-limit` HTTP endpoint (gated by a new, separate `RATECAP_ADMIN_SECRET`) for instantly changing Tier 1's rate or Tier 3's `reserved_critical_pct` fleet-wide without a config re-parse — plus `ratecapctl admin set-limit`.

### Security

- The new admin lever requires its own secret (`RATECAP_ADMIN_SECRET`), separate from `RATECAP_SHARED_SECRET`, given its fleet-wide and effectively unbounded blast radius compared to a normal `/check` call.
```

- [ ] **Step 2: Bump `VERSION`**

```
2.6.0
```

- [ ] **Step 3: Commit**

```bash
git add VERSION CHANGELOG.md
git commit -m "chore: bump VERSION to 2.6.0 for Phase 2"
```

---

## Post-Implementation (not a task — controller responsibility)

After all 16 tasks pass task review and the final whole-branch review is clean: push the branch (the user runs `git push` themselves due to the `DestructiveGuard` hook), open a PR into `develop` titled `feat: RateCap v3 roadmap Phase 2 — Reliability & Testing Hardening`, merge once CI is green, then promote `develop` → `main` and tag/release `v2.6.0` — mirroring exactly the Phase 1 cycle.
