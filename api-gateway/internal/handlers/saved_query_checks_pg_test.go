//go:build integration_pg

// Real-database proof for data checks (migration 138, data-checks-design.md § 11 row 1):
// what sqlmock cannot judge. A model is rebuilt for real, through the gateway's own
// driver path, and a blocking check that fails must leave the live table exactly as it
// was — the same rows in the same relation — with no staging table behind it; the
// models below the blocked run are skipped upstream_check_failed; the results land with
// the run; the alert reaches the model's own workspace's bell and no other; and a
// custom_sql check on a scheduled model waits for an admin.
//
// Each blocked run has a control beside it: the same bad rebuild with the check off
// publishes, and the probe sees the table change. Without it a probe that could not see
// a rebuild would pass the block for nothing.
//
// Postgres rolls a blocked rebuild back. The model runs on the database it is recorded
// in, the harness of workflows_pg_test.go:
//
//	docker run -d --name wf-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55471:5432 postgres:16-alpine
//	WF_PG_DSN='postgres://postgres:verify@localhost:55471/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Checks -v
//
// MySQL has no transaction to roll back: a blocked rebuild's staging table is swept. Its
// test also needs a MySQL, and is skipped without one:
//
//	docker run -d --name chk-mysql -e MYSQL_ROOT_PASSWORD=verify -e MYSQL_DATABASE=chk \
//	    -p 55472:3306 mysql:8.4
//	CHK_MYSQL_PORT=55472
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"api-gateway/internal/notifier"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"

	_ "github.com/go-sql-driver/mysql"
)

const (
	pgChkModel   = "55555555-aaaa-4000-8000-000000000001"
	pgChkBelow   = "55555555-aaaa-4000-8000-000000000002"
	pgChkFirst   = "55555555-aaaa-4000-8000-000000000003"
	pgChkMyModel = "55555555-aaaa-4000-8000-000000000004"
	pgChkMyConn  = "33333333-aaaa-4000-8000-000000000002"
	pgChkOtherWS = "22222222-aaaa-4000-8000-000000000002"
)

