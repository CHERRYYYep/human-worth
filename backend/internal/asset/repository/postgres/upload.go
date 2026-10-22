package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/KDZZZZZZ/human-worth/backend/internal/asset/application"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UploadRepository struct{ DB *pgxpool.Pool }

const assetSelect = `SELECT a.id,a.owner_id,a.run_id,a.attempt_id,a.upload_id,a.state,a.original_filename,a.declared_media_type,a.detected_media_type,a.expected_bytes,a.received_bytes,a.expected_sha256,a.actual_sha256,a.staging_key,a.final_key,a.created_at,a.completed_at`

func scanAsset(row interface{ Scan(...any) error }) (application.Asset, error) {
	var result application.Asset
	var detected *string
	err := row.Scan(&result.ID, &result.Owner, &result.RunID, &result.AttemptID, &result.UploadID, &result.State, &result.Filename, &result.Declared, &detected, &result.Expected, &result.Received, &result.ExpectedHash, &result.ActualHash, &result.StagingKey, &result.FinalKey, &result.Created, &result.Completed)
	if detected != nil {
		result.Detected = *detected
	}
	return result, err
}

func (r UploadRepository) WithChallengeLease(ctx context.Context, key string, run func(application.ChallengeUploadLease) error) error {
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,7185429004))`, key).Scan(&locked); err != nil {
		conn.Conn().Close(ctx)
		return unavailable(err)
	}
	if !locked {
		return application.NewError("aborted", "upload_in_progress")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var unlocked bool
		if unlockErr := conn.QueryRow(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,7185429004))`, key).Scan(&unlocked); unlockErr != nil || !unlocked {
			conn.Conn().Close(cleanup)
		}
	}()
	return run(challengeUploadLease{conn: conn})
}

type challengeUploadLease struct{ conn *pgxpool.Conn }

func (l challengeUploadLease) LoadOrCreate(ctx context.Context, input application.ChallengeUpload) (application.Asset, error) {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return application.Asset{}, unavailable(err)
	}
	defer rollback(ctx, tx)
	row, err := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets a WHERE run_id=$1 AND attempt_id=$2 AND upload_id=$3 FOR UPDATE`, input.RunID, input.AttemptID, input.UploadID))
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return application.Asset{}, unavailable(err)
		}
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return application.Asset{}, unavailable(err)
	}
	assetID, err := newAssetID()
	if err != nil {
		return application.Asset{}, application.NewError("internal", "internal_error")
	}
	row = application.Asset{ID: assetID, Owner: "challenge", RunID: input.RunID, AttemptID: input.AttemptID, UploadID: input.UploadID, State: "staging", Filename: input.Filename, Declared: input.Declared, Expected: input.Expected, ExpectedHash: input.ExpectedHash, StagingKey: "assets/" + assetID + "/challenge/" + input.UploadID, FinalKey: "assets/" + assetID + "/content"}
	_, err = tx.Exec(ctx, `INSERT INTO asset.assets(id,owner_id,run_id,attempt_id,upload_id,state,original_filename,declared_media_type,expected_bytes,expected_sha256,staging_key,final_key) VALUES($1,'challenge',$2,$3,$4,'staging',$5,$6,$7,$8,$9,$10)`, row.ID, row.RunID, row.AttemptID, row.UploadID, row.Filename, row.Declared, row.Expected, row.ExpectedHash, row.StagingKey, row.FinalKey)
	if err != nil {
		return application.Asset{}, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Asset{}, unavailable(err)
	}
	return row, nil
}

func (l challengeUploadLease) SetRetryStaging(ctx context.Context, assetID string) error {
	_, err := l.conn.Exec(ctx, `UPDATE asset.assets SET state='staging',storage_cleaned_at=NULL,received_bytes=0,actual_sha256=NULL,completed_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND state='failed'`, assetID)
	return errApp(err)
}

func (l challengeUploadLease) ResetMultipart(ctx context.Context, assetID, key string, deleteObject func(context.Context, string) error) error {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(ctx, tx)
	var lockedID string
	if err = tx.QueryRow(ctx, `SELECT id FROM asset.assets WHERE id=$1 FOR UPDATE`, assetID).Scan(&lockedID); err != nil {
		return unavailable(err)
	}
	if err = deleteObject(ctx, key); err != nil {
		return unavailable(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.assets SET storage_upload_id='',storage_parts='[]',received_bytes=0,storage_cleaned_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, assetID); err != nil {
		return unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

func (l challengeUploadLease) WithLockedAsset(ctx context.Context, assetID string, run func(application.LockedChallengeAsset) error) error {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(ctx, tx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM asset.assets WHERE id=$1 FOR UPDATE`, assetID).Scan(&id); err != nil {
		return unavailable(err)
	}
	if err = run(lockedChallengeAsset{ctx: ctx, tx: tx, assetID: assetID}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

type lockedChallengeAsset struct {
	ctx     context.Context
	tx      pgx.Tx
	assetID string
}

func (l lockedChallengeAsset) MultipartState() (string, []application.Part, error) {
	var id string
	var parts []application.Part
	if err := l.tx.QueryRow(l.ctx, `SELECT storage_upload_id,storage_parts FROM asset.assets WHERE id=$1`, l.assetID).Scan(&id, &parts); err != nil {
		return "", nil, unavailable(err)
	}
	return id, parts, nil
}
func (l lockedChallengeAsset) SaveMultipartState(id string, parts []application.Part) error {
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET storage_upload_id=$2,storage_parts=$3,storage_cleaned_at=NULL WHERE id=$1`, l.assetID, id, parts)
	return errApp(err)
}
func (l lockedChallengeAsset) SetReceived(received int64) error {
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, l.assetID, received)
	return errApp(err)
}

func (l challengeUploadLease) MarkFailed(ctx context.Context, assetID string) error {
	_, err := l.conn.Exec(ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1 AND state='staging'`, assetID)
	return errApp(err)
}

