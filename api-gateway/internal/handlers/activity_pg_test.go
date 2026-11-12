//go:build integration_pg

// Real-Postgres coverage for GET /api/v1/activity (migration 132): what sqlmock
// cannot judge. A page walk over runs of all three kinds that share one instant
// must list every visible run exactly once, in order, at any page size; the
// status filters must match the real executionStatusSQL; and the stale-run repair
// must still run once the Executions page sends people here.
//
// Needs a Postgres with every api-gateway migration applied (the test applies
// them itself) and nothing else in it:
//
//	docker run -d --name act-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55472:5432 postgres:16-alpine
//	ACT_PG_DSN='postgres://postgres:verify@localhost:55472/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Activity -v
package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgActUserA = "11111111-bbbb-4000-8000-000000000001" // the caller
	pgActUserB = "11111111-bbbb-4000-8000-000000000002" // another member
	pgActWS    = "22222222-bbbb-4000-8000-000000000001"
	pgActWS2   = "22222222-bbbb-4000-8000-000000000002" // someone else's workspace
	pgActConn  = "33333333-bbbb-4000-8000-000000000001"
	pgActPipe  = "44444444-bbbb-4000-8000-000000000001"
	pgActPipe2 = "44444444-bbbb-4000-8000-000000000002"

	pgActE1     = "55555555-bbbb-4000-8000-000000000009" // completed at T0
	pgActE2     = "55555555-bbbb-4000-8000-000000000008" // silent drop at T0
	pgActEStale = "55555555-bbbb-4000-8000-000000000007" // running, but its pipeline moved on
	pgActEWait  = "55555555-bbbb-4000-8000-000000000006" // pending
	pgActEOther = "55555555-bbbb-4000-8000-000000000005" // other workspace
	pgActENull  = "55555555-bbbb-4000-8000-000000000004" // never started

	pgActQPub   = "66666666-bbbb-4000-8000-000000000001"
	pgActQPrivB = "66666666-bbbb-4000-8000-000000000002"
	pgActQPrivA = "66666666-bbbb-4000-8000-000000000003"
	pgActR1     = "77777777-bbbb-4000-8000-000000000009" // public, succeeded at T0
	pgActR2     = "77777777-bbbb-4000-8000-000000000008" // public, skipped at T0
	pgActR3     = "77777777-bbbb-4000-8000-000000000007" // B's private query: hidden
	pgActR4     = "77777777-bbbb-4000-8000-000000000006" // A's private query, failed

	pgActFlow   = "88888888-bbbb-4000-8000-000000000001"
	pgActFlow2  = "88888888-bbbb-4000-8000-000000000002"
	pgActWR1    = "99999999-bbbb-4000-8000-000000000009" // running at T0
	pgActWR2    = "99999999-bbbb-4000-8000-000000000008" // succeeded at T0
	pgActWR3    = "99999999-bbbb-4000-8000-000000000007" // failed
	pgActWROthr = "99999999-bbbb-4000-8000-000000000006" // other workspace
)

// The caller's runs in page order: at T0 the kind breaks the tie (workflow,
// scheduled_query, pipeline), then the id, all descending.
var pgActWantOrder = []string{
	pgActWR1, pgActWR2, pgActR1, pgActR2, pgActE1, pgActE2,
	pgActR4, pgActWR3, pgActEStale, pgActEWait,
}

