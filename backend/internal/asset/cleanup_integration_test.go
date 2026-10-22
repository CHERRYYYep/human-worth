//go:build integration

package asset

import (
	"context"

	assetapp "github.com/KDZZZZZZ/human-worth/backend/internal/asset/application"
	assetpostgres "github.com/KDZZZZZZ/human-worth/backend/internal/asset/repository/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func cleanupExpiredForTest(ctx context.Context, db *pgxpool.Pool, storage Storage) error {
	return (assetapp.CleanupUseCase{
		Repository: assetpostgres.UploadRepository{DB: db},
		Storage:    storage,
	}).Execute(ctx)
}
