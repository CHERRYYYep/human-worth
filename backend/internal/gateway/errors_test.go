package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRPCContextHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		reason string
	}{
		{context.DeadlineExceeded, 504, "deadline_exceeded"},
		{context.Canceled, 503, "request_cancelled"},
		{status.Error(codes.DeadlineExceeded, "private upstream detail"), 504, "deadline_exceeded"},
		{status.Error(codes.Canceled, "private upstream detail"), 503, "request_cancelled"},
		{status.Error(codes.Unavailable, "private upstream detail"), 503, "content_unavailable"},
		{errors.New("private network details"), 503, "content_unavailable"},
		{status.Error(codes.PermissionDenied, "administrator_required"), 403, "administrator_required"},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("wrapper: %w", tc.err)} {
			reason := tc.reason
			if tc.status == 403 && err != tc.err {
				// Existing business-message sanitization also hides wrapper text.
				reason = "content_unavailable"
			}
			w := httptest.NewRecorder()
			rpcErrorFor(w, err, "content_unavailable")
			var body struct {
				Code string `json:"code"`
			}
			if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
				t.Fatal(e)
			}
			if w.Code != tc.status || body.Code != reason {
				t.Errorf("%v: got %d %s", err, w.Code, w.Body)
			}
		}
	}
}
