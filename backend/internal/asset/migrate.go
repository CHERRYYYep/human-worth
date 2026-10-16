package asset

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7185429003)`); err != nil {
		return err
	}
	var schemaExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='asset')`).Scan(&schemaExists); err != nil {
		return err
	}
	if !schemaExists {
		if _, err = tx.Exec(ctx, `CREATE SCHEMA asset`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS asset.schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sql, readErr := migrations.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(sql)
		checksum := hex.EncodeToString(sum[:])
		var previous string
		rowErr := tx.QueryRow(ctx, `SELECT checksum FROM asset.schema_migrations WHERE version=$1`, entry.Name()).Scan(&previous)
		if rowErr == nil {
			if previous != checksum {
				return errors.New("applied migration changed")
			}
			continue
		}
		if !errors.Is(rowErr, pgx.ErrNoRows) {
			return rowErr
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO asset.schema_migrations(version,checksum) VALUES($1,$2)`, entry.Name(), checksum); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func Ready(ctx context.Context, db *pgxpool.Pool) error {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM asset.schema_migrations WHERE version='002_storage_cleanup.sql')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("asset schema not ready")
	}
	return nil
}

// CleanupExpired locks the same rows as upload/completion. Deletes are
// idempotent; a failed deletion or commit leaves a durable retry candidate.
// Never deletes a ready asset or uses a bucket-wide lifecycle rule for final.
func CleanupExpired(ctx context.Context, db *pgxpool.Pool, storage Storage) error {
	if storage == nil {
		return errors.New("storage unavailable")
	}
	if _, err := db.Exec(ctx, `UPDATE asset.upload_sessions SET state='expired',updated_at=clock_timestamp() WHERE state='active' AND expires_at<=clock_timestamp()`); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		done, err := cleanupOne(ctx, db, storage)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
	}
	return nil
}
func cleanupOne(ctx context.Context, db *pgxpool.Pool, storage Storage) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var session, assetID, staging, final string
	err = tx.QueryRow(ctx, `SELECT s.id,a.id,a.staging_key,a.final_key
        FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id
        WHERE s.state IN ('expired','aborted') AND s.storage_cleaned_at IS NULL
          AND a.state IN ('staging','failed')
        ORDER BY s.expires_at,s.id LIMIT 1 FOR UPDATE OF s,a SKIP LOCKED`).Scan(&session, &assetID, &staging, &final)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A non-ready final can exist after copy succeeded but the DB commit failed.
	if err = storage.Delete(ctx, staging); err != nil {
		return false, err
	}
	if err = storage.Delete(ctx, final); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1`, assetID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.upload_sessions SET storage_cleaned_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, session); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
