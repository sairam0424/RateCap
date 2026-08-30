# RateCap v3 Roadmap — Phase 3: Security — mTLS PERMISSIVE Mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** add the migration rung Istio/Linkerd use to move a fleet without a flag day — RateCap currently jumps straight from "off" to "all-or-nothing strict," with no middle step.

**Architecture:** A new `RATECAP_TLS_MODE` env var gates a second, optional TLS gRPC listener on `services/core` (plaintext `:9090` keeps running unchanged); a connection-security metric distinguishes plaintext vs. TLS traffic per call; a `ratecapctl tls check` subcommand catches the SAN/hostname mismatch failure mode the Helm chart's own `values.yaml` already warns about; certificate hot-reload closes the "rotated cert needs a pod restart" gap on both services; an opt-in Helm `NetworkPolicy` operationalizes the network-boundary claim `SECURITY.md` already makes.

**Tech Stack:** Go 1.26; `github.com/fsnotify/fsnotify` (already a `services/core` dependency, new to `services/sidecar`).

**Spec:** `docs/superpowers/specs/2026-08-27-v3-upgrade-roadmap-design.md`, Phase 3 section (items 1–6).

## Global Constraints

- **No shipped default changes in this phase (spec item 6, non-negotiable).** `RATECAP_TLS_MODE` unset (or explicitly `"off"`) MUST behave byte-for-byte identically to the current, pre-Phase-3 code: if `RATECAP_TLS_CERT_PATH`/`KEY_PATH`/`CA_PATH` are unset, plaintext-only on `:9090`; if set, the existing single-listener TLS-only behavior (`RequireAndVerifyClientCert`) — exactly as today. The Helm chart's `tls.enabled` default and the demo `docker-compose.yml` are NOT touched by any task in this plan.
- **`permissive` and `strict` both require the TLS cert env vars to be set** (`RATECAP_TLS_CERT_PATH`/`KEY_PATH`/`CA_PATH`) — fail closed with a clear startup error if a mode requiring TLS is set without them.
- **`permissive` mode is additive, never destructive**: the existing plaintext listener on `:9090` (or `RATECAP_GRPC_ADDR`) keeps running completely unchanged; a *new* listener is added alongside it. Nothing about existing sidecar connectivity changes when an operator sets `RATECAP_TLS_MODE=permissive` on core alone.
- **`-race` is mandatory** on every `go test` invocation touching `services/core` or `services/sidecar`.
- **Cert hot-reload only covers cert/key, not the CA pool** — matching the spec's own literal wording ("watch cert/key paths"). CA rotation remains a restart-required operation; do not silently expand scope.
- **No comments except non-obvious WHY**, matching the existing codebase's terse style.
- Files: 200-400 lines typical, 800 max.
- Never run `git push`, `git branch -D`, or any destructive git command — commit locally only.
- If a `git commit` (or any git operation) is denied by a PreToolUse hook or permission check, STOP and report BLOCKED with the exact denial message verbatim — never retry with `dangerouslyDisableSandbox`, `--no-verify`, or any other bypass. This is a hard, previously-enforced rule from earlier phases of this same roadmap — a violation was caught and corrected once already; it must not recur.

---

### Task 1: `RATECAP_TLS_MODE` and the permissive-mode dual gRPC listener

**Files:**
- Modify: `services/core/main.go`
- Create: `services/core/tlsmode_test.go` (`package main` — internal test, matching this file's existing `redisclient_test.go`/`healthloop_test.go` convention of testing unexported helpers directly)

**Interfaces:**
- Produces: `resolveTLSMode(raw string) (string, error)` — validates and defaults `RATECAP_TLS_MODE`. Empty string defaults to `"off"`; `"off"`/`"permissive"`/`"strict"` are the only valid values.

- [ ] **Step 1: Write the failing test**

```go
// services/core/tlsmode_test.go
package main

import "testing"

func TestResolveTLSMode_EmptyStringDefaultsToOff(t *testing.T) {
	got, err := resolveTLSMode("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "off" {
		t.Errorf(`expected "off", got %q`, got)
	}
}

func TestResolveTLSMode_ExplicitOffIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "off" {
		t.Errorf(`expected "off", got %q`, got)
	}
}

func TestResolveTLSMode_PermissiveIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("permissive")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "permissive" {
		t.Errorf(`expected "permissive", got %q`, got)
	}
}

func TestResolveTLSMode_StrictIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("strict")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "strict" {
		t.Errorf(`expected "strict", got %q`, got)
	}
}

func TestResolveTLSMode_InvalidValueReturnsError(t *testing.T) {
	_, err := resolveTLSMode("YOLO")
	if err == nil {
		t.Fatal("expected an error for an invalid RATECAP_TLS_MODE value")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/core && go test . -race -run TestResolveTLSMode -v`
Expected: FAIL — `resolveTLSMode` undefined.

- [ ] **Step 3: Implement `resolveTLSMode`**

Add to `services/core/main.go` (near the other `resolveX`/`parseX` helpers):
```go
// resolveTLSMode defaults an unset RATECAP_TLS_MODE to "off" — the exact
// pre-Phase-3 behavior (single listener, TLS-only if cert env vars happen
// to be set, plaintext-only otherwise) — preserved unconditionally so no
// existing deployment's behavior changes just because this env var now
// exists.
func resolveTLSMode(raw string) (string, error) {
	switch raw {
	case "":
		return "off", nil
	case "off", "permissive", "strict":
		return raw, nil
	default:
		return "", fmt.Errorf("RATECAP_TLS_MODE=%q is invalid — must be one of: off, permissive, strict", raw)
	}
}
```
Add `"fmt"` to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd services/core && go test . -race -v`

- [ ] **Step 5: Wire the mode into `main()` and add the permissive-mode dual listener**

Change the block starting with `tlsCertPath := os.Getenv(...)` (currently around line 113) to add mode resolution and the fail-closed check right after the existing `EnvVarsPartiallySet` guard:
```go
	tlsCertPath := os.Getenv("RATECAP_TLS_CERT_PATH")
	tlsKeyPath := os.Getenv("RATECAP_TLS_KEY_PATH")
	tlsCAPath := os.Getenv("RATECAP_TLS_CA_PATH")
	if tlsconfig.EnvVarsPartiallySet(tlsCertPath, tlsKeyPath, tlsCAPath) {
		log.Fatalf("RATECAP_TLS_CERT_PATH, RATECAP_TLS_KEY_PATH, and RATECAP_TLS_CA_PATH must be set together or not at all — got cert=%q key=%q ca=%q", tlsCertPath, tlsKeyPath, tlsCAPath)
	}

	tlsMode, err := resolveTLSMode(os.Getenv("RATECAP_TLS_MODE"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	if (tlsMode == "permissive" || tlsMode == "strict") && tlsCertPath == "" {
		log.Fatalf("RATECAP_TLS_MODE=%s requires RATECAP_TLS_CERT_PATH/RATECAP_TLS_KEY_PATH/RATECAP_TLS_CA_PATH to be set", tlsMode)
	}
```

Then change the existing single-listener TLS block (currently `if tlsCertPath != "" { ... }` right before `grpcServer := grpc.NewServer(serverOpts...)`) to skip TLS-on-the-primary-listener specifically when in `permissive` mode (permissive keeps the primary listener plaintext and adds a *second* listener for TLS instead):
```go
	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret), coremetrics.UnaryServerInterceptor()),
	}
	if tlsCertPath != "" && tlsMode != "permissive" {
		tlsConf, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		log.Printf("ratecap-core: mTLS enabled (mode=%s)", tlsMode)
	}
	grpcServer := grpc.NewServer(serverOpts...)
	coreServer := grpcserver.NewServer(pipeline, redisStore, rateLimiter, fleetShedder, []byte(concurrencySigningKey))
	ratecapv1.RegisterRatecapServiceServer(grpcServer, coreServer)
