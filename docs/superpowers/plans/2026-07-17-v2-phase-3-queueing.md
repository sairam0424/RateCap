# RateCap v2 Phase 3: Bounded Queueing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add off-by-default bounded queueing to `ConcurrencyLimiter` (Tier 2) — a request that finds the concurrency cap full can wait, bounded by a local backlog and a max-wait deadline, before falling back to today's instant `REJECT_429`.

**Architecture:** A bounded local semaphore (`MaxBacklog`, a `sync/atomic`-backed counting semaphore mirroring `worker.Shedder`'s existing CAS-loop pattern) gates entry into a polling loop inside `ConcurrencyLimiter.Check()`. The loop re-calls the existing, unmodified `IncrConcurrent` Lua script every `PollIntervalMs` until it succeeds, `MaxQueueWaitMs` elapses, or the request's `ctx` is canceled. No new Redis primitives, no ordering, no changes to `FleetShedder` or `Pipeline`'s composition logic beyond widening its short-circuit condition to include the new `QUEUE` action (queueing is otherwise self-contained inside one `Limiter.Check()`, matching every prior tier's pattern).

**Tech Stack:** Go 1.26 (`services/core` module), `sync/atomic`, `time.Timer`/`time.Ticker`, existing `gopkg.in/yaml.v3` config loading, existing `google.golang.org/protobuf`-generated proto code (regenerated via `protoc`).

## Global Constraints

- TDD: write the failing test first, confirm it fails for the stated reason, implement the minimal code, confirm it passes.
- `gofmt -l` must report zero files before every commit.
- `cd services/core && go test ./... -race` must pass before every commit that touches `services/core`. `cd services/sidecar && go test ./... -race` must pass before every commit that touches `services/sidecar`.
- No comments except non-obvious WHY — this plan's polling/backlog logic has a few genuinely non-obvious WHY comments (e.g. why a CAS loop instead of a channel, why shadow mode skips queueing); write them in the same terse style as the codebase's existing `unboundedCap` comment.
- No `Co-Authored-By` trailers in any commit.
- Every step gives exact commands and exact expected output.
- Scope is `ConcurrencyLimiter` (Tier 2) only. Do not modify `services/core/limiter/fleetshedder.go` or `services/core/limiter/tokenbucket.go`.

### Resolved design gap: `QUEUE`'s exact semantics (found during planning)

The design spec's wording ("the enum gains a `QUEUE` value purely for server-side attribution... a queued-then-served request returns `Decision{Action: ALLOW}`") was checked against the actual mechanism while writing this plan, using a scratch Go module to compile and race-test the real logic before committing it here. Returning plain `ALLOW` from a successful poll would make `QUEUE` permanently unreachable dead code — it would never appear in `ratecap_decisions_total{action="queue"}` or decision logs, contradicting the spec's own stated purpose for adding it. Verified, corrected semantics used throughout this plan:

- **Immediate success** (first `IncrConcurrent` call succeeds, no queueing needed) → `Decision{Action: ALLOW, ...}`, unchanged from today.
- **Backlog full** → `Decision{Action: REJECT_429, ...}`, unchanged from today.
- **Queued, then a later poll succeeds** → `Decision{Action: QUEUE, ...}` (NOT `ALLOW`). This is the one new terminal value `Check()` actually returns. It carries reservations exactly like `ALLOW` does, and the sidecar still responds `200 OK` for it (mirroring exactly how `SHADOW_LOG` already gets its own metrics/log label while still resulting in a `200`) — the client never sees a difference on the wire, only `tier`/`action` server-side attribution differs.
- **Queued, then `MaxQueueWaitMs` elapses** → `Decision{Action: REJECT_429, ...}`, exactly like today's non-queueing rejection.
- **Queued, then `ctx` is canceled** → propagate `ctx.Err()`, exactly like every other tier's existing error-propagation behavior.

This also surfaced a second, real bug this plan fixes: `Pipeline.Check`'s existing short-circuit (`d.Action != ALLOW`) would otherwise skip every later tier (`FleetShedder`) for every request that got queued — a functional regression, not just an attribution nuance, since Tier 3 must still see every request regardless of whether Tier 2 queued it. Task 3 below widens the short-circuit condition to `d.Action != ALLOW && d.Action != QUEUE`, verified with a dedicated test proving a later tier is still called after an earlier `QUEUE`.

### Resolved design gap: shadow-mode/queueing precedence

The original spec did not address what happens when both `shadow_mode: true` and `queueing_enabled: true` are set on the same tier. Resolved during planning: **shadow mode takes precedence, and queueing is skipped entirely when shadow mode is active.** Shadow mode's whole purpose is to observe what would happen without ever actually delaying a real caller; blocking inside a poll loop while "just observing" would defeat that purpose. Verified with a dedicated test (`TestConcurrencyLimiter_ShadowModeSkipsQueueingEvenWithQueueingEnabled`) asserting the shadow-mode path returns near-instantly even with queueing enabled and the cap exceeded.

---

## Task 1: `QUEUE` action — proto, Go enum, and safe conversion at every boundary

**Files:**
- Modify: `proto/ratecap/v1/ratecap.proto` (add `QUEUE = 4;` to the `Action` enum)
- Modify: `proto/ratecap/v1/ratecap.pb.go`, `proto/ratecap/v1/ratecap_grpc.pb.go` (regenerated, not hand-edited)
- Modify: `services/core/limiter/limiter.go` (add `QUEUE` to the Go `Action` enum)
- Modify: `services/core/grpcserver/server.go` (`toProtoAction` gains a `QUEUE` case)
- Modify: `services/sidecar/proxy/proxy.go` (`actionLabel` gains a `QUEUE` case; the `switch action` in `ServeHTTP` gains `ratecapv1.Action_QUEUE` alongside `ALLOW`/`SHADOW_LOG` for the `200` response)
- Modify: `services/sidecar/shadow/shadow.go` — no code change needed, but Step 6 below adds a test proving `CoerceIfShadowOverridden` correctly leaves `QUEUE` untouched (it only coerces `REJECT_429`/`REJECT_503`)
- Test: `services/core/grpcserver/server_test.go`, `services/sidecar/proxy/proxy_test.go`, `services/sidecar/shadow/shadow_test.go`

**Interfaces:**
- Produces: `limiter.QUEUE` (a new `limiter.Action` value, defined as `QUEUE` appended after `SHADOW_LOG` in the existing `const` block in `services/core/limiter/limiter.go`). `ratecapv1.Action_QUEUE` (the proto-generated equivalent, value `4`). Task 2 and Task 3 both depend on `limiter.QUEUE` existing.

This task lands the `QUEUE` value everywhere `ALLOW`/`REJECT_429`/`REJECT_503`/`SHADOW_LOG` already flow, so Task 3's `ConcurrencyLimiter.Check()` change has a value to return, and every existing conversion boundary (`toProtoAction`, `actionLabel`, the sidecar's response-status `switch`) handles it correctly instead of silently falling into a `default` branch.

- [ ] **Step 1: Write the failing proto-conversion test**

Add to `services/core/grpcserver/server_test.go` (append after `TestCheckRateLimit_ReturnsAllowDecision`, using the same `fakeLimiter`/`fakeReleaser` already defined in that file):

