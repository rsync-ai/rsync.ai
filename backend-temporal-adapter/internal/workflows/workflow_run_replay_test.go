package workflows

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Replay of WorkflowRunWorkflow against histories a Temporal server wrote.
//
// WorkflowRunWorkflow's first version had no GetVersion (workflows-design.md § 11 Q1).
// The steps graph (build step 5) is GetVersion "workflow-steps" version 1; DefaultVersion
// is the first version, kept for the runs in flight when it shipped. A later change to the
// commands either path emits must wrap the new path in another workflow.GetVersion, or
// every run in flight at deploy time fails its next workflow task with a nondeterminism
// error. These replays fail a change that forgets to.
//
// The fixtures were CAPTURED against a throwaway local Temporal server and a stand-in
// gateway (WORKFLOW_RUN_CAPTURE_DIR). The first version's, by
// workflow_run_restart_integration_test.go:
//   - workflow_run_scheduled_restart_history.json: a scheduled run answered busy once,
//     then sent to two channels, its workflow worker stopped while the first send was in
//     flight, and finished on the restarted worker.
//   - workflow_run_now_recorded_history.json: Run now, recorded by the gateway at evaluate.
//   - workflow_run_send_refused_history.json: Run now whose first send was refused, so
//     it was finished as a failed step.
//
// The steps graph's, by workflow_graph_run_integration_test.go:
//   - workflow_run_graph_steps_history.json: a query; a pipeline whose first run failed
//     and whose second waited while the pipeline was busy; a model refresh; a send to two
//     channels; and a step for a failure, skipped.
//   - workflow_run_graph_stopped_history.json: a wait, stopped while it waited.
//
// The denominator is requireWorkflowRunHistory: a fixture that stops mid-run, or did not
// take the path it is named for, would replay without testing anything.

// requireWorkflowRunHistory checks that path is a completed history whose commands were
// exactly wantSteps — activity types (sends with their channel, finish with its abort, a
// step call with its step, action, attempt, channel, reason and status, complete with its
// abort), "version <change> <version>", "timer <duration>", "timer canceled", "signal
// <name>" and "completed" — and whose workflow tasks ran on
// exactly wantWorkers, in order of first appearance.
func requireWorkflowRunHistory(t *testing.T, path string, wantSteps, wantWorkers []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	hist, err := client.HistoryFromJSON(f, client.HistoryJSONOptions{})
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var steps, workers []string
	seen := map[string]bool{}
	for _, ev := range hist.GetEvents() {
		if a := ev.GetActivityTaskScheduledEventAttributes(); a != nil {
			step := a.GetActivityType().GetName()
			p := a.GetInput().GetPayloads()
			switch step {
			case "SendWorkflowRunActivity":
				var in WorkflowRunSendInput
				if len(p) != 1 || json.Unmarshal(p[0].GetData(), &in) != nil {
					t.Fatalf("%s: a send's input does not decode", path)
				}
				step += " " + in.ChannelID
			case "FinishWorkflowRunActivity":
				var in WorkflowRunFinishInput
				if len(p) != 1 || json.Unmarshal(p[0].GetData(), &in) != nil {
					t.Fatalf("%s: finish's input does not decode", path)
				}
				step += " abort=" + in.Abort
			case "StepWorkflowRunActivity":
				var in WorkflowRunStepInput
				if len(p) != 1 || json.Unmarshal(p[0].GetData(), &in) != nil {
					t.Fatalf("%s: a step call's input does not decode", path)
				}
				step += " " + in.StepID + " " + in.Action
				if in.Attempt > 0 {
					step += fmt.Sprint(" ", in.Attempt)
				}
				for _, v := range []string{in.ChannelID, in.Reason, in.Status} {
					if v != "" {
						step += " " + v
					}
				}
			case "CompleteWorkflowRunActivity":
				var in WorkflowRunCompleteInput
				if len(p) != 1 || json.Unmarshal(p[0].GetData(), &in) != nil {
					t.Fatalf("%s: complete's input does not decode", path)
				}
				step += fmt.Sprint(" abort=", in.Abort)
			}
			steps = append(steps, step)
		}
		if a := ev.GetMarkerRecordedEventAttributes(); a != nil && a.GetMarkerName() == "Version" {
			var change string
			var version int
			d := a.GetDetails()
			if json.Unmarshal(d["change-id"].GetPayloads()[0].GetData(), &change) != nil ||
				json.Unmarshal(d["version"].GetPayloads()[0].GetData(), &version) != nil {
				t.Fatalf("%s: a version marker does not decode", path)
			}
			steps = append(steps, fmt.Sprint("version ", change, " ", version))
		}
		if a := ev.GetWorkflowExecutionSignaledEventAttributes(); a != nil {
			steps = append(steps, "signal "+a.GetSignalName())
		}
		if a := ev.GetTimerStartedEventAttributes(); a != nil {
			steps = append(steps, "timer "+a.GetStartToFireTimeout().AsDuration().String())
		}
		if ev.GetTimerCanceledEventAttributes() != nil {
			steps = append(steps, "timer canceled")
		}
		if ev.GetWorkflowExecutionCompletedEventAttributes() != nil {
			steps = append(steps, "completed")
		}
		if a := ev.GetWorkflowTaskStartedEventAttributes(); a != nil && !seen[a.GetIdentity()] {
			seen[a.GetIdentity()] = true
			workers = append(workers, a.GetIdentity())
		}
	}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("%s: commands %v, want %v", path, steps, wantSteps)
	}
	if !reflect.DeepEqual(workers, wantWorkers) {
		t.Fatalf("%s: workflow tasks ran on %v, want %v", path, workers, wantWorkers)
	}
}

