//go:build integration

package identity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	votingpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/voting/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/voting"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func votingCatalogReviewApply(ctx context.Context, client contentpb.ContentServiceClient, r proto.Message) (proto.Message, error) {
	switch r := r.(type) {
	case *contentpb.ApplyReviewDecisionRequest:
		return client.ApplyReviewDecision(ctx, r)
	case *contentpb.ApplyEntryReviewDecisionRequest:
		return client.ApplyEntryReviewDecision(ctx, r)
	default:
		return client.ApplyAssetReview(ctx, r.(*contentpb.ApplyAssetReviewRequest))
	}
}

// These are pre-Voting business receipts produced by real HTTPS and mTLS
// commands. Bootstrap must not turn their replay into another catalog change.
func TestVotingCatalogReviewBootstrapReceipts(t *testing.T) {
	for _, method := range []string{"task", "entry", "legacy"} {
		t.Run(method, func(t *testing.T) {
			f := newAdditionalEntryLab(t, true)
			clients := [2]votingpb.VotingServiceClient{f.contents[0].Voting, f.contents[1].Voting}
			for _, server := range f.contents {
				server.Voting = nil
			}
			task := f.publish(t, "catalog-review-bootstrap-parent")
			var request proto.Message
			switch method {
			case "task":
				r := &contentpb.ApplyReviewDecisionRequest{TaskId: task.ID, ReviewId: *task.ReviewID, SubmissionRevision: 1, Outcome: contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE}
				must(t, f.reviewDB.QueryRow(t.Context(), `SELECT decision_id,reason FROM moderation.reviews WHERE id=$1`, r.ReviewId).Scan(&r.DecisionId, &r.Reason))
				request = r
			case "entry":
				entry := f.submit(t, 0, f.bob, task.ID, "catalog-review-bootstrap-entry", additionalInput("human", "historical entry", ""))
				f.deliver(t)
				f.decide(t, entry.ReviewID, "approve", "historical approval")
				r := &contentpb.ApplyEntryReviewDecisionRequest{TaskId: task.ID, EntryId: entry.ID, ReviewId: entry.ReviewID, SubmissionRevision: 1, Outcome: contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE}
				must(t, f.reviewDB.QueryRow(t.Context(), `SELECT decision_id,reason,encode(snapshot_hash,'hex') FROM moderation.reviews WHERE id=$1`, r.ReviewId).Scan(&r.DecisionId, &r.Reason, &r.SnapshotSha256))
				request = r
			case "legacy":
				entries := f.publicPage(t, "/api/tasks/"+task.ID+"/entries")
				request = &contentpb.ApplyAssetReviewRequest{TaskId: task.ID, DecisionId: "catalog-review-historical-revoke", SubmissionRevision: 1, ExpectedRevision: 1, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: entries.Items[0].ID, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}}
			}
			original, err := votingCatalogReviewApply(t.Context(), f.reviewers[0], request)
			must(t, err)
			for i, server := range f.contents {
				server.Voting = clients[i]
			}
			must(t, f.contents[0].BootstrapCatalogs(t.Context()))
			ballot := votingBallotFor(t, f, f.bob, task.ID)
			if ballot.Generation != 1 {
				t.Fatal("bootstrap generation", ballot.Generation)
			}
			changed := proto.Clone(request)
			switch r := changed.(type) {
			case *contentpb.ApplyReviewDecisionRequest:
				r.Reason = proto.String("different content")
			case *contentpb.ApplyEntryReviewDecisionRequest:
				r.Reason = proto.String("different content")
			case *contentpb.ApplyAssetReviewRequest:
				r.ExpectedRevision++
			}
			assertStable := func(t *testing.T) {
				t.Helper()
				var generation, changes int64
				var active *string
				must(t, f.contentDB.QueryRow(t.Context(), `SELECT generation,active_change,(SELECT count(*) FROM content.voting_catalog_changes WHERE task_id=$1) FROM content.voting_catalog_heads WHERE task_id=$1`, task.ID).Scan(&generation, &active, &changes))
				if generation != 1 || changes != 1 || active != nil {
					t.Errorf("historical replay changed catalog: generation=%d changes=%d active=%v", generation, changes, active)
				}
			}
			t.Run("online", func(t *testing.T) {
				_, err := votingCatalogReviewApply(t.Context(), f.reviewers[1], changed)
				grpcCode(t, err, codes.AlreadyExists)
				out, err := votingCatalogReviewApply(t.Context(), f.reviewers[1], request)
				must(t, err)
				if !proto.Equal(out, original) {
					t.Error("historical response changed")
				}
				assertStable(t)
			})
			t.Run("no_voting_client", func(t *testing.T) {
				f.contents[0].Voting = nil
				defer func() { f.contents[0].Voting = clients[0] }()
				out, err := votingCatalogReviewApply(t.Context(), f.reviewers[0], request)
				must(t, err)
				if !proto.Equal(out, original) {
					t.Error("offline historical response changed")
				}
				_, err = votingCatalogReviewApply(t.Context(), f.reviewers[0], changed)
				grpcCode(t, err, codes.AlreadyExists)
				assertStable(t)
			})
			t.Run("stopped_voting", func(t *testing.T) {
				var stopped *grpc.Server
				conn := serviceTestGRPC(t, f.identity, "voting", "content", voting.Authorization, func(g *grpc.Server) {
					stopped = g
					votingpb.RegisterVotingServiceServer(g, f.votes[0])
				})
				stopped.Stop()
				f.contents[0].Voting = votingpb.NewVotingServiceClient(conn)
				defer func() { f.contents[0].Voting = clients[0] }()
				out, err := votingCatalogReviewApply(t.Context(), f.reviewers[0], request)
				must(t, err)
				if !proto.Equal(out, original) {
					t.Error("stopped Voting changed historical response")
				}
				_, err = votingCatalogReviewApply(t.Context(), f.reviewers[0], changed)
				grpcCode(t, err, codes.AlreadyExists)
				assertStable(t)
			})
			// The pre-replay ballot remains usable: a receipt cannot invalidate it.
			f.request(t, 1, f.bob, "PUT", "/api/tasks/"+task.ID+"/vote", votingBody(ballot, ballot.Entries[0].ID, 0), nil, 200).Body.Close()
			votingCatalogReviewActiveReplay(t, f, task.ID, request, changed, original)
		})
	}
}

