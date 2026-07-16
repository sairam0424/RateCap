# RateCap v2 Phase 1: Foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Prometheus observability, structured per-decision logging, and optional mTLS on the sidecar-to-core gRPC hop to RateCap, plus fix three stale/inaccurate documentation sections.

**Architecture:** Tier attribution is added to the wire protocol first (a small, additive proto change), since both metrics and logging need to know which of the 4 tiers produced a given decision and `REJECT_429` is otherwise ambiguous between Tier 1 (rate limiter) and Tier 2 (concurrency limiter). Metrics and logging are then layered on top, instrumented at the two places in `services/sidecar/proxy/proxy.go` where a decision becomes visible to the sidecar: the early Tier-4 (worker shedder) short-circuit, and the later `switch action` block for Tiers 1-3. mTLS is wired additively alongside the existing shared-secret interceptor in both `main.go` files, gated by three new environment variables that default to today's plaintext behavior when unset.

**Tech Stack:** Go 1.26 (`services/core`, `services/sidecar`), `github.com/prometheus/client_golang` (new dependency), `log/slog` (Go standard library), `crypto/tls` + `crypto/x509` (Go standard library), `google.golang.org/grpc/credentials`.

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module: `services/core`, `services/sidecar`) before every commit that touches that module.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Exact commands and exact expected output are given in every step; run them verbatim.
- Docker must be confirmed reachable (`docker info > /dev/null 2>&1`) before any live docker-compose e2e step; if unreachable, report this explicitly rather than skipping silently.
- Per the approved design spec (`docs/superpowers/specs/2026-07-17-v2-phase-1-foundations-design.md`), the following are explicitly OUT OF SCOPE for this plan and must not appear in any task: OpenTelemetry SDK integration, a Grafana dashboard, a core-side `/metrics` endpoint, SPIFFE/SPIRE, cert rotation/hot-reload tooling, and tracked issue #24 (HTTP server timeouts on the sidecar).

## Design correction found during planning

The approved spec described `RATECAP_TLS_CA_PATH` as used by core "to verify client certs" and by the sidecar "to verify server cert" — but genuine **mutual** TLS requires both sides to present their own certificate, not just the server. The spec's env-var list omitted a client-side cert/key for the sidecar to present. This plan corrects that precisely: both `services/core` and `services/sidecar` read the *same three env var names* (`RATECAP_TLS_CERT_PATH`, `RATECAP_TLS_KEY_PATH`, `RATECAP_TLS_CA_PATH`), each pointing at its own service's certificate material (core's own server+client-auth cert; the sidecar's own client cert). This is consistent with the spec's stated test requirement ("an unauthenticated/wrong-cert client is rejected") and with the existing `RATECAP_SHARED_SECRET` pattern of using the same env var name symmetrically on both services — it is a precision fix, not a scope change.

---

### Task 1: Tier attribution on the wire

**Files:**
- Modify: `services/core/limiter/limiter.go`
- Modify: `services/core/limiter/tokenbucket.go`
- Modify: `services/core/limiter/concurrency.go`
- Modify: `services/core/limiter/fleetshedder.go`
- Modify (tests): `services/core/limiter/tokenbucket_test.go`, `services/core/limiter/concurrency_test.go`, `services/core/limiter/fleetshedder_test.go`
- Modify: `proto/ratecap/v1/ratecap.proto`
- Regenerate: `proto/ratecap/v1/ratecap.pb.go`, `proto/ratecap/v1/ratecap_grpc.pb.go`
- Modify: `services/core/grpcserver/server.go`
- Modify (tests): `services/core/grpcserver/server_test.go`

**Interfaces:**
- Consumes: nothing from later tasks.
- Produces: `limiter.Decision.Tier string` (new field — one of `"rate_limiter"`, `"concurrency_limiter"`, `"fleet_shedder"`, populated by each tier's own `Check()`). `ratecapv1.CheckRateLimitResponse.Tier string` (new proto field, field number 4). Task 2 and Task 3 both read `resp.Tier` from the sidecar's `CheckRateLimitResponse` to attribute Tiers 1-3 decisions.

- [ ] **Step 1: Write the failing tests**

Add to `services/core/limiter/tokenbucket_test.go`, after the existing `TestTokenBucketLimiter_AllowsExactlyBurstRequests` test:

```go
func TestTokenBucketLimiter_DecisionCarriesRateLimiterTier(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 10, 5, false)

	d, err := l.Check(context.Background(), limiter.Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, d.Tier)
	}
}
```

Add to `services/core/limiter/concurrency_test.go`, after its first test function:

```go
func TestConcurrencyLimiter_DecisionCarriesConcurrencyLimiterTier(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 10, 30000, false)

	d, err := l.Check(context.Background(), limiter.Request{Key: "user-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "concurrency_limiter" {
		t.Errorf(`expected Tier="concurrency_limiter", got %q`, d.Tier)
	}
}
```

Add to `services/core/limiter/fleetshedder_test.go`, after its first test function:

```go
func TestFleetShedder_DecisionCarriesFleetShedderTier(t *testing.T) {
	fs := newFakeFleetStore()
	l := limiter.NewFleetShedder(fs, 10, 20, 30000, false)

	d, err := l.Check(context.Background(), limiter.Request{Key: "user-1", Priority: limiter.Sheddable})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Tier != "fleet_shedder" {
		t.Errorf(`expected Tier="fleet_shedder", got %q`, d.Tier)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/core && go test ./limiter/... -run 'DecisionCarries' -v 2>&1 | tail -30`
Expected: FAIL — all three tests fail with `expected Tier="...", got ""` (the `Decision` struct has no `Tier` field yet, so this won't even compile until Step 3's struct change lands — confirm the compile error names `Tier` as an undefined field on `Decision`).

- [ ] **Step 3: Add `Tier` to `Decision` and populate it in each tier**

In `services/core/limiter/limiter.go`, replace:

```go
type Decision struct {
	Action       Action
	RetryAfterMs int64
	Reservations []TokenReservation
}
```

with:

```go
type Decision struct {
	Action       Action
	RetryAfterMs int64
	Reservations []TokenReservation
	Tier         string
}
```

In `services/core/limiter/tokenbucket.go`, replace the `Check` method's return statements:

```go
	if allowed {
		return Decision{Action: ALLOW}, nil
	}

	if shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs}, nil
```

with:

```go
	if allowed {
		return Decision{Action: ALLOW, Tier: "rate_limiter"}, nil
	}

	if shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs, Tier: "rate_limiter"}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs, Tier: "rate_limiter"}, nil
```

In `services/core/limiter/concurrency.go`, replace the `Check` method's return statements:

```go
	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: req.Key, Token: token}}}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, req.Key, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: req.Key, Token: reservedToken}}}, nil
	}

	return Decision{Action: REJECT_429}, nil
```

with:

```go
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
```

Note: the `if req.SkipReservations { return Decision{Action: ALLOW}, nil }` early-return at the top of `ConcurrencyLimiter.Check` stays unchanged (no `Tier` set) — a skipped tier produced no real decision, so it should not claim tier attribution. Same applies to `FleetShedder.Check` below.

In `services/core/limiter/fleetshedder.go`, replace the `Check` method's return statements:

```go
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
```

with:

```go
	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: fleetKey, Token: token}}, Tier: "fleet_shedder"}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, fleetKey, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: fleetKey, Token: reservedToken}}, Tier: "fleet_shedder"}, nil
	}

	return Decision{Action: REJECT_503, Tier: "fleet_shedder"}, nil
```

- [ ] **Step 4: Run the limiter tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -v 2>&1 | tail -60`
Expected: PASS — every test in the package, including all 3 new ones and every pre-existing one, reports `--- PASS`, final line `ok      github.com/ratecap/core/limiter`.

- [ ] **Step 5: Add the `tier` field to the proto contract and regenerate**

In `proto/ratecap/v1/ratecap.proto`, replace:

```protobuf
message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  repeated TokenReservation reservations = 3;
}
```

with:

```protobuf
message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  repeated TokenReservation reservations = 3;
  string tier = 4;
}
```

Run from the repo root (`RateCap/`, not this worktree's subdirectory — confirm your current directory with `pwd` first, then `cd` to the repo root if needed):

```bash
PATH="$(go env GOPATH)/bin:$PATH" protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
```

Expected: no output, exit code 0. Then run `git diff --stat proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go` — expect `ratecap_grpc.pb.go` to show zero diff (no RPC signature changed) and `ratecap.pb.go` to show a diff adding a `Tier` field and its getter to the `CheckRateLimitResponse` struct.

- [ ] **Step 6: Thread `Tier` through `grpcserver.Server.CheckRateLimit`**

Write the failing test first. Add to `services/core/grpcserver/server_test.go`, after `TestCheckRateLimit_ReturnsAllowDecision`:

```go
func TestCheckRateLimit_ReturnsTierFromDecision(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW, Tier: "rate_limiter"}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Tier != "rate_limiter" {
		t.Errorf(`expected Tier="rate_limiter", got %q`, resp.Tier)
	}
}
```

Run: `cd services/core && go test ./grpcserver/... -run TestCheckRateLimit_ReturnsTierFromDecision -v 2>&1 | tail -20`
Expected: FAIL — `resp.Tier` is always `""` since `server.go` never copies it from `decision.Tier`.

In `services/core/grpcserver/server.go`, replace:

```go
	return &ratecapv1.CheckRateLimitResponse{
		Action:       toProtoAction(decision.Action),
		RetryAfterMs: decision.RetryAfterMs,
		Reservations: reservations,
	}, nil
