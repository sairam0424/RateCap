# RateCap v3 Roadmap — Phase 0: Housekeeping & Quick Wins — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the cheapest, highest-confidence fixes from the v3 upgrade roadmap first — a real fleet-coordination bug, a benchmark measurement bug, two pre-existing unmerged branches, and the versioning-process cleanup that every later phase's release depends on.

**Architecture:** No new components. Two are correctness fixes to existing Go code (`services/core/limiter/concurrency.go`, `cli/cmd/bench_run.go`), two are merges of pre-existing feature branches, two are new config/doc files (`.github/dependabot.yml`, `VERSION`), one is a CHANGELOG backfill, and the last is the actual git tag/release that captures everything this plan produces.

**Tech Stack:** Go 1.26.2, Cobra CLI, `go test -race`, git, GitHub CLI (`gh`).

**Spec:** `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md` (Phase 0 section, items 1-11)

## Global Constraints

- Every change must keep `go build ./...` and `go test ./... -race` green in every affected module (per `CLAUDE.md`).
- `-race` is mandatory on every test run that touches `ConcurrencyLimiter` — a previous data race in this exact area (`TokenBucketLimiter.Reconfigure`) was caught only this way.
- No comments except non-obvious WHY (hidden constraints, subtle invariants) — matches `CLAUDE.md`'s documented convention.
- Never commit directly to `main` or `develop` — this plan executes on `feature/v3-upgrade-roadmap` (already branched from `develop`).
- Files: 200-400 lines typical, 800 max.

---

### Task 1: Fix Tier 2's bounded-queueing backlog counter to be Redis-backed

**Files:**
- Modify: `services/core/limiter/concurrency.go`
- Test: `services/core/limiter/concurrency_queue_test.go` (add new test, rewrite one existing test)

**Interfaces:**
- Consumes: `concurrencyChecker` interface (`IncrConcurrent`/`DecrConcurrent`, unchanged) — same interface Tier 2's real concurrency slot already uses.
- Produces: `ConcurrencyLimiter.acquireBacklogSlot(ctx, key string, maxBacklog int, maxQueueWaitMs int64) (bool, string, error)` — new signature (was `acquireBacklogSlot(maxBacklog int) bool`). `BacklogDepth()` is removed entirely (confirmed via repo-wide grep: only referenced in this file and in the one test rewritten below).

- [ ] **Step 1: Write the failing cross-instance test**

Add to `services/core/limiter/concurrency_queue_test.go`:

```go
// TestConcurrencyLimiter_BacklogSharedAcrossInstances proves the backlog
// ceiling is enforced fleet-wide (shared across every ConcurrencyLimiter
// instance backed by the same store — e.g. every ratecap-core replica),
// not per-instance. Before the fix, each instance tracked its own local
// atomic.Int64 counter, so a second instance had no visibility into the
// first instance's admissions and would wrongly believe it had room.
func TestConcurrencyLimiter_BacklogSharedAcrossInstances(t *testing.T) {
	const maxBacklog = 3
	const maxQueueWaitMs = 2000
	fs := newFakeConcurrencyStore()
	l1 := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, maxQueueWaitMs, 10)
	l2 := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, maxQueueWaitMs, 10)
	ctx := context.Background()

	// Fill the real concurrency slot so every Check() call falls through to
	// backlog admission instead of an immediate ALLOW, and nothing ever
	// frees it (so a backlog-admitted request just waits until timeout).
	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	// Fill l1's backlog to maxBacklog. Give the goroutines time to acquire
	// their slots before l2 tries — this ordering is what makes the
	// assertion below deterministic.
	var wg sync.WaitGroup
	for i := 0; i < maxBacklog; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l1.Check(ctx, limiter.Request{Key: "k"})
		}()
	}
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	d, err := l2.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected l2 to be rejected once l1 filled the shared backlog, got %v", d.Action)
	}
	// If backlog accounting is per-instance (the bug), l2 wrongly believes
	// it has room and polls for the full maxQueueWaitMs before timing out.
	// If it's shared (the fix), l2's own admission attempt fails
	// immediately against the already-full shared counter.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected l2 to reject immediately (shared backlog full) rather than poll for maxQueueWaitMs (%dms); took %v — backlog accounting is not fleet-wide", maxQueueWaitMs, elapsed)
	}

	wg.Wait()
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/core && go test ./limiter/... -run TestConcurrencyLimiter_BacklogSharedAcrossInstances -v`
Expected: FAIL — `elapsed` will be close to 2000ms (l2 polls for the full `maxQueueWaitMs` before timing out), so the `elapsed > 200*time.Millisecond` check fails. This is today's real bug: `l2`'s independent `backlog atomic.Int64` field is 0, so its own `acquireBacklogSlot(maxBacklog=3)` trivially succeeds even though `l1` already has 3 waiters in backlog.

- [ ] **Step 3: Implement the fix**

Replace `services/core/limiter/concurrency.go` in full:

