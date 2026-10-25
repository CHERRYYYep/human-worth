package content

import (
	"context"
	"errors"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// Used by all public reads/authorization. An outstanding intent conservatively
// denies new reads of the whole task, including before a remote freeze succeeds.
const catalogPublic = `NOT EXISTS(SELECT 1 FROM content.voting_catalog_heads vh WHERE vh.task_id=t.task_id AND vh.active_change IS NOT NULL)`

func catalogSnapshot(ctx context.Context, tx pgx.Tx, task string, generation int64) (*pb.VotingCatalogSnapshot, error) {
	c := &pb.VotingCatalogSnapshot{TaskId: task, Generation: generation, State: "unpublished"}
	var state string
	err := tx.QueryRow(ctx, `SELECT source_revision,revision,catalog_revision,state FROM content.reviewed_tasks WHERE task_id=$1`, task).Scan(&c.TaskRevision, &c.DisplayRevision, &c.CatalogRevision, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		c.Sha256 = contract.CatalogHash(c)
		return c, nil
	}
	if err != nil {
		return nil, storage(err)
	}
	c.State = "closed"
	if state == "published" {
		c.State = "open"
	}
	if c.State == "open" {
		rows, err := tx.Query(ctx, `SELECT e.id,COALESCE(e.author_id,d.author_id),e.approved_at,(`+publicEntryContent+`)-'agentConfiguration'-'permissions'
 FROM content.reviewed_tasks t JOIN content.task_drafts d ON d.id=t.task_id JOIN content.reviewed_entries e ON e.task_id=t.task_id WHERE t.task_id=$1 AND e.state='approved' ORDER BY e.position,e.id LIMIT $2`, task, contract.VotingCatalogEntries+1)
		if err != nil {
			return nil, storage(err)
		}
		size := 0
		for rows.Next() {
			e := &pb.ApprovedEntry{TaskId: task, Author: &pb.PublicAuthor{}}
			var approved *time.Time
			var raw []byte
			if err = rows.Scan(&e.Id, &e.Author.Id, &approved, &raw); err != nil {
				rows.Close()
				return nil, storage(err)
			}
			size += len(raw)
			if size > contract.VotingCatalogBytes || len(c.Entries) >= contract.VotingCatalogEntries {
				rows.Close()
				return nil, status.Error(codes.ResourceExhausted, "catalog_too_large")
			}
			input := &pb.EntryInput{}
			if protojson.Unmarshal(raw, input) != nil {
				rows.Close()
				return nil, status.Error(codes.Internal, "invalid_review_snapshot")
			}
			e.Side = input.Side
			e.Title = input.Title
			e.Description = input.GetDescription()
			e.Source = input.Source
			e.Artifacts = input.Artifacts
			e.ApprovedAt = publicTime(approved)
			c.Entries = append(c.Entries, e)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, storage(err)
		}
	}
	c.Sha256 = contract.CatalogHash(c)
	if contract.ValidateCatalog(c) != nil {
		return nil, status.Error(codes.ResourceExhausted, "catalog_too_large")
	}
	return c, nil
}
func (s *Server) GetVotingCatalogSnapshot(ctx context.Context, r *pb.GetVotingCatalogSnapshotRequest) (*pb.GetVotingCatalogSnapshotResponse, error) {
	if err := requireContentCaller(ctx, "voting"); err != nil {
		return nil, err
	}
	if !taskPattern.MatchString(r.GetTaskId()) {
		return nil, status.Error(codes.NotFound, "task_not_found")
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, storage(err)
	}
	defer tx.Rollback(ctx)
	var generation int64
	var active *string
	err = tx.QueryRow(ctx, `SELECT generation,active_change FROM content.voting_catalog_heads WHERE task_id=$1`, r.TaskId).Scan(&generation, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		var visible bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM content.reviewed_tasks WHERE task_id=$1 AND state='published')`, r.TaskId).Scan(&visible); err != nil {
			return nil, storage(err)
		}
		if !visible {
			return nil, status.Error(codes.NotFound, "task_not_found")
		}
		return nil, status.Error(codes.Unavailable, "catalog_not_initialized")
	}
	if err != nil {
		return nil, storage(err)
	}
	if active != nil || generation == 0 {
		return nil, status.Error(codes.Unavailable, "catalog_change_pending")
	}
	c, err := catalogSnapshot(ctx, tx, r.TaskId, generation)
	if err != nil {
		return nil, err
	}
	return &pb.GetVotingCatalogSnapshotResponse{Catalog: c}, nil
}
