package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
)

const wfChangeID = "ffffffff-0000-4000-8000-0000000000c9"

// The first branch that holds sends, to its own channels and with its own
// message, and the run records which; quiet minutes hold per branch.
func TestEvaluateWorkflowRun_FirstHoldingBranchSends(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
	res := workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": int64(7)}}, RowCount: 1}
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubWorkflowQuery(t, res, nil)
	database := wfDB()

	run := wfRun(workflow.Condition{Type: "always"}, 30)
	run.Definition.ReplaceBranches([]workflow.Branch{
		{Condition: workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 100},
			Message: workflow.Message{Body: "page: {{failed}}"}},
		{Condition: workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 5},
			Message: workflow.Message{Body: "warn: {{failed}}"}},
		{Condition: workflow.Condition{Type: "always"}, Message: workflow.Message{Body: "fine"}},
	})
	expectBranchSend := func(quiet bool) {
		expectRole(mock, "admin")
		mock.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID, wfRunID, 30, "send_2").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(quiet))
		if quiet {
			return
		}
		mock.ExpectQuery(`SELECT dc.id FROM workflow_channels wc`).WithArgs(wsScopeWS, wfID, "send_2").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfChannel))
		mock.ExpectQuery(`FROM delivery_channels WHERE id = \$1 AND workspace_id = \$2`).WithArgs(wfChannel, wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind", "slack_webhook_encrypted", "email_recipients"}).
				AddRow(wfChannel, "#ops", "slack", mustEncrypt(t, "https://hooks.slack.com/services/T0/B0/x"), ""))
	}

	expectBranchSend(false)
	o, plan := evaluateWorkflowRun(context.Background(), database, run)
	if plan == nil || o.branch == nil || *o.branch != 1 || !*o.conditionMet || plan.stepID != "send_2" {
		t.Fatalf("outcome = %+v, plan = %+v", o, plan)
	}
	if len(plan.channels) != 1 || !strings.Contains(plan.msg.Slack, "warn: 7") || strings.Contains(plan.msg.Slack, "page") {
		t.Fatalf("plan = %+v", plan)
	}

	expectBranchSend(true)
	o, plan = evaluateWorkflowRun(context.Background(), database, run)
	if plan != nil || o.branch == nil || *o.branch != 1 || len(o.notes) == 0 ||
		!strings.HasPrefix(o.notes[len(o.notes)-1], "Condition 2 was true, but it sent a message less than 30 minutes ago") {
		t.Fatalf("quiet: outcome = %+v, plan = %+v", o, plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A kept branch keeps what the item leaves unset; a new condition type starts
// afresh. A keep past the end, or a bad threshold, is refused naming the
// branch it is about.
func TestDraftBranches_Keep(t *testing.T) {
	w := &Workflow{Definition: workflow.FromBranches(workflow.Trigger{}, workflow.Query{}, []workflow.Branch{
		{Condition: workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 100},
			Message: workflow.Message{Subject: "S", Body: "B"}},
		{Condition: workflow.Condition{Type: "always"}, Message: workflow.Message{Body: "otherwise"}},
	}, 0)}
	got, refusal := draftBranches([]WorkflowBranchInput{
		{Keep: 1, Threshold: "500"},
		{Keep: 1, Condition: "rows_exist", Message: "rows"},
		{Keep: 2},
	}, w)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	want := []workflow.Branch{
		{Condition: workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 500},
			Message: workflow.Message{Subject: "S", Body: "B"}},
		{Condition: workflow.Condition{Type: "rows_exist"}, Message: workflow.Message{Subject: "S", Body: "rows"}},
		{Condition: workflow.Condition{Type: "always"}, Message: workflow.Message{Body: "otherwise"}},
	}
	if g, wnt := branchesJSON(t, got), branchesJSON(t, want); g != wnt {
		t.Fatalf("branches = %s\nwant %s", g, wnt)
	}
	if w.Definition.Branches()[0].Condition.Threshold != 100 {
		t.Fatal("draftBranches changed the workflow it kept from")
	}

	for _, tc := range []struct {
		in         []WorkflowBranchInput
		code, want string
		branch     int
	}{
		{[]WorkflowBranchInput{{Keep: 3}}, "invalid_branches", "keep 3 names no condition: the workflow has 2", 1},
		{[]WorkflowBranchInput{{Keep: 2}, {Keep: 1, Threshold: "lots"}}, "invalid_threshold", "Condition 2: A value condition", 2},
		{[]WorkflowBranchInput{{Keep: 2}, {Condition: "value", ConditionColumn: "failed", ConditionOp: ">"}},
			"invalid_threshold", "Condition 2: A value condition", 2},
	} {
		if _, r := draftBranches(tc.in, w); r == nil || r.Code != tc.code || r.Branch != tc.branch ||
			!strings.HasPrefix(r.Message, tc.want) || strings.Contains(strings.ToLower(r.Message), "branch") {
			t.Errorf("%+v: refusal = %+v", tc.in, r)
		}
	}
}

func branchesJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// expectWorkflowChange is the stored change wfChangeID: wfEdit's definition and
// channel, proposed at base and expiring at expires.
func expectWorkflowChange(t *testing.T, mock sqlmock.Sqlmock, status string, base, expires time.Time) {
	t.Helper()
	mock.ExpectQuery(`FROM workflow_changes\s+WHERE id = \$1 AND workspace_id = \$2 AND created_by = \$3`).
		WithArgs(wfChangeID, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "actor_kind", "base_updated_at", "definition",
			"node_channel_ids", "summary", "status", "created_at", "expires_at"}).
			AddRow(wfID, "assistant", base, branchesJSON(t, wfEdit().Definition), `{"send_1":["`+wfChannel+`"]}`, "x", status,
				base, expires))
}

func applyWorkflowChange(role string) *httptest.ResponseRecorder {
	r := wsScopeRouterAsRole(http.MethodPost, "/workflow-changes/:id/apply", ApplyWorkflowChange, role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow-changes/"+wfChangeID+"/apply", nil))
	return w
}

// A change is refused once applied, once expired, and once its workflow was
// saved since it was proposed; none of them saves anything.
func TestApplyWorkflowChange_RefusesAClosedChange(t *testing.T) {
	later := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name, status, code string
		base, expires      time.Time
		loadsWorkflow      bool
		http               int
	}{
		{"applied", "applied", "already_applied", wfUpdatedAt, later, false, http.StatusConflict},
		{"expired", "proposed", "expired", wfUpdatedAt, time.Now().Add(-time.Minute), false, http.StatusGone},
		{"stale", "proposed", "workflow_changed", wfUpdatedAt.Add(-time.Minute), later, true, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			calls := stubWorkflowQuery(t, wfFailedLost, nil)
			expectWorkflowChange(t, mock, tc.status, tc.base, tc.expires)
			if tc.loadsWorkflow {
				expectWorkflowLoad(mock, "active")
			}
			w := applyWorkflowChange("admin")
			var body struct{ Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.http || body.Code != tc.code || *calls != 0 {
				t.Fatalf("got %d %s, query ran %d times", w.Code, w.Body.String(), *calls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Applying saves the change as an edit, claims it in the same transaction and
// audits it as apply_workflow_change. A change claimed by another request in
// between rolls the edit back.
func TestApplyWorkflowChange_SavesAndClaims(t *testing.T) {
	for _, claimed := range []bool{true, false} {
		name := "applies"
		if !claimed {
			name = "claimed elsewhere"
		}
		t.Run(name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			calls := stubWorkflowQuery(t, wfFailedLost, nil)
			expectWorkflowChange(t, mock, "proposed", wfUpdatedAt, time.Now().Add(time.Hour))
			expectWorkflowLoad(mock, "active")
			expectEditLookups(mock)
			mock.ExpectBegin()
			mock.ExpectExec(wfUpdateSQL).WillReturnResult(sqlmock.NewResult(0, 1))
			claim := mock.ExpectExec(`UPDATE workflow_changes SET status = 'applied'`).
				WithArgs(wfChangeID, wsScopeWS, wsScopeUser, wfID)
			var details string
			if claimed {
				claim.WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`DELETE FROM workflow_channels WHERE workflow_id = \$1`).WithArgs(wfID).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel, 0).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
				mock.ExpectExec(`INSERT INTO audit_logs`).
					WithArgs(wsScopeUser, "apply_workflow_change", "workflow", wfID, captureJSONArg(&details),
						sqlmock.AnyArg(), wsScopeWS).
					WillReturnResult(sqlmock.NewResult(0, 1))
				expectWorkflowLoad(mock, "active")
			} else {
				claim.WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectRollback()
			}

			w := applyWorkflowChange("admin")
			if claimed && (w.Code != http.StatusOK || *calls != 1 || !strings.Contains(details, `"change_id":"`+wfChangeID+`"`) ||
				strings.Contains(details, "payments")) {
				t.Fatalf("apply: %d %s, query ran %d times, audit %s", w.Code, w.Body.String(), *calls, details)
			}
			if !claimed && (w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"change_changed"`)) {
				t.Fatalf("claimed elsewhere: %d %s", w.Code, w.Body.String())
			}
			if claimed {
				schedules.want(t, upserted("0 8 * * *"))
			} else {
				schedules.want(t)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Applying is for owners and admins: a member is refused before anything is
// read.
func TestApplyWorkflowChange_MemberIsRefused(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	if w := applyWorkflowChange("member"); w.Code != http.StatusForbidden {
		t.Fatalf("member: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
