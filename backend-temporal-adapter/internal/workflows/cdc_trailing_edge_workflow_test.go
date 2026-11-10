package workflows

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// CDC trailing-edge sweep tests. Which pipelines fire, and when, is decided and tested
// in the gateway (handlers/cdc_trailing_edge_test.go). What is guarded here is the
// loop: that it waits before its first sweep, cannot be stopped by a failing gateway,
// and keeps its interval across its history bound. Any of those failing would stop
// every model and workflow after a CDC pipeline, with no error anywhere.
//
// The probe replaces SweepCDCTrailingEdgeActivity under the same name, so nothing here
// reaches an api-gateway.

type cdcTrailingEdgeProbe struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *cdcTrailingEdgeProbe) sweep(context.Context) (CDCTrailingEdgeSweepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return CDCTrailingEdgeSweepResult{}, p.err
	}
	return CDCTrailingEdgeSweepResult{Scanned: 2, Fired: 1}, nil
}

func (p *cdcTrailingEdgeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newCDCTrailingEdgeEnv(probe *cdcTrailingEdgeProbe) *testsuite.TestWorkflowEnvironment {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CDCTrailingEdgeWorkflow)
	env.RegisterActivityWithOptions(probe.sweep, activity.RegisterOptions{Name: "SweepCDCTrailingEdgeActivity"})
	return env
}

func TestCDCTrailingEdgeSweep_WaitsOneIntervalThenSweepsEveryInterval(t *testing.T) {
	probe := &cdcTrailingEdgeProbe{}
	env := newCDCTrailingEdgeEnv(probe)

	var before, after, afterThree int
	env.RegisterDelayedCallback(func() { before = probe.count() }, CDCTrailingEdgeSweepInterval-time.Second)
	env.RegisterDelayedCallback(func() { after = probe.count() }, CDCTrailingEdgeSweepInterval+time.Second)
	env.RegisterDelayedCallback(func() { afterThree = probe.count() }, 3*CDCTrailingEdgeSweepInterval+time.Second)
	env.RegisterDelayedCallback(env.CancelWorkflow, 3*CDCTrailingEdgeSweepInterval+2*time.Second)

	env.ExecuteWorkflow(CDCTrailingEdgeWorkflow, CDCTrailingEdgeInput{})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if before != 0 || after != 1 || afterThree != 3 {
		t.Fatalf("sweeps at 59s/61s/181s = %d/%d/%d, want 0/1/3 — the loop must sleep first, then sweep once per interval",
			before, after, afterThree)
	}
}

func TestCDCTrailingEdgeSweep_AFailingGatewayDoesNotEndIt(t *testing.T) {
	probe := &cdcTrailingEdgeProbe{err: errors.New("CDC trailing-edge sweep endpoint returned http 502")}
	env := newCDCTrailingEdgeEnv(probe)

	env.ExecuteWorkflow(CDCTrailingEdgeWorkflow, CDCTrailingEdgeInput{})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	var can *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &can) {
		t.Fatalf("workflow ended with %v, want a continue-as-new — a failing gateway must not stop the sweep", env.GetWorkflowError())
	}
	if got := probe.count(); got < cdcTrailingEdgeTicksPerRun {
		t.Fatalf("the sweep was attempted on %d ticks, want at least %d", got, cdcTrailingEdgeTicksPerRun)
	}
}

func TestCDCTrailingEdgeSweep_ContinuesAsNewAtItsBoundKeepingItsInterval(t *testing.T) {
	probe := &cdcTrailingEdgeProbe{}
	env := newCDCTrailingEdgeEnv(probe)

	const custom = 120
	env.ExecuteWorkflow(CDCTrailingEdgeWorkflow, CDCTrailingEdgeInput{IntervalSeconds: custom})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	var can *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &can) {
		t.Fatalf("workflow ended with %v, want a continue-as-new", env.GetWorkflowError())
	}
	if got := probe.count(); got != cdcTrailingEdgeTicksPerRun {
		t.Fatalf("continued as new after %d sweeps, want exactly %d", got, cdcTrailingEdgeTicksPerRun)
	}
	var next CDCTrailingEdgeInput
	if err := converter.GetDefaultDataConverter().FromPayloads(can.Input, &next); err != nil {
		t.Fatalf("could not decode the continue-as-new argument: %v", err)
	}
	if next.IntervalSeconds != custom || next.Ticks != 0 {
		t.Fatalf("next run starts with %+v, want interval %d and ticks 0", next, custom)
	}
}

func TestCDCTrailingEdgeSweep_TheIntervalComesFromTheArgument(t *testing.T) {
	probe := &cdcTrailingEdgeProbe{}
	env := newCDCTrailingEdgeEnv(probe)

	const custom = 300
	var atFourMinutes int
	env.RegisterDelayedCallback(func() { atFourMinutes = probe.count() }, 4*time.Minute)
	env.RegisterDelayedCallback(env.CancelWorkflow, 11*time.Minute)

	env.ExecuteWorkflow(CDCTrailingEdgeWorkflow, CDCTrailingEdgeInput{IntervalSeconds: custom})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if atFourMinutes != 0 || probe.count() != 2 {
		t.Fatalf("sweeps at 4m/11m under a 5-minute interval = %d/%d, want 0/2", atFourMinutes, probe.count())
	}
}
