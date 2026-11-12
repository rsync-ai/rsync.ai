//go:build integration_pg

// Real-Postgres coverage for GET /api/v1/activity/metrics (dashboards-design.md
// § 13): every number the Runs page's charts draw equals a hand count over seeded
// rows, and the runs it counts are the runs GET /api/v1/activity lists. The
// session runs in Asia/Kolkata, so a run at 23:30 UTC that is already the next
// day in India proves a day is a UTC day.
//
// Same Postgres as activity_pg_test.go (the test applies every migration):
//
//	docker run -d --name act-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55472:5432 postgres:16-alpine
//	ACT_PG_DSN='postgres://postgres:verify@localhost:55472/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ActivityMetrics -v
package handlers

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgMetUserA = "11111111-cccc-4000-8000-000000000001" // the caller
	pgMetUserB = "11111111-cccc-4000-8000-000000000002" // another member
	pgMetWS    = "22222222-cccc-4000-8000-000000000001"
	pgMetWS2   = "22222222-cccc-4000-8000-000000000002" // someone else's workspace
	pgMetConn  = "33333333-cccc-4000-8000-000000000001"
	pgMetConn2 = "33333333-cccc-4000-8000-000000000002"
	pgMetPipe  = "44444444-cccc-4000-8000-000000000001"
	pgMetPipe2 = "44444444-cccc-4000-8000-000000000002"

	pgMetQPub   = "66666666-cccc-4000-8000-000000000001" // workspace-visible, B's
	pgMetQPrivA = "66666666-cccc-4000-8000-000000000002" // the caller's private query
	pgMetQPrivB = "66666666-cccc-4000-8000-000000000003" // B's private query: hidden
	pgMetQOther = "66666666-cccc-4000-8000-000000000004" // other workspace: hidden
)

