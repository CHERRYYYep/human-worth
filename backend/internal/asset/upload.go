package asset

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
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
func withinUploadTx(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, run func(pgx.Tx) error) error {
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
