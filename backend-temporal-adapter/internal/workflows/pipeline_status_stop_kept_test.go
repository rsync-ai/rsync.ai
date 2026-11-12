package workflows

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// Pins KI-STOP-RACING-COMPLETION-OVERWRITTEN.
//
// Stop writes executions.status='cancelled' and pipelines.status='stopped'
// directly. A run whose last activity was already past the point a cancel could
// reach went on to write its own terminal status on top: the pipeline read
// "completed" about a minute after the user stopped it (the postflight's settle
// wait), and the stop alert was never sent. The terminal write reads the
// execution row first and, when a Stop got there before it, records the Stop.

const stopKeptExecutionID = "44444444-0000-4000-8000-0000000000b1"

func finalizeWith(t *testing.T, state *recordedDB) (runOutcome, *capturingProducer) {
	t.Helper()
	producer := withRecordedDB(t, state)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(FinalizeCompletedRunActivity)
	val, err := env.ExecuteActivity(FinalizeCompletedRunActivity, supPipelineID, stopKeptExecutionID)
	if err != nil {
		t.Fatalf("FinalizeCompletedRunActivity: %v", err)
	}
	var out runOutcome
	if err := val.Get(&out); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	return out, producer
}

func failWith(t *testing.T, state *recordedDB, reason string) (runOutcome, *capturingProducer) {
	t.Helper()
	producer := withRecordedDB(t, state)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	// UpdatePipelineStatusActivity returns only an error; the same body is
	// driven here so the recorded outcome can be read back.
	writeFailed := func(ctx context.Context) (runOutcome, error) {
		return writePipelineStatus(ctx, supPipelineID, stopKeptExecutionID, "failed", reason)
	}
	env.RegisterActivity(writeFailed)
	val, err := env.ExecuteActivity(writeFailed)
	if err != nil {
		t.Fatalf("writePipelineStatus(failed): %v", err)
	}
	var out runOutcome
	if err := val.Get(&out); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	return out, producer
}

// assertRecordedAsStopped checks the three things the user sees: the pipeline
// row, the execution row, and the alert.
func assertRecordedAsStopped(t *testing.T, state *recordedDB, out runOutcome, producer *capturingProducer) {
	t.Helper()
	if out.Status != "stopped" {
		t.Fatalf("outcome = %q, want stopped — a run the user stopped was recorded as %s:\n%s",
			out.Status, out.Status, state.all())
	}
	pArgs, ok := state.argsOf("UPDATE pipelines SET status = $2")
	if !ok || len(pArgs) < 2 || pArgs[1] != "stopped" {
		t.Fatalf("pipelines.status written as %v, want stopped:\n%s", pArgs, state.all())
	}
	eArgs, ok := state.argsOf("UPDATE executions SET status = $2")
	if ok && (len(eArgs) < 2 || eArgs[1] != "cancelled") {
		t.Fatalf("executions.status overwritten with %v, want it left cancelled:\n%s", eArgs, state.all())
	}
	if state.ran("UPDATE pipeline_progress") {
		t.Fatalf("the stopped run's progress was rewritten as a failure:\n%s", state.all())
	}
	if len(producer.sent) != 1 {
		t.Fatalf("raised %d alert(s), want the one stop alert", len(producer.sent))
	}
	body, _ := producer.sent[0].Value.Encode()
	if !strings.Contains(string(body), codeRunStopped) {
		t.Fatalf("the alert is not the stop alert: %s", body)
	}
}

func TestACompletionAfterAStopIsRecordedAsTheStop(t *testing.T) {
	state := &recordedDB{executionStatus: "cancelled"}
	out, producer := finalizeWith(t, state)
	assertRecordedAsStopped(t, state, out, producer)

	// The read that decides it is inside the terminal transaction and locks the
	// row, so a Stop cannot land between the read and the write.
	if !state.ran("SELECT status FROM executions WHERE id = $1 FOR UPDATE") {
		t.Fatalf("the execution row was not read under a lock:\n%s", state.all())
	}
	// A stopped run is not judged for dropped rows: the wait for its stats is
	// what held the overwrite back by a minute, and its verdict would be moot.
	if state.mentioned("pipeline_run_table_stats") {
		t.Fatalf("a stopped run waited on the postflight silent-drop check:\n%s", state.all())
	}
}

func TestAFailureAfterAStopIsRecordedAsTheStop(t *testing.T) {
	state := &recordedDB{executionStatus: "cancelled"}
	out, producer := failWith(t, state, "executor: context canceled")
	assertRecordedAsStopped(t, state, out, producer)
}

// Control: a run nobody stopped is recorded exactly as before.
func TestAnUnstoppedRunIsStillRecordedAsItEnded(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		state := &recordedDB{executionStatus: "running"}
		out, producer := finalizeWith(t, state)
		if out.Status != "completed" {
			t.Fatalf("outcome = %q, want completed:\n%s", out.Status, state.all())
		}
		if args, _ := state.argsOf("UPDATE pipelines SET status = $2"); len(args) < 2 || args[1] != "completed" {
			t.Fatalf("pipelines.status written as %v, want completed", args)
		}
		if len(producer.sent) != 0 {
			t.Fatalf("a completed run raised %d alert(s)", len(producer.sent))
		}
	})
	t.Run("failed", func(t *testing.T) {
		state := &recordedDB{executionStatus: "running"}
		out, producer := failWith(t, state, "sink: destination unreachable")
		if out.Status != "failed" {
			t.Fatalf("outcome = %q, want failed:\n%s", out.Status, state.all())
		}
		if len(producer.sent) != 1 {
			t.Fatalf("a failed run raised %d alert(s), want 1", len(producer.sent))
		}
		body, _ := producer.sent[0].Value.Encode()
		if !strings.Contains(string(body), codeRunFailed) {
			t.Fatalf("the alert is not the failure alert: %s", body)
		}
	})
}

// When the execution row cannot be read, nothing is guessed. Before the
// postflight the read is only a shortcut, so the check runs as it always did;
// inside the transaction it is the decision, so the write rolls back and errors
// (Temporal retries it, and the workflow's deferred write is the backstop)
// rather than recording a status it could not check against a Stop.
func TestAnUnreadableExecutionRowIsNotGuessed(t *testing.T) {
	state := &recordedDB{executionStatus: "cancelled", failStatusRead: true}
	producer := withRecordedDB(t, state)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(FinalizeCompletedRunActivity)
	if _, err := env.ExecuteActivity(FinalizeCompletedRunActivity, supPipelineID, stopKeptExecutionID); err == nil {
		t.Fatalf("the terminal write succeeded without knowing whether the run was stopped:\n%s", state.all())
	}
	if !state.mentioned("pipeline_run_table_stats") {
		t.Fatalf("an unreadable row skipped the postflight check:\n%s", state.all())
	}
	if state.ran("UPDATE pipelines") || state.ran("COMMIT") || !state.ran("ROLLBACK") {
		t.Fatalf("the terminal write committed anyway:\n%s", state.all())
	}
	if len(producer.sent) != 0 {
		t.Fatalf("raised %d alert(s) for a write that did not happen", len(producer.sent))
	}
}
