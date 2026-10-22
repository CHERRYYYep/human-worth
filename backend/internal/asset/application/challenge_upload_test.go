package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

type challengeRepoFake struct {
	asset      Asset
	resetCalls int
	uploadID   string
	parts      []Part
}

func (r *challengeRepoFake) WithChallengeLease(_ context.Context, _ string, run func(ChallengeUploadLease) error) error {
	return run(&challengeLeaseFake{repo: r})
}

type challengeLeaseFake struct{ repo *challengeRepoFake }

func (l *challengeLeaseFake) LoadOrCreate(_ context.Context, input ChallengeUpload) (Asset, error) {
	if l.repo.asset.ID != "" {
		return l.repo.asset, nil
	}
	l.repo.asset = Asset{ID: "ast_test", Owner: "challenge", RunID: input.RunID, AttemptID: input.AttemptID, UploadID: input.UploadID, State: "staging", Filename: input.Filename, Declared: input.Declared, Expected: input.Expected, ExpectedHash: append([]byte(nil), input.ExpectedHash...), StagingKey: "staging", FinalKey: "final"}
	return l.repo.asset, nil
}
func (l *challengeLeaseFake) SetRetryStaging(context.Context, string) error {
	l.repo.asset.State = "staging"
	l.repo.asset.Received = 0
	return nil
}
func (l *challengeLeaseFake) ResetMultipart(ctx context.Context, _ string, key string, deleteObject func(context.Context, string) error) error {
	l.repo.resetCalls++
	if err := deleteObject(ctx, key); err != nil {
		return err
	}
	l.repo.asset.Received = 0
	l.repo.uploadID = ""
	l.repo.parts = nil
	return nil
}
func (l *challengeLeaseFake) WithLockedAsset(_ context.Context, _ string, run func(LockedChallengeAsset) error) error {
	return run(&lockedChallengeFake{repo: l.repo})
}
func (l *challengeLeaseFake) MarkFailed(_ context.Context, _ string) error {
	l.repo.asset.State = "failed"
	return nil
}
func (l *challengeLeaseFake) MarkReady(_ context.Context, _ string, detected string, size int64, actual []byte, _ bool) (Asset, error) {
	l.repo.asset.State = "ready"
	l.repo.asset.Detected = detected
	l.repo.asset.Received = size
	l.repo.asset.ActualHash = append([]byte(nil), actual...)
	now := time.Now()
	l.repo.asset.Completed = &now
	return l.repo.asset, nil
}

type lockedChallengeFake struct{ repo *challengeRepoFake }

func (l *lockedChallengeFake) MultipartState() (string, []Part, error) {
	return l.repo.uploadID, append([]Part(nil), l.repo.parts...), nil
}
func (l *lockedChallengeFake) SaveMultipartState(id string, parts []Part) error {
	l.repo.uploadID = id
	l.repo.parts = append([]Part(nil), parts...)
	return nil
}
func (l *lockedChallengeFake) SetReceived(n int64) error { l.repo.asset.Received = n; return nil }

type challengeStorageFake struct{ objects map[string][]byte }

func (s *challengeStorageFake) WriteAt(_ context.Context, key string, offset int64, data []byte) error {
	prior := s.objects[key]
	if offset > int64(len(prior)) {
		return errors.New("hole")
	}
	next := append([]byte(nil), prior[:offset]...)
	next = append(next, data...)
	s.objects[key] = next
	return nil
}
func (s *challengeStorageFake) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}
func (s *challengeStorageFake) Stat(_ context.Context, key string) (int64, error) {
	v, ok := s.objects[key]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(v)), nil
}
func (s *challengeStorageFake) Open(_ context.Context, key string) (io.ReadCloser, error) {
	v, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}
func (s *challengeStorageFake) Move(_ context.Context, from, to string) error {
	s.objects[to] = s.objects[from]
	delete(s.objects, from)
	return nil
}
func (s *challengeStorageFake) StartMultipart(context.Context, string) (string, error) {
	return "multipart-test", nil
}
func (s *challengeStorageFake) WritePart(_ context.Context, key, _ string, _ int32, data []byte) (string, error) {
	s.objects[key] = append(s.objects[key], data...)
	return "etag-test", nil
}
func (s *challengeStorageFake) CompleteMultipart(context.Context, string, string, []Part) error {
	return nil
}

