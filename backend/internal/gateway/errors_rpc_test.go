package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	identitygrpc "github.com/KDZZZZZZ/human-worth/backend/internal/identity/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type contextFailureIdentity struct {
	pb.UnimplementedIdentityServiceServer
	failure           error
	entered, released chan struct{}
}

func (s *contextFailureIdentity) ResolvePrincipal(ctx context.Context, _ *pb.ResolvePrincipalRequest) (*pb.ResolvePrincipalResponse, error) {
	if s.entered != nil {
		close(s.entered)
		<-ctx.Done()
		close(s.released)
		return nil, identitygrpc.Error(ctx.Err())
	}
	return nil, identitygrpc.Error(s.failure)
}

func contextFailureHTTP(t *testing.T, service *contextFailureIdentity) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterIdentityServiceServer(server, service)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	h, err := New(pb.NewIdentityServiceClient(client), Options{Origin: "https://worth.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewTLSServer(h)
	t.Cleanup(web.Close)
	return web
}

func TestHTTPThroughRPCContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		reason string
	}{
		{"deadline", context.DeadlineExceeded, 504, "deadline_exceeded"},
		{"cancel", context.Canceled, 503, "request_cancelled"},
		{"unavailable", errors.New("private database details"), 503, "identity_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web := contextFailureHTTP(t, &contextFailureIdentity{failure: fmt.Errorf("dependency: %w", tc.cause)})
			req, err := http.NewRequestWithContext(t.Context(), "GET", web.URL+"/api/me", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "worth.example.test"
			response, err := web.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || body.Code != tc.reason {
				t.Fatalf("got %d %s", response.StatusCode, body.Code)
			}
		})
	}
}

func TestHTTPClientCancellationReachesRPC(t *testing.T) {
	s := &contextFailureIdentity{entered: make(chan struct{}), released: make(chan struct{})}
	web := contextFailureHTTP(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", web.URL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "worth.example.test"
	done := make(chan error, 1)
	go func() {
		response, err := web.Client().Do(req)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-s.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("RPC not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP cancellation did not finish")
	}
	select {
	case <-s.released:
	case <-time.After(3 * time.Second):
		t.Fatal("RPC did not release work")
	}
}
