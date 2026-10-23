//go:build integration

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	moderationpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/asset"
	"github.com/KDZZZZZZ/human-worth/backend/internal/content"
	"github.com/KDZZZZZZ/human-worth/backend/internal/gateway"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application/dto"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/repo/postgres"
	identitygrpc "github.com/KDZZZZZZ/human-worth/backend/internal/identity/transport/grpc"
	"github.com/KDZZZZZZ/human-worth/backend/internal/moderation"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type publicBrowsePage struct {
	Items []struct {
		ID, TaskID, Side, Title, Description, Source string
		ApprovedAt                                   *string
		Author                                       struct {
			ID          string
			DisplayName *string
		}
		Artifacts []struct{ Kind, AssetID, URL string }
	}
	NextCursor *string
}

// Real HTTPS, method-bound Identity, mTLS Content/Asset/Moderation, restricted
// PostgreSQL and external S3. No fake approval or direct DB publication setup.
func TestPublicTaskBrowsing(t *testing.T) {
	cfg := sharedS3TestConfig(t)
	l := newIdentityLab(t)
	adb := serviceTestDatabase(t, l, "asset", asset.Migrate, "../../../ops/asset-grants.sql")
	cdb := serviceTestDatabase(t, l, "content", content.Migrate, "../../../ops/content/grants.sql")
	mdb := serviceTestDatabase(t, l, "moderation", moderation.Migrate, "../../../ops/moderation/grants.sql")
	cs := &content.Server{DB: cdb, Identity: l.client(t, 0, "content", l.pki)}
	cc := func(caller string) contentpb.ContentServiceClient {
		return contentpb.NewContentServiceClient(serviceTestGRPC(t, l, "content", caller, content.Authorization, func(g *grpc.Server) { contentpb.RegisterContentServiceServer(g, cs) }))
	}
	storage, err := asset.NewS3Storage(cfg)
	must(t, err)
	as := layeredAssetServer(adb, storage, l.client(t, 0, "asset", l.pki), cc("asset"))
	ac := func(caller string) assetpb.AssetServiceClient {
		return assetpb.NewAssetServiceClient(serviceTestGRPC(t, l, "asset", caller, asset.Authorization, func(g *grpc.Server) { assetpb.RegisterAssetServiceServer(g, as) }))
	}
	cs.Asset = ac("content")
	moderator := cc("moderation")
	ms := &moderation.Server{DB: mdb, Identity: l.client(t, 0, "moderation", l.pki), Content: moderator}
	var moderationServers []*grpc.Server
	mc := func(caller string) moderationpb.ModerationServiceClient {
		return moderationpb.NewModerationServiceClient(serviceTestGRPC(t, l, "moderation", caller, moderation.Authorization, func(g *grpc.Server) {
			moderationpb.RegisterModerationServiceServer(g, ms)
			moderationServers = append(moderationServers, g)
		}))
	}
	sender, mgw := mc("content"), mc("gateway")
	gw := cc("gateway")
	for i := range l.web {
		h, e := gateway.New(l.clients[i], gateway.Options{Origin: l.origin, Content: gw, Asset: ac("gateway"), Moderation: mgw, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		must(t, e)
		l.web[i].Config.Handler = h
	}
	alice, me := l.login(t, "browse-alice", nil)
	bob, _ := l.login(t, "browse-bob", nil)
	_, am := l.login(t, "browse-admin", nil)
	must(t, identitygrpc.Error(application.NewAdmin(postgres.New(l.db), randomToken).ChangeAccount(t.Context(), "test", dto.ChangeAccountInput{AccountID: am.Account.Id, Role: "admin", State: "active", ExpectedVersion: 1})))
	admin, ame := l.login(t, "browse-admin", nil)
	ah := map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/json"}
	mh := map[string]string{"Origin": l.origin, "X-CSRF-Token": ame.CsrfToken, "Content-Type": "application/json"}
	req := func(c *http.Cookie, method, path, body string, h map[string]string) *http.Response {
		var cookies []*http.Cookie
		if c != nil {
			cookies = []*http.Cookie{c}
		}
		return l.request(t, 0, method, path, cookies, body, h)
	}
	data := []byte("approved S3 content, never private configuration\n")
	ah["Idempotency-Key"] = "public-browse-upload-01"
	r := req(alice, "POST", "/api/me/assets/upload-sessions", fmt.Sprintf(`{"filename":"public.txt","mediaType":"text/plain","expectedBytes":%d,"sha256":"%x"}`, len(data), sha256.Sum256(data)), ah)
	httpStatus(t, r, 201)
	var upload struct {
		Asset         struct{ ID string }
		UploadSession struct{ ID string }
	}
	readJSON(t, r, &upload)
	httpStatus(t, req(alice, "PUT", "/api/me/assets/upload-sessions/"+upload.UploadSession.ID+"/content", string(data), map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))}), 200)
	httpStatus(t, req(alice, "POST", "/api/me/assets/"+upload.Asset.ID+"/complete", `{"uploadSessionId":"`+upload.UploadSession.ID+`"}`, ah), 200)
	entries := make([]map[string]any, 7)
	for i := range entries {
		e := map[string]any{"side": "human", "title": fmt.Sprintf("work-%d", i), "description": "approved text", "source": "self", "artifacts": []any{map[string]any{"kind": "file", "assetId": upload.Asset.ID}}, "permissions": map[string]any{"cloudUse": false, "statement": "PRIVATE_AUTHORIZATION"}}
		if i%2 == 1 {
			e["side"] = "agent"
			e["agentConfiguration"] = map[string]any{"model": "PRIVATE_MODEL", "prompt": "PRIVATE_PROMPT", "parameters": map[string]any{"key": "PRIVATE_PARAMETER"}}
		}
		entries[i] = e
	}
	raw, err := json.Marshal(map[string]any{"title": "Frozen title", "summary": "summary", "description": "Frozen body", "category": "绘画", "entries": entries})
	must(t, err)
	ah["Idempotency-Key"] = "public-browse-draft-01"
	r = req(alice, "POST", "/api/tasks", string(raw), ah)
	httpStatus(t, r, 201)
	var task draftResult
	readJSON(t, r, &task)
	detail := "/api/tasks/" + task.ID
	list := detail + "/entries"
	denied := func() {
		t.Helper()
		for _, p := range []string{detail, list} {
			httpStatus(t, req(nil, "GET", p, "", nil), 404)
		}
	}
	denied()
	review := func(revision int64) {
		t.Helper()
		r = req(alice, "POST", "/api/me/tasks/"+task.ID+"/review-requests", fmt.Sprintf(`{"expectedRevision":%d}`, revision), ah)
		httpStatus(t, r, 200)
		readJSON(t, r, &task)
		_, e := cs.DeliverReview(t.Context(), sender)
		must(t, e)
	}
	decide := func(body string) {
		t.Helper()
		httpStatus(t, req(admin, "POST", "/api/admin/reviews/"+*task.ReviewID+"/decision", body, mh), 202)
		_, e := ms.DeliverDecision(t.Context())
		must(t, e)
	}
	review(1)
	denied()
	decide(`{"decision":"reject","reason":"fix provenance"}`)
	denied()
	r = req(alice, "PUT", "/api/me/tasks/"+task.ID, `{"expectedRevision":1,"content":`+string(raw)+`}`, ah)
	httpStatus(t, r, 200)
	readJSON(t, r, &task)
	review(2)
	denied()
	// Persist the administrator decision once; retry its delivery, not the
	// business decision, when Content's COMMIT outcome is unknown.
	httpStatus(t, req(admin, "POST", "/api/admin/reviews/"+*task.ReviewID+"/decision", `{"decision":"approve","reason":"REVIEW_SECRET"}`, mh), 202)
	var decisionID string
	must(t, l.db.QueryRow(t.Context(), `SELECT decision_id FROM moderation.reviews WHERE id=$1`, *task.ReviewID).Scan(&decisionID))
	approvedRequest := &contentpb.ApplyReviewDecisionRequest{DecisionId: decisionID, ReviewId: *task.ReviewID, TaskId: task.ID, SubmissionRevision: 2, Outcome: contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, Reason: proto.String("REVIEW_SECRET")}
	t.Run("approval_lock_wait_cancel_and_deadline_release", func(t *testing.T) {
		tx, e := l.db.Begin(t.Context())
		must(t, e)
		defer tx.Rollback(context.Background())
		_, e = tx.Exec(t.Context(), `SELECT id FROM content.task_drafts WHERE id=$1 FOR UPDATE`, task.ID)
		must(t, e)
		for _, mode := range []string{"cancel", "deadline"} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			done := make(chan error, 1)
			go func() {
				_, e := moderator.ApplyReviewDecision(ctx, approvedRequest)
				done <- e
			}()
			for {
				var waiting bool
				must(t, l.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM content.task_drafts%FOR UPDATE%')`).Scan(&waiting))
				if waiting {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			want := codes.DeadlineExceeded
			if mode == "cancel" {
				want = codes.Canceled
				cancel()
			}
			grpcCode(t, <-done, want)
			cancel()
			probe, stop := context.WithTimeout(t.Context(), 3*time.Second)
			for cdb.Stat().AcquiredConns() != 0 {
				if probe.Err() != nil {
					stop()
					t.Fatal("approval retained its transaction after", mode)
				}
				time.Sleep(10 * time.Millisecond)
			}
			stop()
			// The decision-ID advisory lock must be released as well.
			check, e := l.db.Begin(t.Context())
			must(t, e)
			var unlocked bool
			must(t, check.QueryRow(t.Context(), `SELECT pg_try_advisory_xact_lock(hashtextextended($1,7185429005))`, decisionID).Scan(&unlocked))
			must(t, check.Rollback(t.Context()))
			if !unlocked {
				t.Fatal("approval retained its decision lock after", mode)
			}
		}
		must(t, tx.Rollback(t.Context()))
		denied()
	})
	t.Run("approval_commit_failure_and_unknown_commit_recover", func(t *testing.T) {
		// Deferred failure is raised at COMMIT, after both timestamps and the
		// public rows have been written inside the uncommitted transaction.
		_, e := l.db.Exec(t.Context(), `CREATE FUNCTION content.fail_public_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER fail_public_commit AFTER INSERT ON content.review_decision_receipts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION content.fail_public_commit()`)
		must(t, e)
		defer func() {
			_, e := l.db.Exec(t.Context(), `DROP TRIGGER IF EXISTS fail_public_commit ON content.review_decision_receipts; DROP FUNCTION IF EXISTS content.fail_public_commit()`)
			must(t, e)
		}()
		_, e = ms.DeliverDecision(t.Context())
		grpcCode(t, e, codes.Unavailable)
		denied()
		var count int
		must(t, l.db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM content.reviewed_tasks WHERE task_id=$1)+(SELECT count(*) FROM content.reviewed_entries WHERE task_id=$1)+(SELECT count(*) FROM content.review_decision_receipts WHERE review_id=$2)`, task.ID, *task.ReviewID).Scan(&count))
		if count != 0 {
			t.Fatal("failed commit partially published content", count)
		}
		_, e = l.db.Exec(t.Context(), `DROP TRIGGER fail_public_commit ON content.review_decision_receipts; DROP FUNCTION content.fail_public_commit(); UPDATE moderation.decision_outbox SET available_at=clock_timestamp() WHERE delivered_at IS NULL`)
		must(t, e)
		// PostgreSQL really commits, but its successful response is discarded
		// by the transport. No application mock decides the commit outcome.
		uncertain := publicBrowseLostCommitPool(t, cdb)
		cs.DB = uncertain
		defer func() { cs.DB = cdb }()
		_, e = ms.DeliverDecision(t.Context())
		grpcCode(t, e, codes.Unavailable)
		cs.DB = cdb
		var published time.Time
		must(t, l.db.QueryRow(t.Context(), `SELECT published_at FROM content.reviewed_tasks WHERE task_id=$1`, task.ID).Scan(&published))
		httpStatus(t, req(nil, "GET", detail, "", nil), 200)
		_, e = l.db.Exec(t.Context(), `UPDATE moderation.decision_outbox SET available_at=clock_timestamp() WHERE delivered_at IS NULL`)
		must(t, e)
		_, e = ms.DeliverDecision(t.Context())
		must(t, e)
		var workers sync.WaitGroup
		failures := make(chan error, 6)
		for range 6 {
			workers.Go(func() {
				_, e := moderator.ApplyReviewDecision(t.Context(), approvedRequest)
				failures <- e
			})
		}
		workers.Wait()
		close(failures)
		for e := range failures {
			must(t, e)
		}
		var unchanged, applied bool
		must(t, l.db.QueryRow(t.Context(), `SELECT t.published_at=$2 AND (SELECT count(*)=7 AND bool_and(approved_at=$2) FROM content.reviewed_entries WHERE task_id=t.task_id) AND (SELECT count(*)=1 FROM content.review_decision_receipts WHERE review_id=$3) FROM content.reviewed_tasks t WHERE t.task_id=$1`, task.ID, published, *task.ReviewID).Scan(&unchanged))
		must(t, l.db.QueryRow(t.Context(), `SELECT r.delivery_state='applied' AND NOT o.blocked AND o.delivered_at IS NOT NULL FROM moderation.reviews r JOIN moderation.decision_outbox o ON o.id=r.id WHERE r.id=$1`, *task.ReviewID).Scan(&applied))
		if !unchanged || !applied {
			t.Fatal("retry duplicated/reset public facts or failed to acknowledge delivery")
		}
		changed := proto.Clone(approvedRequest).(*contentpb.ApplyReviewDecisionRequest)
		changed.Reason = proto.String("different decision payload")
		_, e = moderator.ApplyReviewDecision(t.Context(), changed)
		grpcCode(t, e, codes.AlreadyExists)
	})
	receipt, err := moderator.ApplyReviewDecision(t.Context(), approvedRequest)
	must(t, err)
	var firstPublished time.Time
	must(t, l.db.QueryRow(t.Context(), `SELECT published_at FROM content.reviewed_tasks WHERE task_id=$1`, task.ID).Scan(&firstPublished))
	// Alter mutable working copy as a database-only fault injection. Public readers
	// must continue projecting the approved frozen snapshot, never task_drafts.content.
	_, err = l.db.Exec(t.Context(), `UPDATE content.task_drafts SET content=jsonb_set(content,'{title}','"UNAPPROVED_EDIT"') WHERE id=$1`, task.ID)
	must(t, err)
	t.Run("frozen_public_fields_and_credentials", func(t *testing.T) {
		for _, c := range []*http.Cookie{nil, alice, bob, admin} {
			r = req(c, "GET", detail, "", nil)
			httpStatus(t, r, 200)
			var body map[string]any
			readJSON(t, r, &body)
			if body["title"] != "Frozen title" || body["description"] != "Frozen body" || body["revision"] != float64(2) || body["heat"] != nil || len(body["taskAttachments"].([]any)) != 0 || len(body) != 10 {
				t.Fatalf("bad public task: %+v", body)
			}
			author := body["author"].(map[string]any)
			if author["id"] != me.Account.Id || author["displayName"] != nil || len(author) != 2 {
				t.Fatal("untrusted author", author)
			}
			got, e := time.Parse(time.RFC3339Nano, body["publishedAt"].(string))
			must(t, e)
			if !got.Equal(firstPublished) {
				t.Fatal("fabricated publication time")
			}
		}
		for _, h := range []map[string]string{{"Authorization": "Bearer invalid"}, {"Authorization": ""}, {"Cookie": gateway.SessionCookie + "="}, {"Cookie": gateway.SessionCookie + `="unterminated`}, {"Cookie": gateway.SessionCookie + `=invalid\value`}} {
			httpStatus(t, req(nil, "GET", detail, "", h), 401)
		}
		httpStatus(t, req(nil, "GET", detail, "", map[string]string{"Cookie": gateway.SessionCookie + `="unterminated; ` + gateway.SessionCookie + "=valid"}), 400)
		httpStatus(t, req(&http.Cookie{Name: gateway.SessionCookie, Value: "invalid"}, "GET", detail, "", nil), 401)
		httpStatus(t, req(alice, "GET", detail, "", map[string]string{"Authorization": "Bearer invalid"}), 400)
		r = req(alice, "POST", "/api/me/mcp-tokens", `{"name":"browse","createRequestId":"public-browse-token"}`, ah)
		httpStatus(t, r, 201)
		var token struct {
			Token      string
			Credential struct{ ID string }
		}
		readJSON(t, r, &token)
		bearer := map[string]string{"Authorization": "Bearer " + token.Token}
		for _, p := range []string{detail, list} {
			httpStatus(t, req(nil, "GET", p, "", bearer), 200)
		}
		httpStatus(t, req(alice, "DELETE", "/api/me/mcp-tokens/"+token.Credential.ID, "", ah), 204)
		for _, p := range []string{detail, list} {
			httpStatus(t, req(nil, "GET", p, "", bearer), 401)
		}
		// Verify receiving Content enforces exact method, audience and mTLS caller.
		assertion, e := l.clients[0].ResolvePrincipal(t.Context(), &identitypb.ResolvePrincipalRequest{Audience: "content", FullMethod: contentpb.ContentService_GetTask_FullMethodName})
		must(t, e)
		_, e = gw.ListApprovedEntries(t.Context(), &contentpb.ListApprovedEntriesRequest{ActorAssertion: assertion.ActorAssertion, TaskId: task.ID})
		grpcCode(t, e, codes.Unauthenticated)
		_, e = cc("asset").GetTask(t.Context(), &contentpb.GetTaskRequest{ActorAssertion: assertion.ActorAssertion, TaskId: task.ID})
		grpcCode(t, e, codes.PermissionDenied)
		_, e = gw.GetTask(t.Context(), &contentpb.GetTaskRequest{ActorAssertion: "forged", TaskId: task.ID})
		if e == nil {
			t.Fatal("forged assertion accepted")
		}
		_, e = l.clients[0].ResolvePrincipal(t.Context(), &identitypb.ResolvePrincipalRequest{Audience: "asset", FullMethod: contentpb.ContentService_GetTask_FullMethodName})
		grpcCode(t, e, codes.PermissionDenied)
	})
	page := func(path string) publicBrowsePage {
		t.Helper()
		r := req(nil, "GET", path, "", nil)
		httpStatus(t, r, 200)
		body, e := io.ReadAll(r.Body)
		r.Body.Close()
		must(t, e)
		for _, secret := range []string{"PRIVATE_", "REVIEW_SECRET", "agentConfiguration", "permissions", "rejectionReason", "reviewer", "vote", "heat", "deliveryState", "snapshot"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("leaked %s", secret)
			}
		}
		var p publicBrowsePage
		must(t, json.Unmarshal(body, &p))
		var fields struct{ Items []map[string]any }
		must(t, json.Unmarshal(body, &fields))
		for _, entry := range fields.Items {
			if len(entry) != 9 {
				t.Fatalf("unexpected public fields: %v", entry)
			}
		}
		return p
	}
	t.Run("keyset_pages_and_boundaries", func(t *testing.T) {
		for _, limit := range []int{1, 2, 7, 8, 100} {
			seen := map[string]bool{}
			cursor := ""
			for range 10 {
				p := page(fmt.Sprintf("%s?limit=%d&cursor=%s", list, limit, url.QueryEscape(cursor)))
				if len(p.Items) > limit || len(p.Items) == 0 {
					t.Fatal("bad page count")
				}
				for _, e := range p.Items {
					if seen[e.ID] || e.ID != receipt.Entries[len(seen)].Id || e.TaskID != task.ID || e.ApprovedAt == nil || e.Author.ID != me.Account.Id || e.Author.DisplayName != nil {
						t.Fatal("unstable IDs/order/metadata", e)
					}
					seen[e.ID] = true
					got, err := time.Parse(time.RFC3339Nano, *e.ApprovedAt)
					must(t, err)
					if !got.Equal(firstPublished) {
						t.Fatal("wrong approval time")
					}
				}
				if p.NextCursor == nil {
					break
				}
				cursor = *p.NextCursor
			}
			if len(seen) != 7 {
				t.Fatal("pagination omitted entries", len(seen))
			}
		}
		first := page(list + "?limit=2")
		for _, q := range []string{"limit=0", "limit=101", "limit=x", "limit=1&limit=2", "cursor=a&cursor=b", "category=x", "authorId=forged", "cursor=broken", "cursor=" + strings.Repeat("x", 2049)} {
			httpStatus(t, req(nil, "GET", list+"?"+q, "", nil), 400)
		}
		decoded, e := base64.RawURLEncoding.DecodeString(*first.NextCursor)
		must(t, e)
		for _, replacement := range []string{strings.Replace(string(decoded), `"v":1`, `"v":2`, 1), strings.Replace(string(decoded), `"sourceRevision":2`, `"sourceRevision":1`, 1)} {
			httpStatus(t, req(nil, "GET", list+"?cursor="+base64.RawURLEncoding.EncodeToString([]byte(replacement)), "", nil), 400)
		}
		httpStatus(t, req(nil, "GET", "/api/tasks/tsk_"+strings.Repeat("0", 32)+"/entries?cursor="+*first.NextCursor, "", nil), 400)
		httpStatus(t, req(nil, "GET", detail+"?limit=1", "", nil), 400)
	})
	t.Run("database_wait_deadline_and_cancellation_recover", func(t *testing.T) {
		// Ordinary reads never acquire row locks; an exclusive DDL lock is
		// used only to force a real PostgreSQL wait in the HTTP/RPC chain.
		tx, e := l.db.Begin(t.Context())
		must(t, e)
		defer tx.Rollback(context.Background())
		_, e = tx.Exec(t.Context(), `LOCK TABLE content.reviewed_tasks IN ACCESS EXCLUSIVE MODE`)
		must(t, e)
		r := req(nil, "GET", detail, "", nil)
		httpStatus(t, r, 504)
		var problem struct{ Code string }
		readJSON(t, r, &problem)
		if problem.Code != "deadline_exceeded" {
			t.Fatal("database deadline misclassified", problem.Code)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rq, e := http.NewRequestWithContext(ctx, "GET", l.web[0].URL+list, nil)
		must(t, e)
		u, e := url.Parse(l.origin)
		must(t, e)
		rq.Host = u.Host
		done := make(chan error, 1)
		go func() {
			r, e := l.web[0].Client().Do(rq)
			if r != nil {
				r.Body.Close()
			}
			done <- e
		}()
		probe, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		for {
			var waiting bool
			must(t, l.db.QueryRow(probe, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%LEFT JOIN LATERAL%')`).Scan(&waiting))
			if waiting {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case e := <-done:
			if e == nil {
				t.Fatal("cancelled HTTP unexpectedly succeeded")
			}
		case <-probe.Done():
			t.Fatal("HTTP cancellation did not finish")
		}
		for cdb.Stat().AcquiredConns() != 0 {
			select {
			case <-probe.Done():
				t.Fatal("cancelled database read retained a connection")
			case <-time.After(10 * time.Millisecond):
			}
		}
		must(t, tx.Rollback(t.Context()))
		httpStatus(t, req(nil, "GET", detail, "", nil), 200)
		page(list)
		// A method-bound cancelled RPC cannot return cached successful data.
		assertion, e := l.clients[0].ResolvePrincipal(t.Context(), &identitypb.ResolvePrincipalRequest{Audience: "content", FullMethod: contentpb.ContentService_GetTask_FullMethodName})
		must(t, e)
		_, e = gw.GetTask(ctx, &contentpb.GetTaskRequest{ActorAssertion: assertion.ActorAssertion, TaskId: task.ID})
		if status.Code(e) != codes.Canceled {
			t.Fatal("cancelled RPC was not rejected", e)
		}
	})
	t.Run("unknown_legacy_times_are_null", func(t *testing.T) {
		_, err := l.db.Exec(t.Context(), `UPDATE content.reviewed_tasks SET published_at=NULL WHERE task_id=$1`, task.ID)
		must(t, err)
		_, err = l.db.Exec(t.Context(), `UPDATE content.reviewed_entries SET approved_at=NULL WHERE task_id=$1`, task.ID)
		must(t, err)
		defer func() {
			_, err := l.db.Exec(t.Context(), `UPDATE content.reviewed_tasks SET published_at=$2 WHERE task_id=$1`, task.ID, firstPublished)
			must(t, err)
			_, err = l.db.Exec(t.Context(), `UPDATE content.reviewed_entries SET approved_at=$2 WHERE task_id=$1`, task.ID, firstPublished)
			must(t, err)
		}()
		r := req(nil, "GET", detail, "", nil)
		httpStatus(t, r, 200)
		var body map[string]any
		readJSON(t, r, &body)
		if body["publishedAt"] != nil {
			t.Fatal("unknown legacy publication fabricated")
		}
		for _, e := range page(list).Items {
			if e.ApprovedAt != nil {
				t.Fatal("unknown legacy approval fabricated")
			}
		}
	})
	t.Run("moderation_offline_no_voting_writes", func(t *testing.T) {
		for _, g := range moderationServers {
			g.Stop()
		}
		httpStatus(t, req(admin, "GET", "/api/admin/reviews", "", nil), 503)
		httpStatus(t, req(nil, "GET", detail, "", nil), 200)
		page(list)
		// All reads run under a SQL read-only runtime setting as an independent
		// safeguard against accidentally adding ballot/qualification writes.
		readOnlyConfig := cdb.Config().Copy()
		readOnlyConfig.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
		readOnlyDB, e := pgxpool.NewWithConfig(t.Context(), readOnlyConfig)
		must(t, e)
		cs.DB = readOnlyDB
		httpStatus(t, req(nil, "GET", detail, "", nil), 200)
		page(list)
		cs.DB = cdb
		readOnlyDB.Close()
	})
	t.Run("revocation_takedown_and_replay", func(t *testing.T) {
		first := page(list + "?limit=2")
		revision := int64(1)
		revoke := func(id string) {
			t.Helper()
			_, e := moderator.ApplyAssetReview(t.Context(), &contentpb.ApplyAssetReviewRequest{DecisionId: "browse-revoke-" + id, TaskId: task.ID, SubmissionRevision: 2, ExpectedRevision: revision, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: id, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}})
			must(t, e)
			revision++
		}
		revoke(receipt.Entries[2].Id)
		p := page(list + "?limit=2&cursor=" + *first.NextCursor)
		if len(p.Items) != 2 || p.Items[0].ID != receipt.Entries[3].Id {
			t.Fatal("revoked entry returned or false empty page")
		}
		filePath := detail + "/entries/" + receipt.Entries[2].Id + "/assets/" + upload.Asset.ID + "/content"
		httpStatus(t, req(nil, "GET", filePath, "", nil), 404)
		filePath = detail + "/entries/" + receipt.Entries[0].Id + "/assets/" + upload.Asset.ID + "/content"
		r = req(nil, "GET", filePath, "", nil)
		httpStatus(t, r, 200)
		actual, e := io.ReadAll(r.Body)
		r.Body.Close()
		must(t, e)
		if !bytes.Equal(actual, data) {
			t.Fatal("S3 content mismatch")
		}
		for i, e := range receipt.Entries {
			if i != 2 {
				revoke(e.Id)
			}
		}
		empty := page(list)
		if len(empty.Items) != 0 || empty.NextCursor != nil {
			t.Fatal("empty page contract")
		}
		_, e = moderator.ApplyReviewDecision(t.Context(), approvedRequest)
		must(t, e)
		if p := page(list); len(p.Items) != 0 {
			t.Fatal("old approval restored revoked works")
		}
		var after time.Time
		must(t, l.db.QueryRow(t.Context(), `SELECT published_at FROM content.reviewed_tasks WHERE task_id=$1`, task.ID).Scan(&after))
		if !after.Equal(firstPublished) {
			t.Fatal("publication time mutated")
		}
		_, e = moderator.ApplyAssetReview(t.Context(), &contentpb.ApplyAssetReviewRequest{DecisionId: "public-browse-takedown", TaskId: task.ID, SubmissionRevision: 2, ExpectedRevision: revision, Decision: &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_TAKEN_DOWN}})
		must(t, e)
		_, e = moderator.ApplyReviewDecision(t.Context(), approvedRequest)
		must(t, e)
		denied()
		httpStatus(t, req(nil, "GET", list+"?cursor="+*first.NextCursor, "", nil), 404)
		httpStatus(t, req(nil, "GET", filePath, "", nil), 404)
	})
	t.Run("runtime_boundaries_and_dependencies_fail_closed", func(t *testing.T) {
		for _, sql := range []string{`SELECT * FROM identity.accounts`, `SELECT * FROM asset.assets`, `SELECT * FROM moderation.reviews`, `CREATE TABLE content.forbidden(x int)`, `UPDATE content.reviewed_tasks SET published_at=clock_timestamp()`, `UPDATE content.reviewed_entries SET approved_at=clock_timestamp()`} {
			if _, e := cdb.Exec(t.Context(), sql); e == nil {
				t.Fatal("runtime escaped", sql)
			}
		}
		oldAssertion, e := l.clients[0].ResolvePrincipal(t.Context(), &identitypb.ResolvePrincipalRequest{Audience: "content", FullMethod: contentpb.ContentService_GetTask_FullMethodName, Credential: &identitypb.ResolvePrincipalRequest_SessionCookie{SessionCookie: alice.Value}})
		must(t, e)
		httpStatus(t, req(alice, "POST", "/api/auth/logout", "", ah), 204)
		_, e = gw.GetTask(t.Context(), &contentpb.GetTaskRequest{ActorAssertion: oldAssertion.ActorAssertion, TaskId: task.ID})
		grpcCode(t, e, codes.Unauthenticated)
		for _, p := range []string{detail, list} {
			httpStatus(t, req(alice, "GET", p, "", nil), 401)
		}
		cdb.Close()
		for _, p := range []string{detail, list} {
			httpStatus(t, req(nil, "GET", p, "", nil), 503)
		}
		before := cs.Identity
		cs.Identity = nil
		for _, p := range []string{detail, list} {
			httpStatus(t, req(nil, "GET", p, "", nil), 503)
		}
		cs.Identity = before
	})
}

func TestPublicBrowseMigrationUpgrade(t *testing.T) {
	l := newIdentityLab(t)
	_, err := l.db.Exec(t.Context(), `CREATE SCHEMA content; CREATE TABLE content.schema_migrations(version text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	must(t, err)
	for _, name := range []string{"001_content.sql", "002_task_draft_list.sql", "003_reviewed_assets.sql", "004_task_reviews.sql"} {
		raw, e := os.ReadFile("../content/migrations/" + name)
		must(t, e)
		_, e = l.db.Exec(t.Context(), string(raw))
		must(t, e)
		_, e = l.db.Exec(t.Context(), `INSERT INTO content.schema_migrations(version,checksum) VALUES($1,$2)`, name, fmt.Sprintf("%x", sha256.Sum256(raw)))
		must(t, e)
	}
	task := "tsk_" + strings.Repeat("a", 32)
	entry := "ent_" + strings.Repeat("b", 32)
	_, err = l.db.Exec(t.Context(), `INSERT INTO content.task_drafts(id,author_id,create_key,create_hash,content) VALUES($1,'legacy-author','legacy-create-key',decode(repeat('00',32),'hex'),'{"title":"legacy","summary":"","description":"","entries":[]}');
 `, task)
	must(t, err)
	_, err = l.db.Exec(t.Context(), `INSERT INTO content.reviewed_tasks(task_id,source_revision,revision,state,snapshot,updated_at) SELECT id,1,3,'published',content,'2026-01-01' FROM content.task_drafts WHERE id=$1`, task)
	must(t, err)
	_, err = l.db.Exec(t.Context(), `INSERT INTO content.reviewed_entries(id,task_id,position,state) VALUES($1,$2,0,'approved')`, entry, task)
	must(t, err)
	if content.Ready(t.Context(), l.db) == nil {
		t.Fatal("pre-migration ready accepted")
	}
	must(t, content.Migrate(t.Context(), l.db))
	must(t, content.Migrate(t.Context(), l.db))
	must(t, content.Ready(t.Context(), l.db))
	var ok bool
	must(t, l.db.QueryRow(t.Context(), `SELECT t.published_at IS NULL AND e.approved_at IS NULL AND t.snapshot->>'title'='legacy' AND t.revision=3 AND t.updated_at='2026-01-01' FROM content.reviewed_tasks t JOIN content.reviewed_entries e ON e.task_id=t.task_id WHERE t.task_id=$1`, task).Scan(&ok))
	if !ok {
		t.Fatal("historical facts overwritten/fabricated")
	}
	_, err = l.db.Exec(t.Context(), `UPDATE content.schema_migrations SET checksum='tampered' WHERE version='005_public_browse.sql'`)
	must(t, err)
	if content.Migrate(t.Context(), l.db) == nil {
		t.Fatal("checksum mutation accepted")
	}
	// No application service or user account receives owner/DDL grants.
}
