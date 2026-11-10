//go:build integration_pg

// Real-Postgres coverage for workflows (migrations 130, 131, 133, 139): the SQL
// that sqlmock cannot judge. The partial unique index that makes an
// after-pipeline event fire once, the Temporal workflow id that makes a
// scheduled start insert one run, the sends ledger and its claim, the array
// and JSONB round trips, the RESTRICT that keeps a used channel, and a whole
// run through the steps WorkflowRunWorkflow calls (begin, evaluate, send,
// finish): the query runs through the explorer's real Postgres executor
// against the same database, the condition holds, the message goes to a
// stubbed Slack transport, and the run and its audit row are recorded.
// Temporal itself is stubbed: its schedules, its starts, and the message it
// would find in Redis.
//
// Needs a Postgres with every api-gateway migration applied (the test applies
// them itself) and nothing else in it:
//
//	docker run -d --name wf-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55471:5432 postgres:16-alpine
//	WF_PG_DSN='postgres://postgres:verify@localhost:55471/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Workflow -v
package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/notifier"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgWFUser = "11111111-aaaa-4000-8000-000000000001"
	pgWFWS   = "22222222-aaaa-4000-8000-000000000001"
	pgWFConn = "33333333-aaaa-4000-8000-000000000001"
	pgWFPipe = "44444444-aaaa-4000-8000-000000000001"
)

func pgWorkflowDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WF_PG_DSN")
	if dsn == "" {
		t.Skip("WF_PG_DSN not set — see the file header")
	}
	d, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = d
	t.Cleanup(func() { db.DB = prev; _ = d.Close() })
	if err := db.Migrate("../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-wf-pg-0123456789")
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("RESEND_FROM_ADDR", "")
	t.Setenv("SMTP_HOST", "")
	t.Setenv("APP_BASE_URL", "https://app.example")
	notifier.InvalidateChannelCache()

	cfg, _ := json.Marshal(map[string]any{"host": "localhost", "port": portOf(dsn), "user": "postgres",
		"password": "verify", "database": "cplane"})
	enc, err := crypto.EncryptString(string(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM workflows WHERE workspace_id = '` + pgWFWS + `'`,
		`DELETE FROM delivery_channels WHERE workspace_id = '` + pgWFWS + `'`,
		// executions' pipeline key has no cascade (001 wins over 021b).
		`DELETE FROM executions WHERE pipeline_id = '` + pgWFPipe + `'`,
		`DELETE FROM pipelines WHERE id = '` + pgWFPipe + `'`,
		`DELETE FROM connections WHERE id = '` + pgWFConn + `'`,
		`DELETE FROM workspace_members WHERE workspace_id = '` + pgWFWS + `'`,
		`DELETE FROM audit_logs WHERE workspace_id = '` + pgWFWS + `'`,
		`DELETE FROM workspaces WHERE id = '` + pgWFWS + `'`,
		`DELETE FROM users WHERE id = '` + pgWFUser + `'`,
		`DROP TABLE IF EXISTS wf_orders`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'owner@example.com', 'x')`, pgWFUser)
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'WF', 'wf-pg', $2)`, pgWFWS, pgWFUser)
	wfExec(t, d, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, pgWFWS, pgWFUser)
	wfExec(t, d, `INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id)
		VALUES ($1, $2, 'warehouse', 'destination', 'postgresql', $3, $4)`, pgWFConn, pgWFUser, enc, pgWFWS)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id) VALUES ($1, 'orders load', 'x', $2)`,
		pgWFPipe, pgWFWS)
	wfExec(t, d, `CREATE TABLE wf_orders (id int, status text, total numeric(10,2), email text)`)
	wfExec(t, d, `INSERT INTO wf_orders VALUES (1,'failed',10.5,'a@x.io'),(2,'failed',20,'b@x.io'),(3,'paid',30,'c@x.io'),(4,'failed',5,'d@x.io')`)
	return d
}

func portOf(dsn string) int {
	p := 0
	if i := strings.Index(dsn, "localhost:"); i >= 0 {
		for _, ch := range dsn[i+len("localhost:"):] {
			if ch < '0' || ch > '9' {
				break
			}
			p = p*10 + int(ch-'0')
		}
	}
	if p == 0 {
		return 5432
	}
	return p
}

func wfExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func pgRouter(role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgWFUser)
		c.Set(ctxWorkspaceID, pgWFWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.POST("/delivery-channels", CreateDeliveryChannel)
	r.DELETE("/delivery-channels/:id", DeleteDeliveryChannel)
	r.GET("/workflows", ListWorkflows)
	r.POST("/workflows/:id/test", TestWorkflow)
	r.POST("/workflows/:id/activate", ActivateWorkflow)
	r.PUT("/workflows/:id", UpdateWorkflow)
	r.GET("/workflows/:id/runs", ListWorkflowRuns)
	r.DELETE("/workflows/:id", DeleteWorkflow)
	r.GET("/workflow-changes/:id", GetWorkflowChange)
	r.POST("/workflow-changes/:id/apply", ApplyWorkflowChange)
	r.POST("/internal/workflows/:id/runs/begin", BeginWorkflowRunInternal)
	r.POST("/internal/workflow-runs/:id/evaluate", EvaluateWorkflowRunInternal)
	r.POST("/internal/workflow-runs/:id/send", SendWorkflowRunInternal)
	r.POST("/internal/workflow-runs/:id/finish", FinishWorkflowRunInternal)
	return r
}

// pgBegin is a run's first step; it must answer.
func pgBegin(t *testing.T, r *gin.Engine, workflowID string, body map[string]any) map[string]any {
	t.Helper()
	code, out := pgDo(t, r, http.MethodPost, "/internal/workflows/"+workflowID+"/runs/begin", body)
	if code != http.StatusOK {
		t.Fatalf("begin: %d %v", code, out)
	}
	return out
}

// pgScheduled is the begin of the run Temporal's schedule starts at at.
func pgScheduled(workflowID string, at time.Time) map[string]any {
	return map[string]any{"trigger": "schedule",
		"temporal_workflow_id": "workflow-run:" + workflowID + ":schedule-" + at.UTC().Format(time.RFC3339)}
}

// pgRunSteps takes a begun run through the rest of WorkflowRunWorkflow:
// evaluate, a send per planned channel in order, finish. Each send is asked
// twice, as Temporal retries one whose answer was lost: the retry gets the
// same answer and sends nothing (the tests count what reached Slack).
func pgRunSteps(t *testing.T, r *gin.Engine, runID string) {
	t.Helper()
	code, out := pgDo(t, r, http.MethodPost, "/internal/workflow-runs/"+runID+"/evaluate", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("evaluate: %d %v", code, out)
	}
	if out["outcome"] == "finished" {
		return
	}
	channels, _ := out["channels"].([]any)
	for _, ch := range channels {
		code, sent := pgDo(t, r, http.MethodPost, "/internal/workflow-runs/"+runID+"/send", map[string]any{"channel_id": ch})
		if code != http.StatusOK {
			t.Fatalf("send to %v: %d %v", ch, code, sent)
		}
		if code, again := pgDo(t, r, http.MethodPost, "/internal/workflow-runs/"+runID+"/send",
			map[string]any{"channel_id": ch}); code != http.StatusOK || again["status"] != sent["status"] {
			t.Fatalf("send to %v retried: %d %v, first %v", ch, code, again, sent)
		}
	}
	if code, done := pgDo(t, r, http.MethodPost, "/internal/workflow-runs/"+runID+"/finish", map[string]any{}); code != http.StatusOK {
		t.Fatalf("finish: %d %v", code, done)
	}
}

// pgRunNow is Run now: the run is inserted and handed to (a stubbed)
// Temporal, which begins it by id and runs its steps.
func pgRunNow(t *testing.T, r *gin.Engine, d *sql.DB, workflowID string) string {
	t.Helper()
	runID, err := startManualWorkflowRun(context.Background(), d, workflowID, pgWFWS, pgWFUser)
	if err != nil {
		t.Fatal(err)
	}
	if out := pgBegin(t, r, workflowID, map[string]any{"run_id": runID, "trigger": "manual"}); out["action"] != "run" {
		t.Fatalf("begin %s: %v", runID, out)
	}
	pgRunSteps(t, r, runID)
	return runID
}

