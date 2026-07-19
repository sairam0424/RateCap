# RateCap v2 Phase 4c: Helm Chart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a production-shaped Helm chart (`deploy/helm/ratecap/`) covering `redis`/`core`/`sidecar`/`sampleapp`, plus the two small pieces of genuinely new Go code the chart needs (a `grpc.health.v1.Health` service on `core`, a `/healthz` HTTP handler on `sidecar`) — proven end-to-end against a real `kind` cluster, both with and without mTLS.

**Architecture:** Registry-agnostic chart (`values.yaml` takes image repo/tag per component, chart never builds images), `ClusterIP`-only Services, BYO-only Secrets (chart never generates credentials), `sampleapp` gated behind a values flag defaulting off. `core`'s new health service runs on its own dedicated, always-plaintext port — separate from the main mTLS-enforcing gRPC port — because Kubernetes' native `grpc` probe action has no TLS/client-cert support, so a probe on the mTLS port would always fail once mTLS is enabled.

**Tech Stack:** Go 1.26 (`google.golang.org/grpc/health` — already a subpackage of the `google.golang.org/grpc` v1.82.0 dependency already in `services/core/go.mod`, zero new dependencies), Helm 3 chart templates, `kind` for local cluster verification.

## Global Constraints

- TDD for all Go code changes: write the failing test first, confirm it fails for the stated reason, implement, confirm it passes.
- `gofmt -l` must report zero files before every commit touching Go code; `go vet ./...` clean; `go test ./... -race` must pass for the affected module (`services/core` and/or `services/sidecar`) before every commit.
- No comments except non-obvious WHY.
- No `Co-Authored-By` trailers in any commit.
- Every Service in the chart must be `ClusterIP` — never `NodePort`/`LoadBalancer`.
- The chart must never generate, store, or default a secret/cert value — `sharedSecret`/`tls` are BYO-only via `existingSecretName` fields.
- `sampleapp.enabled` must default to `false` in `values.yaml`.
- `deploy/docker-compose.yml`, `deploy/ratecap.yaml`, `deploy/docker-compose.bench.yml`, and `deploy/generate-demo-certs.sh` are left byte-for-byte unchanged — the chart is purely additive.

---

## Task 1: `core` gains a gRPC health service on a dedicated port

**Files:**
- Modify: `services/core/main.go`
- Test: `services/core/health_main_test.go`

**Interfaces:**
- Produces: a `grpc.health.v1.Health`-compliant service, listening on `RATECAP_HEALTH_ADDR` (default `:9091`), reporting `SERVING` unconditionally once started. Reachable independently of `RATECAP_REDIS_ADDR`'s reachability and independently of whether mTLS is configured on the main port. Task 3 (chart templates) consumes this exact env var name and default port.

Every existing `core` env var, flag, and behavior is unchanged — this task only adds new code, appended after the existing `grpcServer`/`ratecapv1.RegisterRatecapServiceServer` setup.

- [ ] **Step 1: Write the failing test**

Create `services/core/health_main_test.go`:

```go
package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestMain_HealthServerRespondsServingRegardlessOfRedisReachability(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ratecap.yaml")
	validConfig := `sync_rate: 5
tiers:
  rate_limiter:
    default_rate: 100
    default_burst: 500
    shadow_mode: false
  concurrency_limiter:
    default_max_concurrent: 50
    max_request_duration_ms: 5000
    shadow_mode: false
  fleet_shedder:
    default_max_concurrent: 100
    reserved_critical_pct: 20
    max_request_duration_ms: 5000
    default_priority: normal
    shadow_mode: false
`
	if err := os.WriteFile(configPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Env = append(os.Environ(),
		"RATECAP_CONFIG_PATH="+configPath,
		"RATECAP_SHARED_SECRET=test-secret",
		"RATECAP_GRPC_ADDR=:0",
		"RATECAP_HEALTH_ADDR=:19191",
		"RATECAP_REDIS_ADDR=127.0.0.1:1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var conn *grpc.ClientConn
	var err error
	for i := 0; i < 20; i++ {
		conn, err = grpc.NewClient("127.0.0.1:19191", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("failed to dial health server: %v", err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	var resp *healthpb.HealthCheckResponse
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		resp, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("expected SERVING, got %v", resp.Status)
	}
}
```

