# Tier 2 Audit Remediation — Security Hardening (Group B) — Design Spec

**Date:** 2026-07-14
**Status:** Approved
**Context:** The second of two remediation groups from a post-implementation security/architecture audit of Tier 2 (Concurrent Requests Limiter), run before opening that branch's PR. Group A (the two guaranteed-trigger architecture bugs — `Pipeline.Check` dropping an earlier tier's reserved token, and `ReleaseConcurrency` hardcoding `req.Key`) is already fixed and merged on `feature/tier-2-concurrent-limiter` (HEAD `8ccb70b`). This spec covers Group B: closing the audit's Critical and Important security findings via minimal hardening, without introducing TLS/mTLS (explicitly deferred to v2 — see Out of Scope).

---

## Problem

The audit found `ratecap-core`'s gRPC server and `ratecap-sidecar`'s gRPC client run with `grpc.NewServer()` / `grpc.WithTransportCredentials(insecure.NewCredentials())` — plaintext, zero authentication — and this posture is **undocumented anywhere**: not in `SECURITY.md`, not in `ARCHITECTURE.md`, not in either design spec. Every other v1 scope boundary (queueing, Rust/WASM core, additional storage backends) is explicitly called out as deferred; this one wasn't, which is the actual defect — an unstated gap, not an accepted trade-off.

Downstream of that root cause, the audit found:
- `ReleaseConcurrency` has no authentication or rate limiting — reachable by any network client that can dial `ratecap-core`.
- `grpcserver`'s two RPC handlers return raw store errors (`return nil, err`), leaking internal details (Go type names, unwrapped go-redis client errors) to any caller.
- The sidecar's `/check` and `/release` HTTP handlers never check `r.Method` — GET/PUT/DELETE/PATCH all succeed identically to the documented GET/POST, making the state-mutating `/release` endpoint reachable via a passive `<img src>`-style GET.

