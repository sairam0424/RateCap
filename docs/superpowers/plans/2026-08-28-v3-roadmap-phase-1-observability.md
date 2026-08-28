# RateCap v3 Roadmap — Phase 1: Observability Foundation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** give `services/core` the self-instrumentation it is missing (metrics, real health checks, a documented fail-open contract), move the sidecar's `/metrics` off its own self-throttled request path, and ship a starter Grafana dashboard — so Phase 2's reliability tests have a real signal to assert against.

**Architecture:** Add a new `services/core/metrics` package mirroring the existing `services/sidecar/metrics` package's shape (promauto metric vars + `RecordX` helpers + `Handler()`), wire a gRPC server interceptor and a new `:9092` HTTP listener into `services/core/main.go`, instrument `services/core/store/redis.go` and the three `services/core/limiter/*.go` tiers, and extend `services/sidecar/metrics` + `services/sidecar/proxy/proxy.go` + `services/sidecar/main.go` with the sidecar-side additions the spec calls for.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang v1.24.1` (new dependency for `services/core`, already used by `services/sidecar` — pinning the same version avoids reintroducing the cross-module skew Phase 0 just fixed), `prometheus/client_golang/prometheus/testutil` for metric assertions in tests.

**Spec:** `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 1 section (items 1–8; item 9, OpenTelemetry tracing, is the spec's own explicitly-named stretch item and is deferred to Phase 5 per its own text: *"if Phase 1's timeline is tight, this item alone can slip to Phase 5 without blocking anything else here"*).

## Global Constraints

- **Fail-open scope (user decision, 2026-08-28):** only Tier 1 (`TokenBucketLimiter`, the request-rate limiter) fails open on a store error. Tiers 2/3 (`ConcurrencyLimiter`, `FleetShedder`) stay fail-**closed** — matching Stripe's own documented precedent already cited in the spec's Phase 2 item 3 ("fail-open on request-rate, fail-closed on concurrent-requests"). This reverses the spec's Phase 1 item 2 assumption that fail-open already existed for all three tiers — it existed for none; Task 5 below implements it for Tier 1 only. Phase 2's planned `TestTier1/2/3_RedisUnavailable_FailsOpen` (when that phase is planned) must be corrected to `TestTier1_RedisUnavailable_FailsOpen` / `TestTier2_RedisUnavailable_FailsClosed` / `TestTier3_RedisUnavailable_FailsClosed`.
- **No behavior change to Tier 2/3 error handling.** Only Tier 1's `Check` method changes; `ConcurrencyLimiter.Check` and `FleetShedder.Check` keep returning `Decision{}, err` on a store error.
- **Metric names are fixed by this plan** — later tasks (dashboard, docs) reference the exact names Tasks 1–11 define. Do not rename mid-implementation.
- **`-race` is mandatory** on every `go test` invocation touching `services/core` or `services/sidecar`, per this repo's `CLAUDE.md`.
- **`services/core`'s Redis-integration tests require real Docker** (`testcontainers-go`, confirmed by the existing pattern in `services/core/store/redis_test.go`). If Docker is unavailable in the execution environment, run those specific tests with `-run` exclusions and say so explicitly in the task report — do not silently skip without noting it.
- **No comments except non-obvious WHY**, matching the existing codebase convention (see `services/core/store/redis.go`'s `signToken` comment for the house style).
- Files: 200–400 lines typical, 800 max (`CLAUDE.md` convention).

---

### Task 1: `services/core/metrics` package — metric definitions

**Files:**
- Create: `services/core/metrics/metrics.go`
- Test: `services/core/metrics/metrics_test.go`
- Modify: `services/core/go.mod` (add `github.com/prometheus/client_golang v1.24.1`), `services/core/go.sum`

**Interfaces:**
- Produces: `metrics.RecordGRPCRequest(method, code string, duration time.Duration)`, `metrics.RecordRedisCall(operation string, duration time.Duration, err error)`, `metrics.RecordConfigReload(result string)` (result is `"success"` or `"failure"`), `metrics.RecordFailOpen(tier, reason string)`, `metrics.Handler() http.Handler`. Task 2 (interceptor), Task 4 (Redis instrumentation), Task 5 (fail-open), Task 6 (config reload) all call these.

- [ ] **Step 1: Add the dependency**

Run from `services/core`:
```bash
go get github.com/prometheus/client_golang@v1.24.1
```
This updates `go.mod`/`go.sum`. Verify the resolved version matches `services/sidecar/go.mod`'s existing `github.com/prometheus/client_golang v1.24.1` exactly — a mismatch would reintroduce the cross-module dependency skew Phase 0's Task 8/9 fixed.

- [ ] **Step 2: Write the failing test**

```go
// services/core/metrics/metrics_test.go
package metrics_test

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/core/metrics"
)

func TestRecordGRPCRequest_IncrementsCounterWithMethodAndCode(t *testing.T) {
	metrics.RecordGRPCRequest("CheckRateLimit", "OK", 10*time.Millisecond)

	got := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "OK"))
	if got != 1 {
		t.Errorf("expected counter=1 for method=CheckRateLimit code=OK, got %v", got)
	}
}

func TestRecordGRPCRequest_ObservesDuration(t *testing.T) {
	before := testutil.CollectAndCount(metrics.GRPCRequestDuration)
	metrics.RecordGRPCRequest("ReleaseConcurrency", "OK", 5*time.Millisecond)
	after := testutil.CollectAndCount(metrics.GRPCRequestDuration)

	if after <= before {
		t.Errorf("expected GRPCRequestDuration observation count to increase, before=%d after=%d", before, after)
	}
}

func TestRecordRedisCall_NoErrorOnlyObservesDuration(t *testing.T) {
	before := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("check_and_decrement"))
	metrics.RecordRedisCall("check_and_decrement", time.Millisecond, nil)
	after := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("check_and_decrement"))

	if after != before {
		t.Errorf("expected RedisErrorsTotal unchanged on a nil error, before=%v after=%v", before, after)
	}
}

func TestRecordRedisCall_ErrorIncrementsErrorCounter(t *testing.T) {
	before := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("incr_concurrent"))
	metrics.RecordRedisCall("incr_concurrent", time.Millisecond, errors.New("dial tcp: connection refused"))
	after := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("incr_concurrent"))

	if after != before+1 {
		t.Errorf("expected RedisErrorsTotal to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordConfigReload_IncrementsByResult(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConfigReloadTotal.WithLabelValues("success"))
	metrics.RecordConfigReload("success")
	after := testutil.ToFloat64(metrics.ConfigReloadTotal.WithLabelValues("success"))

	if after != before+1 {
		t.Errorf("expected ConfigReloadTotal{result=success} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordFailOpen_IncrementsByTierAndReason(t *testing.T) {
	before := testutil.ToFloat64(metrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	metrics.RecordFailOpen("rate_limiter", "store_error")
	after := testutil.ToFloat64(metrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before+1 {
		t.Errorf("expected FailOpenTotal{tier=rate_limiter,reason=store_error} to increment by 1, before=%v after=%v", before, after)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd services/core && go test ./metrics/... -race -v`
Expected: FAIL — `package metrics` does not exist yet.

- [ ] **Step 4: Write the implementation**

```go
// services/core/metrics/metrics.go
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var GRPCRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_grpc_requests_total",
	Help: "Total number of gRPC requests handled by ratecap-core, labeled by method and resulting status code.",
}, []string{"method", "code"})

var GRPCRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_core_grpc_request_duration_seconds",
	Help: "Latency of gRPC requests handled by ratecap-core, labeled by method.",
}, []string{"method"})

var RedisCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_core_redis_call_duration_seconds",
	Help: "Latency of Redis calls made by ratecap-core, labeled by operation.",
}, []string{"operation"})

var RedisErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_redis_errors_total",
	Help: "Total number of failed Redis calls made by ratecap-core, labeled by operation.",
}, []string{"operation"})

var ConfigReloadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_config_reload_total",
	Help: "Total number of config hot-reload attempts, labeled by result (success or failure).",
}, []string{"result"})

// FailOpenTotal has no ratecap_core_ prefix: fail-open is a fleet-wide safety
// signal meaningful regardless of which binary emits it, and the spec names
// it ratecap_fail_open_total verbatim.
var FailOpenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_fail_open_total",
	Help: "Total number of requests allowed through via fail-open behavior after a tier's backing store call errored.",
}, []string{"tier", "reason"})

func RecordGRPCRequest(method, code string, duration time.Duration) {
	GRPCRequestsTotal.WithLabelValues(method, code).Inc()
	GRPCRequestDuration.WithLabelValues(method).Observe(duration.Seconds())
}

func RecordRedisCall(operation string, duration time.Duration, err error) {
	RedisCallDuration.WithLabelValues(operation).Observe(duration.Seconds())
	if err != nil {
		RedisErrorsTotal.WithLabelValues(operation).Inc()
	}
}

func RecordConfigReload(result string) {
	ConfigReloadTotal.WithLabelValues(result).Inc()
}

func RecordFailOpen(tier, reason string) {
	FailOpenTotal.WithLabelValues(tier, reason).Inc()
}

func Handler() http.Handler {
	return promhttp.Handler()
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd services/core && go test ./metrics/... -race -v`
Expected: PASS (all 6 tests)

- [ ] **Step 6: Commit**

```bash
git add services/core/metrics/metrics.go services/core/metrics/metrics_test.go services/core/go.mod services/core/go.sum
git commit -m "feat(core): add services/core/metrics package"
```

---

### Task 2: gRPC server-side metrics interceptor

**Files:**
- Create: `services/core/metrics/interceptor.go`
- Test: `services/core/metrics/interceptor_test.go`

**Interfaces:**
- Consumes: `metrics.RecordGRPCRequest(method, code string, duration time.Duration)` from Task 1.
- Produces: `metrics.UnaryServerInterceptor() grpc.UnaryServerInterceptor` — Task 3 chains this into `services/core/main.go` after `auth.UnaryServerInterceptor`.

- [ ] **Step 1: Write the failing test**

```go
// services/core/metrics/interceptor_test.go
package metrics_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/core/metrics"
)

func TestUnaryServerInterceptor_RecordsOKOnSuccess(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) {
		return "response", nil
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "OK"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "OK"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=CheckRateLimit,code=OK} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsStatusCodeOnError(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/ReleaseConcurrency"}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.PermissionDenied, "invalid concurrency token")
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("ReleaseConcurrency", "PermissionDenied"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("ReleaseConcurrency", "PermissionDenied"))

	if err == nil {
		t.Fatal("expected the handler's error to propagate")
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=ReleaseConcurrency,code=PermissionDenied} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsUnknownForNonStatusError(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("plain error, not a grpc status")
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "Unknown"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "Unknown"))

	if err == nil {
		t.Fatal("expected the handler's error to propagate")
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=CheckRateLimit,code=Unknown} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_StripsServicePrefixFromMethod(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	_, _ = interceptor(context.Background(), "request", info, handler)

	got := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "OK"))
	if got == 0 {
		t.Error("expected the method label to be the bare RPC name (CheckRateLimit), not the full /service/method path")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/core && go test ./metrics/... -race -run TestUnaryServerInterceptor -v`
Expected: FAIL with "undefined: metrics.UnaryServerInterceptor"

- [ ] **Step 3: Write the implementation**

```go
// services/core/metrics/interceptor.go
package metrics

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// methodName strips the "/ratecap.v1.RatecapService/" prefix from
// info.FullMethod so the metric's method label matches the RPC name callers
// actually recognize (CheckRateLimit, ReleaseConcurrency), not the full
// gRPC wire path.
func methodName(fullMethod string) string {
	if idx := strings.LastIndex(fullMethod, "/"); idx != -1 {
		return fullMethod[idx+1:]
	}
	return fullMethod
}

func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		RecordGRPCRequest(methodName(info.FullMethod), status.Code(err).String(), time.Since(start))
		return resp, err
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd services/core && go test ./metrics/... -race -v`
Expected: PASS (all tests from Task 1 and Task 2)

- [ ] **Step 5: Commit**

```bash
git add services/core/metrics/interceptor.go services/core/metrics/interceptor_test.go
git commit -m "feat(core): add gRPC server-side metrics interceptor"
```

---

### Task 3: Wire the interceptor and a `/metrics` HTTP listener into `services/core/main.go`

**Files:**
- Modify: `services/core/main.go:115,124` (interceptor chaining), add new listener block after the health-server block (after line 148)

**Interfaces:**
- Consumes: `metrics.UnaryServerInterceptor()` (Task 2), `metrics.Handler()` (Task 1).

- [ ] **Step 1: Chain the metrics interceptor after the auth interceptor**

In `services/core/main.go`, change:
```go
	serverOpts := []grpc.ServerOption{grpc.UnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret))}
```
to:
```go
	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret), coremetrics.UnaryServerInterceptor()),
	}
```
Auth runs first so an unauthenticated call never reaches the metrics interceptor — `ratecap_core_grpc_requests_total` should reflect real authenticated traffic, not authentication-probe noise.

Add the import (aliased to avoid colliding with the standard-library-shaped name `metrics` some readers might expect from elsewhere):
```go
	coremetrics "github.com/ratecap/core/metrics"
```

- [ ] **Step 2: Add the `/metrics` HTTP listener**

Add this block immediately after the existing health-server block (after the `go func() { ... }()` that starts `healthGRPCServer`, before the final `log.Printf("ratecap-core listening on %s", listenAddr)`):

```go
	metricsAddr := os.Getenv("RATECAP_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9092"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", coremetrics.Handler())
	metricsServer := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("ratecap-core metrics server listening on %s", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics http server failed: %v", err)
		}
	}()
