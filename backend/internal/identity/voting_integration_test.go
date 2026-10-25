//go:build integration

package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	votingpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/voting/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type votingBallot struct {
	ID, TaskID                  string
	CatalogRevision, Generation int64
	Entries                     []struct {
		ID, Side string
		Author   struct{ ID string }
	}
	CurrentVote *struct {
		ID, EntryID string
		Revision    int64
	}
}
type votingVote struct {
	ID, EntryID string
	Revision    int64
}
type votingStats struct {
	TotalVotes, HumanVotes, AgentVotes int64
	EntryVotes                         []struct {
		EntryID   string
		Votes     int64
		VoteShare *float64
	}
	Eligibility struct {
		CanVote, PermanentlyClosed bool
		ClosedAt                   string
	}
	Conclusion                     string
	HumanVoteShare, AgentVoteShare *float64
}

func votingRead(t *testing.T, r *http.Response, out any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	must(t, err)
	must(t, json.Unmarshal(raw, out))
}
func votingNoPrivate(t *testing.T, r *http.Response, out any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	must(t, err)
	for _, bad := range []string{"PRIVATE_", "permissions", "agentConfiguration", "totalVotes", "votes", "voteShare", "reviewer", "deliveryState", "request_hash"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Fatalf("unexpected private/statistical field %s", bad)
		}
	}
	must(t, json.Unmarshal(raw, out))
}
func votingBallotFor(t *testing.T, f *additionalEntryLab, actor additionalActor, task string) votingBallot {
	t.Helper()
	var b votingBallot
	votingNoPrivate(t, f.request(t, 0, actor, "GET", "/api/tasks/"+task+"/ballot", "", nil, 200), &b)
	return b
}
func votingBody(b votingBallot, entry string, revision int64) string {
	return fmt.Sprintf(`{"ballotId":%q,"entryId":%q,"expectedVoteRevision":%d}`, b.ID, entry, revision)
}

