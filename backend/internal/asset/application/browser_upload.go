package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxAssetBytes int64 = 32 << 20
const maxFilenameBytes = 255
const assetTTL = 24 * time.Hour

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// BrowserUpload is the database state observed while the upload-session row
// lock is held by the repository.
type BrowserUploadState struct {
	AssetID, Owner, State, StagingKey string
	Expected, Received                int64
	ExpiresAt                         time.Time
}

type Part struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
}

// LockedBrowserUpload exposes only the database operations needed by the
// browser-upload use case. Implementations keep their transaction and lock
// private and commit only after the callback succeeds.
type LockedBrowserUpload interface {
	Upload() BrowserUploadState
	MultipartState() (string, []Part, error)
	SaveMultipartState(string, []Part) error
	ConfirmReceived(int64) error
}

type BrowserUploadRepository interface {
	WithLockedBrowserUpload(context.Context, string, func(LockedBrowserUpload) error) error
}

type ChunkStorage interface {
	WriteAt(context.Context, string, int64, []byte) error
	Delete(context.Context, string) error
}

type MultipartStorage interface {
	StartMultipart(context.Context, string) (string, error)
	WritePart(context.Context, string, string, int32, []byte) (string, error)
}

type BrowserUploadUseCase struct {
	Repository    BrowserUploadRepository
	Storage       ChunkStorage
	Multipart     MultipartStorage
	ChunkBytes    int64
	MaxAssetBytes int64
}

type Asset struct {
	ID, Owner, State, Filename, Declared, Detected, StagingKey, FinalKey string
	RunID, AttemptID, UploadID                                           string
	Expected, Received                                                   int64
	ExpectedHash, ActualHash                                             []byte
	Created, Completed                                                   *time.Time
}

type Session struct {
	ID, AssetID, Owner, State, IdempotencyKey string
	Received, Expected                        int64
	ExpiresAt                                 time.Time
}

type CreateSessionCommand struct {
	Owner, IdempotencyKey, Filename, Declared, ExpectedSHA256 string
	ExpectedBytes                                             int64
}

type SessionRepository interface {
	CreateUpload(context.Context, Asset, Session, []byte) (Asset, Session, error)
	GetUploadSession(context.Context, string, string) (Session, error)
}

type CompletionState struct {
	SessionID, Owner, SessionState, AssetState, StagingKey, FinalKey, Declared string
	AssetID                                                                    string
	Expected, Received                                                         int64
	ExpectedHash                                                               []byte
	ExpiresAt                                                                  time.Time
}

type LockedCompletion interface {
	State() CompletionState
	Asset() (Asset, error)
	MultipartState() (string, []Part, error)
	MarkExpired() error
	MarkRejected(string) error
	MarkReady(string, []byte) error
	MarkAborted() error
}

type CompletionRepository interface {
	WithLockedCompletion(context.Context, string, func(LockedCompletion) error) error
}

type ObjectStorage interface {
	Stat(context.Context, string) (int64, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Move(context.Context, string, string) error
	Delete(context.Context, string) error
}

type CompleteMultipartStorage interface {
	CompleteMultipart(context.Context, string, string, []Part) error
}

// CleanupRepository performs one durable cleanup transition at a time. The
// callback runs while the candidate rows/lease are held, so a failed object
// deletion leaves the candidate available for a later retry.
type CleanupRepository interface {
	ExpireUploadSessions(context.Context) error
	CleanupNextUpload(context.Context, func(context.Context, string, string) error) (bool, error)
	CleanupStaleChallenge(context.Context, func(context.Context, string, string) error) (bool, error)
}

type CleanupUseCase struct {
	Repository CleanupRepository
	Storage    interface {
		Delete(context.Context, string) error
	}
}

func (u CleanupUseCase) Execute(ctx context.Context) error {
	if u.Repository == nil || u.Storage == nil {
		return failure("unavailable", "asset_cleanup_unavailable")
	}
	if err := u.Repository.ExpireUploadSessions(ctx); err != nil {
		return err
	}
	deletePair := func(ctx context.Context, staging, final string) error {
		if err := u.Storage.Delete(ctx, staging); err != nil {
			return err
		}
		return u.Storage.Delete(ctx, final)
	}
	for i := 0; i < 100; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := u.Repository.CleanupNextUpload(ctx, deletePair)
		if err != nil {
			return err
		}
		if !done {
			break
		}
	}
	for i := 0; i < 100; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := u.Repository.CleanupStaleChallenge(ctx, deletePair)
		if err != nil {
			return err
		}
		if !done {
			break
		}
	}
	return nil
}

