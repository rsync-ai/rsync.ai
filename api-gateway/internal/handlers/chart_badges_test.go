package handlers

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A tile's badges (dashboards-design.md § 5) against sqlmock: which stored fact each
// badge comes from, and the warnings that sit above a chart. No row value is read, so
// the only queries expected are the run, its checks and the freshness fact.

const badgeModel = "bbbbbbbb-0000-4000-8000-0000000000b1"

var badgeNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

var freshnessFactCols = []string{"id", "deadline", "materialization", "reference_at", "never_succeeded",
	"has_schedule", "schedule_status", "schedule_type", "auto_paused", "upstreams"}

func badgeDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func badgeSource(status, runErr string) *chartSourceRow {
	return &chartSourceRow{ID: badgeModel, Name: "daily_orders", Materialization: matTable,
		LastRunStatus: status, LastRunError: runErr, Built: true}
}

func TestChartBadges_SavedQueryReadsNothing(t *testing.T) {
	db, mock := badgeDB(t)
	s := &chartSourceRow{ID: badgeModel, Name: "Daily orders", LastRunStatus: "failed"}
	b, err := loadChartBadges(context.Background(), db, s, badgeNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.SourceKind != "saved_query" || b.DataAsOf != nil || b.Late {
		t.Fatalf("a saved query has no run badges: %+v", b)
	}
	if len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0], "last run failed") {
		t.Fatalf("warnings = %q, want the failed-run warning", b.Warnings)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestChartBadges_ModelFromTheOldestWatermark_LateAndChecked(t *testing.T) {
	db, mock := badgeDB(t)
	marks := `{"orders":{"pipeline_id":"p1","source_time":"2026-10-05T09:30:00Z"},` +
		`"customers":{"pipeline_id":"p2","source_time":"2026-10-05T08:15:00Z"},` +
		`"regions":{"pipeline_id":"p3"}}`
	mock.ExpectQuery(`FROM saved_query_runs\s+WHERE saved_query_id = \$1 AND status = 'succeeded'`).
		WithArgs(badgeModel).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "finished_at", "marks"}).
			AddRow("run-1", time.Date(2026, 10, 5, 9, 45, 0, 0, time.UTC), []byte(marks)))
	mock.ExpectQuery(`FROM saved_query_check_results`).WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"check_name", "severity", "status"}).
			AddRow("orders_not_empty", "error", "pass").
			AddRow("no_null_ids", "warn", "fail"))
	// Deadline one hour; the last success 2h15m ago; no schedule rebuilds it.
	mock.ExpectQuery(`freshness_deadline_seconds IS NOT NULL`).WithArgs(badgeModel).
		WillReturnRows(sqlmock.NewRows(freshnessFactCols).
			AddRow(badgeModel, 3600, matTable, time.Date(2026, 10, 5, 9, 45, 0, 0, time.UTC), false,
				false, "", "", false, 0))

	b, err := loadChartBadges(context.Background(), db, badgeSource("succeeded", ""), badgeNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.DataAsOfFrom != "cdc_watermark" || b.DataAsOf == nil ||
		!b.DataAsOf.Equal(time.Date(2026, 10, 5, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("data as of = %v (%s), want the oldest watermark 08:15", b.DataAsOf, b.DataAsOfFrom)
	}
	if b.RunID != "run-1" || len(b.Checks) != 2 || b.Checks[1].Status != "fail" {
		t.Fatalf("run %q checks %+v, want run-1's two checks", b.RunID, b.Checks)
	}
	if !b.Late || b.LateCause != causeNoSchedule || b.DeadlineSeconds != 3600 {
		t.Fatalf("late=%v cause=%q deadline=%d, want late with no schedule against 1 hour", b.Late, b.LateCause, b.DeadlineSeconds)
	}
	want := "Overdue: the model's rows are from 2026-10-05 08:15 UTC, past its 1 hour deadline (no schedule)."
	if len(b.Warnings) != 1 || b.Warnings[0] != want {
		t.Fatalf("warnings = %q, want %q", b.Warnings, want)
	}
	if b.LineageURL != "/explorer/lineage?focus=model%3A"+badgeModel {
		t.Fatalf("lineage = %q", b.LineageURL)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestChartBadges_ModelWithoutCDC_OnTime_StoppedByACheck(t *testing.T) {
	db, mock := badgeDB(t)
	finished := time.Date(2026, 10, 5, 11, 30, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM saved_query_runs\s+WHERE saved_query_id`).WithArgs(badgeModel).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "finished_at", "marks"}).
			AddRow("run-2", finished, []byte("null")))
	mock.ExpectQuery(`FROM saved_query_check_results`).WithArgs("run-2").
		WillReturnRows(sqlmock.NewRows([]string{"check_name", "severity", "status"}))
	// Deadline one day, built 30 minutes ago: on time.
	mock.ExpectQuery(`freshness_deadline_seconds IS NOT NULL`).WithArgs(badgeModel).
		WillReturnRows(sqlmock.NewRows(freshnessFactCols).
			AddRow(badgeModel, 86400, matTable, finished, false, true, "active", "cron", false, 0))

	b, err := loadChartBadges(context.Background(), db, badgeSource("failed", modelCheckBlockedError), badgeNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.DataAsOfFrom != "run_finished" || b.DataAsOf == nil || !b.DataAsOf.Equal(finished) {
		t.Fatalf("data as of = %v (%s), want the run's finish", b.DataAsOf, b.DataAsOfFrom)
	}
	if b.Late || b.DeadlineSeconds != 86400 {
		t.Fatalf("late=%v deadline=%d, want on time against 1 day", b.Late, b.DeadlineSeconds)
	}
	want := "The model's last run was stopped by a data check; these rows are from 2026-10-05 11:30 UTC."
	if len(b.Warnings) != 1 || b.Warnings[0] != want {
		t.Fatalf("warnings = %q, want %q", b.Warnings, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestChartBadges_NoPromiseNoLateness(t *testing.T) {
	db, mock := badgeDB(t)
	mock.ExpectQuery(`FROM saved_query_runs\s+WHERE saved_query_id`).WithArgs(badgeModel).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "finished_at", "marks"}).
			AddRow("run-3", badgeNow.Add(-72*time.Hour), []byte("null")))
	mock.ExpectQuery(`FROM saved_query_check_results`).WithArgs("run-3").
		WillReturnRows(sqlmock.NewRows([]string{"check_name", "severity", "status"}))
	mock.ExpectQuery(`freshness_deadline_seconds IS NOT NULL`).WithArgs(badgeModel).
		WillReturnError(sql.ErrNoRows)

	b, err := loadChartBadges(context.Background(), db, badgeSource("failed", "relation does not exist"), badgeNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.Late || b.DeadlineSeconds != 0 {
		t.Fatalf("a model with no deadline is never late: %+v", b)
	}
	if len(b.Warnings) != 1 || !strings.HasPrefix(b.Warnings[0], "The model's last run failed; these rows are from ") {
		t.Fatalf("warnings = %q, want the failed-run warning alone", b.Warnings)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHumanSeconds(t *testing.T) {
	for in, want := range map[int]string{86400: "1 day", 172800: "2 days", 3600: "1 hour",
		5400: "90 minutes", 90: "90 seconds", 1: "1 second"} {
		if got := humanSeconds(in); got != want {
			t.Errorf("humanSeconds(%d) = %q, want %q", in, got, want)
		}
	}
}
