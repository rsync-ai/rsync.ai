package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const historyPipeline = "44444444-4444-4444-4444-444444444444"

var historyRowCols = []string{"id", "pipeline_id", "pipeline_name", "status", "live_stream",
	"trigger_source", "scheduled_time", "start_time", "end_time", "error_message"}

// Every status the executions endpoints can report falls in exactly one
// outcome, and the outcomes agree with RunErrorClass: a run has an error class
// exactly when its outcome is failed or cancelled.
func TestRunOutcome(t *testing.T) {
	for status, want := range map[string]string{
		"completed": "succeeded", "success": "succeeded",
		"failed": "failed", "error": "failed", "credential_check_failed": "failed",
		"silent_drop_detected": "failed", "silent_partial_drop_detected": "failed",
		"cancelled": "cancelled", "canceled": "cancelled", "stopped": "cancelled",
		"running": "running", "processing": "running",
		"pending": "pending", "queued": "pending",
		"waiting_for_user": "waiting_for_user",
		"initializing":     "other", "": "other", " Completed ": "succeeded",
	} {
		if got := RunOutcome(status); got != want {
			t.Errorf("RunOutcome(%q) = %q, want %q", status, got, want)
		}
	}
	for status := range runFailedStatuses {
		o := RunOutcome(status)
		if o != "failed" && o != "cancelled" {
			t.Errorf("%q has an error class but outcome %q", status, o)
		}
	}
	for outcome, statuses := range runOutcomeStatuses {
		for _, s := range statuses {
			hasClass := RunErrorClass(s, "") != ""
			if hasClass != (outcome == "failed" || outcome == "cancelled") {
				t.Errorf("status %q (outcome %q): error class present = %v", s, outcome, hasClass)
			}
		}
	}
	// The Admin → Executions filter's spellings land in the same outcomes.
	for filter, outcome := range map[string]string{"success": "succeeded", "failed": "failed", "cancelled": "cancelled"} {
		for _, s := range adminExecutionStatusValues(filter) {
			if got := RunOutcome(s); got != outcome {
				t.Errorf("admin filter %q lists %q, whose outcome is %q", filter, s, got)
			}
		}
	}
}

func historyCounts(mock sqlmock.Sqlmock, args ...driver.Value) {
	mock.ExpectQuery(`SELECT .* COUNT\(\*\)\s+FROM executions e.*WHERE p\.workspace_id = \$1.*GROUP BY`).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("completed", 70).AddRow("success", 6).AddRow("failed", 1).
			AddRow("cancelled", 4).AddRow("running", 4).AddRow("initializing", 2))
}

// The run history is a read of the active workspace's runs: $1 is the
// workspace, never the user. It counts every run by outcome (the count a
// "how many runs failed?" answer needs), lists the newest, and returns an
// error class, never the error text. sqlmock refuses any statement not
// expected, so a write (ListExecutions repairs stale rows) fails here.
func TestListRunHistory_WorkspaceScopedCountsAndRows(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	historyCounts(mock, wsScopeWS)
	start := time.Date(2026, 9, 26, 19, 40, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM executions e.*WHERE p\.workspace_id = \$1\s+AND e\.id <> e\.pipeline_id.*ORDER BY e\.start_time DESC.*LIMIT \$2`).
		WithArgs(wsScopeWS, 25).
		WillReturnRows(sqlmock.NewRows(historyRowCols).
			AddRow("572618d3-0000-4000-8000-000000000001", historyPipeline, "Events Archive Export", "failed", false,
				"manual", nil, start, start.Add(2*time.Minute),
				"activity error (type: ExecutorActivityV2): destination partially dropped rows: tried to convert to int64 LEAKCANARY").
			AddRow("572618d3-0000-4000-8000-000000000002", historyPipeline, "Events Archive Export", "completed", false,
				"schedule", start, start, start.Add(time.Minute), nil))

	r := wsScopeRouter(http.MethodGet, "/runs/history", ListRunHistory)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/history", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "LEAKCANARY") || strings.Contains(w.Body.String(), "error_message") {
		t.Fatalf("the error text reached the answer: %s", w.Body.String())
	}
	var body struct {
		Runs []struct {
			ID            string     `json:"id"`
			Status        string     `json:"status"`
			Outcome       string     `json:"outcome"`
			ErrorClass    string     `json:"error_class"`
			PipelineName  string     `json:"pipeline_name"`
			TriggerSource string     `json:"trigger_source"`
			EndTime       *time.Time `json:"end_time"`
		} `json:"runs"`
		Total     int            `json:"total"`
		ByOutcome map[string]int `json:"by_outcome"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 87 {
		t.Errorf("total = %d, want 87", body.Total)
	}
	want := map[string]int{"succeeded": 76, "failed": 1, "cancelled": 4, "running": 4, "pending": 0, "waiting_for_user": 0, "other": 2}
	for k, v := range want {
		if body.ByOutcome[k] != v {
			t.Errorf("by_outcome[%s] = %d, want %d (%v)", k, body.ByOutcome[k], v, body.ByOutcome)
		}
	}
	if len(body.Runs) != 2 {
		t.Fatalf("runs = %+v", body.Runs)
	}
	if r0 := body.Runs[0]; r0.Outcome != "failed" || r0.ErrorClass != "schema" || r0.EndTime == nil {
		t.Errorf("first run = %+v", r0)
	}
	if r1 := body.Runs[1]; r1.Outcome != "succeeded" || r1.ErrorClass != "" {
		t.Errorf("second run = %+v", r1)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An outcome filter matches every status spelling of that outcome, and the
// total is the filtered count; by_outcome still counts every run.
func TestListRunHistory_OutcomeAndPipelineFilter(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM pipelines WHERE id = \$1 AND workspace_id = \$2\)`).
		WithArgs(historyPipeline, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	historyCounts(mock, wsScopeWS, historyPipeline)
	mock.ExpectQuery(`AND e\.pipeline_id = \$2\s+AND \(CASE.*\) IN \(\$3, \$4\).*LIMIT \$5`).
		WithArgs(wsScopeWS, historyPipeline, "completed", "success", 5).
		WillReturnRows(sqlmock.NewRows(historyRowCols))

	r := wsScopeRouter(http.MethodGet, "/runs/history", ListRunHistory)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/history?pipeline_id="+historyPipeline+"&outcome=succeeded&limit=5", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Runs      []any          `json:"runs"`
		Total     int            `json:"total"`
		ByOutcome map[string]int `json:"by_outcome"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 76 || body.ByOutcome["failed"] != 1 || body.Runs == nil {
		t.Errorf("body = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A pipeline outside the active workspace is a 404, as on every pipeline
// read, and no run of it is read.
func TestListRunHistory_ForeignPipelineIs404(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM pipelines WHERE id = \$1 AND workspace_id = \$2\)`).
		WithArgs(historyPipeline, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	r := wsScopeRouter(http.MethodGet, "/runs/history", ListRunHistory)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/history?pipeline_id="+historyPipeline, nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "pipeline_not_found") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListRunHistory_BadArguments(t *testing.T) {
	for name, query := range map[string]string{
		"outcome":     "?outcome=successful",
		"pipeline id": "?pipeline_id=1%20OR%201=1",
		"limit":       "?limit=0",
		"big limit":   "?limit=101",
	} {
		t.Run(name, func(t *testing.T) {
			_, cleanup := wsScopeMockDB(t)
			defer cleanup()
			r := wsScopeRouter(http.MethodGet, "/runs/history", ListRunHistory)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/history"+query, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
