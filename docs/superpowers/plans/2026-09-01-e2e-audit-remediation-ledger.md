# Audit Remediation SDD Ledger

Task 1: APPROVED — Fix Helm adminSecret install-breaking bug (most severe finding) — commit 062e70c
Task 2: APPROVED — Wire the Helm chart's tls.mode through to RATECAP_TLS_MODE; gate the NetworkPolicy port — commit 851ca6c
Task 3: APPROVED — Fix CHANGELOG.md's phantom [2.4.1] entry — commit 6cc74af
Task 4: APPROVED — Fix README.md's broken Quick Start — commit 0f3aaaa
Task 5: APPROVED — Fix deploy/sampleapp/main.go's /fleet-demo release wire format and reservation leak — commit d577a62
Task 6: APPROVED — Fix deploy/sampleapp/main.go's dropped response headers on relay — commit b0573a6
Task 7: APPROVED — Fix Go SDK's dropped RateLimit-Reset — commit 0c79271
Task 8: APPROVED — Add missing dependabot.yml entry — commit d9a6c0c
Task 9: APPROVED — Add resource requests/limits to the Helm chart — commit 39b817d
Task 10: APPROVED — Fix the benchmark overlay's hidden RPS ceiling — commit 1bfc43c
Task 11: APPROVED — Documentation fixes: CONTRIBUTING.md, ARCHITECTURE.md, README.md, SECURITY.md — commit f56312c
Task 12: APPROVED — Defense-in-depth: validate Cost > 0 server-side in core — commit 0fcd23d
