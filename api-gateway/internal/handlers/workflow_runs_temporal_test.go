package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"go.temporal.io/api/serviceerror"
)

// Workflow runs on Temporal (workflow_runs_temporal.go): the schedules, the
// starts, and the four steps a WorkflowRunWorkflow calls.

// wfRunUUID is a run id the internal endpoints accept (wfRunID is not a uuid).
const wfRunUUID = "ffffffff-0000-4000-8000-0000000000a1"

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

type workflowScheduleCall struct {
	op         string // "upsert" or "delete"
	workflowID string
	trigger    workflow.Trigger
}

type workflowScheduleStub struct {
	upsertErr error
	calls     []workflowScheduleCall
}

// stubWorkflowSchedules records every schedule change instead of making it;
// each upsert answers upsertErr.
func stubWorkflowSchedules(t *testing.T, upsertErr error) *workflowScheduleStub {
	t.Helper()
	s := &workflowScheduleStub{upsertErr: upsertErr}
	prevUpsert, prevDelete := upsertWorkflowSchedule, deleteWorkflowSchedule
	upsertWorkflowSchedule = func(_ context.Context, id string, tr workflow.Trigger) error {
		s.calls = append(s.calls, workflowScheduleCall{op: "upsert", workflowID: id, trigger: tr})
		return s.upsertErr
	}
	deleteWorkflowSchedule = func(_ context.Context, id string) error {
		s.calls = append(s.calls, workflowScheduleCall{op: "delete", workflowID: id})
		return nil
	}
	t.Cleanup(func() { upsertWorkflowSchedule, deleteWorkflowSchedule = prevUpsert, prevDelete })
	return s
}

func (s *workflowScheduleStub) want(t *testing.T, calls ...workflowScheduleCall) {
	t.Helper()
	if len(s.calls) == 0 && len(calls) == 0 {
		return
	}
	if !reflect.DeepEqual(s.calls, calls) {
		t.Fatalf("schedule calls = %+v, want %+v", s.calls, calls)
	}
}

// upserted is wfID's schedule set to cron, in UTC; scheduleDeleted its delete.
func upserted(cron string) workflowScheduleCall {
	return workflowScheduleCall{op: "upsert", workflowID: wfID,
		trigger: workflow.Trigger{Type: workflow.TriggerSchedule, Cron: cron, Timezone: "UTC"}}
}

var scheduleDeleted = workflowScheduleCall{op: "delete", workflowID: wfID}

// fakeWorkflowMessages holds messages in memory; putErr fails every Put.
type fakeWorkflowMessages struct {
	held    map[string]renderedMessage
	putErr  error
	deleted []string
}

func (f *fakeWorkflowMessages) Put(_ context.Context, runID string, m renderedMessage) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.held[runID] = m
	return nil
}

func (f *fakeWorkflowMessages) Get(_ context.Context, runID string) (renderedMessage, bool, error) {
	m, ok := f.held[runID]
	return m, ok, nil
}

func (f *fakeWorkflowMessages) Delete(_ context.Context, runID string) error {
	f.deleted = append(f.deleted, runID)
	delete(f.held, runID)
	return nil
}

func stubWorkflowMessages(t *testing.T) *fakeWorkflowMessages {
	t.Helper()
	f := &fakeWorkflowMessages{held: map[string]renderedMessage{}}
	prev := workflowMessages
	workflowMessages = f
	t.Cleanup(func() { workflowMessages = prev })
	return f
}

type workflowStart struct {
	temporalID string
	in         workflowRunInput
}

// stubStartWorkflowRun records each start; fail decides its answer by attempt
// (0-based), nil for every start taken.
func stubStartWorkflowRun(t *testing.T, fail func(attempt int) error) *[]workflowStart {
	t.Helper()
	var starts []workflowStart
	prev := startWorkflowRunExecution
	startWorkflowRunExecution = func(_ context.Context, temporalID string, in workflowRunInput) error {
		starts = append(starts, workflowStart{temporalID, in})
		if fail == nil {
			return nil
		}
		return fail(len(starts) - 1)
	}
	t.Cleanup(func() { startWorkflowRunExecution = prev })
	return &starts
}

// expectLoadWorkflowRun answers loadWorkflowRun for wfRunUUID.
func expectLoadWorkflowRun(mock sqlmock.Sqlmock, runStatus, definition string) {
	expectLoadWorkflowRunAs(mock, runStatus, definition, false)
}

// expectLoadWorkflowRunStopped is a run someone asked to stop.
func expectLoadWorkflowRunStopped(mock sqlmock.Sqlmock, runStatus, definition string) {
	expectLoadWorkflowRunAs(mock, runStatus, definition, true)
}

func expectLoadWorkflowRunAs(mock sqlmock.Sqlmock, runStatus, definition string, stopRequested bool) {
	mock.ExpectQuery(`FROM workflow_runs r JOIN workflows w`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "trigger", "started_by", "status", "wid", "workspace_id", "name",
			"wstatus", "definition", "run_as_user_id", "stop_requested"}).
			AddRow(wfRunUUID, "schedule", nil, runStatus, wfID, wsScopeWS, "Failed payments", "active", definition,
				wsScopeUser, stopRequested))
	mock.ExpectQuery(`SELECT id FROM connections WHERE workspace_id = \$1 AND id::text = \$2`).WithArgs(wsScopeWS, wfConn).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfConn))
}

