//go:build integration

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/asset"
	"github.com/KDZZZZZZ/human-worth/backend/internal/content"
	"github.com/KDZZZZZZ/human-worth/backend/internal/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// Controlled Open delay, not a replacement authorization/DB fixture.
type delayedPublicStorage struct {
	asset.Storage
	entered chan struct{}
	release chan struct{}
}

func (s delayedPublicStorage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.Storage.Open(ctx, key)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Real PostgreSQL, Identity, HTTP and mTLS. FileStorage is a test adapter;
// separate kind tests exercise the deployed processes and shared S3 storage.
func TestReviewedAssetHTTPAccess(t *testing.T) {
	l := newIdentityLab(t)
	adb := serviceTestDatabase(t, l, "asset", asset.Migrate, "../../../ops/asset-grants.sql")
	cdb := serviceTestDatabase(t, l, "content", content.Migrate, "../../../ops/content/grants.sql")
	cs := &content.Server{DB: cdb, Identity: l.client(t, 0, "content", l.pki)}
	contentConn := func(caller string) *grpc.ClientConn {
		return serviceTestGRPC(t, l, "content", caller, content.Authorization, func(g *grpc.Server) { contentpb.RegisterContentServiceServer(g, cs) })
	}
	reviewer := contentpb.NewContentServiceClient(contentConn("moderation"))
	access := contentpb.NewContentServiceClient(contentConn("asset"))
	gwContent := contentpb.NewContentServiceClient(contentConn("gateway"))
	store := asset.FileStorage{Root: t.TempDir()}
	as := &asset.Server{DB: adb, Identity: l.client(t, 0, "asset", l.pki), Storage: store, Content: access}
	assetConn := func(caller string) *grpc.ClientConn {
		return serviceTestGRPC(t, l, "asset", caller, asset.Authorization, func(g *grpc.Server) { assetpb.RegisterAssetServiceServer(g, as) })
	}
	cs.Asset = assetpb.NewAssetServiceClient(assetConn("content"))
	public := assetpb.NewAssetServiceClient(assetConn("gateway"))
	for i := range l.web {
		h, e := gateway.New(l.clients[i], gateway.Options{Origin: l.origin, Asset: public, Content: gwContent, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		must(t, e)
		l.web[i].Config.Handler = h
	}
	alice, me := l.login(t, "reviewed-asset-alice", nil)
	bob, _ := l.login(t, "reviewed-asset-bob", nil)
	headers := map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/json"}
	data := []byte("reviewed bytes are not a permanent public grant\n")
	var seq int
	upload := func() string {
		t.Helper()
		seq++
		headers["Idempotency-Key"] = fmt.Sprintf("public-upload-key-%04d", seq)
		body := fmt.Sprintf(`{"filename":"reviewed.txt","mediaType":"text/plain","expectedBytes":%d,"sha256":"%x"}`, len(data), sha256.Sum256(data))
		r := l.request(t, 0, "POST", "/api/me/assets/upload-sessions", []*http.Cookie{alice}, body, headers)
		httpStatus(t, r, 201)
		var c struct {
			Asset         struct{ ID string }
			UploadSession struct{ ID string }
		}
		readJSON(t, r, &c)
		h := map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))}
		httpStatus(t, l.request(t, 1, "PUT", "/api/me/assets/upload-sessions/"+c.UploadSession.ID+"/content", []*http.Cookie{alice}, string(data), h), 200)
		httpStatus(t, l.request(t, 0, "POST", "/api/me/assets/"+c.Asset.ID+"/complete", []*http.Cookie{alice}, `{"uploadSessionId":"`+c.UploadSession.ID+`"}`, headers), 200)
		return c.Asset.ID
	}
	assetID, unbound := upload(), upload()
	draft := func(id string) string {
		return `{"title":"review snapshot","summary":"","description":"","entries":[{"side":"human","title":"human","source":"self","artifacts":[{"kind":"file","assetId":"` + id + `"}],"permissions":{"cloudUse":false}},{"side":"agent","title":"agent","source":"model","artifacts":[{"kind":"link","url":"https://example.com/agent"}],"permissions":{"cloudUse":false},"agentConfiguration":{"model":"test"}}]}`
	}
	headers["Idempotency-Key"] = "reviewed-content-key01"
	r := l.request(t, 0, "POST", "/api/tasks", []*http.Cookie{alice}, draft(assetID), headers)
	httpStatus(t, r, 201)
	var task draftResult
	readJSON(t, r, &task)
	request := &contentpb.ApplyAssetReviewRequest{DecisionId: "review-approve-key01", TaskId: task.ID, SubmissionRevision: 1, ExpectedRevision: 0, Decision: &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_PUBLISHED}}
	_, err := gwContent.ApplyAssetReview(t.Context(), request)
	grpcCode(t, err, codes.PermissionDenied)
	_, err = gwContent.CheckAssetAccess(t.Context(), &contentpb.CheckAssetAccessRequest{})
	grpcCode(t, err, codes.PermissionDenied)
	_, err = access.ApplyAssetReview(t.Context(), request)
	grpcCode(t, err, codes.PermissionDenied)
	beforePath := "/api/tasks/" + task.ID + "/entries/ent_00000000000000000000000000000000/assets/" + assetID + "/content"
	httpStatus(t, l.request(t, 0, "GET", beforePath, nil, "", nil), 404)
	wrongSource := proto.Clone(request).(*contentpb.ApplyAssetReviewRequest)
	wrongSource.DecisionId = "review-wrong-source1"
	wrongSource.SubmissionRevision = 99
	_, err = reviewer.ApplyAssetReview(t.Context(), wrongSource)
	grpcCode(t, err, codes.Aborted)
	receipt, err := reviewer.ApplyAssetReview(t.Context(), request)
	must(t, err)
	if receipt.Revision != 1 || len(receipt.Entries) != 2 {
		t.Fatal("invalid review receipt")
	}
	entryID := receipt.Entries[0].Id
	ref := &assetpb.PublicAssetReference{TaskId: task.ID, EntryId: entryID, AssetId: assetID}
	path := func(ref *assetpb.PublicAssetReference) string {
		return "/api/tasks/" + ref.TaskId + "/entries/" + ref.EntryId + "/assets/" + ref.AssetId + "/content"
	}
	get := func(ref *assetpb.PublicAssetReference, cookie *http.Cookie, want int) {
		t.Helper()
		var cookies []*http.Cookie
		if cookie != nil {
			cookies = []*http.Cookie{cookie}
		}
		r := l.request(t, 1, "GET", path(ref), cookies, "", nil)
		httpStatus(t, r, want)
		if want == 200 {
			if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Content-Disposition") == "" {
				t.Fatal("unsafe public response headers")
			}
			actual, e := io.ReadAll(r.Body)
			r.Body.Close()
			must(t, e)
			if !bytes.Equal(actual, data) {
				t.Fatal("public HTTP bytes differ")
			}
		}
	}
	get(ref, nil, 200)
	get(ref, bob, 200)
	for _, change := range []func(*assetpb.PublicAssetReference){func(r *assetpb.PublicAssetReference) { r.AssetId = unbound }, func(r *assetpb.PublicAssetReference) { r.EntryId = receipt.Entries[1].Id }, func(r *assetpb.PublicAssetReference) { r.TaskId = "tsk_00000000000000000000000000000000" }} {
		other := proto.Clone(ref).(*assetpb.PublicAssetReference)
		change(other)
		get(other, alice, 404)
	}
	httpStatus(t, l.request(t, 0, "GET", path(ref)+"?public=true&ownerId=forged", nil, "", nil), 400)
	// Anonymous public access must not turn the owner-only management endpoint public.
	httpStatus(t, l.request(t, 0, "GET", "/api/me/assets/"+assetID+"/content", []*http.Cookie{bob}, "", nil), 404)
	httpStatus(t, l.request(t, 0, "GET", "/api/me/assets/"+assetID+"/content", nil, "", nil), 401)
	// Changes to an editable draft are not automatically part of a reviewed snapshot.
	httpStatus(t, l.request(t, 0, "PUT", "/api/me/tasks/"+task.ID, []*http.Cookie{alice}, `{"expectedRevision":1,"content":`+draft(unbound)+`}`, headers), 200)
	get(ref, nil, 200)
	swapped := proto.Clone(ref).(*assetpb.PublicAssetReference)
	swapped.AssetId = unbound
	get(swapped, nil, 404)
	revoke := &contentpb.ApplyAssetReviewRequest{DecisionId: "review-revoke-key0001", TaskId: task.ID, SubmissionRevision: 1, ExpectedRevision: 1, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: entryID, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}}
	// Concurrent identical decisions return the same receipt and advance once.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	receipts := make(chan *contentpb.ApplyAssetReviewResponse, 2)
	for range 2 {
		wg.Go(func() { r, e := reviewer.ApplyAssetReview(t.Context(), revoke); errs <- e; receipts <- r })
	}
	wg.Wait()
	for range 2 {
		must(t, <-errs)
		if (<-receipts).GetRevision() != 2 {
			t.Fatal("duplicate decision advanced revision")
		}
	}
	get(ref, nil, 404)
	get(ref, alice, 404)
	replay, e := reviewer.ApplyAssetReview(t.Context(), request)
	must(t, e)
	if !proto.Equal(replay, receipt) {
		t.Fatal("receipt is not stable")
	}
	get(ref, nil, 404)
	changed := proto.Clone(revoke).(*contentpb.ApplyAssetReviewRequest)
	changed.ExpectedRevision = 2
	_, err = reviewer.ApplyAssetReview(t.Context(), changed)
	grpcCode(t, err, codes.AlreadyExists)
	stale := proto.Clone(request).(*contentpb.ApplyAssetReviewRequest)
	stale.DecisionId = "review-stale-key00001"
	_, err = reviewer.ApplyAssetReview(t.Context(), stale)
	grpcCode(t, err, codes.Aborted)
	restore := proto.Clone(revoke).(*contentpb.ApplyAssetReviewRequest)
	restore.DecisionId = "review-restore-key001"
	restore.ExpectedRevision = 2
	restore.GetEntryReview().State = contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVED
	_, err = reviewer.ApplyAssetReview(t.Context(), restore)
	must(t, err)
	get(ref, nil, 200)
	takeDown := proto.Clone(request).(*contentpb.ApplyAssetReviewRequest)
	takeDown.DecisionId = "review-takedown-key01"
	takeDown.ExpectedRevision = 3
	takeDown.Decision = &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_TAKEN_DOWN}
	_, err = reviewer.ApplyAssetReview(t.Context(), takeDown)
	must(t, err)
	get(ref, nil, 404)
	// Old approval replays never reopen the task after a tightening decision.
	_, err = reviewer.ApplyAssetReview(t.Context(), request)
	must(t, err)
	get(ref, nil, 404)
	// A restricted runtime cannot change immutable review identity/snapshot or rewrite receipts.
	for _, q := range []string{"UPDATE content.reviewed_tasks SET snapshot='{}' WHERE false", "UPDATE content.reviewed_tasks SET source_revision=99 WHERE false", "UPDATE content.reviewed_entries SET position=9 WHERE false", "UPDATE content.asset_review_receipts SET response='{}' WHERE false", "SELECT * FROM asset.assets", "CREATE TABLE content.denied(id int)"} {
		if _, e = cdb.Exec(t.Context(), q); e == nil {
			t.Fatalf("runtime escaped grants: %s", q)
		}
	}
	// Missing Content configuration is not authorization success, even for owner.
	old := as.Content
	as.Content = nil
	get(ref, alice, 503)
	as.Content = old
	// Bring task back, then demonstrate both non-ready and unavailable storage deny.
	reopen := proto.Clone(request).(*contentpb.ApplyAssetReviewRequest)
	reopen.DecisionId = "review-reopen-key0001"
	reopen.ExpectedRevision = 4
	_, err = reviewer.ApplyAssetReview(t.Context(), reopen)
	must(t, err)
	_, err = l.db.Exec(t.Context(), `UPDATE asset.assets SET state='failed' WHERE id=$1`, assetID)
	must(t, err)
	get(ref, nil, 404)
	_, err = l.db.Exec(t.Context(), `UPDATE asset.assets SET state='ready' WHERE id=$1`, assetID)
	must(t, err)
	as.Storage = nil
	get(ref, nil, 503)
	as.Storage = store
	// Reconstructed service dependencies keep the committed access decision.
	as.Content = contentpb.NewContentServiceClient(contentConn("asset"))
	get(ref, nil, 200)
	// A tightening committed while object Open is blocked must win before any
	// metadata/download bytes are sent (detects removal of the second check).
	delayed := delayedPublicStorage{Storage: store, entered: make(chan struct{}), release: make(chan struct{})}
	as.Storage = delayed
	req, e := http.NewRequestWithContext(t.Context(), "GET", l.web[1].URL+path(ref), nil)
	must(t, e)
	req.Host = strings.TrimPrefix(l.origin, "https://")
	type downloadResult struct {
		response *http.Response
		err      error
	}
	done := make(chan downloadResult, 1)
	go func() { r, e := l.web[1].Client().Do(req); done <- downloadResult{r, e} }()
	select {
	case <-delayed.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("public read did not reach controlled Open")
	}
	finalRevoke := proto.Clone(revoke).(*contentpb.ApplyAssetReviewRequest)
	finalRevoke.DecisionId = "review-delayed-revoke"
	finalRevoke.ExpectedRevision = 5
	_, err = reviewer.ApplyAssetReview(t.Context(), finalRevoke)
	close(delayed.release)
	must(t, err)
	select {
	case result := <-done:
		must(t, result.err)
		httpStatus(t, result.response, 404)
		result.response.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("controlled Open did not finish")
	}
	as.Storage = store
}
