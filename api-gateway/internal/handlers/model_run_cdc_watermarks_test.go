package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"api-gateway/internal/db"
	"api-gateway/internal/validators"
)

// What a model run records about the CDC tables it read (saved_query_runs.cdc_watermarks,
// migration 137). The pure builder decides which watermarks belong to the run; the
// writer decides whether the column is named at all. The PG test
// (TestPG_CDCWatermark_AModelRunRecordsItsWatermark) drives the read end to end.

func TestBuildModelRunCDCWatermarks(t *testing.T) {
	at := func(hhmm string) sql.NullTime {
		ts, err := time.Parse(time.RFC3339, "2026-10-03T"+hhmm+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		return sql.NullTime{Time: ts, Valid: true}
	}
	ref := func(parts ...string) validators.TableRef { return validators.TableRef{Parts: parts} }

	cases := []struct {
		name  string
		refs  []validators.TableRef
		marks []cdcTableWatermark
		want  string // "" = nil
	}{
		{
			name:  "a qualified reference records the table it names",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:07")}, {"p2", "analytics.customers", at("10:08")}},
			want:  `{"analytics.orders":{"pipeline_id":"p1","source_time":"2026-10-03T10:07:00Z"}}`,
		},
		{
			// shop.orders is the SOURCE name of the table analytics.orders lands; the run
			// did not read the destination, so it has no watermark to record.
			name:  "a source-side schema is a different table",
			refs:  []validators.TableRef{ref("shop", "orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:07")}},
		},
		{
			name:  "an unqualified reference records every table of that name",
			refs:  []validators.TableRef{ref("orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:07")}, {"p2", "staging.orders", at("09:00")}},
			want: `{"analytics.orders":{"pipeline_id":"p1","source_time":"2026-10-03T10:07:00Z"},` +
				`"staging.orders":{"pipeline_id":"p2","source_time":"2026-10-03T09:00:00Z"}}`,
		},
		{
			name:  "a watermark with no destination cannot be placed and is left out",
			refs:  []validators.TableRef{ref("orders")},
			marks: []cdcTableWatermark{{"p1", "  ", at("10:07")}},
		},
		{
			name:  "an unknown source time is recorded as null",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", sql.NullTime{}}},
			want:  `{"analytics.orders":{"pipeline_id":"p1","source_time":null}}`,
		},
		{
			name:  "two pipelines landing one table keep the earlier source time",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:07")}, {"p2", "analytics.orders", at("10:05")}},
			want:  `{"analytics.orders":{"pipeline_id":"p2","source_time":"2026-10-03T10:05:00Z"}}`,
		},
		{
			name:  "the earlier source time wins in either row order",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p2", "analytics.orders", at("10:05")}, {"p1", "analytics.orders", at("10:07")}},
			want:  `{"analytics.orders":{"pipeline_id":"p2","source_time":"2026-10-03T10:05:00Z"}}`,
		},
		{
			name:  "an unknown source time is the earliest of all",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:05")}, {"p2", "analytics.orders", sql.NullTime{}}},
			want:  `{"analytics.orders":{"pipeline_id":"p2","source_time":null}}`,
		},
		{
			name:  "equal source times keep the lower pipeline id",
			refs:  []validators.TableRef{ref("analytics", "orders")},
			marks: []cdcTableWatermark{{"p2", "analytics.orders", at("10:05")}, {"p1", "analytics.orders", at("10:05")}},
			want:  `{"analytics.orders":{"pipeline_id":"p1","source_time":"2026-10-03T10:05:00Z"}}`,
		},
		{
			name:  "a model that reads nothing records nothing",
			marks: []cdcTableWatermark{{"p1", "analytics.orders", at("10:07")}},
		},
		{
			name: "no CDC tables records nothing",
			refs: []validators.TableRef{ref("analytics", "orders")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildModelRunCDCWatermarks(tc.refs, tc.marks)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %s, want nil", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRecordModelRunOutcome_ARunThatReadCDCTablesRecordsTheirWatermarks(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	marks := `{"analytics.orders":{"pipeline_id":"p1","source_time":"2026-10-03T10:07:00Z"}}`
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*coalesced_count, cdc_watermarks\s*\)[\s\S]*, \$18::jsonb\s*\)\s*RETURNING run_id::text`).
		WithArgs(savedQueryID, provSchedule, string(triggerScheduled), "succeeded", "read", "analytics.b", sqlmock.AnyArg(), "",
			"", provActor, sqlmock.AnyArg(),
			"", "", "", "", int64(0), int64(0), marks).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read", TargetTable: "analytics.b",
			CDCWatermarks: []byte(marks)},
		modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule, ActorID: provActor})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the run's watermarks were not recorded with it: %v", err)
	}
}

// A run with no map must not name the column: until migration 137 has applied, naming it
// fails the insert and loses the run's whole history row, not just its watermarks.
func TestRecordModelRunOutcome_ARunWithNoCDCWatermarksDoesNotNameTheColumn(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(expectedSQL, actualSQL string) error {
			if strings.Contains(actualSQL, "cdc_watermarks") {
				return fmt.Errorf("a run with no CDC watermarks named the column: %s", actualSQL)
			}
			return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	failOnSwallowedError(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).
		WithArgs(savedQueryID, "", string(triggerManual), "succeeded", "read", "", sqlmock.AnyArg(), "",
			"", provActor, sqlmock.AnyArg(),
			"", "", "", "", int64(0), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	if got := recordModelRunOutcome(context.Background(), mockDB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read"},
		modelRunAudit{Trigger: triggerManual, ActorID: provActor}); got != provRunB {
		t.Fatalf("returned run id %q, want %q", got, provRunB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
