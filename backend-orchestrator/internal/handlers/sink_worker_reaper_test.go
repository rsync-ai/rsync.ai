package handlers

// Tests for the orphan sink-worker reaper (KI-CDC-SINK-WORKER-NO-REAPER). The bug class:
// a sink-worker stop that fails during a pipeline Stop or Delete is never tried again,
// so the worker keeps writing for a pipeline that is gone. The safety half matters as
// much: the reaper must never stop a worker unless the database positively says its
// pipeline is gone.

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

const (
	reaperOrphanPID  = "abd8a64d-1f2e-4c3b-9a7d-5e6f70819234"
	reaperRunningPID = "c228373b-0000-4000-8000-000000000001" // stands in for the kept prod pipeline
	reaperOrphanGrp  = "sink-abd8a64d"
	reaperRunningGrp = "sink-c228373b-stream"
	reaperOwnerQuery = `SELECT status, updated_at < NOW\(\) - make_interval\(secs => \$2\) FROM pipelines WHERE id = \$1::uuid`
)

// fakeReaperSinks answers list_sinks with workers and records stop_sink calls.
type fakeReaperSinks struct {
	mu       sync.Mutex
	workers  []map[string]interface{}
	listErr  error
	stopFail func(group string) string
	stops    []string
	stopCfgs []map[string]interface{}
}

func (f *fakeReaperSinks) ExecuteWithContext(_ context.Context, req mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch req.Operation {
	case "list_sinks":
		if f.listErr != nil {
			return nil, f.listErr
		}
		ws := make([]interface{}, 0, len(f.workers))
		for _, w := range f.workers {
			ws = append(ws, w)
		}
		return &mcp.ExecuteResponse{Success: true, Result: map[string]interface{}{"workers": ws}}, nil
	case "stop_sink":
		cfg, _ := req.Params["config"].(map[string]interface{})
		group, _ := cfg["consumer_group"].(string)
		f.stops = append(f.stops, group)
		f.stopCfgs = append(f.stopCfgs, cfg)
		if f.stopFail != nil {
			if why := f.stopFail(group); why != "" {
				return &mcp.ExecuteResponse{Success: false, Error: why}, nil
			}
		}
		return &mcp.ExecuteResponse{Success: true}, nil
	}
	return &mcp.ExecuteResponse{Success: false, Error: "unexpected operation " + req.Operation}, nil
}

func (f *fakeReaperSinks) stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stops...)
}

func reaperWorker(group, pid string) map[string]interface{} {
	return map[string]interface{}{"consumer_group": group, "pipeline_id": pid}
}

func newReaperDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func expectOwnerDeleted(mock sqlmock.Sqlmock, pid string) {
	mock.ExpectQuery(reaperOwnerQuery).WithArgs(pid, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "settled"}))
}

func expectOwnerStatus(mock sqlmock.Sqlmock, pid, status string, settled bool) {
	mock.ExpectQuery(reaperOwnerQuery).WithArgs(pid, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "settled"}).AddRow(status, settled))
}

func reaperCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A delete whose stop_sink failed left a worker running. The reaper stops it on the
// second look — and never touches the running pipeline's worker beside it.
func TestSinkWorkerReaper_StopsTheWorkerOfADeletedPipelineAndNothingElse(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{
		reaperWorker(reaperOrphanGrp, reaperOrphanPID),
		reaperWorker(reaperRunningGrp, reaperRunningPID),
	}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)

	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerStatus(mock, reaperRunningPID, "running", true)
	if acts := r.Tick(ctx); len(acts) != 0 || len(sinks.stopped()) != 0 {
		t.Fatalf("first look must only record a suspect; actions=%+v stops=%v", acts, sinks.stopped())
	}

	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID) // the re-check right before the stop
	expectOwnerStatus(mock, reaperRunningPID, "running", true)
	acts := r.Tick(ctx)
	if got := sinks.stopped(); !reflect.DeepEqual(got, []string{reaperOrphanGrp}) {
		t.Fatalf("stop_sink calls = %v, want only the deleted pipeline's group %q", got, reaperOrphanGrp)
	}
	if len(acts) != 1 || !acts[0].Stopped || acts[0].Reason != "pipeline deleted" {
		t.Fatalf("actions = %+v", acts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The retry: a stop that fails is tried again on the next tick until it lands.
func TestSinkWorkerReaper_RetriesAStopThatFailed(t *testing.T) {
	db, mock := newReaperDB(t)
	failures := 1
	sinks := &fakeReaperSinks{
		workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)},
		stopFail: func(string) string {
			if failures > 0 {
				failures--
				return "worker did not exit after SIGTERM"
			}
			return ""
		},
	}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)

	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID)
	if acts := r.Tick(ctx); len(acts) != 1 || acts[0].Stopped || acts[0].Failure == "" {
		t.Fatalf("second tick should try and fail; actions=%+v", acts)
	}
	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID)
	if acts := r.Tick(ctx); len(acts) != 1 || !acts[0].Stopped {
		t.Fatalf("third tick should retry and stop; actions=%+v", acts)
	}
	if got := sinks.stopped(); len(got) != 2 {
		t.Fatalf("stop_sink calls = %v, want 2 (the failure, then the retry)", got)
	}
}

// A CDC Stop whose stop_sink failed: status 'stopped' past the grace period is gone.
func TestSinkWorkerReaper_StopsTheWorkerOfAPipelineStoppedPastTheGrace(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
	r.Tick(ctx)
	expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
	expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
	if acts := r.Tick(ctx); len(acts) != 1 || !acts[0].Stopped || acts[0].Reason != "pipeline stopped" {
		t.Fatalf("actions = %+v", acts)
	}
}