Note: `RATECAP_REDIS_ADDR=127.0.0.1:1` deliberately points at an address nothing listens on, and the test asserts the health server still answers `SERVING` — this is what proves the health server isn't blocked on a live Redis or a live gRPC client connection, since `redis.NewClient` and `grpc.NewClient` both connect lazily. No Docker is required for this test.

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd services/core && go test . -run TestMain_HealthServerRespondsServingRegardlessOfRedisReachability -v`
Expected: `FAIL` — dial to `127.0.0.1:19191` never succeeds within the retry loop (nothing is listening there yet), test times out with `failed to dial health server` or `health check failed`.

- [ ] **Step 3: Add the health service to `services/core/main.go`**

Add to the import block:

```go
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
```

(placed alongside the existing `"google.golang.org/grpc/credentials"` import line)

Insert immediately after the existing `ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(pipeline, redisStore))` line, before the final `log.Printf("ratecap-core listening on %s", listenAddr)` / `grpcServer.Serve(lis)` block:

```go
	// The health service is served on its own plaintext, unauthenticated
	// listener rather than the main gRPC port: Kubernetes' native grpc probe
	// action has no TLS/client-cert support, so a probe on the mTLS-enforcing
	// main port would always fail once mTLS is enabled.
	healthAddr := os.Getenv("RATECAP_HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = ":9091"
	}
	healthLis, err := net.Listen("tcp", healthAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", healthAddr, err)
	}
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthGRPCServer := grpc.NewServer()
	healthpb.RegisterHealthServer(healthGRPCServer, healthServer)
	go func() {
		log.Printf("ratecap-core health server listening on %s", healthAddr)
		if err := healthGRPCServer.Serve(healthLis); err != nil {
			log.Fatalf("health grpc server failed: %v", err)
		}
	}()
```

No `go.mod`/`go.sum` change is needed — `google.golang.org/grpc/health` and `google.golang.org/grpc/health/grpc_health_v1` are subpackages of `google.golang.org/grpc`, already required at v1.82.0 in `services/core/go.mod`.

- [ ] **Step 4: Run the test to confirm it passes**

Run: `cd services/core && go test . -run TestMain_HealthServerRespondsServingRegardlessOfRedisReachability -v`
Expected:
```
=== RUN   TestMain_HealthServerRespondsServingRegardlessOfRedisReachability
--- PASS: TestMain_HealthServerRespondsServingRegardlessOfRedisReachability (1.03s)
PASS
ok  	github.com/ratecap/core	1.815s
```

- [ ] **Step 5: Run gofmt, go vet, and the full module test suite**

Run: `cd services/core && gofmt -l .`
Expected: no output

Run: `cd services/core && go vet ./...`
Expected: no output

Run: `cd services/core && go test ./... -race`
Expected: `ok` for every package (`core`, `auth`, `config`, `grpcserver`, `limiter`, `store`, `tlsconfig`) — no regressions in any existing test.

- [ ] **Step 6: Commit**

```bash
git add services/core/main.go services/core/health_main_test.go
git commit -m "feat(core): add grpc.health.v1 health service on a dedicated plaintext port"
```

---

## Task 2: `sidecar` gains a `/healthz` HTTP handler

**Files:**
- Modify: `services/sidecar/main.go`
- Test: `services/sidecar/healthz_main_test.go`

**Interfaces:**
- Consumes: nothing new from Task 1 — independent.
- Produces: a `healthzHandler(w http.ResponseWriter, r *http.Request)` function, registered at `/healthz` on the sidecar's existing `http.NewServeMux()`, alongside `/check`, `/release`, `/metrics`. Always responds `200 OK`. Task 3 (chart templates) consumes this exact path and status code for its `httpGet` probes.

- [ ] **Step 1: Write the failing test**

Create `services/sidecar/healthz_main_test.go`:

```go
package main

import (
	"net/http/httptest"
	"testing"
)