```

with:

```go
	return &ratecapv1.CheckRateLimitResponse{
		Action:       toProtoAction(decision.Action),
		RetryAfterMs: decision.RetryAfterMs,
		Reservations: reservations,
		Tier:         decision.Tier,
	}, nil
```

- [ ] **Step 7: Run all `services/core` tests to verify they pass**

Run: `cd services/core && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `config`, `grpcserver`, `limiter` (and `auth`); `store` needs Docker for its integration tests — if `docker info` fails, this is expected to report a failure only in `store`'s Docker-dependent tests, which is a pre-existing environmental gap unrelated to this task (neither this task nor `store` overlap).

- [ ] **Step 8: gofmt check and commit**

Run: `gofmt -l services/core/limiter/limiter.go services/core/limiter/tokenbucket.go services/core/limiter/concurrency.go services/core/limiter/fleetshedder.go services/core/limiter/tokenbucket_test.go services/core/limiter/concurrency_test.go services/core/limiter/fleetshedder_test.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go`
Expected: no output.

```bash
git add proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/limiter/limiter.go services/core/limiter/tokenbucket.go services/core/limiter/concurrency.go services/core/limiter/fleetshedder.go services/core/limiter/tokenbucket_test.go services/core/limiter/concurrency_test.go services/core/limiter/fleetshedder_test.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go
git commit -m "feat(core): attribute each Decision to the tier that produced it

REJECT_429 is otherwise ambiguous between the rate limiter and the
concurrency limiter — the sidecar cannot tell which tier shed a
request without this. Decision.Tier is populated by each tier's own
Check(), and CheckRateLimitResponse gains an additive tier field
(number 4) carrying it across the wire. A skipped tier (SkipReservations)
sets no Tier, since it produced no real decision."
```

---

### Task 2: Prometheus metrics

**Files:**
- Create: `services/sidecar/metrics/metrics.go`
- Create: `services/sidecar/metrics/metrics_test.go`
- Modify: `services/sidecar/proxy/proxy.go`
- Modify (tests): `services/sidecar/proxy/proxy_test.go`
- Modify: `services/sidecar/main.go`
- Modify: `services/sidecar/go.mod`, `services/sidecar/go.sum`

**Interfaces:**
- Consumes: `resp.Tier` (from Task 1, on `*ratecapv1.CheckRateLimitResponse`).
- Produces: `metrics.RecordDecision(tier, action string)`, `metrics.RecordShadowWouldReject(tier string)`, `metrics.Handler() http.Handler` — used by Task 3 (logging, same instrumentation points) and by `main.go`'s `/metrics` route registration.

- [ ] **Step 1: Add the `prometheus/client_golang` dependency**

Run: `cd services/sidecar && go get github.com/prometheus/client_golang@v1.23.2`
Expected: `go.mod` gains a new `require github.com/prometheus/client_golang v1.23.2` line; `go.sum` gains new entries. Run `go build ./...` afterward to confirm the module resolves cleanly (expected: no output).

- [ ] **Step 2: Write the failing tests**

Create `services/sidecar/metrics/metrics_test.go`:

```go
package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/sidecar/metrics"
)

func TestRecordDecision_IncrementsCounterForTierAndAction(t *testing.T) {
	metrics.RecordDecision("rate_limiter", "reject_429")

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("rate_limiter", "reject_429"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"rate_limiter\",action=\"reject_429\"} >= 1, got %v", got)
	}
}

func TestRecordShadowWouldReject_IncrementsCounterForTier(t *testing.T) {
	metrics.RecordShadowWouldReject("fleet_shedder")

	got := testutil.ToFloat64(metrics.ShadowWouldRejectTotal.WithLabelValues("fleet_shedder"))
	if got < 1 {
		t.Errorf("expected ratecap_shadow_would_reject_total{tier=\"fleet_shedder\"} >= 1, got %v", got)
	}
}

func TestHandler_ServesPrometheusExpositionFormat(t *testing.T) {
	metrics.RecordDecision("worker_shedder", "reject_503")

	req := newRequest(t)
	rec := newRecorder()
	metrics.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ratecap_decisions_total") {
		t.Errorf("expected response body to contain ratecap_decisions_total, got:\n%s", rec.Body.String())
	}
}
```

