package projector

// A finished chat batch run said "1/1 steps" in the chat panel while /runtime said 7/7
// (prod runs e234bde9, d785dfc8). Both read pipeline_progress, which has two writers
// that disagreed on the total. The temporal adapter writes the run's plan length — 1
// for an executor-only run. The projector defaulted an event without
// progress.total_steps to 7, and the v2 workflow's events never carry one: the plan
// rides in metadata.execution_plan. GREATEST in the upsert then kept the 7, and a
// "completed" event landed current_step on it. The projector consumes Kafka after the
// adapter's synchronous write, so its 7/7 was the last word.
//
// The bug class is a writer inventing a value it was not given: an event's total is
// its plan's length, and an event with neither leaves the stored total alone.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/segmentio/kafka-go"
)

func completedEvent(meta map[string]interface{}) map[string]interface{} {
	ev := map[string]interface{}{
		"schema_version": 2,
		"event_type":     "PIPELINE_COMPLETED",
		"pipeline_id":    guardPipelineID,
		"execution_id":   guardExecID,
		"status":         "completed",
	}
	if meta != nil {
		ev["metadata"] = meta
	}
	return ev
}

func executeOnlyPlan() map[string]interface{} {
	return map[string]interface{}{"execution_plan": map[string]interface{}{
		"stages": []interface{}{map[string]interface{}{"id": "executor", "status": "completed", "progress": 100}},
	}}
}

// projectSteps runs one event through projectEvent and returns the current_step ($15)
// and total_steps ($16) bound to the pipeline_progress upsert. rowExists picks whether
// pipeline_progress already holds a row for this execution.
func projectSteps(t *testing.T, rowExists bool, event map[string]interface{}) (current, total driver.Value) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_run_events")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	read := mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(version, 1), status, execution_id")).
		WithArgs(guardPipelineID)
	if rowExists {
		read.WillReturnRows(sqlmock.NewRows([]string{"version", "status", "execution_id"}).
			AddRow(7, "processing", guardExecID))
	} else {
		read.WillReturnError(sql.ErrNoRows)
	}

	cur, tot := &capturedArg{}, &capturedArg{}
	args := make([]driver.Value, 22)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	args[14], args[15] = cur, tot
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_progress")).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := &EventProjector{db: db, ctx: context.Background(), lastSeq: map[string]int64{}, gapSeen: map[string]bool{}}
	if err := p.projectEvent(kafka.Message{Value: payload}); err != nil {
		t.Fatalf("projectEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	if !cur.seen || !tot.seen {
		t.Fatal("current_step/total_steps were never bound")
	}
	return cur.got, tot.got
}

func TestCompletedEventTakesItsTotalFromThePlan(t *testing.T) {
	current, total := projectSteps(t, true, completedEvent(executeOnlyPlan()))
	if total != int64(1) || current != int64(1) {
		t.Fatalf("an executor-only run's completion bound %v/%v steps, want 1/1 — "+
			"the adapter writes the plan's length, and GREATEST keeps whichever is larger", current, total)
	}
}

// 0 binds no total: GREATEST(0, stored) keeps the stored one, and the "completed"
// branch of the upsert lands current_step on that stored total.
func TestEventWithoutATotalLeavesTheStoredTotalAlone(t *testing.T) {
	if _, total := projectSteps(t, true, completedEvent(nil)); total != int64(0) {
		t.Fatalf("an event with no total and no plan bound total_steps=%v over an existing row, want 0 (keep the stored total)", total)
	}
}

// The control: the first write for a pipeline still gets the default.
func TestFirstWriteWithoutATotalStillDefaultsToSeven(t *testing.T) {
	if _, total := projectSteps(t, false, completedEvent(nil)); total != int64(7) {
		t.Fatalf("an insert with no total bound total_steps=%v, want the default 7", total)
	}
}
