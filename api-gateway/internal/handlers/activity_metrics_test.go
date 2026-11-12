package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// GET /api/v1/activity/metrics against sqlmock: the window, who sees which runs,
// which runs a run time is measured over, and how the statuses are folded. That
// the sums equal a hand count over real rows is activity_metrics_pg_test.go.

var (
	metricsNow  = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	metricsFrom = time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	metricsTo   = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
)

var (
	metricCountCols    = []string{"day", "status", "count"}
	metricPipelineCols = []string{"day", "status", "count", "rows"}
	metricTimeCols     = []string{"day", "runs", "median", "p95"}
)

func pinMetricsClock(t *testing.T) {
	t.Helper()
	prev := activityMetricsNow
	activityMetricsNow = func() time.Time { return metricsNow }
	t.Cleanup(func() { activityMetricsNow = prev })
}

func metricsGet(t *testing.T, role, query string) (*httptest.ResponseRecorder, ActivityMetrics) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/api/v1/activity/metrics", ActivityMetricsHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/activity/metrics"+query, nil))
	var body ActivityMetrics
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w, body
}

// expectMetricsQueries expects the five reads in the order the handler makes them,
// each bound to the caller's workspace (and, for a model, the caller) and the window.
func expectMetricsQueries(mock sqlmock.Sqlmock, pipelines, pipelineTimes, models, modelTimes, checks *sqlmock.Rows) {
	win := []driver.Value{timeArg{metricsFrom}, timeArg{metricsTo}}
	// The list's pipeline rule: the workspace's pipelines, never the CDC audit row.
	mock.ExpectQuery(`(?s)FROM executions e.*WHERE p\.workspace_id = \$1 AND e\.id <> e\.pipeline_id AND e\.start_time >= \$2 AND e\.start_time < \$3\s+GROUP BY 1, 2`).
		WithArgs(append([]driver.Value{wsScopeWS}, win...)...).WillReturnRows(pipelines)
	// Run time over the runs that ended in success or failure, with an end.
	finished := pipelineSourceStatuses(activitySucceeded, activityFailed)
	pipeTimeArgs := append([]driver.Value{wsScopeWS}, win...)
	for _, s := range finished {
		pipeTimeArgs = append(pipeTimeArgs, s)
	}
	mock.ExpectQuery(`(?s)percentile_cont\(0\.5\).*FROM executions e.*AND e\.start_time < \$3 AND \(CASE.*\) IN \(\$4, .*\) AND \(CASE.*\) IS NOT NULL`).
		WithArgs(pipeTimeArgs...).WillReturnRows(pipelineTimes)
	// The list's visibility rule: a private query's runs are its author's ($2).
	modelArgs := append([]driver.Value{wsScopeWS, wsScopeUser}, win...)
	mock.ExpectQuery(`(?s)FROM saved_query_runs r\s+JOIN saved_queries sq ON sq\.id = r\.saved_query_id\s+WHERE sq\.workspace_id = \$1 AND \(sq\.visibility = 'workspace' OR sq\.created_by = \$2\) AND r\.started_at >= \$3 AND r\.started_at < \$4\s+GROUP BY 1, 2`).
		WithArgs(modelArgs...).WillReturnRows(models)
	mock.ExpectQuery(`(?s)percentile_cont.*FROM saved_query_runs r.*sq\.created_by = \$2\).*AND r\.status IN \(\$5, \$6\)`).
		WithArgs(append(append([]driver.Value{}, modelArgs...), activitySucceeded, activityFailed)...).WillReturnRows(modelTimes)
	mock.ExpectQuery(`(?s)FROM saved_query_check_results c\s+JOIN saved_query_runs r ON r\.run_id = c\.run_id\s+JOIN saved_queries sq ON sq\.id = r\.saved_query_id\s+WHERE sq\.workspace_id = \$1 AND \(sq\.visibility = 'workspace' OR sq\.created_by = \$2\)`).
		WithArgs(modelArgs...).WillReturnRows(checks)
}

func TestActivityMetrics_FoldsStatusesAndScopesEveryRead(t *testing.T) {
	pinMetricsClock(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectMetricsQueries(mock,
		// Two source words that are both "succeeded" fold into one bar segment, and
		// every status's rows add up to the day's rows written.
		sqlmock.NewRows(metricPipelineCols).
			AddRow("2026-10-05", "silent_drop_detected", 1, 10).
			AddRow("2026-10-05", "completed", 2, 300).
			AddRow("2026-10-05", "success", 1, 50).
			AddRow("2026-10-03", "processing", 1, 7).
			AddRow("2026-10-05", "failed", 1, 0),
		sqlmock.NewRows(metricTimeCols).AddRow("2026-10-05", 4, 12.5, 40.25),
		sqlmock.NewRows(metricCountCols).
			AddRow("2026-10-04", "skipped", 1).
			AddRow("2026-10-04", "succeeded", 3),
		sqlmock.NewRows(metricTimeCols).AddRow("2026-10-04", 3, 2, 3.5),
		sqlmock.NewRows(metricCountCols).
			AddRow("2026-10-04", "fail", 1).
			AddRow("2026-10-04", "pass", 5))

	w, body := metricsGet(t, "viewer", "?days=3")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"2026-10-03", "2026-10-04", "2026-10-05"}; !reflect.DeepEqual(body.Days, want) {
		t.Errorf("days = %v, want %v", body.Days, want)
	}
	if body.TimeZone != "UTC" {
		t.Errorf("time_zone = %q, want UTC", body.TimeZone)
	}
	wantPipe := []MetricCount{
		{"2026-10-03", activityRunning, 1},
		{"2026-10-05", activitySucceeded, 3},
		{"2026-10-05", activityFailed, 2},
	}
	if !reflect.DeepEqual(body.PipelineRuns, wantPipe) {
		t.Errorf("pipeline_runs = %+v, want %+v", body.PipelineRuns, wantPipe)
	}
	wantRows := []MetricRows{{"2026-10-03", 7}, {"2026-10-05", 360}}
	if !reflect.DeepEqual(body.RowsWritten, wantRows) {
		t.Errorf("rows_written = %+v, want %+v", body.RowsWritten, wantRows)
	}
	if want := []MetricRunTime{{"2026-10-05", 4, 12.5, 40.25}}; !reflect.DeepEqual(body.PipelineRunTime, want) {
		t.Errorf("pipeline_run_time = %+v, want %+v", body.PipelineRunTime, want)
	}
	wantModels := []MetricCount{{"2026-10-04", activitySucceeded, 3}, {"2026-10-04", activitySkipped, 1}}
	if !reflect.DeepEqual(body.ModelRuns, wantModels) {
		t.Errorf("model_runs = %+v, want %+v", body.ModelRuns, wantModels)
	}
	if want := []MetricRunTime{{"2026-10-04", 3, 2, 3.5}}; !reflect.DeepEqual(body.ModelRunTime, want) {
		t.Errorf("model_run_time = %+v, want %+v", body.ModelRunTime, want)
	}
	wantChecks := []MetricCount{{"2026-10-04", checkPass, 5}, {"2026-10-04", checkFail, 1}}
	if !reflect.DeepEqual(body.Checks, wantChecks) {
		t.Errorf("checks = %+v, want %+v", body.Checks, wantChecks)
	}
}

