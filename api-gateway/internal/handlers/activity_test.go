package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// GET /api/v1/activity against sqlmock: who sees which runs, the merge order, the
// keyset predicates each kind gets, and which kinds a filter reads at all. The
// paging walk itself, ties included, runs against a real Postgres in
// activity_pg_test.go.

var activityCols = []string{"id", "subject_id", "subject_name", "status", "trigger", "started_at", "finished_at", "error"}

// A pipeline run also carries the schedule that started it.
var pipelineActivityCols = append(append([]string{}, activityCols...), "schedule_id")

const actSchedule = "eeeeeeee-0000-4000-8000-00000000000a"

const (
	actPipeA = "aaaaaaaa-0000-4000-8000-00000000000a"
	actPipeB = "aaaaaaaa-0000-4000-8000-00000000000b"
	actQuery = "bbbbbbbb-0000-4000-8000-00000000000a"
	actFlow  = "cccccccc-0000-4000-8000-00000000000a"
	actRun1  = "dddddddd-0000-4000-8000-000000000001"
	actRun2  = "dddddddd-0000-4000-8000-000000000002"
	actRun3  = "dddddddd-0000-4000-8000-000000000003"
	actRun4  = "dddddddd-0000-4000-8000-000000000004"
)

var actT0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type activityBody struct {
	Runs       []ActivityRun `json:"runs"`
	NextCursor string        `json:"next_cursor"`
}

func activityGet(t *testing.T, role, query string) (*httptest.ResponseRecorder, activityBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/api/v1/activity", ListActivity)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/activity"+query, nil))
	var body activityBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w, body
}

// timeArg matches a bound time by instant, not by representation.
type timeArg struct{ t time.Time }

func (a timeArg) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	return ok && got.Equal(a.t)
}

func expectActivityRepair(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`UPDATE executions e`).WithArgs(wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestListActivity_MergesEveryKindNewestFirst(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectActivityRepair(mock)
	// The workspace's pipelines, never the synthetic CDC row (id = pipeline id).
	mock.ExpectQuery(`(?s)FROM executions e.*WHERE p\.workspace_id = \$1 AND e\.id <> e\.pipeline_id AND e\.start_time IS NOT NULL\s+ORDER BY e\.start_time DESC, e\.id DESC\s+LIMIT 51`).
		WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows(pipelineActivityCols).
			AddRow(actRun2, actPipeA, "orders sync", "silent_drop_detected", "scheduled", actT0.Add(-time.Minute), actT0, "dropped rows", actSchedule).
			AddRow(actRun4, actPipeB, "users cdc", "processing", "manual", actT0.Add(-10*time.Minute), nil, "", ""))
	// A private query's runs are its author's: the caller is bound as $2.
	mock.ExpectQuery(`(?s)FROM saved_query_runs r\s+JOIN saved_queries sq ON sq\.id = r\.saved_query_id\s+WHERE sq\.workspace_id = \$1 AND \(sq\.visibility = 'workspace' OR sq\.created_by = \$2\)\s+ORDER BY r\.started_at DESC, r\.run_id DESC\s+LIMIT 51`).
		WithArgs(wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows(activityCols).
			AddRow(actRun3, actQuery, "daily revenue", "succeeded", "scheduled", actT0.Add(-5*time.Minute), actT0.Add(-4*time.Minute), ""))
	mock.ExpectQuery(`(?s)FROM workflow_runs r\s+JOIN workflows w ON w\.id = r\.workflow_id AND w\.workspace_id = r\.workspace_id\s+WHERE r\.workspace_id = \$1\s+ORDER BY r\.started_at DESC, r\.id DESC\s+LIMIT 51`).
		WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows(activityCols).
			AddRow(actRun1, actFlow, "low stock alert", "failed", "schedule", actT0, actT0, strings.Repeat("é", 400)))

	w, body := activityGet(t, "viewer", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if body.NextCursor != "" {
		t.Fatalf("one page holds all four runs, got next_cursor %q", body.NextCursor)
	}
	type row struct{ kind, id, status, source string }
	want := []row{
		{activityWorkflow, actRun1, activityFailed, "failed"},
		{activityPipeline, actRun2, activityFailed, "silent_drop_detected"},
		{activityScheduledQuery, actRun3, activitySucceeded, "succeeded"},
		{activityPipeline, actRun4, activityRunning, "processing"},
	}
	if len(body.Runs) != len(want) {
		t.Fatalf("got %d runs, want %d: %+v", len(body.Runs), len(want), body.Runs)
	}
	for i, r := range body.Runs {
		if got := (row{r.Kind, r.ID, r.Status, r.SourceStatus}); got != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, got, want[i])
		}
	}
	if body.Runs[1].SubjectID != actPipeA || body.Runs[1].SubjectName != "orders sync" {
		t.Errorf("pipeline run lost its pipeline: %+v", body.Runs[1])
	}
	if body.Runs[1].ScheduleID != actSchedule || body.Runs[3].ScheduleID != "" {
		t.Errorf("schedule ids = %q, %q; want %q and none", body.Runs[1].ScheduleID, body.Runs[3].ScheduleID, actSchedule)
	}
	if body.Runs[3].FinishedAt != nil {
		t.Errorf("a run still going has no finish time, got %v", body.Runs[3].FinishedAt)
	}
	if got := []rune(body.Runs[0].Error); len(got) != activityErrorMaxRunes+1 || got[len(got)-1] != '…' {
		t.Errorf("a long error is cut to %d runes plus an ellipsis, got %d runes", activityErrorMaxRunes, len(got))
	}
}

