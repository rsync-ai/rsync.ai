package workflows

// Unit tests for the node-kind runtime: which kinds run, the graph preflight
// that refuses the rest, and the wait/merge/split outputs. The behaviour
// through the real scheduler is in dag_scheduler_node_kinds_test.go.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	workflowtypes "github.com/rsync-ai/backend-temporal-adapter/internal/workflows/types"
)

type nodeKindsGolden struct {
	Runnable           []string `json:"runnable"`
	NotRunnable        []string `json:"not_runnable"`
	ExecutedByExecutor struct {
		NodeKind  string `json:"node_kind"`
		Operation string `json:"operation"`
	} `json:"executed_by_executor"`
	Wait struct {
		ConfigKey  string  `json:"config_key"`
		MaxSeconds float64 `json:"max_seconds"`
	} `json:"wait"`
}

func loadNodeKindsGolden(t *testing.T) nodeKindsGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "shared", "workflow_node_kinds_golden.json"))
	if err != nil {
		t.Fatalf("reading shared/workflow_node_kinds_golden.json: %v", err)
	}
	var g nodeKindsGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("parsing shared/workflow_node_kinds_golden.json: %v", err)
	}
	if len(g.Runnable) == 0 {
		t.Fatalf("the golden lists no runnable kind")
	}
	return g
}

func TestNodeKindRunnableMatchesSharedGolden(t *testing.T) {
	g := loadNodeKindsGolden(t)

	in := map[string]string{}
	for _, k := range g.Runnable {
		in[k] = "runnable"
	}
	for _, k := range g.NotRunnable {
		if prev, dup := in[k]; dup {
			t.Errorf("golden lists %q under both %s and not_runnable", k, prev)
		}
		in[k] = "not_runnable"
	}
	declared := map[string]bool{}
	for _, kind := range workflowtypes.AllNodeKinds {
		declared[kind] = true
		list, listed := in[kind]
		if !listed {
			t.Errorf("declared node kind %q is in neither runnable nor not_runnable", kind)
			continue
		}
		if got, want := nodeKindRunnable(kind), list == "runnable"; got != want {
			t.Errorf("nodeKindRunnable(%q) = %v, but the golden lists it under %s", kind, got, list)
		}
		if refused := nodeKindCensus[kind].Refused != ""; refused != (list == "not_runnable") {
			t.Errorf("the census Refused entry for %q disagrees with the golden (%s)", kind, list)
		}
	}
	for k := range in {
		if !declared[k] {
			t.Errorf("golden lists %q, which is not in workflowtypes.AllNodeKinds", k)
		}
	}

	want := append([]string(nil), g.Runnable...)
	sort.Strings(want)
	if got := runnableNodeKinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("runnableNodeKinds() = %v, want %v", got, want)
	}
}

func TestOnlyTheGoldenExecutorNodeForcesAnOperation(t *testing.T) {
	// The executor accepts exactly one node task (its guard reads the same
	// golden). A second rule forcing an operation would be refused there.
	g := loadNodeKindsGolden(t)
	for _, kind := range sortedRuleKinds(nodeDispatchRules) {
		op := nodeDispatchRules[kind].Operation
		if kind == g.ExecutedByExecutor.NodeKind {
			if op != g.ExecutedByExecutor.Operation {
				t.Errorf("%s forces operation %q, the golden says the executor runs %q",
					kind, op, g.ExecutedByExecutor.Operation)
			}
			continue
		}
		if op != "" {
			t.Errorf("%s forces operation %q; the executor refuses every node task but %s/%s",
				kind, op, g.ExecutedByExecutor.NodeKind, g.ExecutedByExecutor.Operation)
		}
	}
}

func TestWaitDuration(t *testing.T) {
	g := loadNodeKindsGolden(t)
	if g.Wait.ConfigKey != "wait_seconds" || g.Wait.MaxSeconds != maxWaitSeconds {
		t.Fatalf("golden wait = %+v, want config_key wait_seconds and max_seconds %v", g.Wait, maxWaitSeconds)
	}
	ok := []struct {
		raw  interface{}
		want time.Duration
	}{
		{float64(5), 5 * time.Second},
		{2.5, 2500 * time.Millisecond},
		{5, 5 * time.Second},
		{int64(7), 7 * time.Second},
		{json.Number("9"), 9 * time.Second},
		{float64(86400), 24 * time.Hour},
	}
	for _, c := range ok {
		got, err := waitDuration(map[string]interface{}{"wait_seconds": c.raw})
		if err != nil || got != c.want {
			t.Errorf("waitDuration(%#v) = %v, %v; want %v", c.raw, got, err, c.want)
		}
	}
	bad := []interface{}{nil, 0, float64(0), -1, 86401, math.NaN(), math.Inf(1), "5", true, json.Number("x")}
	for _, raw := range bad {
		if got, err := waitDuration(map[string]interface{}{"wait_seconds": raw}); err == nil {
			t.Errorf("waitDuration(%#v) = %v, want an error", raw, got)
		} else if !strings.Contains(err.Error(), "wait_seconds") {
			t.Errorf("waitDuration(%#v) error %q does not name wait_seconds", raw, err)
		}
	}
	if _, err := waitDuration(nil); err == nil {
		t.Errorf("waitDuration(nil config) returned no error")
	}
}