// callInternal posts body to one of the run's internal endpoints.
func callInternal(t *testing.T, route, path string, h gin.HandlerFunc, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST(route, h)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func mockMet(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Cron: Temporal's reading of a workflow's schedule
// ---------------------------------------------------------------------------

// A cron that sets both day fields runs on either day (robfig v1, which the
// workflow was checked with); Temporal runs it only on both, so it gets one
// expression per day field. A day field robfig reads as a star (any part that
// starts with * or ?, "*/2" too) makes robfig need both, as Temporal does.
func TestTemporalCronExpressions(t *testing.T) {
	for _, tc := range []struct {
		cron string
		want []string
	}{
		{"0 9 * * 1", []string{"0 9 * * 1"}},
		{"0 9 1 * *", []string{"0 9 1 * *"}},
		{"0 9 ? * 1", []string{"0 9 ? * 1"}},
		{"0 9 */1 * 1", []string{"0 9 */1 * 1"}},
		{"0 9 */2 * 1", []string{"0 9 */2 * 1"}},
		{"0 9 *,1 * 1", []string{"0 9 *,1 * 1"}},
		{"0 9 1 * 1", []string{"0 9 1 * *", "0 9 * * 1"}},
		{"0 9 1/2 * 1", []string{"0 9 1/2 * *", "0 9 * * 1"}},
		{"30 6 1,15 1-6 1-5", []string{"30 6 1,15 1-6 *", "30 6 * 1-6 1-5"}},
	} {
		if got := temporalCronExpressions(tc.cron); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.cron, got, tc.want)
		}
	}
}

// The expressions Temporal gets run at exactly the times the workflow's own
// cron does, over a year. Each has a star day field or is passed as is, and
// either way robfig needs both day fields, as Temporal does; so the union of
// their times under robfig is what Temporal runs.
func TestTemporalCronExpressions_RunWhenTheWorkflowDoes(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	times := func(cron string) map[time.Time]bool {
		out := map[time.Time]bool{}
		tr := workflow.Trigger{Type: workflow.TriggerSchedule, Cron: cron, Timezone: "UTC"}
		for at := start; ; {
			next, err := workflow.NextRun(tr, at)
			if err != nil {
				t.Fatalf("%q: %v", cron, err)
			}
			if !next.Before(end) {
				return out
			}
			out[next] = true
			at = next
		}
	}
	for _, cron := range []string{"0 9 1 * 1", "0 9 */2 * 1", "0 9 1/2 * 1", "30 6 1,15 1-6 1-5", "0 9 13 * 5",
		"0 9 * * 1", "0 9 ? * 1"} {
		want := times(cron)
		got := map[time.Time]bool{}
		for _, expr := range temporalCronExpressions(cron) {
			for at := range times(expr) {
				got[at] = true
			}
		}
		if len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: Temporal's expressions run %d times, the workflow %d", cron, len(got), len(want))
		}
	}
}

// ---------------------------------------------------------------------------
// The schedule follows the workflow: on, edit, pause, delete, reconcile
// ---------------------------------------------------------------------------

// Turning a workflow on sets its schedule while the UPDATE holds the row and
// commits only once Temporal has it (TestActivateWorkflow_TurnsOnAfterTheCheck):
// Temporal not answering, or refusing the cron, turns nothing on.
func TestActivateWorkflow_ScheduleNotSetTurnsNothingOn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upsertErr error
		code      int
		want      string
	}{
		{name: "scheduler unreachable", upsertErr: errors.New("connection refused"), code: http.StatusServiceUnavailable,
			want: workflowSchedulerUnreachable},
		{name: "cron refused", upsertErr: serviceerror.NewInvalidArgument("invalid cron"),
			code: http.StatusUnprocessableEntity, want: "The schedule could not be read: invalid cron"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, tc.upsertErr)
			stubWorkflowQuery(t, workflow.Result{Columns: []string{"failed"},
				Rows: []map[string]any{{"failed": int64(2)}}, RowCount: 1}, nil)
			expectWorkflowLoad(mock, "draft")
			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE workflows SET status = 'active'`).
				WithArgs(wfID, wsScopeWS, sqlmock.AnyArg(), wsScopeUser).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectRollback()

			w := activateWorkflow("admin")
			if w.Code != tc.code {
				t.Fatalf("code = %d, body = %s; want %d", w.Code, w.Body.String(), tc.code)
			}
			if wfErrorOf(t, w) != tc.want {
				t.Fatalf("error = %q, want %q", wfErrorOf(t, w), tc.want)
			}
			schedules.want(t, upserted("0 9 * * 1"))
			mockMet(t, mock)
		})
	}
}

// An edit of a workflow that is on moves its schedule inside the edit's
// transaction; one Temporal does not take is not saved.
func TestUpdateWorkflow_ScheduleNotSetIsNotSaved(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	schedules := stubWorkflowSchedules(t, errors.New("connection refused"))
	stubWorkflowQuery(t, wfFailedLost, nil)
	expectWorkflowLoad(mock, "active")
	expectEditLookups(mock)
	mock.ExpectBegin()
	mock.ExpectExec(wfUpdateSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM workflow_channels WHERE workflow_id = \$1`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	w := updateWorkflow(t, "admin", wfEdit())
	if w.Code != http.StatusServiceUnavailable || wfErrorOf(t, w) != workflowSchedulerUnreachable {
		t.Fatalf("code = %d, body = %s; want 503", w.Code, w.Body.String())
	}
	schedules.want(t, upserted("0 8 * * *"))
	mockMet(t, mock)
}

