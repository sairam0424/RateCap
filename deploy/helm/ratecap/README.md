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
