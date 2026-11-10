package handlers

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A scheduled run that stops to ask a question waits for an answer nobody knows
// is wanted: nobody started it, so nobody is watching it. The projector calls
// NotifyPipelineAwaitingInput for each wait it stores (projector
// pipeline_waiting_hook_test.go); these tests pin what the handler then does.
//
// Harness helpers (wsScopeMockDB, wsScopePipeline, wsScopeExecution) come from
// the workspace scoping tests; captureRunNotices from run_notices_test.go.

const waitRowSQL = `SELECT COALESCE\(e\.trigger_source, 'manual'\)[\s\S]*FROM executions e[\s\S]*WHERE e\.id = \$1 AND e\.pipeline_id = \$2`

// waitRow is what the database says about the waiting run when the alert is checked.
type waitRow struct {
	trigger      string
	progressIsIt bool
	progress     string
	blocking     string
}

var scheduledAndWaiting = waitRow{"scheduled", true, "waiting_for_user", "table_selection"}

func expectWaitRow(mock sqlmock.Sqlmock, r waitRow) {
	mock.ExpectQuery(waitRowSQL).
		WithArgs(wsScopeExecution, wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"trigger_source", "progress_is_it", "progress", "blocking"}).
			AddRow(r.trigger, r.progressIsIt, r.progress, r.blocking))
}

// freshWaitAlerts gives a test its own once-per-wait memory.
func freshWaitAlerts(t *testing.T) {
	t.Helper()
	prev := waitAlerts
	waitAlerts = newWaitAlertClaims()
	t.Cleanup(func() { waitAlerts = prev })
}

func TestPipelineWaitAlert_PayloadIsCodedRunScopedAndCarriesNoFreeText(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	freshWaitAlerts(t)
	rec := captureRunNotices(t)
	expectWaitRow(mock, scheduledAndWaiting)

	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")

	sent := rec.notices(t)
	if len(sent) != 1 {
		t.Fatalf("awaiting-input notices sent = %d, want 1: the owner of a scheduled run that stopped to ask is told", len(sent))
	}
	env := noticeError(t, sent[0])
	if env["code"] != "PIPELINE_AWAITING_INPUT" || env["severity"] != "warning" {
		t.Errorf("code = %v, severity = %v; want PIPELINE_AWAITING_INPUT, warning", env["code"], env["severity"])
	}
	if got, want := env["dedup_subject"], wsScopeExecution+":table_selection"; got != want {
		t.Errorf("dedup_subject = %v, want %s: one notice per wait of one run", got, want)
	}
	if sent[0]["pipeline_id"] != wsScopePipeline || sent[0]["action_url"] != "/pipelines/"+wsScopePipeline {
		t.Errorf("pipeline_id = %v, action_url = %v", sent[0]["pipeline_id"], sent[0]["action_url"])
	}
	// The question's own wording (the event's description and details) can name
	// tables and columns. The notice carries a fixed sentence instead.
	if sent[0]["message"] != awaitingInputMessage {
		t.Errorf("message = %q, want the fixed sentence", sent[0]["message"])
	}
	assertMetadataOnly(t, sent[0], "pipeline_id")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// The same wait can reach the projector as two events with two ids (each is
// stored once, so each fires the hook). The owner is told once.
func TestPipelineWaitAlert_TwoProducerCopiesPublishOnce(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	freshWaitAlerts(t)
	rec := captureRunNotices(t)
	expectWaitRow(mock, scheduledAndWaiting)
	expectWaitRow(mock, scheduledAndWaiting)

	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")
	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")

	if sent := rec.notices(t); len(sent) != 1 {
		t.Fatalf("awaiting-input notices sent = %d for two copies of one wait, want 1", len(sent))
	}
}

// Each control is one way the run is not a scheduled run still waiting on this
// question when the alert is checked. None tells anybody.
func TestPipelineWaitAlert_OnlyAScheduledRunStillWaitingIsAlerted(t *testing.T) {
	cases := []struct {
		name string
		row  waitRow
	}{
		{"manual run: whoever started it is watching", waitRow{"manual", true, "waiting_for_user", "table_selection"}},
		{"a run a stream started", waitRow{"cdc", true, "waiting_for_user", "table_selection"}},
		{"paused by a user, not asking", waitRow{"scheduled", true, "waiting_for_user", "paused_by_user"}},
		{"answered already", waitRow{"scheduled", true, "processing", ""}},
		{"a newer run took the progress row", waitRow{"scheduled", false, "waiting_for_user", "table_selection"}},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			freshWaitAlerts(t)
			rec := captureRunNotices(t)
			expectWaitRow(mock, tc.row)

			NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")

			if sent := rec.notices(t); len(sent) != 0 {
				t.Fatalf("awaiting-input notices sent = %d, want 0", len(sent))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// The control for the control table: the one row that does alert, run through
// the same harness, so a table that passes because nothing ever alerts fails here.
func TestPipelineWaitAlert_ControlTableHarnessCanAlert(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	freshWaitAlerts(t)
	rec := captureRunNotices(t)
	expectWaitRow(mock, scheduledAndWaiting)

	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")

	if sent := rec.notices(t); len(sent) != 1 {
		t.Fatalf("awaiting-input notices sent = %d, want 1", len(sent))
	}
}

// A run the database no longer has, and an id that is not a run's, tell nobody.
func TestPipelineWaitAlert_UnknownRunTellsNobody(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	freshWaitAlerts(t)
	rec := captureRunNotices(t)
	mock.ExpectQuery(waitRowSQL).WithArgs(wsScopeExecution, wsScopePipeline).WillReturnError(sql.ErrNoRows)

	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, wsScopeExecution, "table_selection")
	// Not a UUID: never looked up (sqlmock fails a query it was not told about).
	NotifyPipelineAwaitingInput(context.Background(), wsScopePipeline, "legacy-run-1", "table_selection")

	if sent := rec.notices(t); len(sent) != 0 {
		t.Fatalf("awaiting-input notices sent = %d, want 0", len(sent))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
