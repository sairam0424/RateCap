# RateCap Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove RateCap's hybrid architecture (SDK → sidecar → core → Redis) end-to-end using only Tier 1 (Request Rate Limiter), so tiers 2-4 can be added later by reusing proven plumbing instead of guessing at untested seams.

**Architecture:** A Go SDK calls a local `ratecap-sidecar` over gRPC; the sidecar forwards the check to the central `ratecap-core`, which runs the token-bucket algorithm atomically in Redis via a Lua script and returns an allow/reject decision. `ratecap-core` owns `ratecap.yaml` and hot-reloads it on change; the sidecar caches core-supplied config locally.

**Tech Stack:** Go 1.26, gRPC (`google.golang.org/grpc` v1.82.0) + Protocol Buffers (`google.golang.org/protobuf` v1.36.11), `github.com/redis/go-redis/v9` v9.21.0, `github.com/fsnotify/fsnotify` v1.10.1 (config hot-reload), `gopkg.in/yaml.v3` v3.0.1, `github.com/testcontainers/testcontainers-go` v0.43.0 (integration tests), Docker Compose for the demo.

## Global Constraints

- Go module naming follows this workspace's convention: `github.com/ratecap/<service>` (mirrors `github.com/graph-forge/<service>`, `github.com/tombstone/<service>`).
- All Go modules join a single root `go.work` (mirrors Graph-Forge/Tombstone), with intra-repo cross-module deps wired via `require` + `replace ... => ../../<module>` (see `proto/` → `services/*` pattern in Graph-Forge).
- No comments in code unless explaining a non-obvious WHY (hidden constraint, subtle invariant, workaround). Never comment on WHAT the code does.
- Prefer immutability: construct new values rather than mutating in place, except where Go idiom requires mutation (e.g., appending to a pre-sized slice, gRPC server registration).
- Files: 200-400 lines typical, 800 max. Split before a file grows past this.
- Test-first: for every new behavior, write the failing test before the implementation.
- v1 scope is Tier 1 ONLY. Do not implement tiers 2-4, the CLI benchmark runner, the Grafana dashboard, or anything under the spec's "Explicitly Deferred to v2" section. If a step seems to need one of these, stop and flag it rather than building it.
- Response actions must use the 4-value `Action` enum from the spec (`ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`) — do not add a `QUEUE` value.
- Reference spec: `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`

---

## File Structure

```
RateCap/
├── go.work
├── proto/
│   ├── go.mod                          # module github.com/ratecap/proto
│   └── ratecap/v1/
│       └── ratecap.proto
├── services/
│   ├── core/
│   │   ├── go.mod                      # module github.com/ratecap/core
│   │   ├── main.go
│   │   ├── config/
│   │   │   ├── config.go               # Config struct + YAML parsing
│   │   │   ├── config_test.go
│   │   │   ├── watcher.go              # fsnotify hot-reload
│   │   │   └── watcher_test.go
│   │   ├── limiter/
│   │   │   ├── limiter.go              # Limiter interface, Decision, Action
│   │   │   ├── tokenbucket.go          # Tier 1 implementation
│   │   │   └── tokenbucket_test.go     # pure unit tests, no Redis
│   │   ├── store/
│   │   │   ├── store.go                # StateStore interface
│   │   │   ├── redis.go                # Redis-backed StateStore impl
│   │   │   ├── redis_test.go           # testcontainers integration test
│   │   │   └── lua/
│   │   │       └── token_bucket.lua
│   │   └── grpcserver/
│   │       ├── server.go               # RatecapServer implementing proto service
│   │       └── server_test.go
│   └── sidecar/
│       ├── go.mod                      # module github.com/ratecap/sidecar
│       ├── main.go
│       ├── proxy/
│       │   ├── proxy.go                # local HTTP entrypoint -> core gRPC client
│       │   ├── proxy_test.go
│       │   ├── priority.go             # x-ratecap-priority header parsing
│       │   └── priority_test.go
│       └── shadow/
│           ├── shadow.go                # shadow-mode decision coercion
│           └── shadow_test.go
├── packages/
│   └── sdks/
│       └── go/
│           ├── go.mod                  # module github.com/ratecap/sdk-go
│           ├── client.go               # thin wrapper over generated gRPC stub
│           └── client_test.go
├── deploy/
│   ├── docker-compose.yml
│   ├── ratecap.yaml                    # sample config for the demo
│   └── sampleapp/
│       ├── go.mod                      # module github.com/ratecap/sampleapp
│       ├── main.go
│       └── Dockerfile
├── services/core/Dockerfile
└── services/sidecar/Dockerfile
```

---

## Task 1: Define the Tier-1 gRPC contract

**Files:**
- Create: `proto/go.mod`
- Create: `proto/ratecap/v1/ratecap.proto`

**Interfaces:**
- Produces: `RatecapService` gRPC service with one RPC — `CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse)`. `CheckRateLimitRequest{key, cost}`, `CheckRateLimitResponse{action, retry_after_ms}` where `action` is an enum `ALLOW=0, REJECT_429=1, REJECT_503=2, SHADOW_LOG=3`.

- [ ] **Step 1: Write the proto file**

```protobuf
syntax = "proto3";

package ratecap.v1;

option go_package = "github.com/ratecap/proto/ratecap/v1;ratecapv1";

service RatecapService {
  rpc CheckRateLimit(CheckRateLimitRequest) returns (CheckRateLimitResponse);
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
}
```

Save to `proto/ratecap/v1/ratecap.proto`.

- [ ] **Step 2: Initialize the proto Go module**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
mkdir -p proto/ratecap/v1
go mod init github.com/ratecap/proto -C proto 2>/dev/null || (cd proto && go mod init github.com/ratecap/proto)
```

Expected: `proto/go.mod` created with `module github.com/ratecap/proto` and a `go 1.26` directive.

- [ ] **Step 3: Generate Go code from the proto file**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
protoc \
  --go_out=. --go_opt=module=github.com/ratecap/proto \
  --go-grpc_out=. --go-grpc_opt=module=github.com/ratecap/proto \
  proto/ratecap/v1/ratecap.proto
```

