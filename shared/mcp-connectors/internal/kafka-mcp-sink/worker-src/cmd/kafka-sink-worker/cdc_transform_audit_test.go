package main

// Coverage for cdc_transform_audit.go. A consumer chain over N CDC rows used to cost the
// control-plane database N executions INSERTs and N upserts per step, synchronously on the
// consume goroutine. These tests run real rows through applyConsumerTransforms against the
// fake database the CDC counter tests share and pin what replaced that: nothing written
// per row, one write per window carrying the window's sums, and the per-row warning
// logged once.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rsync-ai/shared/transforms"
)

// useFreshTransformAudit swaps in an empty tally for the test, so no other test's rows or
// window clock leak in, and puts the worker's back afterwards.
func useFreshTransformAudit(t *testing.T) {
	t.Helper()
	saved := cdcTransformAudit
	cdcTransformAudit = &transformAuditTally{}
	t.Cleanup(func() { cdcTransformAudit = saved })
}

// seedConsumerChain makes configs pipelineID's consumer chain, as loadConsumerTransformsRaw
// would have cached it from transform_definitions, so no test query reaches the database.
func seedConsumerChain(t *testing.T, pipelineID string, configs ...map[string]interface{}) {
	t.Helper()
	raw := make([]map[string]interface{}, 0, len(configs))
	for i, c := range configs {
		raw = append(raw, map[string]interface{}{
			"id":               fmt.Sprintf("00000000-0000-0000-0000-0000000000%02d", i+1),
			"transform_order":  i + 1,
			"enabled":          true,
			"transform_config": c,
		})
	}
	consumerTransformsCache.Store(pipelineID, consumerTransformsCacheEntry{loadedAt: time.Now().UTC(), raw: raw})
	t.Cleanup(func() { consumerTransformsCache.Delete(pipelineID) })
}

func hashMask(column string) map[string]interface{} {
	return map[string]interface{}{"operation": "mask", "column": column, "mask_type": "hash", "hash_function": "sha256"}
}

// auditCDCRow is one CDC insert into customers, as parseCDCMessage hands it on.
func auditCDCRow(i int) *SinkMessage {
	row := func() map[string]interface{} {
		return map[string]interface{}{"id": i, "email": fmt.Sprintf("u%d@example.com", i), "name": fmt.Sprintf("user %d", i)}
	}
	return &SinkMessage{
		IsCDC:       true,
		PipelineID:  ackFKPipelineID,
		ExecutionID: ackFKExecID,
		Table:       "customers",
		CDCOp:       "c",
		Data:        []map[string]interface{}{row()},
		After:       row(),
		RowCount:    1,
	}
}

func transformRows(t *testing.T, conn *ackFKConn, from, n int) {
	t.Helper()
	db := newAckFKDB(t, conn)
	for i := from; i < from+n; i++ {
		sm := auditCDCRow(i)
		if err := applyConsumerTransforms(context.Background(), db, &WorkerConfig{}, sm); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if got, _ := sm.Data[0]["email"].(string); strings.Contains(got, "@") {
			t.Fatalf("row %d: email %q was not masked; the chain did not run, so nothing here is evidence", i, got)
		}
	}
}

func (c *ackFKConn) auditLogs() [][]driver.NamedValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]driver.NamedValue(nil), c.transformLogs...)
}

func countQueries(qs []string, substr string) int {
	n := 0
	for _, q := range qs {
		if strings.Contains(q, substr) {
			n++
		}
	}
	return n
}

// auditLogRow is one transform_execution_logs upsert, by upsertTransformExecutionLogPG's
// bind order.
type auditLogRow struct {
	table, status string
	order, in     int64
	out           int64
	errMsg        interface{}
}

func decodeAuditLog(args []driver.NamedValue) auditLogRow {
	return auditLogRow{
		table:  args[2].Value.(string),
		order:  args[4].Value.(int64),
		status: args[6].Value.(string),
		errMsg: args[7].Value,
		in:     args[8].Value.(int64),
		out:    args[9].Value.(int64),
	}
}

