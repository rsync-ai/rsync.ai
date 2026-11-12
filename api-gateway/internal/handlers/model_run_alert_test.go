package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// A model rebuild nobody started by hand (a schedule's tick, an upstream finishing)
// that fails used to leave only a red badge on a page nobody had open. These tests
// pin who is now told (the model's owner, through saved_query_id), what the notice
// carries (ids and a code, never the engine's error), and the runs it stays quiet
// about: one started by hand, one a workflow step ran (the workflow's own failure
// notice covers it), and one a data check held back (CHECK_BLOCKED covers it).
//
// Harness helpers: wsScopeMockDB (workspace scoping tests), provHistoryInsert and
// friends (model_run_provenance_test.go), rf* (saved_query_run_failure_test.go),
// captureRunNotices (run_notices_test.go).

// modelRunSecretError is an engine error with everything a notice must never carry.
const modelRunSecretError = `pq: password authentication failed for user "etl" at 10.20.30.40: password=hunter2`

func expectModelRunRecorded(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()
}

// assertNoErrorText fails if any part of the raw notice carries the engine's error.
func assertNoErrorText(t *testing.T, n map[string]interface{}) {
	t.Helper()
	raw := sortedKeys(n) + " " + strings.Join(flattenStrings(n), " ")
	for _, leak := range []string{"hunter2", "10.20.30.40", "etl", "password"} {
		if strings.Contains(raw, leak) {
			t.Errorf("notice carries %q from the engine's error: %v", leak, n)
		}
	}
}

func flattenStrings(v interface{}) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]interface{}:
		var out []string
		for k, e := range x {
			out = append(out, k)
			out = append(out, flattenStrings(e)...)
		}
		return out
	}
	return nil
}

func TestModelRunFailedAlert_AScheduledFailureAlertsOnceAndCarriesNoErrorText(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)
	rec := captureRunNotices(t)
	expectModelRunRecorded(mock)

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "failed", StatementClass: "read", TargetTable: "analytics.orders", Error: modelRunSecretError},
		modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule, ActorID: provActor})

	sent := rec.notices(t)
	if len(sent) != 1 {
		t.Fatalf("model failure notices sent = %d, want 1: a scheduled rebuild that failed at 3am "+
			"would otherwise wait for someone to open the model's page", len(sent))
	}
	env := noticeError(t, sent[0])
	if env["code"] != "MODEL_RUN_FAILED" || env["severity"] != "error" {
		t.Errorf("code = %v, severity = %v; want MODEL_RUN_FAILED, error", env["code"], env["severity"])
	}
	if sent[0]["saved_query_id"] != savedQueryID {
		t.Errorf("saved_query_id = %v, want %s: the model's owner is who is told", sent[0]["saved_query_id"], savedQueryID)
	}
	if want := "/models/" + savedQueryID; sent[0]["action_url"] != want {
		t.Errorf("action_url = %v, want %s (the Runs tab, which has the error)", sent[0]["action_url"], want)
	}
	if env["dedup_subject"] != provRunB {
		t.Errorf("dedup_subject = %v, want the run's row %s: one notice per run", env["dedup_subject"], provRunB)
	}
	if sent[0]["message"] != modelRunFailedMessage {
		t.Errorf("message = %q, want the fixed sentence", sent[0]["message"])
	}
	assertNoErrorText(t, sent[0])
	assertMetadataOnly(t, sent[0], "saved_query_id")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// Every door a rebuild comes through without a person behind it alerts: the clock,
// and an upstream pipeline or model finishing.
func TestModelRunFailedAlert_EveryUnattendedDoorAlerts(t *testing.T) {
	cases := []struct {
		name  string
		audit modelRunAudit
	}{
		{"a schedule's tick", modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule}},
		{"an upstream pipeline finished", modelRunAudit{Trigger: triggerTriggered, ScheduleID: provSchedule,
			Provenance: runProvenance{UpstreamKind: upstreamKindPipeline, UpstreamID: provUpstream, Depth: 1}}},
		{"an upstream model finished", modelRunAudit{Trigger: triggerTriggered, ScheduleID: provSchedule,
			Provenance: runProvenance{UpstreamKind: upstreamKindModel, UpstreamID: provUpstream, UpstreamRunID: provRunA, Depth: 2}}},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureRunNotices(t)
			// No database: the record is lost, and the notice still goes.
			recordModelRunOutcome(context.Background(), nil, savedQueryID,
				&modelRunResult{Status: "failed", Error: "boom"}, tc.audit)
			if sent := rec.notices(t); len(sent) != 1 {
				t.Fatalf("model failure notices sent = %d, want 1", len(sent))
			}
		})
	}
}

