package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func EnvVarsPartiallySet(cert, key, ca string) bool {
	set := 0
	if cert != "" {
		set++
	}
	if key != "" {
		set++
	}
	if ca != "" {
		set++
	}
	return set != 0 && set != 3
}

// Load builds a server-side, mutual-TLS *tls.Config: it presents this
// service's own certificate and requires+verifies the peer's certificate
// against the given CA, so an unauthenticated or wrong-cert client is
// rejected at the transport layer, on top of the existing shared-secret
// interceptor. The returned stop func ends the cert/key hot-reload watcher;
// the CA pool itself is loaded once and is NOT hot-reloaded.
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
