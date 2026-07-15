# Tier 2 Audit Remediation (Architecture Bugs) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the two guaranteed-trigger architecture bugs found by the Tier 2 security/architecture audit — `Pipeline.Check` dropping an earlier tier's reserved token on a later tier's rejection, and `ReleaseConcurrency` hardcoding `req.Key` as the release key — before either bug can manifest when Tier 3 ships.

**Architecture:** Replace the single `Decision.Token string` field with `Decision.Reservations []TokenReservation`, where each `TokenReservation{Key, Token}` self-describes the Redis key it was reserved against. `Pipeline.Check` accumulates reservations from every tier it checks (not just the last one) and always returns the full list, even when a later tier rejects. The wire contract (proto), grpcserver, sidecar, and SDK are updated to carry a list of (key, token) pairs instead of a single token string, so a future caller always releases against the key the server actually reserved — never an assumed one.

**Tech Stack:** Go 1.26 modules (`services/core`, `services/sidecar`, `packages/sdks/go`, `proto`), Protocol Buffers / gRPC (`protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`), standard `go test` (with `-race` where applicable).

## Global Constraints

- TDD: write the failing test first, confirm it fails for the right reason, then write the minimal implementation, then confirm it passes.
- `gofmt -l` must report zero files before any commit.
- Run `go test ./... -race` (per affected module) before every commit that touches that module.
- No comments except non-obvious WHY (matches every prior task in this project).
- This is pre-1.0 internal-only proto — replace fields outright, no deprecation shims or backwards-compat aliases.
- Exact commands and exact expected output are given in every step; run them verbatim.
- `protoc-gen-go` and `protoc-gen-go-grpc` are installed at `$(go env GOPATH)/bin` but not on `PATH` by default in this environment — every `protoc` invocation below is prefixed with `PATH="$(go env GOPATH)/bin:$PATH"` to find them.

---

### Task 1: Proto contract + core limiter/pipeline fix (`TokenReservation`, `Decision.Reservations`)