```go
func TestCheckRateLimit_ConvertsQueueActionToProtoQueue(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.QUEUE, Tier: "concurrency_limiter"}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_QUEUE {
		t.Errorf("expected Action_QUEUE, got %v", resp.Action)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd services/core && go test ./grpcserver/... -run TestCheckRateLimit_ConvertsQueueActionToProtoQueue -v`
Expected: compile error — `limiter.QUEUE` and `ratecapv1.Action_QUEUE` do not exist yet.

- [ ] **Step 3: Add `QUEUE` to the proto `Action` enum**

Edit `proto/ratecap/v1/ratecap.proto`, changing:

```proto
enum Action {
  ALLOW = 0;
  REJECT_429 = 1;
  REJECT_503 = 2;
  SHADOW_LOG = 3;
}
```

to:

```proto
enum Action {
  ALLOW = 0;
  REJECT_429 = 1;
  REJECT_503 = 2;
  SHADOW_LOG = 3;
  QUEUE = 4;
}
```

- [ ] **Step 4: Regenerate the proto Go code**

Ensure `protoc-gen-go` and `protoc-gen-go-grpc` are on `PATH` (they are already installed at `$(go env GOPATH)/bin` in this environment — if the command below fails with "program not found", run `export PATH="$PATH:$(go env GOPATH)/bin"` first). From the repo root:

```bash
protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
```

Expected: no output on success. Confirm the regenerated file contains the new value:

```bash
grep -n "Action_QUEUE" proto/ratecap/v1/ratecap.pb.go
```

Expected output includes a line like `Action_QUEUE Action = 4` and entries for `4: "QUEUE"` / `"QUEUE": 4` in the `Action_name`/`Action_value` maps.

- [ ] **Step 5: Add `QUEUE` to the Go `limiter.Action` enum**

Edit `services/core/limiter/limiter.go`, changing:

```go
const (
	ALLOW Action = iota
	REJECT_429
	REJECT_503
	SHADOW_LOG
)
```

to:

```go
const (
	ALLOW Action = iota
	REJECT_429
	REJECT_503
	SHADOW_LOG
	QUEUE
)
```

- [ ] **Step 6: Add `QUEUE` to `toProtoAction` in `services/core/grpcserver/server.go`**

Change:

```go
func toProtoAction(a limiter.Action) ratecapv1.Action {
	switch a {
	case limiter.ALLOW:
		return ratecapv1.Action_ALLOW
	case limiter.REJECT_429:
		return ratecapv1.Action_REJECT_429
	case limiter.REJECT_503:
		return ratecapv1.Action_REJECT_503
	case limiter.SHADOW_LOG:
		return ratecapv1.Action_SHADOW_LOG
	default:
		return ratecapv1.Action_REJECT_503
	}
}
```

to:

```go
func toProtoAction(a limiter.Action) ratecapv1.Action {
	switch a {
	case limiter.ALLOW:
		return ratecapv1.Action_ALLOW
	case limiter.REJECT_429:
		return ratecapv1.Action_REJECT_429
	case limiter.REJECT_503:
		return ratecapv1.Action_REJECT_503
	case limiter.SHADOW_LOG:
		return ratecapv1.Action_SHADOW_LOG
	case limiter.QUEUE:
		return ratecapv1.Action_QUEUE
	default:
		return ratecapv1.Action_REJECT_503
	}
}
```

- [ ] **Step 7: Run the test to confirm it passes**

Run: `cd services/core && go test ./grpcserver/... -run TestCheckRateLimit_ConvertsQueueActionToProtoQueue -v`
Expected: `--- PASS`

- [ ] **Step 8: Write the failing sidecar `actionLabel`/response-status test**

Add to `services/sidecar/proxy/proxy_test.go` (append after `TestServeHTTP_ShadowLogReturns200`, using the same `fakeRatecapClient` already defined in that file):

```go
func TestServeHTTP_QueueActionReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_QUEUE, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a queued-then-served request (transparent to the client), got %d", rec.Code)
	}
}

func TestServeHTTP_RecordsQueueActionMetricLabel(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_QUEUE, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("concurrency_limiter", "queue"))
	if got < 1 {
		t.Errorf(`expected ratecap_decisions_total{tier="concurrency_limiter",action="queue"} >= 1, got %v`, got)
	}
}
```

