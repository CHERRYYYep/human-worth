package grpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestContextFailureContract(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{context.Canceled, codes.Canceled, "request_cancelled"},
		{context.DeadlineExceeded, codes.DeadlineExceeded, "deadline_exceeded"},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("private dependency detail: %w", tc.err)} {
			failure := application.Failure(err)
			if !errors.Is(failure, tc.err) {
				t.Errorf("lost cause: %v", failure)
			}
			for _, input := range []error{err, failure} {
				got := status.Convert(Error(input))
				if got.Code() != tc.code || got.Message() != tc.reason {
					t.Errorf("%v: got %v", input, got)
				}
			}
		}
	}
	private := errors.New("private database details")
	failure := application.Failure(private)
	if !errors.Is(failure, private) || status.Convert(Error(failure)).Message() != "identity_unavailable" {
		t.Fatal("unknown failure must retain internal cause and hide external details")
	}
	if status.Code(Error(domain.ErrUnauthenticated)) != codes.Unauthenticated || Error(nil) != nil {
		t.Fatal("business/nil mapping changed")
	}
}
