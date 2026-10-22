package asset

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failedUpload struct {
	pb.AssetService_UploadAssetServer
	err error
}

func (s failedUpload) Recv() (*pb.UploadAssetRequest, error) { return nil, s.err }

func TestInitialUploadContextFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{
		{context.Canceled, codes.Canceled}, {context.DeadlineExceeded, codes.DeadlineExceeded},
		{status.Error(codes.Canceled, "private details"), codes.Canceled},
		{status.Error(codes.DeadlineExceeded, "private details"), codes.DeadlineExceeded},
		{io.EOF, codes.InvalidArgument},
	} {
		err := (&Server{}).UploadAsset(failedUpload{err: fmt.Errorf("stream: %w", tc.err)})
		if status.Code(err) != tc.code {
			t.Errorf("%v: %v", tc.err, err)
		}
	}
}

// Limits apply before receiving a potentially 5 MiB message and are returned
// after rejected/closed streams, independently of peer connection count.
type heldUpload struct {
	pb.AssetService_UploadAssetServer
	entered chan<- struct{}
	release <-chan struct{}
}

func (s heldUpload) Recv() (*pb.UploadAssetRequest, error) {
	s.entered <- struct{}{}
	<-s.release
	return nil, io.EOF
}
func TestUploadBufferCapacity(t *testing.T) {
	server := &Server{}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	for i := 0; i < 2; i++ {
		go func() { done <- server.UploadAsset(heldUpload{entered: entered, release: release}) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("upload slot was not acquired")
		}
	}
	if err := server.UploadAsset(failedUpload{err: io.EOF}); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("capacity was not enforced", err)
	}
	close(release)
	release = nil
	for i := 0; i < 2; i++ {
		if err := <-done; status.Code(err) != codes.InvalidArgument {
			t.Fatal(err)
		}
	}
	if err := server.UploadAsset(failedUpload{err: io.EOF}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("upload slot leaked", err)
	}
}