func TestListActivity_CursorPagesAcrossKindsWithTies(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	// Page 1, two per page: a scheduled query and two pipeline runs share actT0.
	// At one instant the kind breaks the tie (descending), then the id.
	expectActivityRepair(mock)
	mock.ExpectQuery(`FROM executions e`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows(pipelineActivityCols).
			AddRow(actRun4, actPipeA, "p", "completed", "manual", actT0, actT0, "", "").
			AddRow(actRun2, actPipeA, "p", "completed", "manual", actT0, actT0, "", "").
			AddRow(actRun1, actPipeA, "p", "completed", "manual", actT0.Add(-time.Hour), actT0, "", ""))
	mock.ExpectQuery(`FROM saved_query_runs r`).WithArgs(wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows(activityCols).
			AddRow(actRun3, actQuery, "q", "succeeded", "manual", actT0, actT0, ""))
	mock.ExpectQuery(`FROM workflow_runs r`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows(activityCols))

	w, body := activityGet(t, "viewer", "?limit=2")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(body.Runs) != 2 || body.Runs[0].ID != actRun3 || body.Runs[1].ID != actRun4 {
		t.Fatalf("page 1 = %+v, want the scheduled query then the higher pipeline id", body.Runs)
	}
	if body.NextCursor == "" {
		t.Fatal("more runs remain, so page 1 must carry a next_cursor")
	}
	c, err := decodeActivityCursor(body.NextCursor)
	if err != nil || !c.at.Equal(actT0) || c.kind != activityPipeline || c.id != actRun4 {
		t.Fatalf("cursor = %+v (%v), want the last row of page 1", c, err)
	}

	// Page 2 from that cursor. Pipeline is the cursor's kind, so the id breaks the
	// tie; scheduled_query and workflow sort before pipeline at actT0, so their
	// actT0 rows were on page 1 and only strictly older ones remain.
	expectActivityRepair(mock)
	mock.ExpectQuery(`AND \(e\.start_time, e\.id\) < \(\$2::timestamptz, \$3::uuid\)`).
		WithArgs(wsScopeWS, timeArg{actT0}, actRun4).
		WillReturnRows(sqlmock.NewRows(activityCols))
	mock.ExpectQuery(`AND r\.started_at < \$3::timestamptz\s+ORDER BY`).
		WithArgs(wsScopeWS, wsScopeUser, timeArg{actT0}).
		WillReturnRows(sqlmock.NewRows(activityCols))
	mock.ExpectQuery(`AND r\.started_at < \$2::timestamptz\s+ORDER BY`).
		WithArgs(wsScopeWS, timeArg{actT0}).
		WillReturnRows(sqlmock.NewRows(activityCols))
	if w, _ := activityGet(t, "viewer", "?limit=2&cursor="+body.NextCursor); w.Code != http.StatusOK {
		t.Fatalf("page 2 status %d: %s", w.Code, w.Body.String())
	}

	// A cursor on a scheduled query: pipeline sorts after it at the same instant,
	// so pipeline runs AT that instant are still to come (<=).
	cur := encodeActivityCursor(ActivityRun{Kind: activityScheduledQuery, ID: actRun3, StartedAt: actT0})
	expectActivityRepair(mock)
	mock.ExpectQuery(`AND e\.start_time <= \$2::timestamptz\s+ORDER BY`).
		WithArgs(wsScopeWS, timeArg{actT0}).
		WillReturnRows(sqlmock.NewRows(activityCols))
	mock.ExpectQuery(`AND \(r\.started_at, r\.run_id\) < \(\$3::timestamptz, \$4::uuid\)`).
		WithArgs(wsScopeWS, wsScopeUser, timeArg{actT0}, actRun3).
		WillReturnRows(sqlmock.NewRows(activityCols))
	mock.ExpectQuery(`AND r\.started_at < \$2::timestamptz\s+ORDER BY`).
		WithArgs(wsScopeWS, timeArg{actT0}).
		WillReturnRows(sqlmock.NewRows(activityCols))
	if w, _ := activityGet(t, "viewer", "?cursor="+cur); w.Code != http.StatusOK {
		t.Fatalf("scheduled-query cursor status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A status filter reads only the kinds that can be in that status, and matches
// a pipeline run by the status the executions endpoints report.
func TestListActivity_StatusAndKindFiltersReadOnlyWhatCanMatch(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		expect func(sqlmock.Sqlmock)
	}{
		{"skipped is a scheduled query's word only", "?status=skipped", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`(?s)FROM saved_query_runs r.*AND r\.status = \$3`).
				WithArgs(wsScopeWS, wsScopeUser, "skipped").WillReturnRows(sqlmock.NewRows(activityCols))
		}},
		{"cancelled is a pipeline's word only", "?status=cancelled", func(m sqlmock.Sqlmock) {
			expectActivityRepair(m)
			m.ExpectQuery(`(?s)FROM executions e.* IN \(\$2\)`).
				WithArgs(wsScopeWS, "cancelled").WillReturnRows(sqlmock.NewRows(activityCols))
		}},
		{"failed spans every kind and each failed pipeline status", "?status=failed", func(m sqlmock.Sqlmock) {
			expectActivityRepair(m)
			m.ExpectQuery(`(?s)FROM executions e.* IN \(\$2, \$3, \$4, \$5, \$6\)`).
				WithArgs(wsScopeWS, "credential_check_failed", "error", "failed", "silent_drop_detected", "silent_partial_drop_detected").
				WillReturnRows(sqlmock.NewRows(activityCols))
			m.ExpectQuery(`(?s)FROM saved_query_runs r.*AND r\.status = \$3`).
				WithArgs(wsScopeWS, wsScopeUser, "failed").WillReturnRows(sqlmock.NewRows(activityCols))
			m.ExpectQuery(`(?s)FROM workflow_runs r.*AND r\.status = \$2`).
				WithArgs(wsScopeWS, "failed").WillReturnRows(sqlmock.NewRows(activityCols))
		}},
		{"running is every pipeline status that is not a known other", "?status=running", func(m sqlmock.Sqlmock) {
			expectActivityRepair(m)
			m.ExpectQuery(`(?s)FROM executions e.*, ''\) NOT IN \(\$2, \$3, \$4, \$5, \$6, \$7, \$8, \$9, \$10\)`).
				WithArgs(wsScopeWS, "cancelled", "completed", "credential_check_failed", "error", "failed",
					"silent_drop_detected", "silent_partial_drop_detected", "success", "waiting_for_user").
				WillReturnRows(sqlmock.NewRows(activityCols))
			m.ExpectQuery(`(?s)FROM workflow_runs r.*AND r\.status = \$2`).
				WithArgs(wsScopeWS, "running").WillReturnRows(sqlmock.NewRows(activityCols))
		}},
		{"one kind reads one table and skips the repair", "?kind=workflow", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`FROM workflow_runs r`).WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows(activityCols))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			tc.expect(mock)
			w, body := activityGet(t, "viewer", tc.query)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if body.Runs == nil {
				t.Fatal("no runs is an empty list, not null")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListActivity_RejectsBadParamsBeforeTheDatabase(t *testing.T) {
	cur := encodeActivityCursor(ActivityRun{Kind: activityPipeline, ID: actRun1, StartedAt: actT0})
	for _, q := range []string{
		"?kind=pipelines", "?status=completed", "?limit=0", "?limit=201", "?limit=x",
		"?cursor=nope", "?cursor=" + cur[:len(cur)-4],
	} {
		t.Run(q, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			if w, _ := activityGet(t, "viewer", q); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListActivity_NeedsAWorkspaceRole(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	if w, _ := activityGet(t, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("no role: status %d, want 403", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An install whose migrations have not reached a kind's table yet still lists
// the others; any other read failure is a 500, never a short list.
func TestListActivity_MissingTableIsSkippedOtherErrorsAreNot(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectActivityRepair(mock)
	mock.ExpectQuery(`FROM executions e`).WillReturnRows(sqlmock.NewRows(pipelineActivityCols).
		AddRow(actRun1, actPipeA, "p", "completed", "manual", actT0, actT0, "", ""))
	mock.ExpectQuery(`FROM saved_query_runs r`).WillReturnRows(sqlmock.NewRows(activityCols))
	mock.ExpectQuery(`FROM workflow_runs r`).
		WillReturnError(errors.New(`ERROR: relation "workflow_runs" does not exist (SQLSTATE 42P01)`))
	w, body := activityGet(t, "viewer", "")
	if w.Code != http.StatusOK || len(body.Runs) != 1 {
		t.Fatalf("status %d runs %d, want 200 with the pipeline run: %s", w.Code, len(body.Runs), w.Body.String())
	}

	expectActivityRepair(mock)
	mock.ExpectQuery(`FROM executions e`).WillReturnError(errors.New("conn reset"))
	if w, _ := activityGet(t, "viewer", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The Runs page is one more route that returns a scheduled query's stored error
// text (stored_run_error_display_test.go), so it scrubs on the way out too.
func TestListActivity_ScheduledQueryErrorIsScrubbed(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_query_runs r`).WillReturnRows(sqlmock.NewRows(activityCols).
		AddRow(actRun3, actQuery, "q", "failed", "scheduled", actT0, actT0, storedRawRunError))
	w, body := activityGet(t, "viewer", "?kind=scheduled_query")
	if w.Code != http.StatusOK || len(body.Runs) != 1 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoSourceAddress(t, "GET /activity", body.Runs[0].Error)
}

// A workflow step's model rebuild is stored as "triggered", the word for an upstream
// finishing, with upstream_kind 'workflow' (workflow_step_models.go). The list read
// the stored word alone and called it "After its upstream".
func TestListActivity_WorkflowStartedModelRunSaysWorkflow(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`(?s)CASE WHEN r\.upstream_kind = 'workflow' THEN 'workflow' ELSE r\.trigger_source END.*FROM saved_query_runs r`).
		WillReturnRows(sqlmock.NewRows(activityCols).
			AddRow(actRun3, actQuery, "q", "succeeded", "workflow", actT0, actT0, ""))
	w, body := activityGet(t, "viewer", "?kind=scheduled_query")
	if w.Code != http.StatusOK || len(body.Runs) != 1 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := body.Runs[0].Trigger; got != "workflow" {
		t.Fatalf("trigger %q, want workflow", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