```go
package limiter

import (
	"context"
	"math"
	"sync"
	"time"
)

// unboundedCap is passed as the Lua script's cap argument to force its
// `count < cap` check to always pass, so IncrConcurrent still reserves a
// slot even when the real cap is already exceeded. Used only for shadow
// mode's would-be-reject path, where the design spec requires the slot to
// still be reserved so concurrency accounting stays accurate. MaxInt32 is
// chosen to be far larger than any real concurrency count while staying
// well under Lua 5.1's 2^53 integer-precision limit for tonumber().
const unboundedCap = math.MaxInt32

// backlogKeyPrefix namespaces Tier 2's bounded-queueing backlog counter in
// the shared store, distinct from the real per-key concurrency-slot key
// (bare req.Key), so the two counters never collide on the same key and so
// the backlog ceiling is enforced fleet-wide across every
// ConcurrencyLimiter instance sharing this store — not per-instance.
const backlogKeyPrefix = "backlog:"

type concurrencyChecker interface {
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error)
	DecrConcurrent(ctx context.Context, key, token string) error
}

type ConcurrencyLimiter struct {
	store concurrencyChecker

	mu              sync.RWMutex
	cap             int
	maxDurationMs   int64
	shadowMode      bool
	queueingEnabled bool
	maxBacklog      int
	maxQueueWaitMs  int64
	pollIntervalMs  int64
}

func NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		store:           s,
		cap:             cap,
		maxDurationMs:   maxDurationMs,
		shadowMode:      shadowMode,
		queueingEnabled: queueingEnabled,
		maxBacklog:      maxBacklog,
		maxQueueWaitMs:  maxQueueWaitMs,
		pollIntervalMs:  pollIntervalMs,
	}
}

// Reconfigure and Check run concurrently in ratecap-core: Reconfigure is
// invoked from the config watcher's goroutine while Check runs on every
// gRPC handler goroutine. The mutex keeps a reload from tearing
// cap/maxDurationMs apart mid-read, matching the design spec's
// atomic-hot-reload requirement (the same pattern TokenBucketLimiter uses).
func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
	l.queueingEnabled = queueingEnabled
	l.maxBacklog = maxBacklog
	l.maxQueueWaitMs = maxQueueWaitMs
	l.pollIntervalMs = pollIntervalMs
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, maxDurationMs, shadowMode := l.cap, l.maxDurationMs, l.shadowMode
	queueingEnabled, maxBacklog, maxQueueWaitMs, pollIntervalMs := l.queueingEnabled, l.maxBacklog, l.maxQueueWaitMs, l.pollIntervalMs
	l.mu.RUnlock()

	allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: req.Key, Token: token}}, Tier: "concurrency_limiter"}, nil
	}

	// Shadow mode's entire purpose is to observe without ever blocking a real
	// caller, so it takes precedence over queueing and skips it entirely.
	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, req.Key, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: req.Key, Token: reservedToken}}, Tier: "concurrency_limiter"}, nil
	}

	if !queueingEnabled {
		return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
	}

	backlogAllowed, backlogToken, err := l.acquireBacklogSlot(ctx, req.Key, maxBacklog, maxQueueWaitMs)
	if err != nil {
		return Decision{}, err
	}
	if !backlogAllowed {
		return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
	}
	defer func() {
		// Best-effort release: a lost DecrConcurrent (e.g. context canceled
		// concurrently with this defer) self-heals via the backlog key's own
		// reap deadline (maxQueueWaitMs, passed as IncrConcurrent's
		// maxDurationMs above) — the same safety net the real concurrency
		// slot already relies on, so the error is deliberately not retried.
		_ = l.store.DecrConcurrent(ctx, backlogKeyPrefix+req.Key, backlogToken)
	}()

	return l.pollUntilAllowedOrDeadline(ctx, req, cap, maxDurationMs, maxQueueWaitMs, pollIntervalMs)
}

// acquireBacklogSlot reserves a backlog slot in the shared store under a
// namespaced key, so the backlog ceiling is fleet-wide across every
// ConcurrencyLimiter instance sharing this store (e.g. every ratecap-core
// replica) — not per-instance, which was the bug this replaces.
func (l *ConcurrencyLimiter) acquireBacklogSlot(ctx context.Context, key string, maxBacklog int, maxQueueWaitMs int64) (bool, string, error) {
	return l.store.IncrConcurrent(ctx, backlogKeyPrefix+key, maxBacklog, maxQueueWaitMs)
}

func (l *ConcurrencyLimiter) pollUntilAllowedOrDeadline(ctx context.Context, req Request, cap int, maxDurationMs, maxQueueWaitMs, pollIntervalMs int64) (Decision, error) {
	deadline := time.NewTimer(time.Duration(maxQueueWaitMs) * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Duration(pollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-deadline.C:
			return Decision{Action: REJECT_429, RetryAfterMs: maxDurationMs, Tier: "concurrency_limiter"}, nil
		case <-ticker.C:
			allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
			if err != nil {
				return Decision{}, err
			}
			if allowed {
				return Decision{Action: QUEUE, Reservations: []TokenReservation{{Key: req.Key, Token: token}}, Tier: "concurrency_limiter"}, nil
			}
		}
	}
}
```