- [ ] **Step 9: Run it to confirm it fails**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_QueueActionReturns200|TestServeHTTP_RecordsQueueActionMetricLabel' -v`
Expected: `TestServeHTTP_QueueActionReturns200` fails with `expected 200 ... got 503` (the `switch action` in `ServeHTTP` falls through to no case, leaving the response uninitialized at its zero-value... actually Go's `http.ResponseWriter` defaults to `200` if `WriteHeader` is never called — so this test may unexpectedly pass before the fix. The real failure is in `actionLabel`, confirmed by the second test.) `TestServeHTTP_RecordsQueueActionMetricLabel` fails with `expected ratecap_decisions_total{tier="concurrency_limiter",action="queue"} >= 1, got 0` because `actionLabel` returns `"unknown"` for `Action_QUEUE` today.

- [ ] **Step 10: Add `QUEUE` to `actionLabel` and the response-status switch in `services/sidecar/proxy/proxy.go`**

Change:

```go
func actionLabel(a ratecapv1.Action) string {
	switch a {
	case ratecapv1.Action_ALLOW:
		return "allow"
	case ratecapv1.Action_REJECT_429:
		return "reject_429"
	case ratecapv1.Action_REJECT_503:
		return "reject_503"
	case ratecapv1.Action_SHADOW_LOG:
		return "shadow_log"
	default:
		return "unknown"
	}
}
```

to:

```go
func actionLabel(a ratecapv1.Action) string {
	switch a {
	case ratecapv1.Action_ALLOW:
		return "allow"
	case ratecapv1.Action_REJECT_429:
		return "reject_429"
	case ratecapv1.Action_REJECT_503:
		return "reject_503"
	case ratecapv1.Action_SHADOW_LOG:
		return "shadow_log"
	case ratecapv1.Action_QUEUE:
		return "queue"
	default:
		return "unknown"
	}
}
```

And change the response-status switch in `ServeHTTP`:

```go
	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
```

to:

```go
	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG, ratecapv1.Action_QUEUE:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
```

- [ ] **Step 11: Run both new proxy tests to confirm they pass**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_QueueActionReturns200|TestServeHTTP_RecordsQueueActionMetricLabel' -v`
Expected: both `--- PASS`

- [ ] **Step 12: Write and run a characterization test for shadow-mode coercion leaving `QUEUE` untouched**

`services/sidecar/shadow/shadow_test.go` already exists, with 4 existing tests (`TestGlobalOverrideEnabled_TrueWhenEnvSet`, `TestGlobalOverrideEnabled_FalseWhenEnvUnset`, `TestCoerceIfShadowOverridden_CoercesRejectToShadowLog`, `TestCoerceIfShadowOverridden_PassesThroughWhenOverrideDisabled`, `TestCoerceIfShadowOverridden_AllowPassesThroughRegardless`). Append the following after the last of those (do not replace the file):

```go
func TestCoerceIfShadowOverridden_LeavesQueueUnchanged(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_QUEUE, true)
	if got != ratecapv1.Action_QUEUE {
		t.Errorf("expected QUEUE to pass through shadow coercion unchanged (only REJECT_429/REJECT_503 are coerced), got %v", got)
	}
}
```

Run: `cd services/sidecar && go test ./shadow/... -run TestCoerceIfShadowOverridden_LeavesQueueUnchanged -v`
Expected: `--- PASS` (no code change needed here — `CoerceIfShadowOverridden`'s existing `if action == ratecapv1.Action_REJECT_429 || action == ratecapv1.Action_REJECT_503` check already correctly excludes `QUEUE`; this step exists to pin that behavior with a test, since Task 4's shadow-mode-skips-queueing precedence depends on it).

- [ ] **Step 13: Run gofmt and the full test suites for both modules**

Run: `gofmt -l services/core services/sidecar`
Expected: no output (zero files need formatting)

Run: `cd services/core && go test ./... -race`
Expected: `ok` for every package (note: `services/core/store`'s tests require Docker; if Docker is unreachable in this environment, those specific packages will fail to start containers — this is a pre-existing condition unrelated to this task's changes, confirm via `git stash` + rerun if in doubt)

Run: `cd services/sidecar && go test ./... -race`
Expected: `ok` for every package

- [ ] **Step 14: Commit**

```bash
git add proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/limiter/limiter.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go services/sidecar/shadow/shadow_test.go
git commit -m "feat(limiter): add QUEUE action to Action enum, wired through every conversion boundary"
```

---

## Task 2: `ConcurrencyLimiterConfig` gains 4 new queueing fields, with validation

**Files:**
- Modify: `services/core/config/config.go` (add 4 fields to `ConcurrencyLimiterConfig`, extend `Validate()`)
- Test: `services/core/config/config_test.go`

**Interfaces:**
- Consumes: nothing new from Task 1.
- Produces: `config.ConcurrencyLimiterConfig{QueueingEnabled bool, MaxBacklog int, MaxQueueWaitMs int64, PollIntervalMs int64}` (4 new fields, alongside the existing `DefaultMaxConcurrent`, `MaxRequestDurationMs`, `ShadowMode`). Task 3's `ConcurrencyLimiter.Reconfigure(...)` signature and Task 5's `main.go` call sites both consume these exact field names and types.

- [ ] **Step 1: Write the failing config-parsing test**

Add to `services/core/config/config_test.go` (append after `TestLoad_ParsesConcurrencyLimiterTier`):

```go
func TestLoad_ParsesConcurrencyLimiterQueueingFields(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 20
    max_request_duration_ms: 30000
    shadow_mode: false
    queueing_enabled: true
    max_backlog: 50
    max_queue_wait_ms: 2000
    poll_interval_ms: 25
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Tiers.ConcurrencyLimiter.QueueingEnabled {
		t.Error("expected QueueingEnabled=true")
	}
	if cfg.Tiers.ConcurrencyLimiter.MaxBacklog != 50 {
		t.Errorf("expected MaxBacklog=50, got %d", cfg.Tiers.ConcurrencyLimiter.MaxBacklog)
	}
	if cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs != 2000 {
		t.Errorf("expected MaxQueueWaitMs=2000, got %d", cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs)
	}
	if cfg.Tiers.ConcurrencyLimiter.PollIntervalMs != 25 {
		t.Errorf("expected PollIntervalMs=25, got %d", cfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
	}
}

func TestLoad_QueueingFieldsDefaultToZeroValuesWhenOmitted(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 20
    max_request_duration_ms: 30000
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Tiers.ConcurrencyLimiter.QueueingEnabled {
		t.Error("expected QueueingEnabled to default to false when the key is omitted (existing configs get zero behavior change)")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd services/core && go test ./config/... -run 'TestLoad_ParsesConcurrencyLimiterQueueingFields|TestLoad_QueueingFieldsDefaultToZeroValuesWhenOmitted' -v`
Expected: compile error — `cfg.Tiers.ConcurrencyLimiter.QueueingEnabled` (and the other 3 fields) do not exist yet.

- [ ] **Step 3: Add the 4 new fields to `ConcurrencyLimiterConfig`**

Change:

```go
type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
}
```

to:

```go
type ConcurrencyLimiterConfig struct {
	DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
	MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
	ShadowMode           bool  `yaml:"shadow_mode"`
	QueueingEnabled      bool  `yaml:"queueing_enabled"`
	MaxBacklog           int   `yaml:"max_backlog"`
	MaxQueueWaitMs       int64 `yaml:"max_queue_wait_ms"`
	PollIntervalMs       int64 `yaml:"poll_interval_ms"`
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `cd services/core && go test ./config/... -run 'TestLoad_ParsesConcurrencyLimiterQueueingFields|TestLoad_QueueingFieldsDefaultToZeroValuesWhenOmitted' -v`
Expected: both `--- PASS`

- [ ] **Step 5: Write the failing validation tests**

Add to `services/core/config/config_test.go` (append after `TestValidate_ErrorMentionsConcurrencyLimiterOnMissingBlock`):

```go
func TestValidate_RejectsZeroMaxBacklogWhenQueueingEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = true
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = 0

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for queueing_enabled=true with max_backlog=0, got nil")
	}
}

func TestValidate_RejectsNegativeMaxBacklogWhenQueueingEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = true
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = -5

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative max_backlog with queueing_enabled=true, got nil")
	}
}

func TestValidate_RejectsZeroMaxQueueWaitMsWhenQueueingEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = true
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = 10
	cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs = 0
	cfg.Tiers.ConcurrencyLimiter.PollIntervalMs = 10

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for queueing_enabled=true with max_queue_wait_ms=0, got nil")
	}
}

func TestValidate_RejectsZeroPollIntervalMsWhenQueueingEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = true
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = 10
	cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs = 2000
	cfg.Tiers.ConcurrencyLimiter.PollIntervalMs = 0

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for queueing_enabled=true with poll_interval_ms=0, got nil")
	}
}

func TestValidate_RejectsPollIntervalMsGreaterThanMaxQueueWaitMs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = true
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = 10
	cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs = 100
	cfg.Tiers.ConcurrencyLimiter.PollIntervalMs = 500

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when poll_interval_ms (500) exceeds max_queue_wait_ms (100) — a waiter would never get to poll before timing out")
	}
}

