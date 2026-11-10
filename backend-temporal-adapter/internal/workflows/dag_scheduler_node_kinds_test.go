package workflows

// KI-NODE-KINDS-DECLARED-BUT-UNROUTED, driven through the REAL DAGScheduler.
//
// The planner could emit ten node kinds; the scheduler ran two of them itself and
// sent every other one to ExecuteGraphNodeActivityV2. On the orchestrator a
// node task with both connection IDs and no forced operation takes the
// executor's plan-less fallback — a full source-to-destination transfer — so a
// `wait` node did not wait, a `merge` or `split` did nothing a merge or split
// means, and a `notification` node copied the whole table once more.
//
// These tests run the scheduler inside the Temporal test environment with a
// probe registered as ExecuteGraphNodeActivityV2: the probe records every node
// that reached the executor. What they pin:
//
//   - wait, merge and split are resolved by the scheduler and never dispatched;
//     wait really sleeps (workflow time moves by its wait_seconds).
//   - A graph the runtime cannot run is refused before ANY node starts: no
//     dispatch, and no STAGE_STARTED event.
//   - Histories recorded before the change replay the way they were recorded.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	workflowtypes "github.com/rsync-ai/backend-temporal-adapter/internal/workflows/types"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// dagNodeKindsTestWorkflow runs one graph through the scheduler and returns how
// much WORKFLOW time the run took, which is what a wait node must move.
func dagNodeKindsTestWorkflow(ctx workflow.Context, graph *workflowtypes.WorkflowGraph) (float64, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	start := workflow.Now(ctx)
	state := &WorkflowState{SourceConnectionID: "src-conn", DestinationConnectionID: "dst-conn"}
	err := NewDAGScheduler(ctx, graph, state, NLPipelineWorkflowV2Input{PipelineID: "p", ExecutionID: "e"}).Execute()
	return workflow.Now(ctx).Sub(start).Seconds(), err
}

type dagRunProbe struct {
	mu         sync.Mutex
	dispatched []ExecuteGraphNodeInput
	started    []string // stage of every STAGE_STARTED event
}

func (p *dagRunProbe) executeNode(_ context.Context, in ExecuteGraphNodeInput) (map[string]interface{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatched = append(p.dispatched, in)
	return map[string]interface{}{"status": "ok"}, nil
}

func (p *dagRunProbe) domainEvent(_ context.Context, ev map[string]interface{}) error {
	if ev["event_type"] == "STAGE_STARTED" {
		stage, _ := ev["stage"].(string)
		p.mu.Lock()
		p.started = append(p.started, stage)
		p.mu.Unlock()
	}
	return nil
}

func (p *dagRunProbe) dispatchedKinds() []string {
	kinds := make([]string, 0, len(p.dispatched))
	for _, in := range p.dispatched {
		kinds = append(kinds, in.NodeKind)
	}
	return kinds
}

type dagRun struct {
	probe   *dagRunProbe
	elapsed float64
	err     error
}

// runDAG executes graph through the scheduler. legacy replays it as a history
// recorded before the node-kind runtime existed.
func runDAG(t *testing.T, graph *workflowtypes.WorkflowGraph, legacy bool) dagRun {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	probe := &dagRunProbe{}
	env.RegisterWorkflow(dagNodeKindsTestWorkflow)
	env.RegisterActivityWithOptions(probe.executeNode, activity.RegisterOptions{Name: "ExecuteGraphNodeActivityV2"})
	env.RegisterActivityWithOptions(probe.domainEvent, activity.RegisterOptions{Name: "EmitDomainEventActivity"})
	env.RegisterActivityWithOptions(func(context.Context, StateUpdateInput) error { return nil },
		activity.RegisterOptions{Name: "StateUpdateActivity"})
	if legacy {
		// The literal, not the constant: a change ID is part of every recorded
		// history, so renaming it is itself a replay break this test must see.
		env.OnGetVersion("dag-node-kind-runtime", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	}

	env.ExecuteWorkflow(dagNodeKindsTestWorkflow, graph)
	if !env.IsWorkflowCompleted() {
		t.Fatalf("the DAG workflow did not complete")
	}
	run := dagRun{probe: probe, err: env.GetWorkflowError()}
	if run.err == nil {
		if err := env.GetWorkflowResult(&run.elapsed); err != nil {
			t.Fatalf("reading the workflow result: %v", err)
		}
	}
	return run
}

// dagGraph builds a graph from "from->to" edge strings; every node named in an
// edge or in nodes must be in nodes.
func dagGraph(nodes []workflowtypes.GraphNode, edges ...string) *workflowtypes.WorkflowGraph {
	g := &workflowtypes.WorkflowGraph{GraphID: "g", SchemaVersion: "1", Nodes: nodes}
	for i, e := range edges {
		from, to, _ := strings.Cut(e, "->")
		g.Edges = append(g.Edges, workflowtypes.GraphEdge{ID: "e" + string(rune('a'+i)), From: from, To: to})
	}
	return g
}

func dagNode(id, kind string, config map[string]interface{}) workflowtypes.GraphNode {
	return workflowtypes.GraphNode{ID: id, Kind: kind, Config: config}
}

func dagTransform(id, typ string) workflowtypes.GraphNode {
	return dagNode(id, workflowtypes.NodeKindTransform, map[string]interface{}{
		"transforms": []interface{}{map[string]interface{}{"type": typ}},
	})
}

// destinationTransforms returns the transform types the destination node was
// dispatched with, read from its single parent's output.
func destinationTransforms(t *testing.T, run dagRun) []string {
	t.Helper()
	for _, in := range run.probe.dispatched {
		if in.NodeKind != workflowtypes.NodeKindDestination {
			continue
		}
		if len(in.NodeInputs) != 1 {
			t.Fatalf("destination inputs = %#v, want exactly one parent", in.NodeInputs)
		}
		for _, v := range in.NodeInputs {
			parent, ok := v.(map[string]interface{})
			if !ok {
				t.Fatalf("destination input %#v is not a map", v)
			}
			return transformTypes(t, parent)
		}
	}
	t.Fatalf("the destination was never dispatched; dispatched kinds = %v", run.probe.dispatchedKinds())
	return nil
}

func assertOnlyDestinationDispatched(t *testing.T, run dagRun) {
	t.Helper()
	if run.err != nil {
		t.Fatalf("the DAG failed: %v", run.err)
	}
	if got := run.probe.dispatchedKinds(); len(got) != 1 || got[0] != workflowtypes.NodeKindDestination {
		t.Fatalf("dispatched to the executor: %v, want only [destination]. Every other "+
			"dispatched node runs the executor's plan-less fallback: a full "+
			"source-to-destination transfer", got)
	}
}

func TestDAGWaitNodeSleepsAndIsNotDispatched(t *testing.T) {
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagTransform("transform_mask", "mask"),
		dagNode("wait_1", workflowtypes.NodeKindWait, map[string]interface{}{"wait_seconds": 5}),
		dagNode("load", workflowtypes.NodeKindDestination, nil),
	}, "extract->transform_mask", "transform_mask->wait_1", "wait_1->load")

	run := runDAG(t, graph, false)
	assertOnlyDestinationDispatched(t, run)
	if run.elapsed < 5 {
		t.Errorf("the run took %.1fs of workflow time, want >= 5s: the wait node did not wait", run.elapsed)
	}
	if got := destinationTransforms(t, run); len(got) != 1 || got[0] != "mask" {
		t.Errorf("destination received transforms %v through the wait node, want [mask]", got)
	}
}

