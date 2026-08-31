# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

RateCap: a hybrid core-engine + sidecar rate-limiter/load-shedder, faithfully recreating Stripe's 4-tier architecture. See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the full design.

## Build & test

- **build all modules**: `for m in proto services/core services/core/integrationtests services/sidecar packages/sdks/go cli deploy/sampleapp; do (cd "$m" && go build ./...); done` — each module is a separate `go.mod` and must be built individually (`cli` is easy to forget here — it's in CI's build matrix but wasn't in this list before)
- **test all Go modules**: `for m in proto services/core services/core/integrationtests services/sidecar packages/sdks/go cli; do (cd "$m" && go test ./... -race); done` — `-race` matches CI and CONTRIBUTING.md; a real data race in `TokenBucketLimiter.Reconfigure` was caught only this way. `services/core/integrationtests`'s testcontainers-based Redis/Toxiproxy tests need Docker running locally — they're isolated into their own module precisely so `services/core` itself doesn't carry the `testcontainers-go` transitive dependency tree into production. Excludes `deploy/sampleapp` (demo binary, no tests).
- **test one module**: `cd services/core && go test ./... -race -v`
- **format check**: `gofmt -l .` must print nothing — this is a CI gate
- **lint**: `golangci-lint run` from inside each Go module directory (config: repo-root `.golangci.yml`) — also a CI gate, matrixed the same way as build-and-test
- **Python SDK** (`packages/sdks/python` — not a Go module, not in `go.work`): `pip install -e . && python -m unittest discover -s tests -v`
- **regenerate proto**: `protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto` (run from repo root; requires `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`; `-I proto` keeps the file descriptor's canonical name as `ratecap/v1/ratecap.proto`, not `proto/ratecap/v1/ratecap.proto`)
- **run the demo stack**: `cd deploy && bash generate-demo-certs.sh && docker compose up --build` — the cert-gen step is required first: `docker-compose.yml` hardcodes mTLS env vars for both services, so startup fails on missing cert files without it. (The README's own top-level Quick Start omits this step and is broken as written; its Benchmarks section and CI's e2e-smoke job both include it.)

## Scope discipline

v1 shipped locked to Stripe's exact 4 mechanisms. v2 additions (e.g. Tier 2's bounded queueing, `queueing_enabled`) only land via a design spec + explicit sign-off first — see `docs/superpowers/specs/` for that history. Don't add a 5th limiting mechanism, additional storage backends, or a Rust/WASM core without the same spec-first process. (`services/sidecar/ratelimit` — a process-wide defensive HTTP limiter wrapping the whole sidecar mux — already exists outside the 4 tiers; it predates and is exempt from this rule, not a violation of it.)

## Gotchas (span multiple files — easy to get wrong)

- `packages/sdks/go` and `packages/sdks/python` have zero relation to `proto/`'s gRPC contract — both are plain HTTP clients to the sidecar's own `/check`/`/release` wire format. The gRPC contract is used only for sidecar↔core traffic.
- A single `/check` call can return more than one concurrency reservation, as indexed headers `Concurrency-Token-N`/`Concurrency-Key-N` (N=0,1,2…) — e.g. one for the caller's own key (Tier 2) and one for the global `"fleet"` key (Tier 3). Each needs its own `/release` call; both SDKs already loop over these, so any new client code must too.
- The `x-ratecap-shared-secret` gRPC metadata key is an independently-declared constant in both `services/core/auth` and `services/sidecar/auth` (separate `go.mod`s, no shared package) — changing the literal in one without the other breaks auth at runtime, not compile time.
- `Config.Validate()` (`services/core/config/config.go`) checks Tier 1's `default_rate`/`default_burst`, the concurrency-limiter/fleet-shedder fields, and queueing — but not the top-level `sync_rate`, which also has no runtime consumer anywhere in the repo despite being required in every deploy config (a vestigial field, not a bug to fix by adding validation for it).

## Conventions

- Go module naming: `github.com/ratecap/<service>`
- Cross-module deps within this repo: `go mod edit -replace github.com/ratecap/X=../../X`
- No comments except non-obvious WHY (hidden constraints, subtle invariants)
- Files: 200-400 lines typical, 800 max
