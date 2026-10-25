//go:build integration

package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	votingpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/voting/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Agent Self-Claimed: drop the acknowledgement of the Nth real PG COMMIT,
// not the commit itself. Later connections keep using the real database.
type votingNthCommitTracer struct{ remaining atomic.Int64 }

func (tr *votingNthCommitTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == "commit" && tr.remaining.Add(-1) == 0 {
		transport := c.PgConn().Conn()
		if tls, ok := transport.(interface{ NetConn() net.Conn }); ok {
			transport = tls.NetConn()
		}
		if wrapped, ok := transport.(*votingConfirmationLossConn); ok {
			transport = wrapped.publicBrowseLostCommitConn
		}
		if conn, ok := transport.(*publicBrowseLostCommitConn); ok {
			conn.drop.Store(true)
		}
	}
	return ctx
}
func (*votingNthCommitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func votingNthCommitPool(t *testing.T, original *pgxpool.Pool, n int64, denyConfirmation ...bool) *pgxpool.Pool {
	t.Helper()
	config := original.Config()
	tracer := &votingNthCommitTracer{}
	tracer.remaining.Store(n)
	config.ConnConfig.Tracer = tracer
	blocked := &atomic.Bool{}
	if len(denyConfirmation) > 0 && denyConfirmation[0] {
		config.MinConns = 0
		config.MaxConns = 1
	}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		if blocked.Load() {
			return nil, errors.New("injected_confirmation_connection_loss")
		}
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		connWithLoss := &publicBrowseLostCommitConn{Conn: conn}
		if len(denyConfirmation) > 0 && denyConfirmation[0] {
			return &votingConfirmationLossConn{publicBrowseLostCommitConn: connWithLoss, blocked: blocked}, nil
		}
		return connWithLoss, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	must(t, err)
	t.Cleanup(pool.Close)
	return pool
}

type votingConfirmationLossConn struct {
	*publicBrowseLostCommitConn
	blocked *atomic.Bool
}

func (c *votingConfirmationLossConn) Read(p []byte) (int, error) {
	n, err := c.publicBrowseLostCommitConn.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		c.blocked.Store(true)
	}
	return n, err
}
func TestVotingStatisticsUnknownCommitCannotConfirm(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-confirmation-loss-task")
	f.votes[0].DB = votingNthCommitPool(t, f.votingDB, 1, true)
	response := f.request(t, 0, f.bob, "POST", "/api/tasks/"+task.ID+"/statistics-access", `{"permanentlyCloseVoting":true}`, nil, 503)
	data, e := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, e)
	for _, field := range []string{"totalVotes", "humanVotes", "agentVotes", "entryVotes", "voteShare"} {
		if bytes.Contains(data, []byte(field)) {
			t.Fatal("unconfirmed commit leaked statistics", field)
		}
	}
	var first time.Time
	must(t, f.votingDB.QueryRow(t.Context(), `SELECT closed_at FROM voting.statistics_access WHERE account_id=$1 AND task_id=$2`, f.bob.account, task.ID).Scan(&first))
	f.votes[0].DB = f.votingDB
	var stats votingStats
	votingRead(t, f.request(t, 1, f.bob, "POST", "/api/tasks/"+task.ID+"/statistics-access", `{"permanentlyCloseVoting":true}`, nil, 200), &stats)
	if !stats.Eligibility.PermanentlyClosed || stats.Eligibility.ClosedAt != first.UTC().Format(time.RFC3339Nano) {
		t.Fatal("recovery changed permanent closure")
	}
}
func TestVotingContentUnknownCommitRecovery(t *testing.T) {
	for n, phase := range []string{"queued", "applied", "completed"} {
		t.Run(phase, func(t *testing.T) {
			f := newAdditionalEntryLab(t, true)
			task := f.publish(t, "voting-unknown-commit-task")
			f.contents[1].DB = votingNthCommitPool(t, f.contentDB, int64(n+1))
			err := votingTakedown(t, f, task.ID, "voting-unknown-commit-down")
			if err == nil {
				t.Fatal("lost Content COMMIT response was not observed")
			}
			f.contents[1].DB = f.contentDB
			var stage string
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT stage FROM content.voting_catalog_changes WHERE task_id=$1 AND generation=2`, task.ID).Scan(&stage))
			if stage != phase {
				t.Fatalf("durable stage=%s, want %s", stage, phase)
			}
			// The same business command recovers its durable intent/application/receipt;
			// another instance does not allocate a new generation or publish stale data.
			if phase != "completed" {
				_, err = f.identity.db.Exec(t.Context(), `UPDATE content.voting_catalog_changes SET available_at=clock_timestamp() WHERE task_id=$1 AND stage IN ('queued','applied')`, task.ID)
				must(t, err)
				result, e := f.contents[0].DeliverCatalog(t.Context())
				must(t, e)
				if result.Outcome != "delivered" {
					t.Fatal(result)
				}
			}
			must(t, votingTakedownReplay(t, f, task.ID, "voting-unknown-commit-down"))
			var count int
			var generation int64
			var active *string
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT generation,active_change FROM content.voting_catalog_heads WHERE task_id=$1`, task.ID).Scan(&generation, &active))
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT count(*) FROM content.voting_catalog_changes WHERE task_id=$1`, task.ID).Scan(&count))
			if generation != 2 || active != nil || count != 2 {
				t.Fatal("unknown commit duplicated or reopened catalog", fmt.Sprint(generation, count))
			}
			f.request(t, 0, additionalActor{}, "GET", "/api/tasks/"+task.ID, "", nil, 404).Body.Close()
		})
	}
}

// A connection really closes between locking the gate and looking up its
// prepare receipt. Connectivity loss must remain retryable, not a durable 409.
type votingActivationQueryLoss struct{}

func (votingActivationQueryLoss) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "SELECT task_id,generation,phase,sha256 FROM voting.catalog_changes") {
		_ = c.PgConn().Close(ctx)
	}
	return ctx
}
func (votingActivationQueryLoss) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestVotingActivationDatabaseFailureIsRetryable(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-activation-failure-task")
	result, err := f.catalogs[0].GetVotingCatalogSnapshot(t.Context(), &contentpb.GetVotingCatalogSnapshotRequest{TaskId: task.ID})
	must(t, err)
	c := proto.Clone(result.Catalog).(*contentpb.VotingCatalogSnapshot)
	c.Generation++
	c.Sha256 = contract.CatalogHash(c)
	key := "chg_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	_, err = f.contents[0].Voting.PrepareCatalogChange(t.Context(), &votingpb.PrepareCatalogChangeRequest{TaskId: task.ID, ChangeId: key, ExpectedGeneration: c.Generation - 1, Generation: c.Generation})
	must(t, err)
	config := f.votingDB.Config()
	config.ConnConfig.Tracer = votingActivationQueryLoss{}
	fault, err := pgxpool.NewWithConfig(t.Context(), config)
	must(t, err)
	t.Cleanup(fault.Close)
	f.votes[0].DB = fault
	_, err = f.contents[0].Voting.ActivateCatalogSnapshot(t.Context(), &votingpb.ActivateCatalogSnapshotRequest{ChangeId: key, Catalog: c})
	grpcCode(t, err, codes.Unavailable)
	f.votes[0].DB = f.votingDB
	_, err = f.contents[1].Voting.ActivateCatalogSnapshot(t.Context(), &votingpb.ActivateCatalogSnapshotRequest{ChangeId: key, Catalog: c})
	must(t, err)
}