- [ ] **Step 4: Rewrite the one existing test that used `BacklogDepth()`**

In `services/core/limiter/concurrency_queue_test.go`, replace `TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog` (it calls `l.BacklogDepth()`, which no longer exists) with:

```go
// TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog hammers a small
// MaxBacklog (3) with 50 concurrent waiters against a permanently-full cap,
// sampling the shared store's backlog key directly (BacklogDepth() was
// removed along with the per-instance local counter it reported on).
func TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog(t *testing.T) {
	const maxBacklog = 3
	const goroutines = 50
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, maxBacklog, 200, 5)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Check(ctx, limiter.Request{Key: "k"})
		}()
	}

	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		for i := 0; i < 100; i++ {
			fs.mu.Lock()
			depth := int64(fs.tokens["backlog:k"])
			fs.mu.Unlock()
			if depth > peak.Load() {
				peak.Store(depth)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	<-sampleDone

	if peak.Load() > maxBacklog {
		t.Fatalf("backlog peaked at %d, exceeding maxBacklog %d — overshoot", peak.Load(), maxBacklog)
	}

	fs.mu.Lock()
	finalDepth := fs.tokens["backlog:k"]
	fs.mu.Unlock()
	if finalDepth != 0 {
		t.Fatalf("expected backlog to return to 0 after all goroutines finished, got %d", finalDepth)
	}
}
```

- [ ] **Step 5: Run the full limiter test suite to verify everything passes**

Run: `cd services/core && go test ./limiter/... -race -v`
Expected: PASS — all tests in the package, including the new `TestConcurrencyLimiter_BacklogSharedAcrossInstances` and the rewritten `TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog`.

- [ ] **Step 6: Commit**

```bash
git add services/core/limiter/concurrency.go services/core/limiter/concurrency_queue_test.go
git commit -m "fix(core): make Tier 2's bounded-queueing backlog fleet-wide

The backlog counter was a per-instance atomic.Int64 — the only non-Redis
local-state field in services/core. With N core replicas, the real
backlog ceiling was maxBacklog*N, not one shared ceiling. Now routes
through the same store.IncrConcurrent/DecrConcurrent the real concurrency
slot already uses, under a backlog: key prefix, so the ceiling is
enforced across every replica sharing the store."
```

---

### Task 2: Fix `bench_run.go`'s silent-failure measurement bug

**Files:**
- Modify: `cli/cmd/bench_run.go`
- Test: `cli/cmd/bench_run_test.go` (add new test)

**Interfaces:**
- Produces: `benchResult` gains `Accepted`, `Rejected`, `Errored int` fields (JSON tags `accepted`/`rejected`/`errored`). `P50Ms`/`P99Ms`/`P999Ms` now compute over accepted-only latencies.

- [ ] **Step 1: Write the failing test**

Add to `cli/cmd/bench_run_test.go`:

```go
func TestBenchRun_TracksAcceptedRejectedAndErroredSeparately(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		switch count % 3 {
		case 0:
			w.WriteHeader(http.StatusOK)
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"bench", "run", "--sidecar-addr", server.URL, "--requests", "9", "--concurrency", "1", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, out.String())
	}

	accepted, ok := result["accepted"].(float64)
	if !ok || accepted != 3 {
		t.Errorf("expected 3 accepted (every 3rd of 9 requests returns 200), got %v (present=%v)", accepted, ok)
	}
	rejected, ok := result["rejected"].(float64)
	if !ok || rejected != 6 {
		t.Errorf("expected 6 rejected (429+503 responses), got %v (present=%v)", rejected, ok)
	}
	errored, ok := result["errored"].(float64)
	if !ok || errored != 0 {
		t.Errorf("expected 0 errored (no transport failures in this test), got %v (present=%v)", errored, ok)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd cli && go test ./cmd/... -run TestBenchRun_TracksAcceptedRejectedAndErroredSeparately -v`
Expected: FAIL — `result["accepted"]` is absent from today's JSON output (`ok` is `false`), since `benchResult` has no such field yet.

- [ ] **Step 3: Implement the fix**

Replace `cli/cmd/bench_run.go` in full:

