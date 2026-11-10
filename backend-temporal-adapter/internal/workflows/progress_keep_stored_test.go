package workflows

import "testing"

// A state update with no execution_plan (the pause and resume writes in
// nl_pipeline_v2_workflow.go) said nothing about progress, yet StateUpdateActivity
// wrote 0% / step 0 / 7 steps over the run's real counts — the "7/7 steps" a
// one-step chat run showed. Same class as the projector's default-7 fix: an
// update that carries no plan must not invent one.

func TestProgressForUpdate_NoPlanKeepsTheRunsStoredProgress(t *testing.T) {
	stored := storedProgress{executionID: "run-1", percent: 40, step: 2, total: 5, ok: true}
	p, s, total := progressForUpdate("waiting_for_user", map[string]interface{}{"paused": true}, "run-1", stored)
	if p != 40 || s != 2 || total != 5 {
		t.Fatalf("pause wrote %d%% %d/%d, want the stored 40%% 2/5", p, s, total)
	}
}

func TestProgressForUpdate_NoPlanCompletedFinishesTheStoredBar(t *testing.T) {
	stored := storedProgress{executionID: "run-1", percent: 80, step: 4, total: 5, ok: true}
	p, s, total := progressForUpdate("completed", nil, "run-1", stored)
	if p != 100 || s != 5 || total != 5 {
		t.Fatalf("completed wrote %d%% %d/%d, want 100%% 5/5", p, s, total)
	}
}

// Another run's counts are not this run's progress.
func TestProgressForUpdate_NoPlanOnANewRunDoesNotInheritTheOldOne(t *testing.T) {
	stored := storedProgress{executionID: "run-old", percent: 100, step: 5, total: 5, ok: true}
	p, s, total := progressForUpdate("running", nil, "run-new", stored)
	if p != 0 || s != 0 || total != 7 {
		t.Fatalf("new run wrote %d%% %d/%d, want the fresh-row default 0%% 0/7", p, s, total)
	}
}

func TestProgressForUpdate_PlanStillWins(t *testing.T) {
	stored := storedProgress{executionID: "run-1", percent: 10, step: 1, total: 9, ok: true}
	meta := map[string]interface{}{"execution_plan": map[string]interface{}{"stages": []interface{}{
		map[string]interface{}{"status": "completed", "progress": float64(100)},
		map[string]interface{}{"status": "running", "progress": float64(0)},
	}}}
	_, _, total := progressForUpdate("running", meta, "run-1", stored)
	if total != 2 {
		t.Fatalf("total = %d, want the plan's 2 stages", total)
	}
}