type challengeFramesFake struct {
	frames []ChallengeFrame
	index  int
}

func (s *challengeFramesFake) Recv() (ChallengeFrame, error) {
	if s.index == len(s.frames) {
		return ChallengeFrame{}, io.EOF
	}
	f := s.frames[s.index]
	s.index++
	return f, nil
}

func TestChallengeUploadUseCaseStreamsAndValidatesStoredBytes(t *testing.T) {
	data := []byte("hello world")
	hash := sha256.Sum256(data)
	repo := &challengeRepoFake{}
	storage := &challengeStorageFake{objects: map[string][]byte{}}
	useCase := ChallengeUploadUseCase{Repository: repo, Storage: storage, ChunkBytes: 5, MaxAssetBytes: 1024}
	frames := &challengeFramesFake{frames: []ChallengeFrame{{Chunk: []byte(" worl")}, {Chunk: []byte("d")}}}
	got, err := useCase.Execute(t.Context(), ChallengeUpload{RunID: "run", AttemptID: "attempt", UploadID: "upload", Filename: "result.txt", Declared: "text/plain", Expected: int64(len(data)), ExpectedHash: hash[:]}, []byte("hello"), frames)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "ready" || got.Detected != "text/plain" || got.Received != int64(len(data)) {
		t.Fatalf("unexpected ready asset: %+v", got)
	}
	if !bytes.Equal(storage.objects[got.FinalKey], data) {
		t.Fatalf("final object=%q want %q", storage.objects[got.FinalKey], data)
	}
}

func TestChallengeUploadUseCaseRejectsDigestMismatch(t *testing.T) {
	data := []byte("not the expected bytes")
	hash := sha256.Sum256([]byte("expected"))
	repo := &challengeRepoFake{}
	storage := &challengeStorageFake{objects: map[string][]byte{}}
	useCase := ChallengeUploadUseCase{Repository: repo, Storage: storage, ChunkBytes: 8, MaxAssetBytes: 1024}
	_, err := useCase.Execute(t.Context(), ChallengeUpload{RunID: "run", AttemptID: "attempt", UploadID: "upload", Filename: "result.txt", Declared: "text/plain", Expected: int64(len(data)), ExpectedHash: hash[:]}, data[:8], &challengeFramesFake{frames: []ChallengeFrame{{Chunk: data[8:16]}, {Chunk: data[16:]}}})
	if !hasErrorCode(err, "failed_precondition") {
		t.Fatalf("error=%v want digest mismatch", err)
	}
	if repo.asset.State != "failed" {
		t.Fatalf("asset state=%q want failed", repo.asset.State)
	}
}

func TestChallengeUploadRetryDiscardsPreviouslyConfirmedParts(t *testing.T) {
	data := []byte("abcdef")
	hash := sha256.Sum256(data)
	repo := &challengeRepoFake{asset: Asset{
		ID: "ast_test", Owner: "challenge", RunID: "run", AttemptID: "attempt", UploadID: "upload",
		State: "staging", Filename: "result.txt", Declared: "text/plain", Expected: int64(len(data)),
		Received: 5, ExpectedHash: hash[:], StagingKey: "staging", FinalKey: "final",
	}}
	storage := &challengeStorageFake{objects: map[string][]byte{"staging": []byte("abcde")}}
	useCase := ChallengeUploadUseCase{Repository: repo, Storage: storage, Multipart: storage, ChunkBytes: 5, MaxAssetBytes: 1024}
	got, err := useCase.Execute(t.Context(), ChallengeUpload{RunID: "run", AttemptID: "attempt", UploadID: "upload", Filename: "result.txt", Declared: "text/plain", Expected: int64(len(data)), ExpectedHash: hash[:]}, data[:5], &challengeFramesFake{frames: []ChallengeFrame{{Chunk: data[5:]}}})
	if err != nil {
		t.Fatal(err)
	}
	if repo.resetCalls != 1 {
		t.Fatalf("reset calls=%d want 1", repo.resetCalls)
	}
	if got.State != "ready" || !bytes.Equal(storage.objects[got.FinalKey], data) {
		t.Fatalf("retry result=%+v bytes=%q", got, storage.objects[got.FinalKey])
	}
}
