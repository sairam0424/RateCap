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
// and verifies the server's certificate against the given CA.
func Load(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
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
		RootCAs:      pool,
	}, nil
}
