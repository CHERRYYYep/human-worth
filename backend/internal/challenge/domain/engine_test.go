package domain

import (
	"reflect"
	"testing"
	"time"
)

func replayState() *State {
	return &State{
		Run: &Run{Id: "run_test", TaskId: "target", Status: "active", Stage: "optimizing_ranker", Configuration: &RunConfiguration{
			InitialTaskPackage: &InitialTaskPackage{TaskRevision: 1},
			Ranker:             &ModelConfiguration{Model: "ranker"}, Packer: &ModelConfiguration{Model: "packer"}, Executor: &ExecutorConfiguration{Model: "executor"},
			RankerFitThreshold: 0.75, RankerMinComparablePairs: 1, RankerRoundLimit: 3, RoundLimit: 2,
		}},
		Task:         &TaskDescription{TaskId: "target", Revision: 1, Description: "Compare the works"},
		RankerPrompt: "Initial criteria",
	}
}

func replaySample() *RankingSample {
	return &RankingSample{Task: &TaskDescription{TaskId: "sample", Revision: 1}, Works: []*Work{{Id: "human"}, {Id: "agent"}}}
}

func replayPreferences() *PreferenceBatch {
	return &PreferenceBatch{Samples: []*HumanPreference{{Snapshot: &SnapshotRef{TaskId: "sample"}, CountsByWork: map[string]int64{"human": 2, "agent": 1}, ValidVoteCount: 3}}}
}

func replayRankings(sample *RankingSample) *Rankings {
	ranking := &Ranking{TaskId: sample.Task.TaskId}
	for _, work := range sample.Works {
		ranking.OrderedWorkIds = append(ranking.OrderedWorkIds, work.Id)
		ranking.Judgments = append(ranking.Judgments, &Judgment{WorkId: work.Id, Criteria: "accuracy", Rationale: "checked", EvidenceRefs: []string{work.Id}})
	}
	return &Rankings{Items: []*Ranking{ranking}}
}

// Exercise every assignment-creating entry point and the R → P → E → R cycle.
// Equal inputs must reproduce the complete state, including IDs and timestamps.
func TestEngineReplay(t *testing.T) {
	engine := Engine{Config: Config{ModelCallLimit: 8, ToolCallLimit: 16, InputPolicy: InputPolicyVersion}}
	now := time.Date(2026, 10, 21, 1, 2, 3, 4, time.UTC)
	replay := func() []*State {
		st := replayState()
		var states []*State
		checkpoint := func(id string, kind WorkKind) {
			t.Helper()
			if st.Assignment == nil || st.Assignment.WorkItemId != id || st.Assignment.Kind != kind {
				t.Fatalf("expected %s/%s, got %+v", id, kind, st.Assignment)
			}
			states = append(states, CloneState(st))
		}
		apply := func(result Result, id string) {
			t.Helper()
			if err := engine.ApplyResult(st, result, "attempt_exec", "fit_test", id, now); err != nil {
				t.Fatal(err)
			}
		}
		engine.SetTraining(st, replayPreferences(), []*RankingSample{replaySample()}, "work_training")
		checkpoint("work_training", WorkKind_WORK_KIND_REFINE_RANKER)
		apply(Result{Prompt: &PromptResult{Prompt: "Revised criteria"}}, "unused_refine")
		if st.Pending != "validation" || st.Assignment != nil || st.Run.RankerRound != 1 {
			t.Fatal("refinement must schedule independent validation")
		}
		engine.SetValidation(st, replayPreferences(), []*RankingSample{replaySample()}, "work_validation")
		checkpoint("work_validation", WorkKind_WORK_KIND_VALIDATE_RANKER)
		apply(Result{Rankings: replayRankings(replaySample())}, "work_pack")
		checkpoint("work_pack", WorkKind_WORK_KIND_PACK_TASK)
		if st.Run.RankerFit.Status != "passed" || !st.Run.RankerFit.EvaluatedAt.Equal(now) || st.Run.Round != 1 {
			t.Fatal("fit and evaluation time must use explicit inputs")
		}
		apply(Result{Prompt: &PromptResult{Prompt: "Execution hints"}}, "work_execute")
		checkpoint("work_execute", WorkKind_WORK_KIND_EXECUTE_PACKAGE)
		apply(Result{Generated: &GeneratedWork{Work: &Work{Id: "work_attempt_exec", Artifacts: []*Artifact{{Value: &Artifact_Text{Text: "My work"}}}}, Report: "Done"}}, "unused_execute")
		if st.Pending != "rank" || st.Assignment != nil || len(st.Run.Candidates) != 1 {
			t.Fatal("execution must await a fresh ranking snapshot")
		}
		input := replaySample()
		input.Task = st.Task
		if err := engine.RankGeneratedWork(st, input, &SnapshotRef{TaskId: "target"}, "work_rank"); err != nil {
			t.Fatal(err)
		}
		checkpoint("work_rank", WorkKind_WORK_KIND_RANK_WORKS)
		apply(Result{Rankings: replayRankings(input)}, "work_explain")
		checkpoint("work_explain", WorkKind_WORK_KIND_EXPLAIN_OWN_WORK)
		if st.Feedback.Rank != 3 || st.Feedback.PopulationSize != 3 {
			t.Fatal("own work must be included in the ranking")
		}
		apply(Result{Judgment: &Judgment{WorkId: "work_attempt_exec", Criteria: "accuracy", Rationale: "checked", EvidenceRefs: []string{"work_attempt_exec"}}}, "work_next_pack")
		checkpoint("work_next_pack", WorkKind_WORK_KIND_PACK_TASK)
		if st.Run.Round != 2 {
			t.Fatal("explanation must advance the next round")
		}
		return states
	}
	if first, second := replay(), replay(); !reflect.DeepEqual(first, second) {
		t.Fatal("identical state, results, IDs and time must reproduce every transition")
	}
}

func TestEngineFailedFitReplay(t *testing.T) {
	engine := Engine{}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, round := range []int32{1, 3} {
		var previous *State
		for repeat := 0; repeat < 2; repeat++ {
			st := replayState()
			st.Run.RankerRound = round
			engine.SetValidation(st, replayPreferences(), []*RankingSample{replaySample()}, "work_validation")
			rankings := replayRankings(replaySample())
			rankings.Items[0].OrderedWorkIds = []string{"agent", "human"}
			if err := engine.ApplyResult(st, Result{Rankings: rankings}, "attempt_validation", "fit_test", "work_retry", now); err != nil {
				t.Fatal(err)
			}
			if round == 1 {
				if st.Assignment == nil || st.Assignment.WorkItemId != "work_retry" || st.Assignment.Kind != WorkKind_WORK_KIND_REFINE_RANKER {
					t.Fatal("failed fit must refine with the supplied ID")
				}
			} else if st.Assignment != nil || st.Pending != "close" || st.Run.FailureReason != "ranker_fit_not_met" {
				t.Fatal("round limit must stop without creating a new assignment")
			}
			if previous != nil && !reflect.DeepEqual(previous, st) {
				t.Fatal("failed-fit replay must reproduce the same state")
			}
			previous = st
		}
	}
}
