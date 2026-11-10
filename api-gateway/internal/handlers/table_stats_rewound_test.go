package handlers

// A batch run whose sink lost batches rewinds its tables' resume positions so a
// Resume re-reads the lost rows (orchestrator cdc.RewindCheckpointsOfExecution,
// which records each table in pipeline_run_rewinds, migration 141). The table stats
// called such a table "degraded", the same word as any table short of rows or with a
// dead-lettered row, so the page could not say "Rewound" apart from "rows not
// written". These pin that the API carries the difference.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestTableStatsSummaryCountsTheDegradedTablesThatWereRewound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	cols := []string{"mode", "status", "cnt", "sum_read_rows", "sum_inserted_rows",
		"sum_inserts", "sum_updates", "sum_deletes", "sum_total_events",
		"sum_applied_inserts", "sum_applied_updates", "sum_applied_deletes", "sum_applied_total_events",
		"sum_dlq_rows", "cnt_tables_with_dlq", "cnt_rewound"}
	rows := sqlmock.NewRows(cols).
		// 3 degraded tables, 2 of them rewound.
		AddRow("batch", "degraded", 3, int64(900), int64(600), nil, nil, nil, nil, nil, nil, nil, nil, int64(40), 1, 2).
		// A completed table its run also rewound (the rewind puts back every table
		// of the run): not a degraded one, so not counted.
		AddRow("batch", "completed", 1, int64(10), int64(10), nil, nil, nil, nil, nil, nil, nil, nil, int64(0), 0, 1)
	// Matched as a regexp against the statement: the summary must count through rewoundSQL.
	mock.ExpectQuery(regexp.QuoteMeta(`COUNT(*) FILTER (WHERE ` + rewoundSQL + `) as cnt_rewound`)).WillReturnRows(rows)

	got := computeTableStatsSummary(db, "p1", "e1")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the summary does not count rewound tables: %v", err)
	}
	if got.TablesDegraded != 3 || got.TablesRewound != 2 {
		t.Fatalf("tables_degraded=%d tables_rewound=%d, want 3 and 2", got.TablesDegraded, got.TablesRewound)
	}
}

// The list lane reads the package-global db.GetDB() behind an RBAC gate, so no unit
// seam drives it (see TestEveryTableStatsQuerySelectsTheOrchestrationExecutionID);
// this pins its query and scan instead. A column selected but not scanned fails the
// scan, and the row is `continue`d — every table would vanish from the page.
func TestTheTableStatsListSelectsAndScansRewound(t *testing.T) {
	src, err := os.ReadFile("table_stats.go")
	if err != nil {
		t.Fatalf("read table_stats.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func GetPipelineTableStats(")
	end := strings.Index(body[start+1:], "\nfunc ")
	if start < 0 || end < 0 {
		t.Fatal("GetPipelineTableStats not found — the scan is broken, so a pass would mean nothing")
	}
	fn := body[start : start+1+end]
	if !strings.Contains(fn, "COALESCE(dlq_rows, 0) AS dlq_rows,\n\t\t\t` + rewoundSQL + ` AS rewound,") {
		t.Error("the list query does not select rewoundSQL AS rewound after dlq_rows")
	}
	if !strings.Contains(fn, "&dlqRows, &rewound,") {
		t.Error("the list scan does not read rewound after dlqRows")
	}
	if !strings.Contains(fn, "stat.Rewound = rewound") {
		t.Error("the list does not put rewound on the table stat")
	}
}

func TestRewoundIsInTheJSONOnlyWhenTrue(t *testing.T) {
	for _, tc := range []struct {
		stat TableStat
		want string
	}{
		{TableStat{QualifiedName: "list_task", Status: "degraded", Rewound: true}, `"rewound":true`},
		{TableStat{QualifiedName: "list_task", Status: "degraded"}, ""},
	} {
		b, err := json.Marshal(tc.stat)
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(string(b), `"rewound"`)
		if tc.want == "" && has {
			t.Errorf("a table that was not rewound carries the field: %s", b)
		}
		if tc.want != "" && !strings.Contains(string(b), tc.want) {
			t.Errorf("want %s in %s", tc.want, b)
		}
	}
}
