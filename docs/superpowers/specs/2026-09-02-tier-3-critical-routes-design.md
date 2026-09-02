# Tier 3 — `critical_routes` Priority-Resolution Fallback — Design Spec

**Date:** 2026-09-02
**Status:** Proposed — awaiting owner sign-off (per this repo's CLAUDE.md "Scope discipline" section; no code has been written against this spec)
**Context:** Closes a fast-follow explicitly deferred by [`docs/superpowers/specs/2026-07-15-tier-3-fleet-shedder-design.md`](2026-07-15-tier-3-fleet-shedder-design.md) ("Out of Scope (this phase)": *"`critical_routes` static config-based priority resolution (v1 spec's fallback-order step 2) — deferred as a fast-follow"*). See [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](2026-07-13-ratecap-v1-design.md)'s "Priority / Criticality Tagging" section (lines 121-128) for the original 3-step fallback order this phase completes.

---

## Problem

The v1 design spec defines RateCap's priority-resolution order as three steps:

> 1. Per-request override: `x-ratecap-priority: critical|sheddable` header (HTTP)... checked by the sidecar first.
> 2. Static route-config match: `critical_routes` list in `ratecap.yaml` (route/method pattern).
> 3. Global `default_priority` (safe default: `sheddable`...).

Verified against the current codebase:

- **Step 1 (header) is fully built and load-bearing.** `services/sidecar/proxy/priority.go`'s `ResolvePriority(headerValue string, defaultPriority Priority) Priority` is called from `services/sidecar/proxy/proxy.go:76` and its result is threaded into `CheckRateLimitRequest.Priority` (proxy.go:151).
- **Step 2 (`critical_routes`) does not exist anywhere in the Go code.** `grep -rn "critical_routes\|CriticalRoutes"` across the entire repo (all `.go`, `.yaml`, `.yml` files) returns zero matches outside the v1 design spec's own prose (`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md:126,151-153`) and the Tier 3 spec's "Out of Scope" line noting the deferral. `services/core/config/config.go`'s `FleetShedderConfig` struct (lines 29-34) has no `CriticalRoutes` field, and `deploy/ratecap.yaml` has no `critical_routes:` key.
- **Step 3 (global default) is built but hardcoded, not operator-configurable.** `services/sidecar/main.go:248` calls `proxy.NewHandlerWithCache(client, proxy.Sheddable, shedder, negativecache.New())` — `proxy.Sheddable` is a Go constant, not read from any config or env var. `SECURITY.md:85` already documents this as intentional: *"There is currently no operator-configurable default: `ratecap-sidecar` always falls back to `sheddable` (`proxy.Sheddable`, hardcoded in `services/sidecar/main.go`)... the safe choice."* This phase does not change step 3; it is confirmed working as designed.

**Two architectural facts that reshape what "route" can mean here**, both re-verified directly against the code:

1. **There is no "route" concept anywhere on the wire today.** `/check` is not a transparent reverse proxy. `services/sidecar/proxy/proxy.go`'s `Handler.ServeHTTP` reads only `key`, `cost`, and `skip_reservations` from the query string and `x-ratecap-priority` from a header (proxy.go:70-76, 127); it never receives or inspects the caller's *real* HTTP method or path — callers call `GET /check?key=<opaque-key>` themselves, separately from whatever real request they're protecting. `proto/ratecap/v1/ratecap.proto`'s `CheckRateLimitRequest` (lines 33-38) has exactly four fields — `key`, `cost`, `skip_reservations`, `priority` — no `method`/`path`/`route` field, and no gRPC change is proposed here.
2. **`ratecap.yaml` (where the v1 spec's schema places `critical_routes`) is loaded only by `services/core`, never by `services/sidecar`.** `services/core/main.go:184` calls `config.Load(configPath)` and `main.go:258` calls `config.Watch(configPath, ...)`; `services/sidecar` has zero import of `services/core/config` and zero `RATECAP_CONFIG_PATH`-equivalent env var (confirmed against the full env-var list `services/sidecar/main.go` reads: `RATECAP_CORE_ADDR`, `RATECAP_SHARED_SECRET`, `RATECAP_ADMIN_SECRET`, `RATECAP_TLS_*`, `RATECAP_MAX_INFLIGHT_REQUESTS`, `RATECAP_SHED_RAMP_START_PCT`, `RATECAP_SIDECAR_MAX_RPS`, `RATECAP_PPROF_ENABLED`, `RATECAP_SIDECAR_ADDR` — no config-file path among them). The v1 spec's own claim that "each `ratecap-sidecar` pulls its local-enforcement config... from `ratecap-core` at startup and on the `sync_rate` interval" (v1 spec line 131) was never built — `CLAUDE.md`'s "Gotchas" section confirms `sync_rate` itself was deleted as vestigial, and the Tier 4 spec's Decision #3 (`2026-07-15-tier-4-worker-shedder-design.md:66-68`) independently confirms the same core-to-sidecar config-sync mechanism was never built and was deferred to v2, precedent this spec follows rather than reopens.

Where a route match would need to plug in mechanically is also confirmed: `ResolvePriority`'s three-way `switch` on `headerValue` (priority.go:13-20) already ends in a `default: return defaultPriority` — step 2 is exactly the missing branch between the header cases and that default.

## Why This Phase Should Be (Mostly) Additive

Every seam this phase touches was built to be extended exactly this way, or is confirmed untouched:

- **`ResolvePriority` already has a `default` fallthrough case** (priority.go:18-19) that is precisely where a route-match check inserts — no restructuring of the existing header logic, only one new check between the existing `switch` and its final `return defaultPriority`.
- **`services/sidecar/proxy/proxy.go`'s existing two-constructor pattern for `Handler`** (`NewHandler` at line 51, `NewHandlerWithCache` at line 58) was deliberately built with a documented rationale — *"kept as a separate constructor... so every existing call site of NewHandler keeps compiling unchanged"* (proxy.go:55-57) — for exactly the situation this phase is in again: adding one more optional dependency without breaking existing callers.
- **The sidecar already depends on `fsnotify` and already has a working, atomic-swap, hot-reload-from-a-local-file pattern in its own module**: `services/sidecar/tlsconfig/reload.go`'s `watchCert` (lines 18-63) watches a file's directory, reloads on write, and atomically swaps an `atomic.Pointer`-held value with a "log and keep last-known-good" failure mode (line 46) — a closer, lower-risk precedent than reaching across to `services/core/config/watcher.go`'s structurally similar but core-only pattern.
- **`services/core` needs zero changes.** `Priority` is already a cross-service enum (`limiter.Priority`, aliased into `proxy.Priority` at priority.go:5) that core's `FleetShedder` (per the Tier 3 spec) already treats as an opaque, already-resolved value — core has no notion of *how* the sidecar arrived at `Critical` vs. `Sheddable`, and this phase does not change that contract. No proto change, no `services/core` change, no re-verification of `FleetShedder`'s own already-tested logic is needed.
- **The Go SDK's functional-options pattern (`packages/sdks/go/client.go`'s `CheckOption`, `WithCost`, `WithPriority`, `applyCheckOptions`/`applyToRequest` at lines 35-67) is built precisely for adding one more optional, additive knob** — a `WithRoute` option follows `WithPriority`'s exact shape with zero signature break to `Allow`/`Acquire`.

## Key Design Decisions

### 1. Where the `critical_routes` list lives and how it reaches the sidecar (the decision this spec must resolve, not assume)

Three real options, given fact 2 above (core's `ratecap.yaml` is unreachable from the sidecar today):

**Option A — a new sidecar-local config file with its own fsnotify-based hot-reload watcher.** A new file (e.g. `critical-routes.yaml`), read once at startup and hot-reloaded on change via a new `services/sidecar/criticalroutes` package, structurally mirroring `services/sidecar/tlsconfig/reload.go`'s already-proven pattern (watch the file's directory, reload+atomically-swap on write, log-and-keep-last-known-good on a parse failure — see `TestLoad_KeepsLastKnownGoodOnReloadFailure`, `tlsconfig/reload_test.go:148`, as the exact test-shape precedent).
- **Pros:** A real, unbounded-size, human-reviewable route list; hot-reloadable without a sidecar restart (matching the responsiveness every other config-hot-reload path in this project already has — `services/core/config.Watch` for core's tiers, `tlsconfig.Load`'s cert watcher for TLS); zero new cross-service dependency — the sidecar's own availability still never depends on core being reachable to resolve priority, which is true today (`grpc.NewClient`'s dial is lazy — see `newHealthzHandler`'s comment at `main.go:33-37` about `Idle` not meaning "down"); reuses an already-vetted dependency (`fsnotify` is already in `services/sidecar/go.mod:6`) and an already-reviewed code shape from the same module.
- **Cons:** A second config file per sidecar replica to keep in sync fleet-wide — but this is not a new class of operational concern: `deploy/docker-compose.yml` already mounts per-replica files this way for TLS certs (`./certs:/etc/ratecap/certs:ro`, line 51) and for core's `ratecap.yaml` (line 28), so a `critical-routes.yaml` volume mount follows an established pattern, not a novel one. Requires a new env var (`RATECAP_CRITICAL_ROUTES_PATH`) and new deploy wiring.

**Option B — environment-variable-encoded list** (e.g. `RATECAP_CRITICAL_ROUTES="POST /v1/charges,POST /v1/payment_intents"`).
- **Pros:** Zero new dependency, zero new file, matches the sidecar's existing all-env-var config story exactly (`RATECAP_MAX_INFLIGHT_REQUESTS`, `RATECAP_SHED_RAMP_START_PCT`, `RATECAP_SIDECAR_MAX_RPS` are all read once at `main()` startup via `resolveMaxInflight`/`resolveRampStartPct`/`resolveMaxRPS`, `main.go:69-115`).
- **Cons — this is the option this spec rejects.** Env vars in this codebase are read once at process start (no equivalent of `config.Watch`); changing a `critical_routes` list would require a full sidecar redeploy, not a config push — a real regression against how responsively operators can already change adjacent knobs (core's `fleet_shedder.reserved_critical_pct` is hot-reloadable via `config.Watch`, and even has a sub-second `/admin/set-limit` override per `SECURITY.md:71`). A delimited env var also degrades badly at realistic list sizes — the v1 spec's own example already has 2 entries, and a real payments API's critical-route list plausibly has a dozen-plus, which turns into an unreadable single line in a PR diff and an easy place to typo a delimiter. This is disproportionate to how much simpler it looks: it trades away hot-reload for no real simplification, since Option A's file format and matching logic are equally simple either way.

**Option C — finally build the real core-to-sidecar config-sync mechanism the v1 spec originally proposed**, and put `critical_routes` in `ratecap.yaml` as originally sketched (v1 spec lines 131, 151-153).
- **Pros:** Matches the v1 spec's original literal design and file location; single source of truth across the fleet, no second file per sidecar.
- **Cons — this is the option this spec rejects, and is disproportionate on its own terms.** This mechanism has already been explicitly and repeatedly declined in this repo's own history: `CLAUDE.md`'s "Gotchas" section documents `sync_rate` (the field that would have driven this sync) as deleted outright rather than wired up; the Tier 4 spec's Decision #3 (`2026-07-15-tier-4-worker-shedder-design.md:66-68`) named this exact mechanism and deferred it to v2; Tier 3 itself left `FleetShedderConfig.DefaultPriority` as an analogous tracked, unbuilt gap rather than building the sync to close it. Building it now, solely to serve a 2-line example route list, means designing and shipping a new core-to-sidecar RPC/polling surface — staleness semantics, behavior during a core outage mid-sync, a new "config API" the sidecar now depends on for something it works fine without today — as a side effect of what should be a narrow, additive feature. It would also retroactively invite (without actually resolving) wiring `FleetShedderConfig.DefaultPriority` and Tier 4's thresholds through the same new mechanism "while we're at it," which is exactly the kind of scope creep `CLAUDE.md`'s "Scope discipline" section gates behind its own dedicated spec.

**Recommendation: Option A.** It is the only option that is both hot-reloadable and proportionate — it reuses an existing dependency and an existing, already-tested code pattern from the same module (`tlsconfig/reload.go`), adds no cross-service coupling, and does not reopen the config-sync question this repo has already deferred twice. Concretely:

- New package `services/sidecar/criticalroutes` (mirroring the sidecar's existing one-word, no-underscore package naming: `negativecache`, `decisionlog`, `tlsconfig`, `otelinit`):
  ```go
  package criticalroutes

  type Set struct {
      routes atomic.Pointer[map[string]struct{}]
  }

  func Load(path string) (map[string]struct{}, error)      // parses `critical_routes: [...]` YAML into a set
  func Watch(path string) (*Set, func(), error)             // initial Load + fsnotify hot-reload, mirroring tlsconfig/reload.go's watchCert
  func (s *Set) Contains(route string) bool                 // nil-receiver-safe: false if s == nil or route == ""
  ```
- File format reuses the v1 spec's own key name and literal example verbatim (`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md:151-153`), just in its own file rather than inside `ratecap.yaml`:
  ```yaml
  critical_routes:
    - "POST /v1/charges"
    - "POST /v1/payment_intents"
  ```
- New env var `RATECAP_CRITICAL_ROUTES_PATH`, read once in `services/sidecar/main.go` alongside the other `RATECAP_*` reads. **Optional, not required**: if unset, `main.go` logs `"RATECAP_CRITICAL_ROUTES_PATH not set — critical_routes priority-resolution fallback (step 2) is disabled; only header (step 1) and default (step 3) apply"` and passes a `nil *criticalroutes.Set` into `Handler` — `Set.Contains`'s nil-receiver safety means this is a true no-op, not a special case threaded through `ServeHTTP`. This keeps every existing deployment's behavior byte-for-byte unchanged unless an operator opts in.
- Deliberately **no debounce window** in `criticalroutes.Watch`, unlike `services/core/config/watcher.go`'s `debounceWindow` (config/watcher.go:14). `tlsconfig/reload.go`'s simpler, non-debounced watch loop is the closer structural precedent, and a route-allowlist file changes at ops cadence (a human editing it during an incident or a deploy), not at a cadence where a single logical write's multiple fsnotify events would cause a user-visible double-reload cost worth guarding against. If this assumption turns out wrong in practice, adding a debounce window later is a small, isolated change to `Watch` alone.

### 2. Wire format: a new HTTP header, not a proto field

`WithRoute`'s value crosses the sidecar's HTTP wire the same way `WithPriority`'s already does — a new header, `x-ratecap-route`, read by `Handler.ServeHTTP` alongside the existing `x-ratecap-priority` read (proxy.go:76). **No proto change.** `route` never reaches `CheckRateLimitRequest` or crosses the sidecar↔core gRPC boundary at all — priority resolution (all three steps) is fully resolved sidecar-side into the existing `Priority priority = 4` field (`ratecap.proto:37`) before the gRPC call is made. This is consistent with fact 1 above: there is no route concept on the wire, and this phase does not introduce one — it only gives the sidecar one more *local* signal to resolve the enum value it already sends.

The header carries the exact literal string the caller declares, matching the config file's own entries verbatim (e.g. `x-ratecap-route: POST /v1/charges`) — the caller states its own route label; the sidecar never derives it from a real request line, per fact 1.

### 3. Match semantics: exact `METHOD PATH` string match only

`Set.Contains(route string) bool` does a plain map lookup — no glob, no prefix, no path-parameter templating (e.g. no `POST /v1/charges/:id`). This matches the v1 spec's own literal example (`"POST /v1/charges"`, not a pattern), and deliberately does not invent syntax the deferred spec item never specified. A `POST /v1/charges/refund` request with a route label that isn't byte-identical to a configured entry does **not** match — this is a required negative test (see Testing Strategy), because silent prefix-matching would be a meaningfully different, undocumented feature, not what step 2 was scoped to be.

### 4. `ResolvePriority`'s new signature and the precedence contract

`services/sidecar/proxy/priority.go` changes from:

```go
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

to:

```go
func ResolvePriority(headerValue string, routeMatched bool, defaultPriority Priority) Priority {
    switch headerValue {
    case "critical":
        return Critical
    case "sheddable":
        return Sheddable
    }
    if routeMatched {
        return Critical
    }
    return defaultPriority
}
```

`ResolvePriority` stays a pure function — `routeMatched` is a caller-computed `bool`, not a `*criticalroutes.Set` passed in, so priority_test.go's existing table-style tests need no fake/mock, only a new boolean parameter. `Handler.ServeHTTP` becomes the one call site that bridges the two:

```go
routeMatched := h.criticalRoutes.Contains(r.Header.Get("x-ratecap-route"))
priority := ResolvePriority(r.Header.Get("x-ratecap-priority"), routeMatched, h.defaultPriority)
```

**Precedence is explicit and load-bearing, not incidental:** an unrecognized or empty `headerValue` (today's `default` branch) now falls through to the route check *before* the global default — this is the literal 3-step order (header, then route, then default), not a 2-step order with route bolted on as a second default. A concrete consequence worth stating plainly: a request with an **invalid** `x-ratecap-priority` value (e.g. a typo) now resolves via route-match if one exists, where today it falls straight to the global default — this is a deliberate behavior change, aligned with the v1 spec's fallback order (an unrecognized header is not a valid override, so it should be treated exactly like an absent one, not skip a fallback step a syntactically-empty header wouldn't skip). The existing `TestResolvePriority_InvalidHeaderFallsBackToDefault` (`priority_test.go:30-35`) still passes once updated for the new signature, since it calls with no route configured (`routeMatched=false` in the updated call).

**This is an internal, unexported-package signature change, not a wire or SDK contract break.** `ResolvePriority` is called only from `services/sidecar/proxy/proxy.go` and its own test file — it is not part of any proto, any SDK's public API, or any cross-module contract, so it carries none of the "pre-1.0, no deprecation shim" weight the Tier 3 spec's proto rename (Decision #3) had to reason about. It is a pure internal refactor.

### 5. `Handler` gains a third optional dependency via a new additive constructor

Following the exact precedent `proxy.go:55-57`'s own comment documents (`NewHandlerWithCache` exists specifically so `NewHandler`'s existing callers keep compiling), this phase adds one more additive constructor rather than changing either existing one's signature:

```go
func NewHandlerWithCriticalRoutes(client ratecapClient, defaultPriority Priority, shedder *worker.Shedder, cache *negativecache.Cache, criticalRoutes *criticalroutes.Set) *Handler
```

`services/sidecar/main.go`'s one real call site (`main.go:248`, currently `NewHandlerWithCache(...)`) switches to this new constructor. **Explicitly not pursued in this phase:** converting `Handler`'s two-constructor pattern into a full `HandlerOption` functional-options constructor. That refactor becomes worth doing once a *fourth* optional dependency shows up — two constructors already tolerate two feature combinations (this phase's addition is a straightforward third), and restructuring the constructor shape is a larger, orthogonal change with no test-coverage or behavior benefit on its own. Noted here as a deliberate YAGNI call, not an oversight.

### 6. Go SDK: `WithRoute`, fully additive

`packages/sdks/go/client.go` gains, mirroring `WithPriority` (client.go:48-50) exactly:

```go
func WithRoute(route string) CheckOption {
    return func(o *checkOptions) { o.route, o.hasRoute = route, true }
}
```

`checkOptions` (client.go:35-40) gains `route string` / `hasRoute bool` fields; `applyToRequest` (client.go:60-67) gains:

```go
if o.hasRoute {
    req.Header.Set("x-ratecap-route", o.route)
}
```

Zero signature break to `Allow`/`Acquire` — both already take `opts ...CheckOption` (client.go:73, 185). This confirms the prior research brief's claim for the Go SDK specifically.

### 7. Python SDK: a plain keyword argument, **not** a `WithRoute(...)`-style option — correcting the brief

The prior research brief's framing ("a new `WithRoute(...)` option would be fully additive... in both Go and Python SDKs") does not hold as written for Python once the actual code is read. `packages/sdks/python/src/ratecap/client.py` has no functional-options pattern at all — `Client.allow(self, key, cost=None, priority=None)` (client.py:91) and `Client.acquire(self, key, cost=None, priority=None)` (client.py:106) are plain functions with optional keyword arguments; there is no `CheckOption`-equivalent builder type to extend. The correct, equally-additive Python change is a third optional keyword parameter matching the existing style exactly:

```python
def allow(self, key, cost=None, priority=None, route=None):
    ...
    headers = {}
    if priority:
        headers["x-ratecap-priority"] = priority
    if route:
        headers["x-ratecap-route"] = route
```

(and the same `route=None` addition to `acquire`). This is still fully additive/non-breaking — existing positional and keyword callers are unaffected — but it is a different *shape* of additive change than Go's, and this spec calls that out explicitly rather than letting "both SDKs get a `WithRoute`" ship as an inaccurate implementation instruction.

### 8. Security posture: identical trust boundary to the existing `x-ratecap-priority` header, not a new one

`SECURITY.md`'s existing "Priority Claims (v1)" section (lines 75-85) already documents that `x-ratecap-priority` is caller-supplied "with no authentication, no cost, and no verification," and that this is v1's accepted, intentional trust boundary (any caller that can reach the sidecar is already inside the trusted network the sidecar itself depends on). A new `x-ratecap-route` header claiming route membership is the same shape of claim through a different door: a malicious caller could already set `x-ratecap-priority: critical` directly today with zero cost, and after this phase could equivalently set `x-ratecap-route: POST /v1/charges` to reach the same outcome indirectly if that route is configured critical. **This is not a new vulnerability class** — it inherits, byte-for-byte, the trust boundary already accepted and documented for the header this phase's new header sits next to — but `SECURITY.md` should gain one sentence under "Priority Claims (v1)" noting that `x-ratecap-route` is governed by the identical boundary, so the security posture stays explicit rather than silently implied. This is a documentation update alongside the code change, not a design change to the trust model itself.

### 9. Rejected alternative (a): reinterpreting "route" as "rate-limit key"

A cheaper implementation exists: skip the new header and config file entirely, and instead let operators list `req.Key` values (already flowing through `/check?key=...`) in a "critical keys" config. This is **rejected** as scope-substitution, not scope-reduction. `req.Key` (an account ID, API key, or similar caller identity — Tiers 1-3 already key every prior mechanism off it) and "route" (an HTTP method+path, per the v1 spec's own `"POST /v1/charges"` example) are different axes entirely: a caller identified by `Key: "acct_123"` calling the genuinely non-critical `GET /v1/balance` is not the same thing as `POST /v1/charges` being called by any account. Silently repurposing "route" to mean "key" would let this feature pass a shallow "does `critical_routes`-style matching exist now" check while implementing something the deferred spec item never asked for — a caller in a "critical accounts" list would get blanket critical treatment for every endpoint they call, and the actual charges/payment-intents endpoints named in the v1 spec's example would get no elevation at all for callers not on that list. Rejected because it solves an easier, different problem under the same name.

### 10. Rejected alternative (b): making the sidecar a transparent reverse proxy to auto-derive real routes

The literal reading of "route" as a real HTTP method+path on the caller's actual request is technically closer to the v1 spec's wording, but requires giving `ratecap-sidecar` reverse-proxy semantics it has never had: upstream target resolution, path handling, timeout/retry/streaming passthrough policy, and (per fact 1 above) a change to every caller's integration model, since today's callers make their own real request separately from their `/check` call — `services/sidecar/proxy/proxy.go`'s `Handler` is confirmed to be decision-only, never forwarding or seeing the caller's real request. This is a materially larger architectural change than "add a fallback priority-resolution step" — it is itself a new mechanism squarely inside `CLAUDE.md`'s "Scope discipline" gate ("Don't add a 5th limiting mechanism... without the same spec-first process"), needing its own dedicated design spec and sign-off, not something to adopt as an implementation detail of this one. **Rejected for this phase.** If a future spec proposes a reverse-proxy mode for RateCap for independent reasons, real-route auto-derivation could ride along with it then.

## Build Order

Following the walking-skeleton-style incremental granularity established by prior tier specs:

1. `services/sidecar/criticalroutes` package: `Load`, `Watch`, `Set.Contains` — pure unit tests (temp files via `t.TempDir()`, no HTTP/gRPC), mirroring `tlsconfig/reload_test.go`'s shape (`TestLoad_KeepsLastKnownGoodOnReloadFailure`-equivalent for a malformed-YAML reload; a file-change-triggers-reload test equivalent to `TestLoad_GetClientCertificateReturnsCurrentCertAfterFileChange`; a `Stop`-ends-the-watcher test equivalent to `TestLoad_StopEndsTheWatcher`). Include the exact-match-only negative test from Decision #3 (`POST /v1/charges/refund` must not match a configured `POST /v1/charges` entry).
2. `services/sidecar/proxy/priority.go`: update `ResolvePriority`'s signature to `(headerValue string, routeMatched bool, defaultPriority Priority) Priority` per Decision #4. Update the four existing tests in `priority_test.go` to the new signature; add the new precedence tests (full list in Testing Strategy below).
3. `services/sidecar/proxy/proxy.go`: `Handler` gains `criticalRoutes *criticalroutes.Set`; new `NewHandlerWithCriticalRoutes` constructor per Decision #5; `ServeHTTP` computes `routeMatched` from `r.Header.Get("x-ratecap-route")` and threads it into the updated `ResolvePriority` call. Handler-level tests in `proxy_test.go` proving the same precedence table end-to-end via `fakeRatecapClient.lastReq.Priority` (mirroring the existing `TestServeHTTP_ThreadsCriticalPriorityHeaderIntoRequest` pattern).
4. `services/sidecar/main.go`: read optional `RATECAP_CRITICAL_ROUTES_PATH`; when set, call `criticalroutes.Watch` and defer its stop func (mirroring the existing `stopCertWatch`/`defer` pattern at `main.go:225`); when unset, log and pass `nil`. Switch the one call site at `main.go:248` to `NewHandlerWithCriticalRoutes`.
5. Go SDK (`packages/sdks/go/client.go`): add `WithRoute` per Decision #6. Unit test `TestAllow_WithRoute_SendsRouteHeader` and `TestAcquire_WithRoute_SendsRouteHeader`, mirroring the existing `TestAllow_WithPriority_SendsPriorityHeader` (`client_test.go:306`).
6. Python SDK (`packages/sdks/python/src/ratecap/client.py`): add `route=None` to `allow`/`acquire` per Decision #7. Unit test `test_allow_sends_route_header_when_given`, mirroring `test_allow_sends_priority_header_when_given` (`test_client.py:229`).
7. `SECURITY.md`: one-sentence addition to "Priority Claims (v1)" per Decision #8.
8. `deploy/`: add `deploy/critical-routes.yaml` with the v1 spec's literal example entries; add `RATECAP_CRITICAL_ROUTES_PATH` + a matching volume mount to `deploy/docker-compose.yml`'s `sidecar` service (mirroring the existing `./ratecap.yaml:/etc/ratecap/ratecap.yaml` mount shape at line 28); extend `deploy/sampleapp/main.go`'s demo (either a new endpoint or an addition to `/fleet-demo`) to demonstrate step 2 specifically: a request that sets `x-ratecap-route: POST /v1/charges` and **no** `x-ratecap-priority` header still gets critical treatment. Re-verify the full docker-compose stack end-to-end.

## Testing Strategy

- **`services/sidecar/criticalroutes` unit tests** (pure, no HTTP/gRPC/Redis): `Load` parses a valid file into the expected set; `Load` returns an error on malformed YAML; `Contains` is exact-match only (positive: configured entry matches; negative: a path that merely starts with a configured entry does **not** match); `Watch` reloads and atomically swaps on a file write; `Watch` logs and keeps the last-known-good set on a subsequent malformed write (mirroring `tlsconfig/reload_test.go:148`'s `TestLoad_KeepsLastKnownGoodOnReloadFailure`); `Contains` on a `nil *Set` returns `false` without panicking.
- **`ResolvePriority` precedence table** (`priority_test.go`), covering every combination the 3-step order implies — this is the specific, required test set proving header precedence over route match, not just that route match works in isolation:
  | `headerValue` | `routeMatched` | expected |
  |---|---|---|
  | `"critical"` | `false` | `Critical` (existing) |
  | `"sheddable"` | `false` | `Sheddable` (existing) |
  | `""` | `false` | `defaultPriority` (existing) |
  | `"garbage"` | `false` | `defaultPriority` (existing, updated signature) |
  | `""` | `true` | `Critical` — **route match applies when header is absent** |
  | `"garbage"` | `true` | `Critical` — **an invalid header falls through to route match, not straight to default** |
  | `"sheddable"` | `true` | `Sheddable` — **header explicitly outranks a route match; the required precedence-ordering test** |
  | `"critical"` | `true` | `Critical` (both agree; trivial but included for table completeness) |
- **Handler-level integration tests** (`proxy_test.go`, fake client asserting `lastReq.Priority`): `TestServeHTTP_ThreadsRouteMatchIntoRequestWhenNoPriorityHeader` and `TestServeHTTP_HeaderPriorityOutranksRouteMatch` (explicit sheddable header + a matching critical route still resolves to `SHEDDABLE` on the wire to core) — the same precedence contract proven at the real handler layer, not just the pure function.
- **Go SDK unit tests**: `WithRoute` sets the `x-ratecap-route` header on both `Allow` and `Acquire`, mirroring the existing `WithPriority` test shape exactly.
- **Python SDK unit tests**: `route=` sets the `x-ratecap-route` header on both `allow` and `acquire`, mirroring the existing `priority=` test shape exactly.
- **Runtime docker-compose proof**, extending the same style Tier 3/Tier 4 used to prove their own mechanisms end-to-end (not just unit tests): with `deploy/critical-routes.yaml` configuring `POST /v1/charges` as critical, (1) a burst of requests that set `x-ratecap-route: POST /v1/charges` and no priority header keep succeeding under fleet pressure that sheds unlabeled/sheddable-labeled peers — proving step 2 actually resolves to critical treatment live; (2) a request that sets **both** `x-ratecap-priority: sheddable` **and** a matching `x-ratecap-route: POST /v1/charges` still gets shed under the same pressure — proving precedence ordering holds at the real wire level, not merely in unit tests, which is the specific runtime proof this spec's task called for.
- **No new Redis/testcontainers integration test is needed.** This phase is entirely sidecar-local priority *resolution* — it never touches `FleetShedder`, `IncrConcurrent`/`DecrConcurrent`, or `concurrent_limiter.lua`. Once `Priority` is resolved to `Critical`/`Sheddable` sidecar-side, everything downstream is exactly the already-tested `FleetShedder` path the Tier 3 spec's own testing section covers, unchanged by this phase.

## Out of Scope (this phase)

- Any change to `services/core` — `FleetShedder`, the `Priority` proto enum, and `CheckRateLimitRequest` are all confirmed unchanged; priority resolution is entirely a sidecar-side concern, and core already treats `Priority` as an opaque, pre-resolved value regardless of how the sidecar arrived at it.
- A real reverse-proxy mode that auto-derives HTTP method+path from the caller's actual request (Rejected Alternative b) — matches the v1 spec's literal wording more closely, but is a disproportionate new architectural mechanism needing its own spec-first gate under `CLAUDE.md`'s "Scope discipline."
- Repurposing `req.Key` as the critical-route signal (Rejected Alternative a) — cheaper, but silently solves a different, easier problem than the one the v1 spec's `critical_routes` example describes.
- The full core-to-sidecar config-sync mechanism (Decision #1, Option C) — still deferred, consistent with the Tier 3 and Tier 4 specs' precedent; not reopened here.
- Glob/prefix/regex/path-parameter route matching — exact `METHOD PATH` string match only, matching the v1 spec's own literal example; no invented pattern language for a feature scoped narrowly to close a fast-follow.
- A configurable header name for `x-ratecap-route` — hardcoded, mirroring `x-ratecap-priority`'s own existing hardcoded-name precedent; no known use case for renaming it.
- Any new authorization/verification of the caller-supplied route label beyond what `x-ratecap-priority` already has (or lacks) today — Decision #8 documents why this is an inherited, not new, trust boundary; hardening it is a v2-scale change per `SECURITY.md`'s existing "deferred to v2" language for priority claims generally.
- A debounce window in `criticalroutes.Watch` — `tlsconfig/reload.go`'s simpler non-debounced pattern is the chosen precedent (Decision #1); revisit only if this file's real-world edit cadence turns out to warrant it.
- Converting `Handler`'s constructor set into a full `HandlerOption` functional-options pattern — a third additive constructor (Decision #5) is proportionate for a third optional dependency; the bigger refactor is deferred until a fourth arrives.
