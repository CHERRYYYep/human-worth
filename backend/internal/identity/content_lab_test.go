//go:build lab

package identity

import (
	"encoding/json"
	"strings"
	"testing"
)

// Synthetic credentials exist only in validate_modules.py's disposable namespace.
func TestKindContentDeployment(t *testing.T) {
	l := newKindLab(t)
	u := l.user()
	account := u.account
	request := func(method, path, body string, authenticated bool, expected int) []byte {
		t.Helper()
		actor := kindUser{}
		if authenticated {
			actor = u
		}
		return l.request(l.client, actor, method, path, body, map[string]string{"Idempotency-Key": "deployment-check-" + account}, expected)
	}
	request("GET", "/api/me", "", true, 200)
	body := `{"title":"deployment check","summary":"temporary","description":"isolated","entries":[]}`
	request("POST", "/api/tasks", body, false, 401)
	data := request("POST", "/api/tasks", body, true, 201)
	var draft struct {
		ID       string `json:"id"`
		AuthorID string `json:"authorId"`
		Revision int    `json:"revision"`
	}
	if json.Unmarshal(data, &draft) != nil || draft.ID == "" || draft.AuthorID != account || draft.Revision != 1 {
		t.Fatal("invalid deployed draft result")
	}
	path := "/api/me/tasks/" + draft.ID
	request("GET", path, "", false, 401)
	request("GET", path, "", true, 200)
	update := `{"expectedRevision":1,"content":{"title":"updated deployment check","summary":"","description":"","entries":[]}}`
	request("PUT", path, update, true, 200)
	request("PUT", path, update, true, 409)
	pods := strings.Fields(string(l.kube("", "get", "pods", "-l", "app=content", "-o", "jsonpath={.items[*].metadata.name}")))
	if len(pods) != 2 {
		t.Fatalf("expected two Content replicas, got %d", len(pods))
	}
	l.kube("", "delete", "pod", pods[0], "--wait=true")
	l.kube("", "rollout", "status", "deployment/content", "--timeout=120s")
	data = request("GET", path, "", true, 200)
	if json.Unmarshal(data, &draft) != nil || draft.Revision != 2 || !strings.Contains(string(data), "updated deployment check") {
		t.Fatal("draft did not survive Content restart")
	}
	request("POST", "/api/auth/logout", "", true, 204)
	request("GET", path, "", true, 401)
	t.Log("HTTPS -> gateway -> Identity -> Content -> PostgreSQL: create/read/replace, conflict, restart persistence and revocation passed")
}