```

Add `"errors"`, `"net/http"`, and `"time"` to the import block (`"time"` and `"net/http"` are new to this file; `"errors"` is new too).

- [ ] **Step 3: Verify the build**

Run: `cd services/core && go build ./...`
Expected: builds cleanly.

- [ ] **Step 4: Manual verification**

`main.go` has no existing test file (this repo's convention — see `services/sidecar/main.go` vs. its own `main_test.go`, which only tests the extracted pure helpers, never wiring). Verify manually instead:
```bash
cd deploy && bash generate-demo-certs.sh && docker compose up --build -d
curl -s localhost:9092/metrics | grep ratecap_core_
docker compose down
```
Expected: `ratecap_core_grpc_requests_total`, `ratecap_core_grpc_request_duration_seconds`, `ratecap_core_redis_call_duration_seconds`, `ratecap_core_redis_errors_total`, and `ratecap_core_config_reload_total` all appear (the redis/config metrics will show `_total 0`/no samples until Task 4/6 land and traffic flows — that's expected at this point in the plan; re-verify at the end of Task 9).

- [ ] **Step 5: Commit**

```bash
git add services/core/main.go
git commit -m "feat(core): wire gRPC metrics interceptor and /metrics HTTP listener"
```

---

### Task 4: Instrument `services/core/store/redis.go` with call latency and error metrics

**Files:**
- Modify: `services/core/store/redis.go` (all three `StateStore` methods)
- Test: `services/core/store/redis_test.go` (add cases; existing tests already require Docker via `startRedis(t)`)

**Interfaces:**
- Consumes: `metrics.RecordRedisCall(operation string, duration time.Duration, err error)` (Task 1).

- [ ] **Step 1: Write the failing tests**

Append to `services/core/store/redis_test.go` (same file, same `startRedis(t)` helper already defined there):

```go
func TestCheckAndDecrement_RecordsRedisCallMetric(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	before := testutil.CollectAndCount(coremetrics.RedisCallDuration)
	_, _, err := s.CheckAndDecrement(ctx, "test-key-metric", 10, 5, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := testutil.CollectAndCount(coremetrics.RedisCallDuration)

	if after <= before {
		t.Errorf("expected RedisCallDuration observation count to increase, before=%d after=%d", before, after)
	}
}

func TestIncrConcurrent_RecordsRedisCallMetric(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	before := testutil.CollectAndCount(coremetrics.RedisCallDuration)
	_, _, err := s.IncrConcurrent(ctx, "test-key-metric-incr", 10, 60000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := testutil.CollectAndCount(coremetrics.RedisCallDuration)

	if after <= before {
		t.Errorf("expected RedisCallDuration observation count to increase, before=%d after=%d", before, after)
	}
}

func TestDecrConcurrent_ErrorIncrementsErrorCounter(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	// Closing the client first forces a real connection error on the next
	// call, giving a genuine non-nil err to assert against instead of
	// simulating one.
	if err := client.Close(); err != nil {
		t.Fatalf("failed to close client: %v", err)
	}

	before := testutil.ToFloat64(coremetrics.RedisErrorsTotal.WithLabelValues("decr_concurrent"))
	_ = s.DecrConcurrent(ctx, "test-key", "some-token")
	after := testutil.ToFloat64(coremetrics.RedisErrorsTotal.WithLabelValues("decr_concurrent"))

	if after != before+1 {
		t.Errorf("expected RedisErrorsTotal{operation=decr_concurrent} to increment by 1 on a closed-client error, before=%v after=%v", before, after)
	}
}
```

Add to the test file's imports:
```go
	"github.com/prometheus/client_golang/prometheus/testutil"

	coremetrics "github.com/ratecap/core/metrics"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/core && go test ./store/... -race -run "RecordsRedisCallMetric|ErrorIncrementsErrorCounter" -v`
Expected: FAIL (no metric observations recorded yet — `after <= before` / `after != before+1` assertions fail).

- [ ] **Step 3: Write the implementation**

In `services/core/store/redis.go`, add the import:
```go
	coremetrics "github.com/ratecap/core/metrics"
```

Change each method to record timing around the existing Redis call. `CheckAndDecrement`:
```go
func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	key = rateLimiterKeyPrefix + key
	now := time.Now().UnixMilli()
	start := time.Now()
	result, err := s.tokenBucket.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	coremetrics.RecordRedisCall("check_and_decrement", time.Since(start), err)
	if err != nil {
		return false, 0, err
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}
	...
```
(the rest of the method body is unchanged — only the two new lines and passing `err` through are added).

`IncrConcurrent`:
```go
func (s *RedisStore) IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error) {
	key = concurrencyKeyPrefix + key
	now := time.Now().UnixMilli()
	candidateToken := signToken(uuid.NewString(), s.signingKey)

	start := time.Now()
	result, err := s.concurrentLimiter.Run(ctx, s.client, []string{key}, cap, maxDurationMs, now, candidateToken).Slice()
	coremetrics.RecordRedisCall("incr_concurrent", time.Since(start), err)
	if err != nil {
		return false, "", err
	}
	...
```

`DecrConcurrent`:
```go
func (s *RedisStore) DecrConcurrent(ctx context.Context, key, token string) error {
	start := time.Now()
	err := s.client.ZRem(ctx, concurrencyKeyPrefix+key, token).Err()
	coremetrics.RecordRedisCall("decr_concurrent", time.Since(start), err)
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/core && go test ./store/... -race -v`
Expected: PASS (all existing store tests plus the 3 new ones). Requires Docker; if unavailable, run `go build ./...` to confirm compilation and note the skipped integration run in the task report.

- [ ] **Step 5: Commit**

```bash
git add services/core/store/redis.go services/core/store/redis_test.go
git commit -m "feat(core): instrument RedisStore calls with latency and error metrics"
```

---

### Task 5: Implement Tier 1 fail-open + `ratecap_fail_open_total`

**Files:**
- Modify: `services/core/limiter/tokenbucket.go`
- Test: Create `services/core/limiter/tokenbucket_test.go` if it does not already exist, or extend it if it does (check first: `ls services/core/limiter/tokenbucket_test.go`)

**Interfaces:**
- Consumes: `metrics.RecordFailOpen(tier, reason string)` (Task 1).
- Produces: `TokenBucketLimiter.Check` now returns `Decision{Action: ALLOW, Tier: "rate_limiter"}, nil` (not an error) when the store call itself errors — this is a behavior change other tasks/tests must not treat as a regression.

- [ ] **Step 1: Check for an existing test file and its fake-store pattern**

Run: `ls services/core/limiter/*_test.go` and read `services/core/limiter/tokenbucket_test.go` if present, to reuse its existing fake `checker` implementation rather than defining a second one. If no fake store test double exists yet in that file, define one matching this shape (used by Step 2 below):

```go
type fakeCheckerStore struct {
	allowed      bool
	retryAfterMs int64
	err          error
}

func (f *fakeCheckerStore) CheckAndDecrement(_ context.Context, _ string, _, _, _ int) (bool, int64, error) {
	return f.allowed, f.retryAfterMs, f.err
}
```

- [ ] **Step 2: Write the failing test**

```go
func TestCheck_FailsOpenOnStoreError(t *testing.T) {
	store := &fakeCheckerStore{err: errors.New("dial tcp: connection refused")}
	l := NewTokenBucketLimiter(store, 100, 500, false)

	decision, err := l.Check(context.Background(), Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("expected fail-open (no error), got: %v", err)
	}
	if decision.Action != ALLOW {
		t.Errorf("expected Action=ALLOW on a store error (fail-open), got %v", decision.Action)
	}
	if decision.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, decision.Tier)
	}
}

func TestCheck_RecordsFailOpenMetricOnStoreError(t *testing.T) {
	store := &fakeCheckerStore{err: errors.New("dial tcp: connection refused")}
	l := NewTokenBucketLimiter(store, 100, 500, false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	_, _ = l.Check(context.Background(), Request{Key: "user-1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before+1 {
		t.Errorf("expected FailOpenTotal{tier=rate_limiter,reason=store_error} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestCheck_NoStoreErrorDoesNotRecordFailOpen(t *testing.T) {
	store := &fakeCheckerStore{allowed: true}
	l := NewTokenBucketLimiter(store, 100, 500, false)

	before := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	_, _ = l.Check(context.Background(), Request{Key: "user-1", Cost: 1})
	after := testutil.ToFloat64(coremetrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before {
		t.Errorf("expected FailOpenTotal unchanged when the store call succeeds, before=%v after=%v", before, after)
	}
}
```

Add to the test file's imports: `"errors"`, `"github.com/prometheus/client_golang/prometheus/testutil"`, `coremetrics "github.com/ratecap/core/metrics"`.

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd services/core && go test ./limiter/... -race -run TestCheck_.*StoreError -v`
Expected: FAIL — current code returns `Decision{}, err` (a non-nil error), not `ALLOW`.

- [ ] **Step 4: Write the implementation**

In `services/core/limiter/tokenbucket.go`, change:
```go
	allowed, retryAfterMs, err := l.store.CheckAndDecrement(ctx, req.Key, rate, burst, req.Cost)
	if err != nil {
		return Decision{}, err
	}
```
to:
```go
	allowed, retryAfterMs, err := l.store.CheckAndDecrement(ctx, req.Key, rate, burst, req.Cost)
	if err != nil {
		// Fail OPEN for Tier 1 only, matching Stripe's documented precedent
		// (fail-open on request-rate, fail-closed on concurrent-requests —
		// see docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md
		// Phase 2 item 3). Tiers 2/3 (ConcurrencyLimiter, FleetShedder) do
		// NOT get this treatment: their whole purpose is bounding concurrent
		// resource usage, so letting them fail open would remove the bound
		// they exist to enforce during exactly the outage when it matters most.
		metrics.RecordFailOpen("rate_limiter", "store_error")
		return Decision{Action: ALLOW, Tier: "rate_limiter"}, nil
	}
```

Add the import:
```go
	"github.com/ratecap/core/metrics"
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -v`
Expected: PASS (all existing limiter tests plus the 3 new ones — confirm no `ConcurrencyLimiter`/`FleetShedder` test broke, since neither changed).

- [ ] **Step 6: Commit**

```bash
git add services/core/limiter/tokenbucket.go services/core/limiter/tokenbucket_test.go
git commit -m "feat(core): fail open on Tier 1 store errors, emit ratecap_fail_open_total"
```

---

### Task 6: Config-reload success/failure counter

**Files:**
- Modify: `services/core/config/watcher.go`, `services/core/config/watcher_test.go`, `services/core/main.go`

**Interfaces:**
- Consumes: `metrics.RecordConfigReload(result string)` (Task 1).
- Produces: `config.Watch`'s `onChange` callback signature changes from `func(*Config)` to `func(*Config, error)` — `err` is non-nil (and `cfg` is nil) when `Load` itself fails (bad YAML), or nil with a valid `cfg` when `Load` succeeds. This is a breaking signature change to `Watch`'s only parameter type; `services/core/main.go` is its only caller in this repo.

- [ ] **Step 1: Update the existing watcher tests for the new signature**

In `services/core/config/watcher_test.go`, change every `config.Watch(path, func(cfg *config.Config) { changes <- cfg })` call site to `config.Watch(path, func(cfg *config.Config, err error) { changes <- cfg })` (three call sites: `TestWatch_TriggersOnChangeOnFileWrite`, `TestWatch_DebouncesRapidFireEvents`, `TestWatch_SkipsInvalidConfigWithoutCrashing`). `TestWatch_SkipsInvalidConfigWithoutCrashing`'s premise (`onChange should not be called for invalid config`) does not change — that test's invalid input is a YAML value type error caught by `Load` itself, so the callback still isn't invoked with a valid `cfg`; it must, however, be updated to accept the new two-arg signature so the file still compiles. Add one new test:

```go
func TestWatch_CallsOnChangeWithErrorWhenLoadFails(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	type result struct {
		cfg *config.Config
		err error
	}
	changes := make(chan result, 1)
	stop, err := config.Watch(path, func(cfg *config.Config, loadErr error) {
		changes <- result{cfg, loadErr}
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	malformedYAML := "not: valid: yaml: at: all: [unterminated"
	if err := os.WriteFile(path, []byte(malformedYAML), 0644); err != nil {
		t.Fatalf("failed to write malformed config: %v", err)
	}

	select {
	case r := <-changes:
		if r.err == nil {
			t.Error("expected onChange to be called with a non-nil error for malformed YAML")
		}
		if r.cfg != nil {
			t.Errorf("expected cfg=nil when Load fails, got %+v", r.cfg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onChange to be called with the Load error")
	}
}
```

- [ ] **Step 2: Run tests to verify the new one fails**

Run: `cd services/core && go test ./config/... -race -run TestWatch_CallsOnChangeWithErrorWhenLoadFails -v`
Expected: FAIL — compile error first (`Watch`'s signature doesn't accept a 2-arg func yet), then once Step 3 lands, this specific test would fail because `onChange` is never called on a `Load` error today.

- [ ] **Step 3: Update the implementation**

In `services/core/config/watcher.go`, change the signature and the `onChange` call site:
```go
func Watch(path string, onChange func(*Config, error)) (stop func(), err error) {
```
and:
```go
			case <-timer.C:
				cfg, loadErr := Load(path)
				if loadErr != nil {
					log.Printf("error reloading config %s: %v", path, loadErr)
					onChange(nil, loadErr)
					continue
				}
				onChange(cfg, nil)
```

- [ ] **Step 4: Update `services/core/main.go`'s callback**

Change:
```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		if err := newCfg.Validate(); err != nil {
			log.Printf("ignoring invalid config reload: %v", err)
			return
		}
		rateLimiter.Reconfigure(...)
		concurrencyLimiter.Reconfigure(...)
		fleetShedder.Reconfigure(...)
	})
```
to:
```go
	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config, loadErr error) {
		if loadErr != nil {
			coremetrics.RecordConfigReload("failure")
			return
		}
		if err := newCfg.Validate(); err != nil {
			log.Printf("ignoring invalid config reload: %v", err)
			coremetrics.RecordConfigReload("failure")
			return
		}
		coremetrics.RecordConfigReload("success")
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode, newCfg.Tiers.ConcurrencyLimiter.QueueingEnabled, newCfg.Tiers.ConcurrencyLimiter.MaxBacklog, newCfg.Tiers.ConcurrencyLimiter.MaxQueueWaitMs, newCfg.Tiers.ConcurrencyLimiter.PollIntervalMs)
		fleetShedder.Reconfigure(newCfg.Tiers.FleetShedder.DefaultMaxConcurrent, newCfg.Tiers.FleetShedder.ReservedCriticalPct, newCfg.Tiers.FleetShedder.MaxRequestDurationMs, newCfg.Tiers.FleetShedder.ShadowMode)
	})
```
(`coremetrics` is the same import alias introduced in Task 3 — no new import needed here if Task 3 already landed first; this plan's task order assumes Task 3 lands before Task 6).

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd services/core && go test ./config/... -race -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add services/core/config/watcher.go services/core/config/watcher_test.go services/core/main.go
git commit -m "feat(core): add config-reload success/failure metric, surface Load errors to onChange"
```

---

### Task 7: Sidecar per-decision latency histogram

**Files:**
- Modify: `services/sidecar/metrics/metrics.go`, `services/sidecar/proxy/proxy.go`
- Test: Create `services/sidecar/metrics/metrics_test.go`

**Interfaces:**
- Produces: `metrics.RecordDecisionLatency(tier string, latency time.Duration)`. Consumed at the two `proxy.go` call sites that already compute `time.Since(start)` for `decisionlog.Log`'s `latency_ms` argument (the worker-shedder-reject branch and the main decision branch).

- [ ] **Step 1: Write the failing test**

```go
// services/sidecar/metrics/metrics_test.go
package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/sidecar/metrics"
)

func TestRecordDecisionLatency_ObservesByTier(t *testing.T) {
	before := testutil.CollectAndCount(metrics.DecisionLatency)
	metrics.RecordDecisionLatency("rate_limiter", 5*time.Millisecond)
	after := testutil.CollectAndCount(metrics.DecisionLatency)

	if after <= before {
		t.Errorf("expected DecisionLatency observation count to increase, before=%d after=%d", before, after)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/sidecar && go test ./metrics/... -race -v`
Expected: FAIL — `metrics.DecisionLatency`/`RecordDecisionLatency` undefined.

- [ ] **Step 3: Write the implementation**

Add to `services/sidecar/metrics/metrics.go`:
```go
var DecisionLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "ratecap_decision_latency_seconds",
	Help: "End-to-end latency of a /check decision as observed by the sidecar, labeled by the tier that produced the final action.",
}, []string{"tier"})

func RecordDecisionLatency(tier string, latency time.Duration) {
	DecisionLatency.WithLabelValues(tier).Observe(latency.Seconds())
}
```
Add `"time"` to this file's imports.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd services/sidecar && go test ./metrics/... -race -v`
Expected: PASS.

- [ ] **Step 5: Wire it into `proxy.go`**

In `services/sidecar/proxy/proxy.go`, both call sites that already call `decisionlog.Log(tier, key, action, priority, time.Since(start))` get one added line each, right after the existing `decisionlog.Log(...)` call:

Worker-shedder-reject branch (around the existing `decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))` — this appears twice, once in the hard-shed path and once in the shadow-mode path):
```go
				decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))
				metrics.RecordDecisionLatency("worker_shedder", time.Since(start))
```
(apply to both occurrences).

Main decision branch (after `decisionlog.Log(resp.Tier, key, actionLabel(realAction), priorityLabel(priority), time.Since(start))`):
```go
	decisionlog.Log(resp.Tier, key, actionLabel(realAction), priorityLabel(priority), time.Since(start))
	metrics.RecordDecisionLatency(resp.Tier, time.Since(start))
```

`metrics` is already imported in `proxy.go` (used for `metrics.RecordDecision`/`RecordShadowWouldReject`/`SetWorkerInFlight`), so no new import is needed.

- [ ] **Step 6: Run the sidecar test suite**

Run: `cd services/sidecar && go test ./... -race`
Expected: PASS (no existing test asserts on the absence of this metric; `proxy_test.go`'s existing tests should be unaffected since they don't inspect Prometheus state).

- [ ] **Step 7: Commit**

```bash
git add services/sidecar/metrics/metrics.go services/sidecar/metrics/metrics_test.go services/sidecar/proxy/proxy.go
git commit -m "feat(sidecar): add ratecap_decision_latency_seconds histogram"
```

---

### Task 8: `/release` and upstream gRPC-failure instrumentation

**Files:**
- Modify: `services/sidecar/metrics/metrics.go`, `services/sidecar/proxy/proxy.go`
- Test: Extend `services/sidecar/metrics/metrics_test.go`; check for and extend `services/sidecar/proxy/proxy_test.go` if it exists (run `ls services/sidecar/proxy/*_test.go` first)

**Interfaces:**
- Produces: `metrics.RecordReleaseResult(result string)` (`"success"`/`"failure"`), `metrics.RecordUpstreamError(endpoint string)`.

- [ ] **Step 1: Write the failing tests**

Append to `services/sidecar/metrics/metrics_test.go`:
```go
func TestRecordReleaseResult_IncrementsByResult(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReleaseTotal.WithLabelValues("success"))
	metrics.RecordReleaseResult("success")
	after := testutil.ToFloat64(metrics.ReleaseTotal.WithLabelValues("success"))

	if after != before+1 {
		t.Errorf("expected ReleaseTotal{result=success} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordUpstreamError_IncrementsByEndpoint(t *testing.T) {
	before := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("check_rate_limit"))
	metrics.RecordUpstreamError("check_rate_limit")
	after := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("check_rate_limit"))

	if after != before+1 {
		t.Errorf("expected UpstreamErrorsTotal{endpoint=check_rate_limit} to increment by 1, before=%v after=%v", before, after)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./metrics/... -race -run "RecordReleaseResult|RecordUpstreamError" -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Write the implementation**

Add to `services/sidecar/metrics/metrics.go`:
```go
var ReleaseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_release_total",
	Help: "Total number of /release calls handled by the sidecar, labeled by result (success or failure).",
}, []string{"result"})

var UpstreamErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_upstream_errors_total",
	Help: "Total number of failed gRPC calls from the sidecar to ratecap-core, labeled by the endpoint that made the call.",
}, []string{"endpoint"})

func RecordReleaseResult(result string) {
	ReleaseTotal.WithLabelValues(result).Inc()
}

func RecordUpstreamError(endpoint string) {
	UpstreamErrorsTotal.WithLabelValues(endpoint).Inc()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./metrics/... -race -v`
Expected: PASS.

- [ ] **Step 5: Wire into `proxy.go`**

`Handler.ServeHTTP`'s upstream-error branch:
```go
	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{...})
	if err != nil {
		log.Printf("sidecar: /check: upstream call failed: %v", err)
		metrics.RecordUpstreamError("check_rate_limit")
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}
```

`ReleaseHandler.ServeHTTP`, both branches:
```go
	_, err := h.client.ReleaseConcurrency(r.Context(), &ratecapv1.ReleaseConcurrencyRequest{Key: key, ConcurrencyToken: token})
	if err != nil {
		log.Printf("sidecar: /release: upstream call failed: %v", err)
		metrics.RecordUpstreamError("release_concurrency")
		metrics.RecordReleaseResult("failure")
		http.Error(w, "upstream release failed", http.StatusInternalServerError)
		return
	}

	metrics.RecordReleaseResult("success")
	w.WriteHeader(http.StatusOK)
```

- [ ] **Step 6: Run the sidecar test suite**

Run: `cd services/sidecar && go test ./... -race`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add services/sidecar/metrics/metrics.go services/sidecar/metrics/metrics_test.go services/sidecar/proxy/proxy.go
git commit -m "feat(sidecar): instrument /release and upstream gRPC failures"
```

---

### Task 9: Move `/metrics` and `/healthz` off the sidecar's self-throttled request path

**Files:**
- Modify: `services/sidecar/main.go`
- Test: `services/sidecar/main_test.go`

**Interfaces:**
- Produces: `newTopMux(protectedHandler http.Handler, limiter *ratelimit.Limiter, metricsHandler http.Handler, healthz http.HandlerFunc) *http.ServeMux` — an extracted, directly testable function replacing the current inline `mux`/`handler` construction, following this file's existing convention of extracting small pure/testable helpers (`resolveMaxInflight`, `resolveMaxRPS`) out of `main()`.

- [ ] **Step 1: Write the failing test**

```go
// services/sidecar/main_test.go — append
func TestNewTopMux_MetricsNeverThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now) // zero burst: every /check call is throttled
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz)
	server := httptest.NewServer(mux)
	defer server.Close()

	for i := 0; i < 5; i++ {
		resp, err := http.Get(server.URL + "/metrics")
		if err != nil {
			t.Fatalf("unexpected error calling /metrics: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("/metrics was throttled on call %d — it must bypass the request-path rate limiter", i)
		}
	}
}

func TestNewTopMux_CheckIsThrottled(t *testing.T) {
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	tinyLimiter := ratelimit.NewWithClock(0, 0, time.Now)
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	healthz := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	mux := newTopMux(protected, tinyLimiter, metricsHandler, healthz)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/check")
	if err != nil {
		t.Fatalf("unexpected error calling /check: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected /check to be throttled by a zero-burst limiter, got status %d", resp.StatusCode)
	}
}
```

Add to `main_test.go`'s imports: `"net/http"`, `"net/http/httptest"`, `"time"`, `"github.com/ratecap/sidecar/ratelimit"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./... -race -run TestNewTopMux -v`
Expected: FAIL — `newTopMux` undefined.

- [ ] **Step 3: Write the implementation**

In `services/sidecar/main.go`, replace:
```go
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", healthzHandler)

	maxRPS := resolveMaxRPS(os.Getenv("RATECAP_SIDECAR_MAX_RPS"), 1000)
	limiter := ratelimit.New(maxRPS)
	handler := ratelimit.Middleware(limiter, mux)
```
with:
```go
	protectedMux := http.NewServeMux()
	protectedMux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	protectedMux.Handle("/release", proxy.NewReleaseHandler(client))

	maxRPS := resolveMaxRPS(os.Getenv("RATECAP_SIDECAR_MAX_RPS"), 1000)
	limiter := ratelimit.New(maxRPS)
	handler := newTopMux(protectedMux, limiter, metrics.Handler(), healthzHandler)
```

Add the extracted function (near `healthzHandler`, before `main()`):
```go
// newTopMux keeps /metrics and /healthz off the same rate limiter that
// throttles real traffic on /check and /release — a Prometheus scrape must
// never compete with production requests for the same token bucket, exactly
// when an operator needs visibility most (e.g. during a real overload event
// this limiter is throttling).
func newTopMux(protected http.Handler, limiter *ratelimit.Limiter, metricsHandler http.Handler, healthz http.HandlerFunc) *http.ServeMux {
	throttled := ratelimit.Middleware(limiter, protected)

	mux := http.NewServeMux()
	mux.Handle("/check", throttled)
	mux.Handle("/release", throttled)
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/healthz", healthz)
	return mux
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./... -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/sidecar/main.go services/sidecar/main_test.go
git commit -m "fix(sidecar): move /metrics and /healthz off the self-throttled request path"
```

---

### Task 10: Sidecar `/healthz` reflects real core connectivity

**Files:**
- Modify: `services/sidecar/main.go`
- Test: `services/sidecar/healthz_main_test.go`

**Interfaces:**
- Produces: `newHealthzHandler(conn *grpc.ClientConn) http.HandlerFunc`, replacing the current unconditional-200 `healthzHandler`.

- [ ] **Step 1: Write the failing tests**

Replace the contents of `services/sidecar/healthz_main_test.go`:
```go
package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNewHealthzHandler_ReturnsOKWhenConnectionIsNotInTransientFailure(t *testing.T) {
	// A freshly constructed, never-dialed client starts in the Idle state
	// (grpc.NewClient never dials eagerly) — that must read as healthy, not
	// as a false-negative outage.
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}
	defer conn.Close()

	handler := newHealthzHandler(conn)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	handler(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 for an idle (never-used) connection, got %d", rec.Code)
	}
}

func TestNewHealthzHandler_Returns503WhenConnectionIsInTransientFailure(t *testing.T) {
	// Dialing a port nothing listens on and forcing a connection attempt
	// drives the connection into TransientFailure, which is the one state
	// this handler must treat as unhealthy.
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}
	defer conn.Close()
	conn.Connect()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn.GetState().String() == "TRANSIENT_FAILURE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	handler := newHealthzHandler(conn)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	handler(rec, req)

	if rec.Code != 503 {
		t.Errorf("expected 503 for a connection stuck in TRANSIENT_FAILURE, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./... -race -run TestNewHealthzHandler -v`
Expected: FAIL — `newHealthzHandler` undefined.

- [ ] **Step 3: Write the implementation**

In `services/sidecar/main.go`, replace:
```go
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
```
with:
```go
// newHealthzHandler treats every connectivity.State except TransientFailure
// and Shutdown as healthy — including Idle, since grpc.NewClient never
// dials eagerly, so a never-yet-used connection would otherwise read as a
// false-negative outage on a sidecar that just started and hasn't served a
// real request yet.
func newHealthzHandler(conn *grpc.ClientConn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := conn.GetState()
		if state == connectivity.TransientFailure || state == connectivity.Shutdown {
			http.Error(w, "core connection unhealthy: "+state.String(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
```
Add the import `"google.golang.org/grpc/connectivity"`.

In `main()`, change:
```go
	mux.HandleFunc("/healthz", healthzHandler)
```
(now inside the `newTopMux` call from Task 9) — update the call site to build the handler from `conn`:
```go
	handler := newTopMux(protectedMux, limiter, metrics.Handler(), newHealthzHandler(conn))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./... -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/sidecar/main.go services/sidecar/healthz_main_test.go
git commit -m "fix(sidecar): /healthz reflects real gRPC connectivity to core"
```

---

### Task 11: Core health server reflects real Redis connectivity

**Files:**
- Modify: `services/core/main.go`
- Test: Create `services/core/healthloop_test.go`

**Interfaces:**
- Produces: `runRedisHealthLoop(interval time.Duration, ping func(context.Context) error, setStatus func(healthpb.HealthCheckResponse_ServingStatus), stop <-chan struct{})` — extracted as a pure, directly testable function (fake `ping`/`setStatus`, no real Redis or gRPC server needed), matching the existing `resolveMaxInflight`-style extraction convention.

- [ ] **Step 1: Write the failing test**

```go
// services/core/healthloop_test.go
package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestRunRedisHealthLoop_SetsServingWhenPingSucceeds(t *testing.T) {
	var mu sync.Mutex
	var lastStatus healthpb.HealthCheckResponse_ServingStatus
	setStatus := func(s healthpb.HealthCheckResponse_ServingStatus) {
		mu.Lock()
		defer mu.Unlock()
		lastStatus = s
	}
	ping := func(ctx context.Context) error { return nil }
	stop := make(chan struct{})

	go runRedisHealthLoop(10*time.Millisecond, ping, setStatus, stop)
	time.Sleep(50 * time.Millisecond)
	close(stop)

	mu.Lock()
	defer mu.Unlock()
	if lastStatus != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("expected SERVING when ping succeeds, got %v", lastStatus)
	}
}

func TestRunRedisHealthLoop_SetsNotServingWhenPingFails(t *testing.T) {
	var mu sync.Mutex
	var lastStatus healthpb.HealthCheckResponse_ServingStatus
	setStatus := func(s healthpb.HealthCheckResponse_ServingStatus) {
		mu.Lock()
		defer mu.Unlock()
		lastStatus = s
	}
	ping := func(ctx context.Context) error { return errors.New("dial tcp: connection refused") }
	stop := make(chan struct{})

	go runRedisHealthLoop(10*time.Millisecond, ping, setStatus, stop)
	time.Sleep(50 * time.Millisecond)
	close(stop)

	mu.Lock()
	defer mu.Unlock()
	if lastStatus != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("expected NOT_SERVING when ping fails, got %v", lastStatus)
	}
}

func TestRunRedisHealthLoop_StopsWhenStopChannelClosed(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	ping := func(ctx context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	setStatus := func(healthpb.HealthCheckResponse_ServingStatus) {}
	stop := make(chan struct{})

	go runRedisHealthLoop(5*time.Millisecond, ping, setStatus, stop)
	time.Sleep(30 * time.Millisecond)
	close(stop)

	mu.Lock()
	callsAtStop := calls
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls != callsAtStop {
		t.Errorf("expected no further ping calls after stop was closed, had %d at stop, now %d", callsAtStop, calls)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/core && go test . -race -run TestRunRedisHealthLoop -v`
Expected: FAIL — `runRedisHealthLoop` undefined.

- [ ] **Step 3: Write the implementation**

Add to `services/core/main.go` (near the health-server block):
```go
// runRedisHealthLoop periodically pings Redis and reflects the result into
// the gRPC health service, so a probe actually detects a Redis outage
// instead of the SERVING status set once at startup and never touched again.
func runRedisHealthLoop(interval time.Duration, ping func(context.Context) error, setStatus func(healthpb.HealthCheckResponse_ServingStatus), stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			err := ping(ctx)
			cancel()
			if err != nil {
				setStatus(healthpb.HealthCheckResponse_NOT_SERVING)
				continue
			}
			setStatus(healthpb.HealthCheckResponse_SERVING)
		}
	}
}
```
Add `"context"` to the import block (if not already present via another change in this file).

In `main()`, after the existing `healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)` line and before `healthGRPCServer.Serve`, start the loop and make sure it's stopped on shutdown:
```go
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	stopHealthLoop := make(chan struct{})
	defer close(stopHealthLoop)
	go runRedisHealthLoop(5*time.Second, func(ctx context.Context) error {
		return redisClient.Ping(ctx).Err()
	}, func(s healthpb.HealthCheckResponse_ServingStatus) {
		healthServer.SetServingStatus("", s)
	}, stopHealthLoop)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/core && go test . -race -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Manual end-to-end verification**

```bash
cd deploy && bash generate-demo-certs.sh && docker compose up --build -d
sleep 3
docker compose exec core grpc_health_probe -addr=:9091 || grpcurl -plaintext localhost:9091 grpc.health.v1.Health/Check
docker compose stop redis
sleep 6
docker compose exec core grpc_health_probe -addr=:9091 || grpcurl -plaintext localhost:9091 grpc.health.v1.Health/Check
# expect SERVING before stopping redis, NOT_SERVING within ~5s after
docker compose down
```
If neither `grpc_health_probe` nor `grpcurl` is available locally, note this in the task report as a manually-unverified step and rely on the unit tests in Step 4 as the primary verification — do not skip writing this step down.

- [ ] **Step 6: Commit**

```bash
git add services/core/main.go services/core/healthloop_test.go
git commit -m "fix(core): health server reflects real Redis connectivity"
```

---

### Task 12: Grafana starter dashboard and alert rules

**Files:**
- Create: `deploy/grafana/ratecap-overview.json`, `deploy/grafana/ratecap-alerts.yml`

- [ ] **Step 1: Write the dashboard JSON**

```json
{
  "title": "RateCap Overview",
  "schemaVersion": 39,
  "timezone": "browser",
  "refresh": "10s",
  "panels": [
    {
      "id": 1,
      "title": "Decisions by tier and action",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 0 },
      "targets": [
        { "expr": "sum by (tier, action) (rate(ratecap_decisions_total[1m]))", "legendFormat": "{{tier}} / {{action}}" }
      ]
    },
    {
      "id": 2,
      "title": "Shadow-mode would-reject rate",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 0 },
      "targets": [
        { "expr": "sum by (tier) (rate(ratecap_shadow_would_reject_total[1m]))", "legendFormat": "{{tier}}" }
      ]
    },
    {
      "id": 3,
      "title": "Worker in-flight requests (Tier 4)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 8 },
      "targets": [
        { "expr": "ratecap_worker_inflight_requests", "legendFormat": "in-flight" }
      ]
    },
    {
      "id": 4,
      "title": "Fail-open events (Tier 1)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 8 },
      "targets": [
        { "expr": "sum by (tier, reason) (rate(ratecap_fail_open_total[1m]))", "legendFormat": "{{tier}} / {{reason}}" }
      ]
    },
    {
      "id": 5,
      "title": "Decision latency (p50/p99) by tier",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "targets": [
        { "expr": "histogram_quantile(0.5, sum by (le, tier) (rate(ratecap_decision_latency_seconds_bucket[5m])))", "legendFormat": "p50 {{tier}}" },
        { "expr": "histogram_quantile(0.99, sum by (le, tier) (rate(ratecap_decision_latency_seconds_bucket[5m])))", "legendFormat": "p99 {{tier}}" }
      ]
    },
    {
      "id": 6,
      "title": "Core gRPC request rate and errors",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 16 },
      "targets": [
        { "expr": "sum by (method, code) (rate(ratecap_core_grpc_requests_total[1m]))", "legendFormat": "{{method}} / {{code}}" }
      ]
    },
    {
      "id": 7,
      "title": "Redis call error rate (core)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 24 },
      "targets": [
        { "expr": "sum by (operation) (rate(ratecap_core_redis_errors_total[1m]))", "legendFormat": "{{operation}}" }
      ]
    },
    {
      "id": 8,
      "title": "Upstream (sidecar→core) error rate",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 24 },
      "targets": [
        { "expr": "sum by (endpoint) (rate(ratecap_upstream_errors_total[1m]))", "legendFormat": "{{endpoint}}" }
      ]
    }
  ]
}
```

- [ ] **Step 2: Write the alert rules**

```yaml
# deploy/grafana/ratecap-alerts.yml
groups:
  - name: ratecap
    rules:
      - alert: RateCapSustained503Rate
        expr: sum(rate(ratecap_decisions_total{action="reject_503"}[5m])) > 5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "RateCap is sustaining a nonzero 503 (load-shed) rate"
          description: "{{ $value }} rejections/sec over the last 5m — Tier 3 or Tier 4 is actively shedding load."

      - alert: RateCapWorkerNearCapacity
        expr: ratecap_worker_inflight_requests > (0.9 * on() group_left() (max(ratecap_worker_inflight_requests) or vector(500)))
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Tier 4 worker shedder in-flight count is near its configured cap"
          description: "in-flight={{ $value }} — the sidecar is close to shedding sheddable-priority traffic."

      - alert: RateCapFailOpenSustained
        expr: sum(rate(ratecap_fail_open_total[5m])) > 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "RateCap Tier 1 is failing open"
          description: "Tier 1's Redis-backed store is erroring and request-rate limiting is not being enforced. Investigate Redis/core connectivity immediately."
```

- [ ] **Step 3: Commit**

```bash
git add deploy/grafana/ratecap-overview.json deploy/grafana/ratecap-alerts.yml
git commit -m "docs(deploy): add starter Grafana dashboard and alert rules"
```

---

### Task 13: `ARCHITECTURE.md` Observability section

**Files:**
- Modify: `ARCHITECTURE.md` (append a new `## Observability` section; read the file first to match its existing heading style and pick the right insertion point — likely right before or after an existing "Known Limitations"-style section, if one exists)

- [ ] **Step 1: Read `ARCHITECTURE.md` to find the right insertion point**

Run: `grep -n "^## " ARCHITECTURE.md` to see the existing section list and pick where an Observability section fits (after any existing "Trust Boundary" / "Security" section, before or merged with any "Known Limitations" section).

- [ ] **Step 2: Write the section**

```markdown
## Observability

### Metrics

`ratecap-sidecar` exposes `/metrics` (Prometheus format) on its main listen address, on a path that bypasses the sidecar's own self-throttle limiter (see "Trust Boundary" — a Prometheus scrape must never compete with real traffic for the same rate-limit budget). `ratecap-core` exposes `/metrics` on a separate listener (`RATECAP_METRICS_ADDR`, default `:9092`), distinct from both its main gRPC port (`:9090`) and its plaintext health port (`:9091`).

| Metric | Emitted by | Labels | Meaning |
| --- | --- | --- | --- |
| `ratecap_decisions_total` | sidecar | `tier`, `action` | Every `/check` decision, by the tier that produced it and the resulting action (`allow`, `reject_429`, `reject_503`, `shadow_log`, `queue`). `queue` is emitted only when Tier 2's bounded queueing (`queueing_enabled`) is on and a request waits for a slot rather than being rejected immediately. |
| `ratecap_shadow_would_reject_total` | sidecar | `tier` | Decisions that would have rejected/shed but were coerced to `allow` by shadow mode. |
| `ratecap_worker_inflight_requests` | sidecar | — | Current Tier 4 (worker shedder) in-flight count on this sidecar instance. |
| `ratecap_decision_latency_seconds` | sidecar | `tier` | End-to-end `/check` latency histogram, labeled by the tier that produced the final decision. |
| `ratecap_release_total` | sidecar | `result` | `/release` call outcomes (`success`/`failure`). |
| `ratecap_upstream_errors_total` | sidecar | `endpoint` | Failed gRPC calls from sidecar to core (`check_rate_limit`, `release_concurrency`). |
| `ratecap_core_grpc_requests_total` | core | `method`, `code` | Every gRPC request core serves, by RPC name and resulting `google.golang.org/grpc/codes` status string. |
| `ratecap_core_grpc_request_duration_seconds` | core | `method` | gRPC request latency histogram. |
| `ratecap_core_redis_call_duration_seconds` | core | `operation` | Redis call latency histogram (`check_and_decrement`, `incr_concurrent`, `decr_concurrent`). |
| `ratecap_core_redis_errors_total` | core | `operation` | Failed Redis calls. |
| `ratecap_core_config_reload_total` | core | `result` | Config hot-reload attempts (`success`/`failure`) — a `failure` covers both a malformed YAML file and a well-formed-but-invalid config caught by `Config.Validate()`. |
| `ratecap_fail_open_total` | core | `tier`, `reason` | Requests allowed through via fail-open after a tier's store call errored. **Only Tier 1 (`rate_limiter`) currently fails open** — Tiers 2/3 fail closed by design; see "Redis-down degradation contract" below. |

A starter Grafana dashboard is at `deploy/grafana/ratecap-overview.json`; baseline alert rules are at `deploy/grafana/ratecap-alerts.yml`.

### Redis-down degradation contract

When a tier's backing Redis call errors (timeout, connection refused, or any other `StateStore` error):

- **Tier 1 (Request Rate Limiter) fails OPEN.** The request is allowed and `ratecap_fail_open_total{tier="rate_limiter",reason="store_error"}` is incremented. This matches Stripe's own documented precedent of failing open on request-rate limiting specifically.
- **Tier 2 (Concurrent Requests Limiter) and Tier 3 (Fleet Usage Load Shedder) fail CLOSED.** The gRPC call returns `codes.Internal`, which the sidecar surfaces as an HTTP 500 to the caller. Both tiers exist specifically to bound concurrent resource usage; failing open would remove that bound during exactly the outage when it matters most.
- This is a **per-tier** contract, not a whole-pipeline guarantee: `Pipeline.Check` still runs every tier in order, so a total Redis outage still surfaces as a 500 overall once the request reaches Tier 2 or Tier 3, even though Tier 1 itself failed open along the way.

### Health checks

- `ratecap-sidecar`'s `/healthz` reflects the real gRPC connectivity state to `ratecap-core` (healthy unless the connection is in `TRANSIENT_FAILURE` or `SHUTDOWN`).
- `ratecap-core`'s gRPC health service (`:9091`, plaintext, unauthenticated — see "Trust Boundary" for why this port has no mTLS) reflects real Redis connectivity, re-checked every 5 seconds via a background ping loop, rather than being set once at startup and never updated.

### Known limitations

- No distributed tracing exists yet. OpenTelemetry trace-context propagation across the sidecar→core gRPC hop is scoped for a future phase (see `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 1 item 9 / Phase 5).
```

- [ ] **Step 3: Commit**

```bash
git add ARCHITECTURE.md
git commit -m "docs: add Observability section to ARCHITECTURE.md"
```

---

### Task 14: Close the versioning gap and cut Phase 1's release

**Files:**
- Modify: `VERSION`, `CHANGELOG.md`

**Context:** Phase 0's roadmap versioning table calls for a `v2.4.1` patch release for its housekeeping work, but Phase 0's commits merged to `main` without ever bumping `VERSION` past `2.4.0` or adding a `[2.4.1]` `CHANGELOG.md` heading — the roadmap's own "ship every phase as its own tagged release" goal has one gap to close before Phase 1 adds another. This task closes both in one pass: a `[2.4.1]` heading for Phase 0's already-shipped work, then a `[2.5.0]` heading and `VERSION` bump for this phase.

- [ ] **Step 1: Add the `[2.4.1]` CHANGELOG entry for Phase 0**

In `CHANGELOG.md`, insert a new heading between the current `[Unreleased]` section and `[2.3.2]`:

```markdown
## [2.4.1] — 2026-08-27 — Phase 0 Housekeeping & Quick Wins

Patch release: Phase 0 of the v3 upgrade roadmap (see `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`) — housekeeping and quick wins, sequenced before any phase that needed a trustworthy version number to build on.

### Fixed

- Tier 2's bounded-queueing backlog counter is now Redis-backed (`store.IncrConcurrent`/`DecrConcurrent` against a `backlog:` key namespace) instead of a per-instance `atomic.Int64` — with N core replicas, the real ceiling was previously `maxBacklog × N`, not one shared ceiling.
- `bench_run.go`'s `--acquire` path no longer silently drops accepted/rejected/errored request outcomes into the same latency distribution; results are now bucketed separately, and every ticket's `Release()` is called even when the request itself was rejected.
- Dependency skew across `proto`/`services/core`/`services/sidecar` closed by merging the 4 open Dependabot PRs (grpc, go-redis, testcontainers, x/sys, x/text) in lockstep rather than per-module.

### Added

- `.github/dependabot.yml` covering all Go module directories plus `pip`, `github-actions`, and `docker` ecosystems, grouped, on a weekly schedule.
- `VERSION` as the single authoritative version source.
- Merged `fix/v3-config-validation` (Tier 1 `rate_limiter` config validation) and `fix/v3-breaking-wire-changes` (`PRIORITY_UNSPECIFIED` proto enum sentinel — a breaking wire-format renumbering, called out explicitly rather than shipped silently).
```

- [ ] **Step 2: Add the `[2.5.0]` CHANGELOG entry for Phase 1**

Insert a new heading above `[2.4.1]` (and move today's `[Unreleased]` content, if any accumulated during this phase's own CI/tooling changes, down into this heading or leave `[Unreleased]` empty — check `git diff` against `main` for anything Phase 1 added outside the items below before finalizing this list):

```markdown
## [2.5.0] — 2026-08-28 — Phase 1 Observability Foundation

Minor release: Phase 1 of the v3 upgrade roadmap — `services/core` gains self-instrumentation it previously had none of, the sidecar's `/metrics` no longer shares its self-throttle limiter with real traffic, and both services' health checks reflect real backing-service connectivity instead of static/startup-only state.

### Added

- `ratecap-core` `/metrics` endpoint (new `:9092` listener) — gRPC request count/latency by method and status, Redis call latency/error count, config-reload success/failure count.
- `ratecap_fail_open_total{tier,reason}` — Tier 1 (request-rate) now fails OPEN on a Redis/store error instead of surfacing an internal error, matching Stripe's documented precedent; Tiers 2/3 remain fail-closed by design (see `ARCHITECTURE.md`'s new Observability section for the full per-tier contract).
- `ratecap_decision_latency_seconds{tier}`, `ratecap_release_total{result}`, and `ratecap_upstream_errors_total{endpoint}` on the sidecar.
- Starter Grafana dashboard (`deploy/grafana/ratecap-overview.json`) and baseline alert rules (`deploy/grafana/ratecap-alerts.yml`).
- An Observability section in `ARCHITECTURE.md` documenting the full metrics contract, the per-tier Redis-down degradation contract, and current tracing limitations.

### Fixed

- Sidecar `/healthz` now reflects real gRPC connectivity to core instead of unconditionally returning 200.
- Core's gRPC health service now reflects real Redis connectivity (re-checked every 5s) instead of being set to `SERVING` once at startup and never updated again.
- `/metrics` and `/healthz` on the sidecar no longer share the process-wide self-throttle rate limiter with `/check`/`/release` — a Prometheus scrape can no longer be 429'd by the same limiter throttling real traffic.
```

- [ ] **Step 3: Bump `VERSION`**

```
2.5.0
```

- [ ] **Step 4: Verify no test asserts the old version string**

Run: `grep -rn "2\.4\.0\|2\.4\.1" --include="*.go" services/ cli/ packages/ 2>/dev/null`
Expected: no matches (nothing in test code hardcodes the version string) — if any match is found, inspect it before proceeding; do not assume it's safe to bump past.

- [ ] **Step 5: Commit**

```bash
git add VERSION CHANGELOG.md
git commit -m "chore: cut v2.4.1 (Phase 0 backfill) and v2.5.0 (Phase 1) in CHANGELOG, bump VERSION to 2.5.0"
```

---

## Post-Implementation (not a task — controller responsibility)

After all 14 tasks pass task review and the final whole-branch review is clean, per `finishing-a-development-branch`: push the branch (the user runs `git push` themselves due to the `DestructiveGuard` hook), open a PR into `develop` titled `feat: RateCap v3 roadmap Phase 1 — Observability Foundation`, and — once merged to `develop` and then promoted to `main` per this repo's established `develop`→`main` release flow — tag `v2.4.1` and `v2.5.0` at their respective commits and publish both GitHub Releases, mirroring exactly how Phase 0's own v2.4.0 tag/release was cut.
