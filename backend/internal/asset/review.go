package asset

import (
	"context"
	"errors"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) DownloadReviewAsset(r *pb.DownloadReviewAssetRequest, stream pb.AssetService_DownloadReviewAssetServer) error {
	ctx := stream.Context()
	if err := requireAssetCaller(ctx, "gateway"); err != nil {
		return err
	}
	authorize := func(ctx context.Context) (bool, error) {
		if s.Identity == nil || s.Content == nil {
			return false, status.Error(codes.Unavailable, "review_access_unavailable")
		}
		verified, err := s.Identity.VerifyActor(ctx, &identitypb.VerifyActorRequest{ActorAssertion: r.GetActorAssertion(), FullMethod: pb.AssetService_DownloadReviewAsset_FullMethodName})
		if err != nil {
			return false, err
		}
		p := verified.GetPrincipal()
		if p.GetRole() != identitypb.Role_ROLE_ADMIN || p.GetClientKind() != identitypb.ClientKind_CLIENT_KIND_WEB_SESSION || p.GetAccountId() == "" {
			return false, status.Error(codes.PermissionDenied, "administrator_required")
		}
		access, err := s.Content.CheckReviewAssetAccess(ctx, &contentpb.CheckReviewAssetAccessRequest{ActorAssertion: r.GetContentActorAssertion(), ReviewId: r.GetReviewId(), AssetId: r.GetAssetId()})
		return access.GetAllowed(), err
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	allowed, err := authorize(check)
	cancel()
	if err != nil {
		return err
	}
	if !allowed {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(ctx, assetSelect+` FROM asset.assets a WHERE id=$1 AND state='ready'`, r.AssetId))
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return unavailable(err)
	}
	return s.streamAuthorizedAsset(ctx, row, authorize, func(frame *pb.DownloadPublicAssetResponse) error {
		out := &pb.DownloadReviewAssetResponse{Offset: frame.Offset}
		switch v := frame.Value.(type) {
		case *pb.DownloadPublicAssetResponse_Metadata:
			out.Value = &pb.DownloadReviewAssetResponse_Metadata{Metadata: v.Metadata}
		case *pb.DownloadPublicAssetResponse_Data:
			out.Value = &pb.DownloadReviewAssetResponse_Data{Data: v.Data}
		}
		return stream.Send(out)
	})
}
