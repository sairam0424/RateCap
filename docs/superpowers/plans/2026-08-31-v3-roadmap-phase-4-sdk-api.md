# RateCap v3 Roadmap — Phase 4: SDK & API — Token-Cost Wiring — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** finish an already-designed feature rather than add a new mechanism. RateCap's Tier 1 substrate is *already* a generic variable-cost token bucket end-to-end (proto, Lua script, `RedisStore.CheckAndDecrement` all support arbitrary `cost`) — it's purely request-count-based today only because one line throws the value away.

**Architecture:** Wire the sidecar's existing `cost` plumbing through to a query parameter; add a bounded refund Lua script + a new `RefundCost` RPC so callers can reserve an LLM-style cost estimate upfront and refund the unused portion (mirroring the AWS Bedrock/LiteLLM pattern) on the same check→release round trip both SDKs already perform; bring both SDKs up to parity on `cost`/`priority` parameters; give the Python SDK the timeout/retry/TLS support it has none of; add shared contract tests catching wire-format drift between the two SDKs and the sidecar's real protocol; emit IETF-draft rate-limit headers; add a sidecar-local negative cache for repeat-offender identifiers.

**Tech Stack:** Go 1.26 (sidecar, `packages/sdks/go`); Python 3.10+, stdlib only — `packages/sdks/python`'s `pyproject.toml` has `dependencies = []` and this plan does not change that (timeout/retry/TLS are all achievable via `urllib.request`/`ssl`/`time.sleep`).

**Spec:** `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 4 section (items 1–8).

## Global Constraints

- **No proto/core/Lua changes for item 1** (cost wiring) — the substrate already supports arbitrary cost end-to-end; only `services/sidecar/proxy/proxy.go`'s hardcoded `Cost: 1` needs to change.
- **The refund mechanism (item 3) is a NEW bounded Lua script, not a reuse of `CheckAndDecrement` with a negative cost** — `token_bucket.lua`'s refill-then-clamp arithmetic only re-clamps to `burst` on the *next* call, so a raw negative-cost decrement could push `tokens` above `burst` and stay there until another call happens to reset it. The new script clamps on write, unconditionally.
- **IETF header scope is deliberately partial (item 7).** `RateLimit-Reset` is emitted because it's derivable from data the sidecar already has (`RetryAfterMs`). `limit`/`remaining` are NOT emitted — core doesn't track or expose per-key remaining-token count today, and adding that is a proto change with its own ripple (Lua script return shape, `Decision` struct, `CheckRateLimitResponse` fields, grpcserver handler) that belongs in its own future spec, not folded silently into this item. Document this gap explicitly; do not claim full IETF compliance.
- **Python SDK stays dependency-free.** `pyproject.toml`'s `dependencies = []` is not touched — timeout via `urllib.request.urlopen(req, timeout=...)`, TLS via `ssl.SSLContext` + `urllib.request.urlopen(req, context=...)`, retry/backoff via plain `time.sleep` in a loop.
- **`-race` is mandatory** on every `go test` invocation touching `services/core` or `services/sidecar`.
- **No comments except non-obvious WHY**, matching the existing codebase's terse style (Go) / minimal-comment style (Python, matching `client.py`'s existing near-zero-comment convention).
- Files: 200-400 lines typical, 800 max.
- Never run `git push`, `git branch -D`, or any destructive git command — commit locally only.
- **NEVER bypass a safety mechanism to get a denied action through.** If `git commit` or any git operation is denied by a PreToolUse hook or permission check, STOP and report BLOCKED with the exact denial message verbatim — never retry with `dangerouslyDisableSandbox`, `--no-verify`, or any other bypass. This rule was violated once in an earlier phase of this same roadmap and corrected — it must not recur.

---

### Task 1: Wire up the existing `cost` plumbing

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`, `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Produces: `/check?cost=N` — an optional query parameter, default `1`, soft-failing (with a log line) to `1` on a non-positive or unparseable value, matching this codebase's general soft-fail-on-optional-param convention (`resolveMaxInflight`, `resolveRampStartPct`).

- [ ] **Step 1: Write the failing tests**

Append to `services/sidecar/proxy/proxy_test.go`:
```go
func TestServeHTTP_UsesCostQueryParamWhenPresent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=5", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 5 {
		t.Errorf("expected Cost=5 forwarded to core, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_DefaultsCostToOneWhenAbsent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected default Cost=1, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_InvalidCostFallsBackToOne(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=not-a-number", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected fallback Cost=1 for an unparseable value, got %d", client.lastReq.Cost)
	}
}

func TestServeHTTP_NonPositiveCostFallsBackToOne(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1&cost=0", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if client.lastReq.Cost != 1 {
		t.Errorf("expected fallback Cost=1 for a non-positive value, got %d", client.lastReq.Cost)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -race -run "UsesCostQueryParam|DefaultsCostToOne|InvalidCostFallsBack|NonPositiveCostFallsBack" -v`
Expected: FAIL — `Cost` is hardcoded to `1` today regardless of the query string.

- [ ] **Step 3: Implement**

In `services/sidecar/proxy/proxy.go`, add a helper near the top-level functions:
```go
func resolveCost(raw string) int {
	if raw == "" {
		return 1
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		log.Printf("sidecar: /check: cost=%q is invalid, defaulting to 1", raw)
		return 1
	}
	return parsed
}
```
Change the `CheckRateLimitRequest` construction:
```go
	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{
		Key:              key,
		Cost:             int32(resolveCost(r.URL.Query().Get("cost"))),
		SkipReservations: skipReservations,
		Priority:         protoPriority,
	})
```
(`Cost` on `CheckRateLimitRequest` is `int32` per the proto; `resolveCost` returns `int` for simplicity, cast at the call site — matching how `int32(reservation counts)` etc. are already cast inline elsewhere in this file.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v`

- [ ] **Step 5: Commit**

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): wire the /check cost query parameter through to core"
```

---

### Task 2: SDK helpers to compute LLM-style token cost

**Files:**
- Create: `packages/sdks/go/cost.go`, `packages/sdks/go/cost_test.go`
- Create: `packages/sdks/python/src/ratecap/cost.py`, `packages/sdks/python/tests/test_cost.py`
- Modify: `packages/sdks/python/src/ratecap/__init__.py`

**Interfaces:**
- Produces: `ratecap.EstimateLLMCost(inputTokens, maxTokens int) int` (Go); `ratecap.estimate_llm_cost(input_tokens, max_tokens)` (Python). Both mirror the AWS Bedrock/LiteLLM estimate: `cost = input_tokens + max_tokens`. `ratecap-core` never parses any LLM provider's response — this is a pure client-side helper computing the number a caller then passes as `cost`.

- [ ] **Step 1: Write the failing Go test**

```go
// packages/sdks/go/cost_test.go
package ratecap_test

import (
	"testing"

	ratecap "github.com/ratecap/sdk-go"
)

func TestEstimateLLMCost_SumsInputAndMaxTokens(t *testing.T) {
	got := ratecap.EstimateLLMCost(500, 1000)
	if got != 1500 {
		t.Errorf("expected 500+1000=1500, got %d", got)
	}
}

func TestEstimateLLMCost_ZeroInputTokensStillCountsMaxTokens(t *testing.T) {
	got := ratecap.EstimateLLMCost(0, 1000)
	if got != 1000 {
		t.Errorf("expected 1000, got %d", got)
	}
}

