package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/challenge/application/dto"
	"github.com/KDZZZZZZ/human-worth/backend/internal/challenge/domain"
)

type failedUploadStream struct {
	UploadStream
	err error
}

func (s failedUploadStream) Context() context.Context                  { return context.Background() }
func (s failedUploadStream) Recv() (*dto.UploadArtifactRequest, error) { return nil, s.err }

func TestUploadInitialContextFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code domain.Code
	}{
		{context.Canceled, domain.Canceled}, {context.DeadlineExceeded, domain.DeadlineExceeded},
		{domain.Reject(domain.Canceled, "request_cancelled"), domain.Canceled},
		{io.EOF, domain.InvalidArgument},
	} {
		got := (&Service{}).UploadArtifact(failedUploadStream{err: fmt.Errorf("stream: %w", tc.err)})
		if domain.ErrorCode(got) != tc.code {
			t.Errorf("%v: %v", tc.err, got)
		}
		if tc.code != domain.InvalidArgument && !errors.Is(got, tc.err) {
			t.Fatal("lost cause")
		}
	}
}