// A run inside a workflow is the workflow's to report: its failure notice names the
// step. A second notice from the model would tell the same owner the same thing twice.
func TestModelRunFailedAlert_AWorkflowStepRunIsNotAlerted(t *testing.T) {
	rec := captureRunNotices(t)
	recordModelRunOutcome(context.Background(), nil, savedQueryID,
		&modelRunResult{Status: "failed", Error: "boom"},
		modelRunAudit{Trigger: triggerTriggered,
			Provenance: runProvenance{UpstreamKind: upstreamKindWorkflow, UpstreamID: provUpstream, OriginExecutionID: "wf-run-1", Depth: 1}})
	if sent := rec.notices(t); len(sent) != 0 {
		t.Fatalf("model failure notices sent = %d for a workflow step's run, want 0", len(sent))
	}
}

// Each control is one way a run is not an unattended rebuild that failed.
func TestModelRunFailedAlert_OnlyAnUnattendedFailureIsAlerted(t *testing.T) {
	scheduled := modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule}
	cases := []struct {
		name  string
		res   *modelRunResult
		audit modelRunAudit
	}{
		{"started by hand: whoever pressed Run is looking at it", &modelRunResult{Status: "failed", Error: "boom"},
			modelRunAudit{Trigger: triggerManual, ActorID: provActor}},
		{"an unknown door is a wiring bug, not an unattended run", &modelRunResult{Status: "failed", Error: "boom"},
			modelRunAudit{Trigger: "replayed"}},
		{"a data check held it back: CHECK_BLOCKED says so", &modelRunResult{Status: "failed", Error: modelCheckBlockedError, CheckBlocked: true},
			scheduled},
		{"it succeeded", &modelRunResult{Status: "succeeded"}, scheduled},
		{"it was skipped on purpose", &modelRunResult{Status: "skipped", Error: "waiting"}, scheduled},
		{"no result", nil, scheduled},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureRunNotices(t)
			recordModelRunOutcome(context.Background(), nil, savedQueryID, tc.res, tc.audit)
			if sent := rec.notices(t); len(sent) != 0 {
				t.Fatalf("model failure notices sent = %d, want 0", len(sent))
			}
		})
	}
}

