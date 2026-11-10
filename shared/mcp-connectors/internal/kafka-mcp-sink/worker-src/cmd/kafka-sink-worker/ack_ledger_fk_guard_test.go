package main

// Regression coverage for KI-CDC-ACKLEDGER-FK-MISMATCH, and the fake database the
// CDC counter tests share.
//
// CDC keys its stats by execution_id == pipeline_id (a stable streaming counter
// bucket, set in parseCDCMessage), but the tables that hang off an execution carry a
// real FK to executions(id). No executions row with id = pipeline_id exists for a
// normal CDC pipeline, so every such write failed with 23503. The ack ledger was the
// first to hit it; transform_execution_logs and the run events still reference the
// row now that CDC writes no per-event acks.
//
// The fix pre-inserts the synthetic executions row before the flush's first write.
// These tests pin the three properties that make it correct rather than merely
// present — the guard runs BEFORE the counter write, runs ONCE per flush, and
// refuses to write garbage ids.
//
// Uses a minimal in-process database/sql driver (no live Postgres, no new module
// dependency), like negative_ack_test.go — but this one records EVERY query, not
// just the last, because ordering and call count are the assertions.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
)

// legacyAck is a per-event CDC row in pipeline_batch_acks, as sinks before
// KI-CDC-ACK-LEDGER-UNBOUNDED wrote them.
type legacyAck struct {
	table, op, topic  string
	partition, offset int64
	failed            bool // last_error set: a negative ack, which never landed
}

// fakePGError carries a PostgreSQL SQLSTATE the way pgconn.PgError does.
type fakePGError struct{ code string }

func (e fakePGError) Error() string    { return "ERROR: fake (SQLSTATE " + e.code + ")" }
func (e fakePGError) SQLState() string { return e.code }

// ackFKConn records every query in order. Implementing driver.ExecerContext and
// driver.QueryerContext routes db.ExecContext / db.QueryContext straight here,
// bypassing Prepare.
//
// It also stands in for cdc_counter_totals (a counter upsert adds to totals) and
// cdc_offset_floors (it keeps the max), and answers the seed reads from those plus
// any legacy per-event rows, the way the UNION ALL in the seed queries does.
type ackFKConn struct {
	mu      sync.Mutex
	queries []string
	// failCounts fails the counter upsert; with failCountsTimes > 0 only that
	// many times, so a retry can succeed.
	failCounts      error
	failCountsTimes int
	countFails      int
	// missingTables answers every statement naming cdc_counter_totals or
	// cdc_offset_floors with 42P01, as a database without migration 146 does.
	missingTables bool
	totals        map[cdcTotalKey]int64
	floors        map[cdcFloorRowKey]int64
	legacy        []legacyAck
	// seedRows, when set, answers the counter seed instead of the stored rows.
	seedRows [][]driver.Value
	// failSeed fails both seed reads; seedRowsErr fails the counter seed after
	// its rows were read.
	failSeed    error
	seedRowsErr error
	seedArgs    []driver.NamedValue
	// watermarks holds each cdc_table_watermarks upsert's bind args, in order;
	// failWatermarks fails those upserts the way a database without migration 137 does.
	watermarks     [][]driver.NamedValue
	failWatermarks error
	// transformLogs holds each transform_execution_logs upsert's bind args, in order.
	transformLogs [][]driver.NamedValue
}

func (c *ackFKConn) Prepare(string) (driver.Stmt, error) { return nil, io.EOF }
func (c *ackFKConn) Close() error                        { return nil }
func (c *ackFKConn) Begin() (driver.Tx, error)           { return nil, io.EOF }

func namesCounterTables(q string) bool {
	return strings.Contains(q, "cdc_counter_totals") || strings.Contains(q, "cdc_offset_floors")
}

func (c *ackFKConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	switch {
	case strings.Contains(q, "transform_execution_logs"):
		c.transformLogs = append(c.transformLogs, args)
		return driver.RowsAffected(1), nil
	case strings.Contains(q, "cdc_table_watermarks"):
		if c.failWatermarks != nil {
			return nil, c.failWatermarks
		}
		c.watermarks = append(c.watermarks, args)
		return driver.RowsAffected(int64(len(args) / cdcWatermarkCols)), nil
	case namesCounterTables(q):
		if c.missingTables {
			return nil, fakePGError{code: "42P01"}
		}
		if c.failCounts != nil && (c.failCountsTimes == 0 || c.countFails < c.failCountsTimes) {
			c.countFails++
			return nil, c.failCounts
		}
		c.applyCountsLocked(q, args)
		return driver.RowsAffected(1), nil
	}
	return driver.RowsAffected(1), nil
}