func TestValidate_IgnoresQueueingFieldsWhenQueueingDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tiers.FleetShedder.DefaultMaxConcurrent = 100
	cfg.Tiers.FleetShedder.ReservedCriticalPct = 20
	cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent = 50
	cfg.Tiers.ConcurrencyLimiter.QueueingEnabled = false
	cfg.Tiers.ConcurrencyLimiter.MaxBacklog = 0
	cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs = 0
	cfg.Tiers.ConcurrencyLimiter.PollIntervalMs = 0

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected zero-valued queueing fields to be valid when queueing_enabled=false (matches every existing config with no queueing block), got error: %v", err)
	}
}
```

- [ ] **Step 6: Run them to confirm they fail**

Run: `cd services/core && go test ./config/... -run 'TestValidate_RejectsZeroMaxBacklogWhenQueueingEnabled|TestValidate_RejectsNegativeMaxBacklogWhenQueueingEnabled|TestValidate_RejectsZeroMaxQueueWaitMsWhenQueueingEnabled|TestValidate_RejectsZeroPollIntervalMsWhenQueueingEnabled|TestValidate_RejectsPollIntervalMsGreaterThanMaxQueueWaitMs|TestValidate_IgnoresQueueingFieldsWhenQueueingDisabled' -v`
Expected: the 5 `Rejects*`/`Greater*` tests each report `expected error ... got nil` (fail); `TestValidate_IgnoresQueueingFieldsWhenQueueingDisabled` passes already (no validation exists yet, so nothing rejects it) — this is expected and fine, it's a characterization test for the eventual implementation, not currently failing.

- [ ] **Step 7: Extend `Validate()` with the new checks**

Change:

```go
func (c *Config) Validate() error {
	if c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.concurrency_limiter.default_max_concurrent must be > 0, got %d (is the concurrency_limiter block missing from your config?)", c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.fleet_shedder.default_max_concurrent must be > 0, got %d (is the fleet_shedder block missing from your config?)", c.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.ReservedCriticalPct < 0 || c.Tiers.FleetShedder.ReservedCriticalPct > 100 {
		return fmt.Errorf("tiers.fleet_shedder.reserved_critical_pct must be between 0 and 100 inclusive, got %d", c.Tiers.FleetShedder.ReservedCriticalPct)
	}
	return nil
}
```

to:

```go
func (c *Config) Validate() error {
	if c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.concurrency_limiter.default_max_concurrent must be > 0, got %d (is the concurrency_limiter block missing from your config?)", c.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.DefaultMaxConcurrent <= 0 {
		return fmt.Errorf("tiers.fleet_shedder.default_max_concurrent must be > 0, got %d (is the fleet_shedder block missing from your config?)", c.Tiers.FleetShedder.DefaultMaxConcurrent)
	}
	if c.Tiers.FleetShedder.ReservedCriticalPct < 0 || c.Tiers.FleetShedder.ReservedCriticalPct > 100 {
		return fmt.Errorf("tiers.fleet_shedder.reserved_critical_pct must be between 0 and 100 inclusive, got %d", c.Tiers.FleetShedder.ReservedCriticalPct)
	}
	if c.Tiers.ConcurrencyLimiter.QueueingEnabled {
		if c.Tiers.ConcurrencyLimiter.MaxBacklog <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.max_backlog must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.MaxBacklog)
		}
		if c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.max_queue_wait_ms must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs)
		}
		if c.Tiers.ConcurrencyLimiter.PollIntervalMs <= 0 {
			return fmt.Errorf("tiers.concurrency_limiter.poll_interval_ms must be > 0 when queueing_enabled is true, got %d", c.Tiers.ConcurrencyLimiter.PollIntervalMs)
		}
		if c.Tiers.ConcurrencyLimiter.PollIntervalMs > c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs {
			return fmt.Errorf("tiers.concurrency_limiter.poll_interval_ms (%d) must not exceed max_queue_wait_ms (%d) — a waiter would never get a chance to poll before timing out", c.Tiers.ConcurrencyLimiter.PollIntervalMs, c.Tiers.ConcurrencyLimiter.MaxQueueWaitMs)
		}
	}
	return nil
}
```

- [ ] **Step 8: Run the tests to confirm they pass**

Run: `cd services/core && go test ./config/... -v`
Expected: every test in the package `--- PASS`, including all pre-existing tests (confirms no regression).

- [ ] **Step 9: Run gofmt and the full config package test suite**

Run: `gofmt -l services/core/config`
Expected: no output

Run: `cd services/core && go test ./config/... -race`
Expected: `ok`

- [ ] **Step 10: Commit**

```bash
git add services/core/config/config.go services/core/config/config_test.go
git commit -m "feat(config): add queueing_enabled/max_backlog/max_queue_wait_ms/poll_interval_ms to concurrency_limiter config"
```

---

## Task 3: Bounded backlog + polling loop in `ConcurrencyLimiter.Check()`

**Files:**
- Modify: `services/core/limiter/concurrency.go`
- Modify: `services/core/limiter/pipeline.go` (widen the short-circuit condition to include `QUEUE`)
- Test: `services/core/limiter/concurrency_test.go`, `services/core/limiter/concurrency_queue_test.go` (new file), `services/core/limiter/pipeline_test.go`

**Interfaces:**
- Consumes: `limiter.QUEUE` (Task 1). `config.ConcurrencyLimiterConfig`'s 4 new fields (Task 2, consumed by Task 5's call sites, not directly by this task).
- Produces: `NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64) *ConcurrencyLimiter` and `(l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool, queueingEnabled bool, maxBacklog int, maxQueueWaitMs, pollIntervalMs int64)` — both signatures widen by exactly 4 trailing parameters, in this exact order. Task 5's `main.go` call sites and Task 2's config fields must match this order exactly: `queueingEnabled, maxBacklog, maxQueueWaitMs, pollIntervalMs`.

This is the plan's largest, most novel task. The exact code below was independently written, compiled, and race-tested against a real fake store in an isolated scratch Go module before being included here — trust it as written.

- [ ] **Step 1: Update existing `concurrency_test.go` call sites for the new constructor/Reconfigure signature**

The 4 new parameters must be threaded through every existing call site in `services/core/limiter/concurrency_test.go` before any new test can compile. Update every `limiter.NewConcurrencyLimiter(fs, X, Y, Z)` call to `limiter.NewConcurrencyLimiter(fs, X, Y, Z, false, 0, 0, 0)` (queueing off, matching today's behavior exactly), and every `l.Reconfigure(X, Y, Z)` call to `l.Reconfigure(X, Y, Z, false, 0, 0, 0)`. Concretely, in `services/core/limiter/concurrency_test.go`:

- Line 43: `l := limiter.NewConcurrencyLimiter(fs, 3, 30000, false)` → `l := limiter.NewConcurrencyLimiter(fs, 3, 30000, false, false, 0, 0, 0)`
- Line 73: `l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false)` → `l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false, false, 0, 0, 0)`
- Line 86: `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true)` → `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, false, 0, 0, 0)`
- Line 104: `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true)` → `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, false, 0, 0, 0)`
- Line 133: `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false)` → `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 0, 0, 0)`
- Line 160: `l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false)` → `l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false, false, 0, 0, 0)`
- Line 175: `l.Reconfigure(10, 30000, n%2 == 0)` → `l.Reconfigure(10, 30000, n%2 == 0, false, 0, 0, 0)`
- Line 183: `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false)` → `l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 0, 0, 0)`
- Line 198: `l.Reconfigure(1, 30000, true)` → `l.Reconfigure(1, 30000, true, false, 0, 0, 0)`

This step alone makes the file fail to compile against the *current* (pre-Task-3) `concurrency.go` — that is expected and correct for TDD; Step 4 below makes it compile again.

- [ ] **Step 2: Write the new queueing test file**

Create `services/core/limiter/concurrency_queue_test.go`:

```go
package limiter_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ratecap/core/limiter"
)

func TestConcurrencyLimiter_BacklogFullReturnsImmediate429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 1, 300, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		l.Check(ctx, limiter.Request{Key: "k"})
	}()
	<-started
	time.Sleep(20 * time.Millisecond)

	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 when backlog is full, got %v", d.Action)
	}
	wg.Wait()
}