```go
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	ratecap "github.com/ratecap/sdk-go"
)

type benchResult struct {
	TotalRequests int     `json:"total_requests"`
	Accepted      int     `json:"accepted"`
	Rejected      int     `json:"rejected"`
	Errored       int     `json:"errored"`
	ElapsedMs     int64   `json:"elapsed_ms"`
	ThroughputRPS float64 `json:"throughput_rps"`
	P50Ms         float64 `json:"p50_ms"`
	P99Ms         float64 `json:"p99_ms"`
	P999Ms        float64 `json:"p999_ms"`
}

type benchOutcome struct {
	elapsed time.Duration
	kind    string // "accepted", "rejected", or "errored"
}

func newBenchRunCmd() *cobra.Command {
	var sidecarAddr string
	var concurrency int
	var requests int
	var keyPrefix string
	var useAcquire bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive concurrent load against a running sidecar and report latency percentiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			result := runBench(cmd.Context(), sidecarAddr, concurrency, requests, keyPrefix, useAcquire)
			if jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Total requests: %d\n", result.TotalRequests)
			fmt.Fprintf(cmd.OutOrStdout(), "Accepted: %d  Rejected: %d  Errored: %d\n", result.Accepted, result.Rejected, result.Errored)
			fmt.Fprintf(cmd.OutOrStdout(), "Elapsed: %dms\n", result.ElapsedMs)
			fmt.Fprintf(cmd.OutOrStdout(), "Throughput: %.1f req/s\n", result.ThroughputRPS)
			fmt.Fprintf(cmd.OutOrStdout(), "P50: %.2fms  P99: %.2fms  P99.9: %.2fms (accepted requests only)\n", result.P50Ms, result.P99Ms, result.P999Ms)
			return nil
		},
	}

	cmd.Flags().StringVar(&sidecarAddr, "sidecar-addr", "http://localhost:8080", "target sidecar address")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "number of parallel workers")
	cmd.Flags().IntVar(&requests, "requests", 1000, "total number of requests across all workers")
	cmd.Flags().StringVar(&keyPrefix, "key-prefix", "bench", "prefix for generated request keys")
	cmd.Flags().BoolVar(&useAcquire, "acquire", false, "use Acquire()/Release() (tier 2) instead of Allow() (tier 1)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON instead of a human-readable summary")

	return cmd
}

func runBench(ctx context.Context, sidecarAddr string, concurrency, requests int, keyPrefix string, useAcquire bool) benchResult {
	client := ratecap.NewClient(sidecarAddr)

	var mu sync.Mutex
	var outcomes []benchOutcome

	var wg sync.WaitGroup
	jobs := make(chan int, requests)
	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)

	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seq := range jobs {
				key := fmt.Sprintf("%s-%d-%d", keyPrefix, workerID, seq)
				reqStart := time.Now()
				kind := "accepted"
				if useAcquire {
					ticket, err := client.Acquire(ctx, key)
					switch {
					case err != nil:
						kind = "errored"
					case !ticket.Allowed:
						kind = "rejected"
					default:
						ticket.Release(ctx)
					}
				} else {
					allowed, _, err := client.Allow(ctx, key)
					switch {
					case err != nil:
						kind = "errored"
					case !allowed:
						kind = "rejected"
					}
				}
				elapsed := time.Since(reqStart)
				mu.Lock()
				outcomes = append(outcomes, benchOutcome{elapsed: elapsed, kind: kind})
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	totalElapsed := time.Since(start)

	var accepted, rejected, errored int
	var acceptedLatencies []time.Duration
	for _, o := range outcomes {
		switch o.kind {
		case "accepted":
			accepted++
			acceptedLatencies = append(acceptedLatencies, o.elapsed)
		case "rejected":
			rejected++
		case "errored":
			errored++
		}
	}
	sort.Slice(acceptedLatencies, func(i, j int) bool { return acceptedLatencies[i] < acceptedLatencies[j] })

	return benchResult{
		TotalRequests: len(outcomes),
		Accepted:      accepted,
		Rejected:      rejected,
		Errored:       errored,
		ElapsedMs:     totalElapsed.Milliseconds(),
		ThroughputRPS: float64(len(outcomes)) / totalElapsed.Seconds(),
		P50Ms:         percentileMs(acceptedLatencies, 0.50),
		P99Ms:         percentileMs(acceptedLatencies, 0.99),
		P999Ms:        percentileMs(acceptedLatencies, 0.999),
	}
}

func percentileMs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return float64(sorted[idx].Microseconds()) / 1000.0
}
```

- [ ] **Step 4: Run the full CLI test suite to verify everything passes**

Run: `cd cli && go test ./cmd/... -race -v`
Expected: PASS — the new test plus all 4 pre-existing `bench_run_test.go` tests (`TestBenchRun_AllModeReportsAllRequestsAgainstFakeSidecar`, `TestBenchRun_JSONModeEmitsValidJSONWithExpectedFields`, `TestBenchRun_AcquireFlagUsesCheckThenReleaseFlow`, `TestBenchRun_KeyPrefixIsUsedInGeneratedKeys`) — none of those hit a non-200 response, so `accepted` still equals their expected `total_requests` in every case.

- [ ] **Step 5: Commit**

```bash
git add cli/cmd/bench_run.go cli/cmd/bench_run_test.go
git commit -m "fix(cli): stop mixing accepted/rejected/errored latencies in bench run

Allow()'s allowed/err and Acquire()'s err were discarded entirely, so a
429, a 503, and a fully successful request all landed in the same
P50/P99/P99.9 array with no distinguishing signal. Now tracks each
outcome separately; percentiles are computed over accepted latencies
only, and accepted/rejected/errored counts are reported alongside them."
```

---

### Task 3: Merge `fix/v3-config-validation`

**Files:**
- Merge: `services/core/config/config.go`, `services/core/config/config_test.go` (from the existing branch, no new authorship)

**Interfaces:**
- No interface changes — this branch adds validation only, to the existing `Config.Validate()` method.

- [ ] **Step 1: Inspect the branch one more time immediately before merging**

