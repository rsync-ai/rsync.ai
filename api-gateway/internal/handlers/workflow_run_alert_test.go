package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"api-gateway/internal/notifier"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
)

// A workflow run nobody started by hand that failed tells the workflow's owner
// (workflow_run_alert.go), from each place a run is recorded failed: finish,
// complete, and evaluate, including a run whose workflow could not be loaded.

const wfAlertModel = "dddddddd-0000-4000-8000-0000000000e1"

// wfAlertDefinition is a graph with one of each step the gate reads: a pipeline,
// a model, a send that runs when one of them failed and one that runs when both
// worked.
func wfAlertDefinition() workflow.Definition {
	return workflow.Definition{Version: 3,
		Trigger: workflow.Trigger{Type: workflow.TriggerAfterPipeline, PipelineID: wfPipe},
		Steps: []workflow.Step{
			{ID: "load", Type: workflow.StepRunPipeline, PipelineID: wfPipe},
			{ID: "refresh", Type: workflow.StepRefreshModel, ModelID: wfAlertModel},
			{ID: "tell", Type: workflow.StepSend, After: []string{"load", "refresh"}, Rule: workflow.RuleAnyFailed,
				Message: &workflow.Message{Subject: "Nightly load failed"}},
			{ID: "report", Type: workflow.StepSend, After: []string{"load", "refresh"}, Rule: workflow.RuleAllSuccess,
				Message: &workflow.Message{Subject: "Nightly load"}},
		}}
}

func wfAlertDefinitionJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(wfAlertDefinition())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// expectStepPipelineRun answers the gate's read of a step's pipeline run.
func expectStepPipelineRun(mock sqlmock.Sqlmock, step string, status any) *sqlmock.ExpectedQuery {
	q := mock.ExpectQuery(`FROM workflow_step_runs s LEFT JOIN executions e ON e.id::text = s.execution_id`).
		WithArgs(wfRunUUID, step)
	if status != nil {
		q.WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
	}
	return q
}

// assertWorkflowFailureNotice is the one notice a failed run sends: the
// workflow's code, on the workflow, keyed by the run, with nothing from it.
func assertWorkflowFailureNotice(t *testing.T, notices []map[string]interface{}) {
	t.Helper()
	if len(notices) != 1 {
		t.Fatalf("workflow failure notices sent = %d, want 1: %v", len(notices), notices)
	}
	n := notices[0]
	env := noticeError(t, n)
	if n["workflow_id"] != wfID || env["code"] != notifier.CodeWorkflowRunFailed || env["severity"] != "error" {
		t.Errorf("notice = %v", n)
	}
	if n["action_url"] != "/workflows/"+wfID || env["dedup_subject"] != wfRunUUID {
		t.Errorf("action_url = %v, dedup_subject = %v", n["action_url"], env["dedup_subject"])
	}
	assertMetadataOnly(t, n, "workflow_id")
	raw, _ := json.Marshal(n)
	for _, leak := range []string{"SMTP refused", "Step ", "relation", "#ops", wsScopeUser} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the notice carries %q from the run: %s", leak, raw)
		}
	}
}

