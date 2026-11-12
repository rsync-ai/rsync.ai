package workflows

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Replay of the next hop against real histories, both directions.
//
// The hop has no GetVersion: whether a workflow hops is decided by its run activity's
// recorded result. These pin that claim against histories a Temporal server wrote.
//
//   - *_succeeded_pre_hop_history.json were CAPTURED from this package as it was before
//     the hop (0dc11afb), each workflow run against a throwaway local Temporal server and
//     a stand-in gateway answering a succeeded run the old way: no run_id, no next_hop.
//     Replayed on this code they must emit no hop, or every model run in flight at
//     deploy time would fail its next workflow task with a nondeterminism error.
//   - *_next_hop_history.json were CAPTURED by the restart test in
//     model_next_hop_restart_integration_test.go (NEXT_HOP_CAPTURE_DIR): the run, the
//     listing of the models below it and the fan-out child, with the scheduled run's
//     workflow worker stopped between its run and its hop. Every later build must keep
//     replaying them, so the hop's commands stay fixed.
//
// The denominator is requireModelRunHistory: a fixture that did not run the post-run
// code (one that stops mid-task, or whose run did not succeed) would replay without
// testing anything. Mutation evidence: hopping without the gate fails both pre-hop
// replays; dropping the fan-out child fails both next-hop replays.

// requireModelRunHistory checks that path is a closed history whose run answered
// next_hop wantHop and whose commands were exactly wantSteps (activity types, child
// workflow types prefixed "child:", and "completed" for the close).
func requireModelRunHistory(t *testing.T, path, wantHop string, wantSteps []string) {
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
	var steps []string
	runs := 0
	for _, ev := range hist.GetEvents() {
		if a := ev.GetActivityTaskScheduledEventAttributes(); a != nil {
			steps = append(steps, a.GetActivityType().GetName())
		}
		if c := ev.GetStartChildWorkflowExecutionInitiatedEventAttributes(); c != nil {
			steps = append(steps, "child:"+c.GetWorkflowType().GetName())
		}
		if ev.GetWorkflowExecutionCompletedEventAttributes() != nil {
			steps = append(steps, "completed")
		}
		// The first completed activity is the run; its result says who owns the hop.
		if c := ev.GetActivityTaskCompletedEventAttributes(); c != nil && runs == 0 {
			runs++
			var res modelRunActivityResult
			p := c.GetResult().GetPayloads()
			if len(p) != 1 || json.Unmarshal(p[0].GetData(), &res) != nil {
				t.Fatalf("%s: the run's result does not decode", path)
			}
			if res.Status != "succeeded" || res.NextHop != wantHop {
				t.Fatalf("%s: run answered status %q next_hop %q, want succeeded and %q", path, res.Status, res.NextHop, wantHop)
			}
		}
	}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("%s: commands %v, want %v", path, steps, wantSteps)
	}
}

func replayModelRunHistory(t *testing.T, wf any, path string) error {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(wf)
	replayer.RegisterWorkflow(UpstreamFanOutWorkflow)
	return replayer.ReplayWorkflowHistoryFromJSONFile(nil, path)
}

var (
	preHopSteps  = []string{"RunModelActivity", "completed"}
	nextHopSteps = []string{"RunModelActivity", "ListDownstreamModelsActivity", "child:UpstreamFanOutWorkflow", "completed"}
)

func TestScheduledModelRun_ReplaysASucceededRunFromBeforeTheHop(t *testing.T) {
	const path = "testdata/scheduled_model_run_succeeded_pre_hop_history.json"
	requireModelRunHistory(t, path, "", preHopSteps)
	if err := replayModelRunHistory(t, ScheduledModelRunWorkflow, path); err != nil {
		t.Fatalf("a model run that succeeded before the hop moved here does not replay: %v", err)
	}
}

func TestModelRefresh_ReplaysASucceededRebuildFromBeforeTheHop(t *testing.T) {
	const path = "testdata/model_refresh_succeeded_pre_hop_history.json"
	requireModelRunHistory(t, path, "", preHopSteps)
	if err := replayModelRunHistory(t, ModelRefreshWorkflow, path); err != nil {
		t.Fatalf("a model refresh loop that rebuilt before the hop moved here does not replay: %v", err)
	}
}

func TestScheduledModelRun_ReplaysARunThatTookItsHopAcrossARestart(t *testing.T) {
	const path = "testdata/scheduled_model_run_next_hop_history.json"
	requireModelRunHistory(t, path, ModelNextHopCaller, nextHopSteps)
	if err := replayModelRunHistory(t, ScheduledModelRunWorkflow, path); err != nil {
		t.Fatalf("a model run that took its own hop does not replay: %v", err)
	}
}

func TestModelRefresh_ReplaysARebuildThatTookItsHop(t *testing.T) {
	const path = "testdata/model_refresh_next_hop_history.json"
	requireModelRunHistory(t, path, ModelNextHopCaller, nextHopSteps)
	if err := replayModelRunHistory(t, ModelRefreshWorkflow, path); err != nil {
		t.Fatalf("a model refresh loop whose rebuild took its own hop does not replay: %v", err)
	}
}
