//go:build integration

package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Only the dedicated test PostgreSQL is used. Downstream RPC responses are
// controlled here; these tests do not claim the complete website/file workflow.
func moderationDatabase(t *testing.T, before func(*pgxpool.Pool)) (owner, runtime *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("IDENTITY_TEST_DATABASE_URL")
	if path := os.Getenv("IDENTITY_TEST_DATABASE_URL_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		mustModeration(t, err)
		dsn = strings.TrimSpace(string(raw))
	}
	if dsn == "" {
		t.Fatal("integration requires isolated PostgreSQL via IDENTITY_TEST_DATABASE_URL[_FILE]")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	mustModeration(t, err)
	name, err := randomID("hw_moderation")
	mustModeration(t, err)
	ownerName, runtimeName := name+"_owner", name+"_runtime"
	identifier := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+identifier(name))
	mustModeration(t, err)
	var db *pgxpool.Pool
	t.Cleanup(func() {
		if runtime != nil {
			runtime.Close()
		}
		if owner != nil {
			owner.Close()
		}
		if db != nil {
			db.Close()
		}
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+identifier(name)+" WITH (FORCE)")
		if err != nil {
			t.Error(err)
		}
		_, err = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+identifier(runtimeName)+", "+identifier(ownerName))
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	mustModeration(t, err)
	cfg.ConnConfig.Database = name
	db, err = pgxpool.NewWithConfig(t.Context(), cfg)
	mustModeration(t, err)
	password, err := randomID("pw")
	mustModeration(t, err)
	_, err = db.Exec(t.Context(), "REVOKE CREATE ON DATABASE "+identifier(name)+" FROM PUBLIC; REVOKE CREATE ON SCHEMA public FROM PUBLIC; CREATE ROLE "+identifier(ownerName)+" LOGIN PASSWORD '"+password+"'; CREATE ROLE "+identifier(runtimeName)+" LOGIN PASSWORD '"+password+"'; CREATE SCHEMA moderation AUTHORIZATION "+identifier(ownerName)+"; CREATE SCHEMA unrelated; CREATE TABLE unrelated.private_data(secret text)")
	mustModeration(t, err)
	ownerConfig := cfg.Copy()
	ownerConfig.ConnConfig.User, ownerConfig.ConnConfig.Password = ownerName, password
	owner, err = pgxpool.NewWithConfig(t.Context(), ownerConfig)
	mustModeration(t, err)
	if before != nil {
		before(owner)
	}
	mustModeration(t, Migrate(t.Context(), owner))
	mustModeration(t, Migrate(t.Context(), owner))
	mustModeration(t, Ready(t.Context(), owner))
	raw, err := os.ReadFile("../../../ops/moderation/grants.sql")
	mustModeration(t, err)
	lines := strings.Split(string(raw), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "\\") {
			filtered = append(filtered, line)
		}
	}
	_, err = owner.Exec(t.Context(), strings.ReplaceAll(strings.Join(filtered, "\n"), "moderation_runtime", identifier(runtimeName)))
	mustModeration(t, err)
	runtimeConfig := cfg.Copy()
	runtimeConfig.ConnConfig.User, runtimeConfig.ConnConfig.Password = runtimeName, password
	runtime, err = pgxpool.NewWithConfig(t.Context(), runtimeConfig)
	mustModeration(t, err)
	return owner, runtime
}