Add this helper file, `services/sidecar/metrics/testhelpers_test.go` (kept separate since it has no assertions of its own, only shared setup):

```go
package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/metrics", nil)
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd services/sidecar && go test ./metrics/... -v 2>&1 | tail -30`
Expected: FAIL to compile — the `metrics` package does not exist yet.

- [ ] **Step 4: Create the metrics package**

Create `services/sidecar/metrics/metrics.go`:

```go
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var DecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_decisions_total",
	Help: "Total number of rate-limit decisions, labeled by the tier that produced them and the resulting action.",
}, []string{"tier", "action"})

var ShadowWouldRejectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_shadow_would_reject_total",
	Help: "Total number of decisions that would have rejected/shed the request but were coerced to allow by shadow mode.",
}, []string{"tier"})

func RecordDecision(tier, action string) {
	DecisionsTotal.WithLabelValues(tier, action).Inc()
}

func RecordShadowWouldReject(tier string) {
	ShadowWouldRejectTotal.WithLabelValues(tier).Inc()
}

func Handler() http.Handler {
	return promhttp.Handler()
}
```

- [ ] **Step 5: Run the metrics tests to verify they pass**

Run: `cd services/sidecar && go test ./metrics/... -v 2>&1 | tail -30`
Expected: PASS — all 3 tests report `--- PASS`, final line `ok      github.com/ratecap/sidecar/metrics`.

- [ ] **Step 6: Write the failing proxy instrumentation tests**

Add to `services/sidecar/proxy/proxy_test.go`, after `TestServeHTTP_AllowReturns200`:

```go
func TestServeHTTP_RecordsDecisionMetricWithTierFromResponse(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("concurrency_limiter", "reject_429"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"concurrency_limiter\",action=\"reject_429\"} >= 1, got %v", got)
	}
}

func TestServeHTTP_RecordsPreCoercionDecisionUnderShadowMode(t *testing.T) {
	t.Setenv("RATECAP_SHADOW_MODE", "true")

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_503, Tier: "fleet_shedder"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (shadow-coerced), got %d", rec.Code)
	}

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("fleet_shedder", "reject_503"))
	if got < 1 {
		t.Errorf("expected the PRE-coercion action (reject_503) to be recorded despite the 200 response, got %v", got)
	}

	shadowGot := testutil.ToFloat64(metrics.ShadowWouldRejectTotal.WithLabelValues("fleet_shedder"))
	if shadowGot < 1 {
		t.Errorf("expected ratecap_shadow_would_reject_total{tier=\"fleet_shedder\"} >= 1, got %v", shadowGot)
	}
}

func TestServeHTTP_RecordsWorkerShedderMetricOnRealShed(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	shedder := worker.NewShedder(0)
	h := proxy.NewHandler(client, proxy.Sheddable, shedder)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues("worker_shedder", "reject_503"))
	if got < 1 {
		t.Errorf("expected ratecap_decisions_total{tier=\"worker_shedder\",action=\"reject_503\"} >= 1, got %v", got)
	}
}
```

Add `"github.com/prometheus/client_golang/prometheus/testutil"` and `"github.com/ratecap/sidecar/metrics"` to `proxy_test.go`'s import block.

- [ ] **Step 7: Run the tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_Records' -v 2>&1 | tail -40`
Expected: FAIL — all 3 assertions on the counters report values less than 1, since `proxy.go` never calls `metrics.RecordDecision`/`metrics.RecordShadowWouldReject` yet.

- [ ] **Step 8: Instrument `proxy.go` at both decision points**

Replace the worker-shedder block in `services/sidecar/proxy/proxy.go`:

```go
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
```

with:

```go
	if priority != Critical {
		if !h.shedder.Allow() {
			if !shadow.GlobalOverrideEnabled() {
				metrics.RecordDecision("worker_shedder", "reject_503")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			metrics.RecordDecision("worker_shedder", "reject_503")
			metrics.RecordShadowWouldReject("worker_shedder")
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			defer h.shedder.Release()
		}
	}
```

Replace the action-translation switch:

```go
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
```

with:

```go
	realAction := resp.Action
	action := realAction
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	metrics.RecordDecision(resp.Tier, actionLabel(realAction))
	if action != realAction {
		metrics.RecordShadowWouldReject(resp.Tier)
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
```

Add this helper function to `proxy.go`, below `ServeHTTP`:

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

Add `"github.com/ratecap/sidecar/metrics"` to `proxy.go`'s import block.

- [ ] **Step 9: Run the proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -80`
Expected: PASS — every test, including the 3 new ones and every pre-existing one, reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`.

- [ ] **Step 10: Mount `/metrics` on the sidecar's HTTP mux**

In `services/sidecar/main.go`, replace:

```go
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
```

with:

```go
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
	mux.Handle("/metrics", metrics.Handler())
```

Add `"github.com/ratecap/sidecar/metrics"` to `main.go`'s import block.

- [ ] **Step 11: Run the full `services/sidecar` build and test suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for every package (`auth`, `metrics`, `proxy`, `shadow`, `worker`; `sidecar` itself has no test files, expect `?` for that line).

- [ ] **Step 12: gofmt check and commit**

Run: `gofmt -l services/sidecar/metrics/metrics.go services/sidecar/metrics/metrics_test.go services/sidecar/metrics/testhelpers_test.go services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go services/sidecar/main.go`
Expected: no output.

```bash
git add services/sidecar/metrics/ services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go services/sidecar/main.go services/sidecar/go.mod services/sidecar/go.sum
git commit -m "feat(sidecar): expose Prometheus metrics for every tier's decisions

ratecap_decisions_total{tier,action} and ratecap_shadow_would_reject_total{tier},
mounted at /metrics. No per-key label — an unbounded label value is
exactly the cardinality-explosion pattern Prometheus's and Envoy's own
docs warn against, so per-key detail lives in structured logs instead.
The decisions counter records the pre-shadow-coercion action, so an
operator can see what a tier would actually enforce before turning it on."
```

---

### Task 3: Structured per-decision logging

**Files:**
- Create: `services/sidecar/decisionlog/decisionlog.go`
- Create: `services/sidecar/decisionlog/decisionlog_test.go`
- Modify: `services/sidecar/proxy/proxy.go`
- Modify (tests): `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: nothing from Task 2 (different concern, same instrumentation points, no shared code — `decisionlog` and `metrics` are independent packages).
- Produces: `decisionlog.Log(tier, key, action, priority string, latency time.Duration)`.

- [ ] **Step 1: Write the failing tests**

Create `services/sidecar/decisionlog/decisionlog_test.go`:

```go
package decisionlog_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ratecap/sidecar/decisionlog"
)