Run: `git log develop..fix/v3-config-validation --oneline` and `git diff develop...fix/v3-config-validation -- services/core/config/`
Expected: still exactly the single commit `18caa61 fix(core): validate Tier 1 rate_limiter config, matching Tier 2/3`, unchanged since the spec-writing investigation.

- [ ] **Step 2: Merge into the current branch**

```bash
git merge --no-ff fix/v3-config-validation -m "merge: fix/v3-config-validation into v3 roadmap Phase 0

Adds Tier 1 rate_limiter config validation, matching what Tier 2/3
already had — closes the 'Config.Validate() never checks Tier 1'
gap referenced throughout the v3 upgrade roadmap. Pre-existing,
already-reviewed work; merged as-is rather than re-implemented."
```

- [ ] **Step 3: Run the full core test suite to verify no regressions**

Run: `cd services/core && go test ./... -race -v`
Expected: PASS, including the new Tier 1 validation tests this branch adds.

---

### Task 4: Merge `fix/v3-breaking-wire-changes`

**Files:**
- Merge: `proto/ratecap/v1/ratecap.proto`, `proto/ratecap/v1/ratecap.pb.go`, `services/core/config/config.go`, `services/core/config/config_test.go`, `services/core/grpcserver/server.go`, `services/core/grpcserver/server_test.go`, `services/core/health_main_test.go`, `services/core/main_test.go`, `cli/cmd/config_validate_test.go`, `deploy/helm/ratecap/values.yaml`, `deploy/ratecap-bench.yaml`, `deploy/ratecap.yaml`, `ARCHITECTURE.md`, `SECURITY.md` (from the existing branch, no new authorship)

**Interfaces:**
- Produces: `ratecapv1.Priority_PRIORITY_UNSPECIFIED` (new enum value, 0). `ratecapv1.Priority_SHEDDABLE`/`Priority_CRITICAL` are renumbered 1/2 (were 0/1) — **breaking wire-format change**, called out explicitly in this task rather than shipped silently.

- [ ] **Step 1: Inspect the branch one more time immediately before merging**

Run: `git log develop..fix/v3-breaking-wire-changes --oneline` and `git diff develop...fix/v3-breaking-wire-changes --stat`
Expected: still exactly the two commits (`b95490f`, `1896f69`) and the 14-file, 70-insertion/37-deletion diff confirmed during spec-writing, unchanged.

- [ ] **Step 2: Merge into the current branch**

```bash
git merge --no-ff fix/v3-breaking-wire-changes -m "merge: fix/v3-breaking-wire-changes into v3 roadmap Phase 0

Adds PRIORITY_UNSPECIFIED=0 sentinel to the Priority enum (renumbering
SHEDDABLE/CRITICAL to 1/2), and deletes the dead
FleetShedderConfig.DefaultPriority config field. Server-side behavior is
unchanged (both PRIORITY_UNSPECIFIED and SHEDDABLE still map to
limiter.Sheddable) but the wire format is renumbered — this IS a
breaking change and must be called out explicitly in the v2.4.0 release
notes (Task 8), not shipped silently the way v2.3.2's breaking patch
bump was."
```

- [ ] **Step 3: Run the full core, cli, and proto test suites to verify no regressions**

Run: `cd proto && go build ./... && cd ../services/core && go test ./... -race -v && cd ../../cli && go test ./... -race -v`
Expected: PASS across all three modules.

---

### Task 5: Establish one authoritative version source

**Files:**
- Create: `VERSION`
- Modify: `README.md` (Status section), `SECURITY.md` (Supported Versions table)

**Interfaces:**
- None (pure docs/config, no code interface).

- [ ] **Step 1: Create the VERSION file**