func TestConcurrencyLimiter_SuccessfulPollReturnsQueueAction(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 5000, 10)
	ctx := context.Background()

	token1 := ""
	if _, tok, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err == nil {
		token1 = tok
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		fs.DecrConcurrent(ctx, "k", token1)
	}()

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.QUEUE {
		t.Fatalf("expected QUEUE once a slot frees after waiting (server-side attribution; wire-transparent to the client), got %v", d.Action)
	}
	if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
		t.Fatalf("expected a reservation from the successful poll, got %+v", d.Reservations)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("expected to wait for the slot to free (~30ms), got %v", elapsed)
	}
}

func TestConcurrencyLimiter_DeadlineExceededReturns429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 50, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 after MaxQueueWaitMs elapses, got %v", d.Action)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected to wait roughly MaxQueueWaitMs (50ms) before timing out, got %v", elapsed)
	}
}

func TestConcurrencyLimiter_ContextCancellationPropagates(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, 5, 5000, 10)
	if _, _, err := fs.IncrConcurrent(context.Background(), "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := l.Check(ctx, limiter.Request{Key: "k"})
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

func TestConcurrencyLimiter_ShadowModeSkipsQueueingEvenWithQueueingEnabled(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true, true, 5, 5000, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG (shadow mode takes precedence over queueing), got %v", d.Action)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("expected shadow mode to return immediately without queueing, took %v", elapsed)
	}
}

func TestConcurrencyLimiter_QueueingDisabledStillReturnsImmediate429(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, false, 5, 5000, 10)
	ctx := context.Background()

	if _, _, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "k"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected immediate REJECT_429 when queueing_enabled is false regardless of MaxBacklog, got %v", d.Action)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("expected immediate rejection with no polling when queueing is disabled, took %v", elapsed)
	}
}

// TestConcurrencyLimiter_StressBacklogNeverExceedsMaxBacklog hammers a small
// MaxBacklog (3) with 50 concurrent waiters against a permanently-full cap,
// sampling the internal backlog counter while they race to acquire slots.
// This mirrors worker.Shedder's own stress-test style
// (services/sidecar/worker/shedder_test.go): real goroutines, no simulation
// framework, a live peak tracker rather than only a final tally.
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
			if v := l.BacklogDepth(); v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	<-sampleDone

	if peak.Load() > maxBacklog {
		t.Fatalf("backlog peaked at %d, exceeding maxBacklog %d — overshoot", peak.Load(), maxBacklog)
	}
	if l.BacklogDepth() != 0 {
		t.Fatalf("expected backlog to return to 0 after all goroutines finished, got %d", l.BacklogDepth())
	}
}

// TestConcurrencyLimiter_StressManyWaitersOneSlotFreeingRepeatedly stresses
// the interleaving of many waiters racing to grab a single slot as it frees
// and refills repeatedly, mirroring
// TestShedder_StressAllowReleaseInterleaving's shape. It asserts every
// completed Check() returned either QUEUE (won a freed slot) or REJECT_429
// (timed out) — never ALLOW (which would mean queueing was bypassed) and
// never an unexpected action.
func TestConcurrencyLimiter_StressManyWaitersOneSlotFreeingRepeatedly(t *testing.T) {
	const goroutines = 40
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false, true, goroutines, 500, 5)
	ctx := context.Background()

	token := ""
	if _, tok, err := fs.IncrConcurrent(ctx, "k", 1, 30000); err == nil {
		token = tok
	}

	stopFreeing := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFreeing:
				return
			default:
				time.Sleep(15 * time.Millisecond)
				fs.DecrConcurrent(ctx, "k", token)
				time.Sleep(5 * time.Millisecond)
				_, newTok, _ := fs.IncrConcurrent(ctx, "k", 1, 30000)
				token = newTok
			}
		}
	}()

	var wg sync.WaitGroup
	results := make(chan limiter.Action, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Check(ctx, limiter.Request{Key: "k"})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			results <- d.Action
		}()
	}
	wg.Wait()
	close(stopFreeing)
	close(results)

	for a := range results {
		if a != limiter.QUEUE && a != limiter.REJECT_429 {
			t.Errorf("expected every queued waiter to resolve to QUEUE or REJECT_429, got %v", a)
		}
	}
}
```

- [ ] **Step 3: Run the new tests to confirm they fail to compile**

Run: `cd services/core && go test ./limiter/... -run TestConcurrencyLimiter -v 2>&1 | head -20`
Expected: compile errors — `NewConcurrencyLimiter` called with 8 arguments but the current function only accepts 4; `l.BacklogDepth` undefined.

- [ ] **Step 4: Implement the backlog semaphore and polling loop in `concurrency.go`**

`services/core/limiter/concurrency.go` currently has two non-obvious WHY comments already in it (on the `unboundedCap` constant and on `Reconfigure`) — preserve both verbatim; do not delete them while making the changes below. The exact current file, with those comments included, is:

```go
package limiter

import (
	"context"
	"math"
	"sync"
)

// unboundedCap is passed as the Lua script's cap argument to force its
// `count < cap` check to always pass, so IncrConcurrent still reserves a
// slot even when the real cap is already exceeded. Used only for shadow
// mode's would-be-reject path, where the design spec requires the slot to
// still be reserved so concurrency accounting stays accurate. MaxInt32 is
// chosen to be far larger than any real concurrency count while staying
// well under Lua 5.1's 2^53 integer-precision limit for tonumber().
const unboundedCap = math.MaxInt32

type concurrencyChecker interface {
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error)
	DecrConcurrent(ctx context.Context, key, token string) error
}

type ConcurrencyLimiter struct {
	store concurrencyChecker

	mu            sync.RWMutex
	cap           int
	maxDurationMs int64
	shadowMode    bool
}

func NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{store: s, cap: cap, maxDurationMs: maxDurationMs, shadowMode: shadowMode}
}

// Reconfigure and Check run concurrently in ratecap-core: Reconfigure is
// invoked from the config watcher's goroutine while Check runs on every
// gRPC handler goroutine. The mutex keeps a reload from tearing
// cap/maxDurationMs apart mid-read, matching the design spec's
// atomic-hot-reload requirement (the same pattern TokenBucketLimiter uses).
func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, maxDurationMs, shadowMode := l.cap, l.maxDurationMs, l.shadowMode
	l.mu.RUnlock()

	allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: req.Key, Token: token}}, Tier: "concurrency_limiter"}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, req.Key, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: req.Key, Token: reservedToken}}, Tier: "concurrency_limiter"}, nil
	}

	return Decision{Action: REJECT_429, Tier: "concurrency_limiter"}, nil
}
```

Replace it with:

```go
package limiter

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
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

	backlog atomic.Int64
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

// BacklogDepth reports the current number of goroutines occupying a backlog
// slot. It exists for tests that need to observe live queue depth under
// concurrent load; production code never calls it.
func (l *ConcurrencyLimiter) BacklogDepth() int64 {
	return l.backlog.Load()
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
		return Decision{Action: REJECT_429, Tier: "concurrency_limiter"}, nil
	}

	if !l.acquireBacklogSlot(maxBacklog) {
		return Decision{Action: REJECT_429, Tier: "concurrency_limiter"}, nil
	}
	defer l.backlog.Add(-1)

	return l.pollUntilAllowedOrDeadline(ctx, req, cap, maxDurationMs, maxQueueWaitMs, pollIntervalMs)
}