func TestTransformAudit_AChainOverManyRowsWritesOncePerWindow(t *testing.T) {
	useFreshTransformAudit(t)
	seedConsumerChain(t, ackFKPipelineID, hashMask("email"), hashMask("name"))
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	ctx := context.Background()

	transformRows(t, conn, 0, 100)
	if qs := conn.recorded(); len(qs) != 0 {
		t.Fatalf("100 transformed rows wrote %d statements before any window closed, want 0 — "+
			"the audit is back to a write per row:\n%s", len(qs), strings.Join(qs, "\n"))
	}

	start := time.Now()
	cdcTransformAudit.writeDue(ctx, db, start)
	qs := conn.recorded()
	if n := countQueries(qs, "INSERT INTO executions"); n != 1 {
		t.Errorf("the window wrote %d executions rows, want 1 for its one execution", n)
	}
	logs := conn.auditLogs()
	if len(logs) != 2 {
		t.Fatalf("the window wrote %d transform_execution_logs upserts, want 2 (one per step)", len(logs))
	}
	if first := strings.Index(strings.Join(qs, "\x00"), "INSERT INTO executions"); first < 0 ||
		first > strings.Index(strings.Join(qs, "\x00"), "transform_execution_logs") {
		t.Errorf("the executions row must be written before the logs that reference it (FK); order:\n%s", strings.Join(qs, "\n"))
	}
	for i, args := range logs {
		got := decodeAuditLog(args)
		want := auditLogRow{table: "customers", order: int64(i + 1), status: transforms.StepStatusSuccess, in: 100, out: 100}
		if got != want {
			t.Errorf("step %d: wrote %+v, want %+v — the window's sums, once", i+1, got, want)
		}
	}

	// The next window: written once transformAuditFlushEvery has passed, not before.
	transformRows(t, conn, 100, 10)
	cdcTransformAudit.writeDue(ctx, db, start.Add(time.Second))
	if n := len(conn.auditLogs()); n != 2 {
		t.Fatalf("a write 1s into the window: %d upserts in all, want still 2", n)
	}
	cdcTransformAudit.writeDue(ctx, db, start.Add(transformAuditFlushEvery))
	logs = conn.auditLogs()
	if len(logs) != 4 {
		t.Fatalf("after the window closed: %d upserts in all, want 4", len(logs))
	}
	for _, args := range logs[2:] {
		if got := decodeAuditLog(args); got.in != 10 || got.out != 10 {
			t.Errorf("second window, step %d: in=%d out=%d, want 10/10 — only the rows since the last write", got.order, got.in, got.out)
		}
	}

	// An idle window writes nothing.
	before := len(conn.recorded())
	cdcTransformAudit.writeDue(ctx, db, start.Add(10*transformAuditFlushEvery))
	if after := len(conn.recorded()); after != before {
		t.Errorf("an empty window wrote %d statements, want 0", after-before)
	}
}

// A window holding a failure is written as failed, with the error, whatever the other
// rows in it did: the upsert keeps 'failed' sticky, so the sum must not launder it.
func TestTransformAudit_AFailureInTheWindowIsWrittenAsFailed(t *testing.T) {
	useFreshTransformAudit(t)
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	step := transforms.CanonicalTransform{ID: "00000000-0000-0000-0000-000000000001", Order: 1, Type: "mask_pii"}

	cdcTransformAudit.record(ackFKPipelineID, ackFKExecID, "customers", step, 1, 1, time.Millisecond, nil)
	cdcTransformAudit.record(ackFKPipelineID, ackFKExecID, "customers", step, 1, 0, time.Millisecond, errors.New("mask: column email is not a string"))
	cdcTransformAudit.record(ackFKPipelineID, ackFKExecID, "customers", step, 1, 1, time.Millisecond, nil)
	cdcTransformAudit.writeDue(context.Background(), db, time.Now())

	logs := conn.auditLogs()
	if len(logs) != 1 {
		t.Fatalf("wrote %d upserts, want 1", len(logs))
	}
	got := decodeAuditLog(logs[0])
	if got.status != transforms.StepStatusFailed || got.errMsg != "mask: column email is not a string" {
		t.Errorf("status=%q error=%v, want failed with the step's error", got.status, got.errMsg)
	}
	if got.in != 3 || got.out != 2 {
		t.Errorf("in=%d out=%d, want 3/2", got.in, got.out)
	}
}