**Files:**
- Modify: `proto/ratecap/v1/ratecap.proto`
- Regenerate: `proto/ratecap/v1/ratecap.pb.go`, `proto/ratecap/v1/ratecap_grpc.pb.go` (generated, do not hand-edit)
- Modify: `services/core/limiter/limiter.go`
- Modify: `services/core/limiter/concurrency.go`
- Modify: `services/core/limiter/pipeline.go`
- Modify (tests): `services/core/limiter/pipeline_test.go`
- Modify (tests): `services/core/limiter/concurrency_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (this is the first task).
- Produces: `limiter.TokenReservation{Key, Token string}`; `limiter.Decision.Reservations []TokenReservation` (replaces `Decision.Token string`); `ratecapv1.TokenReservation{Key, Token string}` (generated proto type); `ratecapv1.CheckRateLimitResponse.Reservations []*ratecapv1.TokenReservation` (replaces `ConcurrencyToken string`). Task 2 and Task 3 both depend on these exact names.

- [ ] **Step 1: Write the failing test proving Bug 1 is fixed (earlier tier's reservation must survive a later tier's rejection)**

Add this test to `services/core/limiter/pipeline_test.go`, after the existing `TestPipeline_SecondTierRejectPropagatesDecision` test (before `TestPipeline_ErrorFromAnyTierShortCircuits`):

```go
func TestPipeline_EarlierTierReservationSurvivesLaterTierRejection(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "tok-tier1"}}}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.REJECT_429, RetryAfterMs: 500}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.REJECT_429 {
		t.Fatalf("expected REJECT_429, got %v", d.Action)
	}
	if len(d.Reservations) != 1 {
		t.Fatalf("expected tier1's reservation to survive tier2's rejection, got %d reservations", len(d.Reservations))
	}
	if d.Reservations[0].Key != "user-1" || d.Reservations[0].Token != "tok-tier1" {
		t.Fatalf("expected tier1's reservation {user-1 tok-tier1} to survive, got %+v", d.Reservations[0])
	}
}
```

Also update the existing `TestPipeline_AllTiersAllowReturnsAllowWithLastToken` test — rename it and change its assertions, since `Decision.Token` won't exist after this task. Replace the entire function with:

```go
func TestPipeline_AllTiersAllowAccumulatesAllReservations(t *testing.T) {
	tier1 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW}}
	tier2 := &fakeTier{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "tok-123"}}}}

	p := limiter.NewPipeline(tier1, tier2)
	d, err := p.Check(context.Background(), limiter.Request{Key: "user-1"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != limiter.ALLOW {
		t.Fatalf("expected ALLOW, got %v", d.Action)
	}
	if len(d.Reservations) != 1 {
		t.Fatalf("expected tier2's reservation to propagate, got %d reservations", len(d.Reservations))
	}
	if d.Reservations[0].Key != "user-1" || d.Reservations[0].Token != "tok-123" {
		t.Fatalf("expected reservation {user-1 tok-123} to propagate, got %+v", d.Reservations[0])
	}
	if !tier1.called || !tier2.called {
		t.Fatal("expected both tiers to be checked")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail to compile (Decision.Reservations doesn't exist yet)**

Run: `cd services/core && go test ./limiter/... 2>&1 | head -20`
Expected: FAIL — compile error, e.g. `unknown field Reservations in struct literal of type limiter.Decision` (this is the "fails for the right reason" check; `Decision.Token`/`TokenReservation` don't exist yet)

- [ ] **Step 3: Update `services/core/limiter/limiter.go` — add `TokenReservation`, replace `Decision.Token`**

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
	Key                  string
	Cost                 int
	SkipConcurrencyLimit bool
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
```

- [ ] **Step 4: Update `services/core/limiter/concurrency.go` — build `Reservations` instead of `Token`**

In `services/core/limiter/concurrency.go`, replace the `Check` method's body from `if allowed {` through the final `return Decision{Action: REJECT_429}, nil` with:

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

- [ ] **Step 5: Update `services/core/limiter/concurrency_test.go` — replace `d.Token` assertions with `d.Reservations` assertions**

In `TestConcurrencyLimiter_AllowsExactlyCapRequests`, replace:

```go
		if d.Token == "" {
			t.Fatalf("request %d: expected non-empty token", i)
		}
```

with:

```go
		if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
			t.Fatalf("request %d: expected exactly one reservation with a non-empty token, got %+v", i, d.Reservations)
		}
		if d.Reservations[0].Key != "user-1" {
			t.Fatalf("request %d: expected reservation key %q, got %q", i, "user-1", d.Reservations[0].Key)
		}
```

In `TestConcurrencyLimiter_ShadowModeStillReservesSlot`, replace:

```go
			if d.Token == "" {
				t.Fatalf("request %d: expected a reserved token even in shadow mode, got empty string", i)
			}
```

with:

```go
			if len(d.Reservations) != 1 || d.Reservations[0].Token == "" {
				t.Fatalf("request %d: expected a reserved token even in shadow mode, got %+v", i, d.Reservations)
			}
```

In `TestConcurrencyLimiter_SkipConcurrencyLimitBypassesTheCapEntirely`, replace:

```go
			if d.Token != "" {
				t.Fatalf("request %d: expected no token reserved when SkipConcurrencyLimit is set, got %q", i, d.Token)
			}
```

with:

```go
			if len(d.Reservations) != 0 {
				t.Fatalf("request %d: expected no reservation when SkipConcurrencyLimit is set, got %+v", i, d.Reservations)
			}
```

- [ ] **Step 6: Update `services/core/limiter/pipeline.go` — accumulate reservations, never drop them on rejection**

Replace the entire file with:

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
	var reserved []TokenReservation
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		reserved = append(reserved, d.Reservations...)
		if err != nil || d.Action != ALLOW {
			d.Reservations = reserved
			return d, err
		}
	}
	return Decision{Action: ALLOW, Reservations: reserved}, nil
}
```

- [ ] **Step 7: Run limiter tests to verify they pass**

Run: `cd services/core && go test ./limiter/... -race -v 2>&1 | tail -40`
Expected: PASS — all tests including `TestPipeline_EarlierTierReservationSurvivesLaterTierRejection`, `TestPipeline_AllTiersAllowAccumulatesAllReservations`, and every `TestConcurrencyLimiter_*` test report `--- PASS`, final line `ok      github.com/ratecap/core/limiter`

- [ ] **Step 8: Update the proto contract — replace `concurrency_token` with `repeated TokenReservation reservations`**

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

message TokenReservation {
  string key = 1;
  string token = 2;
}

message CheckRateLimitRequest {
  string key = 1;
  int32 cost = 2;
  bool skip_concurrency_limit = 3;
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

- [ ] **Step 9: Regenerate the Go proto bindings**

Run (from repo root `/Users/sairamugge/Desktop/Not-Humans-World/RateCap/.claude/worktrees/tier-2-concurrent-limiter`):

```bash
PATH="$(go env GOPATH)/bin:$PATH" protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto
```

Expected: no output, exit code 0. Verify with `echo $?` immediately after — expect `0`.

Then verify the generated file now has the new type:

Run: `grep -n "type TokenReservation struct" proto/ratecap/v1/ratecap.pb.go`
Expected: one match, e.g. `type TokenReservation struct {`

- [ ] **Step 10: Build the proto module to confirm the regenerated code compiles**

Run: `cd proto && go build ./... && cd ..`
Expected: no output, exit code 0

- [ ] **Step 11: Run gofmt check and commit**

Run: `gofmt -l services/core/limiter/limiter.go services/core/limiter/concurrency.go services/core/limiter/pipeline.go services/core/limiter/pipeline_test.go services/core/limiter/concurrency_test.go`
Expected: no output (empty = all files already formatted)

```bash
git add proto/ratecap/v1/ratecap.proto proto/ratecap/v1/ratecap.pb.go proto/ratecap/v1/ratecap_grpc.pb.go services/core/limiter/limiter.go services/core/limiter/concurrency.go services/core/limiter/pipeline.go services/core/limiter/pipeline_test.go services/core/limiter/concurrency_test.go
git commit -m "fix(core): accumulate per-tier token reservations instead of a single overwritten token

Decision.Token was a single field that Pipeline.Check overwrote per tier
and dropped entirely on a later tier's rejection. Replace it with
Decision.Reservations []TokenReservation so every ALLOW-with-reservation
tier's (key, token) pair survives regardless of what a later tier does.
Dormant today (tier 1 never reserves), but guaranteed to leak a tier-2
slot the instant tier 3 sits downstream of tier 2 in the pipeline."
```

---

### Task 2: Wire `grpcserver` to the new `Reservations` field

**Files:**
- Modify: `services/core/grpcserver/server.go`
- Modify (tests): `services/core/grpcserver/server_test.go`

**Interfaces:**
- Consumes: `limiter.Decision.Reservations []limiter.TokenReservation` (Task 1); `ratecapv1.TokenReservation{Key, Token string}` and `ratecapv1.CheckRateLimitResponse.Reservations []*ratecapv1.TokenReservation` (Task 1, regenerated proto).
- Produces: `CheckRateLimitResponse.Reservations` populated from `decision.Reservations` — Task 3's sidecar work depends on this response field existing and being populated.

- [ ] **Step 1: Write the failing test — response carries reservations, not a single token**

In `services/core/grpcserver/server_test.go`, replace `TestCheckRateLimit_ReturnsConcurrencyTokenWhenPresent` entirely with:

```go
func TestCheckRateLimit_ReturnsReservationsWhenPresent(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW, Reservations: []limiter.TokenReservation{{Key: "user-1", Token: "tok-abc"}}}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Reservations) != 1 {
		t.Fatalf("expected 1 reservation, got %d", len(resp.Reservations))
	}
	if resp.Reservations[0].Key != "user-1" || resp.Reservations[0].Token != "tok-abc" {
		t.Fatalf("expected reservation {user-1 tok-abc}, got {%s %s}", resp.Reservations[0].Key, resp.Reservations[0].Token)
	}
}

func TestCheckRateLimit_ReturnsNoReservationsWhenNonePresent(t *testing.T) {
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	s := grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{})

	resp, err := s.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{
		Key:  "user-1",
		Cost: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Reservations) != 0 {
		t.Fatalf("expected 0 reservations, got %d", len(resp.Reservations))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail to compile**

Run: `cd services/core && go test ./grpcserver/... 2>&1 | head -20`
Expected: FAIL — compile error referencing `resp.Reservations` being undefined on the still-old-shaped response, or `decision.Reservations`/`ConcurrencyToken` mismatch (grpcserver.go hasn't been updated yet, so `CheckRateLimitResponse` still expects `ConcurrencyToken`)

- [ ] **Step 3: Update `services/core/grpcserver/server.go` — build `[]*ratecapv1.TokenReservation` from `decision.Reservations`**

Replace the `CheckRateLimit` method with:

```go
func (s *Server) CheckRateLimit(ctx context.Context, req *ratecapv1.CheckRateLimitRequest) (*ratecapv1.CheckRateLimitResponse, error) {
	decision, err := s.pipeline.Check(ctx, limiter.Request{
		Key:                  req.Key,
		Cost:                 int(req.Cost),
		SkipConcurrencyLimit: req.SkipConcurrencyLimit,
	})
	if err != nil {
		return nil, err
	}

	reservations := make([]*ratecapv1.TokenReservation, 0, len(decision.Reservations))
	for _, r := range decision.Reservations {
		reservations = append(reservations, &ratecapv1.TokenReservation{Key: r.Key, Token: r.Token})
	}

	return &ratecapv1.CheckRateLimitResponse{
		Action:       toProtoAction(decision.Action),
		RetryAfterMs: decision.RetryAfterMs,
		Reservations: reservations,
	}, nil
}
```

(`ReleaseConcurrency` and `toProtoAction` are unchanged — leave them exactly as they are in the current file.)

- [ ] **Step 4: Run grpcserver tests to verify they pass**

Run: `cd services/core && go test ./grpcserver/... -race -v 2>&1 | tail -30`
Expected: PASS — all tests including `TestCheckRateLimit_ReturnsReservationsWhenPresent` and `TestCheckRateLimit_ReturnsNoReservationsWhenNonePresent` report `--- PASS`, final line `ok      github.com/ratecap/core/grpcserver`

- [ ] **Step 5: Run the full core module test suite to confirm nothing else broke**

Run: `cd services/core && go build ./... && go test ./... -race 2>&1 | tail -20`
Expected: `ok` for every package (`limiter`, `grpcserver`, `store`, `config` — note `store`'s tests require Docker for testcontainers; if Docker is unreachable those will fail for an unrelated reason, not because of this change — confirm the failure, if any, is a Docker-connectivity error, not a compile or assertion error)

- [ ] **Step 6: Run gofmt check and commit**

Run: `gofmt -l services/core/grpcserver/server.go services/core/grpcserver/server_test.go`
Expected: no output

```bash
git add services/core/grpcserver/server.go services/core/grpcserver/server_test.go
git commit -m "fix(core): return token reservations list from CheckRateLimit response

Wires the CheckRateLimitResponse.reservations field (Task 1's proto
change) from limiter.Decision.Reservations, replacing the old single
concurrency_token field."
```

---

### Task 3: Wire sidecar + Go SDK to reservation-aware release (fixes Bug 2 end-to-end)

**Files:**
- Modify: `services/sidecar/proxy/proxy.go`
- Modify (tests): `services/sidecar/proxy/proxy_test.go`
- Modify: `packages/sdks/go/client.go`
- Modify (tests): `packages/sdks/go/client_test.go`

**Interfaces:**
- Consumes: `ratecapv1.CheckRateLimitResponse.Reservations []*ratecapv1.TokenReservation` (Task 2).
- Produces: sidecar `/check` response now sets a `Concurrency-Key` header (new) alongside the existing `Concurrency-Token` header; SDK `Ticket` now stores the server-supplied key (from `Concurrency-Key`) instead of the caller's original key, so `Release()` always sends back the key the server actually reserved against — the structural fix for Bug 2 completing at the outermost layer.

- [ ] **Step 1: Write the failing sidecar test — `/check` sets `Concurrency-Key` from the first reservation**

In `services/sidecar/proxy/proxy_test.go`, replace `TestServeHTTP_SetsConcurrencyTokenHeaderWhenPresent` and `TestServeHTTP_OmitsConcurrencyTokenHeaderWhenEmpty` entirely with:

```go
func TestServeHTTP_SetsConcurrencyTokenAndKeyHeadersWhenReservationPresent(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{
		Action:       ratecapv1.Action_ALLOW,
		Reservations: []*ratecapv1.TokenReservation{{Key: "user-1", Token: "tok-abc"}},
	}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "tok-abc" {
		t.Errorf("expected Concurrency-Token header %q, got %q", "tok-abc", rec.Header().Get("Concurrency-Token"))
	}
	if rec.Header().Get("Concurrency-Key") != "user-1" {
		t.Errorf("expected Concurrency-Key header %q, got %q", "user-1", rec.Header().Get("Concurrency-Key"))
	}
}

func TestServeHTTP_OmitsConcurrencyHeadersWhenNoReservations(t *testing.T) {
	client := &fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{Action: ratecapv1.Action_ALLOW}}
	h := proxy.NewHandler(client, proxy.Sheddable)

	req := httptest.NewRequest(http.MethodGet, "/check?key=user-1", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Header().Get("Concurrency-Token") != "" {
		t.Errorf("expected no Concurrency-Token header, got %q", rec.Header().Get("Concurrency-Token"))
	}
	if rec.Header().Get("Concurrency-Key") != "" {
		t.Errorf("expected no Concurrency-Key header, got %q", rec.Header().Get("Concurrency-Key"))
	}
}
```

- [ ] **Step 2: Run sidecar proxy tests to verify they fail to compile**

Run: `cd services/sidecar && go test ./proxy/... 2>&1 | head -20`
Expected: FAIL — compile error, `unknown field Reservations in struct literal of type ratecapv1.CheckRateLimitResponse` (proxy.go still reads `resp.ConcurrencyToken`, and the sidecar module's `go.mod` replace-directive vendored copy of the proto module needs the Task 1 regenerated types — see Step 2a below if this instead fails with a stale-proto-version error)

- [ ] **Step 2a (only if needed): confirm sidecar module picks up the regenerated proto types**

Run: `cd services/sidecar && go build ./... 2>&1 | head -20`

If this reports an error about `Reservations` or `TokenReservation` not existing on `ratecapv1.CheckRateLimitResponse`, the sidecar module's dependency on `github.com/ratecap/proto` is stale. Run:

```bash
cd services/sidecar && go mod tidy && cd ..
```

Expected: no output or only `go: added`/`go: removed` lines; exit code 0. (This project's modules use `replace github.com/ratecap/proto => ../../proto` in each service's `go.mod` per established convention, so this should pick up Task 1's regenerated code automatically without needing a version bump — `go mod tidy` just resyncs the sum file if needed.)

- [ ] **Step 3: Update `services/sidecar/proxy/proxy.go` — set `Concurrency-Key` alongside `Concurrency-Token`**

Replace this block in `Handler.ServeHTTP`:

```go
	if resp.ConcurrencyToken != "" {
		w.Header().Set("Concurrency-Token", resp.ConcurrencyToken)
	}
```

with:

```go
	if len(resp.Reservations) > 0 {
		w.Header().Set("Concurrency-Token", resp.Reservations[0].Token)
		w.Header().Set("Concurrency-Key", resp.Reservations[0].Key)
	}
```

- [ ] **Step 4: Run sidecar proxy tests to verify they pass**

Run: `cd services/sidecar && go test ./proxy/... -race -v 2>&1 | tail -30`
Expected: PASS — all tests including `TestServeHTTP_SetsConcurrencyTokenAndKeyHeadersWhenReservationPresent` and `TestServeHTTP_OmitsConcurrencyHeadersWhenNoReservations` report `--- PASS`, final line `ok      github.com/ratecap/sidecar/proxy`

- [ ] **Step 5: Write the failing SDK test — `Ticket` stores the server-supplied `Concurrency-Key`, not the caller's key**

In `packages/sdks/go/client_test.go`, replace `TestTicket_Release_CallsReleaseEndpointWithKeyAndToken` entirely with:

```go
func TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/check":
			w.Header().Set("Concurrency-Token", "tok-abc")
			w.Header().Set("Concurrency-Key", "server-assigned-key")
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
		t.Errorf("expected key=server-assigned-key (from Concurrency-Key header, not the caller's Acquire key), got %q", got)
	}
	if got := capturedQuery.Get("token"); got != "tok-abc" {
		t.Errorf("expected token=tok-abc, got %q", got)
	}
}
```

- [ ] **Step 6: Run SDK tests to verify the new test fails**

Run: `cd packages/sdks/go && go test ./... -run TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey -v 2>&1`
Expected: FAIL — `expected key=server-assigned-key ..., got "caller-supplied-key"` (Acquire still stores the caller's own key, not the header)

- [ ] **Step 7: Update `packages/sdks/go/client.go` — `Acquire` stores the server-supplied key**

Replace the body of `Acquire` from `concurrencyTok := resp.Header.Get("Concurrency-Token")` through the final `return &Ticket{...}` with:

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

- [ ] **Step 8: Run SDK tests to verify they pass**

Run: `cd packages/sdks/go && go test ./... -race -v 2>&1 | tail -40`
Expected: PASS — every test including `TestTicket_Release_UsesServerSuppliedConcurrencyKeyNotCallerKey`, `TestTicket_Release_NoOpWhenNoTokenWasIssued`, `TestAcquire_ReturnsAllowedTicketOn200` reports `--- PASS`, final line `ok      github.com/ratecap/sdk-go`

Note `TestTicket_Release_NoOpWhenNoTokenWasIssued` still passes unchanged: it never sets `Concurrency-Key`/`Concurrency-Token` in its fake server, so `concurrencyKey`/`concurrencyTok` are both `""`, `Release()`'s existing `if t.tok == ""  { return nil }` guard still short-circuits correctly.

- [ ] **Step 9: Run gofmt check on all touched files**

Run: `gofmt -l services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go packages/sdks/go/client.go packages/sdks/go/client_test.go`
Expected: no output

- [ ] **Step 10: Rebuild the full repo to confirm every module still compiles together**

Run: `cd services/core && go build ./... && cd ../sidecar && go build ./... && cd ../../packages/sdks/go && go build ./... && cd ../../../deploy/sampleapp && go build ./... && cd ../..`
Expected: no output, exit code 0 for each `go build`

- [ ] **Step 11: Commit**

```bash
git add services/sidecar/proxy/proxy.go services/sidecar/proxy/proxy_test.go packages/sdks/go/client.go packages/sdks/go/client_test.go
git commit -m "fix(sidecar,sdk): release against the server-supplied reservation key

ReleaseConcurrency previously received whatever key the SDK caller
originally passed to Acquire(), which is only correct because tier 2
happens to reserve at req.Key today. Thread the reservation's own key
through a new Concurrency-Key header end-to-end so Release() always
targets the key the server actually reserved against, not an assumed
one — the fix completing at the outermost layer for Bug 2."
```

---

### Task 4: Full-repo verification (all modules, race detector, e2e smoke)

**Files:** none modified — this task only runs verification commands across every module touched by Tasks 1-3.

**Interfaces:**
- Consumes: everything from Tasks 1-3.
- Produces: a verified-green baseline for the security-hardening plan (Group B) to build on next.

- [ ] **Step 1: Run every affected module's test suite with the race detector**

Run each as its own self-contained subshell (each starts fresh from the repo root, so these are safe to run in any order or in separate tool calls):

```bash
(cd services/core && go test ./... -race 2>&1 | tail -20)
(cd services/sidecar && go test ./... -race 2>&1 | tail -20)
(cd packages/sdks/go && go test ./... -race 2>&1 | tail -20)
```

Expected: `ok` for every package in `services/core` (except `store`, which needs Docker — see below), `ok` for `services/sidecar/proxy` and `services/sidecar/shadow`, `ok` for `packages/sdks/go`.

- [ ] **Step 2: Confirm Docker is reachable, then run the Redis-backed integration tests**

Docker has been observed to go unreachable intermittently in this environment. Run:

```bash
docker info > /dev/null 2>&1 && echo "docker reachable" || echo "docker NOT reachable — start Docker Desktop before continuing"
```

If reachable, run:

```bash
cd services/core && go test ./store/... -race -v 2>&1 | tail -40 && cd ..
```

Expected: PASS — every `TestCheckAndDecrement_*`, `TestIncrConcurrent_*`, `TestDecrConcurrent_*` test reports `--- PASS`, final line `ok      github.com/ratecap/core/store`. This task did not modify `store.go` or the Lua scripts, so this run is a regression check confirming Tasks 1-3 didn't break anything downstream — not new coverage.

- [ ] **Step 3: Full docker-compose e2e smoke test — confirm the token-reservation refactor didn't break the live request path**

Run from `deploy/`:

```bash
cd deploy
docker compose down 2>&1
docker compose build --no-cache 2>&1 | tail -20
docker compose up -d 2>&1
sleep 3
docker compose ps
```

Expected: all 4 containers (`redis`, `core`, `sidecar`, `sampleapp`) report `Up`.

- [ ] **Step 4: Exercise `/slow-report` (Tier 2) to prove Acquire/Release still works end-to-end through the new Reservations/Concurrency-Key path**

Run:

```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "slow-report %{http_code}\n" http://localhost:3000/slow-report & done
wait
```

Expected: exactly 3 lines `slow-report 200` and 2 lines `slow-report 429` (order may interleave due to real parallelism) — matching `default_max_concurrent: 3` in `deploy/ratecap.yaml`. This is the same assertion the original Tier 2 e2e task made; reproducing it here proves the `Decision.Token` → `Decision.Reservations` refactor is transparent to callers.

- [ ] **Step 5: Tear down**

Run: `docker compose down 2>&1 && cd ..`
Expected: containers and network removed, no errors.

- [ ] **Step 6: Update the progress ledger**

Append to `.superpowers/sdd/progress.md` (create the "Tier 2 Audit Remediation" section if it doesn't exist) a short entry per task: task name, commit SHA (`git log --oneline -1`), test result summary, and "e2e re-verified: PASS" for this task's Step 4 result. No code changes in this step — ledger only.

---

## Post-plan note

This plan covers Group A (architecture bugs) only. Group B (security hardening: shared-secret gRPC interceptor, POST-only `/release`, gRPC error sanitization, SECURITY.md/ARCHITECTURE.md documentation of the plaintext-internal-network v1 boundary) is a separate plan, to be written and executed immediately after this one completes, per the user's explicit choice to keep the two fix groups independent. The 5 Minor correctness findings and the `SkipConcurrencyLimit`-proliferation / `Decision.Token`-single-field debt findings are tracked as GitHub follow-up issues, not implemented in either plan.
