# RateLimit-Limit / RateLimit-Remaining Headers — Design Spec

**Date:** 2026-09-02
**Status:** Proposed — awaiting owner sign-off
**Context:** Closes a gap `v2.8.0` (Phase 4 of the v3 upgrade roadmap) shipped deliberately partial. See [`docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`](2026-08-27-v3-upgrade-roadmap-design.md) Phase 4 item 7 for the original roadmap item ("Emit IETF `draft-ietf-httpapi-ratelimit-headers`-compliant response headers") and [`docs/superpowers/plans/2026-08-31-v3-roadmap-phase-4-sdk-api.md`](../plans/2026-08-31-v3-roadmap-phase-4-sdk-api.md)'s "Global Constraints" section for the explicit scope cut: *"`limit`/`remaining` are NOT emitted — core doesn't track or expose per-key remaining-token count today, and adding that is a proto change with its own ripple ... that belongs in its own future spec, not folded silently into this item."* `CHANGELOG.md`'s `[2.8.0]` entry records the same cut: *"`RateLimit-Reset` response header (IETF `draft-ietf-httpapi-ratelimit-headers`) on `429` responses — partial compliance; `limit`/`remaining` require a future proto change and are explicitly out of scope here."* This spec is that future proto change.

---

## Problem

Per the IETF draft (`draft-ietf-httpapi-ratelimit-headers`), a compliant rate-limited response carries three response headers: `RateLimit-Limit` (the enforced quota), `RateLimit-Remaining` (quota left in the current window), and `RateLimit-Reset` (seconds until the window resets). RateCap today emits only the third.

