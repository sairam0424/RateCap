# Tier 2 (Concurrent Requests Limiter) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Stripe's Tier 2 (Concurrent Requests Limiter) to RateCap, composed with the already-shipped Tier 1 (Request Rate Limiter) via a new `Pipeline` type, proven end-to-end through the same SDK → sidecar → core → Redis chain the walking skeleton established.

**Architecture:** Tier 2 bounds simultaneous in-flight requests (not RPS) per key, via a Redis sorted set where the acquire timestamp is the score (enabling stale-slot reaping) and a random token is the member (enabling exact release). A new `Pipeline` composes ordered `Limiter`s — Tier 1 then Tier 2 — short-circuiting on the first reject. Because Tier 2 needs an explicit release (unlike Tier 1's fire-and-forget check), the wire contract gains a token-correlated `ReleaseConcurrency` RPC, and the SDK gains a new `Acquire()`/`Ticket` API alongside the unchanged `Allow()`.

**Tech Stack:** Go 1.26.2, gRPC/Protocol Buffers (existing `proto/` module), `github.com/redis/go-redis/v9`, `github.com/google/uuid` (already an indirect dependency via testcontainers — promoted to direct for token generation), `github.com/testcontainers/testcontainers-go` (integration tests), Docker Compose (e2e demo).

## Global Constraints

- No comments in code unless explaining a non-obvious WHY. Never comment on WHAT the code does.
- Files: 200-400 lines typical, 800 max.
- Test-first: for every new behavior, write the failing test before the implementation.
- `Allow()` on `packages/sdks/go/client.go` must NOT change signature or behavior — it is already shipped and used by `deploy/sampleapp`.
- `Decision.Token` is a single `string` field, not a slice — only Tier 2 issues a token in this phase; this is a deliberate, documented scope boundary (see design spec's Pipeline section), not an oversight.
- Docker is required for Task 2's integration tests and Task 9's end-to-end verification. Docker has been observed to go unreachable intermittently in this environment even after starting successfully once — if a docker command fails, retry `open -a Docker` and poll (`sleep 10 && docker info`) for up to 60 seconds before concluding it's genuinely unavailable.
- Reference spec: `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/docs/superpowers/specs/2026-07-14-tier-2-concurrent-limiter-design.md`
- Reference prior plan (conventions, patterns): `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/docs/superpowers/plans/2026-07-13-walking-skeleton.md`

---

## File Structure

```
proto/ratecap/v1/ratecap.proto              # Modify: concurrency_token field, ReleaseConcurrency RPC
services/core/store/store.go                # Modify: StateStore gains IncrConcurrent/DecrConcurrent
services/core/store/redis.go                # Modify: RedisStore implements the two new methods
services/core/store/lua/concurrent_limiter.lua  # Create: reap-count-add Lua script
services/core/store/redis_test.go           # Modify: add IncrConcurrent/DecrConcurrent integration tests
services/core/limiter/limiter.go            # Modify: Decision gains Token field
services/core/limiter/concurrency.go        # Create: ConcurrencyLimiter implementing Limiter
services/core/limiter/concurrency_test.go   # Create: pure unit tests against a fake store
services/core/limiter/pipeline.go           # Create: Pipeline type composing []Limiter
services/core/limiter/pipeline_test.go      # Create: pure unit tests against fake tiers
services/core/grpcserver/server.go          # Modify: Server holds *limiter.Pipeline, adds ReleaseConcurrency
services/core/grpcserver/server_test.go     # Modify: update NewServer call sites, add ReleaseConcurrency tests
services/core/config/config.go              # Modify: Config gains ConcurrencyLimiterConfig
services/core/config/config_test.go         # Modify: add concurrency_limiter parsing test
services/core/main.go                       # Modify: wire ConcurrencyLimiter + Pipeline + second Reconfigure
services/sidecar/proxy/proxy.go             # Modify: /check returns token header, new /release handler
services/sidecar/proxy/proxy_test.go        # Modify: add token-header and /release tests
services/sidecar/main.go                    # Modify: register the /release route
packages/sdks/go/client.go                  # Modify: add Acquire()/Ticket type (Allow() untouched)
packages/sdks/go/client_test.go             # Modify: add Acquire()/Release() tests
deploy/ratecap.yaml                         # Modify: add concurrency_limiter config block
deploy/sampleapp/main.go                    # Modify: demonstrate tier 2 via a slow endpoint
```

---

## Task 1: Extend the gRPC contract

**Files:**
- Modify: `proto/ratecap/v1/ratecap.proto`
- (generated) `proto/ratecap/v1/ratecap.pb.go`, `proto/ratecap/v1/ratecap_grpc.pb.go`

**Interfaces:**
- Produces: `ratecapv1.CheckRateLimitResponse.ConcurrencyToken` (string field), `ratecapv1.RatecapServiceClient.ReleaseConcurrency(ctx, *ReleaseConcurrencyRequest, ...grpc.CallOption) (*ReleaseConcurrencyResponse, error)`, `ratecapv1.RatecapServiceServer.ReleaseConcurrency(context.Context, *ReleaseConcurrencyRequest) (*ReleaseConcurrencyResponse, error)`, `ratecapv1.UnimplementedRatecapServiceServer` (regenerated to include a no-op `ReleaseConcurrency`).

- [ ] **Step 1: Modify the proto file**

Replace the full contents of `proto/ratecap/v1/ratecap.proto` with:

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

message CheckRateLimitRequest {
  string key = 1;
  int32 cost = 2;
}

message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  string concurrency_token = 3;
}

message ReleaseConcurrencyRequest {
  string key = 1;
  string concurrency_token = 2;
}

message ReleaseConcurrencyResponse {}
```

- [ ] **Step 2: Regenerate the Go stubs**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
export PATH="$PATH:$(go env GOPATH)/bin"
protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto \
  --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto \
  ratecap/v1/ratecap.proto
```

Expected: regenerates `proto/ratecap/v1/ratecap.pb.go` and `proto/ratecap/v1/ratecap_grpc.pb.go` with no errors.

- [ ] **Step 3: Verify the proto module builds and the new symbols exist**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/proto
go build ./...
grep -c "ConcurrencyToken" ratecap/v1/ratecap.pb.go
grep -c "ReleaseConcurrency" ratecap/v1/ratecap_grpc.pb.go
```

Expected: `go build` succeeds; both `grep -c` commands return a count of at least 1.

- [ ] **Step 4: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add proto/
git commit -m "feat(proto): add ReleaseConcurrency RPC and concurrency_token field for tier 2"
```

---

## Task 2: StateStore concurrency methods + Lua script

**Files:**
- Modify: `services/core/store/store.go`
- Modify: `services/core/store/redis.go`
- Create: `services/core/store/lua/concurrent_limiter.lua`
- Modify: `services/core/store/redis_test.go`

**Interfaces:**
- Consumes: nothing new from other tasks — this is a leaf extension of the existing `StateStore`/`RedisStore`.
- Produces:
  ```go
  type StateStore interface {
      CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
      IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (allowed bool, token string, err error)
      DecrConcurrent(ctx context.Context, key, token string) error
  }
  ```
  `RedisStore` implements both new methods.

- [ ] **Step 1: Write the failing integration tests**

Append to `services/core/store/redis_test.go` (the file already has `startRedis(t)` defined — reuse it, do not redefine):

```go
func TestIncrConcurrent_AllowsUpToCap(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, token, err := s.IncrConcurrent(ctx, "concurrent-key-cap", 3, 30000)
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed within cap of 3", i)
		}
		if token == "" {
			t.Fatalf("request %d: expected non-empty token", i)
		}
	}

	allowed, token, err := s.IncrConcurrent(ctx, "concurrent-key-cap", 3, 30000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("4th request should be rejected, cap is 3")
	}
	if token != "" {
		t.Fatalf("expected empty token on rejection, got %q", token)
	}
}

func TestDecrConcurrent_FreesSlotForNextRequest(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	var tokens []string
	for i := 0; i < 2; i++ {
		_, token, err := s.IncrConcurrent(ctx, "concurrent-key-release", 2, 30000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		tokens = append(tokens, token)
	}

	allowed, _, err := s.IncrConcurrent(ctx, "concurrent-key-release", 2, 30000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("3rd request should be rejected, cap is 2")
	}

	if err := s.DecrConcurrent(ctx, "concurrent-key-release", tokens[0]); err != nil {
		t.Fatalf("unexpected error releasing: %v", err)
	}

	allowed, _, err = s.IncrConcurrent(ctx, "concurrent-key-release", 2, 30000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("request after release should be allowed")
	}
}

func TestIncrConcurrent_ReapsStaleEntriesPastMaxDuration(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	allowed, _, err := s.IncrConcurrent(ctx, "concurrent-key-reap", 1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("first request should be allowed")
	}

	time.Sleep(150 * time.Millisecond)

	allowed, token, err := s.IncrConcurrent(ctx, "concurrent-key-reap", 1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("request after maxDurationMs should be allowed — the stale entry should have been reaped")
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestIncrConcurrent_ConcurrentAtomicity(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	const attempts = 50
	const cap = 10
	results := make(chan bool, attempts)

	for i := 0; i < attempts; i++ {
		go func() {
			allowed, _, err := s.IncrConcurrent(ctx, "concurrent-key-atomic", cap, 30000)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results <- allowed
		}()
	}

	allowedCount := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			allowedCount++
		}
	}

	if allowedCount != cap {
		t.Fatalf("expected exactly %d allowed under concurrent load, got %d", cap, allowedCount)
	}
}
```

Add `"time"` to the existing `import` block in `services/core/store/redis_test.go` if not already present (check the current file — it is not currently imported since Task 3's original walking-skeleton tests didn't need it).

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./store/... -run "TestIncrConcurrent|TestDecrConcurrent" -v
```

Expected: FAIL — `store.RedisStore` has no method `IncrConcurrent`/`DecrConcurrent` (compile error).

- [ ] **Step 3: Extend the StateStore interface**

Replace the full contents of `services/core/store/store.go` with:

```go
package store

import "context"

type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
	IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (allowed bool, token string, err error)
	DecrConcurrent(ctx context.Context, key, token string) error
}
```

- [ ] **Step 4: Write the Lua script**

Create `services/core/store/lua/concurrent_limiter.lua`:

```lua
-- KEYS[1] = concurrency set key
-- ARGV[1] = cap (max concurrent slots)
-- ARGV[2] = max_duration_ms (reap cutoff)
-- ARGV[3] = now (unix millis)
-- ARGV[4] = token (random member to add if allowed)
--
-- Returns {allowed (1/0), token or empty string}

