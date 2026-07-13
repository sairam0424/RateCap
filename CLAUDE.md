# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

RateCap: a hybrid core-engine + sidecar rate-limiter/load-shedder, faithfully recreating Stripe's 4-tier architecture. See `docs/superpowers/specs/2026-07-13-ratecap-v1-design.md` for the full design.

## Build & test

- **build all**: `go build ./...` from repo root (uses `go.work`)
- **test all**: `go build ./... && go test ./...` from repo root
- **test one module**: `cd services/core && go test ./... -v`
- **regenerate proto**: `protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto ratecap/v1/ratecap.proto` (run from repo root; requires `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`; `-I proto` keeps the file descriptor's canonical name as `ratecap/v1/ratecap.proto`, not `proto/ratecap/v1/ratecap.proto`)
- **run the demo stack**: `cd deploy && docker compose up --build`

## Scope discipline

v1 is locked to Stripe's exact 4 mechanisms — do not add a 5th limiting mechanism, bounded queueing, additional storage backends, or a Rust/WASM core without updating the design spec first and getting explicit sign-off. See the spec's "Explicitly Deferred to v2" and "Out of Scope" sections.

## Conventions

- Go module naming: `github.com/ratecap/<service>`
- Cross-module deps within this repo: `go mod edit -replace github.com/ratecap/X=../../X`
- No comments except non-obvious WHY (hidden constraints, subtle invariants)
- Files: 200-400 lines typical, 800 max
