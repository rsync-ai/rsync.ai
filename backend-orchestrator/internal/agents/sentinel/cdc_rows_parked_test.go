package sentinel

// Tests for the parked-rows alarm (cdc_rows_parked.go): changes the CDC sink consumed and set
// aside in its dead-letter queue. Every lag signal reads healthy for these, so this alarm is
// the only thing that says the destination is missing them. It fires on GROWTH of the
// cumulative count, so these lock both halves: a new parked row always raises a critical, and
// rows the owner was already told about never raise another one -- not on the next tick, and
// not after a restart.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const parkedPipeline = "11111111-2222-3333-4444-555555555555"

func newParkedMock(t *testing.T) (*CDCSentinel, sqlmock.Sqlmock) {
	t.Helper()
	s := &CDCSentinel{}
	return s, freshMock(t, s)
}

// freshMock points s at a new mock database, keeping what s remembers between ticks.
func freshMock(t *testing.T, s *CDCSentinel) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s.db = db
	return mock
}

// expectParkedRead scripts loadParkedRows; a nil table list is "nothing parked".
func expectParkedRead(mock sqlmock.Sqlmock, tables ...parkedTable) {
	rows := sqlmock.NewRows([]string{"qualified_name", "sum"})
	for _, t := range tables {
		rows.AddRow(t.name, t.rows)
	}
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs(parkedPipeline).WillReturnRows(rows)
}

// metaArg matches the issue's metadata JSON on the counts the alert reports.
type metaArg struct{ dlqRows, newRows int64 }

func (m metaArg) Match(v driver.Value) bool {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		return false
	}
	var meta map[string]interface{}
	if json.Unmarshal(b, &meta) != nil {
		return false
	}
	dlq, _ := meta["dlq_rows"].(float64)
	fresh, _ := meta["new_dlq_rows"].(float64)
	return int64(dlq) == m.dlqRows && int64(fresh) == m.newRows
}

func expectParkedIssue(mock sqlmock.Sqlmock, dlqRows, newRows int64) {
	mock.ExpectQuery(`INSERT INTO sentinel_active_issues`).
		WithArgs(rowsParkedIssueID(parkedPipeline), string(IssueTypeRowsParked), string(IssueSeverityCritical),
			parkedPipeline, string(ComponentTypeCDCPipeline), sqlmock.AnyArg(), metaArg{dlqRows, newRows}).
		WillReturnRows(detectedAtRow())
}

