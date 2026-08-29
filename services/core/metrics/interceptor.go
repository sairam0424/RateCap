package metrics

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// methodName strips the "/ratecap.v1.RatecapService/" prefix from
// info.FullMethod so the metric's method label matches the RPC name callers
// actually recognize (CheckRateLimit, ReleaseConcurrency), not the full
// gRPC wire path.
func methodName(fullMethod string) string {
	if idx := strings.LastIndex(fullMethod, "/"); idx != -1 {
		return fullMethod[idx+1:]
	}
	return fullMethod
}

// classifyTransport inspects the real peer connection info gRPC attaches to
// the context — not which listener/port a call arrived on — so the metric
// reflects actual transport security regardless of how many listeners are
// configured. peer.FromContext returning !ok happens routinely in unit
// tests and for calls with no transport-level peer info attached.
func classifyTransport(ctx context.Context) (transport, clientCert string) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "plaintext", "n/a"
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "plaintext", "n/a"
	}
	if len(tlsInfo.State.PeerCertificates) > 0 {
		return "tls", "present"
	}
	return "tls", "absent"
}

func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		RecordGRPCRequest(methodName(info.FullMethod), status.Code(err).String(), time.Since(start))
		transport, clientCert := classifyTransport(ctx)
		RecordConnectionSecurity(transport, clientCert)
		return resp, err
	}
}
