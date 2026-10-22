package contract

import "testing"

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
