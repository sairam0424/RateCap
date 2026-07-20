package grpcserver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ratecapv1 "github.com/ratecap/proto/ratecap/v1"

	"github.com/ratecap/core/grpcserver"
	"github.com/ratecap/core/limiter"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     []string{"bufnet"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("creating leaf cert for %s: %v", commonName, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling leaf key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("building tls.Certificate for %s: %v", commonName, err)
	}
	return cert
}

func startTLSTestServer(t *testing.T, ca *testCA, serverCert tls.Certificate) (net.Listener, func()) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	lis := bufconn.Listen(1024 * 1024)
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConf)))
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey))

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	return lis, grpcServer.Stop
}

func TestMTLS_RejectsClientWithNoCertificate(t *testing.T) {
	ca := newTestCA(t)
	serverCert := ca.issue(t, "ratecap-core")
	lis, stop := startTLSTestServer(t, ca, serverCert)
	defer stop()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.(*bufconn.Listener).DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool})),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	_, err = client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err == nil {
		t.Fatal("expected an error when the client presents no certificate, since the server requires one")
	}
}

func TestMTLS_AllowsClientWithValidCertificate(t *testing.T) {
	ca := newTestCA(t)
	serverCert := ca.issue(t, "ratecap-core")
	clientCert := ca.issue(t, "ratecap-sidecar")
	lis, stop := startTLSTestServer(t, ca, serverCert)
	defer stop()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.(*bufconn.Listener).DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: pool})),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	resp, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error with a valid client certificate: %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}

func TestMTLS_PlaintextPathUnaffectedWhenTLSNotConfigured(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	fl := &fakeLimiter{decision: limiter.Decision{Action: limiter.ALLOW}}
	ratecapv1.RegisterRatecapServiceServer(grpcServer, grpcserver.NewServer(limiter.NewPipeline(fl), &fakeReleaser{}, testSigningKey))
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := ratecapv1.NewRatecapServiceClient(conn)
	resp, err := client.CheckRateLimit(context.Background(), &ratecapv1.CheckRateLimitRequest{Key: "user-1", Cost: 1})
	if err != nil {
		t.Fatalf("unexpected error on the plaintext path (TLS not configured): %v", err)
	}
	if resp.Action != ratecapv1.Action_ALLOW {
		t.Errorf("expected ALLOW, got %v", resp.Action)
	}
}
