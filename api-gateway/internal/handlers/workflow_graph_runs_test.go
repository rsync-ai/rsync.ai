package handlers

// A graph run's steps (workflow_graph_runs.go): what the result store keeps,
// what the adapter is told, and the calls no retry fixes. The SQL each step
// runs is proved on Postgres (workflow_graph_runs_pg_test.go).

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeWorkflowResults holds results in memory, as Redis would.
type fakeWorkflowResults struct {
	held    map[string]workflow.Result
	ttls    map[string]time.Duration
	deleted []string
}

func (f *fakeWorkflowResults) Put(_ context.Context, key string, r workflow.Result, ttl time.Duration) error {
	raw, err := encodeWorkflowResult(r)
	if err != nil {
		return err
	}
	back, err := decodeWorkflowResult(raw)
	if err != nil {
		return err
	}
	f.held[key], f.ttls[key] = back, ttl
	return nil
}

func (f *fakeWorkflowResults) Get(_ context.Context, key string) (workflow.Result, bool, error) {
	r, ok := f.held[key]
	return r, ok, nil
}

func (f *fakeWorkflowResults) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.held, key)
	return nil
}

func stubWorkflowResults(t *testing.T) *fakeWorkflowResults {
	t.Helper()
	f := &fakeWorkflowResults{held: map[string]workflow.Result{}, ttls: map[string]time.Duration{}}
	prev := workflowResults
	workflowResults = f
	t.Cleanup(func() { workflowResults = prev })
	return f
}

func intp(i int) *int { return &i }

// wfStepsDefinition is a full graph: two pipelines, a model, a wait, then the
// query, its condition and a send for its first case.
func wfStepsDefinition() workflow.Definition {
	return workflow.Definition{
		Version: 3,
		Trigger: workflow.Trigger{Type: "manual"},
		Steps: []workflow.Step{
			{ID: "load_orders", Type: workflow.StepRunPipeline, PipelineID: wfPipe, Retries: 2, TimeoutMinutes: 30},
			{ID: "refresh", Type: workflow.StepRefreshModel, After: []string{"load_orders"},
				ModelID: "77777777-0000-4000-8000-000000000001"},
			{ID: "settle", Type: workflow.StepWait, After: []string{"refresh"}, Minutes: 5},
			{ID: "query", Type: workflow.StepQuery, After: []string{"settle"}, ConnectionID: wfConn,
				SQL: "SELECT count(*) AS failed FROM payments"},
			{ID: "check", Type: workflow.StepCondition, After: []string{"query"},
				Conditions: []workflow.Condition{{Type: "value", Op: ">", Threshold: 50}}},
			{ID: "send_1", Type: workflow.StepSend, After: []string{"check"}, Case: intp(0),
				Message: &workflow.Message{Subject: "Failed payments", Body: "{{failed}} failed"}},
		},
	}
}

func wfStepsDefinitionJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(wfStepsDefinition())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// The result store
// ---------------------------------------------------------------------------

// A result comes back as a condition and a message read it: a BIGINT keeps
// every digit, a float JSON cannot hold and bytes come back as text, and a
// time as a message shows it.
func TestWorkflowResult_RoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, time.FixedZone("IST", 19800))
	raw, err := encodeWorkflowResult(workflow.Result{
		Columns: []string{"big", "nan", "inf", "raw", "at", "n"},
		Rows: []map[string]any{{"big": int64(9007199254740993), "nan": math.NaN(), "inf": math.Inf(1),
			"raw": []byte("abc"), "at": at, "n": nil}},
		RowCount: 1, Truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeWorkflowResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	row := r.Rows[0]
	if row["big"] != json.Number("9007199254740993") {
		t.Fatalf("big = %#v", row["big"])
	}
	if row["nan"] != "NaN" || row["inf"] != "+Inf" || row["raw"] != "abc" {
		t.Fatalf("row = %#v", row)
	}
	if row["at"] != "2026-10-04 04:00:00 UTC" || row["n"] != nil {
		t.Fatalf("row = %#v", row)
	}
	if r.RowCount != 1 || !r.Truncated || !reflect.DeepEqual(r.Columns, []string{"big", "nan", "inf", "raw", "at", "n"}) {
		t.Fatalf("result = %#v", r)
	}
}

// A condition reads the stored number as a number.
func TestWorkflowResult_ConditionReadsStoredNumber(t *testing.T) {
	raw, _ := encodeWorkflowResult(workflow.Result{Columns: []string{"failed"},
		Rows: []map[string]any{{"failed": int64(51)}}, RowCount: 1})
	r, err := decodeWorkflowResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := workflow.Evaluate(workflow.Condition{Type: "value", Op: ">", Threshold: 50}, r)
	if err != nil || !out.Met {
		t.Fatalf("evaluate = %+v, %v", out, err)
	}
}

// The result outlasts every step that can run before its last reader: each
// pipeline's timeout once per attempt, each model's timeout, each wait.
func TestWorkflowResultTTL(t *testing.T) {
	// 60 + 30×3 (load_orders) + 720 (refresh) + 5 (settle)
	if got, want := workflowResultTTL(wfStepsDefinition()), 875*time.Minute; got != want {
		t.Fatalf("ttl = %v, want %v", got, want)
	}
	var plain workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &plain)
	if got := workflowResultTTL(plain); got != time.Hour {
		t.Fatalf("a run with no process step: ttl = %v", got)
	}
}