// Every answer that is not a positive "gone" keeps the worker, across many ticks.
func TestSinkWorkerReaper_NeverStopsWithoutAPositiveGoneAnswer(t *testing.T) {
	cases := []struct {
		name   string
		pid    string
		expect func(sqlmock.Sqlmock)
	}{
		{"running pipeline", reaperRunningPID, func(m sqlmock.Sqlmock) { expectOwnerStatus(m, reaperRunningPID, "running", true) }},
		{"active pipeline", reaperRunningPID, func(m sqlmock.Sqlmock) { expectOwnerStatus(m, reaperRunningPID, "active", true) }},
		{"paused pipeline", reaperRunningPID, func(m sqlmock.Sqlmock) { expectOwnerStatus(m, reaperRunningPID, "paused", true) }},
		{"stopped inside the grace period", reaperRunningPID, func(m sqlmock.Sqlmock) { expectOwnerStatus(m, reaperRunningPID, "stopped", false) }},
		{"lookup error", reaperRunningPID, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(reaperOwnerQuery).WillReturnError(errors.New("connection refused"))
		}},
		{"pipeline id is not a UUID", "sink-legacy", func(sqlmock.Sqlmock) {}},
		{"no pipeline id", "", func(sqlmock.Sqlmock) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newReaperDB(t)
			sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperRunningGrp, tc.pid)}}
			r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
			for i := 0; i < 3; i++ {
				tc.expect(mock)
				r.Tick(reaperCtx(t))
			}
			if got := sinks.stopped(); len(got) != 0 {
				t.Fatalf("stopped %v; a worker must be kept unless the DB says its pipeline is gone", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A pipeline started between the second look and the stop is caught by the re-check.
func TestSinkWorkerReaper_RechecksRightBeforeStopping(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
	r.Tick(ctx)
	expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
	expectOwnerStatus(mock, reaperOrphanPID, "running", true)
	r.Tick(ctx)
	if got := sinks.stopped(); len(got) != 0 {
		t.Fatalf("stopped %v after the re-check said running", got)
	}
}

// A failed listing forgets the suspects: two SUCCESSFUL looks in a row are required.
func TestSinkWorkerReaper_AFailedListingResetsTheSightings(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	sinks.listErr = errors.New("sink service unreachable")
	r.Tick(ctx)
	sinks.listErr = nil
	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	if got := sinks.stopped(); len(got) != 0 {
		t.Fatalf("stopped %v after a failed listing broke the run of sightings", got)
	}
}

// Dry run reports and never stops.
func TestSinkWorkerReaper_DryRunNeverStops(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperDryRun, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	var acts []sinkReapAction
	for i := 0; i < 3; i++ {
		expectOwnerDeleted(mock, reaperOrphanPID)
		acts = r.Tick(ctx)
	}
	if got := sinks.stopped(); len(got) != 0 {
		t.Fatalf("dry run stopped %v", got)
	}
	if len(acts) != 1 || acts[0].Stopped || acts[0].ConsumerGroup != reaperOrphanGrp {
		t.Fatalf("dry run should report the orphan; actions=%+v", acts)
	}
}

// On by default: a worker whose pipeline is gone is stopped unless the operator turns
// the reaper off. "off" and its spellings turn it off; a typo is off too, so a
// misspelled mode never arms something the operator did not ask for.
func TestSinkWorkerReaper_ModeDefaultsEnforce(t *testing.T) {
	for in, want := range map[string]sinkReaperMode{
		"": sinkReaperEnforce, "  ": sinkReaperEnforce, " enforce ": sinkReaperEnforce,
		"off": sinkReaperOff, "OFF": sinkReaperOff, "false": sinkReaperOff, "0": sinkReaperOff,
		"disabled": sinkReaperOff, "none": sinkReaperOff, "enforced": sinkReaperOff, "true": sinkReaperOff,
		"dry_run": sinkReaperDryRun, "DRY-RUN": sinkReaperDryRun,
	} {
		if got := parseSinkReaperMode(in); got != want {
			t.Errorf("parseSinkReaperMode(%q) = %v, want %v", in, got, want)
		}
	}
	t.Setenv("SINK_WORKER_REAPER_MODE", "")
	r := NewSinkWorkerReaperFromEnv(&sql.DB{}, mcp.NewServerManager(t.TempDir()))
	if r == nil || r.Mode() != "enforce" {
		t.Fatalf("an unset SINK_WORKER_REAPER_MODE must start the reaper in enforce mode; got %v", r)
	}
	t.Setenv("SINK_WORKER_REAPER_MODE", "off")
	if r := NewSinkWorkerReaperFromEnv(&sql.DB{}, mcp.NewServerManager(t.TempDir())); r != nil {
		t.Fatal("SINK_WORKER_REAPER_MODE=off must start no reaper")
	}
}

// --- Batch workers: a finished batch run's worker is stopped -----------------------------
//
// The bug class: a batch run starts a sink worker and nothing ever stops it. The run
// completes, the pipeline reads 'completed', and the worker sits in its consumer group
// holding a process, a metrics port and a broker session until the sink service
// restarts. The reaper now stops it, with the same care as an orphan: two looks, a
// fresh look right before the stop, and a stop the sink service refuses when a newer
// run has taken the worker over.

const (
	reaperBatchPID   = "59bd3130-0000-4000-8000-000000000001"
	reaperBatchGrp   = "rsync.sink-59bd3130-batch"
	reaperBatchExec  = "e-1"
	reaperActiveExec = `SELECT EXISTS\(SELECT 1 FROM executions WHERE pipeline_id = \$1::uuid AND status IN \('running', 'pending'\)\)`
)

func reaperBatchWorker(execID string) map[string]interface{} {
	w := reaperWorker(reaperBatchGrp, reaperBatchPID)
	w["execution_id"] = execID
	return w
}

func expectActiveExecution(mock sqlmock.Sqlmock, pid string, active bool) {
	mock.ExpectQuery(reaperActiveExec).WithArgs(pid).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(active))
}

// One look at a finished batch pipeline: settled 'completed' (or 'failed') and no run
// in flight.
func expectBatchFinished(mock sqlmock.Sqlmock, status string) {
	expectOwnerStatus(mock, reaperBatchPID, status, true)
	expectActiveExecution(mock, reaperBatchPID, false)
}

func TestSinkWorkerReaper_StopsTheWorkerOfAFinishedBatchRun(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			db, mock := newReaperDB(t)
			sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperBatchWorker(reaperBatchExec)}}
			r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
			ctx := reaperCtx(t)

			expectBatchFinished(mock, status)
			if acts := r.Tick(ctx); len(acts) != 0 || len(sinks.stopped()) != 0 {
				t.Fatalf("first look must only record a suspect; actions=%+v stops=%v", acts, sinks.stopped())
			}
			expectBatchFinished(mock, status)
			expectBatchFinished(mock, status) // the look right before the stop
			acts := r.Tick(ctx)
			if len(acts) != 1 || !acts[0].Stopped || acts[0].Reason != "batch run finished" {
				t.Fatalf("second look must stop the finished run's worker; actions=%+v", acts)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The stop names the run the reaper saw, so the sink service can refuse it if a newer
// run has reused the worker in between.
func TestSinkWorkerReaper_StopNamesTheRunItSaw(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperBatchWorker(reaperBatchExec)}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	expectBatchFinished(mock, "completed")
	r.Tick(ctx)
	expectBatchFinished(mock, "completed")
	expectBatchFinished(mock, "completed")
	r.Tick(ctx)
	if len(sinks.stopCfgs) != 1 {
		t.Fatalf("want one stop, got %v", sinks.stopped())
	}
	if got := sinks.stopCfgs[0][sinkStopOnlyIfExecutionKey]; got != reaperBatchExec {
		t.Fatalf("the stop must be conditional on run %q; config=%v", reaperBatchExec, sinks.stopCfgs[0])
	}
}

