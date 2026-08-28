package metrics

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
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

func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		RecordGRPCRequest(methodName(info.FullMethod), status.Code(err).String(), time.Since(start))
		return resp, err
	}
}
