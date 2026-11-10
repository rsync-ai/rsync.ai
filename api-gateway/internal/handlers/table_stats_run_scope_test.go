package handlers

// KI-BATCH-UI-STALE-AFTER-RUN (table stats half): a table-stats read that named
// no run listed and summed the rows of every run, one per table per run, so a
// 6-table batch pipeline run twice reported total_tables 12 (prod pipeline
// 9a094389, 2026-09-26). With no run named, a batch read now scopes to the
// pipeline's latest run.
//
// Bug class: an unscoped read over a table keyed by (run, table) aggregates
// across runs. The list, the count and the summary all take the scope from
// tableStatsReadScope, so pinning it covers all three.

import (
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const runScopePipeline = "9a094389-c5a8-4179-8880-2cb42b460d4a"

// latestBatchRunQuery matches the lookup and nothing broader: batch rows only,
// never the CDC stream key, newest run first.
const latestBatchRunQuery = `(?s)SELECT s\.execution_id::text\s+FROM pipeline_run_table_stats s` +
	`.*s\.pipeline_id = \$1::uuid.*s\.mode = 'batch'.*s\.execution_id <> \$1::uuid` +
	`.*GROUP BY s\.execution_id\s+ORDER BY MAX\(s\.created_at\) DESC.*LIMIT 1`

// newRunScopeMock returns a mock, the SQL it was sent, and tableStatsReadScope
// bound to it.
func newRunScopeMock(t *testing.T) (sqlmock.Sqlmock, *[]string, func(execID, mode string) (string, string)) {
	t.Helper()
	var sent []string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		sent = append(sent, actual)
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return mock, &sent, func(execID, mode string) (string, string) {
		return tableStatsReadScope(db, runScopePipeline, execID, mode)
	}
}

func TestTableStatsWithNoRunNamedReadTheLatestRun(t *testing.T) {
	for _, mode := range []string{"", "batch"} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			mock, _, scope := newRunScopeMock(t)
			mock.ExpectQuery(latestBatchRunQuery).
				WithArgs(runScopePipeline).
				WillReturnRows(sqlmock.NewRows([]string{"execution_id"}).AddRow("8c09b6ba-0000-0000-0000-000000000002"))

			execID, gotMode := scope("", mode)
			if execID != "8c09b6ba-0000-0000-0000-000000000002" || gotMode != mode {
				t.Fatalf("no run named, mode %q: read scoped to (%q, %q), want the latest run (8c09b6ba-…, %q)", mode, execID, gotMode, mode)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Asked with no mode, a CDC pipeline keeps its stream rows: the lookup skips a
// pipeline whose row says CDC. Asked for batch, it does not.
func TestTableStatsLatestRunLookupLeavesACDCPipelineUnscoped(t *testing.T) {
	cases := []struct {
		mode        string
		wantCDCSkip bool
	}{{"", true}, {"batch", false}}
	for _, tc := range cases {
		tc := tc
		t.Run("mode="+tc.mode, func(t *testing.T) {
			mock, sent, scope := newRunScopeMock(t)
			mock.ExpectQuery(latestBatchRunQuery).
				WillReturnRows(sqlmock.NewRows([]string{"execution_id"}))
			_, _ = scope("", tc.mode)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			skip := regexp.MustCompile(`NOT EXISTS \(\s*SELECT 1 FROM pipelines p WHERE p\.id = \$1::uuid AND ` + regexp.QuoteMeta(pipelineRowIsCDCSQL))
			if got := skip.MatchString((*sent)[0]); got != tc.wantCDCSkip {
				t.Fatalf("mode %q: CDC-pipeline skip present=%v, want %v:\n%s", tc.mode, got, tc.wantCDCSkip, (*sent)[0])
			}
		})
	}
}

func TestTableStatsWithNoRunToFindReadEverything(t *testing.T) {
	t.Run("no batch run has stats", func(t *testing.T) {
		mock, _, scope := newRunScopeMock(t)
		mock.ExpectQuery(latestBatchRunQuery).WillReturnRows(sqlmock.NewRows([]string{"execution_id"}))
		if execID, mode := scope("", ""); execID != "" || mode != "" {
			t.Fatalf("got (%q, %q), want the unscoped read", execID, mode)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the lookup fails", func(t *testing.T) {
		mock, _, scope := newRunScopeMock(t)
		mock.ExpectQuery(latestBatchRunQuery).WillReturnError(errors.New("boom"))
		if execID, mode := scope("", "batch"); execID != "" || mode != "batch" {
			t.Fatalf("got (%q, %q), want the unscoped read", execID, mode)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// Controls: a named run and a CDC read keep the scope they had.
func TestTableStatsNamedRunAndCDCReadKeepTheirScope(t *testing.T) {
	t.Run("cdc reads the stream key", func(t *testing.T) {
		mock, _, scope := newRunScopeMock(t)
		if execID, mode := scope("", "cdc"); execID != runScopePipeline || mode != "cdc" {
			t.Fatalf("got (%q, %q), want the stream key", execID, mode)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a named batch run is read as named", func(t *testing.T) {
		mock, _, scope := newRunScopeMock(t)
		mock.ExpectQuery(`(?s)SELECT EXISTS.*FROM pipelines p`).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		const run = "6d97b44b-0000-0000-0000-000000000001"
		if execID, mode := scope(run, ""); execID != run || mode != "" {
			t.Fatalf("got (%q, %q), want the named run", execID, mode)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