```
(note: `grpcserver.NewServer(...)`'s result is now assigned to `coreServer` first, since Step 6 below registers the *same* server implementation on a second `grpc.Server` too — the implementation itself is a plain Go struct with no per-listener state, so registering it twice is safe.)

- [ ] **Step 6: Add the permissive-mode second listener**

Add this block immediately after the primary `ratecapv1.RegisterRatecapServiceServer(grpcServer, coreServer)` line, before the existing health-server block:
```go
	if tlsMode == "permissive" {
		tlsAddr := os.Getenv("RATECAP_GRPC_TLS_ADDR")
		if tlsAddr == "" {
			tlsAddr = ":9443"
		}
		tlsLis, err := net.Listen("tcp", tlsAddr)
		if err != nil {
			log.Fatalf("failed to listen on %s: %v", tlsAddr, err)
		}
		permissiveConf, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config for permissive listener: %v", err)
		}
		// VerifyClientCertIfGiven (not RequireAndVerifyClientCert): permissive
		// mode's whole purpose is letting sidecars migrate one at a time — a
		// sidecar without a cert yet must still be able to connect over this
		// listener's TLS transport (server-authenticated only), while one that
		// does present a cert gets it verified.
		permissiveConf.ClientAuth = tls.VerifyClientCertIfGiven
		tlsServerOpts := []grpc.ServerOption{
			grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(sharedSecret), coremetrics.UnaryServerInterceptor()),
			grpc.Creds(credentials.NewTLS(permissiveConf)),
		}
		tlsGrpcServer := grpc.NewServer(tlsServerOpts...)
		ratecapv1.RegisterRatecapServiceServer(tlsGrpcServer, coreServer)
		go func() {
			log.Printf("ratecap-core permissive-mode TLS listener (optional client cert) on %s", tlsAddr)
			if err := tlsGrpcServer.Serve(tlsLis); err != nil {
				log.Fatalf("permissive-mode TLS grpc server failed: %v", err)
			}
		}()
		log.Printf("ratecap-core: TLS_MODE=permissive — plaintext still serving on %s, TLS available on %s", listenAddr, tlsAddr)
	}
