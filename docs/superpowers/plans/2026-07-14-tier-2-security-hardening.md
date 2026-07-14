# Tier 2 Security Hardening (Group B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the audit's Critical (undocumented plaintext/unauthenticated gRPC) and Important (no auth on ReleaseConcurrency, raw error leakage, missing HTTP method enforcement) security findings on RateCap Tier 2, via a shared-secret gRPC interceptor, error sanitization, HTTP method enforcement, and explicit documentation — without introducing TLS/mTLS.

**Architecture:** A `grpc.UnaryServerInterceptor`/`grpc.UnaryClientInterceptor` pair, backed by a shared secret read from `RATECAP_SHARED_SECRET`, wraps every gRPC call between `ratecap-sidecar` and `ratecap-core`. Both services fail closed (refuse to start) if the secret is unset. `grpcserver`'s handlers stop leaking raw store errors; the sidecar's HTTP handlers stop accepting the wrong HTTP method. `SECURITY.md`/`ARCHITECTURE.md` then document this as v1's explicit, intentional network-transport boundary.

**Tech Stack:** Go 1.26 modules (`services/core`, `services/sidecar`), `google.golang.org/grpc` (interceptors, `codes`, `status`, `metadata`, `test/bufconn`), Go standard library (`crypto/subtle`, `log`, `net/http`).

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module) before every commit that touches that module.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Fail-closed: both `ratecap-core` and `ratecap-sidecar` must refuse to start (`log.Fatalf`) if `RATECAP_SHARED_SECRET` is unset or empty — no code path may run either service unauthenticated.
- No TLS/mTLS work — the shared secret is authentication only, not transport encryption (see the spec's Out of Scope section).
- No changes to the proto wire contract's message shapes in this plan.
- Exact commands and exact expected output are given in every step; run them verbatim.

---

### Task 1: `services/core/auth` and `services/sidecar/auth` packages

**Files:**
- Create: `services/core/auth/auth.go`
- Create: `services/core/auth/auth_test.go`
- Create: `services/sidecar/auth/auth.go`
- Create: `services/sidecar/auth/auth_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (first task).
- Produces: `auth.MetadataKey` (string constant, same value `"x-ratecap-shared-secret"` in both packages, defined independently since they're separate modules); `auth.UnaryServerInterceptor(secret string) grpc.UnaryServerInterceptor` (core); `auth.UnaryClientInterceptor(secret string) grpc.UnaryClientInterceptor` (sidecar). Task 2 wires both into `main.go`.

- [ ] **Step 1: Write the failing test for the server interceptor — missing secret is rejected**

Create `services/core/auth/auth_test.go`:

```go
package auth_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ratecap/core/auth"
)

func TestUnaryServerInterceptor_RejectsMissingSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "response", nil
	}

	_, err := interceptor(context.Background(), "request", &grpc.UnaryServerInfo{}, handler)

	if err == nil {
		t.Fatal("expected error when no metadata is present")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
	if handlerCalled {
		t.Error("expected handler NOT to be called when secret is missing")
	}
}

func TestUnaryServerInterceptor_RejectsWrongSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "response", nil
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "wrong-secret"))
	_, err := interceptor(ctx, "request", &grpc.UnaryServerInfo{}, handler)

	if err == nil {
		t.Fatal("expected error when secret is wrong")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
	if handlerCalled {
		t.Error("expected handler NOT to be called when secret is wrong")
	}
}