func (l challengeUploadLease) MarkReady(ctx context.Context, assetID, detected string, size int64, actual []byte, recover bool) (application.Asset, error) {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return application.Asset{}, unavailable(err)
	}
	defer rollback(ctx, tx)
	completion := `clock_timestamp()`
	if recover {
		completion = `COALESCE(completed_at,clock_timestamp())`
	}
	query := `UPDATE asset.assets SET state='ready',detected_media_type=$2,received_bytes=$3,actual_sha256=$4,completed_at=` + completion + `,updated_at=clock_timestamp() WHERE id=$1`
	if _, err = tx.Exec(ctx, query, assetID, detected, size, actual); err != nil {
		return application.Asset{}, unavailable(err)
	}
	row, err := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets a WHERE id=$1`, assetID))
	if err != nil {
		return application.Asset{}, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Asset{}, unavailable(err)
	}
	return row, nil
}

func newAssetID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ast_" + hex.EncodeToString(raw[:]), nil
}

func (r UploadRepository) CreateUpload(ctx context.Context, asset application.Asset, session application.Session, fingerprint []byte) (application.Asset, application.Session, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return application.Asset{}, application.Session{}, unavailable(err)
	}
	defer rollback(ctx, tx)
	var existingID, assetID, state string
	var existingFingerprint []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT id,asset_id,state,request_fingerprint,expires_at FROM asset.upload_sessions WHERE owner_id=$1 AND idempotency_key=$2`, session.Owner, session.IdempotencyKey).Scan(&existingID, &assetID, &state, &existingFingerprint, &expires)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return application.Asset{}, application.Session{}, unavailable(err)
	}
	if err == nil {
		if !sameFingerprint(existingFingerprint, fingerprint) {
			return application.Asset{}, application.Session{}, application.NewError("already_exists", "idempotency_key_conflict")
		}
		row, scanErr := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets a WHERE id=$1`, assetID))
		if scanErr != nil {
			return application.Asset{}, application.Session{}, unavailable(scanErr)
		}
		if err = tx.Commit(ctx); err != nil {
			return application.Asset{}, application.Session{}, unavailable(err)
		}
		return row, application.Session{ID: existingID, AssetID: assetID, Owner: session.Owner, State: state, Received: row.Received, Expected: row.Expected, ExpiresAt: expires}, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO asset.assets(id,owner_id,state,original_filename,declared_media_type,expected_bytes,expected_sha256,staging_key,final_key) VALUES($1,$2,'staging',$3,$4,$5,$6,$7,$8)`, asset.ID, asset.Owner, asset.Filename, asset.Declared, asset.Expected, asset.ExpectedHash, asset.StagingKey, asset.FinalKey)
	if err != nil {
		return application.Asset{}, application.Session{}, unavailable(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO asset.upload_sessions(id,asset_id,owner_id,idempotency_key,request_fingerprint,state,expires_at) VALUES($1,$2,$3,$4,$5,'active',$6)`, session.ID, session.AssetID, session.Owner, session.IdempotencyKey, fingerprint, session.ExpiresAt)
	if err != nil {
		return application.Asset{}, application.Session{}, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Asset{}, application.Session{}, unavailable(err)
	}
	return asset, session, nil
}

func (r UploadRepository) GetUploadSession(ctx context.Context, owner, sessionID string) (application.Session, error) {
	var result application.Session
	err := r.DB.QueryRow(ctx, `SELECT s.id,s.asset_id,s.owner_id,s.state,s.received_bytes,a.expected_bytes,s.expires_at FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 AND s.owner_id=$2`, sessionID, owner).Scan(&result.ID, &result.AssetID, &result.Owner, &result.State, &result.Received, &result.Expected, &result.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Session{}, application.NewError("not_found", "upload_session_not_found")
	}
	if err != nil {
		return application.Session{}, unavailable(err)
	}
	return result, nil
}

func (r UploadRepository) ExpireUploadSessions(ctx context.Context) error {
	_, err := r.DB.Exec(ctx, `UPDATE asset.upload_sessions SET state='expired',updated_at=clock_timestamp() WHERE state='active' AND expires_at<=clock_timestamp()`)
	return errApp(err)
}

func (r UploadRepository) CleanupNextUpload(ctx context.Context, deleteObjects func(context.Context, string, string) error) (bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	defer rollback(ctx, tx)
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
		return false, unavailable(err)
	}
	if err = deleteObjects(ctx, staging, final); err != nil {
		return false, unavailable(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1`, assetID); err != nil {
		return false, unavailable(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.upload_sessions SET storage_cleaned_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, session); err != nil {
		return false, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, unavailable(err)
	}
	return true, nil
}

func (r UploadRepository) CleanupStaleChallenge(ctx context.Context, deleteObjects func(context.Context, string, string) error) (bool, error) {
	rows, err := r.DB.Query(ctx, `SELECT id FROM asset.assets WHERE run_id<>'' AND state IN ('staging','failed') AND storage_cleaned_at IS NULL AND updated_at < clock_timestamp()-interval '24 hours' ORDER BY updated_at,id LIMIT 100`)
	if err != nil {
		return false, unavailable(err)
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, unavailable(err)
		}
		candidates = append(candidates, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, unavailable(err)
	}
	for _, candidate := range candidates {
		done, err := r.cleanupStaleChallengeCandidate(ctx, candidate, deleteObjects)
		if err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
	return false, nil
}

func (r UploadRepository) cleanupStaleChallengeCandidate(ctx context.Context, candidate string, deleteObjects func(context.Context, string, string) error) (bool, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	defer rollback(ctx, tx)
	var assetID, staging, final, run, attempt, upload string
	err = tx.QueryRow(ctx, `SELECT id,staging_key,final_key,run_id,attempt_id,upload_id FROM asset.assets
		WHERE id=$1 AND run_id<>'' AND state IN ('staging','failed') AND storage_cleaned_at IS NULL
		AND updated_at < clock_timestamp()-interval '24 hours'
		FOR UPDATE SKIP LOCKED`, candidate).Scan(&assetID, &staging, &final, &run, &attempt, &upload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, unavailable(err)
	}
	lockKey, _ := json.Marshal([]string{run, attempt, upload})
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,7185429004))`, string(lockKey)).Scan(&locked); err != nil {
		return false, unavailable(err)
	}
	if !locked {
		return false, nil
	}
	if err = deleteObjects(ctx, staging, final); err != nil {
		return false, unavailable(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',storage_cleaned_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, assetID); err != nil {
		return false, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, unavailable(err)
	}
	return true, nil
}

func (r UploadRepository) WithLockedCompletion(ctx context.Context, sessionID string, run func(application.LockedCompletion) error) error {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(ctx, tx)
	locked := &lockedCompletion{ctx: ctx, tx: tx}
	err = tx.QueryRow(ctx, `SELECT s.id,s.owner_id,s.state,s.expires_at,s.asset_id,a.state,a.staging_key,a.final_key,a.declared_media_type,a.expected_bytes,a.received_bytes,a.expected_sha256 FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, sessionID).Scan(&locked.state.SessionID, &locked.state.Owner, &locked.state.SessionState, &locked.state.ExpiresAt, &locked.state.AssetID, &locked.state.AssetState, &locked.state.StagingKey, &locked.state.FinalKey, &locked.state.Declared, &locked.state.Expected, &locked.state.Received, &locked.state.ExpectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.NewError("not_found", "upload_session_not_found")
	}
	if err != nil {
		return unavailable(err)
	}
	if err = run(locked); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