// Finish: a scheduled run whose send failed tells the owner once. The error the
// run recorded (an SMTP server's answer) stays on the run.
func TestWorkflowRunFailedAlert_FinishTellsTheOwner(t *testing.T) {
	const email = "cccccccc-0000-4000-8000-0000000000d2"
	rec := captureRunNotices(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubWorkflowMessages(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, row_count, stop_requested_at IS NOT NULL FROM workflow_runs WHERE id = \$1 FOR UPDATE`).
		WithArgs(wfRunUUID).WillReturnRows(sqlmock.NewRows([]string{"status", "row_count", "stopped"}).
		AddRow("running", 3, false))
	mock.ExpectExec(`UPDATE workflow_sends SET status = 'skipped'`).WithArgs(wfRunUUID, workflowSendSkipError).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE workflow_sends SET status = 'failed'`).WithArgs(wfRunUUID, workflowSendCutOffError).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM workflow_sends WHERE run_id = \$1 ORDER BY position`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "channel_id", "name", "kind", "status", "sent", "error"}).
			AddRow("send_1", wfChannel, "#ops", "slack", "sent", 0, "").
			AddRow("send_1", email, "On-call", "email", "failed", 0, "SMTP refused"))
	mock.ExpectExec(`UPDATE workflow_runs SET status = \$2, deliveries = \$3, error = \$4`).
		WithArgs(wfRunUUID, "failed", sqlmock.AnyArg(), "1 of 2 delivery channels failed.").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectLoadWorkflowRun(mock, "failed", wfDefinitionJSON(workflow.Condition{Type: "always"}, 0))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	if w, out := finishRun(t, ""); w.Code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	assertWorkflowFailureNotice(t, rec.notices(t))
	mockMet(t, mock)
}

// expectCompleteGraphRun answers CompleteWorkflowRunInternal for a run after a
// pipeline whose steps ended with these statuses, and no sends.
func expectCompleteGraphRun(t *testing.T, mock sqlmock.Sqlmock, statuses map[string]string) {
	t.Helper()
	mock.ExpectQuery(`FROM workflow_runs r JOIN workflows w`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "trigger", "started_by", "status", "wid", "workspace_id", "name",
			"wstatus", "definition", "run_as_user_id", "stop_requested"}).
			AddRow(wfRunUUID, workflow.TriggerAfterPipeline, nil, "running", wfID, wsScopeWS, "Nightly load", "active",
				wfAlertDefinitionJSON(t), wsScopeUser, false))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, row_count, stop_requested_at IS NOT NULL FROM workflow_runs WHERE id = \$1 FOR UPDATE`).
		WithArgs(wfRunUUID).WillReturnRows(sqlmock.NewRows([]string{"status", "row_count", "stopped"}).
		AddRow("running", nil, false))
	mock.ExpectExec(`UPDATE workflow_sends SET status = 'skipped'`).WithArgs(wfRunUUID, workflowSendSkipError).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE workflow_sends SET status = 'failed'`).WithArgs(wfRunUUID, workflowSendCutOffError).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE workflow_step_runs SET status = \$2`).WithArgs(wfRunUUID, "failed", workflowStepEndedError, "").
		WillReturnResult(sqlmock.NewResult(0, 0))
	steps := sqlmock.NewRows([]string{"step_id", "status", "error"})
	for _, id := range []string{"load", "refresh", "report", "tell"} {
		if st, ok := statuses[id]; ok {
			steps.AddRow(id, st, "The run failed: relation \"orders\" does not exist")
		}
	}
	mock.ExpectQuery(`SELECT DISTINCT ON \(step_id\) step_id, status`).WithArgs(wfRunUUID).WillReturnRows(steps)
	mock.ExpectQuery(`FROM workflow_sends WHERE run_id = \$1 ORDER BY step_id, position`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "channel_id", "name", "kind", "status", "sent", "error"}))
	mock.ExpectExec(`UPDATE workflow_runs SET status = \$2, deliveries = \$3, error = \$4, finished_at = NOW\(\)`).
		WithArgs(wfRunUUID, "failed", "[]", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// Complete: a run after a pipeline whose model step failed tells the owner (the
// model's own notice is quiet for a workflow's step, model_run_alert.go). One
// whose only failure is its pipeline's run is quiet: that run told the
// pipeline's owner. The second case is the control that the gate reads the step
// complete recorded, not a constant.
func TestWorkflowRunFailedAlert_CompleteTellsTheOwner(t *testing.T) {
	cases := []struct {
		name     string
		statuses map[string]string
		pipeline any // the load step's pipeline run, when the gate reads it
		want     int
	}{
		{"its model step failed", map[string]string{"load": "succeeded", "refresh": "failed", "tell": "skipped",
			"report": "skipped"}, nil, 1},
		{"its pipeline's run failed", map[string]string{"load": "failed", "refresh": "succeeded", "tell": "skipped",
			"report": "skipped"}, "failed", 0},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureRunNotices(t)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			stubWorkflowMessages(t)
			expectCompleteGraphRun(t, mock, tc.statuses)
			if tc.pipeline != nil {
				expectStepPipelineRun(mock, "load", tc.pipeline)
			}
			w, out := callInternal(t, "/workflow-runs/:id/complete", "/workflow-runs/"+wfRunUUID+"/complete",
				CompleteWorkflowRunInternal, map[string]any{})
			if w.Code != http.StatusOK || out["status"] != "failed" {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			if tc.want == 1 {
				assertWorkflowFailureNotice(t, rec.notices(t))
			} else if got := len(rec.notices(t)); got != 0 {
				t.Fatalf("workflow failure notices sent = %d, want 0: %v", got, rec.notices(t))
			}
			mockMet(t, mock)
		})
	}
}

