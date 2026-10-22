package platform

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestRequestIDPropagationInterceptors(t *testing.T) {
	const edgeID = "Abcdefghijklmnop-01234"
	const otherID = "Zbcdefghijklmnop-01234"
	if !ValidRequestID(edgeID) || !ValidRequestID(otherID) {
		t.Fatal("test IDs must match edge format")
	}

	tests := []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{name: "propagates one incoming ID", ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", edgeID)), want: []string{edgeID}},
		{name: "outgoing edge ID takes precedence", ctx: metadata.NewOutgoingContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", otherID)), metadata.Pairs("x-request-id", edgeID, "authorization", "kept")), want: []string{edgeID}},
		{name: "normalizes duplicate outgoing ID", ctx: metadata.NewOutgoingContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", edgeID)), metadata.Pairs("x-request-id", otherID, "x-request-id", otherID)), want: []string{edgeID}},
		{name: "drops invalid incoming ID", ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", "client-controlled")), want: nil},
		{name: "drops multiple incoming IDs", ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", edgeID, "x-request-id", otherID)), want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := func(ctx context.Context) {
				t.Helper()
				got, _ := metadata.FromOutgoingContext(ctx)
				ids := got.Get("x-request-id")
				if len(ids) != len(test.want) {
					t.Fatalf("outgoing request IDs = %v, want %v", ids, test.want)
				}
				for i := range ids {
					if ids[i] != test.want[i] {
						t.Fatalf("outgoing request IDs = %v, want %v", ids, test.want)
					}
				}
			}

			if err := RequestIDUnaryClientInterceptor(test.ctx, "/test.Service/Unary", nil, nil, nil, func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
				check(ctx)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err := RequestIDStreamClientInterceptor(test.ctx, &grpc.StreamDesc{}, nil, "/test.Service/Stream", func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
				check(ctx)
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestIDFromIncomingContextValidation(t *testing.T) {
	const validID = "Abcdefghijklmnop-01234"
	for _, test := range []struct {
		name string
		md   metadata.MD
		want string
	}{
		{name: "valid", md: metadata.Pairs("x-request-id", validID), want: validID},
		{name: "missing", md: metadata.MD{}},
		{name: "invalid", md: metadata.Pairs("x-request-id", "not-an-edge-id")},
		{name: "multiple", md: metadata.Pairs("x-request-id", validID, "x-request-id", validID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), test.md)
			if got := RequestIDFromIncomingContext(ctx); got != test.want {
				t.Fatalf("RequestIDFromIncomingContext() = %q, want %q", got, test.want)
			}
		})
	}
}
