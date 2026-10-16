package asset

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const publicAccessTimeout = 2 * time.Second

var publicTaskPattern = regexp.MustCompile(`^tsk_[a-f0-9]{32}$`)
var publicEntryPattern = regexp.MustCompile(`^ent_[a-f0-9]{32}$`)

// No actor/owner bypass and no cached grant. These methods are deliberately
// separate from private management and Challenge's run/attempt authorization.
func (s *Server) publicAsset(ctx context.Context, ref *pb.PublicAssetReference) (assetRow, error) {
	if err := assetPeer(ctx, pb.AssetService_DownloadPublicAsset_FullMethodName); err != nil {
		return assetRow{}, err
	}
	if !assetPattern.MatchString(ref.GetAssetId()) || !publicTaskPattern.MatchString(ref.GetTaskId()) || !publicEntryPattern.MatchString(ref.GetEntryId()) {
		return assetRow{}, status.Error(codes.NotFound, "asset_not_found")
	}
	if s.Content == nil {
		return assetRow{}, status.Error(codes.Unavailable, "content_unavailable")
	}
	// Child deadline bounds authorization even though the stream allows 15m.
	check, cancel := context.WithTimeout(ctx, publicAccessTimeout)
	defer cancel()
	access, err := s.Content.CheckAssetAccess(check, &contentpb.CheckAssetAccessRequest{Reference: ref})
	if err != nil {
		return assetRow{}, unavailable(err)
	}
	if !access.GetAllowed() {
		return assetRow{}, status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(ctx, assetSelect+` FROM asset.assets a WHERE id=$1 AND state='ready'`, ref.AssetId))
	if errors.Is(err, pgx.ErrNoRows) {
		return assetRow{}, status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return assetRow{}, unavailable(err)
	}
	return row, nil
}

func (s *Server) DownloadPublicAsset(r *pb.DownloadPublicAssetRequest, stream pb.AssetService_DownloadPublicAssetServer) error {
	row, err := s.publicAsset(stream.Context(), r.GetReference())
	if err != nil {
		return err
	}
	if err = storageReady(s); err != nil {
		return err
	}
	size, err := s.Storage.Stat(stream.Context(), row.FinalKey)
	if err != nil || size != row.Expected {
		return status.Error(codes.Unavailable, "asset_unavailable")
	}
	reader, err := s.Storage.Open(stream.Context(), row.FinalKey)
	if err != nil {
		return unavailable(err)
	}
	defer reader.Close()
	// Slow storage open must not create a stale approval window before headers.
	check, cancel := context.WithTimeout(stream.Context(), publicAccessTimeout)
	access, err := s.Content.CheckAssetAccess(check, &contentpb.CheckAssetAccessRequest{Reference: r.Reference})
	cancel()
	if err != nil {
		return unavailable(err)
	}
	if !access.GetAllowed() {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	if err = stream.Send(&pb.DownloadPublicAssetResponse{Value: &pb.DownloadPublicAssetResponse_Metadata{Metadata: assetContract(row)}}); err != nil {
		return unavailable(err)
	}
	buf := make([]byte, 64<<10)
	var offset int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if offset+int64(n) > row.Expected {
				return status.Error(codes.Unavailable, "asset_unavailable")
			}
			if err = stream.Send(&pb.DownloadPublicAssetResponse{Offset: offset, Value: &pb.DownloadPublicAssetResponse_Data{Data: append([]byte(nil), buf[:n]...)}}); err != nil {
				return unavailable(err)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			if offset != row.Expected {
				return status.Error(codes.Unavailable, "asset_unavailable")
			}
			return nil
		}
		if readErr != nil {
			return unavailable(readErr)
		}
	}
}