// Pausing or deleting a scheduled workflow deletes its schedule while the
// row is locked; a workflow on another trigger has none to delete.
func TestPauseAndDeleteWorkflow_DropTheSchedule(t *testing.T) {
	for _, kind := range []string{workflow.TriggerSchedule, workflow.TriggerAfterPipeline} {
		var want []workflowScheduleCall
		if kind == workflow.TriggerSchedule {
			want = []workflowScheduleCall{scheduleDeleted}
		}
		t.Run("pause "+kind, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			mock.ExpectBegin()
			mock.ExpectQuery(`UPDATE workflows SET status = 'paused'`).WithArgs(wfID, wsScopeWS).
				WillReturnRows(sqlmock.NewRows([]string{"trigger_kind"}).AddRow(kind))
			mock.ExpectCommit()
			mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
			r := wsScopeRouterAsRole(http.MethodPost, "/workflows/:id/pause", PauseWorkflow, "admin")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows/"+wfID+"/pause", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			schedules.want(t, want...)
			mockMet(t, mock)
		})
		t.Run("delete "+kind, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			mock.ExpectBegin()
			mock.ExpectQuery(`DELETE FROM workflows WHERE id = \$1 AND workspace_id = \$2`).WithArgs(wfID, wsScopeWS).
				WillReturnRows(sqlmock.NewRows([]string{"name", "trigger_kind"}).AddRow("Failed payments", kind))
			mock.ExpectCommit()
			mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
			r := wsScopeRouterAsRole(http.MethodDelete, "/workflows/:id", DeleteWorkflow, "admin")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/workflows/"+wfID, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			schedules.want(t, want...)
			mockMet(t, mock)
		})
	}
}

var wfReconcileLockSQL = regexp.QuoteMeta(`SELECT definition FROM workflows WHERE id = $1 AND status = 'active' AND trigger_kind = 'schedule'`)

// The gateway's start gives every active scheduled workflow its schedule,
// each read again under its row lock: one paused or edited since the list
// was read is not overwritten. A cron Temporal refuses pauses the workflow
// with the reason; Temporal not answering is retried.
func TestReconcileWorkflowSchedules(t *testing.T) {
	const other = "ffffffff-0000-4000-8000-0000000000f2"
	listed := func(mock sqlmock.Sqlmock, ids ...string) {
		rows := sqlmock.NewRows([]string{"id"})
		for _, id := range ids {
			rows.AddRow(id)
		}
		mock.ExpectQuery(`SELECT id FROM workflows WHERE status = 'active' AND trigger_kind = 'schedule'`).WillReturnRows(rows)
	}
	t.Run("sets each, skips one paused since", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		schedules := stubWorkflowSchedules(t, nil)
		listed(mock, wfID, other)
		mock.ExpectBegin()
		mock.ExpectQuery(wfReconcileLockSQL).WithArgs(wfID).
			WillReturnRows(sqlmock.NewRows([]string{"definition"}).AddRow(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)))
		mock.ExpectCommit()
		mock.ExpectBegin()
		mock.ExpectQuery(wfReconcileLockSQL).WithArgs(other).WillReturnError(sql.ErrNoRows)
		mock.ExpectRollback()
		if done, err := reconcileWorkflowSchedules(context.Background()); !done || err != nil {
			t.Fatalf("done = %v, err = %v", done, err)
		}
		schedules.want(t, upserted("0 9 * * 1"))
		mockMet(t, mock)
	})
	t.Run("a refused cron pauses the workflow", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		stubWorkflowSchedules(t, serviceerror.NewInvalidArgument("invalid time zone"))
		listed(mock, wfID)
		mock.ExpectBegin()
		mock.ExpectQuery(wfReconcileLockSQL).WithArgs(wfID).
			WillReturnRows(sqlmock.NewRows([]string{"definition"}).AddRow(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)))
		mock.ExpectExec(`UPDATE workflows SET status = 'paused'`).
			WithArgs(wfID, "Its schedule could not be read: invalid time zone").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		if done, err := reconcileWorkflowSchedules(context.Background()); !done || err != nil {
			t.Fatalf("done = %v, err = %v", done, err)
		}
		mockMet(t, mock)
	})
	t.Run("Temporal not answering is retried", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		stubWorkflowSchedules(t, errors.New("connection refused"))
		listed(mock, wfID)
		mock.ExpectBegin()
		mock.ExpectQuery(wfReconcileLockSQL).WithArgs(wfID).
			WillReturnRows(sqlmock.NewRows([]string{"definition"}).AddRow(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)))
		mock.ExpectRollback()
		if done, err := reconcileWorkflowSchedules(context.Background()); done || err == nil {
			t.Fatalf("done = %v, err = %v; want a retry", done, err)
		}
		mockMet(t, mock)
	})
}

// ---------------------------------------------------------------------------
// Starting runs
// ---------------------------------------------------------------------------