func TestLog_WritesJSONWithAllFields(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	decisionlog.Log("rate_limiter", "user-1", "reject_429", "sheddable", 12*time.Millisecond)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON, got error %v for output %q", err, buf.String())
	}

	for _, field := range []string{"time", "tier", "key", "action", "priority", "latency_ms"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("expected field %q in log entry, got %v", field, entry)
		}
	}
	if entry["tier"] != "rate_limiter" {
		t.Errorf(`expected tier="rate_limiter", got %v`, entry["tier"])
	}
	if entry["key"] != "user-1" {
		t.Errorf(`expected key="user-1", got %v`, entry["key"])
	}
	if entry["action"] != "reject_429" {
		t.Errorf(`expected action="reject_429", got %v`, entry["action"])
	}
	if entry["priority"] != "sheddable" {
		t.Errorf(`expected priority="sheddable", got %v`, entry["priority"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/sidecar && go test ./decisionlog/... -v 2>&1 | tail -20`
Expected: FAIL to compile — the `decisionlog` package does not exist yet.

- [ ] **Step 3: Create the decisionlog package**

Create `services/sidecar/decisionlog/decisionlog.go`:

```go
package decisionlog

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	logger  = slog.New(slog.NewJSONHandler(os.Stdout, nil))
)

// SetOutput redirects logging output for tests; passing nil restores stdout.
// Production code never calls this — main() and proxy.go only call Log().
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		w = os.Stdout
	}
	logger = slog.New(slog.NewJSONHandler(w, nil))
}

func Log(tier, key, action, priority string, latency time.Duration) {
	mu.Lock()
	l := logger
	mu.Unlock()
	l.Info("decision",
		"tier", tier,
		"key", key,
		"action", action,
		"priority", priority,
		"latency_ms", latency.Milliseconds(),
	)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/sidecar && go test ./decisionlog/... -v 2>&1 | tail -20`
Expected: PASS — `TestLog_WritesJSONWithAllFields` reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/decisionlog`.

- [ ] **Step 5: Write the failing proxy instrumentation tests**

Add to `services/sidecar/proxy/proxy_test.go`, after the metrics tests added in Task 2:

```go
func TestServeHTTP_LogsRealPathWorkerShedderDecision(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(0))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-42", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"tier":"worker_shedder"`) {
		t.Errorf("expected a worker_shedder log entry, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"key":"user-42"`) {
		t.Errorf("expected key=user-42 in the log entry, got:\n%s", buf.String())
	}
}

func TestServeHTTP_LogsRealPathTierDecisionFromResponse(t *testing.T) {
	var buf bytes.Buffer
	decisionlog.SetOutput(&buf)
	defer decisionlog.SetOutput(nil)

	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, Tier: "concurrency_limiter"}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(1000))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"tier":"concurrency_limiter"`) {
		t.Errorf("expected a concurrency_limiter log entry, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"action":"reject_429"`) {
		t.Errorf("expected action=reject_429 in the log entry, got:\n%s", buf.String())
	}
}
```

Add `"bytes"`, `"strings"`, and `"github.com/ratecap/sidecar/decisionlog"` to `proxy_test.go`'s import block.

- [ ] **Step 6: Run the tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_LogsRealPath' -v 2>&1 | tail -30`
Expected: FAIL — both assertions find no matching substring, since `proxy.go` never calls `decisionlog.Log` on the real path yet.

- [ ] **Step 7: Instrument `proxy.go` with structured logging**

Capture a start time and thread `key` through both instrumentation points. Replace the top of `ServeHTTP`:

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
```

with:

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
```

Replace the worker-shedder block (already modified in Task 2) with a version that also logs. The block after Task 2 reads:

```go
	if priority != Critical {
		if !h.shedder.Allow() {
			if !shadow.GlobalOverrideEnabled() {
				metrics.RecordDecision("worker_shedder", "reject_503")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			metrics.RecordDecision("worker_shedder", "reject_503")
			metrics.RecordShadowWouldReject("worker_shedder")
			log.Printf("worker shedder: would have shed request, shadow mode active")
		} else {
			defer h.shedder.Release()
		}
	}
```

Replace it with:

```go
	if priority != Critical {
		if !h.shedder.Allow() {
			shedKey := r.URL.Query().Get("key")
			if !shadow.GlobalOverrideEnabled() {
				metrics.RecordDecision("worker_shedder", "reject_503")
				decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			metrics.RecordDecision("worker_shedder", "reject_503")
			metrics.RecordShadowWouldReject("worker_shedder")
			decisionlog.Log("worker_shedder", shedKey, "reject_503", priorityLabel(priority), time.Since(start))
		} else {
			defer h.shedder.Release()
		}
	}
```

This reads `r.URL.Query().Get("key")` directly rather than moving the existing named `key` variable's declaration (which stays exactly where it is, after this block) — the query value is available on `r` at any point in the handler, so no reordering of the existing key-validation logic is needed, and the 400-on-missing-key behavior for non-shed requests is unchanged.

Replace the action-translation block (already modified in Task 2):

```go
	realAction := resp.Action
	action := realAction
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	metrics.RecordDecision(resp.Tier, actionLabel(realAction))
	if action != realAction {
		metrics.RecordShadowWouldReject(resp.Tier)
	}

	switch action {
```

with:

```go
	realAction := resp.Action
	action := realAction
	if shadow.GlobalOverrideEnabled() {
		action = shadow.CoerceIfShadowOverridden(action, true)
	}

	metrics.RecordDecision(resp.Tier, actionLabel(realAction))
	decisionlog.Log(resp.Tier, key, actionLabel(realAction), priorityLabel(priority), time.Since(start))
	if action != realAction {
		metrics.RecordShadowWouldReject(resp.Tier)
	}

	switch action {
```

Add this helper function to `proxy.go`, alongside `actionLabel`:

```go
func priorityLabel(p Priority) string {
	if p == Critical {
		return "critical"
	}
	return "sheddable"
}
```

Add `"time"` and `"github.com/ratecap/sidecar/decisionlog"` to `proxy.go`'s import block.

- [ ] **Step 8: Run the proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -80`
Expected: PASS — every test, including the 2 new ones, reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`.

- [ ] **Step 9: Run the full `services/sidecar` build and test suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for every package.

- [ ] **Step 10: gofmt check and commit**

Run: `gofmt -l services/sidecar/decisionlog/decisionlog.go services/sidecar/decisionlog/decisionlog_test.go services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go`
Expected: no output.

```bash
git add services/sidecar/decisionlog/ services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): log every decision as structured JSON, including real-path sheds

Extends the existing shadow-mode-only logging to the real (non-shadow)
path for every tier, not just the case that already had a log.Printf.
Closes issue #21 (Tier 4 shed path had zero logging) as a direct side
effect. log/slog, JSON to stdout — no new dependency."
```

---

### Task 4: mTLS on the sidecar-to-core gRPC hop

