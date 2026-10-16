package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Protocol/error unit tests only. Real authorization lives in integration/lab.
type publicAssetClient struct {
	pb.AssetServiceClient
	frames []*pb.DownloadPublicAssetResponse
	err    error
}

func (c publicAssetClient) DownloadPublicAsset(context.Context, *pb.DownloadPublicAssetRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.DownloadPublicAssetResponse], error) {
	return &publicAssetStream{frames: c.frames, err: c.err}, nil
}

type publicAssetStream struct {
	grpc.ClientStream
	frames []*pb.DownloadPublicAssetResponse
	err    error
}

func (s *publicAssetStream) Recv() (*pb.DownloadPublicAssetResponse, error) {
	if len(s.frames) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}
func TestPublicDownloadStreamFailure(t *testing.T) {
	meta := &pb.DownloadPublicAssetResponse{Value: &pb.DownloadPublicAssetResponse_Metadata{Metadata: &pb.Asset{Filename: "safe.txt", MediaType: "text/plain", Bytes: 3}}}
	chunk := &pb.DownloadPublicAssetResponse{Value: &pb.DownloadPublicAssetResponse_Data{Data: []byte("abc")}}
	for _, tc := range []struct {
		name   string
		frames []*pb.DownloadPublicAssetResponse
		err    error
		code   int
		abort  bool
	}{
		{"denied before metadata", nil, status.Error(codes.NotFound, "asset_not_found"), 404, false},
		{"storage error before bytes", []*pb.DownloadPublicAssetResponse{meta}, status.Error(codes.Unavailable, "asset_unavailable"), 503, false},
		{"empty body after metadata", []*pb.DownloadPublicAssetResponse{meta}, nil, 503, false},
		{"success", []*pb.DownloadPublicAssetResponse{meta, chunk}, nil, 200, false},
		{"partial transport failure", []*pb.DownloadPublicAssetResponse{meta, chunk}, status.Error(codes.Unavailable, "asset_unavailable"), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(nil, Options{Origin: "https://worth.example.test", Asset: publicAssetClient{frames: tc.frames, err: tc.err}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "https://worth.example.test/api/tasks/t/entries/e/assets/a/content", nil)
			w := httptest.NewRecorder()
			var got any
			func() { defer func() { got = recover() }(); h.ServeHTTP(w, r) }()
			if (got == http.ErrAbortHandler) != tc.abort || (got != nil && got != http.ErrAbortHandler) {
				t.Fatalf("wrong stream termination: %v", got)
			}
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d", w.Code, tc.code)
			}
			if tc.abort && w.Body.String() != "abc" {
				t.Fatal("mixed file and error JSON")
			}
			if !tc.abort && tc.code == 200 && w.Body.String() != "abc" {
				t.Fatal("incorrect body")
			}
		})
	}
}