// The sink service answered that a newer run owns the worker: that is not a failure to
// retry, it is a worker to keep.
func TestSinkWorkerReaper_KeepsAWorkerANewerRunTookOver(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{
		workers: []map[string]interface{}{reaperBatchWorker(reaperBatchExec)},
		stopFail: func(string) string {
			return sinkWorkerNotOwner + ": e-2"
		},
	}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	expectBatchFinished(mock, "completed")
	r.Tick(ctx)
	expectBatchFinished(mock, "completed")
	expectBatchFinished(mock, "completed")
	acts := r.Tick(ctx)
	if len(acts) != 1 || acts[0].Stopped || acts[0].Failure != "" || !acts[0].Superseded {
		t.Fatalf("a worker a newer run owns must be kept, not failed; actions=%+v", acts)
	}
	if r.suspects[reaperBatchGrp] != 0 {
		t.Fatalf("a kept worker must start its sightings over; suspects=%v", r.suspects)
	}
}

// Every other state keeps a batch worker: a run in flight, a pipeline that finished
// inside the grace period, a status the rule does not know, and a worker listed without
// the run that owns it (an older sink image, which cannot refuse a stale stop).
func TestSinkWorkerReaper_KeepsABatchWorkerWhenInDoubt(t *testing.T) {
	cases := []struct {
		name   string
		worker map[string]interface{}
		expect func(sqlmock.Sqlmock)
	}{
		{"a run in flight", reaperBatchWorker(reaperBatchExec), func(m sqlmock.Sqlmock) {
			expectOwnerStatus(m, reaperBatchPID, "completed", true)
			expectActiveExecution(m, reaperBatchPID, true)
		}},
		{"finished inside the grace", reaperBatchWorker(reaperBatchExec), func(m sqlmock.Sqlmock) {
			expectOwnerStatus(m, reaperBatchPID, "completed", false)
		}},
		{"running", reaperBatchWorker(reaperBatchExec), func(m sqlmock.Sqlmock) {
			expectOwnerStatus(m, reaperBatchPID, "running", true)
		}},
		{"the executions lookup fails", reaperBatchWorker(reaperBatchExec), func(m sqlmock.Sqlmock) {
			expectOwnerStatus(m, reaperBatchPID, "completed", true)
			m.ExpectQuery(reaperActiveExec).WithArgs(reaperBatchPID).WillReturnError(errors.New("boom"))
		}},
		{"no owning run listed", reaperBatchWorker(""), func(m sqlmock.Sqlmock) {
			expectOwnerStatus(m, reaperBatchPID, "completed", true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newReaperDB(t)
			sinks := &fakeReaperSinks{workers: []map[string]interface{}{tc.worker}}
			r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
			ctx := reaperCtx(t)
			// Not even a sighting: a look that only kept the worker because the mock ran
			// out of rows (the re-check before a stop) would pass a stops-only check.
			for i := 0; i < 3; i++ {
				tc.expect(mock)
				r.Tick(ctx)
				if n := r.suspects[reaperBatchGrp]; n != 0 {
					t.Fatalf("look %d counted a kept worker as a suspect (%d sightings)", i+1, n)
				}
			}
			if got := sinks.stopped(); len(got) != 0 {
				t.Fatalf("kept worker was stopped: %v", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A CDC worker is never a batch worker: a 'completed' status does not stop it (only a
// deleted or settled-stopped pipeline does, as before).
func TestSinkWorkerReaper_TheBatchRuleIsOnlyForBatchGroups(t *testing.T) {
	db, mock := newReaperDB(t)
	w := reaperWorker(reaperOrphanGrp, reaperOrphanPID)
	w["execution_id"] = reaperBatchExec
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{w}}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute)
	ctx := reaperCtx(t)
	for i := 0; i < 3; i++ {
		expectOwnerStatus(mock, reaperOrphanPID, "completed", true)
		r.Tick(ctx)
	}
	if got := sinks.stopped(); len(got) != 0 {
		t.Fatalf("a CDC worker was stopped on 'completed': %v", got)
	}
}

// --- A deleted pipeline's topics are deleted once its last worker is stopped -------------
//
// KI-CDC-ORPHAN-TOPICS-RECREATED: the delete tears the topics down while a worker whose
// stop failed is still reading them, and a live reader's metadata requests re-create
// each topic, empty, under the broker's auto-create default. Topic cleanup is only
// final once no reader is left, so the reaper runs it again after it stops the last one.

type fakeReaperCleanup struct {
	mu    sync.Mutex
	calls []string
	errs  func(call int) []string
}

func (f *fakeReaperCleanup) run(_ context.Context, pipelineID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pipelineID)
	if f.errs != nil {
		return f.errs(len(f.calls))
	}
	return nil
}

func (f *fakeReaperCleanup) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Two looks at the orphan of a deleted pipeline, then the stop.
func reapDeletedOrphan(t *testing.T, mock sqlmock.Sqlmock, r *SinkWorkerReaper) []sinkReapAction {
	t.Helper()
	ctx := reaperCtx(t)
	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID)
	return r.Tick(ctx)
}

func TestSinkWorkerReaper_CleansUpADeletedPipelinesTopicsAfterItsLastWorker(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	cleanup := &fakeReaperCleanup{}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute).WithKafkaCleanup(cleanup.run)

	acts := reapDeletedOrphan(t, mock, r)
	if len(acts) != 1 || !acts[0].Stopped {
		t.Fatalf("the orphan must be stopped; actions=%+v", acts)
	}
	if got := cleanup.called(); !reflect.DeepEqual(got, []string{reaperOrphanPID}) {
		t.Fatalf("the deleted pipeline's topics must be cleaned up once its worker is gone; calls=%v", got)
	}

	// Once is enough: the next tick does not clean up again.
	sinks.workers = nil
	r.Tick(reaperCtx(t))
	if got := cleanup.called(); len(got) != 1 {
		t.Fatalf("cleanup ran again after it succeeded: %v", got)
	}
}

// A second worker of the same pipeline is still reading (its stop failed): cleaning up
// now would only have it re-create the topics, so the cleanup waits for it.
func TestSinkWorkerReaper_TopicCleanupWaitsForEveryReader(t *testing.T) {
	db, mock := newReaperDB(t)
	const second = "sink-abd8a64d-stream"
	sinks := &fakeReaperSinks{
		workers: []map[string]interface{}{
			reaperWorker(reaperOrphanGrp, reaperOrphanPID),
			reaperWorker(second, reaperOrphanPID),
		},
		stopFail: func(group string) string {
			if group == second {
				return "sink service busy"
			}
			return ""
		},
	}
	cleanup := &fakeReaperCleanup{}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute).WithKafkaCleanup(cleanup.run)
	ctx := reaperCtx(t)

	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	for i := 0; i < 4; i++ {
		expectOwnerDeleted(mock, reaperOrphanPID)
	}
	r.Tick(ctx) // stops the first, fails on the second
	if got := cleanup.called(); len(got) != 0 {
		t.Fatalf("cleanup ran while a reader was still up: %v", got)
	}

	// Next tick: the first is gone from the listing, the second stops now.
	sinks.workers = sinks.workers[1:]
	sinks.stopFail = nil
	expectOwnerDeleted(mock, reaperOrphanPID)
	expectOwnerDeleted(mock, reaperOrphanPID)
	r.Tick(ctx)
	if got := cleanup.called(); !reflect.DeepEqual(got, []string{reaperOrphanPID}) {
		t.Fatalf("cleanup must run once the last reader is stopped; calls=%v", got)
	}
}

// A cleanup that fails is tried again on the next ticks, a bounded number of times.
func TestSinkWorkerReaper_RetriesAFailedTopicCleanupABoundedNumberOfTimes(t *testing.T) {
	db, mock := newReaperDB(t)
	sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
	cleanup := &fakeReaperCleanup{errs: func(int) []string { return []string{"broker unreachable"} }}
	r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute).WithKafkaCleanup(cleanup.run)

	reapDeletedOrphan(t, mock, r)
	sinks.workers = nil
	for i := 0; i < 5; i++ {
		r.Tick(reaperCtx(t))
	}
	if got := len(cleanup.called()); got != sinkReaperCleanupAttempts {
		t.Fatalf("a failing cleanup must be tried %d times, then given up; tried %d", sinkReaperCleanupAttempts, got)
	}
}

// Only a deleted pipeline's topics are cleaned up. A stopped pipeline keeps its topics
// (it resumes from them), and a dry run touches nothing.
func TestSinkWorkerReaper_NeverCleansUpTopicsOfAStoppedPipelineOrInADryRun(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		db, mock := newReaperDB(t)
		sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
		cleanup := &fakeReaperCleanup{}
		r := newSinkWorkerReaper(db, sinks, sinkReaperEnforce, time.Minute, time.Minute).WithKafkaCleanup(cleanup.run)
		ctx := reaperCtx(t)
		for i := 0; i < 3; i++ {
			expectOwnerStatus(mock, reaperOrphanPID, "stopped", true)
		}
		r.Tick(ctx)
		if acts := r.Tick(ctx); len(acts) != 1 || !acts[0].Stopped {
			t.Fatalf("the stopped pipeline's worker must be stopped; actions=%+v", acts)
		}
		sinks.workers = nil
		r.Tick(ctx)
		if got := cleanup.called(); len(got) != 0 {
			t.Fatalf("a stopped pipeline's topics were cleaned up: %v", got)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		db, mock := newReaperDB(t)
		sinks := &fakeReaperSinks{workers: []map[string]interface{}{reaperWorker(reaperOrphanGrp, reaperOrphanPID)}}
		cleanup := &fakeReaperCleanup{}
		r := newSinkWorkerReaper(db, sinks, sinkReaperDryRun, time.Minute, time.Minute).WithKafkaCleanup(cleanup.run)
		for i := 0; i < 3; i++ {
			expectOwnerDeleted(mock, reaperOrphanPID)
			r.Tick(reaperCtx(t))
		}
		if got := cleanup.called(); len(got) != 0 {
			t.Fatalf("a dry run cleaned up topics: %v", got)
		}
	})
}
