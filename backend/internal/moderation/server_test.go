package moderation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"strings"
	"testing"
	"time"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDecisionAndCursorBoundaries(t *testing.T) {
	reason := "必须补充来源"
	empty := " \n"
	huge := strings.Repeat("x", 4001)
	for _, value := range []string{"bad\x00reason", string([]byte{0xff}), strings.Repeat("绘", 1334)} {
		for _, outcome := range []contentpb.ReviewOutcome{contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT} {
			if _, _, err := decisionInput(outcome, &value); err == nil {
				t.Fatal("invalid decision reason accepted")
			}
		}
	}
	boundary := strings.Repeat("x", 4000)
	if _, saved, err := decisionInput(contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, &boundary); err != nil || saved != &boundary {
		t.Fatal("4000-byte approval reason rejected or changed", err)
	}
	for _, v := range []struct {
		o  contentpb.ReviewOutcome
		r  *string
		ok bool
	}{{contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, nil, true}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT, &reason, true}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT, nil, false}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT, &empty, false}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT, &huge, false}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, &reason, true}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, &huge, false}, {contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, &empty, true}, {0, nil, false}} {
		_, saved, err := decisionInput(v.o, v.r)
		if (err == nil) != v.ok {
			t.Fatalf("decision=%v err=%v", v.o, err)
		}
		if v.ok && saved != v.r {
			t.Fatal("decision reason was discarded or changed")
		}
	}
	r := &pb.Review{Id: "rev_" + strings.Repeat("a", 32), CreatedAt: timestamppb.New(time.Date(2026, 10, 16, 1, 2, 3, 123456000, time.UTC))}
	raw := encodeCursor("admin-a", "pending", r)
	created, id, err := decodeCursor(raw, "admin-a", "pending")
	if err != nil || id != r.Id || !created.Equal(r.CreatedAt.AsTime()) {
		t.Fatal("cursor lost precision", err)
	}
	for _, v := range []struct{ raw, admin, state string }{{raw, "admin-b", "pending"}, {raw, "admin-a", "approved"}, {"broken", "admin-a", "pending"}, {strings.Repeat("x", 2049), "admin-a", "pending"}} {
		if _, _, err = decodeCursor(v.raw, v.admin, v.state); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
}
func TestModerationServiceIdentity(t *testing.T) {
	for _, caller := range []string{"gateway", "content", "asset", "moderation", "challenge"} {
		u, _ := url.Parse("spiffe://human-worth/services/" + caller)
		cert := &x509.Certificate{URIs: []*url.URL{u}}
		ctx := grpcpeer.NewContext(context.Background(), &grpcpeer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
		for _, method := range []string{pb.ModerationService_CreateReviewCase_FullMethodName, pb.ModerationService_DecideReview_FullMethodName} {
			expected := caller == "gateway"
			if method == pb.ModerationService_CreateReviewCase_FullMethodName {
				expected = caller == "content"
			}
			called := false
			_, err := Authorization(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called != expected || (err == nil) != expected {
				t.Fatalf("%s %s err=%v", caller, method, err)
			}
		}
	}
}
