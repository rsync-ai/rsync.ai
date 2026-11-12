//go:build integration_pg

// Real-Postgres coverage for a run of the steps graph (build step 5,
// migration 140): the step rows, their attempts, the pipeline runs and model
// runs they link to, the Stop button and the sweep that fails an abandoned
// run. The adapter is played by the test: it calls /graph, each step's
// actions and /complete as runWorkflowGraph does. The pipeline start is
// stubbed (it writes the execution row a real start writes); the gate before
// it, the pipeline stop, the query, the condition and the sends are real.
//
// Same database and command as workflows_pg_test.go.
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

const (
	pgWFPipe2 = "44444444-aaaa-4000-8000-000000000002"
	pgWFModel = "55555555-aaaa-4000-8000-000000000001"
)

// pgStepsFixture is a workflow of the full graph and the stubs it runs with.
type pgStepsFixture struct {
	d       *sql.DB
	r       *gin.Engine
	wf      *Workflow
	opsID   string
	slack   *[]string
	busy    *bool
	starts  *[]string
	stopped *[]string
	sinks   *[]string
}

func pgStepsSetup(t *testing.T) *pgStepsFixture {
	t.Helper()
	d := pgWorkflowDB(t)
	drop := func() {
		for _, q := range []string{
			`DELETE FROM workflows WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM saved_queries WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM pipeline_progress WHERE pipeline_id IN ('` + pgWFPipe + `', '` + pgWFPipe2 + `')`,
			`DELETE FROM executions WHERE pipeline_id IN ('` + pgWFPipe + `', '` + pgWFPipe2 + `')`,
			`DELETE FROM pipelines WHERE id = '` + pgWFPipe2 + `'`,
			`DROP TABLE IF EXISTS wf_model_out`,
		} {
			if _, err := d.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id) VALUES ($1, 'more load', 'x', $2)`,
		pgWFPipe2, pgWFWS)
	pgChkModelRow(t, d, pgWFModel, pgWFConn, "SELECT status, count(*) AS n FROM wf_orders GROUP BY status", "wf_model_out")
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	t.Setenv("PIPELINE_RUN_COOLDOWN_SECONDS", "0")

	f := &pgStepsFixture{d: d, r: pgRouter("admin"), slack: &[]string{}, busy: new(bool), starts: &[]string{},
		stopped: &[]string{}, sinks: &[]string{}}
	f.r.POST("/workflows/:id/runs/:runId/stop", StopWorkflowRun)
	f.r.POST("/internal/workflow-runs/:id/graph", GraphWorkflowRunInternal)
	f.r.POST("/internal/workflow-runs/:id/steps/:step/:action", StepWorkflowRunInternal)
	f.r.POST("/internal/workflow-runs/:id/complete", CompleteWorkflowRunInternal)

	stubWorkflowSchedules(t, nil)
	stubWorkflowMessages(t)
	stubWorkflowResults(t)
	stubStartWorkflowRun(t, nil)
	prevSlack := postSlack
	postSlack = func(_ context.Context, _, text string) error { *f.slack = append(*f.slack, text); return nil }
	t.Cleanup(func() { postSlack = prevSlack })

	// A start writes the execution row and marks the pipeline running, as
	// startPipelineRun does; busy is the 409 a run in progress gets.
	prevStart := startWorkflowStepPipeline
	startWorkflowStepPipeline = func(ctx context.Context, db *sql.DB, id, ws, _ string, _ RunPipelineRequest,
		opts pipelineRunOpts) (string, *pipelineRunRefusal) {
		if *f.busy {
			return "", &pipelineRunRefusal{status: http.StatusConflict, body: gin.H{"error": "pipeline_already_running"}}
		}
		if ws != pgWFWS || !opts.DraftGate || opts.DraftWorkspaceID != pgWFWS {
			return "", &pipelineRunRefusal{status: http.StatusBadRequest, body: gin.H{"error": "unexpected start " + ws}}
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO executions (id, pipeline_id, status, start_time, workflow_run_id) VALUES ($1, $2, 'running', NOW(), $3)
		`, opts.ExecID, id, opts.WorkflowRunID); err != nil {
			return "", &pipelineRunRefusal{status: http.StatusInternalServerError, body: gin.H{"error": err.Error()}}
		}
		if _, err := db.ExecContext(ctx, `UPDATE pipelines SET status = 'running' WHERE id = $1`, id); err != nil {
			return "", &pipelineRunRefusal{status: http.StatusInternalServerError, body: gin.H{"error": err.Error()}}
		}
		*f.starts = append(*f.starts, id+" "+opts.ExecID)
		return opts.ExecID, nil
	}
	t.Cleanup(func() { startWorkflowStepPipeline = prevStart })

	var mu sync.Mutex
	prevSignal := signalWorkflowRunStop
	signalWorkflowRunStop = func(_ context.Context, temporalID string) error {
		mu.Lock()
		defer mu.Unlock()
		*f.stopped = append(*f.stopped, temporalID)
		return nil
	}
	t.Cleanup(func() { signalWorkflowRunStop = prevSignal })

	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		*f.sinks = append(*f.sinks, req.Method+" "+req.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(orch.Close)
	t.Setenv("ORCHESTRATOR_URL", orch.URL)

	if code, body := pgDo(t, f.r, http.MethodPost, "/delivery-channels",
		map[string]any{"name": "#ops", "kind": "slack", "webhook_url": "https://hooks.slack.com/services/T0/B0/wfpg"}); code != 201 {
		t.Fatalf("slack channel: %d %v", code, body)
	}
	_ = d.QueryRow(`SELECT id FROM delivery_channels WHERE workspace_id = $1 AND name = '#ops'`, pgWFWS).Scan(&f.opsID)

	// Two pipelines, one after the other, then the model, then a wait; the
	// query runs after the wait.
	f.wf = pgDraft(t, WorkflowDraftInput{
		Name: "Load then check", ConnectionID: pgWFConn,
		SQL:     "SELECT count(*) AS failed FROM wf_orders WHERE status = 'failed'",
		Trigger: "manual",
		Steps: []WorkflowStepInput{
			{ID: "load_orders", Type: workflow.StepRunPipeline, PipelineID: pgWFPipe, Retries: 1, TimeoutMinutes: 30},
			{ID: "load_more", Type: workflow.StepRunPipeline, PipelineID: pgWFPipe2, After: []string{"load_orders"}},
			{ID: "refresh", Type: workflow.StepRefreshModel, ModelID: pgWFModel, After: []string{"load_more"}},
			{ID: "settle", Type: workflow.StepWait, Minutes: 1, After: []string{"refresh"}},
		},
		Branches: []WorkflowBranchInput{{Condition: "value", ConditionColumn: "failed", ConditionOp: ">", Threshold: "2",
			Subject: "{{failed}} failed", Message: "{{failed}} payments failed", Channels: []string{"ops"}}},
	})
	return f
}

