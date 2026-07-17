# RateCap v2 Phase 2: Developer Tooling — Design Spec

**Date:** 2026-07-17
**Status:** Approved
**Context:** Second of a 4-phase v2 roadmap. Phase 1 (observability, structured logging, optional mTLS) shipped as v2.0.0. This phase covers a `ratecapctl` CLI and a Python SDK — both promised by the original v1 design spec but never built.

---

## Problem

RateCap's original v1 design spec sketched a `ratecapctl` CLI ("config validate, live-tail decisions, benchmark runner") and mentioned multi-language SDK expansion, but neither was ever built:

- There is zero CLI tooling anywhere in the repo — no `cli/` directory, no Cobra/urfave/kong dependency, and no benchmark infrastructure (`find . -iname "*bench*"` and `grep -rl "testing.B"` both return zero matches).
- The only SDK is `packages/sdks/go`. Operators or teams not using Go have no client at all.

Research findings that settle this phase's key decisions (see the v2 roadmap's research summary for full detail; this document only restates what's load-bearing for design choices below):

- Cobra is kubectl's actual foundation (verified against kubectl's own source, not assumed), actively maintained, and the right fit for a multi-domain tool like `ratecapctl` — redis-cli's flat single-binary style fits a single-protocol client, not this.
- The SDK wire boundary is plain HTTP with 2 endpoints (`GET /check`, `POST /release`) and a handful of headers — not gRPC/protobuf. A second-language SDK is a small, direct, hand-written port, not a generated-from-proto artifact.

## Key Design Decisions

### 1. `ratecapctl` — new top-level `cli/` module, Cobra, kubectl-style subcommands

Lives at `cli/`, a peer of `services/`, `packages/`, `proto/`, `deploy/` in `go.work` — matching the original v1 spec's directory sketch exactly. It is an operator tool that *consumes* `packages/sdks/go`, not an SDK itself, so it does not belong under `packages/sdks/`.

Built on **Cobra**. Subcommands follow kubectl's verb-noun structure:

**`ratecapctl config validate <path>`** — a thin wrapper around the already-existing, already-tested `services/core/config.Load(path)` + `(*Config).Validate()`. Zero new validation logic. Exits non-zero with the validator's own error message on an invalid config; exits 0 with a confirmation message on success.

**`ratecapctl bench run`** — closes the gap the v1 spec's Testing Strategy section promised ("hammers the sidecar, produces the P99/P999 latency numbers published in the README") but never delivered. Reuses `packages/sdks/go`'s `Allow`/`Acquire`/`Ticket.Release` API directly rather than reimplementing HTTP calls.