Verified against the running code (not just the CHANGELOG's own summary of itself):

- `services/sidecar/proxy/proxy.go`'s real `REJECT_429` branch (lines 192-198) sets `Retry-After-Ms` and `RateLimit-Reset` — nothing else. The negative-cache short-circuit branch (lines 88-98, hit when `negativecache.Cache.IsDenied` already knows this key is denied and skips the core round trip entirely) sets the identical two headers from its own cached `remaining time.Duration`.
- `RateLimit-Limit`/`RateLimit-Remaining` are emitted from **no code path** in this repository. `grep -rn "RateLimit-Limit\|RateLimit-Remaining"` across the whole tree returns nothing outside this spec.
- The Go SDK (`packages/sdks/go/client.go`) already parses `RateLimit-Reset` in both `Allow()` (lines 94-99) and `Acquire()` (lines 216-223), exposing it as `rateLimitReset`/`Ticket.RateLimitReset` — added via a standalone follow-up fix, commit `0c79271` (`fix(sdk-go): surface RateLimit-Reset on Allow() and Acquire()`), *after* the `4970e2b` Phase 4 feature commit that shipped the header itself. This is direct, on-point precedent for how this repo has already handled "the sidecar emits a header the SDK doesn't parse yet" — see Decision 8 below.
- The Python SDK (`packages/sdks/python/src/ratecap/client.py`) parses **only** `Retry-After-Ms` (lines 103, 120) in `allow()`/`acquire()`. It has never parsed `RateLimit-Reset` at all — a pre-existing, unrelated parity gap between the two SDKs, not something this phase's proto change introduces.

The blocking reason `limit`/`remaining` were cut from Phase 4, verified by reading the substrate directly:

- `services/core/store/lua/token_bucket.lua` already computes the exact post-decrement (or, on rejection, pre-decrement) token count in its local `tokens` variable — but `return {allowed, retry_after_ms}` discards it.
- `RedisStore.CheckAndDecrement` (`services/core/store/redis.go:67`) unmarshals exactly that 2-element shape and returns `(bool, int64, error)`.
- `StateStore.CheckAndDecrement` (`services/core/store/store.go`) declares the same 3-return-value interface.
- `TokenBucketLimiter.Check` (`services/core/limiter/tokenbucket.go`) wraps the store call into a `Decision{Action, RetryAfterMs, Tier}` (`services/core/limiter/limiter.go`) — `Decision` has no remaining-tokens or limit field at all.
- `grpcserver.Server.CheckRateLimit` (`services/core/grpcserver/server.go:123-128`) maps `Decision` onto `CheckRateLimitResponse` (`proto/ratecap/v1/ratecap.proto`), which likewise has no such fields (`action`, `retry_after_ms`, `reservations`, `tier` — nothing else).

So the gap is real and the chain of missing plumbing is exactly what the roadmap plan predicted. This spec designs and resolves that plumbing.

**The tier-ambiguity this spec must get right.** `REJECT_429` is returned by two different tiers — `TokenBucketLimiter` (`Tier: "rate_limiter"`, a token-bucket window: "requests per second") and `ConcurrencyLimiter` (`Tier: "concurrency_limiter"`, an in-flight cap: "requests in progress"), confirmed by `grep -n "Tier:" services/core/limiter/*.go`. `limit`/`remaining` are a token-bucket-shaped concept; a concurrency cap's analogous quantity (cap minus in-flight count) is a different metric family that must not be reported under the same header names — reporting, say, `RateLimit-Remaining: 3` on a `concurrency_limiter` rejection would tell a client it has 3 *requests* of quota left in a *time window*, when the real fact is "3 concurrent slots free," a fact about parallelism, not throughput. This spec scopes emission to `Tier 1` only and shows, by tracing `Pipeline.Check`'s actual control flow (not just asserting it), that the scoping check is correct.

## Why This Phase Should Be (Mostly) Additive

Every new proto field is additive (`CheckRateLimitResponse` gains two new field numbers; nothing existing is renamed or renumbered — contrast with the `PRIORITY_UNSPECIFIED` enum renumbering the v3 roadmap spec called out as a genuine breaking wire change needing an explicit release note). Every Go interface change (`StateStore.CheckAndDecrement`, the `checker` interface in `tokenbucket.go`) is a same-package, compile-time-checked signature change with exactly three call sites in non-test code (`services/core/limiter/tokenbucket.go`, `services/core/store/redis.go`, `services/core/store/store.go` — verified via `grep -rln "CheckAndDecrement" --include="*.go" . | grep -v _test.go`), so the blast radius is mechanical and fully enumerable up front.

The one place this spec deliberately does **not** touch is `services/core/limiter/pipeline.go`. Tracing `Pipeline.Check`'s real control flow:

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
			return d, err   // <-- returns the rejecting tier's own Decision, untouched except Reservations
		}
		...
		if d.Tier != "" {
			lastTier = d.Tier   // <-- last tier to run wins, on the ALLOW/QUEUE path only
		}
	}
	return Decision{Action: finalAction, Reservations: reserved, Tier: lastTier}, nil
}
```

On a `REJECT_429` (the only outcome this spec emits headers for — see Decision 5), the loop hits the `err != nil || (d.Action != ALLOW && d.Action != QUEUE)` branch and returns `d` — the rejecting tier's *own* `Decision` struct, verbatim, with only `Reservations` overwritten. Whatever `Tier`/`Limit`/`Remaining` that tier set are preserved exactly, with zero changes to this function. `services/core/limiter/pipeline_test.go`'s existing `TestPipeline_SecondTierRejectPropagatesDecision` already proves this propagation for `RetryAfterMs`; Decision 6 below extends it (and adds a sibling test) to cover `Tier`/`Limit`/`Remaining` too, as a regression guard, not a new behavior.

This zero-pipeline-change property is *why* this spec scopes header emission to `REJECT_429` only rather than also to `ALLOW` (see Decision 5's tradeoff): on the `ALLOW`/`QUEUE` path, `lastTier` is overwritten by whichever tier runs *last* (`fleet_shedder` in the shipped 3-tier pipeline `rateLimiter → concurrencyLimiter → fleetShedder`, confirmed by `services/core/main.go:256`'s `limiter.NewPipeline(rateLimiter, concurrencyLimiter, fleetShedder)`), never `rate_limiter` — so a naive "tag on `Decision.Tier == "rate_limiter"`" check would almost never fire on the success path even though Tier 1 ran on every single request and does have a valid `Limit`/`Remaining` to report. Getting `ALLOW`-path coverage right would require `Pipeline.Check` to thread `Limit`/`Remaining` forward independently of `lastTier`'s last-write-wins semantics (analogous to how `Reservations` already accumulates instead of being overwritten) — a real, callable-out design option, deliberately deferred; see Out of Scope.

## Key Design Decisions

### 1. `token_bucket.lua` gains a third return element: the post-check token count

```lua
redis.call("HSET", key, "tokens", tokens, "updated_at", now)
redis.call("EXPIRE", key, math.ceil(burst / rate) + 60)
-- Redis truncates non-integer Lua numbers to whole numbers on return to the
-- client anyway; math.floor makes that truncation explicit rather than
-- relying on the implicit conversion.
return {allowed, retry_after_ms, math.floor(tokens)}
```

`tokens` at this point is already exactly the right value for both outcomes: on the allowed branch it's the post-decrement remaining count (`tokens - cost`, always ≥ 0 since the branch only runs when `tokens >= cost`); on the rejected branch it's the pre-check refilled count (never decremented, since nothing was taken) — i.e. "how many tokens are actually sitting in the bucket right now," which is the correct IETF `remaining` semantic in both cases. No new Redis key, no new script file, no second round trip.

### 2. `StateStore.CheckAndDecrement` / `RedisStore.CheckAndDecrement` grow a return value

```go
// services/core/store/store.go
type StateStore interface {
	CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (allowed bool, retryAfterMs int64, remaining int64, err error)
	...
}
```

```go
// services/core/store/redis.go
func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, int64, error) {
	...
	result, err := s.tokenBucket.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	...
	if len(result) != 3 {
		return false, 0, 0, fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}
	allowed, ok := result[0].(int64)
	...
	retryAfterMs, ok := result[1].(int64)
	...
	remaining, ok := result[2].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("store: unexpected remaining type %T in lua script result", result[2])
	}
	return allowed == 1, retryAfterMs, remaining, nil
}
```

Mechanical, same shape as the existing `allowed`/`retryAfterMs` unmarshalling immediately above it in the same function.

### 3. `limiter.Decision` gains `Limit`/`Remaining`, populated by `TokenBucketLimiter.Check` only

```go
// services/core/limiter/limiter.go
type Decision struct {
	Action       Action
	RetryAfterMs int64
	Reservations []TokenReservation
	Tier         string
	Limit        int64
	Remaining    int64
}
```

`int64` (not `int32`) to match `RetryAfterMs`'s existing type in the same struct, rather than matching `Cost`/`SetDynamicLimit`'s `int32` — this struct's own internal consistency wins over matching a field on a different message.

```go
// services/core/limiter/tokenbucket.go
func (l *TokenBucketLimiter) Check(ctx context.Context, req Request) (Decision, error) {
	l.mu.RLock()
	rate, burst, shadowMode := l.rate, l.burst, l.shadowMode
	l.mu.RUnlock()

	allowed, retryAfterMs, remaining, err := l.store.CheckAndDecrement(ctx, req.Key, rate, burst, req.Cost)
	if err != nil {
		coremetrics.RecordFailOpen("rate_limiter", "store_error")
		return Decision{Action: ALLOW, Tier: "rate_limiter"}, nil // Limit/Remaining left at zero-value: the store call failed, so no real count exists to report
	}

	if allowed {
		return Decision{Action: ALLOW, Tier: "rate_limiter", Limit: int64(burst), Remaining: remaining}, nil
	}
	if shadowMode {
		return Decision{Action: SHADOW_LOG, RetryAfterMs: retryAfterMs, Tier: "rate_limiter", Limit: int64(burst), Remaining: remaining}, nil
	}
	return Decision{Action: REJECT_429, RetryAfterMs: retryAfterMs, Tier: "rate_limiter", Limit: int64(burst), Remaining: remaining}, nil
}
```

`Limit` is `int64(burst)` — the value already captured under the existing `RLock` at the top of `Check`, not a fresh call to the already-existing `Burst()` accessor (which would take a second, redundant `RLock`). This is also why the design brief's framing of `Limit` as "zero store round-trip" is exactly right: it's read from the same in-memory, mutex-guarded snapshot `Check` already takes for `rate`/`shadowMode`, at the moment of the request — so a `Reconfigure`/`SetRate` call updates it immediately, atomically, for the very next `Check`, with no separate propagation path to get wrong. `ConcurrencyLimiter` and `FleetShedder` are untouched — neither sets `Limit`/`Remaining`, by design (Decision 5).

### 4. Proto: two additive fields on `CheckRateLimitResponse`

```protobuf
message CheckRateLimitResponse {
  Action action = 1;
  int64 retry_after_ms = 2;
  repeated TokenReservation reservations = 3;
  string tier = 4;
  int64 limit = 5;
  int64 remaining = 6;
}
```

Field numbers 5 and 6 are the next free numbers on this message (1-4 already used). Proto3 field addition is backward- and forward-compatible by construction: an old `ratecap-core` binary talking to a new sidecar simply never sets fields 5/6, which decode to their zero value (`0`) on the new sidecar — indistinguishable from "no remaining-token data available," which is the truthful state anyway. A new core talking to an old sidecar has the old sidecar's generated Go struct silently lacking the two fields; the extra wire bytes are skipped by proto3's unknown-field handling. No enum renumbering, no field removal, no `oneof` — none of the patterns this repo's own history (`fix/v3-breaking-wire-changes`'s `PRIORITY_UNSPECIFIED` renumbering) flags as needing an explicit breaking-change release note.

`grpcserver.Server.CheckRateLimit` (`services/core/grpcserver/server.go`) gets a two-line, purely mechanical addition to its existing response construction:

```go
return &ratecapv1.CheckRateLimitResponse{
	Action:       toProtoAction(decision.Action),
	RetryAfterMs: decision.RetryAfterMs,
	Reservations: reservations,
	Tier:         decision.Tier,
	Limit:        decision.Limit,
	Remaining:    decision.Remaining,
}, nil
```

### 5. Sidecar emission: `REJECT_429` only, gated on `resp.Tier == "rate_limiter"` — negative-cache path explicitly excluded

`services/sidecar/proxy/proxy.go`'s real `REJECT_429` branch:

```go
case ratecapv1.Action_REJECT_429:
	w.Header().Set("Retry-After-Ms", strconv.FormatInt(resp.RetryAfterMs, 10))
	w.Header().Set("RateLimit-Reset", strconv.FormatInt((resp.RetryAfterMs+999)/1000, 10))
	if resp.Tier == "rate_limiter" {
		w.Header().Set("RateLimit-Limit", strconv.FormatInt(resp.Limit, 10))
		w.Header().Set("RateLimit-Remaining", strconv.FormatInt(resp.Remaining, 10))
	}
	w.WriteHeader(http.StatusTooManyRequests)