// applyCountsLocked applies a buildCDCCountsUpsert statement: 5 bind args per
// totals row, then 6 per floors row. The totals rows are the placeholders before
// the floors INSERT.
func (c *ackFKConn) applyCountsLocked(q string, args []driver.NamedValue) {
	if c.totals == nil {
		c.totals = map[cdcTotalKey]int64{}
	}
	if c.floors == nil {
		c.floors = map[cdcFloorRowKey]int64{}
	}
	nTotals := strings.Count(q[:strings.Index(q, "INSERT INTO cdc_offset_floors")], "$")
	str := func(i int) string { return args[i].Value.(string) }
	num := func(i int) int64 { return args[i].Value.(int64) }
	for b := 0; b+5 <= nTotals; b += 5 {
		c.totals[cdcTotalKey{str(b), str(b + 1), str(b + 2), str(b + 3)}] += num(b + 4)
	}
	for b := nTotals; b+6 <= len(args); b += 6 {
		k := cdcFloorRowKey{str(b), str(b + 1), cdcFloorKey{table: str(b + 2), topic: str(b + 3), partition: num(b + 4)}}
		if cur, ok := c.floors[k]; !ok || num(b+5) > cur {
			c.floors[k] = num(b + 5)
		}
	}
}

func (c *ackFKConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	if !strings.Contains(q, "FROM pipeline_batch_acks") {
		return nil, io.EOF
	}
	c.seedArgs = args
	if c.failSeed != nil {
		return nil, c.failSeed
	}
	if c.missingTables && namesCounterTables(q) {
		return nil, fakePGError{code: "42P01"}
	}
	pid, eid := args[0].Value.(string), args[1].Value.(string)
	withStored := namesCounterTables(q)

	if strings.Contains(q, "last_error IS NULL") { // the counter seed
		rows := &ackFKRows{cols: []string{"table_name", "op", "n"}, vals: c.seedRows, err: c.seedRowsErr}
		if rows.vals != nil {
			return rows, nil
		}
		sums := map[[2]string]int64{}
		if withStored {
			for k, n := range c.totals {
				if k.pipelineID == pid && k.executionID == eid {
					sums[[2]string{k.table, k.op}] += n
				}
			}
		}
		for _, a := range c.legacy {
			if !a.failed {
				sums[[2]string{a.table, a.op}]++
			}
		}
		for k, n := range sums {
			rows.vals = append(rows.vals, []driver.Value{k[0], k[1], n})
		}
		return rows, nil
	}

	// The floor seed.
	maxes := map[cdcFloorKey]int64{}
	keep := func(k cdcFloorKey, o int64) {
		if cur, ok := maxes[k]; !ok || o > cur {
			maxes[k] = o
		}
	}
	if withStored {
		for k, o := range c.floors {
			if k.pipelineID == pid && k.executionID == eid {
				keep(k.cdcFloorKey, o)
			}
		}
	}
	for _, a := range c.legacy {
		keep(cdcFloorKey{table: a.table, topic: a.topic, partition: a.partition}, a.offset)
	}
	rows := &ackFKRows{cols: []string{"table_name", "kafka_topic", "kafka_partition", "max"}}
	for k, o := range maxes {
		rows.vals = append(rows.vals, []driver.Value{k.table, k.topic, k.partition, o})
	}
	return rows, nil
}

type ackFKRows struct {
	cols []string
	vals [][]driver.Value
	err  error // returned once vals are exhausted, in place of io.EOF
	i    int
}

func (r *ackFKRows) Columns() []string { return r.cols }
func (r *ackFKRows) Close() error      { return nil }
func (r *ackFKRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		if r.err != nil {
			return r.err
		}
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}

func (c *ackFKConn) watermarkUpserts() [][]driver.NamedValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]driver.NamedValue(nil), c.watermarks...)
}

func (c *ackFKConn) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.queries))
	copy(out, c.queries)
	return out
}

