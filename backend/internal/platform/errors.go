package platform

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ContextError recognizes raw/wrapped context errors and dependency RPC statuses.
// It returns only stable public reasons; nil means the error is not a context failure.
func ContextError(err error) error {
	if err == nil {
		return nil
	}
	code := status.FromContextError(err).Code()
	if code == codes.Unknown {
		code = status.Code(err)
	}
	switch code {
	case codes.Canceled:
		return status.Error(code, "request_cancelled")
	case codes.DeadlineExceeded:
		return status.Error(code, "deadline_exceeded")
	default:
		return nil
	}
}

// DependencyError preserves cancellation/deadline categories and hides other details.
func DependencyError(err error, reason string) error {
	if contextErr := ContextError(err); contextErr != nil {
		return contextErr
	}
	return status.Error(codes.Unavailable, reason)
}
