package moderation

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func serviceContext(name string) context.Context {
	u, _ := url.Parse("spiffe://human-worth/services/" + name)
	cert := &x509.Certificate{URIs: []*url.URL{u}}
	return grpcpeer.NewContext(context.Background(), &grpcpeer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}

func entrySnapshot(n int) *contentpb.EntryReviewSnapshot {
	stamp := timestamppb.New(time.Date(2026, 10, 23, 1, 2, 3, 123456000, time.UTC))
	return &contentpb.EntryReviewSnapshot{
		ReviewId: fmt.Sprintf("rev_%032x", n), CreatedAt: stamp,
		Submission: &contentpb.EntrySubmission{
			Id: fmt.Sprintf("ent_%032x", n), TaskId: "tsk_" + strings.Repeat("a", 32),
			AuthorId: "account-submitter", Revision: 1, Origin: "additional", State: "pending_review",
			ReviewId: fmt.Sprintf("rev_%032x", n), CreatedAt: stamp, UpdatedAt: stamp,
			Content: &contentpb.EntryInput{Side: "agent", Title: "new work", Source: "model",
				Artifacts:          []*contentpb.ArtifactInput{{Kind: "link", Url: proto.String("https://example.com/work")}},
				Permissions:        &contentpb.PermissionsInput{CloudUse: proto.Bool(false)},
				AgentConfiguration: &contentpb.AgentConfigurationInput{Model: "test", Parameters: &structpb.Struct{Fields: map[string]*structpb.Value{"z": structpb.NewNumberValue(2), "a": structpb.NewStringValue("unchanged")}}}},
		},
	}
}

func TestEntrySnapshotBoundaries(t *testing.T) {
	if err := validateEntrySnapshot(entrySnapshot(1)); err != nil {
		t.Fatal(err)
	}
	if err := validateEntrySnapshot(nil); status.Code(err) != codes.InvalidArgument {
		t.Fatal("nil snapshot accepted", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*contentpb.EntryReviewSnapshot)
	}{
		{"review", func(s *contentpb.EntryReviewSnapshot) { s.ReviewId = "bad" }},
		{"submission", func(s *contentpb.EntryReviewSnapshot) { s.Submission = nil }},
		{"task", func(s *contentpb.EntryReviewSnapshot) { s.Submission.TaskId = "bad" }},
		{"entry", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Id = "bad" }},
		{"author", func(s *contentpb.EntryReviewSnapshot) { s.Submission.AuthorId = "" }},
		{"revision", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Revision = 0 }},
		{"origin", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Origin = "initial" }},
		{"state", func(s *contentpb.EntryReviewSnapshot) { s.Submission.State = "approved" }},
		{"binding", func(s *contentpb.EntryReviewSnapshot) { s.Submission.ReviewId = "rev_" + strings.Repeat("f", 32) }},
		{"content", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Content = nil }},
		{"side", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Content.Side = "other" }},
		{"reason", func(s *contentpb.EntryReviewSnapshot) { s.Submission.RejectionReason = proto.String("old reason") }},
		{"approved", func(s *contentpb.EntryReviewSnapshot) { s.Submission.ApprovedAt = s.CreatedAt }},
		{"created", func(s *contentpb.EntryReviewSnapshot) { s.CreatedAt = nil }},
		{"entry_created", func(s *contentpb.EntryReviewSnapshot) { s.Submission.CreatedAt = nil }},
		{"entry_updated", func(s *contentpb.EntryReviewSnapshot) {
			s.Submission.UpdatedAt = &timestamppb.Timestamp{Seconds: 1 << 62}
		}},
		{"budget", func(s *contentpb.EntryReviewSnapshot) { s.Submission.Content.Title = strings.Repeat("x", 512<<10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := entrySnapshot(1)
			tc.edit(snap)
			if err := validateEntrySnapshot(snap); status.Code(err) != codes.InvalidArgument {
				t.Fatal("invalid snapshot accepted", err)
			}
		})
	}
}