// The clock the window ends on: days=3 is 2026-10-03, 10-04 and 10-05 UTC.
var pgMetNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func pgMetAt(day, clock string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", day+" "+clock)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func pgMetricsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ACT_PG_DSN")
	if dsn == "" {
		t.Skip("ACT_PG_DSN not set — see the file header")
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
	// One connection, so the session time zone below is the one every read runs in.
	d.SetMaxOpenConns(1)
	wfExec(t, d, `SET TIME ZONE 'Asia/Kolkata'`)

	prevNow := activityMetricsNow
	activityMetricsNow = func() time.Time { return pgMetNow }
	t.Cleanup(func() { activityMetricsNow = prevNow })

	wfExec(t, d, `DELETE FROM saved_queries WHERE id IN ($1, $2, $3, $4)`, pgMetQPub, pgMetQPrivA, pgMetQPrivB, pgMetQOther)
	wfExec(t, d, `DELETE FROM pipeline_progress WHERE pipeline_id IN ($1, $2)`, pgMetPipe, pgMetPipe2)
	wfExec(t, d, `DELETE FROM pipelines WHERE id IN ($1, $2)`, pgMetPipe, pgMetPipe2)
	wfExec(t, d, `DELETE FROM connections WHERE id IN ($1, $2)`, pgMetConn, pgMetConn2)
	wfExec(t, d, `DELETE FROM workspaces WHERE id IN ($1, $2)`, pgMetWS, pgMetWS2)
	wfExec(t, d, `DELETE FROM users WHERE id IN ($1, $2)`, pgMetUserA, pgMetUserB)

	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'met-a@example.com', 'x'), ($2, 'met-b@example.com', 'x')`,
		pgMetUserA, pgMetUserB)
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Met', 'met-pg', $3), ($2, 'Other', 'met-pg-2', $4)`,
		pgMetWS, pgMetWS2, pgMetUserA, pgMetUserB)
	wfExec(t, d, `INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id) VALUES
		($1, $3, 'warehouse', 'source', 'postgresql', 'x', $4), ($2, $5, 'theirs', 'source', 'postgresql', 'x', $6)`,
		pgMetConn, pgMetConn2, pgMetUserA, pgMetWS, pgMetUserB, pgMetWS2)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id)
		VALUES ($1, 'orders load', 'x', $2), ($3, 'other load', 'x', $4)`, pgMetPipe, pgMetWS, pgMetPipe2, pgMetWS2)

	// Pipeline runs. A hand count of what each day must show is in the test below.
	for _, e := range []struct {
		id, pipe, status string
		start            time.Time
		end              any
	}{
		{"55555555-cccc-4000-8000-000000000001", pgMetPipe, "completed", pgMetAt("2026-10-03", "10:00:00"), pgMetAt("2026-10-03", "10:00:10")},
		// 23:30 UTC is 05:00 on 10-04 in India: still a 10-03 run.
		{"55555555-cccc-4000-8000-000000000002", pgMetPipe, "failed", pgMetAt("2026-10-03", "23:30:00"), pgMetAt("2026-10-03", "23:30:30")},
		{"55555555-cccc-4000-8000-000000000003", pgMetPipe, "success", pgMetAt("2026-10-04", "09:00:00"), pgMetAt("2026-10-04", "09:01:00")},
		{"55555555-cccc-4000-8000-000000000004", pgMetPipe, "silent_drop_detected", pgMetAt("2026-10-04", "10:00:00"), pgMetAt("2026-10-04", "10:00:20")},
		// Cancelled: counted, but its end is not when its work ended, so no run time.
		{"55555555-cccc-4000-8000-000000000005", pgMetPipe, "cancelled", pgMetAt("2026-10-05", "08:00:00"), pgMetAt("2026-10-05", "08:05:00")},
		{"55555555-cccc-4000-8000-000000000006", pgMetPipe, "running", pgMetAt("2026-10-05", "09:00:00"), nil},
		// pending, but pipeline_progress says it waits for the user (below).
		{"55555555-cccc-4000-8000-000000000007", pgMetPipe, "pending", pgMetAt("2026-10-05", "09:30:00"), nil},
		// Outside the window, one second either side.
		{"55555555-cccc-4000-8000-000000000008", pgMetPipe, "completed", pgMetAt("2026-10-02", "23:59:59"), pgMetAt("2026-10-03", "00:00:30")},
		{"55555555-cccc-4000-8000-000000000009", pgMetPipe, "completed", pgMetAt("2026-10-06", "00:00:00"), pgMetAt("2026-10-06", "00:00:30")},
		// Another workspace's run.
		{"55555555-cccc-4000-8000-00000000000a", pgMetPipe2, "completed", pgMetAt("2026-10-04", "09:00:00"), pgMetAt("2026-10-04", "09:00:05")},
		// The CDC audit row (id = pipeline id) is an FK anchor, not a run.
		{pgMetPipe, pgMetPipe, "completed", pgMetAt("2026-10-04", "12:00:00"), pgMetAt("2026-10-04", "12:00:01")},
	} {
		wfExec(t, d, `INSERT INTO executions (id, pipeline_id, status, start_time, end_time) VALUES ($1, $2, $3, $4, $5)`,
			e.id, e.pipe, e.status, e.start, e.end)
	}
	wfExec(t, d, `INSERT INTO pipeline_progress (pipeline_id, execution_id, status) VALUES ($1, $2, 'waiting_for_user')`,
		pgMetPipe, "55555555-cccc-4000-8000-000000000007")

	// Rows written: batch counts per run, and one CDC row (keyed by the pipeline id)
	// that its orchestration execution, run 3, owns.
	for _, s := range []struct {
		exec, table string
		inserted    any
		ins, upd    any
		orch        any
		mode        string
	}{
		{"55555555-cccc-4000-8000-000000000001", "public.orders", 100, nil, nil, nil, "batch"},
		{"55555555-cccc-4000-8000-000000000002", "public.orders", 5, nil, nil, nil, "batch"},
		{"55555555-cccc-4000-8000-000000000003", "public.orders", 40, nil, nil, nil, "batch"},
		{"55555555-cccc-4000-8000-000000000003", "public.items", 2, nil, nil, nil, "batch"},
		{pgMetPipe, "public.events", nil, 7, 3, "55555555-cccc-4000-8000-000000000003", "cdc"},
		{"55555555-cccc-4000-8000-000000000008", "public.orders", 1000, nil, nil, nil, "batch"},
	} {
		wfExec(t, d, `INSERT INTO pipeline_run_table_stats
			(pipeline_id, execution_id, table_name, qualified_name, mode, status, inserted_rows, applied_inserts, applied_updates, orchestration_execution_id)
			VALUES ($1, $2, $3, $3, $4, 'completed', $5, $6, $7, $8)`,
			pgMetPipe, s.exec, s.table, s.mode, s.inserted, s.ins, s.upd, s.orch)
	}
	wfExec(t, d, `INSERT INTO pipeline_run_table_stats (pipeline_id, execution_id, table_name, qualified_name, mode, status, inserted_rows)
		VALUES ($1, $2, 'public.orders', 'public.orders', 'batch', 'completed', 999)`, pgMetPipe2, "55555555-cccc-4000-8000-00000000000a")

	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, visibility, created_by) VALUES
		($1, $5, $7, 'public revenue', 'select 1', 'workspace', $10),
		($2, $5, $7, 'a private', 'select 1', 'private', $9),
		($3, $5, $7, 'b private', 'select 1', 'private', $10),
		($4, $6, $8, 'theirs', 'select 1', 'workspace', $10)`,
		pgMetQPub, pgMetQPrivA, pgMetQPrivB, pgMetQOther, pgMetWS, pgMetWS2, pgMetConn, pgMetConn2, pgMetUserA, pgMetUserB)

	// Model runs and the checks each ran.
	for _, r := range []struct {
		id, q, status string
		start, end    time.Time
		checks        []string
	}{
		{"77777777-cccc-4000-8000-000000000001", pgMetQPub, "succeeded", pgMetAt("2026-10-03", "01:00:00"), pgMetAt("2026-10-03", "01:00:04"), []string{"pass", "pass", "fail"}},
		{"77777777-cccc-4000-8000-000000000002", pgMetQPub, "failed", pgMetAt("2026-10-03", "02:00:00"), pgMetAt("2026-10-03", "02:00:06"), []string{"error"}},
		{"77777777-cccc-4000-8000-000000000003", pgMetQPub, "skipped", pgMetAt("2026-10-03", "03:00:00"), pgMetAt("2026-10-03", "03:00:00"), nil},
		{"77777777-cccc-4000-8000-000000000004", pgMetQPrivA, "succeeded", pgMetAt("2026-10-04", "05:00:00"), pgMetAt("2026-10-04", "05:00:02"), []string{"pass"}},
		{"77777777-cccc-4000-8000-000000000005", pgMetQPrivB, "succeeded", pgMetAt("2026-10-04", "06:00:00"), pgMetAt("2026-10-04", "06:00:09"), []string{"fail"}},
		{"77777777-cccc-4000-8000-000000000006", pgMetQOther, "failed", pgMetAt("2026-10-04", "07:00:00"), pgMetAt("2026-10-04", "07:00:09"), []string{"fail"}},
		{"77777777-cccc-4000-8000-000000000007", pgMetQPub, "succeeded", pgMetAt("2026-10-02", "12:00:00"), pgMetAt("2026-10-02", "12:00:09"), []string{"pass"}},
	} {
		wfExec(t, d, `INSERT INTO saved_query_runs (run_id, saved_query_id, trigger_source, status, started_at, finished_at)
			VALUES ($1, $2, 'scheduled', $3, $4, $5)`, r.id, r.q, r.status, r.start, r.end)
		for i, status := range r.checks {
			wfExec(t, d, `INSERT INTO saved_query_check_results (run_id, check_name, check_type, severity, status)
				VALUES ($1, $2, 'not_null', 'block', $3)`, r.id, "check "+string(rune('a'+i)), status)
		}
	}
	return d
}