local key = KEYS[1]
local cap = tonumber(ARGV[1])
local max_duration_ms = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local token = ARGV[4]

redis.call("ZREMRANGEBYSCORE", key, "-inf", now - max_duration_ms)

local count = redis.call("ZCARD", key)

if count < cap then
  redis.call("ZADD", key, now, token)
  redis.call("EXPIRE", key, math.ceil(max_duration_ms / 1000) + 60)
  return {1, token}
else
  return {0, ""}
end
```

- [ ] **Step 5: Implement IncrConcurrent and DecrConcurrent**

Modify `services/core/store/redis.go` — add the embed directive, the script field, and the two new methods. Replace the full contents with:

```go
package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

//go:embed lua/concurrent_limiter.lua
var concurrentLimiterScript string

type RedisStore struct {
	client            *redis.Client
	tokenBucket       *redis.Script
	concurrentLimiter *redis.Script
}

var _ StateStore = (*RedisStore)(nil)

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{
		client:            client,
		tokenBucket:       redis.NewScript(tokenBucketScript),
		concurrentLimiter: redis.NewScript(concurrentLimiterScript),
	}
}

func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	now := time.Now().UnixMilli()
	result, err := s.tokenBucket.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}

	allowed, ok := result[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("store: unexpected allowed type %T in lua script result", result[0])
	}
	retryAfterMs, ok := result[1].(int64)
	if !ok {
		return false, 0, fmt.Errorf("store: unexpected retryAfterMs type %T in lua script result", result[1])
	}
	return allowed == 1, retryAfterMs, nil
}

