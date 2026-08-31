# Phase 5 SDD Ledger
Task 1: APPROVED — Publish a second benchmark run against un-loosened shipped defaults — commit 048b5e1
Task 2: APPROVED — --duration soak mode with a bounded streaming histogram — commit b5fd6a3
Task 3: APPROVED — Wire net/http/pprof into core and sidecar, behind a flag, loopback-only — commit 8bf5873
Task 4: APPROVED — Capture resource usage alongside benchmark runs — commit 4b68cf4
Task 5: APPROVED — Nightly CI benchmark regression job — commit 4b9f9cc
Task 6: APPROVED — Add golangci-lint to CI — commit b37471d
Task 7: APPROVED — Isolate testcontainers-go into a separate module — commit 4418391
Task 8: APPROVED — Generate the Helm chart's inline config.yaml from deploy/ratecap.yaml; document the missing concurrencySigningKey secret — commit 8d00aca
Task 8 note: commit 8d00aca's message was corrupted by a broken heredoc (leaked literal `EOF`/`)` lines, unclosed parenthesis) and its deviation narrative had the commit order backwards (e01a27e, 2026-07-18, actually predates abc14fd, 2026-07-19 — the chart's embedded config was incomplete from its very first commit, not something that drifted later). Not amended, per anti-destructive-op guidance for a shared branch — corrected in the follow-up commit immediately after this ledger entry's own commit.
Task 9: APPROVED — CLI entrypoint tests, --version, --qps/pacing — commit 3ae9f57