// Run now hands one run to Temporal, named after the run. One Temporal does
// not take is recorded failed and answered 503 with its id.
func TestRunWorkflowNow_OnTemporal(t *testing.T) {
	for _, taken := range []bool{true, false} {
		t.Run(map[bool]string{true: "taken", false: "not taken"}[taken], func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			starts := stubStartWorkflowRun(t, func(int) error {
				if taken {
					return nil
				}
				return errNoTemporalClient
			})
			expectWorkflowLoad(mock, "active")
			mock.ExpectExec(`UPDATE workflow_runs SET status = 'failed'`).
				WithArgs(wfID, workflowAbandonedError, 900, workflowStepEndedError).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			var runID string
			mock.ExpectExec(`INSERT INTO workflow_runs`).
				WithArgs(captureJSONArg(&runID), wfID, wsScopeWS, wsScopeUser, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))
			if taken {
				mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
			} else {
				mock.ExpectExec(`UPDATE workflow_runs SET status = 'failed'`).
					WithArgs(sqlmock.AnyArg(), workflowNotStartedError).WillReturnResult(sqlmock.NewResult(0, 1))
			}

			r := wsScopeRouterAsRole(http.MethodPost, "/workflows/:id/run", RunWorkflowNow, "admin")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows/"+wfID+"/run", nil))
			var body struct {
				RunID string `json:"run_id"`
				Error string `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			wantCode := http.StatusAccepted
			if !taken {
				wantCode = http.StatusServiceUnavailable
			}
			if w.Code != wantCode || body.RunID == "" || body.RunID != runID {
				t.Fatalf("code = %d, body = %s, inserted run %q", w.Code, w.Body.String(), runID)
			}
			want := []workflowStart{{"workflow-run-" + runID,
				workflowRunInput{RunID: runID, WorkflowID: wfID, Trigger: workflow.TriggerManual}}}
			if !reflect.DeepEqual(*starts, want) {
				t.Fatalf("starts = %+v, want %+v", *starts, want)
			}
			if !taken && !strings.Contains(body.Error, "Try again in a minute") {
				t.Fatalf("error = %q", body.Error)
			}
			mockMet(t, mock)
		})
	}
}

// After-pipeline runs are retried inside the fan-out's budget, behind its
// breaker: a start Temporal takes on a retry is started; ones it never takes
// come back to be recorded failed and open the breaker, which then starts
// nothing until it closes.
func TestStartWorkflowRuns(t *testing.T) {
	fastFanOutRetry(t)
	runs := []pendingWorkflowRun{{RunID: wfRunUUID, WorkflowID: wfID, TemporalID: "workflow-run:" + wfID + ":exec-1"}}

	b := closedBreaker()
	starts := stubStartWorkflowRun(t, func(attempt int) error {
		if attempt < 2 {
			return errors.New("unavailable")
		}
		return nil
	})
	if lost := startWorkflowRuns(context.Background(), b, runs); len(lost) != 0 || len(*starts) != 3 {
		t.Fatalf("retried: lost %+v after %d starts", lost, len(*starts))
	}
	if (*starts)[2].in != (workflowRunInput{RunID: wfRunUUID, WorkflowID: wfID, Trigger: workflow.TriggerAfterPipeline}) ||
		(*starts)[2].temporalID != runs[0].TemporalID {
		t.Fatalf("start = %+v", (*starts)[2])
	}

	stubStartWorkflowRun(t, func(int) error { return errors.New("unavailable") })
	if lost := startWorkflowRuns(context.Background(), b, runs); !reflect.DeepEqual(lost, runs) {
		t.Fatalf("given up: lost %+v", lost)
	}
	starts = stubStartWorkflowRun(t, nil)
	if lost := startWorkflowRuns(context.Background(), b, runs); !reflect.DeepEqual(lost, runs) || len(*starts) != 0 {
		t.Fatalf("breaker open: lost %+v after %d starts", lost, len(*starts))
	}
}

// ---------------------------------------------------------------------------
// begin
// ---------------------------------------------------------------------------

func beginRun(t *testing.T, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callInternal(t, "/workflows/:id/runs/begin", "/workflows/"+wfID+"/runs/begin", BeginWorkflowRunInternal, body)
}

const wfTemporalID = "workflow-run:" + wfID + ":schedule-2026-10-05T09:00:00Z"

var wfScheduledBegin = map[string]string{"trigger": "schedule", "temporal_workflow_id": wfTemporalID}

func expectBeginLock(mock sqlmock.Sqlmock, status, kind, definition string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT workspace_id, status, trigger_kind, definition FROM workflows WHERE id = \$1 FOR UPDATE`).
		WithArgs(wfID).WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "status", "trigger_kind", "definition"}).
		AddRow(wsScopeWS, status, kind, definition))
}

func expectBeginNoRun(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id, status FROM workflow_runs WHERE temporal_workflow_id = \$1`).WithArgs(wfTemporalID).
		WillReturnError(sql.ErrNoRows)
}

// A scheduled run's first step inserts it, keyed on its Temporal workflow id,
// with the workflow locked; the time on the workflows page moves on.
func TestBeginWorkflowRun_InsertsAScheduledRun(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	schedules := stubWorkflowSchedules(t, nil)
	def := wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)
	expectBeginLock(mock, "active", "schedule", def)
	expectBeginNoRun(mock)
	mock.ExpectExec(`UPDATE workflow_runs SET status = 'failed'`).
		WithArgs(wfID, workflowAbandonedError, 900, workflowStepEndedError).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`UPDATE workflows SET next_run_at = \$2 WHERE id = \$1`).WithArgs(wfID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO workflow_runs \(workflow_id, workspace_id, trigger, definition, temporal_workflow_id\)`).
		WithArgs(wfID, wsScopeWS, def, wfTemporalID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfRunUUID))
	mock.ExpectCommit()

	w, out := beginRun(t, wfScheduledBegin)
	if w.Code != http.StatusOK || out["action"] != "run" || out["run_id"] != wfRunUUID {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	schedules.want(t)
	mockMet(t, mock)
}