// Evaluate: a scheduled run whose query failed tells the owner; so does one
// whose workflow could not be read at all, keyed by the run like any other. A
// manual run that could not be read is the control: it is quiet.
func TestWorkflowRunFailedAlert_EvaluateTellsTheOwner(t *testing.T) {
	t.Run("the query failed", func(t *testing.T) {
		t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
		rec := captureRunNotices(t)
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		stubWorkflowQuery(t, workflow.Result{}, errors.New(`relation "payments" does not exist`))
		expectLoadWorkflowRun(mock, "running", wfDefinitionJSON(workflow.Condition{Type: "always"}, 0))
		expectNoLedger(mock)
		expectRole(mock, "admin")
		mock.ExpectExec(`UPDATE workflow_runs\s+SET status = \$2`).
			WithArgs(wfRunUUID, "failed", sqlmock.AnyArg(), sqlmock.AnyArg(), "[]", sqlmock.AnyArg(), sqlmock.AnyArg(),
				sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		if w, out := evaluateRun(t); w.Code != http.StatusOK || out["outcome"] != "finished" {
			t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
		}
		assertWorkflowFailureNotice(t, rec.notices(t))
		mockMet(t, mock)
	})

	for _, tc := range []struct {
		trigger string
		want    int
	}{
		{workflow.TriggerSchedule, 1},
		{workflow.TriggerManual, 0},
	} {
		t.Run("the workflow could not be read, trigger "+tc.trigger, func(t *testing.T) {
			rec := captureRunNotices(t)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(`FROM workflow_runs r JOIN workflows w`).WithArgs(wfRunUUID).
				WillReturnRows(sqlmock.NewRows([]string{"id", "trigger", "started_by", "status", "wid", "workspace_id",
					"name", "wstatus", "definition", "run_as_user_id", "stop_requested"}).
					AddRow(wfRunUUID, tc.trigger, nil, "running", wfID, wsScopeWS, "Failed payments", "active",
						"not a definition", wsScopeUser, false))
			mock.ExpectExec(`UPDATE workflow_runs\s+SET status = \$2`).
				WithArgs(wfRunUUID, "failed", sqlmock.AnyArg(), sqlmock.AnyArg(), "[]", sqlmock.AnyArg(),
					"The workflow could not be loaded.", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`SELECT workflow_id::text, trigger FROM workflow_runs WHERE id = \$1`).WithArgs(wfRunUUID).
				WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "trigger"}).AddRow(wfID, tc.trigger))
			if w, out := evaluateRun(t); w.Code != http.StatusOK || out["outcome"] != "finished" {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			if tc.want == 1 {
				assertWorkflowFailureNotice(t, rec.notices(t))
			} else if got := len(rec.notices(t)); got != 0 {
				t.Fatalf("a manual run sent %d notices: %v", got, rec.notices(t))
			}
			mockMet(t, mock)
		})
	}
}