type CompleteUploadUseCase struct {
	Repository CompletionRepository
	Storage    ObjectStorage
}

func (u CompleteUploadUseCase) Execute(ctx context.Context, owner, sessionID, assetID string) (Asset, error) {
	if u.Repository == nil || u.Storage == nil {
		return Asset{}, failure("unavailable", "asset_upload_unavailable")
	}
	var result Asset
	var rejection error
	var cleanupKey string
	err := u.Repository.WithLockedCompletion(ctx, sessionID, func(locked LockedCompletion) error {
		state := locked.State()
		if state.Owner != owner {
			return failure("not_found", "upload_session_not_found")
		}
		if assetID != "" && state.AssetID != assetID {
			return failure("not_found", "asset_not_found")
		}
		if state.SessionState == "completed" && state.AssetState == "ready" {
			var err error
			result, err = locked.Asset()
			return err
		}
		if state.SessionState != "active" {
			return failure("failed_precondition", "upload_session_not_active")
		}
		if time.Now().After(stateExpiry(state)) {
			if err := locked.MarkExpired(); err != nil {
				return err
			}
			rejection = failure("failed_precondition", "upload_session_expired")
			return nil
		}
		if state.Received != state.Expected {
			return failure("failed_precondition", "upload_incomplete")
		}
		if multipart, ok := u.Storage.(CompleteMultipartStorage); ok {
			id, parts, err := locked.MultipartState()
			if err != nil {
				return err
			}
			if id != "" {
				if err = multipart.CompleteMultipart(ctx, state.StagingKey, id, parts); err != nil {
					return storageError(err)
				}
			}
		}
		key, err := completionObject(ctx, u.Storage, state.StagingKey, state.FinalKey, state.Expected)
		if err != nil {
			return err
		}
		actual, size, detected, err := objectDigest(ctx, u.Storage, key)
		if err != nil {
			return storageError(err)
		}
		if size != state.Expected || !sameBytes(actual, state.ExpectedHash) {
			rejection = failure("failed_precondition", "sha256_mismatch")
		} else {
			rejection = validateDetectedMediaType(state.Declared, detected)
		}
		if rejection != nil {
			if err = locked.MarkRejected(detected); err != nil {
				return err
			}
			cleanupKey = state.StagingKey
			return nil
		}
		if err = u.Storage.Move(ctx, state.StagingKey, state.FinalKey); err != nil {
			return storageError(err)
		}
		finalHash, finalSize, finalType, err := objectDigest(ctx, u.Storage, state.FinalKey)
		if err != nil {
			return storageError(err)
		}
		if finalSize != state.Expected || !sameBytes(finalHash, state.ExpectedHash) || finalType != detected {
			return failure("failed_precondition", "sha256_mismatch")
		}
		if err = locked.MarkReady(detected, actual); err != nil {
			return err
		}
		result, err = locked.Asset()
		return err
	})
	if err != nil {
		// Never remove objects after a failed or uncertain database commit.
		return Asset{}, err
	}
	if rejection != nil {
		if cleanupKey != "" {
			if err := u.Storage.Delete(ctx, cleanupKey); err != nil {
				// Durable cleanup will retry this committed rejection.
			}
		}
		return Asset{}, rejection
	}
	return result, nil
}

type AbortUploadUseCase struct {
	Repository CompletionRepository
	Storage    ObjectStorage
}