Expected: creates `proto/ratecap/v1/ratecap.pb.go` and `proto/ratecap/v1/ratecap_grpc.pb.go`.

- [ ] **Step 4: Add protobuf/grpc runtime deps and verify it builds**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/proto
go get google.golang.org/protobuf@v1.36.11
go get google.golang.org/grpc@v1.82.0
go build ./...
```

Expected: builds with no errors.

- [ ] **Step 5: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add proto/
git commit -m "feat(proto): define RatecapService gRPC contract for tier 1"
```

---

## Task 2: Root go.work

**Files:**
- Create: `go.work`

**Interfaces:**
- Consumes: `proto/go.mod` (Task 1)
- Produces: a workspace file later tasks add their module path to.

- [ ] **Step 1: Create go.work**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
go work init ./proto
cat go.work
```

Expected output:
```
go 1.26.2

use ./proto
```

- [ ] **Step 2: Commit**

```bash
git add go.work
git commit -m "chore: initialize go.work workspace"
```

(Later tasks run `go work use ./services/core` etc. — do not hand-add all paths now, since those modules don't exist yet and `go work use` on a nonexistent dir fails.)

---

## Task 3: StateStore interface + Redis implementation with Lua token bucket

**Files:**
- Create: `services/core/go.mod`
- Create: `services/core/store/store.go`
- Create: `services/core/store/redis.go`
- Create: `services/core/store/lua/token_bucket.lua`
- Test: `services/core/store/redis_test.go`

**Interfaces:**
- Produces:
  ```go
  package store

  type StateStore interface {
      CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
  }

  func NewRedisStore(client *redis.Client) *RedisStore
  ```
  `RedisStore` implements `StateStore`.

- [ ] **Step 1: Initialize the core module and add go-redis**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
mkdir -p services/core/store/lua
cd services/core
go mod init github.com/ratecap/core
go get github.com/redis/go-redis/v9@v9.21.0
```

- [ ] **Step 2: Write the StateStore interface**

```go
// services/core/store/store.go
package store

import "context"

type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, err error)
}
```

- [ ] **Step 3: Write the failing integration test**

```go
// services/core/store/redis_test.go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ratecap/core/store"
)

func startRedis(t *testing.T) *redis.Client {
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
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

func TestCheckAndDecrement_AllowsWithinBurst(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		allowed, _, err := s.CheckAndDecrement(ctx, "test-key-burst", 10, 5, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed within burst of 5", i)
		}
	}
}

func TestCheckAndDecrement_RejectsOverBurst(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, _, err := s.CheckAndDecrement(ctx, "test-key-reject", 10, 5, 1); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	allowed, retryAfterMs, err := s.CheckAndDecrement(ctx, "test-key-reject", 10, 5, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatalf("6th request should be rejected, burst is 5")
	}
	if retryAfterMs <= 0 {
		t.Fatalf("expected positive retryAfterMs, got %d", retryAfterMs)
	}
}

func TestCheckAndDecrement_ConcurrentAtomicity(t *testing.T) {
	client := startRedis(t)
	s := store.NewRedisStore(client)
	ctx := context.Background()

	const attempts = 50
	const burst = 10
	results := make(chan bool, attempts)

	for i := 0; i < attempts; i++ {
		go func() {
			allowed, _, err := s.CheckAndDecrement(ctx, "test-key-concurrent", 1, burst, 1)
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

	if allowedCount != burst {
		t.Fatalf("expected exactly %d allowed under concurrent load, got %d", burst, allowedCount)
	}

	_ = time.Now()
}
```

- [ ] **Step 4: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go get github.com/testcontainers/testcontainers-go@v0.43.0
go test ./store/... -run TestCheckAndDecrement -v
```

Expected: FAIL — `store.NewRedisStore` undefined (redis.go doesn't exist yet).

- [ ] **Step 5: Write the Lua script**

```lua
-- services/core/store/lua/token_bucket.lua
-- KEYS[1] = bucket key
-- ARGV[1] = rate (tokens per second)
-- ARGV[2] = burst (max bucket capacity)
-- ARGV[3] = cost (tokens requested)
-- ARGV[4] = now (unix millis)
--
-- Returns {allowed (1/0), retry_after_ms}

local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])

local bucket = redis.call("HMGET", key, "tokens", "updated_at")
local tokens = tonumber(bucket[1])
local updated_at = tonumber(bucket[2])

if tokens == nil then
  tokens = burst
  updated_at = now
end

local elapsed_ms = math.max(0, now - updated_at)
local refill = (elapsed_ms / 1000) * rate
tokens = math.min(burst, tokens + refill)

if tokens >= cost then
  tokens = tokens - cost
  redis.call("HSET", key, "tokens", tokens, "updated_at", now)
  redis.call("EXPIRE", key, math.ceil(burst / rate) + 60)
  return {1, 0}
else
  local deficit = cost - tokens
  local retry_after_ms = math.ceil((deficit / rate) * 1000)
  redis.call("HSET", key, "tokens", tokens, "updated_at", now)
  redis.call("EXPIRE", key, math.ceil(burst / rate) + 60)
  return {0, retry_after_ms}
end
```

This mirrors Stripe's confirmed approach: atomic check-and-decrement in a single Redis round-trip via Lua, avoiding the race condition a read-then-write from the client would have.

- [ ] **Step 6: Implement RedisStore**

```go
// services/core/store/redis.go
package store

import (
	"context"
	_ "embed"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

type RedisStore struct {
	client *redis.Client
	script *redis.Script
}

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{
		client: client,
		script: redis.NewScript(tokenBucketScript),
	}
}

