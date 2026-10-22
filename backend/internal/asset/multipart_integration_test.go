//go:build integration

package asset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Observe real SDK requests and optionally lose a successful response. Bytes
// still reach the real S3-compatible server; no object-store behavior is faked.
type multipartWire struct {
	parts, bytes, puts, reads, finalRecoveryReads atomic.Int64
	loseStart, losePart, loseComplete, failAbort  atomic.Bool
}

func observeMultipart(store *S3Storage, wire *multipartWire) {
	options := store.client.Options()
	original := options.HTTPClient
	options.HTTPClient = &http.Client{Transport: s3RoundTrip(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.Method == "PUT" && r.Header.Get("X-Amz-Copy-Source") == "" {
			if q.Has("partNumber") {
				wire.parts.Add(1)
				n := r.ContentLength
				if decoded := r.Header.Get("X-Amz-Decoded-Content-Length"); decoded != "" {
					n, _ = strconv.ParseInt(decoded, 10, 64)
				}
				wire.bytes.Add(n)
			} else {
				wire.puts.Add(1)
			}
		}
		if r.Method == "GET" && !q.Has("uploads") && !q.Has("uploadId") && !q.Has("list-type") {
			wire.reads.Add(1)
			if strings.HasSuffix(r.URL.Path, "/content") {
				wire.finalRecoveryReads.Add(1)
			}
		}
		if r.Method == "DELETE" && q.Has("uploadId") && wire.failAbort.Load() {
			return nil, errors.New("injected abort failure")
		}
		result, err := original.Do(r)
		if err != nil {
			return result, err
		}
		lose := false
		if result.StatusCode >= 200 && result.StatusCode < 300 {
			switch {
			case r.Method == "POST" && q.Has("uploads"):
				lose = wire.loseStart.Swap(false)
			case r.Method == "PUT" && q.Has("partNumber"):
				lose = wire.losePart.Swap(false)
			case r.Method == "POST" && q.Has("uploadId"):
				lose = wire.loseComplete.Swap(false)
			}
		}
		if lose {
			io.Copy(io.Discard, result.Body)
			result.Body.Close()
			return nil, errors.New("injected successful response loss")
		}
		return result, nil
	})}
	store.client = s3.New(options)
}
func multipartCreate(t *testing.T, client pb.AssetServiceClient, data []byte) *pb.CreateUploadSessionResponse {
	t.Helper()
	result, err := client.CreateUploadSession(t.Context(), &pb.CreateUploadSessionRequest{ActorAssertion: "alice", IdempotencyKey: fmt.Sprintf("p61-multipart-%d", time.Now().UnixNano()), OriginalFilename: "multipart.txt", DeclaredMediaType: "text/plain", ExpectedBytes: int64(len(data)), ExpectedSha256: sha256Hex(data)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func multipartProgress(t *testing.T, f *assetIntegration, id string) (string, string, int64, []uploadPart) {
	t.Helper()
	var key, upload string
	var received int64
	var parts []uploadPart
	if err := f.db.QueryRow(t.Context(), `SELECT staging_key,storage_upload_id,received_bytes,storage_parts FROM asset.assets WHERE id=$1`, id).Scan(&key, &upload, &received, &parts); err != nil {
		t.Fatal(err)
	}
	return key, upload, received, parts
}
func pendingUploads(t *testing.T, store *S3Storage, key string) int {
	t.Helper()
	pages := s3.NewListMultipartUploadsPaginator(store.client, &s3.ListMultipartUploadsInput{Bucket: aws.String(store.bucket), Prefix: aws.String(key)})
	n := 0
	for pages.HasMorePages() {
		page, err := pages.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range page.Uploads {
			if aws.ToString(u.Key) == key {
				n++
			}
		}
	}
	return n
}
func TestMultipartS3Recovery(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("ASSET_TEST_DATABASE_URL_FILE is required")
	}
	a, b := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	wire := &multipartWire{}
	observeMultipart(a, wire)
	ca, stopA := f.startStorage("gateway", a)
	defer stopA()
	cb, stopB := f.startStorage("gateway", b)
	defer stopB()
	data := bytes.Repeat([]byte("b"), maxChunkBytes+29)
	complete := func(t *testing.T, created *pb.CreateUploadSessionResponse) {
		t.Helper()
		result, err := cb.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
		if err != nil || result.GetAsset().GetSha256() != sha256Hex(data) {
			t.Fatalf("complete: %v %v", result, err)
		}
		if !bytes.Equal(download(t, ca, "alice", created.Asset.Id), data) {
			t.Fatal("ready bytes differ")
		}
	}
	t.Run("legacy_partial_and_full", func(t *testing.T) {
		for _, offset := range []int64{7, maxChunkBytes} {
			created := multipartCreate(t, ca, data)
			key, _, _, _ := multipartProgress(t, f, created.Asset.Id)
			if err := b.WriteAt(t.Context(), key, 0, data[:offset]); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(t.Context(), `UPDATE asset.assets SET received_bytes=$2 WHERE id=$1`, created.Asset.Id, offset); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET received_bytes=$2 WHERE id=$1`, created.UploadSession.Id, offset); err != nil {
				t.Fatal(err)
			}
			_, err := browserUploadAt(t, cb, created.UploadSession.Id, "alice", offset, data[offset:min(int64(len(data)), offset+maxChunkBytes)])
			if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "upload_session_restart_required" {
				t.Fatal("legacy continuation", err)
			}
			_, _, received, _ := multipartProgress(t, f, created.Asset.Id)
			if received != offset || !bytes.Equal(objectBytes(t, b, key), data[:offset]) {
				t.Fatal("legacy prefix/offset changed")
			}
			if _, err = cb.AbortUpload(t.Context(), &pb.AbortUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); err != nil {
				t.Fatal(err)
			}
		}
		small := []byte("legacy complete bytes")
		created := multipartCreate(t, ca, small)
		key, _, _, _ := multipartProgress(t, f, created.Asset.Id)
		if err := b.WriteAt(t.Context(), key, 0, small); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(t.Context(), `UPDATE asset.assets SET received_bytes=expected_bytes WHERE id=$1`, created.Asset.Id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET received_bytes=$2 WHERE id=$1`, created.UploadSession.Id, len(small)); err != nil {
			t.Fatal(err)
		}
		if _, err := cb.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); err != nil {
			t.Fatal("legacy completion", err)
		}
	})
	t.Run("reject_digest_and_expire", func(t *testing.T) {
		small := []byte("untrusted bytes")
		created, err := ca.CreateUploadSession(t.Context(), &pb.CreateUploadSessionRequest{ActorAssertion: "alice", IdempotencyKey: "p61-wrong-digest-0001", OriginalFilename: "bad.txt", DeclaredMediaType: "text/plain", ExpectedBytes: int64(len(small)), ExpectedSha256: strings.Repeat("0", 64)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = browserUpload(t, ca, created.UploadSession.Id, "alice", small); err != nil {
			t.Fatal(err)
		}
		key, _, _, _ := multipartProgress(t, f, created.Asset.Id)
		if _, err = cb.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); status.Code(err) != codes.FailedPrecondition {
			t.Fatal("wrong digest accepted", err)
		}
		assertUploadState(t, f, created.UploadSession.Id, "failed", "aborted")
		if pendingUploads(t, b, key) != 0 {
			t.Fatal("rejected upload leaked multipart")
		}
		expired := multipartCreate(t, ca, data)
		if _, err = browserUpload(t, ca, expired.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
			t.Fatal(err)
		}
		key, _, _, _ = multipartProgress(t, f, expired.Asset.Id)
		if _, err = f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.UploadSession.Id); err != nil {
			t.Fatal(err)
		}
		if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
			t.Fatal(err)
		}
		if pendingUploads(t, b, key) != 0 {
			t.Fatal("expired upload leaked multipart")
		}
		assertUploadState(t, f, expired.UploadSession.Id, "failed", "expired")
	})
	t.Run("initialization_response_lost", func(t *testing.T) {
		created := multipartCreate(t, ca, data)
		wire.loseStart.Store(true)
		if _, err := browserUpload(t, ca, created.UploadSession.Id, "alice", data[:maxChunkBytes]); status.Code(err) != codes.Unavailable {
			t.Fatal(err)
		}
		key, id, offset, parts := multipartProgress(t, f, created.Asset.Id)
		if id != "" || offset != 0 || len(parts) != 0 || pendingUploads(t, b, key) != 1 {
			t.Fatal("lost initialization not recoverable")
		}
		if _, err := browserUpload(t, cb, created.UploadSession.Id, "alice", data[:maxChunkBytes], data[maxChunkBytes:]); err != nil {
			t.Fatal(err)
		}
		if pendingUploads(t, b, key) != 1 {
			t.Fatal("unrecorded initialization leaked")
		}
		complete(t, created)
	})
	for _, failure := range []string{"part_response_lost", "session_update", "commit", "commit_response_lost"} {
		t.Run(failure, func(t *testing.T) {
			created := multipartCreate(t, ca, data)
			if _, err := browserUpload(t, ca, created.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
				t.Fatal(err)
			}
			_, id, _, parts := multipartProgress(t, f, created.Asset.Id)
			if id == "" || len(parts) != 1 || parts[0].ETag == "" {
				t.Fatal("part manifest not persisted")
			}
			original := f.runtimeDB
			client := ca
			if failure == "part_response_lost" {
				wire.losePart.Store(true)
			}
			if failure == "session_update" || failure == "commit" {
				kind := "update_session"
				if failure == "commit" {
					kind = "commit"
				}
				installUploadFailure(t, f, created, kind)
			}
			if failure == "commit_response_lost" {
				f.runtimeDB = lostCommitPool(t, original)
				defer func() { f.runtimeDB = original }()
				var stop func()
				client, stop = f.startStorage("gateway", a)
				defer stop()
			}
			if _, err := browserUploadAt(t, client, created.UploadSession.Id, "alice", maxChunkBytes, data[maxChunkBytes:]); status.Code(err) != codes.Unavailable {
				t.Fatal("unconfirmed part accepted", err)
			}
			f.runtimeDB = original
			_, retryID, offset, parts := multipartProgress(t, f, created.Asset.Id)
			want := int64(maxChunkBytes)
			if failure == "commit_response_lost" {
				want = int64(len(data))
			}
			if offset != want || id != retryID {
				t.Fatal("wrong persisted retry state", offset, want)
			}
			if failure == "session_update" || failure == "commit" {
				if _, err := f.db.Exec(t.Context(), `ALTER TABLE asset.upload_sessions DISABLE TRIGGER fail_upload_test`); err != nil {
					t.Fatal(err)
				}
			}
			if offset == maxChunkBytes {
				if _, err := browserUploadAt(t, cb, created.UploadSession.Id, "alice", offset, data[maxChunkBytes:]); err != nil {
					t.Fatal(err)
				}
			}
			complete(t, created)
		})
	}
	t.Run("complete_response_lost", func(t *testing.T) {
		created := multipartCreate(t, ca, data)
		if _, err := browserUpload(t, ca, created.UploadSession.Id, "alice", data[:maxChunkBytes], data[maxChunkBytes:]); err != nil {
			t.Fatal(err)
		}
		wire.loseComplete.Store(true)
		if _, err := ca.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); status.Code(err) != codes.Unavailable {
			t.Fatal(err)
		}
		assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
		if err := downloadError(t, cb, "alice", created.Asset.Id); status.Code(err) != codes.NotFound {
			t.Fatal("unready readable", err)
		}
		complete(t, created)
	})
	t.Run("abort_failure_survives_restart", func(t *testing.T) {
		created := multipartCreate(t, ca, data)
		if _, err := browserUpload(t, ca, created.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
			t.Fatal(err)
		}
		key, _, _, _ := multipartProgress(t, f, created.Asset.Id)
		// A neighboring prefix must survive cleanup for this exact object key.
		neighbor, err := b.StartMultipart(t.Context(), key+"-neighbor")
		if err != nil || neighbor == "" {
			t.Fatal(err)
		}
		wire.failAbort.Store(true)
		if _, err := ca.AbortUpload(t.Context(), &pb.AbortUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); err != nil {
			t.Fatal(err)
		}
		if pendingUploads(t, b, key) != 1 {
			t.Fatal("fault did not leave durable cleanup work")
		}
		wire.failAbort.Store(false)
		if err := cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
			t.Fatal(err)
		}
		if pendingUploads(t, b, key) != 0 || pendingUploads(t, b, key+"-neighbor") != 1 {
			t.Fatal("cleanup leaked or touched neighboring upload")
		}
		if err := b.Delete(t.Context(), key+"-neighbor"); err != nil {
			t.Fatal(err)
		}
	})
}
func TestMultipartS3LinearTransfer(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("ASSET_TEST_DATABASE_URL_FILE is required")
	}
	a, b := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	wire := &multipartWire{}
	observeMultipart(a, wire)
	client, stop := f.startStorage("gateway", a)
	defer stop()
	data := bytes.Repeat([]byte("c"), int(maxAssetBytes))
	created := multipartCreate(t, client, data)
	for offset := 0; offset < len(data); offset += maxChunkBytes {
		end := min(offset+maxChunkBytes, len(data))
		if _, err := browserUploadAt(t, client, created.UploadSession.Id, "alice", int64(offset), data[offset:end]); err != nil {
			t.Fatal(err)
		}
	}
	if wire.parts.Load() != 7 || wire.bytes.Load() != maxAssetBytes || wire.puts.Load() != 0 || wire.reads.Load() != 0 {
		t.Fatalf("write amplification: parts=%d bytes=%d puts=%d reads=%d", wire.parts.Load(), wire.bytes.Load(), wire.puts.Load(), wire.reads.Load())
	}
	key, _, received, parts := multipartProgress(t, f, created.Asset.Id)
	if received != maxAssetBytes || len(parts) != 7 {
		t.Fatal("full offset/manifest not durable")
	}
	other, stopOther := f.startStorage("gateway", b)
	defer stopOther()
	result, err := other.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
	if err != nil || result.GetAsset().GetSha256() != sha256Hex(data) {
		t.Fatal(err)
	}
	if pendingUploads(t, b, key) != 0 || !bytes.Equal(download(t, other, "alice", created.Asset.Id), data) {
		t.Fatal("full upload did not complete")
	}
	t.Logf("32 MiB uploaded as %d parts, %d payload bytes, zero prefix GET/PutObject requests", wire.parts.Load(), wire.bytes.Load())
}