func TestUnaryServerInterceptor_AllowsCorrectSecret(t *testing.T) {
	interceptor := auth.UnaryServerInterceptor("correct-secret")
	handler := func(ctx context.Context, req any) (any, error) {
		return "response", nil
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "correct-secret"))
	resp, err := interceptor(ctx, "request", &grpc.UnaryServerInfo{}, handler)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "response" {
		t.Errorf("expected handler's response to propagate, got %v", resp)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/core && go test ./auth/... 2>&1 | head -20`
Expected: FAIL — `no required module provides package github.com/ratecap/core/auth` (the package doesn't exist yet)

- [ ] **Step 3: Write `services/core/auth/auth.go`**

```go
package auth

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const MetadataKey = "x-ratecap-shared-secret"

func UnaryServerInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md.Get(MetadataKey)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "missing shared secret")
		}
		if subtle.ConstantTimeCompare([]byte(md.Get(MetadataKey)[0]), []byte(secret)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid shared secret")
		}
		return handler(ctx, req)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/core && go test ./auth/... -race -v 2>&1 | tail -20`
Expected: PASS — `TestUnaryServerInterceptor_RejectsMissingSecret`, `TestUnaryServerInterceptor_RejectsWrongSecret`, `TestUnaryServerInterceptor_AllowsCorrectSecret` all report `--- PASS`, final line `ok      github.com/ratecap/core/auth`

- [ ] **Step 5: Write the failing test for the client interceptor — secret is attached to outgoing metadata**

Create `services/sidecar/auth/auth_test.go`:

```go
package auth_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/ratecap/sidecar/auth"
)

func TestUnaryClientInterceptor_AttachesSecretToOutgoingMetadata(t *testing.T) {
	interceptor := auth.UnaryClientInterceptor("my-secret")
	var capturedCtx context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err := interceptor(context.Background(), "/ratecap.v1.RatecapService/CheckRateLimit", "request", "reply", nil, invoker)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	if !ok {
		t.Fatal("expected outgoing metadata to be set")
	}
	values := md.Get(auth.MetadataKey)
	if len(values) != 1 || values[0] != "my-secret" {
		t.Errorf("expected metadata key %q to carry %q, got %v", auth.MetadataKey, "my-secret", values)
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `cd services/sidecar && go test ./auth/... 2>&1 | head -20`
Expected: FAIL — `no required module provides package github.com/ratecap/sidecar/auth`

- [ ] **Step 7: Write `services/sidecar/auth/auth.go`**

```go
package auth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const MetadataKey = "x-ratecap-shared-secret"

func UnaryClientInterceptor(secret string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, secret)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
```

- [ ] **Step 8: Run the test to verify it passes**

Run: `cd services/sidecar && go test ./auth/... -race -v 2>&1 | tail -20`
Expected: PASS — `TestUnaryClientInterceptor_AttachesSecretToOutgoingMetadata` reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/auth`

- [ ] **Step 9: gofmt check and commit**

Run: `gofmt -l services/core/auth/auth.go services/core/auth/auth_test.go services/sidecar/auth/auth.go services/sidecar/auth/auth_test.go`
Expected: no output

```bash
git add services/core/auth/auth.go services/core/auth/auth_test.go services/sidecar/auth/auth.go services/sidecar/auth/auth_test.go
git commit -m "feat(core,sidecar): add shared-secret gRPC auth interceptors

UnaryServerInterceptor (core) rejects any call missing a matching
x-ratecap-shared-secret metadata value; UnaryClientInterceptor
(sidecar) attaches the shared secret to every outgoing call.
Not yet wired into main.go — that's the next task."
```

---

### Task 2: Wire interceptors into `main.go` (fail-closed) + gRPC integration test

**Files:**
- Modify: `services/core/main.go`
- Modify: `services/sidecar/main.go`
- Create: `services/core/grpcserver/auth_integration_test.go`

**Interfaces:**
- Consumes: `auth.UnaryServerInterceptor` (core, Task 1), `auth.UnaryClientInterceptor` (sidecar, Task 1).
- Produces: both services now read `RATECAP_SHARED_SECRET` from the environment and fail closed if it's unset — Task 5's docker-compose update depends on this env var existing.

- [ ] **Step 1: Write the failing integration test — an unauthenticated call to `grpcserver.Server` is rejected over a real gRPC transport**

Create `services/core/grpcserver/auth_integration_test.go`:

```go
package grpcserver_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/auth"
	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

func startTestServer(t *testing.T, secret string) (ratecapv1.RatecapServiceClient, func()) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(secret)))
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}))

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
	}
	return ratecapv1.NewRatecapServiceClient(conn), cleanup
}

func TestGRPCAuth_RejectsCallWithNoSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	_, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err == nil {
		t.Fatal("expected error when no shared secret is sent")
	}
}