```

Because of Decision 3/the "Why Additive" trace above, `resp.Tier == "rate_limiter"` is a *correct* discriminator at exactly this call site: `Pipeline.Check` only ever returns here (i.e. this `switch` case only ever runs) when the pipeline short-circuited on the first non-`ALLOW`/non-`QUEUE` result, so `resp.Tier` is unambiguously "whichever tier actually rejected this specific request" — never a stale or last-tier-wins value. A `concurrency_limiter` `429` correctly gets `Retry-After-Ms`/`RateLimit-Reset` (unchanged) but not `RateLimit-Limit`/`RateLimit-Remaining`, matching the topic's explicit mandate that Tier 2's cap-minus-in-flight metric must not receive token-bucket header treatment.

**Two scope boundaries deliberately drawn here, not gaps:**

- **`ALLOW` responses never get these headers, matching `RateLimit-Reset`'s existing scope exactly.** The IETF draft recommends emitting rate-limit headers on every response, not only on rejection — this repo's own `v2.8.0` already made the narrower call for `Reset` (CHANGELOG: *"on `429` responses"*), and this spec keeps the same, already-accepted scope rather than quietly expanding it while also adding two new fields. Extending to `ALLOW` is real, valuable future work (see Out of Scope) but needs its own `Pipeline.Check` change (threading `Limit`/`Remaining` through the `ALLOW` path without falling prey to `lastTier`'s overwrite semantics) and is not bundled into this diff.
- **The negative-cache short-circuit branch (lines 88-98) is untouched.** `negativecache.Cache` (`services/sidecar/negativecache/negativecache.go`) stores only `map[string]time.Time` — an expiry instant, nothing else; `MarkDenied`/`IsDenied`'s signatures carry no `Tier`, `Limit`, or `Remaining`, and `MarkDenied` is called unconditionally on any `REJECT_429` (`proxy.go:185`, no tier check) — so a cache hit has no reliable way to know which tier produced the *original* rejection, let alone that tier's `Limit`/`Remaining` at the time. Even if it could store the tier, caching `Remaining` would actively mislead a client: `Remaining` is a live, monotonically-changing quantity (it decreases with every accepted request and increases on refill), so replaying the value observed at the moment of the original rejection on every subsequent cache hit up to `retryAfter` later would report a stale, likely-wrong number — arguably worse than reporting nothing. This is a deliberate scope boundary with a correctness reason, not an oversight; see Out of Scope.

### 6. Regression coverage for the pipeline short-circuit property, not new pipeline behavior

Extend `services/core/limiter/pipeline_test.go`'s existing `TestPipeline_SecondTierRejectPropagatesDecision` (today it only asserts `Action`/`RetryAfterMs` propagate through a second-tier rejection) to also assert `Tier`/`Limit`/`Remaining` propagate, and add a sibling `TestPipeline_FirstTierRejectPropagatesLimitAndRemaining` mirroring the existing `TestPipeline_FirstTierRejectShortCircuitsSecondTier` pattern. These are regression guards proving the "why additive" claim stays true if `pipeline.go` is ever touched later — not new functionality, and not a reason to touch `pipeline.go` itself in this phase.

### 7. Go SDK: `Allow()`'s return tuple grows again, following this repo's own precedent

`packages/sdks/go/client.go`'s `Allow()` already grew from a 3-tuple to a 4-tuple return once, in commit `0c79271` (`fix(sdk-go): surface RateLimit-Reset on Allow() and Acquire()`), landing *after* the Phase 4 feature commit that had shipped the header itself — filed as its own narrow, single-purpose fix, not folded into whatever else was in flight. That commit's own message documents the mechanics: *"Allow()'s signature gained a return value, so its two call sites (`cli/cmd/bench_run.go`, `deploy/sampleapp/main.go`) were updated to match; neither changes behavior, they only discard the new value."*

This spec repeats that exact, now-twice-precedented move:

```go
func (c *Client) Allow(ctx context.Context, key string, opts ...CheckOption) (allowed bool, retryAfterMs int64, rateLimitReset int64, rateLimitLimit int64, rateLimitRemaining int64, err error) {
	...
	if v := resp.Header.Get("RateLimit-Limit"); v != "" {
		rateLimitLimit, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Remaining"); v != "" {
		rateLimitRemaining, _ = strconv.ParseInt(v, 10, 64)
	}
	return false, retryAfterMs, rateLimitReset, rateLimitLimit, rateLimitRemaining, nil
}
```

`Ticket` gains `RateLimitLimit int64` / `RateLimitRemaining int64` fields (same naming convention as the existing `RateLimitReset`), populated identically in `Acquire()`. `cli/cmd/bench_run.go:273` and `deploy/sampleapp/main.go:72` get the same mechanical `, _, _` (now `, _, _, _, _`) call-site update `0c79271` already did once — both are already-known, already-enumerated call sites, not a new discovery.

A tuple with five non-error return values is admittedly getting unwieldy — flagged honestly rather than silently: if a future IETF header (e.g. `RateLimit-Policy`) is ever added, growing this tuple a third time is worth reconsidering in favor of a small returned struct. That redesign is out of scope here: this repo has now chosen the tuple-growth path twice in a row, and unilaterally switching to a struct in a spec about two new headers would be a bigger, more opinionated change than the task requires (see Out of Scope).

### 8. Python SDK: parse the two new headers now; leave the pre-existing `RateLimit-Reset` gap for a separate, later fix

`packages/sdks/python/src/ratecap/client.py`'s `allow()`/`acquire()` gain `rate_limit_limit`/`rate_limit_remaining` parsing in the exact same `except urllib.error.HTTPError as err:` blocks that already parse `Retry-After-Ms`:

```python
retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
rate_limit_limit = int(err.headers.get("RateLimit-Limit", 0) or 0)
rate_limit_remaining = int(err.headers.get("RateLimit-Remaining", 0) or 0)
```

`AllowResult` and `Ticket` gain matching `rate_limit_limit: int = 0` / `rate_limit_remaining: int = 0` fields.

**This is the one decision the originating brief explicitly flagged as unresolved, and it needs a concrete call: should this same change also add the still-missing `RateLimit-Reset` parsing to `client.py`, closing that pre-existing, unrelated gap in the same PR?**

**Recommendation: no — file it as a separate, later `fix(sdk-python)` change, not folded into this one.**

Reasoning:

1. **Direct, on-point precedent already exists in this repo, and it went the "separate" way.** The identical category of gap — "the sidecar already emits a header the SDK doesn't parse yet" — was found and fixed for the Go SDK's `RateLimit-Reset` via commit `0c79271`, filed as its own standalone `fix(sdk-go)` commit, distinct from the `4970e2b` feature commit that had shipped the header. This repo has already established, in its own real history, that the right unit of work for "SDK is missing a header the wire protocol already carries" is a small, single-purpose fix commit — not a rider bundled onto whatever larger change happens to be touching the same file. Doing the opposite for Python (bundling it into this wire-format change) would apply a different standard to the two SDKs for the same class of gap, with no stated reason for the inconsistency.
2. **This phase's diff is already wide.** It touches a Lua script, a store interface, a limiter struct, a proto message, a gRPC server handler, a sidecar response branch, and both SDKs' happy-path header parsing. Folding in an unrelated, pre-existing Python-only bug (present since before this spec, unrelated to the new fields) widens an already multi-layer diff and makes it harder for a reviewer — or a future `git revert` — to treat "added `limit`/`remaining`" and "fixed Python's missed `Reset` parsing" as two independently assessable, independently revertable units. This directly matches this repo's own `git-workflow.md`-style discipline (atomic commits, one logical change) and this project's `CLAUDE.md` Scope Discipline section's broader spirit of not folding unrelated changes into one gated task.
3. **The counter-argument is real but weak.** Yes, `allow()`/`acquire()`'s error-handling blocks are already being edited in this phase to add the two new fields, so adding one more `err.headers.get(...)` line for `Reset` costs almost nothing marginally. But "we're already in the file" is exactly the kind of reasoning that, generalized, erodes atomic-commit discipline over time — and the fix itself is small enough (one line, mirroring an already-known-good pattern) that deferring it costs nothing either. Filing it as its own commit — in the same PR/branch if convenient, or a wholly separate one — preserves a clean, single-purpose, revertable history, exactly matching what commit `0c79271` already demonstrated is this repo's preferred granularity.

Net: this spec's Python SDK change is `rate_limit_limit`/`rate_limit_remaining` only. The pre-existing `RateLimit-Reset` gap is tracked as a separate, small, no-spec-needed follow-up fix (bugfixes to already-shipped behavior don't need the design-spec-and-sign-off gate this document itself is going through — only architectural/wire changes do).

### 9. Rejected alternative: a client-capability-negotiation mechanism

An alternative design considered and rejected: instead of plain additive proto fields, add a negotiation signal — e.g. a `bool wants_limit_headers` field on `CheckRateLimitRequest`, or an `x-ratecap-accepts-limit-headers` request header the sidecar forwards — so an old client explicitly opts in/out rather than the server unconditionally emitting new data.

**Rejected, for concrete reasons specific to this repo, not as a generic "negotiation is always overkill" claim:**

- **Proto3 additive fields already solve the actual compatibility problem for free.** The real (and, per `deploy/helm/ratecap/templates/{core,sidecar}.yaml`, genuinely possible) version-skew scenario is a *transient* one: `core` and `sidecar` are separate Kubernetes `Deployment`s with independent `replicaCount`s in the Helm chart, so a rolling upgrade of one can momentarily run ahead of the other — a new sidecar talking to an old core, or vice versa, for the duration of one rollout. Proto3's additive-field semantics already make this safe with zero extra code: an old core simply never sets fields 5/6 (decoding to zero-value on a new sidecar, indistinguishable from "no data" — the truthful state); a new core's extra fields are silently skipped by an old sidecar's generated struct. A negotiation flag would add a whole extra request/response field, an extra branch on both sides, and a new "did the client actually set this correctly" failure mode — to re-solve a problem proto3's wire format already solves as a structural guarantee, not a best-effort convention.
- **No evidence anywhere in this repo's specs of a requirement for a long-lived mixed-version fleet.** Every prior spec's Build Order (Tier 2's, Tier 3's, the v3 roadmap's own phased rollout) describes core and sidecar shipping together from one repository, one Helm chart, one release train — there is no canary-core-vs-stable-sidecar strategy, no documented SLA for how long a skewed pair may coexist, and no existing negotiation mechanism anywhere in `proto/ratecap/v1/ratecap.proto` for any other field (contrast: `shadow_mode`/`RATECAP_SHADOW_MODE` is a *behavioral* rollout lever, not a wire-capability negotiation — it doesn't change what fields exist on the wire, only what a given response's `Action` value means). Introducing the *first* capability-negotiation primitive in this codebase to protect against a skew window that already self-heals within one rollout, with no stated requirement driving it, is complexity introduced to solve a problem that doesn't exist in this repo today — the definition of unjustified.
- **The SDK side has no proto contract to negotiate over anyway.** Per this repo's own Gotchas section, `packages/sdks/go`/`packages/sdks/python` are plain HTTP clients to the sidecar's `/check` wire format, with zero relation to the gRPC proto contract. A negotiation field on `CheckRateLimitRequest` would only ever govern the core↔sidecar gRPC hop; the SDK↔sidecar HTTP hop already tolerates unknown/absent headers for free (`if v := resp.Header.Get(...); v != "" { ... }` — the exact pattern `RetryAfterMs`/`RateLimitReset` parsing already uses on both SDKs), so there is no SDK-facing compatibility gap a negotiation mechanism would even address.

## Build Order

1. `services/core/store/lua/token_bucket.lua` — add the third return element (`math.floor(tokens)`).
2. `services/core/store/store.go`'s `StateStore.CheckAndDecrement` interface signature; `services/core/store/redis.go`'s `RedisStore.CheckAndDecrement` implementation and its `//go:embed`ded script's result-length check (`!= 2` → `!= 3`). Update the fake stores in `services/core/limiter/tokenbucket_test.go` and `services/core/limiter/property_test.go` to match the new 4-return signature.
3. `services/core/limiter/limiter.go` — add `Limit`/`Remaining int64` to `Decision`. `services/core/limiter/tokenbucket.go`'s `Check` — thread the store's new `remaining` value through, `Limit` from the already-`RLock`ed `burst` local. `ConcurrencyLimiter`/`FleetShedder` untouched.
4. Extend `services/core/limiter/pipeline_test.go`'s `TestPipeline_SecondTierRejectPropagatesDecision` and add `TestPipeline_FirstTierRejectPropagatesLimitAndRemaining` — regression guards, zero changes to `pipeline.go` itself (verify this stays true; if it doesn't, that's a sign the scoping assumption in Decision 5 needs re-checking before proceeding).
5. `proto/ratecap/v1/ratecap.proto` — add `int64 limit = 5;` / `int64 remaining = 6;` to `CheckRateLimitResponse`; regenerate via the repo's documented `protoc` command (`CLAUDE.md`'s Build & test section).
6. `services/core/grpcserver/server.go`'s `CheckRateLimit` — map `decision.Limit`/`decision.Remaining` onto the two new response fields.
7. `services/sidecar/proxy/proxy.go`'s real `REJECT_429` branch — emit `RateLimit-Limit`/`RateLimit-Remaining`, gated on `resp.Tier == "rate_limiter"`. Negative-cache branch untouched (Decision 5).
8. `packages/sdks/go/client.go` — `Ticket` gains `RateLimitLimit`/`RateLimitRemaining`; `Allow()`'s return tuple grows to 6 values; `Acquire()`'s `Ticket` construction updated to match. Mechanically update `cli/cmd/bench_run.go:273` and `deploy/sampleapp/main.go:72`'s call sites (append two more discarded `_`s), mirroring commit `0c79271`'s own diff shape.
9. `packages/sdks/python/src/ratecap/client.py` — `AllowResult`/`Ticket` gain `rate_limit_limit`/`rate_limit_remaining`; `allow()`/`acquire()` parse the two new headers. `RateLimit-Reset` parsing is explicitly **not** added here (Decision 8) — track it separately.
10. `deploy/ratecap.yaml` needs no changes (no new config keys — `Limit` is derived from the existing `default_burst`, not a new tunable). Re-verify the full `docker-compose` stack end-to-end (`cd deploy && bash generate-demo-certs.sh && docker compose up --build`), confirming real, decrementing `RateLimit-Remaining` values across repeated calls against a live Redis-backed Tier 1 bucket (see Testing Strategy's runtime proof).

## Testing Strategy

**Lua/Redis integration tests** (`services/core/integrationtests/store/redis_test.go`, real Redis via testcontainers, mirroring the file's existing `TestCheckAndDecrement_*` naming and `startRedis(t)` helper):
- `TestCheckAndDecrement_ReturnsRemainingAfterAllow` — burst 5, cost 1, assert the first call's `remaining == 4`.
- `TestCheckAndDecrement_RemainingStaysAtLastAllowedValueOnReject` — exhaust the burst, assert the rejecting 6th call's `remaining == 0`, not negative.
- `TestCheckAndDecrement_RemainingReflectsRefillOverTime` — exhaust the burst, sleep past one refill interval, assert `remaining` has increased on the next call (proves the 3rd return element comes from the same refill arithmetic already covered by this file's existing burst tests, not a separately-computed, possibly-inconsistent value).

**`TokenBucketLimiter` unit tests** (`services/core/limiter/tokenbucket_test.go`, extending the existing `fakeStore` to a 4-return signature):
- `TestTokenBucketLimiter_Check_ReportsLimitAndRemainingOnAllow`
- `TestTokenBucketLimiter_Check_ReportsLimitAndRemainingOnReject429`
- `TestTokenBucketLimiter_Check_ReportsUpdatedLimitAfterReconfigure` — construct with `burst=500`, call `Reconfigure(rate, 250, false)`, then `Check`, assert `Decision.Limit == 250` — proves `Limit` is read fresh from the mutex-guarded field at `Check`-time (the exact property `TestTokenBucketLimiter_Burst_ReturnsCurrentBurst` already proves for the `Burst()` accessor; this test proves it end-to-end through `Check`'s returned `Decision` instead).
- `TestTokenBucketLimiter_Check_SetRateDoesNotChangeLimit` — `SetRate` only ever touches `rate`, never `burst`; assert `Decision.Limit` is unchanged after a `SetRate` call, guarding against a future accidental coupling of the two.
- `TestTokenBucketLimiter_Check_OmitsLimitAndRemainingOnFailOpen` — fake store returns an error; assert `Action == ALLOW` (existing fail-open behavior, unregressed) and `Limit == 0`/`Remaining == 0`.

**Pipeline regression tests** (`services/core/limiter/pipeline_test.go`) — per Decision 6/Build Order step 4.

**Sidecar header-emission tests** (`services/sidecar/proxy/proxy_test.go`, mirroring the existing `TestServeHTTP_Reject429Returns429`'s `fakeRatecapClient{resp: &ratecapv1.CheckRateLimitResponse{...}}` + `httptest.NewRecorder()` pattern):
- `TestServeHTTP_Reject429FromRateLimiterSetsLimitAndRemainingHeaders` — `Tier: "rate_limiter", Limit: 500, Remaining: 0`; assert both new headers are set and correct.
- `TestServeHTTP_Reject429FromConcurrencyLimiterOmitsLimitAndRemainingHeaders` — `Tier: "concurrency_limiter"`; assert `rec.Header().Get("RateLimit-Limit")`/`"RateLimit-Remaining"` are both empty, while `Retry-After-Ms`/`RateLimit-Reset` are still set — the core Tier-scoping assertion this spec exists to get right.
- `TestServeHTTP_NegativeCacheShortCircuitOmitsLimitAndRemainingHeaders` — using `NewHandlerWithCache` with a pre-seeded `negativecache.Cache`; assert only `Retry-After-Ms`/`RateLimit-Reset` are set on the cache-hit path, documenting Decision 5's second scope boundary as a passing test, not just prose.

**Go SDK tests** (`packages/sdks/go/client_test.go`, mirroring `TestAllow_ReturnsFalseWithRetryAfterAndRateLimitResetOn429`/`TestAcquire_ReturnsRejectedTicketWithRetryAfterAndRateLimitResetOn429`):
- `TestAllow_ReturnsRateLimitLimitAndRemainingOn429`
- `TestAcquire_ReturnsRateLimitLimitAndRemainingOn429`

**Python SDK tests** (`packages/sdks/python/tests/test_client.py`, mirroring the existing `test_returns_false_with_retry_after_on_429`):
- `test_allow_returns_rate_limit_limit_and_remaining_on_429`
- `test_acquire_returns_rate_limit_limit_and_remaining_on_429`
- (no test added for `RateLimit-Reset` parsing — Decision 8)

**Runtime docker-compose proof** (manual verification before merge, mirroring `.github/workflows/ci.yml`'s `e2e-smoke` job's `curl -D <headers-file>` pattern against the same `deploy/docker-compose.yml` stack):
```bash
cd deploy && bash generate-demo-certs.sh && docker compose up --build -d
for i in 1 2 3; do
  curl -s -D "/tmp/hdrs_$i.txt" -o /dev/null "http://localhost:8080/check?key=ratelimit-headers-demo"
  grep -i "^RateLimit-" "/tmp/hdrs_$i.txt"
done
```
Expected: `RateLimit-Limit` constant across all three calls (the configured `default_burst`); `RateLimit-Remaining` strictly decreasing call-over-call against the same key, only appearing at all once a call actually gets rejected (per this spec's `REJECT_429`-only scope) — drive enough calls to trip Tier 1's configured burst first. Cross-check against `deploy/ratecap.yaml`'s `tiers.rate_limiter.default_burst` value directly, not just "some positive number."

## Out of Scope (this phase)

- **Emitting `RateLimit-Limit`/`RateLimit-Remaining` on `ALLOW` (`200 OK`) responses.** Matches `RateLimit-Reset`'s existing, already-accepted `429`-only scope; extending either header to success responses needs `Pipeline.Check` to thread `Limit`/`Remaining` through the `ALLOW` path without being overwritten by `lastTier`'s last-tier-wins semantics (see "Why This Phase Should Be (Mostly) Additive") — a real, separately-specable follow-up, not silently folded in here.
- **The negative-cache short-circuit path (`services/sidecar/proxy/proxy.go` lines 88-98) ever reporting `Limit`/`Remaining`.** `negativecache.Cache`'s data model (`map[string]time.Time`, no tier/limit/remaining fields) would need to change, and a cached `Remaining` would be stale by construction — a correctness argument against doing this at all, not just a diff-size argument for deferring it (Decision 5).
- **`RateLimit-Limit`/`RateLimit-Remaining` (or any header) for `concurrency_limiter` (Tier 2) or `fleet_shedder` (Tier 3) rejections.** Their analogous quantities (cap minus in-flight count) are a different metric family from a token-bucket window and must not reuse these header names — per the topic's own explicit mandate, reinforced by this spec's tier-scoping trace.
- **The Python SDK's pre-existing, unrelated `RateLimit-Reset` parsing gap.** Explicitly deferred to a separate, later `fix(sdk-python)` change — see Decision 8's full reasoning.
- **A client-capability-negotiation mechanism for these or any future header/field addition.** Rejected outright — see Decision 9.
- **Refactoring the Go SDK's `Allow()` from a growing return-tuple to a returned struct.** Flagged as worth reconsidering if a *third* header/field is ever added to this call, but out of scope for a two-header addition following an already-twice-established tuple-growth precedent (Decision 7).
- **Any config schema change.** `Limit` is derived from the already-configured `default_burst`; no new `ratecap.yaml` key, no `Config.Validate()` change.
- **The `RateLimit-Policy` header** some IETF draft revisions define in addition to `Limit`/`Remaining`/`Reset`. Not part of the gap this spec closes; no compliance claim beyond these two fields is made.