func (s *RedisStore) IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error) {
	now := time.Now().UnixMilli()
	candidateToken := uuid.NewString()

	result, err := s.concurrentLimiter.Run(ctx, s.client, []string{key}, cap, maxDurationMs, now, candidateToken).Slice()
	if err != nil {
		return false, "", err
	}
	if len(result) != 2 {
		return false, "", fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}

	allowed, ok := result[0].(int64)
	if !ok {
		return false, "", fmt.Errorf("store: unexpected allowed type %T in lua script result", result[0])
	}
	token, ok := result[1].(string)
	if !ok {
		return false, "", fmt.Errorf("store: unexpected token type %T in lua script result", result[1])
	}
	return allowed == 1, token, nil
}

func (s *RedisStore) DecrConcurrent(ctx context.Context, key, token string) error {
	return s.client.ZRem(ctx, key, token).Err()
}
```

- [ ] **Step 6: Add google/uuid as a direct dependency**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go get github.com/google/uuid@v1.6.0
GOWORK=off go mod tidy
```

Expected: `go.mod`'s `github.com/google/uuid v1.6.0` line drops its `// indirect` suffix (it was already present transitively via testcontainers; this promotes it to direct since `redis.go` now imports it directly).

- [ ] **Step 7: Ensure Docker is reachable, then run the tests**

```bash
docker info >/dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

If not reachable, run `open -a Docker` and poll (`sleep 10 && docker info`) for up to 60 seconds before proceeding.

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./store/... -v
```

Expected: PASS on all tests — the 3 pre-existing `TestCheckAndDecrement_*` tests plus the 4 new `TestIncrConcurrent_*`/`TestDecrConcurrent_*` tests.

- [ ] **Step 8: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/core/store/ services/core/go.mod services/core/go.sum
git commit -m "feat(core): add IncrConcurrent/DecrConcurrent with atomic reap-count-add Lua script"
```

---

## Task 3: ConcurrencyLimiter (pure decision logic)

**Files:**
- Modify: `services/core/limiter/limiter.go`
- Create: `services/core/limiter/concurrency.go`
- Create: `services/core/limiter/concurrency_test.go`

**Interfaces:**
- Consumes: nothing from `store` directly — like `TokenBucketLimiter`, this uses a narrow local interface so the package stays Redis-free and unit-testable with a fake.
- Produces:
  ```go
  type Decision struct {
      Action       Action
      RetryAfterMs int64
      Token        string
  }

  func NewConcurrencyLimiter(s concurrencyChecker, cap int, maxDurationMs int64, shadowMode bool) *ConcurrencyLimiter
  func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error)
  func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool)
  ```
  `ConcurrencyLimiter` implements `Limiter`.

- [ ] **Step 1: Add the Token field to Decision**

Modify `services/core/limiter/limiter.go` — replace the full contents with:

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

type Decision struct {
	Action       Action
	RetryAfterMs int64
	Token        string
}

type Request struct {
	Key  string
	Cost int
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
```

- [ ] **Step 2: Write the failing unit tests**

Create `services/core/limiter/concurrency_test.go`:

```go
package limiter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeConcurrencyStore struct {
	mu      sync.Mutex
	tokens  map[string]int
	nextTok int
}

func newFakeConcurrencyStore() *fakeConcurrencyStore {
	return &fakeConcurrencyStore{tokens: make(map[string]int)}
}

func (f *fakeConcurrencyStore) IncrConcurrent(_ context.Context, key string, cap int, _ int64) (bool, string, error) {
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

func (f *fakeConcurrencyStore) DecrConcurrent(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[key]--
	return nil
}

func TestConcurrencyLimiter_AllowsExactlyCapRequests(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 3, 30000, false)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1"})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
		if d.Token == "" {
			t.Fatalf("request %d: expected non-empty token", i)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("4th request: expected REJECT_429, got %v", d.Action)
	}
}

func TestConcurrencyLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, true)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over cap in shadow mode, got %v", d.Action)
	}
}

func TestConcurrencyLimiter_ReconfigureChangesCap(t *testing.T) {
	fs := newFakeConcurrencyStore()
	l := limiter.NewConcurrencyLimiter(fs, 1, 30000, false)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-3"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 before reconfigure, got %v", d.Action)
	}

	l.Reconfigure(1, 30000, true)

	d, err = l.Check(ctx, limiter.Request{Key: "user-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG after enabling shadow mode via reconfigure, got %v", d.Action)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./limiter/... -run TestConcurrencyLimiter -v
```

Expected: FAIL — `limiter.NewConcurrencyLimiter` undefined.

- [ ] **Step 4: Implement ConcurrencyLimiter**

Create `services/core/limiter/concurrency.go`:

```go
package limiter

import (
	"context"
	"sync"
)

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

func (l *ConcurrencyLimiter) Reconfigure(cap int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *ConcurrencyLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	l.mu.RLock()
	cap, maxDurationMs, shadowMode := l.cap, l.maxDurationMs, l.shadowMode
	l.mu.RUnlock()

	allowed, token, err := l.store.IncrConcurrent(ctx, req.Key, cap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Token: token}, nil
	}

	if shadowMode {
		return Decision{Action: SHADOW_LOG}, nil
	}

	return Decision{Action: REJECT_429}, nil
}
```

This mirrors `TokenBucketLimiter`'s `sync.RWMutex` pattern exactly — the same real data race that pattern fixed in Tier 1 (see the v1 design spec's fix-round history) would recur here if `Reconfigure`/`Check` weren't synchronized the same way.

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./limiter/... -run TestConcurrencyLimiter -race -v
```

Expected: PASS — all 3 tests, no race warnings.

- [ ] **Step 6: Run the full limiter package suite to confirm no regressions**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./limiter/... -race -v
```

