package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicContentMalformedCredentials(t *testing.T) {
	for _, tc := range []struct {
		cookie, auth string
		status       int
	}{
		{SessionCookie + "=", "", 401},
		{SessionCookie + `="unterminated`, "", 401},
		{SessionCookie + `=invalid\value`, "", 401},
		{SessionCookie, "", 401},
		{SessionCookie + "=; " + SessionCookie + "=valid", "", 400},
		{SessionCookie + `="unterminated; ` + SessionCookie + "=valid", "", 400},
		{SessionCookie + "=", "Bearer supplied", 400},
	} {
		r := httptest.NewRequest("GET", "/api/tasks/tsk_unknown", nil)
		r.Header.Set("Cookie", tc.cookie)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		// These requests must be rejected before any Identity/Content dependency.
		if _, ok := (&Handler{}).publicContentActor(w, r, ""); ok || w.Code != tc.status {
			t.Fatalf("cookie %q: got %d, want %d", tc.cookie, w.Code, tc.status)
		}
	}
}

func TestPublicEntryQuery(t *testing.T) {
	for _, raw := range []string{"", "cursor=", "limit=1", "limit=100&cursor=opaque"} {
		limit, _, err := publicEntryQuery(raw)
		if err != "" || limit < 1 || limit > 100 {
			t.Fatalf("valid %q: %s", raw, err)
		}
	}
	for _, raw := range []string{"limit=0", "limit=101", "limit=", "limit=a", "limit=1&limit=2", "cursor=a&cursor=b", "ownerId=a", "category=x", "%xx"} {
		if _, _, err := publicEntryQuery(raw); err != "invalid_request" {
			t.Fatalf("bad %q: %s", raw, err)
		}
	}
	if _, _, err := publicEntryQuery("cursor=" + strings.Repeat("x", 2049)); err != "invalid_cursor" {
		t.Fatal(err)
	}
}
