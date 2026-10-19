package asset

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func rollbackUploadTx(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		// pgx discards the connection when rollback fails.
		slog.ErrorContext(ctx, "asset_transaction_rollback_failed", "error", err)
	}
}

// withinUploadTx never retries the callback: it may write an object under the
// existing row lock. A business rejection that needs persistence is returned
// by the caller only after this function confirms the commit.
func withinUploadTx(ctx context.Context, db *pgxpool.Pool, run func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollbackUploadTx(ctx, tx)
	if err = run(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Server) writeBrowserChunk(ctx context.Context, owner, session string, offset int64, chunk []byte) (int64, error) {
	next := offset
	err := withinUploadTx(ctx, s.DB, func(tx pgx.Tx) error {
		var assetID, state, key, sessionOwner string
		var expected, received int64
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT s.asset_id,s.owner_id,s.state,s.expires_at,a.staging_key,a.expected_bytes,s.received_bytes FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, session).Scan(&assetID, &sessionOwner, &state, &expires, &key, &expected, &received)
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "upload_session_not_found")
		}
		if err != nil {
			return unavailable(err)
		}
		if sessionOwner != owner {
			return status.Error(codes.NotFound, "upload_session_not_found")
		}
		if state != "active" || time.Now().After(expires) {
			return status.Error(codes.FailedPrecondition, "upload_session_not_active")
		}
		if received != offset {
			return status.Error(codes.Aborted, "upload_offset_conflict")
		}
		if received+int64(len(chunk)) > expected {
			return invalid("upload_too_large")
		}
		if err = s.Storage.WriteAt(ctx, key, received, chunk); err != nil {
			return unavailable(err)
		}
		next = received + int64(len(chunk))
		if _, err = tx.Exec(ctx, `UPDATE asset.assets SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, assetID, next); err != nil {
			return unavailable(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE asset.upload_sessions SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, session, next); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return offset, err
	}
	return next, nil
}

// completionObject permits final-before-commit recovery, but a storage outage
// is not evidence that the upload is incomplete.
func (s *Server) completionObject(ctx context.Context, staging, final string, expected int64) (string, error) {
	for _, key := range []string{staging, final} {
		size, err := s.Storage.Stat(ctx, key)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", unavailable(err)
		}
		if err == nil && size == expected {
			return key, nil
		}
	}
	return "", status.Error(codes.FailedPrecondition, "upload_incomplete")
}

func (s *Server) cleanupRejectedUpload(ctx context.Context, session, key string) {
	if err := s.Storage.Delete(ctx, key); err != nil {
		// The committed aborted session remains eligible for CleanupExpired,
		// which retries staging and final deletion before marking it cleaned.
		slog.WarnContext(ctx, "asset_upload_cleanup_pending", "upload_session_id", session, "error", err)
	}
}
