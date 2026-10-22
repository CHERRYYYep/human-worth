package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

type ChallengeUpload struct {
	RunID, AttemptID, UploadID string
	Filename, Declared         string
	Expected                   int64
	ExpectedHash               []byte
}

type ChallengeFrame struct {
	HasMetadata bool
	Chunk       []byte
}

type ChallengeFrameSource interface {
	Recv() (ChallengeFrame, error)
}

type ChallengeMultipartStorage interface {
	StartMultipart(context.Context, string) (string, error)
	WritePart(context.Context, string, string, int32, []byte) (string, error)
	CompleteMultipart(context.Context, string, string, []Part) error
}

type ChallengeStorage interface {
	ChunkStorage
	ObjectStorage
}

type ChallengeUploadRepository interface {
	WithChallengeLease(context.Context, string, func(ChallengeUploadLease) error) error
}

type ChallengeUploadLease interface {
	LoadOrCreate(context.Context, ChallengeUpload) (Asset, error)
	SetRetryStaging(context.Context, string) error
	ResetMultipart(context.Context, string, string, func(context.Context, string) error) error
	WithLockedAsset(context.Context, string, func(LockedChallengeAsset) error) error
	MarkFailed(context.Context, string) error
	MarkReady(context.Context, string, string, int64, []byte, bool) (Asset, error)
}

type LockedChallengeAsset interface {
	MultipartState() (string, []Part, error)
	SaveMultipartState(string, []Part) error
	SetReceived(int64) error
}

type ChallengeUploadUseCase struct {
	Repository    ChallengeUploadRepository
	Storage       ChallengeStorage
	Multipart     ChallengeMultipartStorage
	ChunkBytes    int
	MaxAssetBytes int64
}

func (u ChallengeUploadUseCase) Execute(ctx context.Context, command ChallengeUpload, firstChunk []byte, source ChallengeFrameSource) (Asset, error) {
	if u.Repository == nil || u.Storage == nil || source == nil || u.ChunkBytes <= 0 {
		return Asset{}, failure("unavailable", "asset_upload_unavailable")
	}
	filename, err := cleanFilename(command.Filename)
	if err != nil {
		return Asset{}, err
	}
	declared, err := mediaType(command.Declared)
	if err != nil {
		return Asset{}, err
	}
	if command.Expected < 1 || command.Expected > u.MaxAssetBytes || len(command.ExpectedHash) != sha256.Size || command.RunID == "" || command.AttemptID == "" || command.UploadID == "" {
		return Asset{}, failure("invalid_argument", "invalid_upload")
	}
	var result Asset
	lockKey, _ := json.Marshal([]string{command.RunID, command.AttemptID, command.UploadID})
	err = u.Repository.WithChallengeLease(ctx, string(lockKey), func(lease ChallengeUploadLease) error {
		row, err := lease.LoadOrCreate(ctx, ChallengeUpload{RunID: command.RunID, AttemptID: command.AttemptID, UploadID: command.UploadID, Filename: filename, Declared: declared, Expected: command.Expected, ExpectedHash: command.ExpectedHash})
		if err != nil {
			return err
		}
		if row.Expected != command.Expected || !sameBytes(row.ExpectedHash, command.ExpectedHash) || row.Filename != filename || row.Declared != declared {
			return failure("aborted", "upload_conflict")
		}
		if row.State == "ready" {
			result = row
			return nil
		}
		if row.State != "staging" && row.State != "failed" {
			return failure("aborted", "upload_conflict")
		}
		if recovered, ok, recoverErr := u.recoverFinal(ctx, lease, row, declared); recoverErr != nil {
			return recoverErr
		} else if ok {
			result = recovered
			return nil
		}
		if row.State == "staging" && u.Multipart != nil && row.Received == row.Expected {
			result, err = u.finish(ctx, lease, row, command, declared)
			return err
		}
		restartMultipart := row.State == "failed" || row.Received > 0
		if row.State == "failed" {
			if err = lease.SetRetryStaging(ctx, row.ID); err != nil {
				return err
			}
			row.State = "staging"
		}
		// Challenge retries resend the complete stream from byte zero. Discard
		// any confirmed parts from an interrupted stream while the session-wide
		// advisory lock is held, then let this attempt build a fresh manifest.
		if u.Multipart != nil && restartMultipart {
			if err = lease.ResetMultipart(ctx, row.ID, row.StagingKey, u.Storage.Delete); err != nil {
				return err
			}
		}
		result, err = u.receive(ctx, lease, row, command, declared, firstChunk, source)
		return err
	})
	if err != nil {
		return Asset{}, err
	}
	return result, nil
}

