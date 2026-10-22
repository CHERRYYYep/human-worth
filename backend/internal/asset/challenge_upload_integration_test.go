//go:build integration

package asset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	assetapp "github.com/KDZZZZZZ/human-worth/backend/internal/asset/application"
	assetpostgres "github.com/KDZZZZZZ/human-worth/backend/internal/asset/repository/postgres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func challengeFrame(id, declared string, data, chunk []byte) *pb.UploadAssetRequest {
	sum := sha256.Sum256(data)
	return &pb.UploadAssetRequest{
		Access:   &pb.Access{TaskId: "task_review", RunId: "run_review", AttemptId: "attempt_review", Purpose: "WORK_KIND_EXECUTE_PACKAGE"},
		UploadId: id, Filename: "review.bin", MediaType: declared,
		Bytes: int64(len(data)), Sha256: hex.EncodeToString(sum[:]), Chunk: chunk,
	}
}

func challengeKeys(t *testing.T, f *assetIntegration, id string) (string, string, string) {
	t.Helper()
	var assetID, staging, final string
	if err := f.runtimeDB.QueryRow(t.Context(), `SELECT id,staging_key,final_key FROM asset.assets WHERE run_id='run_review' AND attempt_id='attempt_review' AND upload_id=$1`, id).Scan(&assetID, &staging, &final); err != nil {
		t.Fatal(err)
	}
	return assetID, staging, final
}

