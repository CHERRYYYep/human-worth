package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDependencyErrorContract(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{context.Canceled, codes.Canceled, "request_cancelled"},
		{context.DeadlineExceeded, codes.DeadlineExceeded, "deadline_exceeded"},
		{status.Error(codes.Canceled, "private details"), codes.Canceled, "request_cancelled"},
		{status.Error(codes.DeadlineExceeded, "private details"), codes.DeadlineExceeded, "deadline_exceeded"},
		{errors.New("private SQL"), codes.Unavailable, "test_unavailable"},
		{status.Error(codes.NotFound, "private detail"), codes.Unavailable, "test_unavailable"},
	} {
		for _, input := range []error{tc.err, fmt.Errorf("operation: %w", tc.err)} {
			got := status.Convert(DependencyError(input, "test_unavailable"))
			if got.Code() != tc.code || got.Message() != tc.reason {
				t.Errorf("%v: %v", input, got)
			}
		}
	}
	if ContextError(nil) != nil || ContextError(errors.New("unknown")) != nil {
		t.Fatal("classified non-context error")
	}
}
