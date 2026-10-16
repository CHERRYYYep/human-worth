package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var reviewedEntryPattern = regexp.MustCompile(`^ent_[a-f0-9]{32}$`)
var reviewedAssetPattern = regexp.MustCompile(`^ast_[a-f0-9]{32}$`)

func requireContentCaller(ctx context.Context, expected string) error {
	name, err := platform.ServiceName(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, "service_identity_required")
	}
	if name != expected {
		return status.Error(codes.PermissionDenied, "service_forbidden")
	}
	return nil
}

// Reads only Content's committed authority. Never call Asset from this method.
func (s *Server) CheckAssetAccess(ctx context.Context, r *pb.CheckAssetAccessRequest) (*pb.CheckAssetAccessResponse, error) {
	if err := requireContentCaller(ctx, "asset"); err != nil {
		return nil, err
	}
	ref := r.GetReference()
	result := &pb.CheckAssetAccessResponse{}
	if !taskPattern.MatchString(ref.GetTaskId()) || !reviewedEntryPattern.MatchString(ref.GetEntryId()) || !reviewedAssetPattern.MatchString(ref.GetAssetId()) {
		return result, nil
	}
	err := s.DB.QueryRow(ctx, `SELECT t.revision,
 t.state='published' AND e.state='approved' AND
 (t.snapshot->'entries'->e.position->'artifacts') @> jsonb_build_array(jsonb_build_object('kind','file','assetId',$3::text))
 FROM content.reviewed_tasks t JOIN content.reviewed_entries e ON e.task_id=t.task_id
 WHERE t.task_id=$1 AND e.id=$2`, ref.TaskId, ref.EntryId, ref.AssetId).Scan(&result.Revision, &result.Allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return &pb.CheckAssetAccessResponse{}, nil
	}
	if err != nil {
		return nil, storage(err)
	}
	return result, nil
}