func mustModeration(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestEntryReviewMigration(t *testing.T) {
	var oldRows string
	owner, runtime := moderationDatabase(t, func(db *pgxpool.Pool) {
		files := fstest.MapFS{}
		for _, name := range []string{"001_moderation.sql", "002_approval_reason.sql"} {
			body, err := migrations.ReadFile("migrations/" + name)
			mustModeration(t, err)
			files["migrations/"+name] = &fstest.MapFile{Data: body}
		}
		mustModeration(t, platform.Migrate(t.Context(), db, files, "moderation", 7185429006))
		_, err := db.Exec(t.Context(), `INSERT INTO moderation.reviews(id,task_id,submission_revision,snapshot,snapshot_hash,created_at) VALUES('pending','task-pending',1,'{}',decode(repeat('01',32),'hex'),clock_timestamp());
INSERT INTO moderation.reviews(id,task_id,submission_revision,snapshot,snapshot_hash,created_at,status,decision,decision_id,reason,reviewer_id,decided_at,delivery_state,receipt) VALUES
('approved','task-approved',1,'{}',decode(repeat('02',32),'hex'),clock_timestamp(),'approved','approve','decision-approved','批准原样','admin',clock_timestamp(),'applied','{"state":"published"}'),
('rejected','task-rejected',1,'{}',decode(repeat('03',32),'hex'),clock_timestamp(),'rejected','reject','decision-rejected','驳回原样','admin',clock_timestamp(),'applied','{"state":"rejected"}');`)
		mustModeration(t, err)
		mustModeration(t, db.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM moderation.reviews r`).Scan(&oldRows))
		if Ready(t.Context(), db) == nil {
			t.Fatal("old schema passed new readiness")
		}
	})
	var upgraded string
	mustModeration(t, owner.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(r)-'object_kind'-'object_id' ORDER BY id)::text FROM moderation.reviews r`).Scan(&upgraded))
	if upgraded != oldRows {
		t.Fatal("migration rewrote legacy snapshots/decisions/hashes/receipts")
	}
	for _, id := range []string{"pending", "approved", "rejected"} {
		r, err := readReview(runtime.QueryRow(t.Context(), reviewSelect+` WHERE id=$1`, id))
		mustModeration(t, err)
		if r.Snapshot == nil || r.EntrySnapshot != nil || r.EntryReceipt != nil || (id == "pending" && (r.Receipt != nil || r.DecisionId != nil || r.Reason != nil || r.ReviewerId != nil || r.DecidedAt != nil)) {
			t.Fatal("legacy NULL mapping changed", r.Review)
		}
	}
	// Old INSERT shape still gets task identity and cannot evade uniqueness.
	_, err := runtime.Exec(t.Context(), `INSERT INTO moderation.reviews(id,task_id,submission_revision,snapshot,snapshot_hash,created_at) VALUES('duplicate','task-pending',1,'{}',decode(repeat('01',32),'hex'),clock_timestamp())`)
	if err == nil {
		t.Fatal("old writer escaped task/revision uniqueness")
	}
	for _, sql := range []string{`UPDATE moderation.reviews SET object_kind='entry'`, `UPDATE moderation.reviews SET object_id='forged'`, `UPDATE moderation.reviews SET snapshot='{}'`, `UPDATE moderation.reviews SET snapshot_hash=decode(repeat('00',32),'hex')`, `DELETE FROM moderation.reviews`, `CREATE TABLE moderation.forbidden(x int)`, `SELECT * FROM unrelated.private_data`, `UPDATE moderation.schema_migrations SET checksum='bad'`} {
		if _, err := runtime.Exec(t.Context(), sql); err == nil {
			t.Fatalf("runtime escaped minimal privileges: %s", sql)
		}
	}
}

func TestEntryReviewCasesAndDelivery(t *testing.T) {
	owner, runtime := moderationDatabase(t, nil)
	s := &Server{DB: runtime, Identity: adminIdentityClient{}}
	contentCtx, adminCtx := serviceContext("content"), serviceContext("gateway")
	create := func(snap *contentpb.EntryReviewSnapshot) *pb.Review {
		t.Helper()
		r, err := s.CreateEntryReviewCase(contentCtx, &pb.CreateEntryReviewCaseRequest{Snapshot: snap})
		mustModeration(t, err)
		if r.Review.Snapshot != nil || r.Review.Receipt != nil || !proto.Equal(r.Review.EntrySnapshot, snap) || r.Review.EntryReceipt != nil {
			t.Fatal("entry was mapped into task fields", r.Review)
		}
		return r.Review
	}
	snap := entrySnapshot(1)
	first := create(snap)
	if !proto.Equal(first, create(snap)) {
		t.Fatal("identical retry changed case")
	}
	create(entrySnapshot(2)) // Same parent and revision=1 must not conflict.
	newer := entrySnapshot(3)
	newer.Submission.Id, newer.Submission.Revision = snap.Submission.Id, 2
	create(newer)
	for _, change := range []func(*contentpb.EntryReviewSnapshot){
		func(r *contentpb.EntryReviewSnapshot) { r.Submission.Content.Title = "changed" },
		func(r *contentpb.EntryReviewSnapshot) {
			r.ReviewId = entrySnapshot(4).ReviewId
			r.Submission.ReviewId = r.ReviewId
		},
		func(r *contentpb.EntryReviewSnapshot) { r.Submission.TaskId = "tsk_" + strings.Repeat("b", 32) },
	} {
		r := proto.Clone(snap).(*contentpb.EntryReviewSnapshot)
		change(r)
		_, err := s.CreateEntryReviewCase(contentCtx, &pb.CreateEntryReviewCaseRequest{Snapshot: r})
		if status.Code(err) != codes.AlreadyExists {
			t.Fatal("conflicting immutable case accepted", err)
		}
	}
	taskSnapshot := &contentpb.ReviewSnapshot{ReviewId: entrySnapshot(10).ReviewId, CreatedAt: snap.CreatedAt,
		Submission: &contentpb.TaskSubmission{Id: snap.Submission.TaskId, AuthorId: "task-author", Revision: 1, State: "pending_review", ReviewId: proto.String(entrySnapshot(10).ReviewId), Content: &contentpb.TaskDraftInput{Entries: []*contentpb.EntryInput{{Side: "human"}, {Side: "agent"}}}},
		EntryIds:   []string{entrySnapshot(20).Submission.Id, entrySnapshot(21).Submission.Id}}
	_, err := s.CreateReviewCase(contentCtx, &pb.CreateReviewCaseRequest{Snapshot: taskSnapshot})
	mustModeration(t, err)
	// Review IDs remain global across object kinds.
	collision := proto.Clone(snap).(*contentpb.EntryReviewSnapshot)
	collision.ReviewId, collision.Submission.ReviewId = taskSnapshot.ReviewId, taskSnapshot.ReviewId
	_, err = s.CreateEntryReviewCase(contentCtx, &pb.CreateEntryReviewCaseRequest{Snapshot: collision})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatal("cross-kind review collision accepted", err)
	}
	seen, cursor := map[string]bool{}, ""
	for {
		page, err := s.ListReviews(adminCtx, &pb.ListReviewsRequest{Limit: 1, Cursor: cursor})
		mustModeration(t, err)
		if len(page.Items) != 1 || seen[page.Items[0].Id] {
			t.Fatal("mixed paging skipped or duplicated review", page)
		}
		seen[page.Items[0].Id] = true
		if proto.Size(page) > 2<<20 {
			t.Fatal("mixed page exceeded protobuf budget")
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatal("mixed list lost task/entry cases", seen)
	}
	t.Run("concurrent_identical_case_creation", func(t *testing.T) {
		snap := entrySnapshot(25)
		var wg sync.WaitGroup
		results := make(chan *pb.CreateEntryReviewCaseResponse, 2)
		errors := make(chan error, 2)
		for range 2 {
			wg.Go(func() {
				r, err := s.CreateEntryReviewCase(contentCtx, &pb.CreateEntryReviewCaseRequest{Snapshot: snap})
				results <- r
				errors <- err
			})
		}
		wg.Wait()
		mustModeration(t, <-errors)
		mustModeration(t, <-errors)
		if !proto.Equal(<-results, <-results) {
			t.Fatal("concurrent retries returned different cases")
		}
	})
	decide := func(id string, outcome contentpb.ReviewOutcome) *pb.Review {
		t.Helper()
		r, err := s.DecideReview(adminCtx, &pb.DecideReviewRequest{ReviewId: id, Decision: outcome, Reason: proto.String("原因原样")})
		mustModeration(t, err)
		return r.Review
	}
	expire := func(id string) {
		t.Helper()
		_, err := owner.Exec(t.Context(), `UPDATE moderation.decision_outbox SET available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
		mustModeration(t, err)
	}
	assertState := func(id, delivery string, blocked bool) {
		t.Helper()
		var actualDelivery string
		var actualBlocked bool
		mustModeration(t, owner.QueryRow(t.Context(), `SELECT r.delivery_state,o.blocked FROM moderation.reviews r JOIN moderation.decision_outbox o ON o.id=r.id WHERE r.id=$1`, id).Scan(&actualDelivery, &actualBlocked))
		if actualDelivery != delivery || actualBlocked != blocked {
			t.Fatalf("state=%s/%t, want %s/%t", actualDelivery, actualBlocked, delivery, blocked)
		}
	}
	decided := decide(snap.ReviewId, contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE)
	if decided.Status != "pending" || decided.DeliveryState != pb.DeliveryState_DELIVERY_STATE_PENDING {
		t.Fatal("decision claimed application without receipt")
	}
	if decide(snap.ReviewId, contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE).GetDecisionId() != decided.GetDecisionId() {
		t.Fatal("decision retry changed identity")
	}
	var firstRequest *contentpb.ApplyEntryReviewDecisionRequest
	s.Content = entryContentClient{apply: func(_ context.Context, r *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
		firstRequest = proto.Clone(r).(*contentpb.ApplyEntryReviewDecisionRequest)
		return nil, status.Error(codes.Unavailable, "response_lost")
	}}
	_, err = s.DeliverDecision(t.Context())
	if status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
	assertState(snap.ReviewId, "pending", false)
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(snap)
	mustModeration(t, err)
	hash := sha256.Sum256(wire)
	if firstRequest.SnapshotSha256 != hex.EncodeToString(hash[:]) || firstRequest.SubmissionRevision != 1 {
		t.Fatal("delivery used newer entry snapshot or wrong digest", firstRequest)
	}
	expire(snap.ReviewId)
	s2 := &Server{DB: runtime, Content: entryContentClient{apply: func(_ context.Context, r *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
		if !proto.Equal(firstRequest, r) {
			t.Fatal("lease recovery changed durable decision")
		}
		return entryReceipt(r), nil
	}}}
	_, err = s2.DeliverDecision(t.Context())
	mustModeration(t, err)
	assertState(snap.ReviewId, "applied", false)
	r, err := s.GetReview(adminCtx, &pb.GetReviewRequest{ReviewId: snap.ReviewId})
	mustModeration(t, err)
	if r.Review.Status != "approved" || r.Review.Receipt != nil || r.Review.EntryReceipt.GetState() != "approved" {
		t.Fatal("entry acknowledgement used task semantics", r.Review)
	}

	t.Run("technical_errors_invalid_receipt_stale_lease_and_blocked", func(t *testing.T) {
		review := create(entrySnapshot(30))
		decide(review.Id, contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE)
		for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Internal, codes.Unknown, codes.ResourceExhausted, codes.Unauthenticated, codes.PermissionDenied, codes.NotFound} {
			expire(review.Id)
			s.Content = entryContentClient{apply: func(context.Context, *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
				return nil, status.Error(code, "technical_failure")
			}}
			_, err := s.deliverDecision(t.Context())
			if status.Code(err) != code {
				t.Fatal("technical error changed", err)
			}
			assertState(review.Id, "pending", false)
		}
		expire(review.Id)
		s.Content = entryContentClient{apply: func(_ context.Context, req *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
			out := entryReceipt(req)
			out.State = "published"
			return out, nil
		}}
		result, err := s.deliverDecision(t.Context())
		if status.Code(err) != codes.Unavailable || result.Stage != "validate_receipt" {
			t.Fatal("invalid receipt was accepted or blocked", result, err)
		}
		assertState(review.Id, "pending", false)
		expire(review.Id)
		s.Content = entryContentClient{apply: func(_ context.Context, req *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
			_, err := owner.Exec(t.Context(), `UPDATE moderation.decision_outbox SET lease_token='new_holder' WHERE id=$1`, review.Id)
			mustModeration(t, err)
			return entryReceipt(req), nil
		}}
		result, err = s.deliverDecision(t.Context())
		mustModeration(t, err)
		if result.Outcome != "lease_lost" {
			t.Fatal("stale lease confirmed receipt", result)
		}
		assertState(review.Id, "pending", false)
		// Failed blocked persistence must roll back both review and outbox.
		_, err = owner.Exec(t.Context(), `CREATE FUNCTION moderation.fail_block() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'blocked commit injection'; END $$;
CREATE CONSTRAINT TRIGGER fail_block AFTER UPDATE ON moderation.reviews DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.delivery_state='blocked') EXECUTE FUNCTION moderation.fail_block()`)
		mustModeration(t, err)
		s.Content = entryContentClient{apply: func(context.Context, *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "task_not_accepting_entries")
		}}
		expire(review.Id)
		result, err = s.deliverDecision(t.Context())
		if status.Code(err) != codes.Unavailable || result.Outcome == "blocked" {
			t.Fatal("uncommitted blocked was reported as durable", result, err)
		}
		assertState(review.Id, "pending", false)
		_, err = owner.Exec(t.Context(), `DROP TRIGGER fail_block ON moderation.reviews; DROP FUNCTION moderation.fail_block()`)
		mustModeration(t, err)
		expire(review.Id)
		result, err = s.deliverDecision(t.Context())
		mustModeration(t, err)
		if result.Outcome != "blocked" || result.Reason != "task_not_accepting_entries" {
			t.Fatal("permanent conflict not blocked", result)
		}
		assertState(review.Id, "blocked", true)
	})
	t.Run("opposite_decisions_and_rejection_receipt", func(t *testing.T) {
		review := create(entrySnapshot(40))
		var wg sync.WaitGroup
		errors := make(chan error, 2)
		for _, outcome := range []contentpb.ReviewOutcome{contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE, contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT} {
			wg.Go(func() {
				_, err := s.DecideReview(adminCtx, &pb.DecideReviewRequest{ReviewId: review.Id, Decision: outcome, Reason: proto.String("原因原样")})
				errors <- err
			})
		}
		wg.Wait()
		a, b := <-errors, <-errors
		if !((a == nil && status.Code(b) == codes.Aborted) || (b == nil && status.Code(a) == codes.Aborted)) {
			t.Fatal("opposite decisions did not serialize", a, b)
		}
		s.Content = entryContentClient{apply: func(_ context.Context, r *contentpb.ApplyEntryReviewDecisionRequest) (*contentpb.ApplyEntryReviewDecisionResponse, error) {
			return entryReceipt(r), nil
		}}
		_, err := s.DeliverDecision(t.Context())
		mustModeration(t, err)
		rejected := create(entrySnapshot(41))
		decide(rejected.Id, contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT)
		_, err = s.DeliverDecision(t.Context())
		mustModeration(t, err)
		r, err := s.GetReview(adminCtx, &pb.GetReviewRequest{ReviewId: rejected.Id})
		mustModeration(t, err)
		if r.Review.Status != "rejected" || r.Review.EntryReceipt.GetState() != "rejected" || r.Review.Receipt != nil || r.Review.GetReason() != "原因原样" {
			t.Fatal("rejection lost receipt or reason", r.Review)
		}
	})
}
