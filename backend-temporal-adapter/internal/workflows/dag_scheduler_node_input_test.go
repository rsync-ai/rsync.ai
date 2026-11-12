package workflows

// A table picked at a DAG node's input wait, for a node with no config.
//
// The planner drops config it made up ("your_database_name", "<bucket>"), so a
// destination node can arrive with `config: {}`. GraphNode.Config is
// `omitempty`: the workflow input's JSON round trip turns that empty map into a
// nil one. handleNodeInput then wrote the pick into it — "assignment to entry in
// nil map" — and the workflow task panicked on every retry; on prod the run sat
// on "Processing node input…" for good (2026-09-30, pipeline d56959e0).
//
// The bug class is a write into a map that decoding can leave nil. The graph goes
// through the Temporal test environment's data converter here, as it does in a
// real run, so the node's config is nil when the pick arrives.
//
// Pinned here:
//   - a node whose config is empty (nil after decoding) takes the pick and runs
//     again with it;
//   - control: a node with config keeps its keys and gains the pick.

import (
	"context"
	"sync"
	"testing"
	"time"

	workflowtypes "github.com/rsync-ai/backend-temporal-adapter/internal/workflows/types"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// nodeInputProbe asks for input the first time the named node runs, then
// records every dispatch.
type nodeInputProbe struct {
	mu         sync.Mutex
	askNode    string
	asked      bool
	dispatched []ExecuteGraphNodeInput
}

func (p *nodeInputProbe) executeNode(_ context.Context, in ExecuteGraphNodeInput) (map[string]interface{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatched = append(p.dispatched, in)
	if in.NodeID == p.askNode && !p.asked {
		p.asked = true
		return nil, toTemporalError(NewPolicyError(PolicyCodeNodeInputRequired, "Select the tables to load", nil))
	}
	return map[string]interface{}{"status": "ok"}, nil
}

// runDAGWithPick runs graph, answers the input wait of graph's "load" node with
// the pick, and returns every dispatch of "load".
func runDAGWithPick(t *testing.T, graph *workflowtypes.WorkflowGraph, pick map[string]interface{}) []ExecuteGraphNodeInput {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	probe := &nodeInputProbe{askNode: "load"}
	env.RegisterWorkflow(dagNodeKindsTestWorkflow)
	env.RegisterActivityWithOptions(probe.executeNode, activity.RegisterOptions{Name: "ExecuteGraphNodeActivityV2"})
	env.RegisterActivityWithOptions(func(context.Context, map[string]interface{}) error { return nil },
		activity.RegisterOptions{Name: "EmitDomainEventActivity"})
	env.RegisterActivityWithOptions(func(context.Context, StateUpdateInput) error { return nil },
		activity.RegisterOptions{Name: "StateUpdateActivity"})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalNodeInputProvided, NodeInputPayload{
			PipelineID:  "p",
			ExecutionID: "e",
			NodeID:      "load",
			Message:     "Selected 1 table",
			ConfigPatch: pick,
		})
	}, time.Minute)

	env.ExecuteWorkflow(dagNodeKindsTestWorkflow, graph)
	if !env.IsWorkflowCompleted() {
		t.Fatalf("the DAG workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the DAG workflow failed: %v", err)
	}

	var loads []ExecuteGraphNodeInput
	for _, in := range probe.dispatched {
		if in.NodeID == "load" {
			loads = append(loads, in)
		}
	}
	return loads
}

func TestDAGNodeInput_NodeWithoutConfigTakesThePick(t *testing.T) {
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, map[string]interface{}{"connector_type": "mongodb"}),
		// What the planner keeps once its placeholders are dropped.
		dagNode("load", workflowtypes.NodeKindDestination, map[string]interface{}{}),
	}, "extract->load")

	loads := runDAGWithPick(t, graph, map[string]interface{}{"selected_tables": []interface{}{"datingapp.matches"}})

	if len(loads) != 2 {
		t.Fatalf("load dispatched %d times, want 2 (asks for input, then runs with the pick)", len(loads))
	}
	got, _ := loads[1].NodeConfig["selected_tables"].([]interface{})
	if len(got) != 1 || got[0] != "datingapp.matches" {
		t.Fatalf("second load ran with selected_tables=%v, want [datingapp.matches]", loads[1].NodeConfig["selected_tables"])
	}
}

func TestDAGNodeInput_Control_NodeWithConfigKeepsItsKeys(t *testing.T) {
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, map[string]interface{}{"connector_type": "mongodb"}),
		dagNode("load", workflowtypes.NodeKindDestination, map[string]interface{}{"connector_type": "gcs"}),
	}, "extract->load")

	loads := runDAGWithPick(t, graph, map[string]interface{}{"selected_tables": []interface{}{"datingapp.matches"}})

	if len(loads) != 2 {
		t.Fatalf("load dispatched %d times, want 2", len(loads))
	}
	cfg := loads[1].NodeConfig
	if cfg["connector_type"] != "gcs" {
		t.Errorf("connector_type = %v after the pick, want gcs", cfg["connector_type"])
	}
	if got, _ := cfg["selected_tables"].([]interface{}); len(got) != 1 {
		t.Errorf("selected_tables = %v after the pick, want [datingapp.matches]", cfg["selected_tables"])
	}
}