func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	now := time.Now().UnixMilli()
	result, err := s.script.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	if err != nil {
		return false, 0, err
	}

	allowed := result[0].(int64) == 1
	retryAfterMs := result[1].(int64)
	return allowed, retryAfterMs, nil
}
```

- [ ] **Step 7: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./store/... -run TestCheckAndDecrement -v
```

Expected: PASS — all 3 tests (requires Docker running locally for testcontainers).

- [ ] **Step 8: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/core/go.mod services/core/go.sum services/core/store/
git commit -m "feat(core): add Redis-backed StateStore with atomic Lua token bucket"
```

---

## Task 4: Tier-1 Limiter (pure decision logic, no Redis)

**Files:**
- Create: `services/core/limiter/limiter.go`
- Create: `services/core/limiter/tokenbucket.go`
- Test: `services/core/limiter/tokenbucket_test.go`

**Interfaces:**
- Consumes: `store.StateStore` (Task 3) — specifically `CheckAndDecrement(ctx, key string, rate, burst, cost int) (bool, int64, error)`.
- Produces:
  ```go
  package limiter

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
  }

  type Request struct {
      Key  string
      Cost int
  }

  type Limiter interface {
      Check(ctx context.Context, req Request) (Decision, error)
  }

  func NewTokenBucketLimiter(s store.StateStore, rate, burst int, shadowMode bool) *TokenBucketLimiter
  ```
  `TokenBucketLimiter` implements `Limiter`.

- [ ] **Step 1: Write the Limiter interface and shared types**

```go
// services/core/limiter/limiter.go
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
}

type Request struct {
	Key  string
	Cost int
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
```

- [ ] **Step 2: Write the failing unit test**

```go
// services/core/limiter/tokenbucket_test.go
package limiter_test

import (
	"context"
	"testing"

	"github.com/ratecap/core/limiter"
)

type fakeStore struct {
	tokens map[string]int
	burst  int
}

func newFakeStore(burst int) *fakeStore {
	return &fakeStore{tokens: make(map[string]int), burst: burst}
}

func (f *fakeStore) CheckAndDecrement(_ context.Context, key string, _, burst, cost int) (bool, int64, error) {
	remaining, ok := f.tokens[key]
	if !ok {
		remaining = burst
	}
	if remaining >= cost {
		f.tokens[key] = remaining - cost
		return true, 0, nil
	}
	return false, 100, nil
}

func TestTokenBucketLimiter_AllowsExactlyBurstRequests(t *testing.T) {
	fs := newFakeStore(5)
	l := limiter.NewTokenBucketLimiter(fs, 10, 5, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := l.Check(ctx, limiter.Request{Key: "user-1", Cost: 1})
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		if d.Action != limiter.ALLOW {
			t.Fatalf("request %d: expected ALLOW, got %v", i, d.Action)
		}
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("6th request: expected REJECT_429, got %v", d.Action)
	}
	if d.RetryAfterMs != 100 {
		t.Fatalf("expected RetryAfterMs=100, got %d", d.RetryAfterMs)
	}
}

func TestTokenBucketLimiter_ShadowModeAlwaysAllows(t *testing.T) {
	fs := newFakeStore(1)
	l := limiter.NewTokenBucketLimiter(fs, 10, 1, true)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-2", Cost: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-2", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG when over limit in shadow mode, got %v", d.Action)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./limiter/... -v
```

Expected: FAIL — `limiter.NewTokenBucketLimiter` undefined.

- [ ] **Step 4: Implement TokenBucketLimiter**

```go
// services/core/limiter/tokenbucket.go
package limiter

import "context"

type checker interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error)
}

type TokenBucketLimiter struct {
	store      checker
	rate       int
	burst      int
	shadowMode bool
}

func NewTokenBucketLimiter(s checker, rate, burst int, shadowMode bool) *TokenBucketLimiter {
	return &TokenBucketLimiter{store: s, rate: rate, burst: burst, shadowMode: shadowMode}
}

func (l *TokenBucketLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	allowed, retryAfterMs, err := l.store.CheckAndDecrement(ctx, req.Key, l.rate, l.burst, req.Cost)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW}, nil
	}

	if l.shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs}, nil
	}

	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs}, nil
}
```

`checker` is a narrow local interface (not `store.StateStore` directly) so this file has no import on the `store` package — keeping the limiter package testable with a fake without pulling in Redis at all, and avoiding a Task-3→Task-4 circular-looking dependency in the test file.

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./limiter/... -v
```

Expected: PASS — both tests.

- [ ] **Step 6: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/core/limiter/
git commit -m "feat(core): add tier-1 token bucket limiter with shadow-mode support"
```

---

## Task 5: Config loading + hot-reload

**Files:**
- Create: `services/core/config/config.go`
- Test: `services/core/config/config_test.go`
- Create: `services/core/config/watcher.go`
- Test: `services/core/config/watcher_test.go`

**Interfaces:**
- Produces:
  ```go
  package config

  type RateLimiterConfig struct {
      DefaultRate  int  `yaml:"default_rate"`
      DefaultBurst int  `yaml:"default_burst"`
      ShadowMode   bool `yaml:"shadow_mode"`
  }

  type Config struct {
      SyncRate int `yaml:"sync_rate"`
      Tiers    struct {
          RateLimiter RateLimiterConfig `yaml:"rate_limiter"`
      } `yaml:"tiers"`
  }

  func Load(path string) (*Config, error)
  func Watch(path string, onChange func(*Config)) (stop func(), err error)
  ```

- [ ] **Step 1: Write the failing config-loading test**

```go
// services/core/config/config_test.go
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ratecap/core/config"
)