func TestChallengeMultipartCleanupAndSmallFrames(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("ASSET_TEST_DATABASE_URL_FILE is required")
	}
	a, b := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	wire := &multipartWire{}
	observeMultipart(a, wire)
	ca, stopA := f.startStorage("challenge", a)
	defer stopA()
	cb, stopB := f.startStorage("challenge", b)
	defer stopB()
	data := bytes.Repeat([]byte("d"), maxChunkBytes+47)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := ca.UploadAsset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frame := challengeFrame("p61-challenge-abandon", "text/plain", data, nil)
	if err = stream.Send(frame); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < maxChunkBytes; offset += 64 << 10 {
		if err = stream.Send(&pb.UploadAssetRequest{Chunk: data[offset : offset+(64<<10)]}); err != nil {
			t.Fatal(err)
		}
	}
	waitChallengeObject(t, f, a, frame.UploadId, maxChunkBytes)
	assetID, staging, _ := challengeKeys(t, f, frame.UploadId)
	// Each stream first probes the final key for a previously completed upload
	// whose database commit may have been lost. This GET is not a staging read;
	// the still-open stream has only uploaded one part, coalesced from small frames.
	if wire.parts.Load() != 1 || wire.bytes.Load() != maxChunkBytes || wire.puts.Load() != 0 || wire.reads.Load() != 1 || wire.finalRecoveryReads.Load() != 1 {
		t.Fatal("small frames did not coalesce to one part")
	}
	if _, err = f.db.Exec(t.Context(), `UPDATE asset.assets SET updated_at=clock_timestamp()-interval '25 hours' WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
		t.Fatal(err)
	}
	if pendingUploads(t, b, staging) != 1 {
		t.Fatal("cleanup aborted a live stream")
	}
	cancel()
	_, _ = stream.CloseAndRecv()
	// Also leave an initiation without a recorded ID, as after a crash.
	if _, err = b.StartMultipart(t.Context(), staging); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err = cleanupExpiredForTest(t.Context(), f.runtimeDB, b); err != nil {
			t.Fatal(err)
		}
		var cleaned bool
		if err = f.db.QueryRow(t.Context(), `SELECT state='failed' AND storage_cleaned_at IS NOT NULL FROM asset.assets WHERE id=$1`, assetID).Scan(&cleaned); err != nil {
			t.Fatal(err)
		}
		if cleaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled Challenge retained its advisory lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pendingUploads(t, b, staging) != 0 {
		t.Fatal("restart cleanup leaked multipart uploads")
	}
	// Retry the same failed upload from small frames, preserving the asset ID.
	recovered, err := cb.UploadAsset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = recovered.Send(frame); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(data); offset += 64 << 10 {
		if err = recovered.Send(&pb.UploadAssetRequest{Chunk: data[offset:min(offset+(64<<10), len(data))]}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := recovered.CloseAndRecv()
	if err != nil || result.GetAsset().GetId() != assetID || result.GetAsset().GetSha256() != sha256Hex(data) {
		t.Fatal("failed Challenge retry", result, err)
	}
	actual, err := readChallenge(t, ca, &pb.ReadAssetRequest{AssetId: assetID, Access: frame.Access})
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("Challenge recovery bytes", err)
	}
}

// Capability probe only. Round-trip support does not prove day-based lifecycle
// execution. Our durable janitor must work independently of this feature.
func TestMultipartLifecycleCapability(t *testing.T) {
	a, _ := newS3Integration(t)
	_, err := a.client.PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(a.bucket), LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{ID: aws.String("p61-abort-incomplete"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("assets/")}, AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)}}}}})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "NotImplemented" || api.ErrorCode() == "InvalidRequest" || api.ErrorCode() == "InvalidArgument") {
			t.Logf("Lifecycle abort unsupported by test store: %s; durable cleanup remains required", api.ErrorCode())
			return
		}
		t.Fatal(err)
	}
	result, err := a.client.GetBucketLifecycleConfiguration(t.Context(), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(a.bucket)})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "NoSuchLifecycleConfiguration" {
			t.Log("Lifecycle PUT accepted but abort-only rule was not retained; no lifecycle safety net is assumed")
			return
		}
		t.Fatal(err)
	}
	supported := false
	for _, rule := range result.Rules {
		if aws.ToString(rule.ID) == "p61-abort-incomplete" && rule.AbortIncompleteMultipartUpload != nil && aws.ToInt32(rule.AbortIncompleteMultipartUpload.DaysAfterInitiation) == 1 {
			supported = true
		}
	}
	t.Logf("Lifecycle AbortIncompleteMultipartUpload preserved on read: %t; actual day-based execution is unverified", supported)
}