Create `VERSION` with exactly this content (the current, real latest tag — `v2.4.0` doesn't exist yet; that's Task 8's job):

```
2.3.2
```

- [ ] **Step 2: Fix README.md's Status section to stop contradicting itself**

In `README.md`, find:

```markdown
## Status

**v1.0.0 — complete.** All four of Stripe's tiers are implemented, live-e2e-verified, and audited end-to-end: Tier 1 (Request Rate Limiter), Tier 2 (Concurrent Requests Limiter), Tier 3 (Fleet Usage Load Shedder), Tier 4 (Worker Utilization Load Shedder). See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the full design and `CHANGELOG.md` for what shipped in each tier.
```

Replace with:

```markdown
## Status

**All four of Stripe's tiers are implemented, live-e2e-verified, and audited end-to-end**: Tier 1 (Request Rate Limiter), Tier 2 (Concurrent Requests Limiter), Tier 3 (Fleet Usage Load Shedder), Tier 4 (Worker Utilization Load Shedder), plus v2 additions (bounded queueing, structured observability, optional mTLS — see the Comparison table below and `CHANGELOG.md`). Current version: see [`VERSION`](VERSION) or `CHANGELOG.md`'s latest entry — this line intentionally doesn't hardcode a version number, since that number changes far more often than this file is reviewed. See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the original v1 design.
```

- [ ] **Step 3: Fix SECURITY.md's Supported Versions table to stop contradicting its own prose**

In `SECURITY.md`, find:

```markdown
| Version | Supported |
| ------- | --------- |
| v1.0.x  | ✅ |
| main    | ✅ |
| < v1.0.0 | ❌ |
```

Replace with:

```markdown
| Version | Supported |
| ------- | --------- |
| Latest tagged release (see [`VERSION`](VERSION)) | ✅ |
| `main` | ✅ |
| All earlier tagged releases | ❌ |
```

- [ ] **Step 4: Commit**

```bash
git add VERSION README.md SECURITY.md
git commit -m "docs: establish VERSION as the single source of truth for current version

README's Status line said 'v1.0.0 — complete' while its own Comparison
table further down marked v2.2.0 features shipped; SECURITY.md's
Supported Versions table listed only v1.0.x while its prose discussed
v2.3.2 as already shipped. Both now point at VERSION/CHANGELOG.md
instead of hand-maintaining a version claim in three places that can
(and did) disagree."
```

---

### Task 6: Backfill `CHANGELOG.md` and document the v2.3.2 semver exception

**Files:**
- Modify: `CHANGELOG.md`

**Interfaces:**
- None (pure docs).

- [ ] **Step 1: Replace `CHANGELOG.md`'s `[Unreleased]` section and add the 7 missing version sections**

`CHANGELOG.md` currently has only `[Unreleased]`, `[1.0.0]`, and `[0.1.0]` sections despite 7 tags existing since v1.0.0. Insert the following between the existing `[Unreleased]` heading (which becomes genuinely empty, since its content — HMAC-signed tokens, header-based `/release` — is v2.3.2's already-shipped work) and `[1.0.0]`:

```markdown
## [Unreleased]

## [2.3.2] — 2026-07-20 — Tier 2 Concurrency-Token Security Hotfix

**Semver note:** this release contains a breaking wire-format change (see below) but was tagged as a patch bump (2.3.1 → 2.3.2), inconsistent with this file's stated intent to follow Semantic Versioning. Documented here as an acknowledged exception rather than corrected retroactively (re-tagging a already-published release would be worse) — going forward, a breaking change gets at minimum a minor bump, called out explicitly in its own CHANGELOG entry, the way this one now is.

Patch release closing issues #12 and #13, both confirmed exploitable via real-world CVE precedent (Portainer CVE-2026-44883, nhost CVE-2026-34969).

### Security

- `ReleaseConcurrency` now verifies an HMAC-SHA256 signature over the concurrency token before releasing a slot, rejecting a forged/tampered/wrong-key token with `codes.PermissionDenied` (#12).
- **Breaking change:** `/release`'s `key` and `token` now travel as request headers (`X-RateCap-Concurrency-Key`/`X-RateCap-Concurrency-Token`) instead of URL query parameters, closing a real leakage vector via proxy/access logs and `Referer` headers (#13). Any direct HTTP caller of `/release` bypassing the Go/Python SDKs must migrate to the new headers.
- New required env var `RATECAP_CONCURRENCY_SIGNING_KEY` on `ratecap-core` (fail-closed at startup).

## [2.3.1] — 2026-07-20 — Tier 2/3/4 Hardening Batch

Patch release: 5 merged PRs (#42-#46) closing 14 tracked issues.

### Added

- Tier 2 hardening: `RetryAfterMs` on rejection, `DecrConcurrent` regression tests, shadow-mode docs (#8, #9, #11).
- Tier 3 hardening: sheddable-cap rounding tests, shadow-mode reservation test, mixed-priority atomicity test, blast-radius docs (#14, #17, #18, #19).
- Tier 4 hardening: `X-RateCap-Shed-Tier` header, in-flight metrics gauge, HTTP server timeouts, `/worker-demo` docs (#20, #22, #24, #25).
- Sidecar error logging: real upstream errors now logged server-side on `/check` and `/release` failures (#41).

### Fixed

- Config watcher debounce: fixes a flaky fsnotify-related test via event coalescing (#37).

## [2.3.0] — 2026-07-19 — Phase 4 Production-Readiness & Adoption

### Added

- Production-readiness and adoption work per Phase 4 of the v2 roadmap (PR #40) — see `docs/superpowers/specs/2026-07-18-v2-phase-4a-comparison-table-design.md`, `2026-07-18-v2-phase-4b-benchmarks-design.md`, and `2026-07-19-v2-phase-4c-helm-chart-design.md` for the full design rationale behind this release's comparison table, benchmarks, and Helm chart.

## [2.2.0] — 2026-07-17 — Phase 3 Bounded Queueing (ConcurrencyLimiter)

### Added

- Bounded queueing for Tier 2's `ConcurrencyLimiter` (PR #34) — see `docs/superpowers/specs/2026-07-17-v2-phase-3-queueing-design.md`.

## [2.1.0] — 2026-07-17 — Phase 2 Developer Tooling

### Added

- `ratecapctl` CLI and the Python SDK (PR #33) — see `docs/superpowers/specs/2026-07-17-v2-phase-2-tooling-design.md`.

## [2.0.0] — 2026-07-16 — Phase 1 Foundations

### Added

- Observability, structured logging, and optional mTLS (PR #31) — see `docs/superpowers/specs/2026-07-17-v2-phase-1-foundations-design.md`.

## [1.0.1] — 2026-07-16 — Config Validation Gaps

### Fixed

- Config validation gaps found in a post-v1.0.0 audit (PR #29) — see `docs/superpowers/specs/2026-07-16-v1.0.1-config-validation-gaps.md`.
```

- [ ] **Step 2: Verify the file still parses as valid Markdown and the version ordering is correct**

Run: `grep -n '^## \[' CHANGELOG.md`
Expected output, in this exact top-to-bottom order: `[Unreleased]`, `[2.3.2]`, `[2.3.1]`, `[2.3.0]`, `[2.2.0]`, `[2.1.0]`, `[2.0.0]`, `[1.0.1]`, `[1.0.0]`, `[0.1.0]` — newest first, matching Keep a Changelog convention (already stated at the top of this file).

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs: backfill CHANGELOG.md for v1.0.1 through v2.3.2

Cut once for v1.0.0 and never again despite 7 subsequent tags —
[Unreleased] was calling already-tagged, already-on-main code
'unreleased.' Backfilled from each tag's own annotated message (richest
for v2.3.1/v2.3.2, terser for the earlier ones where the tag message was
just a PR title). Also documents the v2.3.2 semver exception (a breaking
wire-contract change shipped as a patch bump) explicitly rather than
leaving it silent."
```

---

### Task 7: Add `.github/dependabot.yml`

**Files:**
- Create: `.github/dependabot.yml`

**Interfaces:**
- None (GitHub-native config, no code interface).

- [ ] **Step 1: Create the dependabot config**

Create `.github/dependabot.yml`:

```yaml
version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/proto"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "gomod"
    directory: "/services/core"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "gomod"
    directory: "/services/sidecar"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "gomod"
    directory: "/cli"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "gomod"
    directory: "/deploy/sampleapp"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "gomod"
    directory: "/packages/sdks/go"
    schedule:
      interval: "weekly"
    groups:
      go-dependencies:
        patterns: ["*"]

  - package-ecosystem: "pip"
    directory: "/packages/sdks/python"
    schedule:
      interval: "weekly"
    groups:
      python-dependencies:
        patterns: ["*"]

  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"
    groups:
      github-actions:
        patterns: ["*"]

  - package-ecosystem: "docker"
    directory: "/services/core"
    schedule:
      interval: "weekly"

  - package-ecosystem: "docker"
    directory: "/services/sidecar"
    schedule:
      interval: "weekly"

  - package-ecosystem: "docker"
    directory: "/deploy/sampleapp"
    schedule:
      interval: "weekly"
```

- [ ] **Step 2: Validate the YAML parses**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/dependabot.yml'))" && echo OK`
Expected: `OK` (no exception).

- [ ] **Step 3: Commit**

```bash
git add .github/dependabot.yml
git commit -m "ci: add dependabot.yml for proactive, grouped dependency updates

The 4 open Dependabot PRs (#61-64, closed in Task 8) prove only the
reactive, CVE-triggered security-updates toggle was on — nothing caught
ordinary staleness, which is exactly how proto/go.mod's x/sys and x/text
silently drifted from services/core and services/sidecar. Covers all 6
Go module directories, pip, github-actions, and docker."
```

---

### Task 8: Merge the 4 open Dependabot PRs in lockstep and fix `proto`'s version skew

**Files:**
- Modify: `services/core/go.mod`, `services/core/go.sum`, `services/sidecar/go.mod`, `services/sidecar/go.sum`, `proto/go.mod`, `proto/go.sum`, `go.work.sum`

**Interfaces:**
- None (dependency version bumps only).

- [ ] **Step 1: Confirm the 4 PRs are still open and unchanged**

Run: `gh pr list --state open --search "dependabot"`
Expected: PRs #61 (`golang.org/x/crypto` 0.51.0→0.52.0, `services/core`), #62 (`golang.org/x/net` 0.53.0→0.55.0, `services/sidecar`), #63 (`google.golang.org/grpc` 1.82.0→1.82.1, `services/sidecar`), #64 (`github.com/moby/go-archive` 0.2.0→0.3.0, `services/core`).

- [ ] **Step 2: Merge #61 and #64 (services/core) directly — they don't create cross-module skew**

```bash
gh pr merge 61 --squash
gh pr merge 64 --squash
```

- [ ] **Step 3: Merge #62 and #63 (services/sidecar), then bump the same dependencies in `services/core` and `proto` to match, so all three stay aligned**

Each command below assumes the repo root as the starting directory (`cd` back to repo root between steps if your shell session persists state):

```bash
gh pr merge 62 --squash
gh pr merge 63 --squash
git pull
(cd services/core && go get google.golang.org/grpc@v1.82.1 golang.org/x/net@v0.55.0 && go mod tidy)
(cd proto && go get google.golang.org/grpc@v1.82.1 golang.org/x/net@v0.55.0 && go mod tidy)
```

- [ ] **Step 4: Close `proto`'s pre-existing `x/sys`/`x/text` skew (found during the internal audit, unrelated to the 4 PRs above)**

From the repo root:

```bash
(cd proto && go get golang.org/x/sys@v0.45.0 golang.org/x/text@v0.37.0 && go mod tidy)
```

- [ ] **Step 5: Re-sync the workspace sum file and verify everything still builds and tests clean**

From the repo root:

```bash
go work sync
for m in proto services/core services/sidecar; do (cd "$m" && go build ./... && go test ./... -race); done
```

Expected: all three modules build and test clean.

- [ ] **Step 6: Commit**

```bash
git add proto/go.mod proto/go.sum services/core/go.mod services/core/go.sum services/sidecar/go.mod services/sidecar/go.sum go.work.sum
git commit -m "chore: align grpc/x-net/x-sys/x-text versions across proto, core, sidecar

Merging services/sidecar's Dependabot PRs (#62 grpc, #63 x/net) alone
would have introduced fresh skew against services/core and proto, which
shared the pre-bump versions. Bumps all three in lockstep, and separately
closes proto's pre-existing x/sys/x/text skew (last touched 2026-07-13,
never bumped alongside its siblings since)."
```

---

### Task 9: Publish the missing v2.3.2 GitHub Release and tag `main`'s pending work as v2.4.0

**Files:**
- None (git/GitHub operations only — this is the task that actually cuts the release capturing Tasks 1-8's work plus the pre-existing untagged commits on `main`).

**Interfaces:**
- None.

- [ ] **Step 1: Publish the missing v2.3.2 GitHub Release**

```bash
gh release create v2.3.2 --title "v2.3.2 — Tier 2 Concurrency-Token Security Hotfix" --notes "$(git tag -l --format='%(contents)' v2.3.2)"
```

Expected: `gh release view v2.3.2` now succeeds (it previously returned "release not found" despite the tag existing).

- [ ] **Step 2: Confirm this feature branch is ready to merge — all of Tasks 1-8 committed and tests green**

```bash
git log develop..feature/v3-upgrade-roadmap --oneline
for m in proto services/core services/sidecar cli packages/sdks/go deploy/sampleapp; do (cd "$m" && go build ./... && go test ./... -race); done
```

Expected: every commit from Tasks 1-8 listed, every module builds and tests clean.

- [ ] **Step 3: Open a PR into `develop` per this repo's branch strategy, get it merged**

```bash
git push -u origin feature/v3-upgrade-roadmap
gh pr create --base develop --title "v3 roadmap Phase 0: housekeeping & quick wins" --body "Implements Phase 0 of docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md: fleet-wide backlog fix, bench measurement fix, two merged pre-existing branches, dependabot.yml, dependency skew fix, and the versioning-process cleanup (VERSION file, CHANGELOG backfill, semver exception doc)."
```

Wait for review/CI, then merge (this step is a human/CI gate, not something to script blindly — do not merge without CI passing).

- [ ] **Step 4: After the PR merges to `develop` and `develop` merges to `main`, tag the resulting `main` as v2.4.0**

```bash
git checkout main && git pull
git tag -a v2.4.0 -m "$(cat <<'EOF'
RateCap v2.4.0 — Sidecar self-throttle + priority-enum fix + housekeeping

Tags the self-throttle limiter and CI publish workflows that had been
sitting on main untagged since before this release, plus this release's
own work: fix/v3-breaking-wire-changes' PRIORITY_UNSPECIFIED enum
renumbering (BREAKING: Priority enum values renumbered on the wire —
see CHANGELOG.md), fix/v3-config-validation's Tier 1 config validation,
the Tier 2 backlog fleet-wide fix, the bench_run.go measurement fix,
and the versioning-process cleanup (VERSION, CHANGELOG backfill,
dependabot.yml, dependency alignment).
EOF
)"
git push origin v2.4.0
gh release create v2.4.0 --title "v2.4.0 — Sidecar self-throttle + priority-enum fix + housekeeping" --notes-file <(git tag -l --format='%(contents)' v2.4.0)
```

- [ ] **Step 5: Update `VERSION` on `main` to reflect the new tag**

```bash
echo "2.4.0" > VERSION
git add VERSION
git commit -m "chore: bump VERSION to 2.4.0"
git push
```

---

## Self-Review Notes

**Spec coverage:** All 11 Phase 0 items from the spec map to a task above — items 1+2 → Task 1, item 3 → Task 2, item 10 → Task 3, item 11 → Task 4, item 4 → Task 5, items 5+7 → Task 6, item 8 → Task 7, item 9 → Task 8, item 6 → Task 9.

**Type/signature consistency:** `acquireBacklogSlot`'s new signature (Task 1, Step 3) is used consistently in `Check()` in the same step — no other file calls it. `benchResult`'s new fields (Task 2, Step 3) are read consistently by the test written in Step 1 (same field names, same JSON tags).

**Ordering:** Tasks 1-2 (code fixes) and Tasks 3-4 (branch merges) are independent of each other and of Tasks 5-8; Task 9 is intentionally last since it tags the state *after* every other task has landed.
