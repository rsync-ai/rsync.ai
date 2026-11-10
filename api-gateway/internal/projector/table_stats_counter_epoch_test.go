package projector

// The Usage page showed 0.04 GB of data transfer this month on prod (2026-10-03)
// while a CDC pipeline kept streaming. kafka-mcp-sink keeps counts.bytes_committed
// and counts.dlq_rows as running totals in memory, so a restarted sink reports
// them from zero again. The projector kept the larger of the stored and reported
// total and charged only the amount above the stored one, so it charged nothing
// after a restart until the new process passed the old total, which on the CDC
// lane includes the whole initial snapshot.
//
// Bug class: a per-process running total compared against a durable high-water
// mark. The sink now sends counter_epoch_ms, the time its totals started at zero;
// these tests pin how the projector turns (epoch, raw total) into what it stores
// and charges: a restart, the same process, a replayed event, an event from
// before a restart, and the first event after migration 134.

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// storedRow is what pipeline_run_table_stats holds before the event.
type storedRow struct {
	bytes, dlq                  int64
	epoch, epochBytes, epochDLQ interface{} // int64, or nil for NULL
}

// epochCase is one event through the real upsert, and what it must store and charge.
type epochCase struct {
	name      string
	stored    storedRow
	epoch     int64 // 0: the event carries no counter_epoch_ms
	bytes     int64
	dlq       int64
	wantBytes int64
	wantDLQ   int64
	charge    int64 // 0: no ledger row
	// The epoch state written back, or nil when nothing is written.
	wantEpochState []driver.Value
}