Flags:
- `--sidecar-addr` (default `http://localhost:8080`) — target sidecar.
- `--concurrency` (default a small sane value, e.g. `10`) — number of parallel worker goroutines.
- `--requests` (default e.g. `1000`) — total request count across all workers (simpler to reason about and to reproduce than a duration-based run).
- `--key-prefix` (default `bench`) — each worker generates keys as `{prefix}-{worker-id}-{seq}`, so concurrent workers don't collide on Tier 2/3's per-key concurrency caps unless the caller deliberately wants that contention (achievable by setting a fixed, shared prefix pattern with low cardinality).
- `--acquire` (bool, default false) — when set, benchmark `Acquire()`/`Ticket.Release()` (exercises Tier 2's concurrency-limiter path) instead of the default `Allow()` (Tier 1 only, no release bookkeeping).
- `--json` (bool, default false) — emit a machine-readable JSON summary instead of the human-readable stdout report. This is what would let a future Phase 4 "published benchmarks" effort regenerate numbers programmatically rather than requiring a manual paste.

Output (human-readable default): total requests, elapsed wall time, throughput (req/s), and P50/P99/P99.9 latency in milliseconds. The `--json` form carries the same fields as a flat JSON object.

**`ratecapctl decisions tail` is explicitly OUT OF SCOPE for this phase.** Building a live-streaming endpoint on the sidecar (SSE or chunked HTTP) purely to serve this one CLI command would be new sidecar surface area invented speculatively — no other consumer needs it today, and Phase 1's own plan already flagged this exact command as a YAGNI risk. No stub command is added either: a command that exists in `--help` but errors with "not yet implemented" is dead CLI surface that misleads users about what's actually usable. If a real need for live-tailing emerges, it becomes its own small, focused follow-up phase with a concrete driver.

### 2. Python SDK — `packages/sdks/python/`, zero dependencies, context-manager `Ticket`

Lives at `packages/sdks/python/`, a peer of `packages/sdks/go/`. Python 3.10+ floor. Zero third-party dependencies — uses the standard library's `urllib.request` rather than adding `requests` as a dependency for what is a 2-endpoint HTTP client.

API, mirroring the Go SDK's method names and semantics:

- `allow(key: str) -> AllowResult` — `GET {sidecar_addr}/check?key=<url-escaped>&skip_concurrency=true`. Returns an object with `allowed: bool` and `retry_after_ms: int`. Tier-1-only, no paired release, matching the Go SDK's own `Allow` exactly (including the reasoning documented there: acquiring a Tier-2 slot with no paired release would leak it).
- `acquire(key: str) -> Ticket` — `GET {sidecar_addr}/check?key=<url-escaped>` (no `skip_concurrency` param, so Tier 2's concurrency check is active). Reads `Concurrency-Token`/`Concurrency-Key` response headers unconditionally, mirroring the Go SDK.
- `Ticket` supports **both** explicit `ticket.release()` **and** Python's context-manager protocol (`__enter__`/`__exit__`), so `with client.acquire(key) as ticket:` auto-releases on exit — additive Python idiom layered on the same semantics as the Go SDK, not a divergent API. A `Ticket` with no token (a rejected/errored `acquire`) makes `release()` a no-op, exactly like the Go SDK's `Ticket.Release`.
- No auth/headers are sent by the SDK itself (auth is sidecar-to-core, not client-to-sidecar, matching the Go SDK). No `x-ratecap-priority` header is set — that's the caller's responsibility, same as Go.

Packaging: a minimal `pyproject.toml`, installable via `pip install ./packages/sdks/python` for local/repo use. **PyPI publishing is deferred to Phase 4** — that's a distribution/adoption concern, not a Phase 2 tooling concern.

### 3. Testing strategy

**Unit tests** (no Docker needed, fast iteration):
- `ratecapctl config validate`: trivially testable, calls already-tested code — assert exit codes and message content for valid/invalid configs.
- `ratecapctl bench run`: tested against a `net/http/httptest.Server` fake sidecar, asserting the percentile math and output formatting (both human-readable and `--json`) are correct given controlled, fake response latencies/status codes.
- Python SDK: tested against Python's `http.server`/`unittest.mock`-based fakes, mirroring the Go SDK's own existing fake-based test pattern — `allow`/`acquire`/`release`/context-manager behavior, including the no-token-release-is-a-no-op case.

**One live docker-compose e2e check**, matching every prior phase's pattern: run `ratecapctl bench run` against the real demo stack and confirm it produces sane, non-degenerate percentile numbers (not zero, not obviously wrong); run a small Python script using the new SDK against the real stack, confirming `allow`/`acquire`/`release` genuinely work end-to-end (a real 429/503 is observed under contention, a real token round-trips through `Concurrency-Token`/`Concurrency-Key` headers).

## Out of Scope (this phase)

- `ratecapctl decisions tail` and any streaming endpoint on the sidecar to support it (see above — explicitly deferred, no stub).
- Multi-language SDK expansion beyond Python (Java, Rust, Node, etc.) — Python is the only second SDK in this phase.
- PyPI (or any package registry) publishing for the Python SDK — repo-local install only.
- Buf/BSR or any proto-based codegen tooling — not applicable, since the SDK wire boundary is plain HTTP, not gRPC.
- Any change to `services/core` or `services/sidecar`'s existing behavior — this phase only adds new, additive tooling and a new SDK; it does not modify the engine or sidecar.
