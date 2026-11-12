package main

// Coverage for restart-safe per-table CDC counters (cdc_counter_ledger.go).
//
// The bug: the per-table insert/update/delete totals lived only in process memory
// and the projector keeps GREATEST(stored, reported), so a sink restart threw away
// every row counted before it (a 5,000-row table showed 1,773) and a replay after
// the restart counted the same offsets twice. The counters were then kept in
// pipeline_batch_acks, one row per CDC event, which nothing trimmed
// (KI-CDC-ACK-LEDGER-UNBOUNDED). They now live in cdc_counter_totals (a running
// total per table and op) and cdc_offset_floors (the highest landed offset per
// partition): seeded from both at start, and a message at or below its floor,
// decided when it is consumed, is not counted again.
//
// Uses ackFKConn (ack_ledger_fk_guard_test.go), which applies the two upserts the
// way PostgreSQL does and answers the seed reads.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// ledgerTestMessages builds CDC messages for offsets [from, to) on public.orders, with
// the same partition rule as ackFKMessages so a replayed offset has the same key.
func ledgerTestMessages(from, to int, op string) ([]*SinkMessage, []kafka.Message) {
	var sms []*SinkMessage
	var msgs []kafka.Message
	for i := from; i < to; i++ {
		sms = append(sms, &SinkMessage{
			PipelineID:  ackFKPipelineID,
			ExecutionID: ackFKExecID,
			Table:       "public.orders",
			StorageType: "cdc",
			RowCount:    1,
			CDCOp:       op,
		})
		msgs = append(msgs, kafka.Message{Topic: "cdc.public.orders", Partition: i % 3, Offset: int64(i)})
	}
	return sms, msgs
}

// okDestination answers every destination tool call with success.
type okDestination struct{}

