package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"
)

type digestStorage struct{ data []byte }

func (s digestStorage) WriteAt(context.Context, string, int64, []byte) error { return nil }
func (s digestStorage) Delete(context.Context, string) error                 { return nil }
func (s digestStorage) Stat(context.Context, string) (int64, error)          { return int64(len(s.data)), nil }
func (s digestStorage) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s digestStorage) Move(context.Context, string, string) error { return nil }

func TestObjectDigestUsesStoredBytes(t *testing.T) {
	data := []byte("<html><body>not a PNG</body></html>")
	storage := digestStorage{data: data}
	digest, size, detected, err := objectDigest(t.Context(), storage, "key")
	expected := sha256.Sum256(data)
	if err != nil || size != int64(len(data)) || !sameBytes(digest, expected[:]) || detected != "text/html" {
		t.Fatalf("stored digest=%x size=%d detected=%q err=%v", digest, size, detected, err)
	}
	if err = validateDetectedMediaType("image/png", detected); !hasErrorCode(err, "invalid_argument") {
		t.Fatalf("HTML accepted: %v", err)
	}
	if err = validateDetectedMediaType("image/png", "text/plain"); !hasErrorCode(err, "failed_precondition") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if err = validateDetectedMediaType("image/png", "image/png"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, err = objectDigest(cancelled, storage, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestFilenameAndMediaValidation(t *testing.T) {
	name, err := cleanFilename(`dir\\report.txt`)
	if err != nil || name != "report.txt" {
		t.Fatalf("filename=%q err=%v", name, err)
	}
	if _, err = cleanFilename("\x00"); !hasErrorCode(err, "invalid_argument") {
		t.Fatalf("control-only filename accepted: %v", err)
	}
	if _, err = mediaType("application/x-executable"); !hasErrorCode(err, "invalid_argument") {
		t.Fatalf("unsupported type accepted: %v", err)
	}
	if got, err := mediaType("TEXT/PLAIN; charset=utf-8"); err != nil || got != "text/plain" {
		t.Fatalf("media=%q err=%v", got, err)
	}
}

func hasErrorCode(err error, code string) bool {
	var fault *Error
	return errors.As(err, &fault) && fault.Code == code
}

type testRepository struct {
	upload BrowserUploadState
	id     string
	parts  []Part
}

func (r *testRepository) WithLockedBrowserUpload(_ context.Context, _ string, run func(LockedBrowserUpload) error) error {
	locked := &testLockedUpload{upload: r.upload, id: r.id, parts: append([]Part(nil), r.parts...)}
	if err := run(locked); err != nil {
		return err
	}
	r.id, r.parts, r.upload.Received = locked.id, locked.parts, locked.received
	return nil
}

type testLockedUpload struct {
	upload   BrowserUploadState
	id       string
	parts    []Part
	received int64
}

func (l *testLockedUpload) Upload() BrowserUploadState { return l.upload }
func (l *testLockedUpload) MultipartState() (string, []Part, error) {
	return l.id, append([]Part(nil), l.parts...), nil
}
func (l *testLockedUpload) SaveMultipartState(id string, parts []Part) error {
	l.id, l.parts = id, append([]Part(nil), parts...)
	return nil
}
func (l *testLockedUpload) ConfirmReceived(next int64) error {
	l.received = next
	return nil
}

type testStorage struct {
	writes int
	err    error
}

func (s *testStorage) WriteAt(context.Context, string, int64, []byte) error {
	s.writes++
	return s.err
}
func (*testStorage) Delete(context.Context, string) error { return nil }

func TestBrowserUploadUseCaseCommitsOnlyAfterStorageWrite(t *testing.T) {
	repository := &testRepository{upload: BrowserUploadState{
		Owner: "owner", State: "active", StagingKey: "staging",
		Expected: 8, ExpiresAt: time.Now().Add(time.Hour),
	}}
	storage := &testStorage{err: errors.New("storage unavailable")}
	useCase := BrowserUploadUseCase{Repository: repository, Storage: storage, ChunkBytes: 4, MaxAssetBytes: 16}
	if next, err := useCase.WriteChunk(t.Context(), "owner", "session", 0, []byte("data")); err == nil || next != 0 {
		t.Fatalf("WriteChunk() = (%d, %v), want unchanged offset and error", next, err)
	}
	if repository.upload.Received != 0 || storage.writes != 1 {
		t.Fatalf("received=%d writes=%d, want 0 and 1", repository.upload.Received, storage.writes)
	}
	storage.err = nil
	next, err := useCase.WriteChunk(t.Context(), "owner", "session", 0, []byte("data"))
	if err != nil || next != 4 || repository.upload.Received != 4 {
		t.Fatalf("WriteChunk() = (%d, %v), received=%d", next, err, repository.upload.Received)
	}
}

func TestBrowserUploadUseCaseRejectsOffsetBeforeStorage(t *testing.T) {
	repository := &testRepository{upload: BrowserUploadState{
		Owner: "owner", State: "active", StagingKey: "staging",
		Expected: 8, Received: 4, ExpiresAt: time.Now().Add(time.Hour),
	}}
	storage := &testStorage{}
	useCase := BrowserUploadUseCase{Repository: repository, Storage: storage, ChunkBytes: 4, MaxAssetBytes: 16}
	next, err := useCase.WriteChunk(t.Context(), "owner", "session", 0, []byte("data"))
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "aborted" || next != 0 || storage.writes != 0 {
		t.Fatalf("WriteChunk() = (%d, %v), writes=%d; want aborted, offset 0, no write", next, err, storage.writes)
	}
}