// storedTotal and storedFloor read one stored row (0, false when absent).
func (c *ackFKConn) storedTotal(table, op string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totals[cdcTotalKey{ackFKPipelineID, ackFKExecID, table, op}]
}

func (c *ackFKConn) storedFloor(table, topic string, partition int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.floors[cdcFloorRowKey{ackFKPipelineID, ackFKExecID, cdcFloorKey{table: table, topic: topic, partition: partition}}]
	return o, ok
}

// ackFKDriver hands out ackFKActiveConn. sql.Register is process-global and
// one-shot, hence both the sync.Once and a driver name distinct from
// negative_ack_test.go's "negack_capture".
type ackFKDriver struct{}

func (ackFKDriver) Open(string) (driver.Conn, error) { return ackFKActiveConn, nil }

var (
	ackFKRegisterOnce sync.Once
	ackFKActiveConn   *ackFKConn
)

func newAckFKDB(t *testing.T, conn *ackFKConn) *sql.DB {
	t.Helper()
	ackFKRegisterOnce.Do(func() { sql.Register("ackfk_capture", ackFKDriver{}) })
	ackFKActiveConn = conn
	db, err := sql.Open("ackfk_capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	return db
}

// The CDC convention under test: execution_id == pipeline_id, and both must pass
// looksLikeUUID (36 chars, dashes at 8/13/18/23) or the guard declines to write.
const (
	ackFKPipelineID = "11111111-1111-1111-1111-111111111111"
	ackFKExecID     = ackFKPipelineID
)

func ackFKMessages(t *testing.T, n int) ([]*SinkMessage, []kafka.Message) {
	t.Helper()
	sms := make([]*SinkMessage, n)
	msgs := make([]kafka.Message, n)
	for i := 0; i < n; i++ {
		sms[i] = &SinkMessage{
			PipelineID:  ackFKPipelineID,
			ExecutionID: ackFKExecID,
			Table:       "public.orders",
			StorageType: "cdc",
			BatchOffset: int64(i),
			RowCount:    1,
			CDCOp:       "c",
		}
		msgs[i] = kafka.Message{Topic: "cdc.public.orders", Partition: i % 3, Offset: int64(i)}
	}
	return sms, msgs
}

func countMatching(queries []string, needle string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, needle) {
			n++
		}
	}
	return n
}

// TestPersistCDCCounts_EnsuresExecutionRowBeforeCounterWrite is the core guard:
// the synthetic executions row must be written BEFORE the flush's counter write.
// Order is the whole fix — a guard that ran after would leave the FK violation of
// the rows that reference it exactly as it was.
func TestPersistCDCCounts_EnsuresExecutionRowBeforeCounterWrite(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()

	sms, msgs := ackFKMessages(t, 3)
	if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err != nil {
		t.Fatalf("persistCDCCounts: %v", err)
	}

	queries := conn.recorded()
	execIdx, countIdx := -1, -1
	for i, q := range queries {
		if execIdx == -1 && strings.Contains(q, "INSERT INTO executions") {
			execIdx = i
		}
		if countIdx == -1 && namesCounterTables(q) {
			countIdx = i
		}
	}
	if execIdx == -1 {
		t.Fatalf("no INSERT INTO executions guard was issued — the FK violation is unfixed: %v", queries)
	}
	if countIdx == -1 {
		t.Fatalf("no counter write was issued: %v", queries)
	}
	if execIdx > countIdx {
		t.Fatalf("executions guard ran at %d, AFTER the counter write at %d — ordering is the fix", execIdx, countIdx)
	}
	// The guard must be idempotent: a redelivered flush re-runs it.
	if !strings.Contains(queries[execIdx], "ON CONFLICT (id) DO NOTHING") {
		t.Fatalf("executions guard is not idempotent, missing ON CONFLICT: %q", queries[execIdx])
	}
}

// TestPersistCDCCounts_EnsuresExecutionRowOncePerFlush pins the "once per flush,
// not once per message" property. CDC flushes are hot — one extra INSERT per
// message would add a write per change event for the life of every stream.
func TestPersistCDCCounts_EnsuresExecutionRowOncePerFlush(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()

	sms, msgs := ackFKMessages(t, 25)
	if _, err := persistCDCCounts(context.Background(), db, sms, msgs); err != nil {
		t.Fatalf("persistCDCCounts: %v", err)
	}

	if got := countMatching(conn.recorded(), "INSERT INTO executions"); got != 1 {
		t.Fatalf("expected exactly 1 executions guard for a 25-message flush, got %d", got)
	}
}

