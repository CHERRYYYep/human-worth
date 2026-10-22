package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type uploadAssertionIdentity struct {
	identitypb.IdentityServiceClient
}

func (uploadAssertionIdentity) ResolvePrincipal(context.Context, *identitypb.ResolvePrincipalRequest, ...grpc.CallOption) (*identitypb.ResolvePrincipalResponse, error) {
	return &identitypb.ResolvePrincipalResponse{ActorAssertion: "test-assertion"}, nil
}

type earlyUploadClient struct {
	assetpb.AssetServiceClient
	stream *earlyUploadStream
}

func (c earlyUploadClient) UploadAsset(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[assetpb.UploadAssetRequest, assetpb.UploadAssetResponse], error) {
	return c.stream, nil
}

type earlyUploadStream struct {
	grpc.ClientStream
	failSend, sends, closes int
	sendError, terminal     error
}

func (s *earlyUploadStream) Send(*assetpb.UploadAssetRequest) error {
	s.sends++
	if s.sends == s.failSend {
		return s.sendError
	}
	return nil
}

func (s *earlyUploadStream) CloseAndRecv() (*assetpb.UploadAssetResponse, error) {
	s.closes++
	return &assetpb.UploadAssetResponse{}, s.terminal
}

func TestUploadAssetEarlyRejection(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failSend       int
		send, terminal error
		httpStatus     int
		reason         string
		closes         int
	}{
		{"metadata_denied", 1, io.EOF, status.Error(codes.PermissionDenied, "upload_forbidden"), 403, "upload_forbidden", 1},
		{"chunk_denied", 2, io.EOF, status.Error(codes.PermissionDenied, "upload_forbidden"), 403, "upload_forbidden", 1},
		{"rate_limit", 2, io.EOF, status.Error(codes.ResourceExhausted, "too_many_requests"), 429, "too_many_requests", 1},
		{"capacity", 2, io.EOF, status.Error(codes.ResourceExhausted, "upload_capacity_exhausted"), 503, "asset_unavailable", 1},
		{"deadline", 2, io.EOF, status.Error(codes.DeadlineExceeded, "private detail"), 504, "deadline_exceeded", 1},
		{"unexpected_success", 2, io.EOF, nil, 503, "asset_unavailable", 1},
		{"send_status", 1, status.Error(codes.Unauthenticated, "unauthenticated"), nil, 401, "unauthenticated", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &earlyUploadStream{failSend: tc.failSend, sendError: tc.send, terminal: tc.terminal}
			h, err := New(uploadAssertionIdentity{}, Options{Origin: "https://worth.example.test", Asset: earlyUploadClient{stream: stream}})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("PUT", "https://worth.example.test/api/me/assets/upload-sessions/upl_test/content", strings.NewReader("abc"))
			r.Header.Set("Content-Range", "bytes 0-2/3")
			r.Header.Set("Content-Type", "application/octet-stream")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var body struct {
				Code string `json:"code"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.httpStatus || body.Code != tc.reason || stream.closes != tc.closes {
				t.Fatalf("got HTTP %d %s, terminal reads %d; want HTTP %d %s, terminal reads %d", w.Code, body.Code, stream.closes, tc.httpStatus, tc.reason, tc.closes)
			}
		})
	}
}