// Applies an already-made trusted review decision, not a review workflow. No
// user-supplied asset IDs enter this method: all references come from the exact
// draft revision whose ownership was validated when it was written.
func (s *Server) ApplyAssetReview(ctx context.Context, r *pb.ApplyAssetReviewRequest) (*pb.ApplyAssetReviewResponse, error) {
	if err := requireContentCaller(ctx, "moderation"); err != nil {
		return nil, err
	}
	if !keyPattern.MatchString(r.GetDecisionId()) || !taskPattern.MatchString(r.GetTaskId()) || r.GetSubmissionRevision() < 1 || r.GetExpectedRevision() < 0 {
		return nil, invalid("invalid_review_decision")
	}
	taskState := ""
	entryState := ""
	switch d := r.GetDecision().(type) {
	case *pb.ApplyAssetReviewRequest_TaskState:
		switch d.TaskState {
		case pb.ReviewedTaskState_REVIEWED_TASK_STATE_PUBLISHED:
			taskState = "published"
		case pb.ReviewedTaskState_REVIEWED_TASK_STATE_TAKEN_DOWN:
			taskState = "taken_down"
		default:
			return nil, invalid("invalid_review_decision")
		}
	case *pb.ApplyAssetReviewRequest_EntryReview:
		if !reviewedEntryPattern.MatchString(d.EntryReview.GetEntryId()) {
			return nil, invalid("invalid_review_decision")
		}
		switch d.EntryReview.GetState() {
		case pb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVED:
			entryState = "approved"
		case pb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED:
			entryState = "approval_revoked"
		default:
			return nil, invalid("invalid_review_decision")
		}
	default:
		return nil, invalid("invalid_review_decision")
	}
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
	if err != nil {
		return nil, invalid("invalid_review_decision")
	}
	hash := sha256.Sum256(wire)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, storage(err)
	}
	defer tx.Rollback(ctx)
	// Serialize global decision IDs as well as per-task state, including collisions
	// across tasks. Always acquire the decision lock before the task row lock.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,7185429003))`, r.DecisionId); err != nil {
		return nil, storage(err)
	}
	// All decisions for a task and private draft edits lock the same row first.
	var snapshot []byte
	var draftRevision int64
	err = tx.QueryRow(ctx, `SELECT content,revision FROM content.task_drafts WHERE id=$1 FOR UPDATE`, r.TaskId).Scan(&snapshot, &draftRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "task_not_found")
	}
	if err != nil {
		return nil, storage(err)
	}
	var oldHash, receipt []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM content.asset_review_receipts WHERE decision_id=$1`, r.DecisionId).Scan(&oldHash, &receipt)
	if err == nil {
		if !bytes.Equal(oldHash, hash[:]) {
			return nil, status.Error(codes.AlreadyExists, "idempotency_key_conflict")
		}
		result := &pb.ApplyAssetReviewResponse{}
		if err = protojson.Unmarshal(receipt, result); err != nil {
			return nil, status.Error(codes.Internal, "invalid_review_receipt")
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, storage(err)
	}
	var revision, sourceRevision int64
	err = tx.QueryRow(ctx, `SELECT revision,source_revision FROM content.reviewed_tasks WHERE task_id=$1 FOR UPDATE`, r.TaskId).Scan(&revision, &sourceRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		if r.ExpectedRevision != 0 || r.SubmissionRevision != draftRevision || taskState != "published" {
			return nil, status.Error(codes.Aborted, "revision_conflict")
		}
		input := &pb.TaskDraftInput{}
		if err = protojson.Unmarshal(snapshot, input); err != nil {
			return nil, status.Error(codes.Internal, "invalid_stored_draft")
		}
		human, agent := false, false
		for _, e := range input.Entries {
			human = human || e.GetSide() == "human"
			agent = agent || e.GetSide() == "agent"
		}
		if !human || !agent {
			return nil, status.Error(codes.FailedPrecondition, "initial_bundle_required")
		}
		// Validate persisted data as well; old/corrupt rows cannot bypass the boundary.
		if _, err = validate(input); err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO content.reviewed_tasks(task_id,source_revision,revision,state,snapshot) VALUES($1,$2,1,'published',$3)`, r.TaskId, draftRevision, snapshot)
		if err != nil {
			return nil, storage(err)
		}
		for i := range input.Entries {
			var id [16]byte
			if _, err = rand.Read(id[:]); err != nil {
				return nil, status.Error(codes.Internal, "internal_error")
			}
			_, err = tx.Exec(ctx, `INSERT INTO content.reviewed_entries(id,task_id,position,state) VALUES($1,$2,$3,'approved')`, fmt.Sprintf("ent_%x", id), r.TaskId, i)
			if err != nil {
				return nil, storage(err)
			}
		}
		revision = 1
	} else {
		if err != nil {
			return nil, storage(err)
		}
		if revision != r.ExpectedRevision || sourceRevision != r.SubmissionRevision {
			return nil, status.Error(codes.Aborted, "revision_conflict")
		}
		if entryState != "" {
			tag, e := tx.Exec(ctx, `UPDATE content.reviewed_entries SET state=$3 WHERE task_id=$1 AND id=$2`, r.TaskId, r.GetEntryReview().EntryId, entryState)
			if e != nil {
				return nil, storage(e)
			}
			if tag.RowsAffected() != 1 {
				return nil, status.Error(codes.NotFound, "entry_not_found")
			}
			_, err = tx.Exec(ctx, `UPDATE content.reviewed_tasks SET revision=revision+1,updated_at=clock_timestamp() WHERE task_id=$1`, r.TaskId)
		} else {
			_, err = tx.Exec(ctx, `UPDATE content.reviewed_tasks SET state=$2,revision=revision+1,updated_at=clock_timestamp() WHERE task_id=$1`, r.TaskId, taskState)
		}
		if err != nil {
			return nil, storage(err)
		}
		revision++
	}
	result := &pb.ApplyAssetReviewResponse{Revision: revision}
	rows, err := tx.Query(ctx, `SELECT id,position,state FROM content.reviewed_entries WHERE task_id=$1 ORDER BY position`, r.TaskId)
	if err != nil {
		return nil, storage(err)
	}
	for rows.Next() {
		e := &pb.ReviewedEntry{}
		var state string
		if err = rows.Scan(&e.Id, &e.Position, &state); err != nil {
			rows.Close()
			return nil, storage(err)
		}
		e.State = pb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVED
		if state == "approval_revoked" {
			e.State = pb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED
		}
		result.Entries = append(result.Entries, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, storage(err)
	}
	receipt, err = protojson.Marshal(result)
	if err != nil {
		return nil, status.Error(codes.Internal, "internal_error")
	}
	_, err = tx.Exec(ctx, `INSERT INTO content.asset_review_receipts(decision_id,task_id,request_hash,response) VALUES($1,$2,$3,$4)`, r.DecisionId, r.TaskId, hash[:], receipt)
	if err != nil {
		return nil, storage(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, storage(err)
	}
	return result, nil
}