// A worker shutting down cancels the consume loop's ctx before the drain runs. A write
// on that ctx would fail and drop the window, so the window must survive for the drain.
func TestTransformAudit_ACancelledLoopLeavesTheWindowForTheShutdownDrain(t *testing.T) {
	useFreshTransformAudit(t)
	conn := &ackFKConn{}
	db := newAckFKDB(t, conn)
	step := transforms.CanonicalTransform{ID: "00000000-0000-0000-0000-000000000001", Order: 1, Type: "mask_pii"}
	cdcTransformAudit.record(ackFKPipelineID, ackFKExecID, "customers", step, 7, 7, time.Millisecond, nil)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cdcTransformAudit.writeDue(cancelled, db, time.Now())
	if qs := conn.recorded(); len(qs) != 0 {
		t.Fatalf("a cancelled loop wrote %d statements, want 0", len(qs))
	}

	cdcTransformAudit.write(context.Background(), db, time.Now()) // the drain
	logs := conn.auditLogs()
	if len(logs) != 1 || decodeAuditLog(logs[0]).in != 7 {
		t.Fatalf("the drain wrote %d upserts (%v), want 1 carrying the 7 rows", len(logs), logs)
	}
}

// A rule naming a column no row carries fails its check on every row; it is one warning.
func TestTransformAudit_AMissingColumnIsWarnedOncePerStep(t *testing.T) {
	missingColumnWarned.Range(func(k, _ any) bool { missingColumnWarned.Delete(k); return true })
	t.Cleanup(func() { missingColumnWarned.Range(func(k, _ any) bool { missingColumnWarned.Delete(k); return true }) })
	chain := mongoMaskChain(t, map[string]any{"operation": "type_convert", "column": "status", "to": "string"})

	recs := captureLogLines(t, func() {
		for i := 0; i < 50; i++ {
			if err := applyCDCTransformChain(context.Background(), nil, auditCDCRow(i), chain); err != nil {
				t.Fatalf("row %d: %v", i, err)
			}
		}
	})
	n := 0
	for _, rec := range recs {
		if m, _ := rec["message"].(string); strings.HasPrefix(m, "transform type_convert (order 1) on customers:") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("50 rows without the column logged the warning %d times, want 1", n)
	}
}

// The window's two writers live in the consume loop, which needs a live Kafka reader to
// drive, so their wiring is pinned structurally: the loop polls writeDue every pass, and
// the shutdown drain writes what is left on the drain's own context.
func TestTransformAudit_TheConsumeLoopWritesTheWindow(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("cannot read main.go: %v", err)
	}
	if len(src) < 100_000 {
		t.Fatalf("main.go read back as %d bytes; that is not the worker", len(src))
	}
	for _, c := range []struct{ what, pattern string }{
		{"the loop's per-pass write", `for \{\s*(//[^\n]*\n\s*)*cdcTransformAudit\.writeDue\(ctx, pgDB, time\.Now\(\)\)`},
		{"the shutdown drain's write", `cdcTransformAudit\.write\(drainCtx, pgDB, time\.Now\(\)\)\s*drainCancel\(\)`},
	} {
		if n := len(regexp.MustCompile(c.pattern).FindAll(src, -1)); n != 1 {
			t.Errorf("%s: found %d time(s) in main.go, want 1", c.what, n)
		}
	}
	if n := strings.Count(string(src), "upsertTransformExecutionLogPG("); n != 1 {
		t.Errorf("main.go names upsertTransformExecutionLogPG %d times, want 1 (its definition): "+
			"a call there is a write per row again", n)
	}
}
