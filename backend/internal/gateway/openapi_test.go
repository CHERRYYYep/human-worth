package gateway

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"go.yaml.in/yaml/v3"
)

// Decode the actual contract, not its text or generated Swagger copy. YAML
// duplicate keys (including paths/operation fields) must fail rather than win.
func TestOpenAPIContract(t *testing.T) {
	file, err := os.Open("../../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var document map[string]any
	if err = decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("expected exactly one YAML document, got %v", err)
	}
	if document["openapi"] != "3.1.1" {
		t.Fatalf("unexpected OpenAPI version: %v", document["openapi"])
	}
	var references func(any)
	references = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, value := range node {
				if key == "$ref" {
					ref, ok := value.(string)
					if !ok || !strings.HasPrefix(ref, "#/") {
						t.Fatalf("expected local reference: %v", value)
					}
					var target any = document
					for _, part := range strings.Split(ref[2:], "/") {
						part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
						mapping, ok := target.(map[string]any)
						if !ok || mapping[part] == nil {
							t.Fatalf("unresolved reference: %s", ref)
						}
						target = mapping[part]
					}
				}
				references(value)
			}
		case []any:
			for _, value := range node {
				references(value)
			}
		}
	}
	references(document)
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths")
	}
	operations := map[string]string{}
	for path, item := range paths {
		for method, value := range item.(map[string]any) {
			if !strings.Contains(" get put post delete options head patch trace ", " "+method+" ") {
				continue
			}
			operation := value.(map[string]any)
			id, ok := operation["operationId"].(string)
			if !ok || id == "" || operations[id] != "" || operation["responses"] == nil {
				t.Fatalf("invalid/duplicate operation %s %s: id=%v previous=%s", method, path, operation["operationId"], operations[id])
			}
			operations[id] = method + " " + path
		}
	}
	for id, route := range map[string]string{
		"resubmitRejectedEntry":      "put /api/me/entries/{entryId}",
		"createAssetUploadSession":   "post /api/me/assets/upload-sessions",
		"getAssetUploadSession":      "get /api/me/upload-sessions/{uploadSessionId}",
		"downloadReviewedEntryAsset": "get /api/tasks/{taskId}/entries/{entryId}/assets/{assetId}/content",
	} {
		if operations[id] != route {
			t.Errorf("%s: got %q, want %q", id, operations[id], route)
		}
	}
	for _, route := range contract.HTTPRoutes() {
		want := strings.ToLower(route.Method) + " " + route.Path
		if got := operations[route.SpecOperationID()]; got != want {
			t.Errorf("catalogued HTTP operation %s: got %q, want %q", route.Name, got, want)
		}
	}
	// New private entry routes must never inherit an anonymous/MCP alternative.
	for _, tc := range []struct {
		path, method string
		write        bool
	}{
		{"/api/tasks/{taskId}/entries", "post", true},
		{"/api/me/entries/{entryId}", "get", false},
		{"/api/me/entries/{entryId}", "put", true},
	} {
		op := paths[tc.path].(map[string]any)[tc.method].(map[string]any)
		security := op["security"].([]any)
		if len(security) != 1 {
			t.Fatal("entry operation has alternative credentials", tc, security)
		}
		schemes := security[0].(map[string]any)
		_, web := schemes["WebSession"]
		_, csrf := schemes["CsrfToken"]
		count := 1
		if tc.write {
			count++
		}
		if !web || csrf != tc.write || len(schemes) != count || op["x-deployment-status"] != "unpublished" {
			t.Fatal("entry auth/deployment contract drift", tc, op)
		}
	}
	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	entry := schemas["EntrySubmission"].(map[string]any)
	required := make(map[string]bool)
	for _, name := range entry["required"].([]any) {
		required[name.(string)] = true
	}
	for _, name := range []string{"createdAt", "updatedAt", "approvedAt", "kind", "origin", "taskId", "authorId", "reviewId", "rejectionReason"} {
		if !required[name] {
			t.Fatal("entry response property must be present", name)
		}
	}
	properties := schemas["EntryInput"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"origin", "author", "owner", "authorId", "ownerId", "state", "actorAssertion"} {
		if _, exists := properties[name]; exists {
			t.Fatal("trusted entry field became client input", name)
		}
	}
}
