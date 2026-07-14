package auth

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const MetadataKey = "x-ratecap-shared-secret"

func UnaryServerInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md.Get(MetadataKey)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "missing shared secret")
		}
		if subtle.ConstantTimeCompare([]byte(md.Get(MetadataKey)[0]), []byte(secret)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid shared secret")
		}
		return handler(ctx, req)
	}
}
