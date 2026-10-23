//go:build integration

package identity

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This connection forwards COMMIT to the real database, reads its response,
// then loses that response. The application must recover from durable receipts,
// rather than interpreting the connection error as proof of a rollback.
type publicBrowseLostCommitConn struct {
	net.Conn
	drop atomic.Bool
}

func (c *publicBrowseLostCommitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.drop.Swap(false) {
		_ = c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

type publicBrowseCommitTracer struct{}

func (publicBrowseCommitTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == "commit" {
		transport := c.PgConn().Conn()
		if tls, ok := transport.(interface{ NetConn() net.Conn }); ok {
			transport = tls.NetConn()
		}
		if conn, ok := transport.(*publicBrowseLostCommitConn); ok {
			conn.drop.Store(true)
		}
	}
	return ctx
}

func (publicBrowseCommitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func publicBrowseLostCommitPool(t *testing.T, original *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := original.Config()
	config.ConnConfig.Tracer = publicBrowseCommitTracer{}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &publicBrowseLostCommitConn{Conn: conn}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	must(t, err)
	t.Cleanup(pool.Close)
	return pool
}
