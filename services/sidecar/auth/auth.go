package auth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const MetadataKey = "x-ratecap-shared-secret"

func UnaryClientInterceptor(secret string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, secret)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