func TestHealthzHandler_AlwaysReturns200(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	healthzHandler(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
```

This test lives in `package main` (not `package main_test`), matching this file's existing sibling `main_test.go`, which also uses `package main` to test `resolveMaxInflight` directly as an unexported function.

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd services/sidecar && go test . -run TestHealthzHandler_AlwaysReturns200 -v`
Expected: compile error — `healthzHandler` is undefined.

- [ ] **Step 3: Add `healthzHandler` and register it in `services/sidecar/main.go`**

Add immediately before the existing `resolveMaxInflight` function:

```go
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

```

Change:

```go
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
	mux.Handle("/metrics", metrics.Handler())
```

to:

```go
	mux := http.NewServeMux()
	mux.Handle("/check", proxy.NewHandler(client, proxy.Sheddable, shedder))
	mux.Handle("/release", proxy.NewReleaseHandler(client))
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", healthzHandler)
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `cd services/sidecar && go test . -run TestHealthzHandler_AlwaysReturns200 -v`
Expected:
```
=== RUN   TestHealthzHandler_AlwaysReturns200
--- PASS: TestHealthzHandler_AlwaysReturns200 (0.00s)
PASS
ok  	github.com/ratecap/sidecar	0.667s
```

- [ ] **Step 5: Run gofmt, go vet, and the full module test suite**

Run: `cd services/sidecar && gofmt -l .`
Expected: no output

Run: `cd services/sidecar && go vet ./...`
Expected: no output

Run: `cd services/sidecar && go test ./... -race`
Expected: `ok` for every package (`sidecar`, `auth`, `decisionlog`, `metrics`, `proxy`, `shadow`, `tlsconfig`, `worker`) — no regressions.

- [ ] **Step 6: Commit**

```bash
git add services/sidecar/main.go services/sidecar/healthz_main_test.go
git commit -m "feat(sidecar): add /healthz endpoint"
```

---

## Task 3: The Helm chart

**Files:**
- Create: `deploy/helm/ratecap/Chart.yaml`
- Create: `deploy/helm/ratecap/values.yaml`
- Create: `deploy/helm/ratecap/templates/configmap.yaml`
- Create: `deploy/helm/ratecap/templates/redis.yaml`
- Create: `deploy/helm/ratecap/templates/core.yaml`
- Create: `deploy/helm/ratecap/templates/sidecar.yaml`
- Create: `deploy/helm/ratecap/templates/sampleapp.yaml`
- Create: `deploy/helm/ratecap/README.md`

**Interfaces:**
- Consumes: `RATECAP_HEALTH_ADDR` (Task 1, default `:9091`) and `/healthz` (Task 2) as the exact probe targets `core.yaml`/`sidecar.yaml` wire up.
- Produces: a `helm install`-able chart at `deploy/helm/ratecap`. Task 4's smoke test installs this exact chart.

There is no TDD analog for Helm YAML — `helm lint`/`helm template` dry-run checks serve that role in this task, with Task 4's real `kind`-cluster install as the actual integration proof (mirroring how this project's docker-compose demo has always needed live e2e verification beyond what unit tests alone can show).

- [ ] **Step 1: Create `deploy/helm/ratecap/Chart.yaml`**

```yaml
apiVersion: v2
name: ratecap
description: A faithful, open-source recreation of Stripe's four-tier rate-limiter and load-shedder architecture
type: application
version: 0.1.0
appVersion: "2.2.0"
```

- [ ] **Step 2: Create `deploy/helm/ratecap/values.yaml`**

```yaml
redis:
  image:
    repository: redis
    tag: 7-alpine
    pullPolicy: IfNotPresent
  port: 6379

core:
  image:
    repository: ratecap-core
    tag: latest
    pullPolicy: IfNotPresent
  grpcPort: 9090
  healthPort: 9091
  replicaCount: 1

sidecar:
  image:
    repository: ratecap-sidecar
    tag: latest
    pullPolicy: IfNotPresent
  port: 8080
  maxInflightRequests: 500
  replicaCount: 1

sampleapp:
  enabled: false
  image:
    repository: ratecap-sampleapp
    tag: latest
    pullPolicy: IfNotPresent
  port: 3000

# config.yaml is the content of ratecap.yaml, mounted into core via a
# ConfigMap. Defaults mirror deploy/ratecap.yaml (the docker-compose demo's
# config) exactly, so a chart install behaves the same way out of the box.
config:
  yaml: |
    sync_rate: 5
    tiers:
      rate_limiter:
        default_rate: 2
        default_burst: 5
        shadow_mode: false
      concurrency_limiter:
        default_max_concurrent: 3
        max_request_duration_ms: 30000
        shadow_mode: false
      fleet_shedder:
        default_max_concurrent: 5
        reserved_critical_pct: 40
        max_request_duration_ms: 30000
        default_priority: sheddable
        shadow_mode: false

# sharedSecret is BYO-only: the chart never generates or stores a secret
# value. Create the Secret out-of-band first, e.g.:
#   kubectl create secret generic ratecap-shared-secret \
#     --from-literal=shared-secret=<your-secret-value>
# then set existingSecretName/existingSecretKey to match.
sharedSecret:
  existingSecretName: ""
  existingSecretKey: "shared-secret"

# tls is optional and off by default, matching the existing mTLS precedent
# (RATECAP_TLS_CERT_PATH/KEY_PATH/CA_PATH). Also BYO-only: create the Secret
# out-of-band first. IMPORTANT: certs' subjectAltName MUST match this
# release's actual Service names (<release-name>-core / <release-name>-sidecar)
# — deploy/generate-demo-certs.sh's output (SAN: DNS:core / DNS:sidecar) will
# NOT work here and fails with no server-side log (see README.md). Example:
#   kubectl create secret generic ratecap-core-tls \
#     --from-file=tls.crt=core-cert.pem --from-file=tls.key=core-key.pem \
#     --from-file=ca.crt=ca.pem
# then set enabled: true and the two existingSecretName fields below.
tls:
  enabled: false
  core:
    existingSecretName: ""
  sidecar:
    existingSecretName: ""
```

- [ ] **Step 3: Create `deploy/helm/ratecap/templates/configmap.yaml`**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  ratecap.yaml: |
{{ .Values.config.yaml | indent 4 }}
```

- [ ] **Step 4: Create `deploy/helm/ratecap/templates/redis.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Release.Name }}-redis
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-redis
    spec:
      containers:
        - name: redis
          image: "{{ .Values.redis.image.repository }}:{{ .Values.redis.image.tag }}"
          imagePullPolicy: {{ .Values.redis.image.pullPolicy }}
          ports:
            - containerPort: {{ .Values.redis.port }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-redis
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-redis
  ports:
    - port: {{ .Values.redis.port }}
      targetPort: {{ .Values.redis.port }}
```

- [ ] **Step 5: Create `deploy/helm/ratecap/templates/core.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-core
spec:
  replicas: {{ .Values.core.replicaCount }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}-core
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-core
    spec:
      containers:
        - name: core
          image: "{{ .Values.core.image.repository }}:{{ .Values.core.image.tag }}"
          imagePullPolicy: {{ .Values.core.image.pullPolicy }}
          ports:
            - containerPort: {{ .Values.core.grpcPort }}
            - containerPort: {{ .Values.core.healthPort }}
          env:
            - name: RATECAP_CONFIG_PATH
              value: /etc/ratecap/ratecap.yaml
            - name: RATECAP_REDIS_ADDR
              value: "{{ .Release.Name }}-redis:{{ .Values.redis.port }}"
            - name: RATECAP_GRPC_ADDR
              value: ":{{ .Values.core.grpcPort }}"
            - name: RATECAP_HEALTH_ADDR
              value: ":{{ .Values.core.healthPort }}"
            - name: RATECAP_SHARED_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.sharedSecret.existingSecretName }}
                  key: {{ .Values.sharedSecret.existingSecretKey }}
            {{- if .Values.tls.enabled }}
            - name: RATECAP_TLS_CERT_PATH
              value: /etc/ratecap/certs/tls.crt
            - name: RATECAP_TLS_KEY_PATH
              value: /etc/ratecap/certs/tls.key
            - name: RATECAP_TLS_CA_PATH
              value: /etc/ratecap/certs/ca.crt
            {{- end }}
          volumeMounts:
            - name: config
              mountPath: /etc/ratecap/ratecap.yaml
              subPath: ratecap.yaml
            {{- if .Values.tls.enabled }}
            - name: tls-certs
              mountPath: /etc/ratecap/certs
              readOnly: true
            {{- end }}
          readinessProbe:
            grpc:
              port: {{ .Values.core.healthPort }}
          livenessProbe:
            grpc:
              port: {{ .Values.core.healthPort }}
      volumes:
        - name: config
          configMap:
            name: {{ .Release.Name }}-config
        {{- if .Values.tls.enabled }}
        - name: tls-certs
          secret:
            secretName: {{ .Values.tls.core.existingSecretName }}
        {{- end }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-core
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-core
  ports:
    - name: grpc
      port: {{ .Values.core.grpcPort }}
      targetPort: {{ .Values.core.grpcPort }}
    - name: health
      port: {{ .Values.core.healthPort }}
      targetPort: {{ .Values.core.healthPort }}
```

- [ ] **Step 6: Create `deploy/helm/ratecap/templates/sidecar.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-sidecar
spec:
  replicas: {{ .Values.sidecar.replicaCount }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}-sidecar
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-sidecar
    spec:
      containers:
        - name: sidecar
          image: "{{ .Values.sidecar.image.repository }}:{{ .Values.sidecar.image.tag }}"
          imagePullPolicy: {{ .Values.sidecar.image.pullPolicy }}
          ports:
            - containerPort: {{ .Values.sidecar.port }}
          env:
            - name: RATECAP_CORE_ADDR
              value: "{{ .Release.Name }}-core:{{ .Values.core.grpcPort }}"
            - name: RATECAP_SIDECAR_ADDR
              value: ":{{ .Values.sidecar.port }}"
            - name: RATECAP_MAX_INFLIGHT_REQUESTS
              value: "{{ .Values.sidecar.maxInflightRequests }}"
            - name: RATECAP_SHARED_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.sharedSecret.existingSecretName }}
                  key: {{ .Values.sharedSecret.existingSecretKey }}
            {{- if .Values.tls.enabled }}
            - name: RATECAP_TLS_CERT_PATH
              value: /etc/ratecap/certs/tls.crt
            - name: RATECAP_TLS_KEY_PATH
              value: /etc/ratecap/certs/tls.key
            - name: RATECAP_TLS_CA_PATH
              value: /etc/ratecap/certs/ca.crt
            {{- end }}
          {{- if .Values.tls.enabled }}
          volumeMounts:
            - name: tls-certs
              mountPath: /etc/ratecap/certs
              readOnly: true
          {{- end }}
          readinessProbe:
            httpGet:
              path: /healthz
              port: {{ .Values.sidecar.port }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: {{ .Values.sidecar.port }}
      {{- if .Values.tls.enabled }}
      volumes:
        - name: tls-certs
          secret:
            secretName: {{ .Values.tls.sidecar.existingSecretName }}
      {{- end }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-sidecar
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-sidecar
  ports:
    - port: {{ .Values.sidecar.port }}
      targetPort: {{ .Values.sidecar.port }}
```

- [ ] **Step 7: Create `deploy/helm/ratecap/templates/sampleapp.yaml`**

```yaml
{{- if .Values.sampleapp.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-sampleapp
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Release.Name }}-sampleapp
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}-sampleapp
    spec:
      containers:
        - name: sampleapp
          image: "{{ .Values.sampleapp.image.repository }}:{{ .Values.sampleapp.image.tag }}"
          imagePullPolicy: {{ .Values.sampleapp.image.pullPolicy }}
          ports:
            - containerPort: {{ .Values.sampleapp.port }}
          env:
            - name: RATECAP_SIDECAR_ADDR
              value: "http://{{ .Release.Name }}-sidecar:{{ .Values.sidecar.port }}"
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-sampleapp
spec:
  type: ClusterIP
  selector:
    app: {{ .Release.Name }}-sampleapp
  ports:
    - port: {{ .Values.sampleapp.port }}
      targetPort: {{ .Values.sampleapp.port }}
{{- end }}
```

Note: `deploy/sampleapp/main.go` hardcodes `:3000` and reads no listen-port env var — only `RATECAP_SIDECAR_ADDR`. Do not invent a listen-port env var here; there is none to wire.

- [ ] **Step 8: Create `deploy/helm/ratecap/README.md`, documenting the BYO-secret flow and the mTLS cert SAN gotcha**

```markdown
# RateCap Helm Chart

Deploys `redis`, `ratecap-core`, `ratecap-sidecar`, and (optionally) `deploy/sampleapp` to Kubernetes.

## Images

This chart is registry-agnostic — it does not build images. Point `<component>.image.repository`/`tag` in `values.yaml` at wherever your images live. For local development with `kind`:

```bash
docker build -f services/core/Dockerfile -t ratecap-core:latest .
docker build -f services/sidecar/Dockerfile -t ratecap-sidecar:latest .
docker build -f deploy/sampleapp/Dockerfile -t ratecap-sampleapp:latest .
kind load docker-image ratecap-core:latest ratecap-sidecar:latest ratecap-sampleapp:latest --name <your-cluster-name>
```

(all 3 `docker build` commands run from the repo root, matching `deploy/docker-compose.yml`'s existing `build: context: ..` convention)

## Required: create the shared-secret Secret before installing

This chart never generates or stores a secret value — you must create it yourself first:

```bash
kubectl create secret generic ratecap-shared-secret \
  --from-literal=shared-secret=<your-secret-value>
```

Then install with:

```bash
helm install my-ratecap deploy/helm/ratecap \
  --set sharedSecret.existingSecretName=ratecap-shared-secret
```

## Optional: enabling mTLS

mTLS is off by default. To enable it, you need TLS Secrets for both `core` and `sidecar`:

```bash
kubectl create secret generic ratecap-core-tls \
  --from-file=tls.crt=core-cert.pem --from-file=tls.key=core-key.pem --from-file=ca.crt=ca.pem
kubectl create secret generic ratecap-sidecar-tls \
  --from-file=tls.crt=sidecar-cert.pem --from-file=tls.key=sidecar-key.pem --from-file=ca.crt=ca.pem
```

```bash
helm install my-ratecap deploy/helm/ratecap \
  --set sharedSecret.existingSecretName=ratecap-shared-secret \
  --set tls.enabled=true \
  --set tls.core.existingSecretName=ratecap-core-tls \
  --set tls.sidecar.existingSecretName=ratecap-sidecar-tls
```

### ⚠️ Certificate `subjectAltName` must match your release's actual Service names — do NOT reuse `deploy/generate-demo-certs.sh`'s output as-is

`deploy/generate-demo-certs.sh` (used by the docker-compose demo) generates certs with `subjectAltName=DNS:core` / `DNS:sidecar`, matching compose's static service names. This chart's Kubernetes Service names are release-name-prefixed instead — e.g. installing as `helm install my-ratecap ...` produces Services named `my-ratecap-core` and `my-ratecap-sidecar`, not `core`/`sidecar`.

If you generate certs with the wrong SAN, mTLS will fail: every request through `sidecar` will return a bare `500 upstream check failed`, **with no server-side log at all** (this is a real, separate gap in `services/sidecar/proxy/proxy.go`'s error handling — the failure is silent server-side, unlike other error paths in this codebase that do log). The pods will still report `Ready` (the health probes are unaffected — see below), so this failure mode looks exactly like "everything is healthy, but every real request fails."

When generating your own certs for this chart, set the SAN to match your actual release name, e.g. for a release named `my-ratecap`:

```bash
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout core-key.pem -out core.csr -nodes \
  -subj "/CN=ratecap-core" -addext "subjectAltName=DNS:my-ratecap-core"
# ... sign with your CA, same pattern for sidecar with DNS:my-ratecap-sidecar
```

## Health checks

- `core` is probed via Kubernetes' native `grpc` probe action against the dedicated health port (`core.healthPort`, default `9091`) — a separate, always-plaintext port from the main gRPC port (`core.grpcPort`), specifically so the probe keeps working even when mTLS is enabled on the main port (Kubernetes' `grpc` probe action has no TLS/client-cert support).
- `sidecar` is probed via a plain `httpGet` against `/healthz` on its existing HTTP port.

## Scope

Every Service in this chart is `ClusterIP` — never exposed via `NodePort`/`LoadBalancer`. If you need external access, layer an Ingress or gateway on top yourself; that's outside this chart's scope.

`sampleapp` is disabled by default (`sampleapp.enabled: false`) and is not intended for production use (see the main repo's `SECURITY.md`) — it exists for local verification and demos. Enable it with `--set sampleapp.enabled=true`.
```

- [ ] **Step 9: Lint the chart**

Run: `cd deploy/helm/ratecap && helm lint .`
Expected:
```
==> Linting .
[INFO] Chart.yaml: icon is recommended

1 chart(s) linted, 0 chart(s) failed
```
(the "icon is recommended" note is informational, not a failure)

- [ ] **Step 10: Render the chart's templates and confirm they produce valid manifests**

Run (from the repo root): `helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set sampleapp.enabled=true`
Expected: valid YAML output for every resource (ConfigMap, and a Deployment+Service pair each for `redis`, `core`, `sidecar`, `sampleapp`) with no template errors.

- [ ] **Step 11: Confirm every Service is ClusterIP, and no NodePort/LoadBalancer exists anywhere**

Run: `grep -rn "type: ClusterIP" deploy/helm/ratecap/templates/ | wc -l`
Expected: `4` (one per Service: `redis`, `core`, `sidecar`, `sampleapp`)

Run: `grep -rn "NodePort\|LoadBalancer" deploy/helm/ratecap/templates/`
Expected: no output

- [ ] **Step 12: Confirm no chart-generated secrets exist anywhere**

Run: `grep -rn "randAlphaNum\|randBytes\|genPrivateKey\|genSelfSignedCert" deploy/helm/ratecap/templates/`
Expected: no output

- [ ] **Step 13: Confirm `sampleapp.enabled` defaults to `false`**

Run: `grep -A1 "^sampleapp:" deploy/helm/ratecap/values.yaml`
Expected:
```
sampleapp:
  enabled: false
```

- [ ] **Step 14: Render with `tls.enabled=true` and confirm the TLS env vars appear correctly for both `core` and `sidecar`**

Run: `helm template test-release deploy/helm/ratecap --set sharedSecret.existingSecretName=test-secret --set tls.enabled=true --set tls.core.existingSecretName=core-tls --set tls.sidecar.existingSecretName=sidecar-tls | grep -A5 "RATECAP_TLS_CERT_PATH"`
Expected: two matching blocks (one for `core`, one for `sidecar`), each showing:
```
            - name: RATECAP_TLS_CERT_PATH
              value: /etc/ratecap/certs/tls.crt
            - name: RATECAP_TLS_KEY_PATH
              value: /etc/ratecap/certs/tls.key
            - name: RATECAP_TLS_CA_PATH
              value: /etc/ratecap/certs/ca.crt
```

- [ ] **Step 15: Commit**

```bash
git add deploy/helm/ratecap
git commit -m "feat(deploy): add Helm chart for redis/core/sidecar/sampleapp"
```

---

## Task 4: Real `kind`-cluster smoke test (both without and with mTLS)

**Files:** None — this task makes no code or chart changes. It is a live verification task, mirroring this project's established pattern (e.g. Phase 1's docker-compose live e2e verification) of proving real behavior beyond what unit tests and `helm lint`/`helm template` alone can show.

**Interfaces:**
- Consumes: the chart from Task 3, the health service from Task 1, and `/healthz` from Task 2 — this task is where all three are proven to genuinely work together in a real cluster.

- [ ] **Step 1: Confirm required tooling is present**

Run: `which kind minikube helm kubectl k3d && docker info > /dev/null && echo "DOCKER_OK"`
Expected: all 5 binaries resolve to a path, and `DOCKER_OK` is printed.

- [ ] **Step 2: Create a real `kind` cluster**

Run: `kind create cluster --name ratecap-smoke`
Expected: cluster creation succeeds, ending with `Set kubectl context to "kind-ratecap-smoke"`.

- [ ] **Step 3: Build the 3 images**

Run (from the repo root):
```bash
docker build -f services/core/Dockerfile -t ratecap-core:latest .
docker build -f services/sidecar/Dockerfile -t ratecap-sidecar:latest .
docker build -f deploy/sampleapp/Dockerfile -t ratecap-sampleapp:latest .
```
Expected: all 3 builds succeed (each ends with `naming to docker.io/library/ratecap-<component>:latest done`).

- [ ] **Step 4: Load the images into the `kind` cluster**

Run: `kind load docker-image ratecap-core:latest ratecap-sidecar:latest ratecap-sampleapp:latest --name ratecap-smoke`
Expected: 3 lines, each `Image: "ratecap-<component>:latest" with ID "sha256:..." not yet present on node "ratecap-smoke-control-plane", loading...`

- [ ] **Step 5: Create the shared-secret Secret**

Run: `kubectl create secret generic ratecap-shared-secret --from-literal=shared-secret=smoke-test-secret-value`
Expected: `secret/ratecap-shared-secret created`

- [ ] **Step 6: Install the chart (mTLS off)**

Run: `helm install ratecap-smoke deploy/helm/ratecap --set sharedSecret.existingSecretName=ratecap-shared-secret --set sampleapp.enabled=true`
Expected: `STATUS: deployed`

- [ ] **Step 7: Wait for all pods to reach Ready**

Run: `kubectl wait --for=condition=Ready pod --all --timeout=60s`
Expected: 4 lines, each `pod/ratecap-smoke-<component>-<hash> condition met` — this is the proof that both new health probes (Task 1's `grpc` probe on `core`, Task 2's `httpGet /healthz` probe on `sidecar`) genuinely pass.

- [ ] **Step 8: Confirm clean startup logs**

Run: `kubectl logs deployment/ratecap-smoke-core --tail=10`
Expected:
```
ratecap-core listening on :9090
ratecap-core health server listening on :9091
```
(no "mTLS enabled" line, since TLS is off in this install)

Run: `kubectl logs deployment/ratecap-smoke-sidecar --tail=10`
Expected: `ratecap-sidecar listening on :8080, forwarding to core at ratecap-smoke-core:9090`

- [ ] **Step 9: Port-forward to `sampleapp` and run the full tier-regression check**

Run: `kubectl port-forward svc/ratecap-smoke-sampleapp 13000:3000 &` (background it; note the job/PID to kill later)

Run (Tier 1):
```bash
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w "req $i: %{http_code}\n" http://localhost:13000/checkout; done
```
Expected:
```
req 1: 200
req 2: 200
req 3: 200
req 4: 200
req 5: 200
req 6: 429
```

Run (Tier 2, concurrent):
```bash
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "req $i: %{http_code}\n" http://localhost:13000/slow-report & done
wait
```
Expected: exactly 3 requests report `200` and exactly 2 report `429` (matching `concurrency_limiter.default_max_concurrent: 3` in the chart's default config) — exact ordering may vary since requests run concurrently.

Run (Tier 3, concurrent):
```bash
for i in 1 2 3 4 5 6 7; do curl -s -o /dev/null -w "req $i: %{http_code}\n" "http://localhost:13000/fleet-demo?priority=sheddable" & done
wait
```
Expected: a mix of `200` and `503` responses (matching `fleet_shedder`'s reduced sheddable-traffic cap) — exact counts may vary slightly run-to-run since requests are concurrent, but at least one `503` must appear, confirming Tier 3 is genuinely enforcing its cap.

Run (Tier 4, concurrent):
```bash
for i in $(seq 1 10); do curl -s -o /dev/null -w "req $i: %{http_code}\n" http://localhost:13000/worker-demo & done
wait
```
Expected: all 10 requests report `200`. This is expected, not a bug: the chart's default `sidecar.maxInflightRequests` is `500` (much higher than docker-compose demo's default of `3`), so Tier 4's local shedder does not trigger at this concurrency level — this proves the chart's `RATECAP_MAX_INFLIGHT_REQUESTS` wiring works and sidecar is genuinely handling the traffic, not that Tier 4 is broken.

Kill the port-forward: `kill %1` (or the specific job/PID noted above).

- [ ] **Step 10: Uninstall and confirm clean teardown**

Run: `helm uninstall ratecap-smoke`
Expected: `release "ratecap-smoke" uninstalled`

- [ ] **Step 11: Generate certs with the correct SAN for the mTLS path, and create the TLS Secrets**

Run (from the repo root, using a scratch directory so this does not touch `deploy/certs/`):
```bash
mkdir -p /tmp/ratecap-helm-mtls-certs && cd /tmp/ratecap-helm-mtls-certs

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca-key.pem -out ca.pem -days 1 -nodes \
  -subj "/CN=ratecap-helm-mtls-test-ca"

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout core-key.pem -out core.csr -nodes \
  -subj "/CN=ratecap-core" -addext "subjectAltName=DNS:ratecap-mtls-smoke-core"
openssl x509 -req -in core.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out core-cert.pem -days 1 -copy_extensions copy

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout sidecar-key.pem -out sidecar.csr -nodes \
  -subj "/CN=ratecap-sidecar" -addext "subjectAltName=DNS:ratecap-mtls-smoke-sidecar"
openssl x509 -req -in sidecar.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out sidecar-cert.pem -days 1 -copy_extensions copy
```
Expected: 3 `Certificate request self-signature ok` confirmations (one implicit for the CA, one each for `core`/`sidecar`), no errors.

Note the SAN values (`ratecap-mtls-smoke-core`/`ratecap-mtls-smoke-sidecar`) exactly match `<release-name>-core`/`<release-name>-sidecar` for the release name used in Step 12 below (`ratecap-mtls-smoke`) — this is the exact gotcha documented in Task 3 Step 8's README; getting this wrong (e.g. reusing `deploy/certs/`'s demo certs, which have SAN `core`/`sidecar`) reproduces the silent-500 failure mode.

Run:
```bash
kubectl create secret generic ratecap-core-tls --from-file=tls.crt=/tmp/ratecap-helm-mtls-certs/core-cert.pem --from-file=tls.key=/tmp/ratecap-helm-mtls-certs/core-key.pem --from-file=ca.crt=/tmp/ratecap-helm-mtls-certs/ca.pem
kubectl create secret generic ratecap-sidecar-tls --from-file=tls.crt=/tmp/ratecap-helm-mtls-certs/sidecar-cert.pem --from-file=tls.key=/tmp/ratecap-helm-mtls-certs/sidecar-key.pem --from-file=ca.crt=/tmp/ratecap-helm-mtls-certs/ca.pem
```
Expected: `secret/ratecap-core-tls created` and `secret/ratecap-sidecar-tls created`.

- [ ] **Step 12: Install the chart with mTLS enabled**

Run:
```bash
helm install ratecap-mtls-smoke deploy/helm/ratecap \
  --set sharedSecret.existingSecretName=ratecap-shared-secret \
  --set sampleapp.enabled=true \
  --set tls.enabled=true \
  --set tls.core.existingSecretName=ratecap-core-tls \
  --set tls.sidecar.existingSecretName=ratecap-sidecar-tls
```
Expected: `STATUS: deployed`

- [ ] **Step 13: Wait for Ready and confirm mTLS is genuinely active on both services while the health probes still pass**

Run: `kubectl wait --for=condition=Ready pod --all --timeout=60s`
Expected: 4 lines, each `condition met` — proving the `core` health probe passes even though mTLS is now enabled on `core`'s main port (the whole point of putting the health service on its own dedicated port in Task 1).

Run: `kubectl logs deployment/ratecap-mtls-smoke-core --tail=10`
Expected:
```
ratecap-core: mTLS enabled
ratecap-core listening on :9090
ratecap-core health server listening on :9091
```

Run: `kubectl logs deployment/ratecap-mtls-smoke-sidecar --tail=10`
Expected: includes `ratecap-sidecar: mTLS enabled`

- [ ] **Step 14: Confirm the tier-1 regression still works correctly through the mTLS-secured stack**

Run: `kubectl port-forward svc/ratecap-mtls-smoke-sampleapp 13001:3000 &` (background it)

Run:
```bash
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w "req $i: %{http_code}\n" http://localhost:13001/checkout; done
```
Expected:
```
req 1: 200
req 2: 200
req 3: 200
req 4: 200
req 5: 200
req 6: 429
```
If instead every request returns `500`, the most likely cause is a cert SAN mismatch — re-check that Step 11's certs' SAN exactly matches `ratecap-mtls-smoke-core`/`ratecap-mtls-smoke-sidecar` (the actual Service names for a release named `ratecap-mtls-smoke`), not `core`/`sidecar`.

Kill the port-forward: `kill %1` (or the specific job/PID noted above).

- [ ] **Step 15: Full teardown**

Run:
```bash
helm uninstall ratecap-mtls-smoke
kubectl delete secret ratecap-core-tls ratecap-sidecar-tls ratecap-shared-secret
kind delete cluster --name ratecap-smoke
docker rmi ratecap-core:latest ratecap-sidecar:latest ratecap-sampleapp:latest
rm -rf /tmp/ratecap-helm-mtls-certs
```
Expected: each command completes without error; `kind delete cluster` ends with `Deleted nodes: ["ratecap-smoke-control-plane"]`.

- [ ] **Step 16: No commit for this task** — it makes no file changes. If any step in this task fails, do not proceed to mark this plan complete; investigate and, if a real code/chart defect is found, return to Task 1/2/3 to fix it, re-commit, and re-run this task's steps from the beginning.

---

## Self-Review Notes (completed during plan authoring)

**Spec coverage:** §1 (chart layout) → Task 3. §2 (registry-agnostic images) → Task 3 Step 2's `values.yaml` image fields + README's `kind load docker-image` guidance. §3 (ClusterIP-only) → Task 3 Step 11's verification. §4 (BYO-only secrets) → Task 3 Step 12's verification + README's `kubectl create secret` instructions; the mTLS-SAN gotcha discovered during verification is documented prominently in the README (Task 3 Step 8), not buried only in a values.yaml comment. §5 (sampleapp gated off by default) → Task 3 Step 13's verification. §6 (health checks: grpc for core on its own port, httpGet for sidecar) → Tasks 1-2 (code) and Task 3 Steps 5-6 (chart wiring). §7 (real kind-cluster smoke test, pods Ready + full tier-regression traffic) → Task 4, executed both without and with mTLS.

**Placeholder scan:** No "TBD"/"TODO". Every code block is the actual, already-verified-working content — the health service diff, the `/healthz` handler, all 7 chart files, and the README are all exactly what was built and proven against a real `kind` cluster during planning, not draft text. Every command in Task 4 has the exact real observed output captured during verification, including the specific pod-Ready lines, log lines, and curl status-code sequences.

**Type/reference consistency:** `RATECAP_HEALTH_ADDR`/`:9091` (Task 1) is the exact env var and default Task 3's `core.yaml` template and `values.yaml`'s `core.healthPort` field reference. `/healthz` (Task 2) is the exact path Task 3's `sidecar.yaml` template's `readinessProbe`/`livenessProbe` reference. `healthzHandler` (Task 2's extracted function name) matches between the implementation and its test. The mTLS-SAN gotcha's resolution (cert SAN = `<release-name>-core`/`<release-name>-sidecar`) is stated identically in Task 3 Step 8's README and Task 4 Step 11's cert-generation commands.
