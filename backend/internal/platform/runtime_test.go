package platform

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUnaryUsesMethodDeadline(t *testing.T) {
	const delay = 2500 * time.Millisecond
	type result struct {
		method string
		err    error
	}
	results := make(chan result, 2)
	for _, method := range []string{
		"/humanworth.content.v1.ContentService/ListMySubmissions",
		"/humanworth.content.v1.ContentService/GetMyTaskSubmission",
	} {
		go func(method string) {
			runtime := NewRuntime("test")
			runtime.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			_, err := runtime.Unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
				select {
				case <-time.After(delay):
					return struct{}{}, nil
				case <-ctx.Done():
					return nil, status.FromContextError(ctx.Err()).Err()
				}
			})
			results <- result{method: method, err: err}
		}(method)
	}

	for range 2 {
		result := <-results
		switch result.method {
		case "/humanworth.content.v1.ContentService/ListMySubmissions":
			if result.err != nil {
				t.Fatalf("list deadline did not cover controlled delay: %v", result.err)
			}
		default:
			if status.Code(result.err) != codes.DeadlineExceeded {
				t.Fatalf("ordinary RPC deadline changed: %v", result.err)
			}
		}
	}
}
