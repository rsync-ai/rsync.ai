package projector

// A batch table read "completed" on the run page while it was still copying. The sink
// stamped completed_at on every per-flush "running" event, a flush always has read ==
// written, and the status CASE's recovery branch took "a completion marker AND read ==
// written" as the end of the table. The bug class is a non-terminal event acting as a
// terminal one, so both halves are pinned: a batch "running" event binds no completed_at
// whatever its producer sent, and the recovery branch refuses a "running" event unless the
// stored row is already completed (so a late flush still cannot reopen a finished table).
//
// sqlmock cannot evaluate SQL, so this pins the bindings and the statement text;
// table_stats_captured_pg_test.go runs the running → completed sequence on a real server.

import (
	"database/sql/driver"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// runningCannotCompleteGuard is the recovery branch's gate, matched verbatim.
const runningCannotCompleteGuard = "WHEN EXCLUDED.mode = 'batch'\n" +
	"\t\t\t\t     AND (EXCLUDED.status <> 'running' OR pipeline_run_table_stats.status = 'completed')"

func batchStatsEvent(status string, rows float64, completedAt string) map[string]interface{} {
	meta := map[string]interface{}{
		"source":     "kafka_mcp_sink",
		"mode":       "batch",
		"status":     status,
		"table":      statsTable(),
		"started_at": "2026-10-01T05:00:00Z",
		"counts":     map[string]interface{}{"read_rows": rows, "inserted_rows": rows},
	}
	if completedAt != "" {
		meta["completed_at"] = completedAt
	}
	return map[string]interface{}{
		"event_type":   "TABLE_STATS",
		"pipeline_id":  testStatsPipeline,
		"execution_id": testStatsExec,
		"metadata":     meta,
	}
}

// projectBatchStatus runs one event through the real upsert and returns $7 (status) and
// $23 (completed_at). The statement must carry runningCannotCompleteGuard.
func projectBatchStatus(t *testing.T, ev map[string]interface{}) (status, completedAt driver.Value) {
	t.Helper()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	st, done := &capturedArg{}, &capturedArg{}
	args := make([]driver.Value, 0, 31)
	for i := 1; i <= 31; i++ {
		switch i {
		case 7:
			args = append(args, st)
		case 23:
			args = append(args, done)
		default:
			args = append(args, sqlmock.AnyArg())
		}
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(bytes_committed, 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes_committed"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta(runningCannotCompleteGuard)).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	p := &EventProjector{db: db, lastSeq: map[string]int64{}, gapSeen: map[string]bool{}}
	if err := p.upsertTableStats(ev); err != nil {
		t.Fatalf("upsertTableStats: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if !st.seen || !done.seen {
		t.Fatal("status/completed_at were never bound — the statement no longer carries them")
	}
	return st.got, done.got
}

// A sink from before the fix still stamps completed_at on a flush: the projector drops it.
func TestBatchRunningEventNeverCarriesACompletionMarker(t *testing.T) {
	status, completedAt := projectBatchStatus(t, batchStatsEvent("running", 500, "2026-10-01T05:00:03Z"))
	if status != "running" {
		t.Errorf("status bound as %#v, want running", status)
	}
	if completedAt != "" {
		t.Errorf("completed_at bound as %#v for a batch running event, want \"\" (NULL) — "+
			"with read == written it completes the table mid-copy", completedAt)
	}
}

// The control: the end-of-table event keeps its marker.
func TestBatchTerminalEventKeepsItsCompletionMarker(t *testing.T) {
	for _, s := range []string{"completed", "degraded"} {
		status, completedAt := projectBatchStatus(t, batchStatsEvent(s, 500, "2026-10-01T05:00:09Z"))
		if status != s || completedAt != "2026-10-01T05:00:09Z" {
			t.Errorf("%s event bound status/completed_at %#v/%#v, want %s/2026-10-01T05:00:09Z",
				s, status, completedAt, s)
		}
	}
}
