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

// Load builds a client-side, mutual-TLS *tls.Config: it presents this
// service's own client certificate (so the server can authenticate it)
// and verifies the server's certificate against the given CA. The
// returned stop func ends the cert/key hot-reload watcher; the CA pool
// itself is loaded once and is not hot-reloaded.
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
