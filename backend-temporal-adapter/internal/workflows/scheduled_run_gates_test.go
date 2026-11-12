package workflows

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// A scheduled tick must refuse what the Run button refuses: a workspace past its
// trial or its GB, a creator over their LLM spend, a draft connector, a Reload
// into an app's API. The gateway decides (handlers/pipeline_run_gates.go, tested
// there); what is guarded here is that the wrapper asks before it writes an
// execution, stops on a refusal, and still runs when the gateway cannot answer.
//
// The probes replace the activities under their own names, so nothing here
// reaches a database or an api-gateway. CreateScheduledExecutionActivity answers
// "" (the pipeline is gone), which ends the wrapper before the child starts.

type scheduledGateProbe struct {
	mu      sync.Mutex
	gate    ScheduledRunGate
	gateErr error
	asked   int
	created int
}

func (p *scheduledGateProbe) checkActive(context.Context, string) (bool, error) { return false, nil }

func (p *scheduledGateProbe) checkGates(context.Context, string) (ScheduledRunGate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked++
	return p.gate, p.gateErr
}

func (p *scheduledGateProbe) createExecution(context.Context, ScheduledRunInput) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created++
	return "", nil
}

func runScheduledWrapper(t *testing.T, probe *scheduledGateProbe) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ScheduledPipelineRunWorkflow)
	env.RegisterActivityWithOptions(probe.checkActive, activity.RegisterOptions{Name: "CheckActiveRunActivity"})
	env.RegisterActivityWithOptions(probe.checkGates, activity.RegisterOptions{Name: "CheckScheduledRunGatesActivity"})
	env.RegisterActivityWithOptions(probe.createExecution, activity.RegisterOptions{Name: "CreateScheduledExecutionActivity"})

	env.ExecuteWorkflow(ScheduledPipelineRunWorkflow, ScheduledRunInput{
		ScheduleID: "sched-1",
		PipelineID: "11111111-1111-1111-1111-111111111111",
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("wrapper never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("wrapper error = %v, want nil: a skipped or allowed tick is not a failure", err)
	}
}

func TestScheduledRun_ARefusedTickWritesNoExecution(t *testing.T) {
	probe := &scheduledGateProbe{gate: ScheduledRunGate{
		Reason:  "trial_expired",
		Message: "Your free trial has ended. Upgrade to Pro to run pipelines.",
	}}
	runScheduledWrapper(t, probe)

	if probe.asked != 1 {
		t.Fatalf("gates asked %d times, want 1", probe.asked)
	}
	if probe.created != 0 {
		t.Fatalf("CreateScheduledExecutionActivity ran %d times after a refusal, want 0 — the Run button would not have started this run", probe.created)
	}
}

func TestScheduledRun_AnAllowedTickRuns(t *testing.T) {
	probe := &scheduledGateProbe{gate: ScheduledRunGate{Allowed: true}}
	runScheduledWrapper(t, probe)

	if probe.asked != 1 || probe.created != 1 {
		t.Fatalf("asked/created = %d/%d, want 1/1", probe.asked, probe.created)
	}
}

func TestScheduledRun_AGatewayThatCannotAnswerDoesNotStopTheSchedule(t *testing.T) {
	probe := &scheduledGateProbe{gateErr: errors.New("/scheduled-run/check returned http 502")}
	runScheduledWrapper(t, probe)

	if probe.asked == 0 {
		t.Fatal("gates never asked")
	}
	if probe.created != 1 {
		t.Fatalf("created = %d, want 1 — each gate fails open on a read error, so the check does too", probe.created)
	}
}
