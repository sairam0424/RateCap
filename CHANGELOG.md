# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project intends to follow [Semantic Versioning](https://semver.org/) once a first tagged release is cut.

## [Unreleased]

### Added

- Repository governance docs: `LICENSE` (MIT), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `ARCHITECTURE.md`, `THINKING_DIAGRAM.md`.

## [0.1.0] — Walking Skeleton — 2026-07-13

The first working slice of RateCap: Tier 1 (Request Rate Limiter) proven end-to-end across every architectural seam — SDK → sidecar → core → Redis — before Tiers 2–4 are built on the same plumbing.

### Added

- `proto/` — the `RatecapService` gRPC contract (`CheckRateLimit` RPC, 4-value `Action` enum: `ALLOW`, `REJECT_429`, `REJECT_503`, `SHADOW_LOG`).
- `services/core` — the central engine:
  - `store` — `StateStore` interface with a Redis-backed implementation using an atomic Lua token-bucket script.
  - `limiter` — `Limiter` interface with `TokenBucketLimiter`, pure decision logic with no Redis dependency, unit-tested via a fake store.
  - `config` — YAML config loading and `fsnotify`-based hot-reload, with error-logging on reload failure and hardening for atomic file replacement.
  - `grpcserver` + `main.go` — wires everything into a running gRPC service.
- `services/sidecar` — the local proxy:
  - `proxy` — priority-header resolution (`x-ratecap-priority` → route config → safe default) and the HTTP handler forwarding checks to core.
  - `shadow` — per-tier and global (`RATECAP_SHADOW_MODE`) shadow-mode override for safe production rollout.
- `packages/sdks/go` — a thin Go client SDK wrapping the sidecar's HTTP endpoint.
- `deploy/` — a Docker Compose demo (core + sidecar + Redis + sample app) proving real rate-limiting end-to-end.
- `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` — the full v1 design spec.

### Fixed

- A data race in `TokenBucketLimiter.Reconfigure`, which mutated shared config fields with no synchronization while `Check` read them concurrently from gRPC handler goroutines — caught by the race detector before it shipped, fixed with a `sync.RWMutex`.
- A protobuf descriptor-path leak (`proto/ratecap/v1/...` instead of the idiomatic `ratecap/v1/...`) from an initial `protoc` invocation missing `-I proto`.

### Post-review fixes

- Added a characterization test pinning the sidecar's current "parse the priority header, don't act on it" behavior — a regression tripwire for when Tier 3 makes priority load-bearing.
- Replaced `GOWORK=off` (which caused Docker's build cache to ignore `go.work.sum` entirely) with a build-context-scoped `go.work` generated inline in each Dockerfile, so a real dependency bump now correctly invalidates the Docker layer cache.
