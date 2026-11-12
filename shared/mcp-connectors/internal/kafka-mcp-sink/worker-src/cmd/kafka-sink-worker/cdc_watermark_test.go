package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// The CDC watermark: a model or workflow downstream of a CDC pipeline learns how far
// each table landed from cdc_table_watermarks (api-gateway migration 137), which the
// sink upserts after every counter write. These pin what it records, that a replay
// does not move it, and that its failure never touches the counter result.

func TestBuildCDCWatermarks(t *testing.T) {
	const other = "33333333-3333-3333-3333-333333333333"
	cdc := func(pipeline, table string, ts int64) *SinkMessage {
		return &SinkMessage{PipelineID: pipeline, ExecutionID: pipeline, Table: table, StorageType: "cdc", SourceTS: ts}
	}
	labelled := cdc(ackFKPipelineID, "analytics.customers", 700)
	labelled.SourceTable = "public.customers"
	labelled.DestNamespace = "analytics"

	sms := []*SinkMessage{
		cdc(ackFKPipelineID, "public.orders", 1000),
		cdc(ackFKPipelineID, "public.orders", 3000),
		cdc(ackFKPipelineID, "public.orders", 9000), // a replay: not landed, must not count
		cdc(ackFKPipelineID, "public.orders", 2000), // older, landed late
		cdc(other, "public.orders", 4000),
		nil,
		cdc("pipe-1", "public.orders", 5000),     // not a UUID: would fail the ::uuid cast
		cdc(ackFKPipelineID, "", 5000),           // no table
		cdc(ackFKPipelineID, "app.app.users", 0), // no source time; the db.db.table label is de-duplicated
		labelled,
		cdc(ackFKPipelineID, "public.items", 8000), // beyond landed: no entry, skipped
	}
	landed := []bool{true, true, false, true, true, true, true, true, true, true}

	got := buildCDCWatermarks(sms, landed)
	want := []cdcWatermark{
		{pipelineID: ackFKPipelineID, table: "app.users", sourceTSMs: 0},
		{pipelineID: ackFKPipelineID, table: "public.customers", destination: "analytics.customers", sourceTSMs: 700},
		{pipelineID: ackFKPipelineID, table: "public.orders", sourceTSMs: 3000},
		{pipelineID: other, table: "public.orders", sourceTSMs: 4000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCDCWatermarks =\n %+v\nwant\n %+v", got, want)
	}

	if got := buildCDCWatermarks(sms, make([]bool, len(sms))); len(got) != 0 {
		t.Fatalf("nothing landed must build nothing, got %+v", got)
	}
}

func TestBuildCDCWatermarkUpsert(t *testing.T) {
	if q, args := buildCDCWatermarkUpsert(nil); q != "" || args != nil {
		t.Fatalf("no watermarks must build no statement, got %q %v", q, args)
	}
	q, args := buildCDCWatermarkUpsert([]cdcWatermark{
		{pipelineID: ackFKPipelineID, table: "public.orders", destination: "analytics.orders", sourceTSMs: 3000},
		{pipelineID: ackFKPipelineID, table: "public.users", sourceTSMs: 0},
	})
	for _, part := range []string{
		"($1::uuid,$2,NULLIF($3,''),$4::timestamptz,now()),($5::uuid,$6,NULLIF($7,''),$8::timestamptz,now())",
		"ON CONFLICT (pipeline_id, table_name) DO UPDATE SET",
		"source_time = GREATEST(cdc_table_watermarks.source_time, EXCLUDED.source_time)",
		"COALESCE(EXCLUDED.destination_qualified_name, cdc_table_watermarks.destination_qualified_name)",
		"landed_at = EXCLUDED.landed_at",
	} {
		if !strings.Contains(q, part) {
			t.Errorf("upsert is missing %q:\n%s", part, q)
		}
	}
	want := []interface{}{
		ackFKPipelineID, "public.orders", "analytics.orders", sql.NullTime{Time: time.UnixMilli(3000).UTC(), Valid: true},
		ackFKPipelineID, "public.users", "", sql.NullTime{},
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n %#v\nwant\n %#v", args, want)
	}
}

// wmMessages is a flush of n messages on public.orders with source times 1000·(i+1).
func wmMessages(t *testing.T, n int) ([]*SinkMessage, []kafka.Message) {
	t.Helper()
	sms, msgs := ackFKMessages(t, n)
	for i, sm := range sms {
		sm.SourceTS = int64(1000 * (i + 1))
	}
	return sms, msgs
}

// wantWatermark asserts one upsert's args: a single row for public.orders at ts ms.
func wantWatermark(t *testing.T, args []driver.NamedValue, pipelineID string, ts int64) {
	t.Helper()
	if len(args) != cdcWatermarkCols {
		t.Fatalf("expected one watermark row, got args %v", args)
	}
	if args[0].Value != pipelineID || args[1].Value != "public.orders" {
		t.Fatalf("watermark keyed (%v, %v), want (%s, public.orders)", args[0].Value, args[1].Value, pipelineID)
	}
	if got, ok := args[3].Value.(time.Time); !ok || !got.Equal(time.UnixMilli(ts)) {
		t.Fatalf("watermark source_time = %v, want %v", args[3].Value, time.UnixMilli(ts).UTC())
	}
}

// § 13: a model run records its watermark — which needs the sink to record one. A
// flush records its newest landed source time; replaying the same offsets (a restart
// before the Kafka commit) records nothing, so landed_at does not move for data the
// destination already held.
func TestPersistCDCCounts_RecordsTheWatermarkOfNewlyLandedRows(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()
	floors := newCDCFloorTracker()

	sms, msgs := wmMessages(t, 3)
	for i := range sms {
		markCDCReplay(floors, sms[i], msgs[i])
	}
	if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err != nil {
		t.Fatalf("persistCDCCounts: %v", err)
	}
	ups := conn.watermarkUpserts()
	if len(ups) != 1 {
		t.Fatalf("a flush must record one watermark upsert, got %d", len(ups))
	}
	wantWatermark(t, ups[0], ackFKPipelineID, 3000)
	queries := conn.recorded()
	if !strings.Contains(queries[len(queries)-1], "cdc_table_watermarks") {
		t.Fatalf("the watermark must be written after the counters, got order %v", queries)
	}

	// The same offsets consumed again: every one is at or below its floor.
	for i := range sms {
		markCDCReplay(floors, sms[i], msgs[i])
	}
	if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(conn.watermarkUpserts()); got != 1 {
		t.Fatalf("a replayed flush moved the watermark (%d upserts, want 1)", got)
	}
}

// The watermark rides on "the rows landed", not on "the counter write worked": a
// failed counter write still counts every row, so it still records the watermark. A
// failed watermark write (no migration 137) changes nothing about the counter result.
func TestPersistCDCCounts_WatermarkIsIndependentOfTheCounterResult(t *testing.T) {
	t.Run("counter write fails", func(t *testing.T) {
		conn := &ackFKConn{failCounts: io.ErrUnexpectedEOF}
		db := newAckFKDB(t, conn)
		defer db.Close()
		sms, msgs := wmMessages(t, 2)
		if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err == nil {
			t.Fatal("the counter error must still be returned")
		}
		ups := conn.watermarkUpserts()
		if len(ups) != 1 {
			t.Fatalf("rows that landed must record a watermark even when the counter write failed, got %d upserts", len(ups))
		}
		wantWatermark(t, ups[0], ackFKPipelineID, 2000)
	})

	t.Run("watermark write fails", func(t *testing.T) {
		conn := &ackFKConn{failWatermarks: errors.New(`relation "cdc_table_watermarks" does not exist`)}
		db := newAckFKDB(t, conn)
		defer db.Close()
		sms, msgs := wmMessages(t, 2)
		counted, err := persistCDCCounts(context.Background(), db, sms, msgs)
		if err != nil {
			t.Fatalf("a failed watermark must not fail the counter write: %v", err)
		}
		if !equalBools(counted, []bool{true, true}) {
			t.Fatalf("counted = %v, want both", counted)
		}
		if got := conn.storedTotal("public.orders", "c"); got != 2 {
			t.Fatalf("stored total = %d, want 2", got)
		}
	})
}

// The single-row lane (deletes, append-only mode, per-row recovery) records the
// watermark the same way, and a replay does not move it.
func TestPersistCDCCount_RecordsTheWatermark(t *testing.T) {
	const pipelineID = "44444444-4444-4444-4444-444444444444"
	cdcAuditExecutionEnsured.Delete(pipelineID + "|" + pipelineID)
	t.Cleanup(func() { cdcAuditExecutionEnsured.Delete(pipelineID + "|" + pipelineID) })

	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()
	floors := newCDCFloorTracker()
	newMessage := func() *SinkMessage {
		return &SinkMessage{PipelineID: pipelineID, ExecutionID: pipelineID, Table: "public.orders", StorageType: "cdc", RowCount: 1, CDCOp: "d", SourceTS: 6000}
	}

	for _, s := range []struct {
		offset      int64
		wantCounted bool
		wantWrites  int
	}{
		{offset: 10, wantCounted: true, wantWrites: 1},
		{offset: 10, wantCounted: false, wantWrites: 1}, // replay
		{offset: 11, wantCounted: true, wantWrites: 2},
	} {
		sm := newMessage()
		msg := kafka.Message{Topic: "cdc.public.orders", Offset: s.offset}
		markCDCReplay(floors, sm, msg)
		counted, err := persistCDCCount(context.Background(), db, sm, msg)
		if err != nil || counted != s.wantCounted {
			t.Fatalf("offset %d: counted=%v err=%v, want counted=%v", s.offset, counted, err, s.wantCounted)
		}
		if got := len(conn.watermarkUpserts()); got != s.wantWrites {
			t.Fatalf("offset %d: %d watermark upserts, want %d", s.offset, got, s.wantWrites)
		}
	}
	wantWatermark(t, conn.watermarkUpserts()[0], pipelineID, 6000)

	t.Run("failed counter write still records it and still returns the error", func(t *testing.T) {
		failing := &ackFKConn{failCounts: io.ErrUnexpectedEOF}
		fdb := newAckFKDB(t, failing)
		defer fdb.Close()
		if _, err := persistCDCCount(context.Background(), fdb, newMessage(), kafka.Message{Topic: "cdc.public.orders", Offset: 12}); err == nil {
			t.Fatal("the counter error must still be returned")
		}
		if got := len(failing.watermarkUpserts()); got != 1 {
			t.Fatalf("%d watermark upserts after a failed counter write, want 1", got)
		}
	})
}