func (okDestination) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"success":true}}`)),
		Request:    r,
	}, nil
}

// cdcTestCounters are one sink process's per-table counters and offset floors.
// Snapshot reads live on its Metrics (cdcSnapshotRowsByTable), so the process's
// Metrics travels with them.
type cdcTestCounters struct {
	inserts, updates, deletes, snapshotRows *sync.Map
	metrics                                 *Metrics
	floors                                  *cdcFloorTracker
}

func newCDCTestCounters() cdcTestCounters {
	m := &Metrics{}
	return cdcTestCounters{inserts: &sync.Map{}, updates: &sync.Map{}, deletes: &sync.Map{},
		snapshotRows: &m.cdcSnapshotRowsByTable, metrics: m, floors: newCDCFloorTracker()}
}

// consume marks each message the way the main loop does when it reads it.
func (c cdcTestCounters) consume(sms []*SinkMessage, msgs []kafka.Message) {
	for i := range sms {
		markCDCReplay(c.floors, sms[i], msgs[i])
	}
}

// flushObjectBatch runs one successful cdcObjectBatcher.flushBatch — the object
// storage lane — against pgDB (nil: no database) and the given counters.
func flushObjectBatch(t *testing.T, pgDB *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
	t.Helper()
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "cdc.public.orders"})
	defer reader.Close()
	b := &cdcObjectBatcher{
		cfg:          &WorkerConfig{PipelineID: ackFKPipelineID, SinkMode: "cdc"},
		destType:     "gcs",
		destCfg:      map[string]interface{}{},
		params:       cdcBatchingParams{maxRetries: 0, backoff: time.Millisecond},
		gate:         newCommitGate(reader.CommitMessages, newHighWaterTracker()),
		pgDB:         pgDB,
		httpClient:   &http.Client{Transport: okDestination{}},
		eventsWriter: &kafka.Writer{}, // no address: the stats emit fails, the counters are still updated
		metrics:      c.metrics,
		cdcInserts:   c.inserts,
		cdcUpdates:   c.updates,
		cdcDeletes:   c.deletes,
		cdcBytes:     &sync.Map{},
		batches:      map[string]*cdcObjectBatch{},
	}
	events := make([]map[string]interface{}, len(sms))
	for i := range events {
		events[i] = map[string]interface{}{"id": i}
	}
	batch := &cdcObjectBatch{
		topic:       "cdc.public.orders",
		table:       "public.orders",
		format:      "jsonl",
		compression: "none",
		bucket:      "bucket",
		prefix:      "prefix",
		firstOffset: msgs[0].Offset,
		lastOffset:  msgs[len(msgs)-1].Offset,
		events:      events,
		messages:    msgs,
		sms:         sms,
	}
	const key = "test-batch"
	b.batches[key] = batch
	b.flushBatch(context.Background(), key, batch, "test_flush")
	if _, pending := b.batches[key]; pending {
		t.Fatal("flushBatch did not complete the batch; the counters under test were never reached")
	}
}

// dbDestination is a relational or document destination that reports how many rows
// each write carried, as flushBatch requires. With rejectMultiRow it refuses every
// multi-row write with a row-level error, which sends flushBatch into per-row
// recovery; singleRowWrites counts the one-row writes that recovery makes.
type dbDestination struct {
	rejectMultiRow  bool
	singleRowWrites *int32
}

func (d dbDestination) RoundTrip(r *http.Request) (*http.Response, error) {
	var req struct {
		Params struct {
			Arguments struct {
				Data []json.RawMessage `json:"data"`
			} `json:"arguments"`
		} `json:"params"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)
	n := len(req.Params.Arguments.Data)
	result := fmt.Sprintf(`{"success":true,"rows_upserted":%d}`, n)
	if d.rejectMultiRow && n > 1 {
		result = `{"success":false,"error":"integer out of range"}`
	}
	if n == 1 && d.singleRowWrites != nil {
		atomic.AddInt32(d.singleRowWrites, 1)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`)),
		Request:    r,
	}, nil
}

// flushDBBatch runs one cdcDBBatcher.flushBatch — the lane a PostgreSQL → MongoDB
// pipeline's inserts and updates take — against pgDB (nil: no database), the given
// counters and destination.
func flushDBBatch(t *testing.T, pgDB *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message, dest http.RoundTripper) {
	t.Helper()
	captureFailClosed(t) // a fail-closed exit panics instead of ending the test binary
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "cdc.public.orders"})
	defer reader.Close()
	b := &cdcDBBatcher{
		cfg:          &WorkerConfig{PipelineID: ackFKPipelineID, SinkMode: "cdc"},
		destType:     "mongodb",
		destCfg:      map[string]interface{}{},
		params:       cdcBatchingParams{maxRetries: 0, backoff: time.Millisecond},
		gate:         newCommitGate(reader.CommitMessages, newHighWaterTracker()),
		pgDB:         pgDB,
		httpClient:   &http.Client{Transport: dest},
		ddl:          &DDLSupport{},
		eventsWriter: &kafka.Writer{},
		metrics:      c.metrics,
		cdcInserts:   c.inserts,
		cdcUpdates:   c.updates,
		cdcDeletes:   c.deletes,
		cdcBytes:     &sync.Map{},
		batches:      map[string]*cdcDBBatch{},
	}
	rows := make([]map[string]interface{}, len(sms))
	for i := range rows {
		rows[i] = map[string]interface{}{"id": msgs[i].Offset}
	}
	batch := &cdcDBBatch{
		table:       "public.orders",
		targetTable: "orders",
		topic:       "cdc.public.orders",
		rows:        rows,
		messages:    msgs,
		sms:         sms,
		keyFields:   []string{"id"},
		firstOffset: msgs[0].Offset,
		lastOffset:  msgs[len(msgs)-1].Offset,
	}
	const key = "test-batch"
	b.batches[key] = batch
	// submitFlush, not flushBatch: sealing the batch (removing it from b.batches) is
	// submitFlush's job, so this is the production entry point for the DB batcher.
	// With no lanes configured it seals and then flushes inline, exactly as before.
	// (flushObjectBatch above stays on flushBatch: the object batcher has no lanes.)
	b.submitFlush(context.Background(), key, batch, "test_flush")
	if _, pending := b.batches[key]; pending {
		t.Fatal("submitFlush did not complete the batch; the counters under test were never reached")
	}
}

// deliverDeletes applies each message through processCDCEvent, the lane every CDC
// delete takes on a relational or document destination.
func deliverDeletes(t *testing.T, pgDB *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
	t.Helper()
	cfg := &WorkerConfig{PipelineID: ackFKPipelineID, SinkMode: "cdc", DestinationConnector: "mongodb", DestinationConfig: map[string]interface{}{}}
	client := &http.Client{Transport: dbDestination{}}
	for i, sm := range sms {
		sm.PK = map[string]interface{}{"id": msgs[i].Offset}
		sm.KeyFields = []string{"id"}
		commit, err := processCDCEvent(context.Background(), newCommitGate(nil, newHighWaterTracker()), pgDB, client, cfg,
			&DDLSupport{Enabled: false, resolved: true}, &kafka.Writer{}, nil, msgs[i], sm, c.metrics,
			c.inserts, c.updates, c.deletes, &sync.Map{}, time.Minute)
		if err != nil || !commit {
			t.Fatalf("delete at offset %d was not applied (commit=%v): %v", msgs[i].Offset, commit, err)
		}
	}
}

// storedAfterRestartAndReplay is the reported failure end to end: a sink delivers
// offsets 0-4, restarts before committing 2-4, is handed 2-4 again followed by new
// offsets 5-6, and the projector stores the larger of the two processes' reports.
// Seven distinct rows were delivered, so a correct counter stores 7. Each process
// consumes (marks) its messages before delivering them, as main() does.
func storedAfterRestartAndReplay(t *testing.T, withDB bool, op string, counter func(cdcTestCounters) *sync.Map,
	deliver func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message)) int64 {
	t.Helper()
	var db *sql.DB
	if withDB {
		db = newAckFKDB(t, &ackFKConn{})
		defer db.Close()
	}

	p1 := newCDCTestCounters()
	sms, msgs := ledgerTestMessages(0, 5, op)
	p1.consume(sms, msgs)
	deliver(t, db, p1, sms, msgs)
	if got := loadCounter(counter(p1), "public.orders"); got != 5 {
		t.Fatalf("process 1 counted %d, want 5", got)
	}

	// Restart: fresh counters and floors, seeded the way main() seeds them.
	p2 := newCDCTestCounters()
	if withDB {
		seedCDCCounters(context.Background(), db, ackFKPipelineID, ackFKExecID, p2.floors, p2.inserts, p2.updates, p2.deletes, p2.snapshotRows)
	}
	sms, msgs = ledgerTestMessages(2, 7, op)
	p2.consume(sms, msgs)
	deliver(t, db, p2, sms, msgs)

	p1Total, p2Total := loadCounter(counter(p1), "public.orders"), loadCounter(counter(p2), "public.orders")
	if p1Total > p2Total {
		return p1Total
	}
	return p2Total
}

// assertRestartAndReplayCountsDistinctRows checks the scenario with the stored
// counters, and runs it without a database as a control: that is the old behaviour,
// and if it also reached 7 the scenario would not reproduce the bug and the first
// check proves nothing.
func assertRestartAndReplayCountsDistinctRows(t *testing.T, op string, counter func(cdcTestCounters) *sync.Map,
	deliver func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message)) {
	t.Helper()
	if got := storedAfterRestartAndReplay(t, true, op, counter, deliver); got != 7 {
		t.Fatalf("stored count after restart + replay = %d, want 7 (distinct rows delivered)", got)
	}
	if got := storedAfterRestartAndReplay(t, false, op, counter, deliver); got == 7 {
		t.Fatalf("control without a database also reached 7; the scenario does not reproduce the bug")
	}
}

func insertCounter(c cdcTestCounters) *sync.Map   { return c.inserts }
func updateCounter(c cdcTestCounters) *sync.Map   { return c.updates }
func deleteCounter(c cdcTestCounters) *sync.Map   { return c.deletes }
func snapshotCounter(c cdcTestCounters) *sync.Map { return c.snapshotRows }

// Object storage destinations (GCS and the other object stores).
func TestCDCCounters_SurviveRestartAndReplay(t *testing.T) {
	for _, tc := range []struct {
		op      string
		counter func(cdcTestCounters) *sync.Map
	}{{"c", insertCounter}, {"r", snapshotCounter}} {
		t.Run(tc.op, func(t *testing.T) {
			assertRestartAndReplayCountsDistinctRows(t, tc.op, tc.counter,
				func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
					flushObjectBatch(t, db, c, sms, msgs)
				})
		})
	}
}

// Relational and document destinations: inserts and updates through the DB batcher.
func TestCDCCounters_DBBatcherSurvivesRestartAndReplay(t *testing.T) {
	for _, tc := range []struct {
		op      string
		counter func(cdcTestCounters) *sync.Map
	}{{"c", insertCounter}, {"r", snapshotCounter}, {"u", updateCounter}} {
		t.Run(tc.op, func(t *testing.T) {
			assertRestartAndReplayCountsDistinctRows(t, tc.op, tc.counter,
				func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
					flushDBBatch(t, db, c, sms, msgs, dbDestination{})
				})
		})
	}
}

// The same batches when the destination refuses the whole-batch write and every row
// is written on its own by per-row recovery.
func TestCDCCounters_DBBatcherPerRowRecoverySurvivesRestartAndReplay(t *testing.T) {
	var singleRowWrites int32
	assertRestartAndReplayCountsDistinctRows(t, "c", insertCounter,
		func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
			flushDBBatch(t, db, c, sms, msgs, dbDestination{rejectMultiRow: true, singleRowWrites: &singleRowWrites})
		})
	if singleRowWrites == 0 {
		t.Fatal("no single-row writes: per-row recovery never ran, so this test covered the whole-batch path again")
	}
}

// Deletes, which bypass the DB batcher.
func TestCDCCounters_DeletesSurviveRestartAndReplay(t *testing.T) {
	assertRestartAndReplayCountsDistinctRows(t, "d", deleteCounter, deliverDeletes)
}

// The bug class of KI-CDC-ACK-LEDGER-UNBOUNDED: a CDC flush writing a database row
// per change event. No lane may write pipeline_batch_acks for CDC: the object
// storage batcher, the DB batcher, its per-row recovery, and the delete lane.
func TestCDCFlush_WritesNoPipelineBatchAcksRows(t *testing.T) {
	lanes := []struct {
		name    string
		deliver func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message)
	}{
		{"object storage", flushObjectBatch},
		{"db batcher", func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
			flushDBBatch(t, db, c, sms, msgs, dbDestination{})
		}},
		{"db batcher per-row recovery", func(t *testing.T, db *sql.DB, c cdcTestCounters, sms []*SinkMessage, msgs []kafka.Message) {
			flushDBBatch(t, db, c, sms, msgs, dbDestination{rejectMultiRow: true})
		}},
		{"deletes", deliverDeletes},
	}
	for _, lane := range lanes {
		t.Run(lane.name, func(t *testing.T) {
			conn := &ackFKConn{}
			db := newAckFKDB(t, conn)
			defer db.Close()
			sms, msgs := ledgerTestMessages(0, 30, "c")
			if lane.name == "deletes" {
				sms, msgs = ledgerTestMessages(0, 30, "d")
			}
			lane.deliver(t, db, newCDCTestCounters(), sms, msgs)

			queries := conn.recorded()
			if len(queries) == 0 {
				t.Fatal("the lane issued no query at all; it never reached the counter write")
			}
			if got := countMatching(queries, "INSERT INTO pipeline_batch_acks"); got != 0 {
				t.Fatalf("a CDC flush of 30 events wrote pipeline_batch_acks %d time(s); CDC must keep totals, not a row per event", got)
			}
		})
	}
}

// The rows a flush leaves behind scale with the tables and partitions it touched,
// not with its events: 3,000 inserts over three partitions are one total and three
// floors, written in one statement.
func TestCDCFlush_StoresOneRowPerKeyNotPerEvent(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()
	c := newCDCTestCounters()
	sms, msgs := ledgerTestMessages(0, 3000, "c")
	c.consume(sms, msgs)
	flushObjectBatch(t, db, c, sms, msgs)

	if got := countMatching(conn.recorded(), "INSERT INTO cdc_offset_floors"); got != 1 {
		t.Fatalf("%d counter statements for one flush, want 1", got)
	}
	if len(conn.totals) != 1 || conn.storedTotal("public.orders", "c") != 3000 {
		t.Fatalf("totals = %v, want one row public.orders/c = 3000", conn.totals)
	}
	if len(conn.floors) != 3 {
		t.Fatalf("floors = %v, want one row per partition", conn.floors)
	}
	for p, want := range map[int64]int64{0: 2997, 1: 2998, 2: 2999} {
		if got, _ := conn.storedFloor("public.orders", "cdc.public.orders", p); got != want {
			t.Errorf("partition %d floor = %d, want %d", p, got, want)
		}
	}
}

// The object batcher can flush a partition's later batch before its earlier one.
// The replay decision is made at consume time, so both batches count; a decision
// made at flush time would take the earlier batch for a replay of the later one.
// After a restart, the same six offsets are all replays.
func TestCDCCounters_OutOfOrderFlushesCountEveryRowOnce(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()
	partition0 := func(from, to int) ([]*SinkMessage, []kafka.Message) {
		sms, msgs := ledgerTestMessages(from, to, "c")
		for i := range msgs {
			msgs[i].Partition = 0
		}
		return sms, msgs
	}

	p1 := newCDCTestCounters()
	aSMs, aMsgs := partition0(0, 3)
	bSMs, bMsgs := partition0(3, 6)
	p1.consume(aSMs, aMsgs)
	p1.consume(bSMs, bMsgs)
	for _, flush := range []struct {
		sms  []*SinkMessage
		msgs []kafka.Message
	}{{bSMs, bMsgs}, {aSMs, aMsgs}} { // B lands first
		counted, err := persistCDCCounts(context.Background(), db, flush.sms, flush.msgs)
		if err != nil || !equalBools(counted, []bool{true, true, true}) {
			t.Fatalf("flush of offsets %d-%d: counted=%v err=%v, want all counted", flush.msgs[0].Offset, flush.msgs[2].Offset, counted, err)
		}
	}
	if got := conn.storedTotal("public.orders", "c"); got != 6 {
		t.Fatalf("stored total = %d, want 6", got)
	}

	// Control: deciding in flush order (B, then A) drops A. If it did not, this test
	// would not cover the out-of-order hazard.
	flushOrder := newCDCTestCounters()
	flushOrder.consume(bSMs, bMsgs)
	flushOrder.consume(aSMs, aMsgs)
	if got := cdcCountsFor(aSMs); equalBools(got, []bool{true, true, true}) {
		t.Fatal("control: a flush-order decision also counted the earlier batch; the scenario does not reproduce the hazard")
	}

	// Restart and replay all six.
	p2 := newCDCTestCounters()
	seedCDCCounters(context.Background(), db, ackFKPipelineID, ackFKExecID, p2.floors, p2.inserts, p2.updates, p2.deletes, p2.snapshotRows)
	sms, msgs := partition0(0, 6)
	p2.consume(sms, msgs)
	counted, err := persistCDCCounts(context.Background(), db, sms, msgs)
	if err != nil || !equalBools(counted, make([]bool, 6)) {
		t.Fatalf("replay after restart: counted=%v err=%v, want none counted", counted, err)
	}
	if got := conn.storedTotal("public.orders", "c"); got != 6 {
		t.Fatalf("stored total after the replay = %d, want 6", got)
	}
	if got := loadCounter(p2.inserts, "public.orders"); got != 6 {
		t.Fatalf("the restarted process reports %d, want the seeded 6", got)
	}
}

func TestMarkCDCReplay(t *testing.T) {
	f := newCDCFloorTracker()
	sm := &SinkMessage{Table: "public.orders"}
	at := func(partition int, offset int64) kafka.Message {
		return kafka.Message{Topic: "cdc.public.orders", Partition: partition, Offset: offset}
	}
	steps := []struct {
		msg        kafka.Message
		wantReplay bool
	}{
		{at(0, 5), false},
		{at(0, 5), true},  // the same offset again (a rebalance redelivery)
		{at(0, 4), true},  // below the floor
		{at(1, 4), false}, // another partition has its own floor
		{at(0, 6), false},
	}
	for _, s := range steps {
		markCDCReplay(f, sm, s.msg)
		if sm.cdcReplay != s.wantReplay {
			t.Fatalf("partition %d offset %d: replay = %v, want %v", s.msg.Partition, s.msg.Offset, sm.cdcReplay, s.wantReplay)
		}
	}
	other := &SinkMessage{Table: "public.users"}
	if markCDCReplay(f, other, at(0, 1)); other.cdcReplay {
		t.Fatal("another table on the same topic and partition must have its own floor")
	}
	var none *cdcFloorTracker
	if markCDCReplay(none, sm, at(0, 1)); sm.cdcReplay {
		t.Fatal("a nil tracker must count everything")
	}
	markCDCReplay(f, nil, at(0, 9)) // must not panic
}

func TestWriteCDCCounts_RetriesATransientFailure(t *testing.T) {
	conn := &ackFKConn{failCounts: io.ErrUnexpectedEOF, failCountsTimes: 1}
	db := newAckFKDB(t, conn)
	defer db.Close()
	sms, msgs := ledgerTestMessages(0, 4, "u")
	counted, err := persistCDCCounts(context.Background(), db, sms, msgs)
	if err != nil {
		t.Fatalf("one failed attempt must be retried, got %v", err)
	}
	if !equalBools(counted, []bool{true, true, true, true}) {
		t.Fatalf("counted = %v, want all", counted)
	}
	if got := countMatching(conn.recorded(), "INSERT INTO cdc_offset_floors"); got != 2 {
		t.Fatalf("%d attempts, want 2", got)
	}
	if got := conn.storedTotal("public.orders", "u"); got != 4 {
		t.Fatalf("stored total = %d, want 4 (written once)", got)
	}

	t.Run("gives up after the attempt cap", func(t *testing.T) {
		conn := &ackFKConn{failCounts: io.ErrUnexpectedEOF}
		db := newAckFKDB(t, conn)
		defer db.Close()
		if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err == nil {
			t.Fatal("a write that never succeeds must return its error")
		}
		if got := countMatching(conn.recorded(), "INSERT INTO cdc_offset_floors"); got != cdcCountsAttempts {
			t.Fatalf("%d attempts, want %d", got, cdcCountsAttempts)
		}
	})
}

// A database the api-gateway has not migrated yet has no counter tables. The write
// is not retried (it cannot succeed), the rows still count live, and the watermark,
// a statement of its own, is still recorded.
func TestWriteCDCCounts_MissingTablesIsNotRetried(t *testing.T) {
	conn := &ackFKConn{missingTables: true}
	db := newAckFKDB(t, conn)
	defer db.Close()
	sms, msgs := ledgerTestMessages(0, 2, "c")
	counted, err := persistCDCCounts(context.Background(), db, sms, msgs)
	if !isUndefinedTable(err) {
		t.Fatalf("err = %v, want the 42P01", err)
	}
	if !equalBools(counted, []bool{true, true}) {
		t.Fatalf("counted = %v, want all", counted)
	}
	if got := countMatching(conn.recorded(), "INSERT INTO cdc_offset_floors"); got != 1 {
		t.Fatalf("%d attempts, want 1", got)
	}
	if got := len(conn.watermarkUpserts()); got != 1 {
		t.Fatalf("%d watermark upserts, want 1", got)
	}
}

func TestIsUndefinedTable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fakePGError{code: "42P01"}, true},
		{fmt.Errorf("upsert: %w", fakePGError{code: "42P01"}), true},
		{fakePGError{code: "23505"}, false},
		{io.EOF, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isUndefinedTable(c.err); got != c.want {
			t.Errorf("isUndefinedTable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestBuildCDCCountsUpsert(t *testing.T) {
	sms, msgs := ledgerTestMessages(0, 4, "c")
	users := &SinkMessage{PipelineID: ackFKPipelineID, ExecutionID: ackFKExecID, Table: "public.users", CDCOp: " D "}
	truncate := &SinkMessage{PipelineID: ackFKPipelineID, ExecutionID: ackFKExecID, Table: "public.users", CDCOp: "t"}
	junk := &SinkMessage{PipelineID: "pipe-1", ExecutionID: ackFKExecID, Table: "public.junk", CDCOp: "c"}
	sms = append([]*SinkMessage{users}, append(sms, truncate, junk, nil)...)
	msgs = append([]kafka.Message{{Topic: "cdc.public.users", Partition: 0, Offset: 9}},
		append(msgs, kafka.Message{Topic: "cdc.public.users", Partition: 0, Offset: 10}, kafka.Message{Topic: "x"}, kafka.Message{Topic: "y"})...)
	counted := []bool{true, true, true, false, true, true, true, true}

	q, args, ok := buildCDCCountsUpsert(sms, msgs, counted)
	if !ok {
		t.Fatal("ok = false for a flush with rows")
	}
	if got := strings.Count(q, "$"); got != len(args) {
		t.Fatalf("%d placeholders for %d args", got, len(args))
	}
	for _, part := range []string{
		"WITH totals AS (INSERT INTO cdc_counter_totals",
		"total = cdc_counter_totals.total + EXCLUDED.total",
		"INSERT INTO cdc_offset_floors",
		"max_offset = GREATEST(cdc_offset_floors.max_offset, EXCLUDED.max_offset)",
	} {
		if !strings.Contains(q, part) {
			t.Errorf("statement is missing %q:\n%s", part, q)
		}
	}
	// Totals sorted by table then op: orders/c = 3 (offset 2 not counted), users/d = 1.
	// The truncate gets a floor but no total; the non-UUID row gets neither.
	want := []interface{}{
		ackFKPipelineID, ackFKExecID, "public.orders", "c", int64(3),
		ackFKPipelineID, ackFKExecID, "public.users", "d", int64(1),
		ackFKPipelineID, ackFKExecID, "public.orders", "cdc.public.orders", int64(0), int64(3),
		ackFKPipelineID, ackFKExecID, "public.orders", "cdc.public.orders", int64(1), int64(1),
		ackFKPipelineID, ackFKExecID, "public.orders", "cdc.public.orders", int64(2), int64(2),
		ackFKPipelineID, ackFKExecID, "public.users", "cdc.public.users", int64(0), int64(10),
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n %v\nwant\n %v", args, want)
	}

	t.Run("nothing counted writes floors only", func(t *testing.T) {
		q, args, ok := buildCDCCountsUpsert(sms[1:5], msgs[1:5], make([]bool, 4))
		if !ok || strings.Contains(q, "cdc_counter_totals") || len(args) != 3*6 {
			t.Fatalf("ok=%v args=%d q=%s", ok, len(args), q)
		}
	})
	t.Run("args bounded by keys, not events", func(t *testing.T) {
		sms, msgs := ledgerTestMessages(0, 10000, "c")
		_, args, _ := buildCDCCountsUpsert(sms, msgs, cdcCountsFor(sms))
		if len(args) != 5+3*6 {
			t.Fatalf("%d args for 10,000 events on one table and 3 partitions, want %d", len(args), 5+3*6)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, _, ok := buildCDCCountsUpsert(nil, nil, nil); ok {
			t.Fatal("empty flush must build nothing")
		}
		if _, _, ok := buildCDCCountsUpsert([]*SinkMessage{junk}, []kafka.Message{{}}, []bool{true}); ok {
			t.Fatal("a flush of only unusable ids must build nothing")
		}
		short, _ := ledgerTestMessages(0, 3, "c")
		if _, _, ok := buildCDCCountsUpsert(short, nil, []bool{true, true, true}); ok {
			t.Fatal("messages missing must build nothing, not panic")
		}
	})
}

func TestPersistCDCCounts_NoDatabaseCountsEverything(t *testing.T) {
	sms, msgs := ledgerTestMessages(0, 3, "d")
	counted, err := persistCDCCounts(context.Background(), nil, sms, msgs)
	if err != nil {
		t.Fatalf("persistCDCCounts(nil db): %v", err)
	}
	if want := []bool{true, true, true}; !equalBools(counted, want) {
		t.Fatalf("counted = %v, want %v", counted, want)
	}
}

// The per-event lane (deletes, append-only mode, per-row recovery) writes one
// message at a time. It must anchor the executions row itself — in append-only
// mode no batch flush ever does — and only once per process.
func TestPersistCDCCount_AnchorsExecutionOncePerProcess(t *testing.T) {
	const pipelineID = "22222222-2222-2222-2222-222222222222"
	cdcAuditExecutionEnsured.Delete(pipelineID + "|" + pipelineID)
	t.Cleanup(func() { cdcAuditExecutionEnsured.Delete(pipelineID + "|" + pipelineID) })

	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()
	floors := newCDCFloorTracker()

	for _, s := range []struct {
		offset int64
		want   bool
	}{
		{offset: 10, want: true},
		{offset: 10, want: false}, // replay
		{offset: 11, want: true},
	} {
		sm := &SinkMessage{PipelineID: pipelineID, ExecutionID: pipelineID, Table: "public.orders", StorageType: "cdc", RowCount: 1, CDCOp: "c"}
		msg := kafka.Message{Topic: "cdc.public.orders", Offset: s.offset}
		markCDCReplay(floors, sm, msg)
		counted, err := persistCDCCount(context.Background(), db, sm, msg)
		if err != nil || counted != s.want {
			t.Fatalf("offset %d: counted=%v err=%v, want %v", s.offset, counted, err, s.want)
		}
	}

	queries := conn.recorded()
	if got := countMatching(queries, "INSERT INTO executions"); got != 1 {
		t.Fatalf("executions anchor ran %d times over 3 messages, want once per process", got)
	}
	if !strings.Contains(queries[0], "INSERT INTO executions") {
		t.Fatalf("first query must anchor the executions row, got %q", queries[0])
	}
	if got := conn.totals[cdcTotalKey{pipelineID, pipelineID, "public.orders", "c"}]; got != 2 {
		t.Fatalf("stored total = %d, want 2", got)
	}
}

func TestSeedCDCCounters_MapsOpsToCounters(t *testing.T) {
	conn := &ackFKConn{seedRows: [][]driver.Value{
		{"public.orders", "c", int64(7)},
		{"public.orders", "r", int64(3)},
		{"public.orders", "u", int64(2)},
		{"public.orders", "d", int64(1)},
		{"public.users", "c", int64(5)},
		{"public.users", "t", int64(9)}, // truncate: not a row counter
		{"public.users", "", int64(4)},
	}}
	db := newAckFKDB(t, conn)
	defer db.Close()
	c := newCDCTestCounters()

	seedCDCCounters(context.Background(), db, " "+ackFKPipelineID+" ", ackFKExecID+"\n", c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)

	checks := []struct {
		name  string
		m     *sync.Map
		table string
		want  int64
	}{
		{"orders inserts (c only)", c.inserts, "public.orders", 7},
		{"orders snapshot rows (r)", c.snapshotRows, "public.orders", 3},
		{"orders updates", c.updates, "public.orders", 2},
		{"orders deletes", c.deletes, "public.orders", 1},
		{"users inserts", c.inserts, "public.users", 5},
		{"users updates", c.updates, "public.users", 0},
		{"users deletes", c.deletes, "public.users", 0},
		{"users snapshot rows", c.snapshotRows, "public.users", 0},
	}
	for _, ch := range checks {
		if got := loadCounter(ch.m, ch.table); got != ch.want {
			t.Errorf("%s = %d, want %d", ch.name, got, ch.want)
		}
	}

	queries := conn.recorded()
	if len(queries) != 2 {
		t.Fatalf("expected the counter and floor seed reads, got %v", queries)
	}
	for _, clause := range []string{"FROM cdc_counter_totals", "storage_type = 'cdc'", "last_error IS NULL", "pipeline_id = $1", "execution_id = $2"} {
		if !strings.Contains(queries[0], clause) {
			t.Errorf("counter seed is missing %q (stored totals, or failed or non-CDC acks): %s", clause, queries[0])
		}
	}
	if !strings.Contains(queries[1], "FROM cdc_offset_floors") {
		t.Errorf("floor seed does not read cdc_offset_floors: %s", queries[1])
	}
	if len(conn.seedArgs) != 2 || conn.seedArgs[0].Value != ackFKPipelineID || conn.seedArgs[1].Value != ackFKExecID {
		t.Errorf("seed args = %v, want trimmed pipeline and execution ids", conn.seedArgs)
	}
}

// Until the janitor folds them, per-event rows written before this change add to
// the stored totals and floors. A negative ack never landed: no count, but its
// offset was consumed, so it still raises the floor.
func TestSeedCDCCounters_AddsUnfoldedLegacyRows(t *testing.T) {
	const topic = "cdc.public.orders"
	conn := &ackFKConn{
		totals: map[cdcTotalKey]int64{
			{ackFKPipelineID, ackFKExecID, "public.orders", "c"}:                        10,
			{"33333333-3333-3333-3333-333333333333", ackFKExecID, "public.orders", "c"}: 99, // another pipeline
		},
		floors: map[cdcFloorRowKey]int64{
			{ackFKPipelineID, ackFKExecID, cdcFloorKey{table: "public.orders", topic: topic, partition: 0}}: 15,
			{ackFKPipelineID, ackFKExecID, cdcFloorKey{table: "public.orders", topic: topic, partition: 1}}: 7,
		},
		legacy: []legacyAck{
			{table: "public.orders", op: "c", topic: topic, partition: 0, offset: 18},
			{table: "public.orders", op: "c", topic: topic, partition: 0, offset: 19},
			{table: "public.orders", op: "c", topic: topic, partition: 0, offset: 20, failed: true},
		},
	}
	db := newAckFKDB(t, conn)
	defer db.Close()
	c := newCDCTestCounters()
	seedCDCCounters(context.Background(), db, ackFKPipelineID, ackFKExecID, c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)

	if got := loadCounter(c.inserts, "public.orders"); got != 12 {
		t.Fatalf("seeded inserts = %d, want 12 (10 stored + 2 unfolded, the negative ack excluded)", got)
	}
	for _, s := range []struct {
		partition  int
		offset     int64
		wantCounts bool
	}{
		{0, 20, false}, {0, 21, true}, {1, 7, false}, {1, 8, true}, {2, 0, true},
	} {
		if got := c.floors.claim("public.orders", topic, s.partition, s.offset); got != s.wantCounts {
			t.Errorf("partition %d offset %d counts = %v, want %v", s.partition, s.offset, got, s.wantCounts)
		}
	}
}

// Until the api-gateway runs migration 146, the seed reads the legacy rows alone.
func TestSeedCDCCounters_FallsBackToLegacyWithoutTheTables(t *testing.T) {
	const topic = "cdc.public.orders"
	conn := &ackFKConn{missingTables: true, legacy: []legacyAck{
		{table: "public.orders", op: "u", topic: topic, partition: 2, offset: 40},
		{table: "public.orders", op: "u", topic: topic, partition: 2, offset: 41},
	}}
	db := newAckFKDB(t, conn)
	defer db.Close()
	c := newCDCTestCounters()
	seedCDCCounters(context.Background(), db, ackFKPipelineID, ackFKExecID, c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)

	if got := loadCounter(c.updates, "public.orders"); got != 2 {
		t.Fatalf("seeded updates = %d, want 2", got)
	}
	if c.floors.claim("public.orders", topic, 2, 41) {
		t.Fatal("offset 41 landed before the restart; it must read as a replay")
	}
	queries := conn.recorded()
	if len(queries) != 4 || namesCounterTables(queries[1]) || namesCounterTables(queries[3]) {
		t.Fatalf("want each seed tried on the new tables, then on the legacy rows alone; got %d queries", len(queries))
	}
}

func TestSeedCDCCounters_SkipsWithoutUsableIDs(t *testing.T) {
	cases := []struct{ name, pipelineID, executionID string }{
		{"blank pipeline", "", ackFKExecID},
		{"blank execution", ackFKPipelineID, ""},
		{"non-uuid pipeline", "pipe-1", ackFKExecID},
		{"non-uuid execution", ackFKPipelineID, "exec-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &ackFKConn{seedRows: [][]driver.Value{{"public.orders", "c", int64(1)}}}
			db := newAckFKDB(t, conn)
			defer db.Close()
			c := newCDCTestCounters()
			seedCDCCounters(context.Background(), db, tc.pipelineID, tc.executionID, c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)
			if got := conn.recorded(); len(got) != 0 {
				t.Fatalf("expected no query, got %v", got)
			}
		})
	}
	t.Run("nil db does not panic", func(t *testing.T) {
		c := newCDCTestCounters()
		seedCDCCounters(context.Background(), nil, ackFKPipelineID, ackFKExecID, c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)
	})
}

// A read that fails, up front or partway through the rows, must leave every
// counter at zero rather than seed some tables and not others.
func TestSeedCDCCounters_ReadFailureSeedsNothing(t *testing.T) {
	rows := [][]driver.Value{{"public.orders", "c", int64(7)}, {"public.users", "u", int64(3)}}
	cases := []struct {
		name string
		conn *ackFKConn
	}{
		{"query fails", &ackFKConn{seedRows: rows, failSeed: io.ErrUnexpectedEOF}},
		{"rows fail after reading", &ackFKConn{seedRows: rows, seedRowsErr: io.ErrUnexpectedEOF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newAckFKDB(t, tc.conn)
			defer db.Close()
			c := newCDCTestCounters()
			seedCDCCounters(context.Background(), db, ackFKPipelineID, ackFKExecID, c.floors, c.inserts, c.updates, c.deletes, c.snapshotRows)
			if got := loadCounter(c.inserts, "public.orders"); got != 0 {
				t.Errorf("orders inserts = %d, want 0", got)
			}
			if got := loadCounter(c.updates, "public.users"); got != 0 {
				t.Errorf("users updates = %d, want 0", got)
			}
		})
	}
}

func TestCDCStatsExecutionID(t *testing.T) {
	const pipelineID, executionID = "pipe", "exec"
	cases := []struct {
		name string
		cfg  WorkerConfig
		want string
	}{
		{"cdc mode uses the pipeline id", WorkerConfig{PipelineID: pipelineID, ExecutionID: executionID, SinkMode: "cdc"}, pipelineID},
		{"cdc mode is case and space insensitive", WorkerConfig{PipelineID: " " + pipelineID, ExecutionID: executionID, SinkMode: " CDC "}, pipelineID},
		{"batch mode uses the execution id", WorkerConfig{PipelineID: pipelineID, ExecutionID: " " + executionID + " ", SinkMode: "batch"}, executionID},
		{"no execution id falls back to the pipeline id", WorkerConfig{PipelineID: pipelineID, ExecutionID: "  ", SinkMode: "batch"}, pipelineID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cdcStatsExecutionID(&tc.cfg); got != tc.want {
				t.Fatalf("cdcStatsExecutionID = %q, want %q", got, tc.want)
			}
		})
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