**Files:**
- Modify: `services/core/main.go`
- Modify: `services/sidecar/main.go`
- Create: `services/core/tlsconfig/tlsconfig.go`
- Create: `services/core/tlsconfig/tlsconfig_test.go`
- Create: `services/sidecar/tlsconfig/tlsconfig.go`
- Create: `services/sidecar/tlsconfig/tlsconfig_test.go`
- Create: `services/core/grpcserver/mtls_integration_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1-3 (independent concern — different files, no shared code).
- Produces: `tlsconfig.Load(certPath, keyPath, caPath string) (*tls.Config, error)` in both `services/core/tlsconfig` and `services/sidecar/tlsconfig` (same shape, deliberately duplicated rather than a shared module, since core needs `tls.RequireAndVerifyClientCert` and sidecar needs `RootCAs`, and these are different enough call sites that a shared abstraction would need its own flag to distinguish them — two small, obviously-correct 15-line files are simpler than one parameterized one). `tlsconfig.EnvVarsPartiallySet(cert, key, ca string) bool` in both packages, used for the fail-loud check.

- [ ] **Step 1: Write the failing tests for the fail-loud partial-config check**

Create `services/core/tlsconfig/tlsconfig_test.go`:

```go
package tlsconfig_test

import (
	"testing"

	"github.com/ratecap/core/tlsconfig"
)

func TestEnvVarsPartiallySet_AllEmptyIsNotPartial(t *testing.T) {
	if tlsconfig.EnvVarsPartiallySet("", "", "") {
		t.Error("expected all-empty to not be considered partial (TLS simply disabled)")
	}
}

func TestEnvVarsPartiallySet_AllSetIsNotPartial(t *testing.T) {
	if tlsconfig.EnvVarsPartiallySet("cert.pem", "key.pem", "ca.pem") {
		t.Error("expected all-set to not be considered partial (TLS fully configured)")
	}
}

func TestEnvVarsPartiallySet_OnlyCertSetIsPartial(t *testing.T) {
	if !tlsconfig.EnvVarsPartiallySet("cert.pem", "", "") {
		t.Error("expected cert-only to be considered partial")
	}
}

func TestEnvVarsPartiallySet_CertAndKeySetButNoCAIsPartial(t *testing.T) {
	if !tlsconfig.EnvVarsPartiallySet("cert.pem", "key.pem", "") {
		t.Error("expected cert+key without CA to be considered partial")
	}
}
```

Create the identical test file at `services/sidecar/tlsconfig/tlsconfig_test.go`, replacing only the import path (`"github.com/ratecap/sidecar/tlsconfig"`) and package-qualifying calls accordingly — same 4 test functions, same bodies.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/core && go test ./tlsconfig/... -v 2>&1 | tail -20`
Expected: FAIL to compile — the `tlsconfig` package does not exist yet in `services/core`.

Run: `cd services/sidecar && go test ./tlsconfig/... -v 2>&1 | tail -20`
Expected: FAIL to compile — same, for `services/sidecar`.

- [ ] **Step 3: Create both `tlsconfig` packages**

Create `services/core/tlsconfig/tlsconfig.go`:

```go
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func EnvVarsPartiallySet(cert, key, ca string) bool {
	set := 0
	if cert != "" {
		set++
	}
	if key != "" {
		set++
	}
	if ca != "" {
		set++
	}
	return set != 0 && set != 3
}

// Load builds a server-side, mutual-TLS *tls.Config: it presents this
// service's own certificate and requires+verifies the peer's certificate
// against the given CA, so an unauthenticated or wrong-cert client is
// rejected at the transport layer, on top of the existing shared-secret
// interceptor.
func Load(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading server cert/key: %w", err)
	}

	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", caPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}
```

Create `services/sidecar/tlsconfig/tlsconfig.go`:

```go
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func EnvVarsPartiallySet(cert, key, ca string) bool {
	set := 0
	if cert != "" {
		set++
	}
	if key != "" {
		set++
	}
	if ca != "" {
		set++
	}
	return set != 0 && set != 3
}

// Load builds a client-side, mutual-TLS *tls.Config: it presents this
// service's own client certificate (so the server can authenticate it)
// and verifies the server's certificate against the given CA.
func Load(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}

	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", caPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}, nil
}
```

- [ ] **Step 4: Run the tlsconfig tests to verify they pass**

Run: `cd services/core && go test ./tlsconfig/... -v 2>&1 | tail -20`
Expected: PASS — all 4 tests, `ok      github.com/ratecap/core/tlsconfig`.

Run: `cd services/sidecar && go test ./tlsconfig/... -v 2>&1 | tail -20`
Expected: PASS — all 4 tests, `ok      github.com/ratecap/sidecar/tlsconfig`.

- [ ] **Step 5: Wire optional TLS into `services/core/main.go`**

Replace:

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-core refuses to start without gRPC authentication configured")
	}
```

with:

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-core refuses to start without gRPC authentication configured")
	}

	tlsCertPath := os.Getenv("RATECAP_TLS_CERT_PATH")
	tlsKeyPath := os.Getenv("RATECAP_TLS_KEY_PATH")
	tlsCAPath := os.Getenv("RATECAP_TLS_CA_PATH")
	if tlsconfig.EnvVarsPartiallySet(tlsCertPath, tlsKeyPath, tlsCAPath) {
		log.Fatalf("RATECAP_TLS_CERT_PATH, RATECAP_TLS_KEY_PATH, and RATECAP_TLS_CA_PATH must be set together or not at all — got cert=%q key=%q ca=%q", tlsCertPath, tlsKeyPath, tlsCAPath)
	}
```

Replace:

```go
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret)))
```

with:

```go
	serverOpts := []grpc.ServerOption{grpc.UnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret))}
	if tlsCertPath != "" {
		tlsConf, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		log.Printf("ratecap-core: mTLS enabled")
	}
	grpcServer := grpc.NewServer(serverOpts...)
```

Add `"google.golang.org/grpc/credentials"` and `"github.com/ratecap/core/tlsconfig"` to `main.go`'s import block.

- [ ] **Step 6: Wire optional TLS into `services/sidecar/main.go`**

Replace:

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-sidecar refuses to start without gRPC authentication configured")
	}

	conn, err := grpc.NewClient(
		coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret)),
	)
	if err != nil {
		log.Fatalf("failed to connect to ratecap-core at %s: %v", coreAddr, err)
	}
```

with:

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-sidecar refuses to start without gRPC authentication configured")
	}

	tlsCertPath := os.Getenv("RATECAP_TLS_CERT_PATH")
	tlsKeyPath := os.Getenv("RATECAP_TLS_KEY_PATH")
	tlsCAPath := os.Getenv("RATECAP_TLS_CA_PATH")
	if tlsconfig.EnvVarsPartiallySet(tlsCertPath, tlsKeyPath, tlsCAPath) {
		log.Fatalf("RATECAP_TLS_CERT_PATH, RATECAP_TLS_KEY_PATH, and RATECAP_TLS_CA_PATH must be set together or not at all — got cert=%q key=%q ca=%q", tlsCertPath, tlsKeyPath, tlsCAPath)
	}

	transportCreds := insecure.NewCredentials()
	if tlsCertPath != "" {
		tlsConf, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		transportCreds = credentials.NewTLS(tlsConf)
		log.Printf("ratecap-sidecar: mTLS enabled")
	}

	conn, err := grpc.NewClient(
		coreAddr,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret)),
	)
	if err != nil {
		log.Fatalf("failed to connect to ratecap-core at %s: %v", coreAddr, err)
	}
```

