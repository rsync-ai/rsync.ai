package workflows

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	workflowtypes "github.com/rsync-ai/backend-temporal-adapter/internal/workflows/types"
)

// Pins KI-STOP-DOES-NOT-REACH-RUNNING-TRANSFER (workflow half).
//
// StopPipeline sends the "cancel" signal. The cancel goroutine cancels childCtx,
// but the executor activity ran under the workflow's root ctx, so the running
// transfer never heard it: the activity finished, the loop broke on its success,
// FinalizeCompletedRunActivity recorded the run as completed, and the move to
// COMPLETED then failed against the CANCELLED state the goroutine had set. On
// prod the workflow ended Failed with "invalid state transition: CANCELLED ->
// COMPLETED", the pipeline row said completed, and no stop alert went out.
//
// These drive the REAL NLPipelineWorkflowV2 down the correlation path (the live
// one: ExecutorActivityV2) and send the cancel while the executor is running.

// executorHoldFor bounds how long the fake executor runs when nothing cancels
// it. It is what a run that ignores the Stop costs this test.
const executorHoldFor = 300 * time.Millisecond

type stopHarness struct {
	mu            sync.Mutex
	executorRuns  int
	cancelledActs []string
	stateEvents   []string
	statusWrites  []string

	released    chan struct{}
	releaseOnce sync.Once

	// duringExecutor, when set, runs inside the first executor call, before it
	// starts waiting.
	duringExecutor func()
}

func newStopHarness() *stopHarness {
	return &stopHarness{released: make(chan struct{})}
}

func (h *stopHarness) executor(ctx context.Context, _ NLPipelineWorkflowV2Input, _ *WorkflowState) (map[string]interface{}, error) {
	h.mu.Lock()
	h.executorRuns++
	first := h.executorRuns == 1
	hook := h.duringExecutor
	h.mu.Unlock()
	if first && hook != nil {
		hook()
	}
	// The test environment does not cancel an activity's own context when the
	// workflow cancels it; the cancel listener releases it instead.
	select {
	case <-ctx.Done():
	case <-h.released:
	case <-time.After(executorHoldFor):
	}
	return map[string]interface{}{"status": "success", "rows": 10}, nil
}

func (h *stopHarness) stateUpdate(_ context.Context, in StateUpdateInput) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stateEvents = append(h.stateEvents, in.EventType)
	return nil
}

func (h *stopHarness) pipelineStatus(_ context.Context, _, _, status, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statusWrites = append(h.statusWrites, status)
	return nil
}

func (h *stopHarness) domainEvent(_ context.Context, _ map[string]interface{}) error { return nil }

func (h *stopHarness) snapshot() (runs int, cancelled, events, statuses []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.executorRuns,
		append([]string(nil), h.cancelledActs...),
		append([]string(nil), h.stateEvents...),
		append([]string(nil), h.statusWrites...)
}

func runStopDuringExecutor(t *testing.T, setup func(*testsuite.TestWorkflowEnvironment)) (*stopHarness, *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	h := newStopHarness()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartTime(tableSelectionStart)
	env.RegisterWorkflowWithOptions(NLPipelineWorkflowV2, workflow.RegisterOptions{Name: NLPipelineWorkflowV2Name})
	env.RegisterActivityWithOptions(h.executor, activity.RegisterOptions{Name: "ExecutorActivityV2"})
	env.RegisterActivityWithOptions(h.stateUpdate, activity.RegisterOptions{Name: "StateUpdateActivity"})
	env.RegisterActivityWithOptions(h.pipelineStatus, activity.RegisterOptions{Name: "UpdatePipelineStatusActivity"})
	env.RegisterActivityWithOptions(finalizeVia(h.pipelineStatus), activity.RegisterOptions{Name: "FinalizeCompletedRunActivity"})
	env.RegisterActivityWithOptions(h.domainEvent, activity.RegisterOptions{Name: "EmitDomainEventActivity"})
	env.SetOnActivityCanceledListener(func(info *activity.Info) {
		h.mu.Lock()
		h.cancelledActs = append(h.cancelledActs, info.ActivityType.Name)
		h.mu.Unlock()
		h.releaseOnce.Do(func() { close(h.released) })
	})
	// SignalWorkflow only queues a callback, so it is safe from the activity's
	// goroutine; the workflow sees the cancel while the executor is still running.
	h.duringExecutor = func() {
		env.SignalWorkflow(SignalCancel, map[string]interface{}{"action": "stop"})
	}
	if setup != nil {
		setup(env)
	}

	in := tableSelectionInput()
	in.PipelineID, in.ExecutionID = "pipe-stop-1", "exec-stop-1"
	in.ExecutorDispatch = "" // the correlation path, which is the one prod runs
	env.ExecuteWorkflow(NLPipelineWorkflowV2, in)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if runs, _, _, _ := h.snapshot(); runs == 0 {
		t.Fatal("the executor never ran; the test proves nothing")
	}
	return h, env
}