type lockedCompletion struct {
	ctx   context.Context
	tx    pgx.Tx
	state application.CompletionState
}

func (l *lockedCompletion) State() application.CompletionState { return l.state }
func (l *lockedCompletion) Asset() (application.Asset, error) {
	row, err := scanAsset(l.tx.QueryRow(l.ctx, assetSelect+` FROM asset.assets a WHERE id=$1`, l.state.AssetID))
	if err != nil {
		return application.Asset{}, unavailable(err)
	}
	return row, nil
}
func (l *lockedCompletion) MultipartState() (string, []application.Part, error) {
	var id string
	var parts []application.Part
	if err := l.tx.QueryRow(l.ctx, `SELECT storage_upload_id,storage_parts FROM asset.assets WHERE id=$1 FOR UPDATE`, l.state.AssetID).Scan(&id, &parts); err != nil {
		return "", nil, unavailable(err)
	}
	return id, parts, nil
}
func (l *lockedCompletion) MarkExpired() error {
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.upload_sessions SET state='expired',updated_at=clock_timestamp() WHERE id=$1`, l.state.SessionID)
	return errApp(err)
}
func (l *lockedCompletion) MarkRejected(detected string) error {
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET state='failed',detected_media_type=$2,updated_at=clock_timestamp() WHERE id=$1`, l.state.AssetID, detected); err != nil {
		return errApp(err)
	}
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, l.state.SessionID)
	return errApp(err)
}
func (l *lockedCompletion) MarkReady(detected string, actual []byte) error {
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET state='ready',detected_media_type=$2,actual_sha256=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, l.state.AssetID, detected, actual); err != nil {
		return errApp(err)
	}
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.upload_sessions SET state='completed',updated_at=clock_timestamp() WHERE id=$1`, l.state.SessionID)
	return errApp(err)
}
func (l *lockedCompletion) MarkAborted() error {
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, l.state.SessionID); err != nil {
		return errApp(err)
	}
	_, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1`, l.state.AssetID)
	return errApp(err)
}