Expected: PASS — the 4 pre-existing `TestTokenBucketLimiter_*` tests plus the 3 new `TestConcurrencyLimiter_*` tests.

- [ ] **Step 7: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/core/limiter/limiter.go services/core/limiter/concurrency.go services/core/limiter/concurrency_test.go
git commit -m "feat(core): add ConcurrencyLimiter implementing tier 2's concurrent-requests cap"
```

---

## Task 4: Pipeline composition

**Files:**
- Create: `services/core/limiter/pipeline.go`
- Create: `services/core/limiter/pipeline_test.go`

**Interfaces:**
- Consumes: `Limiter` interface (Task 3, unchanged since Task 1's walking skeleton), `Decision{Action, RetryAfterMs, Token}` (Task 3).
- Produces:
  ```go
  func NewPipeline(tiers ...Limiter) *Pipeline
  func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error)
  ```

- [ ] **Step 1: Write the failing unit tests**

Create `services/core/limiter/pipeline_test.go`:

```go
package limiter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeTier struct {
	decision limiter.Decision
	err      error
	called   bool
}

func (f *fakeTier) Check(_ context.Context, _ limiter.Request) (limiter.Decision, error) {
	f.called = true
	return f.decision, f.err
}

func TestPipeline_AllTiersAllowReturnsAllowWithLastToken(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Token: "tok-123"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW, got %v", d.Action)
	}
	if d.Token != "tok-123" {
		t.Fatalf("expected token from tier2 to propagate, got %q", d.Token)
	}
	if !tier1.called || !tier2.called {
		t.Fatal("expected both tiers to be checked")
	}
}

func TestPipeline_FirstTierRejectShortCircuitsSecondTier(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 500}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Token: "tok-should-not-appear"}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 500 {
		t.Fatalf("expected RetryAfterMs=500, got %d", d.RetryAfterMs)
	}
	if tier2.called {
		t.Fatal("expected tier2 to be short-circuited, but it was called")
	}
}

func TestPipeline_SecondTierRejectPropagatesDecision(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 250}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 250 {
		t.Fatalf("expected RetryAfterMs=250, got %d", d.RetryAfterMs)
	}
}