func pgActivityDB(t *testing.T) *sql.DB {
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
	for _, ws := range []string{pgActWS, pgActWS2} {
		wfExec(t, d, `DELETE FROM workspaces WHERE id = $1`, ws)
	}
	wfExec(t, d, `DELETE FROM pipeline_progress WHERE pipeline_id IN ($1, $2)`, pgActPipe, pgActPipe2)
	wfExec(t, d, `DELETE FROM pipelines WHERE id IN ($1, $2)`, pgActPipe, pgActPipe2)
	wfExec(t, d, `DELETE FROM users WHERE id IN ($1, $2)`, pgActUserA, pgActUserB)

	t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	at := func(d time.Duration) time.Time { return t0.Add(-d) }

	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'act-a@example.com', 'x'), ($2, 'act-b@example.com', 'x')`,
		pgActUserA, pgActUserB)
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Act', 'act-pg', $3), ($2, 'Other', 'act-pg-2', $4)`,
		pgActWS, pgActWS2, pgActUserA, pgActUserB)
	wfExec(t, d, `INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id)
		VALUES ($1, $2, 'warehouse', 'source', 'postgresql', 'x', $3)`, pgActConn, pgActUserA, pgActWS)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id)
		VALUES ($1, 'orders load', 'x', $2), ($3, 'other load', 'x', $4)`, pgActPipe, pgActWS, pgActPipe2, pgActWS2)

	for _, e := range []struct {
		id, pipe, status string
		start            any
	}{
		{pgActE1, pgActPipe, "completed", t0},
		{pgActE2, pgActPipe, "silent_drop_detected", t0},
		{pgActEStale, pgActPipe, "running", at(30 * time.Minute)},
		{pgActEWait, pgActPipe, "pending", at(40 * time.Minute)},
		{pgActEOther, pgActPipe2, "completed", t0},
		{pgActENull, pgActPipe, "pending", nil},
		// The CDC audit row (id = pipeline id) is an FK anchor, not a run.
		{pgActPipe, pgActPipe, "completed", t0},
	} {
		wfExec(t, d, `INSERT INTO executions (id, pipeline_id, status, start_time) VALUES ($1, $2, $3, $4)`,
			e.id, e.pipe, e.status, e.start)
	}
	// The pipeline has moved on to E1, which finished: the older running row is stale.
	wfExec(t, d, `INSERT INTO pipeline_progress (pipeline_id, execution_id, status) VALUES ($1, $2, 'completed')`,
		pgActPipe, pgActE1)

	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, visibility, created_by) VALUES
		($1, $4, $5, 'public revenue', 'select 1', 'workspace', $7),
		($2, $4, $5, 'b private', 'select 1', 'private', $7),
		($3, $4, $5, 'a private', 'select 1', 'private', $6)`,
		pgActQPub, pgActQPrivB, pgActQPrivA, pgActWS, pgActConn, pgActUserA, pgActUserB)
	for _, r := range []struct {
		id, q, status string
		start         time.Time
		errText       any
	}{
		{pgActR1, pgActQPub, "succeeded", t0, nil},
		{pgActR2, pgActQPub, "skipped", t0, nil},
		{pgActR3, pgActQPrivB, "failed", t0, "b's error"},
		{pgActR4, pgActQPrivA, "failed", at(time.Minute), storedRawRunError},
	} {
		wfExec(t, d, `INSERT INTO saved_query_runs (run_id, saved_query_id, trigger_source, status, error, started_at, finished_at)
			VALUES ($1, $2, 'scheduled', $3, $4, $5, $5)`, r.id, r.q, r.status, r.errText, r.start)
	}

	wfExec(t, d, `INSERT INTO workflows (id, workspace_id, name, definition, trigger_kind) VALUES
		($1, $3, 'low stock', '{}', 'manual'), ($2, $4, 'other flow', '{}', 'manual')`,
		pgActFlow, pgActFlow2, pgActWS, pgActWS2)
	for _, r := range []struct {
		id, flow, ws, status string
		start                time.Time
		finished             any
	}{
		{pgActWR1, pgActFlow, pgActWS, "running", t0, nil},
		{pgActWR2, pgActFlow, pgActWS, "succeeded", t0, t0},
		{pgActWR3, pgActFlow, pgActWS, "failed", at(2 * time.Minute), at(2 * time.Minute)},
		{pgActWROthr, pgActFlow2, pgActWS2, "succeeded", t0, t0},
	} {
		wfExec(t, d, `INSERT INTO workflow_runs (id, workflow_id, workspace_id, trigger, status, started_at, finished_at)
			VALUES ($1, $2, $3, 'manual', $4, $5, $6)`, r.id, r.flow, r.ws, r.status, r.start, r.finished)
	}
	return d
}

func pgActivityPage(t *testing.T, params url.Values) activityBody {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgActUserA)
		c.Set(ctxWorkspaceID, pgActWS)
		c.Set(ctxWorkspaceRole, "viewer")
		c.Next()
	})
	r.GET("/api/v1/activity", ListActivity)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/activity?"+params.Encode(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /activity?%s: %d %s", params.Encode(), w.Code, w.Body.String())
	}
	var body activityBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// pgActivityWalk follows next_cursor to the end and returns every run seen.
