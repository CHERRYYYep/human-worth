package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDraftHTTPShape(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"title":"t","summary":"","description":"","entries":[]}`, true},
		{`{"title":"t","summary":"","description":"","entries":[],"category":null}`, true},
		{`{"title":"t","summary":"","description":"","entries":[],"authorId":"forged"}`, false},
		{`{"title":"t","summary":"","description":"","entries":[],"actorAssertion":"forged"}`, false},
		{`{"title":"t","summary":"","description":""}`, false},
		{`{"title":"t","summary":"","description":"","entries":null}`, false},
		{`{"title":"t","title":"duplicate","summary":"","description":"","entries":[]}`, false},
		{`{"title":"t","summary":"","description":"","entries":[{"permissions":{"cloud_use":true}}]}`, false},
		{`{"title":"t","summary":"","description":"","entries":[{"description":null}]}`, false},
		{`null`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			_, ok := draftInput(json.RawMessage(tc.body))
			if ok != tc.valid {
				t.Fatalf("got %v want %v", ok, tc.valid)
			}
		})
	}
}

func TestSubmissionListQuery(t *testing.T) {
	request, reason := submissionListRequest("")
	if reason != "" || request.Limit != 20 || request.Cursor != "" || request.Kind != "" || request.State != "" || request.Category != nil {
		t.Fatalf("defaults: request=%+v reason=%q", request, reason)
	}
	request, reason = submissionListRequest("kind=task&state=draft&category=%E7%BB%98%E7%94%BB&limit=100&cursor=")
	if reason != "" || request.Limit != 100 || request.Cursor != "" || request.Category == nil || *request.Category != "绘画" {
		t.Fatalf("explicit query: request=%+v reason=%q", request, reason)
	}
	legacyCategory := strings.Repeat("a", 6400)
	request, reason = submissionListRequest("category=" + legacyCategory)
	if reason != "" || request.Category == nil || *request.Category != legacyCategory {
		t.Fatalf("legacy category query: request=%+v reason=%q", request, reason)
	}
	cases := map[string]string{
		"kind=entry":      "unsupported_submission_filter",
		"kind=":           "unsupported_submission_filter",
		"state=published": "unsupported_submission_filter",
		"state=":          "unsupported_submission_filter",
		"category=":       "invalid_category",
		"category=%00":    "invalid_category",
		"category=" + strings.Repeat("a", maxCategoryBytes+1): "invalid_category",
		"limit=0":                 "invalid_request",
		"limit=101":               "invalid_request",
		"limit=nope":              "invalid_request",
		"limit=1&limit=2":         "invalid_request",
		"cursor=a&cursor=b":       "invalid_request",
		"kind=task&kind=task":     "invalid_request",
		"state=draft&state=draft": "invalid_request",
		"category=a&category=b":   "invalid_request",
		"unknown=value":           "invalid_request",
		"cursor=%ZZ":              "invalid_request",
		"cursor=" + strings.Repeat("a", maxSubmissionCursorLength+1): "invalid_cursor",
	}
	for raw, expected := range cases {
		t.Run(raw, func(t *testing.T) {
			if request, reason := submissionListRequest(raw); request != nil || reason != expected {
				t.Fatalf("query=%q request=%+v reason=%q want=%q", raw, request, reason, expected)
			}
		})
	}
}

func TestRPCResourceExhaustedMapping(t *testing.T) {
	transport := httptest.NewRecorder()
	rpcErrorFor(transport, status.Error(codes.ResourceExhausted, "grpc: trying to send message larger than max"), "content_unavailable")
	if transport.Code != http.StatusServiceUnavailable {
		t.Fatalf("transport exhaustion mapped to %d", transport.Code)
	}
	limited := httptest.NewRecorder()
	rpcErrorFor(limited, status.Error(codes.ResourceExhausted, "too_many_requests"), "content_unavailable")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit mapped to %d", limited.Code)
	}
}