func TestDAGMergeUnionsBranchTransformsOnce(t *testing.T) {
	// A diamond: both branches carry the shared upstream transform, and the
	// destination must receive it once, not once per branch.
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagTransform("transform_trim", "trim"),
		dagTransform("transform_mask", "mask"),
		dagTransform("transform_cast", "cast"),
		dagNode("merge_1", workflowtypes.NodeKindMerge, nil),
		dagNode("load", workflowtypes.NodeKindDestination, nil),
	},
		"extract->transform_trim",
		"transform_trim->transform_mask", "transform_trim->transform_cast",
		"transform_mask->merge_1", "transform_cast->merge_1",
		"merge_1->load")

	run := runDAG(t, graph, false)
	assertOnlyDestinationDispatched(t, run)
	// Inputs are read in sorted key order (transform_cast before transform_mask),
	// and the first occurrence of a spec wins.
	got := destinationTransforms(t, run)
	want := []string{"trim", "cast", "mask"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("destination received transforms %v through the merge node, want %v", got, want)
	}
}

func TestDAGSplitPassesTransformsThrough(t *testing.T) {
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagTransform("transform_mask", "mask"),
		dagNode("split_1", workflowtypes.NodeKindSplit, nil),
		dagNode("load", workflowtypes.NodeKindDestination, nil),
	}, "extract->transform_mask", "transform_mask->split_1", "split_1->load")

	run := runDAG(t, graph, false)
	assertOnlyDestinationDispatched(t, run)
	if got := destinationTransforms(t, run); len(got) != 1 || got[0] != "mask" {
		t.Errorf("destination received transforms %v through the split node, want [mask]", got)
	}
}