func pgMetricsGet(t *testing.T, days string) ActivityMetrics {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgMetUserA)
		c.Set(ctxWorkspaceID, pgMetWS)
		c.Set(ctxWorkspaceRole, "viewer")
		c.Next()
	})
	r.GET("/api/v1/activity/metrics", ActivityMetricsHandler)
	r.GET("/api/v1/activity", ListActivity)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/activity/metrics?days="+days, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /activity/metrics: %d %s", w.Code, w.Body.String())
	}
	var body ActivityMetrics
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func pgMetSameTimes(t *testing.T, name string, got, want []MetricRunTime) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v, want %+v", name, got, want)
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Day != w.Day || g.Runs != w.Runs ||
			math.Abs(g.MedianSeconds-w.MedianSeconds) > 1e-9 || math.Abs(g.P95Seconds-w.P95Seconds) > 1e-9 {
			t.Errorf("%s[%d] = %+v, want %+v", name, i, g, w)
		}
	}
}

func TestPG_ActivityMetrics_EveryNumberEqualsAHandCount(t *testing.T) {
	pgMetricsDB(t)
	got := pgMetricsGet(t, "3")

	if want := []string{"2026-10-03", "2026-10-04", "2026-10-05"}; !reflect.DeepEqual(got.Days, want) {
		t.Errorf("days = %v, want %v", got.Days, want)
	}
	// 10-03: run 1 completed, run 2 failed (23:30 UTC). 10-04: run 3 success, run 4
	// silent drop. 10-05: run 5 cancelled, run 6 running, run 7 waiting for the user.
	// Never the audit row, another workspace's run or the two just outside.
	wantRuns := []MetricCount{
		{"2026-10-03", activitySucceeded, 1}, {"2026-10-03", activityFailed, 1},
		{"2026-10-04", activitySucceeded, 1}, {"2026-10-04", activityFailed, 1},
		{"2026-10-05", activityCancelled, 1}, {"2026-10-05", activityWaiting, 1}, {"2026-10-05", activityRunning, 1},
	}
	if !reflect.DeepEqual(got.PipelineRuns, wantRuns) {
		t.Errorf("pipeline_runs = %+v\nwant %+v", got.PipelineRuns, wantRuns)
	}
	// 10-03: 100 + 5. 10-04: run 3's 40 + 2 batch rows and the CDC row it owns, 7 + 3.
	wantRows := []MetricRows{{"2026-10-03", 105}, {"2026-10-04", 52}, {"2026-10-05", 0}}
	if !reflect.DeepEqual(got.RowsWritten, wantRows) {
		t.Errorf("rows_written = %+v, want %+v", got.RowsWritten, wantRows)
	}
	// 10-03: 10 s and 30 s → median 20, p95 10 + 0.95 × 20 = 29. 10-04: 60 s and
	// 20 s → median 40, p95 20 + 0.95 × 40 = 58. 10-05: nothing ended in success or failure.
	pgMetSameTimes(t, "pipeline_run_time", got.PipelineRunTime, []MetricRunTime{
		{"2026-10-03", 2, 20, 29}, {"2026-10-04", 2, 40, 58},
	})
	// The caller sees the workspace's query and their own private one; never B's
	// private query, another workspace's, or a run outside the window.
	wantModels := []MetricCount{
		{"2026-10-03", activitySucceeded, 1}, {"2026-10-03", activityFailed, 1}, {"2026-10-03", activitySkipped, 1},
		{"2026-10-04", activitySucceeded, 1},
	}
	if !reflect.DeepEqual(got.ModelRuns, wantModels) {
		t.Errorf("model_runs = %+v\nwant %+v", got.ModelRuns, wantModels)
	}
	// 10-03: 4 s and 6 s (the skip did no work) → median 5, p95 4 + 0.95 × 2 = 5.9.
	pgMetSameTimes(t, "model_run_time", got.ModelRunTime, []MetricRunTime{
		{"2026-10-03", 2, 5, 5.9}, {"2026-10-04", 1, 2, 2},
	})
	wantChecks := []MetricCount{
		{"2026-10-03", checkPass, 2}, {"2026-10-03", checkFail, 1}, {"2026-10-03", checkError, 1},
		{"2026-10-04", checkPass, 1},
	}
	if !reflect.DeepEqual(got.Checks, wantChecks) {
		t.Errorf("checks = %+v\nwant %+v", got.Checks, wantChecks)
	}
}

