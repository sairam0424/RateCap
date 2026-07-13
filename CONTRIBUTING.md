# Contributing to RateCap

Thanks for considering a contribution. RateCap is a faithful, open-source recreation of [Stripe's four-tier rate-limiter and load-shedder architecture](https://stripe.com/blog/rate-limiters) — v1 is scoped strictly to Tier 1 (the Request Rate Limiter); see [`docs/superpowers/specs/2026-07-13-ratecap-v1-design.md`](docs/superpowers/specs/2026-07-13-ratecap-v1-design.md) for the full design and scope boundaries before proposing new features.

## Branch strategy

- `main` — stable, released code only.
- `develop` — integration branch. All feature work merges here first.
- `feature/<name>` — one branch per logical unit of work, branched from `develop`.

Never commit directly to `main` or `develop`. Open a pull request from your `feature/*` branch into `develop`.

## Development setup

RateCap is a multi-module Go workspace — each service/package under `services/`, `packages/`, `proto/`, and `deploy/sampleapp/` is its own Go module, wired together via the root `go.work`.

```bash
git clone https://github.com/sairam0424/RateCap.git
cd RateCap
```

### Build

```bash
for m in proto services/core services/sidecar packages/sdks/go deploy/sampleapp; do
  (cd "$m" && go build ./...)
done
```

### Test

```bash
for m in proto services/core services/sidecar packages/sdks/go; do
  (cd "$m" && go test ./... -race)
done
```

`services/core`'s `store` package includes integration tests against a real Redis instance via [testcontainers-go](https://golang.org/x/testcontainers) — Docker must be running locally for those to pass.

### Run the end-to-end demo

```bash
cd deploy
docker compose up --build
curl http://localhost:3000/checkout   # repeat 6+ times to see a 429
```

### Regenerate the gRPC contract

Only needed if you change `proto/ratecap/v1/ratecap.proto`. Requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` on `PATH`:

```bash
protoc -I proto --go_out=proto --go_opt=module=github.com/ratecap/proto \
  --go-grpc_out=proto --go-grpc_opt=module=github.com/ratecap/proto \
  ratecap/v1/ratecap.proto
```

## Commit conventions

Use [Conventional Commits](https://www.conventionalcommits.org/): `<type>(<scope>): <description>`.

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`, `build`.

```
feat(core): add tier-2 concurrent-requests limiter
fix(sidecar): correct priority header fallback order
docs: update architecture diagram for tier-2 rollout
```

Each commit should be one atomic, logical change. Write *why* in the commit body when the reasoning isn't obvious from the diff alone.

## Test discipline

- Write the failing test before the implementation for new behavior (TDD).
- Bug fixes must include a regression test that reproduces the bug.
- Run the full module's test suite with `-race` before opening a PR — RateCap's core has previously shipped a real concurrency bug (a data race in `TokenBucketLimiter.Reconfigure`) caught only by the race detector, not by sequential tests.

## Scope discipline

v1 is locked to Stripe's exact four mechanisms. Do not add a fifth limiting mechanism, bounded queueing, additional storage backends, or a Rust/WASM core without first updating the design spec and getting explicit sign-off — see the spec's "Explicitly Deferred to v2" and "Out of Scope" sections.

## Pull requests

- Title: short (under 70 characters), describes the change.
- Body: summary of what changed and why, plus a test plan.
- Target `develop`, never `main` directly.
- Request review before merging.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Reporting security issues

See [SECURITY.md](SECURITY.md) — do not open a public issue for security vulnerabilities.