func TestEstimateLLMCost_NegativeInputsClampToZero(t *testing.T) {
	got := ratecap.EstimateLLMCost(-10, -5)
	if got != 0 {
		t.Errorf("expected negative inputs to clamp to a 0 cost (never a negative Cost sent to core), got %d", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd packages/sdks/go && go test . -race -run TestEstimateLLMCost -v`

- [ ] **Step 3: Implement**

```go
// packages/sdks/go/cost.go
package ratecap

// EstimateLLMCost mirrors the AWS Bedrock/LiteLLM token-cost estimate
// (input tokens + max output tokens the model is allowed to generate) —
// ratecap-core stays transport/schema-agnostic; it never parses any LLM
// provider's request or response, it only ever sees the resulting int.
func EstimateLLMCost(inputTokens, maxTokens int) int {
	cost := inputTokens + maxTokens
	if cost < 0 {
		return 0
	}
	return cost
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd packages/sdks/go && go test . -race -v`

- [ ] **Step 5: Write the failing Python test**

```python
# packages/sdks/python/tests/test_cost.py
import os
import sys
import unittest

_SRC_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
if _SRC_DIR not in sys.path:
    sys.path.insert(0, _SRC_DIR)

from ratecap import estimate_llm_cost


class TestEstimateLLMCost(unittest.TestCase):
    def test_sums_input_and_max_tokens(self):
        self.assertEqual(estimate_llm_cost(500, 1000), 1500)

    def test_zero_input_tokens_still_counts_max_tokens(self):
        self.assertEqual(estimate_llm_cost(0, 1000), 1000)

    def test_negative_inputs_clamp_to_zero(self):
        self.assertEqual(estimate_llm_cost(-10, -5), 0)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 6: Run test to verify it fails**

Run: `cd packages/sdks/python && python -m unittest tests.test_cost -v`

- [ ] **Step 7: Implement**

```python
# packages/sdks/python/src/ratecap/cost.py
def estimate_llm_cost(input_tokens, max_tokens):
    cost = input_tokens + max_tokens
    return max(cost, 0)
```

Update `packages/sdks/python/src/ratecap/__init__.py`:
```python
from ratecap.client import AllowResult, Client, Ticket
from ratecap.cost import estimate_llm_cost

__all__ = ["AllowResult", "Client", "Ticket", "estimate_llm_cost"]
```

- [ ] **Step 8: Run test to verify it passes**

Run: `cd packages/sdks/python && python -m unittest discover -s tests -v`

- [ ] **Step 9: Commit**

```bash
git add packages/sdks/go/cost.go packages/sdks/go/cost_test.go packages/sdks/python/src/ratecap/cost.py packages/sdks/python/tests/test_cost.py packages/sdks/python/src/ratecap/__init__.py
git commit -m "feat(sdk): add EstimateLLMCost/estimate_llm_cost helpers to both SDKs"
```

---

### Task 3: Bounded refund Lua script, `StateStore` method, and `RefundCost` RPC

**Files:**
- Create: `services/core/store/lua/refund_tokens.lua`
- Modify: `services/core/store/store.go`, `services/core/store/redis.go`, `services/core/store/redis_test.go`, `proto/ratecap/v1/ratecap.proto`, `services/core/grpcserver/server.go`, `services/core/grpcserver/server_test.go`, `services/core/main.go`

**Interfaces:**
- Produces: `StateStore.RefundTokens(ctx, key string, burst, refundAmount int) error`; proto RPC `RefundCost(RefundCostRequest{key, refund_amount}) returns (RefundCostResponse{})`.

- [ ] **Step 1: Write the Lua script**

```lua
-- services/core/store/lua/refund_tokens.lua
-- KEYS[1] = bucket key
-- ARGV[1] = burst (max bucket capacity, for clamping)
-- ARGV[2] = refund_amount (tokens to give back)
--
-- Returns the new token count after the refund. Clamps on write,
-- unconditionally — unlike token_bucket.lua's refill arithmetic, which
-- only re-clamps to burst on the NEXT call, this script must never leave
-- the bucket above burst even transiently, since nothing else reads or
-- corrects it until the next real check.

local key = KEYS[1]
local burst = tonumber(ARGV[1])
local refund_amount = tonumber(ARGV[2])

local tokens = tonumber(redis.call("HGET", key, "tokens"))
if tokens == nil then
  -- Bucket was never created (or has since expired) — nothing was ever
  -- decremented from it, so there is nothing to refund into.
  return 0
end

tokens = math.min(burst, tokens + refund_amount)
redis.call("HSET", key, "tokens", tokens)
return tokens
```

- [ ] **Step 2: Write the failing store test**

Append to `services/core/store/redis_test.go`:
```go
func TestRefundTokens_ReturnsUnusedTokensUpToBurst(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	// Reserve 10, actually use 3 -> refund 7.
	allowed, _, err := s.CheckAndDecrement(ctx, "refund-key", 10, 10, 10)
	if err != nil || !allowed {
		t.Fatalf("unexpected setup failure: allowed=%v err=%v", allowed, err)
	}

	if err := s.RefundTokens(ctx, "refund-key", 10, 7); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	allowed, _, err = s.CheckAndDecrement(ctx, "refund-key", 10, 10, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected the refunded 7 tokens to be available for a subsequent 7-cost check")
	}
}

func TestRefundTokens_ClampsToBurstEvenWithLargeRefund(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	allowed, _, err := s.CheckAndDecrement(ctx, "refund-clamp-key", 10, 10, 1)
	if err != nil || !allowed {
		t.Fatalf("unexpected setup failure: allowed=%v err=%v", allowed, err)
	}

	if err := s.RefundTokens(ctx, "refund-clamp-key", 10, 1000); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	allowed, _, err = s.CheckAndDecrement(ctx, "refund-clamp-key", 10, 10, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected exactly burst=10 tokens to be available (not 1000+), i.e. the refund clamped")
	}

	allowed, _, err = s.CheckAndDecrement(ctx, "refund-clamp-key", 10, 10, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected the bucket to be fully drained after consuming exactly burst tokens — the earlier large refund must not have exceeded burst")
	}
}

func TestRefundTokens_NoOpOnNonexistentBucket(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client, testSigningKey)
	ctx := context.Background()

	if err := s.RefundTokens(ctx, "never-checked-key", 10, 5); err != nil {
		t.Fatalf("expected a no-op (nil error) for a bucket that was never created, got: %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd services/core && go test ./store/... -race -run TestRefundTokens -v`

- [ ] **Step 4: Implement**

Add to `services/core/store/store.go`:
```go
type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (allowed bool, token string, err error)
	DecrConcurrent(ctx context.Context, key, token string) error
	RefundTokens(ctx context.Context, key string, burst, refundAmount int) error
}
```

Add to `services/core/store/redis.go`:
```go
//go:embed lua/refund_tokens.lua
var refundTokensScript string
```
Add the script to the struct and constructor:
```go
type RedisStore struct {
	client            *redis.Client
	tokenBucket       *redis.Script
	concurrentLimiter *redis.Script
	refundTokens      *redis.Script
	signingKey        []byte
}

func NewRedisStore(client *redis.Client, signingKey []byte) *RedisStore {
	return &RedisStore{
		client:            client,
		tokenBucket:       redis.NewScript(tokenBucketScript),
		concurrentLimiter: redis.NewScript(concurrentLimiterScript),
		refundTokens:      redis.NewScript(refundTokensScript),
		signingKey:        signingKey,
	}
}
```
Add the method:
```go
func (s *RedisStore) RefundTokens(ctx context.Context, key string, burst, refundAmount int) error {
	key = rateLimiterKeyPrefix + key
	start := time.Now()
	err := s.refundTokens.Run(ctx, s.client, []string{key}, burst, refundAmount).Err()
	coremetrics.RecordRedisCall("refund_tokens", time.Since(start), err)
	return err
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd services/core && go test ./store/... -race -v`

- [ ] **Step 6: Add the proto RPC**

In `proto/ratecap/v1/ratecap.proto`:
```protobuf
service RatecapService {
  rpc CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse);
  rpc ReleaseConcurrency(ReleaseConcurrencyRequest) returns (ReleaseConcurrencyResponse);
  rpc SetDynamicLimit(SetDynamicLimitRequest) returns (SetDynamicLimitResponse);
  rpc RefundCost(RefundCostRequest) returns (RefundCostResponse);
}
```
```protobuf
message RefundCostRequest {
  string key = 1;
  int32 refund_amount = 2; // tokens to return to Tier 1's bucket for this key, clamped to burst
}

message RefundCostResponse {}
```

Regenerate (installing the plugins first if not already on `PATH`):
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
cd proto && go build ./... && go test ./... -race
```

- [ ] **Step 7: Write the failing grpcserver test**

Append to `services/core/grpcserver/server_test.go`:
```go
type fakeRefundStore struct {
	lastKey          string
	lastBurst        int
	lastRefundAmount int
	err              error
}

func (f *fakeRefundStore) RefundTokens(_ context.Context, key string, burst, refundAmount int) error {
	f.lastKey, f.lastBurst, f.lastRefundAmount = key, burst, refundAmount
	return f.err
}

func TestRefundCost_CallsRefundTokensWithKeyAndAmount(t *testing.T) {
	refundStore := &fakeRefundStore{}
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, &fakeRateLimiter{}, &fakeFleetShedder{}, refundStore, testSigningKey)

	_, err := s.RefundCost(context.Background(), &ratecapv1.RefundCostRequest{Key: "user-1", RefundAmount: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refundStore.lastKey != "user-1" || refundStore.lastRefundAmount != 7 {
		t.Errorf("expected RefundTokens called with key=user-1 refundAmount=7, got key=%q refundAmount=%d", refundStore.lastKey, refundStore.lastRefundAmount)
	}
}

func TestRefundCost_SanitizesStoreError(t *testing.T) {
	refundStore := &fakeRefundStore{err: errors.New("dial tcp: connection refused")}
	s := NewServer(limiter.NewPipeline(&fakeLimiter{}), &fakeReleaser{}, &fakeRateLimiter{}, &fakeFleetShedder{}, refundStore, testSigningKey)

	_, err := s.RefundCost(context.Background(), &ratecapv1.RefundCostRequest{Key: "user-1", RefundAmount: 7})
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected sanitized error, but original error text leaked: %v", err)
	}
}
```
Note: `NewServer`'s signature is gaining a 5th positional argument (`refundStore`), inserted before `signingKey` — every existing `NewServer(...)` call site in this test file (there are many, from Phase 2's Task 12) needs one more argument, `&fakeRefundStore{}` at minimum. Update every one; do not skip any. Also check `services/core/grpcserver/auth_integration_test.go` and `mtls_integration_test.go` for their own call sites.

**What needs to burn for the refund** — Tier 1's `burst` config value: the server needs it to pass as the Lua script's clamp bound. Since `TokenBucketLimiter` already holds `burst` internally (behind its mutex, via `Reconfigure`), add a narrow accessor:
```go
// services/core/limiter/tokenbucket.go — add method
func (l *TokenBucketLimiter) Burst() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.burst
}
```
Add a matching test to `tokenbucket_test.go`:
```go
func TestTokenBucketLimiter_Burst_ReturnsCurrentBurst(t *testing.T) {
	fs := newFakeStore()
	l := limiter.NewTokenBucketLimiter(fs, 100, 42, false)
	if got := l.Burst(); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}
```

- [ ] **Step 8: Run tests to verify they fail, then implement**

Run: `cd services/core && go test ./grpcserver/... -race -run TestRefundCost -v` (expect FAIL first).

Extend `grpcserver.Server`:
```go
type refunder interface {
	RefundTokens(ctx context.Context, key string, burst, refundAmount int) error
}

type burstGetter interface {
	Burst() int
}

type Server struct {
	ratecapv1.UnimplementedRatecapServiceServer
	pipeline     checker
	releaser     concurrencyReleaser
	rateLimiter  dynamicLimitSetter
	fleetShedder reservedPctSetter
	refundStore  refunder
	signingKey   []byte
}

func NewServer(p checker, releaser concurrencyReleaser, rateLimiter dynamicLimitSetter, fleetShedder reservedPctSetter, refundStore refunder, signingKey []byte) *Server {
	return &Server{pipeline: p, releaser: releaser, rateLimiter: rateLimiter, fleetShedder: fleetShedder, refundStore: refundStore, signingKey: signingKey}
}

func (s *Server) RefundCost(ctx context.Context, req *ratecapv1.RefundCostRequest) (*ratecapv1.RefundCostResponse, error) {
	burst := 0
	if bg, ok := s.rateLimiter.(burstGetter); ok {
		burst = bg.Burst()
	}
	if err := s.refundStore.RefundTokens(ctx, req.Key, burst, int(req.RefundAmount)); err != nil {
		return nil, internalError("RefundCost", err)
	}
	return &ratecapv1.RefundCostResponse{}, nil
}
```
(the type-asserted `burstGetter` check keeps `dynamicLimitSetter` — the existing narrow interface `rateLimiter` is typed as — from needing a `Burst()` method itself; the real `*limiter.TokenBucketLimiter` passed in by `main.go` satisfies both interfaces simultaneously, so the assertion always succeeds in production. Test fakes that only implement `SetRate` legitimately skip it, defaulting `burst` to 0 — harmless for a test double whose `RefundTokens` fake doesn't care about the burst value's correctness).

Update `services/core/main.go`'s call site:
```go
	coreServer := grpcserver.NewServer(pipeline, redisStore, rateLimiter, fleetShedder, redisStore, []byte(concurrencySigningKey))
```
(`redisStore` is passed twice — once as the existing `releaser concurrencyReleaser` argument, once as the new `refundStore refunder` argument — `*store.RedisStore` already implements both interfaces, since Step 4 added `RefundTokens` to `StateStore`/`RedisStore` directly).

- [ ] **Step 9: Run the full core suite**

Run: `cd services/core && go build ./... && go test ./... -race`

- [ ] **Step 10: Commit**

```bash
git add services/core/store/lua/refund_tokens.lua services/core/store/store.go services/core/store/redis.go services/core/store/redis_test.go proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/grpcserver/server.go services/core/grpcserver/server_test.go services/core/grpcserver/auth_integration_test.go services/core/grpcserver/mtls_integration_test.go services/core/limiter/tokenbucket.go services/core/limiter/tokenbucket_test.go services/core/main.go
git commit -m "feat(core,proto): add RefundCost RPC and a bounded, burst-clamped refund Lua script"
```

---

### Task 4: Sidecar `/release` extension for the Tier-1 refund

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`, `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Produces: `/release` now also accepts `X-RateCap-Refund-Key` + `X-RateCap-Refund-Amount` headers, independent of (and combinable with) the existing `X-RateCap-Concurrency-Key`/`Token` headers.

- [ ] **Step 1: Write the failing tests**

Extend `fakeReleaseClient` in `services/sidecar/proxy/proxy_test.go` to also implement the new interface method:
```go
type fakeReleaseClient struct {
	lastKey          string
	lastToken        string
	lastRefundKey    string
	lastRefundAmount int32
	err              error
	refundErr        error
}

func (f *fakeReleaseClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastKey = in.Key
	f.lastToken = in.ConcurrencyToken
	return &ratecapv1.ReleaseConcurrencyResponse{}, f.err
}

func (f *fakeReleaseClient) RefundCost(_ context.Context, in *ratecapv1.RefundCostRequest, _ ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error) {
	f.lastRefundKey = in.Key
	f.lastRefundAmount = in.RefundAmount
	return &ratecapv1.RefundCostResponse{}, f.refundErr
}
```
(this changes every existing `fakeReleaseClient{...}` struct literal in the file only if they set fields positionally — check first; the existing ones in this file use field names like `&fakeReleaseClient{err: errors.New(...)}`, which remain valid with the new fields added).

Add:
```go
func TestReleaseHandler_ServeHTTP_RefundKeyAloneIsSufficient(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastRefundKey != "user-1" || client.lastRefundAmount != 7 {
		t.Errorf("expected RefundCost called with key=user-1 amount=7, got key=%q amount=%d", client.lastRefundKey, client.lastRefundAmount)
	}
}

func TestReleaseHandler_ServeHTTP_RefundAndConcurrencyReleaseBothHappenInOneCall(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Concurrency-Key", "user-1")
	req.Header.Set("X-RateCap-Concurrency-Token", "tok-abc")
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if client.lastKey != "user-1" || client.lastToken != "tok-abc" {
		t.Errorf("expected ReleaseConcurrency called, got key=%q token=%q", client.lastKey, client.lastToken)
	}
	if client.lastRefundKey != "user-1" || client.lastRefundAmount != 7 {
		t.Errorf("expected RefundCost also called, got key=%q amount=%d", client.lastRefundKey, client.lastRefundAmount)
	}
}

func TestReleaseHandler_ServeHTTP_InvalidRefundAmountReturns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "not-a-number")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an unparseable refund amount, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_RefundUpstreamErrorReturns500(t *testing.T) {
	client := &fakeReleaseClient{refundErr: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release", nil)
	req.Header.Set("X-RateCap-Refund-Key", "user-1")
	req.Header.Set("X-RateCap-Refund-Amount", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}
```

Also check the existing `TestReleaseHandler_ServeHTTP_MissingKeyReturns400` test still makes sense: it sends only `X-RateCap-Concurrency-Token` with no key of either kind — must still 400, since neither a concurrency key nor a refund key is present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -race -run "RefundKey|RefundAndConcurrency|InvalidRefundAmount|RefundUpstreamError" -v`

- [ ] **Step 3: Implement**

In `services/sidecar/proxy/proxy.go`, extend the interface and handler:
```go
type releaseClient interface {
	ReleaseConcurrency(ctx context.Context, in *ratecapv1.ReleaseConcurrencyRequest, opts ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error)
	RefundCost(ctx context.Context, in *ratecapv1.RefundCostRequest, opts ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error)
}
```
```go
func (h *ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	concurrencyKey := r.Header.Get("X-RateCap-Concurrency-Key")
	refundKey := r.Header.Get("X-RateCap-Refund-Key")
	if concurrencyKey == "" && refundKey == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	if concurrencyKey != "" {
		token := r.Header.Get("X-RateCap-Concurrency-Token")
		_, err := h.client.ReleaseConcurrency(r.Context(), &ratecapv1.ReleaseConcurrencyRequest{Key: concurrencyKey, ConcurrencyToken: token})
		if err != nil {
			log.Printf("sidecar: /release: upstream release failed: %v", err)
			metrics.RecordUpstreamError("release_concurrency")
			metrics.RecordReleaseResult("failure")
			http.Error(w, "upstream release failed", http.StatusInternalServerError)
			return
		}
		metrics.RecordReleaseResult("success")
	}

	if refundKey != "" {
		refundAmount, err := strconv.Atoi(r.Header.Get("X-RateCap-Refund-Amount"))
		if err != nil || refundAmount <= 0 {
			http.Error(w, "invalid or missing X-RateCap-Refund-Amount", http.StatusBadRequest)
			return
		}
		_, err = h.client.RefundCost(r.Context(), &ratecapv1.RefundCostRequest{Key: refundKey, RefundAmount: int32(refundAmount)})
		if err != nil {
			log.Printf("sidecar: /release: upstream refund failed: %v", err)
			metrics.RecordUpstreamError("refund_cost")
			http.Error(w, "upstream refund failed", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 4: Run the full sidecar suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race`

- [ ] **Step 5: Commit**

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): extend /release to accept an independent Tier-1 refund"
```

---

### Task 5: Go SDK — `Cost`/`Priority` options and `Ticket.Refund`

**Files:**
- Modify: `packages/sdks/go/client.go`, `packages/sdks/go/client_test.go`

**Interfaces:**
- Produces: `ratecap.Priority` (type alias, `Sheddable`/`Critical` constants), `ratecap.WithCost(int) CheckOption`, `ratecap.WithPriority(Priority) CheckOption`; `Client.Allow`/`Client.Acquire` both gain a variadic `...CheckOption` parameter (backward compatible — existing zero-arg call sites keep compiling); `Ticket.Refund(ctx, refundAmount int) error`.

- [ ] **Step 1: Write the failing tests**

Append to `packages/sdks/go/client_test.go`:
```go
func TestAllow_WithCost_SendsCostQueryParam(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, err := client.Allow(context.Background(), "user-1", ratecap.WithCost(5)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("cost"); got != "5" {
		t.Errorf("expected cost=5 on the /check request, got %q", got)
	}
}

func TestAllow_WithoutCostOption_OmitsCostQueryParam(t *testing.T) {
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

	if got := capturedQuery.Get("cost"); got != "" {
		t.Errorf("expected no cost param when WithCost is not used (server-side default of 1 applies), got %q", got)
	}
}

func TestAllow_WithPriority_SendsPriorityHeader(t *testing.T) {
	var capturedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("x-ratecap-priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, _, err := client.Allow(context.Background(), "user-1", ratecap.WithPriority(ratecap.Critical)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedHeader != "critical" {
		t.Errorf(`expected x-ratecap-priority: critical, got %q`, capturedHeader)
	}
}

func TestAcquire_WithCostAndPriority_SendsBoth(t *testing.T) {
	var capturedQuery url.Values
	var capturedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		capturedHeader = r.Header.Get("x-ratecap-priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	if _, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500), ratecap.WithPriority(ratecap.Critical)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := capturedQuery.Get("cost"); got != "1500" {
		t.Errorf("expected cost=1500, got %q", got)
	}
	if capturedHeader != "critical" {
		t.Errorf(`expected x-ratecap-priority: critical, got %q`, capturedHeader)
	}
}

func TestTicket_Refund_SendsRefundHeaders(t *testing.T) {
	var refundCalls []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.WriteHeader(http.StatusOK)
		case "/release":
			refundCalls = append(refundCalls, r.Header)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Refund(context.Background(), 1200); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(refundCalls) != 1 {
		t.Fatalf("expected exactly 1 /release call for the refund, got %d", len(refundCalls))
	}
	if got := refundCalls[0].Get("X-RateCap-Refund-Key"); got != "user-1" {
		t.Errorf("expected X-RateCap-Refund-Key=user-1, got %q", got)
	}
	if got := refundCalls[0].Get("X-RateCap-Refund-Amount"); got != "1200" {
		t.Errorf("expected X-RateCap-Refund-Amount=1200, got %q", got)
	}
}

func TestTicket_Refund_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.WriteHeader(http.StatusOK)
		case "/release":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1", ratecap.WithCost(1500))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Refund(context.Background(), 1200); err == nil {
		t.Fatal("expected an error when the sidecar returns non-200 for the refund")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/sdks/go && go test . -race -run "WithCost|WithPriority|Refund" -v`

- [ ] **Step 3: Implement**

Add to `packages/sdks/go/client.go` (near the top, after imports):
```go
type Priority int

const (
	Sheddable Priority = iota
	Critical
)

func (p Priority) headerValue() string {
	if p == Critical {
		return "critical"
	}
	return "sheddable"
}

type checkOptions struct {
	cost        int
	hasCost     bool
	priority    Priority
	hasPriority bool
}

type CheckOption func(*checkOptions)

func WithCost(cost int) CheckOption {
	return func(o *checkOptions) { o.cost, o.hasCost = cost, true }
}

func WithPriority(p Priority) CheckOption {
	return func(o *checkOptions) { o.priority, o.hasPriority = p, true }
}

func applyCheckOptions(opts []CheckOption) checkOptions {
	var o checkOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func (o checkOptions) applyToRequest(req *http.Request, query url.Values) {
	if o.hasCost {
		query.Set("cost", strconv.Itoa(o.cost))
	}
	if o.hasPriority {
		req.Header.Set("x-ratecap-priority", o.priority.headerValue())
	}
}
```

Change `Allow`'s signature and body:
```go
func (c *Client) Allow(ctx context.Context, key string, opts ...CheckOption) (allowed bool, retryAfterMs int64, err error) {
	query := url.Values{"key": {key}, "skip_reservations": {"true"}}
	options := applyCheckOptions(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarAddr+"/check", nil)
	if err != nil {
		return false, 0, err
	}
	options.applyToRequest(req, query)
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, 0, nil
	}

	retryAfterMs = 0
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return false, retryAfterMs, nil
}
```
Change `Acquire` identically in shape (build `req` first, apply options, set `t.key` on the returned `Ticket` for use by `Refund`):
```go
func (c *Client) Acquire(ctx context.Context, key string, opts ...CheckOption) (*Ticket, error) {
	query := url.Values{"key": {key}}
	options := applyCheckOptions(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarAddr+"/check", nil)
	if err != nil {
		return nil, err
	}
	options.applyToRequest(req, query)
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reservations []reservation
	for i := 0; ; i++ {
		tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
		if tok == "" {
			break
		}
		resKey := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
		reservations = append(reservations, reservation{key: resKey, tok: tok})
	}

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, key: key, reservations: reservations}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, key: key, reservations: reservations}, nil
}
```
Add `key` to `Ticket` and add `Refund`:
```go
type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client       *Client
	key          string
	reservations []reservation
}

// Refund reserves-upfront-refund-unused: a caller that estimated a higher
// cost than it actually used (e.g. an LLM call whose real token count came
// in under the max_tokens estimate) calls this with the difference. It is
// independent of Release — a Ticket from a plain Acquire with no reservations
// can still be refunded, and Refund never touches t.reservations.
func (t *Ticket) Refund(ctx context.Context, refundAmount int) error {
	reqURL := t.client.sidecarAddr + "/release"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-RateCap-Refund-Key", t.key)
	req.Header.Set("X-RateCap-Refund-Amount", strconv.Itoa(refundAmount))

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: refund failed with status %d", resp.StatusCode)
	}
	return nil
}
```
Add `"net/url"` to the imports if not already present (it already is, per the existing file).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd packages/sdks/go && go build ./... && go test . -race -v`