func pgDo(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, &buf))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func pgDraft(t *testing.T, in WorkflowDraftInput) *Workflow {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/assistant", nil)
	c.Set("user_id", pgWFUser)
	c.Set(ctxWorkspaceID, pgWFWS)
	actor := services.Actor{UserID: pgWFUser, WorkspaceID: pgWFWS, Role: security.WSAdmin,
		Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}
	w, err := ProposeWorkflowDraft(c, actor, in)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	return w
}

func TestPG_WorkflowEndToEnd(t *testing.T) {
	d := pgWorkflowDB(t)
	r := pgRouter("admin")
	schedules := stubWorkflowSchedules(t, nil)
	stubWorkflowMessages(t)
	var slack []string
	prevSlack := postSlack
	postSlack = func(_ context.Context, url, text string) error { slack = append(slack, text); return nil }
	t.Cleanup(func() { postSlack = prevSlack })

	if code, body := pgDo(t, r, http.MethodPost, "/delivery-channels",
		map[string]any{"name": "#ops", "kind": "slack", "webhook_url": "https://hooks.slack.com/services/T0/B0/wfpg"}); code != 201 {
		t.Fatalf("slack channel: %d %v", code, body)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/delivery-channels",
		map[string]any{"name": "Finance team", "kind": "email", "recipients": []string{"CFO@example.com", "cfo@example.com"}}); code != 201 {
		t.Fatalf("email channel: %d %v", code, body)
	}
	var recipients string
	if err := d.QueryRow(`SELECT array_to_string(email_recipients, ',') FROM delivery_channels WHERE name = 'Finance team'`).Scan(&recipients); err != nil || recipients != "cfo@example.com" {
		t.Fatalf("recipients = %q, %v", recipients, err)
	}

	// Ask rsync.ai drafts; "ops" matches "#ops".
	w := pgDraft(t, WorkflowDraftInput{
		Name: "Failed payments", ConnectionID: pgWFConn,
		SQL:     "SELECT count(*) AS failed, sum(total) AS amount FROM wf_orders WHERE status = 'failed'",
		Trigger: "schedule", Cron: "*/5 * * * *", Timezone: "Europe/Berlin",
		Branches: []WorkflowBranchInput{{Condition: "value", ConditionColumn: "failed", ConditionOp: ">", Threshold: "2",
			Subject: "{{failed}} failed payments", Message: "{{failed}} payments failed, {{amount}} in total <!channel>\n{{table}}",
			Channels: []string{"ops"}}},
	})
	if w.Status != "draft" || len(w.Channels) != 1 || w.Channels[0].Name != "#ops" || w.Connection == nil {
		t.Fatalf("draft = %+v", w)
	}

	// Test: the query runs for real; nothing is sent.
	code, body := pgDo(t, r, http.MethodPost, "/workflows/"+w.ID+"/test", nil)
	if code != 200 || body["condition_met"] != true || body["row_count"] != float64(1) {
		t.Fatalf("test: %d %v", code, body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(msgs[0].(map[string]any)["body"].(string), "3 payments failed, 35.50 in total &lt;!channel&gt;") {
		t.Fatalf("preview = %v", msgs)
	}
	if len(slack) != 0 {
		t.Fatalf("a test sent %v", slack)
	}

	// Turn on: the clicker becomes the run-as user; next_run_at is set.
	if code, body := pgDo(t, r, http.MethodPost, "/workflows/"+w.ID+"/activate", nil); code != 200 {
		t.Fatalf("activate: %d %v", code, body)
	}
	var runAs string
	var next time.Time
	if err := d.QueryRow(`SELECT run_as_user_id, next_run_at FROM workflows WHERE id = $1`, w.ID).Scan(&runAs, &next); err != nil ||
		runAs != pgWFUser || !next.After(time.Now()) {
		t.Fatalf("run_as=%q next=%s err=%v", runAs, next, err)
	}

	// Its Temporal schedule starts a run: begin inserts it under the Temporal
	// workflow id, once (a retry of begin finds it), and the next start while
	// it runs waits.
	at := time.Now().UTC().Truncate(time.Minute)
	begun := pgBegin(t, r, w.ID, pgScheduled(w.ID, at))
	runID, _ := begun["run_id"].(string)
	if begun["action"] != "run" || runID == "" {
		t.Fatalf("begin: %v", begun)
	}
	if again := pgBegin(t, r, w.ID, pgScheduled(w.ID, at)); again["action"] != "run" || again["run_id"] != runID {
		t.Fatalf("begin retried: %v", again)
	}
	if next := pgBegin(t, r, w.ID, pgScheduled(w.ID, at.Add(5*time.Minute))); next["action"] != "busy" {
		t.Fatalf("begin while running: %v", next)
	}
	pgRunSteps(t, r, runID)
	if again := pgBegin(t, r, w.ID, pgScheduled(w.ID, at)); again["action"] != "none" {
		t.Fatalf("begin retried after the run: %v", again)
	}

	var (
		status, deliveries string
		met                bool
		rows               int
	)
	if err := d.QueryRow(`SELECT status, condition_met, row_count, deliveries::text FROM workflow_runs WHERE id = $1`, runID).
		Scan(&status, &met, &rows, &deliveries); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || !met || rows != 1 || !strings.Contains(deliveries, `"status": "sent"`) {
		t.Fatalf("run: %s met=%v rows=%d deliveries=%s", status, met, rows, deliveries)
	}
	if len(slack) != 1 || !strings.HasPrefix(slack[0], "*3 failed payments*\n3 payments failed, 35.50 in total &lt;!channel&gt;") ||
		!strings.Contains(slack[0], "https://app.example/workflows/"+w.ID) {
		t.Fatalf("slack = %q", slack)
	}
	var audits int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'workflow_send' AND resource_id = $1 AND actor_kind = 'workflow'`, w.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("workflow_send audit rows = %d, %v", audits, err)
	}
	var ledger string
	if err := d.QueryRow(`SELECT string_agg(name || '=' || status, ',' ORDER BY position) FROM workflow_sends WHERE run_id = $1`, runID).
		Scan(&ledger); err != nil || ledger != "#ops=sent" {
		t.Fatalf("sends ledger = %q, %v", ledger, err)
	}

	// A used channel cannot be deleted.
	var opsID string
	_ = d.QueryRow(`SELECT id FROM delivery_channels WHERE name = '#ops'`).Scan(&opsID)
	if code, body := pgDo(t, r, http.MethodDelete, "/delivery-channels/"+opsID, nil); code != http.StatusConflict ||
		!strings.Contains(body["error"].(string), "Failed payments") {
		t.Fatalf("delete used channel: %d %v", code, body)
	}

	// The run-as user is demoted: the next run fails before the query and pauses it.
	// Pausing it deletes its Temporal schedule.
	wfExec(t, d, `UPDATE workspace_members SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, pgWFWS, pgWFUser)
	demoted := pgBegin(t, r, w.ID, pgScheduled(w.ID, at.Add(10*time.Minute)))
	if demoted["action"] != "run" {
		t.Fatalf("begin after demotion: %v", demoted)
	}
	pgRunSteps(t, r, demoted["run_id"].(string))
	var wfStatus, reason string
	_ = d.QueryRow(`SELECT status, COALESCE(paused_reason, '') FROM workflows WHERE id = $1`, w.ID).Scan(&wfStatus, &reason)
	if wfStatus != "paused" || !strings.Contains(reason, "no longer an owner or admin") || len(slack) != 1 {
		t.Fatalf("after demotion: %s %q, slack sends %d", wfStatus, reason, len(slack))
	}
	schedules.want(t,
		workflowScheduleCall{op: "upsert", workflowID: w.ID,
			trigger: workflow.Trigger{Type: workflow.TriggerSchedule, Cron: "*/5 * * * *", Timezone: "Europe/Berlin"}},
		workflowScheduleCall{op: "delete", workflowID: w.ID})
	wfExec(t, d, `UPDATE workspace_members SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, pgWFWS, pgWFUser)
}

func TestPG_WorkflowAfterPipelineFiresOncePerExecution(t *testing.T) {
	d := pgWorkflowDB(t)
	r := pgRouter("owner")
	schedules := stubWorkflowSchedules(t, nil)
	stubWorkflowMessages(t)
	if code, body := pgDo(t, r, http.MethodPost, "/delivery-channels",
		map[string]any{"name": "Finance team", "kind": "email", "recipients": []string{"cfo@example.com"}}); code != 201 {
		t.Fatalf("email channel: %d %v", code, body)
	}
	w := pgDraft(t, WorkflowDraftInput{
		Name: "After load", ConnectionID: pgWFConn, SQL: "SELECT id, email FROM wf_orders WHERE status = 'failed' ORDER BY id",
		Trigger: "after_pipeline", PipelineID: pgWFPipe,
		Branches: []WorkflowBranchInput{{Condition: "rows_exist", Message: "{{table}}", Channels: []string{"finance team"}}},
	})
	if w.Pipeline == nil || w.Pipeline.Name != "orders load" {
		t.Fatalf("pipeline = %+v", w.Pipeline)
	}
	// A draft does not fire.
	if ids, err := fireWorkflowsAfterPipeline(context.Background(), d, pgWFPipe, "exec-0"); err != nil || len(ids) != 0 {
		t.Fatalf("draft fired: %v %v", ids, err)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/workflows/"+w.ID+"/activate", nil); code != 200 {
		t.Fatalf("activate: %d %v", code, body)
	}
	first, err := fireWorkflowsAfterPipeline(context.Background(), d, pgWFPipe, "exec-1")
	if err != nil || len(first) != 1 || first[0].TemporalID != "workflow-run:"+w.ID+":exec-1" {
		t.Fatalf("first: %v %v", first, err)
	}
	// A re-delivered event inserts no run; it hands back the one still
	// running, which Temporal already has under that id.
	if again, err := fireWorkflowsAfterPipeline(context.Background(), d, pgWFPipe, "exec-1"); err != nil ||
		len(again) != 1 || again[0] != first[0] {
		t.Fatalf("re-delivered event: %v %v", again, err)
	}
	if other, err := fireWorkflowsAfterPipeline(context.Background(), d, pgWFPipe, "exec-2"); err != nil || len(other) != 1 ||
		other[0].RunID == first[0].RunID {
		t.Fatalf("next execution: %v %v", other, err)
	}

	// Email with neither Resend nor SMTP: the run fails with a sentence, and the
	// email column was masked by name in the query (the explorer's rule).
	if out := pgBegin(t, r, w.ID, map[string]any{"run_id": first[0].RunID, "trigger": "after_pipeline"}); out["action"] != "run" {
		t.Fatalf("begin: %v", out)
	}
	pgRunSteps(t, r, first[0].RunID)
	var status, deliveries, errText string
	if err := d.QueryRow(`SELECT status, deliveries::text, COALESCE(error, '') FROM workflow_runs WHERE id = $1`, first[0].RunID).
		Scan(&status, &deliveries, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(deliveries, "RESEND_API_KEY") || errText != "1 of 1 delivery channels failed." {
		t.Fatalf("email run: %s %s %q", status, deliveries, errText)
	}
	// Once it is recorded, the event re-delivered again starts nothing.
	if late, err := fireWorkflowsAfterPipeline(context.Background(), d, pgWFPipe, "exec-1"); err != nil || len(late) != 0 {
		t.Fatalf("re-delivered after the run: %v %v", late, err)
	}

	// Deleting the workflow frees the channel and takes its runs with it.
	if code, _ := pgDo(t, r, http.MethodDelete, "/workflows/"+w.ID, nil); code != 200 {
		t.Fatalf("delete workflow: %d", code)
	}
	var runs int
	_ = d.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_id = $1`, w.ID).Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs left: %d", runs)
	}
	schedules.want(t) // an after-pipeline workflow has no schedule
}