func TestGRPCAuth_RejectsCallWithWrongSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.MetadataKey, "wrong-secret")
	_, err := client.CheckRateLimit(ctx, &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err == nil {
		t.Fatal("expected error when wrong shared secret is sent")
	}
}

func TestGRPCAuth_AllowsCallWithCorrectSecret(t *testing.T) {
	client, cleanup := startTestServer(t, "server-secret")
	defer cleanup()

	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.MetadataKey, "server-secret")
	resp, err := client.CheckRateLimit(ctx, &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails to compile or fails**

Run: `cd services/core && go test ./grpcserver/... -run TestGRPCAuth 2>&1 | head -20`
Expected: FAIL — either a compile error (if `bufconn` isn't yet resolvable — see the note below) or, if it compiles, `TestGRPCAuth_AllowsCallWithCorrectSecret` fails because nothing wires the interceptor into a real server yet at this point (the test file itself already wires it directly in `startTestServer`, so if this fails, re-check the test file matches Step 1 exactly — this integration test intentionally builds its own server since it's asserting the interceptor works over real gRPC transport, independent of `main.go`'s wiring)

**Note on `bufconn`:** `google.golang.org/grpc/test/bufconn` ships inside the already-present `google.golang.org/grpc` module (confirmed present at the pinned `v1.82.0` in this environment's module cache) — no `go.mod` change is needed. If `go test` reports `no required module provides package google.golang.org/grpc/test/bufconn`, run `cd services/core && go mod tidy` once to resolve it (this only updates `go.sum`, not a version bump), then re-run the test.

- [ ] **Step 3: Run the test to verify it passes as written (this test doesn't require any main.go change to pass — it's self-contained)**

Run: `cd services/core && go test ./grpcserver/... -run TestGRPCAuth -race -v 2>&1 | tail -20`
Expected: PASS — all 3 `TestGRPCAuth_*` tests report `--- PASS`, final line `ok      github.com/ratecap/core/grpcserver`

- [ ] **Step 4: Wire the server interceptor into `services/core/main.go` with fail-closed behavior**

In `services/core/main.go`, add the import `"github.com/ratecap/core/auth"` to the import block (no other new imports are needed here — `crypto/subtle` is used inside the `auth` package, not `main.go`):

```go
	"github.com/ratecap/core/auth"
	"github.com/ratecap/core/config"
	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
	"github.com/ratecap/core/store"
```

Then insert this block immediately after the `redisAddr` block (right before `redisClient := redis.NewClient(...)`):

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-core refuses to start without gRPC authentication configured")
	}
```

Then replace `grpcServer := grpc.NewServer()` with:

```go
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret)))
```

- [ ] **Step 5: Wire the client interceptor into `services/sidecar/main.go` with fail-closed behavior**

In `services/sidecar/main.go`, add the import `"github.com/ratecap/sidecar/auth"` to the import block:

```go
	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/sidecar/auth"
	"github.com/ratecap/sidecar/proxy"
```

Then insert this block immediately after the `coreAddr` block (right before `conn, err := grpc.NewClient(...)`):

```go
	sharedSecret := os.Getenv("RATECAP_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatalf("RATECAP_SHARED_SECRET must be set — ratecap-sidecar refuses to start without gRPC authentication configured")
	}
```

Then replace the `grpc.NewClient` call with:

```go
	conn, err := grpc.NewClient(
		coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(sharedSecret)),
	)
```

- [ ] **Step 6: Rebuild both modules to confirm they compile**

Run: `cd services/core && go build ./... && cd ../sidecar && go build ./... && cd ../..`
Expected: no output, exit code 0 for both

- [ ] **Step 7: Run each module's full test suite**

Run: `(cd services/core && go test ./... -race 2>&1 | tail -20)`
Expected: `ok` for `auth`, `config`, `grpcserver`, `limiter` (`store` needs Docker — a Docker-connectivity failure here is unrelated to this task, not a regression)

Run: `(cd services/sidecar && go test ./... -race 2>&1 | tail -20)`
Expected: `ok` for `auth`, `proxy`, `shadow`

- [ ] **Step 8: gofmt check and commit**

Run: `gofmt -l services/core/main.go services/sidecar/main.go services/core/grpcserver/auth_integration_test.go`
Expected: no output

```bash
git add services/core/main.go services/sidecar/main.go services/core/grpcserver/auth_integration_test.go
git commit -m "feat(core,sidecar): require RATECAP_SHARED_SECRET, wire gRPC auth interceptors

