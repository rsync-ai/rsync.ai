package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/internal/agents/executor"
	"github.com/rsync-ai/backend-orchestrator/internal/runcancel"
)

// Pins KI-DESTINATION-WRITE-UNCONFIRMED-RETRIED.
//
// A silent-drop verdict is the executor's own finding that rows did not land.
// The asynchronous healer does not retry one (pkg/diagnose/diagnose.go:168 maps
// both statuses to regenerate-connector or escalate), but this synchronous
// classifier only knew the partial-drop spelling. "destination write
// unconfirmed" and "destination silently dropped all rows" matched no rule, fell
// to the attemptCount<2 default, and came back retry_with_backoff: the whole
// transfer was exported again into a destination that may already hold the
// rows. On prod it re-ran a Mongo→GCS transfer the user had stopped.
func TestSilentDropVerdictsAreNeverRetried(t *testing.T) {
	// Quoted from the sites that emit them (agents/executor/silent_drop_check.go),
	// so a reworded reason breaks this test instead of silently reopening the bug.
	cases := []struct {
		name, status, reason string
	}{
		{"write unconfirmed (sink stalled)", "silent_drop_detected",
			"destination write unconfirmed: dispatched 20000 rows via sink but no acks were recorded within the reconcile deadline (execution 0f6c) — failing closed as a total drop; sink error: no destination acks recorded within the reconcile deadline — sink stalled or destination unreachable"},
		// The sink error is free text: a timeout in it must not win the race to
		// the transient-network rule below it.
		{"write unconfirmed, sink error says timeout", "silent_drop_detected",
			"destination write unconfirmed: dispatched 20000 rows via sink but no acks were recorded within the reconcile deadline (execution 0f6c) — failing closed as a total drop; sink error: i/o timeout"},
		{"all rows dropped per ack ledger", "silent_drop_detected",
			"destination silently dropped all rows: dispatched 500, ack ledger confirmed 0 landed across 3 ack batches (execution 0f6c)"},
		{"all rows refused per ack ledger, the sink said why", "silent_drop_detected",
			"destination refused all rows: dispatched 10, ack ledger confirmed 0 landed across 1 ack batches (execution 0f6c); sink error: dest error: 10 of 10 rows refused: HTTP 400"},
		// A caller that passes only the message must still read it as a verdict.
		{"all rows refused, message only", "",
			"destination refused all rows: dispatched 10, ack ledger confirmed 0 landed across 1 ack batches (execution 0f6c); sink error: i/o timeout"},
		{"read N wrote 0", "silent_drop_detected",
			"destination silently dropped all rows: read 500 from source, wrote 0 to destination"},
		{"connector returned 0 but source has rows", "silent_drop_detected",
			"connector returned 0 rows but source actually has 500 events rows — likely a connector bug or auth/scope misconfiguration"},
		{"nothing reached the destination lane", "silent_drop_detected",
			"no data reached the destination lane: all 500 row(s) read from source failed to produce to Kafka across 2 batches; these rows are not retried"},
		{"partial drop, 5% rule", "silent_partial_drop_detected",
			"destination silently dropped some rows: read 1000 from source, wrote 900 to destination (10.0% missing)"},
		// The status alone must be enough: an empty reason is how a new emitting
		// site would most likely look.
		{"status with no reason", "silent_drop_detected", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorContext := map[string]interface{}{
				"error_message":   tc.reason,
				"executor_status": tc.status,
				"attempt_count":   0,
			}
			got := (&ExecutorWorker{}).suggestRecoveryAction(context.Background(), errorContext)
			if got != "fail" {
				t.Fatalf("suggestRecoveryAction = %q, want fail — a silent-drop verdict re-exported "+
					"the whole transfer (reasoning=%v)", got, errorContext["reasoning"])
			}
		})
	}
}

// executeWithHealer must pass the executor's status to the classifier, or the
// status rule above can never fire on the real path.
func TestHealerHandsTheExecutorStatusToTheClassifier(t *testing.T) {
	calls := 0
	w := &ExecutorWorker{runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
		calls++
		return executor.ExecutorResponse{Status: "silent_drop_detected", Error: ""}
	}}
	w.executeWithHealer(context.Background(), stopTestTask(), 0)
	if calls != 1 {
		t.Fatalf("executor ran %d times for a silent-drop verdict with no reason text, want 1", calls)
	}
}

const (
	stopTestPipelineID  = "637a0df2-6a1e-4c55-9d7b-0b1c2d3e4f50"
	stopTestExecutionID = "9a1b2c3d-0000-4000-8000-000000000001"
)

func stopTestTask() executor.ExecutorTask {
	return executor.ExecutorTask{
		TaskID:     "executor-stoptest",
		PipelineID: stopTestPipelineID,
		Params:     map[string]interface{}{"batch_size": 1000},
		Payload:    map[string]interface{}{"execution_id": stopTestExecutionID},
	}
}