func TestPipeline_ErrorFromAnyTierShortCircuits(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{err: errors.New("store unavailable")}

	p := limiter.NewPipeline(tier1, tier2)
	_, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err == nil {
		t.Fatal("expected error to propagate from tier2")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./limiter/... -run TestPipeline -v
```

Expected: FAIL — `limiter.NewPipeline` undefined.

- [ ] **Step 3: Implement Pipeline**

Create `services/core/limiter/pipeline.go`:

```go
package limiter

import "context"

type Pipeline struct {
	tiers []Limiter
}

func NewPipeline(tiers ...Limiter) *Pipeline {
	return &Pipeline{tiers: tiers}
}

func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
	final := Decision{Action: ALLOW}
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		if err != nil || d.Action != ALLOW {
			return d, err
		}
		if d.Token != "" {
			final.Token = d.Token
		}
	}
	return final, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./limiter/... -v
```

Expected: PASS — all limiter package tests (Tier 1's 4, Tier 2's 3, Pipeline's 4).

- [ ] **Step 5: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/core/limiter/pipeline.go services/core/limiter/pipeline_test.go
git commit -m "feat(core): add Pipeline composing ordered Limiters with short-circuit on reject"
```

---

## Task 5: Wire Pipeline into grpcserver, add ReleaseConcurrency RPC

**Files:**
- Modify: `services/core/grpcserver/server.go`
- Modify: `services/core/grpcserver/server_test.go`

**Interfaces:**
- Consumes: `*limiter.Pipeline` (Task 4), `store.StateStore.DecrConcurrent` (Task 2) — the release path talks to the store directly, not through the pipeline (per the design spec: "release is a targeted cleanup of one tier's reservation, not a re-run of the whole check sequence").
- Produces:
  ```go
  func NewServer(p *limiter.Pipeline, releaser concurrencyReleaser) *Server
  ```
  `Server` now implements both `CheckRateLimit` and `ReleaseConcurrency`.

- [ ] **Step 1: Write the failing tests**

Replace the full contents of `services/core/grpcserver/server_test.go`:

```go
package grpcserver_test

import (
	"context"
	"errors"
	"testing"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

type fakeLimiter struct {
	decision limiter.Decision
	err      error
}

func (f *fakeLimiter) Check(_ context.Context, _ limiter.Request) (limiter.Decision, error) {
	return f.decision, f.err
}

type fakeReleaser struct {
	lastKey   string
	lastToken string
	err       error
}

func (f *fakeReleaser) DecrConcurrent(_ context.Context, key, token string) error {
	f.lastKey = key
	f.lastToken = token
	return f.err
}

func TestCheckRateLimit_ReturnsAllowDecision(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}

func TestCheckRateLimit_ReturnsReject429WithRetryAfter(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 250}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_REJECT_429 {
		t.Errorf("expected REJECT_429, got %v", resp.Action)
	}
	if resp.RetryAfterMs != 250 {
		t.Errorf("expected RetryAfterMs=250, got %d", resp.RetryAfterMs)
	}
}

func TestCheckRateLimit_ReturnsConcurrencyTokenWhenPresent(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW, Token: "tok-abc"}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ConcurrencyToken != "tok-abc" {
		t.Errorf("expected ConcurrencyToken=%q, got %q", "tok-abc", resp.ConcurrencyToken)
	}
}

func TestReleaseConcurrency_CallsDecrConcurrentWithKeyAndToken(t *testing.T) {
	releaser := &fakeReleaser{}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: "tok-abc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if releaser.lastKey != "user-1" {
		t.Errorf("expected DecrConcurrent called with key=%q, got %q", "user-1", releaser.lastKey)
	}
	if releaser.lastToken != "tok-abc" {
		t.Errorf("expected DecrConcurrent called with token=%q, got %q", "tok-abc", releaser.lastToken)
	}
}

func TestReleaseConcurrency_PropagatesStoreError(t *testing.T) {
	releaser := &fakeReleaser{err: errors.New("redis unavailable")}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: "tok-abc",
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./grpcserver/... -v
```

Expected: FAIL — `grpcserver.NewServer` signature mismatch (old code takes a single `limiter.Limiter`, tests now pass `*limiter.Pipeline, *fakeReleaser`).

- [ ] **Step 3: Implement the updated server**

Replace the full contents of `services/core/grpcserver/server.go`:

```go
package grpcserver

import (
	"context"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/limiter"
)

type checker interface {
	Check(ctx context.Context, req limiter.Request) (limiter.Decision, error)
}

type concurrencyReleaser interface {
	DecrConcurrent(ctx context.Context, key, token string) error
}

type Server struct {
	ratecapv1.UnimplementedRatecapServiceServer
	pipeline checker
	releaser concurrencyReleaser
}

func NewServer(p checker, releaser concurrencyReleaser) *Server {
	return &Server{pipeline: p, releaser: releaser}
}

func (s *Server) CheckRateLimit(ctx context.Context, req *ratecapv1.CheckRateLimitRequest) (*ratecapv1.CheckRateLimitResponse, error) {
	decision, err := s.pipeline.Check(ctx, limiter.Request{Key: req.Key, Cost: int(req.Cost)})
	if err != nil {
		return nil, err
	}

	return &ratecapv1.CheckRateLimitResponse{
		Action:           toProtoAction(decision.Action),
		RetryAfterMs:     decision.RetryAfterMs,
		ConcurrencyToken: decision.Token,
	}, nil
}

func (s *Server) ReleaseConcurrency(ctx context.Context, req *ratecapv1.ReleaseConcurrencyRequest) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	if err := s.releaser.DecrConcurrent(ctx, req.Key, req.ConcurrencyToken); err != nil {
		return nil, err
	}
	return &ratecapv1.ReleaseConcurrencyResponse{}, nil
}

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

`NewServer`'s first parameter is typed as the narrow local `checker` interface (not `*limiter.Pipeline` directly) so the test's `limiter.NewPipeline(fl)` — which returns `*limiter.Pipeline`, satisfying `checker`'s single `Check` method — works without change, and so a future test could substitute a fake pipeline-like object without needing the real `Pipeline` type.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./grpcserver/... -v
```

Expected: PASS — all 5 tests (2 pre-existing, adapted to the new constructor signature, plus 3 new).

- [ ] **Step 5: Run the full core module test suite to confirm no regressions**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go build ./...
```

Expected: build succeeds. (`main.go` will fail to build after this task since it still calls the old `grpcserver.NewServer(rateLimiter)` — this is expected and fixed in Task 6; do not attempt to fix `main.go` here.)

- [ ] **Step 6: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/core/grpcserver/
git commit -m "feat(core): wire Pipeline into gRPC server, add ReleaseConcurrency RPC handler"
```

---

## Task 6: Config extension + core main.go wiring

**Files:**
- Modify: `services/core/config/config.go`
- Modify: `services/core/config/config_test.go`
- Modify: `services/core/main.go`

**Interfaces:**
- Consumes: `limiter.NewConcurrencyLimiter` (Task 3), `limiter.NewPipeline` (Task 4), `grpcserver.NewServer(checker, concurrencyReleaser)` (Task 5), `store.RedisStore.DecrConcurrent` (Task 2, already satisfies `concurrencyReleaser`).
- Produces:
  ```go
  type ConcurrencyLimiterConfig struct {
      DefaultMaxConcurrent int   `yaml:"default_max_concurrent"`
      MaxRequestDurationMs int64 `yaml:"max_request_duration_ms"`
      ShadowMode           bool  `yaml:"shadow_mode"`
  }
  ```
  `Config.Tiers` gains a `ConcurrencyLimiter ConcurrencyLimiterConfig` field.

- [ ] **Step 1: Write the failing config test**

Append to `services/core/config/config_test.go`:

```go
func TestLoad_ParsesConcurrencyLimiterTier(t *testing.T) {
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

	if cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent != 20 {
		t.Errorf("expected DefaultMaxConcurrent=20, got %d", cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent)
	}
	if cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs != 30000 {
		t.Errorf("expected MaxRequestDurationMs=30000, got %d", cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs)
	}
	if cfg.Tiers.ConcurrencyLimiter.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.ConcurrencyLimiter.ShadowMode)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./config/... -run TestLoad_ParsesConcurrencyLimiterTier -v
```

Expected: FAIL — `cfg.Tiers.ConcurrencyLimiter` undefined.

- [ ] **Step 3: Extend the Config struct**

Replace the full contents of `services/core/config/config.go`:

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

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter        RateLimiterConfig        `yaml:"rate_limiter"`
		ConcurrencyLimiter ConcurrencyLimiterConfig `yaml:"concurrency_limiter"`
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

- [ ] **Step 4: Run the config tests to verify they pass**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./config/... -v
```

Expected: PASS — all config package tests, including the pre-existing `TestLoad_ParsesRateLimiterTier`, `TestLoad_MissingFileReturnsError`, `TestWatch_*` tests, plus the new one.

- [ ] **Step 5: Wire ConcurrencyLimiter and Pipeline into main.go**

Replace the full contents of `services/core/main.go`:

```go
package main

import (
	"log"
	"net"
	"os"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/config"
	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
	"github.com/ratecap/core/store"
)

func main() {
	configPath := os.Getenv("RATECAP_CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/ratecap/ratecap.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	redisAddr := os.Getenv("RATECAP_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	redisStore := store.NewRedisStore(redisClient)

	rateLimiter := limiter.NewTokenBucketLimiter(
		redisStore,
		cfg.Tiers.RateLimiter.DefaultRate,
		cfg.Tiers.RateLimiter.DefaultBurst,
		cfg.Tiers.RateLimiter.ShadowMode,
	)

	concurrencyLimiter := limiter.NewConcurrencyLimiter(
		redisStore,
		cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent,
		cfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs,
		cfg.Tiers.ConcurrencyLimiter.ShadowMode,
	)

	pipeline := limiter.NewPipeline(rateLimiter, concurrencyLimiter)

	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
		concurrencyLimiter.Reconfigure(newCfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent, newCfg.Tiers.ConcurrencyLimiter.MaxRequestDurationMs, newCfg.Tiers.ConcurrencyLimiter.ShadowMode)
	})
	if err != nil {
		log.Fatalf("failed to start config watcher: %v", err)
	}
	defer stopWatch()

	listenAddr := os.Getenv("RATECAP_GRPC_ADDR")
	if listenAddr == "" {
		listenAddr = ":9090"
	}

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", listenAddr, err)
	}

	grpcServer := grpc.NewServer()
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(pipeline, redisStore))

	log.Printf("ratecap-core listening on %s", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("grpc server failed: %v", err)
	}
}
```

- [ ] **Step 6: Build the whole core module to confirm everything compiles together**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go build ./...
```