func errApp(err error) error {
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func sameFingerprint(a, b []byte) bool { return len(a) == len(b) && string(a) == string(b) }

func (r UploadRepository) WithLockedBrowserUpload(ctx context.Context, session string, run func(application.LockedBrowserUpload) error) error {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(ctx, tx)
	locked := &lockedBrowserUpload{ctx: ctx, tx: tx, sessionID: session}
	err = tx.QueryRow(ctx, `SELECT s.asset_id,s.owner_id,s.state,s.expires_at,a.staging_key,a.expected_bytes,s.received_bytes FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, session).Scan(&locked.upload.AssetID, &locked.upload.Owner, &locked.upload.State, &locked.upload.ExpiresAt, &locked.upload.StagingKey, &locked.upload.Expected, &locked.upload.Received)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.NewError("not_found", "upload_session_not_found")
	}
	if err != nil {
		return unavailable(err)
	}
	if err = run(locked); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

type lockedBrowserUpload struct {
	ctx       context.Context
	tx        pgx.Tx
	sessionID string
	upload    application.BrowserUploadState
}

func (l *lockedBrowserUpload) Upload() application.BrowserUploadState { return l.upload }

func (l *lockedBrowserUpload) MultipartState() (string, []application.Part, error) {
	var id string
	var parts []application.Part
	if err := l.tx.QueryRow(l.ctx, `SELECT storage_upload_id,storage_parts FROM asset.assets WHERE id=$1 FOR UPDATE`, l.upload.AssetID).Scan(&id, &parts); err != nil {
		return "", nil, unavailable(err)
	}
	return id, parts, nil
}

func (l *lockedBrowserUpload) SaveMultipartState(id string, parts []application.Part) error {
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET storage_upload_id=$2,storage_parts=$3,storage_cleaned_at=NULL WHERE id=$1`, l.upload.AssetID, id, parts); err != nil {
		return unavailable(err)
	}
	return nil
}

func (l *lockedBrowserUpload) ConfirmReceived(next int64) error {
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.assets SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, l.upload.AssetID, next); err != nil {
		return unavailable(err)
	}
	if _, err := l.tx.Exec(l.ctx, `UPDATE asset.upload_sessions SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1 AND state='active'`, l.sessionID, next); err != nil {
		return unavailable(err)
	}
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		// A failed rollback causes pgx to discard the connection.
		slog.ErrorContext(ctx, "asset_transaction_rollback_failed", "error", err)
	}
}

func unavailable(err error) error {
	if errors.Is(err, context.Canceled) {
		return application.NewError("canceled", "request_cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return application.NewError("deadline_exceeded", "deadline_exceeded")
	}
	return application.NewError("unavailable", "asset_unavailable")
}