func (u ChallengeUploadUseCase) receive(ctx context.Context, lease ChallengeUploadLease, row Asset, command ChallengeUpload, declared string, first []byte, source ChallengeFrameSource) (Asset, error) {
	digest := sha256.New()
	var received, stored int64
	var pending []byte
	if u.Multipart != nil {
		pending = make([]byte, 0, u.ChunkBytes)
	}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		chunk := append([]byte(nil), pending...)
		err := lease.WithLockedAsset(ctx, row.ID, func(locked LockedChallengeAsset) error {
			id, parts, err := locked.MultipartState()
			if err != nil {
				return err
			}
			if id == "" && stored != 0 {
				return failure("failed_precondition", "upload_session_restart_required")
			}
			if stored%int64(u.ChunkBytes) != 0 || len(parts) != int(stored/int64(u.ChunkBytes)) {
				return errors.New("multipart manifest does not match confirmed offset")
			}
			if id == "" {
				if err = u.Storage.Delete(ctx, row.StagingKey); err != nil {
					return storageError(err)
				}
				id, err = u.Multipart.StartMultipart(ctx, row.StagingKey)
				if err != nil {
					return storageError(err)
				}
			}
			number := int32(len(parts) + 1)
			etag, err := u.Multipart.WritePart(ctx, row.StagingKey, id, number, chunk)
			if err != nil {
				return storageError(err)
			}
			parts = append(parts, Part{Number: number, ETag: etag})
			if err = locked.SaveMultipartState(id, parts); err != nil {
				return err
			}
			return locked.SetReceived(stored + int64(len(chunk)))
		})
		if err == nil {
			stored += int64(len(chunk))
			pending = pending[:0]
		}
		return err
	}
	consume := func(chunk []byte) error {
		if len(chunk) > u.ChunkBytes || received+int64(len(chunk)) > command.Expected {
			return failure("invalid_argument", "invalid_upload_size")
		}
		if len(chunk) == 0 {
			return nil
		}
		original := chunk
		chunkSize := len(chunk)
		if u.Multipart != nil {
			for len(chunk) > 0 {
				n := min(u.ChunkBytes-len(pending), len(chunk))
				pending = append(pending, chunk[:n]...)
				chunk = chunk[n:]
				if len(pending) == u.ChunkBytes {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		} else if err := u.Storage.WriteAt(ctx, row.StagingKey, received, chunk); err != nil {
			return storageError(err)
		}
		_, _ = digest.Write(original)
		received += int64(chunkSize)
		return nil
	}
	if err := consume(first); err != nil {
		_ = u.fail(ctx, lease, row)
		return Asset{}, err
	}
	for {
		frame, err := source.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Asset{}, storageError(err)
		}
		if frame.HasMetadata {
			_ = u.fail(ctx, lease, row)
			return Asset{}, failure("invalid_argument", "duplicate_upload_metadata")
		}
		if err = consume(frame.Chunk); err != nil {
			_ = u.fail(ctx, lease, row)
			return Asset{}, err
		}
	}
	if received != command.Expected || !sameBytes(digest.Sum(nil), command.ExpectedHash) {
		_ = u.fail(ctx, lease, row)
		return Asset{}, failure("failed_precondition", "upload_digest_mismatch")
	}
	if u.Multipart != nil {
		if err := flush(); err != nil {
			return Asset{}, err
		}
	}
	return u.finish(ctx, lease, row, command, declared)
}

