//go:build lab

package identity

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Harness regression only: real HTTP servers hold the second caller behind a
// barrier. Business authorization itself is tested with real Pods/PG/mTLS below.
func TestKindRecoveryCheckpointAllCallers(t *testing.T) {
	var first, second atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	a := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first.Add(1)
		w.WriteHeader(404)
	}))
	defer a.Close()
	b := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if second.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(503)
			io.WriteString(w, `{"code":"asset_unavailable"}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer b.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// Only rewrite the destination, as the real kind fixture does. HTTP request
	// construction, status/error decoding, waiting and cancellation are real.
	client := func(server *httptest.Server) *http.Client {
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		transport.TLSClientConfig.ServerName = "127.0.0.1"
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport}
	}
	clients := []*http.Client{client(a), client(b)}
	done := make(chan error, 1)
	go func() { done <- waitReadRecovery(ctx, clients, "", recoveryPublicFile, 404) }()
	select {
	case err := <-done:
		t.Fatalf("checkpoint returned before second caller recovered: %v", err)
	case <-entered:
	case <-ctx.Done():
		t.Fatal("second actual caller was not probed")
	}
	select {
	case err := <-done:
		t.Fatalf("checkpoint accepted unavailable second caller: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if first.Load() != 1 || second.Load() != 2 {
		t.Fatalf("incorrect independent probes: %d/%d", first.Load(), second.Load())
	}
}

func TestKindRecoveryRejectsWrongErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{503, `{"code":"asset_unavailable"}`}, {504, `{"code":"deadline_exceeded"}`}} {
		if err := dependencyFailure(tc.status, []byte(tc.body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{403, `{"code":"administrator_required"}`},
		{500, `{"code":"internal_error"}`},
		{503, `{"code":"request_cancelled"}`},
		{503, `{"code":"invented_unavailable"}`},
		{504, `{"code":"asset_unavailable"}`},
		{503, `not json`},
	} {
		if dependencyFailure(tc.status, []byte(tc.body)) == nil {
			t.Fatalf("accepted non-recoverable response %d", tc.status)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := waitReadRecovery(ctx, []*http.Client{http.DefaultClient}, "", recoveryPublicFile, 404)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestKindRecoveryBoundsUnavailableWait(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, `{"code":"content_unavailable"}`)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := waitReadRecovery(ctx, []*http.Client{{Transport: transport}}, "", recoveryPublicFile, 404)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("persistent outage passed or lost deadline: %v", err)
	}
}
