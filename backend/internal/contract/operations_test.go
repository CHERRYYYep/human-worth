package contract

import (
	"testing"
	"time"
)

func TestOperationCatalogIsCompleteAndUnique(t *testing.T) {
	if err := ValidateOperations(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range Operations() {
		byID, ok := LookupOperation(operation.ID)
		if !ok || byID != operation {
			t.Errorf("operation lookup by ID %q = %#v, %v; want %#v", operation.ID, byID, ok, operation)
		}
		byMethod, ok := LookupMethod(operation.FullMethod)
		if !ok || byMethod != operation {
			t.Errorf("operation lookup by method %q = %#v, %v; want %#v", operation.FullMethod, byMethod, ok, operation)
		}
	}
}

func TestHTTPRouteCatalogRequiresExactExplicitHandlerCoverage(t *testing.T) {
	handlers := make(map[string]bool)
	for _, route := range HTTPRoutes() {
		handlers[route.Name] = true
	}
	if err := ValidateHTTPHandlers(handlers); err != nil {
		t.Fatalf("complete handler map rejected: %v", err)
	}

	delete(handlers, "requestTaskReview")
	if err := ValidateHTTPHandlers(handlers); err == nil {
		t.Fatal("missing route handler must be rejected")
	}
	handlers["requestTaskReview"] = true
	handlers["unregistered"] = true
	if err := ValidateHTTPHandlers(handlers); err == nil {
		t.Fatal("handler without a route definition must be rejected")
	}
}

func TestRequestTaskReviewMetadataIsDeclaredOnce(t *testing.T) {
	operation, ok := LookupOperation(RequestTaskReview)
	if !ok {
		t.Fatal("RequestTaskReview is missing")
	}
	if operation.FullMethod != "/humanworth.content.v1.ContentService/RequestTaskReview" || operation.Policy.Audience != "content" || !operation.Policy.Write || operation.RPCTimeout.String() != "5s" {
		t.Fatalf("unexpected RequestTaskReview definition: %#v", operation)
	}
	var routeFound bool
	for _, route := range HTTPRoutes() {
		if route.Name == "requestTaskReview" {
			routeFound = true
			if route.Target != RequestTaskReview || route.Method != "POST" || route.Path != "/api/me/tasks/{taskId}/review-requests" || route.Timeout.String() != "5s" {
				t.Fatalf("unexpected RequestTaskReview HTTP route: %#v", route)
			}
		}
	}
	if !routeFound {
		t.Fatal("RequestTaskReview HTTP route is missing")
	}
}

// Public browsing must stay catalogued, read-only and method-scoped across all
// adapters; descriptor and gateway OpenAPI tests check the remaining bindings.
func TestPublicBrowseOperations(t *testing.T) {
	for _, tc := range []struct {
		id            OperationID
		method, route string
		timeout       time.Duration
	}{
		{ReadTask, "/humanworth.content.v1.ContentService/GetTask", "/api/tasks/{taskId}", DefaultRPCTimeout},
		{ListApprovedEntries, "/humanworth.content.v1.ContentService/ListApprovedEntries", "/api/tasks/{taskId}/entries", 5 * time.Second},
	} {
		op, ok := LookupOperation(tc.id)
		if !ok || op.FullMethod != tc.method || !op.HasPolicy || op.Policy != (IdentityPolicy{Audience: "content", Anonymous: true, MCP: true}) {
			t.Fatalf("invalid public operation: %#v", op)
		}
		timeout := op.RPCTimeout
		if timeout == 0 {
			timeout = DefaultRPCTimeout
		}
		if timeout != tc.timeout {
			t.Fatal("public RPC budget drift", op)
		}
		found := false
		for _, route := range HTTPRoutes() {
			if route.Path != tc.route || route.Method != "GET" {
				continue
			}
			found = true
			timeout := route.Timeout
			if timeout == 0 {
				timeout = DefaultHTTPTimeout
			}
			if route.Method != "GET" || route.Target != tc.id || timeout != tc.timeout {
				t.Fatal("public HTTP binding/budget drift", route)
			}
		}
		if !found {
			t.Fatal("missing public route", tc.route)
		}
	}
}

func TestAdditionalEntryOperationContracts(t *testing.T) {
	for _, tc := range []struct {
		id           OperationID
		method       string
		actor, write bool
		timeout      time.Duration
	}{
		{SubmitAdditionalEntry, "/humanworth.content.v1.ContentService/SubmitAdditionalEntry", true, true, 5 * time.Second},
		{ReadOwnEntrySubmission, "/humanworth.content.v1.ContentService/GetMyEntrySubmission", true, false, 2 * time.Second},
		{ResubmitRejectedEntry, "/humanworth.content.v1.ContentService/ResubmitRejectedEntry", true, true, 5 * time.Second},
		{ListOwnEntrySubmissions, "/humanworth.content.v1.ContentService/ListMyEntrySubmissions", true, false, 5 * time.Second},
		{GetEntryReviewSnapshot, "/humanworth.content.v1.ContentService/GetEntryReviewSnapshot", false, false, 2 * time.Second},
		{ApplyEntryReviewDecision, "/humanworth.content.v1.ContentService/ApplyEntryReviewDecision", false, false, 5 * time.Second},
		{CreateEntryReviewCase, "/humanworth.moderation.v1.ModerationService/CreateEntryReviewCase", false, false, 5 * time.Second},
	} {
		op, ok := LookupOperation(tc.id)
		timeout := op.RPCTimeout
		if timeout == 0 {
			timeout = DefaultRPCTimeout
		}
		policy := IdentityPolicy{}
		if tc.actor {
			policy = IdentityPolicy{Audience: "content", Write: tc.write}
		}
		if !ok || op.FullMethod != tc.method || op.HasPolicy != tc.actor || op.Policy != policy || timeout != tc.timeout {
			t.Fatalf("additional-entry operation drift: %#v", op)
		}
	}
	for _, tc := range []struct {
		name, method, path string
		target             OperationID
		timeout            time.Duration
	}{
		{"submitAdditionalEntry", "POST", "/api/tasks/{taskId}/entries", SubmitAdditionalEntry, 5 * time.Second},
		{"getMyEntrySubmission", "GET", "/api/me/entries/{entryId}", ReadOwnEntrySubmission, 2 * time.Second},
		{"resubmitRejectedEntry", "PUT", "/api/me/entries/{entryId}", ResubmitRejectedEntry, 5 * time.Second},
	} {
		found := false
		for _, route := range HTTPRoutes() {
			if route.Name != tc.name {
				continue
			}
			found = true
			timeout := route.Timeout
			if timeout == 0 {
				timeout = DefaultHTTPTimeout
			}
			if route.Method != tc.method || route.Path != tc.path || route.Target != tc.target || timeout != tc.timeout {
				t.Fatalf("additional-entry HTTP drift: %#v", route)
			}
		}
		if !found {
			t.Fatal("missing route", tc.name)
		}
	}
}