// run starts a Run now run and begins it, as Temporal does.
func (f *pgStepsFixture) run(t *testing.T) string {
	t.Helper()
	runID, err := startManualWorkflowRun(context.Background(), f.d, f.wf.ID, pgWFWS, pgWFUser)
	if err != nil {
		t.Fatal(err)
	}
	if out := pgBegin(t, f.r, f.wf.ID, map[string]any{"run_id": runID, "trigger": "manual"}); out["action"] != "run" {
		t.Fatalf("begin %s: %v", runID, out)
	}
	return runID
}

// step calls one action of one step; it must answer 200.
func (f *pgStepsFixture) step(t *testing.T, runID, stepID, action string, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	code, out := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/steps/"+stepID+"/"+action, body)
	if code != http.StatusOK {
		t.Fatalf("%s %s: %d %v", stepID, action, code, out)
	}
	return out
}

type pgStepRow struct {
	status, execID, modelRunID, errText, note string
	rowCount, caseIndex                       sql.NullInt64
	finished                                  bool
}

func (f *pgStepsFixture) row(t *testing.T, runID, stepID string, attempt int) pgStepRow {
	t.Helper()
	var r pgStepRow
	if err := f.d.QueryRow(`
		SELECT status, COALESCE(execution_id, ''), COALESCE(saved_query_run_id::text, ''), COALESCE(error, ''),
		       COALESCE(note, ''), row_count, case_index, finished_at IS NOT NULL
		FROM workflow_step_runs WHERE run_id = $1 AND step_id = $2 AND attempt = $3
	`, runID, stepID, attempt).Scan(&r.status, &r.execID, &r.modelRunID, &r.errText, &r.note, &r.rowCount, &r.caseIndex,
		&r.finished); err != nil {
		t.Fatalf("step row %s/%d: %v", stepID, attempt, err)
	}
	return r
}

func (f *pgStepsFixture) execStatus(t *testing.T, execID string) (string, string) {
	t.Helper()
	var status, errMsg string
	if err := f.d.QueryRow(`SELECT status, COALESCE(error_message, '') FROM executions WHERE id = $1`, execID).
		Scan(&status, &errMsg); err != nil {
		t.Fatalf("execution %s: %v", execID, err)
	}
	return status, errMsg
}