func pgActivityWalk(t *testing.T, params url.Values, limit int) []ActivityRun {
	t.Helper()
	var all []ActivityRun
	params.Set("limit", strconv.Itoa(limit))
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("the walk did not end: a cursor that never advances")
		}
		body := pgActivityPage(t, params)
		if len(body.Runs) > limit {
			t.Fatalf("a page of %d holds %d runs", limit, len(body.Runs))
		}
		all = append(all, body.Runs...)
		if body.NextCursor == "" {
			return all
		}
		params.Set("cursor", body.NextCursor)
	}
}

func pgActIDs(runs []ActivityRun) []string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

func TestPG_Activity_EveryPageSizeWalksEachVisibleRunOnceInOrder(t *testing.T) {
	d := pgActivityDB(t)
	for _, limit := range []int{1, 2, 3, 4, 50} {
		got := pgActIDs(pgActivityWalk(t, url.Values{}, limit))
		if strings.Join(got, ",") != strings.Join(pgActWantOrder, ",") {
			t.Fatalf("limit %d walked\n  %v\nwant\n  %v", limit, got, pgActWantOrder)
		}
	}

	runs := pgActivityWalk(t, url.Values{}, 50)
	byID := map[string]ActivityRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	if r := byID[pgActEStale]; r.Status != activityCancelled {
		t.Errorf("the stale run reads %q, want the repair to have cancelled it", r.Status)
	}
	var stored string
	if err := d.QueryRow(`SELECT status FROM executions WHERE id = $1`, pgActEStale).Scan(&stored); err != nil || stored != "cancelled" {
		t.Errorf("executions row is %q (%v), want cancelled by the repair", stored, err)
	}
	if r := byID[pgActE2]; r.Status != activityFailed || r.SourceStatus != "silent_drop_detected" || r.SubjectName != "orders load" {
		t.Errorf("silent drop run = %+v", r)
	}
	if r := byID[pgActWR1]; r.Status != activityRunning || r.FinishedAt != nil || r.SubjectID != pgActFlow {
		t.Errorf("running workflow run = %+v", r)
	}
	assertNoSourceAddress(t, "GET /activity (Postgres)", byID[pgActR4].Error)
}

func TestPG_Activity_FiltersMatchTheRealStatus(t *testing.T) {
	pgActivityDB(t)
	// The first read repairs the stale run; every filter below sees it cancelled.
	pgActivityPage(t, url.Values{})
	cases := map[string][]string{
		"running":   {pgActWR1, pgActEWait}, // pending reads as still going
		"succeeded": {pgActWR2, pgActR1, pgActE1},
		"failed":    {pgActE2, pgActR4, pgActWR3},
		"skipped":   {pgActR2},
		"cancelled": {pgActEStale},
		"waiting":   nil,
	}
	for status, want := range cases {
		for _, limit := range []int{1, 50} {
			got := pgActIDs(pgActivityWalk(t, url.Values{"status": {status}}, limit))
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("status=%s limit=%d: got %v, want %v", status, limit, got, want)
			}
		}
	}
	got := pgActIDs(pgActivityWalk(t, url.Values{"kind": {activityScheduledQuery}}, 1))
	if want := []string{pgActR1, pgActR2, pgActR4}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kind=scheduled_query: got %v, want %v (never B's private query)", got, want)
	}
}

// A model rebuilt by a workflow step is stored as "triggered" with upstream_kind
// 'workflow'; the list sends "workflow" for it, and "triggered" still for a model an
// upstream woke, so neither reads as the other.
func TestPG_Activity_WorkflowStartedModelRunSaysWorkflow(t *testing.T) {
	d := pgActivityDB(t)
	wfExec(t, d, `UPDATE saved_query_runs SET trigger_source = 'triggered', upstream_kind = 'workflow', upstream_id = $2
		WHERE run_id = $1`, pgActR1, pgActFlow)
	wfExec(t, d, `UPDATE saved_query_runs SET trigger_source = 'triggered', upstream_kind = 'pipeline', upstream_id = $2
		WHERE run_id = $1`, pgActR2, pgActPipe)
	got := map[string]string{}
	for _, r := range pgActivityWalk(t, url.Values{"kind": {activityScheduledQuery}}, 50) {
		got[r.ID] = r.Trigger
	}
	if got[pgActR1] != "workflow" || got[pgActR2] != "triggered" || got[pgActR4] != "scheduled" {
		t.Fatalf("triggers %v, want R1 workflow, R2 triggered, R4 scheduled", got)
	}
}