func waitChallengeObject(t *testing.T, f *assetIntegration, store Storage, id string, size int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var key string
		if err := f.runtimeDB.QueryRow(ctx, `SELECT staging_key FROM asset.assets WHERE upload_id=$1`, id).Scan(&key); err == nil {
			if _, multipart := store.(assetapp.ChallengeMultipartStorage); multipart {
				var n int64
				if err := f.db.QueryRow(ctx, `SELECT received_bytes FROM asset.assets WHERE upload_id=$1`, id).Scan(&n); err == nil && n == size {
					return
				}
			} else if n, err := store.Stat(ctx, key); err == nil && n == size {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe upload %s staging size %d", id, size)
}

func testChallengeConcurrentRetry(t *testing.T, f *assetIntegration, a, b Storage) {
	ca, stopA := f.startStorage("challenge", a)
	defer stopA()
	cb, stopB := f.startStorage("challenge", b)
	defer stopB()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	data := []byte("ABCDEF")
	if _, multipart := a.(assetapp.ChallengeMultipartStorage); multipart {
		data = bytes.Repeat([]byte("a"), maxChunkBytes)
	}
	first, err := ca.UploadAsset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frame := challengeFrame("concurrent-review", "text/plain", data, data)
	if err = first.Send(frame); err != nil {
		t.Fatal(err)
	}
	// The original stream remains open after writing all bytes. Previously a
	// retry on another instance overwrote the same S3 key with only "ABC".
	waitChallengeObject(t, f, a, frame.UploadId, int64(len(data)))
	retry, err := cb.UploadAsset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = retry.Send(challengeFrame(frame.UploadId, "text/plain", data, data[:3])); err != nil {
		t.Fatal(err)
	}
	if _, err = retry.CloseAndRecv(); status.Code(err) != codes.Aborted || status.Convert(err).Message() != "upload_in_progress" {
		t.Fatalf("concurrent retry: %v, want Aborted upload_in_progress", err)
	}
	_, staging, final := challengeKeys(t, f, frame.UploadId)
	if _, multipart := b.(assetapp.ChallengeMultipartStorage); multipart {
		if _, err := b.Stat(ctx, staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("incomplete multipart is visible", err)
		}
		var n int64
		if err := f.db.QueryRow(ctx, `SELECT received_bytes FROM asset.assets WHERE id=$1`, func() string { id, _, _ := challengeKeys(t, f, frame.UploadId); return id }()).Scan(&n); err != nil || n != int64(len(data)) {
			t.Fatal("concurrent retry changed confirmed offset", n, err)
		}
	} else if !bytes.Equal(objectBytes(t, b, staging), data) {
		t.Fatal("concurrent retry changed staging")
	}
	completed, err := first.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := readChallenge(t, cb, &pb.ReadAssetRequest{AssetId: completed.Asset.Id, Access: frame.Access})
	if err != nil || !bytes.Equal(actual, data) || !bytes.Equal(objectBytes(t, b, final), data) || completed.Asset.Sha256 != sha256Hex(data) {
		t.Fatalf("ready bytes/metadata corrupted: bytes=%q err=%v asset=%v", actual, err, completed.Asset)
	}
	if _, err = a.Stat(ctx, staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging not removed: %v", err)
	}
	stable, err := challengeUploadNamed(t, cb, frame.Access, frame.UploadId, frame.Filename, data, sha256Hex(data))
	if err != nil || stable.GetId() != completed.Asset.Id {
		t.Fatalf("completed retry changed result: %v %v", stable, err)
	}
	var state, detected string
	var received int64
	var digest []byte
	if err = f.runtimeDB.QueryRow(ctx, `SELECT state,detected_media_type,received_bytes,actual_sha256 FROM asset.assets WHERE id=$1`, completed.Asset.Id).Scan(&state, &detected, &received, &digest); err != nil || state != "ready" || detected != "text/plain" || received != int64(len(data)) || hex.EncodeToString(digest) != sha256Hex(data) {
		t.Fatalf("wrong persisted result: %s %s %d %x %v", state, detected, received, digest, err)
	}
}

func TestChallengeConcurrentS3Retry(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	a, b := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	testChallengeConcurrentRetry(t, f, a, b)
}

func TestChallengeMultipartRetryRestartsInterruptedStream(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	storage, _ := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	firstClient, stopFirst := f.startStorage("challenge", storage)
	defer stopFirst()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	data := bytes.Repeat([]byte("a"), maxChunkBytes+(64<<10))
	frame := challengeFrame("multipart-interrupted-retry", "text/plain", data, data[:maxChunkBytes])
	first, err := firstClient.UploadAsset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Send(frame); err != nil {
		t.Fatal(err)
	}
	waitChallengeObject(t, f, storage, frame.UploadId, maxChunkBytes)
	cancel()
	_, _ = first.CloseAndRecv()

	secondClient, stopSecond := f.startStorage("challenge", storage)
	defer stopSecond()
	second, err := secondClient.UploadAsset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Send(frame); err != nil {
		t.Fatal(err)
	}
	if err = second.Send(&pb.UploadAssetRequest{Chunk: data[maxChunkBytes:]}); err != nil {
		t.Fatal(err)
	}
	result, err := second.CloseAndRecv()
	if err != nil {
		t.Fatal("complete stream retry after interruption:", err)
	}
	actual, err := readChallenge(t, secondClient, &pb.ReadAssetRequest{AssetId: result.GetAsset().GetId(), Access: frame.Access})
	if err != nil || !bytes.Equal(actual, data) || result.GetAsset().GetSha256() != sha256Hex(data) {
		t.Fatalf("retried bytes=%d sha=%s err=%v", len(actual), result.GetAsset().GetSha256(), err)
	}
}

func TestChallengeUploadPostgresRegression(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	f := newAssetIntegration(t, dsn)
	t.Run("panic_releases_advisory_lock", func(t *testing.T) {
		key := assetapp.ChallengeLockKey("panic-run", "panic-attempt", "panic-upload")
		// Hold a separate connection so a leaked session lock cannot pass by
		// being re-acquired reentrantly on the same PostgreSQL connection.
		probe, err := f.runtimeDB.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Release()

		repo := assetpostgres.UploadRepository{DB: f.runtimeDB}
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("expected the lease callback panic to propagate")
				}
			}()
			_ = repo.WithChallengeLease(t.Context(), key, func(assetapp.ChallengeUploadLease) error {
				panic("injected challenge upload panic")
			})
		}()

		var locked bool
		if err = probe.QueryRow(t.Context(), `SELECT pg_try_advisory_lock(hashtextextended($1,7185429004))`, key).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if !locked {
			t.Fatal("Challenge advisory lock remained held after panic")
		}
		if err = probe.QueryRow(t.Context(), `SELECT pg_advisory_unlock(hashtextextended($1,7185429004))`, key).Scan(&locked); err != nil || !locked {
			t.Fatalf("release probe advisory lock: unlocked=%t err=%v", locked, err)
		}
	})
	t.Run("concurrent_retry", func(t *testing.T) { testChallengeConcurrentRetry(t, f, f.storage, f.storage) })
	t.Run("cancel_releases_lock", func(t *testing.T) {
		ca, stopA := f.start("challenge")
		defer stopA()
		cb, stopB := f.start("challenge")
		defer stopB()
		data := []byte("ABCDEF")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream, err := ca.UploadAsset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		frame := challengeFrame("cancel-review", "text/plain", data, data[:3])
		if err = stream.Send(frame); err != nil {
			t.Fatal(err)
		}
		waitChallengeObject(t, f, f.storage, frame.UploadId, 3)
		cancel()
		_, _ = stream.CloseAndRecv()
		deadline := time.Now().Add(5 * time.Second)
		for {
			result, err := challengeUploadNamed(t, cb, frame.Access, frame.UploadId, frame.Filename, data, sha256Hex(data))
			if err == nil {
				if got, err := readChallenge(t, cb, &pb.ReadAssetRequest{AssetId: result.Id, Access: frame.Access}); err != nil || !bytes.Equal(got, data) {
					t.Fatalf("resumed bytes=%q err=%v", got, err)
				}
				break
			}
			if status.Code(err) != codes.Aborted || time.Now().After(deadline) {
				t.Fatal("cancelled stream retained lock or broke retry:", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	for _, test := range []struct {
		name, declared string
		data           []byte
		code           codes.Code
		reason         string
	}{
		{"mismatch", "image/png", []byte("plain text, not a PNG"), codes.FailedPrecondition, "media_type_mismatch"},
		{"html", "image/png", []byte("<html><body>not a PNG</body></html>"), codes.InvalidArgument, "unsupported_media_type"},
		{"png", "image/png", []byte("\x89PNG\r\n\x1a\npayload"), codes.OK, ""},
		{"text", "text/plain", []byte("verified plain text"), codes.OK, ""},
	} {
		for _, recovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("media_%s_recovery_%t", test.name, recovery), func(t *testing.T) {
				client, stop := f.start("challenge")
				defer stop()
				id := fmt.Sprintf("media-%s-%t", test.name, recovery)
				frame := challengeFrame(id, test.declared, test.data, test.data[:4])
				stream, err := client.UploadAsset(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err = stream.Send(frame); err != nil {
					t.Fatal(err)
				}
				if recovery {
					// Simulate copy succeeded but ready DB commit was lost. Both
					// recovery and new uploads must sniff actual stored content.
					if _, err = stream.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
						t.Fatal("partial upload accepted:", err)
					}
					_, _, final := challengeKeys(t, f, id)
					if err = f.storage.WriteAt(t.Context(), final, 0, test.data); err != nil {
						t.Fatal(err)
					}
					stream, err = client.UploadAsset(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if err = stream.Send(frame); err != nil {
						t.Fatal(err)
					}
				} else if err = stream.Send(&pb.UploadAssetRequest{Chunk: test.data[4:]}); err != nil {
					t.Fatal(err)
				}
				result, err := stream.CloseAndRecv()
				if status.Code(err) != test.code || (err != nil && status.Convert(err).Message() != test.reason) {
					t.Fatalf("code=%s error=%v, want %s %s", status.Code(err), err, test.code, test.reason)
				}
				assetID, staging, final := challengeKeys(t, f, id)
				var state string
				if err = f.runtimeDB.QueryRow(t.Context(), `SELECT state FROM asset.assets WHERE id=$1`, assetID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if test.code == codes.OK {
					if state != "ready" || result.Asset.MediaType != test.declared || !bytes.Equal(objectBytes(t, f.storage, final), test.data) {
						t.Fatal("ready metadata/content mismatch")
					}
				} else {
					if state != "failed" {
						t.Fatalf("invalid media left in state %q", state)
					}
					for _, key := range []string{staging, final} {
						if _, err = f.storage.Stat(t.Context(), key); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("rejected media object still exists: %v", err)
						}
					}
					if _, err = client.GetAsset(t.Context(), &pb.GetAssetRequest{AssetId: assetID, Access: frame.Access}); status.Code(err) != codes.NotFound {
						t.Fatalf("invalid media became readable: %v", err)
					}
				}
			})
		}
	}
}