func checkParked(t *testing.T, s *CDCSentinel, mock sqlmock.Sqlmock) {
	t.Helper()
	s.checkRowsParked(context.Background(), parkedPipeline, "orders-cdc", "postgresql")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// checkParkedWritesNothing runs one tick that must not write. sqlmock answers an unscripted
// statement with an error, and emitCDCIssue only logs a failed write, so ExpectationsWereMet
// cannot see a stray alert. A trap can: it is left unconsumed only if nothing wrote.
func checkParkedWritesNothing(t *testing.T, s *CDCSentinel, mock sqlmock.Sqlmock) {
	t.Helper()
	mock.ExpectExec(`.`).WillReturnResult(sqlmock.NewResult(0, 1))
	s.checkRowsParked(context.Background(), parkedPipeline, "orders-cdc", "postgresql")
	err := mock.ExpectationsWereMet()
	if err == nil {
		t.Fatal("the tick wrote to sentinel_active_issues; it must not")
	}
	if !strings.Contains(err.Error(), "ExpectedExec") {
		t.Fatalf("the tick did not run the reads it was scripted for: %v", err)
	}
}

// The first parked rows raise a critical, and the same count on the next tick raises
// nothing: the owner has been told, and the gateway would otherwise re-send every hour for
// the life of the pipeline.
func TestRowsParked_GrowthAlertsAndTheSameCountDoesNot(t *testing.T) {
	s, mock := newParkedMock(t)

	expectParkedRead(mock, parkedTable{"public.orders", 40}, parkedTable{"public.payments", 2})
	mock.ExpectQuery(`FROM sentinel_active_issues`).WithArgs(rowsParkedIssueID(parkedPipeline)).
		WillReturnError(sql.ErrNoRows)
	expectParkedIssue(mock, 42, 42)
	checkParked(t, s, mock)

	// Same count: one read, nothing written.
	mock = freshMock(t, s)
	expectParkedRead(mock, parkedTable{"public.orders", 40}, parkedTable{"public.payments", 2})
	checkParkedWritesNothing(t, s, mock)

	// More parked: alerts again, for the new ones.
	mock = freshMock(t, s)
	expectParkedRead(mock, parkedTable{"public.orders", 45}, parkedTable{"public.payments", 2})
	expectParkedIssue(mock, 47, 5)
	checkParked(t, s, mock)
}

// A restarted orchestrator has no memory, so it reads the count it last reported back from
// the issue row. Without that, every redeploy would re-announce every parked row.
func TestRowsParked_ARestartDoesNotReannounceOldRows(t *testing.T) {
	s, mock := newParkedMock(t)
	expectParkedRead(mock, parkedTable{"public.orders", 42})
	mock.ExpectQuery(`FROM sentinel_active_issues`).WithArgs(rowsParkedIssueID(parkedPipeline)).
		WillReturnRows(sqlmock.NewRows([]string{"dlq_rows"}).AddRow(int64(42)))
	checkParkedWritesNothing(t, s, mock)

	s, mock = newParkedMock(t)
	expectParkedRead(mock, parkedTable{"public.orders", 50})
	mock.ExpectQuery(`FROM sentinel_active_issues`).WithArgs(rowsParkedIssueID(parkedPipeline)).
		WillReturnRows(sqlmock.NewRows([]string{"dlq_rows"}).AddRow(int64(42)))
	expectParkedIssue(mock, 50, 8)
	checkParked(t, s, mock)
}

// Counts that went DOWN (the stats were reset) rewrite the row silently, so the next real
// increase is measured from the new count rather than hidden under the old one.
func TestRowsParked_AFallingCountLowersTheWatermarkWithoutAlerting(t *testing.T) {
	s, mock := newParkedMock(t)
	s.rememberParkedRows(parkedPipeline, 50)

	expectParkedRead(mock, parkedTable{"public.orders", 10})
	mock.ExpectExec(`UPDATE sentinel_active_issues SET description`).
		WithArgs(rowsParkedIssueID(parkedPipeline), sqlmock.AnyArg(), metaArg{10, 0}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	checkParked(t, s, mock)

	mock = freshMock(t, s)
	expectParkedRead(mock, parkedTable{"public.orders", 12})
	expectParkedIssue(mock, 12, 2)
	checkParked(t, s, mock)
}

// Nothing parked clears the issue once, not on every tick for every pipeline.
func TestRowsParked_NothingParkedClearsTheIssueOnce(t *testing.T) {
	s, mock := newParkedMock(t)
	expectParkedRead(mock)
	mock.ExpectExec(`DELETE FROM sentinel_active_issues WHERE id = \$1`).
		WithArgs(rowsParkedIssueID(parkedPipeline)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	checkParked(t, s, mock)

	mock = freshMock(t, s)
	expectParkedRead(mock)
	checkParkedWritesNothing(t, s, mock)
}

// A read that fails changes nothing. In particular a failed read of the reported count must
// not be taken as "nothing reported yet", which would re-announce every parked row.
func TestRowsParked_AFailedReadChangesNothing(t *testing.T) {
	s, mock := newParkedMock(t)
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs(parkedPipeline).
		WillReturnError(errors.New("control DB unavailable"))
	checkParkedWritesNothing(t, s, mock)

	mock = freshMock(t, s)
	expectParkedRead(mock, parkedTable{"public.orders", 42})
	mock.ExpectQuery(`FROM sentinel_active_issues`).WithArgs(rowsParkedIssueID(parkedPipeline)).
		WillReturnError(errors.New("control DB unavailable"))
	checkParkedWritesNothing(t, s, mock)
}

// The description and metadata carry table names and counts, never anything from a row, and
// name at most rowsParkedTablesShown tables.
func TestRowsParkedReport_NamesAndCountsOnly(t *testing.T) {
	desc, meta := rowsParkedReport([]parkedTable{
		{"public.orders", 40}, {"public.payments", 7}, {"public.users", 2}, {"public.audit", 1},
	}, 50, 12)

	for _, want := range []string{"50 change(s)", "(12 new)", "public.orders (40)", "and 1 more table(s)", "NOT in the destination"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q does not say %q", desc, want)
		}
	}
	if strings.Contains(desc, "public.audit") {
		t.Errorf("description names more than %d tables: %q", rowsParkedTablesShown, desc)
	}
	if got := meta["tables"].([]string); len(got) != rowsParkedTablesShown {
		t.Errorf("metadata names %d tables, want %d", len(got), rowsParkedTablesShown)
	}
	want := map[string]bool{"dlq_rows": true, "new_dlq_rows": true, "tables": true, "more_tables": true, "by_table": true}
	for k := range meta {
		if !want[k] {
			t.Errorf("metadata carries %q, which is not a name or a count", k)
		}
	}

	// When every parked row is new, "(N new)" would only repeat the total.
	if desc, _ := rowsParkedReport([]parkedTable{{"public.orders", 3}}, 3, 3); strings.Contains(desc, "new)") {
		t.Errorf("first report repeats the total as new: %q", desc)
	}
}
