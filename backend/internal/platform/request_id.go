package platform

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const requestIDMetadataKey = "x-request-id"

// ValidRequestID matches IDs generated at the HTTP edge.
func ValidRequestID(value string) bool {
	if len(value) != 22 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

// RequestIDFromIncomingContext returns only a single valid edge request ID.
func RequestIDFromIncomingContext(ctx context.Context) string {
	values := metadata.ValueFromIncomingContext(ctx, requestIDMetadataKey)
	if len(values) == 1 && ValidRequestID(values[0]) {
		return values[0]
	}
	return ""
}

func requestIDForCall(ctx context.Context) string {
	if values, ok := metadata.FromOutgoingContext(ctx); ok {
		ids := values.Get(requestIDMetadataKey)
		if len(ids) == 1 && ValidRequestID(ids[0]) {
			return ids[0]
		}
	}
	return RequestIDFromIncomingContext(ctx)
}

func withRequestID(ctx context.Context) context.Context {
	id := requestIDForCall(ctx)
	metadataOut, _ := metadata.FromOutgoingContext(ctx)
	metadataOut = metadataOut.Copy()
	metadataOut.Delete(requestIDMetadataKey)
	if id != "" {
		metadataOut.Set(requestIDMetadataKey, id)
	}
	return metadata.NewOutgoingContext(ctx, metadataOut)
}

// RequestIDUnaryClientInterceptor propagates the request ID across unary RPCs.
func RequestIDUnaryClientInterceptor(ctx context.Context, method string, request, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, options ...grpc.CallOption) error {
	return invoker(withRequestID(ctx), method, request, reply, cc, options...)
}

// RequestIDStreamClientInterceptor propagates the request ID across streaming RPCs.
func RequestIDStreamClientInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, options ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(withRequestID(ctx), desc, cc, method, options...)
}

// WithRequestIDPropagation adds the shared propagation interceptors to client options.
func WithRequestIDPropagation(options ...grpc.DialOption) []grpc.DialOption {
	options = append([]grpc.DialOption(nil), options...)
	return append(options,
		grpc.WithChainUnaryInterceptor(RequestIDUnaryClientInterceptor),
		grpc.WithChainStreamInterceptor(RequestIDStreamClientInterceptor),
	)
}
