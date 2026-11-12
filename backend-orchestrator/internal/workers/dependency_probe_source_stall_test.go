package workers

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Second instance of the KI-CDC-DROPPED-SOURCE-TABLE-REPORTS-HEALTHY class: a probe
// that asks whether the process is up, about a stream that is up and capturing
// nothing. Seen on prod 2026-10-22: a MongoDB connector whose resume token had left
// the oplog stayed RUNNING; the Sentinel's freshness alarm was open and the bell said
// "gone quiet", while the Monitor tab read "Streaming · caught up" from this probe.
//
// RED against the pre-fix source: the healthy path returned straight after the
// dropped-table lookup, so the first case reads healthy and the stall expectation
// goes unmet.

func expectNoDroppedTables(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM cdc_source_table_drops`).
		WithArgs(droppedTablePipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"table_name"}))
}

func expectStallLookup(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`FROM sentinel_active_issues WHERE id = \$1`).
		WithArgs("cdc-source-stalled-" + droppedTablePipelineID)
}

func TestDebeziumTaskProbeSourceStall(t *testing.T) {
	t.Run("an open stall issue degrades a RUNNING connector", func(t *testing.T) {
		p, mock := droppedTableProbe(t, connectStatusRunning)
		expectNoDroppedTables(mock)
		expectStallLookup(mock).WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

		status, lastErr, details := p.probeOne(context.Background(), droppedTablePipelineID,
			"debezium_task", "cdc-abd8a64d", nil)

		// degraded, never unhealthy: unhealthy reads as phase "failed" and turns the
		// list badge red, and a stalled stream has not failed.
		if status != "degraded" {
			t.Fatalf("status = %q, want degraded while the Sentinel says the source is stalled", status)
		}
		if lastErr != "source stream stalled: committed position not advancing" {
			t.Errorf("last_error = %q", lastErr)
		}
		if details["source_stream_stalled"] != true {
			t.Errorf("details = %#v, want source_stream_stalled=true", details)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("the probe never asked whether the stream is stalled: %v", err)
		}
	})

	t.Run("no open stall issue stays healthy", func(t *testing.T) {
		// Resolving the alarm deletes the row, so the degrade clears on the next tick.
		p, mock := droppedTableProbe(t, connectStatusRunning)
		expectNoDroppedTables(mock)
		expectStallLookup(mock).WillReturnError(sql.ErrNoRows)

		status, lastErr, details := p.probeOne(context.Background(), droppedTablePipelineID,
			"debezium_task", "cdc-abd8a64d", nil)

		if status != "healthy" || lastErr != "" {
			t.Fatalf("status = %q, last_error = %q; want healthy", status, lastErr)
		}
		if _, present := details["source_stream_stalled"]; present {
			t.Errorf("details advertise a stall that is not open: %#v", details)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("a failed lookup keeps the healthy verdict", func(t *testing.T) {
		p, mock := droppedTableProbe(t, connectStatusRunning)
		expectNoDroppedTables(mock)
		expectStallLookup(mock).WillReturnError(errors.New("pq: connection reset"))

		status, lastErr, _ := p.probeOne(context.Background(), droppedTablePipelineID,
			"debezium_task", "cdc-abd8a64d", nil)

		if status != "healthy" || lastErr != "" {
			t.Fatalf("status = %q, last_error = %q; want fail-soft healthy", status, lastErr)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("a dropped table keeps priority and skips the stall lookup", func(t *testing.T) {
		// No stall expectation registered: sqlmock fails an unexpected query, and the
		// dropped-table reason must reach the Monitor tab unchanged.
		p, mock := droppedTableProbe(t, connectStatusRunning)
		mock.ExpectQuery(`FROM cdc_source_table_drops`).
			WithArgs(droppedTablePipelineID).
			WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("cdc_drift"))

		status, lastErr, _ := p.probeOne(context.Background(), droppedTablePipelineID,
			"debezium_task", "cdc-abd8a64d", nil)

		if status != "degraded" || lastErr != "source table(s) dropped at origin: cdc_drift" {
			t.Fatalf("status = %q, last_error = %q; want the dropped-table verdict", status, lastErr)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}
