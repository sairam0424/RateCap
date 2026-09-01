package metrics_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sairam0424/RateCap/services/core/metrics"
)

// The FullMethod values below are deliberately synthetic (not the real
// CheckRateLimit/ReleaseConcurrency RPC names) rather than reused from
// metrics_test.go: promauto metrics are process-global, this file's tests
// always run before metrics_test.go's (Go orders test files alphabetically,
// "interceptor_test.go" < "metrics_test.go"), and metrics_test.go asserts
// absolute (not delta) counts against those exact real-RPC label
// combinations — reusing them here would make those tests order-dependent.

func TestUnaryServerInterceptor_RecordsOKOnSuccess(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeAlpha"}
	handler := func(ctx context.Context, req any) (any, error) {
		return "response", nil
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "OK"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "OK"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=InterceptorProbeAlpha,code=OK} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsStatusCodeOnError(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeBeta"}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.PermissionDenied, "invalid concurrency token")
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeBeta", "PermissionDenied"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeBeta", "PermissionDenied"))

	if err == nil {
		t.Fatal("expected the handler's error to propagate")
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=InterceptorProbeBeta,code=PermissionDenied} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_RecordsUnknownForNonStatusError(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeAlpha"}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("plain error, not a grpc status")
	}

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "Unknown"))
	_, err := interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "Unknown"))

	if err == nil {
		t.Fatal("expected the handler's error to propagate")
	}
	if after != before+1 {
		t.Errorf("expected GRPCRequestsTotal{method=InterceptorProbeAlpha,code=Unknown} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestUnaryServerInterceptor_StripsServicePrefixFromMethod(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeAlpha"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	before := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "OK"))
	_, _ = interceptor(context.Background(), "request", info, handler)
	after := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("InterceptorProbeAlpha", "OK"))

	if after != before+1 {
		t.Error("expected the method label to be the bare RPC name (InterceptorProbeAlpha), not the full /service/method path")
	}
}

func TestUnaryServerInterceptor_RecordsPlaintextWhenNoPeerTLSInfo(t *testing.T) {
	interceptor := metrics.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeGamma"}
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
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeGamma"}
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
	info := &grpc.UnaryServerInfo{FullMethod: "/ratecap.v1.RatecapService/InterceptorProbeGamma"}
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