// Every other answer of begin: a retry finds the run its Temporal id already
// inserted; a run of the workflow still running makes it wait; a workflow
// paused or deleted ends the run and deletes the schedule left behind; a
// schedule that can no longer be read pauses the workflow.
func TestBeginWorkflowRun_Answers(t *testing.T) {
	def := wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)
	badZone := strings.Replace(def, `"timezone":"UTC"`, `"timezone":"Mars/Olympus"`, 1)
	for _, tc := range []struct {
		name      string
		expect    func(sqlmock.Sqlmock)
		action    string
		runID     string
		schedules []workflowScheduleCall
	}{
		{name: "a retry finds its run", action: "run", runID: wfRunUUID, expect: func(m sqlmock.Sqlmock) {
			expectBeginLock(m, "active", "schedule", def)
			m.ExpectQuery(`FROM workflow_runs WHERE temporal_workflow_id`).WithArgs(wfTemporalID).
				WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(wfRunUUID, "running"))
			m.ExpectRollback()
		}},
		{name: "a retry after its run finished", action: "none", expect: func(m sqlmock.Sqlmock) {
			expectBeginLock(m, "active", "schedule", def)
			m.ExpectQuery(`FROM workflow_runs WHERE temporal_workflow_id`).WithArgs(wfTemporalID).
				WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(wfRunUUID, "succeeded"))
			m.ExpectRollback()
		}},
		{name: "another run is running", action: "busy", expect: func(m sqlmock.Sqlmock) {
			expectBeginLock(m, "active", "schedule", def)
			expectBeginNoRun(m)
			m.ExpectExec(`UPDATE workflow_runs SET status = 'failed'`).WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			m.ExpectRollback()
		}},
		{name: "paused", action: "none", schedules: []workflowScheduleCall{scheduleDeleted}, expect: func(m sqlmock.Sqlmock) {
			expectBeginLock(m, "paused", "schedule", def)
			expectBeginNoRun(m)
			m.ExpectRollback()
		}},
		{name: "no longer on a schedule", action: "none", schedules: []workflowScheduleCall{scheduleDeleted},
			expect: func(m sqlmock.Sqlmock) {
				expectBeginLock(m, "active", "after_pipeline", def)
				expectBeginNoRun(m)
				m.ExpectRollback()
			}},
		{name: "deleted", action: "none", schedules: []workflowScheduleCall{scheduleDeleted}, expect: func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery(`FROM workflows WHERE id = \$1 FOR UPDATE`).WithArgs(wfID).WillReturnError(sql.ErrNoRows)
			m.ExpectRollback()
		}},
		{name: "a schedule that cannot be read", action: "none", schedules: []workflowScheduleCall{scheduleDeleted},
			expect: func(m sqlmock.Sqlmock) {
				expectBeginLock(m, "active", "schedule", badZone)
				expectBeginNoRun(m)
				m.ExpectExec(`UPDATE workflow_runs SET status = 'failed'`).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
				m.ExpectExec(`UPDATE workflows SET status = 'paused'`).WithArgs(wfID, sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			tc.expect(mock)
			w, out := beginRun(t, wfScheduledBegin)
			if w.Code != http.StatusOK || out["action"] != tc.action {
				t.Fatalf("code = %d, body = %s; want %s", w.Code, w.Body.String(), tc.action)
			}
			if got, _ := out["run_id"].(string); got != tc.runID {
				t.Fatalf("run_id = %q, want %q", got, tc.runID)
			}
			schedules.want(t, tc.schedules...)
			mockMet(t, mock)
		})
	}
}

// A run started with its id (Run now, after a pipeline) runs while it is
// running; a body that names neither a run nor a scheduled start is refused.
func TestBeginWorkflowRun_ByRunID(t *testing.T) {
	for _, status := range []string{"running", "failed"} {
		mock, cleanup := wsScopeMockDB(t)
		mock.ExpectQuery(`SELECT status FROM workflow_runs WHERE id = \$1 AND workflow_id = \$2`).WithArgs(wfRunUUID, wfID).
			WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
		w, out := beginRun(t, map[string]string{"run_id": wfRunUUID, "trigger": "manual"})
		want := map[string]string{"running": "run", "failed": "none"}[status]
		if w.Code != http.StatusOK || out["action"] != want {
			t.Fatalf("%s: code = %d, body = %s", status, w.Code, w.Body.String())
		}
		mockMet(t, mock)
		cleanup()
	}
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	for _, body := range []map[string]string{{"trigger": "manual"}, {"trigger": "schedule"}, {"run_id": "nope"}} {
		if w, _ := beginRun(t, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%v: code = %d, want 400", body, w.Code)
		}
	}
	mockMet(t, mock)
}

// ---------------------------------------------------------------------------
// evaluate
// ---------------------------------------------------------------------------

func evaluateRun(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callInternal(t, "/workflow-runs/:id/evaluate", "/workflow-runs/"+wfRunUUID+"/evaluate",
		EvaluateWorkflowRunInternal, map[string]string{})
}