func (u AbortUploadUseCase) Execute(ctx context.Context, owner, sessionID string) (Session, error) {
	if u.Repository == nil {
		return Session{}, failure("unavailable", "asset_upload_unavailable")
	}
	var result Session
	var cleanupKey string
	err := u.Repository.WithLockedCompletion(ctx, sessionID, func(locked LockedCompletion) error {
		state := locked.State()
		if state.Owner != owner {
			return failure("not_found", "upload_session_not_found")
		}
		if state.SessionState == "completed" {
			return failure("failed_precondition", "upload_already_completed")
		}
		if err := locked.MarkAborted(); err != nil {
			return err
		}
		cleanupKey = state.StagingKey
		result = Session{ID: state.SessionID, AssetID: state.AssetID, Owner: owner, State: "aborted", Received: state.Received, Expected: state.Expected, ExpiresAt: state.ExpiresAt}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if u.Storage != nil && cleanupKey != "" {
		if err = u.Storage.Delete(ctx, cleanupKey); err != nil {
			// Cleanup is durable and retries failed deletions.
		}
	}
	return result, nil
}

func stateExpiry(state CompletionState) time.Time { return state.ExpiresAt }

func completionObject(ctx context.Context, storage ObjectStorage, staging, final string, expected int64) (string, error) {
	for _, key := range []string{staging, final} {
		size, err := storage.Stat(ctx, key)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", storageError(err)
		}
		if err == nil && size == expected {
			return key, nil
		}
	}
	return "", failure("failed_precondition", "upload_incomplete")
}

func objectDigest(ctx context.Context, storage ObjectStorage, key string) ([]byte, int64, string, error) {
	reader, err := storage.Open(ctx, key)
	if err != nil {
		return nil, 0, "", err
	}
	hash := sha256.New()
	var sniff [512]byte
	n, err := io.ReadFull(reader, sniff[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		_ = reader.Close()
		return nil, 0, "", err
	}
	_, _ = hash.Write(sniff[:n])
	rest, err := io.Copy(hash, io.LimitReader(reader, maxAssetBytes+1-int64(n)))
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		return nil, 0, "", errors.Join(err, closeErr)
	}
	detected, _, _ := mime.ParseMediaType(http.DetectContentType(sniff[:n]))
	return hash.Sum(nil), int64(n) + rest, strings.ToLower(detected), nil
}

func validateDetectedMediaType(declared, detected string) error {
	if !allowedMediaTypes[detected] {
		return failure("invalid_argument", "unsupported_media_type")
	}
	if detected != declared {
		return failure("failed_precondition", "media_type_mismatch")
	}
	return nil
}

type SessionUseCase struct{ Repository SessionRepository }

func (u SessionUseCase) Create(ctx context.Context, command CreateSessionCommand) (Asset, Session, error) {
	filename, err := cleanFilename(command.Filename)
	if err != nil {
		return Asset{}, Session{}, err
	}
	declared, err := mediaType(command.Declared)
	if err != nil {
		return Asset{}, Session{}, err
	}
	if !idempotencyKeyPattern.MatchString(command.IdempotencyKey) {
		return Asset{}, Session{}, failure("invalid_argument", "invalid_idempotency_key")
	}
	if command.ExpectedBytes < 1 || command.ExpectedBytes > maxAssetBytes {
		return Asset{}, Session{}, failure("invalid_argument", "invalid_file_size")
	}
	hash, err := hex.DecodeString(command.ExpectedSHA256)
	if err != nil || len(hash) != sha256.Size {
		return Asset{}, Session{}, failure("invalid_argument", "invalid_sha256")
	}
	assetID, err := newID("ast_")
	if err != nil {
		return Asset{}, Session{}, failure("internal", "internal_error")
	}
	sessionID, err := newID("upl_")
	if err != nil {
		return Asset{}, Session{}, failure("internal", "internal_error")
	}
	now := time.Now().UTC()
	asset := Asset{ID: assetID, Owner: command.Owner, State: "staging", Filename: filename, Declared: declared, Expected: command.ExpectedBytes, ExpectedHash: hash, StagingKey: "assets/" + assetID + "/staging/" + sessionID, FinalKey: "assets/" + assetID + "/content", Created: &now}
	session := Session{ID: sessionID, AssetID: assetID, Owner: command.Owner, State: "active", IdempotencyKey: command.IdempotencyKey, Expected: command.ExpectedBytes, ExpiresAt: now.Add(assetTTL)}
	if u.Repository == nil {
		return Asset{}, Session{}, failure("unavailable", "asset_unavailable")
	}
	fingerprint := createFingerprint(command.ExpectedBytes, command.ExpectedSHA256, filename, declared)
	return u.Repository.CreateUpload(ctx, asset, session, fingerprint)
}

func (u SessionUseCase) Get(ctx context.Context, owner, sessionID string) (Session, error) {
	if u.Repository == nil {
		return Session{}, failure("unavailable", "asset_unavailable")
	}
	return u.Repository.GetUploadSession(ctx, owner, sessionID)
}

func createFingerprint(size int64, hash, filename, declared string) []byte {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", size, hash, filename, declared)))
	return digest[:]
}

func newID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

var allowedMediaTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "application/pdf": true, "text/plain": true}

