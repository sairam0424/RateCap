## Summary

<!-- What changed and why. -->

## Test plan

<!-- Commands run, e.g. `go test ./... -race` per affected module, or the demo stack `/healthz`/`/checkout` check. -->

---

Before requesting review, confirm this PR follows [CONTRIBUTING.md](../CONTRIBUTING.md):

- [ ] Targets `develop` (never `main` directly)
- [ ] Commit messages follow [Conventional Commits](../CONTRIBUTING.md#commit-conventions)
- [ ] Tests added/updated per [Test discipline](../CONTRIBUTING.md#test-discipline), and `go test ./... -race` passes for every affected module
- [ ] `gofmt -l .` and `golangci-lint run` are clean for every affected module
- [ ] Any new limiting mechanism, storage backend, or scope change follows [Scope discipline](../CONTRIBUTING.md#scope-discipline)