func TestValidateRunnableGraph(t *testing.T) {
	valid := dagGraph([]workflowtypes.GraphNode{
		dagNode("extract", workflowtypes.NodeKindSource, nil),
		dagNode("wait_1", workflowtypes.NodeKindWait, map[string]interface{}{"wait_seconds": float64(1)}),
		dagNode("merge_1", workflowtypes.NodeKindMerge, nil),
		dagNode("split_1", workflowtypes.NodeKindSplit, nil),
		dagTransform("transform_mask", "mask"),
		dagNode("load", workflowtypes.NodeKindDestination, nil),
	})
	if err := validateRunnableGraph(valid); err != nil {
		t.Errorf("a graph of runnable kinds with one destination was refused: %v", err)
	}

	bad := dagGraph([]workflowtypes.GraphNode{
		dagNode("llm_1", workflowtypes.NodeKindLLM, nil),
		dagNode("wait_1", workflowtypes.NodeKindWait, nil),
		dagNode("api_1", workflowtypes.NodeKindAPICall, nil),
	})
	err := validateRunnableGraph(bad)
	if err == nil {
		t.Fatalf("a graph of refused kinds with no destination was accepted")
	}
	for _, want := range []string{"llm_1", "wait_1", "api_1", "destination", strings.Join(runnableNodeKinds(), ", ")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLocalNodeOutputWaitMergeSplit(t *testing.T) {
	upstream := func(types ...string) map[string]interface{} {
		list := make([]interface{}, 0, len(types))
		for _, typ := range types {
			list = append(list, map[string]interface{}{"type": typ})
		}
		return map[string]interface{}{"metadata": map[string]interface{}{"transforms": list}}
	}
	cases := []struct {
		kind   string
		inputs map[string]interface{}
		want   []string
	}{
		{workflowtypes.NodeKindWait, map[string]interface{}{"t": upstream("mask")}, []string{"mask"}},
		{workflowtypes.NodeKindSplit, map[string]interface{}{"t": upstream("mask", "trim")}, []string{"mask", "trim"}},
		// Sorted input keys, first occurrence wins: "a" is read before "b".
		{workflowtypes.NodeKindMerge, map[string]interface{}{
			"b": upstream("trim", "mask"),
			"a": upstream("trim", "cast"),
		}, []string{"trim", "cast", "mask"}},
	}
	for _, c := range cases {
		// A config must not leak into the output: only transform nodes add specs.
		node := &workflowtypes.GraphNode{ID: c.kind + "_1", Kind: c.kind,
			Config: map[string]interface{}{"wait_seconds": float64(1), "type": "ignored"}}
		out, handled := localNodeOutput(node.ID, node, c.inputs)
		if !handled {
			t.Errorf("%s is not resolved locally", c.kind)
			continue
		}
		if got := transformTypes(t, out); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s output transforms = %v, want %v", c.kind, got, c.want)
		}

		// No upstream transforms: a non-nil output with nothing in it.
		out, handled = localNodeOutput(node.ID, node, nil)
		if !handled || out == nil || len(out) != 0 {
			t.Errorf("%s with no inputs: handled=%v out=%#v, want an empty map", c.kind, handled, out)
		}
	}
}

func TestLegacyLocalNodeKindsAreFrozen(t *testing.T) {
	// A history recorded before the node-kind runtime dispatched every kind but
	// these two. Replay reads this set, so it can never grow: a kind made local
	// later is still dispatched when such a history replays.
	want := map[string]bool{workflowtypes.NodeKindSource: true, workflowtypes.NodeKindTransform: true}
	if !reflect.DeepEqual(preRuntimeLocalNodeKinds, want) {
		t.Errorf("preRuntimeLocalNodeKinds = %v, want exactly %v", preRuntimeLocalNodeKinds, want)
	}
	if nodeKindRuntimeChangeID != "dag-node-kind-runtime" {
		t.Errorf("nodeKindRuntimeChangeID = %q; a change ID is in every recorded history and must never change",
			nodeKindRuntimeChangeID)
	}
}
