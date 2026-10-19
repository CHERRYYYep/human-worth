//go:build integration

package asset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only inject the failed storage operation; SQL, transactions and mTLS RPCs
// remain real. Existing S3 tests separately exercise the shared adapter.
type uploadFaultStorage struct {
	Storage
	stat      func(context.Context, string) (int64, error)
	write     func(context.Context, string, int64, []byte) error
	deleteErr error
	deletes   atomic.Int32
	writes    atomic.Int32
}

func (s *uploadFaultStorage) Stat(ctx context.Context, key string) (int64, error) {
	if s.stat != nil {
		return s.stat(ctx, key)
	}
	return s.Storage.Stat(ctx, key)
}
func (s *uploadFaultStorage) WriteAt(ctx context.Context, key string, offset int64, data []byte) error {
	s.writes.Add(1)
	if s.write != nil {
		return s.write(ctx, key, offset, data)
	}
	return s.Storage.WriteAt(ctx, key, offset, data)
}
func (s *uploadFaultStorage) Delete(ctx context.Context, key string) error {
	s.deletes.Add(1)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Storage.Delete(ctx, key)
}

func TestBrowserUploadChunkAndCompletionRecovery(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	f := newAssetIntegration(t, dsn)
	data := []byte("confirmed upload bytes")
	for _, operation := range []string{"chunk", "complete"} {
		for _, failure := range []string{"update_session", "commit", "commit_response_lost"} {
			t.Run(operation+"_"+failure, func(t *testing.T) {
				created, staging, final := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
				if operation == "chunk" {
					if _, err := f.db.Exec(t.Context(), `UPDATE asset.assets SET received_bytes=0 WHERE id=$1`, created.Asset.Id); err != nil {
						t.Fatal(err)
					}
					if _, err := f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET received_bytes=0 WHERE id=$1`, created.UploadSession.Id); err != nil {
						t.Fatal(err)
					}
				}
				if failure != "commit_response_lost" {
					installUploadFailure(t, f, created, failure)
				}
				original := f.runtimeDB
				if failure == "commit_response_lost" {
					f.runtimeDB = lostCommitPool(t, original)
				}
				defer func() { f.runtimeDB = original }()
				store := &uploadFaultStorage{Storage: f.storage}
				client, stop := f.startStorage("gateway", store)
				defer stop()
				var err error
				if operation == "chunk" {
					_, err = browserUpload(t, client, created.UploadSession.Id, "alice", data)
					if store.writes.Load() != 1 {
						t.Fatalf("chunk write called %d times, want exactly once", store.writes.Load())
					}
				} else {
					_, err = client.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
				}
				if status.Code(err) != codes.Unavailable {
					t.Fatalf("unconfirmed upload returned %v, want Unavailable", err)
				}
				if operation == "chunk" {
					var received int64
					if err = f.db.QueryRow(t.Context(), `SELECT received_bytes FROM asset.upload_sessions WHERE id=$1`, created.UploadSession.Id).Scan(&received); err != nil {
						t.Fatal(err)
					}
					want := int64(0)
					if failure == "commit_response_lost" {
						want = int64(len(data))
					}
					if received != want || !bytes.Equal(objectBytes(t, f.storage, staging), data) {
						t.Fatalf("unexpected recovery state: received=%d, want %d", received, want)
					}
				} else {
					if !bytes.Equal(objectBytes(t, f.storage, final), data) {
						t.Fatal("commit failure lost promoted object")
					}
					if failure == "commit_response_lost" {
						assertUploadState(t, f, created.UploadSession.Id, "ready", "completed")
					} else {
						assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
					}
				}
				// Remove the injected database fault, then recover through an
				// ordinary second service and the same persisted session.
				if failure != "commit_response_lost" {
					if _, err = f.db.Exec(t.Context(), "ALTER TABLE asset.upload_sessions DISABLE TRIGGER fail_upload_test"); err != nil {
						t.Fatal(err)
					}
				}
				f.runtimeDB = original
				recovery, stopRecovery := f.start("gateway")
				defer stopRecovery()
				progress, err := recovery.GetUploadSession(t.Context(), &pb.GetUploadSessionRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
				if err != nil {
					t.Fatal(err)
				}
				if operation == "chunk" && progress.UploadSession.ReceivedBytes == 0 {
					if _, err = browserUpload(t, recovery, created.UploadSession.Id, "alice", data); err != nil {
						t.Fatal("retry chunk:", err)
					}
				}
				completed, err := recovery.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
				if err != nil || completed.GetAsset().GetId() != created.Asset.Id || completed.GetAsset().GetSha256() != sha256Hex(data) {
					t.Fatalf("recovery result=%v err=%v", completed, err)
				}
				assertUploadState(t, f, created.UploadSession.Id, "ready", "completed")
				if got := download(t, recovery, "alice", created.Asset.Id); !bytes.Equal(got, data) {
					t.Fatal("recovered content differs")
				}
			})
		}
	}
}
func prepareBrowserUpload(t *testing.T, f *assetIntegration, data []byte, declared, digest string) (*pb.CreateUploadSessionResponse, string, string) {
	t.Helper()
	client, stop := f.start("gateway")
	defer stop()
	created, err := client.CreateUploadSession(t.Context(), &pb.CreateUploadSessionRequest{
		ActorAssertion: "alice", IdempotencyKey: fmt.Sprintf("p3-upload-%d", time.Now().UnixNano()),
		OriginalFilename: "p3.txt", DeclaredMediaType: declared,
		ExpectedBytes: int64(len(data)), ExpectedSha256: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = browserUpload(t, client, created.UploadSession.Id, "alice", data); err != nil {
		t.Fatal(err)
	}
	var staging, final string
	if err = f.db.QueryRow(t.Context(), `SELECT staging_key,final_key FROM asset.assets WHERE id=$1`, created.Asset.Id).Scan(&staging, &final); err != nil {
		t.Fatal(err)
	}
	return created, staging, final
}

func assertUploadState(t *testing.T, f *assetIntegration, id, assetState, sessionState string) {
	t.Helper()
	var a, s string
	if err := f.db.QueryRow(t.Context(), `SELECT a.state,s.state FROM asset.assets a JOIN asset.upload_sessions s ON s.asset_id=a.id WHERE s.id=$1`, id).Scan(&a, &s); err != nil {
		t.Fatal(err)
	}
	if a != assetState || s != sessionState {
		t.Fatalf("states asset=%s session=%s, want %s/%s", a, s, assetState, sessionState)
	}
}

func TestBrowserUploadFailures(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	f := newAssetIntegration(t, dsn)
	data := []byte("browser upload failure regression")
	t.Run("database_error_is_not_not_found", func(t *testing.T) {
		created, _, _ := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
		role := pgx.Identifier{f.runtimeDB.Config().ConnConfig.User}.Sanitize()
		if _, err := f.db.Exec(t.Context(), "REVOKE SELECT ON asset.upload_sessions FROM "+role); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.db.Exec(t.Context(), "GRANT SELECT ON asset.upload_sessions TO "+role); err != nil {
				t.Error(err)
			}
		}()
		client, stop := f.start("gateway")
		defer stop()
		_, err := client.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("complete: %v, want Unavailable", err)
		}
		_, err = client.AbortUpload(t.Context(), &pb.AbortUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("abort: %v, want Unavailable", err)
		}
		assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
	})

	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"unavailable", errors.New("storage offline"), codes.Unavailable},
		{"cancelled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
	} {
		for _, failFinal := range []bool{false, true} {
			t.Run(fmt.Sprintf("stat_%s_final_%t", tc.name, failFinal), func(t *testing.T) {
				created, staging, final := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
				store := &uploadFaultStorage{Storage: f.storage}
				store.stat = func(_ context.Context, key string) (int64, error) {
					if key == staging && failFinal {
						return 0, os.ErrNotExist
					}
					return 0, tc.err
				}
				client, stop := f.startStorage("gateway", store)
				defer stop()
				_, err := client.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
				if status.Code(err) != tc.code || store.deletes.Load() != 0 {
					t.Fatalf("error=%v deletes=%d, want %s and no deletion", err, store.deletes.Load(), tc.code)
				}
				assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
				if !bytes.Equal(objectBytes(t, f.storage, staging), data) {
					t.Fatal("storage failure changed staging")
				}
				if _, err = f.storage.Stat(t.Context(), final); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("storage failure created final", err)
				}
			})
		}
	}
	t.Run("missing_objects_are_incomplete", func(t *testing.T) {
		created, staging, _ := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
		if err := f.storage.Delete(t.Context(), staging); err != nil {
			t.Fatal(err)
		}
		client, stop := f.start("gateway")
		defer stop()
		_, err := client.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "upload_incomplete" {
			t.Fatal("missing objects:", err)
		}
	})

	for _, outcome := range []string{"digest", "media", "unsupported", "expired"} {
		for _, failure := range []string{"none", "update_asset", "update_session", "commit", "commit_response_lost", "delete"} {
			if outcome == "expired" && (failure == "update_asset" || failure == "delete") {
				continue
			}
			t.Run(outcome+"_"+failure, func(t *testing.T) {
				declared, digest := "text/plain", sha256Hex(data)
				contents := data
				code, reason := codes.FailedPrecondition, "sha256_mismatch"
				if outcome == "digest" {
					digest = strings.Repeat("0", 64)
				} else if outcome == "media" {
					declared, reason = "image/png", "media_type_mismatch"
				} else if outcome == "unsupported" {
					contents = []byte("<html><body>not a PNG</body></html>")
					declared, digest = "image/png", sha256Hex(contents)
					code, reason = codes.InvalidArgument, "unsupported_media_type"
				} else {
					reason = "upload_session_expired"
				}
				created, staging, _ := prepareBrowserUpload(t, f, contents, declared, digest)
				if outcome == "expired" {
					if _, err := f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, created.UploadSession.Id); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "update_asset" || failure == "update_session" || failure == "commit" {
					installUploadFailure(t, f, created, failure)
				}
				store := &uploadFaultStorage{Storage: f.storage}
				if failure == "delete" {
					store.deleteErr = errors.New("delete unavailable")
				}
				// Swap only the pool used to serve this call; keep an ordinary
				// connection for independently checking the persisted outcome.
				if failure == "commit_response_lost" {
					original := f.runtimeDB
					f.runtimeDB = lostCommitPool(t, original)
					defer func() { f.runtimeDB = original }()
				}
				client, stop := f.startStorage("gateway", store)
				defer stop()
				_, err := client.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
				if failure == "update_asset" || failure == "update_session" || failure == "commit" || failure == "commit_response_lost" {
					if status.Code(err) != codes.Unavailable || store.deletes.Load() != 0 {
						t.Fatalf("error=%v deletes=%d, want Unavailable and no deletion", err, store.deletes.Load())
					}
					if failure != "commit_response_lost" {
						assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
					}
				} else if status.Code(err) != code || status.Convert(err).Message() != reason {
					t.Fatalf("error=%v, want %s %s", err, code, reason)
				}
				committed := failure == "none" || failure == "delete" || failure == "commit_response_lost"
				if committed {
					if outcome == "expired" {
						assertUploadState(t, f, created.UploadSession.Id, "staging", "expired")
					} else {
						assertUploadState(t, f, created.UploadSession.Id, "failed", "aborted")
					}
				}
				_, statErr := f.storage.Stat(t.Context(), staging)
				if failure == "none" && outcome != "expired" {
					if store.deletes.Load() != 1 || !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("committed rejection did not clean staging", statErr)
					}
				} else if statErr != nil || !bytes.Equal(objectBytes(t, f.storage, staging), contents) {
					t.Fatal("unconfirmed rejection or failed deletion lost staging", statErr)
				}
				if failure == "delete" || failure == "commit_response_lost" {
					if err := CleanupExpired(t.Context(), f.db, f.storage); err != nil {
						t.Fatal("durable cleanup retry:", err)
					}
					if _, err := f.storage.Stat(t.Context(), staging); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("cleanup retry retained staging", err)
					}
				}
			})
		}
	}
}

func installUploadFailure(t *testing.T, f *assetIntegration, created *pb.CreateUploadSessionResponse, failure string) {
	t.Helper()
	table, id, timing := "upload_sessions", created.UploadSession.Id, "BEFORE"
	constraint := ""
	if failure == "update_asset" {
		table, id = "assets", created.Asset.Id
	} else if failure == "commit" {
		constraint, timing = "CONSTRAINT ", "AFTER"
	}
	deferred := ""
	if failure == "commit" {
		deferred = "DEFERRABLE INITIALLY DEFERRED"
	}
	query := `CREATE FUNCTION asset.fail_upload_test() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.id = TG_ARGV[0] THEN RAISE EXCEPTION 'injected upload state failure'; END IF;
		RETURN NEW;
	END $$;
	CREATE ` + constraint + `TRIGGER fail_upload_test ` + timing + ` UPDATE ON asset.` + table + ` ` + deferred + ` FOR EACH ROW EXECUTE FUNCTION asset.fail_upload_test('` + id + `')`
	if _, err := f.db.Exec(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.db.Exec(context.Background(), "DROP TRIGGER fail_upload_test ON asset."+table+"; DROP FUNCTION asset.fail_upload_test()"); err != nil {
			t.Error(err)
		}
	})
}

// Forward COMMIT and receive PostgreSQL's response, then discard that response
// as a broken connection. The transaction really committed, but the caller
// cannot know it; this must never authorize synchronous object deletion.
type lostCommitConn struct {
	net.Conn
	drop atomic.Bool
}

func (c *lostCommitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.drop.Swap(false) {
		_ = c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

type lostCommitTracer struct{}

func (lostCommitTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "commit" {
		transport := conn.PgConn().Conn()
		// Unwrap TLS without relying on plaintext SQL in socket writes.
		if tls, ok := transport.(interface{ NetConn() net.Conn }); ok {
			transport = tls.NetConn()
		}
		if raw, ok := transport.(*lostCommitConn); ok {
			raw.drop.Store(true)
		}
	}
	return ctx
}
func (lostCommitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func lostCommitPool(t *testing.T, original *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := original.Config()
	config.ConnConfig.Tracer = lostCommitTracer{}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &lostCommitConn{Conn: conn}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