func writeTempConfig(t *testing.T, contents string) string {
	dir := t.TempDir()
	path := filepath.Join(dir, "ratecap.yaml")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestLoad_ParsesRateLimiterTier(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.SyncRate != 5 {
		t.Errorf("expected SyncRate=5, got %d", cfg.SyncRate)
	}
	if cfg.Tiers.RateLimiter.DefaultRate != 100 {
		t.Errorf("expected DefaultRate=100, got %d", cfg.Tiers.RateLimiter.DefaultRate)
	}
	if cfg.Tiers.RateLimiter.DefaultBurst != 500 {
		t.Errorf("expected DefaultBurst=500, got %d", cfg.Tiers.RateLimiter.DefaultBurst)
	}
	if cfg.Tiers.RateLimiter.ShadowMode != false {
		t.Errorf("expected ShadowMode=false, got %v", cfg.Tiers.RateLimiter.ShadowMode)
	}
}

func TestLoad_MissingFileReturnsError(t *testing.T) {
	_, err := config.Load("/nonexistent/ratecap.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go get gopkg.in/yaml.v3@v3.0.1
go test ./config/... -run TestLoad -v
```

Expected: FAIL — `config.Load` undefined.

- [ ] **Step 3: Implement config.go**

```go
// services/core/config/config.go
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

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter RateLimiterConfig `yaml:"rate_limiter"`
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

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./config/... -run TestLoad -v
```

Expected: PASS.

- [ ] **Step 5: Write the failing watcher test**

```go
// services/core/config/watcher_test.go
package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/ratecap/core/config"
)

func TestWatch_TriggersOnChangeOnFileWrite(t *testing.T) {
	path := writeTempConfig(t, `
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
`)

	changes := make(chan *config.Config, 1)
	stop, err := config.Watch(path, func(cfg *config.Config) {
		changes <- cfg
	})
	if err != nil {
		t.Fatalf("unexpected error starting watch: %v", err)
	}
	defer stop()

	time.Sleep(100 * time.Millisecond)

	newContents := `
sync_rate: 10
tiers:
  rate_limiter:
    default_rate: 200
    default_burst: 1000
    shadow_mode: true
`
	if err := os.WriteFile(path, []byte(newContents), 0644); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	select {
	case cfg := <-changes:
		if cfg.Tiers.RateLimiter.DefaultRate != 200 {
			t.Errorf("expected reloaded DefaultRate=200, got %d", cfg.Tiers.RateLimiter.DefaultRate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config reload callback")
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go get github.com/fsnotify/fsnotify@v1.10.1
go test ./config/... -run TestWatch -v
```

Expected: FAIL — `config.Watch` undefined.

- [ ] **Step 7: Implement watcher.go**

```go
// services/core/config/watcher.go
package config

import (
	"github.com/fsnotify/fsnotify"
)

func Watch(path string, onChange func(*Config)) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := watcher.Add(path); err != nil {
		_ = watcher.Close()
		return nil, err
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					cfg, err := Load(path)
					if err == nil {
						onChange(cfg)
					}
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
				return
			}
		}
	}()

	stop = func() {
		close(done)
		_ = watcher.Close()
	}
	return stop, nil
}
```

- [ ] **Step 8: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./config/... -v
```

Expected: PASS — all config tests.

- [ ] **Step 9: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/core/config/
git commit -m "feat(core): add config loading and fsnotify-based hot-reload"
```

---

## Task 6: gRPC server wiring core together

**Files:**
- Create: `services/core/grpcserver/server.go`
- Test: `services/core/grpcserver/server_test.go`
- Create: `services/core/main.go`
- Modify: `services/core/go.mod` (add proto dependency + replace directive)
- Modify: `go.work` (add `./services/core`)

**Interfaces:**
- Consumes: `limiter.Limiter` (Task 4), `ratecapv1.RatecapServiceServer` (generated from Task 1's proto), `config.Config` (Task 5).
- Produces:
  ```go
  package grpcserver

  func NewServer(l limiter.Limiter) *Server  // Server implements ratecapv1.RatecapServiceServer
  ```

- [ ] **Step 1: Wire the proto module into core's go.mod**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go mod edit -require github.com/ratecap/proto@v0.0.0-00010101000000-000000000000
go mod edit -replace github.com/ratecap/proto=../../proto
go mod tidy
```

- [ ] **Step 2: Write the failing server test**

```go
// services/core/grpcserver/server_test.go
package grpcserver_test

import (
	"context"
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

func TestCheckRateLimit_ReturnsAllowDecision(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(fl)

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
	s := grpcserver.NewServer(fl)

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
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./grpcserver/... -v
```

Expected: FAIL — `grpcserver.NewServer` undefined.

- [ ] **Step 4: Implement server.go**

```go
// services/core/grpcserver/server.go
package grpcserver

import (
	"context"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/limiter"
)

type Server struct {
	ratecapv1.UnimplementedRatecapServiceServer
	limiter limiter.Limiter
}

func NewServer(l limiter.Limiter) *Server {
	return &Server{limiter: l}
}

func (s *Server) CheckRateLimit(ctx context.Context, req *ratecapv1.CheckRateLimitRequest) (*ratecapv1.CheckRateLimitResponse, error) {
	decision, err := s.limiter.Check(ctx, limiter.Request{Key: req.Key, Cost: int(req.Cost)})
	if err != nil {
		return nil, err
	}

	return &ratecapv1.CheckRateLimitResponse{
		Action:       toProtoAction(decision.Action),
		RetryAfterMs: decision.RetryAfterMs,
	}, nil
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

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./grpcserver/... -v
```

Expected: PASS.

- [ ] **Step 6: Write main.go wiring everything together**

```go
// services/core/main.go
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

	stopWatch, err := config.Watch(configPath, func(newCfg *config.Config) {
		rateLimiter.Reconfigure(newCfg.Tiers.RateLimiter.DefaultRate, newCfg.Tiers.RateLimiter.DefaultBurst, newCfg.Tiers.RateLimiter.ShadowMode)
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
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(rateLimiter))

	log.Printf("ratecap-core listening on %s", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("grpc server failed: %v", err)
	}
}
```

This calls `rateLimiter.Reconfigure(...)`, which doesn't exist yet on `TokenBucketLimiter` — add it now since main.go's hot-reload wiring needs it.

- [ ] **Step 7: Add Reconfigure to TokenBucketLimiter and a test for it**

Add to `services/core/limiter/tokenbucket_test.go`:

```go
func TestTokenBucketLimiter_ReconfigureChangesLimits(t *testing.T) {
	fs := newFakeStore(1)
	l := limiter.NewTokenBucketLimiter(fs, 10, 1, false)
	ctx := context.Background()

	if _, err := l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429 before reconfigure, got %v", d.Action)
	}

	l.Reconfigure(10, 1, true)

	d, err = l.Check(ctx, limiter.Request{Key: "user-3", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.SHADOW_LOG {
		t.Fatalf("expected SHADOW_LOG after enabling shadow mode via reconfigure, got %v", d.Action)
	}
}
```

Add to `services/core/limiter/tokenbucket.go`:

```go
func (l *TokenBucketLimiter) Reconfigure(rate, burst int, shadowMode bool) {
	l.rate = rate
	l.burst = burst
	l.shadowMode = shadowMode
}
```

`TokenBucketLimiter`'s fields (`rate`, `burst`, `shadowMode`) are mutated here rather than replaced wholesale — this is the one deliberate exception to the immutability preference in this codebase, because the limiter is a long-lived singleton shared across concurrent gRPC handlers and swapping it out atomically on every config change would require a mutex-guarded pointer indirection for no behavioral benefit at v1's scale.

- [ ] **Step 8: Run all core tests to verify everything passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go test ./... -v
```

Expected: PASS — all tests across config, limiter, store, grpcserver.

- [ ] **Step 9: Build main.go to verify it compiles**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/core
go build ./...
```

Expected: builds with no errors.

- [ ] **Step 10: Add core to go.work**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
go work use ./services/core
```

- [ ] **Step 11: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/core/ go.work go.work.sum
git commit -m "feat(core): wire gRPC server, config hot-reload, and main entrypoint"
```

---

## Task 7: Sidecar — priority header parsing

**Files:**
- Create: `services/sidecar/go.mod`
- Create: `services/sidecar/proxy/priority.go`
- Test: `services/sidecar/proxy/priority_test.go`

**Interfaces:**
- Produces:
  ```go
  package proxy

  type Priority int
  const (
      Sheddable Priority = iota
      Critical
  )

  func ResolvePriority(headerValue string, defaultPriority Priority) Priority
  ```

- [ ] **Step 1: Initialize the sidecar module**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
mkdir -p services/sidecar/proxy
cd services/sidecar
go mod init github.com/ratecap/sidecar
```

- [ ] **Step 2: Write the failing test**

```go
// services/sidecar/proxy/priority_test.go
package proxy_test

import (
	"testing"

	"github.com/ratecap/sidecar/proxy"
)

func TestResolvePriority_HeaderCriticalOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("critical", proxy.Sheddable)
	if got != proxy.Critical {
		t.Errorf("expected Critical, got %v", got)
	}
}

func TestResolvePriority_HeaderSheddableOverridesDefault(t *testing.T) {
	got := proxy.ResolvePriority("sheddable", proxy.Critical)
	if got != proxy.Sheddable {
		t.Errorf("expected Sheddable, got %v", got)
	}
}

func TestResolvePriority_EmptyHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("", proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical, got %v", got)
	}
}

func TestResolvePriority_InvalidHeaderFallsBackToDefault(t *testing.T) {
	got := proxy.ResolvePriority("not-a-real-priority", proxy.Critical)
	if got != proxy.Critical {
		t.Errorf("expected fallback to default Critical for invalid header, got %v", got)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go test ./proxy/... -v
```

Expected: FAIL — `proxy.ResolvePriority` undefined.

- [ ] **Step 4: Implement priority.go**

```go
// services/sidecar/proxy/priority.go
package proxy

type Priority int

const (
	Sheddable Priority = iota
	Critical
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

Tier 1 doesn't consume `Priority` yet — this is scaffolding the spec's priority-tagging resolution order (header → route config → default) ahead of tier 3, which is where it becomes load-bearing. Keeping it in its own file/test now means tier 3's implementation only has to wire it in, not design it.

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go test ./proxy/... -v
```

Expected: PASS — all 4 tests.

- [ ] **Step 6: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/sidecar/go.mod services/sidecar/proxy/priority.go services/sidecar/proxy/priority_test.go
git commit -m "feat(sidecar): add priority header resolution (header -> default)"
```

---

## Task 8: Sidecar — shadow-mode env override + HTTP proxy to core

**Files:**
- Create: `services/sidecar/shadow/shadow.go`
- Test: `services/sidecar/shadow/shadow_test.go`
- Create: `services/sidecar/proxy/proxy.go`
- Test: `services/sidecar/proxy/proxy_test.go`
- Create: `services/sidecar/main.go`
- Modify: `services/sidecar/go.mod`
- Modify: `go.work`

**Interfaces:**
- Consumes: `ratecapv1.RatecapServiceClient` (generated gRPC client from Task 1's proto).
- Produces:
  ```go
  package shadow

  func GlobalOverrideEnabled() bool  // reads RATECAP_SHADOW_MODE env var
  func CoerceIfShadowOverridden(action ratecapv1.Action, override bool) ratecapv1.Action

  package proxy

  type ratecapClient interface {
      CheckRateLimit(ctx context.Context, in *ratecapv1.CheckRateLimitRequest, opts ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error)
  }
  type Handler struct { ... }
  func NewHandler(client ratecapClient, defaultPriority Priority) *Handler
  func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)
  ```
  `ratecapClient` is a narrow local interface matching the generated `ratecapv1.RatecapServiceClient`'s method set exactly, so both the real gRPC client (Task 6's proto codegen) and a test double satisfy it without an adapter.

- [ ] **Step 1: Write the failing shadow-mode test**

```go
// services/sidecar/shadow/shadow_test.go
package shadow_test

import (
	"os"
	"testing"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/shadow"
)

func TestGlobalOverrideEnabled_TrueWhenEnvSet(t *testing.T) {
	os.Setenv("RATECAP_SHADOW_MODE", "true")
	defer os.Unsetenv("RATECAP_SHADOW_MODE")

	if !shadow.GlobalOverrideEnabled() {
		t.Error("expected GlobalOverrideEnabled to be true when RATECAP_SHADOW_MODE=true")
	}
}

func TestGlobalOverrideEnabled_FalseWhenEnvUnset(t *testing.T) {
	os.Unsetenv("RATECAP_SHADOW_MODE")

	if shadow.GlobalOverrideEnabled() {
		t.Error("expected GlobalOverrideEnabled to be false when RATECAP_SHADOW_MODE is unset")
	}
}

func TestCoerceIfShadowOverridden_CoercesRejectToShadowLog(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_REJECT_429, true)
	if got != ratecapv1.Action_SHADOW_LOG {
		t.Errorf("expected SHADOW_LOG, got %v", got)
	}
}

func TestCoerceIfShadowOverridden_PassesThroughWhenOverrideDisabled(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_REJECT_429, false)
	if got != ratecapv1.Action_REJECT_429 {
		t.Errorf("expected REJECT_429 unchanged, got %v", got)
	}
}

func TestCoerceIfShadowOverridden_AllowPassesThroughRegardless(t *testing.T) {
	got := shadow.CoerceIfShadowOverridden(ratecapv1.Action_ALLOW, true)
	if got != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW unchanged even in override, got %v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go mod edit -require github.com/ratecap/proto@v0.0.0-00010101000000-000000000000
go mod edit -replace github.com/ratecap/proto=../../proto
go mod tidy
go test ./shadow/... -v
```

Expected: FAIL — `shadow.GlobalOverrideEnabled` undefined.

- [ ] **Step 3: Implement shadow.go**

```go
// services/sidecar/shadow/shadow.go
package shadow

import "os"

import ratecapv1 "github.com/ratecap/proto/ratecap/v1"

func GlobalOverrideEnabled() bool {
	return os.Getenv("RATECAP_SHADOW_MODE") == "true"
}

func CoerceIfShadowOverridden(action ratecapv1.Action, override bool) ratecapv1.Action {
	if !override {
		return action
	}
	if action == ratecapv1.Action_REJECT_429 || action == ratecapv1.Action_REJECT_503 {
		return ratecapv1.Action_SHADOW_LOG
	}
	return action
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go test ./shadow/... -v
```

Expected: PASS — all 5 tests.

- [ ] **Step 5: Write the failing proxy HTTP handler test**

```go
// services/sidecar/proxy/proxy_test.go
package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/proxy"
)

type fakeRatecapClient struct {
	resp *ratecapv1.CheckRateLimitResponse
	err  error
}

func (f *fakeRatecapClient) CheckRateLimit(_ context.Context, _ *ratecapv1.CheckRateLimitRequest, _ ...grpc.CallOption) (*ratecapv1.CheckRateLimitResponse, error) {
	return f.resp, f.err
}

func TestServeHTTP_AllowReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestServeHTTP_Reject429Returns429(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_REJECT_429, RetryAfterMs: 500}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After-Ms") != "500" {
		t.Errorf("expected Retry-After-Ms header of 500, got %q", rec.Header().Get("Retry-After-Ms"))
	}
}

func TestServeHTTP_ShadowLogReturns200(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_SHADOW_LOG}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 in shadow mode, got %d", rec.Code)
	}
}
```

Note: this test's `fakeRatecapClient` implements the same method set as the generated `ratecapv1.RatecapServiceClient` interface (from `ratecap_grpc.pb.go`, generated in Task 1) — `CheckRateLimit(ctx context.Context, in *CheckRateLimitRequest, opts ...grpc.CallOption) (*CheckRateLimitResponse, error)` — so it satisfies `Handler`'s `ratecapClient` interface parameter without an adapter.

- [ ] **Step 6: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go get google.golang.org/grpc@v1.82.0
go test ./proxy/... -run TestServeHTTP -v
```

Expected: FAIL — `proxy.NewHandler` undefined.

- [ ] **Step 7: Implement proxy.go**

```go
// services/sidecar/proxy/proxy.go
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

Priority is resolved (`ResolvePriority` call) but intentionally discarded (`_ =`) here — tier 1 doesn't use priority at all per the spec (only tier 3 does), but the resolution call is exercised end-to-end now so the wiring is proven before tier 3 needs to actually branch on it.

- [ ] **Step 8: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go test ./proxy/... -v
```

Expected: PASS — all tests in the proxy package.

- [ ] **Step 9: Write main.go for the sidecar**

```go
// services/sidecar/main.go
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
	handler := proxy.NewHandler(client, proxy.Sheddable)

	listenAddr := os.Getenv("RATECAP_SIDECAR_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	log.Printf("ratecap-sidecar listening on %s, forwarding to core at %s", listenAddr, coreAddr)
	if err := http.ListenAndServe(listenAddr, http.HandlerFunc(handler.ServeHTTP)); err != nil {
		log.Fatalf("sidecar http server failed: %v", err)
	}
}
```

`ratecapv1.NewRatecapServiceClient(conn)` returns the generated `ratecapv1.RatecapServiceClient` interface, whose `CheckRateLimit` method signature (`ctx context.Context, in *CheckRateLimitRequest, opts ...grpc.CallOption) (*CheckRateLimitResponse, error)`) matches `Handler`'s `ratecapClient` parameter exactly (defined in Step 7 above), so this satisfies the interface with no adapter needed.

- [ ] **Step 10: Run all sidecar tests and build main.go**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/services/sidecar
go test ./... -v
go build ./...
```

Expected: PASS on tests, clean build.

- [ ] **Step 11: Add sidecar to go.work**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
go work use ./services/sidecar
```

- [ ] **Step 12: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/sidecar/ go.work go.work.sum
git commit -m "feat(sidecar): add HTTP proxy handler with shadow-mode override and gRPC forwarding"
```

---

## Task 9: Go SDK

**Files:**
- Create: `packages/sdks/go/go.mod`
- Create: `packages/sdks/go/client.go`
- Test: `packages/sdks/go/client_test.go`

**Interfaces:**
- Consumes: the sidecar's HTTP `/check?key=...` endpoint (Task 8).
- Produces:
  ```go
  package ratecap

  type Client struct { ... }
  func NewClient(sidecarAddr string) *Client
  func (c *Client) Allow(ctx context.Context, key string) (allowed bool, retryAfterMs int64, err error)
  ```

- [ ] **Step 1: Initialize the SDK module**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
mkdir -p packages/sdks/go
cd packages/sdks/go
go mod init github.com/ratecap/sdk-go
```

- [ ] **Step 2: Write the failing test**

```go
// packages/sdks/go/client_test.go
package ratecap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ratecap "github.com/ratecap/sdk-go"
)

func TestAllow_ReturnsTrueOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true on 200 response")
	}
}

func TestAllow_ReturnsFalseWithRetryAfterOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "750")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, retryAfterMs, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false on 429 response")
	}
	if retryAfterMs != 750 {
		t.Errorf("expected retryAfterMs=750, got %d", retryAfterMs)
	}
}

func TestAllow_ReturnsFalseOn503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := ratecap.NewClient(server.URL)
	allowed, _, err := client.Allow(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false on 503 response")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/packages/sdks/go
go test ./... -v
```

Expected: FAIL — `ratecap.NewClient` undefined.

- [ ] **Step 4: Implement client.go**

```go
// packages/sdks/go/client.go
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
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/packages/sdks/go
go test ./... -v
```

Expected: PASS — all 3 tests.

- [ ] **Step 6: Add SDK to go.work and commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
go work use ./packages/sdks/go
git add packages/ go.work go.work.sum
git commit -m "feat(sdk-go): add thin HTTP client SDK wrapping the sidecar endpoint"
```

---

## Task 10: Docker Compose demo (core + sidecar + Redis + sample app)

**Files:**
- Create: `services/core/Dockerfile`
- Create: `services/sidecar/Dockerfile`
- Create: `deploy/sampleapp/go.mod`
- Create: `deploy/sampleapp/main.go`
- Create: `deploy/sampleapp/Dockerfile`
- Create: `deploy/ratecap.yaml`
- Create: `deploy/docker-compose.yml`

**Interfaces:**
- Consumes: `ratecap.NewClient` (Task 9), `services/core` and `services/sidecar` binaries (Tasks 6, 8).

- [ ] **Step 1: Write the core Dockerfile**

```dockerfile
# services/core/Dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY proto/ proto/
COPY services/core/ services/core/
WORKDIR /src/services/core
RUN go build -o /ratecap-core .

FROM alpine:3.20
COPY --from=build /ratecap-core /usr/local/bin/ratecap-core
ENTRYPOINT ["/usr/local/bin/ratecap-core"]
```

- [ ] **Step 2: Write the sidecar Dockerfile**

```dockerfile
# services/sidecar/Dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY proto/ proto/
COPY services/sidecar/ services/sidecar/
WORKDIR /src/services/sidecar
RUN go build -o /ratecap-sidecar .

FROM alpine:3.20
COPY --from=build /ratecap-sidecar /usr/local/bin/ratecap-sidecar
ENTRYPOINT ["/usr/local/bin/ratecap-sidecar"]
```

- [ ] **Step 3: Write the sample app**

```go
// deploy/sampleapp/main.go
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

	log.Println("sample app listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}
```

```bash
mkdir -p /Users/sairamugge/Desktop/Not-Humans-World/RateCap/deploy/sampleapp
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/deploy/sampleapp
go mod init github.com/ratecap/sampleapp
go mod edit -require github.com/ratecap/sdk-go@v0.0.0-00010101000000-000000000000
go mod edit -replace github.com/ratecap/sdk-go=../../packages/sdks/go
go mod tidy
```

- [ ] **Step 4: Write the sample app Dockerfile**

```dockerfile
# deploy/sampleapp/Dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages/ packages/
COPY deploy/sampleapp/ deploy/sampleapp/
WORKDIR /src/deploy/sampleapp
RUN go build -o /sampleapp .

FROM alpine:3.20
COPY --from=build /sampleapp /usr/local/bin/sampleapp
ENTRYPOINT ["/usr/local/bin/sampleapp"]
```

- [ ] **Step 5: Write the demo config**

```yaml
# deploy/ratecap.yaml
sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 2
    default_burst: 5
    shadow_mode: false
```

A deliberately low rate (2/sec, burst 5) so the demo visibly hits 429s within a handful of curl requests, rather than requiring a load-testing tool to observe the limiter working.

- [ ] **Step 6: Write docker-compose.yml**

```yaml
# deploy/docker-compose.yml
services:
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  core:
    build:
      context: ..
      dockerfile: services/core/Dockerfile
    environment:
      RATECAP_CONFIG_PATH: /etc/ratecap/ratecap.yaml
      RATECAP_REDIS_ADDR: redis:6379
      RATECAP_GRPC_ADDR: :9090
    volumes:
      - ./ratecap.yaml:/etc/ratecap/ratecap.yaml
    depends_on:
      - redis
    ports:
      - "9090:9090"

  sidecar:
    build:
      context: ..
      dockerfile: services/sidecar/Dockerfile
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
    depends_on:
      - core
    ports:
      - "8080:8080"

  sampleapp:
    build:
      context: ..
      dockerfile: deploy/sampleapp/Dockerfile
    environment:
      RATECAP_SIDECAR_ADDR: http://sidecar:8080
    depends_on:
      - sidecar
    ports:
      - "3000:3000"
```

- [ ] **Step 7: Add go.work.sum entries for the sample app and add it to go.work**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
go work use ./deploy/sampleapp
```

- [ ] **Step 8: Bring the stack up and verify end-to-end behavior**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/deploy
docker compose up --build -d
sleep 5
for i in 1 2 3 4 5 6 7; do
  curl -s -o /dev/null -w "request $i: %{http_code}\n" http://localhost:3000/checkout
done
```

Expected: the first 5 requests print `200`, requests 6 and 7 print `429` — proving the token bucket (rate=2, burst=5) enforces its limit end-to-end through SDK → sidecar → core → Redis.

- [ ] **Step 9: Verify shadow mode**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/deploy
docker compose stop core
docker compose run -e RATECAP_SHADOW_MODE=true -p 8080:8080 sidecar &
sleep 2
for i in 1 2 3 4 5 6 7; do
  curl -s -o /dev/null -w "request %{http_code}\n" http://localhost:3000/checkout
done
```

Expected: all requests return `200` even past the burst limit, because `RATECAP_SHADOW_MODE=true` on the sidecar coerces rejects to allow-through.

(This step exercises the sidecar's global override path in a live environment; stop and restart the compose stack cleanly afterward with `docker compose down` before continuing.)

- [ ] **Step 10: Tear down and commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap/deploy
docker compose down
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add services/core/Dockerfile services/sidecar/Dockerfile deploy/ go.work go.work.sum
git commit -m "feat(deploy): add docker-compose demo proving tier-1 rate limiting end-to-end"
```

---

## Task 11: Root README and CLAUDE.md for the new project

**Files:**
- Create: `README.md`
- Create: `CLAUDE.md`

**Interfaces:** None — documentation only.

- [ ] **Step 1: Write README.md**

```markdown
# RateCap

A faithful, open-source recreation of [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters), built as a hybrid core-engine + sidecar system.

## Status

v1 walking skeleton: Tier 1 (Request Rate Limiter) is implemented end-to-end. Tiers 2-4 (Concurrent Requests Limiter, Fleet Usage Load Shedder, Worker Utilization Load Shedder) are planned next — see `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`.

## Architecture

```
App -> SDK -> sidecar (local) -> core (gRPC) -> Redis (Lua token bucket)
```

## Quick start

```bash
cd deploy
docker compose up --build
curl http://localhost:3000/checkout   # repeat 6+ times to see a 429
```

## Project layout

- `proto/` — gRPC contract (source of truth for all SDKs)
- `services/core/` — central engine: limiter logic, Redis state, config hot-reload
- `services/sidecar/` — local proxy: priority resolution, shadow-mode override
- `packages/sdks/go/` — thin Go client SDK
- `deploy/` — docker-compose demo and sample app

## Design docs

- [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md) — full v1 design
- [`docs/superpowers/plans/2026-07-13-walking-skeleton.md`](docs/superpowers/plans/2026-07-13-walking-skeleton.md) — this implementation's plan
```

- [ ] **Step 2: Write CLAUDE.md**

```markdown
# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

RateCap: a hybrid core-engine + sidecar rate-limiter/load-shedder, faithfully recreating Stripe's 4-tier architecture. See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the full design.

## Build & test

- **build all**: `go build ./...` from repo root (uses `go.work`)
- **test all**: `go build ./... && go test ./...` from repo root
- **test one module**: `cd services/core && go test ./... -v`
- **regenerate proto**: `protoc --go_out=. --go_opt=module=github.com/ratecap/proto --go-grpc_out=. --go-grpc_opt=module=github.com/ratecap/proto proto/ratecap/v1/ratecap.proto` (run from repo root; requires `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`)
- **run the demo stack**: `cd deploy && docker compose up --build`

## Scope discipline

v1 is locked to Stripe's exact 4 mechanisms — do not add a 5th limiting mechanism, bounded queueing, additional storage backends, or a Rust/WASM core without updating the design spec first and getting explicit sign-off. See the spec's "Explicitly Deferred to v2" and "Out of Scope" sections.

## Conventions

- Go module naming: `github.com/ratecap/<service>`
- Cross-module deps within this repo: `go mod edit -replace github.com/ratecap/X=../../X`
- No comments except non-obvious WHY (hidden constraints, subtle invariants)
- Files: 200-400 lines typical, 800 max
```

- [ ] **Step 3: Commit**

```bash
cd /Users/sairamugge/Desktop/Not-Humans-World/RateCap
git add README.md CLAUDE.md
git commit -m "docs: add project README and CLAUDE.md"
```

---

## Spec Coverage Check

- Tier 1 mechanism (token bucket, atomic Lua/Redis) — Tasks 3, 4 ✓
- gRPC contract as source of truth for SDKs — Task 1 ✓
- Hybrid core+sidecar, all-Go v1 — Tasks 6, 8 ✓
- StateStore interface (swappable backend) — Task 3 ✓
- Limiter interface (swappable for future Rust/WASM) — Task 4 ✓
- Config hot-reload, core-owned — Task 5, wired in Task 6 ✓
- Shadow mode (per-tier config + global env override) — Tasks 4, 8 ✓
- Priority header resolution scaffolding — Task 7, wired (unused) in Task 8 ✓
- Response Action enum (ALLOW/REJECT_429/REJECT_503/SHADOW_LOG, no QUEUE) — Task 4 ✓
- Thin Go SDK, no reimplemented limiter logic — Task 9 ✓
- Docker-compose demo proving end-to-end — Task 10 ✓
- Unit tests (pure, no Redis) for tier-1 decision logic — Task 4 ✓
- Integration test against real Redis proving Lua atomicity under concurrency — Task 3 ✓

Deferred to a later plan (per this plan's explicit out-of-scope list and the spec): tiers 2-4, `ratecapctl` CLI/benchmark runner, Grafana dashboard, per-key config overrides, Rust/WASM core, bounded queueing, additional storage backends.