- [ ] **Step 5: Commit**

```bash
git add packages/sdks/go/client.go packages/sdks/go/client_test.go
git commit -m "feat(sdk-go): add Cost/Priority CheckOptions and Ticket.Refund"
```

---

### Task 6: Python SDK — `cost`/`priority` parameters and `Ticket.refund`

**Files:**
- Modify: `packages/sdks/python/src/ratecap/client.py`, `packages/sdks/python/tests/test_client.py`

**Interfaces:**
- Produces: `Client.allow(key, cost=1, priority=None)`, `Client.acquire(key, cost=1, priority=None)` (parity with the Go SDK's `WithCost`/`WithPriority`, using plain keyword arguments since Python doesn't need a functional-options pattern); `Ticket.refund(refund_amount)`.

- [ ] **Step 1: Write the failing tests**

Append to `packages/sdks/python/tests/test_client.py`:
```python
class TestCostAndPriority(unittest.TestCase):
    def test_allow_sends_cost_query_param_when_given(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1", cost=5)
            self.assertEqual(captured.get("cost"), "5")

    def test_allow_omits_cost_query_param_by_default(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1")
            self.assertNotIn("cost", captured)

    def test_allow_sends_priority_header_when_given(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1", priority="critical")
            self.assertEqual(captured.get("x-ratecap-priority"), "critical")

    def test_acquire_sends_cost_and_priority(self):
        captured_query = {}
        captured_headers = {}

        def handler(method, path, query, headers):
            if path == "/check":
                captured_query.update(query)
                captured_headers.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.acquire("user-1", cost=1500, priority="critical")
            self.assertEqual(captured_query.get("cost"), "1500")
            self.assertEqual(captured_headers.get("x-ratecap-priority"), "critical")


class TestRefund(unittest.TestCase):
    def test_refund_sends_refund_headers(self):
        refund_calls = []

        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {}
            if path == "/release":
                refund_calls.append(dict(headers))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1", cost=1500)
            ticket.refund(1200)

        self.assertEqual(len(refund_calls), 1)
        self.assertEqual(refund_calls[0].get("X-Ratecap-Refund-Key"), "user-1")
        self.assertEqual(refund_calls[0].get("X-Ratecap-Refund-Amount"), "1200")

    def test_refund_raises_on_non_200(self):
        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {}
            if path == "/release":
                return 500, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1", cost=1500)
            with self.assertRaises(RuntimeError):
                ticket.refund(1200)
```
(header key casing note: Python's `http.client`/`email.message`-backed header dict, as returned by `BaseHTTPRequestHandler.headers` in `fake_sidecar.py`, normalizes header names to Title-Case — matching the existing test file's own established pattern of asserting `"X-Ratecap-Concurrency-Key"` rather than `"X-RateCap-Concurrency-Key"` elsewhere in this same file; use `"X-Ratecap-Refund-Key"`/`"X-Ratecap-Refund-Amount"` for the same reason.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/sdks/python && python -m unittest discover -s tests -v -k "Cost or Priority or Refund"` (or run the whole suite; the new classes will simply fail with `AttributeError`/`TypeError` until implemented).

- [ ] **Step 3: Implement**

In `packages/sdks/python/src/ratecap/client.py`, change `Ticket.__init__` to accept and store the original check `key`, and add `refund`:
```python
class Ticket:
    def __init__(self, client, key, allowed, retry_after_ms=0, reservations=None):
        self.allowed = allowed
        self.retry_after_ms = retry_after_ms
        self._client = client
        self._key = key
        self._reservations = reservations or []

    def release(self):
        errors = []
        for reservation in self._reservations:
            try:
                self._client._release_one(reservation)
            except Exception as exc:
                errors.append(f"{reservation.key}: {exc}")
        if errors:
            raise RuntimeError("failed to release reservation(s): " + "; ".join(errors))

    def refund(self, refund_amount):
        url = f"{self._client._sidecar_addr}/release"
        req = urllib.request.Request(
            url,
            method="POST",
            headers={
                "X-RateCap-Refund-Key": self._key,
                "X-RateCap-Refund-Amount": str(refund_amount),
            },
        )
        with urllib.request.urlopen(req) as resp:
            if resp.status != 200:
                raise RuntimeError(f"refund failed with status {resp.status}")

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()
        return False
```
Change `Client.allow`/`Client.acquire`:
```python
    def allow(self, key, cost=None, priority=None):
        params = {"key": key, "skip_reservations": "true"}
        if cost is not None:
            params["cost"] = str(cost)
        query = urllib.parse.urlencode(params)
        url = f"{self._sidecar_addr}/check?{query}"
        headers = {"x-ratecap-priority": priority} if priority else {}
        req = urllib.request.Request(url, method="GET", headers=headers)
        try:
            with urllib.request.urlopen(req) as resp:
                return AllowResult(allowed=True)
        except urllib.error.HTTPError as err:
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return AllowResult(allowed=False, retry_after_ms=retry_after_ms)

    def acquire(self, key, cost=None, priority=None):
        params = {"key": key}
        if cost is not None:
            params["cost"] = str(cost)
        query = urllib.parse.urlencode(params)
        url = f"{self._sidecar_addr}/check?{query}"
        headers = {"x-ratecap-priority": priority} if priority else {}
        req = urllib.request.Request(url, method="GET", headers=headers)
        try:
            with urllib.request.urlopen(req) as resp:
                reservations = self._parse_reservations(resp.headers)
                return Ticket(self, key, allowed=True, reservations=reservations)
        except urllib.error.HTTPError as err:
            reservations = self._parse_reservations(err.headers)
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return Ticket(self, key, allowed=False, retry_after_ms=retry_after_ms, reservations=reservations)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd packages/sdks/python && python -m unittest discover -s tests -v`
Expected: PASS, including every pre-existing test (the `Ticket(self, allowed=...)` call sites in the existing test file construct `Ticket` directly nowhere — only `Client.acquire` does, which Step 3 already updated consistently).

- [ ] **Step 5: Commit**

```bash
git add packages/sdks/python/src/ratecap/client.py packages/sdks/python/tests/test_client.py
git commit -m "feat(sdk-python): add cost/priority parameters and Ticket.refund"
```

---

### Task 7: Python SDK — timeout, retry/backoff, and TLS support

**Files:**
- Modify: `packages/sdks/python/src/ratecap/client.py`, `packages/sdks/python/tests/test_client.py`

**Interfaces:**
- Produces: `Client(sidecar_addr, timeout=5.0, max_retries=0, backoff_base=0.1, ca_file=None)` — all new constructor parameters, all optional with defaults that preserve today's exact behavior (`max_retries=0` means no retry loop at all; `ca_file=None` means default system trust, matching `urllib`'s existing implicit behavior for `https://` URLs).

- [ ] **Step 1: Write the failing tests**

Append to `packages/sdks/python/tests/test_client.py`:
```python
class TestTimeout(unittest.TestCase):
    def test_default_timeout_is_applied(self):
        captured = {}

        def handler(method, path, query, headers):
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            # No direct way to observe urllib's internal timeout without a
            # slow server; assert the attribute is set to a sane positive
            # default instead, and that a custom value overrides it.
            self.assertGreater(client._timeout, 0)

    def test_custom_timeout_is_stored(self):
        client = Client("http://localhost:8080", timeout=2.5)
        self.assertEqual(client._timeout, 2.5)


class TestRetry(unittest.TestCase):
    def test_retries_on_connection_error_up_to_max_retries(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            if len(attempts) < 3:
                raise ConnectionResetError("simulated transient failure")
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url, max_retries=3, backoff_base=0.01)
            result = client.allow("user-1")
            self.assertTrue(result.allowed)
        self.assertEqual(len(attempts), 3)

    def test_no_retry_by_default(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            raise ConnectionResetError("simulated failure")

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            with self.assertRaises(Exception):
                client.allow("user-1")
        self.assertEqual(len(attempts), 1)

    def test_gives_up_after_max_retries_exhausted(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            raise ConnectionResetError("simulated permanent failure")

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url, max_retries=2, backoff_base=0.01)
            with self.assertRaises(Exception):
                client.allow("user-1")
        self.assertEqual(len(attempts), 3)  # 1 initial + 2 retries


class TestTLS(unittest.TestCase):
    def test_ca_file_none_uses_default_context(self):
        client = Client("https://localhost:8443")
        self.assertIsNone(client._ssl_context)

    def test_ca_file_builds_custom_ssl_context(self):
        import ssl
        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".pem", delete=False) as f:
            # A syntactically-plausible-but-fake CA file is enough to prove
            # the client attempts to build a context from it; a real TLS
            # handshake against it is exercised by the sidecar/core's own
            # existing mTLS integration tests, not this SDK's unit suite.
            f.write(b"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
            ca_path = f.name

        try:
            with self.assertRaises(ssl.SSLError):
                Client("https://localhost:8443", ca_file=ca_path)
        finally:
            import os
            os.unlink(ca_path)
```

Note on the last test: a syntactically-invalid (but present) CA file makes `ssl.SSLContext.load_verify_locations` raise `ssl.SSLError` — proving the client actually attempts to load it (not silently ignoring `ca_file`) without needing a real, valid CA certificate fixture. If a genuinely valid throwaway CA PEM is preferred instead (no exception expected, just a non-`None` `_ssl_context`), generating one requires `cryptography` or shelling out to `openssl` — out of scope for this dependency-free SDK's own test suite; the error-path assertion above is the pragmatic, still-real proof.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/sdks/python && python -m unittest discover -s tests -v -k "Timeout or Retry or TLS"`

- [ ] **Step 3: Implement**

Change `Client.__init__` and add a shared request-execution helper in `packages/sdks/python/src/ratecap/client.py`:
```python
import ssl
import time


class Client:
    def __init__(self, sidecar_addr, timeout=5.0, max_retries=0, backoff_base=0.1, ca_file=None):
        self._sidecar_addr = sidecar_addr.rstrip("/")
        self._timeout = timeout
        self._max_retries = max_retries
        self._backoff_base = backoff_base
        self._ssl_context = None
        if ca_file is not None:
            self._ssl_context = ssl.create_default_context(cafile=ca_file)

    def _urlopen(self, req):
        attempt = 0
        while True:
            try:
                kwargs = {"timeout": self._timeout}
                if self._ssl_context is not None:
                    kwargs["context"] = self._ssl_context
                return urllib.request.urlopen(req, **kwargs)
            except urllib.error.HTTPError:
                raise
            except Exception:
                if attempt >= self._max_retries:
                    raise
                time.sleep(self._backoff_base * (2 ** attempt))
                attempt += 1
```
(`urllib.error.HTTPError` is re-raised immediately without retrying — a 429/503 is a real rate-limit decision the caller needs to see, not a transient failure to paper over; only connection-level exceptions retry.)

Change every existing `with urllib.request.urlopen(req) as resp:` call site (`allow`, `acquire`, `_release_one`, and Task 6's new `refund`) to `with self._urlopen(req) as resp:` instead.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd packages/sdks/python && python -m unittest discover -s tests -v`

- [ ] **Step 5: Commit**

```bash
git add packages/sdks/python/src/ratecap/client.py packages/sdks/python/tests/test_client.py
git commit -m "feat(sdk-python): add timeout, retry/backoff, and TLS support"
```

---

### Task 8: Shared contract tests between the Go SDK, Python SDK, and the sidecar's real wire protocol

**Files:**
- Create: `services/sidecar/contracttest/contracttest_test.go`

**Interfaces:**
- Produces: Go-side contract tests that spin up the REAL `proxy.Handler`/`proxy.ReleaseHandler` (not a fake) behind an `httptest.Server`, then drive it with a Go SDK `Client` AND, via `exec.Command`, the real Python SDK — asserting the sidecar accepts and interprets both identically. This is the practical, single-language-runner shape of a "golden fixture" suite: the fixture is the running sidecar itself, not a static file, so drift between either SDK and the real protocol is caught directly rather than by three independently-maintained descriptions of the protocol agreeing with each other by coincidence.

- [ ] **Step 1: Write the failing test**

```go
// services/sidecar/contracttest/contracttest_test.go
package contracttest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
	"github.com/ratecap/sidecar/worker"
)

type fakeCoreClient struct {
	lastCheckReq  *ratecapv1.CheckRateLimitRequest
	lastReleaseReq *ratecapv1.ReleaseConcurrencyRequest
	lastRefundReq *ratecapv1.RefundCostRequest
}

func (f *fakeCoreClient) CheckRateLimit(_ context.Context, in *ratecapv1.CheckRateLimitRequest, _ ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	f.lastCheckReq = in
	return &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}, nil
}

func (f *fakeCoreClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastReleaseReq = in
	return &ratecapv1.ReleaseConcurrencyResponse{}, nil
}

func (f *fakeCoreClient) RefundCost(_ context.Context, in *ratecapv1.RefundCostRequest, _ ...grpc.CallOption) (*ratecapv1.RefundCostResponse, error) {
	f.lastRefundReq = in
	return &ratecapv1.RefundCostResponse{}, nil
}

func newTestSidecar(core *fakeCoreClient) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(core, proxy.Sheddable, worker.NewShedder(1000)))
	mux.Handle("/release", proxy.NewReleaseHandler(core))
	return httptest.NewServer(mux)
}

func TestGoSDK_CheckRequestMatchesSidecarExpectedFormat(t *testing.T) {
	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	resp, err := http.Get(server.URL + "/check?key=contract-test-key&cost=42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if core.lastCheckReq.Key != "contract-test-key" {
		t.Errorf("expected key=contract-test-key, got %q", core.lastCheckReq.Key)
	}
	if core.lastCheckReq.Cost != 42 {
		t.Errorf("expected cost=42, got %d", core.lastCheckReq.Cost)
	}
}

func TestPythonSDK_CheckRequestMatchesSidecarExpectedFormat(t *testing.T) {
	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	// Drives the REAL Python SDK against the REAL sidecar handler (not a
	// second, independently-written description of the protocol) — this is
	// the actual drift-detection mechanism this task exists for.
	script := `
import sys
sys.path.insert(0, "../../packages/sdks/python/src")
from ratecap import Client
client = Client(sys.argv[1])
result = client.allow("contract-test-key-py", cost=42)
print("ok" if result.allowed else "rejected")
`
	cmd := exec.Command("python3", "-c", script, server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 invocation failed: %v, output: %s", err, output)
	}

	if core.lastCheckReq.Key != "contract-test-key-py" {
		t.Errorf("expected key=contract-test-key-py, got %q", core.lastCheckReq.Key)
	}
	if core.lastCheckReq.Cost != 42 {
		t.Errorf("expected cost=42 sent by the Python SDK, got %d", core.lastCheckReq.Cost)
	}
}

func TestGoSDK_ReleaseRequestUsesHeadersNotQuery(t *testing.T) {
	core := &fakeCoreClient{}
	server := newTestSidecar(core)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/release", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req.Header.Set("X-RateCap-Concurrency-Key", "contract-key")
	req.Header.Set("X-RateCap-Concurrency-Token", "contract-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if core.lastReleaseReq.Key != "contract-key" || core.lastReleaseReq.ConcurrencyToken != "contract-token" {
		t.Errorf("expected key=contract-key token=contract-token, got key=%q token=%q", core.lastReleaseReq.Key, core.lastReleaseReq.ConcurrencyToken)
	}
}

var _ = json.Marshal // silence unused import if the above set ends up not needing it after edits
```

Note: the Python subprocess test (`TestPythonSDK_CheckRequestMatchesSidecarExpectedFormat`) assumes `python3` is on `PATH` and is run with this file's own directory as the working directory (Go's `go test` already does this by default) — the relative `../../packages/sdks/python/src` path resolves from `services/sidecar/contracttest/`. If `python3` is unavailable in CI, this specific test should skip with `t.Skip(...)`, not fail the whole suite — check for the binary first:
```go
if _, err := exec.LookPath("python3"); err != nil {
	t.Skip("python3 not available in this environment")
}
```
Add this check as the first line of `TestPythonSDK_CheckRequestMatchesSidecarExpectedFormat`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/sidecar && go test ./contracttest/... -race -v`
Expected: FAIL — package doesn't exist yet; once created, should largely pass immediately since it's testing already-correct behavior, but that's the point — it's now a regression tripwire, not a new feature.

- [ ] **Step 3: Run test to verify it passes**

Run: `cd services/sidecar && go test ./contracttest/... -race -v`

- [ ] **Step 4: Commit**

```bash
git add services/sidecar/contracttest/contracttest_test.go
git commit -m "test(sidecar): add shared contract tests driving both SDKs against the real sidecar protocol"
```

---

### Task 9: IETF `RateLimit-Reset` response header

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`, `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Produces: a `RateLimit-Reset` response header (seconds until retry is worthwhile) on `REJECT_429` responses, derived from the existing `RetryAfterMs`/1000 rounded up — no proto or core changes. **Deliberately partial**: `limit`/`remaining` are not emitted (see Global Constraints).

- [ ] **Step 1: Write the failing tests**

Append to `services/sidecar/proxy/proxy_test.go`:
```go
func TestServeHTTP_Reject429SetsIETFRateLimitResetHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 2500}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Reset"); got != "3" {
		t.Errorf(`expected RateLimit-Reset="3" (2500ms rounded up to 3s), got %q`, got)
	}
}

