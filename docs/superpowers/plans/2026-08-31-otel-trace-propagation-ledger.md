# OTel Trace Propagation SDD Ledger
Task 1: APPROVED — OTel SDK bootstrap in both services (gated by RATECAP_OTEL_EXPORTER_ENDPOINT) — commit 040528c
Task 2: APPROVED — otelgrpc stats handlers wired onto both core RatecapService servers and the sidecar's core client; integration test proves 2 spans share a trace ID with a parent-child relationship over a real bufconn RPC
