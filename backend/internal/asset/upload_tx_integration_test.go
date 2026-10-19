//go:build integration

package asset

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"github.com/jackc/pgx/v5"
)

func TestBrowserChunkTransactionBoundaries(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	f := newAssetIntegration(t, dsn)
	data := []byte("chunk transactions end before the next frame")
	for _, duringWrite := range []bool{false, true} {
		name := "open_stream"
		if duringWrite {
			name = "cancel_during_write"
		}
		t.Run(name, func(t *testing.T) {
			created, _, _ := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
			if _, err := f.db.Exec(t.Context(), `UPDATE asset.assets SET received_bytes=0 WHERE id=$1`, created.Asset.Id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(t.Context(), `UPDATE asset.upload_sessions SET received_bytes=0 WHERE id=$1`, created.UploadSession.Id); err != nil {
				t.Fatal(err)
			}
			written := make(chan struct{})
			store := &uploadFaultStorage{Storage: f.storage}
			if duringWrite {
				store.write = func(ctx context.Context, key string, offset int64, chunk []byte) error {
					if err := f.storage.WriteAt(ctx, key, offset, chunk); err != nil {
						return err
					}
					close(written)
					<-ctx.Done()
					return ctx.Err()
				}
			}
			client, stop := f.startStorage("gateway", store)
			defer stop()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream, err := client.UploadAsset(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = stream.Send(&pb.UploadAssetRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id, UploadAttemptId: "p3-transaction-boundary"}); err != nil {
				t.Fatal(err)
			}
			if err = stream.Send(&pb.UploadAssetRequest{Chunk: data}); err != nil {
				t.Fatal(err)
			}
			if duringWrite {
				select {
				case <-written:
				case <-ctx.Done():
					t.Fatal("write not reached")
				}
				cancel()
				_, _ = stream.CloseAndRecv()
			}
			probe, probeCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer probeCancel()
			want := int64(len(data))
			if duringWrite {
				want = 0
			}
			for {
				var received int64
				if err = f.db.QueryRow(probe, `SELECT received_bytes FROM asset.upload_sessions WHERE id=$1`, created.UploadSession.Id).Scan(&received); err != nil {
					t.Fatal(err)
				}
				if received == want && f.runtimeDB.Stat().AcquiredConns() == 0 {
					break
				}
				select {
				case <-probe.Done():
					t.Fatal("stream retained its transaction/connection")
				case <-time.After(10 * time.Millisecond):
				}
			}
			// Complete from a separate instance while the original stream is
			// still waiting for its next frame, or resume after cancellation.
			recovery, stopRecovery := f.start("gateway")
			defer stopRecovery()
			if duringWrite {
				if _, err = browserUpload(t, recovery, created.UploadSession.Id, "alice", data); err != nil {
					t.Fatal("cancel recovery:", err)
				}
			}
			if _, err = recovery.CompleteUpload(probe, &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id}); err != nil {
				t.Fatal("completion blocked by earlier chunk:", err)
			}
			assertUploadState(t, f, created.UploadSession.Id, "ready", "completed")
		})
	}
}

func TestUploadTransactionCleanup(t *testing.T) {
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("INCONCLUSIVE: ASSET_TEST_DATABASE_URL[_FILE] is not configured")
	}
	f := newAssetIntegration(t, dsn)
	data := []byte("rollback must survive cancellation")
	created, _, _ := prepareBrowserUpload(t, f, data, "text/plain", sha256Hex(data))
	for _, mode := range []string{"cancel", "panic", "rejection"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			failure := errors.New("injected callback failure")
			var result error
			var caught any
			func() {
				defer func() { caught = recover() }()
				result = withinUploadTx(ctx, f.runtimeDB, func(tx pgx.Tx) error {
					calls++
					if _, err := tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='aborted' WHERE id=$1`, created.UploadSession.Id); err != nil {
						return err
					}
					if mode == "panic" {
						panic(failure)
					}
					if mode == "cancel" {
						cancel()
						return ctx.Err()
					}
					return failure
				})
			}()
			if calls != 1 || (mode == "panic" && caught != failure) || (mode == "rejection" && !errors.Is(result, failure)) || (mode == "cancel" && !errors.Is(result, context.Canceled)) {
				t.Fatalf("calls=%d panic=%v result=%v", calls, caught, result)
			}
			assertUploadState(t, f, created.UploadSession.Id, "staging", "active")
			// A second connection must acquire the same row immediately;
			// checking state alone would not detect a retained row lock.
			probe, probeCancel := context.WithTimeout(t.Context(), time.Second)
			defer probeCancel()
			tx, err := f.db.Begin(probe)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err = tx.Exec(probe, `SELECT id FROM asset.upload_sessions WHERE id=$1 FOR UPDATE NOWAIT`, created.UploadSession.Id); err != nil {
				t.Fatal("rollback retained row lock:", err)
			}
			if f.runtimeDB.Stat().AcquiredConns() != 0 {
				t.Fatal("transaction leaked a pool connection")
			}
		})
	}
}