// Controlled downstream responses test Moderation's contract, not Content's
// receipt implementation. Real Content/Identity/mTLS regression lives in identity.
type entryContentClient struct {
	contentpb.ContentServiceClient
	apply func(context.Context, *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error)
}

func (c entryContentClient) ApplyEntryReviewDecision(ctx context.Context, r *contentpb.ApplyEntryReviewDecisionRequest, _ ...grpc.CallOption) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
	return c.apply(ctx, r)
}

func entryReceipt(r *contentpb.ApplyEntryReviewDecisionRequest) *contentpb.ApplyEntryReviewDecisionResponse {
	state := "rejected"
	if r.Outcome == contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE {
		state = "approved"
	}
	return &contentpb.ApplyEntryReviewDecisionResponse{DecisionId: r.DecisionId, ReviewId: r.ReviewId, EntryId: r.EntryId, Revision: r.SubmissionRevision, State: state}
}

type adminIdentityClient struct {
	identitypb.IdentityServiceClient
}

func (adminIdentityClient) VerifyActor(context.Context, *identitypb.VerifyActorRequest, ...grpc.CallOption) (*identitypb.VerifyActorResponse, error) {
	return &identitypb.VerifyActorResponse{Principal: &identitypb.Principal{AccountId: "admin", Role: identitypb.Role_ROLE_ADMIN, ClientKind: identitypb.ClientKind_CLIENT_KIND_WEB_SESSION}}, nil
}

func TestEntryDecisionDigestAndReceipt(t *testing.T) {
	snap := entrySnapshot(1)
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(wire)
	for _, outcome := range []contentpb.ReviewOutcome{contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT} {
		for _, edit := range []func(*contentpb.ApplyEntryReviewDecisionResponse){
			nil,
			func(r *contentpb.ApplyEntryReviewDecisionResponse) { r.DecisionId = "wrong" },
			func(r *contentpb.ApplyEntryReviewDecisionResponse) { r.ReviewId = "wrong" },
			func(r *contentpb.ApplyEntryReviewDecisionResponse) { r.EntryId = "wrong" },
			func(r *contentpb.ApplyEntryReviewDecisionResponse) { r.Revision++ },
			func(r *contentpb.ApplyEntryReviewDecisionResponse) { r.State = "published" },
		} {
			s := &Server{Content: entryContentClient{apply: func(ctx context.Context, r *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
				if r.SnapshotSha256 != hex.EncodeToString(hash[:]) || r.EntryId != snap.Submission.Id || r.TaskId != snap.Submission.TaskId || r.SubmissionRevision != 1 || r.GetReason() != "原样理由" {
					t.Fatal("decision lost immutable binding", r)
				}
				out := entryReceipt(r)
				if edit != nil {
					edit(out)
				}
				return out, nil
			}}}
			r := &storedReview{Review: &pb.Review{Id: snap.ReviewId, EntrySnapshot: snap, DecisionId: proto.String("dec_test"), Decision: outcome, Reason: proto.String("原样理由")}, snapshotHash: hash[:]}
			_, err := s.applyDecision(t.Context(), r)
			if (edit == nil && err != nil) || (edit != nil && status.Code(err) != codes.Unavailable) {
				t.Fatal("incorrect receipt validation", err)
			}
		}
	}
}

func TestEntryDecisionRejectsMissingHashAndNilReceipt(t *testing.T) {
	snap := entrySnapshot(1)
	s := &Server{Content: entryContentClient{apply: func(context.Context, *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
		return nil, nil
	}}}
	r := &storedReview{Review: &pb.Review{Id: snap.ReviewId, EntrySnapshot: snap, DecisionId: proto.String("dec_test"), Decision: contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE}}
	if _, err := s.applyDecision(t.Context(), r); status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "invalid_review_snapshot_hash" {
		t.Fatal("missing durable hash reached Content", err)
	}
	r.snapshotHash = make([]byte, 32)
	if _, err := s.applyDecision(t.Context(), r); status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "invalid_review_receipt" {
		t.Fatal("nil receipt acknowledged or panicked", err)
	}
}