Both services now fail closed (log.Fatalf) at startup if
RATECAP_SHARED_SECRET is unset, and every gRPC call between them
is now authenticated via the Task 1 interceptors. A real in-process
gRPC integration test (bufconn) proves an unauthenticated or
wrongly-authenticated call is rejected end-to-end, not just at the
interceptor-function level."
```

---

### Task 3: Error sanitization in `grpcserver`

**Files:**
- Modify: `services/core/grpcserver/server.go`
- Modify: `services/core/grpcserver/server_test.go`

**Interfaces:**
- Consumes: nothing new from earlier tasks.
- Produces: `internalError(context string, err error) error` (package-private helper) — used by both RPC handlers; no external consumer, this is the terminal fix for the error-leakage finding.

- [ ] **Step 1: Update the failing/changing test — store errors must NOT leak into the returned error**

In `services/core/grpcserver/server_test.go`, replace `TestReleaseConcurrency_PropagatesStoreError` entirely with:

```go
func TestReleaseConcurrency_SanitizesStoreErrorButPropagatesFailure(t *testing.T) {
	releaser := &fakeReleaser{err: errors.New("dial tcp 10.0.0.5:6379: connect: connection refused")}
	s := grpcserver.NewServer(limiter.NewPipeline(&fakeLimiter{}), releaser)

	_, err := s.ReleaseConcurrency(context.Background(), &ratecapv1.ReleaseConcurrencyRequest{
		Key:              "user-1",
		ConcurrencyToken: "tok-abc",
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
	if strings.Contains(err.Error(), "10.0.0.5") || strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected sanitized error, but original error text leaked: %v", err)
	}
}
```

Add `"strings"`, `"google.golang.org/grpc/codes"`, and `"google.golang.org/grpc/status"` to the test file's import block (it already imports `"context"`, `"errors"`, `"testing"`, the proto package, and `grpcserver`/`limiter`):

```go
import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)
```

Also add a new test proving `CheckRateLimit` sanitizes errors the same way — insert after `TestCheckRateLimit_PropagatesSkipConcurrencyLimitToPipeline`:

```go
func TestCheckRateLimit_SanitizesStoreError(t *testing.T) {
	fl := &fakeLimiter{err: errors.New("redis: unexpected type *redis.StatusCmd for result")}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	_, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
	if strings.Contains(err.Error(), "StatusCmd") {
		t.Errorf("expected sanitized error, but original error text leaked: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/core && go test ./grpcserver/... -run 'TestReleaseConcurrency_SanitizesStoreErrorButPropagatesFailure|TestCheckRateLimit_SanitizesStoreError' -v 2>&1 | tail -30`
Expected: FAIL — both tests fail on `status.Code(err) != codes.Internal` (current code returns the raw error, whose gRPC status code defaults to `codes.Unknown`, not `codes.Internal`)

- [ ] **Step 3: Add the `internalError` helper and route both handlers through it**

In `services/core/grpcserver/server.go`, add `"log"`, `"google.golang.org/grpc/codes"`, and `"google.golang.org/grpc/status"` to the import block:

```go
import (
	"context"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/limiter"
)
```

Replace the `CheckRateLimit` method's error check:

```go
	if err != nil {
		return nil, err
	}
```

with:

```go
	if err != nil {
		return nil, internalError("CheckRateLimit", err)
	}
```

Replace the `ReleaseConcurrency` method's error check:

```go
	if err := s.releaser.DecrConcurrent(ctx, req.Key, req.ConcurrencyToken); err != nil {
		return nil, err
	}
```

with:

```go
	if err := s.releaser.DecrConcurrent(ctx, req.Key, req.ConcurrencyToken); err != nil {
		return nil, internalError("ReleaseConcurrency", err)
	}
```

Add the helper function after `ReleaseConcurrency` and before `toProtoAction`:

```go
func internalError(context string, err error) error {
	log.Printf("grpcserver: %s: %v", context, err)
	return status.Error(codes.Internal, "internal error")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/core && go test ./grpcserver/... -race -v 2>&1 | tail -40`
Expected: PASS — every test including `TestReleaseConcurrency_SanitizesStoreErrorButPropagatesFailure` and `TestCheckRateLimit_SanitizesStoreError` reports `--- PASS`, final line `ok      github.com/ratecap/core/grpcserver`

- [ ] **Step 5: gofmt check and commit**

Run: `gofmt -l services/core/grpcserver/server.go services/core/grpcserver/server_test.go`
Expected: no output

```bash
git add services/core/grpcserver/server.go services/core/grpcserver/server_test.go
git commit -m "fix(core): sanitize gRPC error responses, log real errors server-side

CheckRateLimit and ReleaseConcurrency previously returned raw store
errors (redis client errors, %T type names) verbatim to any caller.
Both now log the real error server-side and return a generic
codes.Internal status, closing the audit's error-leakage finding."
```

---

### Task 4: HTTP method enforcement on sidecar handlers

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify: `services/sidecar/proxy/proxy_test.go`

**Interfaces:**
- Consumes: nothing new from earlier tasks.
- Produces: `/check` now rejects non-GET with 405; `/release` now rejects non-POST with 405. No new exported symbols — behavior-only change.

- [ ] **Step 1: Write the failing tests**

In `services/sidecar/proxy/proxy_test.go`, add after `TestServeHTTP_NoSkipConcurrencyParamLeavesSkipConcurrencyLimitFalse`:

```go
func TestServeHTTP_RejectsNonGETMethod(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodPost, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}
```

Add after `TestReleaseHandler_ServeHTTP_UpstreamErrorReturns500` (at the end of the file):

```go
func TestReleaseHandler_ServeHTTP_RejectsNonPOSTMethod(t *testing.T) {
	client := &fakeReleaseClient{}
	h := proxy.NewReleaseHandler(client)

	req := httptest.NewRequest(http.MethodGet, "/release?key=user-1&token=tok-abc", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./proxy/... -run 'TestServeHTTP_RejectsNonGETMethod|TestReleaseHandler_ServeHTTP_RejectsNonPOSTMethod' -v 2>&1 | tail -20`
Expected: FAIL — both report `expected 405, got 200` (no method check exists yet, and the fake clients return a 200-shaped response by default)

- [ ] **Step 3: Add the method check to `Handler.ServeHTTP`**

In `services/sidecar/proxy/proxy.go`, insert at the very top of `Handler.ServeHTTP`, before the `key := r.URL.Query().Get("key")` line:

```go
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
```

- [ ] **Step 4: Add the method check to `ReleaseHandler.ServeHTTP`**

In `services/sidecar/proxy/proxy.go`, insert at the very top of `ReleaseHandler.ServeHTTP`, before the `key := r.URL.Query().Get("key")` line:

```go
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
```

- [ ] **Step 5: Run the full proxy test suite to verify the new tests pass and nothing else broke**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -50`
Expected: PASS — every test including the two new ones, and every pre-existing test (`TestServeHTTP_AllowReturns200`, `TestReleaseHandler_ServeHTTP_CallsReleaseConcurrencyWithKeyAndToken`, etc. — all of which already use the correct method) reports `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`

- [ ] **Step 6: gofmt check and commit**

Run: `gofmt -l services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go`
Expected: no output

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go
git commit -m "fix(sidecar): enforce GET on /check and POST on /release

Neither handler checked r.Method, so GET/PUT/DELETE/PATCH all
succeeded identically to the documented method — making the
state-mutating /release endpoint reachable via a passive GET.
Both now return 405 on the wrong method."
```

---

### Task 5: docker-compose wiring + full end-to-end re-verification

**Files:**
- Modify: `deploy/docker-compose.yml`

**Interfaces:**
- Consumes: `RATECAP_SHARED_SECRET` (Task 2's fail-closed requirement).
- Produces: a live-verified demo stack proving Tasks 1-4 work together over real gRPC/HTTP, not just in unit tests.

- [ ] **Step 1: Add `RATECAP_SHARED_SECRET` to both `core` and `sidecar` in `deploy/docker-compose.yml`**

Replace the `core` service's `environment:` block:

```yaml
    environment:
      RATECAP_CONFIG_PATH: /etc/ratecap/ratecap.yaml
      RATECAP_REDIS_ADDR: redis:6379
      RATECAP_GRPC_ADDR: :9090
```

with:

```yaml
    environment:
      RATECAP_CONFIG_PATH: /etc/ratecap/ratecap.yaml
      RATECAP_REDIS_ADDR: redis:6379
      RATECAP_GRPC_ADDR: :9090
      # Demo-only value — real deployments must inject this via proper secrets
      # management (e.g. a mounted secret file or orchestrator-native secret),
      # never a value committed to a compose file.
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
```

Replace the `sidecar` service's `environment:` block:

```yaml
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
```

with:

```yaml
    environment:
      RATECAP_CORE_ADDR: core:9090
      RATECAP_SIDECAR_ADDR: :8080
      RATECAP_SHARED_SECRET: demo-shared-secret-do-not-use-in-production
```

- [ ] **Step 2: Confirm Docker is reachable**

Docker has been observed to go unreachable intermittently in this environment. Run:

```bash
docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

If not reachable, start Docker Desktop and re-run until it reports reachable before continuing.

- [ ] **Step 3: Clean rebuild and bring the stack up**

Run from `deploy/`:

```bash
cd deploy
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`. If `core` or `sidecar` immediately exits, check logs with `docker compose logs core sidecar` — the most likely cause is a typo in the `RATECAP_SHARED_SECRET` env var name, since both services now `log.Fatalf` if it's missing.

- [ ] **Step 4: Re-verify Tier 1 still works (regression check)**

Run:

```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "checkout %{http_code}\n" http://localhost:3000/checkout; done
```

Expected: exactly 5 lines `checkout 200` followed by 2 lines `checkout 429` (matching `rate_limiter.default_rate=2, default_burst=5` in `deploy/ratecap.yaml`) — proving Tier 1 still functions correctly with auth now enabled end-to-end (sampleapp → sidecar → core, all now authenticated).

- [ ] **Step 5: Re-verify Tier 2 still works (regression check)**

Run:

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "slow-report %{http_code}\n" http://localhost:3000/slow-report & done
wait
```

Expected: exactly 3 lines `slow-report 200` and 2 lines `slow-report 429` (matching `concurrency_limiter.default_max_concurrent=3` in `deploy/ratecap.yaml`) — proving Tier 2's Acquire/Release path also still works with auth enabled.

- [ ] **Step 6: Confirm the auth boundary is actually live — an unauthenticated direct call to core's gRPC port must be rejected**

`grpcurl` is the standard tool for this, but may not be installed. Use this fallback shell check via a throwaway Go program invoked with `go run`, which requires no new dependency (uses only the already-vendored `google.golang.org/grpc` and the repo's own generated proto types):

```bash
cat > /tmp/ratecap-auth-check.go << 'EOF'
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"
)

func main() {
	conn, err := grpc.NewClient("localhost:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("dial error:", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = client.CheckRateLimit(ctx, &ratecapv1.CheckRateLimitRequest{Key: "probe", Cost: 1})
	if err == nil {
		fmt.Println("UNEXPECTED: call succeeded with no shared secret")
		os.Exit(1)
	}
	fmt.Println("EXPECTED: call rejected:", err)
}
EOF
cd services/core && go run /tmp/ratecap-auth-check.go
```

Expected output: a line starting with `EXPECTED: call rejected:` containing `Unauthenticated` and `missing shared secret` — confirming an unauthenticated caller cannot reach `CheckRateLimit` even with direct network access to `ratecap-core`'s exposed port (`9090` is published in `docker-compose.yml`, so this really is reachable from the host, not just from inside the sidecar's network namespace — the auth boundary, not network isolation, is what's stopping it here).

Clean up: `rm /tmp/ratecap-auth-check.go`

- [ ] **Step 7: Teardown**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 8: Commit**

```bash
git add deploy/docker-compose.yml
git commit -m "chore(deploy): set RATECAP_SHARED_SECRET for the demo stack

Both core and sidecar now require this env var to start (Task 2's
fail-closed behavior); the demo stack needs a value to keep working.
Re-verified end-to-end: Tier 1 and Tier 2 both still function
correctly with auth enabled, and a direct unauthenticated gRPC call
to core's exposed port is confirmed rejected."
```

---

### Task 6: Document the v1 network-transport boundary

**Files:**
- Modify: `SECURITY.md`
- Modify: `ARCHITECTURE.md`

**Interfaces:**
- Consumes: nothing (documentation only).
- Produces: nothing consumed by later tasks — this is the last task.

- [ ] **Step 1: Add a "Network Transport Security (v1)" section to `SECURITY.md`**

Insert this new section immediately before the existing `## Scope` section (i.e. after `## Reporting a Vulnerability` and its content, before `## Scope`):

```markdown
## Network Transport Security (v1)

`ratecap-core` and `ratecap-sidecar` communicate over plaintext gRPC, authenticated by a shared secret (`RATECAP_SHARED_SECRET`) rather than TLS/mTLS. This is v1's explicit, intentional posture:

- The shared secret proves a caller is a legitimate RateCap component; it does **not** encrypt traffic or protect against a network-level eavesdropper or man-in-the-middle.
- **`ratecap-core` and `ratecap-sidecar` must run on a private, trusted network only** — e.g. a Docker Compose network, a Kubernetes cluster-internal `ClusterIP`, or an equivalent isolated segment. Never expose `ratecap-core`'s gRPC port to an untrusted network.
- Both services fail closed: if `RATECAP_SHARED_SECRET` is unset, neither service starts. There is no supported configuration where gRPC auth is silently disabled.
- TLS/mTLS for this hop is deferred to v2.

If your deployment cannot guarantee a private network between `ratecap-core` and `ratecap-sidecar`, do not run RateCap v1 in that environment — wait for v2's TLS support, or open an issue describing your constraint.
```

- [ ] **Step 2: Extend the sidecar bullet in `ARCHITECTURE.md`'s component-overview section**

In `ARCHITECTURE.md`, replace this exact bullet:

```markdown
- **`ratecap-sidecar`** (`services/sidecar/`) — a thin, co-located proxy. Apps talk to the sidecar over plain HTTP; the sidecar forwards checks to `ratecap-core` over gRPC. This is where safe-rollout (shadow mode) and priority resolution live.
```

with:

```markdown
- **`ratecap-sidecar`** (`services/sidecar/`) — a thin, co-located proxy. Apps talk to the sidecar over plain HTTP; the sidecar forwards checks to `ratecap-core` over gRPC, authenticated with a shared secret (`RATECAP_SHARED_SECRET`) but not encrypted — this hop must stay on a private network (see [`SECURITY.md`](SECURITY.md#network-transport-security-v1); TLS/mTLS is deferred to v2). This is where safe-rollout (shadow mode) and priority resolution live.
```

- [ ] **Step 3: Verify the edits landed correctly**

Run: `grep -n "Network Transport Security" SECURITY.md && grep -n "RATECAP_SHARED_SECRET" ARCHITECTURE.md`
Expected: one match in each file, confirming both edits are present.

- [ ] **Step 4: Commit**

```bash
git add SECURITY.md ARCHITECTURE.md
git commit -m "docs: document plaintext-gRPC-plus-shared-secret as v1's explicit network boundary

Closes the audit's core finding: this posture existed in code but
was undocumented everywhere. SECURITY.md now states the private-
network requirement explicitly; ARCHITECTURE.md's component overview
cross-references it at the sidecar-to-core hop."
```

---

## Post-plan note

This plan completes Group B (security hardening). Combined with Group A (already merged — the two architecture-bug fixes), this closes every audit finding the user chose to fix before opening the Tier 2 PR. The remaining findings (per-token ownership on `ReleaseConcurrency`, the concurrency token traveling as a plaintext URL query parameter, and the Minor correctness/spec-fidelity findings from the earlier audit run) are tracked as GitHub follow-up issues, not implemented in this plan, per the user's explicitly approved scope.