// Pins KI-STOP-DOES-NOT-REACH-RUNNING-TRANSFER (orchestrator half).
//
// The healer re-executes the whole task on retry_with_backoff and
// retry_smaller_batch. It never asked whether the run it was about to re-run had
// been stopped, so a Stop that landed while the first attempt was failing was
// followed by a complete second export — double-writing into the destination
// after the user was told the pipeline had stopped.
func TestHealerNeverReRunsAStoppedRun(t *testing.T) {
	cases := []struct {
		name string
		// failure is chosen so the classifier WOULD retry it; a case whose
		// failure the classifier refuses anyway would pass for the wrong reason.
		failure      string
		stopMidRun   bool // Stop cancels the run's context while it runs
		rowCancelled bool // the Stop's executions write is visible in the DB
		wantRuns     int
		wantStatus   string
	}{
		{"stop cancels the run context", "read tcp 10.0.0.4:5432: i/o timeout", true, false, 1, "cancelled"},
		{"stop recorded on the execution row", "read tcp 10.0.0.4:5432: i/o timeout", false, true, 1, "cancelled"},
		{"stop before a smaller-batch retry", "response too large for a single batch", false, true, 1, "cancelled"},
		// Control: the same failure on a run nobody stopped is still retried, so
		// the cases above cannot pass merely because retrying broke.
		{"control: unstopped run is still retried", "read tcp 10.0.0.4:5432: i/o timeout", false, false, 2, "failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			mock.MatchExpectationsInOrder(false)
			for i := 0; i < 3; i++ {
				mock.ExpectQuery("SELECT EXISTS").WithArgs(stopTestExecutionID).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.rowCancelled))
			}

			ctx, done := newExecutorRunContext(context.Background(), stopTestPipelineID, time.Hour)
			defer done()

			var mu sync.Mutex
			runs := 0
			w := &ExecutorWorker{db: db, runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
				mu.Lock()
				runs++
				first := runs == 1
				mu.Unlock()
				if first && tc.stopMidRun {
					if n := runcancel.Cancel(stopTestPipelineID); n != 1 {
						t.Errorf("Stop reached %d running executor(s), want 1", n)
					}
				}
				return executor.ExecutorResponse{Status: "failed", Error: tc.failure}
			}}

			resp := w.executeWithHealer(ctx, stopTestTask(), 0)
			if runs != tc.wantRuns {
				t.Fatalf("executor ran %d times, want %d — a stopped run was exported again", runs, tc.wantRuns)
			}
			if resp.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
		})
	}
}

// The probe is best effort: a database it cannot reach must not turn every
// failure into "stopped", or a flaky DB would silently disable healing.
func TestHealerStopProbeFailsOpen(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < 3; i++ {
		mock.ExpectQuery("SELECT EXISTS").WillReturnError(errors.New("connection refused"))
	}
	runs := 0
	w := &ExecutorWorker{db: db, runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
		runs++
		return executor.ExecutorResponse{Status: "failed", Error: "i/o timeout"}
	}}
	w.executeWithHealer(context.Background(), stopTestTask(), 0)
	if runs != 2 {
		t.Fatalf("executor ran %d times, want 2 — an unreachable stop probe must not block the retry", runs)
	}
}

// A run whose own deadline expired is over too. Re-running it hands the executor
// an already-dead context, which answers "cancelled" at once — reporting a
// timeout as a user stop.
func TestHealerDoesNotReRunAnExpiredRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs := 0
	w := &ExecutorWorker{runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
		runs++
		return executor.ExecutorResponse{Status: "failed", Error: "i/o timeout"}
	}}
	resp := w.executeWithHealer(ctx, stopTestTask(), 0)
	if runs != 1 {
		t.Fatalf("executor ran %d times on a context that was already over, want 1", runs)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed — only a user stop may report cancelled", resp.Status)
	}
}

// processCorrelationRequest runs the executor under this context. The Stop
// handler reaches it through runcancel, and only while the run is in flight.
func TestExecutorRunContextIsReachableByStop(t *testing.T) {
	ctx, done := newExecutorRunContext(context.Background(), stopTestPipelineID, time.Hour)
	if n := runcancel.Cancel(stopTestPipelineID); n != 1 {
		t.Fatalf("Cancel reached %d runs, want 1", n)
	}
	if ctx.Err() == nil {
		t.Fatal("the run's context is still live after Stop")
	}
	if !errors.Is(context.Cause(ctx), runcancel.ErrStopped) {
		t.Fatalf("cause = %v, want runcancel.ErrStopped so the run reports cancelled, not failed", context.Cause(ctx))
	}
	done()
	if n := runcancel.Cancel(stopTestPipelineID); n != 0 {
		t.Fatalf("a finished run is still registered (%d)", n)
	}
}