func TestVotingHTTPWorkflow(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-parent-key-01")
	base := "/api/tasks/" + task.ID
	t.Run("authenticated-purpose-and-parsing", func(t *testing.T) {
		f.request(t, 0, additionalActor{}, "GET", base+"/ballot", "", nil, 401).Body.Close()
		for _, q := range []string{"?limit=1", "?accountId=forged", "?cursor=x&cursor=y"} {
			f.request(t, 0, f.bob, "GET", base+"/ballot"+q, "", nil, 400).Body.Close()
		}
		for _, body := range []string{`{}`, `{"permanentlyCloseVoting":false}`} {
			f.request(t, 0, f.bob, "POST", base+"/statistics-access", body, nil, 422).Body.Close()
		}
		for _, body := range []string{`{"permanentlyCloseVoting":true,"accountId":"A"}`, `{"permanentlyCloseVoting":true,"permanentlyCloseVoting":true}`} {
			f.request(t, 0, f.bob, "POST", base+"/statistics-access", body, nil, 400).Body.Close()
		}
		var n int
		must(t, f.votingDB.QueryRow(t.Context(), `SELECT count(*) FROM voting.statistics_access`).Scan(&n))
		if n != 0 {
			t.Fatal("unconfirmed access closed eligibility")
		}
	})
	data := []byte("Voting candidate from actual S3 uploaded by B\n")
	asset := f.upload(t, f.bob, "voting-bob-file-01", data, true)
	entry := f.submit(t, 0, f.bob, task.ID, "voting-additional-01", additionalInput("human", "Bob work", asset))
	f.deliver(t)
	f.decide(t, entry.ReviewID, "approve", "approved")
	ballot := votingBallotFor(t, f, f.bob, task.ID)
	if len(ballot.Entries) != 3 || ballot.Entries[2].ID != entry.ID || ballot.Entries[2].Author.ID != f.bob.account {
		t.Fatal("complete directory or contributor identity lost")
	}
	file := base + "/entries/" + entry.ID + "/assets/" + asset + "/content"
	response := f.request(t, 1, additionalActor{}, "GET", file, "", nil, 200)
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("real object bytes mismatch")
	}
	var vote votingVote
	body := votingBody(ballot, ballot.Entries[0].ID, 0)
	votingNoPrivate(t, f.request(t, 0, f.bob, "PUT", base+"/vote", body, nil, 200), &vote)
	if vote.Revision != 1 {
		t.Fatal("first vote revision")
	}
	f.request(t, 1, f.alice, "PUT", base+"/vote", body, nil, 409).Body.Close()
	var retried votingVote
	votingRead(t, f.request(t, 1, f.bob, "PUT", base+"/vote", body, nil, 200), &retried)
	if retried.ID != vote.ID || retried.Revision != 1 {
		t.Fatal("lost-response retry duplicated vote")
	}
	body2 := votingBody(ballot, entry.ID, 1)
	votingRead(t, f.request(t, 1, f.bob, "PUT", base+"/vote", body2, nil, 200), &vote)
	if vote.Revision != 2 {
		t.Fatal("change must increment revision")
	}
	f.request(t, 0, f.bob, "PUT", base+"/vote", body, nil, 409).Body.Close()
	var stats votingStats
	votingRead(t, f.request(t, 0, f.bob, "POST", base+"/statistics-access", `{"permanentlyCloseVoting":true}`, nil, 200), &stats)
	if stats.TotalVotes != 1 || stats.HumanVotes != 1 || stats.AgentVotes != 0 || len(stats.EntryVotes) != 3 || !stats.Eligibility.PermanentlyClosed || stats.Eligibility.CanVote || stats.Conclusion != "undetermined" {
		t.Fatal("closure or statistics invariant failed")
	}
	closedAt := stats.Eligibility.ClosedAt
	f.request(t, 1, f.bob, "PUT", base+"/vote", body2, nil, 403).Body.Close()
	f.request(t, 0, f.bob, "GET", base+"/ballot", "", nil, 403).Body.Close()
	votingRead(t, f.request(t, 1, f.bob, "POST", base+"/statistics-access", `{"permanentlyCloseVoting":true}`, nil, 200), &stats)
	if closedAt != stats.Eligibility.ClosedAt {
		t.Fatal("permanent first closure changed")
	}
	// Closing voting does not affect ordinary reading, contribution or other tasks.
	f.request(t, 0, f.bob, "GET", base, "", nil, 200).Body.Close()
	next := f.submit(t, 0, f.bob, task.ID, "voting-after-closed-01", additionalInput("agent", "later work", ""))
	f.deliver(t)
	f.decide(t, next.ReviewID, "approve", "")
	f.request(t, 1, f.bob, "GET", base+"/ballot", "", nil, 403).Body.Close()
	other := f.publish(t, "voting-other-task-01")
	_ = votingBallotFor(t, f, f.bob, other.ID)
	// Tightening is itself fenced and persisted, exact old approval cannot reopen.
	var display, source int64
	must(t, f.identity.db.QueryRow(t.Context(), `SELECT revision,source_revision FROM content.reviewed_tasks WHERE task_id=$1`, task.ID).Scan(&display, &source))
	_, err = f.reviewers[0].ApplyAssetReview(t.Context(), &contentpb.ApplyAssetReviewRequest{DecisionId: "voting-revoke-key-01", TaskId: task.ID, SubmissionRevision: source, ExpectedRevision: display, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: entry.ID, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}})
	must(t, err)
	f.request(t, 1, additionalActor{}, "GET", file, "", nil, 404).Body.Close()
	votingRead(t, f.request(t, 1, f.bob, "POST", base+"/statistics-access", `{"permanentlyCloseVoting":true}`, nil, 200), &stats)
	if stats.TotalVotes != 0 || stats.HumanVoteShare != nil || len(stats.EntryVotes) != 3 {
		t.Fatal("revoked choice counted or zero candidates omitted")
	}
	_, err = f.reviewers[0].ApplyAssetReview(t.Context(), &contentpb.ApplyAssetReviewRequest{DecisionId: "voting-takedown-key-01", TaskId: task.ID, SubmissionRevision: source, ExpectedRevision: display + 1, Decision: &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_TAKEN_DOWN}})
	must(t, err)
	for _, path := range []string{base, base + "/entries", file, base + "/ballot", base + "/voting-eligibility", base + "/statistics-access"} {
		method, body := "GET", ""
		if strings.HasSuffix(path, "statistics-access") {
			method, body = "POST", `{"permanentlyCloseVoting":true}`
		}
		f.request(t, 0, f.alice, method, path, body, nil, 404).Body.Close()
	}
}