```
Add `"crypto/tls"` to the import block.

- [ ] **Step 7: Build and run the full core suite**

Run: `cd services/core && go build ./... && go test ./... -race`

- [ ] **Step 8: Manual verification**

```bash
cd deploy && bash generate-demo-certs.sh
cd ../services/core
RATECAP_CONFIG_PATH=../../deploy/ratecap.yaml \
RATECAP_SHARED_SECRET=test-secret \
RATECAP_CONCURRENCY_SIGNING_KEY=test-signing-key \
RATECAP_REDIS_ADDR=localhost:6379 \
RATECAP_TLS_CERT_PATH=../../deploy/certs/core-cert.pem \
RATECAP_TLS_KEY_PATH=../../deploy/certs/core-key.pem \
RATECAP_TLS_CA_PATH=../../deploy/certs/ca.pem \
RATECAP_TLS_MODE=permissive \
RATECAP_GRPC_ADDR=:19090 \
RATECAP_GRPC_TLS_ADDR=:19443 \
RATECAP_HEALTH_ADDR=:19091 \
RATECAP_METRICS_ADDR=:19092 \
go run . &
sleep 2
# expect both log lines: plaintext listening on :19090, TLS listener on :19443
kill %1
```
Requires a locally running Redis (`docker run -d -p 6379:6379 redis:7-alpine` if not already running) and the demo certs generated first. If Docker/Redis is unavailable, note this in the report and rely on the unit tests in Step 4 plus a clean `go build ./...` as the primary verification.

- [ ] **Step 9: Commit**

```bash
git add services/core/main.go services/core/tlsmode_test.go
git commit -m "feat(core): add RATECAP_TLS_MODE with an additive permissive-mode TLS listener"
```

---

### Task 2: Plaintext-vs-TLS connection observability

**Files:**
- Modify: `services/core/metrics/metrics.go`, `services/core/metrics/metrics_test.go`, `services/core/metrics/interceptor.go`, `services/core/metrics/interceptor_test.go`

**Interfaces:**
- Produces: `metrics.RecordConnectionSecurity(transport, clientCert string)`, a `ratecap_core_connection_security_total{transport,client_cert}` counter. `UnaryServerInterceptor()` now also records this on every call, using `peer.FromContext(ctx)` to inspect the real transport.

- [ ] **Step 1: Write the failing metrics test**

Append to `services/core/metrics/metrics_test.go`:
```go
func TestRecordConnectionSecurity_IncrementsByTransportAndClientCert(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))
	metrics.RecordConnectionSecurity("tls", "present")
	after := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))

	if after != before+1 {
		t.Errorf("expected ConnectionSecurityTotal{transport=tls,client_cert=present} to increment by 1, before=%v after=%v", before, after)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/core && go test ./metrics/... -race -run TestRecordConnectionSecurity -v`

- [ ] **Step 3: Implement the metric**

Add to `services/core/metrics/metrics.go`:
```go
var ConnectionSecurityTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ratecap_core_connection_security_total",
	Help: "Total gRPC calls by transport (plaintext or tls) and whether a client certificate was presented (present, absent, or n/a for plaintext) — the 'is anything still on plaintext' signal a default flip needs before it's safe.",
}, []string{"transport", "client_cert"})

func RecordConnectionSecurity(transport, clientCert string) {
	ConnectionSecurityTotal.WithLabelValues(transport, clientCert).Inc()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd services/core && go test ./metrics/... -race -v`

- [ ] **Step 5: Write the failing interceptor test**

Append to `services/core/metrics/interceptor_test.go`:
```go
func TestUnaryServerInterceptor_RecordsPlaintextWhenNoPeerTLSInfo(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	before := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("plaintext", "n/a"))
	_, _ = interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("plaintext", "n/a"))

	if after != before+1 {
		t.Errorf("expected ConnectionSecurityTotal{transport=plaintext,client_cert=n/a} to increment when ctx has no peer info, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsTLSWithClientCertAbsent(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: nil}},
	})

	before := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "absent"))
	_, _ = interceptor(ctx, "request", info, handler)
	after := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "absent"))

	if after != before+1 {
		t.Errorf("expected ConnectionSecurityTotal{transport=tls,client_cert=absent} to increment for a TLS peer with no client cert, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsTLSWithClientCertPresent(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/CheckRateLimit"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}},
	})

	before := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))
	_, _ = interceptor(ctx, "request", info, handler)
	after := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))

	if after != before+1 {
		t.Errorf("expected ConnectionSecurityTotal{transport=tls,client_cert=present} to increment for a TLS peer WITH a client cert, before=%v after=%v", before, after)
	}
}
```
Add `"crypto/tls"`, `"crypto/x509"`, `"google.golang.org/grpc/credentials"`, and `"google.golang.org/grpc/peer"` to this test file's imports.

- [ ] **Step 6: Run tests to verify they fail**

Run: `cd services/core && go test ./metrics/... -race -run TestUnaryServerInterceptor_Records -v`

- [ ] **Step 7: Implement**

In `services/core/metrics/interceptor.go`, add a helper and call it from the interceptor:
```go
// classifyTransport inspects the real peer connection info gRPC attaches to
// the context — not which listener/port a call arrived on — so the metric
// reflects actual transport security regardless of how many listeners are
// configured. peer.FromContext returning !ok happens routinely in unit
// tests and for calls with no transport-level peer info attached.
func classifyTransport(ctx context.Context) (transport, clientCert string) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "plaintext", "n/a"
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "plaintext", "n/a"
	}
	if len(tlsInfo.State.PeerCertificates) > 0 {
		return "tls", "present"
	}
	return "tls", "absent"
}
```
Add the call inside `UnaryServerInterceptor()`'s returned closure, alongside the existing `RecordGRPCRequest` call:
```go
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		RecordGRPCRequest(methodName(info.FullMethod), status.Code(err).String(), time.Since(start))
		transport, clientCert := classifyTransport(ctx)
		RecordConnectionSecurity(transport, clientCert)
		return resp, err
	}
```
Add `"google.golang.org/grpc/credentials"` and `"google.golang.org/grpc/peer"` to `interceptor.go`'s imports.

- [ ] **Step 8: Run tests to verify they pass**

Run: `cd services/core && go test ./metrics/... -race -v`

- [ ] **Step 9: Add the metric to the starter Grafana dashboard**

In `deploy/grafana/ratecap-overview.json`, add one more panel (following the existing panel-object shape, incrementing `id` and `gridPos.y`):
```json
{
  "id": 9,
  "title": "Plaintext vs TLS connections (core)",
  "type": "timeseries",
  "gridPos": { "h": 8, "w": 12, "x": 0, "y": 32 },
  "targets": [
    { "expr": "sum by (transport, client_cert) (rate(ratecap_core_connection_security_total[1m]))", "legendFormat": "{{transport}} / {{client_cert}}" }
  ]
}
```

- [ ] **Step 10: Commit**

```bash
git add services/core/metrics/metrics.go services/core/metrics/metrics_test.go services/core/metrics/interceptor.go services/core/metrics/interceptor_test.go deploy/grafana/ratecap-overview.json
git commit -m "feat(core): add plaintext-vs-TLS connection observability (ratecap_core_connection_security_total)"
```

---

### Task 3: `ratecapctl tls check` — SAN/hostname preflight

**Files:**
- Create: `cli/cmd/tls.go`, `cli/cmd/tls_test.go`
- Modify: `cli/cmd/root.go`

**Interfaces:**
- Produces: `ratecapctl tls check <cert-path> <expected-host>` — exits non-zero with a clear message if the cert's SAN list doesn't cover the given host.

- [ ] **Step 1: Write the failing tests**

```go
// cli/cmd/tls_test.go
package cmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCert(t *testing.T, sans []string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-cert"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create cert: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test-cert.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	return path
}

