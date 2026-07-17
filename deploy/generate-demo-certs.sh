#!/usr/bin/env bash
set -euo pipefail

# Demo-only certs for RateCap's docker-compose stack. Do not use in
# production — see SECURITY.md for the real deployment guidance
# (operator-provided certs via RATECAP_TLS_CERT_PATH/KEY_PATH/CA_PATH).

cd "$(dirname "$0")"
mkdir -p certs
cd certs

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca-key.pem -out ca.pem -days 1 -nodes \
  -subj "/CN=ratecap-demo-ca"

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout core-key.pem -out core.csr -nodes \
  -subj "/CN=ratecap-core" -addext "subjectAltName=DNS:core"
openssl x509 -req -in core.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out core-cert.pem -days 1 -copy_extensions copy

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout sidecar-key.pem -out sidecar.csr -nodes \
  -subj "/CN=ratecap-sidecar" -addext "subjectAltName=DNS:sidecar"
openssl x509 -req -in sidecar.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out sidecar-cert.pem -days 1 -copy_extensions copy

rm -f core.csr sidecar.csr ca.srl
echo "Demo certs generated in deploy/certs/ (gitignored, 1-day validity, do not use in production)."