// pgChecksDB is the workflows harness plus a source table, chk_src, that the models
// read, and nothing at the names they build.
func pgChecksDB(t *testing.T) *sql.DB {
	t.Helper()
	d := pgWorkflowDB(t)
	drop := func() {
		for _, q := range []string{
			// Deleting the models removes their checks, runs, results and alerts.
			`DELETE FROM saved_queries WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM connections WHERE id = '` + pgChkMyConn + `'`,
			`DELETE FROM workspace_members WHERE workspace_id = '` + pgChkOtherWS + `'`,
			`DELETE FROM workspaces WHERE id = '` + pgChkOtherWS + `'`,
			`DROP TABLE IF EXISTS chk_src, chk_orders, chk_orders__rsync_new, chk_below, chk_first, chk_first__rsync_new`,
		} {
			if _, err := d.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	wfExec(t, d, `CREATE TABLE chk_src (id int, region text)`)
	wfExec(t, d, `INSERT INTO chk_src VALUES (1, 'eu'), (2, 'us')`)
	prev := currentModelCheckNotifier()
	SetModelCheckNotifier(notifier.NewEmitter(d))
	t.Cleanup(func() { SetModelCheckNotifier(prev) })
	return d
}

// pgChecksRouter is the Checks tab's routes, the bell, and the adapter's /downstream,
// for the harness user in the given workspace with the given role.
func pgChecksRouter(ws, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgWFUser)
		c.Set(ctxWorkspaceID, ws)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/saved/:id/checks", ListModelChecks)
	r.POST("/saved/:id/checks", CreateModelCheck)
	r.POST("/saved/:id/checks/test", TestModelChecks)
	r.PUT("/saved/:id/checks/:checkId", UpdateModelCheck)
	r.DELETE("/saved/:id/checks/:checkId", DeleteModelCheck)
	r.POST("/saved/:id/checks/pending/:pendingId/approve", ApproveModelCheckEdit)
	r.POST("/saved/:id/checks/pending/:pendingId/reject", RejectModelCheckEdit)
	r.GET("/saved/:id/runs/:runId/checks", ListModelRunChecks)
	r.GET("/notifications", ListNotifications)
	r.POST("/models/:id/downstream", ListSavedQueryModelDownstreamInternal)
	return r
}

func pgChkModelRow(t *testing.T, d *sql.DB, id, conn, sqlText, target string) {
	t.Helper()
	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by, materialization, target_table)
		VALUES ($1, $2, $3, $4, $5, $6, 'table', $7)`, id, pgWFWS, conn, "model "+id[len(id)-1:], sqlText, pgWFUser, target)
}

// pgChkRun rebuilds a model and records the run, as the run endpoint does.
func pgChkRun(t *testing.T, d *sql.DB, modelID string) (*modelRunResult, string) {
	t.Helper()
	started := time.Now()
	res, err := runSavedQueryModel(context.Background(), d, modelID, pgWFUser)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	runID := recordModelRunOutcome(context.Background(), d, modelID, res,
		modelRunAudit{Trigger: triggerManual, ActorID: pgWFUser, StartedAt: started})
	if runID == "" {
		t.Fatalf("the run was not recorded: %+v", res)
	}
	return res, runID
}

// pgTable is what a rebuild changes: the relation at the name, and what it holds.
type pgTable struct {
	OID  int64
	Rows string
}

func pgTableAt(t *testing.T, d *sql.DB, name string) *pgTable {
	t.Helper()
	var oid sql.NullInt64
	if err := d.QueryRow(`SELECT to_regclass($1)::oid::bigint`, name).Scan(&oid); err != nil {
		t.Fatalf("regclass %s: %v", name, err)
	}
	if !oid.Valid {
		return nil
	}
	var rows string
	if err := d.QueryRow(`SELECT COALESCE(string_agg(t::text, ',' ORDER BY t::text), '') FROM ` + name + ` t`).Scan(&rows); err != nil {
		t.Fatalf("rows of %s: %v", name, err)
	}
	return &pgTable{OID: oid.Int64, Rows: rows}
}

func pgChkAdd(t *testing.T, r *gin.Engine, model string, check map[string]any) string {
	t.Helper()
	code, body := pgDo(t, r, http.MethodPost, "/saved/"+model+"/checks", check)
	if code != http.StatusCreated {
		t.Fatalf("add %v: %d %v", check["name"], code, body)
	}
	return body["check_id"].(string)
}

type pgChkResult struct {
	Name, Severity, Status string
	Observed               sql.NullInt64
}

func pgChkResults(t *testing.T, d *sql.DB, runID string) []pgChkResult {
	t.Helper()
	rows, err := d.Query(`SELECT check_name, severity, status, observed FROM saved_query_check_results
		WHERE run_id = $1 ORDER BY check_name`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pgChkResult
	for rows.Next() {
		var r pgChkResult
		if err := rows.Scan(&r.Name, &r.Severity, &r.Status, &r.Observed); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// pgChkAlert waits for the run's alert: notifyModelCheckOutcome raises it off the run's
// goroutine.
func pgChkAlert(t *testing.T, d *sql.DB, runID string) (id, code, severity string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := d.QueryRow(`SELECT id::text, metadata->>'error_code', severity FROM pipeline_notifications
			WHERE saved_query_id = $1 AND action_url LIKE '%run=' || $2`, pgChkModel, runID).Scan(&id, &code, &severity)
		if err == nil {
			return id, code, severity
		}
		if err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no alert for run %s", runID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func pgChkBell(t *testing.T, ws string) map[string]bool {
	t.Helper()
	code, body := pgDo(t, pgChecksRouter(ws, "admin"), http.MethodGet, "/notifications", nil)
	if code != http.StatusOK {
		t.Fatalf("bell: %d %v", code, body)
	}
	ids := map[string]bool{}
	list, _ := body["notifications"].([]any)
	for _, n := range list {
		ids[n.(map[string]any)["id"].(string)] = true
	}
	return ids
}

func TestPG_ChecksABlockKeepsTheLiveTableAndSkipsWhatRunsAfter(t *testing.T) {
	d := pgChecksDB(t)
	r := pgChecksRouter(pgWFWS, "admin")
	pgChkModelRow(t, d, pgChkModel, pgWFConn, `SELECT id, region FROM public.chk_src`, "public.chk_orders")

	// The model below, rebuilt after each run of the one above.
	pgChkModelRow(t, d, pgChkBelow, pgWFConn, `SELECT id FROM public.chk_orders`, "public.chk_below")
	var schedule string
	if err := d.QueryRow(`INSERT INTO saved_query_schedules
		(saved_query_id, schedule_type, schedule_spec, status, run_as_user_id, created_by)
		VALUES ($1, 'after_upstream', '{}', 'active', $2, $2) RETURNING schedule_id`, pgChkBelow, pgWFUser).Scan(&schedule); err != nil {
		t.Fatal(err)
	}
	wfExec(t, d, `INSERT INTO saved_query_schedule_upstreams (schedule_id, upstream_kind, upstream_saved_query_id)
		VALUES ($1, 'model', $2)`, schedule, pgChkModel)

	if res, _ := pgChkRun(t, d, pgChkModel); res.Status != "succeeded" {
		t.Fatalf("first run: %+v", res)
	}
	live := pgTableAt(t, d, "public.chk_orders")
	if live == nil || live.Rows != "(1,eu),(2,us)" {
		t.Fatalf("first run published %+v", live)
	}

	pgChkAdd(t, r, pgChkModel, map[string]any{"name": "id present", "type": "not_null", "columns": []string{"id"}, "severity": "block"})
	wfExec(t, d, `INSERT INTO chk_src VALUES (NULL, 'apac')`)

	res, runID := pgChkRun(t, d, pgChkModel)
	if res.Status != "failed" || res.Error != modelCheckBlockedError || !res.CheckBlocked {
		t.Fatalf("blocked run: %+v", res)
	}
	if got := pgTableAt(t, d, "public.chk_orders"); *got != *live {
		t.Fatalf("a blocked run changed the live table: %+v, was %+v", got, live)
	}
	if pgTableAt(t, d, "public.chk_orders__rsync_new") != nil {
		t.Fatal("a blocked run left its staging table")
	}
	got := pgChkResults(t, d, runID)
	if len(got) != 1 || got[0] != (pgChkResult{"id present", "block", "fail", sql.NullInt64{Int64: 1, Valid: true}}) {
		t.Fatalf("results = %+v", got)
	}
	code, body := pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/runs/"+runID+"/checks", nil)
	if code != http.StatusOK || len(body["results"].([]any)) != 1 {
		t.Fatalf("run checks: %d %v", code, body)
	}

	// The alert: on the model's workspace's bell, and not on another workspace's.
	alert, alertCode, severity := pgChkAlert(t, d, runID)
	if alertCode != notifier.CodeCheckBlocked || severity != "critical" {
		t.Fatalf("alert = %s %s", alertCode, severity)
	}
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Other', 'chk-other', $2)`, pgChkOtherWS, pgWFUser)
	wfExec(t, d, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, pgChkOtherWS, pgWFUser)
	if !pgChkBell(t, pgWFWS)[alert] {
		t.Fatal("the alert is not on its model's workspace's bell")
	}
	if pgChkBell(t, pgChkOtherWS)[alert] {
		t.Fatal("the alert reached another workspace's bell")
	}

	// What runs after it is skipped, pointing at the blocked run.
	code, body = pgDo(t, r, http.MethodPost, "/models/"+pgChkModel+"/downstream", map[string]any{"run_id": runID, "depth": 0})
	if code != http.StatusOK {
		t.Fatalf("downstream: %d %v", code, body)
	}
	if targets, _ := body["targets"].([]any); len(targets) != 0 {
		t.Fatalf("a blocked run woke %v", targets)
	}
	var status, reason, upstream string
	if err := d.QueryRow(`SELECT status, skip_reason, upstream_run_id::text FROM saved_query_runs WHERE saved_query_id = $1`,
		pgChkBelow).Scan(&status, &reason, &upstream); err != nil {
		t.Fatalf("the model below has no run: %v", err)
	}
	if status != "skipped" || reason != skipUpstreamCheckFailed || upstream != runID {
		t.Fatalf("below = %s %s %s", status, reason, upstream)
	}

	// The control: the same rebuild with the check off publishes, and the probe sees it.
	code, body = pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/checks", nil)
	if code != http.StatusOK || body["can_block"] != true {
		t.Fatalf("list: %d %v", code, body)
	}
	chk := body["checks"].([]any)[0].(map[string]any)
	off := map[string]any{"name": chk["name"], "type": chk["type"], "columns": chk["columns"], "severity": "block", "enabled": false}
	if code, body := pgDo(t, r, http.MethodPut, "/saved/"+pgChkModel+"/checks/"+chk["check_id"].(string), off); code != http.StatusOK {
		t.Fatalf("turn off: %d %v", code, body)
	}
	if res, _ := pgChkRun(t, d, pgChkModel); res.Status != "succeeded" {
		t.Fatalf("control run: %+v", res)
	}
	rebuilt := pgTableAt(t, d, "public.chk_orders")
	if rebuilt.OID == live.OID || rebuilt.Rows != "(,apac),(1,eu),(2,us)" {
		t.Fatalf("the probe did not see an unchecked rebuild: %+v, was %+v", rebuilt, live)
	}

	// A warning publishes and says so.
	warn := map[string]any{"name": chk["name"], "type": chk["type"], "columns": chk["columns"], "severity": "warn", "enabled": true}
	if code, body := pgDo(t, r, http.MethodPut, "/saved/"+pgChkModel+"/checks/"+chk["check_id"].(string), warn); code != http.StatusOK {
		t.Fatalf("make it a warning: %d %v", code, body)
	}
	res, runID = pgChkRun(t, d, pgChkModel)
	if res.Status != "succeeded" || pgTableAt(t, d, "public.chk_orders").OID == rebuilt.OID {
		t.Fatalf("a warning held the table back: %+v", res)
	}
	if got := pgChkResults(t, d, runID); len(got) != 1 || got[0].Severity != "warn" || got[0].Status != "fail" {
		t.Fatalf("warn results = %+v", got)
	}
	if _, alertCode, severity := pgChkAlert(t, d, runID); alertCode != notifier.CodeCheckWarned || severity != "warning" {
		t.Fatalf("warn alert = %s %s", alertCode, severity)
	}

	// A custom_sql block reads the staging table as {{this}}: it fails on the new rows,
	// not on the live ones, which already hold apac.
	wfExec(t, d, `DELETE FROM chk_src WHERE id IS NULL`)
	if code, _ := pgDo(t, r, http.MethodDelete, "/saved/"+pgChkModel+"/checks/"+chk["check_id"].(string), nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	pgChkAdd(t, r, pgChkModel, map[string]any{"name": "no us rows", "type": "custom_sql",
		"sql_text": "SELECT * FROM {{this}} WHERE region = 'us'", "severity": "block"})
	before := pgTableAt(t, d, "public.chk_orders")
	if res, runID = pgChkRun(t, d, pgChkModel); !res.CheckBlocked {
		t.Fatalf("custom_sql did not block: %+v", res)
	}
	if got := pgTableAt(t, d, "public.chk_orders"); *got != *before {
		t.Fatalf("a blocked custom_sql run changed the live table: %+v, was %+v", got, before)
	}
	if got := pgChkResults(t, d, runID); len(got) != 1 || got[0].Observed.Int64 != 1 {
		t.Fatalf("custom_sql results = %+v", got)
	}

	// A passing block publishes.
	wfExec(t, d, `DELETE FROM chk_src WHERE region = 'us'`)
	if res, runID = pgChkRun(t, d, pgChkModel); res.Status != "succeeded" {
		t.Fatalf("passing run: %+v", res)
	}
	if got := pgTableAt(t, d, "public.chk_orders"); got.Rows != "(1,eu)" {
		t.Fatalf("passing run published %+v", got)
	}
	if got := pgChkResults(t, d, runID); len(got) != 1 || got[0].Status != "pass" {
		t.Fatalf("passing results = %+v", got)
	}

	// Test now reads the live table and records nothing.
	code, body = pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/test", nil)
	if code != http.StatusOK || body["results"].([]any)[0].(map[string]any)["status"] != "pass" {
		t.Fatalf("test now: %d %v", code, body)
	}
	code, body = pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/checks", nil)
	saved := len(body["checks"].([]any))

	// The Add check form's Test: one unsaved check, alone, judged as a save judges it.
	// It saves nothing and, like Test now, records nothing.
	draft := map[string]any{"check": map[string]any{"name": "five rows", "type": "row_count", "params": map[string]any{"min": 5}}}
	code, body = pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/test", draft)
	if code != http.StatusOK || len(body["results"].([]any)) != 1 {
		t.Fatalf("test a draft: %d %v", code, body)
	}
	if got := body["results"].([]any)[0].(map[string]any); got["name"] != "five rows" || got["status"] != "fail" || got["observed"] != float64(1) {
		t.Fatalf("draft result = %v", got)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/test",
		map[string]any{"check": map[string]any{"name": "no column", "type": "not_null"}}); code != http.StatusBadRequest {
		t.Fatalf("a draft a save would refuse: %d %v", code, body)
	}
	if code, body = pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/checks", nil); len(body["checks"].([]any)) != saved {
		t.Fatalf("testing a draft saved it: %d %v", code, body)
	}
	// A draft takes the role saving it does; Test now on the saved checks stays a viewer's.
	setRole := func(role string) {
		wfExec(t, d, `UPDATE workspace_members SET role = $1 WHERE workspace_id = $2 AND user_id = $3`, role, pgWFWS, pgWFUser)
	}
	setRole("viewer")
	t.Cleanup(func() { setRole("admin") })
	if code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/test", draft); code != http.StatusForbidden {
		t.Fatalf("a viewer tested a draft: %d %v", code, body)
	}
	if code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/test", nil); code != http.StatusOK {
		t.Fatalf("a viewer's Test now: %d %v", code, body)
	}
	setRole("admin")

	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM saved_query_check_results r JOIN saved_query_runs u USING (run_id)
		WHERE u.saved_query_id = $1`, pgChkModel).Scan(&n); err != nil || n != 4 {
		t.Fatalf("result rows = %d, %v; Test now must record none", n, err)
	}
}

func TestPG_ChecksABlockedFirstRunPublishesNothing(t *testing.T) {
	d := pgChecksDB(t)
	r := pgChecksRouter(pgWFWS, "admin")
	pgChkModelRow(t, d, pgChkFirst, pgWFConn, `SELECT id, region FROM public.chk_src`, "public.chk_first")
	pgChkAdd(t, r, pgChkFirst, map[string]any{"name": "enough rows", "type": "row_count",
		"params": map[string]any{"min": 3}, "severity": "block"})

	res, runID := pgChkRun(t, d, pgChkFirst)
	if !res.CheckBlocked {
		t.Fatalf("first run: %+v", res)
	}
	if pgTableAt(t, d, "public.chk_first") != nil || pgTableAt(t, d, "public.chk_first__rsync_new") != nil {
		t.Fatal("a blocked first run left a table")
	}
	var owned bool
	if err := d.QueryRow(`SELECT target_owned FROM saved_queries WHERE id = $1`, pgChkFirst).Scan(&owned); err != nil || owned {
		t.Fatalf("target_owned = %v, %v: a blocked first run built nothing to own", owned, err)
	}
	if got := pgChkResults(t, d, runID); len(got) != 1 || got[0].Observed.Int64 != 2 {
		t.Fatalf("results = %+v", got)
	}

	// The control: one more row and the same run publishes.
	wfExec(t, d, `INSERT INTO chk_src VALUES (3, 'apac')`)
	if res, _ := pgChkRun(t, d, pgChkFirst); res.Status != "succeeded" || pgTableAt(t, d, "public.chk_first") == nil {
		t.Fatalf("control run: %+v", res)
	}
}

func TestPG_ChecksCustomSQLOnAScheduledModelWaitsForAnAdmin(t *testing.T) {
	d := pgChecksDB(t)
	r := pgChecksRouter(pgWFWS, "admin")
	pgChkModelRow(t, d, pgChkModel, pgWFConn, `SELECT id, region FROM public.chk_src`, "public.chk_orders")
	// The role is read from workspace_members. As a member the harness user still edits
	// the checks, being the model's creator; approving needs an admin.
	role := func(role string) {
		wfExec(t, d, `UPDATE workspace_members SET role = $1 WHERE workspace_id = $2 AND user_id = $3`, role, pgWFWS, pgWFUser)
	}

	custom := map[string]any{"name": "no negatives", "type": "custom_sql",
		"sql_text": "SELECT * FROM {{this}} WHERE id < 0", "severity": "block"}
	// Unscheduled: it applies.
	id := pgChkAdd(t, r, pgChkModel, custom)
	if code, _ := pgDo(t, r, http.MethodDelete, "/saved/"+pgChkModel+"/checks/"+id, nil); code != http.StatusOK {
		t.Fatal("delete")
	}

	wfExec(t, d, `INSERT INTO saved_query_schedules (saved_query_id, schedule_type, schedule_spec, temporal_schedule_id, status, run_as_user_id, created_by)
		VALUES ($1, 'cron', '{"cron":"0 * * * *"}', 'chk-pg-cron', 'active', $2, $2)`, pgChkModel, pgWFUser)

	role("member")
	code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks", custom)
	if code != http.StatusOK || body["pending_approval"] == nil {
		t.Fatalf("scheduled custom_sql: %d %v", code, body)
	}
	pending := body["pending_approval"].(map[string]any)["id"].(string)
	if code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks", custom); code != http.StatusConflict {
		t.Fatalf("a second proposal for the same check: %d %v", code, body)
	}
	code, body = pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/checks", nil)
	if code != http.StatusOK || len(body["checks"].([]any)) != 0 || len(body["pending"].([]any)) != 1 {
		t.Fatalf("list while pending: %d %v", code, body)
	}
	if code, _ := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/pending/"+pending+"/approve", nil); code != http.StatusForbidden {
		t.Fatalf("a member approved: %d", code)
	}
	role("admin")
	if code, body := pgDo(t, r, http.MethodPost, "/saved/"+pgChkModel+"/checks/pending/"+pending+"/approve",
		map[string]any{"note": "ok"}); code != http.StatusOK || body["applied"] != true {
		t.Fatalf("approve: %d %v", code, body)
	}
	code, body = pgDo(t, r, http.MethodGet, "/saved/"+pgChkModel+"/checks", nil)
	checks := body["checks"].([]any)
	if code != http.StatusOK || len(checks) != 1 || len(body["pending"].([]any)) != 0 ||
		checks[0].(map[string]any)["sql_text"] != "SELECT * FROM {{this}} WHERE id < 0" {
		t.Fatalf("list after approval: %d %v", code, body)
	}

	// The approved query now runs, and passes on these rows.
	if res, _ := pgChkRun(t, d, pgChkModel); res.Status != "succeeded" {
		t.Fatalf("run after approval: %+v", res)
	}
}

// mysqlChk opens the MySQL from the file header, or skips.
func mysqlChk(t *testing.T) (*sql.DB, int) {
	t.Helper()
	port, _ := strconv.Atoi(os.Getenv("CHK_MYSQL_PORT"))
	if port == 0 {
		t.Skip("CHK_MYSQL_PORT not set — see the file header")
	}
	m, err := sql.Open("mysql", fmt.Sprintf("root:verify@tcp(127.0.0.1:%d)/chk", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, q := range []string{
		"DROP TABLE IF EXISTS chk_src, chk_orders, chk_orders__rsync_new",
		"CREATE TABLE chk_src (id int NULL, region varchar(16))",
		"INSERT INTO chk_src VALUES (1, 'eu'), (2, 'us')",
	} {
		if _, err := m.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return m, port
}

// mysqlTableAt is the table's rows and its comment: a rebuild makes a new table, which
// has no comment.
func mysqlTableAt(t *testing.T, m *sql.DB, name string) (rows, comment string, ok bool) {
	t.Helper()
	err := m.QueryRow(`SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'chk' AND TABLE_NAME = ?`, name).Scan(&comment)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	var s sql.NullString
	if err := m.QueryRow("SELECT GROUP_CONCAT(CONCAT(COALESCE(id, 'null'), ':', region) ORDER BY id SEPARATOR ',') FROM chk." + name).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String, comment, true
}

func TestPG_ChecksMySQLSweepsABlockedRebuild(t *testing.T) {
	d := pgChecksDB(t)
	m, port := mysqlChk(t)
	r := pgChecksRouter(pgWFWS, "admin")

	cfg, _ := json.Marshal(map[string]any{"host": "127.0.0.1", "port": port, "user": "root", "password": "verify", "database": "chk"})
	enc, err := crypto.EncryptString(string(cfg))
	if err != nil {
		t.Fatal(err)
	}
	wfExec(t, d, `INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id)
		VALUES ($1, $2, 'mysql warehouse', 'destination', 'mysql', $3, $4)`, pgChkMyConn, pgWFUser, enc, pgWFWS)
	pgChkModelRow(t, d, pgChkMyModel, pgChkMyConn, "SELECT id, region FROM chk.chk_src", "chk.chk_orders")

	if res, _ := pgChkRun(t, d, pgChkMyModel); res.Status != "succeeded" {
		t.Fatalf("first run: %+v", res)
	}
	// Marks this table: a rebuild's new table carries no comment.
	if _, err := m.Exec("ALTER TABLE chk.chk_orders COMMENT = 'live-1'"); err != nil {
		t.Fatal(err)
	}
	liveRows, _, _ := mysqlTableAt(t, m, "chk_orders")
	if liveRows != "1:eu,2:us" {
		t.Fatalf("first run published %q", liveRows)
	}

	code, body := pgDo(t, r, http.MethodGet, "/saved/"+pgChkMyModel+"/checks", nil)
	if code != http.StatusOK || body["can_block"] != true {
		t.Fatalf("MySQL cannot block an owned table: %d %v", code, body)
	}
	id := pgChkAdd(t, r, pgChkMyModel, map[string]any{"name": "id present", "type": "not_null", "columns": []string{"id"}, "severity": "block"})
	if _, err := m.Exec("INSERT INTO chk_src VALUES (NULL, 'apac')"); err != nil {
		t.Fatal(err)
	}

	res, runID := pgChkRun(t, d, pgChkMyModel)
	if !res.CheckBlocked {
		t.Fatalf("blocked run: %+v", res)
	}
	rows, comment, ok := mysqlTableAt(t, m, "chk_orders")
	if !ok || rows != liveRows || comment != "live-1" {
		t.Fatalf("a blocked run changed the live table: %q %q %v", rows, comment, ok)
	}
	if _, _, ok := mysqlTableAt(t, m, "chk_orders__rsync_new"); ok {
		t.Fatal("a blocked run left its staging table")
	}
	if got := pgChkResults(t, d, runID); len(got) != 1 || got[0].Status != "fail" || got[0].Observed.Int64 != 1 {
		t.Fatalf("results = %+v", got)
	}

	// The control: with the check off the same rebuild replaces the table.
	off := map[string]any{"name": "id present", "type": "not_null", "columns": []string{"id"}, "severity": "block", "enabled": false}
	if code, body := pgDo(t, r, http.MethodPut, "/saved/"+pgChkMyModel+"/checks/"+id, off); code != http.StatusOK {
		t.Fatalf("turn off: %d %v", code, body)
	}
	if res, _ := pgChkRun(t, d, pgChkMyModel); res.Status != "succeeded" {
		t.Fatalf("control run: %+v", res)
	}
	rows, comment, _ = mysqlTableAt(t, m, "chk_orders")
	if rows != "null:apac,1:eu,2:us" || comment == "live-1" {
		t.Fatalf("the probe did not see an unchecked rebuild: %q %q", rows, comment)
	}
}