// A failure the gateway never answered is recorded by the workflow after the fact
// (RecordSavedQueryModelRunFailureInternal). It alerts once, keyed by the attempt's
// started_at; the at-least-once retry of the same report finds the row and is quiet.
func TestRecordModelRunFailure_ARecordedFailureAlertsAndADuplicateDoesNot(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_SECRET", rfSecret)
	body := func() string {
		return rfBody(t, map[string]any{"schedule_id": rfScheduleID, "error": modelRunSecretError, "started_at": rfStartedWire})
	}

	mock := rfMock(t)
	rec := captureRunNotices(t)
	rfExpectUpToDedupe(mock, false)
	rfExpectExists(mock, false)
	mock.ExpectExec(rfStampSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(rfInsertSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if w := rfServe(t, rfPath, body(), rfSecret); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	sent := rec.notices(t)
	if len(sent) != 1 {
		t.Fatalf("model failure notices sent = %d for a recorded failure, want 1", len(sent))
	}
	env := noticeError(t, sent[0])
	if env["code"] != "MODEL_RUN_FAILED" || sent[0]["saved_query_id"] != rfModelID {
		t.Errorf("code = %v, saved_query_id = %v", env["code"], sent[0]["saved_query_id"])
	}
	if want := rfModelID + ":" + rfStartedStored.Format(time.RFC3339Nano); env["dedup_subject"] != want {
		t.Errorf("dedup_subject = %v, want %s: the attempt, at the precision it is stored at", env["dedup_subject"], want)
	}
	assertNoErrorText(t, sent[0])
	assertMetadataOnly(t, sent[0], "saved_query_id")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}

	// The retry of the same report.
	mock = rfMock(t)
	rec = captureRunNotices(t)
	rfExpectUpToDedupe(mock, false)
	rfExpectExists(mock, true)
	mock.ExpectRollback()
	if w := rfServe(t, rfPath, body(), rfSecret); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if sent := rec.notices(t); len(sent) != 0 {
		t.Fatalf("model failure notices sent = %d for a duplicate delivery, want 0", len(sent))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A report about a schedule that is gone records nothing, so it tells nobody.
func TestRecordModelRunFailure_NothingRecordedTellsNobody(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_SECRET", rfSecret)
	mock := rfMock(t)
	rec := captureRunNotices(t)
	rfExpectModel(mock)
	mock.ExpectQuery(rfScheduleLookupSQL(false)).
		WithArgs(rfModelID, rfScheduleID, scheduleAfterUpstream).
		WillReturnRows(sqlmock.NewRows([]string{"schedule_id", "temporal_schedule_id", "run_as_user_id", "status"}))
	mock.ExpectRollback()

	w := rfServe(t, rfPath, rfBody(t, map[string]any{
		"schedule_id": rfScheduleID, "error": "timed out", "started_at": rfStartedWire,
	}), rfSecret)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if sent := rec.notices(t); len(sent) != 0 {
		t.Fatalf("model failure notices sent = %d, want 0", len(sent))
	}
}

// A schedule that paused itself will not run again until somebody resumes it, and
// nobody knows to. The owner is told once, with no reason text: the run's row has it.
func TestModelSchedulePaused_AnAutoPauseTellsTheOwner(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	rec := captureRunNotices(t)
	mock.ExpectExec(`UPDATE saved_query_schedules[\s\S]*SET status = 'paused', auto_paused_at = NOW\(\)`).
		WithArgs(provSchedule, modelRunSecretError).
		WillReturnResult(sqlmock.NewResult(0, 1))

	autoPauseModelSchedule(context.Background(), db.DB, savedQueryID, provSchedule, "", modelRunSecretError)

	sent := rec.notices(t)
	if len(sent) != 1 {
		t.Fatalf("schedule-paused notices sent = %d, want 1: a paused schedule never runs again by itself", len(sent))
	}
	env := noticeError(t, sent[0])
	if env["code"] != "MODEL_SCHEDULE_PAUSED" || env["severity"] != "warning" {
		t.Errorf("code = %v, severity = %v; want MODEL_SCHEDULE_PAUSED, warning", env["code"], env["severity"])
	}
	if sent[0]["saved_query_id"] != savedQueryID || sent[0]["action_url"] != "/models/"+savedQueryID {
		t.Errorf("saved_query_id = %v, action_url = %v", sent[0]["saved_query_id"], sent[0]["action_url"])
	}
	if subject, _ := env["dedup_subject"].(string); !strings.HasPrefix(subject, "schedule|"+provSchedule+"|") {
		t.Errorf("dedup_subject = %q, want schedule|%s|<when>", subject, provSchedule)
	}
	if sent[0]["message"] != modelSchedulePausedMessage {
		t.Errorf("message = %q, want the fixed sentence", sent[0]["message"])
	}
	assertNoErrorText(t, sent[0])
	assertMetadataOnly(t, sent[0], "saved_query_id")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A pause that wrote nothing (the schedule is gone, or the write failed) did not
// pause anything, so it tells nobody.
func TestModelSchedulePaused_APauseThatWroteNothingTellsNobody(t *testing.T) {
	cases := []struct {
		name   string
		result func(e *sqlmock.ExpectedExec)
	}{
		{"no such schedule", func(e *sqlmock.ExpectedExec) { e.WillReturnResult(sqlmock.NewResult(0, 0)) }},
		{"the write failed", func(e *sqlmock.ExpectedExec) { e.WillReturnError(errors.New("connection reset")) }},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			rec := captureRunNotices(t)
			tc.result(mock.ExpectExec(`UPDATE saved_query_schedules`))

			autoPauseModelSchedule(context.Background(), db.DB, savedQueryID, provSchedule, "", "refused")

			if sent := rec.notices(t); len(sent) != 0 {
				t.Fatalf("schedule-paused notices sent = %d, want 0", len(sent))
			}
		})
	}
}