Add `"google.golang.org/grpc/credentials"` and `"github.com/ratecap/sidecar/tlsconfig"` to `main.go`'s import block.

- [ ] **Step 7: Run both modules' build and test suites**

Run: `cd services/core && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `config`, `grpcserver`, `limiter`, `tlsconfig`, `auth`.

Run: `cd services/sidecar && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for every package.

- [ ] **Step 8: Write the mTLS integration test proving both the enabled and disabled paths**

This test mirrors `services/core/grpcserver/auth_integration_test.go`'s bufconn pattern, adding a real TLS handshake instead of the existing plaintext bufconn dial. Generate throwaway test certs at test-run time using Go's standard library (no shelling out to `openssl`, no fixture files to maintain).

Create `services/core/grpcserver/mtls_integration_test.go`:

```go
package grpcserver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("creating leaf cert for %s: %v", commonName, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling leaf key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("building tls.Certificate for %s: %v", commonName, err)
	}
	return cert
}

func startTLSTestServer(t *testing.T, ca *testCA, serverCert tls.Certificate) (net.Listener, func()) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	lis := bufconn.Listen(1024 * 1024)
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConf)))
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}))

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	return lis, grpcServer.Stop
}

func TestMTLS_RejectsClientWithNoCertificate(t *testing.T) {
	ca := newTestCA(t)
	serverCert := ca.issue(t, "ratecap-core")
	lis, stop := startTLSTestServer(t, ca, serverCert)
	defer stop()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.(*bufconn.Listener).DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool})),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	_, err = client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err == nil {
		t.Fatal("expected an error when the client presents no certificate, since the server requires one")
	}
}

func TestMTLS_AllowsClientWithValidCertificate(t *testing.T) {
	ca := newTestCA(t)
	serverCert := ca.issue(t, "ratecap-core")
	clientCert := ca.issue(t, "ratecap-sidecar")
	lis, stop := startTLSTestServer(t, ca, serverCert)
	defer stop()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.(*bufconn.Listener).DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: pool})),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	resp, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error with a valid client certificate: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}

func TestMTLS_PlaintextPathUnaffectedWhenTLSNotConfigured(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}))
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	resp, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error on the plaintext path (TLS not configured): %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}
```

Note: `bufconn.Listen` returns a `*bufconn.Listener`, so the type assertion `lis.(*bufconn.Listener)` in the two mTLS tests is safe since `startTLSTestServer` always constructs it that way — this mirrors the existing `auth_integration_test.go`'s `startTestServer` pattern, adapted to also return the listener (needed here for the dialer) rather than a client directly, since two different client configurations (no-cert vs valid-cert) must be tested against the same server instance.

- [ ] **Step 9: Run the test to verify it fails for the right reason, then passes**