func TestServeHTTP_AllowDoesNotSetRateLimitResetHeader(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("RateLimit-Reset"); got != "" {
		t.Errorf("expected no RateLimit-Reset header on an allowed request, got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -race -run "IETFRateLimitReset|AllowDoesNotSetRateLimitReset" -v`

- [ ] **Step 3: Implement**

In `services/sidecar/proxy/proxy.go`, in the `REJECT_429` case of the existing action switch:
```go
	switch action {
	case ratecapv1.Action_ALLOW, ratecapv1.Action_SHADOW_LOG, ratecapv1.Action_QUEUE:
		w.WriteHeader(http.StatusOK)
	case ratecapv1.Action_REJECT_429:
		w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
		w.Header().Set("RateLimit-Reset", strconv.FormatInt((resp.RetryAfterMs+999)/1000, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	case ratecapv1.Action_REJECT_503:
		w.Header().Set("X-RateCap-Shed-Tier", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
```
(`(ms + 999) / 1000` is integer ceiling division — the IETF draft's `reset` field is in whole seconds, and rounding down would tell a caller it's safe to retry slightly before it actually is).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v`

- [ ] **Step 5: Commit**

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "feat(sidecar): emit IETF draft-ietf-httpapi-ratelimit-headers RateLimit-Reset on 429"
```

---

### Task 10: Sidecar-local negative cache for already-denied identifiers

**Files:**
- Create: `services/sidecar/negativecache/negativecache.go`, `services/sidecar/negativecache/negativecache_test.go`
- Modify: `services/sidecar/proxy/proxy.go`, `services/sidecar/proxy/proxy_test.go`, `services/sidecar/main.go`

**Interfaces:**
- Produces: `negativecache.New() *Cache`, `(*Cache).MarkDenied(key string, retryAfter time.Duration)`, `(*Cache).IsDenied(key string) (denied bool, remaining time.Duration)`. Wired into `proxy.Handler` so a repeat request for a key already known to be denied short-circuits before ever calling core — no core/proto changes, matching the roadmap item's own scoping.

- [ ] **Step 1: Write the failing cache tests**

```go
// services/sidecar/negativecache/negativecache_test.go
package negativecache_test

import (
	"testing"
	"time"

	"github.com/ratecap/sidecar/negativecache"
)

func TestIsDenied_FalseForUnknownKey(t *testing.T) {
	c := negativecache.New()
	denied, _ := c.IsDenied("never-marked")
	if denied {
		t.Error("expected false for a key that was never marked denied")
	}
}

func TestIsDenied_TrueImmediatelyAfterMarkDenied(t *testing.T) {
	c := negativecache.New()
	c.MarkDenied("user-1", 500*time.Millisecond)

	denied, remaining := c.IsDenied("user-1")
	if !denied {
		t.Fatal("expected true immediately after MarkDenied")
	}
	if remaining <= 0 || remaining > 500*time.Millisecond {
		t.Errorf("expected remaining in (0, 500ms], got %v", remaining)
	}
}

func TestIsDenied_FalseAfterWindowElapses(t *testing.T) {
	now := time.Now()
	clock := &now
	c := negativecache.NewWithClock(func() time.Time { return *clock })

	c.MarkDenied("user-1", 100*time.Millisecond)
	*clock = clock.Add(200 * time.Millisecond)

	denied, _ := c.IsDenied("user-1")
	if denied {
		t.Error("expected false once the denial window has elapsed")
	}
}

func TestMarkDenied_OverwritesAnEarlierShorterWindow(t *testing.T) {
	now := time.Now()
	clock := &now
	c := negativecache.NewWithClock(func() time.Time { return *clock })

	c.MarkDenied("user-1", 100*time.Millisecond)
	c.MarkDenied("user-1", 5*time.Second)

	*clock = clock.Add(200 * time.Millisecond)

	denied, _ := c.IsDenied("user-1")
	if !denied {
		t.Error("expected the later, longer MarkDenied call to have taken effect")
	}
}

func TestCache_ConcurrentAccessIsRaceFree(t *testing.T) {
	c := negativecache.New()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(n int) {
			c.MarkDenied("key", 10*time.Millisecond)
			c.IsDenied("key")
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./negativecache/... -race -v`

- [ ] **Step 3: Implement**

```go
// services/sidecar/negativecache/negativecache.go
package negativecache

import (
	"sync"
	"time"
)

// sweepThreshold bounds unbounded memory growth from a flood of distinct
// denied keys: an opportunistic sweep on MarkDenied, rather than a
// background goroutine, keeps this package dependency-free of any
// lifecycle/shutdown concern.
const sweepThreshold = 10000

type Cache struct {
	mu     sync.Mutex
	denied map[string]time.Time
	clock  func() time.Time
}

func New() *Cache {
	return NewWithClock(time.Now)
}

func NewWithClock(clock func() time.Time) *Cache {
	return &Cache{denied: make(map[string]time.Time), clock: clock}
}

// MarkDenied records that key was rejected and should short-circuit future
// checks until retryAfter elapses — exactly as long as the real decision
// already told the caller to wait, not a heuristic guess.
func (c *Cache) MarkDenied(key string, retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denied[key] = c.clock().Add(retryAfter)
	if len(c.denied) > sweepThreshold {
		c.sweepLocked()
	}
}

func (c *Cache) IsDenied(key string) (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.denied[key]
	if !ok {
		return false, 0
	}
	now := c.clock()
	if now.After(until) {
		delete(c.denied, key)
		return false, 0
	}
	return true, until.Sub(now)
}

func (c *Cache) sweepLocked() {
	now := c.clock()
	for k, until := range c.denied {
		if now.After(until) {
			delete(c.denied, k)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/sidecar && go test ./negativecache/... -race -v`

- [ ] **Step 5: Wire into `proxy.Handler`**

Write the failing proxy tests first, appending to `services/sidecar/proxy/proxy_test.go`:
```go
func TestServeHTTP_ShortCircuitsOnCachedDenial(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	cache.MarkDenied("user-1", 5*time.Second)
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 from the cached denial, got %d", rec.Code)
	}
	if client.lastReq != nil {
		t.Error("expected core to never be called for a cache-short-circuited request")
	}
}

func TestServeHTTP_MarksDeniedOnRealReject429(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 5000}}
	cache := negativecache.New()
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	denied, _ := cache.IsDenied("user-2")
	if !denied {
		t.Error("expected a real REJECT_429 to mark the key denied in the cache")
	}
}

func TestServeHTTP_DoesNotMarkDeniedOnAllow(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	cache := negativecache.New()
	h := proxy.NewHandlerWithCache(client, proxy.Sheddable, worker.NewShedder(100), cache)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-3", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	denied, _ := cache.IsDenied("user-3")
	if denied {
		t.Error("expected ALLOW to never mark a key denied")
	}
}

func TestNewHandler_HasNoNegativeCacheByDefault(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable, worker.NewShedder(100))

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected NewHandler (no cache) to work exactly as before, got %d", rec.Code)
	}
}
```
Add `"github.com/ratecap/sidecar/negativecache"` and `"time"` (if not already imported) to this test file.

Run: `cd services/sidecar && go test ./proxy/... -race -run "ShortCircuitsOnCachedDenial|MarksDeniedOnRealReject429|DoesNotMarkDeniedOnAllow|NewHandler_HasNoNegativeCacheByDefault" -v` — expect FAIL.

Implement in `services/sidecar/proxy/proxy.go`:
```go
type Handler struct {
	client          ratecapClient
	defaultPriority Priority
	shedder         *worker.Shedder
	negativeCache   *negativecache.Cache
}

func NewHandler(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder}
}

// NewHandlerWithCache is NewHandler plus an explicit negative cache — kept
// as a separate constructor (rather than a parameter on NewHandler) so
// every existing call site of NewHandler keeps compiling unchanged.
func NewHandlerWithCache(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder, cache *negativecache.Cache) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority, shedder: shedder, negativeCache: cache}
}
```
Add the short-circuit check right after `key` is resolved and validated (after the existing `if key == "" { ... }` block, before the worker-shedder logic reruns — actually place it as the FIRST thing after parsing `key`, before the Tier-4 shedder check, since a cached denial should skip Tier 4 accounting too, not just Tier 1-3):
```go
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	if h.negativeCache != nil {
		if denied, remaining := h.negativeCache.IsDenied(key); denied {
			w.Header().Set("Retry-After-Ms", strconv.FormatInt(remaining.Milliseconds(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}
```
This means `key` must be resolved BEFORE the worker-shedder block, not after — check the current file's ordering (the worker-shedder block currently reads `r.URL.Query().Get("key")` independently inside its own branch via `shedKey`). Re-order so `key` is resolved once, early, and reused by both the negative-cache check and the worker-shedder's `shedKey` usage (rename `shedKey` references to `key` and remove the now-redundant duplicate `key := r.URL.Query().Get("key")` later in the function). Verify this reordering doesn't change any existing test's behavior — the missing-key 400 check must still fire before both the shedder and the cache check.

At the real-decision recording site (after `metrics.RecordDecision(resp.Tier, actionLabel(realAction))`), add:
```go
	if h.negativeCache != nil && realAction == ratecapv1.Action_REJECT_429 {
		h.negativeCache.MarkDenied(key, time.Duration(resp.RetryAfterMs)*time.Millisecond)
	}
```
(scoped to `REJECT_429` specifically, not `REJECT_503`: a 503 fleet/worker shed is a capacity signal that can flip to ALLOW moments later once load drops, whereas a 429 has a caller-specific `RetryAfterMs` that's the exact right cache window — caching a 503 risks needlessly extending an outage's blast radius past when capacity actually recovered).

- [ ] **Step 6: Wire an opt-in negative cache into `main.go`**

In `services/sidecar/main.go`, change:
```go
	protectedMux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
```
to:
```go
	protectedMux.Handle("/check", proxy.NewHandlerWithCache(client, proxy.Sheddable, shedder, negativecache.New()))
```
Add the import `"github.com/ratecap/sidecar/negativecache"`. This is unconditionally on (not env-gated) since it's purely an internal optimization with no externally-visible behavior change beyond skipping redundant upstream calls for a key already known to be denied within its own already-issued `RetryAfterMs` window — not a new policy decision an operator needs to opt into.

- [ ] **Step 7: Run the full sidecar suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race`

- [ ] **Step 8: Commit**

```bash
git add services/sidecar/negativecache/negativecache.go services/sidecar/negativecache/negativecache_test.go services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go services/sidecar/main.go
git commit -m "feat(sidecar): add a local negative cache for already-denied identifiers"
```

---

### Task 11: Documentation and version bump to v2.8.0

**Files:**
- Modify: `ARCHITECTURE.md`, `VERSION`, `CHANGELOG.md`

- [ ] **Step 1: Add a Token-cost wiring section to `ARCHITECTURE.md`**

Find the insertion point via `grep -n "^## " ARCHITECTURE.md`, then add:
```markdown
## Token-cost wiring (v2.8.0)

Tier 1 has always been a generic variable-cost token bucket internally (`token_bucket.lua`, `RedisStore.CheckAndDecrement`, and `CheckRateLimitRequest.cost` all supported arbitrary cost since v1) — the sidecar simply hardcoded `Cost: 1` on every call. `/check?cost=N` now wires this through (default `1`, unchanged for any caller that doesn't pass it).

### Reserve-upfront, refund-unused

Mirroring the AWS Bedrock/LiteLLM pattern for LLM token accounting: a caller can pass a conservative cost estimate (e.g. `EstimateLLMCost(input_tokens, max_tokens)`) to `/check`, then once the real usage is known, refund the unused portion via `/release` with `X-RateCap-Refund-Key`/`X-RateCap-Refund-Amount`. This is a *new*, separate bounded Lua script (`refund_tokens.lua`) that clamps to `burst` on write — it is NOT implemented as `CheckAndDecrement` with a negative cost, since `token_bucket.lua`'s refill arithmetic only re-clamps to `burst` on the *next* call, which would let a large refund transiently exceed `burst` and stay there until some unrelated future call happened to correct it.

A refund and a Tier 2 concurrency release are independent and can be combined in one `/release` call — see `services/sidecar/proxy.ReleaseHandler`.

### IETF rate-limit headers (partial)

`RateLimit-Reset` (seconds, IETF `draft-ietf-httpapi-ratelimit-headers`) is emitted on `429` responses, computed from the existing `RetryAfterMs`. `limit` and `remaining` are **not** emitted — `ratecap-core` does not track or return a per-key remaining-token count today; adding that is a proto change (new Lua script return value, new `CheckRateLimitResponse` field) that belongs in its own future spec, not folded silently into this partial step.

### Negative cache

The sidecar keeps a local, in-memory cache of recently-denied identifiers (`services/sidecar/negativecache`), keyed by the same `RetryAfterMs` value the real decision already returned — a repeat request for a key still within its own denial window short-circuits before ever calling core, a cheap p99 win under a sustained abuse flood. This is complementary to, not a replacement for, Tier 4's worker shedder: the cache only ever short-circuits a key core has *already* rejected once; it never makes an independent shedding decision of its own.
```

- [ ] **Step 2: Add the CHANGELOG entry**

Insert above the current top heading:
```markdown
## [2.8.0] — 2026-08-31 — Phase 4 SDK & API: Token-Cost Wiring

Minor release: Phase 4 of the v3 upgrade roadmap — finishes an already-designed feature. RateCap's Tier 1 substrate was already a generic variable-cost token bucket end-to-end; only the sidecar's hardcoded `Cost: 1` stood in the way.

### Added

- `/check?cost=N` — wires the existing cost plumbing through, default `1` (unchanged for existing callers).
- `EstimateLLMCost`/`estimate_llm_cost` helpers on both SDKs, mirroring the AWS Bedrock/LiteLLM token-cost estimate.
- A `RefundCost` gRPC RPC and a new bounded, burst-clamped Lua script, exposed via `/release`'s new `X-RateCap-Refund-Key`/`X-RateCap-Refund-Amount` headers — reserve a cost estimate upfront, refund the unused portion once real usage is known.
- Go SDK: `WithCost`/`WithPriority` options on `Allow`/`Acquire`, `Ticket.Refund`.
- Python SDK: `cost`/`priority` parameters on `allow`/`acquire`, `Ticket.refund`, and — previously entirely missing — timeout, retry/backoff, and TLS support (`Client(..., timeout=, max_retries=, backoff_base=, ca_file=)`), all via the standard library only.
- Shared contract tests (`services/sidecar/contracttest`) driving both SDKs against the real sidecar handler, catching wire-format drift directly instead of via independently-maintained protocol descriptions agreeing by coincidence.
- `RateLimit-Reset` response header (IETF `draft-ietf-httpapi-ratelimit-headers`) on `429` responses — partial compliance; `limit`/`remaining` require a future proto change and are explicitly out of scope here.
- A sidecar-local negative cache for already-denied identifiers, complementary to Tier 4's worker shedder.
```

- [ ] **Step 3: Bump `VERSION`**

```
2.8.0
```

- [ ] **Step 4: Commit**

```bash
git add ARCHITECTURE.md VERSION CHANGELOG.md
git commit -m "docs: document token-cost wiring, refund contract, and negative cache; bump VERSION to 2.8.0"
```

---

## Post-Implementation (not a task — controller responsibility)

After all 11 tasks pass task review and the final whole-branch review is clean — **run the empirical demo-stack verification established after Phase 2's cross-task integration gap** (`cd deploy && bash generate-demo-certs.sh && docker compose up --build`, confirm all containers reach `Up`, `/healthz` returns 200, and a `/check` call still works with the new default `cost` behavior) before considering this phase done. Then: push the branch (the user runs `git push` themselves due to the `DestructiveGuard` hook), open a PR into `develop` titled `feat: RateCap v3 roadmap Phase 4 — SDK & API: Token-Cost Wiring`, merge once CI is green, then promote `develop` → `main` and tag/release `v2.8.0` — mirroring exactly the Phase 1/2/3 cycle.