// acquireBacklogSlot is a counting semaphore via CAS loop, mirroring
// worker.Shedder's exact pattern (services/sidecar/worker/shedder.go),
// rather than a buffered channel — maxBacklog is hot-reloadable via
// Reconfigure, and a channel's capacity cannot be resized after creation.
func (l *ConcurrencyLimiter) acquireBacklogSlot(maxBacklog int) bool {
	for {
		current := l.backlog.Load()
		if current >= int64(maxBacklog) {
			return false
		}
		if l.backlog.CompareAndSwap(current, current+1) {
			return true
		}
	}
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
			return Decision{Action: REJECT_429, Tier: "concurrency_limiter"}, nil
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

- [ ] **Step 5: Run all `concurrency_test.go` and `concurrency_queue_test.go` tests to confirm they pass**

Run: `cd services/core && go test ./limiter/... -run 'TestConcurrencyLimiter' -race -v`
Expected: every test `--- PASS`, including all pre-existing tests in `concurrency_test.go` (confirms the 8-argument signature change did not break existing behavior when queueing is off) and all new tests in `concurrency_queue_test.go`.

- [ ] **Step 6: Write the failing Pipeline test proving `QUEUE` does not short-circuit later tiers**

Add to `services/core/limiter/pipeline_test.go` (append after `TestPipeline_SecondTierRejectPropagatesDecision`):

```go
func TestPipeline_QueueFromEarlierTierContinuesToLaterTier(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.QUEUE, Tier: "concurrency_limiter"}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Tier: "fleet_shedder"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tier2.called {
		t.Fatal("expected tier2 (e.g. FleetShedder) to still be checked after tier1 returned QUEUE, not short-circuited")
	}
	if d.Action != limiter.QUEUE {
		t.Fatalf("expected the overall decision to still carry QUEUE for attribution, got %v", d.Action)
	}
	if d.Tier != "fleet_shedder" {
		t.Errorf(`expected the last tier's Tier ("fleet_shedder") to propagate, got %q`, d.Tier)
	}
}

func TestPipeline_LaterTierRejectAfterEarlierQueueStillWins(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.QUEUE, Tier: "concurrency_limiter"}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_503, Tier: "fleet_shedder"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_503 {
		t.Fatalf("expected REJECT_503 from tier2 to win over tier1's QUEUE, got %v", d.Action)
	}
}
```

- [ ] **Step 7: Run them to confirm they fail**

Run: `cd services/core && go test ./limiter/... -run 'TestPipeline_QueueFromEarlierTierContinuesToLaterTier|TestPipeline_LaterTierRejectAfterEarlierQueueStillWins' -v`
Expected: `TestPipeline_QueueFromEarlierTierContinuesToLaterTier` fails with `expected tier2 ... to still be checked after tier1 returned QUEUE, not short-circuited` (tier2 is never called, because the current `Pipeline.Check` short-circuits on any `d.Action != ALLOW`).

- [ ] **Step 8: Widen `Pipeline.Check`'s short-circuit condition to include `QUEUE`**

Change `services/core/limiter/pipeline.go` from:

```go
func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
	var reserved []TokenReservation
	var lastTier string
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		reserved = append(reserved, d.Reservations...)
		if err != nil || d.Action != ALLOW {
			d.Reservations = reserved
			return d, err
		}
		if d.Tier != "" {
			lastTier = d.Tier
		}
	}
	return Decision{Action: ALLOW, Reservations: reserved, Tier: lastTier}, nil
}
```

to:

```go
func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
	var reserved []TokenReservation
	var lastTier string
	finalAction := ALLOW
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		reserved = append(reserved, d.Reservations...)
		if err != nil || (d.Action != ALLOW && d.Action != QUEUE) {
			d.Reservations = reserved
			return d, err
		}
		if d.Action == QUEUE {
			finalAction = QUEUE
		}
		if d.Tier != "" {
			lastTier = d.Tier
		}
	}
	return Decision{Action: finalAction, Reservations: reserved, Tier: lastTier}, nil
}
```

- [ ] **Step 9: Run the Pipeline tests to confirm they pass**

Run: `cd services/core && go test ./limiter/... -run TestPipeline -race -v`
Expected: every test `--- PASS`, including all pre-existing `TestPipeline_*` tests (confirms the widened condition did not change behavior for any non-QUEUE action).

- [ ] **Step 10: Run gofmt and the full limiter package test suite**

Run: `gofmt -l services/core/limiter`
Expected: no output

Run: `cd services/core && go test ./limiter/... -race`
Expected: `ok`

- [ ] **Step 11: Commit**

```bash
git add services/core/limiter/concurrency.go services/core/limiter/concurrency_test.go services/core/limiter/concurrency_queue_test.go services/core/limiter/pipeline.go services/core/limiter/pipeline_test.go
git commit -m "feat(limiter): bounded backlog + polling queueing in ConcurrencyLimiter, widen Pipeline short-circuit for QUEUE"
```

---

## Task 4: Real-Redis integration test proving the polling loop works against the actual Lua script

**Files:**
- Test: `services/core/store/redis_test.go`

**Interfaces:**
- Consumes: `store.NewRedisStore` (existing, unchanged), `services/core/limiter.NewConcurrencyLimiter` (Task 3's new 8-argument signature).

**Why this task exists:** Every test in Task 3 uses `fakeConcurrencyStore`, which returns instantly — it cannot prove the polling loop genuinely round-trips to a real Redis instance under real network latency, only that the Go-level logic is correct against an idealized fake. This mirrors the project's own established pattern (`services/core/store/redis_test.go`'s existing `testcontainers-go`-based tests prove the Lua scripts' atomicity for real; Tier 4's zero-round-trip claim was verified live before shipping). This single test closes that gap cheaply — it reuses the existing `startRedis(t)` helper already defined in this file, adding no new test infrastructure.

- [ ] **Step 1: Write the failing integration test**

Add to `services/core/store/redis_test.go` (append after `TestIncrConcurrent_ConcurrentAtomicity`; note this file's package is `store_test`, and it will need to additionally import `"github.com/ratecap/core/limiter"`):

```go
func TestConcurrencyLimiter_QueueingPollsRealRedisUntilSlotFrees(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	l := limiter.NewConcurrencyLimiter(s, 1, 30000, false, true, 5, 3000, 50)
	ctx := context.Background()

	_, token, err := s.IncrConcurrent(ctx, "queue-integration-key", 1, 30000)
	if err != nil {
		t.Fatalf("unexpected error occupying the cap: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := s.DecrConcurrent(ctx, "queue-integration-key", token); err != nil {
			t.Errorf("unexpected error releasing: %v", err)
		}
	}()

	start := time.Now()
	d, err := l.Check(ctx, limiter.Request{Key: "queue-integration-key"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.QUEUE {
		t.Fatalf("expected QUEUE once the real Redis-backed slot frees, got %v", d.Action)
	}
	if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
		t.Fatalf("expected a real reservation token from the successful poll against real Redis, got %+v", d.Reservations)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("expected to wait for the real slot to free (~200ms), got %v — suspiciously fast, is this actually polling Redis?", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("expected to succeed well before the 3s MaxQueueWaitMs deadline, took %v", elapsed)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd services/core && go test ./store/... -run TestConcurrencyLimiter_QueueingPollsRealRedisUntilSlotFrees -v`
Expected: compile error before Task 3 lands (`NewConcurrencyLimiter` argument count mismatch) — since this task runs after Task 3 in this plan's sequence, the actual expected failure here is none: the test should already compile. Run it anyway to confirm it currently passes cleanly against Task 3's implementation; if Docker is unreachable in this environment, expect `testcontainers` to fail with a connection error to the Docker daemon, not a test assertion failure — that is an environment gap, not a code defect, and should be reported as such rather than treated as this task's failure signal.

- [ ] **Step 3: Confirm the test passes against a real Docker+Redis**

Run: `cd services/core && go test ./store/... -run TestConcurrencyLimiter_QueueingPollsRealRedisUntilSlotFrees -v`
Expected: `--- PASS`, with the real elapsed time printed via `-v` output falling between ~150ms and 3s.

- [ ] **Step 4: Run gofmt and the full store package test suite**

Run: `gofmt -l services/core/store`
Expected: no output

Run: `cd services/core && go test ./store/... -race`
Expected: `ok` (requires Docker; this package's tests already require Docker today, so this is not a new environment requirement introduced by this task)

- [ ] **Step 5: Commit**

```bash
git add services/core/store/redis_test.go
git commit -m "test(store): prove ConcurrencyLimiter queueing polls a real Redis-backed IncrConcurrent, not just a fake"
```

---

## Task 5: Wire queueing config through `main.go`'s construction and hot-reload call sites

**Files:**
- Modify: `services/core/main.go`
- Modify: `services/core/main_test.go` (update the duplicated construction/reload logic in `TestMain_SkipsInvalidConfigReloadWithoutReconfiguring` to match)
- Modify: `deploy/ratecap.yaml` (document the new fields, left commented-out/absent to keep queueing off in the demo stack by default — no demo behavior change)

**Interfaces:**
- Consumes: `config.ConcurrencyLimiterConfig`'s 4 new fields (Task 2). `limiter.NewConcurrencyLimiter(...)`'s widened 8-argument constructor and `Reconfigure(...)`'s widened 7-argument method (Task 3).

- [ ] **Step 1: Update the construction call site in `services/core/main.go`**

Change:

```go
	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
	)
```

to:

```go
	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
		cfg.Tiers.ConcurrencyLimiter.QueueingEnabled,
		cfg.Tiers.ConcurrencyLimiter.MaxBacklog,
		cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs,
		cfg.Tiers.ConcurrencyLimiter.PollIntervalMs,
	)
```

- [ ] **Step 2: Update the hot-reload call site in `services/core/main.go`**

Change:

```go
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
```

to:

```go
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode, newCfg.Tiers.ConcurrencyLimiter.QueueingEnabled, newCfg.Tiers.ConcurrencyLimiter.MaxBacklog, newCfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs, newCfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
```

- [ ] **Step 3: Confirm `services/core` builds**

Run: `cd services/core && go build ./...`
Expected: no output (success). If this fails, it means `main_test.go`'s duplicated construction logic (see Step 4) is now out of sync — fix that before proceeding.

- [ ] **Step 4: Update the duplicated call sites in `services/core/main_test.go`**

`TestMain_SkipsInvalidConfigReloadWithoutReconfiguring` duplicates `main.go`'s construction and reload logic to test the reload path directly (it does not invoke `main()` itself). Change:

```go
	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
	)
```

to:

```go
	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
		cfg.Tiers.ConcurrencyLimiter.QueueingEnabled,
		cfg.Tiers.ConcurrencyLimiter.MaxBacklog,
		cfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs,
		cfg.Tiers.ConcurrencyLimiter.PollIntervalMs,
	)