Expected: builds with no errors.

- [ ] **Step 7: Run the full core module test suite**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/core
GOWORK=off go test ./... -race
```

Expected: PASS — all packages (`config`, `grpcserver`, `limiter`, `store`; `store` requires Docker per Task 2's caveat).

- [ ] **Step 8: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/core/config/ services/core/main.go
git commit -m "feat(core): extend config with concurrency_limiter tier, wire pipeline in main.go"
```

---

## Task 7: Sidecar — token header on /check, new /release endpoint

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify: `services/sidecar/proxy/proxy_test.go`
- Modify: `services/sidecar/main.go`

**Interfaces:**
- Consumes: `ratecapv1.CheckRateLimitResponse.ConcurrencyToken` (Task 1), `ratecapv1.RatecapServiceClient.ReleaseConcurrency` (Task 1).
- Produces:
  ```go
  func NewReleaseHandler(client releaseClient) *ReleaseHandler
  func (h *ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)
  ```
  `Handler.ServeHTTP` (the existing `/check` handler) now sets a `Concurrency-Token` response header when the token is non-empty.

- [ ] **Step 1: Write the failing tests**

Append to `services/sidecar/proxy/proxy_test.go`:

```go
func TestServeHTTP_SetsConcurrencyTokenHeaderWhenPresent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW, ConcurrencyToken: "tok-abc"}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "tok-abc" {
		t.Errorf("expected Concurrency-Token header %q, got %q", "tok-abc", rec.Header().Get("Concurrency-Token"))
	}
}

func TestServeHTTP_OmitsConcurrencyTokenHeaderWhenEmpty(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW, ConcurrencyToken: ""}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "" {
		t.Errorf("expected no Concurrency-Token header, got %q", rec.Header().Get("Concurrency-Token"))
	}
}

type fakeReleaseClient struct {
	lastKey   string
	lastToken string
	err       error
}

func (f *fakeReleaseClient) ReleaseConcurrency(_ context.Context, in *ratecapv1.ReleaseConcurrencyRequest, _ ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error) {
	f.lastKey = in.Key
	f.lastToken = in.ConcurrencyToken
	return &ratecapv1.ReleaseConcurrencyResponse{}, f.err
}

func TestReleaseHandler_ServeHTTP_CallsReleaseConcurrencyWithKeyAndToken(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if client.lastKey != "user-1" {
		t.Errorf("expected ReleaseConcurrency called with key=%q, got %q", "user-1", client.lastKey)
	}
	if client.lastToken != "tok-abc" {
		t.Errorf("expected ReleaseConcurrency called with token=%q, got %q", "tok-abc", client.lastToken)
	}
}

func TestReleaseHandler_ServeHTTP_MissingKeyReturns400(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestReleaseHandler_ServeHTTP_UpstreamErrorReturns500(t *testing.T) {
	client := &fakeReleaseClient{err: errors.New("core unavailable")}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}
```

Add `"errors"` to the `import` block in `services/sidecar/proxy/proxy_test.go` if not already present.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/sidecar
GOWORK=off go test ./proxy/... -v
```

Expected: FAIL — `proxy.NewReleaseHandler` undefined; `fakeRatecapClient`/response literals fail to compile until `ConcurrencyToken` exists (it does, from Task 1).

- [ ] **Step 3: Implement the token header and ReleaseHandler**

Replace the full contents of `services/sidecar/proxy/proxy.go`:

```go
package proxy

import (
	"context"
	"net/http"
	"strconv"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
)

type ratecapClient interface {
	CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error)
}

type Handler struct {
	client          ratecapClient
	defaultPriority Priority
}