func mediaType(value string) (string, error) {
	parsed, _, err := mime.ParseMediaType(value)
	parsed = strings.ToLower(parsed)
	if err != nil || !allowedMediaTypes[parsed] {
		return "", failure("invalid_argument", "unsupported_media_type")
	}
	return parsed, nil
}

func cleanFilename(value string) (string, error) {
	if !utf8.ValidString(value) || value == "" || len(value) > maxFilenameBytes {
		return "", failure("invalid_argument", "invalid_filename")
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if i := strings.LastIndexByte(value, '/'); i >= 0 {
		value = value[i+1:]
	}
	var builder strings.Builder
	for _, r := range value {
		if r >= 0x20 && r != 0x7f {
			builder.WriteRune(r)
		}
	}
	value = strings.TrimSpace(builder.String())
	if value == "" || value == "." || value == ".." || len(value) > maxFilenameBytes {
		return "", failure("invalid_argument", "invalid_filename")
	}
	return value, nil
}

func sameBytes(a, b []byte) bool { return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1 }

type Error struct {
	Code   string
	Reason string
}

func (e *Error) Error() string { return e.Reason }

func failure(code, reason string) error { return &Error{Code: code, Reason: reason} }

func NewError(code, reason string) error { return failure(code, reason) }

func (u BrowserUploadUseCase) WriteChunk(ctx context.Context, owner, session string, offset int64, chunk []byte) (int64, error) {
	if u.Repository == nil || u.Storage == nil || u.ChunkBytes <= 0 || len(chunk) == 0 || int64(len(chunk)) > u.ChunkBytes {
		return offset, failure("invalid_argument", "invalid_upload_chunk")
	}
	if u.MaxAssetBytes <= 0 {
		return offset, errors.New("browser upload maximum must be positive")
	}
	next := offset
	err := u.Repository.WithLockedBrowserUpload(ctx, session, func(locked LockedBrowserUpload) error {
		upload := locked.Upload()
		if upload.Owner != owner {
			return failure("not_found", "upload_session_not_found")
		}
		if upload.State != "active" || time.Now().After(upload.ExpiresAt) {
			return failure("failed_precondition", "upload_session_not_active")
		}
		if upload.Received != offset {
			return failure("aborted", "upload_offset_conflict")
		}
		if offset < 0 || offset > u.MaxAssetBytes-int64(len(chunk)) || offset+int64(len(chunk)) > upload.Expected {
			return failure("invalid_argument", "upload_too_large")
		}
		if upload.Received%u.ChunkBytes != 0 {
			return failure("failed_precondition", "upload_session_restart_required")
		}
		if int64(len(chunk)) != u.ChunkBytes && offset+int64(len(chunk)) != upload.Expected {
			return failure("invalid_argument", "invalid_upload_chunk_size")
		}

		if u.Multipart == nil {
			if err := u.Storage.WriteAt(ctx, upload.StagingKey, offset, chunk); err != nil {
				return storageError(err)
			}
		} else {
			id, parts, err := locked.MultipartState()
			if err != nil {
				return err
			}
			if id == "" && offset != 0 {
				return failure("failed_precondition", "upload_session_restart_required")
			}
			if offset%u.ChunkBytes != 0 || int64(len(parts)) != offset/u.ChunkBytes {
				return errors.New("multipart manifest does not match confirmed offset")
			}
			if id == "" {
				// Recover an initiation whose response or database commit was lost.
				if err := u.Storage.Delete(ctx, upload.StagingKey); err != nil {
					return storageError(err)
				}
				id, err = u.Multipart.StartMultipart(ctx, upload.StagingKey)
				if err != nil {
					return storageError(err)
				}
			}
			number := int32(len(parts) + 1)
			etag, err := u.Multipart.WritePart(ctx, upload.StagingKey, id, number, chunk)
			if err != nil {
				return storageError(err)
			}
			parts = append(parts, Part{Number: number, ETag: etag})
			if err := locked.SaveMultipartState(id, parts); err != nil {
				return err
			}
		}

		next = offset + int64(len(chunk))
		return locked.ConfirmReceived(next)
	})
	if err != nil {
		return offset, err
	}
	return next, nil
}

func storageError(err error) error {
	var fault *Error
	if errors.As(err, &fault) && fault.Code == "failed_precondition" {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return failure("canceled", "request_cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("deadline_exceeded", "deadline_exceeded")
	}
	return failure("unavailable", "asset_unavailable")
}