func TestPG_WorkflowStepsRunEndToEnd(t *testing.T) {
	f := pgStepsSetup(t)
	runID := f.run(t)

	code, graph := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/graph", map[string]any{})
	steps, _ := graph["steps"].([]any)
	if code != http.StatusOK || graph["action"] != "run" || len(steps) != 7 {
		t.Fatalf("graph: %d %v", code, graph)
	}
	if q := steps[4].(map[string]any); q["id"] != "query" || fmt.Sprint(q["after"]) != "[settle]" {
		t.Fatalf("query step = %v", q)
	}

	// load_orders: attempt 1 fails, attempt 2 succeeds. A retried start of
	// the same attempt starts nothing.
	a1 := f.step(t, runID, "load_orders", "start", nil)
	exec1, _ := a1["execution_id"].(string)
	if a1["status"] != "running" || exec1 != workflowStepExecutionID(runID, "load_orders", 1) {
		t.Fatalf("start: %v", a1)
	}
	if again := f.step(t, runID, "load_orders", "start", nil); again["execution_id"] != exec1 || len(*f.starts) != 1 {
		t.Fatalf("start retried: %v, starts %v", again, *f.starts)
	}
	if p := f.step(t, runID, "load_orders", "poll", nil); p["again"] != true || p["status"] != "running" {
		t.Fatalf("poll while running: %v", p)
	}
	wfExec(t, f.d, `UPDATE executions SET status = 'failed', error_message = 'the source refused the login' WHERE id = $1`, exec1)
	if p := f.step(t, runID, "load_orders", "poll", nil); p["status"] != "failed" || p["again"] == true {
		t.Fatalf("poll after failure: %v", p)
	}
	if r := f.row(t, runID, "load_orders", 1); r.status != "failed" || r.errText != "the source refused the login" || !r.finished {
		t.Fatalf("attempt 1 = %+v", r)
	}
	a2 := f.step(t, runID, "load_orders", "start", map[string]any{"attempt": 2})
	exec2, _ := a2["execution_id"].(string)
	if exec2 == "" || exec2 == exec1 {
		t.Fatalf("attempt 2 start: %v", a2)
	}
	if code, out := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/steps/load_orders/start",
		map[string]any{"attempt": 3}); code != http.StatusBadRequest {
		t.Fatalf("attempt 3: %d %v", code, out)
	}
	wfExec(t, f.d, `UPDATE executions SET status = 'completed', end_time = NOW() WHERE id = $1`, exec2)
	if p := f.step(t, runID, "load_orders", "poll", map[string]any{"attempt": 2}); p["status"] != "succeeded" {
		t.Fatalf("attempt 2 poll: %v", p)
	}

	// load_more: the cooldown parks it, then a run in progress does, then it
	// starts its own run.
	t.Setenv("PIPELINE_RUN_COOLDOWN_SECONDS", "3600")
	wfExec(t, f.d, `INSERT INTO executions (pipeline_id, status, start_time, end_time) VALUES ($1, 'completed', NOW(), NOW())`, pgWFPipe2)
	if p := f.step(t, runID, "load_more", "start", nil); p["status"] != "waiting" || p["again"] != true {
		t.Fatalf("start in the cooldown: %v", p)
	}
	if r := f.row(t, runID, "load_more", 1); !strings.Contains(r.note, "started in the last 3600 seconds") || r.execID != "" {
		t.Fatalf("parked by the cooldown = %+v", r)
	}
	t.Setenv("PIPELINE_RUN_COOLDOWN_SECONDS", "0")
	*f.busy = true
	if p := f.step(t, runID, "load_more", "start", nil); p["status"] != "waiting" || p["again"] != true {
		t.Fatalf("start while busy: %v", p)
	}
	if r := f.row(t, runID, "load_more", 1); r.note != workflowStepPipelineBusy {
		t.Fatalf("parked while busy = %+v", r)
	}
	*f.busy = false
	m := f.step(t, runID, "load_more", "start", nil)
	exec3, _ := m["execution_id"].(string)
	if m["status"] != "running" || exec3 == "" {
		t.Fatalf("start when free: %v", m)
	}
	if r := f.row(t, runID, "load_more", 1); r.note != "" {
		t.Fatalf("a started step keeps its waiting note: %+v", r)
	}
	wfExec(t, f.d, `UPDATE executions SET status = 'completed', end_time = NOW() WHERE id = $1`, exec3)
	if p := f.step(t, runID, "load_more", "poll", nil); p["status"] != "succeeded" {
		t.Fatalf("load_more poll: %v", p)
	}

	// refresh: the model's first run since the step asked was skipped as busy,
	// so the step asks again; the next run succeeds with a warning check.
	req := f.step(t, runID, "refresh", "request", nil)
	sig, _ := req["signal"].(map[string]any)
	if req["status"] != "waiting" || sig["saved_query_id"] != pgWFModel || sig["upstream_kind"] != "workflow" ||
		sig["upstream_id"] != f.wf.ID || sig["execution_id"] != runID {
		t.Fatalf("request: %v", req)
	}
	if p := f.step(t, runID, "refresh", "poll", nil); p["again"] != true {
		t.Fatalf("poll before a run: %v", p)
	}
	var skipped string
	if err := f.d.QueryRow(`
		INSERT INTO saved_query_runs (saved_query_id, trigger_source, status, error, started_at, finished_at,
		                              upstream_kind, upstream_id)
		VALUES ($1, 'triggered', 'skipped', $2, NOW(), NOW(), 'workflow', $3) RETURNING run_id::text
	`, pgWFModel, modelRunInProgressError, f.wf.ID).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	p := f.step(t, runID, "refresh", "poll", nil)
	if p["resignal"] != true || p["skipped_run_id"] != skipped {
		t.Fatalf("poll after a busy skip: %v", p)
	}
	if again := f.step(t, runID, "refresh", "request", map[string]any{"resignal": true, "skipped_run_id": skipped}); again["signal"] == nil {
		t.Fatalf("resignal: %v", again)
	}
	if p := f.step(t, runID, "refresh", "poll", nil); p["again"] != true || p["resignal"] == true {
		t.Fatalf("poll after the resignal: %v", p)
	}
	var modelRun string
	if err := f.d.QueryRow(`
		INSERT INTO saved_query_runs (saved_query_id, trigger_source, status, rows_affected, started_at, finished_at,
		                              upstream_kind, upstream_id)
		VALUES ($1, 'triggered', 'succeeded', 3, NOW(), NOW(), 'workflow', $2) RETURNING run_id::text
	`, pgWFModel, f.wf.ID).Scan(&modelRun); err != nil {
		t.Fatal(err)
	}
	wfExec(t, f.d, `INSERT INTO saved_query_check_results (run_id, check_name, check_type, severity, status)
		VALUES ($1, 'rows', 'row_count_change', 'warn', 'fail')`, modelRun)
	if p := f.step(t, runID, "refresh", "poll", nil); p["status"] != "warned" {
		t.Fatalf("poll after the run: %v", p)
	}
	if r := f.row(t, runID, "refresh", 1); r.modelRunID != modelRun || !r.rowCount.Valid || r.rowCount.Int64 != 3 ||
		!strings.Contains(r.note, "warned") {
		t.Fatalf("refresh = %+v", r)
	}

	// settle: the adapter records the wait's start and end.
	if w := f.step(t, runID, "settle", "record", map[string]any{"status": "running", "note": "Waiting 1 minute."}); w["status"] != "waiting" {
		t.Fatalf("wait start: %v", w)
	}
	var alive time.Time
	_ = f.d.QueryRow(`SELECT alive_until FROM workflow_runs WHERE id = $1`, runID).Scan(&alive)
	if time.Until(alive) < 15*time.Minute {
		t.Fatalf("a wait keeps the run alive past its minutes: alive_until in %s", time.Until(alive))
	}
	if w := f.step(t, runID, "settle", "record", map[string]any{"status": "succeeded"}); w["status"] != "succeeded" {
		t.Fatalf("wait end: %v", w)
	}

	// The query, its condition and the send.
	if q := f.step(t, runID, "query", "run", nil); q["status"] != "succeeded" {
		t.Fatalf("query: %v", q)
	}
	if c := f.step(t, runID, "condition", "run", nil); c["status"] != "succeeded" || c["case"] != float64(0) {
		t.Fatalf("condition: %v", c)
	}
	plan := f.step(t, runID, "send_1", "plan", nil)
	chs, _ := plan["channels"].([]any)
	if len(chs) != 1 || chs[0] != f.opsID {
		t.Fatalf("plan: %v", plan)
	}
	if s := f.step(t, runID, "send_1", "send", map[string]any{"channel_id": f.opsID}); s["status"] != "sent" {
		t.Fatalf("send: %v", s)
	}
	if s := f.step(t, runID, "send_1", "finish", nil); s["status"] != "succeeded" {
		t.Fatalf("finish: %v", s)
	}
	if code, done := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/complete", map[string]any{}); code != 200 ||
		done["status"] != "succeeded" {
		t.Fatalf("complete: %d %v", code, done)
	}
	if code, again := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/complete", map[string]any{}); code != 200 ||
		again["status"] != "succeeded" {
		t.Fatalf("complete retried: %d %v", code, again)
	}

	var (
		status string
		met    bool
		rows   int
	)
	if err := f.d.QueryRow(`SELECT status, condition_met, row_count FROM workflow_runs WHERE id = $1`, runID).
		Scan(&status, &met, &rows); err != nil || status != "succeeded" || !met || rows != 1 {
		t.Fatalf("run: %s met=%v rows=%d err=%v", status, met, rows, err)
	}
	if len(*f.slack) != 1 || !strings.HasPrefix((*f.slack)[0], "*3 failed*") {
		t.Fatalf("slack = %q", *f.slack)
	}
	var linked int
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM executions WHERE workflow_run_id = $1`, runID).Scan(&linked); err != nil || linked != 3 {
		t.Fatalf("pipeline runs linked to the workflow run = %d, %v", linked, err)
	}

	// The runs API: each step's last attempt, with its links.
	code, body := pgDo(t, f.r, http.MethodGet, "/workflows/"+f.wf.ID+"/runs", nil)
	runs, _ := body["runs"].([]any)
	if code != 200 || len(runs) != 1 {
		t.Fatalf("runs: %d %v", code, body)
	}
	got := map[string]map[string]any{}
	for _, s := range runs[0].(map[string]any)["steps"].([]any) {
		st := s.(map[string]any)
		got[st["step_id"].(string)] = st
	}
	if len(got) != 7 {
		t.Fatalf("run steps = %v", got)
	}
	if s := got["load_orders"]; s["attempt"] != float64(2) || s["status"] != "succeeded" || s["execution_id"] != exec2 ||
		s["pipeline_id"] != pgWFPipe {
		t.Fatalf("load_orders = %v", s)
	}
	if s := got["load_more"]; s["execution_id"] != exec3 || s["pipeline_id"] != pgWFPipe2 {
		t.Fatalf("load_more = %v", s)
	}
	if s := got["refresh"]; s["status"] != "warned" || s["model_id"] != pgWFModel || s["model_run_id"] != modelRun ||
		s["row_count"] != float64(3) {
		t.Fatalf("refresh = %v", s)
	}
	if s := got["condition"]; s["case"] != float64(0) {
		t.Fatalf("condition = %v", s)
	}
	if s := got["query"]; s["row_count"] != float64(1) {
		t.Fatalf("query = %v", s)
	}

	// The list names each process step's pipeline or model.
	code, body = pgDo(t, f.r, http.MethodGet, "/workflows", nil)
	list, _ := body["workflows"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, body)
	}
	refs, _ := list[0].(map[string]any)["step_refs"].(map[string]any)
	if fmt.Sprint(refs["load_orders"]) != "map[id:"+pgWFPipe+" name:orders load]" ||
		fmt.Sprint(refs["load_more"]) != "map[id:"+pgWFPipe2+" name:more load]" ||
		refs["refresh"].(map[string]any)["id"] != pgWFModel || refs["settle"] != nil {
		t.Fatalf("step_refs = %v", refs)
	}
	// Its last run carries the same steps, which the canvas marks.
	last, _ := list[0].(map[string]any)["last_run"].(map[string]any)
	if lastSteps, _ := last["steps"].([]any); last["id"] != runID || len(lastSteps) != 7 {
		t.Fatalf("last_run = %v", last)
	}
}

func TestPG_WorkflowStepsStop(t *testing.T) {
	f := pgStepsSetup(t)
	runID := f.run(t)
	start := f.step(t, runID, "load_orders", "start", nil)
	execID, _ := start["execution_id"].(string)
	fake := installHITLTemporal(t, []string{execID}, nil)

	if code, body := pgDo(t, f.r, http.MethodPost, "/workflows/"+f.wf.ID+"/runs/"+runID+"/stop", nil); code != http.StatusAccepted ||
		body["status"] != "stopping" {
		t.Fatalf("stop: %d %v", code, body)
	}
	if len(*f.stopped) != 1 || (*f.stopped)[0] != "workflow-run-"+runID {
		t.Fatalf("stop signals = %v", *f.stopped)
	}
	if code, body := pgDo(t, f.r, http.MethodPost, "/workflows/"+f.wf.ID+"/runs/"+runID+"/stop", nil); code != http.StatusConflict ||
		body["error"] != "already_stopping" {
		t.Fatalf("stop again: %d %v", code, body)
	}
	_, body := pgDo(t, f.r, http.MethodGet, "/workflows/"+f.wf.ID+"/runs", nil)
	if r := body["runs"].([]any)[0].(map[string]any); r["stop_requested"] != true || r["status"] != "running" {
		t.Fatalf("run while stopping = %v", r)
	}
	var by string
	if err := f.d.QueryRow(`SELECT stop_requested_by::text FROM workflow_runs WHERE id = $1`, runID).Scan(&by); err != nil || by != pgWFUser {
		t.Fatalf("stop_requested_by = %q, %v", by, err)
	}

	// The step's next call stops it and its pipeline run.
	if p := f.step(t, runID, "load_orders", "poll", nil); p["status"] != "stopped" || p["stop"] != true {
		t.Fatalf("poll after stop: %v", p)
	}
	if st, why := f.execStatus(t, execID); st != "cancelled" || why != "Stopped by its workflow run." {
		t.Fatalf("pipeline run = %s %q", st, why)
	}
	var pipeStatus string
	_ = f.d.QueryRow(`SELECT status FROM pipelines WHERE id = $1`, pgWFPipe).Scan(&pipeStatus)
	if pipeStatus != "stopped" {
		t.Fatalf("pipeline = %s", pipeStatus)
	}
	if fmt.Sprint(fake.delivered()) != "["+execID+" cancel]" {
		t.Fatalf("temporal signals = %v", fake.delivered())
	}
	if fmt.Sprint(*f.sinks) != "[PUT /api/v1/pipelines/"+pgWFPipe+"/sink/stop]" {
		t.Fatalf("sink stops = %v", *f.sinks)
	}
	// A step not yet begun is stopped at its first call.
	if q := f.step(t, runID, "query", "run", nil); q["status"] != "stopped" || q["stop"] != true {
		t.Fatalf("query after stop: %v", q)
	}
	if r := f.row(t, runID, "query", 1); r.status != "stopped" || r.note != workflowStepStoppedNote {
		t.Fatalf("query row = %+v", r)
	}

	if code, done := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/complete", map[string]any{}); code != 200 ||
		done["status"] != "stopped" {
		t.Fatalf("complete: %d %v", code, done)
	}
	var status, errText string
	_ = f.d.QueryRow(`SELECT status, COALESCE(error, '') FROM workflow_runs WHERE id = $1`, runID).Scan(&status, &errText)
	if status != "stopped" || errText != "" || len(*f.slack) != 0 {
		t.Fatalf("run = %s %q, slack %v", status, errText, *f.slack)
	}
	var audits int
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'stop_workflow_run' AND resource_id = $1`, f.wf.ID).
		Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("stop audit rows = %d, %v", audits, err)
	}
	if code, body := pgDo(t, f.r, http.MethodPost, "/workflows/"+f.wf.ID+"/runs/"+runID+"/stop", nil); code != http.StatusConflict ||
		body["error"] != "not_running" {
		t.Fatalf("stop after the run: %d %v", code, body)
	}
	// A step called after the run ended halts.
	if p := f.step(t, runID, "load_more", "start", nil); p["halt"] != true || p["status"] != "stopped" {
		t.Fatalf("start after the run: %v", p)
	}
}

