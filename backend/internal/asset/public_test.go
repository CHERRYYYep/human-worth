package asset

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type publicAccessClient struct {
	contentpb.ContentServiceClient
	check func(context.Context, *contentpb.CheckAssetAccessRequest) (*contentpb.CheckAssetAccessResponse, error)
}

func (c publicAccessClient) CheckAssetAccess(ctx context.Context, r *contentpb.CheckAssetAccessRequest, _ ...grpc.CallOption) (*contentpb.CheckAssetAccessResponse, error) {
	return c.check(ctx, r)
}
func publicCaller(name string) context.Context {
	u, _ := url.Parse("spiffe://human-worth/services/" + name)
	cert := &x509.Certificate{URIs: []*url.URL{u}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}
func TestPublicAccessFailClosed(t *testing.T) {
	ref := &pb.PublicAssetReference{TaskId: "tsk_00000000000000000000000000000000", EntryId: "ent_00000000000000000000000000000000", AssetId: "ast_00000000000000000000000000000000"}
	for _, caller := range []string{"content", "challenge", "moderation"} {
		_, err := (&Server{}).publicAsset(publicCaller(caller), ref)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal(caller, err)
		}
	}
	ctx := publicCaller("gateway")
	s := &Server{}
	_, err := s.publicAsset(ctx, ref)
	if status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
	for _, remoteErr := range []error{nil, status.Error(codes.Unavailable, "private database path must not leak"), context.DeadlineExceeded} {
		called := false
		s.Content = publicAccessClient{check: func(c context.Context, r *contentpb.CheckAssetAccessRequest) (*contentpb.CheckAssetAccessResponse, error) {
			called = true
			deadline, ok := c.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Error("unbounded authorization")
			}
			if r.GetReference() != ref {
				t.Error("changed resource context")
			}
			return &contentpb.CheckAssetAccessResponse{Allowed: false}, remoteErr
		}}
		_, err = s.publicAsset(ctx, ref)
		if !called || err == nil || status.Code(err) == codes.OK || status.Convert(err).Message() == "private database path must not leak" {
			t.Fatal("failed-open or leaked dependency", err)
		}
	}
}