// An edit saved while a run is queued does not change that run: each run
// copies the definition it started with (migration 131) and runs the copy.
// The run history marks the runs of an earlier version; a run from before
// the copy existed runs the workflow's own definition. The edit itself is
// saved only over the version the editor loaded, keeps the workflow on with
// its next run recomputed, and its audit row names parts, never the SQL.
func TestPG_WorkflowEditKeepsQueuedRunsOnTheirVersion(t *testing.T) {
	d := pgWorkflowDB(t)
	r := pgRouter("admin")
	ctx := context.Background()
	schedules := stubWorkflowSchedules(t, nil)
	stubWorkflowMessages(t)
	stubStartWorkflowRun(t, nil)
	var slack []string
	prevSlack := postSlack
	postSlack = func(_ context.Context, url, text string) error { slack = append(slack, text); return nil }
	t.Cleanup(func() { postSlack = prevSlack })

	if code, body := pgDo(t, r, http.MethodPost, "/delivery-channels",
		map[string]any{"name": "#ops", "kind": "slack", "webhook_url": "https://hooks.slack.com/services/T0/B0/wfpg"}); code != 201 {
		t.Fatalf("slack channel: %d %v", code, body)
	}
	w := pgDraft(t, WorkflowDraftInput{
		Name: "Failed orders", ConnectionID: pgWFConn,
		SQL:     "SELECT count(*) AS failed FROM wf_orders WHERE status = 'failed'",
		Trigger: "schedule", Cron: "*/5 * * * *",
		Branches: []WorkflowBranchInput{{Condition: "always", Message: "{{failed}} failed", Channels: []string{"ops"}}},
	})
	if code, body := pgDo(t, r, http.MethodPost, "/workflows/"+w.ID+"/activate", nil); code != 200 {
		t.Fatalf("activate: %d %v", code, body)
	}

	// A run from before migration 131 has no copy: it runs the workflow's own.
	var legacy string
	if err := d.QueryRow(`INSERT INTO workflow_runs (workflow_id, workspace_id, trigger) VALUES ($1, $2, 'manual') RETURNING id`,
		w.ID, pgWFWS).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if run, err := loadWorkflowRun(ctx, d, legacy); err != nil || run.ConnectionID != pgWFConn ||
		!strings.Contains(run.Definition.Query().SQL, "'failed'") {
		t.Fatalf("legacy run = %+v, %v", run, err)
	}
	wfExec(t, d, `DELETE FROM workflow_runs WHERE id = $1`, legacy)

	// Its schedule begins a run; the edit lands before the run evaluates.
	begun := pgBegin(t, r, w.ID, pgScheduled(w.ID, time.Now()))
	queued, _ := begun["run_id"].(string)
	if begun["action"] != "run" || queued == "" {
		t.Fatalf("begin: %v", begun)
	}
	loaded, err := loadWorkflow(ctx, d, pgWFWS, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	def := loaded.Definition.Clone()
	def.SetQuery(workflow.Query{ConnectionID: pgWFConn, SQL: "SELECT count(*) AS paid FROM wf_orders WHERE status = 'paid'"})
	def.CaseSend(0).Message.Body = "{{paid}} paid"
	edit := map[string]any{"name": "Paid orders", "definition": def,
		"channel_ids": []string{loaded.Channels[0].ID}, "updated_at": loaded.UpdatedAt}
	code, body := pgDo(t, r, http.MethodPut, "/workflows/"+w.ID, edit)
	if code != 200 || body["query_checked"] != true {
		t.Fatalf("edit: %d %v", code, body)
	}
	// The same edit again names a version that is gone.
	if code, body := pgDo(t, r, http.MethodPut, "/workflows/"+w.ID, edit); code != http.StatusConflict {
		t.Fatalf("stale edit: %d %v", code, body)
	}
	var (
		status, runAs string
		next          time.Time
	)
	if err := d.QueryRow(`SELECT status, run_as_user_id, next_run_at FROM workflows WHERE id = $1`, w.ID).
		Scan(&status, &runAs, &next); err != nil || status != "active" || runAs != pgWFUser || !next.After(time.Now()) {
		t.Fatalf("after edit: %s run_as=%s next=%s %v", status, runAs, next, err)
	}

	// The edit kept the schedule, so Temporal's was not touched.
	schedules.want(t, workflowScheduleCall{op: "upsert", workflowID: w.ID, trigger: loaded.Definition.Trigger})

	// The begun run runs the version it began with; a new run, the edit.
	pgRunSteps(t, r, queued)
	now := pgRunNow(t, r, d, w.ID)
	if len(slack) != 2 || !strings.Contains(slack[0], "3 failed") || !strings.Contains(slack[1], "1 paid") {
		t.Fatalf("slack = %q", slack)
	}

	code, body = pgDo(t, r, http.MethodGet, "/workflows/"+w.ID+"/runs", nil)
	runs, _ := body["runs"].([]any)
	if code != 200 || len(runs) != 2 {
		t.Fatalf("runs: %d %v", code, body)
	}
	newest, older := runs[0].(map[string]any), runs[1].(map[string]any)
	if newest["id"] != now || newest["earlier_version"] != nil || newest["definition"] != nil {
		t.Fatalf("newest run = %v", newest)
	}
	oldDef, _ := older["definition"].(map[string]any)
	oldSteps, _ := oldDef["steps"].([]any)
	if older["id"] != queued || older["earlier_version"] != true || len(oldSteps) == 0 ||
		!strings.Contains(oldSteps[0].(map[string]any)["sql"].(string), "'failed'") {
		t.Fatalf("older run = %v", older)
	}

	var details string
	if err := d.QueryRow(`SELECT details::text FROM audit_logs WHERE action = 'update_workflow' AND resource_id = $1`, w.ID).
		Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"query"`) || !strings.Contains(details, `"branches"`) || !strings.Contains(details, `"name"`) ||
		strings.Contains(details, "wf_orders") || strings.Contains(details, "paid}}") {
		t.Fatalf("audit details = %s", details)
	}
}

// Branches on one query (migration 133): the first branch that holds sends to
// its own channels, the run records which, and a change Ask rsync.ai proposes
// is applied once, through the edit path, and never over a newer edit.
func TestPG_WorkflowBranchesAndChange(t *testing.T) {
	d := pgWorkflowDB(t)
	r := pgRouter("admin")
	ctx := context.Background()
	stubWorkflowSchedules(t, nil)
	stubWorkflowMessages(t)
	stubStartWorkflowRun(t, nil)
	var slack []string
	prevSlack := postSlack
	postSlack = func(_ context.Context, url, text string) error { slack = append(slack, url+"|"+text); return nil }
	t.Cleanup(func() { postSlack = prevSlack })
	for _, name := range []string{"ops", "finance"} {
		if code, body := pgDo(t, r, http.MethodPost, "/delivery-channels", map[string]any{"name": "#" + name,
			"kind": "slack", "webhook_url": "https://hooks.slack.com/services/T0/B0/" + name}); code != 201 {
			t.Fatalf("slack channel: %d %v", code, body)
		}
	}

	// 3 failed orders: "more than 5" does not hold, so "otherwise" sends.
	w := pgDraft(t, WorkflowDraftInput{
		Name: "Failed orders", ConnectionID: pgWFConn,
		SQL:     "SELECT count(*) AS failed FROM wf_orders WHERE status = 'failed'",
		Trigger: "schedule", Cron: "*/5 * * * *",
		Branches: []WorkflowBranchInput{
			{Condition: "value", ConditionColumn: "failed", ConditionOp: ">", Threshold: "5",
				Message: "{{failed}} failed: page someone", Channels: []string{"ops"}},
			{Condition: "always", Message: "{{failed}} failed", Channels: []string{"finance", "ops"}},
		},
	})
	if len(w.Definition.Branches()) != 2 || len(w.Channels) != 3 || w.Channels[0].NodeID != "send_1" ||
		w.Channels[1].NodeID != "send_2" {
		t.Fatalf("draft = %+v / %+v", w.Definition.Branches(), w.Channels)
	}
	var stored int
	if err := d.QueryRow(`SELECT COUNT(*) FROM workflow_channels WHERE workflow_id = $1 AND node_id = 'send_2'`, w.ID).
		Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("send_2 channels = %d, %v", stored, err)
	}
	run := pgRunNow(t, r, d, w.ID)
	var branch sql.NullInt64
	if err := d.QueryRow(`SELECT branch FROM workflow_runs WHERE id = $1`, run).Scan(&branch); err != nil ||
		!branch.Valid || branch.Int64 != 1 {
		t.Fatalf("run branch = %v, %v", branch, err)
	}
	if len(slack) != 2 || !strings.HasSuffix(strings.SplitN(slack[0], "|", 2)[0], "/finance") ||
		!strings.Contains(slack[0], "3 failed") || strings.Contains(slack[0], "page someone") {
		t.Fatalf("slack = %q", slack)
	}

	// Ask rsync.ai proposes "more than 2": nothing changes until it is applied.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/assistant", nil)
	c.Set("user_id", pgWFUser)
	c.Set(ctxWorkspaceID, pgWFWS)
	actor := services.Actor{UserID: pgWFUser, WorkspaceID: pgWFWS, Role: security.WSAdmin,
		Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}
	change, err := ProposeWorkflowChange(c, actor, w.ID, []WorkflowBranchInput{{Keep: 1, Threshold: "2"}, {Keep: 2}},
		"Page at more than 2.")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if change.Proposed.Definition.Branches()[0].Condition.Threshold != 2 ||
		w.Definition.Branches()[0].Condition.Threshold != 5 {
		t.Fatalf("proposed = %+v", change.Proposed.Definition.Branches())
	}
	code, body := pgDo(t, r, http.MethodGet, "/workflow-changes/"+change.ID, nil)
	if got, _ := body["change"].(map[string]any); code != 200 || got["status"] != "proposed" {
		t.Fatalf("get change: %d %v", code, body)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/workflow-changes/"+change.ID+"/apply", nil); code != 200 {
		t.Fatalf("apply: %d %v", code, body)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/workflow-changes/"+change.ID+"/apply", nil); code != http.StatusConflict ||
		body["code"] != "already_applied" {
		t.Fatalf("second apply: %d %v", code, body)
	}
	var audits int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'apply_workflow_change' AND resource_id = $1`, w.ID).
		Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("apply audit rows = %d, %v", audits, err)
	}
	// Channels came through the change: the second branch still has both.
	if err := d.QueryRow(`SELECT COUNT(*) FROM workflow_channels WHERE workflow_id = $1 AND node_id = 'send_2'`, w.ID).
		Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("send_2 channels after apply = %d, %v", stored, err)
	}

	// Now the first branch holds and sends to #ops alone.
	run = pgRunNow(t, r, d, w.ID)
	if err := d.QueryRow(`SELECT branch FROM workflow_runs WHERE id = $1`, run).Scan(&branch); err != nil || branch.Int64 != 0 {
		t.Fatalf("run branch = %v, %v", branch, err)
	}
	if len(slack) != 3 || !strings.Contains(slack[2], "/ops|") || !strings.Contains(slack[2], "page someone") {
		t.Fatalf("slack = %q", slack)
	}

	// A change proposed before an edit is refused after it.
	change2, err := ProposeWorkflowChange(c, actor, w.ID, []WorkflowBranchInput{{Keep: 2}}, "")
	if err != nil {
		t.Fatalf("propose 2: %v", err)
	}
	loaded, err := loadWorkflow(ctx, d, pgWFWS, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	perNode := map[string][]string{}
	for _, ch := range loaded.Channels {
		perNode[ch.NodeID] = append(perNode[ch.NodeID], ch.ID)
	}
	if code, body := pgDo(t, r, http.MethodPut, "/workflows/"+w.ID, map[string]any{"name": "Failed orders, renamed",
		"definition": loaded.Definition, "node_channel_ids": perNode, "updated_at": loaded.UpdatedAt}); code != 200 {
		t.Fatalf("edit: %d %v", code, body)
	}
	code, body = pgDo(t, r, http.MethodGet, "/workflow-changes/"+change2.ID, nil)
	if got, _ := body["change"].(map[string]any); code != 200 || got["status"] != changeStatusStale {
		t.Fatalf("stale change: %d %v", code, body)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/workflow-changes/"+change2.ID+"/apply", nil); code != http.StatusConflict ||
		body["code"] != "workflow_changed" {
		t.Fatalf("stale apply: %d %v", code, body)
	}
	if loaded, _ := loadWorkflow(ctx, d, pgWFWS, w.ID); loaded == nil || len(loaded.Definition.Branches()) != 2 {
		t.Fatalf("a stale change was applied: %+v", loaded)
	}
}
