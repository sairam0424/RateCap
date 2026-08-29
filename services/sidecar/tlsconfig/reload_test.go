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