func runEpochCase(t *testing.T, c epochCase) {
	t.Helper()
	meta := fmt.Sprintf(`"source": "kafka_mcp_sink", "mode": "cdc", "status": "running",
		"table": {"schema": "public", "name": "orders", "qualified_name": "public.orders"},
		"counts": {"inserts": 10, "total_events": 10, "bytes_committed": %d, "dlq_rows": %d}`, c.bytes, c.dlq)
	if c.epoch > 0 {
		meta += fmt.Sprintf(`, "counter_epoch_ms": %d`, c.epoch)
	}
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"event_type": "TABLE_STATS",
		"pipeline_id": "`+testStatsPipeline+`",
		"execution_id": "`+testStatsPipeline+`",
		"metadata": {`+meta+`}
	}`), &ev); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	bytesArg, dlqArg := &capturedArg{}, &capturedArg{}
	args := make([]driver.Value, 0, 31)
	for i := 1; i <= 31; i++ {
		switch i {
		case 24:
			args = append(args, bytesArg)
		case 25:
			args = append(args, dlqArg)
		default:
			args = append(args, sqlmock.AnyArg())
		}
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(bytes_committed, 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes_committed"}).AddRow(c.stored.bytes))
	if c.epoch > 0 {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT counter_epoch_ms, epoch_bytes_committed, epoch_dlq_rows, COALESCE(dlq_rows, 0)")).
			WillReturnRows(sqlmock.NewRows([]string{"counter_epoch_ms", "epoch_bytes_committed", "epoch_dlq_rows", "dlq_rows"}).
				AddRow(c.stored.epoch, c.stored.epochBytes, c.stored.epochDLQ, c.stored.dlq))
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO pipeline_run_table_stats")).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if c.wantEpochState != nil {
		mock.ExpectExec(regexp.QuoteMeta("UPDATE pipeline_run_table_stats SET counter_epoch_ms")).
			WithArgs(append([]driver.Value{testStatsPipeline, testStatsPipeline, "public.orders"}, c.wantEpochState...)...).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	if c.charge > 0 {
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_transfer_usage_events")).
			WithArgs(testStatsPipeline, testStatsPipeline, "public.orders", "cdc", c.charge).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	p := &EventProjector{db: db, lastSeq: map[string]int64{}, gapSeen: map[string]bool{}}
	if err := p.upsertTableStats(ev); err != nil {
		t.Fatalf("upsertTableStats: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if bytesArg.got != c.wantBytes {
		t.Errorf("bytes_committed bound as %#v, want %d", bytesArg.got, c.wantBytes)
	}
	if dlqArg.got != c.wantDLQ {
		t.Errorf("dlq_rows bound as %#v, want %d", dlqArg.got, c.wantDLQ)
	}
}

func TestTableStatsCounterEpoch(t *testing.T) {
	for _, c := range []epochCase{
		{
			// The bug: the restarted sink reports 300 bytes and 2 DLQ rows from zero.
			// Before the fix 300 < 1000 charged nothing and stored nothing new.
			name:   "a restarted sink's totals are added and charged",
			stored: storedRow{bytes: 1000, dlq: 5, epoch: int64(100), epochBytes: int64(1000), epochDLQ: int64(5)},
			epoch:  200, bytes: 300, dlq: 2,
			wantBytes: 1300, wantDLQ: 7, charge: 300,
			wantEpochState: []driver.Value{int64(200), int64(300), int64(2)},
		},
		{
			name:   "the same process charges only its increase",
			stored: storedRow{bytes: 1300, dlq: 7, epoch: int64(200), epochBytes: int64(300), epochDLQ: int64(2)},
			epoch:  200, bytes: 450, dlq: 2,
			wantBytes: 1450, wantDLQ: 7, charge: 150,
			wantEpochState: []driver.Value{int64(200), int64(450), int64(2)},
		},
		{
			// Kafka redelivers after a projector restart: at-least-once.
			name:   "a replayed event adds nothing",
			stored: storedRow{bytes: 1450, dlq: 7, epoch: int64(200), epochBytes: int64(450), epochDLQ: int64(2)},
			epoch:  200, bytes: 450, dlq: 2,
			wantBytes: 1450, wantDLQ: 7,
			wantEpochState: []driver.Value{int64(200), int64(450), int64(2)},
		},
		{
			// A replay from before the restart must not count as yet another restart.
			name:   "an event from an older process adds nothing and keeps the epoch",
			stored: storedRow{bytes: 1450, dlq: 7, epoch: int64(200), epochBytes: int64(450), epochDLQ: int64(2)},
			epoch:  100, bytes: 900, dlq: 5,
			wantBytes: 1450, wantDLQ: 7,
		},
		{
			// Rows from before migration 134 have no epoch; the sink that sends the first
			// one started after the deploy, at zero.
			name:   "the first event after the upgrade counts as a restart",
			stored: storedRow{bytes: 5000, dlq: 0, epoch: nil, epochBytes: nil, epochDLQ: nil},
			epoch:  200, bytes: 40, dlq: 0,
			wantBytes: 5040, wantDLQ: 0, charge: 40,
			wantEpochState: []driver.Value{int64(200), int64(40), int64(0)},
		},
		{
			// A table's first event: nothing stored yet.
			name:   "a new row stores and charges the reported total",
			stored: storedRow{epoch: nil, epochBytes: nil, epochDLQ: nil},
			epoch:  200, bytes: 64, dlq: 0,
			wantBytes: 64, wantDLQ: 0, charge: 64,
			wantEpochState: []driver.Value{int64(200), int64(64), int64(0)},
		},
		{
			// A sink older than this change sends no epoch: the old rule, unchanged.
			name:   "no epoch keeps the larger total",
			stored: storedRow{bytes: 1000, dlq: 5},
			bytes:  1200, dlq: 5,
			wantBytes: 1200, wantDLQ: 5, charge: 200,
		},
	} {
		t.Run(c.name, func(t *testing.T) { runEpochCase(t, c) })
	}
}

// A table that has stats but no stored row (deleted between the two reads) reads
// as a restart, not an error.
func TestEpochDeltasNoStoredRow(t *testing.T) {
	got := epochDeltas(storedEpoch{}, 200, sql.NullInt64{Int64: 10, Valid: true}, sql.NullInt64{})
	if got.bytesDelta != 10 || got.dlqDelta != 0 || !got.record {
		t.Fatalf("epochDeltas = %+v, want bytes +10, dlq +0, recorded", got)
	}
	if got.dlqRaw.Valid {
		t.Fatalf("dlq raw stored as %+v though the event sent none", got.dlqRaw)
	}
}
