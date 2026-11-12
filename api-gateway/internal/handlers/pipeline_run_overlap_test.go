package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// What the open-runs query selects (CDC stream row excluded, parked and recent
// flags) is proven against real PostgreSQL in pipeline_run_overlap_pg_test.go.
// These tests prove what the gateway does with its answer.

const (
	overlapPipeID = "22222222-0000-4000-8000-000000000001"
	overlapWSID   = "22222222-0000-4000-8000-000000000002"
	overlapOldRun = "22222222-0000-4000-8000-0000000000a1"
	overlapNewRun = "22222222-0000-4000-8000-0000000000b1"
)

var openRunColumns = []string{"id", "parked", "recent"}

// stubRunWorkflows stands in for Temporal: describe answers from the map (a
// missing id is "still running"), and every cancel is recorded.
func stubRunWorkflows(t *testing.T, ended map[string]string) (described, cancelled *[]string) {
	t.Helper()
	var d, cl []string
	prevDescribe, prevCancel := describeRunWorkflow, cancelReplacedRunWorkflow
	describeRunWorkflow = func(_ context.Context, execID string) (bool, string) {
		d = append(d, execID)
		how, ok := ended[execID]
		return ok, how
	}
	cancelReplacedRunWorkflow = func(_ context.Context, _, execID string) {
		cl = append(cl, execID)
	}
	t.Cleanup(func() { describeRunWorkflow, cancelReplacedRunWorkflow = prevDescribe, prevCancel })
	return &d, &cl
}

func expectOpenRuns(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(openRunsSQL)).
		WithArgs(overlapPipeID, 120).
		WillReturnRows(rows)
}

