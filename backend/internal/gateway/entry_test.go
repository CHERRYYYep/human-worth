package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	moderationpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const entryBody = `{"side":"human","title":"work","source":"source","artifacts":[{"kind":"link","url":"https://example.test/work"}],"permissions":{"cloudUse":false}}`

type entryHTTPIdentity struct {
	identitypb.IdentityServiceClient
	requests   []*identitypb.ResolvePrincipalRequest
	failure    error
	denyMethod string
}

func (c *entryHTTPIdentity) ResolvePrincipal(_ context.Context, r *identitypb.ResolvePrincipalRequest, _ ...grpc.CallOption) (*identitypb.ResolvePrincipalResponse, error) {
	c.requests = append(c.requests, r)
	if c.failure != nil && (c.denyMethod == "" || c.denyMethod == r.FullMethod) {
		return nil, c.failure
	}
	return &identitypb.ResolvePrincipalResponse{ActorAssertion: "assertion:" + r.FullMethod}, nil
}

type entryHTTPAsset struct{ assetpb.AssetServiceClient }

type entryHTTPContent struct {
	pb.ContentServiceClient
	create     *pb.SubmitAdditionalEntryRequest
	get        *pb.GetMyEntrySubmissionRequest
	replace    *pb.ResubmitRejectedEntryRequest
	list       *pb.ListMyEntrySubmissionsRequest
	tasks      *pb.ListMySubmissionsRequest
	submission *pb.EntrySubmission
	nextCursor string
	failure    error
	calls      int
	check      func(context.Context)
}

func (c *entryHTTPContent) called(ctx context.Context) {
	c.calls++
	if c.check != nil {
		c.check(ctx)
	}
}
func (c *entryHTTPContent) SubmitAdditionalEntry(ctx context.Context, r *pb.SubmitAdditionalEntryRequest, _ ...grpc.CallOption) (*pb.SubmitAdditionalEntryResponse, error) {
	c.called(ctx)
	c.create = r
	return &pb.SubmitAdditionalEntryResponse{Submission: c.submission}, c.failure
}
func (c *entryHTTPContent) GetMyEntrySubmission(ctx context.Context, r *pb.GetMyEntrySubmissionRequest, _ ...grpc.CallOption) (*pb.GetMyEntrySubmissionResponse, error) {
	c.called(ctx)
	c.get = r
	return &pb.GetMyEntrySubmissionResponse{Submission: c.submission}, c.failure
}
func (c *entryHTTPContent) ResubmitRejectedEntry(ctx context.Context, r *pb.ResubmitRejectedEntryRequest, _ ...grpc.CallOption) (*pb.ResubmitRejectedEntryResponse, error) {
	c.called(ctx)
	c.replace = r
	return &pb.ResubmitRejectedEntryResponse{Submission: c.submission}, c.failure
}
func (c *entryHTTPContent) ListMyEntrySubmissions(ctx context.Context, r *pb.ListMyEntrySubmissionsRequest, _ ...grpc.CallOption) (*pb.ListMyEntrySubmissionsResponse, error) {
	c.called(ctx)
	c.list = r
	out := &pb.ListMyEntrySubmissionsResponse{NextCursor: c.nextCursor}
	if c.submission != nil {
		out.Items = []*pb.EntrySubmission{c.submission}
	}
	return out, c.failure
}
func (c *entryHTTPContent) ListMySubmissions(ctx context.Context, r *pb.ListMySubmissionsRequest, _ ...grpc.CallOption) (*pb.ListMySubmissionsResponse, error) {
	c.called(ctx)
	c.tasks = r
	return &pb.ListMySubmissionsResponse{}, c.failure
}