func TestTLSCheck_MatchingSANSucceeds(t *testing.T) {
	certPath := writeTestCert(t, []string{"ratecap-core", "core.default.svc.cluster.local"})

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"tls", "check", certPath, "ratecap-core"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error for a matching SAN: %v", err)
	}
	if out.String() == "" {
		t.Error("expected some confirmation output")
	}
}

func TestTLSCheck_MismatchedSANFails(t *testing.T) {
	certPath := writeTestCert(t, []string{"core"})

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"tls", "check", certPath, "my-release-core"})

	if err := root.Execute(); err == nil {
		t.Error("expected an error when the expected host is not in the cert's SAN list — this is exactly the demo-certs-vs-Helm-release-name failure mode documented in values.yaml")
	}
}

func TestTLSCheck_MissingFileFails(t *testing.T) {
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"tls", "check", "/nonexistent/cert.pem", "core"})

	if err := root.Execute(); err == nil {
		t.Error("expected an error for a nonexistent cert file")
	}
}

func TestTLSCheck_RequiresTwoArgs(t *testing.T) {
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"tls", "check", "only-one-arg"})

	if err := root.Execute(); err == nil {
		t.Error("expected an error when the expected-host argument is missing")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd cli && go test ./cmd/... -race -run TestTLSCheck -v`

- [ ] **Step 3: Implement**

```go
// cli/cmd/tls.go
package cmd

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newTLSCmd() *cobra.Command {
	tlsCmd := &cobra.Command{
		Use:   "tls",
		Short: "TLS certificate commands",
	}
	tlsCmd.AddCommand(newTLSCheckCmd())
	return tlsCmd
}

func newTLSCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <cert-path> <expected-host>",
		Short: "Verify a certificate's SAN list covers the given hostname before deploying it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			certPath, expectedHost := args[0], args[1]

			data, err := os.ReadFile(certPath)
			if err != nil {
				return fmt.Errorf("reading %s: %w", certPath, err)
			}
			block, _ := pem.Decode(data)
			if block == nil {
				return fmt.Errorf("%s does not contain a valid PEM block", certPath)
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return fmt.Errorf("parsing certificate in %s: %w", certPath, err)
			}

			if err := cert.VerifyHostname(expectedHost); err != nil {
				return fmt.Errorf("%s's SAN list %v does not cover %q: %w — this is the exact failure mode Helm chart deployments hit when demo certs (SAN: core/sidecar) are reused with a real release name, and it produces no server-side log", certPath, cert.DNSNames, expectedHost, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s: SAN list %v covers %q\n", certPath, cert.DNSNames, expectedHost)
			return nil
		},
	}
}
```

In `cli/cmd/root.go`, add:
```go
	root.AddCommand(newTLSCmd())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cli && go build ./... && go test ./cmd/... -race -v`

- [ ] **Step 5: Commit**

```bash
git add cli/cmd/tls.go cli/cmd/tls_test.go cli/cmd/root.go
git commit -m "feat(cli): add ratecapctl tls check for the SAN/hostname preflight failure mode"
```

---

### Task 4: Certificate hot-reload — `services/core` (server cert)

**Files:**
- Create: `services/core/tlsconfig/reload.go`, `services/core/tlsconfig/reload_test.go`
- Modify: `services/core/tlsconfig/tlsconfig.go`, `services/core/main.go`

**Interfaces:**
- Produces: `tlsconfig.Load(certPath, keyPath, caPath string) (*tls.Config, func(), error)` — signature change: now returns a `stop func()` for the reload watcher. Every existing call site updates.

- [ ] **Step 1: Write the failing tests**

```go
// services/core/tlsconfig/reload_test.go
package tlsconfig_test

import (
	"crypto/tls"
	"os"
	"testing"
	"time"

	"github.com/ratecap/core/tlsconfig"
)

func TestLoad_GetCertificateReturnsCurrentCertAfterFileChange(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedKeyPair(t, dir, "cert-v1.pem", "key-v1.pem", "host-v1")
	caPath := writeCA(t, dir)

	tlsConf, stop, err := tlsconfig.Load(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	cert1, err := tlsConf.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("unexpected error calling GetCertificate: %v", err)
	}
	if cert1 == nil {
		t.Fatal("expected a non-nil certificate on first call")
	}

	newCertPath, newKeyPath := writeSelfSignedKeyPair(t, dir, "cert-v1.pem", "key-v1.pem", "host-v2")
	if newCertPath != certPath || newKeyPath != keyPath {
		t.Fatalf("expected the rewrite to reuse the same paths, got %s/%s", newCertPath, newKeyPath)
	}

	deadline := time.Now().Add(2 * time.Second)
	var cert2 *tls.Certificate
	for time.Now().Before(deadline) {
		cert2, err = tlsConf.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatalf("unexpected error calling GetCertificate: %v", err)
		}
		if cert2.Leaf != nil && cert2.Leaf.DNSNames[0] == "host-v2" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cert2 == nil || cert2.Leaf == nil || cert2.Leaf.DNSNames[0] != "host-v2" {
		t.Fatal("timed out waiting for GetCertificate to reflect the rewritten cert file")
	}
}

func TestLoad_KeepsLastKnownGoodOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedKeyPair(t, dir, "cert.pem", "key.pem", "host-good")
	caPath := writeCA(t, dir)

	tlsConf, stop, err := tlsconfig.Load(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if err := os.WriteFile(certPath, []byte("not a valid cert"), 0644); err != nil {
		t.Fatalf("failed to write corrupt cert: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	cert, err := tlsConf.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("expected GetCertificate to keep serving the last-known-good cert, got error: %v", err)
	}
	if cert.Leaf == nil || cert.Leaf.DNSNames[0] != "host-good" {
		t.Error("expected the last-known-good cert to still be served after a corrupt rewrite")
	}
}

func TestLoad_StopEndsTheWatcher(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedKeyPair(t, dir, "cert.pem", "key.pem", "host-a")
	caPath := writeCA(t, dir)

	_, stop, err := tlsconfig.Load(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop()
	stop()
}
```

Add two shared test helpers to the SAME file (`reload_test.go`):
```go
func writeSelfSignedKeyPair(t *testing.T, dir, certFile, keyFile, dnsName string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create cert: %v", err)
	}

	certPath = filepath.Join(dir, certFile)
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyPath = filepath.Join(dir, keyFile)
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
	keyOut.Close()

	return certPath, keyPath
}

func writeCA(t *testing.T, dir string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create CA cert: %v", err)
	}

	caPath := filepath.Join(dir, "ca.pem")
	f, err := os.Create(caPath)
	if err != nil {
		t.Fatalf("failed to create CA file: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to write CA cert: %v", err)
	}
	return caPath
}
```
Add imports: `"crypto/ecdsa"`, `"crypto/elliptic"`, `"crypto/rand"`, `"crypto/x509"`, `"crypto/x509/pkix"`, `"encoding/pem"`, `"math/big"`, `"path/filepath"`.

Note: `tls.Certificate.Leaf` is only populated by `tls.LoadX509KeyPair` since Go 1.23+ parses and caches it automatically — this repo targets Go 1.26.2 (`go.mod`), so `.Leaf` is available without an extra manual `x509.ParseCertificate` call.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd services/core && go test ./tlsconfig/... -race -v`
Expected: FAIL — compile error (`Load` doesn't return a `stop func()` yet).

- [ ] **Step 3: Implement the reloadable-cert primitive**

```go
// services/core/tlsconfig/reload.go
package tlsconfig

import (
	"crypto/tls"
	"log"
	"path/filepath"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type reloadableCert struct {
	certPath, keyPath string
	current           atomic.Pointer[tls.Certificate]
}

func watchCert(certPath, keyPath string) (*reloadableCert, func(), error) {
	r := &reloadableCert{certPath: certPath, keyPath: keyPath}
	if err := r.reload(); err != nil {
		return nil, nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	dirs := map[string]bool{filepath.Dir(certPath): true, filepath.Dir(keyPath): true}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, nil, err
		}
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == certPath || event.Name == keyPath {
					if err := r.reload(); err != nil {
						log.Printf("tlsconfig: failed to reload cert/key, keeping last-known-good: %v", err)
					}
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
				_ = watcher.Close()
				return
			}
		}
	}()

	stop := func() { close(done) }
	return r, stop, nil
}

func (r *reloadableCert) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.current.Store(&cert)
	return nil
}

func (r *reloadableCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}
```

- [ ] **Step 4: Update `Load` to use it**

In `services/core/tlsconfig/tlsconfig.go`, change:
```go
func Load(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading server cert/key: %w", err)
	}

	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", caPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}
```
to:
```go
func Load(certPath, keyPath, caPath string) (*tls.Config, func(), error) {
	reloadable, stop, err := watchCert(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading server cert/key: %w", err)
	}

	caData, err := os.ReadFile(caPath)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		stop()
		return nil, nil, fmt.Errorf("no valid certificates found in CA file %s", caPath)
	}

	return &tls.Config{
		GetCertificate: reloadable.GetCertificate,
		ClientCAs:      pool,
		ClientAuth:     tls.RequireAndVerifyClientCert,
	}, stop, nil
}
```

- [ ] **Step 5: Update every call site**

`services/core/main.go` has two call sites (Task 1 introduced the second one):
```go
	if tlsCertPath != "" && tlsMode != "permissive" {
		tlsConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		defer stopCertWatch()
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		log.Printf("ratecap-core: mTLS enabled (mode=%s)", tlsMode)
	}
```
```go
		permissiveConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config for permissive listener: %v", err)
		}
		defer stopCertWatch()
		permissiveConf.ClientAuth = tls.VerifyClientCertIfGiven
```
Also grep for any other `tlsconfig.Load(` call sites in `services/core` (e.g. existing tests in `services/core/grpcserver/mtls_integration_test.go`) and update every one identically — do not leave any uncompiled call site.

- [ ] **Step 6: Run the full core suite**

Run: `cd services/core && go build ./... && go test ./... -race`

- [ ] **Step 7: Commit**

```bash
git add services/core/tlsconfig/reload.go services/core/tlsconfig/reload_test.go services/core/tlsconfig/tlsconfig.go services/core/main.go services/core/grpcserver/mtls_integration_test.go
git commit -m "feat(core): hot-reload the server cert/key via fsnotify instead of loading once at startup"
```

---

### Task 5: Certificate hot-reload — `services/sidecar` (client cert)

**Files:**
- Create: `services/sidecar/tlsconfig/reload.go`, `services/sidecar/tlsconfig/reload_test.go`
- Modify: `services/sidecar/tlsconfig/tlsconfig.go`, `services/sidecar/main.go`, `services/sidecar/go.mod`, `services/sidecar/go.sum`

**Interfaces:**
- Produces: `tlsconfig.Load(certPath, keyPath, caPath string) (*tls.Config, func(), error)` — same signature-change pattern as Task 4, but using `GetClientCertificate` (client-side hook) instead of `GetCertificate` (server-side).

- [ ] **Step 1: Add the fsnotify dependency**

```bash
cd services/sidecar && go get github.com/fsnotify/fsnotify@v1.10.1
```
(pin to the exact version already used by `services/core`, avoiding the cross-module version-skew class of bug Phase 0 fixed once already).

- [ ] **Step 2: Write the failing tests**

```go
// services/sidecar/tlsconfig/reload_test.go
package tlsconfig_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ratecap/sidecar/tlsconfig"
)

func writeSelfSignedKeyPair(t *testing.T, dir, certFile, keyFile, dnsName string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create cert: %v", err)
	}

	certPath = filepath.Join(dir, certFile)
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyPath = filepath.Join(dir, keyFile)
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
	keyOut.Close()

	return certPath, keyPath
}

func writeCA(t *testing.T, dir string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create CA cert: %v", err)
	}

	caPath := filepath.Join(dir, "ca.pem")
	f, err := os.Create(caPath)
	if err != nil {
		t.Fatalf("failed to create CA file: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to write CA cert: %v", err)
	}
	return caPath
}

func TestLoad_GetClientCertificateReturnsCurrentCertAfterFileChange(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedKeyPair(t, dir, "cert.pem", "key.pem", "sidecar-v1")
	caPath := writeCA(t, dir)

	tlsConf, stop, err := tlsconfig.Load(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	cert1, err := tlsConf.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("unexpected error calling GetClientCertificate: %v", err)
	}
	if cert1 == nil {
		t.Fatal("expected a non-nil certificate on first call")
	}

	writeSelfSignedKeyPair(t, dir, "cert.pem", "key.pem", "sidecar-v2")

	deadline := time.Now().Add(2 * time.Second)
	var cert2 *tls.Certificate
	for time.Now().Before(deadline) {
		cert2, err = tlsConf.GetClientCertificate(&tls.CertificateRequestInfo{})
		if err != nil {
			t.Fatalf("unexpected error calling GetClientCertificate: %v", err)
		}
		if cert2.Leaf != nil && cert2.Leaf.DNSNames[0] == "sidecar-v2" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cert2 == nil || cert2.Leaf == nil || cert2.Leaf.DNSNames[0] != "sidecar-v2" {
		t.Fatal("timed out waiting for GetClientCertificate to reflect the rewritten cert file")
	}
}

func TestLoad_KeepsLastKnownGoodOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedKeyPair(t, dir, "cert.pem", "key.pem", "sidecar-good")
	caPath := writeCA(t, dir)

	tlsConf, stop, err := tlsconfig.Load(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stop()

	if err := os.WriteFile(certPath, []byte("not a valid cert"), 0644); err != nil {
		t.Fatalf("failed to write corrupt cert: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	cert, err := tlsConf.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("expected GetClientCertificate to keep serving the last-known-good cert, got error: %v", err)
	}
	if cert.Leaf == nil || cert.Leaf.DNSNames[0] != "sidecar-good" {
		t.Error("expected the last-known-good cert to still be served after a corrupt rewrite")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd services/sidecar && go test ./tlsconfig/... -race -v`

- [ ] **Step 4: Implement**

```go
// services/sidecar/tlsconfig/reload.go
package tlsconfig

import (
	"crypto/tls"
	"log"
	"path/filepath"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type reloadableCert struct {
	certPath, keyPath string
	current           atomic.Pointer[tls.Certificate]
}

func watchCert(certPath, keyPath string) (*reloadableCert, func(), error) {
	r := &reloadableCert{certPath: certPath, keyPath: keyPath}
	if err := r.reload(); err != nil {
		return nil, nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	dirs := map[string]bool{filepath.Dir(certPath): true, filepath.Dir(keyPath): true}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, nil, err
		}
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == certPath || event.Name == keyPath {
					if err := r.reload(); err != nil {
						log.Printf("tlsconfig: failed to reload cert/key, keeping last-known-good: %v", err)
					}
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-done:
				_ = watcher.Close()
				return
			}
		}
	}()

	stop := func() { close(done) }
	return r, stop, nil
}

func (r *reloadableCert) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.current.Store(&cert)
	return nil
}

func (r *reloadableCert) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}
```

Change `services/sidecar/tlsconfig/tlsconfig.go`'s `Load`:
```go
func Load(certPath, keyPath, caPath string) (*tls.Config, func(), error) {
	reloadable, stop, err := watchCert(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading client cert/key: %w", err)
	}

	caData, err := os.ReadFile(caPath)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		stop()
		return nil, nil, fmt.Errorf("no valid certificates found in CA file %s", caPath)
	}

	return &tls.Config{
		GetClientCertificate: reloadable.GetClientCertificate,
		RootCAs:              pool,
	}, stop, nil
}
```

- [ ] **Step 5: Update the call site in `services/sidecar/main.go`**

```go
	transportCreds := insecure.NewCredentials()
	if tlsCertPath != "" {
		tlsConf, stopCertWatch, err := tlsconfig.Load(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Fatalf("failed to load TLS config: %v", err)
		}
		defer stopCertWatch()
		transportCreds = credentials.NewTLS(tlsConf)
		log.Printf("ratecap-sidecar: mTLS enabled")
	}
```
Also check `services/sidecar/main_test.go` and any integration test file for other `tlsconfig.Load(` call sites and update identically.

- [ ] **Step 6: Run the full sidecar suite**

Run: `cd services/sidecar && go build ./... && go test ./... -race`

- [ ] **Step 7: Commit**

```bash
git add services/sidecar/tlsconfig/reload.go services/sidecar/tlsconfig/reload_test.go services/sidecar/tlsconfig/tlsconfig.go services/sidecar/main.go services/sidecar/go.mod services/sidecar/go.sum
git commit -m "feat(sidecar): hot-reload the client cert/key via fsnotify instead of loading once at startup"
```

---

### Task 6: Helm `NetworkPolicy` template (opt-in)

**Files:**
- Create: `deploy/helm/ratecap/templates/networkpolicy.yaml`
- Modify: `deploy/helm/ratecap/values.yaml`

**Interfaces:**
- Produces: `networkPolicy.enabled` values flag, default `false` (consistent with this chart's established opt-in-security-feature convention: `tls.enabled`, `redis.sentinel.enabled`).

- [ ] **Step 1: Add the values flag**

In `deploy/helm/ratecap/values.yaml`, append:
```yaml
# networkPolicy is off by default, matching this chart's tls/sentinel
# opt-in convention. When enabled, it operationalizes the network-boundary
# claim SECURITY.md already makes ("must run on private, trusted network
# only") at the Kubernetes level — restricting which pods can reach core's
# gRPC/health ports and Redis's port, rather than leaving that entirely to
# operator-managed cluster-wide policy. Requires a CNI that enforces
# NetworkPolicy (Calico, Cilium, etc.) — on a CNI that doesn't, this
# resource is created but has no effect (fails open, not closed).
networkPolicy:
  enabled: false
```

- [ ] **Step 2: Write the template**

```yaml
# deploy/helm/ratecap/templates/networkpolicy.yaml
{{- if .Values.networkPolicy.enabled }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .Release.Name }}-core
spec:
  podSelector:
    matchLabels:
      app: {{ .Release.Name }}-core
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: {{ .Release.Name }}-sidecar
      ports:
        - port: {{ .Values.core.grpcPort }}
        - port: {{ .Values.core.healthPort }}
        {{- if .Values.tls.enabled }}
        - port: 9443
        {{- end }}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .Release.Name }}-redis
spec:
  podSelector:
    matchLabels:
      app: {{ .Release.Name }}-redis
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: {{ .Release.Name }}-core
      ports:
        - port: {{ .Values.redis.port }}
{{- if .Values.redis.sentinel.enabled }}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .Release.Name }}-redis-sentinel
spec:
  podSelector:
    matchLabels:
      app: {{ .Release.Name }}-redis-sentinel
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: {{ .Release.Name }}-core
      ports:
        - port: {{ .Values.redis.sentinel.port }}
{{- end }}
{{- end }}
```

- [ ] **Step 3: Verify with the existing CI-equivalent commands**

```bash
helm lint deploy/helm/ratecap
helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set concurrencySigningKey.existingSecretName=test-signing-key --set adminSecret.existingSecretName=test-admin-secret
helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set concurrencySigningKey.existingSecretName=test-signing-key --set adminSecret.existingSecretName=test-admin-secret --set networkPolicy.enabled=true
helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set concurrencySigningKey.existingSecretName=test-signing-key --set adminSecret.existingSecretName=test-admin-secret --set networkPolicy.enabled=true --set redis.sentinel.enabled=true
```
Expected: all four render without error; only the last two include `NetworkPolicy` resources, and only the very last includes the Sentinel `NetworkPolicy`.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/ratecap/templates/networkpolicy.yaml deploy/helm/ratecap/values.yaml
git commit -m "feat(helm): add opt-in NetworkPolicy templates restricting core/redis/sentinel ingress"
```

---

### Task 7: Documentation — the full mTLS migration path

**Files:**
- Modify: `ARCHITECTURE.md`, `SECURITY.md`

- [ ] **Step 1: Add an mTLS migration section to `ARCHITECTURE.md`**

Find the right insertion point via `grep -n "^## " ARCHITECTURE.md` (likely right after the existing Observability section from Phase 1), then add:
```markdown
## mTLS migration path (v2.7.0)

Flipping mTLS from off to fully enforced in one step is a flag day — any sidecar without a cert yet goes down the instant the switch flips. `RATECAP_TLS_MODE` adds the middle rung every mature service mesh (Istio, Linkerd) ships for exactly this reason:

| `RATECAP_TLS_MODE` | `services/core` behavior |
| --- | --- |
| unset / `off` (default) | Unchanged from pre-v2.7.0: plaintext-only on `:9090`, unless `RATECAP_TLS_CERT_PATH`/`KEY_PATH`/`CA_PATH` happen to be set, in which case the single listener becomes TLS-only (`RequireAndVerifyClientCert`) — this is the same implicit behavior that existed before this env var did. |
| `permissive` | The plaintext listener on `:9090` keeps running unchanged. A **second** listener (`RATECAP_GRPC_TLS_ADDR`, default `:9443`) is added, with `ClientAuth: VerifyClientCertIfGiven` — a sidecar without a cert can still connect (server-authenticated only); a sidecar with a cert gets it verified. Sidecars migrate one at a time by pointing `RATECAP_CORE_ADDR` at the TLS port once they have certs. |
| `strict` | Same as the implicit "certs set" behavior above, now reachable via an explicit, self-documenting mode string: single listener, TLS-only, `RequireAndVerifyClientCert`. |

Both `permissive` and `strict` require the TLS cert env vars to be set — core fails closed at startup otherwise.

**Recommended rollout sequence** (mirrors Istio's/Linkerd's own documented migration path — do not skip a step):
1. Ship with `off` as the default (this release does — no shipped default changes).
2. Once every sidecar in a fleet is capable of connecting with a cert, an operator sets `RATECAP_TLS_MODE=permissive` on core and migrates sidecars one at a time.
3. Watch `ratecap_core_connection_security_total{transport="plaintext"}` (see the Observability section) drop to zero across a full deploy cycle — this is the "is anything still on plaintext" signal a strict cutover needs before it's safe.
4. Only once that metric is confirmed zero does an operator flip to `RATECAP_TLS_MODE=strict`.

RateCap does not flip any of this automatically or by default — every transition above is an explicit operator action.

### Certificate SAN/hostname preflight

`ratecapctl tls check <cert-path> <expected-host>` verifies a certificate's SAN list covers the hostname it will actually serve/dial *before* deploying it — catching the exact "demo certs' SAN (core/sidecar) don't match a real Helm release name" failure mode `deploy/helm/ratecap/values.yaml` already documents as producing no server-side log.

### Certificate hot-reload

Both services now watch their cert/key files via the same `fsnotify` library already used for `ratecap.yaml`'s config hot-reload, and swap in the reloaded certificate without a restart — an externally-rotated cert (e.g. cert-manager) takes effect automatically. The CA pool is loaded once at startup and is NOT hot-reloaded; CA rotation still requires a restart.
```

- [ ] **Step 2: Add a subsection to `SECURITY.md`**

Find the right insertion point via `grep -n "^## " SECURITY.md`, then add a subsection near the existing transport-encryption discussion:
```markdown
## mTLS migration mode

`RATECAP_TLS_MODE=permissive` on `services/core` intentionally accepts connections from clients *without* a certificate (`ClientAuth: VerifyClientCertIfGiven`) on its TLS listener, alongside the still-running plaintext listener — this is a deliberate transitional weakening of the all-or-nothing mTLS posture, scoped to migration only. Do not leave a fleet running in `permissive` mode indefinitely; the whole point is to reach `strict` once `ratecap_core_connection_security_total{transport="plaintext"}` confirms zero remaining plaintext traffic. The shared-secret (`RATECAP_SHARED_SECRET`) gRPC interceptor remains active and enforced on both listeners throughout every mode — mTLS is a second, independent layer, not a replacement for it.
```

- [ ] **Step 3: Commit**

```bash
git add ARCHITECTURE.md SECURITY.md
git commit -m "docs: document the mTLS permissive-mode migration path, SAN preflight, and cert hot-reload"
```

---

### Task 8: Version bump to v2.7.0

**Files:**
- Modify: `VERSION`, `CHANGELOG.md`

- [ ] **Step 1: Add the CHANGELOG entry**

Insert above the current top heading:
```markdown
## [2.7.0] — 2026-08-29 — Phase 3 Security: mTLS PERMISSIVE Mode

Minor release: Phase 3 of the v3 upgrade roadmap — adds the migration rung between "off" and "all-or-nothing strict" mTLS that every mature service mesh ships. No shipped default changes.

### Added

- `RATECAP_TLS_MODE=off|permissive|strict` on `services/core`. `off` (default, unset) preserves exact pre-v2.7.0 behavior. `permissive` adds a second, additive TLS listener (`RATECAP_GRPC_TLS_ADDR`, default `:9443`) accepting connections with or without a client cert, alongside the unchanged plaintext listener — letting sidecars migrate one at a time. `strict` is the existing all-TLS behavior, now reachable via an explicit mode string.
- `ratecap_core_connection_security_total{transport,client_cert}` — the "is anything still on plaintext" signal a `strict` cutover needs before it's safe.
- `ratecapctl tls check <cert-path> <expected-host>` — a SAN/hostname preflight check catching the exact failure mode the Helm chart's `values.yaml` already documents as producing no server-side log.
- Certificate hot-reload via `fsnotify` on both `services/core` (server cert) and `services/sidecar` (client cert) — an externally-rotated cert now takes effect without a pod restart. The CA pool is not hot-reloaded.
- An opt-in Helm `NetworkPolicy` (`networkPolicy.enabled`, default `false`) restricting core's gRPC/health ports to the sidecar's pod selector and Redis's (and, if enabled, Sentinel's) port to core's selector.

### Security

- `RATECAP_TLS_MODE=permissive` is a deliberate, scoped transitional weakening (`ClientAuth: VerifyClientCertIfGiven` on the new listener only) — see `SECURITY.md`'s new mTLS migration mode section for the intended usage and the signal that says it's safe to move to `strict`.
```

- [ ] **Step 2: Bump `VERSION`**

```
2.7.0
```

- [ ] **Step 3: Commit**

```bash
git add VERSION CHANGELOG.md
git commit -m "chore: bump VERSION to 2.7.0 for Phase 3"
```

---

## Post-Implementation (not a task — controller responsibility)

After all 8 tasks pass task review and the final whole-branch review is clean — **run the empirical demo-stack verification the Phase 2 final review established as necessary** (`cd deploy && bash generate-demo-certs.sh && docker compose up --build`, confirm all containers reach `Up`, `/healthz` returns 200, and the admin lever still works) before considering this phase done, since Task 1 and Task 4 both touch `services/core`'s TLS/listener startup path in ways a task-scoped review might not catch a demo-stack regression in. Then: push the branch (the user runs `git push` themselves due to the `DestructiveGuard` hook), open a PR into `develop` titled `feat: RateCap v3 roadmap Phase 3 — Security: mTLS PERMISSIVE Mode`, merge once CI is green, then promote `develop` → `main` and tag/release `v2.7.0` — mirroring exactly the Phase 1/2 cycle.