func expectNoLedger(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT channel_id FROM workflow_sends WHERE run_id = \$1 ORDER BY position`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"channel_id"}))
}

// A run whose condition holds writes its sends ledger and what it decided in
// one transaction, holds the message outside Temporal, and answers with the
// channels only: the step's answer, which Temporal keeps, carries no value
// from the rows.
func TestEvaluateWorkflowRun_PlansTheSends(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	messages := stubWorkflowMessages(t)
	stubWorkflowQuery(t, workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": int64(51)}},
		RowCount: 1}, nil)
	expectLoadWorkflowRun(mock, "running", wfDefinitionJSON(workflow.Condition{Type: "value", Op: ">", Threshold: 50}, 0))
	expectNoLedger(mock)
	expectRole(mock, "admin")
	expectSendChannel(t, mock)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status FROM workflow_runs WHERE id = \$1 FOR UPDATE`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("running"))
	expectNoLedger(mock)
	mock.ExpectExec(`INSERT INTO workflow_sends`).WithArgs(wfRunUUID, "send_1", wfChannel, 0, "#ops", "slack").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workflow_runs SET condition_met = \$2, branch = \$3, row_count = \$4, notes = \$5`).
		WithArgs(wfRunUUID, true, 0, 1, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w, out := evaluateRun(t)
	if w.Code != http.StatusOK || out["outcome"] != "send" || !reflect.DeepEqual(out["channels"], []any{wfChannel}) {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "51") {
		t.Fatalf("the answer carries a value from the rows: %s", w.Body.String())
	}
	if m, ok := messages.held[wfRunUUID]; !ok || !strings.Contains(m.Slack, "51 failed") {
		t.Fatalf("held message = %+v", messages.held)
	}
	mockMet(t, mock)
}

// The other answers of evaluate: a retry after the plan was written returns
// it without running the query again; a condition that does not hold records
// the run there; a message that cannot be held is retried (503) with nothing
// written; a run no longer running ends.
func TestEvaluateWorkflowRun_Answers(t *testing.T) {
	notMet := wfDefinitionJSON(workflow.Condition{Type: "value", Op: ">", Threshold: 100}, 0)
	met := wfDefinitionJSON(workflow.Condition{Type: "value", Op: ">", Threshold: 50}, 0)
	for _, tc := range []struct {
		name    string
		putErr  error
		expect  func(*testing.T, sqlmock.Sqlmock)
		code    int
		outcome string
		queries int
	}{
		{name: "a retry reads the plan", code: http.StatusOK, outcome: "send", expect: func(_ *testing.T, m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "running", met)
			m.ExpectQuery(`SELECT channel_id FROM workflow_sends`).WithArgs(wfRunUUID).
				WillReturnRows(sqlmock.NewRows([]string{"channel_id"}).AddRow(wfChannel))
		}},
		{name: "the condition does not hold", code: http.StatusOK, outcome: "finished", queries: 1,
			expect: func(_ *testing.T, m sqlmock.Sqlmock) {
				expectLoadWorkflowRun(m, "running", notMet)
				expectNoLedger(m)
				expectRole(m, "admin")
				m.ExpectExec(`UPDATE workflow_runs\s+SET status = \$2`).
					WithArgs(wfRunUUID, "succeeded", false, 1, "[]", sqlmock.AnyArg(), nil, nil).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}},
		{name: "the message cannot be held", putErr: errors.New("Redis is not available"),
			code: http.StatusServiceUnavailable, queries: 1, expect: func(t *testing.T, m sqlmock.Sqlmock) {
				expectLoadWorkflowRun(m, "running", met)
				expectNoLedger(m)
				expectRole(m, "admin")
				expectSendChannel(t, m)
				m.ExpectBegin()
				m.ExpectQuery(`FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("running"))
				expectNoLedger(m)
				m.ExpectRollback()
			}},
		{name: "the run is not running", code: http.StatusOK, outcome: "finished", expect: func(_ *testing.T, m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "failed", met)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			messages := stubWorkflowMessages(t)
			messages.putErr = tc.putErr
			calls := stubWorkflowQuery(t, workflow.Result{Columns: []string{"failed"},
				Rows: []map[string]any{{"failed": int64(51)}}, RowCount: 1}, nil)
			tc.expect(t, mock)
			w, out := evaluateRun(t)
			if w.Code != tc.code || (tc.outcome != "" && out["outcome"] != tc.outcome) || *calls != tc.queries {
				t.Fatalf("code = %d, body = %s, query ran %d times", w.Code, w.Body.String(), *calls)
			}
			mockMet(t, mock)
		})
	}
}

// ---------------------------------------------------------------------------
// send
// ---------------------------------------------------------------------------

func sendRun(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callInternal(t, "/workflow-runs/:id/send", "/workflow-runs/"+wfRunUUID+"/send", SendWorkflowRunInternal,
		map[string]string{"channel_id": wfChannel})
}

// expectSendRow is the ledger row of a legacy run's send (no step named): send_1's.
func expectSendRow(mock sqlmock.Sqlmock, status string, startedAt any) {
	mock.ExpectQuery(`SELECT step_id, status, started_at FROM workflow_sends\s+WHERE run_id = \$1 AND channel_id = \$2`).
		WithArgs(wfRunUUID, wfChannel, "").
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "status", "started_at"}).AddRow("send_1", status, startedAt))
}