// The fix: the cancel reaches the running executor, and the run ends as stopped —
// no workflow failure, no completed write, no PIPELINE_COMPLETED.
func TestNLPipelineV2_StopDuringExecutorCancelsItAndEndsRunAsStopped(t *testing.T) {
	h, env := runStopDuringExecutor(t, nil)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a user stop must not end the run with an error, got: %v", err)
	}
	runs, cancelled, events, statuses := h.snapshot()
	if countOf(cancelled, "ExecutorActivityV2") != 1 {
		t.Fatalf("the Stop never reached the running executor (cancelled activities %v)", cancelled)
	}
	if runs != 1 {
		t.Fatalf("executor must not run again after a stop, ran %d times", runs)
	}
	if len(statuses) != 1 || statuses[0] != "stopped" {
		t.Fatalf("expected exactly one terminal status write of \"stopped\", got %v", statuses)
	}
	if containsString(events, "PIPELINE_COMPLETED") {
		t.Fatalf("a stopped run announced PIPELINE_COMPLETED (state events %v)", events)
	}
}

// Replay safety: a history recorded before executor-honours-cancel has no marker,
// so the executor must keep running under the root context — cancelling it
// would add a RequestCancelActivityTask command the old history never recorded.
// Only the command sequence is pinned here; what such a run records at the end
// is the finalize write the old code made.
func TestNLPipelineV2_StopDuringExecutorOnDefaultVersionKeepsOldCommands(t *testing.T) {
	h, _ := runStopDuringExecutor(t, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion("executor-honours-cancel", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	})

	_, cancelled, _, statuses := h.snapshot()
	if len(cancelled) != 0 {
		t.Fatalf("an old history cancelled activities %v; replay would diverge", cancelled)
	}
	if !containsString(statuses, "completed") {
		t.Fatalf("an old history must still reach the finalize write, got status writes %v", statuses)
	}
}

// Pins the finalize half of KI-STOP-RACING-COMPLETION-OVERWRITTEN. The Stop's
// executions write landed before the run finished, so the terminal write kept
// the stop and reported "stopped" — the workflow must end there, not announce a
// completion the database no longer records.
func TestNLPipelineV2_FinalizeReportingStoppedEndsTheRun(t *testing.T) {
	h := &postflightOrderHarness{finalize: func() (runOutcome, error) {
		return runOutcome{Status: "stopped"}, nil
	}}
	log := runPostflightOrder(t, h, nil)

	if n := countOf(log, "event:PIPELINE_COMPLETED"); n != 0 {
		t.Fatalf("a run the terminal write recorded as stopped emitted PIPELINE_COMPLETED %d time(s); log %v", n, log)
	}
	if n := countOf(log, "state:PIPELINE_COMPLETED"); n != 0 {
		t.Fatalf("a run the terminal write recorded as stopped wrote a PIPELINE_COMPLETED state %d time(s); log %v", n, log)
	}
	if countOf(log, "event:PIPELINE_FAILED") != 0 {
		t.Fatalf("a stopped run is not a failure; log %v", log)
	}
	for _, s := range log {
		if len(s) > len("status:") && s[:len("status:")] == "status:" {
			t.Fatalf("the deferred updater wrote the status again (%s) after finalize recorded it; log %v", s, log)
		}
	}
}