```

And change:

```go
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
```

to:

```go
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode, newCfg.Tiers.ConcurrencyLimiter.QueueingEnabled, newCfg.Tiers.ConcurrencyLimiter.MaxBacklog, newCfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs, newCfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
```

The `validConfig` YAML string earlier in the same test function does not need new fields added — the new config fields correctly default to their zero values (`queueing_enabled: false` implicitly), which is exactly the "no behavior change for existing configs" guarantee this plan must preserve, and this test's existing assertions (about `fleetShedder`, not `concurrencyLimiter`) are unaffected either way.

- [ ] **Step 5: Confirm `services/core` builds and the full test suite passes**

Run: `cd services/core && go build ./...`
Expected: no output

Run: `cd services/core && go test ./... -race`
Expected: `ok` for every package (Docker-dependent packages skip or pass depending on Docker availability, matching pre-existing behavior)

- [ ] **Step 6: Document the new fields in `deploy/ratecap.yaml` (commented out, queueing stays off in the demo)**

Change:

```yaml
  concurrency_limiter:
    default_max_concurrent: 3
    max_request_duration_ms: 30000
    shadow_mode: false
```

to:

```yaml
  concurrency_limiter:
    default_max_concurrent: 3
    max_request_duration_ms: 30000
    shadow_mode: false
    # Bounded queueing (v2 Phase 3) — off by default, uncomment to enable:
    # queueing_enabled: true
    # max_backlog: 20
    # max_queue_wait_ms: 2000
    # poll_interval_ms: 25
```

- [ ] **Step 7: Confirm the demo config still parses and validates with queueing left commented out**

Run: `cd services/core && go run . -h 2>&1 | head -1 || true` — this is not the actual validation path; instead, directly exercise `config.Load`+`Validate` against the real file from the repo root:

```bash
cd services/core && cat <<'EOF' > /tmp/validate_demo_config.go
package main

import (
	"fmt"
	"os"

	"github.com/ratecap/core/config"
)