// TestEnsureExecutionRowForCDCAudit_RefusesNonUUIDIdentifiers proves the guard
// will not manufacture rows from junk. `executions.id` is a uuid column, so a
// non-UUID id would itself error (22P02) — turning a fixed FK warning into a new
// one. The helper must short-circuit before touching the DB.
func TestEnsureExecutionRowForCDCAudit_RefusesNonUUIDIdentifiers(t *testing.T) {
	cases := []struct {
		name        string
		pipelineID  string
		executionID string
	}{
		{name: "both blank", pipelineID: "", executionID: ""},
		{name: "blank pipeline", pipelineID: "", executionID: ackFKExecID},
		{name: "blank execution", pipelineID: ackFKPipelineID, executionID: ""},
		{name: "non-uuid pipeline", pipelineID: "pipe-1", executionID: ackFKExecID},
		{name: "non-uuid execution", pipelineID: ackFKPipelineID, executionID: "exec-1"},
		{name: "right length wrong dashes", pipelineID: ackFKPipelineID, executionID: "111111111111111111111111111111111111"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &ackFKConn{}
			db := newAckFKDB(t, conn)
			defer db.Close()

			ensureExecutionRowForCDCAudit(context.Background(), db, tc.pipelineID, tc.executionID)

			if got := conn.recorded(); len(got) != 0 {
				t.Fatalf("expected no query for %s, got %v", tc.name, got)
			}
		})
	}

	t.Run("nil db does not panic", func(t *testing.T) {
		ensureExecutionRowForCDCAudit(context.Background(), nil, ackFKPipelineID, ackFKExecID)
	})

	t.Run("valid uuids do write", func(t *testing.T) {
		conn := &ackFKConn{}
		db := newAckFKDB(t, conn)
		defer db.Close()

		ensureExecutionRowForCDCAudit(context.Background(), db, ackFKPipelineID, ackFKExecID)

		got := conn.recorded()
		if len(got) != 1 || !strings.Contains(got[0], "INSERT INTO executions") {
			t.Fatalf("expected one executions insert, got %v", got)
		}
	})
}

// TestPersistCDCCounts_StaysBestEffortWhenTheWriteFails keeps the severity
// contract intact: the counters are bookkeeping (real exactly-once lives in
// _rsync_cdc_offsets, committed with the data write). If the counter write fails
// for good, the guard must still have run, every message must still count, and the
// error must be RETURNED (the caller logs it non-fatally) rather than panicking or
// being swallowed here.
func TestPersistCDCCounts_StaysBestEffortWhenTheWriteFails(t *testing.T) {
	conn := &ackFKConn{failCounts: io.ErrUnexpectedEOF}
	db := newAckFKDB(t, conn)
	defer db.Close()

	sms, msgs := ackFKMessages(t, 2)
	counted, err := persistCDCCounts(context.Background(), db, sms, msgs)
	if err == nil {
		t.Fatal("expected the counter write error to be returned so the caller can log it")
	}
	if !equalBools(counted, []bool{true, true}) {
		t.Fatalf("counted = %v after a failed write; the live counters must keep moving", counted)
	}
	if got := countMatching(conn.recorded(), "INSERT INTO executions"); got != 1 {
		t.Fatalf("guard must run even when the counter write fails, got %d executions inserts", got)
	}
}

// TestPersistCDCCounts_NoMessagesIsANoOp guards the empty-flush path: with nothing
// to count there is nothing to anchor, so the guard must not create a synthetic
// executions row (which would otherwise appear for pipelines that never streamed a
// single event).
func TestPersistCDCCounts_NoMessagesIsANoOp(t *testing.T) {
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	defer db.Close()

	if _, err := persistCDCCounts(context.Background(), db, nil, nil); err != nil {
		t.Fatalf("persistCDCCounts(nil): %v", err)
	}
	if got := conn.recorded(); len(got) != 0 {
		t.Fatalf("expected no queries for an empty flush, got %v", got)
	}
}