func TestVotingCompleteLargeBallotAndConcurrentVotes(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-full-parent-01")
	base := "/api/tasks/" + task.ID
	var additionalIDs []string
	for i := 0; i < 20; i++ {
		entry := f.submit(t, 0, f.bob, task.ID, fmt.Sprintf("voting-more-entry-%03d", i), additionalInput("human", fmt.Sprintf("Work %d", i), ""))
		additionalIDs = append(additionalIDs, entry.ID)
		f.deliver(t)
		f.decide(t, entry.ReviewID, "approve", "")
	}
	b := votingBallotFor(t, f, f.alice, task.ID)
	if len(b.Entries) != 22 {
		t.Fatalf("ballot truncated: %d", len(b.Entries))
	}
	for i, id := range additionalIDs {
		if b.Entries[i+2].ID != id || b.Entries[i+2].Author.ID != f.bob.account {
			t.Fatal("complete ballot lost stable append order/author")
		}
	}
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(t.Context(), "PUT", f.identity.web[i%2].URL+base+"/vote", strings.NewReader(votingBody(b, b.Entries[i].ID, 0)))
			req.Host = strings.TrimPrefix(f.identity.origin, "https://")
			req.AddCookie(f.alice.cookie)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", f.identity.origin)
			req.Header.Set("X-CSRF-Token", f.alice.csrf)
			r, e := f.identity.web[i%2].Client().Do(req)
			if e != nil {
				results <- 0
				return
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			results <- r.StatusCode
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for code := range results {
		if code == 200 {
			success++
		} else if code != 409 {
			t.Fatalf("unexpected concurrent response %d", code)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent first votes: %d successes", success)
	}
	var count int
	must(t, f.votingDB.QueryRow(t.Context(), `SELECT count(*) FROM voting.current_votes WHERE task_id=$1`, task.ID).Scan(&count))
	if count != 1 {
		t.Fatal("duplicate current votes")
	}
}

// Separate component / protocol checks still use real DB+mTLS; simulated network
// response loss is explicitly identified and never presented as a real Pod crash.
func TestVotingCatalogProtocol(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-protocol-parent-01")
	ctx := t.Context()
	c, err := f.catalogs[0].GetVotingCatalogSnapshot(ctx, &contentpb.GetVotingCatalogSnapshotRequest{TaskId: task.ID})
	must(t, err)
	v := f.contents[0].Voting
	change := "chg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	gen := c.Catalog.Generation + 1
	req := &votingpb.PrepareCatalogChangeRequest{ChangeId: change, TaskId: task.ID, ExpectedGeneration: gen - 1, Generation: gen}
	p, err := v.PrepareCatalogChange(ctx, req)
	must(t, err)
	if p.Transition.Phase != "prepared" {
		t.Fatal("missing freeze receipt")
	}
	recovered, err := f.contents[1].Voting.GetCatalogTransition(ctx, &votingpb.GetCatalogTransitionRequest{ChangeId: change})
	must(t, err)
	if !proto.Equal(recovered.Transition, p.Transition) {
		t.Fatal("persistent prepare query changed receipt")
	}

	// Repeat prepare and restart-equivalent new RPC client recover persistent receipt.
	_, err = f.contents[1].Voting.PrepareCatalogChange(ctx, req)
	must(t, err)
	target := proto.Clone(c.Catalog).(*contentpb.VotingCatalogSnapshot)
	target.Generation = gen
	target.State = "closed"
	target.Entries = nil
	target.Sha256 = contract.CatalogHash(target)
	_, err = v.ActivateCatalogSnapshot(ctx, &votingpb.ActivateCatalogSnapshotRequest{ChangeId: change, Catalog: target})
	must(t, err)
	old := proto.Clone(c.Catalog).(*contentpb.VotingCatalogSnapshot)
	old.Generation = gen
	old.Sha256 = contract.CatalogHash(old)
	_, err = v.ActivateCatalogSnapshot(ctx, &votingpb.ActivateCatalogSnapshotRequest{ChangeId: change, Catalog: old})
	if status.Code(err) != codes.Aborted {
		t.Fatal("same change with different content accepted", err)
	}
	_, err = v.PrepareCatalogChange(ctx, &votingpb.PrepareCatalogChangeRequest{ChangeId: "chg_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TaskId: task.ID, ExpectedGeneration: gen, Generation: gen + 1})
	must(t, err)
	old.Generation++
	old.Sha256 = contract.CatalogHash(old)
	_, err = v.ActivateCatalogSnapshot(ctx, &votingpb.ActivateCatalogSnapshotRequest{ChangeId: "chg_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Catalog: old})
	if status.Code(err) != codes.Aborted {
		t.Fatal("closed tombstone reopened", err)
	}
	_, err = f.voteClients[0].PrepareCatalogChange(ctx, req)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("gateway called internal protocol", err)
	}
}

func TestVotingBallotInvalidationByAdditionAndRevoke(t *testing.T) {
	f := newAdditionalEntryLab(t, true)
	task := f.publish(t, "voting-invalidation-parent")
	base := "/api/tasks/" + task.ID
	old := votingBallotFor(t, f, f.alice, task.ID)
	entry := f.submit(t, 0, f.bob, task.ID, "voting-invalidation-entry", additionalInput("human", "new candidate", ""))
	f.deliver(t)
	f.decide(t, entry.ReviewID, "approve", "")
	f.request(t, 1, f.alice, "PUT", base+"/vote", votingBody(old, old.Entries[0].ID, 0), nil, 409).Body.Close()
	current := votingBallotFor(t, f, f.alice, task.ID)
	var v votingVote
	votingRead(t, f.request(t, 1, f.alice, "PUT", base+"/vote", votingBody(current, entry.ID, 0), nil, 200), &v)
	var display, source int64
	must(t, f.contentDB.QueryRow(t.Context(), `SELECT revision,source_revision FROM content.reviewed_tasks WHERE task_id=$1`, task.ID).Scan(&display, &source))
	_, err := f.reviewers[0].ApplyAssetReview(t.Context(), &contentpb.ApplyAssetReviewRequest{TaskId: task.ID, DecisionId: "voting-invalidation-revoke", SubmissionRevision: source, ExpectedRevision: display, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: entry.ID, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}})
	must(t, err)
	f.request(t, 0, f.alice, "PUT", base+"/vote", votingBody(current, current.Entries[0].ID, 1), nil, 409).Body.Close()
	refreshed := votingBallotFor(t, f, f.alice, task.ID)
	if len(refreshed.Entries) != 2 || refreshed.CurrentVote == nil || refreshed.CurrentVote.EntryID != entry.ID {
		t.Fatal("revocation erased current choice or retained revoked candidate")
	}
	votingRead(t, f.request(t, 1, f.alice, "PUT", base+"/vote", votingBody(refreshed, refreshed.Entries[0].ID, 1), nil, 200), &v)
	if v.Revision != 2 {
		t.Fatal("refresh/change duplicated current vote")
	}
	other := f.publish(t, "voting-invalidation-other")
	f.request(t, 0, f.alice, "PUT", "/api/tasks/"+other.ID+"/vote", votingBody(refreshed, refreshed.Entries[0].ID, 0), nil, 409).Body.Close()
}