func votingCatalogReviewActiveReplay(t *testing.T, f *additionalEntryLab, task string, request, changed, original proto.Message) {
	t.Helper()
	var stopped *grpc.Server
	conn := serviceTestGRPC(t, f.identity, "voting", "content", voting.Authorization, func(g *grpc.Server) {
		stopped = g
		votingpb.RegisterVotingServiceServer(g, f.votes[0])
	})
	stopped.Stop()
	for _, s := range f.contents {
		s.Voting = votingpb.NewVotingServiceClient(conn)
	}
	grpcCode(t, votingTakedown(t, f, task, "catalog-review-other-active-change"), codes.Unavailable)
	state := func() string {
		var out string
		must(t, f.identity.db.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(h),(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM content.voting_catalog_changes c WHERE c.task_id=$1),(SELECT to_jsonb(g) FROM voting.catalog_gates g WHERE g.task_id=$1))::text FROM content.voting_catalog_heads h WHERE task_id=$1`, task).Scan(&out))
		return out
	}
	before := state()
	for _, nilClient := range []bool{false, true} {
		if nilClient {
			f.contents[0].Voting = nil
		}
		out, err := votingCatalogReviewApply(t.Context(), f.reviewers[0], request)
		must(t, err)
		if !proto.Equal(out, original) {
			t.Fatal("active change altered old receipt")
		}
		_, err = votingCatalogReviewApply(t.Context(), f.reviewers[0], changed)
		grpcCode(t, err, codes.AlreadyExists)
		if after := state(); before != after {
			t.Fatal("receipt replay/conflict changed active head, intent or Voting gate")
		}
	}
}

func TestVotingCatalogReviewCompletedReceiptDuringActiveChange(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "catalog-review-completed-parent")
	var raw []byte
	must(t, f.contentDB.QueryRow(t.Context(), `SELECT request FROM content.voting_catalog_changes WHERE task_id=$1 AND method='task' AND stage='completed'`, task.ID).Scan(&raw))
	r := &contentpb.ApplyReviewDecisionRequest{}
	must(t, proto.Unmarshal(raw, r))
	original, err := votingCatalogReviewApply(t.Context(), f.reviewers[0], r)
	must(t, err)
	changed := proto.Clone(r).(*contentpb.ApplyReviewDecisionRequest)
	changed.Reason = proto.String("same key different content")
	votingCatalogReviewActiveReplay(t, f, task.ID, r, changed, original)
}

// A real mTLS Voting endpoint either stops or rejects a method before its
// handler, matching transport/runtime admission failures rather than fake data.
func TestVotingCatalogReviewTransientRecovery(t *testing.T) {
	for _, failure := range []struct {
		name, method, stage, gate string
		stop                      bool
	}{
		{"stopped_prepare", votingpb.VotingService_PrepareCatalogChange_FullMethodName, "queued", "open", true},
		{"limited_prepare", votingpb.VotingService_PrepareCatalogChange_FullMethodName, "queued", "open", false},
		{"limited_activate", votingpb.VotingService_ActivateCatalogSnapshot_FullMethodName, "applied", "frozen", false},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f := newAdditionalEntryLab(t, true)
			task := f.publish(t, "catalog-review-recovery-parent")
			original := f.contents[0].Voting
			var denied atomic.Int32
			var server *grpc.Server
			conn := serviceTestGRPC(t, f.identity, "voting", "content", func(ctx context.Context, r any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
				return voting.Authorization(ctx, r, info, func(ctx context.Context, r any) (any, error) {
					if info.FullMethod == failure.method {
						denied.Add(1)
						return nil, status.Error(codes.ResourceExhausted, "too_many_requests")
					}
					return next(ctx, r)
				})
			}, func(g *grpc.Server) {
				server = g
				votingpb.RegisterVotingServiceServer(g, f.votes[0])
			})
			if failure.stop {
				server.Stop()
			}
			for _, s := range f.contents {
				s.Voting = votingpb.NewVotingServiceClient(conn)
			}
			err := votingTakedown(t, f, task.ID, "catalog-review-recovery-takedown")
			if err == nil {
				t.Fatal("Voting failure reported successful catalog change")
			}
			var intent, stage, gate string
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT id,stage FROM content.voting_catalog_changes WHERE task_id=$1 AND generation=2`, task.ID).Scan(&intent, &stage))
			must(t, f.votingDB.QueryRow(t.Context(), `SELECT state FROM voting.catalog_gates WHERE task_id=$1`, task.ID).Scan(&gate))
			if stage != failure.stage || gate != failure.gate {
				t.Fatalf("transient failure became terminal or lost fence: stage=%s gate=%s, want %s/%s", stage, gate, failure.stage, failure.gate)
			}
			if !failure.stop && denied.Load() != 1 {
				t.Fatal("real admission failure not reached", denied.Load())
			}
			var raw []byte
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT request FROM content.voting_catalog_changes WHERE id=$1`, intent).Scan(&raw))
			pending := &contentpb.ApplyAssetReviewRequest{}
			must(t, proto.Unmarshal(raw, pending))
			// In particular, applied already has a local business receipt. It must
			// remain pending until Voting activates and Content confirms completion.
			_, err = f.reviewers[0].ApplyAssetReview(t.Context(), pending)
			grpcCode(t, err, codes.Unavailable)
			changed := proto.Clone(pending).(*contentpb.ApplyAssetReviewRequest)
			changed.ExpectedRevision++
			_, err = f.reviewers[0].ApplyAssetReview(t.Context(), changed)
			grpcCode(t, err, codes.AlreadyExists)
			must(t, f.contentDB.QueryRow(t.Context(), `SELECT stage FROM content.voting_catalog_changes WHERE id=$1`, intent).Scan(&stage))
			if stage != failure.stage {
				t.Fatal("pending replay/conflict changed stage", stage)
			}
			f.request(t, 0, additionalActor{}, "GET", "/api/tasks/"+task.ID, "", nil, 404).Body.Close()
			// Expire only the isolated test lease; another replica resumes the same ID.
			_, err = f.identity.db.Exec(t.Context(), `UPDATE content.voting_catalog_changes SET available_at=clock_timestamp() WHERE id=$1`, intent)
			must(t, err)
			for _, s := range f.contents {
				s.Voting = original
			}
			result, err := f.contents[1].DeliverCatalog(t.Context())
			must(t, err)
			if result.Outcome != "delivered" || result.ItemID != intent {
				t.Fatal("same intent did not recover", result)
			}
			must(t, votingTakedownReplay(t, f, task.ID, "catalog-review-recovery-takedown"))
			var stable bool
			must(t, f.identity.db.QueryRow(t.Context(), `SELECT (SELECT generation=2 AND active_change IS NULL FROM content.voting_catalog_heads WHERE task_id=$1) AND (SELECT count(*)=2 FROM content.voting_catalog_changes WHERE task_id=$1) AND (SELECT stage='completed' FROM content.voting_catalog_changes WHERE id=$2) AND (SELECT state='closed' AND generation=2 FROM voting.catalog_gates WHERE task_id=$1)`, task.ID, intent).Scan(&stable))
			if !stable {
				t.Fatal("recovery duplicated intent or lost closed gate")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = f.contents[0].DeliverCatalog(ctx)
			must(t, err)
		})
	}
}