func TestClassifyOpenRuns(t *testing.T) {
	cases := []struct {
		name      string
		rows      [][3]interface{} // id, parked, recent
		ended     map[string]string
		blocking  string
		parked    []string
		stale     []staleRun
		described []string
	}{
		{
			name: "no open run",
		},
		{
			name:      "a run still copying blocks",
			rows:      [][3]interface{}{{overlapOldRun, false, false}},
			blocking:  overlapOldRun,
			described: []string{overlapOldRun},
		},
		{
			name: "a parked run is replaced without asking Temporal",
			rows: [][3]interface{}{{overlapOldRun, true, false}},
			// Its workflow is running (waiting on a signal) — that must not block.
			parked: []string{overlapOldRun},
		},
		{
			name:      "a run whose workflow completed is stale",
			rows:      [][3]interface{}{{overlapOldRun, false, false}},
			ended:     map[string]string{overlapOldRun: "completed"},
			stale:     []staleRun{{ID: overlapOldRun, HowEnded: "completed"}},
			described: []string{overlapOldRun},
		},
		{
			name:      "an old run with no workflow is stale",
			rows:      [][3]interface{}{{overlapOldRun, false, false}},
			ended:     map[string]string{overlapOldRun: workflowNotFound},
			stale:     []staleRun{{ID: overlapOldRun, HowEnded: workflowNotFound}},
			described: []string{overlapOldRun},
		},
		{
			// The scheduled wrapper writes the execution two activities before it
			// starts the workflow; closing it there would race a run that is starting.
			name:      "a just-started run with no workflow yet blocks",
			rows:      [][3]interface{}{{overlapOldRun, false, true}},
			ended:     map[string]string{overlapOldRun: workflowNotFound},
			blocking:  overlapOldRun,
			described: []string{overlapOldRun},
		},
		{
			name:      "a just-started run whose workflow already ended is stale",
			rows:      [][3]interface{}{{overlapOldRun, false, true}},
			ended:     map[string]string{overlapOldRun: "failed"},
			stale:     []staleRun{{ID: overlapOldRun, HowEnded: "failed"}},
			described: []string{overlapOldRun},
		},
		{
			name: "parked and stale together, nothing live",
			rows: [][3]interface{}{
				{overlapNewRun, true, false},
				{overlapOldRun, false, false},
			},
			ended:     map[string]string{overlapOldRun: "terminated"},
			parked:    []string{overlapNewRun},
			stale:     []staleRun{{ID: overlapOldRun, HowEnded: "terminated"}},
			described: []string{overlapOldRun},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer sqlDB.Close()
			described, cancelled := stubRunWorkflows(t, tc.ended)

			rows := sqlmock.NewRows(openRunColumns)
			for _, r := range tc.rows {
				rows.AddRow(r[0], r[1], r[2])
			}
			expectOpenRuns(mock, rows)

			plan, err := classifyOpenRuns(context.Background(), sqlDB, overlapPipeID)
			if err != nil {
				t.Fatalf("classifyOpenRuns: %v", err)
			}
			if plan.Blocking != tc.blocking {
				t.Errorf("Blocking = %q, want %q", plan.Blocking, tc.blocking)
			}
			if strings.Join(plan.Parked, ",") != strings.Join(tc.parked, ",") {
				t.Errorf("Parked = %v, want %v", plan.Parked, tc.parked)
			}
			if len(plan.Stale) != len(tc.stale) {
				t.Fatalf("Stale = %v, want %v", plan.Stale, tc.stale)
			}
			for i := range tc.stale {
				if plan.Stale[i] != tc.stale[i] {
					t.Errorf("Stale[%d] = %v, want %v", i, plan.Stale[i], tc.stale[i])
				}
			}
			if strings.Join(*described, ",") != strings.Join(tc.described, ",") {
				t.Errorf("described %v, want %v", *described, tc.described)
			}
			if len(*cancelled) != 0 {
				t.Errorf("classifying cancelled %v; only applyRunOverlap may", *cancelled)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

func overlapContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+overlapPipeID+"/run", nil)
	return c, rr
}

func TestRefuseOverlappingRun_CannotReadRunsIs503(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	stubRunWorkflows(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta(openRunsSQL)).
		WithArgs(overlapPipeID, 120).
		WillReturnError(errors.New("connection reset"))

	c, rr := overlapContext()
	if _, ok := refuseOverlappingRun(c, sqlDB, overlapPipeID); ok {
		t.Fatal("refuseOverlappingRun allowed a run it could not check")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if got := decodeBody(t, rr)["error"]; got != "run_check_failed" {
		t.Fatalf("error = %v, want run_check_failed", got)
	}
}

// The run button, a healer retry and the WAL watchdog all enqueue through here.
// A run that is copying must refuse the new one before anything is written:
// sqlmock fails any statement it was not told to expect, so the pipeline row,
// the execution row and pipeline_progress are provably untouched.
func TestEnqueuePipelineRun_RefusesWhileARunIsCopying(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	_, cancelled := stubRunWorkflows(t, nil)

	mock.ExpectQuery(`SELECT name, status, source_connection_id`).
		WithArgs(overlapPipeID, overlapWSID).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "status", "source_connection_id", "destination_connection_id",
			"natural_language_request", "created_by", "config", "dataset", "default_run_mode",
		}).AddRow("orders", "running", nil, nil, nil, nil, []byte(`{}`), nil, nil))
	expectOpenRuns(mock, sqlmock.NewRows(openRunColumns).AddRow(overlapOldRun, false, false))

	c, rr := overlapContext()
	enqueuePipelineRun(c, sqlDB, overlapPipeID, overlapWSID, "", RunPipelineRequest{})

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "run_in_progress" || body["execution_id"] != overlapOldRun {
		t.Fatalf("body = %v, want run_in_progress naming %s", body, overlapOldRun)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "already has a run in progress") {
		t.Fatalf("message = %q, want it to say a run is in progress", msg)
	}
	if len(*cancelled) != 0 {
		t.Fatalf("a refused run cancelled %v", *cancelled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestApplyRunOverlap_ClosesStaleAndReplacesParked(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	_, cancelled := stubRunWorkflows(t, nil)

	const staleRunID = "22222222-0000-4000-8000-0000000000c1"
	mock.ExpectExec(`UPDATE executions\s+SET status = 'failed'`).
		WithArgs(staleRunID, "This run stopped without recording a result (workflow not found). Closed when run "+overlapNewRun+" started.").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE executions\s+SET status = 'cancelled'`).
		WithArgs(overlapOldRun, "Replaced by run "+overlapNewRun+" while waiting for input.").
		WillReturnResult(sqlmock.NewResult(0, 1))

	applyRunOverlap(context.Background(), sqlDB, overlapPipeID, overlapNewRun, runOverlap{
		Parked: []string{overlapOldRun},
		Stale:  []staleRun{{ID: staleRunID, HowEnded: workflowNotFound}},
	})

	// Only the parked run's own workflow is told to stop — never the new run, and
	// never the stale one, whose workflow has already ended.
	if strings.Join(*cancelled, ",") != overlapOldRun {
		t.Fatalf("cancelled %v, want only %s", *cancelled, overlapOldRun)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
