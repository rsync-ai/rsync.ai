package projector

// pipeline_run_table_stats.files_written read 0 on every batch run to GCS on prod
// (2026-09-30) although each run wrote a Parquet object: no producer sent the count.
// The kafka-mcp-sink now sends metadata.counts.files_written for an object-storage
// destination (kafka-sink-worker table_stats_files_written_test.go); this is the
// projector half, pinning that the value it sends is the one bound as $11.

import (
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// projectFilesWritten runs one batch TABLE_STATS event, decoded from the JSON the sink
// sends, through the real upsert and returns the files_written argument it bound.
func projectFilesWritten(t *testing.T, counts string) driver.Value {
	t.Helper()
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"event_type": "TABLE_STATS",
		"pipeline_id": "`+testStatsPipeline+`",
		"execution_id": "`+testStatsExec+`",
		"metadata": {
			"source": "kafka_mcp_sink", "mode": "batch", "status": "completed",
			"table": {"schema": "datingapp", "name": "matches", "qualified_name": "datingapp.matches"},
			"counts": `+counts+`
		}
	}`), &ev); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	files := &capturedArg{}
	args := make([]driver.Value, 0, 31)
	for i := 1; i <= 31; i++ {
		if i == 11 {
			args = append(args, files)
			continue
		}
		args = append(args, sqlmock.AnyArg())
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(bytes_committed, 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes_committed"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta("read_rows, inserted_rows, bytes_read, files_written,")).
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
	if !files.seen {
		t.Fatal("files_written was never bound")
	}
	return files.got
}

func TestUpsertTableStatsStoresTheFilesTheSinkWrote(t *testing.T) {
	got := projectFilesWritten(t, `{"read_rows": 4000, "inserted_rows": 4000, "files_written": 1}`)
	if got != int64(1) {
		t.Fatalf("files_written bound as %#v, want 1", got)
	}
}

// A table destination sends no count; NULL leaves any stored value to the
// GREATEST() in the ON CONFLICT clause.
func TestUpsertTableStatsSendsNullFilesWhenTheSinkSentNone(t *testing.T) {
	if got := projectFilesWritten(t, `{"read_rows": 5, "inserted_rows": 5}`); got != nil {
		t.Fatalf("files_written bound as %#v, want NULL", got)
	}
}
