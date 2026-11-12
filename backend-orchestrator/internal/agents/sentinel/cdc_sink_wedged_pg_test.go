//go:build integration_pg

// Real-PostgreSQL coverage for closing the sink-wedged escalation. The sqlmock tests pin the
// statement and what a deleted row does to the restart bookkeeping; only a real server proves
// the DELETE … EXISTS executes (pipeline_run_table_stats.pipeline_id is a uuid, the argument
// is text) and compares the right timestamps. Run it as batch_sentinel_pg_test.go's header says.
package sentinel

import (
	"context"
	"testing"
)

// TestPGSinkWedgedClosesOnlyOnAnApplyAfterTheAlert: an apply from before the alert, a batch
// row applied after it, and another pipeline's apply all leave it open; this pipeline's CDC
// apply after it closes it, and nothing else.
func TestPGSinkWedgedClosesOnlyOnAnApplyAfterTheAlert(t *testing.T) {
	db := pgTestDB(t)
	pgStopSeed(t, db, "running")
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%.60q: %v", q, err)
		}
	}
	wedged := sinkWedgedEscalationIssueID(pgStopPipelineID)
	mustExec(`UPDATE sentinel_active_issues SET last_occurrence = NOW() - interval '10 minutes' WHERE id = $1`, wedged)
	applied := func(pipelineID, table, mode, ago string) {
		mustExec(`INSERT INTO pipeline_run_table_stats
		          (pipeline_id, execution_id, table_name, qualified_name, mode, status, last_applied_ts)
		          VALUES ($1::uuid, $2::uuid, $3, $3, $4, 'running', NOW() - $5::interval)`,
			pipelineID, pgTestExecutionID, table, mode, ago)
	}
	s := &CDCSentinel{db: db, sinkRestartState: map[string]*connRestartState{
		pgStopPipelineID: {attempts: 3, escalated: true},
	}}
	open := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sentinel_active_issues WHERE id = $1`, wedged).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		return n == 1
	}
	before := len(pgOpenIssues(t, db))

	applied(pgStopPipelineID, "orders", "cdc", "20 minutes")   // before the alert
	applied(pgStopPipelineID, "backfill", "batch", "1 minute") // after it, but not the stream
	applied(pgRunPipelineID, "orders", "cdc", "1 minute")      // after it, another pipeline
	s.resolveSinkWedged(context.Background(), pgStopPipelineID)
	if !open() {
		t.Fatal("closed with no CDC apply by this pipeline since the alert")
	}
	if !s.sinkRestartState[pgStopPipelineID].escalated {
		t.Fatal("re-armed the escalation without closing anything")
	}

	applied(pgStopPipelineID, "customers", "cdc", "1 minute")
	s.resolveSinkWedged(context.Background(), pgStopPipelineID)
	if open() {
		t.Fatal("still open after this pipeline's sink applied a change after the alert")
	}
	if s.sinkRestartState[pgStopPipelineID].escalated {
		t.Fatal("closed the alert but left the escalation spent")
	}
	if got := len(pgOpenIssues(t, db)); got != before-1 {
		t.Fatalf("%d issues open after the close, want %d: it closed more than the wedged one", got, before-1)
	}
}
