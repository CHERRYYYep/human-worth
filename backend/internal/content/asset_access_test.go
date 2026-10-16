package content

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func serviceContext(name string) context.Context {
	u, _ := url.Parse("spiffe://human-worth/services/" + name)
	cert := &x509.Certificate{URIs: []*url.URL{u}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}
func TestContentAssetServiceBoundary(t *testing.T) {
	for _, caller := range []string{"gateway", "asset", "content", "moderation", "challenge"} {
		for _, method := range []string{pb.ContentService_CheckAssetAccess_FullMethodName, pb.ContentService_ApplyAssetReview_FullMethodName, pb.ContentService_CreateTaskDraft_FullMethodName} {
			allowed := (caller == "asset" && method == pb.ContentService_CheckAssetAccess_FullMethodName) || (caller == "moderation" && method == pb.ContentService_ApplyAssetReview_FullMethodName) || (caller == "gateway" && method == pb.ContentService_CreateTaskDraft_FullMethodName)
			called := false
			_, err := Authorization(serviceContext(caller), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called != allowed || (err == nil) != allowed {
				t.Fatalf("%s %s allowed=%t error=%v", caller, method, allowed, err)
			}
		}
	}
	s := &Server{}
	if _, err := s.CheckAssetAccess(context.Background(), &pb.CheckAssetAccessRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	if _, err := s.ApplyAssetReview(serviceContext("gateway"), &pb.ApplyAssetReviewRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	if _, err := s.ApplyAssetReview(serviceContext("moderation"), &pb.ApplyAssetReviewRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	access, err := s.CheckAssetAccess(serviceContext("asset"), &pb.CheckAssetAccessRequest{})
	if err != nil || access.Allowed {
		t.Fatal("missing context was authorized", err)
	}
}