func main() {
	cfg, err := config.Load("../deploy/ratecap.yaml")
	if err != nil {
		fmt.Println("LOAD ERROR:", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Println("VALIDATE ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("OK: demo config is valid, queueing_enabled =", cfg.Tiers.ConcurrencyLimiter.QueueingEnabled)
}
EOF
go run /tmp/validate_demo_config.go
rm /tmp/validate_demo_config.go
```

Expected output: `OK: demo config is valid, queueing_enabled = false`

- [ ] **Step 8: Commit**

```bash
git add services/core/main.go services/core/main_test.go deploy/ratecap.yaml
git commit -m "feat(core): wire queueing config through construction and hot-reload call sites"
```

---

## Task 6: Documentation — `ARCHITECTURE.md` and `SECURITY.md`

**Files:**
- Modify: `ARCHITECTURE.md`
- Modify: `SECURITY.md`

**Interfaces:** None — pure documentation, no code.

- [ ] **Step 1: Update `ARCHITECTURE.md`'s Tier 1 section's forward-reference to `QUEUE`**

Change the existing line (currently in the "Tier 1: Request Rate Limiter" section, describing the fixed 4-value enum):

```markdown
- **Response actions:** `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG` — a fixed 4-value enum. `REJECT_503` and a future `QUEUE` action are reserved for tiers/behaviors not yet built; v1 only emits `ALLOW`, `REJECT_429`, and `SHADOW_LOG`.
```

to:

```markdown
- **Response actions:** `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`, `QUEUE` — a 5-value enum (v1 shipped the first 4; `QUEUE` was added in v2 Phase 3, see below). `REJECT_503` remains reserved for `FleetShedder`'s shed path; Tier 1 itself only ever emits `ALLOW`, `REJECT_429`, and `SHADOW_LOG`.
```

- [ ] **Step 2: Add a new "Tier 2 bounded queueing (v2 Phase 3)" section to `ARCHITECTURE.md`**

Insert a new section immediately after the existing "## Swappable interfaces (why v2 doesn't require a rewrite)" section (i.e. after its closing paragraph, before "## Configuration and hot-reload"):

```markdown
## Tier 2 bounded queueing (v2 Phase 3)

`ConcurrencyLimiter` optionally queues a request that finds the concurrency cap full, instead of instantly rejecting it. This is off by default (`queueing_enabled: false`) — enabling it is an explicit per-deployment opt-in with no change to existing behavior otherwise.

When enabled, a request that finds the cap full first tries to acquire a slot in a bounded local semaphore (`max_backlog`). If the semaphore is full, the request is rejected immediately, exactly like today's non-queueing behavior — queueing never makes rejection *more* likely, only adds a bounded chance of eventual success. If a slot is acquired, the request polls the existing, unmodified `IncrConcurrent` Redis Lua script every `poll_interval_ms` until it succeeds, `max_queue_wait_ms` elapses, or the request's context is canceled.

**This backlog is per-`ratecap-core`-instance, not fleet-wide.** Each core instance enforces its own `max_backlog` independently; there is no cross-instance coordination of queue depth. Worst-case total backlog across a fleet of N core instances is `max_backlog × N`, not a single coordinated ceiling. This mirrors Tier 4's already-accepted local-only worker shedder (`services/sidecar/worker/shedder.go`) — RateCap already has this exact category of precedent, and it is stated here deliberately rather than left implicit.

No ordering (LIFO/FIFO) is imposed on waiters — with independent polling goroutines, "who gets served first" is naturally whichever waiter's poll happens to succeed first. A queued-then-served request is fully transparent to the client: it returns a plain `200`, with the `QUEUE` action existing only for server-side attribution (feeding `ratecap_decisions_total{tier="concurrency_limiter",action="queue"}` and structured decision logs, where the elevated `latency_ms` already makes queueing visible without a dedicated wire field).
```

- [ ] **Step 3: Update `SECURITY.md`'s Network Transport Security section's affected-component list if needed, and add a queueing-specific note**

Insert a new paragraph at the end of the existing "## Network Transport Security" section (after its last bullet, before the "## Priority Claims (v1)" heading):

```markdown
### Bounded queueing backlog is per-instance (v2 Phase 3)

`ConcurrencyLimiter`'s optional bounded queueing (`queueing_enabled`, off by default) enforces `max_backlog` independently on each `ratecap-core` instance — it is not coordinated across a fleet. An operator running N core instances with the same `max_backlog` value should expect up to `max_backlog × N` total in-flight queued requests fleet-wide, not a single shared ceiling. This is a known, accepted limitation (matching Tier 4's existing local-only worker shedder), not an oversight — if your deployment needs a fleet-wide coordinated backlog ceiling, do not rely on `max_backlog` alone to provide it.
```

- [ ] **Step 4: Confirm both files still render as valid markdown (no broken headers/links)**

Run: `grep -c "^## " ARCHITECTURE.md SECURITY.md`
Expected: a count for each file consistent with one new `##` heading added to `SECURITY.md` is NOT the case here (the new content is a `###` sub-section within the existing `## Network Transport Security` heading, and a `##` section in `ARCHITECTURE.md`) — confirm `ARCHITECTURE.md`'s count increased by exactly 1 compared to its pre-change count, and `SECURITY.md`'s `## ` count is unchanged (only a new `### ` sub-heading was added there).

- [ ] **Step 5: Commit**

```bash
git add ARCHITECTURE.md SECURITY.md
git commit -m "docs: document v2 Phase 3 bounded queueing, including the per-instance backlog limitation"
```

---

## Self-Review Notes (completed during plan authoring)

**Spec coverage:**
- §1 (mechanism) → Task 3 (backlog semaphore + polling loop), verified in a scratch module before inclusion.
- §1 (documented per-instance limitation) → Task 6.
- §2 (scope: `ConcurrencyLimiter` only) → enforced throughout; no task touches `fleetshedder.go` or `tokenbucket.go`.
- §3 (config, off by default, extends `Reconfigure`) → Task 2 (schema+validation) and Task 5 (wiring).
- §4 (no ordering) → Task 3's implementation uses a plain CAS-loop semaphore with no queue data structure; `TestConcurrencyLimiter_StressManyWaitersOneSlotFreeingRepeatedly` characterizes the resulting unordered behavior.
- §5 (wire-transparent, `QUEUE` for server-side attribution only) → Task 1 (enum + every conversion boundary) and Task 3 (the one code path that actually returns it).
- §6 (unit tests + real concurrency stress tests) → Task 3's `concurrency_queue_test.go`, mirroring `shedder_test.go`'s style; Task 4 adds the real-Redis integration proof.
- Out-of-scope items (FleetShedder queueing, ordering primitives, client-visible signals, fleet-wide coordination, Redis BLPOP/pub-sub) → none implemented anywhere in this plan; confirmed by scope review of every task's file list.

**Two real design gaps found and resolved during planning** (documented in the Global Constraints section above): the spec's literal "QUEUE is never returned by Check()" framing was checked against the actual mechanism and corrected (a successful poll returns `QUEUE`, not `ALLOW`, or the action would be permanently unreachable dead code) — and this in turn required widening `Pipeline.Check`'s short-circuit condition, since the original condition would have silently skipped `FleetShedder` for every queued request. Both were verified against real compiled-and-tested code in an isolated scratch module (`/tmp/ratecap-phase3-scratch2`) before being written into this plan, including a race-detector run.

**Placeholder scan:** no "TBD"/"TODO" strings; every code step shows complete code; every command step shows exact expected output (including the one intentionally-nuanced case in Task 1 Step 9 and Task 4 Step 2, where the "expected" output is explained rather than a bare string, because the true failure signal is more subtle than a simple pass/fail).

**Type consistency:** `NewConcurrencyLimiter`'s 8-parameter signature (Task 3) and `Reconfigure`'s 7-parameter signature (Task 3) are used identically in Task 5's `main.go`/`main_test.go` call sites and Task 4's integration test — same parameter order (`cap, maxDurationMs, shadowMode, queueingEnabled, maxBacklog, maxQueueWaitMs, pollIntervalMs`) throughout. `config.ConcurrencyLimiterConfig`'s 4 new field names (Task 2: `QueueingEnabled`, `MaxBacklog`, `MaxQueueWaitMs`, `PollIntervalMs`) match exactly what Task 5 reads off `cfg.Tiers.ConcurrencyLimiter.*`. `limiter.QUEUE` (Task 1) is the same symbol Task 3 returns and Task 1's `toProtoAction`/`actionLabel` convert.