func NewHandler(client ratecapClient, defaultPriority Priority) *Handler {
	return &Handler{client: client, defaultPriority: defaultPriority}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	_ = ResolvePriority(r.Header.Get("x-ratecap-priority"), h.defaultPriority)

	resp, err := h.client.CheckRateLimit(r.Context(), &ratecapv1.CheckRateLimitRequest{Key: key, Cost: 1})
	if err != nil {
		http.Error(w, "upstream check failed", http.StatusInternalServerError)
		return
	}

	if resp.ConcurrencyToken != "" {
		w.Header().Set("Concurrency-Token", resp.ConcurrencyToken)
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

type releaseClient interface {
	ReleaseConcurrency(ctx context.Context, in *ratecapv1.ReleaseConcurrencyRequest, opts ...grpc.CallOption) (*ratecapv1.ReleaseConcurrencyResponse, error)
}

type ReleaseHandler struct {
	client releaseClient
}

func NewReleaseHandler(client releaseClient) *ReleaseHandler {
	return &ReleaseHandler{client: client}
}

func (h *ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	token := r.URL.Query().Get("token")

	_, err := h.client.ReleaseConcurrency(r.Context(), &ratecapv1.ReleaseConcurrencyRequest{Key: key, ConcurrencyToken: token})
	if err != nil {
		http.Error(w, "upstream release failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
```

Both `ratecapClient` and `releaseClient` are narrow local interfaces (rather than one combined interface) so `Handler` and `ReleaseHandler` each depend on exactly the one gRPC method they use — the real `ratecapv1.RatecapServiceClient` satisfies both since it has all the methods either interface needs.

- [ ] **Step 4: Run the proxy tests to verify they pass**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/sidecar
GOWORK=off go test ./proxy/... -v
```

Expected: PASS — all tests (5 pre-existing plus 5 new).

- [ ] **Step 5: Register the /release route in sidecar main.go**

Replace the full contents of `services/sidecar/main.go`:

```go
package main

import (
	"log"
	"net/http"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
)

func main() {
	coreAddr := os.Getenv("RATECAP_CORE_ADDR")
	if coreAddr == "" {
		coreAddr = "localhost:9090"
	}

	conn, err := grpc.NewClient(coreAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect to ratecap-core at %s: %v", coreAddr, err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)

	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable))
	mux.Handle("/release", proxy.NewReleaseHandler(client))

	listenAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	log.Printf("ratecap-sidecar listening on %s, forwarding to core at %s", listenAddr, coreAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("sidecar http server failed: %v", err)
	}
}
```

This switches from a bare `http.HandlerFunc(handler.ServeHTTP)` (which only ever served one path) to an `http.ServeMux` registering both `/check` and `/release` — the minimal routing needed now that there are two distinct endpoints.

- [ ] **Step 6: Build the sidecar module to confirm main.go compiles**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/sidecar
GOWORK=off go build ./...
```

Expected: builds with no errors.

- [ ] **Step 7: Run the full sidecar module test suite**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/services/sidecar
GOWORK=off go test ./... -race
```

Expected: PASS — all packages (`proxy`, `shadow`).

- [ ] **Step 8: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add services/sidecar/
git commit -m "feat(sidecar): expose concurrency token on /check, add /release endpoint"
```

---

## Task 8: Go SDK — Acquire()/Ticket

**Files:**
- Modify: `packages/sdks/go/client.go`
- Modify: `packages/sdks/go/client_test.go`

**Interfaces:**
- Consumes: the sidecar's `GET /check` (now returning a `Concurrency-Token` header, Task 7) and `POST /release?key=...&token=...` (Task 7).
- Produces:
  ```go
  type Ticket struct {
      Allowed      bool
      RetryAfterMs int64
  }
  func (t *Ticket) Release(ctx context.Context) error

  func (c *Client) Acquire(ctx context.Context, key string) (*Ticket, error)
  ```
  `Client.Allow` is unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `packages/sdks/go/client_test.go`:

```go
func TestAcquire_ReturnsAllowedTicketOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Concurrency-Token", "tok-abc")
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

func TestAcquire_ReturnsRejectedTicketWithRetryAfterOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ticket.Allowed {
		t.Error("expected Allowed=false on 429 response")
	}
	if ticket.RetryAfterMs != 750 {
		t.Errorf("expected RetryAfterMs=750, got %d", ticket.RetryAfterMs)
	}
}

func TestTicket_Release_CallsReleaseEndpointWithKeyAndToken(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token", "tok-abc")
			w.WriteHeader(http.StatusOK)
		case "/release":
			capturedPath = r.URL.RequestURI()
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

	if capturedPath == "" {
		t.Fatal("expected /release to be called")
	}
}

func TestTicket_Release_NoOpWhenNoTokenWasIssued(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			t.Error("expected /release NOT to be called when no token was issued")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	ticket, err := client.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ticket.Release(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/packages/sdks/go
GOWORK=off go test ./... -v
```

Expected: FAIL — `client.Acquire` undefined.

- [ ] **Step 3: Implement Acquire and Ticket**

Replace the full contents of `packages/sdks/go/client.go`:

```go
package ratecap

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

type Client struct {
	sidecarAddr string
	httpClient  *http.Client
}

func NewClient(sidecarAddr string) *Client {
	return &Client{sidecarAddr: sidecarAddr, httpClient: http.DefaultClient}
}

func (c *Client) Allow(ctx context.Context, key string) (allowed bool, retryAfterMs int64, err error) {
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, 0, err
	}

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

type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client *Client
	key    string
	token  string
}

func (t *Ticket) Release(ctx context.Context) error {
	if t.token == "" {
		return nil
	}

	reqURL := t.client.sidecarAddr + "/release?key=" + url.QueryEscape(t.key) + "&token=" + url.QueryEscape(t.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

func (c *Client) Acquire(ctx context.Context, key string) (*Ticket, error) {
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	token := resp.Header.Get("Concurrency-Token")

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, key: key, token: token}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, key: key, token: token}, nil
}
```

`Release` is a no-op returning `nil` when `token == ""` — this is the case where the pipeline rejected before Tier 2 ever reserved a slot, so there is nothing to release. Per the design spec, `Release` is best-effort with no retry: a failed HTTP call to `/release` returns its error to the caller but does not retry, since the reaper (Task 2) is the actual resilience mechanism for a lost release.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/packages/sdks/go
GOWORK=off go test ./... -v
```

Expected: PASS — all tests (3 pre-existing `TestAllow_*` plus 4 new).

- [ ] **Step 5: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add packages/sdks/go/
git commit -m "feat(sdk-go): add Acquire()/Ticket API for tier 2, Allow() unchanged"
```

---

## Task 9: Demo config + sample app + end-to-end verification

**Files:**
- Modify: `deploy/ratecap.yaml`
- Modify: `deploy/sampleapp/main.go`

**Interfaces:**
- Consumes: `ratecap.Client.Acquire`/`Ticket.Release` (Task 8).

- [ ] **Step 1: Extend the demo config**

Replace the full contents of `deploy/ratecap.yaml`:

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
```

`default_max_concurrent: 3` is deliberately low (like tier 1's `default_burst: 5`) so the demo visibly trips tier 2 with a handful of concurrent requests, without needing a load-testing tool.

- [ ] **Step 2: Add a slow endpoint to the sample app demonstrating tier 2**

Replace the full contents of `deploy/sampleapp/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	ratecap "github.com/ratecap/sdk-go"
)

func main() {
	sidecarAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if sidecarAddr == "" {
		sidecarAddr = "http://localhost:8080"
	}

	client := ratecap.NewClient(sidecarAddr)

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

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}
```

`/slow-report` holds its ticket for 2 seconds (simulating a CPU-intensive endpoint) — with `default_max_concurrent: 3`, firing 4+ concurrent requests at this endpoint demonstrates tier 2 tripping, which a single sequential curl loop (as used for tier 1's `/checkout` check) cannot exercise, since tier 2 bounds concurrency, not rate.

- [ ] **Step 3: Ensure Docker is reachable**

```bash
docker info >/dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

If not reachable, run `open -a Docker` and poll (`sleep 10 && docker info`) for up to 60 seconds before proceeding.

- [ ] **Step 4: Rebuild and bring up the full stack**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/deploy
docker compose build --no-cache
docker compose up -d
sleep 3
```

Expected: all three custom images build and start successfully.

- [ ] **Step 5: Verify tier 1 still works (regression check)**

```bash
for i in 1 2 3 4 5 6 7; do
  curl -s -o /dev/null -w "request $i: %{http_code}\n" http://localhost:3000/checkout
done
```

Expected: requests 1-5 print `200`, requests 6-7 print `429` — identical to the walking skeleton's original behavior, proving tier 1 wasn't broken by this phase's changes.

- [ ] **Step 6: Verify tier 2 trips under concurrent load**

```bash
for i in 1 2 3 4 5; do
  curl -s -o /dev/null -w "concurrent request $i: %{http_code}\n" http://localhost:3000/slow-report &
done
wait
```

Expected: exactly 3 requests print `200` (matching `default_max_concurrent: 3`) and exactly 2 print `429` — proving tier 2's concurrency cap is enforced end-to-end through the full SDK → sidecar → core → Redis chain, with the SDK's `defer ticket.Release(ctx)` correctly freeing slots.

- [ ] **Step 7: Verify slots free up after requests complete**

```bash
sleep 3
curl -s -o /dev/null -w "request after slots freed: %{http_code}\n" http://localhost:3000/slow-report
```

Expected: `200` — proving `Release()` (called via `defer` when each of Step 6's requests completed) actually freed the slots, rather than requests staying stuck until the `max_request_duration_ms` reap window.

- [ ] **Step 8: Tear down**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter/deploy
docker compose down
```

- [ ] **Step 9: Confirm the working tree is clean apart from the intended changes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git status --short
```

Expected: only `deploy/ratecap.yaml` and `deploy/sampleapp/main.go` show as changed.

- [ ] **Step 10: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter
git add deploy/ratecap.yaml deploy/sampleapp/main.go
git commit -m "feat(deploy): demonstrate tier 2 via a slow endpoint, verify end-to-end"
```

---

## Self-Review

**Spec coverage:**
- Wire contract (`concurrency_token` field, `ReleaseConcurrency` RPC) — Task 1 ✓
- StateStore extension (`IncrConcurrent`/`DecrConcurrent`, reap-count-add Lua atomicity, reaping proven with a real test) — Task 2 ✓
- `ConcurrencyLimiter` implementing `Limiter`, same `sync.RWMutex` pattern as `TokenBucketLimiter` — Task 3 ✓
- `Pipeline` composing ordered `[]Limiter`, short-circuit on first reject, single-token-field design (documented, not silently limiting) — Task 4 ✓
- `grpcserver` wiring (`Pipeline` + `ReleaseConcurrency` handler calling the store directly, not through the pipeline) — Task 5 ✓
- Config extension mirroring Tier 1's shape, hot-reload for both tiers — Task 6 ✓
- Sidecar token header + `/release` endpoint — Task 7 ✓
- SDK `Acquire()`/`Ticket.Release()`, `Allow()` untouched, best-effort no-retry release — Task 8 ✓
- Demo config + sample app + end-to-end verification proving both tiers — Task 9 ✓
- Spec's "Out of Scope" (tiers 3/4, priority tagging, release-retry machinery, `Allow()` changes) — correctly untouched by all 9 tasks ✓

**Placeholder scan:** No TBD/TODO/FIXME; every step shows exact file contents or exact commands with expected output.

**Type/interface consistency, verified across tasks:**
- `Decision{Action, RetryAfterMs, Token}` (Task 3) is consumed identically by `Pipeline` (Task 4), `grpcserver.Server.CheckRateLimit` (Task 5), and never mutated elsewhere.
- `StateStore.IncrConcurrent(ctx, key string, cap int, maxDurationMs int64) (bool, string, error)` (Task 2) matches `ConcurrencyLimiter`'s local `concurrencyChecker` interface (Task 3) exactly, and `ConcurrencyLimiter`'s constructor argument order (`store, cap, maxDurationMs, shadowMode`) matches `TokenBucketLimiter`'s established argument order pattern (`store, rate, burst, shadowMode`) for consistency.
- `grpcserver.NewServer(p checker, releaser concurrencyReleaser)` (Task 5) is called in Task 6's `main.go` as `grpcserver.NewServer(pipeline, redisStore)` — `*limiter.Pipeline` satisfies `checker` (single `Check` method) and `*store.RedisStore` satisfies `concurrencyReleaser` (single `DecrConcurrent` method), both verified against Task 2's and Task 4's actual produced signatures.
- Sidecar's `releaseClient` interface (Task 7) and SDK's `/release` URL construction (Task 8) agree on query parameter names (`key`, `token`) and HTTP method (`POST`).
- `ConcurrencyLimiterConfig` field names (Task 6) match what Task 6's own `main.go` wiring code reads (`cfg.Tiers.ConcurrencyLimiter.DefaultMaxConcurrent`, `.MaxRequestDurationMs`, `.ShadowMode`) — no drift between struct definition and usage.
