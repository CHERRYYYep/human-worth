//go:build integration

package asset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	assetapp "github.com/KDZZZZZZ/human-worth/backend/internal/asset/application"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The config must point to a disposable S3 test store, with permission to create
// and delete a fresh random bucket. No FileStorage or HTTP fake is substituted.
func newS3Integration(t *testing.T) (*S3Storage, *S3Storage) {
	t.Helper()
	path := os.Getenv("ASSET_S3_TEST_CONFIG_FILE")
	if path == "" {
		t.Skip("INCONCLUSIVE: ASSET_S3_TEST_CONFIG_FILE is not configured")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("INCONCLUSIVE: S3 test config requires a private readable file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read S3 test configuration")
	}
	var config S3Config
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal("invalid S3 test configuration")
	}
	config.Bucket = fmt.Sprintf("hw-asset-test-%x", time.Now().UnixNano())
	a, err := NewS3Storage(config)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewS3Storage(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(config.Bucket)})
	if err != nil {
		t.Fatalf("create isolated S3 bucket: %T", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pending := s3.NewListMultipartUploadsPaginator(a.client, &s3.ListMultipartUploadsInput{Bucket: aws.String(a.bucket)})
		for pending.HasMorePages() {
			page, e := pending.NextPage(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			for _, u := range page.Uploads {
				if e = a.Delete(ctx, aws.ToString(u.Key)); e != nil {
					t.Error(e)
				}
			}
		}
		pages := s3.NewListObjectsV2Paginator(a.client, &s3.ListObjectsV2Input{Bucket: aws.String(a.bucket)})
		for pages.HasMorePages() {
			page, e := pages.NextPage(ctx)
			if e != nil {
				t.Errorf("list test objects for cleanup: %T", e)
				return
			}
			for _, object := range page.Contents {
				if e = a.Delete(ctx, aws.ToString(object.Key)); e != nil {
					t.Errorf("delete test object: %T", e)
				}
			}
		}
		if _, e := a.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(a.bucket)}); e != nil {
			t.Errorf("delete test bucket: %T", e)
		}
	})
	if err = CheckStorage(t.Context(), a); err != nil {
		t.Fatalf("S3 readiness: %T", err)
	}
	return a, b
}
func objectBytes(t *testing.T, store Storage, key string) []byte {
	t.Helper()
	reader, err := store.Open(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(firstErr(err, closeErr))
	}
	return data
}
func TestS3StorageIntegration(t *testing.T) {
	a, b := newS3Integration(t)
	storageWriteContract(t, a)
	key, final := "assets/ast_test/staging/upload", "assets/ast_test/content"
	data := bytes.Repeat([]byte("real shared object storage\n"), 12000)
	offset := 0
	for offset < len(data) {
		end := min(offset+maxChunkBytes, len(data))
		if err := a.WriteAt(t.Context(), key, int64(offset), data[offset:end]); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if size, err := b.Stat(t.Context(), key); err != nil || size != int64(len(data)) {
		t.Fatalf("shared size=%d err=%v", size, err)
	}
	if !bytes.Equal(objectBytes(t, b, key), data) {
		t.Fatal("second adapter read different content")
	}
	// Simulate PUT succeeded but DB offset commit failed; retry must drop tail.
	if err := a.WriteAt(t.Context(), key, 0, []byte("prefix stale tail")); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteAt(t.Context(), key, 6, []byte(" final")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(objectBytes(t, a, key), []byte("prefix final")) {
		t.Fatal("uncommitted tail not discarded")
	}
	if err := a.Move(t.Context(), key, final); err != nil {
		t.Fatal(err)
	}
	if err := b.Move(t.Context(), key, final); err != nil {
		t.Fatal("copy/delete retry:", err)
	}
	if _, err := a.Stat(t.Context(), key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging remains", err)
	}
	if !bytes.Equal(objectBytes(t, b, final), []byte("prefix final")) {
		t.Fatal("copied final differs")
	}
	if err := b.Delete(t.Context(), final); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(t.Context(), final); err != nil {
		t.Fatal("delete not idempotent", err)
	}
	if _, err := b.Open(t.Context(), final); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted final readable", err)
	}
	interrupted := "assets/ast_interrupt/staging/upload"
	if err := a.WriteAt(t.Context(), interrupted, 0, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(t.Context(), interrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Stat(t.Context(), interrupted); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted staging remains", err)
	}
}
func TestAssetS3SharedPostgresIntegration(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	a, b := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	ca, stopA := f.startStorage("gateway", a)
	defer stopA()
	cb, stopB := f.startStorage("gateway", b)
	defer stopB()
	create := func(key string, data []byte) *pb.CreateUploadSessionResponse {
		t.Helper()
		sum := sha256.Sum256(data)
		result, err := ca.CreateUploadSession(t.Context(), &pb.CreateUploadSessionRequest{ActorAssertion: "alice", IdempotencyKey: key, OriginalFilename: "shared.txt", DeclaredMediaType: "text/plain", ExpectedBytes: int64(len(data)), ExpectedSha256: hex.EncodeToString(sum[:])})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	keys := func(id string) (string, string) {
		var staging, final string
		if err := f.runtimeDB.QueryRow(t.Context(), "SELECT staging_key,final_key FROM asset.assets WHERE id=$1", id).Scan(&staging, &final); err != nil {
			t.Fatal(err)
		}
		return staging, final
	}
	data := bytes.Repeat([]byte("s"), maxChunkBytes+37)
	created := create("s3-shared-upload-0001", data)
	if _, err := browserUpload(t, ca, created.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
		t.Fatal(err)
	}
	progress, err := cb.GetUploadSession(t.Context(), &pb.GetUploadSessionRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
	if err != nil || progress.GetUploadSession().GetReceivedBytes() != maxChunkBytes {
		t.Fatal("replica B progress", err)
	}
	staging, final := keys(created.Asset.Id)
	// Object PUT ahead of DB: another replica resumes from the DB offset.
	var multipartID string
	if err = f.db.QueryRow(t.Context(), `SELECT storage_upload_id FROM asset.assets WHERE id=$1`, created.Asset.Id).Scan(&multipartID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.WritePart(t.Context(), staging, multipartID, 2, []byte("uncommitted bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err = browserUploadAt(t, cb, created.UploadSession.Id, "alice", maxChunkBytes, data[maxChunkBytes:]); err != nil {
		t.Fatal(err)
	}
	completed, err := cb.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
	if err != nil || completed.GetAsset().GetState() != pb.AssetState_ASSET_STATE_READY {
		t.Fatal("replica B complete", err)
	}
	if _, err = a.Stat(t.Context(), staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging not deleted", err)
	}
	if !bytes.Equal(objectBytes(t, a, final), data) {
		t.Fatal("final bytes differ")
	}
	var state, sessionState string
	var received, expected int64
	var actual []byte
	err = f.runtimeDB.QueryRow(t.Context(), `SELECT a.state,s.state,a.received_bytes,a.expected_bytes,a.actual_sha256 FROM asset.assets a JOIN asset.upload_sessions s ON s.asset_id=a.id WHERE a.id=$1`, created.Asset.Id).Scan(&state, &sessionState, &received, &expected, &actual)
	sum := sha256.Sum256(data)
	if err != nil || state != "ready" || sessionState != "completed" || received != int64(len(data)) || expected != received || !bytes.Equal(actual, sum[:]) {
		t.Fatal("database/final disagree", err)
	}
	download, err := ca.DownloadAsset(t.Context(), &pb.DownloadAssetRequest{ActorAssertion: "alice", AssetId: created.Asset.Id})
	if err != nil {
		t.Fatal(err)
	}
	var downloaded []byte
	for {
		chunk, e := download.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		downloaded = append(downloaded, chunk.GetData()...)
	}
	if !bytes.Equal(downloaded, data) {
		t.Fatal("A cannot download B-completed asset")
	}
	// Copy+delete succeeded, DB still active/staging: a different server recovers.
	recovering := create("s3-recover-upload-01", data)
	if _, err = browserUpload(t, ca, recovering.UploadSession.Id, "alice", data[:maxChunkBytes], data[maxChunkBytes:]); err != nil {
		t.Fatal(err)
	}
	stage2, final2 := keys(recovering.Asset.Id)
	completeTestStaging(t, f, a, recovering.Asset.Id, stage2, int64(len(data)))
	if err = a.Move(t.Context(), stage2, final2); err != nil {
		t.Fatal(err)
	}
	if _, err = cb.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: recovering.UploadSession.Id}); err != nil {
		t.Fatal("recover final-before-commit", err)
	}
	// Abandoned staging/final (including copy-without-delete) can be cleaned;
	// an already-ready final must survive all expiration passes.
	expired := create("s3-expired-upload-01", data)
	if _, err = browserUpload(t, ca, expired.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
		t.Fatal(err)
	}
	stage3, final3 := keys(expired.Asset.Id)
	if err = a.WriteAt(t.Context(), final3, 0, []byte("orphan copy")); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(t.Context(), "UPDATE asset.upload_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1 OR id=$2", expired.UploadSession.Id, created.UploadSession.Id); err != nil {
		t.Fatal(err)
	}
	if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
		t.Fatal(err)
	}
	if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, a); err != nil {
		t.Fatal("cleanup repeat", err)
	}
	for _, key := range []string{stage3, final3} {
		if _, err = b.Stat(t.Context(), key); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expired object remains", err)
		}
	}
	if !bytes.Equal(objectBytes(t, b, final), data) {
		t.Fatal("ready final deleted by cleanup")
	}
	var cleaned bool
	if err = f.runtimeDB.QueryRow(t.Context(), "SELECT state='expired' AND storage_cleaned_at IS NOT NULL FROM asset.upload_sessions WHERE id=$1", expired.UploadSession.Id).Scan(&cleaned); err != nil || !cleaned {
		t.Fatal("cleanup not persisted", err)
	}
	// Independent instances serialize a shared session through PostgreSQL locks.
	concurrent := create("s3-concurrent-key-01", []byte("xx"))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, client := range []pb.AssetServiceClient{ca, cb} {
		wg.Add(1)
		go func(client pb.AssetServiceClient) {
			defer wg.Done()
			_, e := browserUpload(t, client, concurrent.UploadSession.Id, "alice", []byte("xx"))
			results <- e
		}(client)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		switch status.Code(e) {
		case codes.OK:
			success++
		case codes.Aborted:
			conflict++
		default:
			t.Fatal("unexpected concurrent upload error", e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	if _, err = cb.AbortUpload(t.Context(), &pb.AbortUploadRequest{ActorAssertion: "alice", UploadSessionId: concurrent.UploadSession.Id}); err != nil {
		t.Fatal(err)
	}
	if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
		t.Fatal(err)
	}
	// New server instance, same remote bucket and DB.
	restarted, stop := f.startStorage("gateway", b)
	defer stop()
	if _, err = restarted.GetAssetMetadata(t.Context(), &pb.GetAssetMetadataRequest{ActorAssertion: "alice", AssetId: created.Asset.Id}); err != nil {
		t.Fatal("restart metadata", err)
	}
}

func completeTestStaging(t *testing.T, f *assetIntegration, storage Storage, id, key string, size int64) {
	t.Helper()
	var uploadID string
	var parts []assetapp.Part
	if err := f.runtimeDB.QueryRow(t.Context(), `SELECT storage_upload_id,storage_parts FROM asset.assets WHERE id=$1`, id).Scan(&uploadID, &parts); err != nil {
		t.Fatal(err)
	}
	if uploadID == "" {
		return
	}
	multipart, ok := storage.(assetapp.ChallengeMultipartStorage)
	if !ok {
		t.Fatal("storage does not support multipart completion")
	}
	if err := multipart.CompleteMultipart(t.Context(), key, uploadID, parts); err != nil {
		t.Fatal(err)
	}
	_ = size
}
