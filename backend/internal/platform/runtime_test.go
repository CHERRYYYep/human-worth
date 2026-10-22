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

func TestPublicAssetTimeouts(t *testing.T) {
	if methodTimeout("/humanworth.asset.v1.AssetService/DownloadPublicAsset") != 15*time.Minute {
		t.Fatal("public stream deadline mismatch")
	}
	if methodTimeout("/humanworth.asset.v1.AssetService/CompleteUpload") != AssetCompleteTimeout {
		t.Fatal("asset completion deadline mismatch")
	}
	for _, method := range []string{"/humanworth.content.v1.ContentService/CheckAssetAccess", "/humanworth.content.v1.ContentService/ApplyAssetReview"} {
		if methodTimeout(method) != 2*time.Second {
			t.Fatal("public control-plane deadline mismatch", method)
		}
	}
}

func TestReviewMethodTimeouts(t *testing.T) {
	for _, method := range []string{"/humanworth.content.v1.ContentService/RequestTaskReview", "/humanworth.content.v1.ContentService/ApplyReviewDecision", "/humanworth.moderation.v1.ModerationService/CreateReviewCase", "/humanworth.moderation.v1.ModerationService/ListReviews"} {
		if methodTimeout(method) != 5*time.Second {
			t.Fatal("review delivery deadline mismatch", method)
		}
	}
	for _, method := range []string{"/humanworth.moderation.v1.ModerationService/DecideReview", "/humanworth.moderation.v1.ModerationService/GetReview", "/humanworth.content.v1.ContentService/GetReviewSnapshot", "/humanworth.content.v1.ContentService/CheckReviewAssetAccess"} {
		if methodTimeout(method) != 2*time.Second {
			t.Fatal("review control-plane deadline widened", method)
		}
	}
	if methodTimeout("/humanworth.asset.v1.AssetService/DownloadReviewAsset") != 15*time.Minute {
		t.Fatal("pending file stream deadline mismatch")
	}
}
