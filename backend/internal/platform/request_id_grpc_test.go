package platform_test

import (
	"context"
	"net"
	"testing"
	"time"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type identityRequestIDProbe struct {
	identitypb.UnimplementedIdentityServiceServer
	seen chan []string
}

func (s *identityRequestIDProbe) VerifyActor(ctx context.Context, _ *identitypb.VerifyActorRequest) (*identitypb.VerifyActorResponse, error) {
	s.seen <- metadata.ValueFromIncomingContext(ctx, "x-request-id")
	return &identitypb.VerifyActorResponse{}, nil
}

type assetRequestIDProbe struct {
	assetpb.UnimplementedAssetServiceServer
	seen chan []string
}

func (s *assetRequestIDProbe) UploadAsset(stream grpc.ClientStreamingServer[assetpb.UploadAssetRequest, assetpb.UploadAssetResponse]) error {
	s.seen <- metadata.ValueFromIncomingContext(stream.Context(), "x-request-id")
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	return stream.SendAndClose(&assetpb.UploadAssetResponse{})
}

func TestGRPCClientPropagatesRequestIDForUnaryAndStreaming(t *testing.T) {
	const edgeID = "Abcdefghijklmnop-01234"
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	identitySeen, assetSeen := make(chan []string, 1), make(chan []string, 1)
	identitypb.RegisterIdentityServiceServer(server, &identityRequestIDProbe{seen: identitySeen})
	assetpb.RegisterAssetServiceServer(server, &assetRequestIDProbe{seen: assetSeen})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := platform.GRPCClient("passthrough:///request-id-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-request-id", edgeID))
	if _, err = identitypb.NewIdentityServiceClient(conn).VerifyActor(ctx, &identitypb.VerifyActorRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := <-identitySeen; len(got) != 1 || got[0] != edgeID {
		t.Fatalf("unary server received request IDs %v, want [%s]", got, edgeID)
	}

	stream, err := assetpb.NewAssetServiceClient(conn).UploadAsset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&assetpb.UploadAssetRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-assetSeen:
		if len(got) != 1 || got[0] != edgeID {
			t.Fatalf("stream server received request IDs %v, want [%s]", got, edgeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream server did not receive the upload")
	}
}