func TestPG_WorkflowStepsTimeoutAndAbandoned(t *testing.T) {
	f := pgStepsSetup(t)
	runID := f.run(t)
	start := f.step(t, runID, "load_orders", "start", nil)
	execID, _ := start["execution_id"].(string)
	installHITLTemporal(t, []string{execID}, nil)

	// The step outlived its 30 minutes: it fails, and so does its pipeline run.
	if s := f.step(t, runID, "load_orders", "stop", map[string]any{"reason": "timeout"}); s["status"] != "failed" || s["stop"] == true {
		t.Fatalf("timeout: %v", s)
	}
	if r := f.row(t, runID, "load_orders", 1); r.errText != "The step did not finish within 30 minutes." {
		t.Fatalf("timed-out step = %+v", r)
	}
	if st, why := f.execStatus(t, execID); st != "cancelled" || why != "The step did not finish within 30 minutes." {
		t.Fatalf("pipeline run = %s %q", st, why)
	}
	if code, out := pgDo(t, f.r, http.MethodPost, "/internal/workflow-runs/"+runID+"/steps/load_orders/stop",
		map[string]any{"reason": "bored"}); code != http.StatusBadRequest {
		t.Fatalf("stop with no reason: %d %v", code, out)
	}

	// The adapter went away with a step running: the sweep fails the run and
	// the step once alive_until has passed, not before.
	f.step(t, runID, "settle", "record", map[string]any{"status": "running"})
	if err := failAbandonedWorkflowRuns(context.Background(), f.d, f.wf.ID); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, runID, "settle", 1); r.status != "waiting" {
		t.Fatalf("swept while alive: %+v", r)
	}
	wfExec(t, f.d, `UPDATE workflow_runs SET alive_until = NOW() - interval '1 minute' WHERE id = $1`, runID)
	if err := failAbandonedWorkflowRuns(context.Background(), f.d, f.wf.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = f.d.QueryRow(`SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status)
	if r := f.row(t, runID, "settle", 1); status != "failed" || r.status != "failed" || r.errText != workflowStepEndedError {
		t.Fatalf("after the sweep: run %s, step %+v", status, r)
	}
	if r := f.row(t, runID, "load_orders", 1); r.errText != "The step did not finish within 30 minutes." {
		t.Fatalf("the sweep rewrote a finished step: %+v", r)
	}
}

// TestPG_WorkflowStepsRefuseWhatAStepCannotRun covers validateProcessStep, the
// check a draft, an edit and turning a workflow on make of each process step:
// a run_pipeline step needs a batch pipeline of the workspace, a
// refresh_model step a model of the workspace. A run_pipeline step checks its
// pipeline again when it starts, since its mode can change after the save.
func TestPG_WorkflowStepsRefuseWhatAStepCannotRun(t *testing.T) {
	f := pgStepsSetup(t)
	const (
		otherWS     = "22222222-aaaa-4000-8000-000000000002"
		cdcPipe     = "44444444-aaaa-4000-8000-000000000003"
		cdcModePipe = "44444444-aaaa-4000-8000-000000000004"
		pollPipe    = "44444444-aaaa-4000-8000-000000000005"
		otherPipe   = "44444444-aaaa-4000-8000-000000000006"
		plainQuery  = "55555555-aaaa-4000-8000-000000000002"
		otherModel  = "55555555-aaaa-4000-8000-000000000003"
	)
	drop := func() {
		for _, q := range []string{
			`DELETE FROM saved_queries WHERE id IN ('` + plainQuery + `', '` + otherModel + `')`,
			`DELETE FROM pipelines WHERE id IN ('` + cdcPipe + `', '` + cdcModePipe + `', '` + pollPipe + `', '` + otherPipe + `')`,
			`DELETE FROM workspaces WHERE id = '` + otherWS + `'`,
		} {
			if _, err := f.d.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	wfExec(t, f.d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Other', 'wf-pg-other', $2)`, otherWS, pgWFUser)
	pipe := `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode, cdc_mode)
		VALUES ($1, $2, 'x', $3, NULLIF($4, ''), NULLIF($5, ''))`
	wfExec(t, f.d, pipe, cdcPipe, "orders stream", pgWFWS, "cdc", "")
	wfExec(t, f.d, pipe, cdcModePipe, "orders tail", pgWFWS, "", "streaming_only")
	wfExec(t, f.d, pipe, pollPipe, "tickets poll", pgWFWS, "api_polling", "")
	wfExec(t, f.d, pipe, otherPipe, "their load", otherWS, "", "")
	query := `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by, materialization, target_table)
		VALUES ($1, $2, $3, $4, 'SELECT 1', $5, $6, NULLIF($7, ''))`
	wfExec(t, f.d, query, plainQuery, pgWFWS, pgWFConn, "a saved query", pgWFUser, "none", "")
	wfExec(t, f.d, query, otherModel, otherWS, pgWFConn, "their model", pgWFUser, "table", "their_out")

	hint := func(_, field string) string { return "Pick another " + field + "." }
	for _, c := range []struct {
		name   string
		step   workflow.Step
		code   string
		status int
		says   string
	}{
		{"a batch pipeline", workflow.Step{Type: workflow.StepRunPipeline, PipelineID: pgWFPipe}, "", 0, ""},
		{"a CDC pipeline", workflow.Step{Type: workflow.StepRunPipeline, PipelineID: cdcPipe},
			"pipeline_not_batch", http.StatusUnprocessableEntity, "orders stream is a CDC (streaming) pipeline"},
		{"a pipeline with a CDC mode", workflow.Step{Type: workflow.StepRunPipeline, PipelineID: cdcModePipe},
			"pipeline_not_batch", http.StatusUnprocessableEntity, "orders tail is a CDC (streaming) pipeline"},
		{"a polling pipeline", workflow.Step{Type: workflow.StepRunPipeline, PipelineID: pollPipe},
			"pipeline_not_batch", http.StatusUnprocessableEntity, "tickets poll is a polling pipeline"},
		{"another workspace's pipeline", workflow.Step{Type: workflow.StepRunPipeline, PipelineID: otherPipe},
			"pipeline_not_found", http.StatusNotFound, "no pipeline " + otherPipe + " in this workspace. Pick another pipeline."},
		{"a model", workflow.Step{Type: workflow.StepRefreshModel, ModelID: pgWFModel}, "", 0, ""},
		{"a saved query that is not a model", workflow.Step{Type: workflow.StepRefreshModel, ModelID: plainQuery},
			"model_not_found", http.StatusNotFound, "no model " + plainQuery + " in this workspace. Pick another model."},
		{"another workspace's model", workflow.Step{Type: workflow.StepRefreshModel, ModelID: otherModel},
			"model_not_found", http.StatusNotFound, "no model " + otherModel},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.step.ID = "the_step"
			refusal, err := validateProcessStep(context.Background(), f.d, pgWFWS, c.step, hint)
			if err != nil {
				t.Fatal(err)
			}
			if c.code == "" {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal)
				}
				return
			}
			if refusal == nil || refusal.Code != c.code || refusal.Status != c.status || refusal.Step != "the_step" ||
				!strings.Contains(refusal.Message, c.says) {
				t.Fatalf("refusal = %+v, want %s %d saying %q", refusal, c.code, c.status, c.says)
			}
		})
	}

	// A draft is refused before anything is saved.
	gin.SetMode(gin.TestMode)
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest(http.MethodPost, "/assistant", nil)
	gc.Set("user_id", pgWFUser)
	gc.Set(ctxWorkspaceID, pgWFWS)
	_, err := ProposeWorkflowDraft(gc, services.Actor{UserID: pgWFUser, WorkspaceID: pgWFWS, Role: security.WSAdmin,
		Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}, WorkflowDraftInput{
		Name: "Stream then check", ConnectionID: pgWFConn, SQL: "SELECT count(*) AS n FROM wf_orders", Trigger: "manual",
		Steps: []WorkflowStepInput{{ID: "stream", Type: workflow.StepRunPipeline, PipelineID: cdcPipe}},
		Branches: []WorkflowBranchInput{{Condition: "always", Subject: "done", Message: "done",
			Channels: []string{"ops"}}},
	})
	var refusal *PlanRefusal
	if !errors.As(err, &refusal) || refusal.Code != "pipeline_not_batch" || refusal.Step != "stream" {
		t.Fatalf("draft with a CDC step: %v", err)
	}
	var drafts int
	_ = f.d.QueryRow(`SELECT count(*) FROM workflows WHERE workspace_id = $1 AND name = 'Stream then check'`, pgWFWS).Scan(&drafts)
	if drafts != 0 {
		t.Fatalf("a refused draft was saved (%d rows)", drafts)
	}

	// A pipeline turned to streaming after the save: its step fails when it
	// starts, and no pipeline run starts.
	runID := f.run(t)
	wfExec(t, f.d, `UPDATE pipelines SET sync_mode = 'cdc' WHERE id = $1`, pgWFPipe2)
	if s := f.step(t, runID, "load_more", "start", nil); s["status"] != "failed" || s["refused"] != true {
		t.Fatalf("start of a step whose pipeline is now CDC: %v", s)
	}
	if r := f.row(t, runID, "load_more", 1); r.status != "failed" || r.execID != "" ||
		!strings.Contains(r.errText, "more load is now a CDC (streaming) pipeline") {
		t.Fatalf("step row = %+v", r)
	}
	if len(*f.starts) != 0 {
		t.Fatalf("a pipeline run started: %v", *f.starts)
	}
}

