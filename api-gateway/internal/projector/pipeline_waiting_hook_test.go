package projector

// OnPipelineWaiting is how a run that stopped to ask a question reaches someone who
// is not watching it: the gateway's handler (handlers/pipeline_wait_alert.go) decides
// whether the run is one nobody started by hand, and tells its owner. These tests pin
// the projector's half: the hook fires for a PIPELINE_WAITING it is seeing for the
// first time, after that wait is on the progress row, and for nothing else. A replay
// of this topic (see after_pipeline_hook_test.go) must not ask the owner again.

import (
	"context"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/segmentio/kafka-go"
)

const waitingEventJSON = `{
	"event_id": "evt-waiting-1",
	"event_type": "PIPELINE_WAITING",
	"pipeline_id": "8f14e45f-ceea-467a-9f52-f5b3a1f2c7d9",
	"execution_id": "1d2b3c4a-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
	"stage": "intent",
	"status": "waiting_for_user",
	"blocking_reason": {"type": "table_selection", "description": "Select table(s) to sync"},
	"timestamp": "2026-10-16T09:00:00Z"
}`

type waitCall struct{ pipelineID, executionID, blockingType string }

// projectWaiting runs one message through projectEvent with the run-event INSERT
// reporting rowsAffected, no progress row yet, and the progress upsert answering
// upsertErr. It returns every call the waiting hook received.
func projectWaiting(t *testing.T, payload string, rowsAffected int64, upsertErr error) []waitCall {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_run_events")).
		WillReturnResult(sqlmock.NewResult(0, rowsAffected))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(version, 1), status, execution_id")).
		WillReturnRows(sqlmock.NewRows([]string{"version", "status", "execution_id"}))
	upsert := mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_progress"))
	if upsertErr != nil {
		upsert.WillReturnError(upsertErr)
	} else {
		upsert.WillReturnResult(sqlmock.NewResult(0, 1))
	}

	var calls []waitCall
	p := &EventProjector{db: db, ctx: context.Background(), lastSeq: map[string]int64{}, gapSeen: map[string]bool{}}
	p.OnPipelineWaiting = func(_ context.Context, pid, eid, bt string) {
		calls = append(calls, waitCall{pid, eid, bt})
	}
	_ = p.projectEvent(kafka.Message{Value: []byte(payload)})
	return calls
}

func TestPipelineWaitingHook_FiresOnceOnFirstSightingAndNotOnReplay(t *testing.T) {
	calls := projectWaiting(t, waitingEventJSON, 1, nil)
	if len(calls) != 1 {
		t.Fatalf("waiting hook fired %d times for a new wait, want 1: a scheduled run "+
			"that stopped to ask a question would wait with nobody told", len(calls))
	}
	want := waitCall{"8f14e45f-ceea-467a-9f52-f5b3a1f2c7d9", "1d2b3c4a-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "table_selection"}
	if calls[0] != want {
		t.Errorf("hook got %+v, want %+v", calls[0], want)
	}

	// The same event again: 0 rows from ON CONFLICT DO NOTHING is a replay.
	if calls := projectWaiting(t, waitingEventJSON, 0, nil); len(calls) != 0 {
		t.Fatalf("waiting hook fired %d times on a replay of a stored event, want 0: a "+
			"projector restart would ask every owner again about waits long answered", len(calls))
	}
}

// The handler re-reads the progress row to see the run is still waiting, so the hook
// fires only once this event's wait is on it. A failed write means it is not.
func TestPipelineWaitingHook_DoesNotFireWhenTheWaitWasNotWritten(t *testing.T) {
	if calls := projectWaiting(t, waitingEventJSON, 1, errors.New("connection reset")); len(calls) != 0 {
		t.Fatalf("waiting hook fired %d times though the wait never reached pipeline_progress", len(calls))
	}
}

func TestPipelineWaitingHook_IgnoresEveryOtherEventType(t *testing.T) {
	types := []string{"PIPELINE_STARTED", "STAGE_PROGRESS", "STAGE_COMPLETED", "PIPELINE_COMPLETED", "PIPELINE_FAILED"}
	if len(types) == 0 {
		t.Fatal("no event types")
	}
	for _, eventType := range types {
		t.Run(eventType, func(t *testing.T) {
			payload := regexp.MustCompile(`"PIPELINE_WAITING"`).ReplaceAllString(waitingEventJSON, `"`+eventType+`"`)
			if calls := projectWaiting(t, payload, 1, nil); len(calls) != 0 {
				t.Errorf("the waiting hook fired for %s", eventType)
			}
		})
	}
}

// A nil hook is every binary and test that does not wire one.
func TestPipelineWaitingHook_NilHookIsSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_run_events")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(version, 1), status, execution_id")).
		WillReturnRows(sqlmock.NewRows([]string{"version", "status", "execution_id"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_progress")).WillReturnResult(sqlmock.NewResult(0, 1))

	p := &EventProjector{db: db, ctx: context.Background(), lastSeq: map[string]int64{}, gapSeen: map[string]bool{}}
	if err := p.projectEvent(kafka.Message{Value: []byte(waitingEventJSON)}); err != nil {
		t.Fatalf("projectEvent: %v", err)
	}
}