// A run time measures work: a cancelled pipeline run ends when someone (or the
// stale-run repair) stopped it, and a waiting or running one has not ended.
func TestActivityMetrics_RunTimeIsSucceededAndFailedOnly(t *testing.T) {
	got := pipelineSourceStatuses(activitySucceeded, activityFailed)
	want := []string{"completed", "credential_check_failed", "error", "failed",
		"silent_drop_detected", "silent_partial_drop_detected", "success"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finished statuses = %v, want %v", got, want)
	}
	for _, s := range got {
		if s == "cancelled" || s == "waiting_for_user" {
			t.Errorf("%q is not a measured run", s)
		}
	}
}

func TestActivityMetrics_EmptySeriesAreArraysNotNull(t *testing.T) {
	pinMetricsClock(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectMetricsQueries(mock,
		sqlmock.NewRows(metricPipelineCols), sqlmock.NewRows(metricTimeCols),
		sqlmock.NewRows(metricCountCols), sqlmock.NewRows(metricTimeCols), sqlmock.NewRows(metricCountCols))
	w, _ := metricsGet(t, "viewer", "?days=3")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"pipeline_runs", "pipeline_run_time", "rows_written", "model_runs", "model_run_time", "checks"} {
		if !strings.Contains(w.Body.String(), `"`+key+`":[]`) {
			t.Errorf("%s is not [] in %s", key, w.Body.String())
		}
	}
}

func TestActivityMetrics_DefaultWindowIsSevenDays(t *testing.T) {
	from, to, days := metricsWindow(metricsNow, defaultActivityMetricsDays)
	if !from.Equal(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)) || !to.Equal(metricsTo) {
		t.Fatalf("window = [%v, %v)", from, to)
	}
	if len(days) != 7 || days[0] != "2026-09-29" || days[6] != "2026-10-05" {
		t.Fatalf("days = %v", days)
	}
	// A minute before midnight UTC, and a clock in another zone, is still that UTC day.
	ist := time.FixedZone("IST", 5*3600+1800)
	_, to2, days2 := metricsWindow(time.Date(2026, 10, 6, 5, 29, 0, 0, ist), 1)
	if !to2.Equal(metricsTo) || len(days2) != 1 || days2[0] != "2026-10-05" {
		t.Fatalf("23:59 UTC: to %v days %v, want the day 2026-10-05", to2, days2)
	}
}

func TestActivityMetrics_RejectsBadDaysBeforeTheDatabase(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	for _, q := range []string{"?days=0", "?days=91", "?days=x", "?days=-1"} {
		if w, _ := metricsGet(t, "viewer", q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestActivityMetrics_NeedsAWorkspaceRole(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	if w, _ := metricsGet(t, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("no role: status %d, want 403", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An install that has not reached migration 138 still draws its runs; any other
// read failure is a 500, never a chart with a gap.
func TestActivityMetrics_MissingTableIsEmptyOtherErrorsAreNot(t *testing.T) {
	pinMetricsClock(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM executions e`).WillReturnRows(sqlmock.NewRows(metricPipelineCols).AddRow("2026-10-05", "completed", 1, 5))
	mock.ExpectQuery(`FROM executions e`).WillReturnRows(sqlmock.NewRows(metricTimeCols))
	mock.ExpectQuery(`FROM saved_query_runs r`).WillReturnRows(sqlmock.NewRows(metricCountCols))
	mock.ExpectQuery(`FROM saved_query_runs r`).WillReturnRows(sqlmock.NewRows(metricTimeCols))
	mock.ExpectQuery(`FROM saved_query_check_results c`).
		WillReturnError(errors.New(`ERROR: relation "saved_query_check_results" does not exist (SQLSTATE 42P01)`))
	w, body := metricsGet(t, "viewer", "?days=3")
	if w.Code != http.StatusOK || len(body.PipelineRuns) != 1 || body.Checks == nil || len(body.Checks) != 0 {
		t.Fatalf("status %d body %s, want 200 with the pipeline run and no checks", w.Code, w.Body.String())
	}

	mock.ExpectQuery(`FROM executions e`).WillReturnError(errors.New("conn reset"))
	if w, _ := metricsGet(t, "viewer", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