// TestPG_WorkflowModelOwnership runs modelUpstreamsSQL, the query
// validateModelOwnership asks, on the real schema (migrations 085, 100): a
// model's trigger and the triggers of the models above it, a paused trigger,
// and a hop through another workspace's model, which the fire path does not
// follow either (downstreamModelLookup).
func TestPG_WorkflowModelOwnership(t *testing.T) {
	f := pgStepsSetup(t)
	const (
		otherWS = "22222222-aaaa-4000-8000-000000000003"
		cleanM  = "55555555-aaaa-4000-8000-000000000011" // after "orders load"
		revM    = "55555555-aaaa-4000-8000-000000000012" // after cleanM
		pausedM = "55555555-aaaa-4000-8000-000000000013" // after "more load", paused
		theirsM = "55555555-aaaa-4000-8000-000000000014" // another workspace's, after "more load"
		crossM  = "55555555-aaaa-4000-8000-000000000015" // after theirsM
	)
	drop := func() {
		for _, q := range []string{
			`DELETE FROM saved_queries WHERE id IN ('` + cleanM + `', '` + revM + `', '` + pausedM + `', '` + theirsM +
				`', '` + crossM + `')`,
			`DELETE FROM workspaces WHERE id = '` + otherWS + `'`,
		} {
			if _, err := f.d.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	wfExec(t, f.d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Other', 'wf-pg-own', $2)`, otherWS, pgWFUser)
	model := `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by, materialization, target_table)
		VALUES ($1, $2, $3, $4, 'SELECT 1', $5, 'table', $6)`
	wfExec(t, f.d, model, cleanM, pgWFWS, pgWFConn, "clean orders", pgWFUser, "own_clean")
	wfExec(t, f.d, model, revM, pgWFWS, pgWFConn, "revenue", pgWFUser, "own_revenue")
	wfExec(t, f.d, model, pausedM, pgWFWS, pgWFConn, "paused", pgWFUser, "own_paused")
	wfExec(t, f.d, model, theirsM, otherWS, pgWFConn, "theirs", pgWFUser, "own_theirs")
	wfExec(t, f.d, model, crossM, pgWFWS, pgWFConn, "across", pgWFUser, "own_cross")
	trigger := func(modelID, status, kind, upstream string) {
		var sid string
		if err := f.d.QueryRow(`INSERT INTO saved_query_schedules
			(saved_query_id, schedule_type, schedule_spec, temporal_schedule_id, status, run_as_user_id, created_by)
			VALUES ($1, 'after_upstream', '{}', NULL, $2, $3, $3) RETURNING schedule_id`, modelID, status, pgWFUser).
			Scan(&sid); err != nil {
			t.Fatal(err)
		}
		col := "upstream_pipeline_id"
		if kind == upstreamKindModel {
			col = "upstream_saved_query_id"
		}
		wfExec(t, f.d, `INSERT INTO saved_query_schedule_upstreams (schedule_id, upstream_kind, `+col+`)
			VALUES ($1, $2, $3)`, sid, kind, upstream)
	}
	trigger(cleanM, "active", upstreamKindPipeline, pgWFPipe)
	trigger(revM, "active", upstreamKindModel, cleanM)
	trigger(pausedM, "paused", upstreamKindPipeline, pgWFPipe2)
	trigger(theirsM, "active", upstreamKindPipeline, pgWFPipe2)
	trigger(crossM, "active", upstreamKindModel, theirsM)

	run := func(pipe string) workflow.Step {
		return workflow.Step{ID: "load", Type: workflow.StepRunPipeline, PipelineID: pipe}
	}
	refresh := func(m string) workflow.Step {
		return workflow.Step{ID: "rebuild", Type: workflow.StepRefreshModel, ModelID: m, After: []string{"load"}}
	}
	for _, c := range []struct {
		name  string
		steps []workflow.Step
		says  string
	}{
		{"a model whose trigger is the pipeline a step runs", []workflow.Step{run(pgWFPipe), refresh(cleanM)},
			`model "clean orders" already rebuilds after the pipeline "orders load" finishes, through its own trigger`},
		{"a model two triggers below it", []workflow.Step{run(pgWFPipe), refresh(revM)},
			`model "revenue" already rebuilds after the pipeline "orders load" finishes, through the triggers of the models between them`},
		{"a paused trigger owns nothing", []workflow.Step{run(pgWFPipe2), refresh(pausedM)}, ""},
		{"a hop through another workspace's model is not followed", []workflow.Step{run(pgWFPipe2), refresh(crossM)}, ""},
		{"a model after another pipeline", []workflow.Step{run(pgWFPipe2), refresh(revM)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			def := workflow.Definition{Version: 3, Trigger: workflow.Trigger{Type: workflow.TriggerManual}, Steps: c.steps}
			refusal, err := validateModelOwnership(context.Background(), f.d, pgWFWS, &def)
			if err != nil {
				t.Fatal(err)
			}
			if c.says == "" {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal)
				}
				return
			}
			if refusal == nil || refusal.Code != "model_already_follows" || refusal.Step != "rebuild" ||
				!strings.Contains(refusal.Message, c.says) {
				t.Fatalf("refusal = %+v, want model_already_follows on rebuild saying %q", refusal, c.says)
			}
		})
	}
}