// The charts count the runs the list beside them shows: walk GET /activity, put
// each pipeline and model run in its UTC start day, and the counts must match.
func TestPG_ActivityMetrics_CountsTheRunsTheListShows(t *testing.T) {
	pgMetricsDB(t)
	got := pgMetricsGet(t, "3")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgMetUserA)
		c.Set(ctxWorkspaceID, pgMetWS)
		c.Set(ctxWorkspaceRole, "viewer")
		c.Next()
	})
	r.GET("/api/v1/activity", ListActivity)
	listed := map[string]map[MetricCount]int64{activityPipeline: {}, activityScheduledQuery: {}}
	from, to := pgMetAt("2026-10-03", "00:00:00"), pgMetAt("2026-10-06", "00:00:00")
	params := url.Values{"limit": {"2"}}
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("the walk did not end")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/activity?"+params.Encode(), nil))
		var body activityBody
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("GET /activity: %d %s", w.Code, w.Body.String())
		}
		for _, run := range body.Runs {
			if m, ok := listed[run.Kind]; ok && !run.StartedAt.Before(from) && run.StartedAt.Before(to) {
				m[MetricCount{Day: run.StartedAt.UTC().Format("2006-01-02"), Status: run.Status}]++
			}
		}
		if body.NextCursor == "" {
			break
		}
		params.Set("cursor", body.NextCursor)
	}
	for kind, series := range map[string][]MetricCount{activityPipeline: got.PipelineRuns, activityScheduledQuery: got.ModelRuns} {
		charted := map[MetricCount]int64{}
		for _, p := range series {
			charted[MetricCount{Day: p.Day, Status: p.Status}] = p.Count
		}
		if !reflect.DeepEqual(charted, listed[kind]) {
			t.Errorf("%s: the chart counts %v, the list shows %v", kind, charted, listed[kind])
		}
		if len(charted) == 0 {
			t.Errorf("%s: nothing to compare — the seed did not land", kind)
		}
	}
}