Run: `cd services/core && go test ./grpcserver/... -run TestMTLS -v 2>&1 | tail -40`
Expected on first run (before this step exists code-wise, it can't fail-then-pass since Step 5 already wired the real `main.go`, not a to-be-implemented API) — this test exercises `grpc.Creds`/`tls.Config` directly, which already exist in the standard library and `google.golang.org/grpc`, so this test is valid from the moment it's written. Confirm all 3 tests report `--- PASS`, final line `ok      github.com/ratecap/core/grpcserver`. If `TestMTLS_RejectsClientWithNoCertificate` does not fail before any implementation and pass after — investigate: this specific test's correctness depends only on `tls.RequireAndVerifyClientCert` behavior, which is already correct as written, so there is no separate red/green cycle for this step; running once and confirming PASS is sufficient, unlike the TDD steps elsewhere in this task.

- [ ] **Step 10: Run the full `services/core` test suite**

Run: `cd services/core && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for `config`, `grpcserver`, `limiter`, `tlsconfig`, `auth` (`store` needs Docker, same pre-existing caveat as Task 1).

- [ ] **Step 11: gofmt check and commit**

Run: `gofmt -l services/core/main.go services/sidecar/main.go services/core/tlsconfig/tlsconfig.go services/core/tlsconfig/tlsconfig_test.go services/sidecar/tlsconfig/tlsconfig.go services/sidecar/tlsconfig/tlsconfig_test.go services/core/grpcserver/mtls_integration_test.go`
Expected: no output.

```bash
git add services/core/main.go services/sidecar/main.go services/core/tlsconfig/ services/sidecar/tlsconfig/ services/core/grpcserver/mtls_integration_test.go
git commit -m "feat: add optional mutual TLS on the sidecar-to-core gRPC hop

RATECAP_TLS_CERT_PATH/KEY_PATH/CA_PATH, same three env var names on
both services, each pointing at its own certificate material. Additive
alongside the existing shared-secret interceptor, not a replacement —
one is transport-layer mutual auth+encryption, the other is app-layer
auth-in-depth. Off by default: unset env vars mean today's plaintext
behavior, so no existing v1.0.1 deployment breaks on upgrade. Fails
loud (not silent-partial) if only some of the 3 vars are set on a
given service."
```

---

### Task 5: Demo self-signed certs for docker-compose

**Files:**
- Create: `deploy/generate-demo-certs.sh`
- Modify: `deploy/docker-compose.yml`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: nothing from earlier tasks except the env var names from Task 4 (`RATECAP_TLS_CERT_PATH`, `RATECAP_TLS_KEY_PATH`, `RATECAP_TLS_CA_PATH`).
- Produces: `deploy/certs/` (gitignored, generated at demo-setup time, not committed) containing `ca.pem`, `core-cert.pem`, `core-key.pem`, `sidecar-cert.pem`, `sidecar-key.pem`.

- [ ] **Step 1: Write the cert-generation script**

Create `deploy/generate-demo-certs.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

# Demo-only certs for RateCap's docker-compose stack. Do not use in
# production — see SECURITY.md for the real deployment guidance
# (operator-provided certs via RATECAP_TLS_CERT_PATH/KEY_PATH/CA_PATH).

cd "$(dirname "$0")"
mkdir -p certs
cd certs

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca-key.pem -out ca.pem -days 1 -nodes \
  -subj "/CN=ratecap-demo-ca"

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout core-key.pem -out core.csr -nodes \
  -subj "/CN=ratecap-core"
openssl x509 -req -in core.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out core-cert.pem -days 1

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout sidecar-key.pem -out sidecar.csr -nodes \
  -subj "/CN=ratecap-sidecar"
openssl x509 -req -in sidecar.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out sidecar-cert.pem -days 1

rm -f core.csr sidecar.csr ca.srl
echo "Demo certs generated in deploy/certs/ (gitignored, 1-day validity, do not use in production)."
```

Run: `chmod +x deploy/generate-demo-certs.sh`

- [ ] **Step 2: Add `deploy/certs/` to `.gitignore`**

In `.gitignore`, add a new line after the existing entries:

```
/deploy/certs/
```

- [ ] **Step 3: Verify the script actually generates usable certs**

Run:

```bash
./deploy/generate-demo-certs.sh
ls -la deploy/certs/
openssl verify -CAfile deploy/certs/ca.pem deploy/certs/core-cert.pem
openssl verify -CAfile deploy/certs/ca.pem deploy/certs/sidecar-cert.pem
```

Expected: `deploy/certs/` contains `ca.pem`, `ca-key.pem`, `core-cert.pem`, `core-key.pem`, `sidecar-cert.pem`, `sidecar-key.pem`; both `openssl verify` commands print `...: OK`.

- [ ] **Step 4: Wire the certs into `docker-compose.yml`**

In `deploy/docker-compose.yml`, replace the `core` service's `environment`/`volumes` blocks:

```yaml
  core:
    build:
      context: ..
      dockerfile: services/core/Dockerfile
    environment:
      RATECAP_CONFIG_PATH: /etc/ratecap/ratecap.yaml
      RATECAP_REDIS_ADDR: redis:6379
      RATECAP_GRPC_ADDR: :9090
      # Demo-only value — real deployments must inject this via proper secrets
      # management (e.g. a mounted secret file or orchestrator-native secret),
      # never a value committed to a compose file.
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
    volumes:
      - ./ratecap.yaml:/etc/ratecap/ratecap.yaml
    depends_on:
      - redis
    ports:
      - "9090:9090"
```

with:

```yaml
  core:
    build:
      context: ..
      dockerfile: services/core/Dockerfile
    environment:
      RATECAP_CONFIG_PATH: /etc/ratecap/ratecap.yaml
      RATECAP_REDIS_ADDR: redis:6379
      RATECAP_GRPC_ADDR: :9090
      # Demo-only value — real deployments must inject this via proper secrets
      # management (e.g. a mounted secret file or orchestrator-native secret),
      # never a value committed to a compose file.
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
      # Demo-only, self-signed, 1-day-validity certs generated by
      # deploy/generate-demo-certs.sh — real deployments bring their own.
      RATECAP_TLS_CERT_PATH: /etc/ratecap/certs/core-cert.pem
      RATECAP_TLS_KEY_PATH: /etc/ratecap/certs/core-key.pem
      RATECAP_TLS_CA_PATH: /etc/ratecap/certs/ca.pem
    volumes:
      - ./ratecap.yaml:/etc/ratecap/ratecap.yaml
      - ./certs:/etc/ratecap/certs:ro
    depends_on:
      - redis
    ports:
      - "9090:9090"
```

Replace the `sidecar` service's `environment` block:

```yaml
  sidecar:
    build:
      context: ..
      dockerfile: services/sidecar/Dockerfile
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
      RATECAP_MAX_INFLIGHT_REQUESTS: "3"
    depends_on:
      - core
    ports:
      - "8080:8080"
```

with:

```yaml
  sidecar:
    build:
      context: ..
      dockerfile: services/sidecar/Dockerfile
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
      RATECAP_MAX_INFLIGHT_REQUESTS: "3"
      RATECAP_TLS_CERT_PATH: /etc/ratecap/certs/sidecar-cert.pem
      RATECAP_TLS_KEY_PATH: /etc/ratecap/certs/sidecar-key.pem
      RATECAP_TLS_CA_PATH: /etc/ratecap/certs/ca.pem
    volumes:
      - ./certs:/etc/ratecap/certs:ro
    depends_on:
      - core
    ports:
      - "8080:8080"
```

- [ ] **Step 5: Commit**

Run: `gofmt -l .gitignore 2>&1 || true` (gofmt doesn't apply to non-Go files — this step is a no-op sanity check, expected to report nothing relevant).

```bash
git add deploy/generate-demo-certs.sh deploy/docker-compose.yml .gitignore
git commit -m "feat(deploy): generate self-signed demo certs, enable mTLS in the demo stack

deploy/generate-demo-certs.sh produces a throwaway CA plus a core and
a sidecar leaf cert, 1-day validity, gitignored — mirrors the existing
demo-only shared-secret literal's own explicit disclaimer. Real
deployments bring their own certs via the same three env vars."
```

---

### Task 6: Live end-to-end verification

**Files:** none modified — this task only runs and observes the demo stack.

**Interfaces:**
- Consumes: everything from Tasks 1-5.
- Produces: nothing — a verification report appended to this task's own execution log.

- [ ] **Step 1: Confirm Docker is reachable**

Run: `docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"`

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 2: Generate demo certs and bring up the full stack**

Run from `deploy/`:

```bash
cd deploy
./generate-demo-certs.sh
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`.

- [ ] **Step 3: Confirm mTLS is genuinely active, not silently skipped**

Run: `docker compose logs core sidecar 2>&1 | grep -i "mtls enabled"`
Expected: two matching lines, one from `core` and one from `sidecar` — both containing "mTLS enabled" from the `log.Printf` calls added in Task 4. If either is missing, the TLS env vars did not take effect — investigate before continuing (do not proceed to claim mTLS was verified if this check fails).

- [ ] **Step 4: Regression-check all 4 tiers still behave identically to pre-Phase-1**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```
Expected: exactly 5x `checkout 200` then 2x `checkout 429`.

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "fleet-demo %{http_code}\n" "http://localhost:3000/fleet-demo?priority=sheddable" & done
wait
```
Expected: exactly 3x `fleet-demo 200` and 2x `fleet-demo 503`.

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "worker-demo %{http_code}\n" http://localhost:3000/worker-demo & done
wait
```
Expected: exactly 3x `worker-demo 200` and 2x `worker-demo 503`.

- [ ] **Step 5: Confirm `/metrics` reflects the regression traffic above**

Run: `curl -s http://localhost:8080/metrics | grep ratecap_`
Expected: output includes `ratecap_decisions_total` lines for `tier="rate_limiter"`, `tier="fleet_shedder"`, and `tier="worker_shedder"` labels with nonzero counts, matching the actions driven in Step 4 (e.g. `ratecap_decisions_total{action="reject_429",tier="rate_limiter"} 2` from the 2 checkout 429s). Note: `tier="concurrency_limiter"` will not appear unless `/slow-report` was also exercised — that's expected, not a bug, since Step 4 didn't drive that endpoint.

- [ ] **Step 6: Confirm structured decision logs are being emitted on the real path**

Run: `docker compose logs sidecar 2>&1 | grep '"tier"' | tail -5`
Expected: JSON lines containing `"tier"`, `"key"`, `"action"`, `"priority"`, `"latency_ms"` fields, corresponding to the traffic driven in Step 4 — confirming real-path logging works end-to-end, not just in unit tests.

- [ ] **Step 7: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 8: Run every module's full test suite one final time**

Run:

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
```

Expected: `ok` for every package (`services/core/store` needs Docker, which is reachable per Step 1, so it runs live).

No commit for this task — it verifies, it doesn't change code.

---

### Task 7: Documentation hygiene

**Files:**
- Modify: `ARCHITECTURE.md`
- Modify: `SECURITY.md`

**Interfaces:**
- Consumes: nothing (pure documentation, no code dependency).
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Fix `ARCHITECTURE.md`'s stale tier-completeness line**

Replace:

```
RateCap faithfully recreates [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters) as a hybrid core-engine + sidecar system. v1 implements Tier 1 (the Request Rate Limiter) end-to-end; Tiers 2–4 are planned next.
```

with:

```
RateCap faithfully recreates [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters) as a hybrid core-engine + sidecar system. v1.0.0 implements all four tiers end-to-end; this document is updated as v2 work lands.
```

Also update the now-stale sentence in the "Swappable interfaces" section:

```
- `Limiter` is implemented today only by `TokenBucketLimiter`. Tiers 2–4 (concurrent-requests limiter, fleet-usage shedder, worker-utilization shedder) will each be a new `Limiter` implementation composed into a pipeline in `ratecap-core`, reusing the same gRPC/config/observability scaffolding already proven by Tier 1.
```

with:

```
- `Limiter` is implemented by `TokenBucketLimiter` (Tier 1), `ConcurrencyLimiter` (Tier 2), and `FleetShedder` (Tier 3), each composed into a pipeline in `ratecap-core`. Tier 4 (the worker-utilization shedder) is deliberately sidecar-local, not a `Limiter` — see `services/sidecar/worker/shedder.go`.
```

And the "Priority resolution" section's now-inaccurate framing:

```
Tier 1 does not use request priority — only Tier 3 (the fleet-usage shedder, not yet built) will. The resolution mechanism is nonetheless built and tested now (`services/sidecar/proxy/priority.go`), in this fallback order:
```

with:

```
Tier 1 does not use request priority — only Tier 3 (the fleet-usage shedder) does. The resolution mechanism lives in `services/sidecar/proxy/priority.go`, in this fallback order:
```

- [ ] **Step 2: Fix `SECURITY.md`'s stale "Supported Versions" section**

Replace:

```
RateCap is currently in v1 development (Tier 1 walking skeleton). Until a tagged v1.0.0 release exists, only the `main` branch receives security fixes.

| Version | Supported |
| ------- | --------- |
| main (pre-release) | ✅ |
```

with:

```
RateCap follows semantic versioning. The latest tagged release and the `main` branch receive security fixes.

| Version | Supported |
| ------- | --------- |
| v1.0.x  | ✅ |
| main    | ✅ |
| < v1.0.0 | ❌ |
```

- [ ] **Step 3: Update `SECURITY.md`'s "Network Transport Security" section for the new, optional-mTLS threat model**

Replace the entire section (heading through the closing paragraph):

```
## Network Transport Security (v1)

`ratecap-core` and `ratecap-sidecar` communicate over plaintext gRPC, authenticated by a shared secret (`RATECAP_SHARED_SECRET`) rather than TLS/mTLS. This is v1's explicit, intentional posture:

- The shared secret proves a caller is a legitimate RateCap component; it does **not** encrypt traffic or protect against a network-level eavesdropper or man-in-the-middle.
- **`ratecap-core` and `ratecap-sidecar` must run on a private, trusted network only** — e.g. a Docker Compose network, a Kubernetes cluster-internal `ClusterIP`, or an equivalent isolated segment. Never expose `ratecap-core`'s gRPC port to an untrusted network.
- Both services fail closed: if `RATECAP_SHARED_SECRET` is unset, neither service starts. There is no supported configuration where gRPC auth is silently disabled.
- TLS/mTLS for this hop is deferred to v2.

If your deployment cannot guarantee a private network between `ratecap-core` and `ratecap-sidecar`, do not run RateCap v1 in that environment — wait for v2's TLS support, or open an issue describing your constraint.
```

with:

```
## Network Transport Security

`ratecap-core` and `ratecap-sidecar` are always authenticated by a shared secret (`RATECAP_SHARED_SECRET`); both services fail closed if it is unset. Transport encryption is separate and optional:

- **Without TLS configured** (the default): communication is plaintext, authenticated only by the shared secret. This does **not** encrypt traffic or protect against a network-level eavesdropper or man-in-the-middle. **`ratecap-core` and `ratecap-sidecar` must run on a private, trusted network only** — e.g. a Docker Compose network, a Kubernetes cluster-internal `ClusterIP`, or an equivalent isolated segment. Never expose `ratecap-core`'s gRPC port to an untrusted network.
- **With TLS configured** (`RATECAP_TLS_CERT_PATH`, `RATECAP_TLS_KEY_PATH`, `RATECAP_TLS_CA_PATH` set on both services): the hop is encrypted, and both sides present and verify certificates via mutual TLS — the sidecar cannot connect to an impostor core, and core rejects any client that doesn't present a certificate signed by the configured CA. This is layered on top of, not a replacement for, the shared-secret check.
- mTLS is optional and off by default specifically so upgrading an existing deployment never silently breaks it. It is recommended, not required, for v2. If your deployment cannot guarantee a private network and cannot yet configure certificates, treat this as an open risk and prioritize enabling mTLS.
- Certificate provisioning is the operator's responsibility — RateCap does not issue, rotate, or manage certificates. See `deploy/generate-demo-certs.sh` for how the docker-compose demo generates throwaway, 1-day-validity certs; do not reuse that script's output anywhere but the demo.
```

- [ ] **Step 4: Commit**

```bash
git add ARCHITECTURE.md SECURITY.md
git commit -m "docs: fix stale tier-completeness claims, document optional mTLS

ARCHITECTURE.md still said tiers 2-4 were planned next, despite
v1.0.0 shipping all four over a month ago. SECURITY.md's Supported
Versions table still said pre-release/main-only despite tagged
releases existing, and its Network Transport Security section
needs to describe the new optional-mTLS threat model this phase
ships, not just the plaintext-only one it replaces."
```

---

## Post-plan note

This closes the observability and transport-security gaps from RateCap's original v1 design spec, plus fixes 3 stale documentation sections, per the approved `docs/superpowers/specs/2026-07-17-v2-phase-1-foundations-design.md`. It also closes tracked issue #21 as a side effect of Task 3's logging work. Explicitly out of scope, per the spec: OpenTelemetry SDK integration, a Grafana dashboard, a core-side `/metrics` endpoint, SPIFFE/SPIRE, cert rotation/hot-reload tooling, and tracked issue #24 (HTTP server timeouts). Once this plan's 7 tasks are reviewed and merged, this branch is ready for a PR into `develop`, and the v2 roadmap moves to Phase 2 (`ratecapctl` CLI + Python SDK).