// expectSettle is a send recorded without a post, then read back.
func expectSettle(mock sqlmock.Sqlmock, from, to, why string) {
	mock.ExpectExec(`UPDATE workflow_sends SET status = \$5, error = \$6`).
		WithArgs(wfRunUUID, "send_1", wfChannel, from, to, why).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT status FROM workflow_sends WHERE run_id = \$1 AND step_id = \$2 AND channel_id = \$3`).
		WithArgs(wfRunUUID, "send_1", wfChannel).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(to))
}

// A send claims its ledger row before it posts and records the answer after,
// so the post happens once.
func TestSendWorkflowRun_SendsOnce(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	messages := stubWorkflowMessages(t)
	messages.held[wfRunUUID] = renderedMessage{Slack: "*Failed payments* 51 failed"}
	sent := stubSlack(t, nil)
	def := wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)
	expectLoadWorkflowRun(mock, "running", def)
	expectSendRow(mock, "pending", nil)
	expectRole(mock, "admin")
	mock.ExpectQuery(`FROM delivery_channels WHERE id = \$1 AND workspace_id = \$2`).WithArgs(wfChannel, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind", "slack_webhook_encrypted", "email_recipients"}).
			AddRow(wfChannel, "#ops", "slack", mustEncrypt(t, "https://hooks.slack.com/services/T0/B0/x"), ""))
	mock.ExpectExec(`UPDATE workflow_sends s SET status = 'sending'`).WithArgs(wfRunUUID, "send_1", wfChannel).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workflow_sends SET status = \$4, sent = \$5, error = \$6`).
		WithArgs(wfRunUUID, "send_1", wfChannel, "sent", 0, nullArg{null: true}).WillReturnResult(sqlmock.NewResult(0, 1))

	w, out := sendRun(t)
	if w.Code != http.StatusOK || out["status"] != "sent" || len(*sent) != 1 ||
		(*sent)[0] != "https://hooks.slack.com/services/T0/B0/x|*Failed payments* 51 failed" {
		t.Fatalf("code = %d, body = %s, sent = %v", w.Code, w.Body.String(), *sent)
	}
	mockMet(t, mock)

	// The retry Temporal makes after the answer was lost finds it sent.
	expectLoadWorkflowRun(mock, "running", def)
	expectSendRow(mock, "sent", time.Now())
	if w, out := sendRun(t); w.Code != http.StatusOK || out["status"] != "sent" || len(*sent) != 1 {
		t.Fatalf("retry: code = %d, body = %s, sent %d times", w.Code, w.Body.String(), len(*sent))
	}
	mockMet(t, mock)
}

// The other answers of send, none of which posts: one in progress elsewhere
// is asked again (409); one cut off mid-post is failed, as it may not have
// arrived; a run that stopped skips it; a message no longer held, a channel
// deleted, or a run-as user who lost the role fails it, and the last pauses
// the workflow and deletes its schedule.
func TestSendWorkflowRun_Answers(t *testing.T) {
	def := wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)
	for _, tc := range []struct {
		name      string
		held      bool
		expect    func(sqlmock.Sqlmock)
		code      int
		status    string
		schedules []workflowScheduleCall
	}{
		{name: "in progress", code: http.StatusConflict, expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "running", def)
			expectSendRow(m, "sending", time.Now().Add(-time.Minute))
		}},
		{name: "cut off", code: http.StatusOK, status: "failed", expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "running", def)
			expectSendRow(m, "sending", time.Now().Add(-3*time.Minute))
			expectSettle(m, "sending", "failed", workflowSendCutOffError)
		}},
		{name: "the run stopped", held: true, code: http.StatusOK, status: "skipped", expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "failed", def)
			expectSendRow(m, "pending", nil)
			expectSettle(m, "pending", "skipped", workflowSendSkipError)
		}},
		{name: "someone stopped the run", held: true, code: http.StatusOK, status: "skipped", expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRunStopped(m, "running", def)
			expectSendRow(m, "pending", nil)
			expectSettle(m, "pending", "skipped", workflowSendSkipError)
		}},
		{name: "the message is gone", code: http.StatusOK, status: "failed", expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "running", def)
			expectSendRow(m, "pending", nil)
			expectRole(m, "admin")
			expectSettle(m, "pending", "failed", workflowMessageGone)
		}},
		{name: "the channel is gone", held: true, code: http.StatusOK, status: "failed", expect: func(m sqlmock.Sqlmock) {
			expectLoadWorkflowRun(m, "running", def)
			expectSendRow(m, "pending", nil)
			expectRole(m, "admin")
			m.ExpectQuery(`FROM delivery_channels WHERE id = \$1`).WillReturnError(sql.ErrNoRows)
			expectSettle(m, "pending", "failed", workflowChannelGone)
		}},
		{name: "the run-as user lost the role", held: true, code: http.StatusOK, status: "failed",
			schedules: []workflowScheduleCall{scheduleDeleted}, expect: func(m sqlmock.Sqlmock) {
				expectLoadWorkflowRun(m, "running", def)
				expectSendRow(m, "pending", nil)
				expectRole(m, "member")
				refusal := workflowRoleRefusal("member")
				m.ExpectExec(`UPDATE workflow_sends SET status = \$5`).
					WithArgs(wfRunUUID, "send_1", wfChannel, "pending", "failed", refusal).WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectBegin()
				m.ExpectExec(`UPDATE workflows SET status = 'paused'`).WithArgs(wfID, refusal).
					WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
				m.ExpectQuery(`SELECT status FROM workflow_sends`).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("failed"))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			messages := stubWorkflowMessages(t)
			if tc.held {
				messages.held[wfRunUUID] = renderedMessage{Slack: "x"}
			}
			sent := stubSlack(t, nil)
			tc.expect(mock)
			w, out := sendRun(t)
			if w.Code != tc.code || (tc.status != "" && out["status"] != tc.status) || len(*sent) != 0 {
				t.Fatalf("code = %d, body = %s, posted %d times", w.Code, w.Body.String(), len(*sent))
			}
			schedules.want(t, tc.schedules...)
			mockMet(t, mock)
		})
	}
}