func replayWorkflowRunHistory(t *testing.T, path string) error {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(WorkflowRunWorkflow)
	return replayer.ReplayWorkflowHistoryFromJSONFile(nil, path)
}

func TestWorkflowRun_ReplaysAScheduledRunThatWaitedSentAndRestarted(t *testing.T) {
	const path = "testdata/workflow_run_scheduled_restart_history.json"
	requireWorkflowRunHistory(t, path, []string{
		"BeginWorkflowRunActivity", "timer 10s", "BeginWorkflowRunActivity",
		"timer 10m0s", "EvaluateWorkflowRunActivity",
		"SendWorkflowRunActivity ch-a", "SendWorkflowRunActivity ch-b",
		"timer canceled", "FinishWorkflowRunActivity abort=", "completed",
	}, []string{"before-restart", "after-restart"})
	if err := replayWorkflowRunHistory(t, path); err != nil {
		t.Fatalf("a scheduled run that waited, sent and restarted does not replay: %v", err)
	}
}

func TestWorkflowRun_ReplaysARunNowRecordedAtEvaluate(t *testing.T) {
	const path = "testdata/workflow_run_now_recorded_history.json"
	requireWorkflowRunHistory(t, path, []string{
		"BeginWorkflowRunActivity", "timer 10m0s", "EvaluateWorkflowRunActivity", "timer canceled", "completed",
	}, []string{"after-restart"})
	if err := replayWorkflowRunHistory(t, path); err != nil {
		t.Fatalf("a run recorded at evaluate does not replay: %v", err)
	}
}

func TestWorkflowRun_ReplaysARunWhoseSendWasRefused(t *testing.T) {
	const path = "testdata/workflow_run_send_refused_history.json"
	requireWorkflowRunHistory(t, path, []string{
		"BeginWorkflowRunActivity", "timer 10m0s", "EvaluateWorkflowRunActivity",
		"SendWorkflowRunActivity ch-x", "timer canceled", "FinishWorkflowRunActivity abort=step_failed", "completed",
	}, []string{"after-restart"})
	if err := replayWorkflowRunHistory(t, path); err != nil {
		t.Fatalf("a run finished as a failed step does not replay: %v", err)
	}
}

func TestWorkflowRun_ReplaysARunOfSteps(t *testing.T) {
	const path = "testdata/workflow_run_graph_steps_history.json"
	requireWorkflowRunHistory(t, path, []string{
		"BeginWorkflowRunActivity", "version workflow-steps 1", "GraphWorkflowRunActivity",
		"StepWorkflowRunActivity q run",
		"StepWorkflowRunActivity load start 1", "timer 1h0m0s",
		"StepWorkflowRunActivity load poll 1", "StepWorkflowRunActivity load poll 1",
		"StepWorkflowRunActivity load start 2", "StepWorkflowRunActivity load start 2",
		"StepWorkflowRunActivity load poll 2", "StepWorkflowRunActivity load poll 2", "timer canceled",
		"StepWorkflowRunActivity model request", "StepWorkflowRunActivity oops record skipped", "timer 12h0m0s",
		"SignalModelRefreshActivity",
		"StepWorkflowRunActivity model poll", "StepWorkflowRunActivity model poll", "timer canceled",
		"StepWorkflowRunActivity tell plan",
		"StepWorkflowRunActivity tell send ch-a", "StepWorkflowRunActivity tell send ch-b",
		"StepWorkflowRunActivity tell finish",
		"CompleteWorkflowRunActivity abort=false", "completed",
	}, []string{"graph-worker"})
	if err := replayWorkflowRunHistory(t, path); err != nil {
		t.Fatalf("a run of steps does not replay: %v", err)
	}
}

func TestWorkflowRun_ReplaysARunStoppedWhileItWaited(t *testing.T) {
	const path = "testdata/workflow_run_graph_stopped_history.json"
	requireWorkflowRunHistory(t, path, []string{
		"BeginWorkflowRunActivity", "version workflow-steps 1", "GraphWorkflowRunActivity",
		"StepWorkflowRunActivity w record running", "timer 45m0s", "timer 30m0s",
		"signal " + WorkflowRunStopSignal, "timer canceled", "timer canceled",
		"StepWorkflowRunActivity w stop stop",
		"CompleteWorkflowRunActivity abort=false", "completed",
	}, []string{"graph-worker"})
	if err := replayWorkflowRunHistory(t, path); err != nil {
		t.Fatalf("a run stopped while it waited does not replay: %v", err)
	}
}