// ---------------------------------------------------------------------------
// /graph
// ---------------------------------------------------------------------------

// The adapter gets each step's place and limits, never its SQL or message.
func TestGraphSteps(t *testing.T) {
	got := graphSteps(wfStepsDefinition())
	want := []graphStep{
		{ID: "load_orders", Type: workflow.StepRunPipeline, TimeoutMinutes: 30, Retries: 2},
		{ID: "refresh", Type: workflow.StepRefreshModel, After: []string{"load_orders"},
			TimeoutMinutes: workflow.DefaultStepTimeoutMinutes},
		{ID: "settle", Type: workflow.StepWait, After: []string{"refresh"}, Minutes: 5},
		{ID: "query", Type: workflow.StepQuery, After: []string{"settle"}},
		{ID: "check", Type: workflow.StepCondition, After: []string{"query"}},
		{ID: "send_1", Type: workflow.StepSend, After: []string{"check"}, Case: intp(0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("graph = %+v", got)
	}
	raw, _ := json.Marshal(got)
	for _, leak := range []string{"payments", "Failed payments", wfConn, wfPipe, "77777777"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the graph carries %q: %s", leak, raw)
		}
	}
}

func TestGraphWorkflowRun_EndedRunHasNothingToRun(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLoadWorkflowRun(mock, "succeeded", wfStepsDefinitionJSON(t))

	w, out := callInternal(t, "/workflow-runs/:id/graph", "/workflow-runs/"+wfRunUUID+"/graph",
		GraphWorkflowRunInternal, map[string]string{})
	if w.Code != http.StatusOK || out["action"] != "none" {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	mockMet(t, mock)
}

func TestGraphWorkflowRun_RunningRunGetsItsSteps(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLoadWorkflowRun(mock, "running", wfStepsDefinitionJSON(t))
	mock.ExpectExec(`UPDATE workflow_runs\s+SET alive_until = GREATEST`).WithArgs(wfRunUUID, workflowStepAliveMinutes).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w, out := callInternal(t, "/workflow-runs/:id/graph", "/workflow-runs/"+wfRunUUID+"/graph",
		GraphWorkflowRunInternal, map[string]string{})
	steps, _ := out["steps"].([]any)
	if w.Code != http.StatusOK || out["action"] != "run" || len(steps) != 6 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "payments") {
		t.Fatalf("the graph carries the SQL: %s", w.Body.String())
	}
	mockMet(t, mock)
}

// ---------------------------------------------------------------------------
// /steps/:step/:action — the calls no retry fixes
// ---------------------------------------------------------------------------

func stepCall(t *testing.T, step, action string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callInternal(t, "/workflow-runs/:id/steps/:step/:action",
		"/workflow-runs/"+wfRunUUID+"/steps/"+step+"/"+action, StepWorkflowRunInternal, body)
}

func TestGraphStepTakes(t *testing.T) {
	for _, tc := range []struct {
		typ, action string
		takes       bool
	}{
		{workflow.StepQuery, "run", true},
		{workflow.StepQuery, "poll", false},
		{workflow.StepCondition, "run", true},
		{workflow.StepChart, "run", true},
		{workflow.StepChart, "plan", false},
		{workflow.StepDashboard, "run", true},
		{workflow.StepDashboard, "plan", false},
		{workflow.StepSend, "plan", true},
		{workflow.StepSend, "send", true},
		{workflow.StepSend, "finish", true},
		{workflow.StepSend, "run", false},
		{workflow.StepRunPipeline, "start", true},
		{workflow.StepRunPipeline, "poll", true},
		{workflow.StepRunPipeline, "request", false},
		{workflow.StepRefreshModel, "request", true},
		{workflow.StepRefreshModel, "poll", true},
		{workflow.StepRefreshModel, "start", false},
		{workflow.StepWait, "record", true},
		{workflow.StepWait, "stop", true},
		{workflow.StepWait, "poll", false},
		{"nope", "run", false},
	} {
		if got := graphStepTakes(tc.typ, tc.action); got != tc.takes {
			t.Errorf("%s %s = %v, want %v", tc.typ, tc.action, got, tc.takes)
		}
	}
}

// Any step can be skipped or failed by the adapter; only a wait records its
// own start and end.
func TestRecordRefusal(t *testing.T) {
	for _, tc := range []struct {
		typ, status string
		ok          bool
	}{
		{workflow.StepSend, workflow.StatusSkipped, true},
		{workflow.StepRunPipeline, workflow.StatusFailed, true},
		{workflow.StepWait, workflow.StatusRunning, true},
		{workflow.StepWait, workflow.StatusSucceeded, true},
		{workflow.StepRunPipeline, workflow.StatusSucceeded, false},
		{workflow.StepQuery, workflow.StatusRunning, false},
		{workflow.StepWait, workflow.StatusWarned, false},
		{workflow.StepWait, workflow.StatusStopped, false},
		{workflow.StepWait, "", false},
	} {
		s := &graphStepCall{step: workflow.Step{Type: tc.typ}, req: graphStepRequest{Status: tc.status}}
		if got := s.recordRefusal() == ""; got != tc.ok {
			t.Errorf("%s records %q = %v, want %v", tc.typ, tc.status, got, tc.ok)
		}
	}
}

func TestStepWorkflowRun_RefusesWhatNoRetryFixes(t *testing.T) {
	for _, tc := range []struct {
		name, step, action string
		body               any
		want               string
	}{
		{"unknown step", "nope", "run", map[string]any{}, "the run has no such step"},
		{"action the type does not take", "query", "poll", map[string]any{}, `a query step does not take "poll"`},
		{"attempt past the retries", "load_orders", "start", map[string]any{"attempt": 4}, "attempt must be 1 to 3"},
		{"a second attempt of a model", "refresh", "request", map[string]any{"attempt": 2}, "attempt must be 1 to 1"},
		{"a send with no channel", "send_1", "send", map[string]any{}, "invalid channel_id"},
		{"a record the type cannot make", "load_orders", "record", map[string]any{"status": "succeeded"},
			`a run_pipeline step cannot record "succeeded"`},
		{"a stop with no reason", "settle", "stop", map[string]any{}, "reason must be stop or timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectLoadWorkflowRun(mock, "running", wfStepsDefinitionJSON(t))

			w, out := stepCall(t, tc.step, tc.action, tc.body)
			if w.Code != http.StatusBadRequest || out["error"] != tc.want {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			// Refused before the run is touched: no write was made.
			mockMet(t, mock)
		})
	}
}

func TestStepWorkflowRun_BadBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := wsScopeRouter(http.MethodPost, "/workflow-runs/:id/steps/:step/:action", StepWorkflowRunInternal)
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow-runs/"+wfRunUUID+"/steps/query/run",
		strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// A run that ended, or was deleted, runs nothing more.
func TestStepWorkflowRun_HaltsAnEndedRun(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLoadWorkflowRun(mock, "stopped", wfStepsDefinitionJSON(t))
	w, out := stepCall(t, "load_orders", "start", map[string]any{"attempt": 1})
	if w.Code != http.StatusOK || out["halt"] != true || out["status"] != "stopped" {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	mock.ExpectQuery(`FROM workflow_runs r JOIN workflows w`).WithArgs(wfRunUUID).WillReturnError(sql.ErrNoRows)
	w, out = stepCall(t, "load_orders", "start", map[string]any{"attempt": 1})
	if w.Code != http.StatusOK || out["halt"] != true {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	mockMet(t, mock)
}

func TestStepWorkflowRun_BadRunID(t *testing.T) {
	w, _ := callInternal(t, "/workflow-runs/:id/steps/:step/:action", "/workflow-runs/nope/steps/query/run",
		StepWorkflowRunInternal, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// One attempt of one step starts one pipeline run, however often its start
// is retried; the next attempt starts another.
func TestWorkflowStepExecutionID(t *testing.T) {
	a := workflowStepExecutionID(wfRunUUID, "load_orders", 1)
	if a != workflowStepExecutionID(wfRunUUID, "load_orders", 1) {
		t.Fatal("the same attempt gave two ids")
	}
	for _, other := range []string{
		workflowStepExecutionID(wfRunUUID, "load_orders", 2),
		workflowStepExecutionID(wfRunUUID, "load_more", 1),
		workflowStepExecutionID("ffffffff-0000-4000-8000-0000000000a2", "load_orders", 1),
	} {
		if other == a {
			t.Fatalf("two attempts share the id %s", a)
		}
	}
}

func TestRowCountParam(t *testing.T) {
	if got := rowCountParam(sql.NullInt64{Int64: 42, Valid: true}); got != int64(42) {
		t.Fatalf("42 = %#v", got)
	}
	for _, n := range []sql.NullInt64{{}, {Int64: math.MaxInt32 + 1, Valid: true}, {Int64: math.MinInt32 - 1, Valid: true}} {
		if got := rowCountParam(n); got != (sql.NullInt64{}) {
			t.Fatalf("%v = %#v, want NULL", n, got)
		}
	}
}

// The run's error names the first step, in the workflow's order, whose
// failure failed the run; a send that is not required does not.
func TestFirstFailedStep(t *testing.T) {
	d := wfStepsDefinition()
	statuses := map[string]string{"load_orders": "succeeded", "refresh": "failed", "send_1": "failed"}
	errs := map[string]string{"refresh": " The model's run failed. ", "send_1": "1 of 1 delivery channels failed."}
	if got := firstFailedStep(d, statuses, errs); got != "Step refresh failed: The model's run failed." {
		t.Fatalf("error = %q", got)
	}
	if got := firstFailedStep(d, map[string]string{"send_1": "failed"}, errs); got != workflowStepFailedError {
		t.Fatalf("a send that is not required failed the run: %q", got)
	}
	d.Steps[5].Required = true
	if got := firstFailedStep(d, map[string]string{"send_1": "failed"}, map[string]string{}); got != "Step send_1 failed." {
		t.Fatalf("error = %q", got)
	}
}

// ---------------------------------------------------------------------------
// The Stop button
// ---------------------------------------------------------------------------

func TestStopWorkflowRun_RefusesWhatIsNotARun(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	for _, path := range []string{"/workflows/nope/runs/" + wfRunUUID + "/stop", "/workflows/" + wfID + "/runs/nope/stop"} {
		r := wsScopeRouter(http.MethodPost, "/workflows/:id/runs/:runId/stop", StopWorkflowRun)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: code = %d, body = %s", path, w.Code, w.Body.String())
		}
	}
	mockMet(t, mock)
}

// Only an owner or admin stops a run.
func TestStopWorkflowRun_EditorCannotStop(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	r := wsScopeRouterAsRole(http.MethodPost, "/workflows/:id/runs/:runId/stop", StopWorkflowRun, "editor")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows/"+wfID+"/runs/"+wfRunUUID+"/stop", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	mockMet(t, mock)
}

func TestStopWorkflowRun_SignalsTheRun(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	var signalled []string
	prev := signalWorkflowRunStop
	signalWorkflowRunStop = func(_ context.Context, id string) error {
		signalled = append(signalled, id)
		return nil
	}
	t.Cleanup(func() { signalWorkflowRunStop = prev })
	mock.ExpectQuery(`UPDATE workflow_runs SET stop_requested_at = NOW\(\)`).
		WithArgs(wfRunUUID, wfID, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_workflow_id"}).AddRow("workflow-run-" + wfRunUUID))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := wsScopeRouter(http.MethodPost, "/workflows/:id/runs/:runId/stop", StopWorkflowRun)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows/"+wfID+"/runs/"+wfRunUUID+"/stop", nil))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"stopping"`) {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(signalled, []string{"workflow-run-" + wfRunUUID}) {
		t.Fatalf("signalled = %v", signalled)
	}
}
