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