// The gate, case by case. Told: a scheduled or after-pipeline run that failed,
// whatever else failed with it. Quiet (each a control that the gate can say no):
// a run someone started by hand or that did not fail; one whose only failures
// are its pipelines' runs, which told the pipeline's owner; one whose own
// any_failed send went out, which told whoever it was set up to tell. A read
// that fails tells the owner anyway: a notice too many beats one lost.
func TestWorkflowRunFailedAlert_Gate(t *testing.T) {
	failed := func(steps ...string) runOutcome {
		return runOutcome{status: workflow.StatusFailed, failedSteps: steps}
	}
	sent := func(step, status string) runOutcome {
		o := failed("refresh")
		o.deliveries = []DeliveryOutcome{{StepID: step, ChannelID: wfChannel, Name: "#ops", Kind: "slack", Status: status}}
		return o
	}
	cases := []struct {
		name    string
		trigger string
		nilRun  bool
		o       runOutcome
		expect  func(sqlmock.Sqlmock)
		want    int
	}{
		{name: "a scheduled run that failed", trigger: workflow.TriggerSchedule, o: failed("refresh"), want: 1},
		{name: "a run after a pipeline that failed", trigger: workflow.TriggerAfterPipeline, o: failed("refresh"), want: 1},
		{name: "a run that failed with no step to blame", trigger: workflow.TriggerSchedule, o: failed(), want: 1},
		{name: "a manual run", trigger: workflow.TriggerManual, o: failed("refresh"), want: 0},
		{name: "a trigger it does not know", trigger: "", o: failed("refresh"), want: 0},
		{name: "a run that succeeded", trigger: workflow.TriggerSchedule,
			o: runOutcome{status: workflow.StatusSucceeded}, want: 0},
		{name: "a run someone stopped", trigger: workflow.TriggerSchedule,
			o: runOutcome{status: workflow.StatusStopped}, want: 0},
		{name: "no run", nilRun: true, o: failed("refresh"), want: 0},

		{name: "its pipeline's run failed", trigger: workflow.TriggerSchedule, o: failed("load"), want: 0,
			expect: func(m sqlmock.Sqlmock) { expectStepPipelineRun(m, "load", "failed") }},
		{name: "its pipeline's run was cancelled", trigger: workflow.TriggerSchedule, o: failed("load"), want: 0,
			expect: func(m sqlmock.Sqlmock) { expectStepPipelineRun(m, "load", "cancelled") }},
		{name: "its pipeline step failed while the run went on", trigger: workflow.TriggerSchedule, o: failed("load"),
			want: 1, expect: func(m sqlmock.Sqlmock) { expectStepPipelineRun(m, "load", "running") }},
		{name: "its pipeline step never started a run", trigger: workflow.TriggerSchedule, o: failed("load"), want: 1,
			expect: func(m sqlmock.Sqlmock) { expectStepPipelineRun(m, "load", "") }},
		{name: "its pipeline step has no row", trigger: workflow.TriggerSchedule, o: failed("load"), want: 1,
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`FROM workflow_step_runs s`).WithArgs(wfRunUUID, "load").
					WillReturnRows(sqlmock.NewRows([]string{"status"}))
			}},
		{name: "its pipeline's run could not be read", trigger: workflow.TriggerSchedule, o: failed("load"), want: 1,
			expect: func(m sqlmock.Sqlmock) {
				expectStepPipelineRun(m, "load", nil).WillReturnError(errors.New("connection reset"))
			}},
		{name: "its pipeline's run and its model failed", trigger: workflow.TriggerSchedule,
			o: failed("load", "refresh"), want: 1},

		{name: "its any_failed send went out", trigger: workflow.TriggerSchedule, o: sent("tell", deliveryStatusSent),
			want: 0},
		{name: "its any_failed send failed", trigger: workflow.TriggerSchedule, o: sent("tell", deliveryStatusFailed),
			want: 1},
		{name: "an all_success send went out", trigger: workflow.TriggerSchedule, o: sent("report", deliveryStatusSent),
			want: 1},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	quiet := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureRunNotices(t)
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if tc.expect != nil {
				tc.expect(mock)
			}
			var run *workflowRunRow
			if !tc.nilRun {
				run = &workflowRunRow{RunID: wfRunUUID, Trigger: tc.trigger, WorkflowID: wfID, WorkspaceID: wsScopeWS,
					Name: "Nightly load", Definition: wfAlertDefinition(), RunAsUserID: wsScopeUser}
			}
			notifyWorkflowRunFailed(context.Background(), database, run, tc.o)
			if tc.want == 1 {
				assertWorkflowFailureNotice(t, rec.notices(t))
			} else if got := len(rec.notices(t)); got != 0 {
				t.Fatalf("notices sent = %d, want 0: %v", got, rec.notices(t))
			}
			mockMet(t, mock)
		})
		if tc.want == 0 {
			quiet++
		}
	}
	if quiet == 0 || quiet == len(cases) {
		t.Fatalf("%d of %d cases are quiet; the table must hold both answers", quiet, len(cases))
	}
}

// Without a publisher (every binary and test that did not wire one) the gate
// reads nothing: no run's recording waits on a lookup nobody will use.
func TestWorkflowRunFailedAlert_NoPublisherReadsNothing(t *testing.T) {
	prev := currentRunNoticeNotifier()
	SetRunNoticeNotifier(nil)
	t.Cleanup(func() { SetRunNoticeNotifier(prev) })
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run := &workflowRunRow{RunID: wfRunUUID, Trigger: workflow.TriggerSchedule, WorkflowID: wfID,
		Definition: wfAlertDefinition()}
	notifyWorkflowRunFailed(context.Background(), database, run, runOutcome{status: workflow.StatusFailed,
		failedSteps: []string{"load"}})
	notifyUnloadedWorkflowRunFailed(context.Background(), database, wfRunUUID)
	mockMet(t, mock)
}