func (u ChallengeUploadUseCase) finish(ctx context.Context, lease ChallengeUploadLease, row Asset, command ChallengeUpload, declared string) (Asset, error) {
	if u.Multipart != nil {
		err := lease.WithLockedAsset(ctx, row.ID, func(locked LockedChallengeAsset) error {
			id, parts, err := locked.MultipartState()
			if err != nil {
				return err
			}
			if id == "" {
				return nil
			}
			if len(parts) != int((command.Expected+int64(u.ChunkBytes)-1)/int64(u.ChunkBytes)) {
				return errors.New("incomplete multipart manifest")
			}
			for i, part := range parts {
				if part.Number != int32(i+1) || part.ETag == "" {
					return errors.New("invalid multipart manifest")
				}
			}
			return u.Multipart.CompleteMultipart(ctx, row.StagingKey, id, parts)
		})
		if err != nil {
			return Asset{}, storageError(err)
		}
	}
	actual, size, detected, err := objectDigest(ctx, u.Storage, row.StagingKey)
	if err != nil {
		return Asset{}, storageError(err)
	}
	if size != command.Expected || !sameBytes(actual, command.ExpectedHash) {
		_ = u.fail(ctx, lease, row)
		return Asset{}, failure("failed_precondition", "upload_digest_mismatch")
	}
	if err = validateDetectedMediaType(declared, detected); err != nil {
		_ = u.fail(ctx, lease, row)
		return Asset{}, err
	}
	if err = u.Storage.Move(ctx, row.StagingKey, row.FinalKey); err != nil {
		return Asset{}, storageError(err)
	}
	finalHash, finalSize, finalType, err := objectDigest(ctx, u.Storage, row.FinalKey)
	if err != nil {
		return Asset{}, storageError(err)
	}
	if finalSize != size || !sameBytes(finalHash, command.ExpectedHash) || finalType != detected {
		return Asset{}, failure("failed_precondition", "upload_digest_mismatch")
	}
	return lease.MarkReady(ctx, row.ID, detected, size, actual, false)
}

func (u ChallengeUploadUseCase) recoverFinal(ctx context.Context, lease ChallengeUploadLease, row Asset, declared string) (Asset, bool, error) {
	actual, size, detected, err := objectDigest(ctx, u.Storage, row.FinalKey)
	if errors.Is(err, os.ErrNotExist) {
		return row, false, nil
	}
	if err != nil {
		return Asset{}, false, storageError(err)
	}
	if size != row.Expected || !sameBytes(actual, row.ExpectedHash) {
		if err = u.Storage.Delete(ctx, row.FinalKey); err != nil {
			return Asset{}, false, storageError(err)
		}
		return row, false, nil
	}
	if err = validateDetectedMediaType(declared, detected); err != nil {
		_ = u.fail(ctx, lease, row)
		if deleteErr := u.Storage.Delete(ctx, row.FinalKey); deleteErr != nil {
			return Asset{}, false, storageError(deleteErr)
		}
		return Asset{}, false, err
	}
	if err = u.Storage.Delete(ctx, row.StagingKey); err != nil {
		return Asset{}, false, storageError(err)
	}
	ready, err := lease.MarkReady(ctx, row.ID, detected, size, actual, true)
	return ready, err == nil, err
}

func (u ChallengeUploadUseCase) fail(ctx context.Context, lease ChallengeUploadLease, row Asset) error {
	if err := lease.MarkFailed(ctx, row.ID); err != nil {
		return err
	}
	return u.Storage.Delete(ctx, row.StagingKey)
}

func ChallengeLockKey(run, attempt, upload string) string {
	value, _ := json.Marshal([]string{run, attempt, upload})
	return string(value)
}

func ParseChallengeSHA256(value string) ([]byte, error) {
	if len(value) != sha256.Size*2 {
		return nil, failure("invalid_argument", "invalid_upload")
	}
	result, err := hex.DecodeString(value)
	if err != nil {
		return nil, failure("invalid_argument", "invalid_upload")
	}
	return result, nil
}