// The DAG sibling: the scheduler ran its node activities under the root ctx too,
// so a Stop during a DAG run was just as unreachable. This drives the real
// workflow through the agentic phases to a planner that returns a graph.
func TestNLPipelineV2_StopDuringDAGNodeCancelsItAndEndsRunAsStopped(t *testing.T) {
	h := newStopHarness()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartTime(tableSelectionStart)
	env.RegisterWorkflowWithOptions(NLPipelineWorkflowV2, workflow.RegisterOptions{Name: NLPipelineWorkflowV2Name})

	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("src", workflowtypes.NodeKindSource, map[string]interface{}{"table": "orders"}),
		dagNode("dst", workflowtypes.NodeKindDestination, map[string]interface{}{"table": "orders"}),
	}, "src->dst")
	raw, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	var graphMap map[string]interface{}
	if err := json.Unmarshal(raw, &graphMap); err != nil {
		t.Fatal(err)
	}

	nodeRuns := 0
	for name, fn := range map[string]interface{}{
		"IntentActivityV2": func(context.Context, NLPipelineWorkflowV2Input) (map[string]interface{}, error) {
			return map[string]interface{}{"source_type": "source-db", "destination_type": "dest-warehouse"}, nil
		},
		"ConnectorResolverActivityV2": func(context.Context, NLPipelineWorkflowV2Input, *WorkflowState) (map[string]interface{}, error) {
			return map[string]interface{}{
				"resolved_connectors": map[string]interface{}{"source": "source-db", "destination": "dest-warehouse"},
			}, nil
		},
		"ConnectorAvailabilityActivityV2": func(_ context.Context, req ConnectorCheckRequest) (*ConnectorCheckResult, error) {
			return &ConnectorCheckResult{CorrelationID: req.CorrelationID, SourceConnectorAvailable: true,
				DestConnectorAvailable: true, AllConnectorsAvailable: true}, nil
		},
		"ConnectionValidationActivityV2": func(context.Context, ConnectionValidationRequest) (*ConnectionValidationResult, error) {
			return &ConnectionValidationResult{AllConnectionsValid: true}, nil
		},
		"ConnectionValidatorActivityV2": func(context.Context, NLPipelineWorkflowV2Input, *WorkflowState) error { return nil },
		"PlannerActivityV2": func(context.Context, NLPipelineWorkflowV2Input, *WorkflowState) (map[string]interface{}, error) {
			return map[string]interface{}{"workflow_graph": graphMap}, nil
		},
		"CostEstimatorActivityV2": func(context.Context, NLPipelineWorkflowV2Input, *WorkflowState) (map[string]interface{}, error) {
			return nil, nil
		},
		"ValidatorActivityV2": func(context.Context, NLPipelineWorkflowV2Input, *WorkflowState) (map[string]interface{}, error) {
			return map[string]interface{}{"valid": true}, nil
		},
		"ExecuteGraphNodeActivityV2": func(ctx context.Context, _ ExecuteGraphNodeInput) (map[string]interface{}, error) {
			h.mu.Lock()
			nodeRuns++
			first := nodeRuns == 1
			h.mu.Unlock()
			if first {
				env.SignalWorkflow(SignalCancel, map[string]interface{}{"action": "stop"})
			}
			select {
			case <-ctx.Done():
			case <-h.released:
			case <-time.After(executorHoldFor):
			}
			return map[string]interface{}{"status": "ok"}, nil
		},
		"StateUpdateActivity":          h.stateUpdate,
		"UpdatePipelineStatusActivity": h.pipelineStatus,
		"FinalizeCompletedRunActivity": finalizeVia(h.pipelineStatus),
		"EmitDomainEventActivity":      h.domainEvent,
	} {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	env.SetOnActivityCanceledListener(func(info *activity.Info) {
		h.mu.Lock()
		h.cancelledActs = append(h.cancelledActs, info.ActivityType.Name)
		h.mu.Unlock()
		h.releaseOnce.Do(func() { close(h.released) })
	})

	in := agenticInput()
	in.PipelineID, in.ExecutionID = "pipe-stop-dag-1", "exec-stop-dag-1"
	in.ExecutorDispatch = ""
	env.ExecuteWorkflow(NLPipelineWorkflowV2, in)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	_, cancelled, events, statuses := h.snapshot()
	h.mu.Lock()
	ran := nodeRuns
	h.mu.Unlock()
	if ran == 0 {
		t.Fatalf("no DAG node ran (state events %v, workflow error %v); the test proves nothing", events, env.GetWorkflowError())
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a user stop must not end the run with an error, got: %v", err)
	}
	if countOf(cancelled, "ExecuteGraphNodeActivityV2") != 1 {
		t.Fatalf("the Stop never reached the running DAG node (cancelled activities %v)", cancelled)
	}
	if ran != 1 {
		t.Fatalf("the DAG dispatched %d nodes after the stop, want only the one already running", ran)
	}
	if len(statuses) != 1 || statuses[0] != "stopped" {
		t.Fatalf("expected exactly one terminal status write of \"stopped\", got %v", statuses)
	}
	if containsString(events, "PIPELINE_COMPLETED") {
		t.Fatalf("a stopped DAG run announced PIPELINE_COMPLETED (state events %v)", events)
	}
}