// ---------------------------------------------------------------------------
// finish
// ---------------------------------------------------------------------------

func finishRun(t *testing.T, abort string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callInternal(t, "/workflow-runs/:id/finish", "/workflow-runs/"+wfRunUUID+"/finish", FinishWorkflowRunInternal,
		map[string]string{"abort": abort})
}

// Finish records the run from its ledger, once: sends not made are skipped,
// one cut off mid-post failed, and any failed channel fails the run. The
// message is deleted and the sends audited.
func TestFinishWorkflowRun_CountsFailedChannels(t *testing.T) {
	const email = "cccccccc-0000-4000-8000-0000000000d2"
	for _, tc := range []struct {
		abort, status, err string
		stopped            bool
	}{
		{"", "failed", "1 of 2 delivery channels failed.", false},
		{"deadline", "failed", workflowDeadlineError, false},
		{"step_failed", "failed", workflowStepFailedError, false},
		// Someone stopped it: stopped, whatever else went wrong.
		{"", "stopped", "", true},
	} {
		t.Run(fmt.Sprintf("abort=%s stopped=%v", tc.abort, tc.stopped), func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			messages := stubWorkflowMessages(t)
			messages.held[wfRunUUID] = renderedMessage{Slack: "x"}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT status, row_count, stop_requested_at IS NOT NULL FROM workflow_runs WHERE id = \$1 FOR UPDATE`).
				WithArgs(wfRunUUID).WillReturnRows(sqlmock.NewRows([]string{"status", "row_count", "stopped"}).
				AddRow("running", 3, tc.stopped))
			mock.ExpectExec(`UPDATE workflow_sends SET status = 'skipped'`).WithArgs(wfRunUUID, workflowSendSkipError).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`UPDATE workflow_sends SET status = 'failed'`).WithArgs(wfRunUUID, workflowSendCutOffError).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`FROM workflow_sends WHERE run_id = \$1 ORDER BY position`).WithArgs(wfRunUUID).
				WillReturnRows(sqlmock.NewRows([]string{"step_id", "channel_id", "name", "kind", "status", "sent", "error"}).
					AddRow("send_1", wfChannel, "#ops", "slack", "sent", 0, "").
					AddRow("send_1", email, "On-call", "email", "failed", 0, "SMTP refused"))
			var deliveries string
			var errArg any = tc.err
			if tc.err == "" {
				errArg = nullArg{null: true}
			}
			mock.ExpectExec(`UPDATE workflow_runs SET status = \$2, deliveries = \$3, error = \$4`).
				WithArgs(wfRunUUID, tc.status, captureJSONArg(&deliveries), errArg).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			expectLoadWorkflowRun(mock, "failed", wfDefinitionJSON(workflow.Condition{Type: "always"}, 0))
			var audit string
			mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(wsScopeUser, wfID, captureJSONArg(&audit), wsScopeWS).
				WillReturnResult(sqlmock.NewResult(0, 1))

			w, out := finishRun(t, tc.abort)
			if w.Code != http.StatusOK || out["status"] != tc.status {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			var ds []DeliveryOutcome
			if err := json.Unmarshal([]byte(deliveries), &ds); err != nil || len(ds) != 2 || ds[1].Error != "SMTP refused" {
				t.Fatalf("deliveries = %s (%v)", deliveries, err)
			}
			if !strings.Contains(audit, `"row_count":3`) || !strings.Contains(audit, `"status":"failed"`) {
				t.Fatalf("audit = %s", audit)
			}
			if !reflect.DeepEqual(messages.deleted, []string{wfRunUUID}) {
				t.Fatalf("deleted = %v", messages.deleted)
			}
			mockMet(t, mock)
		})
	}
}

// A run already recorded is not recorded again, and its message is left to
// the attempt that recorded it; an abort it does not know is refused.
func TestFinishWorkflowRun_Once(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	messages := stubWorkflowMessages(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, row_count, stop_requested_at IS NOT NULL FROM workflow_runs`).WithArgs(wfRunUUID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "row_count", "stopped"}).AddRow("succeeded", 3, false))
	mock.ExpectRollback()
	if w, out := finishRun(t, ""); w.Code != http.StatusOK || out["status"] != "succeeded" || len(messages.deleted) != 0 {
		t.Fatalf("code = %d, body = %s, deleted %v", w.Code, w.Body.String(), messages.deleted)
	}
	if w, _ := finishRun(t, "cancel"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown abort: code = %d", w.Code)
	}
	mockMet(t, mock)
}
