# OTel Trace Propagation SDD Ledger
Task 1: APPROVED — OTel SDK bootstrap in both services (gated by RATECAP_OTEL_EXPORTER_ENDPOINT) — commit 040528c
Task 2: APPROVED — otelgrpc stats handlers wired onto both core RatecapService servers and the sidecar's core client; integration test proves 2 spans share a trace ID with a parent-child relationship over a real bufconn RPC
Task 3: APPROVED — Priority attribute on both spans; enforce the no-key-in-telemetry rule — commit b90b67f
Task 4: APPROVED — Documentation (ARCHITECTURE.md Tracing subsection, CHANGELOG.md 2.10.0 entry, VERSION bump) — commit ad03288
