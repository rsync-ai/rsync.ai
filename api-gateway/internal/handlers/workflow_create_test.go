package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
)

const wfChannel2 = "cccccccc-0000-4000-8000-0000000000d2"

// wfNew is a workflow as a person builds it in the editor: a schedule, a
// query, one condition and its message, sent to one channel.
func wfNew() workflowCreateRequest {
	def := workflow.FromBranches(workflow.Trigger{Type: "schedule", Cron: "0 8 * * *", Timezone: "UTC"},
		workflow.Query{ConnectionID: wfConn,
			SQL: "SELECT count(*) AS failed, sum(amount) AS lost FROM payments WHERE status = 'failed'"},
		[]workflow.Branch{{Condition: workflow.Condition{Type: "always"},
			Message: workflow.Message{Subject: "Failed payments", Body: "{{failed}} failed payments, {{lost}} lost"}}}, 0)
	return workflowCreateRequest{Name: "Failed payments", Definition: def,
		NodeChannelIDs: map[string][]string{"send_1": {wfChannel}}}
}

func createWorkflow(t *testing.T, role string, req any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := wsScopeRouterAsRole(http.MethodPost, "/workflows", CreateWorkflow, role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewReader(body)))
	return w
}

func expectDraftCount(mock sqlmock.Sqlmock, n int) {
	mock.ExpectQuery(wfDraftsSQL).WithArgs(wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
}

// Building a workflow is for owners and admins, as drafting one is; a member
// or a viewer is refused before anything is read.
func TestCreateWorkflow_MemberIsRefused(t *testing.T) {
	for _, role := range []string{"member", "viewer"} {
		mock, cleanup := wsScopeMockDB(t)
		if w := createWorkflow(t, role, wfNew()); w.Code != http.StatusForbidden {
			t.Fatalf("%s: code = %d, body = %s; want 403", role, w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		cleanup()
	}
}

// A workflow built in the editor is a query, its conditions and one message
// for each. A pipeline, model, wait, chart or dashboard step is drafted by Ask
// rsync.ai; it is refused before anything is read.
func TestCreateWorkflow_OnlyAQueryItsConditionsAndTheirMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*workflow.Definition)
	}{
		{name: "a wait step", edit: func(d *workflow.Definition) {
			d.Steps = append(d.Steps, workflow.Step{ID: "pause", Type: workflow.StepWait, After: []string{"send_1"},
				Minutes: 5})
		}},
		{name: "a message that is no condition's", edit: func(d *workflow.Definition) {
			d.Steps = append(d.Steps, workflow.Step{ID: "note", Type: workflow.StepSend, After: []string{"send_1"},
				Message: &workflow.Message{Body: "Sent."}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			req := wfNew()
			tc.edit(&req.Definition)
			w := createWorkflow(t, "admin", req)
			var body struct{ Error, Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusBadRequest || body.Code != "invalid_steps" || !strings.Contains(body.Error, "Ask rsync.ai") {
				t.Fatalf("got %d %q (%s); want 400 invalid_steps naming Ask rsync.ai", w.Code, body.Code, body.Error)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A new workflow gets every check an edit gets, the query run included, and
// is refused in a person's words with a code naming the step at fault, so the
// editor can open it. Nothing is written when it is refused.
func TestCreateWorkflow_RefusesWhatAnEditWould(t *testing.T) {
	conn := func(m sqlmock.Sqlmock) {
		m.ExpectQuery(wfConnSQL).WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	}
	for _, tc := range []struct {
		name     string
		edit     func(*workflowCreateRequest)
		lookups  func(sqlmock.Sqlmock)
		res      workflow.Result
		qerr     error
		code     int
		wantCode string
		want     string
		wantStep string
	}{
		{name: "an empty name", edit: func(r *workflowCreateRequest) { r.Name = "  " }, lookups: func(sqlmock.Sqlmock) {},
			code: http.StatusBadRequest, wantCode: "invalid_name"},
		{name: "a schedule that is not one", edit: func(r *workflowCreateRequest) { r.Definition.Trigger.Cron = "every monday" },
			lookups: func(sqlmock.Sqlmock) {}, code: http.StatusBadRequest, wantCode: "invalid_cron"},
		{name: "SQL that writes", edit: func(r *workflowCreateRequest) {
			r.Definition.SetQuery(workflow.Query{ConnectionID: wfConn, SQL: "DELETE FROM payments"})
		}, lookups: conn, code: http.StatusBadRequest, wantCode: "invalid_sql"},
		{name: "no channel", edit: func(r *workflowCreateRequest) { r.NodeChannelIDs = nil }, lookups: conn,
			code: http.StatusBadRequest, wantCode: "no_channels", wantStep: "send_1"},
		{name: "a channel the workspace lacks", edit: func(*workflowCreateRequest) {},
			lookups: func(m sqlmock.Sqlmock) {
				conn(m)
				m.ExpectQuery(wfChannelsByIDSQL).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}))
			},
			code: http.StatusNotFound, wantCode: "channel_not_found", want: "Pick another", wantStep: "send_1"},
		{name: "a slot the query does not return", edit: func(*workflowCreateRequest) {}, lookups: expectEditLookups,
			res:  workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": int64(2)}}, RowCount: 1},
			code: http.StatusUnprocessableEntity, wantCode: "invalid_message", want: "nothing was saved"},
		{name: "SQL the database rejects", edit: func(*workflowCreateRequest) {}, lookups: expectEditLookups,
			qerr: errors.New(`pq: column "amount" does not exist`),
			code: http.StatusUnprocessableEntity, wantCode: "query_failed", want: `column "amount" does not exist`},
		{name: "a person with as many drafts as one may have", edit: func(*workflowCreateRequest) {},
			lookups: func(m sqlmock.Sqlmock) { expectEditLookups(m); expectDraftCount(m, workflowDraftsMax) },
			res:     wfFailedLost, code: http.StatusTooManyRequests, wantCode: "too_many_drafts", want: "Turn some on or delete them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			stubWorkflowQuery(t, tc.res, tc.qerr)
			tc.lookups(mock)
			req := wfNew()
			tc.edit(&req)

			w := createWorkflow(t, "admin", req)
			var body struct{ Error, Code, Step string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.code || body.Code != tc.wantCode {
				t.Fatalf("got %d %q (%s); want %d %q", w.Code, body.Code, body.Error, tc.code, tc.wantCode)
			}
			if !strings.Contains(body.Error, tc.want) || strings.Contains(body.Error, "list_") {
				t.Errorf("error %q: want %q, in a person's words", body.Error, tc.want)
			}
			if tc.wantStep != "" && body.Step != tc.wantStep {
				t.Errorf("step = %q, want %q", body.Step, tc.wantStep)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A workflow a person built is stored as their draft, proposed by no model,
// with each message's channels, in one transaction, after its query ran once.
// It sends nothing until they turn it on.
func TestCreateWorkflow_StoresADraftAPersonMade(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	schedules := stubWorkflowSchedules(t, nil)
	calls := stubWorkflowQuery(t, wfFailedLost, nil)

	req := wfNew()
	bs := req.Definition.Branches()
	req.Definition.ReplaceBranches([]workflow.Branch{
		{Condition: workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 10},
			Message: workflow.Message{Body: "{{failed}} failed payments: look now"}},
		bs[0],
	})
	// ReplaceBranches gave case 0 the first send; the editor keeps each
	// message's channels with its condition.
	req.NodeChannelIDs = map[string][]string{"send_1": {wfChannel2}, "send_2": {wfChannel}}

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsByIDSQL).WithArgs(wsScopeWS, wfChannel2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel2, "Finance team", "email"))
	mock.ExpectQuery(wfChannelsByIDSQL).WithArgs(wsScopeWS, wfChannel).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "#ops", "slack"))
	expectDraftCount(mock, workflowDraftsMax-1)
	mock.ExpectBegin()
	var stored string
	mock.ExpectQuery(`INSERT INTO workflows`).
		WithArgs(wsScopeWS, "Failed payments", captureJSONArg(&stored), "schedule", wfConn, nullArg{null: true},
			wsScopeUser, "none").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfID))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel2, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_2", wfChannel, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowLoad(mock, "draft")

	w := createWorkflow(t, "owner", req)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Workflow     Workflow `json:"workflow"`
		CanManage    bool     `json:"can_manage"`
		QueryChecked bool     `json:"query_checked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Workflow.ID != wfID || body.Workflow.Status != "draft" || !body.CanManage || !body.QueryChecked || *calls != 1 {
		t.Fatalf("body = %s, query ran %d times", w.Body.String(), *calls)
	}
	var def workflow.Definition
	if err := json.Unmarshal([]byte(stored), &def); err != nil {
		t.Fatal(err)
	}
	got := def.Branches()
	if def.Version != 3 || def.Trigger.Cron != "0 8 * * *" || len(got) != 2 || got[0].Condition.Type != "value" ||
		got[1].Condition.Type != "always" || !def.CaseSend(0).Required || !def.CaseSend(1).Required {
		t.Fatalf("stored definition = %s", stored)
	}
	schedules.want(t) // a draft has no schedule
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// wfShape is a normalized workflow of a query, a condition step with these
// conditions and one send per condition.
func wfShape(t *testing.T, conds ...workflow.Condition) workflow.Definition {
	t.Helper()
	bs := make([]workflow.Branch, len(conds))
	for i, c := range conds {
		bs[i] = workflow.Branch{Condition: c, Message: workflow.Message{Body: "{{failed}} failed"}}
	}
	d := workflow.FromBranches(workflow.Trigger{Type: "schedule", Cron: "0 9 * * 1", Timezone: "UTC"},
		workflow.Query{ConnectionID: wfConn, SQL: "SELECT count(*) AS failed FROM payments"}, bs, 0)
	if err := workflow.Normalize(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// An edit of a workflow that is a query, its conditions and a message for
// each may add, remove or reorder conditions, each condition with its own
// message step, so a message keeps its channels when another condition comes
// or goes. The query and the condition step stay; a message step added is
// required, as one Ask rsync.ai drafts is. A workflow with more steps than
// that still changes its conditions through Ask rsync.ai.
func TestWorkflowEditShapeRefusal_ConditionsComeAndGo(t *testing.T) {
	rows, failed, always := workflow.Condition{Type: "rows_exist"},
		workflow.Condition{Type: "value", Column: "failed", Op: ">", Threshold: 5}, workflow.Condition{Type: "always"}
	have := wfShape(t, rows, always) // send_1 (rows_exist), send_2 (always)
	normalized := func(d workflow.Definition) workflow.Definition {
		t.Helper()
		if err := workflow.Normalize(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// added puts a condition in the middle with a new send, send_3; the
	// always send moves to case 2 and keeps its id.
	added := func() workflow.Definition {
		d := have.Clone()
		c := d.ConditionStep()
		c.Conditions = []workflow.Condition{rows, failed, always}
		two := 2
		d.Step("send_2").Case = &two
		one := 1
		d.Steps = append(d.Steps, workflow.Step{ID: "send_3", Type: workflow.StepSend, After: []string{"condition"},
			Rule: workflow.RuleAllSuccess, Case: &one, Required: true, Message: &workflow.Message{Body: "{{failed}} failed"}})
		return d
	}

	for _, tc := range []struct {
		name string
		got  func() workflow.Definition
	}{
		{name: "a condition added", got: added},
		{name: "a condition removed, its message with it", got: func() workflow.Definition {
			d := have.Clone()
			d.ConditionStep().Conditions = []workflow.Condition{always}
			zero := 0
			d.Step("send_2").Case = &zero
			d.Steps = append(d.Steps[:2], d.Steps[3:]...)
			return d
		}},
		{name: "the conditions as they were", got: have.Clone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := workflowEditShapeRefusal(have, normalized(tc.got())); r != nil {
				t.Fatalf("refused: %+v", r)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		have     workflow.Definition
		got      func() workflow.Definition
		wantCode string
		wantStep string
		want     string
	}{
		{name: "the query step renamed", have: have, got: func() workflow.Definition {
			d := added()
			d.Steps[0].ID = "orders"
			d.ConditionStep().After = []string{"orders"}
			return d
		}, wantCode: "invalid_steps", wantStep: "query"},
		{name: "a message added that a failed delivery would not fail", have: have, got: func() workflow.Definition {
			d := added()
			d.Step("send_3").Required = false
			return d
		}, wantCode: "invalid_steps", wantStep: "send_3"},
		{name: "a kept message made optional", have: have, got: func() workflow.Definition {
			d := have.Clone()
			d.Step("send_1").Required = false
			return d
		}, wantCode: "invalid_steps", wantStep: "send_1"},
		{name: "a step that is no condition's message", have: have, got: func() workflow.Definition {
			d := have.Clone()
			d.Steps = append(d.Steps, workflow.Step{ID: "pause", Type: workflow.StepWait, After: []string{"send_1"}, Minutes: 5})
			return d
		}, wantCode: "invalid_steps", want: "ask rsync.ai"},
		{name: "a condition added to a workflow with a wait step", have: func() workflow.Definition {
			d := have.Clone()
			d.Steps = append(d.Steps, workflow.Step{ID: "pause", Type: workflow.StepWait, After: []string{"send_1"}, Minutes: 5})
			return normalized(d)
		}(), got: func() workflow.Definition {
			d := added()
			d.Steps = append(d.Steps, workflow.Step{ID: "pause", Type: workflow.StepWait, After: []string{"send_1"}, Minutes: 5})
			return d
		}, wantCode: "invalid_branches", want: "ask rsync.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := workflowEditShapeRefusal(tc.have, normalized(tc.got()))
			if r == nil || r.Code != tc.wantCode || r.Status != http.StatusBadRequest || r.Step != tc.wantStep ||
				!strings.Contains(r.Message, tc.want) {
				t.Fatalf("refusal = %+v; want %s on step %q saying %q", r, tc.wantCode, tc.wantStep, tc.want)
			}
		})
	}
}

// An edit that adds a condition saves it with its own message step and that
// step's channels; the message that was there keeps its id and its channels.
func TestUpdateWorkflow_AddsAConditionWithItsMessage(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubWorkflowSchedules(t, nil)
	stubWorkflowQuery(t, wfFailedLost, nil)
	expectWorkflowLoad(mock, "draft")

	edit := wfEdit()
	d := &edit.Definition
	keep := d.Branches()[0]
	d.ConditionStep().Conditions = []workflow.Condition{{Type: "value", Column: "failed", Op: ">", Threshold: 10}, keep.Condition}
	one, zero := 1, 0
	d.Step("send_1").Case = &one
	d.Steps = append(d.Steps, workflow.Step{ID: "send_2", Type: workflow.StepSend, After: []string{"condition"},
		Rule: workflow.RuleAllSuccess, Case: &zero, Required: true,
		Message: &workflow.Message{Body: "{{failed}} failed payments: look now"}})
	edit.ChannelIDs = nil
	edit.NodeChannelIDs = map[string][]string{"send_1": {wfChannel}, "send_2": {wfChannel2}}

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsByIDSQL).WithArgs(wsScopeWS, wfChannel).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "#ops", "slack"))
	mock.ExpectQuery(wfChannelsByIDSQL).WithArgs(wsScopeWS, wfChannel2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel2, "Finance team", "email"))
	mock.ExpectBegin()
	var stored string
	mock.ExpectExec(wfUpdateSQL).WithArgs(wfID, wsScopeWS, "Failed payments", captureJSONArg(&stored), "schedule", wfConn,
		nullArg{null: true}, nullArg{null: true}, nullArg{null: true}, wfUpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM workflow_channels WHERE workflow_id = \$1`).WithArgs(wfID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_2", wfChannel2, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowLoad(mock, "draft")

	w := updateWorkflow(t, "admin", edit)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var def workflow.Definition
	if err := json.Unmarshal([]byte(stored), &def); err != nil {
		t.Fatal(err)
	}
	if bs := def.Branches(); len(bs) != 2 || bs[0].Condition.Type != "value" || def.CaseSend(1).ID != "send_1" ||
		def.CaseSend(0).ID != "send_2" {
		t.Fatalf("stored definition = %s", stored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