func entryFixture(t *testing.T) *pb.EntrySubmission {
	t.Helper()
	input, ok := entryInput(json.RawMessage(entryBody))
	if !ok {
		t.Fatal("invalid fixture")
	}
	created := timestamppb.New(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	return &pb.EntrySubmission{Id: "ent_test", TaskId: "tsk_test", AuthorId: "account_test", Revision: 1, Origin: "additional", State: "pending_review", Content: input, ReviewId: "rev_test", CreatedAt: created, UpdatedAt: created}
}

func entryHandler(t *testing.T, identity *entryHTTPIdentity, content *entryHTTPContent, asset bool) http.Handler {
	t.Helper()
	options := Options{Origin: "https://worth.example.test", Content: content, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if asset {
		options.Asset = &entryHTTPAsset{}
	}
	h, err := New(identity, options)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func entryRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://worth.example.test"+path, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "web-session"})
	r.Header.Set("Origin", "https://worth.example.test")
	r.Header.Set("X-CSRF-Token", "csrf")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "entry-create-key-1234")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdditionalEntryHTTPBinding(t *testing.T) {
	file := strings.Replace(entryBody, `"kind":"link","url":"https://example.test/work"`, `"kind":"file","assetId":"ast_test"`, 1)
	for _, tc := range []struct {
		method, path, body, rpc string
		code                    int
		file                    bool
	}{
		{"POST", "/api/tasks/tsk_test/entries", entryBody, pb.ContentService_SubmitAdditionalEntry_FullMethodName, 201, false},
		{"POST", "/api/tasks/tsk_test/entries", file, pb.ContentService_SubmitAdditionalEntry_FullMethodName, 201, true},
		{"GET", "/api/me/entries/ent_test", "", pb.ContentService_GetMyEntrySubmission_FullMethodName, 200, false},
		{"PUT", "/api/me/entries/ent_test", `{"expectedRevision":2,"content":` + file + `}`, pb.ContentService_ResubmitRejectedEntry_FullMethodName, 200, true},
	} {
		t.Run(tc.method+tc.body, func(t *testing.T) {
			identity := &entryHTTPIdentity{}
			content := &entryHTTPContent{submission: entryFixture(t)}
			content.check = func(ctx context.Context) {
				count := 1
				if tc.file {
					count = 2
				}
				if len(identity.requests) != count {
					t.Fatal("business RPC entered before all assertions", identity.requests)
				}
				deadline, ok := ctx.Deadline()
				budget := 5 * time.Second
				if tc.method == "GET" {
					budget = 2 * time.Second
				}
				if remaining := time.Until(deadline); !ok || remaining <= budget-time.Second || remaining > budget {
					t.Fatal("wrong HTTP deadline", remaining)
				}
			}
			w := entryRequest(entryHandler(t, identity, content, tc.file), tc.method, tc.path, tc.body)
			if w.Code != tc.code {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private response cacheable")
			}
			for i, req := range identity.requests {
				method, audience := tc.rpc, "content"
				if i == 1 {
					method, audience = assetpb.AssetService_CheckAssetOwnership_FullMethodName, "asset"
				}
				if req.FullMethod != method || req.Audience != audience || req.GetSessionCookie() != "web-session" || req.Origin != "https://worth.example.test" || req.CsrfToken != "csrf" {
					t.Fatal("wrong assertion scope/credential", req)
				}
			}
			actor, asset := "assertion:"+tc.rpc, ""
			if tc.file {
				asset = "assertion:" + assetpb.AssetService_CheckAssetOwnership_FullMethodName
			}
			switch tc.method {
			case "POST":
				if r := content.create; r == nil || r.ActorAssertion != actor || r.AssetActorAssertion != asset || r.TaskId != "tsk_test" || r.IdempotencyKey != "entry-create-key-1234" || r.Content.GetPermissions().GetCloudUse() {
					t.Fatal("incorrect create forwarding", r)
				}
			case "GET":
				if r := content.get; r == nil || r.ActorAssertion != actor || r.EntryId != "ent_test" {
					t.Fatal("incorrect read forwarding", r)
				}
			case "PUT":
				if r := content.replace; r == nil || r.ActorAssertion != actor || r.AssetActorAssertion != asset || r.EntryId != "ent_test" || r.ExpectedRevision != 2 {
					t.Fatal("incorrect resubmission forwarding", r)
				}
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["kind"] != "entry" || body["origin"] != "additional" || body["createdAt"] != "2026-10-23T00:00:00Z" || body["updatedAt"] != "2026-10-23T00:00:00Z" || body["approvedAt"] != nil || body["rejectionReason"] != nil || body["reviewId"] != "rev_test" || body["revision"] != float64(1) {
				t.Fatal("incorrect HTTP projection", body)
			}
		})
	}
}

func TestAdditionalEntryRejectsInvalidHTTPFields(t *testing.T) {
	for _, field := range []string{"origin", "author", "owner", "authorId", "ownerId", "state", "reviewId", "actorAssertion", "assetActorAssertion", "taskId"} {
		body := strings.TrimSuffix(entryBody, "}") + `,"` + field + `":"forged"}`
		if _, ok := entryInput(json.RawMessage(body)); ok {
			t.Fatal("accepted client identity/state", field)
		}
	}
	for _, body := range []string{
		`null`, `{}`, `[]`,
		strings.Replace(entryBody, `"side":"human"`, `"side":1`, 1),
		strings.Replace(entryBody, `"side":"human"`, `"side":"agent"`, 1),
		strings.Replace(entryBody, `"title":"work"`, `"title":"work","title":"duplicate"`, 1),
		strings.Replace(entryBody, `"title":"work"`, `"Title":"work"`, 1),
		strings.Replace(entryBody, `"source":"source",`, ``, 1),
		strings.Replace(entryBody, `"cloudUse":false`, `"cloud_use":false`, 1),
		strings.Replace(entryBody, `"cloudUse":false`, `"cloudUse":null`, 1),
		strings.Replace(entryBody, `"cloudUse":false`, `"cloudUse":false,"cloudUse":true`, 1),
		strings.Replace(entryBody, `"cloudUse":false`, `"cloudUse":"false"`, 1),
		strings.Replace(entryBody, `"cloudUse":false`, ``, 1),
		strings.Replace(entryBody, `"permissions":{"cloudUse":false}`, `"permissions":null`, 1),
		strings.Replace(entryBody, `"kind":"link"`, `"kind":"file"`, 1),
		strings.Replace(entryBody, `"kind":"link"`, `"kind":"link","assetId":"ast_test"`, 1),
		strings.TrimSuffix(entryBody, "}") + `,"description":null}`,
		strings.TrimSuffix(entryBody, "}") + `,"agentConfiguration":{"model":"m"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			content := &entryHTTPContent{}
			w := entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), "POST", "/api/tasks/tsk_test/entries", body)
			if w.Code != 400 || content.calls != 0 {
				t.Fatalf("invalid body reached Content: %d %s", w.Code, w.Body)
			}
		})
	}
	// Model parameters are metadata, not HTTP field aliases or identity claims.
	agent := strings.Replace(entryBody, `"side":"human"`, `"side":"agent"`, 1)
	agent = strings.TrimSuffix(agent, "}") + `,"agentConfiguration":{"model":"m","parameters":{"author":"label","cloud_use":null,"nested":[1,true,null]}}}`
	if _, ok := entryInput(json.RawMessage(agent)); !ok {
		t.Fatal("valid model metadata rejected")
	}
}

func TestEntryResubmissionStrictEnvelope(t *testing.T) {
	valid := `{"expectedRevision":1,"content":` + entryBody + `}`
	for _, body := range []string{
		`null`, `{}`, `{"content":` + entryBody + `}`,
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":null`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":"1"`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":1.5`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":0`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":9223372036854775808`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expectedRevision":1,"expectedRevision":2`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"ExpectedRevision":1`, 1),
		strings.Replace(valid, `"expectedRevision":1`, `"expected_revision":1`, 1),
		strings.Replace(valid, `"content":`, `"content":null,"content":`, 1),
		strings.TrimSuffix(valid, "}") + `,"owner":"forged"}`,
		strings.TrimSuffix(valid, "}") + `,"origin":"additional"}`,
	} {
		t.Run(body, func(t *testing.T) {
			content := &entryHTTPContent{}
			w := entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), "PUT", "/api/me/entries/ent_test", body)
			if w.Code != 400 || content.calls != 0 {
				t.Fatalf("invalid envelope reached Content: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSubmissionListDispatchAndScope(t *testing.T) {
	for _, query := range []string{"", "kind=task", "kind=task&state=pending_review", "kind=entry", "kind=entry&state=approved&category=%E7%BB%98%E7%94%BB&limit=3&cursor=opaque"} {
		t.Run(query, func(t *testing.T) {
			identity, content := &entryHTTPIdentity{}, &entryHTTPContent{}
			w := entryRequest(entryHandler(t, identity, content, false), "GET", "/api/me/submissions?"+query, "")
			if w.Code != 200 || content.calls != 1 || len(identity.requests) != 1 {
				t.Fatalf("dispatch failed: %d %s", w.Code, w.Body)
			}
			method := pb.ContentService_ListMySubmissions_FullMethodName
			if strings.Contains(query, "kind=entry") {
				method = pb.ContentService_ListMyEntrySubmissions_FullMethodName
				if content.list == nil || content.tasks != nil || content.list.ActorAssertion != "assertion:"+method {
					t.Fatal("entry used task RPC/assertion", content.list)
				}
				if query == "kind=entry" && (content.list.State != "" || content.list.Limit != 20 || content.list.Category != nil || content.list.Cursor != "") {
					t.Fatal("entry default must select all states", content.list)
				}
				if strings.Contains(query, "state=") && (content.list.State != "approved" || content.list.GetCategory() != "绘画" || content.list.Limit != 3 || content.list.Cursor != "opaque") {
					t.Fatal("entry filters changed", content.list)
				}
			} else {
				if content.tasks == nil || content.list != nil || content.tasks.ActorAssertion != "assertion:"+method {
					t.Fatal("task dispatch changed")
				}
				if query == "" && (content.tasks.Kind != "" || content.tasks.State != "" || content.tasks.Limit != 20) {
					t.Fatal("legacy task defaults changed", content.tasks)
				}
			}
			if identity.requests[0].FullMethod != method || identity.requests[0].Audience != "content" {
				t.Fatal("wrong list assertion scope", identity.requests)
			}
			if strings.TrimSpace(w.Body.String()) != `{"items":[],"nextCursor":null}` {
				t.Fatal("empty pagination contract changed", w.Body)
			}
		})
	}
	content := &entryHTTPContent{submission: entryFixture(t), nextCursor: "next-page"}
	w := entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), "GET", "/api/me/submissions?kind=entry", "")
	var page struct {
		Items      []map[string]any
		NextCursor string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Items) != 1 || page.Items[0]["kind"] != "entry" || page.NextCursor != "next-page" {
		t.Fatal("entry page projection", w.Body)
	}
}

func TestEntryHTTPFailuresStayClosed(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   int
		reason string
	}{
		{status.Error(codes.NotFound, "entry_not_found"), 404, "entry_not_found"},
		{status.Error(codes.Aborted, "revision_conflict"), 409, "revision_conflict"},
		{status.Error(codes.AlreadyExists, "idempotency_key_conflict"), 409, "idempotency_key_conflict"},
		{status.Error(codes.FailedPrecondition, "asset_not_ready"), 409, "asset_not_ready"},
		{context.DeadlineExceeded, 504, "deadline_exceeded"},
		{context.Canceled, 503, "request_cancelled"},
		{status.Error(codes.Unavailable, "private database detail"), 503, "content_unavailable"},
	} {
		content := &entryHTTPContent{failure: tc.err}
		w := entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), "POST", "/api/tasks/tsk_test/entries", entryBody)
		var problem struct{ Code string }
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || w.Code != tc.code || problem.Code != tc.reason {
			t.Fatalf("wrong failure: %d %s", w.Code, w.Body)
		}
	}
	for _, method := range []string{"POST", "PUT", "GET"} {
		content := &entryHTTPContent{}
		path, body := "/api/me/entries/ent_test", `{"expectedRevision":1,"content":`+entryBody+`}`
		if method == "POST" {
			path, body = "/api/tasks/tsk_test/entries", entryBody
		}
		w := entryRequest(entryHandler(t, &entryHTTPIdentity{failure: status.Error(codes.PermissionDenied, "csrf_invalid")}, content, false), method, path, body)
		if w.Code != 403 || content.calls != 0 {
			t.Fatal("Identity denial reached Content", w.Code)
		}
		w = entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), method, path+"?owner=forged", body)
		if w.Code != 400 || content.calls != 0 {
			t.Fatal("unknown query reached Content", w.Code)
		}
	}
	file := strings.Replace(entryBody, `"kind":"link","url":"https://example.test/work"`, `"kind":"file","assetId":"ast_test"`, 1)
	content := &entryHTTPContent{}
	w := entryRequest(entryHandler(t, &entryHTTPIdentity{}, content, false), "POST", "/api/tasks/tsk_test/entries", file)
	if w.Code != 503 || content.calls != 0 {
		t.Fatal("missing Asset was bypassed", w.Code)
	}
	identity := &entryHTTPIdentity{failure: status.Error(codes.PermissionDenied, "credential_purpose_forbidden"), denyMethod: assetpb.AssetService_CheckAssetOwnership_FullMethodName}
	w = entryRequest(entryHandler(t, identity, content, true), "POST", "/api/tasks/tsk_test/entries", file)
	if w.Code != 403 || content.calls != 0 || len(identity.requests) != 2 {
		t.Fatal("Asset assertion failure reached Content", w.Code)
	}
}

func TestEntryHTTPEnvelopeAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		alter      func(*http.Request)
	}{
		{"missing_key", entryBody, 400, func(r *http.Request) { r.Header.Del("Idempotency-Key") }},
		{"duplicate_key", entryBody, 400, func(r *http.Request) { r.Header.Add("Idempotency-Key", "another-create-key") }},
		{"invalid_key", entryBody, 400, func(r *http.Request) { r.Header.Set("Idempotency-Key", "bad key") }},
		{"media", entryBody, 415, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"two_objects", entryBody + `{}`, 400, nil},
		{"large_body", strings.Replace(entryBody, "work", strings.Repeat("w", 17000), 1), 413, nil},
		{"large_suffix", entryBody + strings.Repeat(" ", 17000), 413, nil},
		{"mixed_credentials", entryBody, 400, func(r *http.Request) { r.Header.Set("Authorization", "Bearer supplied") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, identity := &entryHTTPContent{}, &entryHTTPIdentity{}
			r := httptest.NewRequest("POST", "https://worth.example.test/api/tasks/tsk_test/entries", strings.NewReader(tc.body))
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "web-session"})
			r.Header.Set("Idempotency-Key", "entry-create-key-1234")
			r.Header.Set("Content-Type", "application/json")
			if tc.alter != nil {
				tc.alter(r)
			}
			w := httptest.NewRecorder()
			entryHandler(t, identity, content, false).ServeHTTP(w, r)
			if w.Code != tc.code || content.calls != 0 {
				t.Fatalf("envelope reached Content: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestEntryReviewHTTPProjection(t *testing.T) {
	sub := entryFixture(t)
	for _, applied := range []bool{false, true} {
		r := &moderationpb.Review{Id: "rev_test", EntrySnapshot: &pb.EntryReviewSnapshot{ReviewId: "rev_test", Submission: sub, CreatedAt: sub.CreatedAt}, Status: "pending", CreatedAt: sub.CreatedAt, DeliveryState: moderationpb.DeliveryState_DELIVERY_STATE_PENDING}
		code := 202
		var revision any
		if applied {
			r.DeliveryState = moderationpb.DeliveryState_DELIVERY_STATE_APPLIED
			r.EntryReceipt = &pb.ApplyEntryReviewDecisionResponse{DecisionId: "decision", ReviewId: r.Id, EntryId: sub.Id, Revision: 2, State: "approved"}
			r.Status = "approved"
			code, revision = 200, float64(2)
		}
		w := httptest.NewRecorder()
		reviewResponse(w, r)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != code || body["applicationRevision"] != revision {
			t.Fatal("wrong entry receipt projection", w.Code, w.Body)
		}
		if body["submission"].(map[string]any)["kind"] != "entry" || len(body["entryIds"].([]any)) != 1 || body["entryIds"].([]any)[0] != sub.Id {
			t.Fatal("entry review rendered as task", body)
		}
		if body["entrySnapshot"] != nil || body["entryReceipt"] != nil {
			t.Fatal("internal snapshot leaked")
		}
	}
	task := &moderationpb.Review{Snapshot: &pb.ReviewSnapshot{Submission: &pb.TaskSubmission{Content: &pb.TaskDraftInput{}}}, Receipt: &pb.ApplyReviewDecisionResponse{Revision: 3}}
	value, err := reviewJSON(task)
	if err != nil || value["submission"].(map[string]any)["kind"] != "task" || value["applicationRevision"] != int64(3) {
		t.Fatal("legacy task review changed", value, err)
	}
	for _, invalid := range []*moderationpb.Review{nil, {}, {Snapshot: task.Snapshot, EntrySnapshot: &pb.EntryReviewSnapshot{Submission: sub}}, {EntrySnapshot: &pb.EntryReviewSnapshot{Submission: sub}, Receipt: task.Receipt}, {Snapshot: task.Snapshot, EntryReceipt: &pb.ApplyEntryReviewDecisionResponse{}}} {
		if _, err := reviewJSON(invalid); err == nil {
			t.Fatal("ambiguous/mismatched review accepted", invalid)
		}
	}
}
