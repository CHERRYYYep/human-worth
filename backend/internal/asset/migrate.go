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
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM asset.schema_migrations WHERE version='003_multipart.sql')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("asset schema not ready")
	}
	return nil
}