The user has explicitly chosen the remediation shape: a shared-secret gRPC interceptor (not mTLS — that's a v2-scale project), POST-only enforcement, error sanitization, and explicit documentation of the plaintext-internal-network boundary. mTLS and "document only, no code" were both explicitly rejected as options.

## Key Design Decisions

### 1. Shared-secret gRPC authentication via native interceptors

Go's `google.golang.org/grpc` package (already a direct dependency of every service module) provides `grpc.UnaryServerInterceptor` and `grpc.UnaryClientInterceptor` specifically for this kind of cross-cutting concern — registering once at server/client construction covers every RPC automatically, with no per-handler wiring and no custom protocol invented. This matches the project's established preference for proven, framework-native patterns over bespoke schemes.

**`services/core/auth/auth.go`** (new package):

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

**`services/sidecar/auth/auth.go`** (new package):

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

`crypto/subtle.ConstantTimeCompare` avoids a timing side-channel on the secret comparison — cheap to do correctly, no reason not to. The comparison happens only on the server side (the client has no one to check against); the client's interceptor just attaches the secret to every outgoing call's metadata.

Wiring:
- `services/core/main.go`: `grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(secret)))` — replaces the bare `grpc.NewServer()`. Covers `CheckRateLimit` and `ReleaseConcurrency` both, automatically.
- `services/sidecar/main.go`: `grpc.NewClient(coreAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(auth.UnaryClientInterceptor(secret)))`.

### 2. Fail-closed on a missing secret

Both `main.go`s read `RATECAP_SHARED_SECRET` from the environment (the same `os.Getenv` pattern already used for `RATECAP_REDIS_ADDR`, `RATECAP_CORE_ADDR`, etc. — this is bootstrap/infra config, not a runtime-tunable value, so it does not belong in `ratecap.yaml`, which is watched and could in principle be world-readable in some deployments). If the variable is unset or empty, each service calls `log.Fatalf` and refuses to start — the same failure pattern `config.Load` already uses for a missing/invalid config file.

This is a deliberate choice over fail-open-with-a-warning: a warning is exactly the kind of thing that gets missed in production logs, and "undocumented, silently-unauthenticated gRPC" is the precise defect this whole remediation exists to close. Fail-closed means there is no code path, in any environment, where `ratecap-core` or `ratecap-sidecar` runs without authentication enabled.

`deploy/docker-compose.yml` gets `RATECAP_SHARED_SECRET` set to a fixed demo value on both the `core` and `sidecar` services, so the stack keeps working out of the box — with a comment marking it as a demo-only value that real deployments must replace via proper secrets management, not a committed literal.

### 3. Error sanitization at the gRPC boundary

A small helper in `services/core/grpcserver`:

```go
func internalError(context string, err error) error {
	log.Printf("grpcserver: %s: %v", context, err)
	return status.Error(codes.Internal, "internal error")
}
```

`CheckRateLimit` and `ReleaseConcurrency` both route their store-layer errors through this instead of `return nil, err` — the real error (redis client errors, unexpected Lua result shapes, `%T` type mismatches) is logged server-side where an operator can see it, while callers receive a generic, contentless `Internal` status. This is the standard gRPC pattern for this exact problem (`google.golang.org/grpc/status` and `codes` are already transitive dependencies via the existing gRPC import).

### 4. HTTP method enforcement on the sidecar

Both sidecar handlers gain a method check at the top of `ServeHTTP`, returning `405 Method Not Allowed` on mismatch:

- `Handler.ServeHTTP` (`/check`) — must be `GET` (already the documented and exclusively-tested method).
- `ReleaseHandler.ServeHTTP` (`/release`) — must be `POST` (already the documented and exclusively-tested method).

This closes the audit's finding for both endpoints, not just the state-mutating one — `/check` doesn't mutate state, but enforcing its documented method is free and consistent.

### 5. Documentation: making the v1 boundary explicit

`SECURITY.md` gains a new section, "Network Transport Security (v1)", stating plaintext gRPC + shared-secret authentication is v1's explicit posture: `ratecap-core` and `ratecap-sidecar` must run on a private, trusted network (e.g. a Docker Compose network, a Kubernetes cluster-internal ClusterIP, or an equivalent) — never exposed to an untrusted network — and that TLS/mTLS is deferred to v2. This mirrors the existing "Explicitly Deferred to v2" phrasing already used elsewhere in the project's docs.

`ARCHITECTURE.md`'s component-overview section gets a matching note on the sidecar→core gRPC hop: shared-secret-authenticated, plaintext, private-network-only in v1.

## Build Order

1. `services/core/auth` package + unit tests (valid secret allows, missing/wrong secret rejects with `Unauthenticated`), `services/sidecar/auth` package + unit test (attaches the secret to outgoing metadata) — pure, no network.
2. Wire the server interceptor into `services/core/main.go` (fail-closed on missing `RATECAP_SHARED_SECRET`) and the client interceptor into `services/sidecar/main.go` (same fail-closed behavior) — plus an integration-style test proving `grpcserver.Server` rejects an unauthenticated call end-to-end (bufconn or an in-process `grpc.NewServer`/`grpc.NewClient` pair, no real network).
3. Error sanitization: `grpcserver.internalError` helper + route both RPC handlers' store errors through it, unit-tested (a fake store returning an error produces a generic `Internal` status, never the original error text).
4. HTTP method enforcement on both sidecar handlers, unit-tested (wrong method on `/check` and `/release` each return 405; the already-tested correct method still works).
5. `deploy/docker-compose.yml` gets `RATECAP_SHARED_SECRET` on `core` and `sidecar`; full docker-compose end-to-end re-verification (both tiers still behave correctly with auth now enabled) — plus a manual check that a request to `ratecap-core` **without** the secret header is rejected, proving the auth boundary is actually live in the demo stack, not just unit-tested in isolation.
6. `SECURITY.md` and `ARCHITECTURE.md` documentation updates.

## Testing Strategy

- Unit tests for both auth packages (server interceptor: missing secret / wrong secret / correct secret; client interceptor: metadata attached correctly) — no network.
- An integration-style test wiring `grpcserver.Server` behind a real (in-process) gRPC server/client pair, proving an unauthenticated call is rejected and an authenticated one succeeds — this is the one place a fake/mock can't stand in, since the behavior under test is the interceptor's interaction with real gRPC metadata propagation.
- Unit tests for error sanitization (fake store error in → generic `Internal` status out, original error text never appears in the response).
- Unit tests for HTTP method enforcement on both sidecar handlers.
- End-to-end docker-compose re-verification: both tiers still function correctly with auth enabled, and a direct unauthenticated request to core's gRPC port is confirmed rejected.

## Out of Scope (this group)

- TLS/mTLS for the sidecar↔core hop — explicitly deferred to v2, matching the user's decision. The shared secret is an authentication mechanism only; it does not provide transport encryption or protect against a network-level eavesdropper. This is exactly why the network-boundary documentation in `SECURITY.md`/`ARCHITECTURE.md` matters: the secret is only meaningful on a network where eavesdropping/MITM isn't a live threat.
- Per-token ownership checks on `ReleaseConcurrency` (i.e. binding a token to the specific caller/session that acquired it) — the shared secret establishes "this caller is a legitimate RateCap component," not "this caller is the same one that acquired this specific token." Tightening this further is a larger design question (would need a session/identity concept RateCap doesn't have) and is tracked as a follow-up issue, not fixed here.
- The concurrency token being transmitted as a plaintext URL query parameter (`packages/sdks/go/client.go`'s `Release()`) — tracked as a follow-up issue per the user's approved scope for this remediation, not fixed in this group.
- Rate-limiting the `ReleaseConcurrency` RPC itself beyond what the shared-secret auth provides — the audit's finding here was really about the RPC being reachable by anyone; requiring the shared secret closes that. A dedicated rate limit on this specific RPC is not part of this group's approved scope.
- Any change to the wire contract's message shapes (this group is server/client wiring and docs, not a proto change).