func TestDAGRefusesAGraphItCannotRunBeforeAnyNodeStarts(t *testing.T) {
	type refusal struct {
		name  string
		graph *workflowtypes.WorkflowGraph
		want  string // substring of the workflow error
	}
	var cases []refusal

	// One case per kind the census records as refused — derived, so a kind that
	// is refused there and accepted here cannot exist.
	for _, kind := range sortedCensusKinds(nodeKindCensus) {
		if nodeKindCensus[kind].Refused == "" {
			continue
		}
		cases = append(cases, refusal{
			name: "refused kind " + kind,
			graph: dagGraph([]workflowtypes.GraphNode{
				dagNode("extract", workflowtypes.NodeKindSource, nil),
				dagNode("step", kind, nil),
				dagNode("load", workflowtypes.NodeKindDestination, nil),
			}, "extract->step", "step->load"),
			want: kind,
		})
	}
	if len(cases) == 0 {
		t.Fatalf("the census records no refused kind; this test would assert only the shape cases")
	}

	waitWith := func(cfg map[string]interface{}) *workflowtypes.WorkflowGraph {
		return dagGraph([]workflowtypes.GraphNode{
			dagNode("extract", workflowtypes.NodeKindSource, nil),
			dagNode("wait_1", workflowtypes.NodeKindWait, cfg),
			dagNode("load", workflowtypes.NodeKindDestination, nil),
		}, "extract->wait_1", "wait_1->load")
	}
	cases = append(cases,
		refusal{
			name: "no destination",
			graph: dagGraph([]workflowtypes.GraphNode{
				dagNode("extract", workflowtypes.NodeKindSource, nil),
				dagTransform("transform_mask", "mask"),
			}, "extract->transform_mask"),
			want: "destination",
		},
		refusal{
			// Each destination node is a whole transfer: two would copy the data twice.
			name: "two destinations",
			graph: dagGraph([]workflowtypes.GraphNode{
				dagNode("extract", workflowtypes.NodeKindSource, nil),
				dagNode("load_a", workflowtypes.NodeKindDestination, nil),
				dagNode("load_b", workflowtypes.NodeKindDestination, nil),
			}, "extract->load_a", "extract->load_b"),
			want: "destination",
		},
		refusal{name: "wait without wait_seconds", graph: waitWith(nil), want: "wait_seconds"},
		refusal{name: "wait of zero seconds", graph: waitWith(map[string]interface{}{"wait_seconds": 0}), want: "wait_seconds"},
		refusal{name: "negative wait", graph: waitWith(map[string]interface{}{"wait_seconds": -3}), want: "wait_seconds"},
		refusal{name: "wait over a day", graph: waitWith(map[string]interface{}{"wait_seconds": 86401}), want: "wait_seconds"},
		refusal{name: "wait as text", graph: waitWith(map[string]interface{}{"wait_seconds": "5"}), want: "wait_seconds"},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runDAG(t, tc.graph, false)
			if run.err == nil {
				t.Fatalf("the DAG ran; want it refused (dispatched: %v)", run.probe.dispatchedKinds())
			}
			if !strings.Contains(run.err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", run.err.Error(), tc.want)
			}
			if got := run.probe.dispatchedKinds(); len(got) != 0 {
				t.Errorf("dispatched %v before refusing the graph; want nothing dispatched", got)
			}
			if len(run.probe.started) != 0 {
				t.Errorf("STAGE_STARTED for %v before refusing the graph; want the refusal "+
					"before any node starts", run.probe.started)
			}
		})
	}
}

func TestDAGRefusalListsEveryProblemInNodeOrder(t *testing.T) {
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagNode("notify", workflowtypes.NodeKindNotification, nil),
		dagNode("wait_1", workflowtypes.NodeKindWait, nil),
	}, "extract->notify", "notify->wait_1")

	run := runDAG(t, graph, false)
	if run.err == nil {
		t.Fatalf("the DAG ran; want it refused")
	}
	msg := run.err.Error()
	order := []string{"notify", "wait_1", "destination"}
	last := -1
	for _, want := range order {
		i := strings.Index(msg, want)
		if i < 0 {
			t.Fatalf("error %q does not mention %q; a refusal must list every problem at once", msg, want)
		}
		if i < last {
			t.Errorf("error %q lists %q out of order; want %v", msg, want, order)
		}
		last = i
	}
}

func TestDAGLegacyHistoryStillDispatchesWaitMergeAndSplit(t *testing.T) {
	// A run recorded before the node-kind runtime dispatched these kinds and
	// never slept. Replay must take the same path, or it diverges from its
	// history — the executor refuses such a task now, which is the fix for those
	// runs, not a replay change.
	graph := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagNode("wait_1", workflowtypes.NodeKindWait, map[string]interface{}{"wait_seconds": 5}),
		dagNode("merge_1", workflowtypes.NodeKindMerge, nil),
		dagNode("split_1", workflowtypes.NodeKindSplit, nil),
		dagNode("notify", workflowtypes.NodeKindNotification, nil),
		dagNode("load", workflowtypes.NodeKindDestination, nil),
	}, "extract->wait_1", "wait_1->merge_1", "merge_1->split_1", "split_1->notify", "notify->load")

	run := runDAG(t, graph, true)
	if run.err != nil {
		t.Fatalf("a legacy history was refused: %v", run.err)
	}
	got := run.probe.dispatchedKinds()
	sort.Strings(got)
	want := []string{"destination", "merge", "notification", "split", "wait"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("legacy replay dispatched %v, want %v", got, want)
	}
	if run.elapsed >= 5 {
		t.Errorf("legacy replay took %.1fs of workflow time; a recorded run had no timer", run.elapsed)
	}
}
